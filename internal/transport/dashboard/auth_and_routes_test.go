package dashboard

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"memorize/internal/domain"
	"memorize/internal/infra/config"
	"memorize/internal/infra/sqlite"
	thttp "memorize/internal/transport/http"
)

func TestI18nENKOParity(t *testing.T) {
	for k, v := range enTranslations {
		if kv, ok := koTranslations[k]; !ok || kv == "" {
			t.Errorf("en key %q (=%q) missing or empty in koTranslations", k, v)
		}
	}
	for k, v := range koTranslations {
		if ev, ok := enTranslations[k]; !ok || ev == "" {
			t.Errorf("ko key %q (=%q) missing or empty in enTranslations", k, v)
		}
	}
}

func TestDashboardHTTPRoutesMutationsEnginesAndCSRF(t *testing.T) {
	ctx := context.Background()
	deps := newTestDeps(t, ":memory:", noopEval{}, "", thttp.DashboardHooks{})
	svc := deps.Memory

	h := NewHandler(deps)
	ts := httptest.NewServer(h)
	defer ts.Close()

	for _, tc := range []struct {
		enabled string
		wantCfg string
	}{
		{"1", "1"},
		{"0", "0"},
	} {
		code, _ := doHX(t, http.MethodPost, ts.URL+"/ui/personal-toggle", url.Values{"enabled": {tc.enabled}})
		if code != http.StatusOK {
			t.Fatalf("POST /ui/personal-toggle enabled=%s -> %d", tc.enabled, code)
		}
		if got := svc.GetConfigMap(ctx)["include_personal_in_project"]; got != tc.wantCfg {
			t.Fatalf("expected include_personal_in_project=%q, got %q", tc.wantCfg, got)
		}
	}

	for _, txt := range []string{
		"첫 번째 라우트 테스트 규칙",
		"두 번째 라우트 테스트 규칙",
	} {
		cCode, _ := doHX(t, http.MethodPost, ts.URL+"/ui/memories/commit", url.Values{
			"new_scope":        {"project:routes"},
			"new_category":     {"architecture"},
			"new_sub_category": {"routing"},
			"new_account":      {"alice"},
			"new_tags":         {"http,ssr"},
			"candidate":        {txt},
		})
		if cCode != http.StatusOK {
			t.Fatalf("POST /ui/memories/commit(%q) -> %d", txt, cCode)
		}
	}

	uCode, _ := doHX(t, http.MethodPost, ts.URL+"/ui/memories/update", url.Values{
		"id":           {"1"},
		"scope":        {"project:routes"},
		"category":     {"architecture"},
		"sub_category": {"security"},
		"account":      {"bob"},
		"tags":         {"auth,csrf"},
		"content":      {"수정된 첫 번째 라우트 테스트 규칙"},
		"pinned":       {"1"},
	})
	if uCode != http.StatusOK {
		t.Fatalf("POST /ui/memories/update -> %d", uCode)
	}
	detCode, detHTML := doHX(t, http.MethodGet, ts.URL+"/ui/modal/detail?id=1", nil)
	if detCode != http.StatusOK || !strings.Contains(detHTML, "수정된 첫 번째 라우트 테스트 규칙") || !strings.Contains(detHTML, "bob") {
		t.Fatalf("GET /ui/modal/detail?id=1 failed (%d):\n%s", detCode, detHTML)
	}

	prevCode, prevHTML := doHX(t, http.MethodPost, ts.URL+"/ui/consolidate/preview", url.Values{
		"merge_scope":    {"project:routes"},
		"merge_category": {"architecture"},
		"ids":            {"1", "2"},
	})
	if prevCode != http.StatusOK || !strings.Contains(prevHTML, "수정된 첫 번째 라우트 테스트 규칙") {
		t.Fatalf("POST /ui/consolidate/preview failed (%d):\n%s", prevCode, prevHTML)
	}
	execCode, _ := doHX(t, http.MethodPost, ts.URL+"/ui/consolidate/execute", url.Values{
		"merge_scope":    {"project:routes"},
		"merge_category": {"architecture"},
		"ids":            {"1", "2"},
		"merge_pinned":   {"1"},
		"content":        {"라우트 테스트 규칙 통합본"},
	})
	if execCode != http.StatusOK {
		t.Fatalf("POST /ui/consolidate/execute failed: %d", execCode)
	}
	fCode, _ := doHX(t, http.MethodPost, ts.URL+"/ui/memories/forget", url.Values{"ids": {"3"}})
	if fCode != http.StatusOK {
		t.Fatalf("POST /ui/memories/forget failed: %d", fCode)
	}
	unCode, _ := doHX(t, http.MethodPost, ts.URL+"/ui/memories/undo", url.Values{"id": {"3"}})
	if unCode != http.StatusOK {
		t.Fatalf("POST /ui/memories/undo failed: %d", unCode)
	}
	pCode, _ := doHX(t, http.MethodPost, ts.URL+"/ui/memories/purge", url.Values{"scope": {"project:routes"}})
	if pCode != http.StatusOK {
		t.Fatalf("POST /ui/memories/purge failed: %d", pCode)
	}

	thCode, _ := doHX(t, http.MethodPost, ts.URL+"/ui/thresholds", url.Values{
		"min_permanent":      {"0.79"},
		"min_confidence":     {"0.83"},
		"pin_invariant":      {"0.91"},
		"min_relevance":      {"3.7"},
		"min_rel_confidence": {"0.77"},
	})
	if thCode != http.StatusOK {
		t.Fatalf("POST /ui/thresholds failed: %d", thCode)
	}
	if th := svc.GetThresholds(ctx); th.MinPermanent != 0.79 || th.PinInvariant != 0.91 {
		t.Fatalf("expected updated thresholds (0.79, 0.91), got %+v", th)
	}
	rstCode, _ := doHX(t, http.MethodPost, ts.URL+"/ui/thresholds", url.Values{"reset": {"1"}})
	if rstCode != http.StatusOK {
		t.Fatalf("POST /ui/thresholds reset=1 failed: %d", rstCode)
	}
	if th := svc.GetThresholds(ctx); th != domain.DefaultThresholds() {
		t.Fatalf("expected reset thresholds %+v, got %+v", domain.DefaultThresholds(), th)
	}

	verCode, verHTML := doHX(t, http.MethodPost, ts.URL+"/ui/verify", url.Values{
		"scope": {"project:routes"},
		"plan":  {"라우트 테스트 규칙을 준수해 안전하게 반영한다"},
	})
	if verCode != http.StatusOK || !strings.Contains(verHTML, "PASS") {
		t.Fatalf("POST /ui/verify failed (%d):\n%s", verCode, verHTML)
	}
	prbCode, prbHTML := doHX(t, http.MethodPost, ts.URL+"/ui/probe", url.Values{
		"scope": {"project:routes"},
		"query": {"라우트 테스트"},
	})
	if prbCode != http.StatusOK || !strings.Contains(prbHTML, "라우트 테스트 규칙 통합본") {
		t.Fatalf("POST /ui/probe failed (%d):\n%s", prbCode, prbHTML)
	}

	for _, view := range []string{"gemini", "gguf&discover_hf=1"} {
		gCode, gHTML := doHX(t, http.MethodGet, ts.URL+"/ui/engines?view="+view, nil)
		if gCode != http.StatusOK || len(gHTML) == 0 {
			t.Fatalf("GET /ui/engines?view=%s failed (%d)", view, gCode)
		}
	}

	secretVal := "AIzaSuperSecretValue987654321"
	sCode, sHTML := doHX(t, http.MethodPost, ts.URL+"/ui/engines/save", url.Values{
		"provider":       {"gemini"},
		"gemini_model":   {"gemini-3.5-flash-lite"},
		"gemini_api_key": {secretVal},
	})
	if sCode != http.StatusOK {
		t.Fatalf("POST /ui/engines/save failed (%d)", sCode)
	}
	if strings.Contains(sHTML, secretVal) {
		t.Fatalf("security failure: raw API secret %q echoed back in /ui/engines HTML", secretVal)
	}

	discCode, discHTML := doHX(t, http.MethodPost, ts.URL+"/ui/engines/discover-gemini", url.Values{
		"tier": {"speed"},
	})
	if discCode != http.StatusOK || !strings.Contains(discHTML, "gemini") {
		t.Fatalf("POST /ui/engines/discover-gemini failed (%d)", discCode)
	}

	dlBadCode, _ := doHX(t, http.MethodPost, ts.URL+"/ui/models/download", url.Values{
		"custom_url": {"http://evil.example/insecure.gguf"},
	})
	if dlBadCode != http.StatusBadRequest {
		t.Fatalf("expected non-HTTPS/non-allowlisted custom model URL to return 400, got %d", dlBadCode)
	}
	dlCancelCode, _ := doHX(t, http.MethodPost, ts.URL+"/ui/models/download", url.Values{"action": {"cancel"}})
	if dlCancelCode != http.StatusOK {
		t.Fatalf("POST /ui/models/download cancel -> %d", dlCancelCode)
	}
	srvStopCode, _ := doHX(t, http.MethodPost, ts.URL+"/ui/models/server", url.Values{"action": {"stop"}})
	if srvStopCode != http.StatusOK {
		t.Fatalf("POST /ui/models/server stop -> %d", srvStopCode)
	}
	srvBadCode, _ := doHX(t, http.MethodPost, ts.URL+"/ui/models/server", url.Values{"action": {"invalid"}})
	if srvBadCode != http.StatusBadRequest {
		t.Fatalf("POST /ui/models/server invalid -> expected 400, got %d", srvBadCode)
	}
}

func TestProgressAndGuardrailCleanupJSONCSRF(t *testing.T) {
	ctx := context.Background()
	deps := newTestDeps(t, ":memory:", noopEval{}, "", thttp.DashboardHooks{})

	if _, err := deps.Progress.RecordProgress(ctx, domain.ProgressInput{
		Scope:   "project:csrf",
		Track:   "Track Alpha",
		Summary: "First activity entry for older_than_days=0 cleanup",
		Source:  "agent",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := deps.Progress.RecordProgress(ctx, domain.ProgressInput{
		Scope:   "project:csrf",
		Track:   "Track Beta",
		Summary: "Second activity entry for older_than_days=0 cleanup",
		Source:  "agent",
	}); err != nil {
		t.Fatal(err)
	}

	h := NewHandler(deps)

	getReq := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:18095/api/progress/cleanup", nil)
	getRec := httptest.NewRecorder()
	h.ServeHTTP(getRec, getReq)
	if getRec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("expected 405 for GET /api/progress/cleanup, got %d", getRec.Code)
	}

	plainReq := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:18095/api/progress/cleanup", bytes.NewBufferString(`{"scope":"project:csrf"}`))
	plainReq.Header.Set("Content-Type", "text/plain")
	plainRec := httptest.NewRecorder()
	h.ServeHTTP(plainRec, plainReq)
	if plainRec.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("expected 415 for text/plain POST, got %d", plainRec.Code)
	}

	evilOrigReq := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:18095/api/progress/cleanup", bytes.NewBufferString(`{"scope":"project:csrf","older_than_days":0}`))
	evilOrigReq.Header.Set("Content-Type", "application/json")
	evilOrigReq.Header.Set("Origin", "https://evil.example")
	evilOrigRec := httptest.NewRecorder()
	h.ServeHTTP(evilOrigRec, evilOrigReq)
	if evilOrigRec.Code != http.StatusForbidden {
		t.Fatalf("expected 403 for cross-origin POST, got %d", evilOrigRec.Code)
	}

	okReq := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:18095/api/progress/cleanup", bytes.NewBufferString(`{"scope":"project:csrf","older_than_days":0}`))
	okReq.Header.Set("Content-Type", "application/json")
	okRec := httptest.NewRecorder()
	h.ServeHTTP(okRec, okReq)
	if okRec.Code != http.StatusOK {
		t.Fatalf("expected 200 for valid JSON POST, got %d (%s)", okRec.Code, okRec.Body.String())
	}
	var res struct {
		Deleted int `json:"deleted"`
	}
	if err := json.NewDecoder(okRec.Body).Decode(&res); err != nil {
		t.Fatalf("decode cleanup response: %v", err)
	}
	if res.Deleted != 2 {
		t.Fatalf("expected 2 deleted entries, got %d", res.Deleted)
	}

	ov, err := deps.Progress.GetProgressOverviewWithQuery(ctx, domain.ProgressQuery{Scope: "project:csrf"})
	if err != nil {
		t.Fatal(err)
	}
	if ov.TotalCount != 0 || len(ov.RecentHistory) != 0 {
		t.Fatalf("expected empty activity history after older_than_days=0 cleanup, got %+v", ov)
	}
}

func TestDashboardServerModeAuthRBACAndPersonalIsolation(t *testing.T) {
	d := newTestDeps(t, "", noopMutEval{}, "", thttp.DashboardHooks{})
	d.ServerMode = true
	d.DB = d.Memory.DB()
	h := NewHandler(d)
	host := "team.example.com:18095"

	ctx := context.Background()
	adminIss, _, err := sqlite.EnsureBootstrapAdmin(ctx, d.DB, "admin", "")
	if err != nil {
		t.Fatal(err)
	}
	bobIss, err := sqlite.CreateUser(ctx, d.DB, "bob", domain.RoleMember)
	if err != nil {
		t.Fatal(err)
	}

	{
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "http://"+host+"/", nil)
		req.Host = host
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusFound || rec.Header().Get("Location") != "/login" {
			t.Fatalf("expected 302 -> /login, got %d (%s)", rec.Code, rec.Header().Get("Location"))
		}

		recHX := httptest.NewRecorder()
		reqHX := httptest.NewRequest(http.MethodGet, "http://"+host+"/ui/memories", nil)
		reqHX.Host = host
		reqHX.Header.Set("HX-Request", "true")
		h.ServeHTTP(recHX, reqHX)
		if recHX.Code != http.StatusUnauthorized || recHX.Header().Get("HX-Redirect") != "/login" {
			t.Fatalf("expected HTMX 401 + HX-Redirect, got %d (%s)", recHX.Code, recHX.Header().Get("HX-Redirect"))
		}

		recAPI := httptest.NewRecorder()
		reqAPI := httptest.NewRequest(http.MethodGet, "http://"+host+"/api/records", nil)
		reqAPI.Host = host
		h.ServeHTTP(recAPI, reqAPI)
		if recAPI.Code != http.StatusUnauthorized {
			t.Fatalf("expected /api/records 401, got %d", recAPI.Code)
		}
	}

	{
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "http://"+host+"/login", strings.NewReader("token=bad"))
		req.Host = host
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized || !strings.Contains(rec.Header().Get("Content-Type"), "text/html") {
			t.Fatalf("expected 401 HTML on invalid token login, got %d (%s)", rec.Code, rec.Header().Get("Content-Type"))
		}
	}

	adminCookie := loginSessionCookie(t, h, host, adminIss.PlainToken)

	createForm := url.Values{"account": {"alice"}, "role": {"member"}}.Encode()
	recCreate := httptest.NewRecorder()
	reqCreate := httptest.NewRequest(http.MethodPost, "http://"+host+"/ui/users/create", strings.NewReader(createForm))
	reqCreate.Host = host
	reqCreate.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	reqCreate.Header.Set("HX-Request", "true")
	reqCreate.AddCookie(adminCookie)
	h.ServeHTTP(recCreate, reqCreate)
	if recCreate.Code != http.StatusOK || !strings.Contains(recCreate.Body.String(), "mem_") || !strings.Contains(recCreate.Body.String(), "alice") || !strings.Contains(recCreate.Body.String(), "/mcp") {
		t.Fatalf("admin user create failed (expected token and MCP snippet): %d %s", recCreate.Code, recCreate.Body.String())
	}

	recAdminHome := httptest.NewRecorder()
	reqAdminHome := httptest.NewRequest(http.MethodGet, "http://"+host+"/", nil)
	reqAdminHome.Host = host
	reqAdminHome.AddCookie(adminCookie)
	h.ServeHTTP(recAdminHome, reqAdminHome)
	if recAdminHome.Code != http.StatusOK || !strings.Contains(recAdminHome.Body.String(), "btnOpenUsers") || !strings.Contains(recAdminHome.Body.String(), "tabBtn-engines") {
		t.Fatalf("expected admin dashboard to expose users modal & engines tab")
	}

	recDelLast := httptest.NewRecorder()
	reqDelLast := httptest.NewRequest(http.MethodPost, "http://"+host+"/ui/users/delete", strings.NewReader("account=admin"))
	reqDelLast.Host = host
	reqDelLast.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	reqDelLast.Header.Set("HX-Request", "true")
	reqDelLast.AddCookie(adminCookie)
	h.ServeHTTP(recDelLast, reqDelLast)
	if recDelLast.Code != http.StatusBadRequest {
		t.Fatalf("expected deleting last admin via dashboard to fail 400, got %d", recDelLast.Code)
	}

	recRot := httptest.NewRecorder()
	reqRot := httptest.NewRequest(http.MethodPost, "http://"+host+"/ui/users/rotate", strings.NewReader("account=alice"))
	reqRot.Host = host
	reqRot.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	reqRot.Header.Set("HX-Request", "true")
	reqRot.AddCookie(adminCookie)
	h.ServeHTTP(recRot, reqRot)
	if recRot.Code != http.StatusOK {
		t.Fatalf("rotate alice token failed: %d", recRot.Code)
	}
	aliceUsers, _ := sqlite.ListUsers(ctx, d.DB)
	_ = aliceUsers

	bobCookie := loginSessionCookie(t, h, host, bobIss.PlainToken)
	recBobHome := httptest.NewRecorder()
	reqBobHome := httptest.NewRequest(http.MethodGet, "http://"+host+"/", nil)
	reqBobHome.Host = host
	reqBobHome.AddCookie(bobCookie)
	h.ServeHTTP(recBobHome, reqBobHome)
	bodyBob := recBobHome.Body.String()
	if recBobHome.Code != http.StatusOK || strings.Contains(bodyBob, "tabBtn-engines") || strings.Contains(bodyBob, "tabBtn-calibrate") || strings.Contains(bodyBob, "btnOpenUsers") {
		t.Fatalf("member dashboard should hide admin tabs and user management")
	}

	adminOnlyGets := []string{"/ui/engines", "/ui/calibrate", "/ui/modal/ports", "/ui/modal/users", "/ui/modal/categories"}
	for _, p := range adminOnlyGets {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "http://"+host+p, nil)
		req.Host = host
		req.AddCookie(bobCookie)
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("member GET %s expected 403, got %d", p, rec.Code)
		}
	}
	adminOnlyPosts := []string{"/ui/engines/save", "/ui/thresholds", "/ui/personal-toggle", "/ui/categories/save", "/ui/users/create", "/ui/progress/cleanup", "/ui/guardrail-logs/cleanup"}
	for _, p := range adminOnlyPosts {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "http://"+host+p, strings.NewReader(""))
		req.Host = host
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("HX-Request", "true")
		req.AddCookie(bobCookie)
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("member POST %s expected 403, got %d", p, rec.Code)
		}
	}

	commitForm := url.Values{
		"candidate":   {"BOB_PRIVATE_NOTE_123"},
		"new_scope":   {"personal"},
		"new_account": {"admin"}, // Must be ignored in ServerMode!
	}.Encode()
	recCommit := httptest.NewRecorder()
	reqCommit := httptest.NewRequest(http.MethodPost, "http://"+host+"/ui/memories/commit", strings.NewReader(commitForm))
	reqCommit.Host = host
	reqCommit.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	reqCommit.Header.Set("HX-Request", "true")
	reqCommit.AddCookie(bobCookie)
	h.ServeHTTP(recCommit, reqCommit)
	if recCommit.Code != http.StatusOK || !strings.Contains(recCommit.Body.String(), "BOB_PRIVATE_NOTE_123") {
		t.Fatalf("bob commit failed: %d %s", recCommit.Code, recCommit.Body.String())
	}

	recAdmMem := httptest.NewRecorder()
	reqAdmMem := httptest.NewRequest(http.MethodGet, "http://"+host+"/ui/memories?scope=personal&state=all", nil)
	reqAdmMem.Host = host
	reqAdmMem.AddCookie(adminCookie)
	h.ServeHTTP(recAdmMem, reqAdmMem)
	if strings.Contains(recAdmMem.Body.String(), "BOB_PRIVATE_NOTE_123") {
		t.Fatalf("admin should never see bob personal memory in /ui/memories")
	}

	if _, err := sqlite.RotateUserToken(ctx, d.DB, "bob"); err != nil {
		t.Fatal(err)
	}
	recRevoked := httptest.NewRecorder()
	reqRevoked := httptest.NewRequest(http.MethodGet, "http://"+host+"/api/records", nil)
	reqRevoked.Host = host
	reqRevoked.AddCookie(bobCookie)
	h.ServeHTTP(recRevoked, reqRevoked)
	if recRevoked.Code != http.StatusUnauthorized {
		t.Fatalf("expected rotated member cookie to fail 401 immediately, got %d", recRevoked.Code)
	}

	recRemSrv := httptest.NewRecorder()
	reqRemSrv := httptest.NewRequest(http.MethodGet, "http://"+host+"/ui/modal/remotes", nil)
	reqRemSrv.Host = host
	reqRemSrv.AddCookie(adminCookie)
	h.ServeHTTP(recRemSrv, reqRemSrv)
	if recRemSrv.Code != http.StatusNotFound {
		t.Fatalf("expected /ui/modal/remotes 404 in ServerMode, got %d", recRemSrv.Code)
	}
}

func TestDashboardLocalRemoteConnectionsUI(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("MEMORY_DATA_DIR", tmp)
	dbPath := filepath.Join(tmp, "memories.db")
	d := newTestDeps(t, dbPath, noopMutEval{}, filepath.Join(tmp, "models"), thttp.DashboardHooks{})
	h := NewHandler(d)

	teamSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/daemon/status" {
			http.NotFound(w, r)
			return
		}
		if r.Header.Get("Authorization") != "Bearer mem_valid_team" {
			http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
			return
		}
		_ = json.NewEncoder(w).Encode(thttp.DaemonStatus{
			Running:    true,
			ServerMode: true,
			Account:    "alice",
			Role:       "member",
		})
	}))
	defer teamSrv.Close()

	recHome := httptest.NewRecorder()
	reqHome := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:18095/", nil)
	h.ServeHTTP(recHome, reqHome)
	if recHome.Code != http.StatusOK || !strings.Contains(recHome.Body.String(), "btnOpenRemotes") {
		t.Fatalf("expected solo dashboard to render btnOpenRemotes, code=%d", recHome.Code)
	}

	badForm := url.Values{"mode": {"global"}, "url": {teamSrv.URL}, "token": {"wrong"}}.Encode()
	recBad := httptest.NewRecorder()
	reqBad := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:18095/ui/remotes/connect", strings.NewReader(badForm))
	reqBad.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	reqBad.Header.Set("HX-Request", "true")
	h.ServeHTTP(recBad, reqBad)
	if recBad.Code != http.StatusBadRequest {
		t.Fatalf("expected bad token connect to return 400, got %d", recBad.Code)
	}

	teamRepo := filepath.Join(tmp, "team-project")
	goodForm := url.Values{"mode": {"repo"}, "repo_path": {teamRepo}, "url": {teamSrv.URL}, "token": {"mem_valid_team"}}.Encode()
	recGood := httptest.NewRecorder()
	reqGood := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:18095/ui/remotes/connect", strings.NewReader(goodForm))
	reqGood.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	reqGood.Header.Set("HX-Request", "true")
	h.ServeHTTP(recGood, reqGood)
	bodyGood := recGood.Body.String()
	if recGood.Code != http.StatusOK || !strings.Contains(bodyGood, teamSrv.URL) || !strings.Contains(bodyGood, "@alice") || !strings.Contains(bodyGood, "hx-swap-oob=\"outerHTML\"") {
		t.Fatalf("expected valid connect response with URL/@alice/OOB badge, got %d %s", recGood.Code, bodyGood)
	}
	if strings.Contains(bodyGood, "mem_valid_team") {
		t.Fatalf("plaintext token must not be leaked into HTML modal")
	}
	if got, ok := config.ResolveRemoteTarget(tmp, teamRepo); !ok || got.URL != teamSrv.URL {
		t.Fatalf("expected saved remote binding for %s, got ok=%v target=%+v", teamRepo, ok, got)
	}

	discForm := url.Values{"mode": {"repo"}, "repo_path": {teamRepo}}.Encode()
	recDisc := httptest.NewRecorder()
	reqDisc := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:18095/ui/remotes/disconnect", strings.NewReader(discForm))
	reqDisc.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	reqDisc.Header.Set("HX-Request", "true")
	h.ServeHTTP(recDisc, reqDisc)
	if recDisc.Code != http.StatusOK || strings.Contains(recDisc.Body.String(), teamRepo) {
		t.Fatalf("expected repo removed after disconnect: %d %s", recDisc.Code, recDisc.Body.String())
	}
	if _, ok := config.ResolveRemoteTarget(tmp, teamRepo); ok {
		t.Fatalf("expected remote binding cleared after disconnect")
	}
}

func TestDashboardServerModeTeamReadOnlyConfigRBACAndSharedToPersonalGuard(t *testing.T) {
	d := newTestDeps(t, "", noopMutEval{}, "", thttp.DashboardHooks{})
	d.ServerMode = true
	d.DB = d.Memory.DB()
	h := NewHandler(d)

	ctx := context.Background()
	adminIssue, _, err := sqlite.EnsureBootstrapAdmin(ctx, d.DB, "admin", "")
	if err != nil {
		t.Fatalf("EnsureBootstrapAdmin: %v", err)
	}
	aliceIssue, err := sqlite.CreateUser(ctx, d.DB, "alice", domain.RoleMember)
	if err != nil {
		t.Fatalf("CreateUser alice: %v", err)
	}
	host := "127.0.0.1:18095"
	adminCookie := loginSessionCookie(t, h, host, adminIssue.PlainToken)
	aliceCookie := loginSessionCookie(t, h, host, aliceIssue.PlainToken)

	ctxAdmin := domain.WithAuthPrincipal(ctx, domain.AuthPrincipal{
		Account:       "admin",
		Role:          domain.RoleAdmin,
		ServerMode:    true,
		Authenticated: true,
	})
	if _, err := d.Memory.CommitMemoryFull(ctxAdmin, "SHARED_PROJECT_RULE", "architecture", "", "", "project:teamapp"); err != nil {
		t.Fatalf("seed project memory: %v", err)
	}
	var projMemID int64
	if err := d.DB.QueryRowContext(ctx, `SELECT id FROM memories WHERE content = 'SHARED_PROJECT_RULE' ORDER BY id DESC LIMIT 1`).Scan(&projMemID); err != nil || projMemID <= 0 {
		t.Fatalf("lookup seeded project memory ID: id=%d err=%v", projMemID, err)
	}

	recTeamAlice := httptest.NewRecorder()
	reqTeamAlice := httptest.NewRequest(http.MethodGet, "http://"+host+"/ui/team?team_scope=project:teamapp", nil)
	reqTeamAlice.Host = host
	reqTeamAlice.AddCookie(aliceCookie)
	h.ServeHTTP(recTeamAlice, reqTeamAlice)
	bodyAlice := recTeamAlice.Body.String()
	if recTeamAlice.Code != http.StatusOK || !strings.Contains(bodyAlice, ".memorize/TEAM_RULES.md") ||
		strings.Contains(bodyAlice, `id="btnTeamSync"`) || strings.Contains(bodyAlice, `id="btnTeamToggleAdd"`) || strings.Contains(bodyAlice, `id="teamAuthorForm"`) {
		t.Fatalf("unexpected member /ui/team HTML (%d):\n%s", recTeamAlice.Code, bodyAlice)
	}

	recTeamAdmin := httptest.NewRecorder()
	reqTeamAdmin := httptest.NewRequest(http.MethodGet, "http://"+host+"/ui/team?team_scope=project:teamapp", nil)
	reqTeamAdmin.Host = host
	reqTeamAdmin.AddCookie(adminCookie)
	h.ServeHTTP(recTeamAdmin, reqTeamAdmin)
	bodyAdmin := recTeamAdmin.Body.String()
	if recTeamAdmin.Code != http.StatusOK || !strings.Contains(bodyAdmin, `id="teamAuthorForm"`) ||
		strings.Contains(bodyAdmin, `id="btnTeamSync"`) || strings.Contains(bodyAdmin, `id="btnTeamToggleAdd"`) {
		t.Fatalf("unexpected admin /ui/team HTML (%d):\n%s", recTeamAdmin.Code, bodyAdmin)
	}

	cfgForm := url.Values{"team_scope": {"project:teamapp"}, "author": {"lead"}}.Encode()
	recCfgAlice := httptest.NewRecorder()
	reqCfgAlice := httptest.NewRequest(http.MethodPost, "http://"+host+"/ui/team/config", strings.NewReader(cfgForm))
	reqCfgAlice.Host = host
	reqCfgAlice.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	reqCfgAlice.Header.Set("HX-Request", "true")
	reqCfgAlice.AddCookie(aliceCookie)
	h.ServeHTTP(recCfgAlice, reqCfgAlice)
	if recCfgAlice.Code != http.StatusForbidden {
		t.Fatalf("expected member POST /ui/team/config 403, got %d %s", recCfgAlice.Code, recCfgAlice.Body.String())
	}

	recCfgAdmin := httptest.NewRecorder()
	reqCfgAdmin := httptest.NewRequest(http.MethodPost, "http://"+host+"/ui/team/config", strings.NewReader(cfgForm))
	reqCfgAdmin.Host = host
	reqCfgAdmin.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	reqCfgAdmin.Header.Set("HX-Request", "true")
	reqCfgAdmin.AddCookie(adminCookie)
	h.ServeHTTP(recCfgAdmin, reqCfgAdmin)
	if recCfgAdmin.Code != http.StatusOK || !strings.Contains(recCfgAdmin.Body.String(), "@lead") {
		t.Fatalf("expected admin POST /ui/team/config 200 with @lead, got %d %s", recCfgAdmin.Code, recCfgAdmin.Body.String())
	}

	for _, ep := range []string{"/ui/team/sync", "/ui/modal/team-draft", "/ui/team/publish", "/ui/team/delete"} {
		form := url.Values{"team_scope": {"project:teamapp"}, "rule_key": {"k"}, "raw_rule": {"test rule"}}.Encode()
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "http://"+host+ep, strings.NewReader(form))
		req.Host = host
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("HX-Request", "true")
		req.AddCookie(adminCookie)
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("expected %s rejected (400) in ServerMode, got %d %s", ep, rec.Code, rec.Body.String())
		}
	}

	recDetail := httptest.NewRecorder()
	reqDetail := httptest.NewRequest(http.MethodGet, "http://"+host+"/ui/modal/detail?id="+url.QueryEscape(strings.TrimSpace(strconv.FormatInt(projMemID, 10))), nil)
	reqDetail.Host = host
	reqDetail.AddCookie(aliceCookie)
	h.ServeHTTP(recDetail, reqDetail)
	detailBody := recDetail.Body.String()
	if recDetail.Code != http.StatusOK || strings.Contains(detailBody, `"personal"`) || strings.Contains(detailBody, `"global"`) || strings.Contains(detailBody, "/ui/modal/team-promote") {
		t.Fatalf("expected ServerMode modal_detail to omit personal/global target & promote button (%d):\n%s", recDetail.Code, detailBody)
	}

	for _, privScope := range []string{"personal", "global"} {
		updForm := url.Values{
			"id":      {strconv.FormatInt(projMemID, 10)},
			"scope":   {privScope},
			"content": {"SHARED_PROJECT_RULE"},
		}.Encode()
		recUpd := httptest.NewRecorder()
		reqUpd := httptest.NewRequest(http.MethodPost, "http://"+host+"/ui/memories/update", strings.NewReader(updForm))
		reqUpd.Host = host
		reqUpd.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		reqUpd.Header.Set("HX-Request", "true")
		reqUpd.AddCookie(aliceCookie)
		h.ServeHTTP(recUpd, reqUpd)
		if recUpd.Code == http.StatusOK {
			t.Fatalf("expected POST /ui/memories/update moving shared memory into %s to fail in ServerMode", privScope)
		}
	}

	recConf := httptest.NewRecorder()
	reqConf := httptest.NewRequest(http.MethodPost, "http://"+host+"/ui/team/conflict", strings.NewReader(url.Values{
		"team_scope": {"project:teamapp"},
		"rule_key":   {"sample-rule"},
		"decision":   {"local_override"},
	}.Encode()))
	reqConf.Host = host
	reqConf.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	reqConf.Header.Set("HX-Request", "true")
	reqConf.AddCookie(adminCookie)
	h.ServeHTTP(recConf, reqConf)
	if recConf.Code != http.StatusBadRequest {
		t.Fatalf("expected ServerMode local_override conflict resolution rejected (400), got %d %s", recConf.Code, recConf.Body.String())
	}

	recProg := httptest.NewRecorder()
	reqProg := httptest.NewRequest(http.MethodGet, "http://"+host+"/ui/progress", nil)
	reqProg.Host = host
	reqProg.AddCookie(aliceCookie)
	h.ServeHTTP(recProg, reqProg)
	if recProg.Code != http.StatusOK || strings.Contains(recProg.Body.String(), `id="btnSyncProgress"`) {
		t.Fatalf("expected ServerMode /ui/progress to hide btnSyncProgress (%d):\n%s", recProg.Code, recProg.Body.String())
	}

	recProgSync := httptest.NewRecorder()
	reqProgSync := httptest.NewRequest(http.MethodPost, "http://"+host+"/ui/progress/sync", strings.NewReader("scope=project:teamapp"))
	reqProgSync.Host = host
	reqProgSync.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	reqProgSync.Header.Set("HX-Request", "true")
	reqProgSync.AddCookie(aliceCookie)
	h.ServeHTTP(recProgSync, reqProgSync)
	if recProgSync.Code != http.StatusForbidden {
		t.Fatalf("expected member POST /ui/progress/sync 403 in ServerMode, got %d", recProgSync.Code)
	}

	for i := 1; i <= 10; i++ {
		recBad := httptest.NewRecorder()
		reqBad := httptest.NewRequest(http.MethodPost, "http://"+host+"/login", strings.NewReader("token=wrong"))
		reqBad.Host = host
		reqBad.RemoteAddr = "198.51.100.42:12345"
		reqBad.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		h.ServeHTTP(recBad, reqBad)
		if i < 10 && recBad.Code != http.StatusUnauthorized {
			t.Fatalf("attempt %d expected 401, got %d", i, recBad.Code)
		}
		if i == 10 && recBad.Code != http.StatusTooManyRequests {
			t.Fatalf("attempt 10 expected 429 lockout, got %d", recBad.Code)
		}
	}
	recLockedValid := httptest.NewRecorder()
	reqLockedValid := httptest.NewRequest(http.MethodPost, "http://"+host+"/login", strings.NewReader(url.Values{"token": {adminIssue.PlainToken}}.Encode()))
	reqLockedValid.Host = host
	reqLockedValid.RemoteAddr = "198.51.100.42:12345"
	reqLockedValid.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	h.ServeHTTP(recLockedValid, reqLockedValid)
	if recLockedValid.Code != http.StatusTooManyRequests {
		t.Fatalf("expected locked-out IP to stay 429, got %d", recLockedValid.Code)
	}
	recOtherIP := httptest.NewRecorder()
	reqOtherIP := httptest.NewRequest(http.MethodPost, "http://"+host+"/login", strings.NewReader(url.Values{"token": {adminIssue.PlainToken}}.Encode()))
	reqOtherIP.Host = host
	reqOtherIP.RemoteAddr = "198.51.100.77:12345"
	reqOtherIP.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	h.ServeHTTP(recOtherIP, reqOtherIP)
	if recOtherIP.Code != http.StatusSeeOther {
		t.Fatalf("expected non-throttled IP login 303, got %d", recOtherIP.Code)
	}
}

func TestDashboardMultiAccountSwitcherAndBundleImportModal(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "switcher.db")
	d := newTestDeps(t, dbPath, noopEval{}, "", thttp.DashboardHooks{})
	d.ServerMode = true
	d.DB = d.Memory.DB()
	admIss, _, err := sqlite.EnsureBootstrapAdmin(ctx, d.DB, "home", "")
	if err != nil {
		t.Fatal(err)
	}
	bobIss, err := sqlite.CreateUser(ctx, d.DB, "bob", domain.RoleMember)
	if err != nil {
		t.Fatal(err)
	}
	h := NewHandler(d)
	host := "home.local:18095"

	findCookie := func(res *http.Response, name string) *http.Cookie {
		for _, c := range res.Cookies() {
			if c.Name == name {
				return c
			}
		}
		return nil
	}

	// Login as @home -> must set both memorize_session and memorize_accounts cookies.
	recLogin := httptest.NewRecorder()
	reqLogin := httptest.NewRequest(http.MethodPost, "http://"+host+"/login", strings.NewReader(url.Values{"token": {admIss.PlainToken}}.Encode()))
	reqLogin.Host = host
	reqLogin.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	h.ServeHTTP(recLogin, reqLogin)
	if recLogin.Code != http.StatusSeeOther {
		t.Fatalf("login failed: %d", recLogin.Code)
	}
	sessCookie := findCookie(recLogin.Result(), domain.SessionCookieName)
	accCookie := findCookie(recLogin.Result(), domain.AccountsCookieName)
	if sessCookie == nil || accCookie == nil {
		t.Fatalf("expected both session and accounts cookies on login")
	}

	// Add @bob via /ui/accounts/add -> switches active session to @bob and keeps both in accounts cookie.
	recAdd := httptest.NewRecorder()
	reqAdd := httptest.NewRequest(http.MethodPost, "http://"+host+"/ui/accounts/add", strings.NewReader(url.Values{"token": {bobIss.PlainToken}}.Encode()))
	reqAdd.Host = host
	reqAdd.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	reqAdd.Header.Set("HX-Request", "true")
	reqAdd.AddCookie(sessCookie)
	reqAdd.AddCookie(accCookie)
	h.ServeHTTP(recAdd, reqAdd)
	if recAdd.Code != http.StatusOK || recAdd.Header().Get("HX-Refresh") != "true" {
		t.Fatalf("add account bob failed: %d hdr=%v body=%s", recAdd.Code, recAdd.Header(), recAdd.Body.String())
	}
	sessCookie = findCookie(recAdd.Result(), domain.SessionCookieName)
	accCookie = findCookie(recAdd.Result(), domain.AccountsCookieName)

	// Open switcher modal as @bob -> must list both @home and @bob.
	recModal := httptest.NewRecorder()
	reqModal := httptest.NewRequest(http.MethodGet, "http://"+host+"/ui/modal/accounts", nil)
	reqModal.Host = host
	reqModal.AddCookie(sessCookie)
	reqModal.AddCookie(accCookie)
	h.ServeHTTP(recModal, reqModal)
	if recModal.Code != http.StatusOK || !strings.Contains(recModal.Body.String(), "@home") || !strings.Contains(recModal.Body.String(), "@bob") {
		t.Fatalf("modal_accounts missing accounts: %d %s", recModal.Code, recModal.Body.String())
	}

	// Switch back to @home with 1 click.
	recSw := httptest.NewRecorder()
	reqSw := httptest.NewRequest(http.MethodPost, "http://"+host+"/ui/accounts/switch", strings.NewReader("account=home"))
	reqSw.Host = host
	reqSw.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	reqSw.Header.Set("HX-Request", "true")
	reqSw.AddCookie(sessCookie)
	reqSw.AddCookie(accCookie)
	h.ServeHTTP(recSw, reqSw)
	if recSw.Code != http.StatusOK || recSw.Header().Get("HX-Refresh") != "true" {
		t.Fatalf("switch back to home failed: %d %s", recSw.Code, recSw.Body.String())
	}
	sessCookie = findCookie(recSw.Result(), domain.SessionCookieName)
	if p, ok := sqlite.AuthenticateSessionCookie(ctx, d.DB, sessCookie.Value); !ok || p.Account != "home" {
		t.Fatalf("expected active session switched to home, got %+v ok=%v", p, ok)
	}

	// Import modal preview + apply + export as @home.
	bundlePayload := `{"format":"memorize.bundle.v1","items":[{"scope":"global","category":"convention","content":"Dashboard imported rule","pinned":true}]}`
	recPrev := httptest.NewRecorder()
	reqPrev := httptest.NewRequest(http.MethodPost, "http://"+host+"/ui/memories/import/preview", strings.NewReader(url.Values{
		"bundle_json": {bundlePayload},
		"mode":        {"exact"},
	}.Encode()))
	reqPrev.Host = host
	reqPrev.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	reqPrev.Header.Set("HX-Request", "true")
	reqPrev.AddCookie(sessCookie)
	h.ServeHTTP(recPrev, reqPrev)
	if recPrev.Code != http.StatusOK || !strings.Contains(recPrev.Body.String(), "Dashboard imported rule") {
		t.Fatalf("import preview failed: %d %s", recPrev.Code, recPrev.Body.String())
	}

	recApply := httptest.NewRecorder()
	reqApply := httptest.NewRequest(http.MethodPost, "http://"+host+"/ui/memories/import/apply", strings.NewReader(url.Values{
		"bundle_json": {bundlePayload},
		"mode":        {"exact"},
	}.Encode()))
	reqApply.Host = host
	reqApply.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	reqApply.Header.Set("HX-Request", "true")
	reqApply.AddCookie(sessCookie)
	h.ServeHTTP(recApply, reqApply)
	if recApply.Code != http.StatusOK || !strings.Contains(recApply.Body.String(), "Dashboard imported rule") {
		t.Fatalf("import apply failed: %d %s", recApply.Code, recApply.Body.String())
	}

	recExp := httptest.NewRecorder()
	reqExp := httptest.NewRequest(http.MethodGet, "http://"+host+"/ui/memories/export?scope=global", nil)
	reqExp.Host = host
	reqExp.AddCookie(sessCookie)
	h.ServeHTTP(recExp, reqExp)
	if recExp.Code != http.StatusOK || !strings.Contains(recExp.Body.String(), "Dashboard imported rule") {
		t.Fatalf("dashboard export failed: %d %s", recExp.Code, recExp.Body.String())
	}

	// Logout must clear BOTH session and accounts cookies.
	recOut := httptest.NewRecorder()
	reqOut := httptest.NewRequest(http.MethodPost, "http://"+host+"/logout", nil)
	reqOut.Host = host
	reqOut.Header.Set("HX-Request", "true")
	reqOut.AddCookie(sessCookie)
	reqOut.AddCookie(accCookie)
	h.ServeHTTP(recOut, reqOut)
	clearedSess := findCookie(recOut.Result(), domain.SessionCookieName)
	clearedAcc := findCookie(recOut.Result(), domain.AccountsCookieName)
	if clearedSess == nil || clearedSess.MaxAge >= 0 || clearedAcc == nil || clearedAcc.MaxAge >= 0 {
		t.Fatalf("logout must clear both session and accounts cookies: sess=%+v acc=%+v", clearedSess, clearedAcc)
	}
}
