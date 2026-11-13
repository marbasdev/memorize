package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"memorize/internal/memory"
	"memorize/internal/memory/dashboard"
	"memorize/internal/memory/localmodel"
)

type portApplyRequest struct {
	DaemonPort int  `json:"daemon_port"`
	GGUFPort   int  `json:"gguf_port"`
	Reset      bool `json:"reset,omitempty"`
}

type portApplyResponse struct {
	OK               bool   `json:"ok"`
	ConfiguredDaemon int    `json:"configured_daemon_port"`
	ActiveDaemon     int    `json:"active_daemon_port"`
	ListenAddr       string `json:"listen_addr"`
	DaemonFallback   bool   `json:"daemon_fallback"`
	Restarting       bool   `json:"restarting"`
	NewURL           string `json:"new_url"`
	ConfiguredGGUF   int    `json:"configured_gguf_port"`
	ActiveGGUF       int    `json:"active_gguf_port"`
	GGUFEndpoint     string `json:"gguf_endpoint"`
	GGUFFallback     bool   `json:"gguf_fallback"`
	GGUFRunning      bool   `json:"gguf_running"`
	Error            string `json:"error,omitempty"`
}

func explicitDaemonOverride(explicit string) string {
	for _, v := range []string{explicit, os.Getenv("MEMORY_DAEMON_ADDR"), os.Getenv("MEMORY_DASHBOARD_ADDR")} {
		if s := strings.TrimSpace(v); s != "" {
			return s
		}
	}
	return ""
}

func resolveDaemonTargetAddr(explicit, dbPath string) (string, bool) {
	if ov := explicitDaemonOverride(explicit); ov != "" {
		return ov, true
	}
	cfg := memory.LoadPortConfig(dbPath)
	return net.JoinHostPort(memory.DefaultDaemonHost, strconv.Itoa(cfg.DaemonPort)), false
}

func probeDashboardMetaAlive(addr string) bool {
	client := &http.Client{Timeout: 300 * time.Millisecond}
	resp, err := client.Get("http://" + addr + "/api/dashboard/meta")
	if err != nil {
		return false
	}
	_ = resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

func listenDaemonWithFallback(targetAddr string, excludePorts ...int) (net.Listener, error) {
	ln, err := dashboard.ListenWithHandover(targetAddr, false)
	if err == nil {
		return ln, nil
	}

	host, port, ok := memory.ParseHostPort(targetAddr)
	if !ok {
		return nil, err
	}
	excluded := make(map[int]bool, len(excludePorts))
	for _, ex := range excludePorts {
		if ex > 0 {
			excluded[ex] = true
		}
	}

	if port > 0 {
		for offset := 1; offset <= 32; offset++ {
			cand := port + offset
			if cand > 65535 {
				cand = 1024 + (cand - 65535)
			}
			if excluded[cand] {
				continue
			}
			candAddr := net.JoinHostPort(host, strconv.Itoa(cand))
			if fbLn, ferr := net.Listen("tcp", candAddr); ferr == nil {
				log.Printf("[memorize daemon] port %s busy (%v); auto-fallback bound to %s", targetAddr, err, fbLn.Addr().String())
				return fbLn, nil
			}
		}
	}

	for attempt := 0; attempt < 8; attempt++ {
		fbLn, ferr := net.Listen("tcp", net.JoinHostPort(host, "0"))
		if ferr != nil {
			continue
		}
		if tcpAddr, ok := fbLn.Addr().(*net.TCPAddr); ok && excluded[tcpAddr.Port] {
			_ = fbLn.Close()
			continue
		}
		log.Printf("[memorize daemon] port %s busy (%v); auto-fallback bound to %s", targetAddr, err, fbLn.Addr().String())
		return fbLn, nil
	}
	return nil, err
}

func pickNextDaemonAddrForRestart(host string, desiredPort int, excludePorts ...int) (string, int, bool, error) {
	if strings.TrimSpace(host) == "" {
		host = memory.DefaultDaemonHost
	}
	desiredAddr := net.JoinHostPort(host, strconv.Itoa(desiredPort))
	if ln, err := net.Listen("tcp", desiredAddr); err == nil {
		_ = ln.Close()
		return desiredAddr, desiredPort, false, nil
	}
	if probeDashboardMetaAlive(desiredAddr) {
		return desiredAddr, desiredPort, false, nil
	}
	p, fb, err := memory.PickAvailableLoopbackPort(host, desiredPort, excludePorts...)
	if err != nil {
		return "", 0, false, err
	}
	return net.JoinHostPort(host, strconv.Itoa(p)), p, fb, nil
}

func discoverLiveDaemonStatus(explicitAddr, dbPath string) (*daemonStatus, string, bool) {
	seen := make(map[string]bool)
	var candidates []string
	addCand := func(a string) {
		s := strings.TrimSpace(a)
		s = strings.TrimPrefix(strings.TrimPrefix(s, "http://"), "https://")
		s = strings.TrimRight(s, "/")
		if s == "" || seen[s] {
			return
		}
		seen[s] = true
		candidates = append(candidates, s)
	}

	if ov := explicitDaemonOverride(explicitAddr); ov != "" {
		addCand(ov)
		if st, ok := memory.ReadDaemonState(dbPath); ok && st.ListenAddr != "" {
			addCand(st.ListenAddr)
		}
	} else {
		if st, ok := memory.ReadDaemonState(dbPath); ok && st.ListenAddr != "" {
			addCand(st.ListenAddr)
		}
		cfg := memory.LoadPortConfig(dbPath)
		addCand(net.JoinHostPort(memory.DefaultDaemonHost, strconv.Itoa(cfg.DaemonPort)))
		addCand(dashboard.DefaultDashboardAddr)
	}

	for _, cand := range candidates {
		if st, err := queryDaemonStatus(cand); err == nil && st.Running && sameFileClean(st.DBPath, dbPath) {
			return st, cand, true
		}
	}
	return nil, "", false
}

func filterDaemonEnv(base []string, dbPath, modelsDir, explicitAddr string) []string {
	skip := map[string]bool{
		"MEMORY_DB_PATH":        true,
		"MEMORY_MODELS_DIR":     true,
		"MEMORY_DAEMON_ADDR":    true,
		"MEMORY_DASHBOARD_ADDR": true,
	}
	out := make([]string, 0, len(base)+4)
	for _, entry := range base {
		k := entry
		if idx := strings.IndexByte(entry, '='); idx >= 0 {
			k = entry[:idx]
		}
		if skip[k] {
			continue
		}
		out = append(out, entry)
	}
	out = append(out,
		"MEMORY_DB_PATH="+dbPath,
		"MEMORY_MODELS_DIR="+modelsDir,
	)
	if strings.TrimSpace(explicitAddr) != "" {
		out = append(out, "MEMORY_DAEMON_ADDR="+strings.TrimSpace(explicitAddr))
	}
	return out
}

func replaceOrAppendAddrFlag(args []string, boundAddr string) []string {
	out := make([]string, 0, len(args)+2)
	skipNext := false
	for i := 0; i < len(args); i++ {
		if skipNext {
			skipNext = false
			continue
		}
		a := args[i]
		if a == "-addr" || a == "--addr" || a == "-dashboard" || a == "--dashboard" {
			skipNext = true
			continue
		}
		if strings.HasPrefix(a, "-addr=") || strings.HasPrefix(a, "--addr=") ||
			strings.HasPrefix(a, "-dashboard=") || strings.HasPrefix(a, "--dashboard=") {
			continue
		}
		out = append(out, a)
	}
	return append(out, "-addr", boundAddr)
}

func portCLIArgs() []string {
	rest := flag.Args()
	for i, a := range rest {
		if strings.EqualFold(strings.TrimSpace(a), "port") {
			if i+1 < len(rest) {
				return rest[i+1:]
			}
			return nil
		}
	}
	return nil
}

func postDaemonPortApply(addr string, req portApplyRequest) (portApplyResponse, error) {
	var out portApplyResponse
	body, err := json.Marshal(req)
	if err != nil {
		return out, err
	}
	httpReq, err := http.NewRequest(http.MethodPost, "http://"+addr+"/api/daemon/port", bytes.NewReader(body))
	if err != nil {
		return out, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(httpReq)
	if err != nil {
		return out, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err := json.Unmarshal(raw, &out); err != nil && resp.StatusCode >= 300 {
		return out, fmt.Errorf("daemon returned %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	if resp.StatusCode >= 300 || !out.OK {
		msg := out.Error
		if msg == "" {
			msg = strings.TrimSpace(string(raw))
		}
		return out, fmt.Errorf("port update failed: %s", msg)
	}
	return out, nil
}

func waitDaemonPortHealthy(expectedAddr, excludeOldAddr, dbPath string, timeout time.Duration) (*daemonStatus, bool) {
	deadline := time.Now().Add(timeout)
	exp := strings.TrimPrefix(strings.TrimPrefix(strings.TrimRight(expectedAddr, "/"), "http://"), "https://")
	old := strings.TrimPrefix(strings.TrimPrefix(strings.TrimRight(excludeOldAddr, "/"), "http://"), "https://")
	for time.Now().Before(deadline) {
		if exp != "" {
			if st, err := queryDaemonStatus(exp); err == nil && st.Running && sameFileClean(st.DBPath, dbPath) {
				return st, true
			}
		}
		if rt, ok := memory.ReadDaemonState(dbPath); ok && rt.ListenAddr != "" && (old == "" || !strings.EqualFold(rt.ListenAddr, old)) {
			if st, err := queryDaemonStatus(rt.ListenAddr); err == nil && st.Running && sameFileClean(st.DBPath, dbPath) {
				return st, true
			}
		}
		time.Sleep(80 * time.Millisecond)
	}
	return nil, false
}

func runDaemonPortCLI(args []string, explicitAddr, dbPath, modelsDir string) int {
	fs := flag.NewFlagSet("port", flag.ContinueOnError)
	daemonFlag := fs.Int("daemon", 0, "daemon / Web Console port (1..65535)")
	ggufFlag := fs.Int("gguf", 0, "built-in Local GGUF llama-server port (1..65535)")
	resetFlag := fs.Bool("reset", false, "reset both ports to defaults (18095 / 18088)")
	jsonOut := fs.Bool("json", false, "output JSON")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	pos := fs.Args()
	if len(pos) > 0 && strings.EqualFold(strings.TrimSpace(pos[0]), "reset") {
		*resetFlag = true
		pos = pos[1:]
	}

	curCfg := memory.LoadPortConfig(dbPath)
	isSet := *resetFlag || *daemonFlag > 0 || *ggufFlag > 0 || len(pos) > 0

	if !isSet {
		st, liveAddr, running := discoverLiveDaemonStatus(explicitAddr, dbPath)
		mgr := localmodel.NewManager(modelsDir)
		lm := mgr.Status(curCfg.GGUFPort)
		activeDaemon := 0
		daemonFB := false
		if running && st != nil {
			if _, p, ok := memory.ParseHostPort(st.ListenAddr); ok {
				activeDaemon = p
				daemonFB = p != curCfg.DaemonPort
			}
		}
		if *jsonOut {
			b, _ := json.MarshalIndent(map[string]any{
				"configured_daemon_port": curCfg.DaemonPort,
				"active_daemon_port":     activeDaemon,
				"daemon_running":         running,
				"daemon_addr":            liveAddr,
				"daemon_fallback":        daemonFB,
				"configured_gguf_port":   curCfg.GGUFPort,
				"active_gguf_port":       lm.Port,
				"gguf_running":           lm.ServerRunning,
				"gguf_endpoint":          lm.Endpoint,
				"gguf_fallback":          lm.PortFallback,
				"ports_file":             memory.PortsFilePath(dbPath),
				"daemon_state_file":      memory.DaemonStateFilePath(dbPath),
			}, "", "  ")
			fmt.Println(string(b))
			return 0
		}
		fmt.Printf("memorize port configuration:\n")
		if running {
			fbNote := ""
			if daemonFB {
				fbNote = " [fallback]"
			}
			fmt.Printf("  daemon: configured=%d  active=%d%s  status=running (http://%s, pid=%d)\n",
				curCfg.DaemonPort, activeDaemon, fbNote, liveAddr, st.PID)
		} else {
			fmt.Printf("  daemon: configured=%d  active=none  status=idle\n", curCfg.DaemonPort)
		}
		if lm.ServerRunning {
			fbNote := ""
			if lm.PortFallback {
				fbNote = " [fallback]"
			}
			fmt.Printf("  gguf:   configured=%d  active=%d%s  status=running (%s)\n",
				curCfg.GGUFPort, lm.Port, fbNote, lm.Endpoint)
		} else {
			fmt.Printf("  gguf:   configured=%d  active=none  status=idle (%s)\n",
				curCfg.GGUFPort, lm.Endpoint)
		}
		if p := memory.PortsFilePath(dbPath); p != "" {
			fmt.Printf("  config: %s\n", p)
		}
		if s := memory.DaemonStateFilePath(dbPath); s != "" {
			fmt.Printf("  state:  %s\n", s)
		}
		return 0
	}

	nextCfg := curCfg
	if *resetFlag {
		nextCfg = memory.DefaultPortConfig()
	} else {
		if *daemonFlag > 0 {
			nextCfg.DaemonPort = *daemonFlag
		}
		if *ggufFlag > 0 {
			nextCfg.GGUFPort = *ggufFlag
		}
		if len(pos) >= 1 {
			p, err := strconv.Atoi(strings.TrimSpace(pos[0]))
			if err != nil {
				fmt.Fprintf(os.Stderr, "invalid daemon port %q: %v\n", pos[0], err)
				return 2
			}
			nextCfg.DaemonPort = p
		}
		if len(pos) >= 2 {
			p, err := strconv.Atoi(strings.TrimSpace(pos[1]))
			if err != nil {
				fmt.Fprintf(os.Stderr, "invalid gguf port %q: %v\n", pos[1], err)
				return 2
			}
			nextCfg.GGUFPort = p
		}
		if len(pos) > 2 {
			fmt.Fprintf(os.Stderr, "usage: memorize daemon port [<daemon_port> [<gguf_port>]] [--daemon N] [--gguf N] [--reset] [--json]\n")
			return 2
		}
	}

	if err := memory.ValidatePortConfig(nextCfg.DaemonPort, nextCfg.GGUFPort); err != nil {
		fmt.Fprintf(os.Stderr, "memorize port error: %v\n", err)
		return 2
	}

	// If a daemon is running, delegate the change to it so DB, GGUF server, and daemon listener stay synchronized.
	if _, liveAddr, running := discoverLiveDaemonStatus(explicitAddr, dbPath); running {
		res, err := postDaemonPortApply(liveAddr, portApplyRequest{
			DaemonPort: nextCfg.DaemonPort,
			GGUFPort:   nextCfg.GGUFPort,
			Reset:      *resetFlag,
		})
		if err != nil {
			fmt.Fprintf(os.Stderr, "memorize daemon port error: %v\n", err)
			return 1
		}
		if res.Restarting && res.NewURL != "" {
			if newSt, ok := waitDaemonPortHealthy(res.NewURL, liveAddr, dbPath, 6*time.Second); ok {
				res.ListenAddr = newSt.ListenAddr
				if _, ap, okp := memory.ParseHostPort(newSt.ListenAddr); okp {
					res.ActiveDaemon = ap
					res.DaemonFallback = ap != nextCfg.DaemonPort
				}
				res.NewURL = "http://" + newSt.ListenAddr
			} else {
				fmt.Fprintf(os.Stderr, "memorize daemon restart onto %s did not become healthy within timeout\n", res.NewURL)
				return 1
			}
		}
		if *jsonOut {
			b, _ := json.MarshalIndent(res, "", "  ")
			fmt.Println(string(b))
			return 0
		}
		fmt.Printf("memorize ports updated:\n")
		fmt.Printf("  daemon: configured=%d  active=%d  url=%s\n", res.ConfiguredDaemon, res.ActiveDaemon, res.NewURL)
		if res.DaemonFallback {
			fmt.Printf("  note: configured daemon port %d was busy; using auto-fallback %d\n", res.ConfiguredDaemon, res.ActiveDaemon)
		}
		fmt.Printf("  gguf:   configured=%d  active=%d  endpoint=%s  running=%v\n",
			res.ConfiguredGGUF, res.ActiveGGUF, res.GGUFEndpoint, res.GGUFRunning)
		return 0
	}

	// Offline update: save ports.json, sync SQLite gguf_endpoint if DB exists, and restart standalone llama-server if active.
	if err := memory.SavePortConfig(dbPath, nextCfg); err != nil {
		fmt.Fprintf(os.Stderr, "memorize port save error: %v\n", err)
		return 1
	}

	mgr := localmodel.NewManager(modelsDir)
	effGGUFPort := nextCfg.GGUFPort
	effGGUFEP := fmt.Sprintf("http://127.0.0.1:%d", effGGUFPort)
	ggufRunning := false
	ggufFallback := false
	lm := mgr.Status(curCfg.GGUFPort)
	if lm.ServerRunning && lm.Supported && lm.ActiveModel != "" {
		ggufRunning = true
		if lm.Port != nextCfg.GGUFPort {
			if ep, serr := mgr.StartServer(context.Background(), lm.ActiveModel, nextCfg.GGUFPort); serr == nil && ep != "" {
				effGGUFEP = ep
				if _, p, okp := memory.ParseHostPort(ep); okp && p > 0 {
					effGGUFPort = p
					ggufFallback = p != nextCfg.GGUFPort
				}
			} else if serr != nil {
				fmt.Fprintf(os.Stderr, "warning: saved GGUF port %d, but failed to restart running llama-server: %v\n", nextCfg.GGUFPort, serr)
			}
		}
	}

	if svc, err := memory.NewService(dbPath, nil); err == nil {
		_ = memory.SyncManagedGGUFEndpoint(context.Background(), svc, effGGUFEP)
		_ = svc.Close()
	}

	if *jsonOut {
		b, _ := json.MarshalIndent(map[string]any{
			"ok":                     true,
			"configured_daemon_port": nextCfg.DaemonPort,
			"active_daemon_port":     0,
			"restarting":             false,
			"daemon_running":         false,
			"configured_gguf_port":   nextCfg.GGUFPort,
			"active_gguf_port":       effGGUFPort,
			"gguf_endpoint":          effGGUFEP,
			"gguf_fallback":          ggufFallback,
			"gguf_running":           ggufRunning,
		}, "", "  ")
		fmt.Println(string(b))
		return 0
	}
	fmt.Printf("memorize ports saved (daemon currently idle):\n")
	fmt.Printf("  daemon: configured=%d (takes effect on next start)\n", nextCfg.DaemonPort)
	fmt.Printf("  gguf:   configured=%d  endpoint=%s  running=%v\n", nextCfg.GGUFPort, effGGUFEP, ggufRunning)
	return 0
}
