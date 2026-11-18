package dashboard_test

import (
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
	"memorize/internal/transport/dashboard"
	thttp "memorize/internal/transport/http"
)

type noopMutEval struct{ noopEval }

func (noopMutEval) EvaluateMutation(_ context.Context, _ string, _ string, _ []domain.Fact) (domain.MutationDecision, error) {
	return domain.MutationDecision{Action: "INSERT", Category: "convention", IsPermanent: 0.9, Confidence: 0.9}, nil
}

func loginSessionCookie(t *testing.T, h http.Handler, host, token string) *http.Cookie {
	t.Helper()
	form := url.Values{"token": {token}}.Encode()
	req := httptest.NewRequest(http.MethodPost, "http://"+host+"/login", strings.NewReader(form))
	req.Host = host
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("POST /login expected 303, got %d (%s)", rec.Code, rec.Body.String())
	}
	for _, c := range rec.Result().Cookies() {
		if c.Name == domain.SessionCookieName {
			return c
		}
	}
	t.Fatalf("missing session cookie on /login response")
	return nil
}

func TestDashboardServerModeAuthRBACAndPersonalIsolation(t *testing.T) {
	d := newTestDeps(t, "", noopMutEval{}, "", thttp.DashboardHooks{})
	d.ServerMode = true
	d.DB = d.Memory.DB()
	h := dashboard.NewHandler(d)
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

	// Unauthenticated GET / redirects to /login; HTMX gets 401 + HX-Redirect; JSON API gets 401.
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

	// Invalid login returns 401 HTML.
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

	// Admin creates member Alice via /ui/users/create and receives 1-time plaintext token.
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

	// Admin sees Accounts button and admin tabs.
	recAdminHome := httptest.NewRecorder()
	reqAdminHome := httptest.NewRequest(http.MethodGet, "http://"+host+"/", nil)
	reqAdminHome.Host = host
	reqAdminHome.AddCookie(adminCookie)
	h.ServeHTTP(recAdminHome, reqAdminHome)
	if recAdminHome.Code != http.StatusOK || !strings.Contains(recAdminHome.Body.String(), "btnOpenUsers") || !strings.Contains(recAdminHome.Body.String(), "tabBtn-engines") {
		t.Fatalf("expected admin dashboard to expose users modal & engines tab")
	}

	// Cannot delete last active admin via /ui/users/delete.
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

	// Rotate alice token via admin UI to get alice's live plaintext token cleanly.
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

	// Bob signs in as member: hidden admin UI & 403 on all admin routes.
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

	// Bob commits a personal memory via dashboard UI; neither admin nor anyone else sees it.
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

	// Rotating bob's token invalidates bob's cookie immediately.
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

	// /ui/modal/remotes is disabled in ServerMode (returns 404).
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
	h := dashboard.NewHandler(d)

	// Start a mock verified team server for /api/daemon/status Bearer checks.
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

	// Solo dashboard shows Team Server button (#btnOpenRemotes).
	recHome := httptest.NewRecorder()
	reqHome := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:18095/", nil)
	h.ServeHTTP(recHome, reqHome)
	if recHome.Code != http.StatusOK || !strings.Contains(recHome.Body.String(), "btnOpenRemotes") {
		t.Fatalf("expected solo dashboard to render btnOpenRemotes, code=%d", recHome.Code)
	}

	// Reject bad token on /ui/remotes/connect cleanly (400).
	badForm := url.Values{"mode": {"global"}, "url": {teamSrv.URL}, "token": {"wrong"}}.Encode()
	recBad := httptest.NewRecorder()
	reqBad := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:18095/ui/remotes/connect", strings.NewReader(badForm))
	reqBad.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	reqBad.Header.Set("HX-Request", "true")
	h.ServeHTTP(recBad, reqBad)
	if recBad.Code != http.StatusBadRequest {
		t.Fatalf("expected bad token connect to return 400, got %d", recBad.Code)
	}

	// Valid repo binding succeeds, omits plaintext token in HTML, updates OOB nav badge, and saves remotes.json.
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

	// Disconnect removes binding and updates OOB badge back to 0.
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
	h := dashboard.NewHandler(d)

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

	// Seed a shared project memory via admin context so /ui/team has a project scope and modal_detail can inspect it.
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

	// GET /ui/team as member renders ServerMode read-only notice, hides host-git buttons & #teamAuthorForm.
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

	// GET /ui/team as admin includes #teamAuthorForm while still hiding server-local git buttons.
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

	// POST /ui/team/config rejects member (403) and accepts admin (200).
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

	// Server-local git team rule mutation endpoints reject requests in ServerMode cleanly (400).
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

	// GET /ui/modal/detail for shared project memory omits "personal" scope option & promote button in ServerMode,
	// and POST /ui/memories/update moving shared project memory into personal fails.
	recDetail := httptest.NewRecorder()
	reqDetail := httptest.NewRequest(http.MethodGet, "http://"+host+"/ui/modal/detail?id="+url.QueryEscape(strings.TrimSpace(strconv.FormatInt(projMemID, 10))), nil)
	reqDetail.Host = host
	reqDetail.AddCookie(aliceCookie)
	h.ServeHTTP(recDetail, reqDetail)
	detailBody := recDetail.Body.String()
	if recDetail.Code != http.StatusOK || strings.Contains(detailBody, `"personal"`) || strings.Contains(detailBody, "/ui/modal/team-promote") {
		t.Fatalf("expected ServerMode modal_detail to omit personal target & promote button (%d):\n%s", recDetail.Code, detailBody)
	}

	updForm := url.Values{
		"id":      {strconv.FormatInt(projMemID, 10)},
		"scope":   {"personal"},
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
		t.Fatalf("expected POST /ui/memories/update moving shared memory into personal to fail in ServerMode")
	}
}
