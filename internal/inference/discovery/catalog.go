package discovery

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"

	"praxis/internal/domain"
	"praxis/internal/inference/providers"
)

func NormalizePresetCategoriesJSON(raw string) string {
	s := strings.TrimSpace(raw)
	if s == "" || s == "reset" {
		return domain.DefaultPresetCategoriesJSON
	}
	var parsed map[string][]string
	if err := json.Unmarshal([]byte(s), &parsed); err != nil {
		return domain.DefaultPresetCategoriesJSON
	}
	clean := make(map[string][]string, len(parsed))
	for k, subs := range parsed {
		cat := domain.NormalizeCategoryForScope("global", k)
		if cat == "" {
			continue
		}
		seen := make(map[string]bool, len(subs))
		var subList []string
		for _, sub := range subs {
			sc := domain.NormalizeSubCategory(sub)
			if sc != "" && !seen[sc] {
				seen[sc] = true
				subList = append(subList, sc)
			}
		}
		sort.Strings(subList)
		clean[cat] = subList
	}
	if len(clean) == 0 {
		return domain.DefaultPresetCategoriesJSON
	}
	b, err := json.Marshal(clean)
	if err != nil {
		return domain.DefaultPresetCategoriesJSON
	}
	return string(b)
}

func EnvOrDefault(raw, envVar, fallback string) string {
	if s := strings.TrimSpace(raw); s != "" {
		return s
	}
	if s := strings.TrimSpace(os.Getenv(envVar)); s != "" {
		return s
	}
	return fallback
}

func MaskSecret(raw, envVar string) string {
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

func PublicConfigWithGGUFPort(raw map[string]string, providerName string, ggufPort int) map[string]string {
	if ggufPort < 1 || ggufPort > 65535 {
		ggufPort = domain.DefaultGGUFPort
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
			eff = domain.DefaultProvider()
		}
	}
	defaultTypesafeURL := "https://api.typesafe.ai/v1/sys" + "temone"
	jevURL := EnvOrDefault(raw["jev_api_url"], "JEV_API_URL", defaultTypesafeURL)
	defaultJevModel := "jev-latest"
	if providers.IsTypesafeCloudURL(jevURL) {
		defaultJevModel = "jev-1.13.0"
	}
	res := map[string]string{
		"active_provider":             raw["active_provider"],
		"effective_provider":          eff,
		"gen_provider":                domain.NormalizeProviderID(raw["gen_provider"]),
		"gen_model":                   strings.TrimSpace(raw["gen_model"]),
		"gguf_endpoint":               EnvOrDefault(raw["gguf_endpoint"], "LOCAL_GGUF_URL", defaultGGUFURL),
		"gguf_model":                  raw["gguf_model"],
		"gemini_model":                EnvOrDefault(raw["gemini_model"], "GEMINI_MODEL", "gemini-3.5-flash-lite"),
		"gemini_key_status":           MaskSecret(raw["gemini_api_key"], "GEMINI_API_KEY"),
		"jev_api_url":                 jevURL,
		"jev_model":                   EnvOrDefault(raw["jev_model"], "JEV_MODEL", defaultJevModel),
		"typesafe_key_status":         MaskSecret(raw["typesafe_api_key"], "TYPESAFE_API_KEY"),
		"local_model_url":             EnvOrDefault(raw["local_model_url"], "LOCAL_MODEL_URL", "http://127.0.0.1:11434/v1"),
		"local_model_name":            EnvOrDefault(raw["local_model_name"], "LOCAL_MODEL_NAME", "qwen3:4b"),
		"local_key_status":            MaskSecret(raw["local_api_key"], "LOCAL_API_KEY"),
		"include_personal_in_project": EnvOrDefault(raw["include_personal_in_project"], "PRAXIS_INCLUDE_PERSONAL", "0"),
		"preset_categories":           NormalizePresetCategoriesJSON(raw["preset_categories"]),
	}
	for _, spec := range domain.AllEvalTasks() {
		res[spec.ProviderKey] = domain.NormalizeProviderID(raw[spec.ProviderKey])
		res[spec.ModelKey] = strings.TrimSpace(raw[spec.ModelKey])
	}
	return res
}

type GeminiModelOption struct {
	ID            string `json:"id"`
	Name          string `json:"name"`
	Tier          string `json:"tier"` // "speed", "decision", "balanced", "accuracy"
	DecisionOnly  bool   `json:"decision_only,omitempty"`
	Recommended   bool   `json:"recommended"`
	ContextTokens int    `json:"context_tokens,omitempty"`
	Label         string `json:"label"`
	Desc          string `json:"desc"`
}

func ValidateAndApplyRoutingPairs(genProv, genMod string, applyGen bool, routes map[domain.EvalTask][2]string, pairs map[string]string) error {
	if applyGen {
		gp := domain.NormalizeProviderID(genProv)
		gm := strings.TrimSpace(genMod)
		if gp != "" {
			if spec, ok := domain.FindProviderSpec(gp); !ok || !spec.GenerativeCapable {
				return fmt.Errorf("generative companion must support text generation (got %q)", genProv)
			}
			if domain.IsDecisionOnlySpec(gp, gm) {
				return fmt.Errorf("generative companion cannot use decision-only model %s:%s", gp, gm)
			}
		} else if gm != "" && domain.IsDecisionOnlySpec("", gm) {
			return fmt.Errorf("generative companion cannot use decision-only model %q", gm)
		}
		pairs["gen_provider"] = gp
		if gp == "" && gm == "" {
			pairs["gen_model"] = ""
		} else {
			pairs["gen_model"] = gm
		}
	}
	for _, spec := range domain.AllEvalTasks() {
		pair, ok := routes[spec.Task]
		if !ok {
			continue
		}
		tp := domain.NormalizeProviderID(pair[0])
		tm := strings.TrimSpace(pair[1])
		if err := domain.ValidateTaskRoute(spec.Task, tp, tm); err != nil {
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
