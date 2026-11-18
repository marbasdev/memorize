package router

import (
	"context"
	"database/sql"
	"os"
	"strings"
	"sync"

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
	if customRes != nil && cfg[config.CfgActiveProvider] != "" {
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
	if strings.TrimSpace(cfg[config.CfgActiveProvider]) != "" || defEval != nil {
		return true, ""
	}
	return false, "no inference engine configured"
}

func providerDefaultModel(prov string, cfg map[string]string) string {
	return domain.ProviderModelFromConfig(cfg, prov)
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
	tracker := newCooldownTracker()

	getOrBuild := func(prov, modelOverride string, cfg map[string]string) domain.Evaluator {
		drv, ok := providers.LookupDriver(prov)
		if !ok {
			return nil
		}
		in := providers.DriverBuildInput{
			Config:        cfg,
			ModelOverride: modelOverride,
			RawActiveProv: cfg[config.CfgActiveProvider],
			GGUFSupported: localmodel.IsSupportedPlatform(),
			WakeGGUF:      wakeFn,
		}
		key := drv.CacheKey(in)

		mu.Lock()
		defer mu.Unlock()
		if ev, ok := cache[key]; ok && ev != nil {
			return ev
		}
		ev := drv.Build(in)
		if ev == nil {
			return nil
		}
		if len(cache) >= 64 {
			cache = make(map[string]domain.Evaluator)
		}
		cache[key] = ev
		return ev
	}

	resolveFallback := func(primary domain.Evaluator, primaryProv, primaryMod string, task domain.EvalTask, spec domain.EvalTaskSpec, hasSpec bool, cfg map[string]string) domain.Evaluator {
		if primary == nil {
			return nil
		}
		bulkProv := domain.NormalizeProviderID(cfg[config.CfgActiveProvider])
		bulkMod := providerDefaultModel(bulkProv, cfg)
		genProv := domain.NormalizeProviderID(cfg["gen_provider"])
		genMod := strings.TrimSpace(cfg["gen_model"])

		tryCandidate := func(p, m string) domain.Evaluator {
			p = domain.NormalizeProviderID(p)
			if p == "" {
				return nil
			}
			if hasSpec && spec.RequiresGenerative && domain.IsDecisionOnlySpec(p, m) {
				m = safeGenerativeModel(p, m, cfg)
			}
			if hasSpec && domain.ValidateTaskRoute(task, p, m) != nil {
				return nil
			}
			ev := getOrBuild(p, m, cfg)
			if ev == nil || ev.Name() == primary.Name() {
				return nil
			}
			return ev
		}

		if bulkProv != "" {
			candMod := bulkMod
			if domain.IsDecisionOnlySpec(bulkProv, candMod) {
				candMod = safeGenerativeModel(bulkProv, candMod, cfg)
			}
			if fb := tryCandidate(bulkProv, candMod); fb != nil {
				return fb
			}
		}
		if genProv != "" {
			if fb := tryCandidate(genProv, safeGenerativeModel(genProv, genMod, cfg)); fb != nil {
				return fb
			}
		}
		if domain.NormalizeProviderID(primaryProv) == "gemini" && domain.IsDecisionOnlySpec("gemini", primaryMod) {
			if fb := tryCandidate("gemini", safeGenerativeModel("gemini", primaryMod, cfg)); fb != nil {
				return fb
			}
		}
		return nil
	}

	return func(cfg map[string]string, task domain.EvalTask) (domain.Evaluator, bool) {
		bulkProv := domain.NormalizeProviderID(cfg[config.CfgActiveProvider])
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
						fb := resolveFallback(ev, ovProv, effMod, task, spec, hasSpec, cfg)
						return newFallbackEvaluator(ev, fb, tracker), true
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
		if ev == nil {
			return nil, false
		}
		fb := resolveFallback(ev, bulkProv, bulkMod, task, spec, hasSpec, cfg)
		return newFallbackEvaluator(ev, fb, tracker), true
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

// CheckConfiguredEngineReadiness probes whether the effective inference provider is ready to serve calls.
func CheckConfiguredEngineReadiness(ctx context.Context, cfg map[string]string, mgr *localmodel.Manager) (bool, string) {
	eff := domain.NormalizeProviderID(cfg[config.CfgActiveProvider])
	if eff == "" {
		eff = domain.ResolveExplicitEnvProvider()
	}
	if eff == "" {
		eff = domain.DefaultProvider()
	}
	if eff == "gguf" && !localmodel.IsSupportedPlatform() {
		eff = "local"
	}
	drv, ok := providers.LookupDriver(eff)
	if !ok || drv.CheckReadiness == nil {
		return true, ""
	}
	var probe providers.GGUFRuntimeProbe
	if mgr != nil {
		st := mgr.Status(0)
		probe = providers.GGUFRuntimeProbe{
			Supported:        localmodel.IsSupportedPlatform(),
			ServerRunning:    st.ServerRunning,
			RuntimeInstalled: st.RuntimeInstalled,
			InstalledCount:   len(st.Installed),
			DownloadActive:   st.Download.Active,
			DownloadTarget:   st.Download.Target,
		}
	}
	return drv.CheckReadiness(ctx, providers.DriverBuildInput{
		Config:    cfg,
		GGUFProbe: probe,
	})
}

// SelectStartupEvaluator instantiates the initial default Evaluator from env flags and platform defaults.
func SelectStartupEvaluator(mgr *localmodel.Manager) domain.Evaluator {
	rawProv := strings.ToLower(strings.TrimSpace(os.Getenv("MEMORY_PROVIDER")))
	if rawProv == "" {
		rawProv = domain.ResolveExplicitEnvProvider()
	}
	var ensureFn func(context.Context) error
	if mgr != nil {
		ensureFn = func(ctx context.Context) error {
			return mgr.EnsureRunning(ctx, "", 0)
		}
	}
	return providers.StartupEvaluator(rawProv, providers.DriverBuildInput{
		GGUFSupported: localmodel.IsSupportedPlatform(),
		EnsureGGUF:    ensureFn,
	})
}
