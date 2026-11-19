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

	"memorize/internal/domain"
	"memorize/internal/infra/config"
	thttp "memorize/internal/transport/http"
)

type RuntimeHooks struct {
	EnsureDaemonRunning func(explicitAddr, dbPath, modelsDir string) (*thttp.DaemonStatus, bool)
	RunDaemonServer     func(explicitAddr, dbPath, modelsDir string) int
	RunTeamServer       func(explicitAddr, dbPath, modelsDir string) int
	ServeInlineStdio    func(dbPath, modelsDir string, withDashboard bool, dashAddr string) int
	RunInlineHook       func(ctx context.Context, dbPath, modelsDir, mode string, rawStdin []byte) (string, int)
}

func resolveClientDataDir(dbPath string) string {
	if envDir := strings.TrimSpace(os.Getenv("MEMORY_DATA_DIR")); envDir != "" {
		return envDir
	}
	if dbPath != "" && dbPath != ":memory:" && !strings.HasPrefix(dbPath, "file::memory:") {
		return filepath.Dir(dbPath)
	}
	return config.DefaultDataDir()
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
	if !isInlineModeForced(dbPath) {
		dataDir := resolveClientDataDir(dbPath)
		if remote, ok := resolveRemoteTargetForHook(dataDir, rawStdin); ok {
			out, code, handled := runHookViaRemote(remote, dataDir, mode, rawStdin, procStart)
			if handled {
				if strings.TrimSpace(out) != "" {
					fmt.Println(out)
				}
				return code
			}
		}
		if hooks.EnsureDaemonRunning != nil {
			if out, code, ok := runHookViaDaemon(explicitAddr, dbPath, modelsDir, mode, rawStdin, procStart, hooks.EnsureDaemonRunning); ok {
				if strings.TrimSpace(out) != "" {
					fmt.Println(out)
				}
				return code
			}
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
	dbPath, modelsDir := resolveStoragePaths()

	if len(args) > 0 {
		switch strings.ToLower(strings.TrimSpace(args[0])) {
		case "serve":
			return runServeCLI(args[1:], dbPath, modelsDir, hooks)
		case "user", "users":
			return runUserCLI(args[1:], dbPath)
		case "connect":
			return runConnectCLI(args[1:], dbPath)
		case "disconnect":
			return runDisconnectCLI(args[1:], dbPath)
		case "switch":
			return runSwitchCLI(args[1:], dbPath)
		case "push":
			return runPushCLI(args[1:], dbPath)
		case "export":
			return runExportCLI(args[1:], dbPath)
		case "import":
			return runImportCLI(args[1:], dbPath, modelsDir, hooks)
		}
	}

	fs := flag.NewFlagSet("memorize", flag.ContinueOnError)
	showVersion := fs.Bool("version", false, "print memorize version and exit")
	installMode := fs.String("install", "", "install Git pre-commit guardrail hook ('git-hook')")
	daemonMode := fs.Bool("daemon", false, "run single unified background daemon (SQLite + Dashboard + MCP + Hooks)")
	serveMode := fs.Bool("serve", false, "run authenticated team server mode (default 0.0.0.0:18095)")
	daemonAddr := fs.String("addr", "", "daemon or team server listen address (default 127.0.0.1:18095 / 0.0.0.0:18095)")
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

	explicitAddr := domain.FirstNonEmpty(*daemonAddr, *dashAddr)
	posArgs := fs.Args()

	if *serveMode {
		if hooks.RunTeamServer == nil {
			fmt.Fprintln(os.Stderr, "team server handler not configured")
			return 1
		}
		listenAddr := strings.TrimSpace(explicitAddr)
		if listenAddr == "" {
			listenAddr = domain.DefaultServeAddr
		}
		return hooks.RunTeamServer(listenAddr, dbPath, modelsDir)
	}

	if ctlAction := parseDaemonCtlAction(*daemonCtl, posArgs); ctlAction != "" {
		return runDaemonControlCLI(ctlAction, posArgs, explicitAddr, dbPath, modelsDir, hooks)
	}

	if m := strings.TrimSpace(*hookMode); m != "" {
		return runHookCommand(m, explicitAddr, dbPath, modelsDir, readHookStdin(), procStart, hooks)
	}

	cwd, _ := os.Getwd()
	_, hasRemote := config.ResolveRemoteTarget(resolveClientDataDir(dbPath), cwd)
	if !*daemonMode && strings.TrimSpace(*dashAddr) == "" && !isInlineModeForced(dbPath) && (hasRemote || hooks.EnsureDaemonRunning != nil) {
		if hasRemote || func() bool { _, ok := hooks.EnsureDaemonRunning(explicitAddr, dbPath, modelsDir); return ok }() {
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

func Execute(version string, hooks RuntimeHooks) int {
	return executeArgs(os.Args[1:], version, hooks)
}

func reorderFlagsBeforePositional(args []string, valueFlags map[string]bool) []string {
	var flags, pos []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			pos = append(pos, args[i+1:]...)
			break
		}
		if strings.HasPrefix(a, "-") && len(a) > 1 {
			flags = append(flags, a)
			name := strings.TrimLeft(a, "-")
			if eq := strings.IndexByte(name, '='); eq >= 0 {
				continue
			}
			if valueFlags[name] && i+1 < len(args) {
				flags = append(flags, args[i+1])
				i++
			}
			continue
		}
		pos = append(pos, a)
	}
	return append(flags, pos...)
}
