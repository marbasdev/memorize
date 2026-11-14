package providers

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"memorize/internal/domain"
)

var geminiQuotaFallbacks = []string{
	"gemini-3.1-flash-lite",
	"gemini-3.5-flash-lite",
	"gemini-3.5-flash",
	"gemini-3.6-flash",
	"gemini-3.8-flash",
}

type GeminiAdapter struct {
	apiKey        string
	model         string
	noLogprobs    atomic.Bool
	noThinking    atomic.Bool
	cooldownMu    sync.Mutex
	cooldownUntil map[string]time.Time
}

func supportsThinkingBudget(model string) bool {
	m := strings.ToLower(strings.TrimSpace(model))
	return strings.Contains(m, "gemini-2.5") || strings.Contains(m, "gemini-3")
}

func NewGeminiAdapter() *BatchEvaluator {
	return NewGeminiAdapterConfig("", "")
}

func NewGeminiAdapterConfig(apiKey, model string) *BatchEvaluator {
	k := strings.TrimSpace(apiKey)
	if k == "" {
		k = os.Getenv("GEMINI_API_KEY")
	}
	m := strings.TrimSpace(model)
	if m == "" {
		m = os.Getenv("GEMINI_MODEL")
	}
	if m == "" {
		m = "gemini-3.5-flash-lite"
	}
	if domain.IsDecisionOnlySpec("gemini", m) {
		return NewGeminiDecisionsAdapterConfig(k, m)
	}
	tr := &GeminiAdapter{apiKey: k, model: m}
	return NewBatchEvaluator("gemini:"+m, newSchemaEvaluator(tr), 25, 6)
}

func geminiBaseURL() string {
	base := strings.TrimSpace(os.Getenv("GEMINI_API_BASE"))
	if base == "" {
		base = "https://generativelanguage.googleapis.com"
	}
	return strings.TrimRight(base, "/")
}

func geminiErrorMessage(errBytes []byte) string {
	msg := strings.TrimSpace(string(errBytes))
	var apiErr struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(errBytes, &apiErr) == nil && apiErr.Error.Message != "" {
		msg = apiErr.Error.Message
	}
	if len(msg) > 360 {
		msg = msg[:360] + "..."
	}
	return msg
}

func (g *GeminiAdapter) Name() string { return "gemini:" + g.model }

func (g *GeminiAdapter) Dialect() schemaDialect {
	return schemaDialect{upperTypes: true, strictNoAdditional: false}
}

func (g *GeminiAdapter) candidateModels() []string {
	if os.Getenv("GEMINI_DISABLE_FALLBACK") == "1" {
		return []string{g.model}
	}
	now := time.Now()
	seen := make(map[string]bool)
	var active, cooled []string

	g.cooldownMu.Lock()
	for _, m := range append([]string{g.model}, geminiQuotaFallbacks...) {
		m = strings.TrimSpace(m)
		if m == "" || seen[m] {
			continue
		}
		seen[m] = true
		if until, ok := g.cooldownUntil[m]; ok && now.Before(until) {
			cooled = append(cooled, m)
		} else {
			active = append(active, m)
		}
	}
	g.cooldownMu.Unlock()
	return append(active, cooled...)
}

func (g *GeminiAdapter) markCooldown(model string, d time.Duration) {
	g.cooldownMu.Lock()
	if g.cooldownUntil == nil {
		g.cooldownUntil = make(map[string]time.Time)
	}
	g.cooldownUntil[model] = time.Now().Add(d)
	g.cooldownMu.Unlock()
}

func (g *GeminiAdapter) CallJSON(ctx context.Context, prompt string, schema map[string]any, target any) (float64, error) {
	candidates := g.candidateModels()
	var conf float64
	var err error
	for idx, model := range candidates {
		useLP := !g.noLogprobs.Load() && (os.Getenv("GEMINI_API_BASE") != "" || os.Getenv("GEMINI_ENABLE_LOGPROBS") == "1")
		useZeroThink := !g.noThinking.Load() && supportsThinkingBudget(model)
		var retryReason string

		for attempt := 0; attempt < 3; attempt++ {
			if attempt > 0 {
				select {
				case <-ctx.Done():
					return 0, ctx.Err()
				case <-time.After(time.Duration(attempt*350) * time.Millisecond):
				}
			}
			for range 3 {
				conf, retryReason, err = g.doGenerateJSON(ctx, model, prompt, schema, useLP, useZeroThink, target)
				if err != nil && retryReason == "logprobs" {
					g.noLogprobs.Store(true)
					useLP = false
					continue
				}
				if err != nil && retryReason == "thinking" {
					g.noThinking.Store(true)
					useZeroThink = false
					continue
				}
				break
			}
			if err == nil || !strings.Contains(err.Error(), "status 503") {
				break
			}
		}
		if err == nil {
			return conf, nil
		}
		// On per-model quota exhaustion (429) or retired fallback alias (404), advance to the next Flash-Lite / Flash bucket.
		if strings.Contains(err.Error(), "status 429") {
			g.markCooldown(model, 10*time.Minute)
			if idx+1 < len(candidates) {
				continue
			}
		} else if idx > 0 && strings.Contains(err.Error(), "status 404") && idx+1 < len(candidates) {
			g.markCooldown(model, 1*time.Hour)
			continue
		}
		break
	}
	return conf, err
}

func (g *GeminiAdapter) doGenerateJSON(ctx context.Context, model, prompt string, schema map[string]any, withLogprobs, zeroThinking bool, target any) (float64, string, error) {
	endpoint := fmt.Sprintf("%s/v1beta/models/%s:generateContent", geminiBaseURL(), model)
	genCfg := map[string]any{
		"temperature":      0.0,
		"responseMimeType": "application/json",
		"responseSchema":   schema,
	}
	if withLogprobs {
		genCfg["responseLogprobs"] = true
	}
	if zeroThinking {
		genCfg["thinkingConfig"] = map[string]any{"thinkingBudget": 0}
	}
	body, err := json.Marshal(map[string]any{
		"contents": []map[string]any{
			{"parts": []map[string]string{{"text": prompt}}},
		},
		"generationConfig": genCfg,
	})
	if err != nil {
		return 0, "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return 0, "", err
	}
	req.Header.Set("Content-Type", "application/json")
	if g.apiKey != "" {
		req.Header.Set("x-goog-api-key", g.apiKey)
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return 0, "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		errBytes, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		msg := geminiErrorMessage(errBytes)
		lowerMsg := strings.ToLower(msg)
		if withLogprobs && resp.StatusCode == http.StatusBadRequest && strings.Contains(lowerMsg, "logprob") {
			return 0, "logprobs", fmt.Errorf("logprobs unsupported on %s", model)
		}
		if zeroThinking && resp.StatusCode == http.StatusBadRequest && (strings.Contains(lowerMsg, "thinking") || strings.Contains(lowerMsg, "invalid argument")) {
			return 0, "thinking", fmt.Errorf("thinkingBudget=0 unsupported on %s", model)
		}
		if msg != "" {
			return 0, "", fmt.Errorf("gemini status %d: %s", resp.StatusCode, msg)
		}
		return 0, "", fmt.Errorf("gemini status %d", resp.StatusCode)
	}
	var out struct {
		Candidates []struct {
			Content struct {
				Parts []struct {
					Text    string `json:"text"`
					Thought bool   `json:"thought"`
				} `json:"parts"`
			} `json:"content"`
			AvgLogprobs    float64 `json:"avgLogprobs"`
			LogprobsResult struct {
				ChosenCandidates []struct {
					Token          string  `json:"token"`
					LogProbability float64 `json:"logProbability"`
				} `json:"chosenCandidates"`
			} `json:"logprobsResult"`
		} `json:"candidates"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return 0, "", err
	}
	if len(out.Candidates) == 0 || len(out.Candidates[0].Content.Parts) == 0 {
		return 0, "", fmt.Errorf("empty gemini response")
	}
	c := out.Candidates[0]
	payloadText := ""
	for _, p := range c.Content.Parts {
		if !p.Thought && strings.TrimSpace(p.Text) != "" {
			payloadText = p.Text
			break
		}
	}
	if payloadText == "" {
		payloadText = c.Content.Parts[len(c.Content.Parts)-1].Text
	}
	tokens := make([]tokenLogprob, 0, len(c.LogprobsResult.ChosenCandidates))
	for _, tk := range c.LogprobsResult.ChosenCandidates {
		tokens = append(tokens, tokenLogprob{Token: tk.Token, Logprob: tk.LogProbability})
	}
	conf := semanticConfidence(tokens, c.AvgLogprobs)
	return conf, "", json.Unmarshal([]byte(payloadText), target)
}
