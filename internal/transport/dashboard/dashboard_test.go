package dashboard_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"memorize/internal/domain"
	"memorize/internal/inference/localmodel"
	"memorize/internal/modules/guardrail"
	"memorize/internal/modules/memory"
	"memorize/internal/modules/progress"
	"memorize/internal/transport/dashboard"
	thttp "memorize/internal/transport/http"
)

func newTestDeps(t *testing.T, dbPath string, eval domain.Evaluator, modelsDir string, hooks thttp.DashboardHooks) dashboard.Deps {
	t.Helper()
	if dbPath == "" {
		dbPath = ":memory:"
	}
	if modelsDir == "" {
		modelsDir = t.TempDir()
	}
	mem, err := memory.Open(dbPath, eval)
	if err != nil {
		t.Fatalf("memory.Open: %v", err)
	}
	t.Cleanup(func() { _ = mem.Close() })
	prog := progress.New(mem)
	gr := guardrail.New(mem, prog)
	mgr := localmodel.NewManager(modelsDir)
	mem.Router().AttachLocalManager(mgr)
	return dashboard.Deps{
		Memory:     mem,
		Progress:   prog,
		Guardrail:  gr,
		LocalModel: mgr,
		Hooks:      hooks,
	}
}

type noopEval struct{}

func (noopEval) Name() string { return "noop" }
func (noopEval) ScoreRelevance(_ context.Context, _ string, facts []domain.Fact) ([]domain.ScoredFact, error) {
	out := make([]domain.ScoredFact, len(facts))
	for i, f := range facts {
		out[i] = domain.ScoredFact{Fact: f, Score: 4.5, Confidence: 0.9}
	}
	return out, nil
}
func (noopEval) EvaluateMutation(_ context.Context, _ string, _ string, _ []domain.Fact) (domain.MutationDecision, error) {
	return domain.MutationDecision{
		Category:          "architecture",
		Tier:              "invariant",
		IsPermanent:       0.95,
		IsGlobalInvariant: 0.92,
		Action:            "INSERT",
		Confidence:        0.95,
	}, nil
}
func (noopEval) VerifyPlan(_ context.Context, _ string, _ []domain.Fact) ([]domain.PlanViolation, error) {
	return nil, nil
}
func (noopEval) SynthesizeConsolidation(_ context.Context, _ string, _ string, facts []domain.Fact) (string, error) {
	if len(facts) > 0 {
		return facts[0].Content, nil
	}
	return "", nil
}

func doHX(t *testing.T, method, targetURL string, form url.Values) (int, string) {
	t.Helper()
	var body io.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	}
	req, err := http.NewRequest(method, targetURL, body)
	if err != nil {
		t.Fatalf("NewRequest %s %s: %v", method, targetURL, err)
	}
	if method == http.MethodPost {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	req.Header.Set("HX-Request", "true")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, targetURL, err)
	}
	b, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	return resp.StatusCode, string(b)
}

func TestModularStaticAssetsDNSRebindingAndPinPreservation(t *testing.T) {
	ctx := context.Background()
	deps := newTestDeps(t, ":memory:", noopEval{}, "", thttp.DashboardHooks{})
	svc := deps.Memory

	_, err := svc.CommitMemory(ctx, "핵심 고정 불변 규칙", "architecture", "project:ui")
	if err != nil {
		t.Fatalf("CommitMemory: %v", err)
	}

	h := dashboard.NewHandler(deps)
	ts := httptest.NewServer(h)
	defer ts.Close()

	// 1. Modular embedded SSR v3 assets serve cleanly with proper Content-Type
	for _, tc := range []struct {
		path  string
		ctype string
		want  string
	}{
		{"/", "text/html", `/assets/styles.css`},
		{"/assets/styles.css", "text/css", "--bg:"},
		{"/assets/ui.js", "application/javascript", "dashboardApp"},
		{"/assets/vendor/htmx.min.js", "application/javascript", "htmx"},
		{"/assets/vendor/alpine.min.js", "application/javascript", "MutationObserver"},
	} {
		resp, err := http.Get(ts.URL + tc.path)
		if err != nil || resp.StatusCode != http.StatusOK {
			t.Fatalf("GET %s failed: status=%v err=%v", tc.path, resp, err)
		}
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if !strings.Contains(resp.Header.Get("Content-Type"), tc.ctype) {
			t.Fatalf("GET %s unexpected Content-Type %q (want %q)", tc.path, resp.Header.Get("Content-Type"), tc.ctype)
		}
		if !strings.Contains(string(body), tc.want) {
			t.Fatalf("GET %s missing expected content %q", tc.path, tc.want)
		}
	}

	// 2. DNS Rebinding & cross-site Sec-Fetch-Site defense
	rebindReq := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:18095/api/records", nil)
	rebindReq.Host = "evil-rebind.attacker.example:18095"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, rebindReq)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected DNS rebinding Host header to be blocked with 403, got %d", rec.Code)
	}

	crossSiteReq := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:18095/api/records", nil)
	crossSiteReq.Header.Set("Sec-Fetch-Site", "cross-site")
	crossRec := httptest.NewRecorder()
	h.ServeHTTP(crossRec, crossSiteReq)
	if crossRec.Code != http.StatusForbidden {
		t.Fatalf("expected Sec-Fetch-Site: cross-site to be blocked with 403, got %d", crossRec.Code)
	}

	// 3. Partial UpdateMemoryFull (pinned == nil) AND SSR /ui/memories/update preserve Pinned=true state
	if err := svc.UpdateMemoryFull(ctx, 1, "", "", nil, "수정된 핵심 고정 불변 규칙 (svc)", nil); err != nil {
		t.Fatalf("UpdateMemoryFull partial failed: %v", err)
	}
	recs, _ := svc.ListRecords(ctx, "project:ui")
	if len(recs) != 1 || !recs[0].Pinned || recs[0].Content != "수정된 핵심 고정 불변 규칙 (svc)" {
		t.Fatalf("expected nil pinned pointer to preserve Pinned=true, got %+v", recs)
	}
	if code, body := doHX(t, http.MethodPost, ts.URL+"/ui/memories/update", url.Values{
		"id":       {"1"},
		"scope":    {"project:ui"},
		"category": {"architecture"},
		"content":  {"수정된 핵심 고정 불변 규칙"},
		"pinned":   {"1"},
	}); code != http.StatusOK {
		t.Fatalf("POST /ui/memories/update failed (%d): %s", code, body)
	}
	recs, _ = svc.ListRecords(ctx, "project:ui")
	if len(recs) != 1 || !recs[0].Pinned || recs[0].Content != "수정된 핵심 고정 불변 규칙" {
		t.Fatalf("expected /ui/memories/update to preserve Pinned=true, got %+v", recs)
	}

	// 4. Empty candidate on CommitMemory / POST /ui/memories/commit is rejected before touching existing records
	if _, err := svc.CommitMemory(ctx, "   ", "architecture", "project:ui"); err == nil {
		t.Fatalf("expected whitespace-only CommitMemory to fail")
	}
	if emptyCode, _ := doHX(t, http.MethodPost, ts.URL+"/ui/memories/commit", url.Values{
		"new_scope": {"project:ui"},
		"candidate": {"  "},
	}); emptyCode != http.StatusBadRequest {
		t.Fatalf("expected POST /ui/memories/commit with empty candidate to return 400, got %d", emptyCode)
	}

	// 5. System 1 (OmniJev/Flide) discovery & jev_model persistence via SSR /ui/engines/*
	flideSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/health":
			_, _ = w.Write([]byte(`{"status":"ok","model":"OmniJev/OneJev1.6-0.8B","format":"bf16","engine":"flide","version":"0.2.0"}`))
		case "/v1/models":
			_, _ = w.Write([]byte(`{"models":[{"name":"OmniJev/OneJev1.6-0.8B","description":"0.8B System 1 decider"}]}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer flideSrv.Close()

	discCode, discHTML := doHX(t, http.MethodPost, ts.URL+"/ui/engines/discover-jev", url.Values{
		"jev_api_url": {flideSrv.URL},
	})
	if discCode != http.StatusOK || !strings.Contains(discHTML, "OmniJev/OneJev1.6-0.8B") {
		t.Fatalf("expected POST /ui/engines/discover-jev to discover OmniJev/OneJev1.6-0.8B (status=%d):\n%s", discCode, discHTML)
	}

	provCode, provHTML := doHX(t, http.MethodPost, ts.URL+"/ui/engines/save", url.Values{
		"provider":    {"jev"},
		"jev_api_url": {flideSrv.URL},
		"jev_model":   {"OmniJev/OneJev1.6-0.8B"},
	})
	if provCode != http.StatusOK || !strings.Contains(provHTML, "OmniJev/OneJev1.6-0.8B") {
		t.Fatalf("POST /ui/engines/save for Flide failed (%d):\n%s", provCode, provHTML)
	}
	if cfg := svc.GetConfigMap(ctx); cfg["jev_model"] != "OmniJev/OneJev1.6-0.8B" {
		t.Fatalf("expected saved jev_model=OmniJev/OneJev1.6-0.8B, got %q", cfg["jev_model"])
	}
}

func TestServerSideRecordsPaginationStatsFacetsAndLookup(t *testing.T) {
	deps := newTestDeps(t, ":memory:", noopEval{}, "", thttp.DashboardHooks{})
	svc := deps.Memory

	h := dashboard.NewHandler(deps)
	ts := httptest.NewServer(h)
	defer ts.Close()

	commitOne := func(scope, account, cat, subCat, text string) {
		t.Helper()
		code, body := doHX(t, http.MethodPost, ts.URL+"/ui/memories/commit", url.Values{
			"new_scope":        {scope},
			"new_account":      {account},
			"new_category":     {cat},
			"new_sub_category": {subCat},
			"candidate":        {text},
		})
		if code != http.StatusOK {
			t.Fatalf("POST /ui/memories/commit (%s) failed (%d): %s", text, code, body)
		}
	}

	// Insert 25 active records across scopes/accounts/categories, including underscore edge cases.
	for i := 1; i <= 23; i++ {
		commitOne("project:scale", "", "architecture", "storage", fmt.Sprintf("대용량 페이지네이션 검증 항목 %02d", i))
	}
	commitOne("personal", "alice", "preference", "naming", "정확한 언더스코어 토큰 foo_bar 매칭 규칙")
	commitOne("personal", "bob", "preference", "naming", "언더스코어 오탐 방지용 fooXbar 규칙")

	// 1. Unparameterized GET /api/records must cap records at default page_size=20 (never dumping all 25 rows),
	//    while global stats, facets, and SSR / reflect all 25 records cleanly.
	recResp, err := http.Get(ts.URL + "/api/records")
	if err != nil || recResp.StatusCode != http.StatusOK {
		t.Fatalf("GET /api/records failed: %v", err)
	}
	var stPage domain.RecordPage
	if err := json.NewDecoder(recResp.Body).Decode(&stPage); err != nil {
		t.Fatalf("decode /api/records: %v", err)
	}
	_ = recResp.Body.Close()

	if len(stPage.Records) != 20 || stPage.PageSize != 20 || stPage.TotalRecords != 25 || stPage.TotalPages != 2 {
		t.Fatalf("expected capped page_size=20 out of 25 (2 pages), got len=%d page_size=%d total=%d pages=%d",
			len(stPage.Records), stPage.PageSize, stPage.TotalRecords, stPage.TotalPages)
	}
	ctx := context.Background()
	stats, err := svc.GetDashboardStatsForScope(ctx, "")
	if err != nil || stats.ActiveCount != 25 || stats.TotalCount != 25 || stats.ConfidenceBins[4] != 25 {
		t.Fatalf("unexpected global stats (%v): %+v", err, stats)
	}
	facets, err := svc.GetDashboardFacets(ctx)
	if err != nil || len(facets.Accounts) < 2 || len(facets.Categories) < 2 || len(facets.SubCategories["preference"]) == 0 {
		t.Fatalf("expected distinct facets (%v), got accounts=%v cats=%v subs=%v",
			err, facets.Accounts, facets.Categories, facets.SubCategories)
	}
	rootResp, err := http.Get(ts.URL + "/")
	if err != nil || rootResp.StatusCode != http.StatusOK {
		t.Fatalf("GET / failed: %v", err)
	}
	rootBytes, _ := io.ReadAll(rootResp.Body)
	_ = rootResp.Body.Close()
	if !strings.Contains(string(rootBytes), "alice") || !strings.Contains(string(rootBytes), "bob") {
		t.Fatalf("expected SSR / to render account facets alice & bob")
	}

	// 2. GET /api/records?q=foo_bar escapes SQLite LIKE '_' wildcard so fooXbar is NOT matched.
	qResp, err := http.Get(ts.URL + "/api/records?q=foo_bar&state=active&page=1&page_size=10")
	if err != nil || qResp.StatusCode != http.StatusOK {
		t.Fatalf("GET /api/records?q=foo_bar failed: %v", err)
	}
	var qPage domain.RecordPage
	if err := json.NewDecoder(qResp.Body).Decode(&qPage); err != nil {
		t.Fatalf("decode /api/records: %v", err)
	}
	_ = qResp.Body.Close()
	if qPage.FilteredRecords != 1 || len(qPage.Records) != 1 || !strings.Contains(qPage.Records[0].Content, "foo_bar") {
		t.Fatalf("expected exact 1 match for literal underscore foo_bar, got %+v", qPage)
	}

	// 3. GET /api/records?scope=project:scale&ids_only=1 returns all 23 active IDs across pages without record bodies.
	idsResp, err := http.Get(ts.URL + "/api/records?scope=project:scale&state=active&ids_only=1")
	if err != nil || idsResp.StatusCode != http.StatusOK {
		t.Fatalf("GET /api/records?ids_only=1 failed: %v", err)
	}
	var idsPayload struct {
		IDs []int64 `json:"ids"`
	}
	if err := json.NewDecoder(idsResp.Body).Decode(&idsPayload); err != nil {
		t.Fatalf("decode ids_only: %v", err)
	}
	_ = idsResp.Body.Close()
	if len(idsPayload.IDs) != 23 {
		t.Fatalf("expected 23 matching active IDs for project:scale, got %d", len(idsPayload.IDs))
	}

	// 4. SSR /ui/modal/detail + GetRecordsByIDs fetch targeted records across pages.
	detCode, detHTML := doHX(t, http.MethodGet, ts.URL+"/ui/modal/detail?id=24", nil)
	if detCode != http.StatusOK || !strings.Contains(detHTML, "foo_bar") {
		t.Fatalf("expected GET /ui/modal/detail?id=24 to return foo_bar record (status=%d)", detCode)
	}
	lookedUp, err := svc.GetRecordsByIDs(ctx, []int64{1, 24, 25})
	if err != nil || len(lookedUp) != 3 {
		t.Fatalf("expected 3 looked-up records (%v), got %d", err, len(lookedUp))
	}
}

func TestSessionFilterConsolidationCapAndLanguageDropdown(t *testing.T) {
	ctx := context.Background()
	deps := newTestDeps(t, ":memory:", noopEval{}, "", thttp.DashboardHooks{})
	svc := deps.Memory

	if _, err := svc.CommitMemory(ctx, "세션 A에 로드될 첫 번째 아키텍처 규칙", "architecture", "project:alpha"); err != nil {
		t.Fatalf("CommitMemory 1: %v", err)
	}
	if _, err := svc.CommitMemory(ctx, "세션 A에 로드될 두 번째 아키텍처 규칙", "architecture", "project:alpha"); err != nil {
		t.Fatalf("CommitMemory 2: %v", err)
	}
	if _, err := svc.CommitMemory(ctx, "세션 B에만 로드될 별도 프로젝트 규칙", "architecture", "project:beta"); err != nil {
		t.Fatalf("CommitMemory 3: %v", err)
	}

	if _, err := svc.QuerySessionMemory(ctx, "sess-alpha-123456", "아키텍처 작업", "", "project:alpha", false); err != nil {
		t.Fatalf("QuerySessionMemory alpha: %v", err)
	}
	if _, err := svc.QuerySessionMemory(ctx, "sess-beta-654321", "별도 작업", "", "project:beta", false); err != nil {
		t.Fatalf("QuerySessionMemory beta: %v", err)
	}

	h := dashboard.NewHandler(deps)
	ts := httptest.NewServer(h)
	defer ts.Close()

	// 1. UI HTML includes English-default <select id="langSelect"> and <select id="filterSession">,
	//    and all embedded static assets stay strictly below 800 lines.
	uiResp, err := http.Get(ts.URL + "/")
	if err != nil || uiResp.StatusCode != http.StatusOK {
		t.Fatalf("GET / failed: %v", err)
	}
	uiBody, _ := io.ReadAll(uiResp.Body)
	_ = uiResp.Body.Close()
	htmlStr := string(uiBody)
	for _, needle := range []string{`id="langSelect"`, `<option value="en" selected>English</option>`, `<option value="ko" >한국어</option>`, `id="filterSession"`, `id="daemonBadge"`} {
		if !strings.Contains(htmlStr, needle) {
			t.Fatalf("expected UI HTML to contain %q", needle)
		}
	}
	for _, forbidden := range []string{`id="btnRestartDaemon"`, `id="btnRefresh"`, `id="btnRefreshProgress"`, `id="btnRefreshCalibrate"`} {
		if strings.Contains(htmlStr, forbidden) {
			t.Fatalf("expected UI header HTML to omit manual button %q", forbidden)
		}
	}
	for _, assetPath := range []string{"/assets/styles.css", "/assets/ui.js"} {
		ar, err := http.Get(ts.URL + assetPath)
		if err != nil || ar.StatusCode != http.StatusOK {
			t.Fatalf("GET %s failed: %v", assetPath, err)
		}
		ab, _ := io.ReadAll(ar.Body)
		_ = ar.Body.Close()
		if lines := strings.Count(string(ab), "\n") + 1; lines >= 800 {
			t.Fatalf("static asset %s has %d lines (must stay < 800)", assetPath, lines)
		}
	}
	tplFiles, err := filepath.Glob("templates/*.html")
	if err != nil || len(tplFiles) == 0 {
		t.Fatalf("expected templates/*.html files, got %v (%v)", tplFiles, err)
	}
	for _, tf := range tplFiles {
		raw, err := os.ReadFile(tf)
		if err != nil {
			t.Fatalf("read template %s: %v", tf, err)
		}
		if lines := strings.Count(string(raw), "\n") + 1; lines >= 800 {
			t.Fatalf("template %s has %d lines (must stay < 800)", tf, lines)
		}
	}

	// 2. /ui/memories exposes active sessions facet and /api/records?session_id=... filters loaded memories.
	memCode, memHTML := doHX(t, http.MethodGet, ts.URL+"/ui/memories", nil)
	if memCode != http.StatusOK || !strings.Contains(memHTML, "sess-alpha-123456") || !strings.Contains(memHTML, "sess-beta-654321") {
		t.Fatalf("expected 2 active sessions in /ui/memories (status=%d)", memCode)
	}

	sessResp, err := http.Get(ts.URL + "/api/records?session_id=sess-alpha-123456&state=active")
	if err != nil || sessResp.StatusCode != http.StatusOK {
		t.Fatalf("GET /api/records?session_id=sess-alpha-123456 failed: %v", err)
	}
	var sessPage domain.RecordPage
	if err := json.NewDecoder(sessResp.Body).Decode(&sessPage); err != nil {
		t.Fatalf("decode session records: %v", err)
	}
	_ = sessResp.Body.Close()
	if sessPage.FilteredRecords != 2 || len(sessPage.Records) != 2 {
		t.Fatalf("expected 2 records loaded in sess-alpha-123456, got %+v", sessPage)
	}
	for _, rec := range sessPage.Records {
		if rec.Scope != "project:alpha" {
			t.Fatalf("unexpected scope %q in sess-alpha-123456 records", rec.Scope)
		}
	}

	tokResp, err := http.Get(ts.URL + "/api/records?q=session:sess-beta&state=active")
	if err != nil || tokResp.StatusCode != http.StatusOK {
		t.Fatalf("GET /api/records?q=session:sess-beta failed: %v", err)
	}
	var tokPage domain.RecordPage
	if err := json.NewDecoder(tokResp.Body).Decode(&tokPage); err != nil {
		t.Fatalf("decode session token search: %v", err)
	}
	_ = tokResp.Body.Close()
	if tokPage.FilteredRecords != 1 || tokPage.Records[0].Scope != "project:beta" {
		t.Fatalf("expected 1 project:beta record for q=session:sess-beta, got %+v", tokPage)
	}

	// 3. Consolidation enforces MaxConsolidatedRunes (180 chars) and accepts concise merges.
	tooLong := strings.Repeat("가", domain.MaxConsolidatedRunes+1)
	badCode, badBody := doHX(t, http.MethodPost, ts.URL+"/ui/consolidate/execute", url.Values{
		"merge_scope":    {"project:alpha"},
		"merge_category": {"architecture"},
		"ids":            {"1", "2"},
		"merge_pinned":   {"1"},
		"content":        {tooLong},
	})
	if badCode != http.StatusBadRequest {
		t.Fatalf("expected >%d-char consolidation to fail with 400, got %d (%s)", domain.MaxConsolidatedRunes, badCode, badBody)
	}

	okCode, okBody := doHX(t, http.MethodPost, ts.URL+"/ui/consolidate/execute", url.Values{
		"merge_scope":    {"project:alpha"},
		"merge_category": {"architecture"},
		"ids":            {"1", "2"},
		"merge_pinned":   {"1"},
		"content":        {"세션 A의 첫 번째·두 번째 아키텍처 규칙을 하나로 통합한 간결한 원칙"},
	})
	if okCode != http.StatusOK {
		t.Fatalf("expected concise consolidation to succeed, got %d (%s)", okCode, okBody)
	}
}

func TestSSRv3PartialsNewestSortQuickTabsAndEngineActiveIsolation(t *testing.T) {
	ctx := context.Background()
	deps := newTestDeps(t, ":memory:", noopEval{}, "", thttp.DashboardHooks{})
	svc := deps.Memory

	// #1: Older pinned rule
	if _, err := svc.CommitMemory(ctx, "오래된 핵심 고정 규칙 #1", "architecture", "project:ssr"); err != nil {
		t.Fatalf("CommitMemory #1: %v", err)
	}
	// #2: Newer general (unpinned) rule
	if _, err := svc.CommitMemory(ctx, "방금 추가한 일반 최신 규칙 #2", "architecture", "project:ssr"); err != nil {
		t.Fatalf("CommitMemory #2: %v", err)
	}
	pinnedFalse := false
	sub := ""
	if err := svc.UpdateMemoryFull(domain.WithAdminBypass(ctx), 2, "project:ssr", "architecture", &sub, "방금 추가한 일반 최신 규칙 #2", &pinnedFalse); err != nil {
		t.Fatalf("UpdateMemoryFull unpin #2: %v", err)
	}

	h := dashboard.NewHandler(deps)
	ts := httptest.NewServer(h)
	defer ts.Close()

	getBody := func(path string) string {
		t.Helper()
		resp, err := http.Get(ts.URL + path)
		if err != nil || resp.StatusCode != http.StatusOK {
			t.Fatalf("GET %s failed: status=%v err=%v", path, resp, err)
		}
		b, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		return string(b)
	}

	// 1. Newest-first sort (id DESC) in All Active + Quick State Tabs (Pinned / General) (#57)
	activeHTML := getBody("/ui/memories?state=active")
	idxNew := strings.Index(activeHTML, "방금 추가한 일반 최신 규칙 #2")
	idxOld := strings.Index(activeHTML, "오래된 핵심 고정 규칙 #1")
	if idxNew < 0 || idxOld < 0 || idxNew >= idxOld {
		t.Fatalf("expected newest general memory #2 to render BEFORE older pinned memory #1 (idxNew=%d, idxOld=%d)", idxNew, idxOld)
	}

	pinnedHTML := getBody("/ui/memories?state=pinned")
	if !strings.Contains(pinnedHTML, "오래된 핵심 고정 규칙 #1") || strings.Contains(pinnedHTML, "방금 추가한 일반 최신 규칙 #2") {
		t.Fatalf("expected state=pinned partial to contain ONLY #1")
	}

	generalHTML := getBody("/ui/memories?state=general")
	if !strings.Contains(generalHTML, "방금 추가한 일반 최신 규칙 #2") || strings.Contains(generalHTML, "오래된 핵심 고정 규칙 #1") {
		t.Fatalf("expected state=general partial to contain ONLY unpinned #2")
	}

	// 2. HTMX CSRF protection (#26, #59): POST /ui/* without HX-Request: true must fail with 403
	noHXReq, _ := http.NewRequest(http.MethodPost, ts.URL+"/ui/engines/save", strings.NewReader("provider=gemini"))
	noHXReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	noHXResp, err := http.DefaultClient.Do(noHXReq)
	if err != nil || noHXResp.StatusCode != http.StatusForbidden {
		t.Fatalf("expected POST /ui/engines/save without HX-Request header to return 403, got %v (%v)", noHXResp, err)
	}
	_ = noHXResp.Body.Close()

	// 3. Engine Active vs Preview Isolation (#58):
	//    Save Gemini as active engine, then preview Jev tab and verify Jev NEVER renders as ACTIVE.
	saveForm := url.Values{
		"provider":     {"gemini"},
		"gemini_model": {"gemini-3.5-flash-lite"},
	}
	hxReq, _ := http.NewRequest(http.MethodPost, ts.URL+"/ui/engines/save", strings.NewReader(saveForm.Encode()))
	hxReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	hxReq.Header.Set("HX-Request", "true")
	hxResp, err := http.DefaultClient.Do(hxReq)
	if err != nil || hxResp.StatusCode != http.StatusOK {
		t.Fatalf("POST /ui/engines/save with HX-Request failed: %v (%v)", hxResp, err)
	}
	saveHTMLBytes, _ := io.ReadAll(hxResp.Body)
	_ = hxResp.Body.Close()
	saveHTML := string(saveHTMLBytes)
	if !strings.Contains(hxResp.Header.Get("HX-Trigger"), "memorizeToast") || !strings.Contains(saveHTML, `id="engineActiveBadge"`) || strings.Contains(saveHTML, `id="providerBadge"`) {
		t.Fatalf("expected HX-Trigger toast and in-tab engineActiveBadge (without topbar providerBadge OOB) on engine save")
	}
	if strings.Contains(getBody("/"), `id="providerBadge"`) {
		t.Fatalf("expected topbar header NOT to render #providerBadge")
	}

	jevPreviewHTML := getBody("/ui/engines?view=jev")
	if !strings.Contains(jevPreviewHTML, "ACTIVE: GEMINI · PREVIEW: JEV (Unsaved)") {
		t.Fatalf("expected preview mode badge 'ACTIVE: GEMINI · PREVIEW: JEV (Unsaved)', got:\n%s", jevPreviewHTML)
	}
	catIdx := strings.Index(jevPreviewHTML, `id="jevCatalog"`)
	if catIdx < 0 {
		t.Fatalf("expected #jevCatalog in preview HTML")
	}
	jevCatalogHTML := jevPreviewHTML[catIdx:]
	if routeIdx := strings.Index(jevCatalogHTML, `id="taskRoutingSection"`); routeIdx >= 0 {
		jevCatalogHTML = jevCatalogHTML[:routeIdx]
	}
	if strings.Contains(jevCatalogHTML, `class="badge b-ok"`) {
		t.Fatalf("expected NO green ACTIVE badge inside #jevCatalog when effective_provider is gemini:\n%s", jevCatalogHTML)
	}
	if !strings.Contains(jevCatalogHTML, `Selected`) {
		t.Fatalf("expected default jev-latest row to render muted 'Selected' badge, not 'ACTIVE'")
	}

	// Navigating back to top-level Engines tab (/ui/engines without ?view=) resets to active provider (gemini)
	resetEnginesHTML := getBody("/ui/engines")
	if !strings.Contains(resetEnginesHTML, `id="pane-gemini"`) || !strings.Contains(resetEnginesHTML, `>ACTIVE: GEMINI<`) {
		t.Fatalf("expected /ui/engines without view param to reset to active pane-gemini")
	}

	// 4. SSR Detail Modal & Language Cookie switch (#59)
	detailHTML := getBody("/ui/modal/detail?id=1")
	if !strings.Contains(detailHTML, `hx-post="/ui/memories/update"`) || !strings.Contains(detailHTML, "오래된 핵심 고정 규칙 #1") {
		t.Fatalf("expected SSR detail modal for #1, got:\n%s", detailHTML)
	}

	langReq, _ := http.NewRequest(http.MethodPost, ts.URL+"/ui/lang", strings.NewReader("lang=ko"))
	langReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	langReq.Header.Set("HX-Request", "true")
	langResp, err := http.DefaultClient.Do(langReq)
	if err != nil || langResp.StatusCode != http.StatusOK || langResp.Header.Get("HX-Refresh") != "true" {
		t.Fatalf("POST /ui/lang failed: %v (%v)", langResp, err)
	}
	_ = langResp.Body.Close()

	// 5. Horizontal 5-column confidence histogram (#60)
	rootHTML := getBody("/")
	cssText := getBody("/assets/styles.css")
	if !strings.Contains(rootHTML, `<div class="hist-bars" id="confHist"`) || !strings.Contains(cssText, `grid-template-columns: repeat(5`) {
		t.Fatalf("expected horizontal 5-column .hist-bars for #confHist")
	}
}

func TestDashboardSecurityHardeningAndDevHotReloadGate(t *testing.T) {
	deps := newTestDeps(t, ":memory:", noopEval{}, "", thttp.DashboardHooks{
		DaemonStatusFn: func() thttp.DaemonPortState {
			return thttp.DaemonPortState{PID: 1001, ListenAddr: "127.0.0.1:18095", ConfiguredPort: 18095, ActivePort: 18095}
		},
		ApplyDaemonPortFn: func(_ context.Context, dp, gp int) (thttp.DaemonPortApplyResult, error) {
			return thttp.DaemonPortApplyResult{
				ListenAddr:       fmt.Sprintf("127.0.0.1:%d", dp),
				ActiveDaemonPort: dp,
				NewURL:           fmt.Sprintf("http://127.0.0.1:%d", dp),
				ActiveGGUFPort:   gp,
			}, nil
		},
	})
	svc := deps.Memory

	ctx := context.Background()
	if err := svc.SaveConfig(ctx, map[string]string{
		"typesafe_api_key": "secret-typesafe-token",
	}); err != nil {
		t.Fatal(err)
	}

	var leakedAuth string
	foreignSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if auth := r.Header.Get("Authorization"); auth != "" {
			leakedAuth = auth
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"id":"foreign/model"}]}`))
	}))
	defer foreignSrv.Close()

	h := dashboard.NewHandler(deps)

	// 1. Security headers + empty Host rejection.
	emptyHostReq := httptest.NewRequest(http.MethodGet, "/", nil)
	emptyHostReq.Host = ""
	emptyHostRec := httptest.NewRecorder()
	h.ServeHTTP(emptyHostRec, emptyHostReq)
	if emptyHostRec.Code != http.StatusForbidden {
		t.Fatalf("expected 403 Forbidden for empty Host header, got %d", emptyHostRec.Code)
	}

	ts := httptest.NewServer(h)
	defer ts.Close()

	rootResp, err := http.Get(ts.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	rootBytes, _ := io.ReadAll(rootResp.Body)
	_ = rootResp.Body.Close()
	if rootResp.Header.Get("X-Content-Type-Options") != "nosniff" ||
		rootResp.Header.Get("X-Frame-Options") != "DENY" ||
		rootResp.Header.Get("Referrer-Policy") != "no-referrer" ||
		!strings.Contains(rootResp.Header.Get("Content-Security-Policy"), "default-src 'self'") {
		t.Fatalf("missing expected security headers: %+v", rootResp.Header)
	}
	if !strings.Contains(string(rootBytes), `memorizeRefreshEngines from:body`) {
		t.Fatalf("expected layout #enginesSection to listen for memorizeRefreshEngines event")
	}

	// 2. Reflected XSS breakout prevented in /ui/memories x-data="comboBox(...)" + isolated download-box polling.
	xssScope := `global"',alert(1),"`
	memCode, memHTML := doHX(t, http.MethodGet, ts.URL+"/ui/memories?scope="+url.QueryEscape(xssScope), nil)
	if memCode != http.StatusOK {
		t.Fatalf("GET /ui/memories failed: %d", memCode)
	}
	if strings.Contains(memHTML, xssScope) || strings.Contains(memHTML, `",alert(1),`) || strings.Contains(memHTML, `',alert(1),`) {
		t.Fatalf("detected unescaped x-data XSS breakout in /ui/memories:\n%s", memHTML)
	}

	dlCode, dlHTML := doHX(t, http.MethodGet, ts.URL+"/ui/engines/download-box", nil)
	if dlCode != http.StatusOK || !strings.Contains(dlHTML, `id="engineDownloadBox"`) {
		t.Fatalf("expected GET /ui/engines/download-box to render #engineDownloadBox (code=%d):\n%s", dlCode, dlHTML)
	}

	// 3. Stored typesafe_api_key must NEVER be forwarded to a foreign custom URL during discovery.
	discCode, discHTML := doHX(t, http.MethodPost, ts.URL+"/ui/engines/discover-jev", url.Values{
		"jev_api_url": {foreignSrv.URL + "/v1"},
	})
	if discCode != http.StatusOK || !strings.Contains(discHTML, "foreign/model") {
		t.Fatalf("POST /ui/engines/discover-jev failed (%d):\n%s", discCode, discHTML)
	}
	if leakedAuth != "" {
		t.Fatalf("security failure: leaked stored Authorization header %q to foreign discovery URL", leakedAuth)
	}

	// 4. Korean HX-Trigger header uses \uXXXX ASCII escaping and HttpOnly language cookie.
	portReq, _ := http.NewRequest(http.MethodPost, ts.URL+"/ui/ports/save", strings.NewReader(url.Values{"daemon_port": {"19295"}, "gguf_port": {"19288"}}.Encode()))
	portReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	portReq.Header.Set("HX-Request", "true")
	portReq.AddCookie(&http.Cookie{Name: "memorize_lang", Value: "ko"})
	portResp, err := http.DefaultClient.Do(portReq)
	if err != nil {
		t.Fatal(err)
	}
	_ = portResp.Body.Close()
	rawTrigger := portResp.Header.Get("HX-Trigger")
	for i := 0; i < len(rawTrigger); i++ {
		if rawTrigger[i] >= 0x80 {
			t.Fatalf("expected pure ASCII \\uXXXX-escaped HX-Trigger header, got non-ASCII byte at %d: %q", i, rawTrigger)
		}
	}
	if !strings.Contains(rawTrigger, `\u`) {
		t.Fatalf("expected Korean HX-Trigger toast to contain \\uXXXX escapes, got %q", rawTrigger)
	}

	// 5. Live asset hot-reload from CWD must be disabled unless MEMORIZE_DEV_HOTRELOAD=1.
	cwdTmp := t.TempDir()
	t.Chdir(cwdTmp)
	liveThemePath := filepath.Join(cwdTmp, "internal", "transport", "dashboard", "assets", "theme.js")
	if err := os.MkdirAll(filepath.Dir(liveThemePath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(liveThemePath, []byte("// HOTRELOAD_GATE_MARKER\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MEMORIZE_DEV_HOTRELOAD", "")
	resOff, err := http.Get(ts.URL + "/assets/theme.js")
	if err != nil {
		t.Fatal(err)
	}
	bodyOff, _ := io.ReadAll(resOff.Body)
	_ = resOff.Body.Close()
	if strings.Contains(string(bodyOff), "HOTRELOAD_GATE_MARKER") {
		t.Fatalf("expected embedded theme.js when MEMORIZE_DEV_HOTRELOAD is unset, got live override")
	}
	t.Setenv("MEMORIZE_DEV_HOTRELOAD", "1")
	resOn, err := http.Get(ts.URL + "/assets/theme.js")
	if err != nil {
		t.Fatal(err)
	}
	bodyOn, _ := io.ReadAll(resOn.Body)
	_ = resOn.Body.Close()
	if !strings.Contains(string(bodyOn), "HOTRELOAD_GATE_MARKER") {
		t.Fatalf("expected live theme.js override when MEMORIZE_DEV_HOTRELOAD=1, got %q", string(bodyOn))
	}
}
