package providers

import (
	"context"
	"encoding/json"
	"io"
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
					} else if _, hasViol := m["VIOLATION"]; hasViol {
						ch = "VIOLATION"
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
	verifyOutcome := "VIOLATION"
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
					case m["VIOLATION"] != nil:
						sel = verifyOutcome
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
	ev := NewGeminiAdapterConfig("gem-dec-key", "gemini-decision-1")
	if ev.Name() != "gemini:gemini-decision-1" {
		t.Fatalf("expected Name() = gemini:gemini-decision-1, got %q", ev.Name())
	}

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
	for _, nonViol := range []string{"ALLOW"} {
		verifyOutcome = nonViol
		got, err := ev.VerifyPlan(context.Background(), "Run gofmt -l .", facts)
		if err != nil || len(got) != 0 {
			t.Fatalf("expected %s to yield 0 violations, got %+v (err=%v)", nonViol, got, err)
		}
	}

	cal, err := ev.CalibrateConfidence(context.Background(), "project:app", facts)
	if err != nil || cal[10] != 0.92 {
		t.Fatalf("CalibrateConfidence failed: cal=%+v err=%v", cal, err)
	}
}

func TestGeminiDecisionAnswerOrdinalScoreConfidence(t *testing.T) {
	ptr := func(v float64) *float64 { return &v }

	splitAns := geminiDecisionAnswerWire{
		ExpectedScore: ptr(4.25),
		Confidence:    0.5126,
		Probabilities: map[string]float64{"5": 0.5126, "4": 0.2901, "3": 0.1439, "2": 0.03, "1": 0.015, "0": 0.0084},
	}.toDecisionAnswer()
	if splitAns.Confidence < 0.80 || splitAns.Confidence > 0.81 {
		t.Fatalf("expected ordinal adjacent-bin confidence ~0.8027, got %.4f", splitAns.Confidence)
	}

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

func TestContextWindowProbingForGGUFOpenAIAndGemini(t *testing.T) {
	ctx := context.Background()

	ggufSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/props" {
			_, _ = w.Write([]byte(`{"default_generation_settings":{"n_ctx":8192}}`))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer ggufSrv.Close()
	ggufEv := NewGGUFAdapter(ggufSrv.URL, "qwen3.gguf")
	if got := domain.EvaluatorInputTokenLimit(ctx, ggufEv); got != 8192 {
		t.Fatalf("expected GGUF InputTokenLimit=8192 from /props, got %d", got)
	}

	vllmSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/models" {
			_, _ = w.Write([]byte(`{"data":[{"id":"qwen3-coder","max_model_len":16384},{"id":"ollama-only"}]}`))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer vllmSrv.Close()
	vllmEv := NewOpenAICompatibleAdapter(vllmSrv.URL+"/v1", "qwen3-coder", "")
	if got := domain.EvaluatorInputTokenLimit(ctx, vllmEv); got != 16384 {
		t.Fatalf("expected vLLM max_model_len=16384, got %d", got)
	}
	ollamaEv := NewOpenAICompatibleAdapter(vllmSrv.URL+"/v1", "ollama-only", "")
	if got := domain.EvaluatorInputTokenLimit(ctx, ollamaEv); got != 0 {
		t.Fatalf("expected unknown Ollama context limit to return 0 (no guessing), got %d", got)
	}

	gemSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1beta/models/gemini-custom-test" {
			_, _ = w.Write([]byte(`{"name":"models/gemini-custom-test","inputTokenLimit":32768}`))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer gemSrv.Close()
	t.Setenv("GEMINI_API_BASE", gemSrv.URL)

	gemCustom := NewGeminiAdapterConfig("k-gem", "gemini-custom-test")
	if got := domain.EvaluatorInputTokenLimit(ctx, gemCustom); got != 32768 {
		t.Fatalf("expected custom Gemini model limit=32768, got %d", got)
	}
	gemDec := NewGeminiDecisionsAdapterConfig("k-gem", "gemini-decision-1")
	if got := domain.EvaluatorInputTokenLimit(ctx, gemDec); got != 262144 {
		t.Fatalf("expected canonical gemini-decision-1 limit=262144, got %d", got)
	}
}

func TestSchemaEvaluatorSummarizeCommitMilestones(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"choices": [{
				"message": {
					"content": "{\"milestones\":[{\"track\":\"Guardrail\",\"category\":\"architecture\",\"sub_category\":\"guardrail\",\"date\":\"2026-11-18\",\"summary\":\"Diff chunking and Stop retry loop completed\",\"commit_shas\":[\"aaa1111\",\"bbb2222\",\"ccc3333\"]}]}"
				}
			}]
		}`))
	}))
	defer srv.Close()

	ev := NewOpenAICompatibleAdapter(srv.URL+"/v1", "qwen3:4b", "")
	ms, ok := any(ev).(domain.ProgressMilestoneSummarizer)
	if !ok {
		t.Fatal("expected BatchEvaluator to implement ProgressMilestoneSummarizer")
	}
	got, err := ms.SummarizeCommitMilestones(context.Background(), "project:memorize", "en", []domain.GitCommitBrief{
		{SHA: "aaa1111", Date: "2026-11-18", Subject: "feat: chunk diffs"},
		{SHA: "bbb2222", Date: "2026-11-18", Subject: "feat: add budget probe"},
		{SHA: "ccc3333", Date: "2026-11-18", Subject: "feat: bounded stop re-verify"},
	})
	if err != nil || len(got) != 1 || len(got[0].CommitSHAs) != 3 {
		t.Fatalf("expected 1 synthesized milestone covering 3 SHAs, got %+v (err=%v)", got, err)
	}
}

func TestDriverRegistryBuiltinsAndAlias(t *testing.T) {
	for _, id := range []string{"gguf", "gemini", "jev", "local"} {
		drv, ok := LookupDriver(id)
		if !ok || drv.ID != id {
			t.Fatalf("expected driver %q registered, got ok=%v drv=%+v", id, ok, drv)
		}
		if drv.CacheKey == nil || drv.Build == nil || drv.CheckReadiness == nil {
			t.Fatalf("driver %q missing required hooks", id)
		}
	}
	drv, ok := LookupDriver("flide")
	if !ok || drv.ID != "jev" {
		t.Fatalf("expected flide alias to resolve to jev driver, got %+v ok=%v", drv, ok)
	}
	if ev := StartupEvaluator("flide", DriverBuildInput{GGUFSupported: true}); ev == nil || !strings.HasPrefix(ev.Name(), "flide:") {
		t.Fatalf("expected flide: startup evaluator, got %v", ev)
	}
}

func TestRegisterCustomDriver(t *testing.T) {
	RegisterDriver(ProviderDriver{
		ID:       "custom-test-drv",
		CacheKey: func(in DriverBuildInput) string { return "custom|" + in.ModelOverride },
		Build:    func(_ DriverBuildInput) domain.Evaluator { return nil },
		CheckReadiness: func(_ context.Context, _ DriverBuildInput) (bool, string) {
			return true, ""
		},
	})
	t.Cleanup(func() {
		driversMu.Lock()
		delete(drivers, "custom-test-drv")
		driversMu.Unlock()
	})
	drv, ok := LookupDriver("custom-test-drv")
	if !ok || drv.CacheKey(DriverBuildInput{ModelOverride: "m1"}) != "custom|m1" {
		t.Fatalf("unexpected custom driver state: ok=%v drv=%+v", ok, drv)
	}
}

func TestDraftRulePackProposalSanitizesAndValidatesAfterIndex(t *testing.T) {
	var seenPrompt string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		bodyBytes, _ := io.ReadAll(r.Body)
		seenPrompt = string(bodyBytes)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"candidates": [{
				"content": {
					"parts": [{
						"text": "{\"category\":\"database\",\"sub_category\":\"SQLite Driver\",\"title\":\"[invariant] Always compile with CGO_ENABLED=0\\nusing modernc.org/sqlite across all OS targets.\",\"after_index\":1}"
					}]
				}
			}]
		}`))
	}))
	defer srv.Close()

	t.Setenv("GEMINI_API_BASE", srv.URL)
	ev := NewGeminiAdapterConfig("test-key", "gemini-2.5-flash")
	planner, ok := any(ev).(domain.RulePackPlanner)
	if !ok {
		t.Fatalf("expected Gemini BatchEvaluator to implement domain.RulePackPlanner")
	}

	ctx := domain.WithPresetCategories(context.Background(), `{"database":["sqlite-driver"]}`)
	outline := []domain.RuleOutlineEntry{
		{RuleKey: "db-wal-mode", Category: "database", SubCategory: "sqlite-driver", Title: "Enable WAL mode."},
	}
	prop, err := planner.DraftRulePackProposal(ctx, "project:app", "CGO 없이 modernc.org/sqlite 사용", "database", "", outline)
	if err != nil {
		t.Fatalf("DraftRulePackProposal failed: %v", err)
	}
	if !strings.Contains(seenPrompt, "[1] (database/sqlite-driver) Enable WAL mode.") || strings.Contains(seenPrompt, "db-wal-mode") || !strings.Contains(seenPrompt, "CGO 없이") {
		t.Fatalf("expected indexed prompt without exposed keys, got: %s", seenPrompt)
	}
	wantKey := domain.ComputeRuleKey("database", prop.Title)
	if prop.RuleKey != wantKey || !strings.HasPrefix(prop.RuleKey, "r-") {
		t.Fatalf("unexpected hashed rule_key %q (want %q)", prop.RuleKey, wantKey)
	}
	if prop.AfterKey != "" {
		t.Fatalf("expected empty after_key when appending to category end, got %q", prop.AfterKey)
	}
	if strings.Contains(prop.Title, "`[invariant]`") || strings.Contains(prop.Title, "\n") {
		t.Fatalf("expected title sanitized to single line without tier prefix, got %q", prop.Title)
	}
}

func TestFindRedundantLocalMemoriesFiltersAndShortens(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		if n == 1 {
			longTrimmed := strings.Repeat("한국어고유규칙세부사항", 20) // > 180 runes
			payload, _ := json.Marshal(map[string]any{
				"items": []map[string]any{
					{"local_memory_id": 10, "team_rule_index": 1, "kind": "duplicate", "trimmed_content": "should be ignored", "reason": "팀 룰과 동일"},
					{"local_memory_id": 11, "team_rule_index": 2, "kind": "partial_overlap", "trimmed_content": longTrimmed, "reason": "로컬 디버그 포트 정보만 고유"},
					{"local_memory_id": 11, "team_rule_index": 2, "kind": "duplicate", "trimmed_content": "", "reason": "duplicate local id ignored"},
					{"local_memory_id": 999, "team_rule_index": 1, "kind": "duplicate", "trimmed_content": "", "reason": "unknown local id"},
					{"local_memory_id": 12, "team_rule_index": 99, "kind": "duplicate", "trimmed_content": "", "reason": "unknown team rule"},
				},
			})
			resp, _ := json.Marshal(map[string]any{
				"candidates": []map[string]any{
					{"content": map[string]any{"parts": []map[string]any{{"text": string(payload)}}}},
				},
			})
			_, _ = w.Write(resp)
			return
		}
		_, _ = w.Write([]byte(`{
			"candidates": [{
				"content": {
					"parts": [{
						"text": "{\"trimmed_content\":\"로컬 디버그 서버는 19090 포트를 사용한다.\"}"
					}]
				}
			}]
		}`))
	}))
	defer srv.Close()

	t.Setenv("GEMINI_API_BASE", srv.URL)
	ev := NewGeminiAdapterConfig("test-key", "gemini-2.5-flash")

	teamRules := []domain.TeamRule{
		{RuleKey: "db-no-cgo", Scope: "project:app", Category: "database", Pinned: true, Title: "Never enable CGO.", Why: "Static binaries.", How: "CGO_ENABLED=0"},
		{RuleKey: "api-auth", Scope: "project:app", Category: "api", Title: "Require bearer auth.", Why: "Security.", How: "Middleware"},
	}
	localFacts := []domain.Fact{
		{ID: 10, Scope: "project:app", Category: "database", Content: "CGO를 끄고 빌드한다."},
		{ID: 11, Scope: "project:app", Category: "api", Content: "Bearer auth를 쓰고 로컬 디버그 서버는 19090 포트를 사용한다."},
		{ID: 12, Scope: "project:app", Category: "testing", Content: "Unit tests run in parallel."},
	}

	out, err := ev.FindRedundantLocalMemories(context.Background(), "project:app", teamRules, localFacts)
	if err != nil {
		t.Fatalf("FindRedundantLocalMemories failed: %v", err)
	}
	if len(out) != 2 {
		t.Fatalf("expected exactly 2 validated suggestions, got %+v", out)
	}
	if out[0].LocalMemoryID != 10 || out[0].Kind != "duplicate" || out[0].TrimmedContent != "" {
		t.Fatalf("unexpected first suggestion: %+v", out[0])
	}
	if out[1].LocalMemoryID != 11 || out[1].Kind != "partial_overlap" || out[1].TrimmedContent != "로컬 디버그 서버는 19090 포트를 사용한다." {
		t.Fatalf("expected shortened partial_overlap trimmed_content, got: %+v", out[1])
	}
	if calls.Load() != 2 {
		t.Fatalf("expected 1 main call + 1 shortening retry = 2 calls, got %d", calls.Load())
	}
}

func TestLocalLayaMicroBatching(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"answers":{"perm":{"noul":0.9,"confidence":0.9},"inv":{"noul":0.2,"confidence":0.9},"cat":{"choice":"architecture","confidence":0.9},"r_6":{"choice":"OVERWRITE","confidence":0.92}}}`))
	}))
	defer srv.Close()

	t.Setenv("LOCAL_MODEL_URL", srv.URL)
	adapter := NewLocalLayaAdapterConfig("", "")

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
	gem := NewGeminiAdapterConfig("", "")

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
	gemEval := NewGeminiAdapterConfig("test-key", "gemini-3.8-flash")
	scored, err := gemEval.ScoreRelevance(context.Background(), "테스트 작업", []domain.Fact{{ID: 7, Scope: "project:sec", Category: "security", Content: "보안 규칙"}})
	if err != nil {
		t.Fatalf("GeminiAdapter fallback ScoreRelevance failed: %v", err)
	}
	if atomic.LoadInt32(&calls) != 2 || len(scored) != 1 || scored[0].Score != 4.7 || scored[0].Confidence < 0.85 {
		t.Fatalf("expected 2 calls (400 retry -> 200) and score 4.7, got calls=%d scored=%+v", atomic.LoadInt32(&calls), scored)
	}
}
