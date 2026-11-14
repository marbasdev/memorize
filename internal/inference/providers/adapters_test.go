package providers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"memorize/internal/domain"
)

func TestGeminiZeroThinkingFallbackAndThoughtPartFiltering(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := calls.Add(1)
		var reqBody struct {
			GenerationConfig map[string]any `json:"generationConfig"`
		}
		_ = json.NewDecoder(r.Body).Decode(&reqBody)

		w.Header().Set("Content-Type", "application/json")
		if n == 1 {
			// Verify thinkingBudget=0 is sent first for Gemini 3.x models, then simulate an endpoint rejecting thinkingConfig
			tc, ok := reqBody.GenerationConfig["thinkingConfig"].(map[string]any)
			if !ok || tc["thinkingBudget"] != float64(0) {
				t.Errorf("expected first call to include thinkingBudget=0, got %+v", reqBody.GenerationConfig["thinkingConfig"])
			}
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":{"message":"thinkingBudget is not supported for this model"}}`))
			return
		}
		if _, has := reqBody.GenerationConfig["thinkingConfig"]; has {
			t.Errorf("expected fallback call to omit thinkingConfig, got %+v", reqBody.GenerationConfig["thinkingConfig"])
		}
		// Include a thought part before the JSON payload part to ensure non-thought part selection works
		_, _ = w.Write([]byte(`{
			"candidates": [{
				"content": {
					"parts": [
						{"text": "internal reasoning text", "thought": true},
						{"text": "{\"items\":[{\"id\":1,\"score\":4.6}]}"}
					]
				},
				"avgLogprobs": -0.02
			}]
		}`))
	}))
	defer srv.Close()

	t.Setenv("GEMINI_API_BASE", srv.URL)
	t.Setenv("GEMINI_API_KEY", "test-key")
	ev := NewGeminiAdapterConfig("test-key", "gemini-3.5-flash-lite")

	scored, err := ev.ScoreRelevance(context.Background(), "auth middleware", []domain.Fact{
		{ID: 1, Scope: "project:app", Category: "security", Content: "Use HttpOnly cookies"},
	})
	if err != nil || len(scored) != 1 || scored[0].Score != 4.6 {
		t.Fatalf("expected automatic thinking fallback + thought part skip to succeed, got scored=%+v err=%v", scored, err)
	}
	if calls.Load() != 2 {
		t.Fatalf("expected 2 HTTP calls (initial + fallback), got %d", calls.Load())
	}

	// Subsequent call must reuse cached noThinking state in 1 request
	_, err = ev.ScoreRelevance(context.Background(), "auth middleware", []domain.Fact{
		{ID: 1, Scope: "project:app", Category: "security", Content: "Use HttpOnly cookies"},
	})
	if err != nil || calls.Load() != 3 {
		t.Fatalf("expected cached noThinking to succeed in 1 call (total 3), got total=%d err=%v", calls.Load(), err)
	}
}

func TestGeminiQuota429ModelFallbackAndCooldown(t *testing.T) {
	var primaryHits, fallbackHits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(r.URL.Path, "gemini-3.5-flash-lite") {
			primaryHits.Add(1)
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"error":{"code":429,"message":"You exceeded your current quota"}}`))
			return
		}
		if strings.Contains(r.URL.Path, "gemini-3.1-flash-lite") {
			fallbackHits.Add(1)
			_, _ = w.Write([]byte(`{
				"candidates": [{
					"content": {"parts": [{"text": "{\"items\":[{\"id\":1,\"score\":4.8}]}"}]},
					"avgLogprobs": -0.01
				}]
			}`))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	t.Setenv("GEMINI_API_BASE", srv.URL)
	ev := NewGeminiAdapterConfig("test-key", "gemini-3.5-flash-lite")

	scored, err := ev.ScoreRelevance(context.Background(), "daemon test", []domain.Fact{
		{ID: 1, Scope: "project:app", Category: "architecture", Content: "Single local daemon"},
	})
	if err != nil || len(scored) != 1 || scored[0].Score != 4.8 {
		t.Fatalf("expected 429 on primary model to fall back to gemini-3.1-flash-lite, got scored=%+v err=%v", scored, err)
	}
	if primaryHits.Load() != 1 || fallbackHits.Load() != 1 {
		t.Fatalf("expected 1 primary hit + 1 fallback hit, got primary=%d fallback=%d", primaryHits.Load(), fallbackHits.Load())
	}

	// Second call within cooldown window must skip the exhausted primary model and hit fallback directly.
	_, err = ev.ScoreRelevance(context.Background(), "daemon test", []domain.Fact{
		{ID: 1, Scope: "project:app", Category: "architecture", Content: "Single local daemon"},
	})
	if err != nil || primaryHits.Load() != 1 || fallbackHits.Load() != 2 {
		t.Fatalf("expected cooldown to skip primary on second call, got primary=%d fallback=%d err=%v", primaryHits.Load(), fallbackHits.Load(), err)
	}
}

func TestOpenAICompatibleAdapterAndMinimalJSONObjectFallback(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			t.Errorf("expected /v1/chat/completions path (no duplicate /v1/v1), got %s", r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer sk-local-test" {
			t.Errorf("expected Bearer auth header, got %q", r.Header.Get("Authorization"))
		}
		n := calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		if n == 1 {
			// Simulate an OpenAI-compatible server rejecting strict json_schema mode with 400
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":{"message":"json_schema not supported"}}`))
			return
		}
		_, _ = w.Write([]byte(`{
			"choices": [{
				"message": {"content": "{\"items\":[{\"id\":7,\"score\":4.4}]}"}
			}]
		}`))
	}))
	defer srv.Close()

	ev := NewOpenAICompatibleAdapter(srv.URL+"/v1", "qwen3:4b", "sk-local-test")
	scored, err := ev.ScoreRelevance(context.Background(), "database migration", []domain.Fact{
		{ID: 7, Scope: "project:app", Category: "database", Content: "Run migrations inside transactions"},
	})
	if err != nil || len(scored) != 1 || scored[0].Score != 4.4 {
		t.Fatalf("expected OpenAI-compatible adapter to succeed with minimal fallback, got scored=%+v err=%v", scored, err)
	}
	if calls.Load() != 2 {
		t.Fatalf("expected 2 calls (json_schema -> json_object fallback), got %d", calls.Load())
	}
}

func TestFlideAndJevSystemOneAdapterPrefixAndModels(t *testing.T) {
	sysOnePath := "/v1/sys" + "temone"
	var states []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != sysOnePath {
			t.Errorf("expected System 1 path %s, got %s", sysOnePath, r.URL.Path)
		}
		if auth := r.Header.Get("Authorization"); auth != "" {
			t.Errorf("expected empty Authorization header for keyless Flide server, got %q", auth)
		}
		var req struct {
			State     string `json:"state"`
			Model     string `json:"model"`
			Questions map[string]struct {
				Type         string `json:"type"`
				Instructions string `json:"instructions"`
				Criteria     any    `json:"criteria"`
			} `json:"questions"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		if req.Model != "jev-latest" {
			t.Errorf("expected default Flide model alias jev-latest, got %q", req.Model)
		}
		states = append(states, req.State)

		answers := make(map[string]any, len(req.Questions))
		for k, q := range req.Questions {
			switch q.Type {
			case "score":
				answers[k] = map[string]any{"type": "score", "score": 4.7, "confidence": 0.94}
			case "choice":
				ch := "architecture"
				if m, ok := q.Criteria.(map[string]any); ok {
					if _, hasRule := m["rule"]; hasRule {
						ch = "rule"
					} else if _, hasEx := m["exclusive"]; hasEx {
						ch = "exclusive"
					} else if _, hasInd := m["INDEPENDENT"]; hasInd {
						ch = "INDEPENDENT"
					}
				}
				answers[k] = map[string]any{"type": "choice", "choice": ch, "confidence": 0.93}
			default:
				answers[k] = map[string]any{"type": "noul", "noul": 0.92}
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"model": "OmniJev/OneJev1.6-0.8B", "answers": answers})
	}))
	defer srv.Close()

	ev := NewFlideAdapter(srv.URL, "", "")
	if ev.Name() != "flide:jev-latest" {
		t.Fatalf("expected Name() = flide:jev-latest, got %q", ev.Name())
	}

	facts := []domain.Fact{
		{ID: 1, Scope: "project:app", Category: "architecture", Content: "Always keep SQLite in WAL mode"},
	}

	scored, err := ev.ScoreRelevance(context.Background(), "add index migration", facts)
	if err != nil || len(scored) != 1 || scored[0].Score != 4.7 {
		t.Fatalf("ScoreRelevance failed: scored=%+v err=%v", scored, err)
	}
	if idxFacts, idxTask := strings.Index(states[0], "Active Facts:"), strings.Index(states[0], "Current Task:"); idxFacts < 0 || idxTask <= idxFacts {
		t.Fatalf("expected Active Facts before Current Task for Flide prefix caching, got state:\n%s", states[0])
	}

	dec, err := ev.EvaluateMutation(context.Background(), "project:app", "Never disable foreign keys", facts)
	if err != nil || dec.IsPermanent < 0.9 || dec.Confidence < 0.9 {
		t.Fatalf("EvaluateMutation failed: dec=%+v err=%v", dec, err)
	}
	if idxExist, idxCand := strings.Index(states[1], "Existing Facts:"), strings.Index(states[1], "Candidate Fact:"); idxExist < 0 || idxCand <= idxExist {
		t.Fatalf("expected Existing Facts before Candidate Fact for Flide prefix caching, got state:\n%s", states[1])
	}

	viols, err := ev.VerifyPlan(context.Background(), "Switch SQLite journal_mode to DELETE", facts)
	if err != nil || len(viols) != 1 {
		t.Fatalf("VerifyPlan failed: viols=%+v err=%v", viols, err)
	}
	if idxRules, idxPlan := strings.Index(states[2], "Project Rules:"), strings.Index(states[2], "Proposed Plan:"); idxRules < 0 || idxPlan <= idxRules {
		t.Fatalf("expected Project Rules before Proposed Plan for Flide prefix caching, got state:\n%s", states[2])
	}
}

func TestGeminiDecisionsAdapterWireProtocolAndTasks(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := hits.Add(1)
		if r.URL.Path != "/v1beta/decisions" {
			t.Errorf("expected /v1beta/decisions path, got %s", r.URL.Path)
		}
		if r.Header.Get("x-goog-api-key") != "gem-dec-key" {
			t.Errorf("expected x-goog-api-key gem-dec-key, got %q", r.Header.Get("x-goog-api-key"))
		}
		if n == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"error":{"message":"temporarily unavailable"}}`))
			return
		}
		var req struct {
			Model     string `json:"model"`
			Input     string `json:"input"`
			Questions map[string]struct {
				Type         string `json:"type"`
				Instructions string `json:"instructions"`
				Criteria     any    `json:"criteria"`
			} `json:"questions"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("decode /v1beta/decisions request: %v", err)
		}
		if req.Model != "gemini-decision-1" {
			t.Errorf("expected model gemini-decision-1, got %q", req.Model)
		}
		if req.Input == "" {
			t.Errorf("expected non-empty input field")
		}

		answers := make(map[string]any, len(req.Questions))
		for k, q := range req.Questions {
			switch q.Type {
			case "score":
				answers[k] = map[string]any{
					"expectedScore": 4.5,
					"confidence":    0.94,
					"probabilities": map[string]float64{"4": 0.5, "5": 0.5},
				}
			case "choice":
				sel := "architecture"
				if m, ok := q.Criteria.(map[string]any); ok {
					switch {
					case m["rule"] != nil:
						sel = "rule"
					case m["exclusive"] != nil:
						sel = "exclusive"
					case m["OVERWRITE"] != nil:
						sel = "OVERWRITE"
					case m["band_90"] != nil:
						sel = "band_90"
					}
				}
				answers[k] = map[string]any{
					"selectedOption": sel,
					"confidence":     0.91,
				}
			default:
				answers[k] = map[string]any{
					"noul":       0.92,
					"confidence": 0.92,
				}
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"answers": answers})
	}))
	defer srv.Close()

	t.Setenv("GEMINI_API_BASE", srv.URL)
	// Calling NewGeminiAdapterConfig with gemini-decision-1 must auto-select GeminiDecisionsAdapter.
	ev := NewGeminiAdapterConfig("gem-dec-key", "gemini-decision-1")
	if ev.Name() != "gemini:gemini-decision-1" {
		t.Fatalf("expected Name() = gemini:gemini-decision-1, got %q", ev.Name())
	}

	// Capability check: decision tasks allowed, generative-only tasks rejected cleanly.
	for _, tk := range []domain.EvalTask{domain.TaskRelevance, domain.TaskMutation, domain.TaskVerify, domain.TaskCalibrate} {
		if !ev.SupportsTask(tk) {
			t.Fatalf("expected gemini-decision-1 to support decision task %s", tk)
		}
	}
	for _, tk := range []domain.EvalTask{domain.TaskConsolidate, domain.TaskProgress, domain.TaskTaxonomy} {
		if ev.SupportsTask(tk) {
			t.Fatalf("expected gemini-decision-1 NOT to claim text-gen task %s", tk)
		}
	}

	facts := []domain.Fact{{ID: 10, Scope: "project:app", Category: "architecture", Content: "Always keep DB migrations transactional"}}
	scored, err := ev.ScoreRelevance(context.Background(), "write SQL migration", facts)
	if err != nil || len(scored) != 1 || scored[0].Score != 4.5 || scored[0].Confidence < 0.93 {
		t.Fatalf("ScoreRelevance failed: scored=%+v err=%v", scored, err)
	}
	if hits.Load() != 2 {
		t.Fatalf("expected 503 retry recovery on ScoreRelevance (2 hits), got %d", hits.Load())
	}

	dec, err := ev.EvaluateMutation(context.Background(), "project:app", "Migrations must run inside BEGIN/COMMIT", facts)
	if err != nil || dec.Action != "OVERWRITE" || dec.ConflictFactID != 10 || dec.Category != "architecture" {
		t.Fatalf("EvaluateMutation failed: dec=%+v err=%v", dec, err)
	}

	viols, err := ev.VerifyPlan(context.Background(), "Run DDL without transactions", facts)
	if err != nil || len(viols) != 1 || viols[0].ID != 10 || viols[0].Probability < 0.9 {
		t.Fatalf("VerifyPlan failed: viols=%+v err=%v", viols, err)
	}

	cal, err := ev.CalibrateConfidence(context.Background(), "project:app", facts)
	if err != nil || cal[10] != 0.92 {
		t.Fatalf("CalibrateConfidence failed: cal=%+v err=%v", cal, err)
	}
}

func TestGeminiDecisionAnswerOrdinalScoreConfidence(t *testing.T) {
	ptr := func(v float64) *float64 { return &v }

	// Adjacent ordinal mass split between 4 and 5 should reflect contiguous certainty (~0.80)
	// rather than single-bin argmax (0.51).
	splitAns := geminiDecisionAnswerWire{
		ExpectedScore: ptr(4.25),
		Confidence:    0.5126,
		Probabilities: map[string]float64{"5": 0.5126, "4": 0.2901, "3": 0.1439, "2": 0.03, "1": 0.015, "0": 0.0084},
	}.toDecisionAnswer()
	if splitAns.Confidence < 0.80 || splitAns.Confidence > 0.81 {
		t.Fatalf("expected ordinal adjacent-bin confidence ~0.8027, got %.4f", splitAns.Confidence)
	}

	// Uniform / bimodal distributions must remain low-confidence so uncertain matches are filtered.
	bimodalAns := geminiDecisionAnswerWire{
		ExpectedScore: ptr(2.5),
		Confidence:    0.50,
		Probabilities: map[string]float64{"0": 0.50, "1": 0, "2": 0, "3": 0, "4": 0, "5": 0.50},
	}.toDecisionAnswer()
	if bimodalAns.Confidence > 0.51 {
		t.Fatalf("expected bimodal confidence to stay <=0.50, got %.4f", bimodalAns.Confidence)
	}

	uniformAns := geminiDecisionAnswerWire{
		ExpectedScore: ptr(2.5),
		Confidence:    0.17,
		Probabilities: map[string]float64{"0": 0.17, "1": 0.17, "2": 0.17, "3": 0.17, "4": 0.16, "5": 0.16},
	}.toDecisionAnswer()
	if uniformAns.Confidence > 0.35 {
		t.Fatalf("expected uniform confidence to remain low (<=0.35), got %.4f", uniformAns.Confidence)
	}
}

func TestIsTypesafeCloudURL(t *testing.T) {
	cases := []struct {
		raw  string
		want bool
	}{
		{"https://api.typesafe.ai/v1/systemone", true},
		{"https://typesafe.ai/v1", true},
		{"https://API.TYPESAFE.AI:443/v1", true},
		{"https://evil.com/api.typesafe.ai", false},
		{"https://api.typesafe.ai.evil.com/v1", false},
		{"https://evil-typesafe.ai/v1", false},
		{"not a url", false},
	}
	for _, tc := range cases {
		if got := IsTypesafeCloudURL(tc.raw); got != tc.want {
			t.Fatalf("IsTypesafeCloudURL(%q) = %v, want %v", tc.raw, got, tc.want)
		}
	}
}
