package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"praxis/internal/domain"
	"praxis/internal/inference/localmodel"
	"praxis/internal/infra/config"
	"praxis/internal/infra/git"
	"praxis/internal/infra/sqlite"
	"praxis/internal/modules/memory"
	thttp "praxis/internal/transport/http"
)

type DoctorCheck struct {
	Category string `json:"category"`
	Status   string `json:"status"` // "ok", "warn", "fail", "info"
	Title    string `json:"title"`
	Detail   string `json:"detail"`
}

type DoctorReport struct {
	OK      bool          `json:"ok"`
	Version string        `json:"version"`
	Checks  []DoctorCheck `json:"checks"`
}

func runDoctorCLI(args []string, dbPath, modelsDir, version string, hooks RuntimeHooks) int {
	fs := flag.NewFlagSet("doctor", flag.ContinueOnError)
	jsonFlag := fs.Bool("json", false, "output JSON format")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	effVer := strings.TrimSpace(version)
	if effVer == "" {
		effVer = "dev"
	}

	checks := []DoctorCheck{
		checkDoctorBinary(effVer),
		checkDoctorDaemon(dbPath),
		checkDoctorStorage(ctx, dbPath),
		checkDoctorGitScope(ctx),
		checkDoctorInference(ctx, dbPath, modelsDir),
		checkDoctorRemotes(dbPath),
	}

	allOK := true
	for _, c := range checks {
		if c.Status == "fail" {
			allOK = false
			break
		}
	}

	report := DoctorReport{
		OK:      allOK,
		Version: effVer,
		Checks:  checks,
	}

	if *jsonFlag {
		_ = json.NewEncoder(os.Stdout).Encode(report)
		if allOK {
			return 0
		}
		return 1
	}

	fmt.Println("[praxis doctor]")
	for _, c := range checks {
		tag := "[OK]  "
		switch c.Status {
		case "warn":
			tag = "[WARN]"
		case "fail":
			tag = "[FAIL]"
		case "info":
			tag = "[INFO]"
		}
		fmt.Printf("%s %-18s %s\n", tag, c.Title, c.Detail)
	}
	fmt.Println()
	if allOK {
		fmt.Println("All critical checks passed. System is healthy.")
		return 0
	}
	fmt.Println("One or more checks failed. Review the report above.")
	return 1
}

func checkDoctorBinary(version string) DoctorCheck {
	title := "Binary & Plugin"
	exe, _ := os.Executable()
	if abs, err := filepath.Abs(exe); err == nil {
		exe = abs
	}

	homeDir, _ := os.UserHomeDir()
	if homeDir != "" {
		pluginPath := filepath.Join(homeDir, ".gemini", "config", "plugins", "praxis", "plugin.json")
		if data, err := os.ReadFile(pluginPath); err == nil {
			var p struct {
				Version string `json:"version"`
			}
			if json.Unmarshal(data, &p) == nil && p.Version != "" {
				cleanManifestVer := strings.TrimPrefix(p.Version, "v")
				cleanBinVer := strings.TrimPrefix(version, "v")
				if cleanManifestVer == cleanBinVer || cleanBinVer == "dev" {
					return DoctorCheck{
						Category: "binary",
						Status:   "ok",
						Title:    title,
						Detail:   fmt.Sprintf("v%s (%s, in-sync with Antigravity plugin)", version, exe),
					}
				}
				return DoctorCheck{
					Category: "binary",
					Status:   "warn",
					Title:    title,
					Detail:   fmt.Sprintf("binary v%s (%s) vs plugin manifest v%s (run 'agy plugin install ./plugins/praxis')", version, exe, p.Version),
				}
			}
		}
	}

	return DoctorCheck{
		Category: "binary",
		Status:   "ok",
		Title:    title,
		Detail:   fmt.Sprintf("v%s (%s)", version, exe),
	}
}

func checkDoctorDaemon(dbPath string) DoctorCheck {
	title := "Daemon Process"
	st, targetAddr, ok, authReq := thttp.ProbeLiveDaemonState("", dbPath)
	if targetAddr == "" {
		targetAddr, _ = thttp.ResolveDaemonTargetAddr("", dbPath)
	}
	healthz := thttp.ProbeDaemonHealthz(targetAddr)
	healthzStr := "unreachable"
	if healthz {
		healthzStr = "ok"
	}

	if authReq {
		return DoctorCheck{
			Category: "daemon",
			Status:   "ok",
			Title:    title,
			Detail:   fmt.Sprintf("team server running on http://%s (authenticated, healthz=%s)", targetAddr, healthzStr),
		}
	}
	if ok && st != nil && st.Running {
		fallback := ""
		if st.DaemonFallback {
			fallback = fmt.Sprintf(" [fallback from :%d]", st.ConfiguredDaemonPort)
		}
		return DoctorCheck{
			Category: "daemon",
			Status:   "ok",
			Title:    title,
			Detail:   fmt.Sprintf("running on http://%s%s (pid=%d, uptime=%ds, healthz=%s)", targetAddr, fallback, st.PID, st.UptimeSec, healthzStr),
		}
	}
	return DoctorCheck{
		Category: "daemon",
		Status:   "warn",
		Title:    title,
		Detail:   fmt.Sprintf("stopped on http://%s (auto-spawns on first MCP or hook call)", targetAddr),
	}
}

func checkDoctorStorage(ctx context.Context, dbPath string) DoctorCheck {
	title := "Storage"
	if !config.IsPersistentDBPath(dbPath) {
		return DoctorCheck{
			Category: "storage",
			Status:   "ok",
			Title:    title,
			Detail:   fmt.Sprintf("%s (in-memory mode)", dbPath),
		}
	}

	dbFile, err := os.Stat(dbPath)
	if err != nil {
		if os.IsNotExist(err) {
			return DoctorCheck{
				Category: "storage",
				Status:   "ok",
				Title:    title,
				Detail:   fmt.Sprintf("%s (ready to initialize)", dbPath),
			}
		}
		return DoctorCheck{
			Category: "storage",
			Status:   "fail",
			Title:    title,
			Detail:   fmt.Sprintf("%s (stat error: %v)", dbPath, err),
		}
	}

	db, oerr := sqlite.Open(dbPath)
	if oerr != nil {
		return DoctorCheck{
			Category: "storage",
			Status:   "fail",
			Title:    title,
			Detail:   fmt.Sprintf("%s (open error: %v)", dbPath, oerr),
		}
	}
	defer db.Close()

	ins, _ := sqlite.InspectStorage(ctx, db)
	memCount := 0
	if mem, merr := memory.Open(dbPath, nil); merr == nil {
		if p, qerr := mem.QueryRecordsPage(ctx, domain.RecordQuery{State: "active"}); qerr == nil {
			memCount = p.FilteredActiveCount
			if memCount == 0 {
				memCount = p.TotalRecords
			}
		}
		_ = mem.Close()
	}

	modeStr := ins.JournalMode
	if modeStr == "" {
		modeStr = "WAL"
	}
	bto := ins.BusyTimeout
	if bto == 0 {
		bto = 10000
	}

	permWarn := ""
	if dbFile.Mode().Perm()&0o077 != 0 {
		permWarn = fmt.Sprintf(" [permissions %04o, recommended 0600]", dbFile.Mode().Perm())
	}

	return DoctorCheck{
		Category: "storage",
		Status:   "ok",
		Title:    title,
		Detail:   fmt.Sprintf("%s (mode=%s, busy_timeout=%dms, active_memories=%d%s)", dbPath, strings.ToUpper(modeStr), bto, memCount, permWarn),
	}
}

func checkDoctorGitScope(ctx context.Context) DoctorCheck {
	title := "Git & Scope"
	cwd, _ := os.Getwd()
	scope, root := git.CanonicalScope(ctx, cwd)
	if root == "" {
		return DoctorCheck{
			Category: "git_scope",
			Status:   "warn",
			Title:    title,
			Detail:   fmt.Sprintf("not in a Git repository (falling back to folder scope: %s)", scope),
		}
	}

	hookStatus := "not installed"
	preCommitHook := filepath.Join(root, ".git", "hooks", "pre-commit")
	if b, err := os.ReadFile(preCommitHook); err == nil && strings.Contains(string(b), "praxis") {
		hookStatus = "installed"
	}

	teamRulesNote := ""
	teamRulesPath := filepath.Join(root, ".praxis", "TEAM_RULES.md")
	if b, err := os.ReadFile(teamRulesPath); err == nil {
		ruleCount := 0
		for _, line := range strings.Split(string(b), "\n") {
			if strings.HasPrefix(strings.TrimSpace(line), "- ") {
				ruleCount++
			}
		}
		teamRulesNote = fmt.Sprintf(", team_rules=%d", ruleCount)
	}

	return DoctorCheck{
		Category: "git_scope",
		Status:   "ok",
		Title:    title,
		Detail:   fmt.Sprintf("%s (scope=%s, pre-commit=%s%s)", root, scope, hookStatus, teamRulesNote),
	}
}

func checkDoctorInference(ctx context.Context, dbPath, modelsDir string) DoctorCheck {
	title := "Inference Engine"
	var cfg map[string]string
	if db, err := sqlite.Open(dbPath); err == nil {
		cfg = config.GetConfigMap(ctx, db)
		_ = db.Close()
	}
	if cfg == nil {
		cfg = make(map[string]string)
	}

	eff := domain.NormalizeProviderID(cfg[config.CfgActiveProvider])
	if eff == "" {
		eff = domain.ResolveExplicitEnvProvider()
	}
	if eff == "" {
		eff = domain.DefaultProvider()
	}

	mgr := localmodel.NewManager(modelsDir)
	switch eff {
	case "gguf":
		st := mgr.Status(0)
		modName := cfg[config.CfgGGUFModel]
		if modName == "" && len(st.Installed) > 0 {
			modName = st.Installed[0].FileName
		}
		if modName == "" {
			modName = "none installed"
		}
		if st.ServerRunning {
			return DoctorCheck{
				Category: "inference",
				Status:   "ok",
				Title:    title,
				Detail:   fmt.Sprintf("gguf:%s (running on %s)", modName, st.Endpoint),
			}
		}
		if len(st.Installed) > 0 {
			return DoctorCheck{
				Category: "inference",
				Status:   "ok",
				Title:    title,
				Detail:   fmt.Sprintf("gguf:%s (installed, ready to start)", modName),
			}
		}
		return DoctorCheck{
			Category: "inference",
			Status:   "warn",
			Title:    title,
			Detail:   "gguf (no model downloaded; 1-click install in Web Console -> Engines)",
		}

	case "gemini":
		apiKey := domain.FirstNonEmpty(cfg["gemini_api_key"], os.Getenv("GEMINI_API_KEY"))
		mod := domain.FirstNonEmpty(cfg["gemini_model"], os.Getenv("GEMINI_MODEL"), "gemini-3.5-flash-lite")
		if apiKey != "" {
			return DoctorCheck{
				Category: "inference",
				Status:   "ok",
				Title:    title,
				Detail:   fmt.Sprintf("gemini:%s (API key configured)", mod),
			}
		}
		return DoctorCheck{
			Category: "inference",
			Status:   "warn",
			Title:    title,
			Detail:   fmt.Sprintf("gemini:%s (API key missing; set GEMINI_API_KEY or configure in Web Console)", mod),
		}

	case "local":
		u := domain.FirstNonEmpty(cfg["local_model_url"], os.Getenv("LOCAL_MODEL_URL"), "http://127.0.0.1:11434/v1")
		m := domain.FirstNonEmpty(cfg["local_model_name"], os.Getenv("LOCAL_MODEL_NAME"), "qwen3:4b")
		return DoctorCheck{
			Category: "inference",
			Status:   "ok",
			Title:    title,
			Detail:   fmt.Sprintf("local:%s (%s)", m, u),
		}

	case "jev":
		u := domain.FirstNonEmpty(cfg["jev_api_url"], os.Getenv("JEV_API_URL"), "http://127.0.0.1:8000")
		m := domain.FirstNonEmpty(cfg["jev_model"], os.Getenv("JEV_MODEL"), "jev-latest")
		return DoctorCheck{
			Category: "inference",
			Status:   "ok",
			Title:    title,
			Detail:   fmt.Sprintf("jev:%s (%s)", m, u),
		}

	default:
		return DoctorCheck{
			Category: "inference",
			Status:   "warn",
			Title:    title,
			Detail:   fmt.Sprintf("%s (configure in Web Console -> Engines)", eff),
		}
	}
}

func checkDoctorRemotes(dbPath string) DoctorCheck {
	title := "Team Remote"
	dataDir := resolveClientDataDir(dbPath)
	bindings := config.LoadRemoteBindings(dataDir)

	cwd, _ := os.Getwd()
	if target, ok := config.ResolveRemoteTarget(dataDir, cwd); ok {
		st, err := thttp.QueryDaemonStatusWithAuth(target.URL, target.Token)
		if err == nil && st != nil {
			acc := target.Account
			if acc == "" {
				acc = st.Account
			}
			role := target.Role
			if role == "" {
				role = st.Role
			}
			return DoctorCheck{
				Category: "remotes",
				Status:   "ok",
				Title:    title,
				Detail:   fmt.Sprintf("connected to %s (account: @%s, role: %s)", target.URL, acc, role),
			}
		}
		return DoctorCheck{
			Category: "remotes",
			Status:   "warn",
			Title:    title,
			Detail:   fmt.Sprintf("bound to %s (server unreachable: %v)", target.URL, err),
		}
	}

	if len(bindings.Profiles) > 0 {
		return DoctorCheck{
			Category: "remotes",
			Status:   "ok",
			Title:    title,
			Detail:   fmt.Sprintf("local solo mode (%d saved remote profiles; use 'praxis switch' to connect)", len(bindings.Profiles)),
		}
	}

	return DoctorCheck{
		Category: "remotes",
		Status:   "info",
		Title:    title,
		Detail:   "local solo mode (no remote team server bound)",
	}
}
