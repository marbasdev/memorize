package cli

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"memorize/internal/domain"
	"memorize/internal/inference/localmodel"
	"memorize/internal/infra/config"
	"memorize/internal/infra/sqlite"
	thttp "memorize/internal/transport/http"
)

func portCLIArgs(rest []string) []string {
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

	curCfg := config.LoadPortConfig(dbPath)
	isSet := *resetFlag || *daemonFlag > 0 || *ggufFlag > 0 || len(pos) > 0

	if !isSet {
		st, liveAddr, running := thttp.DiscoverLiveDaemonStatus(explicitAddr, dbPath)
		mgr := localmodel.NewManager(modelsDir)
		lm := mgr.Status(curCfg.GGUFPort)
		activeDaemon := 0
		daemonFB := false
		if running && st != nil {
			if _, p, ok := domain.ParseHostPort(st.ListenAddr); ok {
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
				"ports_file":             config.PortsFilePath(dbPath),
				"daemon_state_file":      config.DaemonStateFilePath(dbPath),
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
		if p := config.PortsFilePath(dbPath); p != "" {
			fmt.Printf("  config: %s\n", p)
		}
		if s := config.DaemonStateFilePath(dbPath); s != "" {
			fmt.Printf("  state:  %s\n", s)
		}
		return 0
	}

	nextCfg := curCfg
	if *resetFlag {
		nextCfg = domain.DefaultPortConfig()
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

	if err := domain.ValidatePortConfig(nextCfg.DaemonPort, nextCfg.GGUFPort); err != nil {
		fmt.Fprintf(os.Stderr, "memorize port error: %v\n", err)
		return 2
	}

	// If a daemon is running, delegate the change to it so DB, GGUF server, and daemon listener stay synchronized.
	if _, liveAddr, running := thttp.DiscoverLiveDaemonStatus(explicitAddr, dbPath); running {
		res, err := thttp.PostDaemonPortApply(liveAddr, thttp.PortApplyRequest{
			DaemonPort: nextCfg.DaemonPort,
			GGUFPort:   nextCfg.GGUFPort,
			Reset:      *resetFlag,
		})
		if err != nil {
			fmt.Fprintf(os.Stderr, "memorize daemon port error: %v\n", err)
			return 1
		}
		if res.Restarting && res.NewURL != "" {
			if newSt, ok := thttp.WaitDaemonPortHealthy(res.NewURL, liveAddr, dbPath, 6*time.Second); ok {
				res.ListenAddr = newSt.ListenAddr
				if _, ap, okp := domain.ParseHostPort(newSt.ListenAddr); okp {
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
	if err := config.SavePortConfig(dbPath, nextCfg); err != nil {
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
				if _, p, okp := domain.ParseHostPort(ep); okp && p > 0 {
					effGGUFPort = p
					ggufFallback = p != nextCfg.GGUFPort
				}
			} else if serr != nil {
				fmt.Fprintf(os.Stderr, "warning: saved GGUF port %d, but failed to restart running llama-server: %v\n", nextCfg.GGUFPort, serr)
			}
		}
	}

	if db, err := sqlite.Open(dbPath); err == nil {
		if sqlite.InitSchema(db) == nil {
			_ = config.SyncManagedGGUFEndpoint(context.Background(), db, dbPath, effGGUFEP)
		}
		_ = db.Close()
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
