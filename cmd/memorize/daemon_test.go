package main

import (
	"bufio"
	"bytes"
	"context"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"memorize/internal/memory"
)

func freeLoopbackAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("allocate free port: %v", err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	return addr
}

func TestUnifiedDaemonAutoSpawnHotRestartAndStop(t *testing.T) {
	tmp := t.TempDir()
	binPath := filepath.Join(tmp, "memorize")
	if runtime.GOOS == "windows" {
		binPath += ".exe"
	}
	buildCmd := exec.Command("go", "build", "-ldflags=-X main.version=1.5.0-daemon-test", "-o", binPath, ".")
	if out, err := buildCmd.CombinedOutput(); err != nil {
		t.Fatalf("build memorize failed: %v\n%s", err, string(out))
	}

	addr := freeLoopbackAddr(t)
	dbPath := filepath.Join(tmp, "data", "memories.db")
	modelsDir := filepath.Join(tmp, "data", "models")
	logPath := filepath.Join(tmp, "data", "daemon.log")

	baseEnv := append(os.Environ(),
		"MEMORY_DAEMON_ADDR="+addr,
		"MEMORY_DB_PATH="+dbPath,
		"MEMORY_MODELS_DIR="+modelsDir,
		"MEMORY_DAEMON_LOG="+logPath,
		"MEMORY_DAEMON_BIN="+binPath,
		"MEMORY_DISABLE_BG_DASHBOARD=",
		"MEMORY_INLINE_STDIO=",
	)

	t.Cleanup(func() {
		stopCmd := exec.Command(binPath, "daemon", "stop")
		stopCmd.Env = baseEnv
		_ = stopCmd.Run()
	})

	// 1. Initially daemon status should report stopped without spawning anything.
	stCmd := exec.Command(binPath, "-addr", addr, "daemon", "status")
	stCmd.Env = baseEnv
	stOut, err := stCmd.CombinedOutput()
	if err != nil || !strings.Contains(string(stOut), "stopped") {
		t.Fatalf("expected initial daemon status stopped, got: %q (%v)", string(stOut), err)
	}

	// 2. Open an MCP stdio bridge (zero-touch: daemon is NOT running yet; first RPC auto-spawns it).
	bridgeCmd := exec.Command(binPath)
	bridgeCmd.Env = baseEnv
	stdin, err := bridgeCmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := bridgeCmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := bridgeCmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = stdin.Close()
		_ = bridgeCmd.Wait()
	}()

	reader := bufio.NewReader(stdout)
	sendAndRead := func(msg string) string {
		t.Helper()
		if _, err := stdin.Write([]byte(msg + "\n")); err != nil {
			t.Fatalf("write stdin: %v", err)
		}
		line, err := reader.ReadString('\n')
		if err != nil {
			t.Fatalf("read stdout line: %v", err)
		}
		return line
	}

	initResp := sendAndRead(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{"roots":{"listChanged":true}},"clientInfo":{"name":"ide-test","version":"1.0"}}}`)
	if !strings.Contains(initResp, memory.PlaybookURI) {
		t.Fatalf("expected initialize via auto-spawned daemon to contain %s, got: %s", memory.PlaybookURI, initResp)
	}

	// Send notifications/initialized -> bridge asks client for roots/list -> reply with workspace root.
	_, _ = stdin.Write([]byte(`{"jsonrpc":"2.0","method":"notifications/initialized"}` + "\n"))
	rootsReq, err := reader.ReadString('\n')
	if err != nil {
		t.Fatalf("read roots/list inquiry: %v", err)
	}
	if !strings.Contains(rootsReq, `"roots/list"`) {
		t.Fatalf("expected roots/list inquiry from bridge, got: %s", rootsReq)
	}
	_, _ = stdin.Write([]byte(`{"jsonrpc":"2.0","id":"memorize-bridge-roots","result":{"roots":[{"uri":"file:///tmp/demo-workspace"}]}}` + "\n"))

	resLine := sendAndRead(`{"jsonrpc":"2.0","id":2,"method":"resources/read","params":{"uri":"memorize://playbook/memory-curator"}}`)
	if !strings.Contains(resLine, "Golden Filter") {
		t.Fatalf("expected resources/read via daemon bridge to succeed, got: %s", resLine)
	}

	// 3. Verify single daemon status, Dashboard UI controls, Hook delegation, and Streamable HTTP /mcp.
	st1, err := queryDaemonStatus(addr)
	if err != nil || !st1.Running || st1.PID <= 0 {
		t.Fatalf("expected running daemon status on %s, got st=%+v err=%v", addr, st1, err)
	}
	if st1.Version != "1.5.0-daemon-test" {
		t.Fatalf("unexpected daemon version: %s", st1.Version)
	}

	uiResp, err := http.Get("http://" + addr + "/")
	if err != nil {
		t.Fatalf("GET dashboard root failed: %v", err)
	}
	uiBytes, _ := io.ReadAll(uiResp.Body)
	_ = uiResp.Body.Close()
	if !strings.Contains(string(uiBytes), "daemonBadge") || strings.Contains(string(uiBytes), "btnRestartDaemon") || strings.Contains(string(uiBytes), "btnRefresh") {
		t.Fatalf("expected dashboard HTML to include daemonBadge without manual restart/refresh header buttons")
	}

	hookCmd := exec.Command(binPath, "-hook", "antigravity-start")
	hookCmd.Env = baseEnv
	hookCmd.Stdin = strings.NewReader(`{"cwd":"/tmp/demo-workspace"}`)
	if out, err := hookCmd.CombinedOutput(); err != nil {
		t.Fatalf("hook via daemon failed: %v (%s)", err, string(out))
	}

	stAfterHook, err := queryDaemonStatus(addr)
	if err != nil || stAfterHook.PID != st1.PID {
		t.Fatalf("expected hook to reuse exact same daemon PID %d, got %+v (%v)", st1.PID, stAfterHook, err)
	}

	mcpReq, _ := http.NewRequest(http.MethodPost, "http://"+addr+"/mcp", bytes.NewReader([]byte(`{"jsonrpc":"2.0","id":99,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{},"clientInfo":{"name":"http-client","version":"1.0"}}}`)))
	mcpReq.Header.Set("Content-Type", "application/json")
	mcpResp, err := http.DefaultClient.Do(mcpReq)
	if err != nil {
		t.Fatalf("POST /mcp failed: %v", err)
	}
	mcpBody, _ := io.ReadAll(mcpResp.Body)
	_ = mcpResp.Body.Close()
	if mcpResp.StatusCode != http.StatusOK || !strings.Contains(string(mcpBody), memory.PlaybookURI) {
		t.Fatalf("unexpected Streamable HTTP /mcp response (%d): %s", mcpResp.StatusCode, string(mcpBody))
	}

	// 3b. Verify browser CSRF defenses reject Simple-POST form submits and cross-site Sec-Fetch-Site requests.
	formCSRF, err := http.Post("http://"+addr+"/api/daemon/stop", "application/x-www-form-urlencoded", strings.NewReader("stop=1"))
	if err != nil {
		t.Fatalf("Simple-POST CSRF check failed: %v", err)
	}
	_ = formCSRF.Body.Close()
	if formCSRF.StatusCode != http.StatusForbidden {
		t.Fatalf("expected Simple-POST /api/daemon/stop to be rejected with 403, got %d", formCSRF.StatusCode)
	}

	crossReq, _ := http.NewRequest(http.MethodPost, "http://"+addr+"/api/daemon/stop", bytes.NewReader([]byte(`{}`)))
	crossReq.Header.Set("Content-Type", "application/json")
	crossReq.Header.Set("Sec-Fetch-Site", "cross-site")
	crossResp, err := http.DefaultClient.Do(crossReq)
	if err != nil {
		t.Fatalf("Sec-Fetch-Site CSRF check failed: %v", err)
	}
	_ = crossResp.Body.Close()
	if crossResp.StatusCode != http.StatusForbidden {
		t.Fatalf("expected cross-site /api/daemon/stop to be rejected with 403, got %d", crossResp.StatusCode)
	}
	if stCSRF, err := queryDaemonStatus(addr); err != nil || !stCSRF.Running || stCSRF.PID != st1.PID {
		t.Fatalf("expected daemon PID %d to stay alive after blocked CSRF attempts, got %+v (%v)", st1.PID, stCSRF, err)
	}

	// 4. Restart the daemon while the stdio bridge remains open -> bridge must survive seamlessly!
	time.Sleep(1100 * time.Millisecond) // ensure uptime/started_at advances visibly before restart
	restartCmd := exec.Command(binPath, "daemon", "restart")
	restartCmd.Env = baseEnv
	if out, err := restartCmd.CombinedOutput(); err != nil {
		t.Fatalf("daemon restart CLI failed: %v\n%s", err, string(out))
	}

	promptLine := sendAndRead(`{"jsonrpc":"2.0","id":3,"method":"prompts/get","params":{"name":"memory-curator"}}`)
	if !strings.Contains(promptLine, "Golden Filter") {
		t.Fatalf("expected open stdio bridge to survive daemon restart on prompts/get, got: %s", promptLine)
	}

	// Also verify tool call after restart uses the cached workspace root ("project:demo-workspace") without re-initializing!
	toolLine := sendAndRead(`{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"manage_memory","arguments":{"action":"purge"}}}`)
	if !strings.Contains(toolLine, "scope=project:demo-workspace") {
		t.Fatalf("expected open stdio bridge to preserve workspace root across daemon restart, got: %s", toolLine)
	}

	// Verify cold-start onboarding skip/reset actions inside manage_memory (zero extra MCP tool bloat)!
	skipLine := sendAndRead(`{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{"name":"manage_memory","arguments":{"action":"skip_onboarding"}}}`)
	if !strings.Contains(skipLine, "ONBOARDING_SKIPPED (scope=project:demo-workspace)") {
		t.Fatalf("expected manage_memory skip_onboarding to succeed, got: %s", skipLine)
	}
	resetLine := sendAndRead(`{"jsonrpc":"2.0","id":6,"method":"tools/call","params":{"name":"manage_memory","arguments":{"action":"reset_onboarding"}}}`)
	if !strings.Contains(resetLine, "ONBOARDING_RESET (scope=project:demo-workspace, status=pending)") {
		t.Fatalf("expected manage_memory reset_onboarding to succeed, got: %s", resetLine)
	}

	// 5. Stop the daemon via CLI and verify clean shutdown.
	_ = stdin.Close()
	stopCmd := exec.Command(binPath, "daemon", "stop")
	stopCmd.Env = baseEnv
	stopOut, err := stopCmd.CombinedOutput()
	if err != nil || !strings.Contains(string(stopOut), "stopped") {
		t.Fatalf("daemon stop failed: %v (%s)", err, string(stopOut))
	}
	if st, err := queryDaemonStatus(addr); err == nil && st.Running {
		t.Fatalf("expected daemon to be stopped, still got running: %+v", st)
	}
}

func TestResolveHookTimeoutFromManifestAndDynamicBudgets(t *testing.T) {
	tmp := t.TempDir()
	hooksJSON := `{
		"memorize-enforcer": {
			"SessionStart": [{"type": "command", "command": "sh ./scripts/run-memorize.sh -hook antigravity-start", "timeout": 12}],
			"PreToolUse": [{"matcher": "Bash", "hooks": [{"type": "command", "command": "sh ./scripts/run-memorize.sh -hook antigravity-pretool", "timeout": 15}]}]
		}
	}`
	if err := os.WriteFile(filepath.Join(tmp, "hooks.json"), []byte(hooksJSON), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MEMORY_PLUGIN_DIR", tmp)
	t.Setenv("MEMORY_HOOK_TIMEOUT", "")
	_ = os.Unsetenv("MEMORY_HOOK_TIMEOUT")

	// 1. Dynamically reads timeout from hooks.json without hardcoded constants
	if d, ok := resolveConfiguredHookTimeout("antigravity-pretool"); !ok || d != 15*time.Second {
		t.Fatalf("expected 15s from hooks.json for antigravity-pretool, got d=%v ok=%v", d, ok)
	}
	if d, ok := resolveConfiguredHookTimeout("antigravity-start"); !ok || d != 12*time.Second {
		t.Fatalf("expected 12s from hooks.json for antigravity-start, got d=%v ok=%v", d, ok)
	}

	// 2. Unlisted hook (e.g. git-precommit) has no artificial timeout cap
	if d, ok := resolveConfiguredHookTimeout("git-precommit"); ok || d != 0 {
		t.Fatalf("expected no cap for git-precommit, got d=%v ok=%v", d, ok)
	}

	// 3. Explicit MEMORY_HOOK_TIMEOUT overrides manifest (and "0" disables cap)
	t.Setenv("MEMORY_HOOK_TIMEOUT", "4500ms")
	if d, ok := resolveConfiguredHookTimeout("antigravity-pretool"); !ok || d != 4500*time.Millisecond {
		t.Fatalf("expected 4500ms from MEMORY_HOOK_TIMEOUT, got d=%v ok=%v", d, ok)
	}
	t.Setenv("MEMORY_HOOK_TIMEOUT", "0")
	if _, ok := resolveConfiguredHookTimeout("antigravity-pretool"); ok {
		t.Fatalf("expected MEMORY_HOOK_TIMEOUT=0 to disable deadline")
	}

	// 4. Remaining budgets subtract elapsed startup time and leave exit margin (< clientBudget < remaining)
	procStart := time.Now().Add(-500 * time.Millisecond)
	srvB, cliB, alive := remainingHookBudgets(procStart, 8*time.Second)
	if !alive || srvB <= 0 || cliB <= srvB || cliB >= 7600*time.Millisecond {
		t.Fatalf("unexpected budgets: srv=%v cli=%v alive=%v", srvB, cliB, alive)
	}

	// 5. Nearly exhausted deadline fails open instead of risking SIGKILL
	exhaustedStart := time.Now().Add(-7950 * time.Millisecond)
	if _, _, alive := remainingHookBudgets(exhaustedStart, 8*time.Second); alive {
		t.Fatalf("expected nearly exhausted deadline to report !alive for fail-open exit")
	}
}

func TestTouchMCPLiveSessionDoesNotCrossTalk(t *testing.T) {
	svc, err := memory.NewService(":memory:", nil)
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	defer svc.Close()

	ctx := context.Background()
	repo := t.TempDir()
	scope := "project:" + strings.ToLower(filepath.Base(repo))
	sidA := "conv-mcp-isolated-a"

	// Session A starts via hook and finishes cleanly via Stop.
	_, _, _ = svc.ExecuteHook(ctx, "antigravity-turn", []byte(`{"conversationId":"`+sidA+`","workspacePaths":[`+`"`+repo+`"`+`],"lastUserInput":"작업 1"}`), "")
	_, _, _ = svc.ExecuteHook(ctx, "antigravity-stop", []byte(`{"conversationId":"`+sidA+`","workspacePaths":[`+`"`+repo+`"`+`],"fullyIdle":true}`), "")

	// Generic bridge MCP call without explicit session_id must NOT guess turn_state or resurrect finished Session A.
	bridgeCtx := memory.WithSessionID(ctx, "bridge-999")
	touchMCPLiveSession(bridgeCtx, svc, scope, "", "commit_memory")
	if live := svc.ListLiveSessions(); len(live) != 0 {
		t.Fatalf("expected generic bridge MCP call not to resurrect finished sidA, got %+v", live)
	}

	// Explicit non-generic session_id updates live session status cleanly without polluting Progress DB.
	touchMCPLiveSession(bridgeCtx, svc, scope, "conv-explicit-mcp", "query_project_memory")
	live := svc.ListLiveSessions()
	if len(live) != 1 || live[0].SessionID != "conv-explicit-mcp" || live[0].Tool != "query_project_memory" {
		t.Fatalf("expected explicit session_id MCP call in ListLiveSessions, got %+v", live)
	}
	ov, err := svc.GetProgressOverviewWithQuery(ctx, memory.ProgressQuery{Scope: scope})
	if err != nil {
		t.Fatal(err)
	}
	if ov.TotalCount != 0 {
		t.Fatalf("expected live MCP session not to pollute persistent Progress DB, got total=%d", ov.TotalCount)
	}
}
