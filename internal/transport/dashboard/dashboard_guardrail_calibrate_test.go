package dashboard_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"memorize/internal/domain"
	"memorize/internal/inference/providers"
	"memorize/internal/transport/dashboard"
	thttp "memorize/internal/transport/http"
)

type plannerEval struct {
	noopEval
}

func (plannerEval) Name() string { return "mock-ai-planner" }

func (plannerEval) SuggestConsolidations(_ context.Context, _ string, facts []domain.Fact) ([]domain.ConsolidationProposal, error) {
	if len(facts) < 2 {
		return nil, nil
	}
	return []domain.ConsolidationProposal{
		{
			SourceIDs:     []int64{facts[0].ID, facts[1].ID},
			Category:      "architecture/ui",
			MergedContent: "UI 템플릿과 커스텀 정적 자산은 각 800줄 미만으로 모듈 분리해 유지한다.",
			Reason:        "UI 모듈 라인 제한 중복 통합",
		},
	}, nil
}

func (plannerEval) SuggestTaxonomyRebalance(_ context.Context, _ string, facts []domain.Fact) ([]domain.TaxonomyRebalanceItem, error) {
	var out []domain.TaxonomyRebalanceItem
	for _, f := range facts {
		if f.SubCategory == "" {
			out = append(out, domain.TaxonomyRebalanceItem{
				ID:             f.ID,
				NewCategory:    f.Category,
				NewSubCategory: "tilemap",
				Reason:         "미분류 규칙 서브카테고리 배정",
			})
		}
	}
	return out, nil
}

func TestAIConsolidationSuggestAndOneClickConfirm(t *testing.T) {
	ctx := context.Background()
	deps := newTestDeps(t, ":memory:", plannerEval{}, "", thttp.DashboardHooks{})
	svc := deps.Memory

	if _, err := svc.CommitMemory(ctx, "UI 템플릿 파일은 800줄 미만으로 유지한다", "architecture/ui", "project:ai-merge"); err != nil {
		t.Fatalf("CommitMemory 1: %v", err)
	}
	if _, err := svc.CommitMemory(ctx, "커스텀 CSS 및 JS 자산 파일도 800줄 미만으로 분리한다", "architecture/ui", "project:ai-merge"); err != nil {
		t.Fatalf("CommitMemory 2: %v", err)
	}
	if _, err := svc.CommitMemory(ctx, "데이터베이스 트랜잭션은 WAL 모드로 동작한다", "database/schema", "project:ai-merge"); err != nil {
		t.Fatalf("CommitMemory 3: %v", err)
	}
	if _, err := svc.CommitMemoryFull(ctx, "분포 패널은 2단 타일 맵으로 표시한다", "architecture", "", "", "project:ai-merge"); err != nil {
		t.Fatalf("CommitMemory 4 (unclassified): %v", err)
	}

	h := dashboard.NewHandler(deps)
	ts := httptest.NewServer(h)
	defer ts.Close()

	// 1. Opening /ui/modal/consolidate defaults to AI Recommend mode on the richest scope (project:ai-merge)
	modalResp, err := http.Get(ts.URL + "/ui/modal/consolidate")
	if err != nil || modalResp.StatusCode != http.StatusOK {
		t.Fatalf("GET /ui/modal/consolidate failed: %v", err)
	}
	modalBytes, _ := io.ReadAll(modalResp.Body)
	_ = modalResp.Body.Close()
	modalHTML := string(modalBytes)
	if !strings.Contains(modalHTML, `hx-post="/ui/consolidate/suggest"`) || !strings.Contains(modalHTML, `<option value="project:ai-merge" selected>`) {
		t.Fatalf("expected AI Recommend modal defaulting to project:ai-merge, got:\n%s", modalHTML)
	}

	// 2. POST /ui/consolidate/suggest returns unified AI merge proposals AND re-categorization items together
	sugReq, _ := http.NewRequest(http.MethodPost, ts.URL+"/ui/consolidate/suggest", strings.NewReader("merge_scope=project:ai-merge"))
	sugReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	sugReq.Header.Set("HX-Request", "true")
	sugResp, err := http.DefaultClient.Do(sugReq)
	if err != nil || sugResp.StatusCode != http.StatusOK {
		t.Fatalf("POST /ui/consolidate/suggest failed: %v (%v)", sugResp, err)
	}
	sugBytes, _ := io.ReadAll(sugResp.Body)
	_ = sugResp.Body.Close()
	sugHTML := string(sugBytes)
	for _, needle := range []string{
		`id="btnConfirmAIMerge"`, `value="1,2"`, `UI 모듈 라인 제한 중복 통합`, `mock-ai-planner`,
		`AI Merge Recommendations`, `architecture/ui · 2 -&gt; 1`,
		`AI Re-categorize`, `value="tilemap"`,
	} {
		if !strings.Contains(sugHTML, needle) {
			t.Fatalf("expected unified AI suggestion partial to contain %q, got:\n%s", needle, sugHTML)
		}
	}

	// 3. One-click Confirm (POST /ui/consolidate/apply-plan) merges #1 & #2 into #5 AND re-categorizes #4 -> architecture/tilemap
	applyForm := url.Values{
		"merge_scope":     {"project:ai-merge"},
		"group_idx":       {"0"},
		"group_0_ids":     {"1,2"},
		"group_0_cat":     {"architecture/ui"},
		"group_0_pinned":  {"1"},
		"group_0_content": {"UI 템플릿과 커스텀 정적 자산은 각 800줄 미만으로 모듈 분리해 유지한다."},
		"item_idx":        {"0"},
		"item_0_id":       {"4"},
		"item_0_cat":      {"architecture"},
		"item_0_sub":      {"tilemap"},
	}
	appReq, _ := http.NewRequest(http.MethodPost, ts.URL+"/ui/consolidate/apply-plan", strings.NewReader(applyForm.Encode()))
	appReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	appReq.Header.Set("HX-Request", "true")
	appResp, err := http.DefaultClient.Do(appReq)
	if err != nil || appResp.StatusCode != http.StatusOK {
		t.Fatalf("POST /ui/consolidate/apply-plan failed: %v (%v)", appResp, err)
	}
	_ = appResp.Body.Close()
	if !strings.Contains(appResp.Header.Get("HX-Trigger"), "memorizeCloseModal") {
		t.Fatalf("expected memorizeCloseModal HX-Trigger after apply-plan")
	}

	page, err := svc.QueryRecordsPage(ctx, domain.RecordQuery{Scope: "project:ai-merge", State: "active", Page: 1, PageSize: 20})
	if err != nil || len(page.Records) != 3 {
		t.Fatalf("expected 3 active records (#5 merged + #4 re-categorized + #3 untouched), got %+v (%v)", page.Records, err)
	}
	if page.Records[0].ID != 5 || !page.Records[0].Pinned || !strings.Contains(page.Records[0].Content, "800줄 미만") {
		t.Fatalf("expected newest active record #5 to be pinned merged rule, got %+v", page.Records[0])
	}
	rec4, _ := svc.GetRecordsByIDs(ctx, []int64{4})
	if len(rec4) != 1 || rec4[0].SubCategory != "tilemap" || rec4[0].Content != "분포 패널은 2단 타일 맵으로 표시한다" {
		t.Fatalf("expected #4 re-categorized to tilemap with untouched content, got %+v", rec4)
	}
}

func TestIndividualConfidenceAndRecalibration(t *testing.T) {
	ctx := context.Background()

	// Verify DefaultThresholds match Write Gate (0.75, 0.80, 0.88) & Read Gate (3.5, 0.75)
	defTh := domain.DefaultThresholds()
	if defTh.MinPermanent != 0.75 || defTh.MinConfidence != 0.80 || defTh.PinInvariant != 0.88 || defTh.MinRelevance != 3.5 || defTh.MinRelConfidence != 0.75 {
		t.Fatalf("unexpected DefaultThresholds: %+v", defTh)
	}

	// Mock OpenAI-compatible schema transport without logprobs to prove per-item 5-band confidence works without 0.85 fallback
	llm := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Messages []struct {
				Content string `json:"content"`
			} `json:"messages"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		prompt := ""
		if len(req.Messages) > 0 {
			prompt = req.Messages[len(req.Messages)-1].Content
		}
		var content string
		switch {
		case strings.Contains(prompt, "Assign an individual semantic confidence"):
			content = `{"items":[{"id":1,"confidence":0.97},{"id":2,"confidence":0.92},{"id":3,"confidence":0.82}]}`
		case strings.Contains(prompt, "Evaluate each fact individually against the Current Task"):
			content = `{"items":[{"id":2,"score":4.4,"confidence":0.93},{"id":3,"score":4.1,"confidence":0.68}]}`
		default:
			content = `{"category":"architecture","sub_category":"core","tags":["test"],"tier":"rule","cardinality":"additive","is_permanent":0.90,"is_global_invariant":0.40,"confidence":0.91,"action":"INSERT","conflict_fact_ids":[]}`
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"choices":[{"message":{"content":%q}}]}`, content)
	}))
	defer llm.Close()

	ev := providers.NewOpenAICompatibleAdapter(llm.URL+"/v1", "qwen3:4b", "")
	deps := newTestDeps(t, ":memory:", ev, "", thttp.DashboardHooks{})
	svc := deps.Memory

	for _, txt := range []string{
		"CGO 없이 순수 Go로만 빌드한다",
		"API 타임아웃은 기본 10초로 설정한다",
		"주석은 가급적 간결한 문장을 선호한다",
	} {
		if _, err := svc.CommitMemory(ctx, txt, "architecture", "project:conf"); err != nil {
			t.Fatalf("CommitMemory(%q): %v", txt, err)
		}
	}
	pinTrue := true
	sub := "build"
	if err := svc.UpdateMemoryFull(domain.WithAdminBypass(ctx), 1, "project:conf", "architecture", &sub, "CGO 없이 순수 Go로만 빌드한다", &pinTrue); err != nil {
		t.Fatalf("pin #1: %v", err)
	}

	ts := httptest.NewServer(dashboard.NewHandler(deps))
	defer ts.Close()

	// 1. SSR root renders Write Gate & Read Gate grouping and horizontal #confHist
	rootResp, err := http.Get(ts.URL + "/?lang=ko")
	if err != nil || rootResp.StatusCode != http.StatusOK {
		t.Fatalf("GET /?lang=ko failed: %v", err)
	}
	rootBytes, _ := io.ReadAll(rootResp.Body)
	_ = rootResp.Body.Close()
	rootHTML := string(rootBytes)
	for _, want := range []string{"저장 게이트", "조회 게이트", "개별 확신도 재산정", `class="hist-bars" id="confHist"`} {
		if !strings.Contains(rootHTML, want) {
			t.Fatalf("expected SSR root HTML to contain %q", want)
		}
	}

	// 2. Recalibrate via POST /ui/calibrate-confidence updates individual confidences for cal_scope while keeping #confHist independent
	calReq, _ := http.NewRequest(http.MethodPost, ts.URL+"/ui/calibrate-confidence?lang=ko", strings.NewReader("cal_scope=project:conf&scope=project:conf"))
	calReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	calReq.Header.Set("HX-Request", "true")
	calResp, err := http.DefaultClient.Do(calReq)
	if err != nil || calResp.StatusCode != http.StatusOK {
		t.Fatalf("POST /ui/calibrate-confidence failed: %v (%v)", calResp, err)
	}
	calBytes, _ := io.ReadAll(calResp.Body)
	_ = calResp.Body.Close()
	calHTML := string(calBytes)
	for _, want := range []string{`id="calHistScope"`, `<option value="project:conf" selected>`, `id="confHist"`, "conf 0.97", "conf 0.92", "conf 0.82"} {
		if !strings.Contains(calHTML, want) {
			t.Fatalf("expected recalibrated SSR response to contain %q, got:\n%s", want, calHTML)
		}
	}

	stats, err := svc.GetDashboardStats(ctx)
	if err != nil {
		t.Fatalf("GetDashboardStats: %v", err)
	}
	// Bins: [<0.80: 0, 0.80-0.84: 1 (#3=0.82), 0.85-0.89: 0, 0.90-0.94: 1 (#2=0.92), 0.95+: 1 (#1=0.97)]
	if stats.ConfidenceBins[1] != 1 || stats.ConfidenceBins[3] != 1 || stats.ConfidenceBins[4] != 1 {
		t.Fatalf("expected individualized distribution [0 1 0 1 1], got %v", stats.ConfidenceBins)
	}

	// 3. Relevance Probe displays per-item Score AND Conf, dropping #3 because conf 0.68 < RelConf 0.75 despite score 4.1 >= 3.5
	probeForm := url.Values{"scope": {"project:conf"}, "query": {"API 타임아웃 확인"}}
	pReq, _ := http.NewRequest(http.MethodPost, ts.URL+"/ui/probe", strings.NewReader(probeForm.Encode()))
	pReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	pReq.Header.Set("HX-Request", "true")
	pResp, err := http.DefaultClient.Do(pReq)
	if err != nil || pResp.StatusCode != http.StatusOK {
		t.Fatalf("POST /ui/probe failed: %v (%v)", pResp, err)
	}
	pBytes, _ := io.ReadAll(pResp.Body)
	_ = pResp.Body.Close()
	pHTML := string(pBytes)
	if !strings.Contains(pHTML, "PASS · 4.4 · conf 0.93") || !strings.Contains(pHTML, "DROP · 4.1 · conf 0.68") {
		t.Fatalf("expected probe results to show per-item PASS/DROP with Score and Conf, got:\n%s", pHTML)
	}
}

func TestTelemetryDashboardGuardrailAndReset(t *testing.T) {
	ctx := context.Background()
	deps := newTestDeps(t, ":memory:", noopEval{}, "", thttp.DashboardHooks{})

	if _, err := deps.Memory.CommitMemory(ctx, "Always run gofmt on Go packages", "architecture", "project:telui"); err != nil {
		t.Fatalf("CommitMemory: %v", err)
	}
	if _, err := deps.Guardrail.VerifyPlan(ctx, "Run gofmt across cmd/ and internal/", "project:telui"); err != nil {
		t.Fatalf("VerifyPlan: %v", err)
	}

	h := dashboard.NewHandler(deps)
	ts := httptest.NewServer(h)
	defer ts.Close()

	// 1. Guardrails partial shows recorded ElapsedMs badge on log entry
	grResp, err := http.Get(ts.URL + "/ui/guardrail-logs?gr_scope=project:telui")
	if err != nil {
		t.Fatal(err)
	}
	grBodyBytes, _ := io.ReadAll(grResp.Body)
	_ = grResp.Body.Close()
	grBody := string(grBodyBytes)
	if grResp.StatusCode != http.StatusOK || !strings.Contains(grBody, "ms</span>") && !strings.Contains(grBody, "s</span>") {
		t.Fatalf("expected elapsed latency badge in /ui/guardrail-logs (%d):\n%s", grResp.StatusCode, grBody)
	}

	// 2. Engines partial displays telemetry visualization charts, per-task avg/p95/max stats, and Reset button
	engResp, err := http.Get(ts.URL + "/ui/engines?view=gemini")
	if err != nil {
		t.Fatal(err)
	}
	engBytes, _ := io.ReadAll(engResp.Body)
	_ = engResp.Body.Close()
	engHTML := string(engBytes)
	if strings.Count(engHTML, `id="telemetrySummaryBadge"`) != 1 {
		t.Fatalf("expected exactly 1 #telemetrySummaryBadge, got %d", strings.Count(engHTML, `id="telemetrySummaryBadge"`))
	}
	telIdx := strings.Index(engHTML, `id="evalTelemetryPanel"`)
	pillsIdx := strings.Index(engHTML, `id="enginePills"`)
	if telIdx < 0 || pillsIdx < 0 || telIdx > pillsIdx {
		t.Fatalf("expected #evalTelemetryPanel to appear above #enginePills (tel=%d, pills=%d)", telIdx, pillsIdx)
	}
	for _, want := range []string{
		`id="evalTelemetryPanel"`,
		`class="cat-breakdown"`,
		`:open="telemetryOpen"`,
		`@click.prevent="setTelemetryOpen(!telemetryOpen)"`,
		`id="telemetrySummaryBadge"`,
		`id="btnTelemetryReload"`,
		`id="btnResetTelemetry"`,
		`id="telemetryTaskChart"`,
		`id="telemetryEngineChart"`,
		`id="telemetrySparkChart"`,
		"24h:",
		"avg ",
		"p95 ",
		"max ",
		"Σ ",
		"noop · ",
	} {
		if !strings.Contains(engHTML, want) {
			t.Fatalf("expected %q in /ui/engines HTML:\n%s", want, engHTML)
		}
	}

	uiJSResp, err := http.Get(ts.URL + "/assets/ui.js")
	if err != nil {
		t.Fatal(err)
	}
	uiJSBytes, _ := io.ReadAll(uiJSResp.Body)
	_ = uiJSResp.Body.Close()
	for _, want := range []string{"telemetryOpen:", "setTelemetryOpen(", "memorize_telemetry_open"} {
		if !strings.Contains(string(uiJSBytes), want) {
			t.Fatalf("expected %q in /assets/ui.js", want)
		}
	}

	// 2b. Time window switcher updates TelemetryViz window cleanly (7d and all)
	for _, tc := range []struct {
		tw   string
		want string
	}{
		{tw: "7d", want: "7d:"},
		{tw: "all", want: "All:"},
	} {
		wResp, err := http.Get(ts.URL + "/ui/engines?view=gemini&tw=" + tc.tw)
		if err != nil {
			t.Fatal(err)
		}
		wBytes, _ := io.ReadAll(wResp.Body)
		_ = wResp.Body.Close()
		if wResp.StatusCode != http.StatusOK || !strings.Contains(string(wBytes), tc.want) {
			t.Fatalf("expected %q for tw=%s (status=%d)", tc.want, tc.tw, wResp.StatusCode)
		}
	}

	// 3. Reset telemetry without HX-Request must be blocked (403)
	badReq, _ := http.NewRequest(http.MethodPost, ts.URL+"/ui/engines/telemetry/reset", nil)
	badResp, err := http.DefaultClient.Do(badReq)
	if err != nil {
		t.Fatal(err)
	}
	_ = badResp.Body.Close()
	if badResp.StatusCode != http.StatusForbidden {
		t.Fatalf("expected 403 without HX-Request on /ui/engines/telemetry/reset, got %d", badResp.StatusCode)
	}

	// 4. Reset telemetry with HX-Request=true clears counters to 0 calls and removes Reset button
	resetReq, _ := http.NewRequest(http.MethodPost, ts.URL+"/ui/engines/telemetry/reset", strings.NewReader("view=gemini&tw=7d"))
	resetReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resetReq.Header.Set("HX-Request", "true")
	resetResp, err := http.DefaultClient.Do(resetReq)
	if err != nil {
		t.Fatal(err)
	}
	resetBytes, _ := io.ReadAll(resetResp.Body)
	_ = resetResp.Body.Close()
	resetHTML := string(resetBytes)
	if resetResp.StatusCode != http.StatusOK || !strings.Contains(resetHTML, "7d: 0 calls") || strings.Contains(resetHTML, `id="btnResetTelemetry"`) {
		t.Fatalf("expected reset HTML to show 7d: 0 calls and omit reset button (status=%d):\n%s", resetResp.StatusCode, resetHTML)
	}
}
