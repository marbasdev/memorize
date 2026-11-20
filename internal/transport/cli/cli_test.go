package cli

import (
	"archive/tar"
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"praxis/internal/domain"
	"praxis/internal/infra/config"
	"praxis/internal/infra/sqlite"
	"praxis/internal/modules/guardrail"
	"praxis/internal/modules/memory"
	"praxis/internal/modules/progress"
	"praxis/internal/transport/dashboard"
	thttp "praxis/internal/transport/http"
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
	hooksJSON := `{"hooks":{"PreToolUse":[{"hooks":[{"type":"command","command":"praxis -hook claude-pretool","timeout":8}]}]}}`
	if err := os.WriteFile(filepath.Join(pluginDir, "hooks.json"), []byte(hooksJSON), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PRAXIS_HOOK_TIMEOUT", "")
	os.Unsetenv("PRAXIS_HOOK_TIMEOUT")
	t.Setenv("PRAXIS_PLUGIN_DIR", pluginDir)

	if d, ok := resolveConfiguredHookTimeout("claude-pretool"); !ok || d != 8*time.Second {
		t.Fatalf("resolveConfiguredHookTimeout = (%v, %v), want 8s", d, ok)
	}
	if d, ok := resolveConfiguredHookTimeout("git-precommit"); ok || d != 0 {
		t.Fatalf("expected no cap for git-precommit, got d=%v ok=%v", d, ok)
	}

	t.Setenv("PRAXIS_HOOK_TIMEOUT", "4s")
	if d, ok := resolveConfiguredHookTimeout("claude-pretool"); !ok || d != 4*time.Second {
		t.Fatalf("PRAXIS_HOOK_TIMEOUT override = (%v, %v), want 4s", d, ok)
	}
	t.Setenv("PRAXIS_HOOK_TIMEOUT", "0")
	if _, ok := resolveConfiguredHookTimeout("claude-pretool"); ok {
		t.Fatalf("expected PRAXIS_HOOK_TIMEOUT=0 to disable deadline")
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
	t.Setenv("PRAXIS_DB_PATH", dbPath)
	t.Setenv("PRAXIS_MODELS_DIR", filepath.Join(tmp, "models"))

	if code := executeArgs([]string{"-version"}, "9.9.9-unit", RuntimeHooks{}); code != 0 {
		t.Fatalf("-version exited %d", code)
	}

	if code := executeArgs([]string{"daemon", "port", "--daemon", "19195", "--gguf", "19188", "--json"}, "9.9.9-unit", RuntimeHooks{}); code != 0 {
		t.Fatalf("daemon port set exited %d", code)
	}
	if code := executeArgs([]string{"daemon", "port", "--reset", "--json"}, "9.9.9-unit", RuntimeHooks{}); code != 0 {
		t.Fatalf("daemon port reset exited %d", code)
	}
	if code := executeArgs([]string{"daemon", "port", "--json"}, "9.9.9-unit", RuntimeHooks{}); code != 0 {
		t.Fatalf("daemon port status exited %d", code)
	}

	t.Setenv("PRAXIS_DISABLE_BG_DASHBOARD", "1")
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

type cliTeamEval struct{}

func (cliTeamEval) Name() string { return "cli-team-eval" }
func (cliTeamEval) ScoreRelevance(_ context.Context, _ string, facts []domain.Fact) ([]domain.ScoredFact, error) {
	out := make([]domain.ScoredFact, len(facts))
	for i, f := range facts {
		out[i] = domain.ScoredFact{Fact: f, Score: 4.5, Confidence: 0.9}
	}
	return out, nil
}
func (cliTeamEval) EvaluateMutation(_ context.Context, _ string, _ string, _ []domain.Fact) (domain.MutationDecision, error) {
	return domain.MutationDecision{Action: "INSERT", Category: "security", IsPermanent: 0.95, IsGlobalInvariant: 0.95, Confidence: 0.95}, nil
}
func (cliTeamEval) VerifyPlan(_ context.Context, plan string, rules []domain.Fact) ([]domain.PlanViolation, error) {
	if strings.Contains(plan, "BAD_TEAM_TOKEN") && len(rules) > 0 {
		return []domain.PlanViolation{{
			Fact:        rules[0],
			Probability: 0.95,
			Confidence:  0.95,
		}}, nil
	}
	return nil, nil
}

func captureStdout(t *testing.T, fn func() int) (int, string) {
	t.Helper()
	old := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w
	code := fn()
	_ = w.Close()
	os.Stdout = old
	b, _ := io.ReadAll(r)
	_ = r.Close()
	return code, string(b)
}

func TestCLIUserConnectAndThinClientEndToEnd(t *testing.T) {
	tmpRoot := t.TempDir()
	srvDBPath := filepath.Join(tmpRoot, "server", "memories.db")
	cliDataDir := filepath.Join(tmpRoot, "client")
	cliDBPath := filepath.Join(cliDataDir, "memories.db")
	teamRepo := filepath.Join(tmpRoot, "repos", "team-service")
	soloRepo := filepath.Join(tmpRoot, "repos", "solo-notes")
	if err := os.MkdirAll(teamRepo, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(soloRepo, 0o755); err != nil {
		t.Fatal(err)
	}

	t.Setenv("PRAXIS_DATA_DIR", "")
	t.Setenv("PRAXIS_DB_PATH", srvDBPath)

	code, out := captureStdout(t, func() int {
		return executeArgs([]string{"user", "add", "hostadmin", "--role", "admin", "--json"}, "test", RuntimeHooks{})
	})
	if code != 0 {
		t.Fatalf("user add hostadmin failed (%d): %s", code, out)
	}
	if code, _ := captureStdout(t, func() int {
		return executeArgs([]string{"user", "disable", "hostadmin"}, "test", RuntimeHooks{})
	}); code == 0 {
		t.Fatalf("expected disabling sole admin to exit non-zero")
	}

	code, aliceOut := captureStdout(t, func() int {
		return executeArgs([]string{"user", "add", "alice", "--role", "member", "--json"}, "test", RuntimeHooks{})
	})
	if code != 0 {
		t.Fatalf("user add alice failed (%d): %s", code, aliceOut)
	}
	var aliceIssue domain.UserTokenIssue
	if err := json.Unmarshal([]byte(aliceOut), &aliceIssue); err != nil || aliceIssue.PlainToken == "" {
		t.Fatalf("decode alice token issue failed: %v (%s)", err, aliceOut)
	}

	mem, err := memory.Open(srvDBPath, cliTeamEval{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = mem.Close() })
	prog := progress.New(mem)
	gr := guardrail.New(mem, prog)

	ctxAdmin := domain.WithAuthPrincipal(context.Background(), domain.AuthPrincipal{
		Account: "hostadmin", Role: domain.RoleAdmin, ServerMode: true, Authenticated: true,
	})
	if _, err := mem.CommitMemoryFull(ctxAdmin, "Never write BAD_TEAM_TOKEN in source files", "security", "", "", "project:team-service"); err != nil {
		t.Fatal(err)
	}

	dashH := dashboard.NewHandler(dashboard.Deps{
		ServerMode: true, DB: mem.DB(), Memory: mem, Progress: prog, Guardrail: gr,
	})
	srv := httptest.NewServer(thttp.NewDaemonMux(thttp.DaemonDeps{
		ServerMode:    true,
		DB:            mem.DB(),
		Memory:        mem,
		Guardrail:     gr,
		Dashboard:     dashH,
		BuildStatusFn: func() thttp.DaemonStatus { return thttp.DaemonStatus{Running: true, Version: "test"} },
	}))
	defer srv.Close()

	t.Setenv("PRAXIS_DB_PATH", cliDBPath)
	code, connOut := captureStdout(t, func() int {
		return executeArgs([]string{"connect", "--url", srv.URL, "--token", aliceIssue.PlainToken, "--repo", teamRepo, "--json"}, "test", RuntimeHooks{})
	})
	if code != 0 || !strings.Contains(connOut, `"account":"alice"`) {
		t.Fatalf("connect failed: code=%d out=%s", code, connOut)
	}
	if _, ok := config.ResolveRemoteTarget(cliDataDir, soloRepo); ok {
		t.Fatalf("unbound personal repo should remain local solo")
	}

	targetFile := filepath.Join(teamRepo, "handler.go")
	if err := os.WriteFile(targetFile, []byte("package main\nfunc ok() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	ctxAlice := domain.WithAuthPrincipal(context.Background(), domain.AuthPrincipal{
		Account: "alice", Role: domain.RoleMember, ServerMode: true, Authenticated: true,
	})
	mem.MarkTurnScopeMutatedCtx(ctxAlice, "project:team-service")

	preJSON, _ := json.Marshal(map[string]any{
		"session_id":      "cli-s1",
		"workspace_roots": []string{teamRepo},
		"tool_name":       "edit_file",
		"tool_input":      map[string]any{"file_path": targetFile},
	})
	_, _, handled := runHookViaRemote(domain.RemoteTargetConfig{URL: srv.URL, Token: aliceIssue.PlainToken}, cliDataDir, "claude-pretool", preJSON, time.Now())
	if !handled {
		t.Fatalf("expected remote pretool handled")
	}

	if err := os.WriteFile(targetFile, []byte("package main\nfunc ok() { BAD_TEAM_TOKEN() }\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	stopJSON, _ := json.Marshal(map[string]any{
		"session_id":      "cli-s1",
		"workspace_roots": []string{teamRepo},
	})
	outStop, _, handled := runHookViaRemote(domain.RemoteTargetConfig{URL: srv.URL, Token: aliceIssue.PlainToken}, cliDataDir, "claude-stop", stopJSON, time.Now())
	if !handled || !strings.Contains(outStop, "block") {
		t.Fatalf("expected remote Thin Client Stop hook to block BAD_TEAM_TOKEN, out=%s", outStop)
	}
	logs, err := gr.QueryGuardrailLogsPage(ctxAlice, domain.GuardrailLogQuery{State: "all", PageSize: 10})
	if err != nil || logs.TotalCount != 1 || logs.Records[0].Account != "alice" {
		t.Fatalf("expected guardrail log attributed to alice, got %+v (err=%v)", logs, err)
	}
	if rem := config.ConsumeClientTurnDiffs(cliDataDir, "cli-s1", false); len(rem) == 0 {
		t.Fatalf("expected client baseline kept while Stop blocked")
	}

	if err := os.WriteFile(targetFile, []byte("package main\nfunc ok() { cleanCall() }\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	outClean, _, _ := runHookViaRemote(domain.RemoteTargetConfig{URL: srv.URL, Token: aliceIssue.PlainToken}, cliDataDir, "claude-stop", stopJSON, time.Now())
	if strings.Contains(outClean, "block") {
		t.Fatalf("expected clean Stop to pass, got %s", outClean)
	}
	if rem := config.ConsumeClientTurnDiffs(cliDataDir, "cli-s1", false); len(rem) != 0 {
		t.Fatalf("expected client baseline cleared after passing Stop, got %d", len(rem))
	}

	code, discOut := captureStdout(t, func() int {
		return executeArgs([]string{"disconnect", "--repo", teamRepo, "--json"}, "test", RuntimeHooks{})
	})
	if code != 0 || !strings.Contains(discOut, `"disconnected":true`) {
		t.Fatalf("disconnect failed: code=%d out=%s", code, discOut)
	}
	if _, ok := config.ResolveRemoteTarget(cliDataDir, teamRepo); ok {
		t.Fatalf("expected teamRepo unbound after disconnect")
	}
}

func TestSingleBinaryInstallersAndEmbeddedMCPResource(t *testing.T) {
	if !strings.Contains(memory.PlaybookSkillMD, "name: memory-curator") {
		t.Fatalf("expected embedded PlaybookSkillMD frontmatter")
	}
	body := memory.PlaybookBodyMD()
	if strings.HasPrefix(body, "---") || !strings.Contains(body, "Golden Filter") {
		t.Fatalf("expected stripped PlaybookBodyMD, got:\n%s", body)
	}

	repoRoot, err := filepath.Abs(filepath.Join("..", "..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	tmp := t.TempDir()
	binPath := filepath.Join(tmp, "praxis")
	if runtime.GOOS == "windows" {
		binPath += ".exe"
	}
	buildCmd := exec.Command("go", "build", "-ldflags=-X main.version=1.5.0-test", "-o", binPath, "./cmd/praxis")
	buildCmd.Dir = repoRoot
	if out, err := buildCmd.CombinedOutput(); err != nil {
		t.Fatalf("build praxis failed: %v\n%s", err, string(out))
	}

	verOut, err := exec.Command(binPath, "-version").CombinedOutput()
	if err != nil || strings.TrimSpace(string(verOut)) != "praxis 1.5.0-test" {
		t.Fatalf("unexpected -version output: %q (%v)", string(verOut), err)
	}

	dataDir := filepath.Join(tmp, "data")
	repoDir := filepath.Join(tmp, "repo")
	if err := os.MkdirAll(filepath.Join(repoDir, ".git", "hooks"), 0o755); err != nil {
		t.Fatal(err)
	}
	instGit := exec.Command(binPath, "-install", "git-hook")
	instGit.Dir = repoDir
	instGit.Env = append(os.Environ(), "PRAXIS_DATA_DIR="+dataDir)
	if out, err := instGit.CombinedOutput(); err != nil {
		t.Fatalf("-install git-hook failed: %v\n%s", err, string(out))
	}
	hookPath := filepath.Join(repoDir, ".git", "hooks", "pre-commit")
	hookBytes, err := os.ReadFile(hookPath)
	if err != nil {
		t.Fatalf("missing pre-commit hook: %v", err)
	}
	realBinPath, err := filepath.EvalSymlinks(binPath)
	if err != nil {
		realBinPath = binPath
	}
	if !strings.Contains(string(hookBytes), "'"+realBinPath+"'") && !strings.Contains(string(hookBytes), "'"+binPath+"'") {
		t.Fatalf("expected single-quoted binary path in pre-commit hook, got:\n%s", string(hookBytes))
	}
	if runtime.GOOS != "windows" {
		symRepo := filepath.Join(tmp, "symrepo")
		if err := os.MkdirAll(filepath.Join(symRepo, ".git", "hooks"), 0o755); err != nil {
			t.Fatal(err)
		}
		targetVictim := filepath.Join(tmp, "victim.txt")
		if err := os.WriteFile(targetVictim, []byte("untouched"), 0o600); err != nil {
			t.Fatal(err)
		}
		symHook := filepath.Join(symRepo, ".git", "hooks", "pre-commit")
		if err := os.Symlink(targetVictim, symHook); err != nil {
			t.Fatal(err)
		}
		symCmd := exec.Command(binPath, "-install", "git-hook")
		symCmd.Dir = symRepo
		symCmd.Env = append(os.Environ(), "PRAXIS_DATA_DIR="+dataDir)
		if out, err := symCmd.CombinedOutput(); err == nil {
			t.Fatalf("expected -install git-hook to refuse symlink pre-commit hook, output:\n%s", string(out))
		}
		if got, _ := os.ReadFile(targetVictim); string(got) != "untouched" {
			t.Fatalf("symlink target was modified by -install git-hook")
		}
	}

	srvCmd := exec.Command(binPath)
	srvCmd.Env = append(os.Environ(),
		"PRAXIS_DB_PATH="+filepath.Join(tmp, "stdio.db"),
		"PRAXIS_DISABLE_BG_DASHBOARD=1",
	)
	stdin, err := srvCmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := srvCmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := srvCmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = stdin.Close()
		_ = srvCmd.Wait()
	}()

	reader := bufio.NewReader(stdout)
	sendRPC := func(msg string) string {
		t.Helper()
		_, _ = stdin.Write([]byte(msg + "\n"))
		line, err := reader.ReadString('\n')
		if err != nil {
			t.Fatalf("read rpc line: %v", err)
		}
		return line
	}

	initLine := sendRPC(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{},"clientInfo":{"name":"test","version":"1.0"}}}`)
	if !strings.Contains(initLine, memory.PlaybookURI) {
		t.Fatalf("expected serverInstructions in initialize response to reference %s, got: %s", memory.PlaybookURI, initLine)
	}
	for _, wantToken := range []string{"project_progress", "record", "Never self-revoke rules silently"} {
		if !strings.Contains(initLine, wantToken) {
			t.Fatalf("expected serverInstructions in initialize response to describe %q, got: %s", wantToken, initLine)
		}
	}

	resLine := sendRPC(`{"jsonrpc":"2.0","id":2,"method":"resources/read","params":{"uri":"praxis://playbook/memory-curator"}}`)
	var resPayload struct {
		Result struct {
			Contents []struct {
				URI  string `json:"uri"`
				Text string `json:"text"`
			} `json:"contents"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(resLine), &resPayload); err != nil {
		t.Fatalf("unmarshal resources/read: %v (%s)", err, resLine)
	}
	if len(resPayload.Result.Contents) != 1 || !strings.Contains(resPayload.Result.Contents[0].Text, "Golden Filter") || !strings.Contains(resPayload.Result.Contents[0].Text, "Evolve Stale Rules Explicitly") {
		t.Fatalf("unexpected embedded MCP resource contents: %+v", resPayload)
	}

	promptLine := sendRPC(`{"jsonrpc":"2.0","id":3,"method":"prompts/get","params":{"name":"memory-curator"}}`)
	if !strings.Contains(promptLine, "Golden Filter") {
		t.Fatalf("expected prompts/get memory-curator to return playbook text, got: %s", promptLine)
	}
}

func packageBinaryTarGz(t *testing.T, binBytes []byte) ([]byte, string) {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	hdr := &tar.Header{
		Name: "praxis",
		Mode: 0o755,
		Size: int64(len(binBytes)),
	}
	if err := tw.WriteHeader(hdr); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write(binBytes); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	archive := buf.Bytes()
	sum := sha256.Sum256(archive)
	return archive, hex.EncodeToString(sum[:])
}

func TestPluginManifestsAndAutoUpdateBootstrapper(t *testing.T) {
	repoRoot, err := filepath.Abs(filepath.Join("..", "..", ".."))
	if err != nil {
		t.Fatal(err)
	}

	readManifestVersion := func(relPath string) string {
		t.Helper()
		raw, err := os.ReadFile(filepath.Join(repoRoot, relPath))
		if err != nil {
			t.Fatalf("read %s: %v", relPath, err)
		}
		var doc map[string]any
		if err := json.Unmarshal(raw, &doc); err != nil {
			t.Fatalf("invalid JSON in %s: %v", relPath, err)
		}
		if v, ok := doc["version"].(string); ok && v != "" {
			return v
		}
		if plugins, ok := doc["plugins"].([]any); ok && len(plugins) > 0 {
			if first, ok := plugins[0].(map[string]any); ok {
				if v, ok := first["version"].(string); ok {
					return v
				}
			}
		}
		return ""
	}

	agyVer := readManifestVersion("plugins/praxis/plugin.json")
	claudeVer := readManifestVersion("plugins/praxis/.claude-plugin/plugin.json")
	mpVer := readManifestVersion(".claude-plugin/marketplace.json")
	if agyVer == "" || agyVer != claudeVer || agyVer != mpVer {
		t.Fatalf("version mismatch across manifests: agy=%q claude=%q marketplace=%q", agyVer, claudeVer, mpVer)
	}

	for _, rel := range []string{
		"plugins/praxis/mcp_config.json",
		"plugins/praxis/hooks.json",
		"plugins/praxis/.claude-plugin/plugin.json",
	} {
		raw, err := os.ReadFile(filepath.Join(repoRoot, rel))
		if err != nil {
			t.Fatalf("read %s: %v", rel, err)
		}
		var doc map[string]any
		if err := json.Unmarshal(raw, &doc); err != nil {
			t.Fatalf("invalid JSON in %s: %v", rel, err)
		}
	}

	if _, err := os.Stat(filepath.Join(repoRoot, "plugins/praxis/rules/AGENTS.md")); !os.IsNotExist(err) {
		t.Fatalf("plugins/praxis/rules/AGENTS.md must not exist (praxis replaces static AGENTS.md rule files via MCP instructions & DB rules); stat err=%v", err)
	}

	skillBytes, err := os.ReadFile(filepath.Join(repoRoot, "plugins/praxis/skills/memory-curator/SKILL.md"))
	if err != nil {
		t.Fatalf("read plugin SKILL.md: %v", err)
	}
	if string(skillBytes) != memory.PlaybookSkillMD {
		t.Fatalf("plugins/praxis/skills/memory-curator/SKILL.md is out of sync with internal/modules/memory/playbook/SKILL.md")
	}

	if runtime.GOOS == "windows" {
		return
	}
	tmp := t.TempDir()
	bin100Path := filepath.Join(tmp, "praxis-1.0.0-bin")
	bin110Path := filepath.Join(tmp, "praxis-1.1.0-bin")
	for ver, outPath := range map[string]string{"1.0.0": bin100Path, "1.1.0": bin110Path} {
		cmd := exec.Command("go", "build", "-ldflags=-X main.version="+ver, "-o", outPath, "./cmd/praxis")
		cmd.Dir = repoRoot
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("build %s failed: %v\n%s", ver, err, string(out))
		}
	}

	b100, err := os.ReadFile(bin100Path)
	if err != nil {
		t.Fatal(err)
	}
	b110, err := os.ReadFile(bin110Path)
	if err != nil {
		t.Fatal(err)
	}
	archiveName := fmt.Sprintf("praxis-%s-%s.tar.gz", runtime.GOOS, runtime.GOARCH)
	tgz100, sum100 := packageBinaryTarGz(t, b100)
	tgz110, sum110 := packageBinaryTarGz(t, b110)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/org/praxis/-/releases/v1.0.0/downloads/" + archiveName,
			"/github.com/org/praxis/releases/download/v1.0.0/" + archiveName:
			_, _ = w.Write(tgz100)
		case "/org/praxis/-/releases/v1.0.0/downloads/SHA256SUMS",
			"/github.com/org/praxis/releases/download/v1.0.0/SHA256SUMS":
			_, _ = fmt.Fprintf(w, "%s  ./%s\n", sum100, archiveName)
		case "/org/praxis/-/releases/v1.1.0/downloads/" + archiveName,
			"/github.com/org/praxis/releases/download/v1.1.0/" + archiveName:
			_, _ = w.Write(tgz110)
		case "/org/praxis/-/releases/v1.1.0/downloads/SHA256SUMS",
			"/github.com/org/praxis/releases/download/v1.1.0/SHA256SUMS":
			_, _ = fmt.Fprintf(w, "%s  ./%s\n", sum110, archiveName)
		case "/org/nosums/-/releases/v1.0.0/downloads/" + archiveName:
			_, _ = w.Write(tgz100)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	isolatedPlugin := filepath.Join(tmp, "isolated-plugin", "praxis")
	if err := os.MkdirAll(filepath.Join(isolatedPlugin, "scripts"), 0o755); err != nil {
		t.Fatal(err)
	}
	runnerBytes, err := os.ReadFile(filepath.Join(repoRoot, "plugins/praxis/scripts/run-praxis.sh"))
	if err != nil {
		t.Fatal(err)
	}
	runnerPath := filepath.Join(isolatedPlugin, "scripts", "run-praxis.sh")
	if err := os.WriteFile(runnerPath, runnerBytes, 0o755); err != nil {
		t.Fatal(err)
	}

	dataDir := filepath.Join(tmp, "user-data")
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		t.Fatal(err)
	}
	dbMarkerPath := filepath.Join(dataDir, "memories.db")
	if err := os.WriteFile(dbMarkerPath, []byte("sqlite-state-preserved"), 0o600); err != nil {
		t.Fatal(err)
	}

	writePluginManifest := func(ver string) {
		t.Helper()
		manifest := fmt.Sprintf("{\n  \"name\": \"praxis\",\n  \"version\": %q,\n  \"repository\": %q\n}\n", ver, srv.URL+"/org/praxis")
		if err := os.WriteFile(filepath.Join(isolatedPlugin, "plugin.json"), []byte(manifest), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	writePluginManifest("1.0.0")
	cmd100 := exec.Command("/bin/sh", runnerPath, "-version")
	cmd100.Env = append(os.Environ(), "PRAXIS_DATA_DIR="+dataDir)
	var stdout100, stderr100 bytes.Buffer
	cmd100.Stdout = &stdout100
	cmd100.Stderr = &stderr100
	if err := cmd100.Run(); err != nil {
		t.Fatalf("run-praxis.sh v1.0.0 failed: %v\nstderr: %s", err, stderr100.String())
	}
	if strings.TrimSpace(stdout100.String()) != "praxis 1.0.0" {
		t.Fatalf("expected clean stdout 'praxis 1.0.0', got %q (stderr: %s)", stdout100.String(), stderr100.String())
	}
	if _, err := os.Stat(filepath.Join(dataDir, "bin", "praxis-1.0.0")); err != nil {
		t.Fatalf("expected cached praxis-1.0.0 binary: %v", err)
	}

	writePluginManifest("1.1.0")
	cmd110 := exec.Command("/bin/sh", runnerPath, "-version")
	cmd110.Env = append(os.Environ(), "PRAXIS_DATA_DIR="+dataDir)
	var stdout110, stderr110 bytes.Buffer
	cmd110.Stdout = &stdout110
	cmd110.Stderr = &stderr110
	if err := cmd110.Run(); err != nil {
		t.Fatalf("run-praxis.sh v1.1.0 upgrade failed: %v\nstderr: %s", err, stderr110.String())
	}
	if strings.TrimSpace(stdout110.String()) != "praxis 1.1.0" {
		t.Fatalf("expected clean stdout 'praxis 1.1.0', got %q (stderr: %s)", stdout110.String(), stderr110.String())
	}
	if _, err := os.Stat(filepath.Join(dataDir, "bin", "praxis-1.1.0")); err != nil {
		t.Fatalf("expected cached praxis-1.1.0 binary: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dataDir, "bin", "praxis-1.0.0")); !os.IsNotExist(err) {
		t.Fatalf("expected outdated praxis-1.0.0 binary to be pruned, stat err=%v", err)
	}
	preservedDB, err := os.ReadFile(dbMarkerPath)
	if err != nil || string(preservedDB) != "sqlite-state-preserved" {
		t.Fatalf("expected memories.db to be preserved across binary upgrade")
	}

	ghDataDir := filepath.Join(tmp, "gh-user-data")
	ghManifest := fmt.Sprintf("{\n  \"name\": \"praxis\",\n  \"version\": \"1.1.0\",\n  \"repository\": %q\n}\n", srv.URL+"/github.com/org/praxis")
	if err := os.WriteFile(filepath.Join(isolatedPlugin, "plugin.json"), []byte(ghManifest), 0o644); err != nil {
		t.Fatal(err)
	}
	cmdGH := exec.Command("/bin/sh", runnerPath, "-version")
	cmdGH.Env = append(os.Environ(), "PRAXIS_DATA_DIR="+ghDataDir)
	var stdoutGH, stderrGH bytes.Buffer
	cmdGH.Stdout = &stdoutGH
	cmdGH.Stderr = &stderrGH
	if err := cmdGH.Run(); err != nil {
		t.Fatalf("run-praxis.sh GitHub release bootstrap failed: %v\nstderr: %s", err, stderrGH.String())
	}
	if strings.TrimSpace(stdoutGH.String()) != "praxis 1.1.0" {
		t.Fatalf("expected clean stdout 'praxis 1.1.0' from GitHub release bootstrap, got %q", stdoutGH.String())
	}

	noSumsDataDir := filepath.Join(tmp, "nosums-data")
	noSumsManifest := fmt.Sprintf("{\n  \"name\": \"praxis\",\n  \"version\": \"1.0.0\",\n  \"repository\": %q\n}\n", srv.URL+"/org/nosums")
	if err := os.WriteFile(filepath.Join(isolatedPlugin, "plugin.json"), []byte(noSumsManifest), 0o644); err != nil {
		t.Fatal(err)
	}
	cmdNoSums := exec.Command("/bin/sh", runnerPath, "-version")
	cmdNoSums.Env = append(os.Environ(), "PRAXIS_DATA_DIR="+noSumsDataDir)
	if out, err := cmdNoSums.CombinedOutput(); err == nil {
		t.Fatalf("expected run-praxis.sh to fail closed when SHA256SUMS is missing, output:\n%s", string(out))
	}
	if _, err := os.Stat(filepath.Join(noSumsDataDir, "bin", "praxis-1.0.0")); !os.IsNotExist(err) {
		t.Fatalf("expected no binary installed when SHA256SUMS is missing, stat err=%v", err)
	}

	httpDataDir := filepath.Join(tmp, "insecure-http-data")
	cmdInsecure := exec.Command("/bin/sh", runnerPath, "-version")
	cmdInsecure.Env = append(os.Environ(),
		"PRAXIS_DATA_DIR="+httpDataDir,
		"PRAXIS_RELEASE_BASE_URL=http://evil.example.com/v{VERSION}",
	)
	if out, err := cmdInsecure.CombinedOutput(); err == nil {
		t.Fatalf("expected run-praxis.sh to reject non-HTTPS PRAXIS_RELEASE_BASE_URL, output:\n%s", string(out))
	}
}

func TestCLISwitchPushExportAndImportEndToEnd(t *testing.T) {
	tmp := t.TempDir()
	srvDBPath := filepath.Join(tmp, "server", "memories.db")
	cliDataDir := filepath.Join(tmp, "client")
	cliDBPath := filepath.Join(cliDataDir, "memories.db")
	if err := os.MkdirAll(filepath.Dir(srvDBPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(cliDataDir, 0o755); err != nil {
		t.Fatal(err)
	}

	srvMem, err := memory.Open(srvDBPath, cliTeamEval{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = srvMem.Close() })
	ctx := context.Background()
	homeIss, _, err := sqlite.EnsureBootstrapAdmin(ctx, srvMem.DB(), "home", "")
	if err != nil {
		t.Fatal(err)
	}
	aliceIss, err := sqlite.CreateUser(ctx, srvMem.DB(), "alice", domain.RoleMember)
	if err != nil {
		t.Fatal(err)
	}

	srv := httptest.NewServer(thttp.NewDaemonMux(thttp.DaemonDeps{
		ServerMode:    true,
		DB:            srvMem.DB(),
		Memory:        srvMem,
		BuildStatusFn: func() thttp.DaemonStatus { return thttp.DaemonStatus{Running: true, Version: "test"} },
	}))
	defer srv.Close()

	// Seed local Solo DB on the client machine with global + project memories.
	locMem, err := memory.Open(cliDBPath, cliTeamEval{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := locMem.ImportBundle(ctx, domain.MemoryBundle{
		Format: domain.MemoryBundleFormat,
		Items: []domain.MemoryBundleItem{
			{Scope: "global", Category: "workflow", Content: "Always use gopls before grep", Pinned: true},
			{Scope: "project:secret-work", Category: "architecture", Content: "Internal work-only note"},
		},
	}, domain.BundleImportOptions{Mode: "exact"}); err != nil {
		t.Fatal(err)
	}
	_ = locMem.Close()

	t.Setenv("PRAXIS_DATA_DIR", cliDataDir)
	t.Setenv("PRAXIS_DB_PATH", cliDBPath)
	t.Setenv("PRAXIS_REMOTE_URL", "")
	t.Setenv("PRAXIS_REMOTE_TOKEN", "")

	// Connect both @home and @alice tokens (auto-saved into remotes.json profiles).
	if code, out := captureStdout(t, func() int {
		return executeArgs([]string{"connect", "--global", "--url", srv.URL, "--token", homeIss.PlainToken, "--json"}, "test", RuntimeHooks{})
	}); code != 0 || !strings.Contains(out, `"account":"home"`) {
		t.Fatalf("connect home failed: %d %s", code, out)
	}
	if code, out := captureStdout(t, func() int {
		return executeArgs([]string{"connect", "--global", "--url", srv.URL, "--token", aliceIss.PlainToken, "--json"}, "test", RuntimeHooks{})
	}); code != 0 || !strings.Contains(out, `"account":"alice"`) {
		t.Fatalf("connect alice failed: %d %s", code, out)
	}

	// Switch back to @home using saved profile token.
	if code, out := captureStdout(t, func() int {
		return executeArgs([]string{"switch", "home", "--json"}, "test", RuntimeHooks{})
	}); code != 0 || !strings.Contains(out, `"account":"home"`) {
		t.Fatalf("switch home failed: %d %s", code, out)
	}
	if code, out := captureStdout(t, func() int {
		return executeArgs([]string{"switch", "--json"}, "test", RuntimeHooks{})
	}); code != 0 || !strings.Contains(out, `"active_account":"home"`) || !strings.Contains(out, `"alice"`) {
		t.Fatalf("switch list failed: %d %s", code, out)
	}

	// Safety check: `praxis push` without --scope or --all must refuse.
	if code, _ := captureStdout(t, func() int {
		return executeArgs([]string{"push"}, "test", RuntimeHooks{})
	}); code == 0 {
		t.Fatalf("expected push without --scope or --all to fail safety check")
	}

	// Push only global scope to @home (must NOT upload project:secret-work).
	if code, out := captureStdout(t, func() int {
		return executeArgs([]string{"push", "--scope", "global", "--json"}, "test", RuntimeHooks{})
	}); code != 0 || !strings.Contains(out, `"inserted":1`) {
		t.Fatalf("push --scope global failed: %d %s", code, out)
	}

	ctxHome := domain.WithAuthPrincipal(ctx, domain.AuthPrincipal{Account: "home", Role: domain.RoleAdmin, ServerMode: true, Authenticated: true})
	homePage, _ := srvMem.QueryRecordsPage(ctxHome, domain.RecordQuery{State: "active"})
	if len(homePage.Records) != 1 || homePage.Records[0].Scope != "global" || !homePage.Records[0].Pinned {
		t.Fatalf("expected only pushed global memory on server for @home, got %+v", homePage.Records)
	}

	// Export @home's global rules to JSON file, then import into @alice's profile.
	bundleFile := filepath.Join(tmp, "shared-global.json")
	if code, _ := captureStdout(t, func() int {
		return executeArgs([]string{"export", "--scope", "global", "-o", bundleFile}, "test", RuntimeHooks{})
	}); code != 0 {
		t.Fatalf("export failed: %d", code)
	}
	if code, out := captureStdout(t, func() int {
		return executeArgs([]string{"import", bundleFile, "--profile", "alice", "--mode", "exact", "--json"}, "test", RuntimeHooks{})
	}); code != 0 || !strings.Contains(out, `"inserted":1`) {
		t.Fatalf("import to alice failed: %d %s", code, out)
	}

	ctxAlice := domain.WithAuthPrincipal(ctx, domain.AuthPrincipal{Account: "alice", Role: domain.RoleMember, ServerMode: true, Authenticated: true})
	alicePage, _ := srvMem.QueryRecordsPage(ctxAlice, domain.RecordQuery{Scope: "global", State: "active"})
	if len(alicePage.Records) != 1 || alicePage.Records[0].Account != "alice" {
		t.Fatalf("expected alice to receive imported global rule under her own account, got %+v", alicePage.Records)
	}
}

func TestDoctorCLIReportAndJSON(t *testing.T) {
	tmp := t.TempDir()
	dbPath := filepath.Join(tmp, "memories.db")
	t.Setenv("PRAXIS_DB_PATH", dbPath)
	t.Setenv("PRAXIS_MODELS_DIR", filepath.Join(tmp, "models"))
	t.Setenv("PRAXIS_DISABLE_BG_DASHBOARD", "1")

	// 1. Test doctor with --json flag
	var report DoctorReport
	code, out := captureStdout(t, func() int {
		return executeArgs([]string{"doctor", "--json"}, "1.2.0-test", RuntimeHooks{})
	})
	if code != 0 {
		t.Fatalf("expected doctor --json exit 0, got %d. Output: %s", code, out)
	}
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatalf("unmarshal doctor --json output failed: %v\nOutput: %s", err, out)
	}
	if report.Version != "1.2.0-test" {
		t.Errorf("expected version 1.2.0-test, got %q", report.Version)
	}
	if len(report.Checks) == 0 {
		t.Errorf("expected doctor checks to be non-empty")
	}

	categories := make(map[string]bool)
	for _, c := range report.Checks {
		categories[c.Category] = true
	}
	for _, wantCat := range []string{"binary", "daemon", "storage", "git_scope", "inference", "remotes"} {
		if !categories[wantCat] {
			t.Errorf("expected category %q in doctor report checks, got %v", wantCat, categories)
		}
	}

	// 2. Test daemon doctor human-readable output
	codeDaemon, outDaemon := captureStdout(t, func() int {
		return executeArgs([]string{"daemon", "doctor"}, "1.2.0-test", RuntimeHooks{})
	})
	if codeDaemon != 0 {
		t.Fatalf("expected daemon doctor exit 0, got %d. Output: %s", codeDaemon, outDaemon)
	}
	if !strings.Contains(outDaemon, "[praxis doctor]") {
		t.Errorf("expected daemon doctor output to contain header, got: %s", outDaemon)
	}
	if !strings.Contains(outDaemon, "v1.2.0-test") {
		t.Errorf("expected daemon doctor output to contain version, got: %s", outDaemon)
	}
	if codeDaemonJSON, outDaemonJSON := captureStdout(t, func() int {
		return executeArgs([]string{"daemon", "doctor", "--json"}, "1.2.0-test", RuntimeHooks{})
	}); codeDaemonJSON != 0 || !strings.Contains(outDaemonJSON, `"version":"1.2.0-test"`) {
		t.Fatalf("expected daemon doctor --json to forward --json flag, code=%d out=%s", codeDaemonJSON, outDaemonJSON)
	}
}
