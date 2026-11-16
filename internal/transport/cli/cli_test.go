package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"memorize/internal/domain"
	"memorize/internal/modules/memory"
	thttp "memorize/internal/transport/http"
)

func TestHookTimeoutParsingAndBudgets(t *testing.T) {
	if d, ok := parseTimeoutSpec("12s"); !ok || d != 12*time.Second {
		t.Fatalf("parseTimeoutSpec(12s) = (%v, %v)", d, ok)
	}
	if d, ok := parseTimeoutSpec("7.5"); !ok || d != 7500*time.Millisecond {
		t.Fatalf("parseTimeoutSpec(7.5) = (%v, %v)", d, ok)
	}
	if _, ok := parseTimeoutSpec("off"); ok {
		t.Fatalf("expected off to disable timeout")
	}

	pluginDir := t.TempDir()
	hooksJSON := `{"hooks":{"PreToolUse":[{"hooks":[{"type":"command","command":"memorize -hook claude-pretool","timeout":8}]}]}}`
	if err := os.WriteFile(filepath.Join(pluginDir, "hooks.json"), []byte(hooksJSON), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MEMORY_HOOK_TIMEOUT", "")
	os.Unsetenv("MEMORY_HOOK_TIMEOUT")
	t.Setenv("MEMORY_PLUGIN_DIR", pluginDir)

	if d, ok := resolveConfiguredHookTimeout("claude-pretool"); !ok || d != 8*time.Second {
		t.Fatalf("resolveConfiguredHookTimeout = (%v, %v), want 8s", d, ok)
	}
	if d, ok := resolveConfiguredHookTimeout("git-precommit"); ok || d != 0 {
		t.Fatalf("expected no cap for git-precommit, got d=%v ok=%v", d, ok)
	}

	t.Setenv("MEMORY_HOOK_TIMEOUT", "4s")
	if d, ok := resolveConfiguredHookTimeout("claude-pretool"); !ok || d != 4*time.Second {
		t.Fatalf("MEMORY_HOOK_TIMEOUT override = (%v, %v), want 4s", d, ok)
	}
	t.Setenv("MEMORY_HOOK_TIMEOUT", "0")
	if _, ok := resolveConfiguredHookTimeout("claude-pretool"); ok {
		t.Fatalf("expected MEMORY_HOOK_TIMEOUT=0 to disable deadline")
	}

	srv, cliB, alive := remainingHookBudgets(time.Now().Add(-500*time.Millisecond), 5*time.Second)
	if !alive || srv <= 0 || cliB <= srv {
		t.Fatalf("unexpected remainingHookBudgets: srv=%v cli=%v alive=%v", srv, cliB, alive)
	}
	if _, _, stillAlive := remainingHookBudgets(time.Now().Add(-10*time.Second), 5*time.Second); stillAlive {
		t.Fatalf("expected expired hook budget to return alive=false")
	}
}

func TestExecuteDispatchWithRuntimeHooksAndPortCLI(t *testing.T) {
	tmp := t.TempDir()
	dbPath := filepath.Join(tmp, "memories.db")
	t.Setenv("MEMORY_DB_PATH", dbPath)
	t.Setenv("MEMORY_MODELS_DIR", filepath.Join(tmp, "models"))

	if code := executeArgs([]string{"-version"}, "9.9.9-unit", RuntimeHooks{}); code != 0 {
		t.Fatalf("-version exited %d", code)
	}

	// Offline port update & inspect via CLI flags without requiring daemon process.
	if code := executeArgs([]string{"daemon", "port", "--daemon", "19195", "--gguf", "19188", "--json"}, "9.9.9-unit", RuntimeHooks{}); code != 0 {
		t.Fatalf("daemon port set exited %d", code)
	}
	if code := executeArgs([]string{"daemon", "port", "--reset", "--json"}, "9.9.9-unit", RuntimeHooks{}); code != 0 {
		t.Fatalf("daemon port reset exited %d", code)
	}
	if code := executeArgs([]string{"daemon", "port", "--json"}, "9.9.9-unit", RuntimeHooks{}); code != 0 {
		t.Fatalf("daemon port status exited %d", code)
	}

	// Inline hook routing when daemon mode is disabled.
	t.Setenv("MEMORY_DISABLE_BG_DASHBOARD", "1")
	var hookCalled bool
	var gotHookMode string
	code := executeArgs([]string{"-hook", "claude-start"}, "9.9.9-unit", RuntimeHooks{
		RunInlineHook: func(ctx context.Context, db, models, mode string, rawStdin []byte) (string, int) {
			hookCalled = true
			gotHookMode = mode
			return `{"continue":true}`, 0
		},
	})
	if code != 0 || !hookCalled || gotHookMode != "claude-start" {
		t.Fatalf("inline hook mismatch: code=%d called=%v mode=%q", code, hookCalled, gotHookMode)
	}

	// Explicit daemon server hook dispatch.
	var daemonServed bool
	code = executeArgs([]string{"-daemon", "-addr", "127.0.0.1:0"}, "9.9.9-unit", RuntimeHooks{
		RunDaemonServer: func(explicitAddr, gotDB, gotModels string) int {
			daemonServed = explicitAddr == "127.0.0.1:0" && gotDB == dbPath
			return 0
		},
	})
	if code != 0 || !daemonServed {
		t.Fatalf("-daemon dispatch mismatch: code=%d served=%v", code, daemonServed)
	}

	// Inline stdio hook dispatch when daemon bridge is disabled.
	var inlineStdioCalled bool
	code = executeArgs(nil, "9.9.9-unit", RuntimeHooks{
		ServeInlineStdio: func(gotDB, gotModels string, withDash bool, dashAddr string) int {
			inlineStdioCalled = gotDB == dbPath && !withDash
			return 0
		},
	})
	if code != 0 || !inlineStdioCalled {
		t.Fatalf("inline stdio dispatch mismatch: code=%d called=%v", code, inlineStdioCalled)
	}
}

func TestRunHookViaDaemonHTTP(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/daemon/hook" {
			http.NotFound(w, r)
			return
		}
		var req map[string]any
		_ = json.NewDecoder(r.Body).Decode(&req)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"output":    "ok:" + req["mode"].(string),
			"exit_code": 0,
		})
	}))
	defer srv.Close()

	addr := strings.TrimPrefix(srv.URL, "http://")
	out, code, ok := runHookViaDaemon(addr, t.TempDir()+"/memories.db", "", "antigravity-start", []byte(`{}`), time.Now(), func(explicitAddr, dbPath, modelsDir string) (*thttp.DaemonStatus, bool) {
		return &thttp.DaemonStatus{Running: true, ListenAddr: addr}, true
	})
	if !ok || code != 0 || out != "ok:antigravity-start" {
		t.Fatalf("runHookViaDaemon = (%q, %d, %v)", out, code, ok)
	}
}

type noopEval struct{}

func (noopEval) Name() string { return "noop" }
func (noopEval) ScoreRelevance(_ context.Context, _ string, _ []domain.Fact) ([]domain.ScoredFact, error) {
	return nil, nil
}
func (noopEval) EvaluateMutation(_ context.Context, _, _ string, _ []domain.Fact) (domain.MutationDecision, error) {
	return domain.MutationDecision{Category: "arch", IsPermanent: 0.95, Action: "INSERT", Confidence: 0.9}, nil
}
func (noopEval) VerifyPlan(_ context.Context, _ string, _ []domain.Fact) ([]domain.PlanViolation, error) {
	return nil, nil
}

func TestSyncCLIAndServeSyncOverDaemonHTTP(t *testing.T) {
	t.Cleanup(domain.SetMemorySyncEnabledForTest(true))
	tmp := t.TempDir()
	dbA := filepath.Join(tmp, "a.db")
	dbB := filepath.Join(tmp, "b.db")
	t.Setenv("MEMORY_DB_PATH", dbB)

	svcA, err := memory.Open(dbA, noopEval{})
	if err != nil {
		t.Fatal(err)
	}
	defer svcA.Close()
	svcB, err := memory.Open(dbB, noopEval{})
	if err != nil {
		t.Fatal(err)
	}
	defer svcB.Close()

	ctx := domain.WithAdminBypass(context.Background())
	if _, err := svcA.CommitMemory(ctx, "CLI sync initiator rule", "arch", "global"); err != nil {
		t.Fatal(err)
	}
	if _, err := svcB.CommitMemory(ctx, "Daemon sync responder rule", "arch", "global"); err != nil {
		t.Fatal(err)
	}

	// Serve svcB behind real NewDaemonMux HTTP server.
	mux := thttp.NewDaemonMux(thttp.DaemonDeps{Memory: svcB})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	addr := strings.TrimPrefix(srv.URL, "http://")

	// Inline sync hook dispatch when daemon is disabled via :memory:
	var inlineSyncCalled bool
	err = serveSyncIO(ctx, "", ":memory:", "", RuntimeHooks{
		ServeInlineSync: func(_ context.Context, db, _ string, _ io.Reader, _ io.Writer) error {
			inlineSyncCalled = db == ":memory:"
			return nil
		},
	}, strings.NewReader(""), io.Discard)
	if err != nil || !inlineSyncCalled {
		t.Fatalf("inline serveSyncIO failed: err=%v called=%v", err, inlineSyncCalled)
	}

	// Daemon sync-serve round-trip over pipe against real /api/daemon/sync.
	c2sR, c2sW := io.Pipe()
	s2cR, s2cW := io.Pipe()
	srvDone := make(chan error, 1)
	hooks := RuntimeHooks{
		EnsureDaemonRunning: func(explicitAddr, dbPath, modelsDir string) (*thttp.DaemonStatus, bool) {
			return &thttp.DaemonStatus{Running: true, ListenAddr: addr}, true
		},
	}
	go func() {
		srvDone <- serveSyncIO(ctx, addr, dbB, "", hooks, c2sR, s2cW)
		_ = c2sR.Close()
		_ = s2cW.Close()
	}()

	rw := struct {
		io.Reader
		io.Writer
	}{Reader: s2cR, Writer: c2sW}
	report, err := svcA.RunSyncStream(ctx, domain.SyncPeer{PeerID: "daemon-b", SSHTarget: "daemon-b.local", ScopeMode: domain.SyncScopeModeAll}, rw)
	_ = c2sW.Close()
	_ = s2cR.Close()
	if err != nil {
		t.Fatalf("RunSyncStream against daemon mux: %v", err)
	}
	if serr := <-srvDone; serr != nil {
		t.Fatalf("serveSyncIO daemon error: %v", serr)
	}
	if report.PulledApplied != 1 || report.PushedApplied != 1 {
		t.Fatalf("expected bidirectional 1 pull + 1 push via daemon HTTP, got %+v", report)
	}

	// Verify flag reordering + `sync status --json` command via executeArgs.
	reordered := reorderFlagsBeforePositional(
		[]string{"dev@box", "--scope", "global,project:a", "--test", "--remote-bin", "/usr/local/bin/memorize"},
		map[string]bool{"scope": true, "remote-bin": true},
	)
	if len(reordered) != 6 || reordered[len(reordered)-1] != "dev@box" {
		t.Fatalf("unexpected reorderFlagsBeforePositional: %v", reordered)
	}

	// Security guard check: non-JSON POST to /api/daemon/sync must be 403.
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/api/daemon/sync", bytes.NewBufferString(`{"action":"status"}`))
	req.Header.Set("Content-Type", "text/plain")
	secResp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = secResp.Body.Close()
	if secResp.StatusCode != http.StatusForbidden {
		t.Fatalf("expected 403 for non-JSON /api/daemon/sync POST, got %d", secResp.StatusCode)
	}

	if code := executeArgs([]string{"sync", "status", "--json", "-addr", addr}, "9.9.9-unit", hooks); code != 0 {
		t.Fatalf("memorize sync status exited %d", code)
	}
}

func TestSyncCLIAndDaemonRPCBlockedWhenDisabled(t *testing.T) {
	if domain.IsMemorySyncEnabled() {
		t.Fatalf("expected IsMemorySyncEnabled() == false by default")
	}
	dbPath := filepath.Join(t.TempDir(), "sync-disabled.db")
	svc, err := memory.Open(dbPath, noopEval{})
	if err != nil {
		t.Fatal(err)
	}
	defer svc.Close()

	mux := thttp.NewDaemonMux(thttp.DaemonDeps{Memory: svc})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	addr := strings.TrimPrefix(srv.URL, "http://")

	hooks := RuntimeHooks{
		EnsureDaemonRunning: func(explicitAddr, dbPath, modelsDir string) (*thttp.DaemonStatus, bool) {
			return &thttp.DaemonStatus{Running: true, ListenAddr: addr}, true
		},
	}
	if code := executeArgs([]string{"sync", "status", "-addr", addr}, "9.9.9-unit", hooks); code != 1 {
		t.Fatalf("expected exit 1 from disabled `memorize sync status`, got %d", code)
	}
	if code := executeArgs([]string{"-sync-serve"}, "9.9.9-unit", hooks); code != 1 {
		t.Fatalf("expected exit 1 from disabled `--sync-serve`, got %d", code)
	}

	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/api/daemon/sync", bytes.NewBufferString(`{"action":"status"}`))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("expected 403 from /api/daemon/sync when disabled, got %d", resp.StatusCode)
	}
}
