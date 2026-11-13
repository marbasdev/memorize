package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/mark3labs/mcp-go/server"

	"memorize/internal/memory"
	"memorize/internal/memory/adapters"
	"memorize/internal/memory/dashboard"
	"memorize/internal/memory/localmodel"
)

var version = "1.0.0"

const serverInstructions = `Memorize Protocol:
1. Scope: 'project:<repo>', 'global', or 'personal' (auto-detected if omitted).
2. Query: call query_project_memory on new topics (refresh=true after compaction). Follow 'memorize://playbook/memory-curator' on Git onboarding/sync.
3. Commit Before Mutating: commit user decisions/approvals atomically (1 behavior = 1 item in candidate_facts, user's language) via commit_memory or manage_memory(update) BEFORE edits/commands; commit new choices before finishing.
4. Onboarding: if [ONBOARDING AVAILABLE], ask user first; call manage_memory(skip_onboarding) if declined, or inspect files, resolve conflicts, commit, and offer cleanup.
5. SSOT: active memories replace static rule files (CLAUDE.md/AGENTS.md) and override older turns; ignore (Replaces: ...) and [REVOKED ...].
6. Progress: use project_progress for work tracks ('planned' unstarted, 'in_progress' active, 'blocked' halted with blocker in next_step, 'done').`

func attachDynamicResolvers(svc *memory.Service, mgr *localmodel.Manager) {
	dbPath := svc.DBPath()
	mgr.SetPreferredPortFunc(func() int {
		return memory.LoadPortConfig(dbPath).GGUFPort
	})
	mgr.SetDashboardURLFunc(func() string {
		return memory.ActiveDaemonURL(dbPath)
	})
	wake := func(ctx context.Context, model string) (string, error) {
		ep, err := mgr.EnsureRunningEndpoint(ctx, model, 0)
		if err == nil && ep != "" {
			_ = memory.SyncManagedGGUFEndpoint(ctx, svc, ep)
		}
		return ep, err
	}
	taskRes := adapters.NewTaskResolverWithEndpoint(wake)
	svc.SetTaskEvaluatorResolver(taskRes)
	svc.SetEvaluatorResolver(func(cfg map[string]string) memory.Evaluator {
		ev, _ := taskRes(cfg, "")
		return ev
	})
	svc.SetReadinessChecker(func(ctx context.Context, cfg map[string]string) (bool, string) {
		return checkConfiguredEngineReadiness(ctx, cfg, mgr)
	})
}

var setupProbeClient = &http.Client{Timeout: 200 * time.Millisecond}

func probeHTTPAlive(ctx context.Context, rawURL string) bool {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return false
	}
	resp, err := setupProbeClient.Do(req)
	if err != nil {
		return false
	}
	_ = resp.Body.Close()
	return resp.StatusCode < 500
}

func checkConfiguredEngineReadiness(ctx context.Context, cfg map[string]string, mgr *localmodel.Manager) (bool, string) {
	eff := memory.NormalizeProviderID(cfg["active_provider"])
	if eff == "" {
		eff = memory.ResolveExplicitEnvProvider()
	}
	if eff == "" {
		eff = memory.DefaultProvider()
	}
	if eff == "gguf" && !localmodel.IsSupportedPlatform() {
		eff = "local"
	}
	switch eff {
	case "gguf":
		if envEP := strings.TrimSpace(os.Getenv("LOCAL_GGUF_URL")); envEP != "" {
			if probeHTTPAlive(ctx, strings.TrimRight(envEP, "/")+"/health") {
				return true, ""
			}
			return false, "LOCAL_GGUF_URL unreachable (" + envEP + ")"
		}
		if mgr != nil {
			st := mgr.Status(0)
			if st.ServerRunning || (len(st.Installed) > 0 && st.RuntimeInstalled) {
				return true, ""
			}
			if st.Download.Active {
				return false, "downloading local GGUF model (" + st.Download.Target + ")"
			}
			if len(st.Installed) > 0 && !st.RuntimeInstalled {
				return false, "llama-server runtime not installed"
			}
		}
		return false, "no local GGUF model installed"
	case "gemini":
		if firstNonEmptyStr(cfg["gemini_api_key"], os.Getenv("GEMINI_API_KEY")) == "" {
			return false, "GEMINI_API_KEY not set"
		}
		return true, ""
	case "jev":
		jevURL := firstNonEmptyStr(cfg["jev_api_url"], os.Getenv("JEV_API_URL"), "https://api.typesafe.ai/v1/sys"+"temone")
		if adapters.IsTypesafeCloudURL(jevURL) {
			if firstNonEmptyStr(cfg["typesafe_api_key"], os.Getenv("TYPESAFE_API_KEY"), os.Getenv("FLIDE_API_KEY")) == "" {
				return false, "TYPESAFE_API_KEY not set"
			}
			return true, ""
		}
		base := adapters.ResolveSystemOneBaseURL(jevURL)
		if probeHTTPAlive(ctx, base+"/health") || probeHTTPAlive(ctx, base+"/v1/models") {
			return true, ""
		}
		return false, "System 1 endpoint unreachable (" + jevURL + ")"
	case "local":
		locURL := firstNonEmptyStr(cfg["local_model_url"], os.Getenv("LOCAL_MODEL_URL"), "http://127.0.0.1:11434/v1")
		locKey := firstNonEmptyStr(cfg["local_api_key"], os.Getenv("LOCAL_API_KEY"), os.Getenv("OPENAI_API_KEY"))
		if locKey != "" && firstNonEmptyStr(cfg["local_model_url"], os.Getenv("LOCAL_MODEL_URL")) != "" {
			return true, ""
		}
		modelsURL := strings.TrimRight(locURL, "/")
		if !strings.HasSuffix(modelsURL, "/v1") {
			modelsURL += "/v1"
		}
		modelsURL += "/models"
		if probeHTTPAlive(ctx, modelsURL) {
			return true, ""
		}
		return false, "local OpenAI-compatible server unreachable (" + locURL + ")"
	default:
		return true, ""
	}
}

func selectEvaluator(mgr *localmodel.Manager) memory.Evaluator {
	rawProv := strings.ToLower(strings.TrimSpace(os.Getenv("MEMORY_PROVIDER")))
	if rawProv == "" {
		rawProv = memory.ResolveExplicitEnvProvider()
	}
	switch rawProv {
	case "gemini":
		return adapters.NewGeminiAdapter()
	case "local":
		return adapters.NewOpenAICompatibleAdapter("", "", "")
	case "jev":
		return adapters.NewJevAdapter()
	case "flide":
		return adapters.NewFlideAdapter("", "", "")
	case "gguf":
		// fall through to GGUF adapter below
	default:
		if memory.DefaultProvider() == "local" || !localmodel.IsSupportedPlatform() {
			return adapters.NewOpenAICompatibleAdapter("", "", "")
		}
	}
	var fn func(context.Context) error
	if mgr != nil {
		fn = func(ctx context.Context) error {
			return mgr.EnsureRunning(ctx, "", 0)
		}
	}
	return adapters.NewGGUFAdapter("", "", fn)
}

func maybeAutoBootstrap(ctx context.Context, svc *memory.Service, mgr *localmodel.Manager) {
	if !localmodel.IsSupportedPlatform() {
		return
	}
	cfg := svc.GetConfigMap(ctx)
	if strings.TrimSpace(cfg["active_provider"]) != "" ||
		strings.TrimSpace(os.Getenv("MEMORY_PROVIDER")) != "" ||
		strings.TrimSpace(os.Getenv("GEMINI_API_KEY")) != "" ||
		strings.TrimSpace(os.Getenv("TYPESAFE_API_KEY")) != "" {
		return
	}
	st := mgr.Status(0)
	if len(st.Installed) == 0 && os.Getenv("MEMORY_AUTO_BOOTSTRAP") != "1" {
		return
	}
	_, _, _ = mgr.AutoBootstrap(ctx, 0, func(ep, model string) {
		_ = svc.SaveConfig(context.Background(), map[string]string{
			"active_provider": "gguf",
			"gguf_endpoint":   ep,
			"gguf_model":      model,
		})
	})
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

func runHookInline(svc *memory.Service, mode string, rawStdin []byte, procStart time.Time) {
	ctx := context.Background()
	var cancel context.CancelFunc = func() {}
	if total, ok := resolveConfiguredHookTimeout(mode); ok {
		srvBudget, _, alive := remainingHookBudgets(procStart, total)
		if !alive {
			os.Exit(0)
		}
		ctx, cancel = context.WithTimeout(ctx, srvBudget)
	}
	out, code, _ := svc.ExecuteHook(ctx, mode, rawStdin, "")
	cancel()
	if strings.TrimSpace(out) != "" {
		fmt.Println(out)
	}
	os.Exit(code)
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
		if dataDir := memory.DefaultDataDir(); dataDir != "" {
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

func parseDaemonCtlAction(explicit string) string {
	if act := strings.TrimSpace(explicit); act != "" {
		return act
	}
	if flag.NArg() == 0 {
		return ""
	}
	first := strings.ToLower(strings.TrimSpace(flag.Arg(0)))
	switch first {
	case "daemon", "service", "server":
		if flag.NArg() >= 2 {
			return flag.Arg(1)
		}
		return "status"
	case "status", "start", "stop", "restart", "reload", "logs", "port":
		return first
	default:
		return ""
	}
}

func main() {
	procStart := effectiveProcStart(time.Now())
	showVersion := flag.Bool("version", false, "print memorize version and exit")
	installMode := flag.String("install", "", "install Git pre-commit guardrail hook ('git-hook')")
	daemonMode := flag.Bool("daemon", false, "run single unified background daemon (SQLite + Dashboard + MCP + Hooks)")
	daemonAddr := flag.String("addr", "", "daemon listen address (default 127.0.0.1:18095)")
	daemonCtl := flag.String("daemon-ctl", "", "control background daemon: status, start, stop, restart, logs, port")
	dashAddr := flag.String("dashboard", "", "alias to run unified daemon/dashboard on explicit listen address (e.g. 127.0.0.1:18095)")
	hookMode := flag.String("hook", "", "execute zero-touch lifecycle hook mode (claude-start, claude-turn, claude-pretool, claude-stop, claude-end, antigravity-start, antigravity-turn, antigravity-pretool, antigravity-stop, git-precommit)")
	flag.Parse()

	if *showVersion {
		fmt.Printf("memorize %s\n", version)
		return
	}
	if target := strings.TrimSpace(*installMode); target != "" {
		if err := installTarget(target); err != nil {
			log.Fatalf("install failed: %v", err)
		}
		return
	}

	dbPath, modelsDir := resolveStoragePaths()

	explicitAddr := firstNonEmptyStr(*daemonAddr, *dashAddr)
	if ctlAction := parseDaemonCtlAction(*daemonCtl); ctlAction != "" {
		os.Exit(runDaemonControlCLI(ctlAction, explicitAddr, dbPath, modelsDir))
	}

	resolvedAddr := explicitAddr

	// Fast-path 1: Hook CLI delegates to the unified background daemon (auto-spawning if needed),
	// falling back to inline SQLite execution only when daemon mode is disabled or unreachable.
	if m := strings.TrimSpace(*hookMode); m != "" {
		rawStdin := readHookStdin()
		if !isInlineModeForced(dbPath) {
			if out, code, ok := runHookViaDaemon(resolvedAddr, dbPath, modelsDir, m, rawStdin, procStart); ok {
				if strings.TrimSpace(out) != "" {
					fmt.Println(out)
				}
				os.Exit(code)
			}
		}
		mgr := localmodel.NewManager(modelsDir)
		svc, err := memory.NewService(dbPath, selectEvaluator(mgr))
		if err != nil {
			log.Fatalf("failed to initialize memory service: %v", err)
		}
		defer svc.Close()
		svc.SetAutoGitSync(os.Getenv("MEMORY_AUTO_GIT_SYNC") == "1")
		attachDynamicResolvers(svc, mgr)
		runHookInline(svc, m, rawStdin, procStart)
	}

	// Fast-path 2: Default MCP stdio mode runs as a lightweight stateless bridge to the unified daemon
	// (auto-spawning the daemon if needed), so all open chat sessions share 1 daemon & hot-reload instantly!
	if !*daemonMode && strings.TrimSpace(*dashAddr) == "" && !isInlineModeForced(dbPath) {
		if _, ok := ensureDaemonRunning(resolvedAddr, dbPath, modelsDir); ok {
			if err := runStdioDaemonBridge(resolvedAddr, dbPath, modelsDir); err != nil {
				log.Fatalf("stdio bridge error: %v", err)
			}
			return
		}
	}

	// Unified Daemon Mode (or inline fallback when MEMORY_DISABLE_BG_DASHBOARD=1 / custom isolated test DB)
	mgr := localmodel.NewManager(modelsDir)
	svc, err := memory.NewService(dbPath, selectEvaluator(mgr))
	if err != nil {
		log.Fatalf("failed to initialize memory service: %v", err)
	}
	defer svc.Close()

	svc.SetAutoGitSync(os.Getenv("MEMORY_AUTO_GIT_SYNC") == "1")
	attachDynamicResolvers(svc, mgr)

	maybeAutoBootstrap(context.Background(), svc, mgr)

	if *daemonMode || strings.TrimSpace(*dashAddr) != "" {
		runDaemonServer(resolvedAddr, dbPath, modelsDir, svc, mgr)
		return
	}

	if os.Getenv("MEMORY_DISABLE_BG_DASHBOARD") != "1" {
		if _, closeDash, _ := dashboard.StartBackgroundServer(os.Getenv("MEMORY_DASHBOARD_ADDR"), svc, mgr); closeDash != nil {
			defer closeDash()
		}
	}

	srv := buildMCPServer(svc)
	if err := server.ServeStdio(srv); err != nil {
		log.Fatalf("stdio server error: %v", err)
	}
}
