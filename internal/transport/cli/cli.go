package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"memorize/internal/infra/config"
	thttp "memorize/internal/transport/http"
)

// RuntimeHooks decouples CLI flag dispatch from application-layer daemon composition.
type RuntimeHooks struct {
	EnsureDaemonRunning func(explicitAddr, dbPath, modelsDir string) (*thttp.DaemonStatus, bool)
	RunDaemonServer     func(explicitAddr, dbPath, modelsDir string) int
	ServeInlineStdio    func(dbPath, modelsDir string, withDashboard bool, dashAddr string) int
	RunInlineHook       func(ctx context.Context, dbPath, modelsDir, mode string, rawStdin []byte) (string, int)
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if s := strings.TrimSpace(v); s != "" {
			return s
		}
	}
	return ""
}

func readHookStdin() []byte {
	if st, err := os.Stdin.Stat(); err == nil && (st.Mode()&os.ModeCharDevice) == 0 {
		b, _ := io.ReadAll(io.LimitReader(os.Stdin, 1<<20))
		return b
	}
	return nil
}

func effectiveProcStart(goStart time.Time) time.Time {
	if raw := strings.TrimSpace(os.Getenv("MEMORY_LAUNCHER_START_SEC")); raw != "" {
		if sec, err := strconv.ParseInt(raw, 10, 64); err == nil && sec > 0 {
			// Use end-of-second upper bound so integer second rounding never overstates elapsed time,
			// while still capturing multi-second shell bootstrap/codesign work.
			launcherEndOfSec := time.Unix(sec, 999_999_999)
			if launcherEndOfSec.Before(goStart) && goStart.Sub(launcherEndOfSec) < 5*time.Minute {
				return launcherEndOfSec
			}
		}
	}
	return goStart
}

func isInlineModeForced(dbPath string) bool {
	return os.Getenv("MEMORY_DISABLE_BG_DASHBOARD") == "1" ||
		os.Getenv("MEMORY_INLINE_STDIO") == "1" ||
		dbPath == ":memory:" ||
		strings.HasPrefix(dbPath, "file::memory:")
}

func resolveStoragePaths() (dbPath, modelsDir string) {
	dbPath = strings.TrimSpace(os.Getenv("MEMORY_DB_PATH"))
	if dbPath == "" {
		if dataDir := config.DefaultDataDir(); dataDir != "" {
			_ = os.MkdirAll(dataDir, 0o700)
			dbPath = filepath.Join(dataDir, "memories.db")
		} else {
			dbPath = "memories.db"
		}
	}
	modelsDir = strings.TrimSpace(os.Getenv("MEMORY_MODELS_DIR"))
	if modelsDir == "" && dbPath != ":memory:" {
		if absDB, err := filepath.Abs(dbPath); err == nil {
			modelsDir = filepath.Join(filepath.Dir(absDB), "models")
		}
	}
	return dbPath, modelsDir
}

func parseDaemonCtlAction(explicit string, args []string) string {
	if act := strings.TrimSpace(explicit); act != "" {
		return act
	}
	if len(args) == 0 {
		return ""
	}
	first := strings.ToLower(strings.TrimSpace(args[0]))
	switch first {
	case "daemon", "service", "server":
		if len(args) >= 2 {
			return args[1]
		}
		return "status"
	case "status", "start", "stop", "restart", "reload", "logs", "port":
		return first
	default:
		return ""
	}
}

func runHookCommand(mode, explicitAddr, dbPath, modelsDir string, rawStdin []byte, procStart time.Time, hooks RuntimeHooks) int {
	if !isInlineModeForced(dbPath) && hooks.EnsureDaemonRunning != nil {
		if out, code, ok := runHookViaDaemon(explicitAddr, dbPath, modelsDir, mode, rawStdin, procStart, hooks.EnsureDaemonRunning); ok {
			if strings.TrimSpace(out) != "" {
				fmt.Println(out)
			}
			return code
		}
	}
	if hooks.RunInlineHook == nil {
		log.Printf("inline hook handler not configured")
		return 1
	}
	ctx := context.Background()
	var cancel context.CancelFunc = func() {}
	if total, ok := resolveConfiguredHookTimeout(mode); ok {
		srvBudget, _, alive := remainingHookBudgets(procStart, total)
		if !alive {
			return 0
		}
		ctx, cancel = context.WithTimeout(ctx, srvBudget)
	}
	defer cancel()
	out, code := hooks.RunInlineHook(ctx, dbPath, modelsDir, mode, rawStdin)
	if strings.TrimSpace(out) != "" {
		fmt.Println(out)
	}
	return code
}

func executeArgs(args []string, version string, hooks RuntimeHooks) int {
	procStart := effectiveProcStart(time.Now())
	fs := flag.NewFlagSet("memorize", flag.ContinueOnError)
	showVersion := fs.Bool("version", false, "print memorize version and exit")
	installMode := fs.String("install", "", "install Git pre-commit guardrail hook ('git-hook')")
	daemonMode := fs.Bool("daemon", false, "run single unified background daemon (SQLite + Dashboard + MCP + Hooks)")
	daemonAddr := fs.String("addr", "", "daemon listen address (default 127.0.0.1:18095)")
	daemonCtl := fs.String("daemon-ctl", "", "control background daemon: status, start, stop, restart, logs, port")
	dashAddr := fs.String("dashboard", "", "alias to run unified daemon/dashboard on explicit listen address (e.g. 127.0.0.1:18095)")
	hookMode := fs.String("hook", "", "execute zero-touch lifecycle hook mode (claude-start, claude-turn, claude-pretool, claude-stop, claude-end, antigravity-start, antigravity-turn, antigravity-pretool, antigravity-stop, git-precommit)")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}

	if *showVersion {
		fmt.Printf("memorize %s\n", version)
		return 0
	}
	if target := strings.TrimSpace(*installMode); target != "" {
		if err := installTarget(target); err != nil {
			fmt.Fprintf(os.Stderr, "install failed: %v\n", err)
			return 1
		}
		return 0
	}

	dbPath, modelsDir := resolveStoragePaths()
	explicitAddr := firstNonEmpty(*daemonAddr, *dashAddr)
	posArgs := fs.Args()

	if ctlAction := parseDaemonCtlAction(*daemonCtl, posArgs); ctlAction != "" {
		return runDaemonControlCLI(ctlAction, posArgs, explicitAddr, dbPath, modelsDir, hooks)
	}

	if m := strings.TrimSpace(*hookMode); m != "" {
		return runHookCommand(m, explicitAddr, dbPath, modelsDir, readHookStdin(), procStart, hooks)
	}

	// Default MCP stdio mode runs as a lightweight stateless bridge to the unified daemon
	// (auto-spawning if needed), so all sessions share 1 daemon & hot-reload transparently.
	if !*daemonMode && strings.TrimSpace(*dashAddr) == "" && !isInlineModeForced(dbPath) && hooks.EnsureDaemonRunning != nil {
		if _, ok := hooks.EnsureDaemonRunning(explicitAddr, dbPath, modelsDir); ok {
			if err := runStdioDaemonBridge(explicitAddr, dbPath, modelsDir, hooks.EnsureDaemonRunning); err != nil {
				fmt.Fprintf(os.Stderr, "stdio bridge error: %v\n", err)
				return 1
			}
			return 0
		}
	}

	if *daemonMode || strings.TrimSpace(*dashAddr) != "" {
		if hooks.RunDaemonServer == nil {
			fmt.Fprintln(os.Stderr, "daemon server handler not configured")
			return 1
		}
		return hooks.RunDaemonServer(explicitAddr, dbPath, modelsDir)
	}

	if hooks.ServeInlineStdio == nil {
		fmt.Fprintln(os.Stderr, "inline stdio handler not configured")
		return 1
	}
	withDash := os.Getenv("MEMORY_DISABLE_BG_DASHBOARD") != "1"
	return hooks.ServeInlineStdio(dbPath, modelsDir, withDash, os.Getenv("MEMORY_DASHBOARD_ADDR"))
}

// Execute parses CLI flags/subcommands from os.Args and returns the process exit code.
func Execute(version string, hooks RuntimeHooks) int {
	return executeArgs(os.Args[1:], version, hooks)
}
