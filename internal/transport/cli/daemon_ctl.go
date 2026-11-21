package cli

import (
	"fmt"
	"os"
	"strings"
	"time"

	"praxis/internal/infra/config"
	thttp "praxis/internal/transport/http"
)

func runDaemonControlCLI(action string, posArgs []string, addr, dbPath, modelsDir, version string, hooks RuntimeHooks) int {
	act := strings.ToLower(strings.TrimSpace(action))
	switch act {
	case "port":
		return runDaemonPortCLI(daemonSubArgs(posArgs, "port"), addr, dbPath, modelsDir)

	case "status":
		st, targetAddr, ok, authReq := thttp.ProbeLiveDaemonState(addr, dbPath)
		if authReq {
			fmt.Printf("praxis team server: running on http://%s (authentication required; manage via 'praxis user' or 'praxis connect')\n", targetAddr)
			return 0
		}
		if !ok || st == nil || !st.Running {
			cfg := config.LoadPortConfig(dbPath)
			cfgAddr, _ := thttp.ResolveDaemonTargetAddr(addr, dbPath)
			fmt.Printf("praxis daemon: stopped (configured_daemon=%d, configured_gguf=%d, addr=http://%s, db=%s)\n",
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
			cfg := config.LoadPortConfig(dbPath)
			ggufInfo = fmt.Sprintf(":%d (idle)", cfg.GGUFPort)
		}
		fmt.Printf("praxis daemon: running\n  PID:      %d\n  Version:  %s\n  Uptime:   %ds (since %s)\n  Console:  http://%s%s\n  MCP HTTP: http://%s/mcp\n  GGUF:     %s\n  DB:       %s\n  Provider: %s\n  Log:      %s\n",
			st.PID, st.Version, st.UptimeSec, st.StartedAt, targetAddr, daemonFBNote, targetAddr, ggufInfo, st.DBPath, st.Provider, st.LogPath)
		return 0

	case "start":
		if hooks.EnsureDaemonRunning == nil {
			fmt.Fprintln(os.Stderr, "daemon runtime hook not configured")
			return 1
		}
		st, ok := hooks.EnsureDaemonRunning(addr, dbPath, modelsDir)
		if !ok || st == nil {
			targetAddr, _ := thttp.ResolveDaemonTargetAddr(addr, dbPath)
			fmt.Fprintf(os.Stderr, "failed to start praxis daemon on %s\n", targetAddr)
			return 1
		}
		fmt.Printf("praxis daemon started (pid=%d, version=%s, url=http://%s)\n", st.PID, st.Version, st.ListenAddr)
		return 0

	case "stop":
		st, targetAddr, ok := thttp.DiscoverLiveDaemonStatus(addr, dbPath)
		if !ok || st == nil || !st.Running {
			cfgAddr, _ := thttp.ResolveDaemonTargetAddr(addr, dbPath)
			if thttp.RequestDashboardYield(cfgAddr) {
				fmt.Printf("stopped legacy praxis dashboard on %s\n", cfgAddr)
				return 0
			}
			fmt.Printf("praxis daemon is already stopped (%s)\n", cfgAddr)
			return 0
		}
		thttp.PostDaemonStop(targetAddr)
		for range 40 {
			time.Sleep(40 * time.Millisecond)
			if _, err := thttp.QueryDaemonStatus(targetAddr); err != nil {
				break
			}
		}
		config.RemoveDaemonStateIfPID(dbPath, st.PID)
		fmt.Printf("praxis daemon stopped (was pid=%d on %s)\n", st.PID, targetAddr)
		return 0

	case "restart", "reload":
		if hooks.EnsureDaemonRunning == nil {
			fmt.Fprintln(os.Stderr, "daemon runtime hook not configured")
			return 1
		}
		oldSt, targetAddr, ok := thttp.DiscoverLiveDaemonStatus(addr, dbPath)
		if ok && oldSt != nil && oldSt.Running {
			thttp.PostDaemonRestart(targetAddr)
			for range 40 {
				time.Sleep(40 * time.Millisecond)
				curSt, err := thttp.QueryDaemonStatus(targetAddr)
				if err != nil || curSt == nil || !curSt.Running || curSt.PID != oldSt.PID {
					break
				}
			}
		}
		newSt, ok := hooks.EnsureDaemonRunning(addr, dbPath, modelsDir)
		if !ok || newSt == nil {
			cfgAddr, _ := thttp.ResolveDaemonTargetAddr(addr, dbPath)
			fmt.Fprintf(os.Stderr, "failed to restart praxis daemon on %s\n", cfgAddr)
			return 1
		}
		fmt.Printf("praxis daemon restarted (pid=%d, version=%s, url=http://%s)\n", newSt.PID, newSt.Version, newSt.ListenAddr)
		return 0

	case "logs", "log":
		lp := config.DaemonLogPath(dbPath)
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

	case "doctor":
		return runDoctorCLI(daemonSubArgs(posArgs, "doctor"), dbPath, modelsDir, version, hooks)

	default:
		fmt.Fprintf(os.Stderr, "unknown daemon command %q (expected: status, start, stop, restart, logs, port, doctor)\n", action)
		return 2
	}
}
