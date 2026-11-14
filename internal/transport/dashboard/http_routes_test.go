package dashboard_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"memorize/internal/domain"
	"memorize/internal/transport/dashboard"
	thttp "memorize/internal/transport/http"
)

func TestDashboardHTTPRoutesMutationsEnginesAndCSRF(t *testing.T) {
	ctx := context.Background()
	deps := newTestDeps(t, ":memory:", noopEval{}, "", thttp.DashboardHooks{})
	svc := deps.Memory

	h := dashboard.NewHandler(deps)
	ts := httptest.NewServer(h)
	defer ts.Close()

	// 1. /ui/personal-toggle persists include_personal_in_project
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

	// 2. /ui/memories/commit + /ui/memories/update + /ui/modal/detail + /ui/memories/{forget,undo,purge}
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

	// 3. /ui/consolidate/preview and /ui/consolidate/execute
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

	// 4. /ui/thresholds (custom save + reset=1) + /ui/verify + /ui/probe
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

	// 5. Engines views, secret masking, and model server/download validation
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
		Track:   "Active Track",
		Status:  domain.ProgressStatusInProgress,
		Summary: "Should not be deleted by completed cleanup",
		Source:  "agent",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := deps.Progress.RecordProgress(ctx, domain.ProgressInput{
		Scope:   "project:csrf",
		Track:   "Done Track",
		Status:  domain.ProgressStatusDone,
		Summary: "Eligible for older_than_days=0 cleanup",
		Source:  "agent",
	}); err != nil {
		t.Fatal(err)
	}

	h := dashboard.NewHandler(deps)

	// 405 Method Not Allowed for GET
	getReq := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:18095/api/progress/cleanup", nil)
	getRec := httptest.NewRecorder()
	h.ServeHTTP(getRec, getReq)
	if getRec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("expected 405 for GET /api/progress/cleanup, got %d", getRec.Code)
	}

	// 415 Unsupported Media Type for text/plain POST
	plainReq := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:18095/api/progress/cleanup", bytes.NewBufferString(`{"scope":"project:csrf"}`))
	plainReq.Header.Set("Content-Type", "text/plain")
	plainRec := httptest.NewRecorder()
	h.ServeHTTP(plainRec, plainReq)
	if plainRec.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("expected 415 for text/plain POST, got %d", plainRec.Code)
	}

	// 403 Forbidden for cross-site Origin / Sec-Fetch-Site / non-loopback Host
	evilOrigReq := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:18095/api/progress/cleanup", bytes.NewBufferString(`{"scope":"project:csrf","older_than_days":0}`))
	evilOrigReq.Header.Set("Content-Type", "application/json")
	evilOrigReq.Header.Set("Origin", "https://evil.example")
	evilOrigRec := httptest.NewRecorder()
	h.ServeHTTP(evilOrigRec, evilOrigReq)
	if evilOrigRec.Code != http.StatusForbidden {
		t.Fatalf("expected 403 for cross-origin POST, got %d", evilOrigRec.Code)
	}

	// 200 OK for valid loopback JSON POST
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
	if res.Deleted != 1 {
		t.Fatalf("expected 1 deleted completed entry, got %d", res.Deleted)
	}

	ov, err := deps.Progress.GetProgressOverviewWithQuery(ctx, domain.ProgressQuery{Scope: "project:csrf"})
	if err != nil {
		t.Fatal(err)
	}
	if len(ov.ActiveTracks) != 1 || ov.StatusCounts[domain.ProgressStatusDone] != 0 {
		t.Fatalf("expected active track preserved and done count 0, got %+v", ov)
	}
}
