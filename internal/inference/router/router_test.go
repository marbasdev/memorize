package router

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"praxis/internal/domain"
	"praxis/internal/infra/sqlite"
)

func TestDynamicResolverAndPerFeatureRouting(t *testing.T) {
	var decHits, genHits, localHits, flideHits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/sys" + "temone":
			flideHits.Add(1)
			var req struct {
				Questions map[string]struct {
					Type string `json:"type"`
				} `json:"questions"`
			}
			_ = json.NewDecoder(r.Body).Decode(&req)
			answers := make(map[string]any, len(req.Questions))
			for k := range req.Questions {
				answers[k] = map[string]any{"type": "noul", "noul": 0.92}
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"model": "OmniJev/OneJev1.6-0.8B", "answers": answers})
		case "/v1beta/decisions":
			decHits.Add(1)
			_, _ = w.Write([]byte(`{"answers":{"v_1":{"noul":0.91,"confidence":0.91},"f_1":{"expectedScore":4.2,"confidence":0.9}}}`))
		case "/v1beta/models/gemini-3.5-flash-lite:generateContent":
			genHits.Add(1)
			_, _ = w.Write([]byte(`{"candidates":[{"content":{"parts":[{"text":"{\"merged_content\":\"Unified transactional DB rule.\"}"}]}}]}`))
		case "/v1/chat/completions":
			localHits.Add(1)
			_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"{\"items\":[{\"id\":1,\"new_category\":\"database\",\"new_sub_category\":\"tx\",\"is_new_taxonomy\":false,\"reason\":\"db\"}]}"}}]}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()
	t.Setenv("GEMINI_API_BASE", srv.URL)

	res := NewTaskResolverWithEndpoint(nil)
	resolved, ok := res(map[string]string{
		"active_provider": "jev",
		"jev_api_url":     srv.URL,
		"jev_model":       "OmniJev/OneJev1.6-0.8B",
	}, "")
	if !ok || resolved == nil || resolved.Name() != "flide:OmniJev/OneJev1.6-0.8B" {
		t.Fatalf("expected resolver to instantiate flide:OmniJev/OneJev1.6-0.8B, got %v", resolved)
	}

	baseCfg := map[string]string{
		"active_provider":  "gemini",
		"gemini_model":     "gemini-decision-1",
		"gemini_api_key":   "k-gem",
		"local_model_url":  srv.URL + "/v1",
		"local_model_name": "qwen3:4b",
	}

	evRel, ok := res(baseCfg, domain.TaskRelevance)
	if !ok || evRel == nil || evRel.Name() != "gemini:gemini-decision-1" {
		t.Fatalf("expected gemini:gemini-decision-1 for relevance, got ev=%v ok=%v", evRel, ok)
	}
	evCons, ok := res(baseCfg, domain.TaskConsolidate)
	if !ok || evCons != nil {
		t.Fatalf("expected handled=true, nil evaluator when bulk is decision-only and no gen_provider, got ev=%v ok=%v", evCons, ok)
	}

	cfgWithGen := map[string]string{}
	for k, v := range baseCfg {
		cfgWithGen[k] = v
	}
	cfgWithGen["gen_provider"] = "gemini"
	evCons, ok = res(cfgWithGen, domain.TaskConsolidate)
	if !ok || evCons == nil || evCons.Name() != "gemini:gemini-3.5-flash-lite" {
		t.Fatalf("expected generative companion gemini:gemini-3.5-flash-lite, got ev=%v ok=%v", evCons, ok)
	}
	evVer, _ := res(cfgWithGen, domain.TaskVerify)
	if evVer == nil || evVer.Name() != "gemini:gemini-decision-1" {
		t.Fatalf("expected verify to remain gemini:gemini-decision-1, got %v", evVer)
	}

	cfgMixed := map[string]string{}
	for k, v := range cfgWithGen {
		cfgMixed[k] = v
	}
	cfgMixed["task_taxonomy_provider"] = "local"
	cfgMixed["task_taxonomy_model"] = "qwen3:4b"
	cfgMixed["task_consolidate_provider"] = "gemini"
	cfgMixed["task_consolidate_model"] = "gemini-decision-1"

	evTax, _ := res(cfgMixed, domain.TaskTaxonomy)
	if evTax == nil || evTax.Name() != "local:qwen3:4b" {
		t.Fatalf("expected taxonomy override local:qwen3:4b, got %v", evTax)
	}
	evCons2, _ := res(cfgMixed, domain.TaskConsolidate)
	if evCons2 == nil || evCons2.Name() != "gemini:gemini-3.5-flash-lite" {
		t.Fatalf("expected invalid decision override on consolidate to fall back to gemini-3.5-flash-lite, got %v", evCons2)
	}

	_, _ = evVer.VerifyPlan(context.Background(), "skip migrations", []domain.Fact{{ID: 1, Category: "database", Content: "Always wrap schema migrations"}})
	merged, err := evCons2.(domain.Consolidator).ConsolidateFacts(context.Background(), "project:app", "database", []domain.Fact{{ID: 1, Content: "a"}, {ID: 2, Content: "b"}})
	if err != nil || merged == "" {
		t.Fatalf("ConsolidateFacts via generative companion failed: merged=%q err=%v", merged, err)
	}
	taxItems, err := evTax.(domain.TaxonomyPlanner).SuggestTaxonomyRebalance(context.Background(), "project:app", []domain.Fact{{ID: 1, Category: "general", Content: "wrap db tx"}})
	if err != nil || len(taxItems) != 1 {
		t.Fatalf("SuggestTaxonomyRebalance via local override failed: items=%+v err=%v", taxItems, err)
	}
	if decHits.Load() != 1 || genHits.Load() != 1 || localHits.Load() != 1 {
		t.Fatalf("expected 1 hit each for decisions/generative/local, got dec=%d gen=%d local=%d", decHits.Load(), genHits.Load(), localHits.Load())
	}
	_ = flideHits.Load()
}

func TestRouterCircuitBreakerCooldownAndFallback(t *testing.T) {
	var decHits, flashHits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1beta/decisions":
			decHits.Add(1)
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"error":{"message":"Quota exceeded for gemini-decision-1"}}`))
		case "/v1beta/models/gemini-3.5-flash-lite:generateContent":
			flashHits.Add(1)
			_, _ = w.Write([]byte(`{"candidates":[{"content":{"parts":[{"text":"{\"items\":[{\"id\":1,\"score\":4.7,\"confidence\":0.94}]}"}]}}]}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()
	t.Setenv("GEMINI_API_BASE", srv.URL)

	res := NewTaskResolverWithEndpoint(nil)
	cfg := map[string]string{
		"active_provider":      "gemini",
		"gemini_model":         "gemini-3.5-flash-lite",
		"gemini_api_key":       "k-gem",
		"task_relevance_model": "gemini-decision-1",
	}

	ev1, ok := res(cfg, domain.TaskRelevance)
	if !ok || ev1 == nil || ev1.Name() != "gemini:gemini-decision-1" {
		t.Fatalf("expected initial Name() = gemini:gemini-decision-1, got %v", ev1)
	}

	facts := []domain.Fact{{ID: 1, Scope: "project:app", Category: "architecture", Content: "Use WAL mode"}}
	scored1, err := ev1.ScoreRelevance(context.Background(), "database setup", facts)
	if err != nil || len(scored1) != 1 || scored1[0].Score != 4.7 {
		t.Fatalf("expected seamless fallback on 429, got scored=%+v err=%v", scored1, err)
	}
	if ev1.Name() != "gemini:gemini-3.5-flash-lite" {
		t.Fatalf("expected effective Name() after fallback = gemini:gemini-3.5-flash-lite, got %q", ev1.Name())
	}
	if decHits.Load() != 1 || flashHits.Load() != 1 {
		t.Fatalf("expected 1 fast-fail hit on decisions and 1 fallback hit on flash-lite, got dec=%d flash=%d", decHits.Load(), flashHits.Load())
	}

	ev2, _ := res(cfg, domain.TaskRelevance)
	scored2, err := ev2.ScoreRelevance(context.Background(), "database setup", facts)
	if err != nil || len(scored2) != 1 {
		t.Fatalf("expected second call during cooldown to succeed via fallback, got %+v (%v)", scored2, err)
	}
	if decHits.Load() != 1 || flashHits.Load() != 2 {
		t.Fatalf("expected cooldown to skip decisions endpoint (decHits=1, flashHits=2), got dec=%d flash=%d", decHits.Load(), flashHits.Load())
	}
}

func TestFallbackEvaluatorInputTokenLimitPropagation(t *testing.T) {
	t.Setenv("GEMINI_API_BASE", "")
	res := NewTaskResolverWithEndpoint(nil)

	cfgBoth := map[string]string{
		"active_provider":      "gemini",
		"gemini_model":         "gemini-3.5-flash-lite",
		"gemini_api_key":       "k-gem",
		"task_relevance_model": "gemini-decision-1",
	}
	evBoth, ok := res(cfgBoth, domain.TaskRelevance)
	if !ok || evBoth == nil {
		t.Fatal("expected resolved evaluator for TaskRelevance")
	}
	if got := domain.EvaluatorInputTokenLimit(context.Background(), evBoth); got != 262144 {
		t.Fatalf("expected min known limit=262144 across primary+fallback, got %d", got)
	}

	srv := httptest.NewServer(http.NotFoundHandler())
	defer srv.Close()
	cfgUnknownFB := map[string]string{
		"active_provider":      "local",
		"local_model_url":      srv.URL + "/v1",
		"local_model_name":     "ollama-unknown",
		"gemini_api_key":       "k-gem",
		"task_verify_provider": "gemini",
		"task_verify_model":    "gemini-decision-1",
	}
	evUnknown, _ := res(cfgUnknownFB, domain.TaskVerify)
	if got := domain.EvaluatorInputTokenLimit(context.Background(), evUnknown); got != 0 {
		t.Fatalf("expected conservative 0 when fallback context limit is unknown, got %d", got)
	}
}

type stubRouterEval struct{ name string }

func (s stubRouterEval) Name() string { return s.name }
func (stubRouterEval) ScoreRelevance(_ context.Context, _ string, _ []domain.Fact) ([]domain.ScoredFact, error) {
	return nil, nil
}
func (stubRouterEval) EvaluateMutation(_ context.Context, _, _ string, _ []domain.Fact) (domain.MutationDecision, error) {
	return domain.MutationDecision{}, nil
}
func (stubRouterEval) VerifyPlan(_ context.Context, _ string, _ []domain.Fact) ([]domain.PlanViolation, error) {
	return nil, nil
}

func TestRouterTelemetryAndReadiness(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "router.db")
	db, err := sqlite.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	ctx := context.Background()
	r := New(db, dbPath, stubRouterEval{name: "stub:v1"}, nil)
	if got := r.ProviderNameForTask(ctx, domain.TaskVerify); got != "stub:v1" {
		t.Fatalf("ProviderNameForTask = %q", got)
	}

	r.RecordTelemetry(ctx, domain.TaskVerify, stubRouterEval{name: "gemini:flash"}, time.Now().Add(-20*time.Millisecond), 5, nil)
	r.RecordTelemetry(ctx, domain.TaskVerify, stubRouterEval{name: "gemini:flash"}, time.Now().Add(-50*time.Millisecond), 5, context.DeadlineExceeded)
	r.RecordTelemetry(ctx, domain.TaskRelevance, stubRouterEval{name: "gemini:flash"}, time.Now().Add(-15*time.Millisecond), 3, errors.New("upstream 500"))

	rep, err := r.SummarizeTelemetry(ctx, 24)
	if err != nil {
		t.Fatalf("SummarizeTelemetry: %v", err)
	}
	if rep.TotalCalls != 3 || rep.TotalTimeouts != 1 || rep.TotalErrors != 1 || len(rep.Engines) != 1 {
		t.Fatalf("unexpected telemetry summary: %+v", rep)
	}
	if rep.ByTask[string(domain.TaskVerify)].Calls != 2 || rep.P95ElapsedMs <= 0 {
		t.Fatalf("unexpected verify task telemetry: %+v", rep.ByTask[string(domain.TaskVerify)])
	}
	if err := r.ResetTelemetry(ctx); err != nil {
		t.Fatalf("ResetTelemetry: %v", err)
	}
	afterReset, err := r.SummarizeTelemetry(ctx, 24)
	if err != nil || afterReset.TotalCalls != 0 {
		t.Fatalf("expected 0 calls after reset, got %+v err=%v", afterReset, err)
	}

	t.Setenv("GEMINI_API_KEY", "")
	if ready, _ := CheckConfiguredEngineReadiness(ctx, map[string]string{"active_provider": "gemini"}, nil); ready {
		t.Fatalf("expected gemini without key to report not ready")
	}
	if ready, reason := CheckConfiguredEngineReadiness(ctx, map[string]string{"active_provider": "gemini", "gemini_api_key": "k-ok"}, nil); !ready || reason != "" {
		t.Fatalf("expected gemini with key ready, got ready=%v reason=%q", ready, reason)
	}
}
