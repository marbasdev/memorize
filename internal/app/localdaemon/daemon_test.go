package localdaemon

import (
	"bufio"
	"bytes"
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

	"memorize/internal/modules/memory"
	thttp "memorize/internal/transport/http"
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
	repoRoot, err := filepath.Abs(filepath.Join("..", "..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	tmp := t.TempDir()
	binPath := filepath.Join(tmp, "memorize")
	if runtime.GOOS == "windows" {
		binPath += ".exe"
	}
	buildCmd := exec.Command("go", "build", "-ldflags=-X main.version=1.5.0-daemon-test", "-o", binPath, "./cmd/memorize")
	buildCmd.Dir = repoRoot
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

	// 1. Initially daemon status reports stopped without spawning anything.
	stCmd := exec.Command(binPath, "-addr", addr, "daemon", "status")
	stCmd.Env = baseEnv
	stOut, err := stCmd.CombinedOutput()
	if err != nil || !strings.Contains(string(stOut), "stopped") {
		t.Fatalf("expected initial daemon status stopped, got: %q (%v)", string(stOut), err)
	}

	// 2. Open an MCP stdio bridge (zero-touch: daemon auto-spawns on first RPC).
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
	st1, err := thttp.QueryDaemonStatus(addr)
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

	stAfterHook, err := thttp.QueryDaemonStatus(addr)
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
	if stCSRF, err := thttp.QueryDaemonStatus(addr); err != nil || !stCSRF.Running || stCSRF.PID != st1.PID {
		t.Fatalf("expected daemon PID %d to stay alive after blocked CSRF attempts, got %+v (%v)", st1.PID, stCSRF, err)
	}

	// 4. Restart the daemon while the stdio bridge remains open -> bridge survives transparently.
	time.Sleep(1100 * time.Millisecond)
	restartCmd := exec.Command(binPath, "daemon", "restart")
	restartCmd.Env = baseEnv
	if out, err := restartCmd.CombinedOutput(); err != nil {
		t.Fatalf("daemon restart CLI failed: %v\n%s", err, string(out))
	}

	promptLine := sendAndRead(`{"jsonrpc":"2.0","id":3,"method":"prompts/get","params":{"name":"memory-curator"}}`)
	if !strings.Contains(promptLine, "Golden Filter") {
		t.Fatalf("expected open stdio bridge to survive daemon restart on prompts/get, got: %s", promptLine)
	}

	toolLine := sendAndRead(`{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"manage_memory","arguments":{"action":"purge"}}}`)
	if !strings.Contains(toolLine, "scope=project:demo-workspace") {
		t.Fatalf("expected open stdio bridge to preserve workspace root across daemon restart, got: %s", toolLine)
	}

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
	if st, err := thttp.QueryDaemonStatus(addr); err == nil && st.Running {
		t.Fatalf("expected daemon to be stopped, still got running: %+v", st)
	}
}
