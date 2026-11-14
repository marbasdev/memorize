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

	"memorize/internal/infra/config"
)

var ggufClient = &http.Client{Timeout: 30 * time.Second}

type GGUFAdapter struct {
	prefix        string
	epMu          sync.RWMutex
	endpoint      string
	model         string
	apiKey        string
	ensureFn      func(context.Context) error
	ensureEPFn    func(context.Context) (string, error)
	minimalOpenAI atomic.Bool
}

func NewGGUFAdapter(endpoint, model string, ensureFn ...func(context.Context) error) *BatchEvaluator {
	var fn func(context.Context) error
	if len(ensureFn) > 0 {
		fn = ensureFn[0]
	}
	return newGGUFAdapter(endpoint, model, fn, nil)
}

// NewGGUFAdapterWithEndpoint behaves like NewGGUFAdapter while accepting an auto-wake callback
// that returns the dynamically chosen endpoint (for example, after an automatic port fallback).
func NewGGUFAdapterWithEndpoint(endpoint, model string, wakeEPFn func(context.Context) (string, error)) *BatchEvaluator {
	return newGGUFAdapter(endpoint, model, nil, wakeEPFn)
}

func newGGUFAdapter(endpoint, model string, fn func(context.Context) error, wakeEPFn func(context.Context) (string, error)) *BatchEvaluator {
	ep := strings.TrimSpace(endpoint)
	if ep == "" {
		ep = os.Getenv("LOCAL_GGUF_URL")
	}
	if ep == "" {
		cfg := config.LoadPortConfigDir(config.DefaultDataDir())
		ep = fmt.Sprintf("http://127.0.0.1:%d", cfg.GGUFPort)
	}
	m := strings.TrimSpace(model)
	if m == "" {
		m = os.Getenv("LOCAL_GGUF_MODEL")
	}
	if m == "" {
		m = "local-gguf"
	}
	tr := &GGUFAdapter{prefix: "gguf:", endpoint: ep, model: m, ensureFn: fn, ensureEPFn: wakeEPFn}
	return NewBatchEvaluator("gguf:"+m, newSchemaEvaluator(tr), 20, 2)
}

// NewOpenAICompatibleAdapter connects to any OpenAI-compatible /v1/chat/completions server
// (Ollama, LM Studio, vLLM, llama.cpp, OpenRouter, or corporate OpenAI gateway),
// while preserving legacy /v1/systemone compatibility if explicitly configured.
func NewOpenAICompatibleAdapter(endpoint, model, apiKey string) *BatchEvaluator {
	ep := strings.TrimSpace(endpoint)
	if ep == "" {
		ep = strings.TrimSpace(os.Getenv("LOCAL_MODEL_URL"))
	}
	if ep == "" {
		ep = "http://127.0.0.1:11434/v1"
	}
	if strings.HasSuffix(strings.TrimRight(ep, "/"), "/systemone") {
		return NewLocalLayaAdapterConfig(ep, apiKey)
	}
	m := strings.TrimSpace(model)
	if m == "" {
		m = strings.TrimSpace(os.Getenv("LOCAL_MODEL_NAME"))
	}
	if m == "" {
		m = "qwen3:4b"
	}
	k := strings.TrimSpace(apiKey)
	if k == "" {
		k = strings.TrimSpace(os.Getenv("LOCAL_API_KEY"))
	}
	if k == "" {
		k = strings.TrimSpace(os.Getenv("OPENAI_API_KEY"))
	}
	tr := &GGUFAdapter{prefix: "local:", endpoint: ep, model: m, apiKey: k}
	return NewBatchEvaluator("local:"+m, newSchemaEvaluator(tr), 15, 4)
}

func (g *GGUFAdapter) Name() string { return g.prefix + g.model }

func (g *GGUFAdapter) Dialect() schemaDialect {
	return schemaDialect{upperTypes: false, strictNoAdditional: true}
}

func extractJSONObject(raw string) []byte {
	s := strings.TrimSpace(raw)
	if idx := strings.Index(s, "</think>"); idx >= 0 {
		s = strings.TrimSpace(s[idx+len("</think>"):])
	}
	start := strings.IndexByte(s, '{')
	end := strings.LastIndexByte(s, '}')
	if start >= 0 && end >= start {
		return []byte(s[start : end+1])
	}
	return []byte(s)
}

func resolveChatCompletionsURL(rawEndpoint string) string {
	u := strings.TrimRight(strings.TrimSpace(rawEndpoint), "/")
	switch {
	case strings.HasSuffix(u, "/chat/completions"):
		return u
	case strings.HasSuffix(u, "/v1"):
		return u + "/chat/completions"
	default:
		return u + "/v1/chat/completions"
	}
}

func (g *GGUFAdapter) buildRequestPayload(prompt string, schema map[string]any, minimal bool) ([]byte, error) {
	sysPrompt := "You are a calibrated System 1 state-memory evaluator. Output strictly valid JSON matching the requested schema."
	if minimal {
		schemaJSON, _ := json.Marshal(schema)
		sysPrompt += " Required JSON Schema: " + string(schemaJSON)
		return json.Marshal(map[string]any{
			"model":           g.model,
			"temperature":     0.0,
			"response_format": map[string]any{"type": "json_object"},
			"messages": []map[string]string{
				{"role": "system", "content": sysPrompt},
				{"role": "user", "content": prompt},
			},
		})
	}
	reqMap := map[string]any{
		"model":       g.model,
		"temperature": 0.0,
		"logprobs":    true,
		"response_format": map[string]any{
			"type": "json_schema",
			"json_schema": map[string]any{
				"name":   "memory_decision",
				"strict": true,
				"schema": schema,
			},
		},
		"messages": []map[string]string{
			{"role": "system", "content": sysPrompt},
			{"role": "user", "content": prompt},
		},
	}
	if g.prefix == "gguf:" {
		reqMap["cache_prompt"] = true
		reqMap["chat_template_kwargs"] = map[string]any{"enable_thinking": false}
	}
	return json.Marshal(reqMap)
}

func (g *GGUFAdapter) currentEndpoint() string {
	g.epMu.RLock()
	ep := g.endpoint
	g.epMu.RUnlock()
	return ep
}

func (g *GGUFAdapter) setEndpoint(ep string) {
	trimmed := strings.TrimSpace(ep)
	if trimmed == "" {
		return
	}
	g.epMu.Lock()
	g.endpoint = trimmed
	g.epMu.Unlock()
}

func (g *GGUFAdapter) CallJSON(ctx context.Context, prompt string, schema map[string]any, target any) (float64, error) {
	minimal := g.minimalOpenAI.Load()

	sendOnce := func(isMinimal bool) (*http.Response, error) {
		url := resolveChatCompletionsURL(g.currentEndpoint())
		body, err := g.buildRequestPayload(prompt, schema, isMinimal)
		if err != nil {
			return nil, err
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Content-Type", "application/json")
		if g.apiKey != "" {
			req.Header.Set("Authorization", "Bearer "+g.apiKey)
		}
		return ggufClient.Do(req)
	}

	resp, err := sendOnce(minimal)
	if err != nil && (g.ensureEPFn != nil || g.ensureFn != nil) {
		var werr error
		if g.ensureEPFn != nil {
			var wep string
			wep, werr = g.ensureEPFn(ctx)
			if werr == nil && wep != "" {
				g.setEndpoint(wep)
			}
		} else {
			werr = g.ensureFn(ctx)
		}
		if werr == nil {
			resp, err = sendOnce(minimal)
		} else {
			return 0, fmt.Errorf("local GGUF engine not ready (%s): %w", g.currentEndpoint(), werr)
		}
	}
	if err != nil {
		if g.prefix == "gguf:" {
			return 0, fmt.Errorf("local GGUF server unreachable (%s) — open %s (Engines tab) to download/start a GGUF model or switch engines: %w", g.currentEndpoint(), config.ActiveDaemonURL(""), err)
		}
		return 0, fmt.Errorf("OpenAI/GGUF server unreachable (%s): %w", g.currentEndpoint(), err)
	}
	if resp.StatusCode == http.StatusBadRequest && !minimal {
		_ = resp.Body.Close()
		g.minimalOpenAI.Store(true)
		resp, err = sendOnce(true)
		if err != nil {
			return 0, fmt.Errorf("OpenAI/GGUF fallback request failed (%s): %w", g.currentEndpoint(), err)
		}
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		errBytes, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		msg := strings.TrimSpace(string(errBytes))
		if msg != "" {
			return 0, fmt.Errorf("OpenAI/GGUF status %d: %s", resp.StatusCode, msg)
		}
		return 0, fmt.Errorf("OpenAI/GGUF status %d", resp.StatusCode)
	}
	var out struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
			Logprobs struct {
				Content []struct {
					Token   string  `json:"token"`
					Logprob float64 `json:"logprob"`
				} `json:"content"`
			} `json:"logprobs"`
		} `json:"choices"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return 0, err
	}
	if len(out.Choices) == 0 {
		return 0, fmt.Errorf("empty OpenAI/GGUF response")
	}
	ch := out.Choices[0]
	tokens := make([]tokenLogprob, 0, len(ch.Logprobs.Content))
	for _, tk := range ch.Logprobs.Content {
		tokens = append(tokens, tokenLogprob{Token: tk.Token, Logprob: tk.Logprob})
	}
	conf := semanticConfidence(tokens, 0)
	return conf, json.Unmarshal(extractJSONObject(ch.Message.Content), target)
}
