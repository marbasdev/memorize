package dashboard

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"memorize/internal/memory"
	"memorize/internal/memory/adapters"
)

func normalizePresetCategoriesJSON(raw string) string {
	s := strings.TrimSpace(raw)
	if s == "" || s == "reset" {
		return memory.DefaultPresetCategoriesJSON
	}
	var parsed map[string][]string
	if err := json.Unmarshal([]byte(s), &parsed); err != nil {
		return memory.DefaultPresetCategoriesJSON
	}
	clean := make(map[string][]string, len(parsed))
	for k, subs := range parsed {
		cat := memory.NormalizeCategoryForScope("global", k)
		if cat == "" {
			continue
		}
		seen := make(map[string]bool, len(subs))
		var subList []string
		for _, sub := range subs {
			sc := memory.NormalizeSubCategory(sub)
			if sc != "" && !seen[sc] {
				seen[sc] = true
				subList = append(subList, sc)
			}
		}
		sort.Strings(subList)
		clean[cat] = subList
	}
	if len(clean) == 0 {
		return memory.DefaultPresetCategoriesJSON
	}
	b, err := json.Marshal(clean)
	if err != nil {
		return memory.DefaultPresetCategoriesJSON
	}
	return string(b)
}

var geminiCatalogClient = &http.Client{
	Timeout: 6 * time.Second,
	CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
		return http.ErrUseLastResponse
	},
}

func envOrDefault(raw, envVar, fallback string) string {
	if s := strings.TrimSpace(raw); s != "" {
		return s
	}
	if s := strings.TrimSpace(os.Getenv(envVar)); s != "" {
		return s
	}
	return fallback
}

func maskSecret(raw, envVar string) string {
	s := strings.TrimSpace(raw)
	src := "Stored in DB"
	if s == "" {
		s = strings.TrimSpace(os.Getenv(envVar))
		src = "Env " + envVar
	}
	if s == "" {
		return ""
	}
	if len(s) <= 8 {
		return src + " (••••)"
	}
	return src + " (" + s[:4] + "••••" + s[len(s)-4:] + ")"
}

func publicConfig(raw map[string]string, providerName string) map[string]string {
	return publicConfigWithGGUFPort(raw, providerName, memory.DefaultGGUFPort)
}

func publicConfigWithGGUFPort(raw map[string]string, providerName string, ggufPort int) map[string]string {
	if ggufPort < 1 || ggufPort > 65535 {
		ggufPort = memory.DefaultGGUFPort
	}
	defaultGGUFURL := fmt.Sprintf("http://127.0.0.1:%d", ggufPort)
	eff := strings.ToLower(strings.TrimSpace(raw["active_provider"]))
	if eff == "flide" {
		eff = "jev"
	}
	if eff == "" {
		low := strings.ToLower(providerName)
		switch {
		case strings.HasPrefix(low, "gguf"):
			eff = "gguf"
		case strings.HasPrefix(low, "gemini"):
			eff = "gemini"
		case low == "local" || strings.HasPrefix(low, "local:"):
			eff = "local"
		case strings.HasPrefix(low, "jev") || strings.HasPrefix(low, "flide"):
			eff = "jev"
		default:
			eff = memory.DefaultProvider()
		}
	}
	defaultTypesafeURL := "https://api.typesafe.ai/v1/sys" + "temone"
	jevURL := envOrDefault(raw["jev_api_url"], "JEV_API_URL", defaultTypesafeURL)
	defaultJevModel := "jev-latest"
	if adapters.IsTypesafeCloudURL(jevURL) {
		defaultJevModel = "jev-1.13.0"
	}
	res := map[string]string{
		"active_provider":             raw["active_provider"],
		"effective_provider":          eff,
		"gen_provider":                memory.NormalizeProviderID(raw["gen_provider"]),
		"gen_model":                   strings.TrimSpace(raw["gen_model"]),
		"gguf_endpoint":               envOrDefault(raw["gguf_endpoint"], "LOCAL_GGUF_URL", defaultGGUFURL),
		"gguf_model":                  raw["gguf_model"],
		"gemini_model":                envOrDefault(raw["gemini_model"], "GEMINI_MODEL", "gemini-3.5-flash-lite"),
		"gemini_key_status":           maskSecret(raw["gemini_api_key"], "GEMINI_API_KEY"),
		"jev_api_url":                 jevURL,
		"jev_model":                   envOrDefault(raw["jev_model"], "JEV_MODEL", defaultJevModel),
		"typesafe_key_status":         maskSecret(raw["typesafe_api_key"], "TYPESAFE_API_KEY"),
		"local_model_url":             envOrDefault(raw["local_model_url"], "LOCAL_MODEL_URL", "http://127.0.0.1:11434/v1"),
		"local_model_name":            envOrDefault(raw["local_model_name"], "LOCAL_MODEL_NAME", "qwen3:4b"),
		"local_key_status":            maskSecret(raw["local_api_key"], "LOCAL_API_KEY"),
		"include_personal_in_project": envOrDefault(raw["include_personal_in_project"], "MEMORY_INCLUDE_PERSONAL", "0"),
		"preset_categories":           normalizePresetCategoriesJSON(raw["preset_categories"]),
	}
	for _, spec := range memory.AllEvalTasks() {
		res[spec.ProviderKey] = memory.NormalizeProviderID(raw[spec.ProviderKey])
		res[spec.ModelKey] = strings.TrimSpace(raw[spec.ModelKey])
	}
	return res
}

type geminiModelOption struct {
	ID            string `json:"id"`
	Name          string `json:"name"`
	Tier          string `json:"tier"` // "speed", "decision", "balanced", "accuracy"
	DecisionOnly  bool   `json:"decision_only,omitempty"`
	Recommended   bool   `json:"recommended"`
	ContextTokens int    `json:"context_tokens,omitempty"`
	Label         string `json:"label"`
	Desc          string `json:"desc"`
}

func defaultGeminiModels() []geminiModelOption {
	return []geminiModelOption{
		{
			ID: "gemini-3.5-flash-lite", Name: "Gemini 3.5 Flash-Lite", Tier: "speed", Recommended: true, ContextTokens: 1048576,
			Label: "Low-Latency · Gemini 3.5 Flash-Lite",
			Desc:  "Fastest Flash-Lite endpoint for full-scope scoring and structured JSON classification",
		},
		{
			ID: "gemini-3.1-flash-lite", Name: "Gemini 3.1 Flash-Lite", Tier: "speed", ContextTokens: 1048576,
			Label: "Lightweight · Gemini 3.1 Flash-Lite",
			Desc:  "Cost-efficient lightweight endpoint for background evaluation",
		},
		{
			ID: "gemini-decision-1", Name: "Gemini Decision 1 (0 Output Tokens)", Tier: "decision", DecisionOnly: true, Recommended: true, ContextTokens: 262144,
			Label: "Decision-Only · Gemini Decision 1",
			Desc:  "Single-pass closed-set decision/guardrail/rubric model (0 output tokens · cannot generate free-form text)",
		},
		{
			ID: "gemini-3.8-flash", Name: "Gemini 3.8 Flash", Tier: "balanced", Recommended: true, ContextTokens: 1048576,
			Label: "Balanced · Gemini 3.8 Flash",
			Desc:  "Balanced latency and semantic reasoning for rule conflict detection",
		},
		{
			ID: "gemini-3.6-flash", Name: "Gemini 3.6 Flash", Tier: "balanced", ContextTokens: 1048576,
			Label: "Balanced · Gemini 3.6 Flash",
			Desc:  "Standard Flash endpoint with consistent latency and accuracy",
		},
		{
			ID: "gemini-3.1-pro-preview", Name: "Gemini 3.1 Pro Preview", Tier: "accuracy", Recommended: true, ContextTokens: 1048576,
			Label: "High-Precision · Gemini 3.1 Pro Preview",
			Desc:  "Deep reasoning for multi-rule supersession and plan violation verification",
		},
	}
}

var geminiVerRe = regexp.MustCompile(`^gemini-(\d+(?:\.\d+)?)`)

func geminiVersionNum(id string) float64 {
	m := geminiVerRe.FindStringSubmatch(strings.ToLower(id))
	if len(m) < 2 {
		return 0
	}
	v, _ := strconv.ParseFloat(m[1], 64)
	return v
}

func isCanonicalGeminiAlias(id string) bool {
	low := strings.ToLower(id)
	for _, suf := range []string{"-001", "-002", "-exp", "preview-", "-latest"} {
		if strings.Contains(low, suf) {
			return false
		}
	}
	return true
}

func discoverGeminiModels(ctx context.Context, rawCfg map[string]string, overrideKey string) ([]geminiModelOption, string, string) {
	apiKey := envOrDefault(overrideKey, "", envOrDefault(rawCfg["gemini_api_key"], "GEMINI_API_KEY", ""))
	if apiKey == "" {
		return defaultGeminiModels(), "builtin", ""
	}
	base := strings.TrimRight(envOrDefault("", "GEMINI_API_BASE", "https://generativelanguage.googleapis.com"), "/")
	cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(cctx, http.MethodGet, base+"/v1beta/models?pageSize=100", nil)
	if err != nil {
		return defaultGeminiModels(), "builtin", err.Error()
	}
	req.Header.Set("x-goog-api-key", apiKey)
	resp, err := geminiCatalogClient.Do(req)
	if err != nil {
		return defaultGeminiModels(), "builtin", err.Error()
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 300 {
		errBytes, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		var apiErr struct {
			Error struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		msg := fmt.Sprintf("HTTP %d", resp.StatusCode)
		if json.Unmarshal(errBytes, &apiErr) == nil && apiErr.Error.Message != "" {
			msg = apiErr.Error.Message
		}
		return defaultGeminiModels(), "builtin", msg
	}

	var payload struct {
		Models []struct {
			Name                       string   `json:"name"`
			DisplayName                string   `json:"displayName"`
			Description                string   `json:"description"`
			InputTokenLimit            int      `json:"inputTokenLimit"`
			SupportedGenerationMethods []string `json:"supportedGenerationMethods"`
		} `json:"models"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&payload); err != nil {
		return defaultGeminiModels(), "builtin", err.Error()
	}

	var list []geminiModelOption
	for _, m := range payload.Models {
		id := strings.TrimPrefix(m.Name, "models/")
		low := strings.ToLower(id)
		if !strings.HasPrefix(low, "gemini-") {
			continue
		}
		for _, skip := range []string{"tts", "image", "embed", "vision", "audio", "live", "veo", "imagen", "robotics", "computer-use", "aqa", "1.0-", "1.5-", "2.0-"} {
			if strings.Contains(low, skip) {
				id = ""
				break
			}
		}
		if id == "" {
			continue
		}
		canGen, canDec := false, strings.HasPrefix(low, "gemini-decision")
		for _, gm := range m.SupportedGenerationMethods {
			if gm == "generateContent" {
				canGen = true
			}
			if strings.Contains(strings.ToLower(gm), "decision") {
				canDec = true
			}
		}
		if !canGen && !canDec {
			continue
		}
		tier := "balanced"
		badge := "Balanced"
		roleHint := "Balanced Flash reasoning model"
		decisionOnly := strings.HasPrefix(low, "gemini-decision") || (canDec && !canGen)
		switch {
		case decisionOnly:
			tier = "decision"
			badge = "Decision-Only"
			roleHint = "Closed-set probability/guardrail/rubric model (0 output tokens · cannot generate text)"
		case strings.Contains(low, "flash-lite") || strings.Contains(low, "lite"):
			tier = "speed"
			badge = "Low-Latency"
			roleHint = "Fast, low-cost full-scope evaluation"
		case strings.Contains(low, "pro"):
			tier = "accuracy"
			badge = "High-Precision"
			roleHint = "High-precision constraint and plan verification"
		}
		disp := strings.TrimSpace(m.DisplayName)
		if disp == "" {
			disp = id
		}
		desc := strings.TrimSpace(m.Description)
		if desc == "" {
			desc = roleHint + " (Live Google API)"
		} else if len(desc) > 130 {
			desc = desc[:130] + "..."
		}
		list = append(list, geminiModelOption{
			ID:            id,
			Name:          disp,
			Tier:          tier,
			DecisionOnly:  decisionOnly,
			ContextTokens: m.InputTokenLimit,
			Label:         badge + " · " + disp,
			Desc:          desc,
		})
	}
	if len(list) == 0 {
		return defaultGeminiModels(), "builtin", ""
	}
	tierOrder := map[string]int{"speed": 0, "decision": 1, "balanced": 2, "accuracy": 3}
	sort.Slice(list, func(i, j int) bool {
		if tierOrder[list[i].Tier] != tierOrder[list[j].Tier] {
			return tierOrder[list[i].Tier] < tierOrder[list[j].Tier]
		}
		vi, vj := geminiVersionNum(list[i].ID), geminiVersionNum(list[j].ID)
		if vi != vj {
			return vi > vj
		}
		ci, cj := isCanonicalGeminiAlias(list[i].ID), isCanonicalGeminiAlias(list[j].ID)
		if ci != cj {
			return ci
		}
		return list[i].ID > list[j].ID
	})
	topMarked := map[string]bool{}
	for i := range list {
		if !topMarked[list[i].Tier] {
			list[i].Recommended = true
			topMarked[list[i].Tier] = true
		}
	}
	return list, "live_api", ""
}

var systemOneClient = &http.Client{
	Timeout: 2500 * time.Millisecond,
	CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
		return http.ErrUseLastResponse
	},
}

func sameURLHost(rawA, rawB string) bool {
	uA, errA := url.Parse(strings.TrimSpace(rawA))
	uB, errB := url.Parse(strings.TrimSpace(rawB))
	if errA != nil || errB != nil || uA.Hostname() == "" || uB.Hostname() == "" {
		return false
	}
	return strings.EqualFold(uA.Hostname(), uB.Hostname())
}

type systemOneModelOption struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Engine      string `json:"engine"` // "flide" or "jev"
	Recommended bool   `json:"recommended"`
	Desc        string `json:"desc"`
}

type systemOneDiscovery struct {
	Source       string                 `json:"source"`        // "live_flide", "typesafe_cloud", "builtin"
	Engine       string                 `json:"engine"`        // "flide" or "jev"
	HealthStatus string                 `json:"health_status"` // "ok", "loading", "cloud", "offline"
	ServedModel  string                 `json:"served_model,omitempty"`
	Format       string                 `json:"format,omitempty"`
	Version      string                 `json:"version,omitempty"`
	Models       []systemOneModelOption `json:"models"`
	APIError     string                 `json:"api_error,omitempty"`
}

func defaultFlideModels() []systemOneModelOption {
	return []systemOneModelOption{
		{
			ID: "jev-latest", Name: "jev-latest (Universal Alias)", Engine: "flide", Recommended: true,
			Desc: "Universal Flide alias — automatically routes to whichever model the Flide server has loaded",
		},
		{
			ID: "OmniJev/OneJev1.6-0.8B", Name: "OmniJev/OneJev1.6-0.8B", Engine: "flide", Recommended: true,
			Desc: "Dedicated 0.8B System 1 decision engine (~1.6 GB VRAM, sub-10ms single-token probability)",
		},
		{
			ID: "perplexity-ai/pplx-decider-flash-v1.2", Name: "pplx-decider-flash-v1.2", Engine: "flide",
			Desc: "High-precision System 1 decider model supported by OmniJev/Flide",
		},
	}
}

func defaultTypesafeJevModels() []systemOneModelOption {
	return []systemOneModelOption{
		{
			ID: "jev-1.13.0", Name: "TypeSafe Jev 1.13.0", Engine: "jev", Recommended: true,
			Desc: "Managed TypeSafe Cloud System 1 decision engine",
		},
		{
			ID: "jev-latest", Name: "jev-latest", Engine: "jev",
			Desc: "Rolling latest System 1 model alias",
		},
	}
}

func discoverSystemOneEndpoint(ctx context.Context, rawCfg map[string]string, overrideURL, overrideKey string) systemOneDiscovery {
	defaultCloud := "https://api.typesafe.ai/v1/sys" + "temone"
	savedURL := envOrDefault(rawCfg["jev_api_url"], "JEV_API_URL", "")
	targetURL := strings.TrimSpace(overrideURL)
	if targetURL == "" {
		targetURL = envOrDefault(savedURL, "", defaultCloud)
	}
	apiKey := strings.TrimSpace(overrideKey)
	if apiKey == "" && (overrideURL == "" || adapters.IsTypesafeCloudURL(targetURL) || (savedURL != "" && sameURLHost(targetURL, savedURL))) {
		apiKey = envOrDefault(rawCfg["typesafe_api_key"], "TYPESAFE_API_KEY", "")
	}

	u, err := url.Parse(targetURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return systemOneDiscovery{
			Source: "builtin", Engine: "flide", HealthStatus: "offline",
			Models: defaultFlideModels(), APIError: "invalid System 1 API URL",
		}
	}

	if adapters.IsTypesafeCloudURL(targetURL) {
		return systemOneDiscovery{
			Source:       "typesafe_cloud",
			Engine:       "jev",
			HealthStatus: "cloud",
			Models:       defaultTypesafeJevModels(),
		}
	}

	base := adapters.ResolveSystemOneBaseURL(targetURL)
	out := systemOneDiscovery{
		Source:       "builtin",
		Engine:       "flide",
		HealthStatus: "offline",
	}

	cctx, cancel := context.WithTimeout(ctx, 2500*time.Millisecond)
	defer cancel()

	var probeErr string
	if hReq, err := http.NewRequestWithContext(cctx, http.MethodGet, base+"/health", nil); err == nil {
		if apiKey != "" {
			hReq.Header.Set("Authorization", "Bearer "+apiKey)
		}
		if resp, err := systemOneClient.Do(hReq); err == nil {
			var h struct {
				Status  string `json:"status"`
				Model   string `json:"model"`
				Format  string `json:"format"`
				Engine  string `json:"engine"`
				Version string `json:"version"`
			}
			_ = json.NewDecoder(io.LimitReader(resp.Body, 8192)).Decode(&h)
			resp.Body.Close()
			if h.Status == "ok" || h.Status == "loading" {
				out.HealthStatus = h.Status
				out.ServedModel = strings.TrimSpace(h.Model)
				out.Format = strings.TrimSpace(h.Format)
				out.Version = strings.TrimSpace(h.Version)
				out.Source = "live_flide"
			} else if resp.StatusCode >= 200 && resp.StatusCode < 300 {
				out.HealthStatus = "ok"
				out.Source = "live_flide"
			}
		} else {
			probeErr = err.Error()
		}
	}

	var liveModels []systemOneModelOption
	seen := map[string]bool{}
	addModel := func(opt systemOneModelOption) {
		key := strings.TrimSpace(opt.ID)
		if key == "" || seen[key] {
			return
		}
		seen[key] = true
		liveModels = append(liveModels, opt)
	}

	if mReq, err := http.NewRequestWithContext(cctx, http.MethodGet, base+"/v1/models", nil); err == nil {
		if apiKey != "" {
			mReq.Header.Set("Authorization", "Bearer "+apiKey)
		}
		if resp, err := systemOneClient.Do(mReq); err == nil {
			if resp.StatusCode >= 200 && resp.StatusCode < 300 {
				var payload struct {
					Models []struct {
						Name        string `json:"name"`
						Description string `json:"description"`
					} `json:"models"`
					Data []struct {
						ID string `json:"id"`
					} `json:"data"`
				}
				if json.NewDecoder(io.LimitReader(resp.Body, 32768)).Decode(&payload) == nil {
					out.Source = "live_flide"
					if out.HealthStatus == "offline" {
						out.HealthStatus = "ok"
					}
					addModel(systemOneModelOption{
						ID: "jev-latest", Name: "jev-latest (Universal Alias)", Engine: "flide", Recommended: true,
						Desc: "Universal Flide alias — routes to currently served model",
					})
					for _, m := range payload.Models {
						desc := strings.TrimSpace(m.Description)
						if desc == "" {
							desc = "Currently served by Flide (/v1/models)"
						}
						addModel(systemOneModelOption{
							ID: m.Name, Name: m.Name, Engine: "flide", Recommended: true, Desc: desc,
						})
						if out.ServedModel == "" {
							out.ServedModel = strings.TrimSpace(m.Name)
						}
					}
					for _, d := range payload.Data {
						addModel(systemOneModelOption{
							ID: d.ID, Name: d.ID, Engine: "flide", Recommended: d.ID == out.ServedModel,
							Desc: "Discovered from /v1/models",
						})
					}
				}
			}
			resp.Body.Close()
		} else if probeErr == "" {
			probeErr = err.Error()
		}
	}

	if out.ServedModel != "" && !seen[out.ServedModel] {
		addModel(systemOneModelOption{
			ID: "jev-latest", Name: "jev-latest (Universal Alias)", Engine: "flide", Recommended: true,
			Desc: "Universal Flide alias — routes to " + out.ServedModel,
		})
		addModel(systemOneModelOption{
			ID: out.ServedModel, Name: out.ServedModel, Engine: "flide", Recommended: true,
			Desc: "Currently loaded on Flide server",
		})
	}
	for _, def := range defaultFlideModels() {
		addModel(def)
	}
	out.Models = liveModels
	if out.HealthStatus == "offline" && probeErr != "" {
		out.APIError = probeErr
	}
	return out
}

func validateAndApplyRoutingPairs(genProv, genMod string, applyGen bool, routes map[memory.EvalTask][2]string, pairs map[string]string) error {
	if applyGen {
		gp := memory.NormalizeProviderID(genProv)
		gm := strings.TrimSpace(genMod)
		if gp != "" {
			switch gp {
			case "gemini", "gguf", "local":
			default:
				return fmt.Errorf("generative companion must be gemini, gguf, or local (got %q)", genProv)
			}
			if memory.IsDecisionOnlySpec(gp, gm) {
				return fmt.Errorf("generative companion cannot use decision-only model %s:%s", gp, gm)
			}
		} else if gm != "" && memory.IsDecisionOnlySpec("", gm) {
			return fmt.Errorf("generative companion cannot use decision-only model %q", gm)
		}
		pairs["gen_provider"] = gp
		if gp == "" && gm == "" {
			pairs["gen_model"] = ""
		} else {
			pairs["gen_model"] = gm
		}
	}
	for _, spec := range memory.AllEvalTasks() {
		pair, ok := routes[spec.Task]
		if !ok {
			continue
		}
		tp := memory.NormalizeProviderID(pair[0])
		tm := strings.TrimSpace(pair[1])
		if err := memory.ValidateTaskRoute(spec.Task, tp, tm); err != nil {
			return err
		}
		pairs[spec.ProviderKey] = tp
		if tp == "" && tm == "" {
			pairs[spec.ModelKey] = ""
		} else {
			pairs[spec.ModelKey] = tm
		}
	}
	return nil
}

// Local inference server HTTP probing (11434/1234/8000) is modularized in local_probe.go.
