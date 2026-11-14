package cli

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

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
