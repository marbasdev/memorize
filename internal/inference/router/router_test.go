package router

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"memorize/internal/domain"
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

	resolver := NewDynamicResolver()
	resolved := resolver(map[string]string{
		"active_provider": "jev",
		"jev_api_url":     srv.URL,
		"jev_model":       "OmniJev/OneJev1.6-0.8B",
	})
	if resolved == nil || resolved.Name() != "flide:OmniJev/OneJev1.6-0.8B" {
		t.Fatalf("expected resolver to instantiate flide:OmniJev/OneJev1.6-0.8B, got %v", resolved)
	}

	res := NewTaskResolver()
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
