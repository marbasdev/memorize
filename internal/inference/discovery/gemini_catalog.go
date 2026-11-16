package discovery

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

var geminiCatalogClient = &http.Client{
	Timeout: 6 * time.Second,
	CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
		return http.ErrUseLastResponse
	},
}

// DefaultGeminiModels returns the built-in Gemini model preset catalog.
func DefaultGeminiModels() []GeminiModelOption {
	return defaultGeminiModels()
}

func defaultGeminiModels() []GeminiModelOption {
	return []GeminiModelOption{
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

// DiscoverGeminiModels queries the Gemini models catalog or returns builtin defaults.
func DiscoverGeminiModels(ctx context.Context, rawCfg map[string]string, overrideKey string) ([]GeminiModelOption, string, string) {
	apiKey := EnvOrDefault(overrideKey, "", EnvOrDefault(rawCfg["gemini_api_key"], "GEMINI_API_KEY", ""))
	if apiKey == "" {
		return defaultGeminiModels(), "builtin", ""
	}
	base := strings.TrimRight(EnvOrDefault("", "GEMINI_API_BASE", "https://generativelanguage.googleapis.com"), "/")
	cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	code, body, err := httpGetBody(cctx, geminiCatalogClient, base+"/v1beta/models?pageSize=100", "x-goog-api-key", apiKey, 1<<20)
	if err != nil {
		return defaultGeminiModels(), "builtin", err.Error()
	}
	if code >= 300 {
		var apiErr struct {
			Error struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		msg := fmt.Sprintf("HTTP %d", code)
		if json.Unmarshal(body, &apiErr) == nil && apiErr.Error.Message != "" {
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
	if err := json.Unmarshal(body, &payload); err != nil {
		return defaultGeminiModels(), "builtin", err.Error()
	}

	var list []GeminiModelOption
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
		list = append(list, GeminiModelOption{
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
