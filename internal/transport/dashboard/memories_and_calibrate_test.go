package dashboard

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
	"memorize/internal/inference/providers"
	thttp "memorize/internal/transport/http"
)

func TestModularStaticAssetsDNSRebindingAndPinPreservation(t *testing.T) {
	ctx := context.Background()
	deps := newTestDeps(t, ":memory:", noopEval{}, "", thttp.DashboardHooks{})
	svc := deps.Memory

	_, err := svc.CommitMemory(ctx, "핵심 고정 불변 규칙", "architecture", "project:ui")
	if err != nil {
		t.Fatalf("CommitMemory: %v", err)
	}

	h := NewHandler(deps)
	ts := httptest.NewServer(h)
	defer ts.Close()

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

	if err := svc.UpdateMemoryFull(ctx, 1, "", "", nil, "수정된 핵심 고정 불변 규칙 (svc)", nil); err != nil {
		t.Fatalf("UpdateMemoryFull partial failed: %v", err)
	}
	page, _ := svc.QueryRecordsPage(ctx, domain.RecordQuery{Scope: "project:ui", State: "all", PageSize: 50})
	if len(page.Records) != 1 || !page.Records[0].Pinned || page.Records[0].Content != "수정된 핵심 고정 불변 규칙 (svc)" {
		t.Fatalf("expected nil pinned pointer to preserve Pinned=true, got %+v", page.Records)
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
	page, _ = svc.QueryRecordsPage(ctx, domain.RecordQuery{Scope: "project:ui", State: "all", PageSize: 50})
	if len(page.Records) != 1 || !page.Records[0].Pinned || page.Records[0].Content != "수정된 핵심 고정 불변 규칙" {
		t.Fatalf("expected /ui/memories/update to preserve Pinned=true, got %+v", page.Records)
	}

	if _, err := svc.CommitMemory(ctx, "   ", "architecture", "project:ui"); err == nil {
		t.Fatalf("expected whitespace-only CommitMemory to fail")
	}
	if emptyCode, _ := doHX(t, http.MethodPost, ts.URL+"/ui/memories/commit", url.Values{
		"new_scope": {"project:ui"},
		"candidate": {"  "},
	}); emptyCode != http.StatusBadRequest {
		t.Fatalf("expected POST /ui/memories/commit with empty candidate to return 400, got %d", emptyCode)
	}

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

	h := NewHandler(deps)
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

	for i := 1; i <= 23; i++ {
		commitOne("project:scale", "", "architecture", "storage", fmt.Sprintf("대용량 페이지네이션 검증 항목 %02d", i))
	}
	commitOne("personal", "alice", "preference", "naming", "정확한 언더스코어 토큰 foo_bar 매칭 규칙")
	commitOne("personal", "bob", "preference", "naming", "언더스코어 오탐 방지용 fooXbar 규칙")

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

	h := NewHandler(deps)
	ts := httptest.NewServer(h)
	defer ts.Close()

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
		if len(ab) == 0 {
			t.Fatalf("static asset %s returned empty body", assetPath)
		}
	}
	tplFiles, err := filepath.Glob("templates/*.html")
	if err != nil || len(tplFiles) == 0 {
		t.Fatalf("expected templates/*.html files, got %v (%v)", tplFiles, err)
	}
	for _, tf := range tplFiles {
		raw, err := os.ReadFile(tf)
		if err != nil || len(raw) == 0 {
			t.Fatalf("read template %s: %v (len=%d)", tf, err, len(raw))
		}
	}

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

	if _, err := svc.CommitMemory(ctx, "오래된 핵심 고정 규칙 #1", "architecture", "project:ssr"); err != nil {
		t.Fatalf("CommitMemory #1: %v", err)
	}
	if _, err := svc.CommitMemory(ctx, "방금 추가한 일반 최신 규칙 #2", "architecture", "project:ssr"); err != nil {
		t.Fatalf("CommitMemory #2: %v", err)
	}
	pinnedFalse := false
	sub := ""
	if err := svc.UpdateMemoryFull(domain.WithAdminBypass(ctx), 2, "project:ssr", "architecture", &sub, "방금 추가한 일반 최신 규칙 #2", &pinnedFalse); err != nil {
		t.Fatalf("UpdateMemoryFull unpin #2: %v", err)
	}

	h := NewHandler(deps)
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

	noHXReq, _ := http.NewRequest(http.MethodPost, ts.URL+"/ui/engines/save", strings.NewReader("provider=gemini"))
	noHXReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	noHXResp, err := http.DefaultClient.Do(noHXReq)
	if err != nil || noHXResp.StatusCode != http.StatusForbidden {
		t.Fatalf("expected POST /ui/engines/save without HX-Request header to return 403, got %v (%v)", noHXResp, err)
	}
	_ = noHXResp.Body.Close()

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

	resetEnginesHTML := getBody("/ui/engines")
	if !strings.Contains(resetEnginesHTML, `id="pane-gemini"`) || !strings.Contains(resetEnginesHTML, `>ACTIVE: GEMINI<`) {
		t.Fatalf("expected /ui/engines without view param to reset to active pane-gemini")
	}

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

	h := NewHandler(deps)

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

	discCode, discHTML := doHX(t, http.MethodPost, ts.URL+"/ui/engines/discover-jev", url.Values{
		"jev_api_url": {foreignSrv.URL + "/v1"},
	})
	if discCode != http.StatusOK || !strings.Contains(discHTML, "foreign/model") {
		t.Fatalf("POST /ui/engines/discover-jev failed (%d):\n%s", discCode, discHTML)
	}
	if leakedAuth != "" {
		t.Fatalf("security failure: leaked stored Authorization header %q to foreign discovery URL", leakedAuth)
	}

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

	h := NewHandler(deps)
	ts := httptest.NewServer(h)
	defer ts.Close()

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

	defTh := domain.DefaultThresholds()
	if defTh.MinPermanent != 0.75 || defTh.MinConfidence != 0.80 || defTh.PinInvariant != 0.88 || defTh.MinRelevance != 3.5 || defTh.MinRelConfidence != 0.75 {
		t.Fatalf("unexpected DefaultThresholds: %+v", defTh)
	}

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

	ts := httptest.NewServer(NewHandler(deps))
	defer ts.Close()

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

	stats, err := svc.GetDashboardStatsForScope(ctx, "")
	if err != nil {
		t.Fatalf("GetDashboardStatsForScope: %v", err)
	}
	if stats.ConfidenceBins[1] != 1 || stats.ConfidenceBins[3] != 1 || stats.ConfidenceBins[4] != 1 {
		t.Fatalf("expected individualized distribution [0 1 0 1 1], got %v", stats.ConfidenceBins)
	}

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

	h := NewHandler(deps)
	ts := httptest.NewServer(h)
	defer ts.Close()

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

	badReq, _ := http.NewRequest(http.MethodPost, ts.URL+"/ui/engines/telemetry/reset", nil)
	badResp, err := http.DefaultClient.Do(badReq)
	if err != nil {
		t.Fatal(err)
	}
	_ = badResp.Body.Close()
	if badResp.StatusCode != http.StatusForbidden {
		t.Fatalf("expected 403 without HX-Request on /ui/engines/telemetry/reset, got %d", badResp.StatusCode)
	}

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
