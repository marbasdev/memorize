package dashboard_test

import (
	"bufio"
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
	"time"

	"memorize/internal/domain"
	"memorize/internal/inference/discovery"
	"memorize/internal/inference/localmodel"
	"memorize/internal/inference/providers"
	"memorize/internal/inference/router"
	"memorize/internal/infra/config"
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

func TestFourTabProgressDistributionAndScopeStatsUI(t *testing.T) {
	ctx := context.Background()
	deps := newTestDeps(t, ":memory:", plannerEval{}, "", thttp.DashboardHooks{})
	svc := deps.Memory
	prog := deps.Progress
	gr := deps.Guardrail

	// Seed 6 architecture rules in project:one (including 1 unclassified sub_category) + 2 rules in project:two
	for i := 1; i <= 6; i++ {
		sub := "ui"
		if i == 6 {
			sub = ""
		}
		if _, err := svc.CommitMemoryFull(ctx, fmt.Sprintf("프로젝트 원 아키텍처 핵심 규칙 #%d", i), "architecture", sub, "", "project:one"); err != nil {
			t.Fatalf("Commit project:one %d: %v", i, err)
		}
	}
	for i := 1; i <= 2; i++ {
		if _, err := svc.CommitMemoryFull(ctx, fmt.Sprintf("프로젝트 투 별도 규칙 #%d", i), "database", "wal", "", "project:two"); err != nil {
			t.Fatalf("Commit project:two %d: %v", i, err)
		}
	}

	ts := httptest.NewServer(dashboard.NewHandler(deps))
	defer ts.Close()

	// 1. 4-tab SSR layout includes Progress tab + fixed-height 2D squarified treemap + persistent collapse state
	rootResp, err := http.Get(ts.URL + "/?lang=ko")
	if err != nil || rootResp.StatusCode != http.StatusOK {
		t.Fatalf("GET /?lang=ko failed: %v", err)
	}
	rootBytes, _ := io.ReadAll(rootResp.Body)
	_ = rootResp.Body.Close()
	rootHTML := string(rootBytes)
	for _, needle := range []string{
		`id="tabBtn-progress"`,
		`id="panel-progress"`,
		`id="tabBtn-guardrails"`,
		`id="panel-guardrails"`,
		`가드레일`,
		`id="tabBtn-calibrate"`,
		`class="cat-breakdown" :open="distOpen"`,
		`@click.prevent="setDistOpen(!distOpen)"`,
		`class="treemap-mosaic treemap-2d"`,
		`style="left:0.00%;top:0.00%;width:100.00%;height:100.00%;"`,
		`class="treemap-tile uncat`,
		`(미분류)`,
	} {
		if !strings.Contains(rootHTML, needle) {
			t.Fatalf("expected 5-tab root HTML to contain %q", needle)
		}
	}

	// 2. Scope-linked KPIs & distribution via /ui/memories?scope=project:one (HX-Request OOB swap) and /api/state?scope=project:one
	memReq, _ := http.NewRequest(http.MethodGet, ts.URL+"/ui/memories?scope=project:one&state=active", nil)
	memReq.Header.Set("HX-Request", "true")
	memResp, err := http.DefaultClient.Do(memReq)
	if err != nil || memResp.StatusCode != http.StatusOK {
		t.Fatalf("GET /ui/memories?scope=project:one failed: %v", err)
	}
	memBytes, _ := io.ReadAll(memResp.Body)
	_ = memResp.Body.Close()
	memHTML := string(memBytes)
	if !strings.Contains(memHTML, `id="kpiStrip"`) || !strings.Contains(memHTML, `id="statActive">6<`) {
		t.Fatalf("expected OOB #statActive to reflect scope=project:one count (6), got:\n%s", memHTML)
	}
	if strings.Contains(memHTML, `id="confHist"`) {
		t.Fatalf("expected /ui/memories OOB response NOT to overwrite Calibration tab #confHist")
	}

	// 2b. Calibration tab confidence histogram has its own independent scope selector (#calHistScope)
	histOneResp, err := http.Get(ts.URL + "/ui/calibrate/histogram?cal_scope=project:one&lang=ko")
	if err != nil || histOneResp.StatusCode != http.StatusOK {
		t.Fatalf("GET /ui/calibrate/histogram?cal_scope=project:one failed: %v", err)
	}
	histOneBytes, _ := io.ReadAll(histOneResp.Body)
	_ = histOneResp.Body.Close()
	if !strings.Contains(string(histOneBytes), `<option value="project:one" selected>`) || !strings.Contains(string(histOneBytes), `>6</span>`) {
		t.Fatalf("expected independent cal_scope=project:one histogram with active count 6, got:\n%s", string(histOneBytes))
	}
	histTwoResp, err := http.Get(ts.URL + "/ui/calibrate/histogram?cal_scope=project:two&lang=ko")
	if err != nil || histTwoResp.StatusCode != http.StatusOK {
		t.Fatalf("GET /ui/calibrate/histogram?cal_scope=project:two failed: %v", err)
	}
	histTwoBytes, _ := io.ReadAll(histTwoResp.Body)
	_ = histTwoResp.Body.Close()
	if !strings.Contains(string(histTwoBytes), `<option value="project:two" selected>`) || !strings.Contains(string(histTwoBytes), `>2</span>`) {
		t.Fatalf("expected independent cal_scope=project:two histogram with active count 2, got:\n%s", string(histTwoBytes))
	}

	if code, body := doHX(t, http.MethodGet, ts.URL+"/ui/memories?scope=project:two&state=active", nil); code != http.StatusOK || !strings.Contains(body, `project:two`) {
		t.Fatalf("GET /ui/memories?scope=project:two failed: status=%d", code)
	}
	statsTwo, err := svc.GetDashboardStatsForScope(ctx, "project:two")
	if err != nil {
		t.Fatalf("GetDashboardStatsForScope(project:two): %v", err)
	}
	distTwo, err := svc.GetCategoryDistribution(ctx, "project:two")
	if err != nil {
		t.Fatalf("GetCategoryDistribution(project:two): %v", err)
	}
	if statsTwo.ActiveCount != 2 || distTwo.TotalActive != 2 {
		t.Fatalf("expected scope=project:two stats/distribution active=2, got stats=%+v dist=%+v", statsTwo, distTwo)
	}

	// 3. Category-scoped Consolidate modal from distribution [Merge...] button
	catMergeResp, err := http.Get(ts.URL + "/ui/modal/consolidate?merge_scope=project:one&merge_category=architecture")
	if err != nil || catMergeResp.StatusCode != http.StatusOK {
		t.Fatalf("GET category-scoped consolidate modal failed: %v", err)
	}
	catMergeBytes, _ := io.ReadAll(catMergeResp.Body)
	_ = catMergeResp.Body.Close()
	if !strings.Contains(string(catMergeBytes), `name="merge_category" value="architecture"`) {
		t.Fatalf("expected hidden merge_category=architecture in consolidate modal")
	}

	// 3b. Unified AI Recommend (suggest -> apply-plan) shows both merge & re-categorization in Korean
	uniSugReq, _ := http.NewRequest(http.MethodPost, ts.URL+"/ui/consolidate/suggest?lang=ko", strings.NewReader("merge_scope=project:one"))
	uniSugReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	uniSugReq.Header.Set("HX-Request", "true")
	uniSugResp, err := http.DefaultClient.Do(uniSugReq)
	if err != nil || uniSugResp.StatusCode != http.StatusOK {
		t.Fatalf("POST /ui/consolidate/suggest?lang=ko failed: %v (%v)", uniSugResp, err)
	}
	uniSugBytes, _ := io.ReadAll(uniSugResp.Body)
	_ = uniSugResp.Body.Close()
	for _, needle := range []string{`AI 병합 추천 분포`, `AI 재분류 추천`, `value="tilemap"`, `id="btnConfirmAIMerge"`} {
		if !strings.Contains(string(uniSugBytes), needle) {
			t.Fatalf("expected unified Korean AI plan to contain %q, got:\n%s", needle, string(uniSugBytes))
		}
	}

	rebAppForm := url.Values{
		"merge_scope": {"project:one"},
		"item_idx":    {"0"},
		"item_0_id":   {"6"},
		"item_0_cat":  {"architecture"},
		"item_0_sub":  {"tilemap"},
	}
	rebAppReq, _ := http.NewRequest(http.MethodPost, ts.URL+"/ui/consolidate/apply-plan?lang=ko", strings.NewReader(rebAppForm.Encode()))
	rebAppReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rebAppReq.Header.Set("HX-Request", "true")
	rebAppResp, err := http.DefaultClient.Do(rebAppReq)
	if err != nil || rebAppResp.StatusCode != http.StatusOK {
		t.Fatalf("POST /ui/consolidate/apply-plan (rebalance-only) failed: %v (%v)", rebAppResp, err)
	}
	_ = rebAppResp.Body.Close()
	rebCheck, _ := svc.GetRecordsByIDs(ctx, []int64{6})
	if len(rebCheck) != 1 || rebCheck[0].SubCategory != "tilemap" || !strings.Contains(rebCheck[0].Content, "핵심 규칙 #6") {
		t.Fatalf("expected #6 sub_category=tilemap with untouched content, got %+v", rebCheck)
	}

	// 4. Progress HTMX routes: Read-Only 3-lane board + pagination + Sync button (no manual add/edit/status/delete routes)
	csrfReq, _ := http.NewRequest(http.MethodPost, ts.URL+"/ui/progress/sync", strings.NewReader("scope=project:one"))
	csrfReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	csrfResp, err := http.DefaultClient.Do(csrfReq)
	if err != nil || csrfResp.StatusCode != http.StatusForbidden {
		t.Fatalf("expected POST /ui/progress/sync without HX-Request to fail with 403, got %v (%v)", csrfResp, err)
	}
	_ = csrfResp.Body.Close()

	// Seed agent-driven progress entries (in_progress, planned, and 12 done records) to verify read-only 3-lane board & pagination
	if _, err := prog.RecordProgress(ctx, domain.ProgressInput{
		Scope:     "project:one",
		Track:     "4탭 진행현황 타임라인 개편",
		Status:    domain.ProgressStatusInProgress,
		Summary:   "4탭 진행상황 UI 구현",
		Details:   "상단 3칸 읽기 전용 보드\n날짜별 세로 히스토리 페이지네이션",
		NextStep:  "E2E 검증",
		MemoryIDs: []int64{1, 2},
		Source:    "agent",
	}); err != nil {
		t.Fatalf("RecordProgress in_progress: %v", err)
	}
	if _, err := prog.RecordProgress(ctx, domain.ProgressInput{
		Scope:   "project:one",
		Track:   "세션 자동 연동",
		Status:  domain.ProgressStatusPlanned,
		Summary: "SessionStart 미완료 작업 컨텍스트 주입 점검",
		Source:  "agent",
	}); err != nil {
		t.Fatalf("RecordProgress planned: %v", err)
	}
	for i := 1; i <= 12; i++ {
		details := ""
		if i == 12 {
			details = "최근 완료 세부 내역 검증"
		}
		if _, err := prog.RecordProgress(ctx, domain.ProgressInput{
			Scope:   "project:one",
			Track:   fmt.Sprintf("완료 작업 #%d", i),
			Status:  domain.ProgressStatusDone,
			Summary: fmt.Sprintf("완료된 마일스톤 항목 %d", i),
			Details: details,
			Source:  "agent",
		}); err != nil {
			t.Fatalf("RecordProgress done %d: %v", i, err)
		}
	}

	progReq, _ := http.NewRequest(http.MethodGet, ts.URL+"/ui/progress?scope=project:one&lang=ko&page=1&page_size=10", nil)
	progReq.Header.Set("HX-Request", "true")
	progResp, err := http.DefaultClient.Do(progReq)
	if err != nil || progResp.StatusCode != http.StatusOK {
		t.Fatalf("GET /ui/progress page 1 failed: %v", err)
	}
	progBytes, _ := io.ReadAll(progResp.Body)
	_ = progResp.Body.Close()
	progHTML := string(progBytes)
	for _, needle := range []string{
		`id="activeTracksBoard"`,
		`lane-doing`,
		`lane-next`,
		`lane-done`,
		`id="btnSyncProgress"`,
		`id="progressTimeline"`,
		`id="progPaginationBar"`,
		`4탭 진행현황 타임라인 개편`,
		`4탭 진행상황 UI 구현`,
		`E2E 검증`,
		`class="prog-detail-fold"`,
	} {
		if !strings.Contains(progHTML, needle) {
			t.Fatalf("expected read-only progress page 1 HTML to contain %q, got:\n%s", needle, progHTML)
		}
	}
	for _, forbidden := range []string{
		`/ui/progress/add`,
		`/ui/progress/status`,
		`/ui/progress/update`,
		`/ui/progress/delete`,
		`/ui/modal/progress-edit`,
	} {
		if strings.Contains(progHTML, forbidden) {
			t.Fatalf("expected read-only progress HTML to omit manual mutation route %q", forbidden)
		}
	}

	// Verify page 2 of history pagination
	page2Req, _ := http.NewRequest(http.MethodGet, ts.URL+"/ui/progress?scope=project:one&lang=ko&page=2&page_size=10", nil)
	page2Req.Header.Set("HX-Request", "true")
	page2Resp, err := http.DefaultClient.Do(page2Req)
	if err != nil || page2Resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /ui/progress page 2 failed: %v", err)
	}
	page2Bytes, _ := io.ReadAll(page2Resp.Body)
	_ = page2Resp.Body.Close()
	if !strings.Contains(string(page2Bytes), "완료된 마일스톤 항목 1") || !strings.Contains(string(page2Bytes), `lane-doing`) {
		t.Fatalf("expected page 2 to render older history items while preserving top 3-lane board")
	}

	// Verify track filter on /ui/progress isolates the requested track
	trkReq, _ := http.NewRequest(http.MethodGet, ts.URL+"/ui/progress?scope=project:one&track="+url.QueryEscape("4탭 진행현황 타임라인 개편"), nil)
	trkReq.Header.Set("HX-Request", "true")
	trkResp, err := http.DefaultClient.Do(trkReq)
	if err != nil || trkResp.StatusCode != http.StatusOK {
		t.Fatalf("GET /ui/progress?track=... failed: %v", err)
	}
	trkBytes, _ := io.ReadAll(trkResp.Body)
	_ = trkResp.Body.Close()
	if !strings.Contains(string(trkBytes), "4탭 진행상황 UI 구현") {
		t.Fatalf("expected track-filtered progress HTML to include matching track item")
	}

	progOv, err := prog.GetProgressOverviewWithQuery(ctx, domain.ProgressQuery{Scope: "project:one", Page: 1, PageSize: 10})
	if err != nil {
		t.Fatalf("GetProgressOverviewWithQuery failed: %v", err)
	}
	if progOv.TotalCount < 14 || len(progOv.Records) != 10 || progOv.TotalPages < 2 || len(progOv.RecentDone) != 5 {
		t.Fatalf("expected paginated progress overview with RecentDone=5, got %+v", progOv)
	}

	// Verify Korean toast in HX-Trigger header uses pure 7-bit ASCII \uXXXX escapes (preventing browser Latin-1 mojibake)
	syncReq, _ := http.NewRequest(http.MethodPost, ts.URL+"/ui/progress/sync?lang=ko", strings.NewReader("scope=project:one"))
	syncReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	syncReq.Header.Set("HX-Request", "true")
	syncResp, err := http.DefaultClient.Do(syncReq)
	if err != nil || syncResp.StatusCode != http.StatusOK {
		t.Fatalf("POST /ui/progress/sync?lang=ko failed: %v (%v)", syncResp, err)
	}
	_ = syncResp.Body.Close()
	rawTrig := syncResp.Header.Get("HX-Trigger")
	for i := 0; i < len(rawTrig); i++ {
		if rawTrig[i] > 0x7F {
			t.Fatalf("expected HX-Trigger HTTP header to be pure 7-bit ASCII (\\uXXXX escaped), found byte 0x%X in %q", rawTrig[i], rawTrig)
		}
	}
	var trigMap map[string]any
	if err := json.Unmarshal([]byte(rawTrig), &trigMap); err != nil {
		t.Fatalf("unmarshal HX-Trigger %q: %v", rawTrig, err)
	}
	toastStr, _ := trigMap["memorizeToast"].(string)
	if !strings.HasPrefix(toastStr, "동기화 완료") {
		t.Fatalf("expected decoded Korean toast starting with '동기화 완료', got %q", toastStr)
	}

	// 5. Verify Guardrail & Plan Verification Logs UI + OOB simulator update + server-side pagination
	for i := 1; i <= 11; i++ {
		_, _ = gr.RecordGuardrailLog(ctx, domain.GuardrailLogInput{
			Scope:       "project:one",
			Source:      "antigravity-pretool",
			ToolName:    "run_command",
			Verdict:     domain.GuardrailVerdictRuleViolation,
			Blocked:     true,
			RuleCount:   3,
			ViolatedIDs: []int64{1},
			PlanSnippet: fmt.Sprintf("위반 명령 테스트 #%d", i),
			Details:     "[#1 project:one/architecture · 96%] 핵심 규칙 위반 차단",
		})
	}
	verReq, _ := http.NewRequest(http.MethodPost, ts.URL+"/ui/verify?lang=ko", strings.NewReader("scope=project:one&plan="+url.QueryEscape("정상 아키텍처 준수 계획 점검")))
	verReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	verReq.Header.Set("HX-Request", "true")
	verResp, err := http.DefaultClient.Do(verReq)
	if err != nil || verResp.StatusCode != http.StatusOK {
		t.Fatalf("POST /ui/verify failed: %v (%v)", verResp, err)
	}
	verBytes, _ := io.ReadAll(verResp.Body)
	_ = verResp.Body.Close()
	if !strings.Contains(string(verBytes), `id="guardrailLogsSection"`) || !strings.Contains(string(verBytes), `hx-swap-oob="true"`) {
		t.Fatalf("expected POST /ui/verify to include OOB #guardrailLogsSection refresh")
	}

	// Guardrails tab (/ui/guardrails) and Calibration tab (/ui/calibrate) must render their separated responsibilities cleanly
	grTabResp, err := http.Get(ts.URL + "/ui/guardrails?lang=ko")
	if err != nil || grTabResp.StatusCode != http.StatusOK {
		t.Fatalf("GET /ui/guardrails failed: %v", err)
	}
	grTabBytes, _ := io.ReadAll(grTabResp.Body)
	_ = grTabResp.Body.Close()
	grTabHTML := string(grTabBytes)
	verIdx := strings.Index(grTabHTML, `id="verifySimulatorCard"`)
	logIdx := strings.Index(grTabHTML, `id="guardrailLogsSection"`)
	if verIdx < 0 || logIdx < 0 || verIdx > logIdx || strings.Contains(grTabHTML, `id="thresholdsCard"`) {
		t.Fatalf("expected /ui/guardrails to place verifySimulatorCard ABOVE guardrailLogsSection and exclude thresholdsCard:\n%s", grTabHTML)
	}
	calTabResp, err := http.Get(ts.URL + "/ui/calibrate?lang=ko")
	if err != nil || calTabResp.StatusCode != http.StatusOK {
		t.Fatalf("GET /ui/calibrate failed: %v", err)
	}
	calTabBytes, _ := io.ReadAll(calTabResp.Body)
	_ = calTabResp.Body.Close()
	calTabHTML := string(calTabBytes)
	if !strings.Contains(calTabHTML, `id="thresholdsCard"`) || !strings.Contains(calTabHTML, `id="probeResults"`) || strings.Contains(calTabHTML, `id="guardrailLogsSection"`) || strings.Contains(calTabHTML, `id="verifySimulatorCard"`) {
		t.Fatalf("expected /ui/calibrate to contain thresholds + relevance probe ONLY (without guardrailLogsSection or verifySimulatorCard):\n%s", calTabHTML)
	}

	grPage1Resp, err := http.Get(ts.URL + "/ui/guardrail-logs?gr_scope=project:one&gr_page=1&gr_page_size=10&lang=ko")
	if err != nil || grPage1Resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /ui/guardrail-logs page 1 failed: %v", err)
	}
	grPage1Bytes, _ := io.ReadAll(grPage1Resp.Body)
	_ = grPage1Resp.Body.Close()
	for _, needle := range []string{`id="guardrailLogsSection"`, `규칙 위반 차단`, `위반 명령 테스트 #11`, `id="guardrailPaginationBar"`} {
		if !strings.Contains(string(grPage1Bytes), needle) {
			t.Fatalf("expected guardrail logs page 1 HTML to contain %q, got:\n%s", needle, string(grPage1Bytes))
		}
	}

	grPage2Resp, err := http.Get(ts.URL + "/ui/guardrail-logs?gr_scope=project:one&gr_page=2&gr_page_size=10&lang=ko")
	if err != nil || grPage2Resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /ui/guardrail-logs page 2 failed: %v", err)
	}
	grPage2Bytes, _ := io.ReadAll(grPage2Resp.Body)
	_ = grPage2Resp.Body.Close()
	if !strings.Contains(string(grPage2Bytes), `위반 명령 테스트 #1`) {
		t.Fatalf("expected guardrail logs page 2 to contain older entry '위반 명령 테스트 #1'")
	}

	grPage, err := gr.QueryGuardrailLogsPage(ctx, domain.GuardrailLogQuery{Scope: "project:one", Page: 1, PageSize: 10})
	if err != nil {
		t.Fatalf("QueryGuardrailLogsPage failed: %v", err)
	}
	if grPage.TotalCount != 12 || grPage.BlockedCount != 11 || grPage.PassCount != 1 || grPage.TotalPages != 2 || len(grPage.Records) != 10 {
		t.Fatalf("unexpected guardrail logs pagination payload: %+v", grPage)
	}

	// 6. Verify default Forever (0d) retention, header-exposed Auto-TTL select, and explicit 2-step Confirm Cleanup controls
	defRetResp, err := http.Get(ts.URL + "/api/retention")
	if err != nil || defRetResp.StatusCode != http.StatusOK {
		t.Fatalf("GET default /api/retention failed: %v", err)
	}
	var defRetPol domain.RetentionPolicy
	if err := json.NewDecoder(defRetResp.Body).Decode(&defRetPol); err != nil {
		t.Fatalf("decode default /api/retention: %v", err)
	}
	_ = defRetResp.Body.Close()
	if defRetPol.ProgressDays != 0 || defRetPol.GuardrailDays != 0 {
		t.Fatalf("expected default ProgressDays=0 GuardrailDays=0 (Forever), got %+v", defRetPol)
	}

	// Single-line exposed header controls (Auto-TTL select + Immediate Cleanup period select + explicit Confirm button, no collapsible drawer):
	for _, needle := range []string{
		`id="progRetentionDays"`,
		`<option value="0" selected>영구 보관</option>`,
		`id="progCleanDaysSelect"`,
		`id="progCleanupControls"`,
		`id="btnCleanupProgressAsk"`,
		`id="btnCleanupProgress"`,
	} {
		if !strings.Contains(progHTML, needle) {
			t.Fatalf("expected /ui/progress HTML to contain %q", needle)
		}
	}
	for _, needle := range []string{
		`id="guardrailRetentionDays"`,
		`<option value="0" selected>영구 보관</option>`,
		`id="guardrailCleanDaysSelect"`,
		`id="guardrailCleanupIncBlocked"`,
		`id="guardrailCleanupControls"`,
		`id="btnCleanupGuardrailAsk"`,
		`id="btnCleanupGuardrail"`,
	} {
		if !strings.Contains(string(grPage1Bytes), needle) {
			t.Fatalf("expected /ui/guardrail-logs HTML to contain %q", needle)
		}
	}

	// Set retention via SSR endpoint
	retReq, _ := http.NewRequest(http.MethodPost, ts.URL+"/ui/progress/retention?lang=ko", strings.NewReader("scope=project:one&retention_days=30"))
	retReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	retReq.Header.Set("HX-Request", "true")
	retResp, err := http.DefaultClient.Do(retReq)
	if err != nil || retResp.StatusCode != http.StatusOK {
		t.Fatalf("POST /ui/progress/retention failed: %v (%v)", retResp, err)
	}
	_ = retResp.Body.Close()

	grRetReq, _ := http.NewRequest(http.MethodPost, ts.URL+"/ui/guardrail-logs/retention?lang=ko", strings.NewReader("gr_scope=project:one&retention_days=7"))
	grRetReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	grRetReq.Header.Set("HX-Request", "true")
	grRetResp, err := http.DefaultClient.Do(grRetReq)
	if err != nil || grRetResp.StatusCode != http.StatusOK {
		t.Fatalf("POST /ui/guardrail-logs/retention failed: %v (%v)", grRetResp, err)
	}
	_ = grRetResp.Body.Close()

	apiRetResp, err := http.Get(ts.URL + "/api/retention")
	if err != nil || apiRetResp.StatusCode != http.StatusOK {
		t.Fatalf("GET /api/retention failed: %v", err)
	}
	var retPol domain.RetentionPolicy
	if err := json.NewDecoder(apiRetResp.Body).Decode(&retPol); err != nil {
		t.Fatalf("decode /api/retention: %v", err)
	}
	_ = apiRetResp.Body.Close()
	if retPol.ProgressDays != 30 || retPol.GuardrailDays != 7 {
		t.Fatalf("expected ProgressDays=30 GuardrailDays=7, got %+v", retPol)
	}

	// Clean guardrail PASS logs only (older_than_days=0 -> removes the 1 PASS log while keeping all 11 blocked violations)
	grCleanPassReq, _ := http.NewRequest(http.MethodPost, ts.URL+"/ui/guardrail-logs/cleanup?lang=ko", strings.NewReader("gr_scope=project:one&older_than_days=0"))
	grCleanPassReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	grCleanPassReq.Header.Set("HX-Request", "true")
	grCleanPassResp, err := http.DefaultClient.Do(grCleanPassReq)
	if err != nil || grCleanPassResp.StatusCode != http.StatusOK {
		t.Fatalf("POST /ui/guardrail-logs/cleanup (PASS only) failed: %v (%v)", grCleanPassResp, err)
	}
	_ = grCleanPassResp.Body.Close()

	grAfterPassPage, _ := gr.QueryGuardrailLogsPage(ctx, domain.GuardrailLogQuery{Scope: "project:one", State: "all", Page: 1, PageSize: 50})
	if grAfterPassPage.TotalCount != 11 || grAfterPassPage.BlockedCount != 11 || grAfterPassPage.PassCount != 0 {
		t.Fatalf("expected PASS-only cleanup to preserve all 11 blocked logs, got %+v", grAfterPassPage)
	}

	// Clean guardrail logs INCLUDING blocked violations (include_blocked=1, older_than_days=0)
	grCleanAllReq, _ := http.NewRequest(http.MethodPost, ts.URL+"/ui/guardrail-logs/cleanup?lang=ko", strings.NewReader("gr_scope=project:one&older_than_days=0&include_blocked=1"))
	grCleanAllReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	grCleanAllReq.Header.Set("HX-Request", "true")
	grCleanAllResp, err := http.DefaultClient.Do(grCleanAllReq)
	if err != nil || grCleanAllResp.StatusCode != http.StatusOK {
		t.Fatalf("POST /ui/guardrail-logs/cleanup (include_blocked=1) failed: %v (%v)", grCleanAllResp, err)
	}
	_ = grCleanAllResp.Body.Close()

	grAfterAllPage, _ := gr.QueryGuardrailLogsPage(ctx, domain.GuardrailLogQuery{Scope: "project:one", State: "all", Page: 1, PageSize: 50})
	if grAfterAllPage.TotalCount != 0 {
		t.Fatalf("expected 0 guardrail logs after include_blocked=1 cleanup, got %+v", grAfterAllPage)
	}

	// Clean all completed progress history (older_than_days=0): removes done milestones while preserving ongoing in_progress & planned tracks!
	progCleanReq, _ := http.NewRequest(http.MethodPost, ts.URL+"/ui/progress/cleanup?lang=ko", strings.NewReader("scope=project:one&older_than_days=0"))
	progCleanReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	progCleanReq.Header.Set("HX-Request", "true")
	progCleanResp, err := http.DefaultClient.Do(progCleanReq)
	if err != nil || progCleanResp.StatusCode != http.StatusOK {
		t.Fatalf("POST /ui/progress/cleanup failed: %v (%v)", progCleanResp, err)
	}
	progCleanHTML, _ := io.ReadAll(progCleanResp.Body)
	_ = progCleanResp.Body.Close()
	if !strings.Contains(string(progCleanHTML), "4탭 진행상황 UI 구현") || strings.Contains(string(progCleanHTML), "완료된 마일스톤 항목 1") {
		t.Fatalf("expected completed history cleaned while ongoing in_progress track remains intact")
	}
}

func TestUnifiedProgressBoardAndLiveSSE(t *testing.T) {
	ctx := context.Background()
	deps := newTestDeps(t, ":memory:", noopEval{}, "", thttp.DashboardHooks{})
	prog := deps.Progress

	ts := httptest.NewServer(dashboard.NewHandler(deps))
	defer ts.Close()

	// 1. Cross-origin SSE must be rejected with 403
	evilReq, _ := http.NewRequest(http.MethodGet, ts.URL+"/api/events", nil)
	evilReq.Header.Set("Origin", "https://evil.example.com")
	evilResp, err := http.DefaultClient.Do(evilReq)
	if err != nil || evilResp.StatusCode != http.StatusForbidden {
		t.Fatalf("expected cross-origin /api/events to return 403, got %v (%v)", evilResp, err)
	}
	_ = evilResp.Body.Close()

	// 2. Connect SSE stream and verify real-time push on Progress & Guardrail mutations
	sseCtx, sseCancel := context.WithTimeout(ctx, 4*time.Second)
	defer sseCancel()
	sseReq, _ := http.NewRequestWithContext(sseCtx, http.MethodGet, ts.URL+"/api/events", nil)
	sseResp, err := http.DefaultClient.Do(sseReq)
	if err != nil || sseResp.StatusCode != http.StatusOK {
		t.Fatalf("GET /api/events failed: %v (%v)", sseResp, err)
	}
	defer sseResp.Body.Close()

	reader := bufio.NewReader(sseResp.Body)
	firstLine, err := reader.ReadString('\n')
	if err != nil || !strings.HasPrefix(firstLine, ": connected") {
		t.Fatalf("expected initial ': connected' SSE comment, got %q (%v)", firstLine, err)
	}
	gotHello := false
	for i := 0; i < 5; i++ {
		hline, herr := reader.ReadString('\n')
		if herr != nil {
			t.Fatalf("read SSE hello line: %v", herr)
		}
		if strings.HasPrefix(hline, "event: hello") {
			hdata, _ := reader.ReadString('\n')
			if strings.Contains(hdata, `"boot":`) {
				gotHello = true
				break
			}
		}
	}
	if !gotHello {
		t.Fatalf("expected initial SSE 'event: hello' with boot ID")
	}

	// Trigger mutations across multiple projects, sessions, and work kinds
	ctxAlpha := domain.WithSessionID(ctx, "sess-alpha-11112222")
	if _, err := prog.RecordProgress(ctxAlpha, domain.ProgressInput{
		Scope:    "project:alpha",
		Track:    "인증 게이트웨이",
		Status:   domain.ProgressStatusInProgress,
		Summary:  "ALPHA_WORK_SUMMARY JWT 토큰 회전 구현 중",
		NextStep: "ALPHA_NEXT_STEP 리프레시 토큰 검증",
		Source:   "agent",
	}); err != nil {
		t.Fatalf("RecordProgress alpha: %v", err)
	}

	// Verify SSE pushed event: change + JSON seq payload
	gotChangeEvent := false
	for i := 0; i < 6; i++ {
		line, rerr := reader.ReadString('\n')
		if rerr != nil {
			t.Fatalf("read SSE event line: %v", rerr)
		}
		if strings.HasPrefix(line, "event: change") {
			dataLine, _ := reader.ReadString('\n')
			if strings.HasPrefix(dataLine, "data: {\"seq\":") && strings.Contains(dataLine, `"domain":"progress"`) {
				gotChangeEvent = true
				break
			}
		}
	}
	if !gotChangeEvent {
		t.Fatalf("expected real-time SSE 'event: change' notification after RecordProgress")
	}
	sseCancel()

	ctxBeta := domain.WithSessionID(ctx, "sess-beta-33334444")
	if _, err := prog.RecordProgress(ctxBeta, domain.ProgressInput{
		Scope:    "project:beta",
		Track:    "결제 정산 배치",
		Status:   domain.ProgressStatusInProgress,
		Summary:  "BETA_WORK_SUMMARY 정산 파이프라인 구축 중",
		NextStep: "BETA_NEXT_STEP 실패 재시도 큐",
		Source:   "agent",
	}); err != nil {
		t.Fatalf("RecordProgress beta: %v", err)
	}
	if _, err := prog.RecordProgress(ctxBeta, domain.ProgressInput{
		Scope:     "project:beta",
		Track:     "architecture / settlement",
		Status:    domain.ProgressStatusDone,
		Summary:   "BETA_MEM_SUMMARY 정산 멱등성 키 불변 규칙 반영",
		MemoryIDs: []int64{7},
		Source:    "memory",
	}); err != nil {
		t.Fatalf("RecordProgress beta memory: %v", err)
	}
	if _, err := prog.RecordProgress(ctx, domain.ProgressInput{
		Scope:     "project:alpha",
		Track:     "architecture / auth",
		Status:    domain.ProgressStatusDone,
		Summary:   "ALPHA_GIT_SUMMARY feat(auth): add JWKS cache",
		CommitSHA: "a1b2c3d4",
		Source:    "git_sync",
	}); err != nil {
		t.Fatalf("RecordProgress alpha git: %v", err)
	}

	fetchProg := func(query string) string {
		t.Helper()
		req, _ := http.NewRequest(http.MethodGet, ts.URL+"/ui/progress?"+query, nil)
		req.Header.Set("HX-Request", "true")
		resp, derr := http.DefaultClient.Do(req)
		if derr != nil || resp.StatusCode != http.StatusOK {
			t.Fatalf("GET /ui/progress?%s failed: %v (%v)", query, resp, derr)
		}
		b, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		return string(b)
	}

	// 3. Default All-Projects unified view shows BOTH projects, BOTH sessions, and ALL 3 work kinds
	allHTML := fetchProg("lang=ko")
	for _, needle := range []string{
		"전체 프로젝트",
		"alpha",
		"beta",
		"sess-alp",
		"sess-bet",
		"구현 · 작업",
		"기억 · 규칙",
		"Git 커밋",
		"ALPHA_WORK_SUMMARY",
		"BETA_WORK_SUMMARY",
		"BETA_MEM_SUMMARY",
		"ALPHA_GIT_SUMMARY",
	} {
		if !strings.Contains(allHTML, needle) {
			t.Fatalf("expected All-Projects unified progress HTML to contain %q", needle)
		}
	}

	// 4. Filtering by session_id updates BOTH top 3-lane board AND bottom history (zero alpha leak!)
	betaSessHTML := fetchProg("lang=ko&session_id=sess-beta-33334444")
	if !strings.Contains(betaSessHTML, "BETA_WORK_SUMMARY") || !strings.Contains(betaSessHTML, "BETA_MEM_SUMMARY") {
		t.Fatalf("expected beta session filter to include beta work & memory entries")
	}
	if strings.Contains(betaSessHTML, "ALPHA_WORK_SUMMARY") || strings.Contains(betaSessHTML, "ALPHA_NEXT_STEP") || strings.Contains(betaSessHTML, "ALPHA_GIT_SUMMARY") {
		t.Fatalf("expected beta session filter to exclude alpha from BOTH top board and bottom timeline")
	}

	// 5. Filtering by kind=git updates BOTH top 3-lane board AND bottom history
	gitOnlyHTML := fetchProg("lang=ko&kind=git")
	if !strings.Contains(gitOnlyHTML, "ALPHA_GIT_SUMMARY") {
		t.Fatalf("expected kind=git filter to include git commit milestone")
	}
	if strings.Contains(gitOnlyHTML, "ALPHA_WORK_SUMMARY") || strings.Contains(gitOnlyHTML, "BETA_WORK_SUMMARY") || strings.Contains(gitOnlyHTML, "BETA_MEM_SUMMARY") {
		t.Fatalf("expected kind=git filter to exclude work/memory items from top board and timeline")
	}

	// 6. Filtering by track updates BOTH top 3-lane board AND bottom history
	trackOnlyHTML := fetchProg("lang=ko&track=" + url.QueryEscape("결제 정산 배치"))
	if !strings.Contains(trackOnlyHTML, "BETA_WORK_SUMMARY") || strings.Contains(trackOnlyHTML, "ALPHA_WORK_SUMMARY") {
		t.Fatalf("expected track filter to synchronize top DoingTracks board with selected track only")
	}
}

func TestGeminiDecisionsCatalogAndFeatureRoutingUI(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "dash_routing.db")
	deps := newTestDeps(t, dbPath, noopEval{}, filepath.Join(t.TempDir(), "models"), thttp.DashboardHooks{})
	svc := deps.Memory

	taskRes := router.NewTaskResolver()
	svc.Router().SetTaskResolver(taskRes)
	svc.Router().SetDynamicResolver(func(cfg map[string]string) domain.Evaluator {
		ev, _ := taskRes(cfg, "")
		return ev
	})

	ts := httptest.NewServer(dashboard.NewHandler(deps))
	defer ts.Close()

	postHX := func(path string, form url.Values) (*http.Response, string) {
		req, _ := http.NewRequest(http.MethodPost, ts.URL+path, strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("HX-Request", "true")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("POST %s failed: %v", path, err)
		}
		b, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		return resp, string(b)
	}

	// 1. Gemini Decision tier shows gemini-decision-1 with Decision-Only badge
	reqDec, _ := http.NewRequest(http.MethodGet, ts.URL+"/ui/engines?view=gemini&gtier=decision", nil)
	reqDec.Header.Set("HX-Request", "true")
	respDec, err := http.DefaultClient.Do(reqDec)
	if err != nil || respDec.StatusCode != http.StatusOK {
		t.Fatalf("GET decision tier failed: %v (%v)", respDec, err)
	}
	decHTMLBytes, _ := io.ReadAll(respDec.Body)
	_ = respDec.Body.Close()
	decHTML := string(decHTMLBytes)
	if !strings.Contains(decHTML, "gemini-decision-1") || !strings.Contains(decHTML, "Decision-Only (No text gen)") {
		t.Fatalf("expected gemini-decision-1 with Decision-Only badge in gtier=decision:\n%s", decHTML)
	}

	// 2. Saving gemini-decision-1 as bulk default exposes Generative Companion selector + heuristic indicators
	saveResp, saveHTML := postHX("/ui/engines/save", url.Values{
		"provider":     {"gemini"},
		"gemini_model": {"gemini-decision-1"},
	})
	if saveResp.StatusCode != http.StatusOK {
		t.Fatalf("POST /ui/engines/save(gemini-decision-1) failed status=%d body=%s", saveResp.StatusCode, saveHTML)
	}
	for _, want := range []string{
		`id="engineActiveBadge"`,
		`id="taskRoutingSection"`,
		`Decision-Model Compatible`,
		`Decision OK (0-tok)`,
		`Text-Generation Only (No Decision Models)`,
		`Text-Gen Only (No Decision)`,
		`name="gen_provider"`,
		`gemini:gemini-decision-1`,
		`Built-in Heuristic`,
	} {
		if !strings.Contains(saveHTML, want) {
			t.Fatalf("expected %q in engines HTML after saving gemini-decision-1:\n%s", want, saveHTML)
		}
	}

	// 3. Server rejects assigning non-generative models to text-generation tasks or gen_provider
	badForm := url.Values{
		"task_consolidate_provider": {"gemini"},
		"task_consolidate_model":    {"gemini-decision-1"},
	}
	badResp, badBody := postHX("/ui/engines/routing", badForm)
	if badResp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400 rejecting gemini-decision-1 on consolidate task, got %d (%s)", badResp.StatusCode, badBody)
	}
	badGenResp, badGenBody := postHX("/ui/engines/routing", url.Values{"gen_provider": {"jev"}})
	if badGenResp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400 rejecting jev as generative companion, got %d (%s)", badGenResp.StatusCode, badGenBody)
	}

	// 4. Valid mixed routing: gen_provider=gemini (3.8-flash) + task_taxonomy=local (qwen3:4b)
	okResp, okHTML := postHX("/ui/engines/routing", url.Values{
		"gen_provider":           {"gemini"},
		"gen_model":              {"gemini-3.8-flash"},
		"task_taxonomy_provider": {"local"},
		"task_taxonomy_model":    {"qwen3:4b"},
	})
	if okResp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 on valid routing save, got %d (%s)", okResp.StatusCode, okHTML)
	}
	for _, want := range []string{
		`id="engineActiveBadge"`,
		`+1 custom`,
		"Custom Routing",
		"Reset All to Bulk",
		"gemini:gemini-3.8-flash",
		"local:qwen3:4b",
		"gemini:gemini-decision-1",
	} {
		if !strings.Contains(okHTML, want) {
			t.Fatalf("expected %q in custom routing response:\n%s", want, okHTML)
		}
	}

	// 5. Reset All to Bulk clears custom task routes back to Bulk Mode
	rstResp, rstHTML := postHX("/ui/engines/routing", url.Values{"mode": {"reset_all"}})
	if rstResp.StatusCode != http.StatusOK || !strings.Contains(rstHTML, "Bulk Mode (All Inherit)") {
		t.Fatalf("expected Reset All to Bulk to restore Bulk Mode, got status=%d\n%s", rstResp.StatusCode, rstHTML)
	}

	// 6. SSR /ui/engines/routing rejects non-generative provider on progress task and resolves mixed TaskProviders
	badProgResp, badProgBody := postHX("/ui/engines/routing", url.Values{"task_progress_provider": {"jev"}})
	if badProgResp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400 from /ui/engines/routing when assigning jev to progress task, got %d (%s)", badProgResp.StatusCode, badProgBody)
	}

	okMixResp, okMixBody := postHX("/ui/engines/routing", url.Values{
		"gen_provider":           {"gemini"},
		"gen_model":              {"gemini-3.5-flash-lite"},
		"task_verify_provider":   {"gemini"},
		"task_verify_model":      {"gemini-decision-1"},
		"task_progress_provider": {"local"},
		"task_progress_model":    {"qwen3:4b"},
	})
	if okMixResp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 from /ui/engines/routing mixed update, got %d (%s)", okMixResp.StatusCode, okMixBody)
	}
	if p := svc.ProviderNameForTask(ctx, domain.TaskVerify); p != "gemini:gemini-decision-1" {
		t.Fatalf("expected TaskVerify gemini:gemini-decision-1, got %q", p)
	}
	if p := svc.ProviderNameForTask(ctx, domain.TaskProgress); p != "local:qwen3:4b" {
		t.Fatalf("expected TaskProgress local:qwen3:4b, got %q", p)
	}
	if p := svc.ProviderNameForTask(ctx, domain.TaskConsolidate); p != "gemini:gemini-3.5-flash-lite" {
		t.Fatalf("expected TaskConsolidate gemini:gemini-3.5-flash-lite, got %q", p)
	}
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

func TestTopEngineSetupWarningBannerAndOOBClearing(t *testing.T) {
	t.Setenv("GEMINI_API_KEY", "")
	t.Setenv("MEMORY_PROVIDER", "")
	t.Setenv("TYPESAFE_API_KEY", "")
	t.Setenv("LOCAL_MODEL_URL", "")

	deps := newTestDeps(t, ":memory:", nil, "", thttp.DashboardHooks{})
	deps.Memory.Router().SetTaskResolver(router.NewTaskResolver())

	h := dashboard.NewHandler(deps)
	ts := httptest.NewServer(h)
	defer ts.Close()

	getRoot := func(lang string) string {
		req, _ := http.NewRequest(http.MethodGet, ts.URL+"/", nil)
		if lang != "" {
			req.AddCookie(&http.Cookie{Name: "memorize_lang", Value: lang})
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("GET /: %v", err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return string(b)
	}

	// 1. Unconfigured engine shows the top setup warning banner + Configure Engine button in both EN and KO
	enRoot := getRoot("en")
	if !strings.Contains(enRoot, `id="engineSetupAlert" class="setup-alert"`) || !strings.Contains(enRoot, `id="btnGoEngines"`) {
		t.Fatalf("expected visible #engineSetupAlert banner and #btnGoEngines on unconfigured dashboard:\n%s", enRoot)
	}
	koRoot := getRoot("ko")
	if !strings.Contains(koRoot, "추론 엔진 설정 필요") || !strings.Contains(koRoot, "추론 엔진 설정하기") {
		t.Fatalf("expected Korean setup warning text on dashboard:\n%s", koRoot)
	}

	// 2. Selecting Gemini without GEMINI_API_KEY updates banner via OOB to keep warning active
	code, noKeyHTML := doHX(t, http.MethodPost, ts.URL+"/ui/engines/save", url.Values{
		"provider":     {"gemini"},
		"gemini_model": {"gemini-3.5-flash-lite"},
	})
	if code != http.StatusOK || !strings.Contains(noKeyHTML, `id="engineSetupAlert" class="setup-alert" role="alert" hx-swap-oob="outerHTML"`) || !strings.Contains(noKeyHTML, "GEMINI_API_KEY") {
		t.Fatalf("expected OOB GEMINI_API_KEY warning after saving gemini without key (code=%d):\n%s", code, noKeyHTML)
	}

	// 3. Providing GEMINI_API_KEY clears the banner live via OOB and keeps it hidden on reload
	code, withKeyHTML := doHX(t, http.MethodPost, ts.URL+"/ui/engines/save", url.Values{
		"provider":       {"gemini"},
		"gemini_model":   {"gemini-3.5-flash-lite"},
		"gemini_api_key": {"AIzaTestDummyKeyForUnitTest1234"},
	})
	if code != http.StatusOK || !strings.Contains(withKeyHTML, `<div id="engineSetupAlert" hidden hx-swap-oob="outerHTML"></div>`) {
		t.Fatalf("expected OOB hidden #engineSetupAlert after saving valid gemini key (code=%d):\n%s", code, withKeyHTML)
	}
	clearedRoot := getRoot("en")
	if strings.Contains(clearedRoot, `class="setup-alert"`) || !strings.Contains(clearedRoot, `<div id="engineSetupAlert" hidden ></div>`) {
		t.Fatalf("expected hidden #engineSetupAlert after reload:\n%s", clearedRoot)
	}
}

func TestPortModalSaveResetCSRFAndDaemonHooks(t *testing.T) {
	tmp := t.TempDir()
	dbPath := filepath.Join(tmp, "memories.db")
	modelsDir := filepath.Join(tmp, "models")

	var appliedDP, appliedGP int
	deps := newTestDeps(t, dbPath, noopEval{}, modelsDir, thttp.DashboardHooks{
		DaemonStatusFn: func() thttp.DaemonPortState {
			cfg := config.LoadPortConfig(dbPath)
			return thttp.DaemonPortState{
				PID:            4242,
				ListenAddr:     fmt.Sprintf("127.0.0.1:%d", cfg.DaemonPort),
				UptimeSec:      12,
				ConfiguredPort: cfg.DaemonPort,
				ActivePort:     cfg.DaemonPort,
			}
		},
		ApplyDaemonPortFn: func(_ context.Context, dp, gp int) (thttp.DaemonPortApplyResult, error) {
			appliedDP, appliedGP = dp, gp
			return thttp.DaemonPortApplyResult{
				ListenAddr:       fmt.Sprintf("127.0.0.1:%d", dp),
				ActiveDaemonPort: dp,
				Restarting:       true,
				NewURL:           fmt.Sprintf("http://127.0.0.1:%d", dp),
				ActiveGGUFPort:   gp,
				GGUFEndpoint:     fmt.Sprintf("http://127.0.0.1:%d", gp),
			}, nil
		},
	})
	h := dashboard.NewHandler(deps)
	ts := httptest.NewServer(h)
	defer ts.Close()

	// 1. Root page includes clickable #daemonBadge -> /ui/modal/ports, and ui.js handles memorizePortRedirect.
	rootResp, err := http.Get(ts.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	rootBytes, _ := io.ReadAll(rootResp.Body)
	_ = rootResp.Body.Close()
	if !strings.Contains(string(rootBytes), `id="daemonBadge"`) || !strings.Contains(string(rootBytes), `/ui/modal/ports`) {
		t.Fatalf("expected clickable #daemonBadge linked to /ui/modal/ports in root HTML")
	}

	jsResp, err := http.Get(ts.URL + "/assets/ui.js")
	if err != nil {
		t.Fatal(err)
	}
	jsBytes, _ := io.ReadAll(jsResp.Body)
	_ = jsResp.Body.Close()
	if !strings.Contains(string(jsBytes), "memorizePortRedirect") {
		t.Fatalf("expected ui.js to handle memorizePortRedirect event")
	}

	// 2. Render modal_ports with current port inputs.
	code, modalHTML := doHX(t, http.MethodGet, ts.URL+"/ui/modal/ports", nil)
	if code != http.StatusOK || !strings.Contains(modalHTML, `id="portsDialog"`) ||
		!strings.Contains(modalHTML, `name="daemon_port"`) || !strings.Contains(modalHTML, `name="gguf_port"`) {
		t.Fatalf("expected modal_ports HTML (code=%d):\n%s", code, modalHTML)
	}

	// 3. Reject non-HTMX POST (CSRF) and invalid/equal ports.
	unauthResp, err := http.PostForm(ts.URL+"/ui/ports/save", url.Values{"daemon_port": {"19095"}, "gguf_port": {"19088"}})
	if err != nil {
		t.Fatal(err)
	}
	_ = unauthResp.Body.Close()
	if unauthResp.StatusCode != http.StatusForbidden {
		t.Fatalf("expected 403 Forbidden for non-HX POST /ui/ports/save, got %d", unauthResp.StatusCode)
	}

	badCode, _ := doHX(t, http.MethodPost, ts.URL+"/ui/ports/save", url.Values{"daemon_port": {"19000"}, "gguf_port": {"19000"}})
	if badCode != http.StatusBadRequest {
		t.Fatalf("expected 400 Bad Request for equal ports, got %d", badCode)
	}

	// 4. Valid HX save persists ports.json, invokes daemon hook, and emits memorizePortRedirect trigger.
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/ui/ports/save", strings.NewReader(url.Values{"daemon_port": {"19195"}, "gguf_port": {"19188"}}.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("HX-Request", "true")
	saveResp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	saveBody, _ := io.ReadAll(saveResp.Body)
	_ = saveResp.Body.Close()
	if saveResp.StatusCode != http.StatusOK || appliedDP != 19195 || appliedGP != 19188 {
		t.Fatalf("expected 200 OK & applied (19195,19188), got code=%d applied=(%d,%d) body=%s", saveResp.StatusCode, appliedDP, appliedGP, string(saveBody))
	}
	if cfg := config.LoadPortConfig(dbPath); cfg.DaemonPort != 19195 || cfg.GGUFPort != 19188 {
		t.Fatalf("expected saved ports.json (19195, 19188), got %+v", cfg)
	}
	var trig map[string]any
	if err := json.Unmarshal([]byte(saveResp.Header.Get("HX-Trigger")), &trig); err != nil {
		t.Fatalf("parse HX-Trigger %q: %v", saveResp.Header.Get("HX-Trigger"), err)
	}
	redir, _ := trig["memorizePortRedirect"].(map[string]any)
	if redir["url"] != "http://127.0.0.1:19195" {
		t.Fatalf("expected memorizePortRedirect url http://127.0.0.1:19195, got %+v", trig)
	}

	// 5. Reset action restores defaults (18095 / 18088).
	resetCode, _ := doHX(t, http.MethodPost, ts.URL+"/ui/ports/save", url.Values{"reset": {"1"}})
	if resetCode != http.StatusOK || appliedDP != domain.DefaultDaemonPort || appliedGP != domain.DefaultGGUFPort {
		t.Fatalf("expected reset to restore (18095, 18088), got code=%d applied=(%d,%d)", resetCode, appliedDP, appliedGP)
	}
}

func TestDashboardProgressLiveSessionAndDoneWindow(t *testing.T) {
	deps := newTestDeps(t, ":memory:", noopEval{}, "", thttp.DashboardHooks{})
	prog := deps.Progress

	ctx := context.Background()
	scope := "project:liveui"
	if _, err := prog.RecordProgress(ctx, domain.ProgressInput{
		Scope:    scope,
		Track:    "Completed Feature",
		Category: "backend",
		Status:   domain.ProgressStatusDone,
		Summary:  "Finished live window selector backend",
		Source:   "agent",
	}); err != nil {
		t.Fatal(err)
	}

	h := dashboard.NewHandler(deps)
	ts := httptest.NewServer(h)
	defer ts.Close()

	// 1. Progress HTML renders done_window selector, completion-time clarity, and liveSessionsStrip
	//    while strictly keeping ephemeral live sessions out of SQLite Doing lane and preserving read-only invariants.
	prog.TouchLiveSession(scope, "conv-ui-live-1", "edit_file", "/repo/daemon.go")

	resp, err := http.Get(ts.URL + "/ui/progress?scope=" + url.QueryEscape(scope) + "&done_window=24h&lang=ko")
	if err != nil {
		t.Fatal(err)
	}
	bodyBytes, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	html := string(bodyBytes)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 OK from /ui/progress, got %d", resp.StatusCode)
	}
	for _, want := range []string{
		`id="progDoneWindow"`,
		`id="liveSessionsStrip"`,
		`value="24h" selected`,
		`최근 24시간`,
		`완료 시각순`,
		`방금 전`,
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("expected progress HTML to contain %q, got:\n%s", want, html)
		}
	}
	if strings.Contains(html, "conv-ui-live-1") {
		t.Fatalf("expected ephemeral live session NOT written into SQLite SSR cards")
	}
	for _, forbidden := range []string{"/ui/progress/add", "/ui/progress/status", "/ui/progress/update", "/ui/progress/delete", "/ui/modal/progress-edit"} {
		if strings.Contains(html, forbidden) {
			t.Fatalf("read-only Progress contract violated: found forbidden route %q", forbidden)
		}
	}

	// Verify English rendering, alternate done_window=30d selection, and root layout header badge.
	enResp, err := http.Get(ts.URL + "/ui/progress?scope=" + url.QueryEscape(scope) + "&done_window=30d&lang=en")
	if err != nil {
		t.Fatal(err)
	}
	enBytes, _ := io.ReadAll(enResp.Body)
	_ = enResp.Body.Close()
	enHTML := string(enBytes)
	for _, want := range []string{`value="30d" selected`, `Last 30d`, `by completion time`} {
		if !strings.Contains(enHTML, want) {
			t.Fatalf("expected English /ui/progress HTML to contain %q", want)
		}
	}

	rootResp, err := http.Get(ts.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	rootBytes, _ := io.ReadAll(rootResp.Body)
	_ = rootResp.Body.Close()
	if !strings.Contains(string(rootBytes), `id="liveSessionsBadge"`) {
		t.Fatalf("expected root layout to include liveSessionsBadge")
	}

	// 2. GET /api/live-sessions serves ephemeral in-memory session status and reflects ClearLiveSession immediately.
	liveResp, err := http.Get(ts.URL + "/api/live-sessions")
	if err != nil {
		t.Fatal(err)
	}
	var livePayload struct {
		Sessions []domain.LiveSession `json:"sessions"`
	}
	if err := json.NewDecoder(liveResp.Body).Decode(&livePayload); err != nil {
		_ = liveResp.Body.Close()
		t.Fatalf("decode /api/live-sessions: %v", err)
	}
	_ = liveResp.Body.Close()
	if len(livePayload.Sessions) != 1 || livePayload.Sessions[0].Tool != "edit_file" || livePayload.Sessions[0].Target != "daemon.go" {
		t.Fatalf("unexpected /api/live-sessions payload: %+v", livePayload)
	}

	prog.ClearLiveSession(scope, "conv-ui-live-1")
	liveAfterResp, err := http.Get(ts.URL + "/api/live-sessions")
	if err != nil {
		t.Fatal(err)
	}
	var liveAfter struct {
		Sessions []domain.LiveSession `json:"sessions"`
	}
	if err := json.NewDecoder(liveAfterResp.Body).Decode(&liveAfter); err != nil {
		_ = liveAfterResp.Body.Close()
		t.Fatalf("decode cleared /api/live-sessions: %v", err)
	}
	_ = liveAfterResp.Body.Close()
	if len(liveAfter.Sessions) != 0 {
		t.Fatalf("expected empty /api/live-sessions after ClearLiveSession, got %+v", liveAfter.Sessions)
	}

	// 3. Minimalist ui.js keeps lightweight live-session badge updates + domain SSE filtering without custom DOM morpher bloat.
	jsResp, err := http.Get(ts.URL + "/assets/ui.js")
	if err != nil {
		t.Fatal(err)
	}
	jsBytes, _ := io.ReadAll(jsResp.Body)
	_ = jsResp.Body.Close()
	js := string(jsBytes)
	for _, want := range []string{
		"refreshLiveSessions", "addEventListener('live'", "es.onerror",
		"handleConnectivityResume", "visibilitychange", "progDoneWindow", "domainToTabs", "defaultSettleDelay",
	} {
		if !strings.Contains(js, want) {
			t.Fatalf("expected ui.js to contain %q", want)
		}
	}
	for _, removed := range []string{"morphTargetHTML", "startFallbackPoll"} {
		if strings.Contains(js, removed) {
			t.Fatalf("expected removed bloat %q not to appear in ui.js", removed)
		}
	}

	cssResp, err := http.Get(ts.URL + "/assets/styles.css")
	if err != nil {
		t.Fatal(err)
	}
	cssBytes, _ := io.ReadAll(cssResp.Body)
	_ = cssResp.Body.Close()
	css := string(cssBytes)
	if !strings.Contains(css, "scrollbar-gutter: stable") || !strings.Contains(css, ".live-sessions-strip") {
		t.Fatalf("expected styles.css to include scrollbar-gutter: stable and .live-sessions-strip")
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

type noneEval struct{ noopEval }

func (noneEval) Name() string { return "none" }

func TestLocalInferenceProbeAndSetupLanding(t *testing.T) {
	ctx := context.Background()

	ollamaSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"id":"qwen3:4b"},{"id":"gemma3:12b"}]}`))
	}))
	defer ollamaSrv.Close()

	flideSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/models":
			http.NotFound(w, r)
		case "/health":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"status":"ok","model":"OmniJev/OneJev1.6-0.8B"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer flideSrv.Close()

	htmlSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte(`<html><body>not an engine</body></html>`))
	}))
	defer htmlSrv.Close()

	closedSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
	closedURL := closedSrv.URL
	closedSrv.Close()

	got := discovery.ProbeLocalInferenceServers(ctx,
		discovery.LocalProbeTarget{Label: "Ollama", Provider: "local", BaseURL: ollamaSrv.URL + "/v1"},
		discovery.LocalProbeTarget{Label: "Flide / OmniJev", Provider: "jev", BaseURL: flideSrv.URL},
		discovery.LocalProbeTarget{Label: "WebUI", Provider: "local", BaseURL: htmlSrv.URL},
		discovery.LocalProbeTarget{Label: "Down", Provider: "local", BaseURL: closedURL},
	)
	if len(got) != 2 {
		t.Fatalf("expected 2 verified local inference servers, got %d (%+v)", len(got), got)
	}
	if got[0].Label != "Ollama" || got[0].Provider != "local" || len(got[0].Models) != 2 || got[0].Models[0] != "qwen3:4b" {
		t.Fatalf("unexpected Ollama probe result: %+v", got[0])
	}
	if got[1].Label != "Flide / OmniJev" || got[1].Provider != "jev" || len(got[1].Models) < 2 || got[1].Models[0] != "jev-latest" {
		t.Fatalf("unexpected Flide probe result: %+v", got[1])
	}

	// Verify landing page emits data-setup-required="1" when unconfigured and clears it after saving Gemini key.
	for _, k := range []string{
		"MEMORY_PROVIDER", "GEMINI_API_KEY", "TYPESAFE_API_KEY",
		"FLIDE_API_KEY", "JEV_API_URL", "LOCAL_MODEL_URL", "LOCAL_GGUF_URL",
	} {
		t.Setenv(k, "")
	}
	deps := newTestDeps(t, ":memory:", noneEval{}, "", thttp.DashboardHooks{})

	h := dashboard.NewHandler(deps)
	ts := httptest.NewServer(h)
	defer ts.Close()

	resp1, err := http.Get(ts.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	body1, _ := io.ReadAll(resp1.Body)
	_ = resp1.Body.Close()
	if !strings.Contains(string(body1), `data-setup-required="1"`) {
		t.Fatalf("expected data-setup-required=\"1\" on unconfigured landing page")
	}

	form := url.Values{
		"provider":       {"gemini"},
		"gemini_api_key": {"AIzaTestKey123456"},
	}
	saveReq, err := http.NewRequest(http.MethodPost, ts.URL+"/ui/engines/save", strings.NewReader(form.Encode()))
	if err != nil {
		t.Fatal(err)
	}
	saveReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	saveReq.Header.Set("HX-Request", "true")
	saveResp, err := http.DefaultClient.Do(saveReq)
	if err != nil {
		t.Fatal(err)
	}
	_ = saveResp.Body.Close()
	if saveResp.StatusCode != http.StatusOK {
		t.Fatalf("POST /ui/engines/save status = %d", saveResp.StatusCode)
	}

	resp2, err := http.Get(ts.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	body2, _ := io.ReadAll(resp2.Body)
	_ = resp2.Body.Close()
	if strings.Contains(string(body2), `data-setup-required="1"`) || !strings.Contains(string(body2), `data-setup-required=""`) {
		t.Fatalf("expected data-setup-required=\"\" after configuring Gemini key")
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
