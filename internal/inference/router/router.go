package router

import (
	"context"
	"database/sql"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"memorize/internal/domain"
	"memorize/internal/inference/localmodel"
	"memorize/internal/inference/providers"
	"memorize/internal/infra/config"
)

// Router manages per-task evaluator selection, local GGUF wake callbacks, engine readiness, and telemetry.
type Router struct {
	mu          sync.RWMutex
	db          *sql.DB
	dbPath      string
	mgr         *localmodel.Manager
	defaultEval domain.Evaluator
	taskRes     func(map[string]string, domain.EvalTask) (domain.Evaluator, bool)
	customRes   func(map[string]string) domain.Evaluator
	readinessFn func(context.Context, map[string]string) (bool, string)
}

// New constructs an inference Router backed by config table settings in db.
func New(db *sql.DB, dbPath string, fallbackEval domain.Evaluator, mgr *localmodel.Manager) *Router {
	r := &Router{
		db:          db,
		dbPath:      dbPath,
		defaultEval: fallbackEval,
	}
	if mgr != nil {
		r.AttachLocalManager(mgr)
	} else {
		r.taskRes = NewTaskResolverWithEndpoint(nil)
	}
	return r
}

// AttachLocalManager binds the local GGUF runtime manager, wiring dynamic port lookup and auto-wake.
func (r *Router) AttachLocalManager(mgr *localmodel.Manager) {
	if r == nil {
		return
	}
	r.mu.Lock()
	r.mgr = mgr
	dbPath := r.dbPath
	db := r.db
	r.mu.Unlock()

	if mgr == nil {
		r.mu.Lock()
		r.taskRes = NewTaskResolverWithEndpoint(nil)
		r.readinessFn = nil
		r.mu.Unlock()
		return
	}
	mgr.SetPreferredPortFunc(func() int {
		return config.LoadPortConfig(dbPath).GGUFPort
	})
	mgr.SetDashboardURLFunc(func() string {
		return config.ActiveDaemonURL(dbPath)
	})
	wake := func(ctx context.Context, model string) (string, error) {
		ep, err := mgr.EnsureRunningEndpoint(ctx, model, 0)
		if err == nil && ep != "" && db != nil {
			_ = config.SyncManagedGGUFEndpoint(ctx, db, dbPath, ep)
		}
		return ep, err
	}
	taskRes := NewTaskResolverWithEndpoint(wake)
	r.mu.Lock()
	r.taskRes = taskRes
	r.readinessFn = func(ctx context.Context, cfg map[string]string) (bool, string) {
		return CheckConfiguredEngineReadiness(ctx, cfg, mgr)
	}
	r.mu.Unlock()
}

// SetTaskResolver overrides the task-aware evaluator resolver (useful for custom test routing).
func (r *Router) SetTaskResolver(fn func(map[string]string, domain.EvalTask) (domain.Evaluator, bool)) {
	r.mu.Lock()
	r.taskRes = fn
	r.mu.Unlock()
}

// SetDynamicResolver overrides the legacy single-evaluator resolver.
func (r *Router) SetDynamicResolver(fn func(map[string]string) domain.Evaluator) {
	r.mu.Lock()
	r.customRes = fn
	r.mu.Unlock()
}

// SetReadinessChecker overrides engine readiness probing.
func (r *Router) SetReadinessChecker(fn func(context.Context, map[string]string) (bool, string)) {
	r.mu.Lock()
	r.readinessFn = fn
	r.mu.Unlock()
}

// EvalForTask resolves the appropriate Evaluator for task from persisted config or the fallback Evaluator.
func (r *Router) EvalForTask(ctx context.Context, task domain.EvalTask) domain.Evaluator {
	if r == nil {
		return nil
	}
	r.mu.RLock()
	taskRes := r.taskRes
	customRes := r.customRes
	defaultEval := r.defaultEval
	db := r.db
	r.mu.RUnlock()

	cfg := config.GetConfigMap(ctx, db)
	if taskRes != nil {
		if ev, handled := taskRes(cfg, task); handled {
			return ev
		}
	}
	if customRes != nil && cfg["active_provider"] != "" {
		if ev := customRes(cfg); ev != nil {
			return ev
		}
	}
	return defaultEval
}

// EvalFor resolves the default evaluator without task overrides.
func (r *Router) EvalFor(ctx context.Context) domain.Evaluator {
	return r.EvalForTask(ctx, "")
}

// ProviderName returns the default resolved engine identifier or "none".
func (r *Router) ProviderName(ctx context.Context) string {
	return r.ProviderNameForTask(ctx, "")
}

// ProviderNameForTask returns the effective resolved engine identifier for task.
func (r *Router) ProviderNameForTask(ctx context.Context, task domain.EvalTask) string {
	ev := r.EvalForTask(ctx, task)
	if ev == nil {
		return "none"
	}
	return ev.Name()
}

// CheckReadiness reports whether the active or default inference engine is configured and reachable.
func (r *Router) CheckReadiness(ctx context.Context) (bool, string) {
	if r == nil {
		return false, "no inference engine configured"
	}
	r.mu.RLock()
	db := r.db
	fn := r.readinessFn
	mgr := r.mgr
	defEval := r.defaultEval
	r.mu.RUnlock()

	cfg := config.GetConfigMap(ctx, db)
	if fn != nil {
		return fn(ctx, cfg)
	}
	if mgr != nil {
		return CheckConfiguredEngineReadiness(ctx, cfg, mgr)
	}
	if strings.TrimSpace(cfg["active_provider"]) != "" || defEval != nil {
		return true, ""
	}
	return false, "no inference engine configured"
}

func providerDefaultModel(prov string, cfg map[string]string) string {
	switch domain.NormalizeProviderID(prov) {
	case "gguf":
		return strings.TrimSpace(cfg["gguf_model"])
	case "gemini":
		return strings.TrimSpace(cfg["gemini_model"])
	case "jev":
		return strings.TrimSpace(cfg["jev_model"])
	case "local":
		return strings.TrimSpace(cfg["local_model_name"])
	default:
		return ""
	}
}

func safeGenerativeModel(prov, rawMod string, cfg map[string]string) string {
	mod := strings.TrimSpace(rawMod)
	if mod == "" {
		mod = providerDefaultModel(prov, cfg)
	}
	if domain.NormalizeProviderID(prov) == "gemini" && domain.IsDecisionOnlySpec("gemini", mod) {
		return "gemini-3.5-flash-lite"
	}
	return mod
}

// NewTaskResolver builds a cached task-aware Evaluator resolver backed by SQLite config.
func NewTaskResolver(autoWake ...func(ctx context.Context, model string) error) func(map[string]string, domain.EvalTask) (domain.Evaluator, bool) {
	var wakeFn func(context.Context, string) (string, error)
	if len(autoWake) > 0 && autoWake[0] != nil {
		raw := autoWake[0]
		wakeFn = func(ctx context.Context, model string) (string, error) {
			return "", raw(ctx, model)
		}
	}
	return NewTaskResolverWithEndpoint(wakeFn)
}

// NewTaskResolverWithEndpoint builds a cached task-aware Evaluator resolver propagating dynamic GGUF endpoints.
func NewTaskResolverWithEndpoint(wakeFn func(ctx context.Context, model string) (string, error)) func(map[string]string, domain.EvalTask) (domain.Evaluator, bool) {
	var mu sync.Mutex
	cache := make(map[string]domain.Evaluator)

	getOrBuild := func(prov, modelOverride string, cfg map[string]string) domain.Evaluator {
		prov = domain.NormalizeProviderID(prov)
		if prov == "" {
			return nil
		}
		ep := strings.TrimSpace(cfg["gguf_endpoint"])
		mod := strings.TrimSpace(modelOverride)
		if mod == "" {
			mod = providerDefaultModel(prov, cfg)
		}
		gemKey := strings.TrimSpace(cfg["gemini_api_key"])
		jevURL := strings.TrimSpace(cfg["jev_api_url"])
		tsKey := strings.TrimSpace(cfg["typesafe_api_key"])
		locURL := strings.TrimSpace(cfg["local_model_url"])
		locKey := strings.TrimSpace(cfg["local_api_key"])

		var key string
		switch prov {
		case "gguf":
			if !localmodel.IsSupportedPlatform() {
				key = strings.Join([]string{"local", locURL, mod, locKey}, "|")
			} else {
				key = strings.Join([]string{"gguf", ep, mod}, "|")
			}
		case "gemini":
			key = strings.Join([]string{"gemini", gemKey, mod}, "|")
		case "local":
			if locKey == "" {
				locKey = tsKey
			}
			key = strings.Join([]string{"local", locURL, mod, locKey}, "|")
		case "jev":
			key = strings.Join([]string{strings.ToLower(strings.TrimSpace(cfg["active_provider"])), "jev", jevURL, mod, tsKey}, "|")
		default:
			return nil
		}

		mu.Lock()
		defer mu.Unlock()
		if ev, ok := cache[key]; ok && ev != nil {
			return ev
		}
		var ev domain.Evaluator
		switch prov {
		case "gguf":
			if !localmodel.IsSupportedPlatform() {
				ev = providers.NewOpenAICompatibleAdapter(locURL, mod, locKey)
				break
			}
			if wakeFn != nil {
				targetMod := mod
				ev = providers.NewGGUFAdapterWithEndpoint(ep, mod, func(ctx context.Context) (string, error) {
					return wakeFn(ctx, targetMod)
				})
			} else {
				ev = providers.NewGGUFAdapter(ep, mod)
			}
		case "gemini":
			ev = providers.NewGeminiAdapterConfig(gemKey, mod)
		case "local":
			ev = providers.NewOpenAICompatibleAdapter(locURL, mod, locKey)
		case "jev":
			rawAct := strings.ToLower(strings.TrimSpace(cfg["active_provider"]))
			if rawAct == "flide" && jevURL == "" {
				ev = providers.NewFlideAdapter("", tsKey, mod)
			} else {
				ev = providers.NewJevAdapterConfig(jevURL, tsKey, mod)
			}
		default:
			return nil
		}
		if len(cache) >= 64 {
			cache = make(map[string]domain.Evaluator)
		}
		cache[key] = ev
		return ev
	}

	return func(cfg map[string]string, task domain.EvalTask) (domain.Evaluator, bool) {
		bulkProv := domain.NormalizeProviderID(cfg["active_provider"])
		bulkMod := providerDefaultModel(bulkProv, cfg)

		spec, hasSpec := domain.FindEvalTask(task)
		if hasSpec {
			ovProv := domain.NormalizeProviderID(cfg[spec.ProviderKey])
			ovMod := strings.TrimSpace(cfg[spec.ModelKey])
			if ovProv == "" && ovMod != "" {
				ovProv = bulkProv
			}
			if ovProv != "" {
				if domain.ValidateTaskRoute(task, ovProv, ovMod) == nil {
					effMod := ovMod
					if effMod == "" && spec.RequiresGenerative {
						effMod = safeGenerativeModel(ovProv, "", cfg)
					}
					if ev := getOrBuild(ovProv, effMod, cfg); ev != nil {
						return ev, true
					}
				}
			}
		}

		if bulkProv == "" {
			return nil, false
		}
		if hasSpec && spec.RequiresGenerative && domain.IsDecisionOnlySpec(bulkProv, bulkMod) {
			genProv := domain.NormalizeProviderID(cfg["gen_provider"])
			genMod := strings.TrimSpace(cfg["gen_model"])
			if genProv != "" && !domain.IsDecisionOnlySpec(genProv, genMod) {
				effMod := safeGenerativeModel(genProv, genMod, cfg)
				if ev := getOrBuild(genProv, effMod, cfg); ev != nil {
					return ev, true
				}
			}
			return nil, true
		}

		ev := getOrBuild(bulkProv, bulkMod, cfg)
		return ev, ev != nil
	}
}

// NewDynamicResolver builds a cached hot-swappable Evaluator resolver backed by SQLite config.
func NewDynamicResolver(autoWake ...func(ctx context.Context, model string) error) func(map[string]string) domain.Evaluator {
	taskRes := NewTaskResolver(autoWake...)
	return func(cfg map[string]string) domain.Evaluator {
		ev, _ := taskRes(cfg, "")
		return ev
	}
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

func firstNonEmptyStr(vals ...string) string {
	for _, v := range vals {
		if t := strings.TrimSpace(v); t != "" {
			return t
		}
	}
	return ""
}

// CheckConfiguredEngineReadiness probes whether the effective inference provider is ready to serve calls.
func CheckConfiguredEngineReadiness(ctx context.Context, cfg map[string]string, mgr *localmodel.Manager) (bool, string) {
	eff := domain.NormalizeProviderID(cfg["active_provider"])
	if eff == "" {
		eff = domain.ResolveExplicitEnvProvider()
	}
	if eff == "" {
		eff = domain.DefaultProvider()
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
		if providers.IsTypesafeCloudURL(jevURL) {
			if firstNonEmptyStr(cfg["typesafe_api_key"], os.Getenv("TYPESAFE_API_KEY"), os.Getenv("FLIDE_API_KEY")) == "" {
				return false, "TYPESAFE_API_KEY not set"
			}
			return true, ""
		}
		base := providers.ResolveSystemOneBaseURL(jevURL)
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

// SelectStartupEvaluator instantiates the initial default Evaluator from env flags and platform defaults.
func SelectStartupEvaluator(mgr *localmodel.Manager) domain.Evaluator {
	rawProv := strings.ToLower(strings.TrimSpace(os.Getenv("MEMORY_PROVIDER")))
	if rawProv == "" {
		rawProv = domain.ResolveExplicitEnvProvider()
	}
	switch rawProv {
	case "gemini":
		return providers.NewGeminiAdapter()
	case "local":
		return providers.NewOpenAICompatibleAdapter("", "", "")
	case "jev":
		return providers.NewJevAdapter()
	case "flide":
		return providers.NewFlideAdapter("", "", "")
	case "gguf":
	default:
		if domain.DefaultProvider() == "local" || !localmodel.IsSupportedPlatform() {
			return providers.NewOpenAICompatibleAdapter("", "", "")
		}
	}
	var fn func(context.Context) error
	if mgr != nil {
		fn = func(ctx context.Context) error {
			return mgr.EnsureRunning(ctx, "", 0)
		}
	}
	return providers.NewGGUFAdapter("", "", fn)
}
