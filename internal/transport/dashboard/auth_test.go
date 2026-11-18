package dashboard_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
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
