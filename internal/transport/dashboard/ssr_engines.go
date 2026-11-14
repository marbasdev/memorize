package dashboard

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"strings"

	"memorize/internal/domain"
	"memorize/internal/inference/discovery"
	"memorize/internal/inference/localmodel"
	"memorize/internal/inference/providers"
	"memorize/internal/infra/config"
)

type ssrEnginesData struct {
	Lang                 string
	OOB                  bool
	EngineSetupWarning   string
	ProviderName         string
	EffectiveProvider    string
	ViewedProvider       string
	ProviderTabs         []ssrTabOption
	Config               map[string]string
	LocalModels          localmodel.StatusSnapshot
	InstalledSet         map[string]bool
	GGUFTier             string
	GGUFTierTabs         []ssrTabOption
	HFSort               string
	HFQuery              string
	HFLive               bool
	HFSource             string
	GGUFCatalog          []localmodel.Preset
	GeminiTier           string
	GeminiTierTabs       []ssrTabOption
	GeminiSource         string
	GeminiAllCount       int
	GeminiModels         []discovery.GeminiModelOption
	JevCloud             bool
	JevURLVal            string
	JevModelVal          string
	JevSourceClass       string
	JevSourceLabel       string
	JevKeyClass          string
	JevKeyLabel          string
	JevModels            []discovery.SystemOneModelOption
	DetectedLocalServers []discovery.DetectedLocalServer
	BulkDecisionOnly     bool
	BulkSummary          string
	CustomRouteCount     int
	GenProviderVal       string
	GenModelVal          string
	GenEffective         string
	KnownGenModels       []string
	KnownAllModels       []string
	GenProviderOptions   []ssrTabOption
	TaskRoutes           []ssrTaskRouteRow
	Telemetry            domain.EvalTelemetryReport
	TelemetryViz         ssrTelemetryViz
}

type ssrTaskRouteRow struct {
	Lang               string
	Task               string
	Label              string
	Desc               string
	RequiresGenerative bool
	RouteProvider      string
	RouteModel         string
	EffectiveEngine    string
	IsHeuristic        bool
	ProviderOptions    []ssrTabOption
	Stats              domain.EvalTaskStats
}

type ggufCatalogResult struct {
	installedSet map[string]bool
	tier         string
	hfSort       string
	hfQuery      string
	hfLive       bool
	hfSource     string
	catalog      []localmodel.Preset
}

func buildGGUFCatalog(ctx context.Context, lm localmodel.StatusSnapshot, viewedProv string, q url.Values) ggufCatalogResult {
	installedSet := make(map[string]bool, len(lm.Installed))
	for _, inst := range lm.Installed {
		installedSet[inst.FileName] = true
	}

	ggufTier := strings.TrimSpace(q.Get("tier"))
	if ggufTier == "" {
		ggufTier = "balanced"
	}
	hfSort := strings.TrimSpace(q.Get("sort"))
	if hfSort == "" {
		hfSort = "latest"
	}
	hfQuery := strings.TrimSpace(q.Get("q"))
	hfLive := false
	hfSource := "Presets"
	catalog := lm.Presets
	if viewedProv == "gguf" && lm.Supported && q.Get("discover_hf") == "1" {
		if live, err := localmodel.DiscoverHuggingFaceModels(ctx, hfSort, hfQuery, q.Get("refresh") == "1"); err == nil && len(live) > 0 {
			hfLive = true
			hfSource = fmt.Sprintf("HF Live (%d)", len(live))
			if hfQuery != "" {
				catalog = live
			} else {
				seen := map[string]bool{}
				merged := make([]localmodel.Preset, 0, len(live)+len(lm.Presets))
				for _, m := range live {
					seen[strings.ToLower(m.Repo)] = true
					merged = append(merged, m)
				}
				for _, p := range lm.Presets {
					if !seen[strings.ToLower(p.Repo)] {
						merged = append(merged, p)
					}
				}
				catalog = merged
			}
		}
	}
	var filtered []localmodel.Preset
	for _, p := range catalog {
		if ggufTier == "all" || p.Tier == ggufTier {
			filtered = append(filtered, p)
		}
	}
	if len(filtered) == 0 && hfQuery != "" && ggufTier != "all" {
		ggufTier = "all"
		filtered = catalog
	}
	return ggufCatalogResult{
		installedSet: installedSet,
		tier:         ggufTier,
		hfSort:       hfSort,
		hfQuery:      hfQuery,
		hfLive:       hfLive,
		hfSource:     hfSource,
		catalog:      filtered,
	}
}

func buildGeminiCatalog(ctx context.Context, rawCfg, cfg map[string]string, viewedProv string, q url.Values, overrideKey string) (tier, source string, allCount int, filtered []discovery.GeminiModelOption, allModels []discovery.GeminiModelOption) {
	tier = strings.TrimSpace(q.Get("gtier"))
	if tier == "" {
		if domain.IsDecisionOnlySpec("gemini", cfg["gemini_model"]) {
			tier = "decision"
		} else {
			tier = "speed"
		}
	}
	allModels = discovery.DefaultGeminiModels()
	source = "builtin"
	if viewedProv == "gemini" && (q.Get("discover_gemini") == "1" || overrideKey != "") {
		allModels, source, _ = discovery.DiscoverGeminiModels(ctx, rawCfg, overrideKey)
		hasDecision := false
		for _, m := range allModels {
			if m.DecisionOnly {
				hasDecision = true
				break
			}
		}
		if !hasDecision {
			for _, dm := range discovery.DefaultGeminiModels() {
				if dm.DecisionOnly {
					allModels = append(allModels, dm)
				}
			}
		}
	}
	for _, m := range allModels {
		if tier == "all" || m.Tier == tier {
			filtered = append(filtered, m)
		}
	}
	return tier, source, len(allModels), filtered, allModels
}

type jevCatalogResult struct {
	cloud       bool
	urlVal      string
	modelVal    string
	sourceClass string
	sourceLabel string
	keyClass    string
	keyLabel    string
	models      []discovery.SystemOneModelOption
}

func buildJevCatalog(ctx context.Context, lang string, rawCfg, cfg map[string]string, viewedProv string, q url.Values, overrideKey, overrideURL string) jevCatalogResult {
	jevURL := cfg["jev_api_url"]
	jevModel := cfg["jev_model"]
	switch q.Get("jev_preset") {
	case "flide":
		jevURL = "http://127.0.0.1:8000"
		jevModel = "jev-latest"
	case "typesafe":
		jevURL = "https://api.typesafe.ai/v1/sys" + "temone"
		jevModel = "jev-1.13.0"
	}
	if overrideURL != "" {
		jevURL = overrideURL
	}
	jevCloud := providers.IsTypesafeCloudURL(jevURL)
	var jevDisc discovery.SystemOneDiscovery
	if viewedProv == "jev" && (q.Get("discover_jev") == "1" || overrideURL != "" || overrideKey != "") {
		jevDisc = discovery.DiscoverSystemOneEndpoint(ctx, rawCfg, jevURL, overrideKey)
	}
	jevModels := jevDisc.Models
	if len(jevModels) == 0 {
		if jevCloud {
			jevModels = discovery.DefaultTypesafeJevModels()
		} else {
			jevModels = discovery.DefaultFlideModels()
		}
	}

	jevSrcClass, jevSrcLabel := "b-muted", "Flide Local"
	switch {
	case jevDisc.HealthStatus == "ok":
		jevSrcClass = "b-ok"
		jevSrcLabel = "Flide Ready"
		if jevDisc.ServedModel != "" {
			jevSrcLabel += " · " + jevDisc.ServedModel
		}
	case jevDisc.HealthStatus == "loading":
		jevSrcClass, jevSrcLabel = "b-warn", "Flide Loading..."
	case jevCloud || jevDisc.HealthStatus == "cloud":
		jevSrcClass, jevSrcLabel = "b-accent", "TypeSafe Cloud"
	case jevDisc.Source != "":
		jevSrcClass, jevSrcLabel = "b-muted", "Flide Offline (Presets)"
	}

	jevKeyClass, jevKeyLabel := "b-muted", "No Auth (Flide)"
	if cfg["typesafe_key_status"] != "" {
		jevKeyClass, jevKeyLabel = "b-ok", cfg["typesafe_key_status"]
	} else if jevCloud {
		jevKeyClass, jevKeyLabel = "b-warn", uiText(lang, "key_missing")
	}

	return jevCatalogResult{
		cloud:       jevCloud,
		urlVal:      jevURL,
		modelVal:    jevModel,
		sourceClass: jevSrcClass,
		sourceLabel: jevSrcLabel,
		keyClass:    jevKeyClass,
		keyLabel:    jevKeyLabel,
		models:      jevModels,
	}
}

func buildTaskRouteRows(ctx context.Context, d Deps, lang string, cfg map[string]string, windowHours int) ([]ssrTaskRouteRow, int, domain.EvalTelemetryReport) {
	inheritGenOpts := []ssrTabOption{
		{ID: "", Label: uiText(lang, "route_inherit")},
		{ID: "gemini", Label: uiText(lang, "prov_gemini")},
		{ID: "gguf", Label: uiText(lang, "prov_gguf")},
		{ID: "local", Label: uiText(lang, "prov_local")},
	}
	inheritAllOpts := []ssrTabOption{
		{ID: "", Label: uiText(lang, "route_inherit")},
		{ID: "gemini", Label: uiText(lang, "prov_gemini")},
		{ID: "gguf", Label: uiText(lang, "prov_gguf")},
		{ID: "jev", Label: uiText(lang, "prov_jev")},
		{ID: "local", Label: uiText(lang, "prov_local")},
	}

	telemetry, _ := d.Memory.Router().SummarizeTelemetry(ctx, windowHours)
	customCount := 0
	specs := domain.AllEvalTasks()
	rows := make([]ssrTaskRouteRow, 0, len(specs))
	for _, spec := range specs {
		rp := cfg[spec.ProviderKey]
		rm := cfg[spec.ModelKey]
		if rp != "" || rm != "" {
			customCount++
		}
		effName := d.Memory.Router().ProviderNameForTask(ctx, spec.Task)
		isHeur := spec.RequiresGenerative && (effName == "" || effName == "none")
		if isHeur {
			effName = uiText(lang, "route_eff_heuristic")
		}
		opts := inheritAllOpts
		if spec.RequiresGenerative {
			opts = inheritGenOpts
		}
		rows = append(rows, ssrTaskRouteRow{
			Lang:               lang,
			Task:               string(spec.Task),
			Label:              uiText(lang, "task_"+string(spec.Task)+"_lbl"),
			Desc:               uiText(lang, "task_"+string(spec.Task)+"_desc"),
			RequiresGenerative: spec.RequiresGenerative,
			RouteProvider:      rp,
			RouteModel:         rm,
			EffectiveEngine:    effName,
			IsHeuristic:        isHeur,
			ProviderOptions:    opts,
			Stats:              telemetry.ByTask[string(spec.Task)],
		})
	}
	return rows, customCount, telemetry
}

func buildEnginesView(ctx context.Context, d Deps, lang string, q url.Values, overrideKey, overrideURL string) ssrEnginesData {
	provName := d.Memory.ProviderName()
	rawCfg := d.Memory.GetConfigMap(ctx)
	portCfg := config.LoadPortConfig(d.Memory.DBPath())
	cfg := discovery.PublicConfigWithGGUFPort(rawCfg, provName, portCfg.GGUFPort)
	lm := d.LocalModel.Status(portCfg.GGUFPort)

	effProv := cfg["effective_provider"]
	if effProv == "" {
		if lm.Supported {
			effProv = "gguf"
		} else {
			effProv = "local"
		}
	}
	viewedProv := strings.ToLower(strings.TrimSpace(q.Get("view")))
	switch viewedProv {
	case "gguf", "gemini", "jev", "local":
	default:
		viewedProv = effProv
	}

	gguf := buildGGUFCatalog(ctx, lm, viewedProv, q)
	gemTier, gemSource, gemAllCount, filteredGem, gemModels := buildGeminiCatalog(ctx, rawCfg, cfg, viewedProv, q, overrideKey)
	jev := buildJevCatalog(ctx, lang, rawCfg, cfg, viewedProv, q, overrideKey, overrideURL)

	bulkMod := cfg["gguf_model"]
	switch effProv {
	case "gemini":
		bulkMod = cfg["gemini_model"]
	case "jev":
		bulkMod = cfg["jev_model"]
	case "local":
		bulkMod = cfg["local_model_name"]
	}
	bulkDecOnly := domain.IsDecisionOnlySpec(effProv, bulkMod)
	bulkSummary := effProv
	if bulkMod != "" {
		bulkSummary += ":" + bulkMod
	}

	genOnlyOpts := []ssrTabOption{
		{ID: "", Label: uiText(lang, "gen_none_heuristic")},
		{ID: "gemini", Label: uiText(lang, "prov_gemini")},
		{ID: "gguf", Label: uiText(lang, "prov_gguf")},
		{ID: "local", Label: uiText(lang, "prov_local")},
	}
	winID, winHours := parseTelemetryWindow(q.Get("tw"))
	taskRows, customCount, evalTelemetry := buildTaskRouteRows(ctx, d, lang, cfg, winHours)

	genEff := d.Memory.Router().ProviderNameForTask(ctx, domain.TaskConsolidate)
	if genEff == "" || genEff == "none" {
		genEff = uiText(lang, "route_eff_heuristic")
	}
	setupWarn := buildEngineSetupWarning(lang, rawCfg, cfg, provName, lm, jev)
	var detectedLocal []discovery.DetectedLocalServer
	if setupWarn != "" || viewedProv == "local" || q.Get("probe_local") == "1" {
		detectedLocal = discovery.ProbeLocalInferenceServers(ctx)
	}

	return ssrEnginesData{
		Lang:                 lang,
		EngineSetupWarning:   setupWarn,
		DetectedLocalServers: detectedLocal,
		ProviderName:         provName,
		EffectiveProvider:    effProv,
		ViewedProvider:       viewedProv,
		ProviderTabs: []ssrTabOption{
			{ID: "gguf", Label: uiText(lang, "prov_gguf")},
			{ID: "gemini", Label: uiText(lang, "prov_gemini")},
			{ID: "jev", Label: uiText(lang, "prov_jev")},
			{ID: "local", Label: uiText(lang, "prov_local")},
		},
		Config:       cfg,
		LocalModels:  lm,
		InstalledSet: gguf.installedSet,
		GGUFTier:     gguf.tier,
		GGUFTierTabs: []ssrTabOption{
			{ID: "balanced", Label: uiText(lang, "tier_balanced")},
			{ID: "speed", Label: uiText(lang, "tier_speed")},
			{ID: "accuracy", Label: uiText(lang, "tier_accuracy")},
			{ID: "all", Label: uiText(lang, "tier_all")},
		},
		HFSort:             gguf.hfSort,
		HFQuery:            gguf.hfQuery,
		HFLive:             gguf.hfLive,
		HFSource:           gguf.hfSource,
		GGUFCatalog:        gguf.catalog,
		GeminiTier:         gemTier,
		GeminiTierTabs:     []ssrTabOption{{ID: "speed", Label: "Flash-Lite"}, {ID: "decision", Label: uiText(lang, "tier_decision")}, {ID: "balanced", Label: "Flash"}, {ID: "accuracy", Label: "Pro"}, {ID: "all", Label: uiText(lang, "tier_all")}},
		GeminiSource:       gemSource,
		GeminiAllCount:     gemAllCount,
		GeminiModels:       filteredGem,
		JevCloud:           jev.cloud,
		JevURLVal:          jev.urlVal,
		JevModelVal:        jev.modelVal,
		JevSourceClass:     jev.sourceClass,
		JevSourceLabel:     jev.sourceLabel,
		JevKeyClass:        jev.keyClass,
		JevKeyLabel:        jev.keyLabel,
		JevModels:          jev.models,
		BulkDecisionOnly:   bulkDecOnly,
		BulkSummary:        bulkSummary,
		CustomRouteCount:   customCount,
		GenProviderVal:     cfg["gen_provider"],
		GenModelVal:        cfg["gen_model"],
		GenEffective:       genEff,
		KnownGenModels:     buildKnownModelSuggestions(gemModels, lm, cfg, true),
		KnownAllModels:     buildKnownModelSuggestions(gemModels, lm, cfg, false),
		GenProviderOptions: genOnlyOpts,
		TaskRoutes:         taskRows,
		Telemetry:          evalTelemetry,
		TelemetryViz:       buildTelemetryViz(lang, winID, evalTelemetry),
	}
}

func buildKnownModelSuggestions(gemModels []discovery.GeminiModelOption, lm localmodel.StatusSnapshot, cfg map[string]string, generativeOnly bool) []string {
	seen := map[string]bool{}
	var out []string
	add := func(id string) {
		id = strings.TrimSpace(id)
		if id == "" || seen[id] {
			return
		}
		if generativeOnly && domain.IsDecisionOnlySpec("", id) {
			return
		}
		seen[id] = true
		out = append(out, id)
	}
	for _, m := range gemModels {
		if generativeOnly && m.DecisionOnly {
			continue
		}
		add(m.ID)
	}
	add(cfg["gemini_model"])
	add(cfg["gen_model"])
	for _, inst := range lm.Installed {
		add(inst.FileName)
	}
	add(cfg["gguf_model"])
	add(cfg["local_model_name"])
	if !generativeOnly {
		add("gemini-decision-1")
		add("jev-latest")
		add(cfg["jev_model"])
	}
	return out
}

func setupMsg(lang, ko, en string) string {
	if normalizeLang(lang) == "ko" {
		return ko
	}
	return en
}

func buildEngineSetupWarning(lang string, rawCfg, cfg map[string]string, provName string, lm localmodel.StatusSnapshot, jev jevCatalogResult) string {
	low := strings.ToLower(strings.TrimSpace(provName))
	eff := domain.NormalizeProviderID(rawCfg["active_provider"])
	if eff == "" {
		eff = domain.ResolveExplicitEnvProvider()
	}
	if eff == "" {
		switch {
		case low == "none":
			eff = "none"
		case strings.HasPrefix(low, "gguf:"):
			eff = "gguf"
		case strings.HasPrefix(low, "gemini:"):
			eff = "gemini"
		case strings.HasPrefix(low, "local:"):
			eff = "local"
		case strings.HasPrefix(low, "jev:") || strings.HasPrefix(low, "flide:"):
			eff = "jev"
		case low != "":
			return ""
		default:
			eff = domain.DefaultProvider()
		}
	}

	switch eff {
	case "none":
		return setupMsg(lang,
			"활성화된 추론 엔진이 없습니다. 추론 엔진 탭에서 로컬 GGUF 다운로드 또는 Gemini · 로컬 URL · System 1을 설정하세요.",
			"No inference engine is configured. Open the Engines tab to download Local GGUF or configure Gemini, Custom Local URL, or System 1.")
	case "gguf":
		if !lm.Supported {
			return checkDashboardLocalSetup(lang, rawCfg)
		}
		if len(lm.Installed) == 0 && !lm.ServerRunning && !lm.Download.Active {
			return setupMsg(lang,
				"설치된 로컬 GGUF 모델이 없습니다. 추론 엔진 탭에서 권장 모델을 1클릭 다운로드하거나 Gemini · 로컬 URL · System 1을 설정하세요.",
				"No local GGUF model is installed yet. Open the Engines tab to download a model in 1 click or configure Gemini, Custom Local URL, or System 1.")
		}
		if len(lm.Installed) > 0 && !lm.RuntimeInstalled && !lm.ServerRunning && !lm.Download.Active {
			return setupMsg(lang,
				"로컬 GGUF 모델은 있으나 llama-server 런타임이 설치되지 않았습니다. 추론 엔진 탭에서 런타임을 설치하거나 다른 엔진을 선택하세요.",
				"A local GGUF model is present, but the llama-server runtime is not installed yet. Open the Engines tab to install it or select another engine.")
		}
	case "gemini":
		if strings.TrimSpace(cfg["gemini_key_status"]) == "" {
			return setupMsg(lang,
				"Gemini 엔진이 선택되었으나 GEMINI_API_KEY가 비어 있습니다. 추론 엔진 탭에서 API 키를 입력하고 저장하세요.",
				"Gemini is selected, but GEMINI_API_KEY is not set. Enter and save your API key in the Engines tab.")
		}
	case "jev":
		if jev.cloud && strings.TrimSpace(cfg["typesafe_key_status"]) == "" && strings.TrimSpace(os.Getenv("FLIDE_API_KEY")) == "" {
			return setupMsg(lang,
				"TypeSafe Cloud System 1이 선택되었으나 TYPESAFE_API_KEY가 비어 있습니다. 추론 엔진 탭에서 API 키를 입력하거나 로컬 서버 URL을 지정하세요.",
				"TypeSafe Cloud System 1 is selected, but TYPESAFE_API_KEY is missing. Enter your key in the Engines tab or configure a local server URL.")
		}
	case "local":
		return checkDashboardLocalSetup(lang, rawCfg)
	}
	return ""
}

func checkDashboardLocalSetup(lang string, rawCfg map[string]string) string {
	if strings.TrimSpace(rawCfg["active_provider"]) == "" &&
		strings.TrimSpace(os.Getenv("MEMORY_PROVIDER")) == "" &&
		strings.TrimSpace(rawCfg["local_model_url"]) == "" &&
		strings.TrimSpace(os.Getenv("LOCAL_MODEL_URL")) == "" {
		return setupMsg(lang,
			"추론 엔진이 아직 설정되지 않았습니다 (기본 로컬 URL: http://127.0.0.1:11434/v1). 추론 엔진 탭에서 Ollama · LM Studio, System 1, 또는 Gemini를 연결하세요.",
			"No inference engine is configured yet (default expects a local server at http://127.0.0.1:11434/v1). Open the Engines tab to configure Ollama, LM Studio, System 1, or Gemini.")
	}
	return ""
}
