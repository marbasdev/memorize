package dashboard_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"memorize/internal/domain"
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
	if recCreate.Code != http.StatusOK || !strings.Contains(recCreate.Body.String(), "mem_") || !strings.Contains(recCreate.Body.String(), "alice") {
		t.Fatalf("admin user create failed: %d %s", recCreate.Code, recCreate.Body.String())
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
}
