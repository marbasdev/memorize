package main

import (
	"bytes"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"memorize/internal/memory"
	"memorize/internal/memory/dashboard"
)

func runDaemonControlCLI(action, addr, dbPath, modelsDir string) int {
	act := strings.ToLower(strings.TrimSpace(action))
	switch act {
	case "port":
		return runDaemonPortCLI(portCLIArgs(), addr, dbPath, modelsDir)
	case "status":
		st, targetAddr, ok := discoverLiveDaemonStatus(addr, dbPath)
		if !ok || st == nil || !st.Running {
			cfg := memory.LoadPortConfig(dbPath)
			cfgAddr, _ := resolveDaemonTargetAddr(addr, dbPath)
			fmt.Printf("memorize daemon: stopped (configured_daemon=%d, configured_gguf=%d, addr=http://%s, db=%s)\n",
				cfg.DaemonPort, cfg.GGUFPort, cfgAddr, dbPath)
			return 0
		}
		daemonFBNote := ""
		if st.DaemonFallback {
			daemonFBNote = fmt.Sprintf(" [fallback from :%d]", st.ConfiguredDaemonPort)
		}
		ggufInfo := fmt.Sprintf(":%d (idle)", st.ConfiguredGGUFPort)
		if st.GGUFRunning {
			ggufFB := ""
			if st.GGUFFallback {
				ggufFB = fmt.Sprintf(" [fallback from :%d]", st.ConfiguredGGUFPort)
			}
			ggufInfo = fmt.Sprintf("%s%s (running)", st.GGUFEndpoint, ggufFB)
		} else if st.ConfiguredGGUFPort == 0 {
			cfg := memory.LoadPortConfig(dbPath)
			ggufInfo = fmt.Sprintf(":%d (idle)", cfg.GGUFPort)
		}
		fmt.Printf("memorize daemon: running\n  PID:      %d\n  Version:  %s\n  Uptime:   %ds (since %s)\n  Console:  http://%s%s\n  MCP HTTP: http://%s/mcp\n  GGUF:     %s\n  DB:       %s\n  Provider: %s\n  Log:      %s\n",
			st.PID, st.Version, st.UptimeSec, st.StartedAt, targetAddr, daemonFBNote, targetAddr, ggufInfo, st.DBPath, st.Provider, st.LogPath)
		return 0

	case "start":
		st, ok := ensureDaemonRunning(addr, dbPath, modelsDir)
		if !ok || st == nil {
			targetAddr, _ := resolveDaemonTargetAddr(addr, dbPath)
			fmt.Fprintf(os.Stderr, "failed to start memorize daemon on %s\n", targetAddr)
			return 1
		}
		fmt.Printf("memorize daemon started (pid=%d, version=%s, url=http://%s)\n", st.PID, st.Version, st.ListenAddr)
		return 0

	case "stop":
		st, targetAddr, ok := discoverLiveDaemonStatus(addr, dbPath)
		if !ok || st == nil || !st.Running {
			cfgAddr, _ := resolveDaemonTargetAddr(addr, dbPath)
			if dashboard.RequestDashboardYield(cfgAddr) {
				fmt.Printf("stopped legacy memorize dashboard on %s\n", cfgAddr)
				return 0
			}
			fmt.Printf("memorize daemon is already stopped (%s)\n", cfgAddr)
			return 0
		}
		req, _ := http.NewRequest(http.MethodPost, "http://"+targetAddr+"/api/daemon/stop", bytes.NewReader([]byte(`{}`)))
		req.Header.Set("Content-Type", "application/json")
		if resp, err := daemonProbeHTTP.Do(req); err == nil {
			_ = resp.Body.Close()
		}
		for range 40 {
			time.Sleep(40 * time.Millisecond)
			if _, err := queryDaemonStatus(targetAddr); err != nil {
				break
			}
		}
		memory.RemoveDaemonStateIfPID(dbPath, st.PID)
		fmt.Printf("memorize daemon stopped (was pid=%d on %s)\n", st.PID, targetAddr)
		return 0

	case "restart", "reload":
		oldSt, targetAddr, ok := discoverLiveDaemonStatus(addr, dbPath)
		if ok && oldSt != nil && oldSt.Running {
			req, _ := http.NewRequest(http.MethodPost, "http://"+targetAddr+"/api/daemon/restart", bytes.NewReader([]byte(`{}`)))
			req.Header.Set("Content-Type", "application/json")
			if resp, rerr := daemonProbeHTTP.Do(req); rerr == nil {
				_ = resp.Body.Close()
			}
			time.Sleep(220 * time.Millisecond)
		}
		newSt, ok := ensureDaemonRunning(addr, dbPath, modelsDir)
		if !ok || newSt == nil {
			cfgAddr, _ := resolveDaemonTargetAddr(addr, dbPath)
			fmt.Fprintf(os.Stderr, "failed to restart memorize daemon on %s\n", cfgAddr)
			return 1
		}
		fmt.Printf("memorize daemon restarted (pid=%d, version=%s, url=http://%s)\n", newSt.PID, newSt.Version, newSt.ListenAddr)
		return 0

	case "logs", "log":
		lp := daemonLogPath(dbPath)
		data, err := os.ReadFile(lp)
		if err != nil {
			fmt.Printf("no daemon log at %s\n", lp)
			return 0
		}
		lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
		if len(lines) > 50 {
			lines = lines[len(lines)-50:]
		}
		fmt.Println(strings.Join(lines, "\n"))
		return 0

	default:
		fmt.Fprintf(os.Stderr, "unknown daemon command %q (expected: status, start, stop, restart, logs, port)\n", action)
		return 2
	}
}
