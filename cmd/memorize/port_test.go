package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"memorize/internal/memory"
)

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
	tmp := t.TempDir()
	binName := "memorize-port-test"
	if runtime.GOOS == "windows" {
		binName += ".exe"
	}
	binPath := filepath.Join(tmp, binName)
	buildCmd := exec.Command("go", "build", "-ldflags", "-X main.version=1.5.0-port-test", "-o", binPath, ".")
	if out, err := buildCmd.CombinedOutput(); err != nil {
		t.Fatalf("failed to build test binary: %v\n%s", err, string(out))
	}

	dataDir := filepath.Join(tmp, "data")
	dbPath := filepath.Join(dataDir, "memories.db")
	modelsDir := filepath.Join(dataDir, "models")
	if err := os.MkdirAll(modelsDir, 0o700); err != nil {
		t.Fatal(err)
	}

	// Intentionally strip MEMORY_DAEMON_ADDR / MEMORY_DASHBOARD_ADDR / MEMORY_DB_PATH so discovery uses ports.json + daemon.json.
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

	// Ensure cleanup even on assertion failure.
	defer func() {
		_ = occupantLn.Close()
		stopCmd := exec.Command(binPath, "daemon", "stop")
		stopCmd.Env = baseEnv
		_, _ = stopCmd.CombinedOutput()
	}()

	// 1. Offline CLI: reject invalid/equal ports and persist valid ports.json.
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
	if loaded := memory.LoadPortConfig(dbPath); loaded.DaemonPort != busyPort || loaded.GGUFPort != ggufPort1 {
		t.Fatalf("expected ports.json (%d, %d), got %+v", busyPort, ggufPort1, loaded)
	}

	// 2. Start daemon while busyPort is occupied -> auto-fallback must bind a free port and publish daemon.json.
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
	rt, ok := memory.ReadDaemonState(dbPath)
	if !ok || rt.ListenAddr != fbAddr || rt.PID <= 0 {
		t.Fatalf("expected daemon.json to record fallback addr %q, got ok=%v rt=%+v", fbAddr, ok, rt)
	}

	// 3. CLI status & Hook invocation auto-discover the fallback port without MEMORY_DAEMON_ADDR.
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

	// 4. Open MCP stdio bridge onto the fallback daemon, then change ports live via CLI.
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

	// Switch daemon + GGUF ports cleanly via CLI while bridge remains connected.
	_ = occupantLn.Close()
	nextDaemonPort, nextGGUFPort := freeTwoDistinctLoopbackPorts(t)

	changeCmd := exec.Command(binPath, "daemon", "port", "--json", strconv.Itoa(nextDaemonPort), strconv.Itoa(nextGGUFPort))
	changeCmd.Env = baseEnv
	changeOut, err := changeCmd.CombinedOutput()
	if err != nil {
		t.Fatalf("live daemon port change failed: %v\n%s", err, string(changeOut))
	}
	var changeRes portApplyResponse
	if err := json.Unmarshal(changeOut, &changeRes); err != nil {
		t.Fatalf("decode change response: %v\n%s", err, string(changeOut))
	}
	if changeRes.ActiveDaemon != nextDaemonPort || changeRes.ActiveGGUF != nextGGUFPort {
		t.Fatalf("unexpected live port change result: %+v", changeRes)
	}

	nextAddr := fmt.Sprintf("127.0.0.1:%d", nextDaemonPort)
	stNew, err := queryDaemonStatus(nextAddr)
	if err != nil || !stNew.Running || stNew.ConfiguredDaemonPort != nextDaemonPort || stNew.ConfiguredGGUFPort != nextGGUFPort {
		t.Fatalf("expected healthy daemon on new port %s with updated config, got st=%+v err=%v", nextAddr, stNew, err)
	}

	// Open stdio bridge must transparently follow daemon.json onto the new port!
	promptLine := sendAndRead(`{"jsonrpc":"2.0","id":2,"method":"prompts/get","params":{"name":"memory-curator"}}`)
	if !strings.Contains(promptLine, "Golden Filter") {
		t.Fatalf("expected open stdio bridge to survive live daemon port change, got: %s", promptLine)
	}

	// 5. Switch port via Web Console POST /ui/ports/save (verifying CSRF + memorizePortRedirect header).
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
	if st3, ok := waitDaemonPortHealthy(uiAddr3, nextAddr, dbPath, 6*time.Second); !ok || !st3.Running {
		t.Fatalf("expected daemon to restart onto Web UI port %s cleanly", uiAddr3)
	}

	// 6. Stop daemon automatically via discovery and verify daemon.json removed.
	_ = stdin.Close()
	stopCmd := exec.Command(binPath, "daemon", "stop")
	stopCmd.Env = baseEnv
	stopOut, err := stopCmd.CombinedOutput()
	if err != nil || !strings.Contains(string(stopOut), "stopped") {
		t.Fatalf("daemon stop after port moves failed: %v\n%s", err, string(stopOut))
	}
	if _, stillExists := memory.ReadDaemonState(dbPath); stillExists {
		t.Fatalf("expected daemon.json to be cleaned up on daemon stop")
	}
}
