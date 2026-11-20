package domain

import (
	"os"
	"strings"
)

type ProviderSpec struct {
	ID                    string
	Aliases               []string
	LabelKey              string
	ModelConfigKey        string
	URLConfigKey          string
	APIKeyConfigKey       string
	DefaultModel          string
	DetectEnvVars         []string
	DecisionOnly          bool
	GenerativeCapable     bool
	EvaluatorNamePrefixes []string
}

var providerCatalog = []ProviderSpec{
	{
		ID:                    "gguf",
		LabelKey:              "prov_gguf",
		ModelConfigKey:        "gguf_model",
		URLConfigKey:          "gguf_endpoint",
		DetectEnvVars:         []string{"LOCAL_GGUF_URL"},
		GenerativeCapable:     true,
		EvaluatorNamePrefixes: []string{"gguf:"},
	},
	{
		ID:                    "gemini",
		LabelKey:              "prov_gemini",
		ModelConfigKey:        "gemini_model",
		APIKeyConfigKey:       "gemini_api_key",
		DefaultModel:          "gemini-3.5-flash-lite",
		DetectEnvVars:         []string{"GEMINI_API_KEY"},
		GenerativeCapable:     true,
		EvaluatorNamePrefixes: []string{"gemini:"},
	},
	{
		ID:                    "jev",
		Aliases:               []string{"flide"},
		LabelKey:              "prov_jev",
		ModelConfigKey:        "jev_model",
		URLConfigKey:          "jev_api_url",
		APIKeyConfigKey:       "typesafe_api_key",
		DetectEnvVars:         []string{"TYPESAFE_API_KEY", "FLIDE_API_KEY", "JEV_API_URL"},
		DecisionOnly:          true,
		GenerativeCapable:     false,
		EvaluatorNamePrefixes: []string{"jev:", "flide:"},
	},
	{
		ID:                    "local",
		LabelKey:              "prov_local",
		ModelConfigKey:        "local_model_name",
		URLConfigKey:          "local_model_url",
		APIKeyConfigKey:       "local_api_key",
		DefaultModel:          "qwen3:4b",
		DetectEnvVars:         []string{"LOCAL_MODEL_URL"},
		GenerativeCapable:     true,
		EvaluatorNamePrefixes: []string{"local:"},
	},
}

func ProviderCatalog() []ProviderSpec {
	out := make([]ProviderSpec, len(providerCatalog))
	copy(out, providerCatalog)
	return out
}

func FindProviderSpec(provider string) (ProviderSpec, bool) {
	norm := NormalizeProviderID(provider)
	if norm == "" {
		return ProviderSpec{}, false
	}
	for _, spec := range providerCatalog {
		if spec.ID == norm {
			return spec, true
		}
	}
	return ProviderSpec{}, false
}

// NormalizeProviderID canonicalizes provider identifiers and known aliases (e.g. flide -> jev).
func NormalizeProviderID(provider string) string {
	p := strings.ToLower(strings.TrimSpace(provider))
	if p == "" {
		return ""
	}
	for _, spec := range providerCatalog {
		if spec.ID == p {
			return spec.ID
		}
		for _, alias := range spec.Aliases {
			if alias == p {
				return spec.ID
			}
		}
	}
	return p
}

func ResolveExplicitEnvProvider() string {
	if explicit := NormalizeProviderID(os.Getenv("PRAXIS_PROVIDER")); explicit != "" {
		return explicit
	}
	var matched []string
	for _, spec := range providerCatalog {
		for _, envKey := range spec.DetectEnvVars {
			if strings.TrimSpace(os.Getenv(envKey)) != "" {
				matched = append(matched, spec.ID)
				break
			}
		}
	}
	if len(matched) == 1 {
		return matched[0]
	}
	return ""
}

func ProviderIDFromEvaluatorName(name string) string {
	low := strings.ToLower(strings.TrimSpace(name))
	if low == "" {
		return ""
	}
	for _, spec := range providerCatalog {
		for _, prefix := range spec.EvaluatorNamePrefixes {
			if strings.HasPrefix(low, prefix) {
				return spec.ID
			}
		}
	}
	return ""
}

func ProviderModelFromConfig(cfg map[string]string, provider string) string {
	spec, ok := FindProviderSpec(provider)
	if !ok || spec.ModelConfigKey == "" || cfg == nil {
		return ""
	}
	return strings.TrimSpace(cfg[spec.ModelConfigKey])
}
