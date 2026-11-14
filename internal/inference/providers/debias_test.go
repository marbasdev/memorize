package providers_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"memorize/internal/domain"
	"memorize/internal/inference/providers"
)

func TestLocalLayaMicroBatching(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"answers":{"perm":{"noul":0.9,"confidence":0.9},"inv":{"noul":0.2,"confidence":0.9},"cat":{"choice":"architecture","confidence":0.9},"r_6":{"choice":"OVERWRITE","confidence":0.92}}}`))
	}))
	defer srv.Close()

	t.Setenv("LOCAL_MODEL_URL", srv.URL)
	adapter := providers.NewLocalLayaAdapter()

	var facts []domain.Fact
	for i := int64(1); i <= 7; i++ {
		facts = append(facts, domain.Fact{ID: i, Scope: "project:demo", Category: "architecture", Content: "rule"})
	}
	dec, err := adapter.EvaluateMutation(context.Background(), "project:demo", "new rule", facts)
	if err != nil {
		t.Fatalf("LocalLayaAdapter error: %v", err)
	}
	if dec.Action != "OVERWRITE" || dec.ConflictFactID != 6 {
		t.Fatalf("expected OVERWRITE of fact 6 across batches, got %+v", dec)
	}
}

func TestGeminiHeaderAndLogprobsDebiasing(t *testing.T) {
	mode := "tokens"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.RawQuery != "" {
			t.Errorf("API key leaked into URL query string: %q", r.URL.RawQuery)
		}
		if got := r.Header.Get("x-goog-api-key"); got != "header-only-test-key" {
			t.Errorf("expected x-goog-api-key header, got %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		if mode == "tokens" {
			_, _ = w.Write([]byte(`{
				"candidates": [{
					"content": {"parts": [{"text": "{\"items\":[{\"id\":1,\"score\":2.5}]}"}]},
					"avgLogprobs": -0.05,
					"logprobsResult": {
						"chosenCandidates": [
							{"token": "{", "logProbability": -0.0001},
							{"token": "\"items\"", "logProbability": -0.0001},
							{"token": ":[{", "logProbability": -0.0001},
							{"token": "\"id\"", "logProbability": -0.0001},
							{"token": "1", "logProbability": -0.65},
							{"token": "\"score\"", "logProbability": -0.0001},
							{"token": "2.5", "logProbability": -0.65},
							{"token": "}]}", "logProbability": -0.0001}
						]
					}
				}]
			}`))
			return
		}
		_, _ = w.Write([]byte(`{
			"candidates": [{
				"content": {"parts": [{"text": "{\"items\":[{\"id\":1,\"score\":4.1}]}"}]},
				"avgLogprobs": -0.12
			}]
		}`))
	}))
	defer srv.Close()

	t.Setenv("GEMINI_API_BASE", srv.URL)
	t.Setenv("GEMINI_API_KEY", "header-only-test-key")
	gem := providers.NewGeminiAdapter()

	facts := []domain.Fact{{ID: 1, Scope: "project:gem", Category: "security", Content: "Use HttpOnly cookies"}}
	scored1, err := gem.ScoreRelevance(context.Background(), "login handler", facts)
	if err != nil || len(scored1) != 1 {
		t.Fatalf("Gemini ScoreRelevance (tokens) failed: %v (%+v)", err, scored1)
	}
	if scored1[0].Confidence >= 0.60 {
		t.Fatalf("expected debiased semantic token confidence < 0.60, got %.3f", scored1[0].Confidence)
	}

	mode = "avg_fallback"
	scored2, err := gem.ScoreRelevance(context.Background(), "login handler", facts)
	if err != nil || len(scored2) != 1 {
		t.Fatalf("Gemini ScoreRelevance (fallback) failed: %v (%+v)", err, scored2)
	}
	if scored2[0].Confidence >= 0.80 || scored2[0].Confidence < 0.70 {
		t.Fatalf("expected debiased avgLogprobs fallback in [0.70, 0.80), got %.3f", scored2[0].Confidence)
	}
}

func TestGeminiSchemaDialectAndLogprobsFallbackRetry(t *testing.T) {
	var calls int32
	mockGemini := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&calls, 1)
		bodyBytes, _ := io.ReadAll(r.Body)
		bodyStr := string(bodyBytes)
		if !strings.Contains(bodyStr, `"OBJECT"`) || strings.Contains(bodyStr, `"additionalProperties"`) {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":{"message":"invalid OpenAPI responseSchema dialect"}}`))
			return
		}
		if n == 1 && strings.Contains(bodyStr, `"responseLogprobs":true`) {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":{"message":"responseLogprobs is not supported for gemini-3.8-flash"}}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"candidates":[{"content":{"parts":[{"text":"{\"items\":[{\"id\":7,\"score\":4.7}]}"}]}}]}`))
	}))
	defer mockGemini.Close()

	t.Setenv("GEMINI_API_BASE", mockGemini.URL)
	gemEval := providers.NewGeminiAdapterConfig("test-key", "gemini-3.8-flash")
	scored, err := gemEval.ScoreRelevance(context.Background(), "테스트 작업", []domain.Fact{{ID: 7, Scope: "project:sec", Category: "security", Content: "보안 규칙"}})
	if err != nil {
		t.Fatalf("GeminiAdapter fallback ScoreRelevance failed: %v", err)
	}
	if atomic.LoadInt32(&calls) != 2 || len(scored) != 1 || scored[0].Score != 4.7 || scored[0].Confidence < 0.85 {
		t.Fatalf("expected 2 calls (400 retry -> 200) and score 4.7, got calls=%d scored=%+v", atomic.LoadInt32(&calls), scored)
	}
}
