package localdaemon

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"memorize/internal/domain"
	"memorize/internal/infra/config"
	"memorize/internal/modules/guardrail"
	"memorize/internal/modules/memory"
	"memorize/internal/modules/progress"
	"memorize/internal/transport/dashboard"
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

	stCmd := exec.Command(binPath, "-addr", addr, "daemon", "status")
	stCmd.Env = baseEnv
	stOut, err := stCmd.CombinedOutput()
	if err != nil || !strings.Contains(string(stOut), "stopped") {
		t.Fatalf("expected initial daemon status stopped, got: %q (%v)", string(stOut), err)
	}

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

func TestWatchedBinaryCandidatesAndDevHotReloadGate(t *testing.T) {
	t.Setenv("MEMORY_DAEMON_BIN", "")
	t.Setenv("MEMORIZE_DEV_HOTRELOAD", "")

	repoRoot, err := filepath.Abs(filepath.Join("..", "..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	binName := "memorize"
	if runtime.GOOS == "windows" {
		binName = "memorize.exe"
	}
	repoBin := filepath.Join(repoRoot, "bin", binName)

	for _, cand := range WatchedBinaryCandidates() {
		if config.SamePathClean(cand, repoBin) {
			t.Fatalf("did not expect repo binary %s in WatchedBinaryCandidates when MEMORIZE_DEV_HOTRELOAD is unset; got %v", repoBin, WatchedBinaryCandidates())
		}
	}

	t.Setenv("MEMORIZE_DEV_HOTRELOAD", "1")
	foundRepoBin := false
	for _, cand := range WatchedBinaryCandidates() {
		if config.SamePathClean(cand, repoBin) {
			foundRepoBin = true
			break
		}
	}
	if !foundRepoBin {
		t.Fatalf("expected %s in WatchedBinaryCandidates when MEMORIZE_DEV_HOTRELOAD=1; got %v", repoBin, WatchedBinaryCandidates())
	}

	custom := filepath.Join(t.TempDir(), "custom-memorize")
	t.Setenv("MEMORY_DAEMON_BIN", custom)
	cands := WatchedBinaryCandidates()
	if len(cands) == 0 || !strings.HasSuffix(cands[0], "custom-memorize") {
		t.Fatalf("expected MEMORY_DAEMON_BIN override, got %v", cands)
	}
}

func TestDaemonLogPathAndStartBackgroundServerHandover(t *testing.T) {
	tmp := t.TempDir()
	customDB := filepath.Join(tmp, "memories.db")
	t.Setenv("MEMORY_DAEMON_LOG", "")
	if got := config.DaemonLogPath(customDB); got != filepath.Join(tmp, "daemon.log") {
		t.Fatalf("DaemonLogPath = %q, want %q", got, filepath.Join(tmp, "daemon.log"))
	}

	mem, err := memory.Open(":memory:", nil)
	if err != nil {
		t.Fatalf("memory.Open: %v", err)
	}
	defer mem.Close()
	prog := progress.New(mem)
	gr := guardrail.New(mem, prog)

	deps := dashboard.Deps{
		Memory:    mem,
		Progress:  prog,
		Guardrail: gr,
	}

	lnProbe, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := lnProbe.Addr().String()
	_ = lnProbe.Close()

	bound1, close1, err := StartBackgroundServer(addr, deps, customDB)
	if err != nil || bound1 == "" || close1 == nil {
		t.Fatalf("StartBackgroundServer #1 failed: bound=%q err=%v", bound1, err)
	}
	defer func() { _ = close1() }()

	resp, err := http.Get("http://" + bound1 + "/api/dashboard/meta")
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 on /api/dashboard/meta, got resp=%v err=%v", resp, err)
	}
	_ = resp.Body.Close()

	ln2, err := ListenWithHandover(bound1, false)
	if err != nil {
		t.Fatalf("ListenWithHandover takeover failed: %v", err)
	}
	_ = ln2.Close()

	time.Sleep(20 * time.Millisecond)
	_ = os.RemoveAll(tmp)
}

func freeTwoDistinctLoopbackPorts(t *testing.T) (int, int) {
	t.Helper()
	ln1, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("allocate free port 1: %v", err)
	}
	p1 := ln1.Addr().(*net.TCPAddr).Port
	var ln2 net.Listener
	var p2 int
	for i := 0; i < 10; i++ {
		ln2, err = net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			_ = ln1.Close()
			t.Fatalf("allocate free port 2: %v", err)
		}
		p2 = ln2.Addr().(*net.TCPAddr).Port
		if p2 != p1 && p2 != p1+1 && p2 != p1-1 {
			break
		}
		_ = ln2.Close()
		ln2 = nil
	}
	_ = ln1.Close()
	if ln2 == nil {
		t.Fatalf("failed to allocate distinct loopback ports")
	}
	_ = ln2.Close()
	return p1, p2
}

func envWithoutKeys(base []string, drop ...string) []string {
	skip := make(map[string]bool, len(drop))
	for _, k := range drop {
		skip[k] = true
	}
	out := make([]string, 0, len(base))
	for _, item := range base {
		k := item
		if idx := strings.IndexByte(item, '='); idx >= 0 {
			k = item[:idx]
		}
		if skip[k] {
			continue
		}
		out = append(out, item)
	}
	return out
}

func TestDaemonPortChangeFallbackAndAutoDiscovery(t *testing.T) {
	repoRoot, err := filepath.Abs(filepath.Join("..", "..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	tmp := t.TempDir()
	binName := "memorize-port-test"
	if runtime.GOOS == "windows" {
		binName += ".exe"
	}
	binPath := filepath.Join(tmp, binName)
	buildCmd := exec.Command("go", "build", "-ldflags", "-X main.version=1.5.0-port-test", "-o", binPath, "./cmd/memorize")
	buildCmd.Dir = repoRoot
	if out, err := buildCmd.CombinedOutput(); err != nil {
		t.Fatalf("failed to build test binary: %v\n%s", err, string(out))
	}

	dataDir := filepath.Join(tmp, "data")
	dbPath := filepath.Join(dataDir, "memories.db")
	modelsDir := filepath.Join(dataDir, "models")
	if err := os.MkdirAll(modelsDir, 0o700); err != nil {
		t.Fatal(err)
	}

	baseEnv := envWithoutKeys(os.Environ(),
		"MEMORY_DAEMON_ADDR", "MEMORY_DASHBOARD_ADDR",
		"MEMORY_DB_PATH", "MEMORY_MODELS_DIR", "MEMORY_DATA_DIR", "MEMORY_INLINE",
	)
	baseEnv = append(baseEnv,
		"MEMORY_DATA_DIR="+dataDir,
		"MEMORY_DB_PATH="+dbPath,
		"MEMORY_MODELS_DIR="+modelsDir,
	)

	occupantLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen occupant: %v", err)
	}
	busyPort := occupantLn.Addr().(*net.TCPAddr).Port
	ggufPort1 := 19000 + (busyPort % 1000)
	if ggufPort1 == busyPort || ggufPort1 == busyPort+1 {
		ggufPort1 += 2
	}

	defer func() {
		_ = occupantLn.Close()
		stopCmd := exec.Command(binPath, "daemon", "stop")
		stopCmd.Env = baseEnv
		_, _ = stopCmd.CombinedOutput()
	}()

	badCmd := exec.Command(binPath, "daemon", "port", strconv.Itoa(busyPort), strconv.Itoa(busyPort))
	badCmd.Env = baseEnv
	if out, err := badCmd.CombinedOutput(); err == nil {
		t.Fatalf("expected equal ports to fail, got ok: %s", string(out))
	}

	setOffline := exec.Command(binPath, "daemon", "port", "--json", strconv.Itoa(busyPort), strconv.Itoa(ggufPort1))
	setOffline.Env = baseEnv
	if out, err := setOffline.CombinedOutput(); err != nil {
		t.Fatalf("offline daemon port set failed: %v\n%s", err, string(out))
	}
	if loaded := config.LoadPortConfig(dbPath); loaded.DaemonPort != busyPort || loaded.GGUFPort != ggufPort1 {
		t.Fatalf("expected ports.json (%d, %d), got %+v", busyPort, ggufPort1, loaded)
	}

	startCmd := exec.Command(binPath, "daemon", "start")
	startCmd.Env = baseEnv
	startOut, err := startCmd.CombinedOutput()
	if err != nil {
		t.Fatalf("daemon start with occupied configured port failed: %v\n%s", err, string(startOut))
	}

	stMap := func() map[string]any {
		c := exec.Command(binPath, "daemon", "port", "--json")
		c.Env = baseEnv
		b, err := c.CombinedOutput()
		if err != nil {
			t.Fatalf("daemon port --json failed: %v\n%s", err, string(b))
		}
		var m map[string]any
		if err := json.Unmarshal(b, &m); err != nil {
			t.Fatalf("decode daemon port --json: %v\n%s", err, string(b))
		}
		return m
	}()

	fbPort := int(stMap["active_daemon_port"].(float64))
	fbAddr, _ := stMap["daemon_addr"].(string)
	if fbPort <= 0 || fbPort == busyPort || !stMap["daemon_fallback"].(bool) {
		t.Fatalf("expected fallback active daemon port != %d with daemon_fallback=true, got %+v", busyPort, stMap)
	}
	rt, ok := config.ReadDaemonState(dbPath)
	if !ok || rt.ListenAddr != fbAddr || rt.PID <= 0 {
		t.Fatalf("expected daemon.json to record fallback addr %q, got ok=%v rt=%+v", fbAddr, ok, rt)
	}

	statusCmd := exec.Command(binPath, "daemon", "status")
	statusCmd.Env = baseEnv
	statusOut, err := statusCmd.CombinedOutput()
	if err != nil || !strings.Contains(string(statusOut), fbAddr) {
		t.Fatalf("expected daemon status to discover %s: err=%v out=%s", fbAddr, err, string(statusOut))
	}

	hookCmd := exec.Command(binPath, "-hook", "antigravity-start")
	hookCmd.Env = baseEnv
	hookCmd.Stdin = strings.NewReader(`{"cwd":"/tmp/port-discovery-ws"}`)
	if out, err := hookCmd.CombinedOutput(); err != nil {
		t.Fatalf("hook via discovered fallback daemon failed: %v\n%s", err, string(out))
	}

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
		t.Fatalf("start stdio bridge: %v", err)
	}
	defer func() {
		_ = stdin.Close()
		if bridgeCmd.Process != nil {
			_ = bridgeCmd.Process.Kill()
			_, _ = bridgeCmd.Process.Wait()
		}
	}()

	reader := bufio.NewReader(stdout)
	readLine := func(label string) string {
		ch := make(chan string, 1)
		go func() {
			s, _ := reader.ReadString('\n')
			ch <- s
		}()
		select {
		case got := <-ch:
			return got
		case <-time.After(8 * time.Second):
			t.Fatalf("timed out waiting for bridge output (%s)", label)
			return ""
		}
	}
	sendAndRead := func(line string) string {
		if _, err := stdin.Write([]byte(line + "\n")); err != nil {
			t.Fatalf("write bridge stdin: %v", err)
		}
		return readLine(line)
	}

	initLine := sendAndRead(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{"roots":{"listChanged":true}},"clientInfo":{"name":"port-test","version":"1.0"}}}`)
	if !strings.Contains(initLine, `"serverInfo"`) {
		t.Fatalf("unexpected MCP initialize reply: %s", initLine)
	}
	_, _ = stdin.Write([]byte(`{"jsonrpc":"2.0","method":"notifications/initialized"}` + "\n"))
	rootsReq := readLine("roots/list")
	if !strings.Contains(rootsReq, `"roots/list"`) {
		t.Fatalf("expected roots/list request after notifications/initialized, got: %s", rootsReq)
	}
	_, _ = stdin.Write([]byte(`{"jsonrpc":"2.0","id":"memorize-bridge-roots","result":{"roots":[{"uri":"file:///tmp/port-discovery-ws"}]}}` + "\n"))

	_ = occupantLn.Close()
	nextDaemonPort, nextGGUFPort := freeTwoDistinctLoopbackPorts(t)

	changeCmd := exec.Command(binPath, "daemon", "port", "--json", strconv.Itoa(nextDaemonPort), strconv.Itoa(nextGGUFPort))
	changeCmd.Env = baseEnv
	changeOut, err := changeCmd.CombinedOutput()
	if err != nil {
		t.Fatalf("live daemon port change failed: %v\n%s", err, string(changeOut))
	}
	var changeRes thttp.PortApplyResponse
	if err := json.Unmarshal(changeOut, &changeRes); err != nil {
		t.Fatalf("decode change response: %v\n%s", err, string(changeOut))
	}
	if changeRes.ActiveDaemon != nextDaemonPort || changeRes.ActiveGGUF != nextGGUFPort {
		t.Fatalf("unexpected live port change result: %+v", changeRes)
	}

	nextAddr := fmt.Sprintf("127.0.0.1:%d", nextDaemonPort)
	stNew, err := thttp.QueryDaemonStatus(nextAddr)
	if err != nil || !stNew.Running || stNew.ConfiguredDaemonPort != nextDaemonPort || stNew.ConfiguredGGUFPort != nextGGUFPort {
		t.Fatalf("expected healthy daemon on new port %s with updated config, got st=%+v err=%v", nextAddr, stNew, err)
	}

	promptLine := sendAndRead(`{"jsonrpc":"2.0","id":2,"method":"prompts/get","params":{"name":"memory-curator"}}`)
	if !strings.Contains(promptLine, "Golden Filter") {
		t.Fatalf("expected open stdio bridge to survive live daemon port change, got: %s", promptLine)
	}

	uiPort3, ggufPort3 := freeTwoDistinctLoopbackPorts(t)
	unauthForm := url.Values{"daemon_port": {strconv.Itoa(uiPort3)}, "gguf_port": {strconv.Itoa(ggufPort3)}}
	csrfResp, err := http.PostForm("http://"+nextAddr+"/ui/ports/save", unauthForm)
	if err != nil {
		t.Fatalf("unauth POST /ui/ports/save: %v", err)
	}
	_ = csrfResp.Body.Close()
	if csrfResp.StatusCode != http.StatusForbidden {
		t.Fatalf("expected non-HX /ui/ports/save to return 403 Forbidden, got %d", csrfResp.StatusCode)
	}

	hxReq, _ := http.NewRequest(http.MethodPost, "http://"+nextAddr+"/ui/ports/save", strings.NewReader(unauthForm.Encode()))
	hxReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	hxReq.Header.Set("HX-Request", "true")
	hxResp, err := http.DefaultClient.Do(hxReq)
	if err != nil {
		t.Fatalf("HX POST /ui/ports/save: %v", err)
	}
	_, _ = io.Copy(io.Discard, hxResp.Body)
	_ = hxResp.Body.Close()
	wantRedirect := "http://127.0.0.1:" + strconv.Itoa(uiPort3)
	if hxResp.StatusCode != http.StatusOK || !strings.Contains(hxResp.Header.Get("HX-Trigger"), wantRedirect) {
		t.Fatalf("expected 200 with memorizePortRedirect to %s, got code=%d trigger=%q", wantRedirect, hxResp.StatusCode, hxResp.Header.Get("HX-Trigger"))
	}

	uiAddr3 := fmt.Sprintf("127.0.0.1:%d", uiPort3)
	if st3, ok := thttp.WaitDaemonPortHealthy(uiAddr3, nextAddr, dbPath, 6*time.Second); !ok || !st3.Running {
		t.Fatalf("expected daemon to restart onto Web UI port %s cleanly", uiAddr3)
	}

	_ = stdin.Close()
	stopCmd := exec.Command(binPath, "daemon", "stop")
	stopCmd.Env = baseEnv
	stopOut, err := stopCmd.CombinedOutput()
	if err != nil || !strings.Contains(string(stopOut), "stopped") {
		t.Fatalf("daemon stop after port moves failed: %v\n%s", err, string(stopOut))
	}
	if _, stillExists := config.ReadDaemonState(dbPath); stillExists {
		t.Fatalf("expected daemon.json to be cleaned up on daemon stop")
	}
}

func TestEnsureRunningSkipsDuplicateDaemonWhenTeamServerActive(t *testing.T) {
	tmp := t.TempDir()
	dbPath := filepath.Join(tmp, "memories.db")
	modelsDir := filepath.Join(tmp, "models")

	authSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/daemon/status" {
			http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
			return
		}
		http.NotFound(w, r)
	}))
	defer authSrv.Close()

	listenAddr := strings.TrimPrefix(authSrv.URL, "http://")
	if err := config.WriteDaemonState(dbPath, domain.DaemonRuntimeState{
		PID:        99999,
		ListenAddr: listenAddr,
		ServerMode: true,
		DBPath:     dbPath,
	}); err != nil {
		t.Fatal(err)
	}

	st, ok := EnsureRunning(listenAddr, dbPath, modelsDir)
	if ok {
		t.Fatalf("expected EnsureRunning not to treat authenticated team server as unauthenticated solo daemon")
	}
	if st == nil || !st.ServerMode || !st.Running {
		t.Fatalf("expected authRequired team server status hint, got %+v", st)
	}
}
