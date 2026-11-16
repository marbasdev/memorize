package dashboard_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"

	"memorize/internal/domain"
	"memorize/internal/modules/memory"
	"memorize/internal/transport/dashboard"
	thttp "memorize/internal/transport/http"
)

func doHTMXReq(t *testing.T, h http.Handler, method, target string, form url.Values, withHX bool) *httptest.ResponseRecorder {
	t.Helper()
	var body io.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	}
	req := httptest.NewRequest(method, "http://127.0.0.1:18095"+target, body)
	if method == http.MethodPost {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("Origin", "http://127.0.0.1:18095")
	}
	if withHX {
		req.Header.Set("HX-Request", "true")
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestDashboardSyncModalPeerCRUDAndValidation(t *testing.T) {
	t.Cleanup(domain.SetMemorySyncEnabledForTest(true))
	tmp := t.TempDir()
	deps := newTestDeps(t, filepath.Join(tmp, "dash.db"), noopEval{}, "", thttp.DashboardHooks{})
	h := dashboard.NewHandler(deps)

	// Main page includes Sync button
	homeRec := doHTMXReq(t, h, http.MethodGet, "/", nil, false)
	if homeRec.Code != http.StatusOK || !strings.Contains(homeRec.Body.String(), `id="btnOpenSync"`) {
		t.Fatalf("expected Sync button on home page, code=%d", homeRec.Code)
	}

	// CSRF / Origin / HTMX header enforcement on sync routes
	evilGet := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:18095/ui/modal/sync", nil)
	evilGet.Header.Set("Origin", "https://evil.example")
	evilRec := httptest.NewRecorder()
	h.ServeHTTP(evilRec, evilGet)
	if evilRec.Code != http.StatusForbidden {
		t.Fatalf("expected 403 for cross-origin GET /ui/modal/sync, got %d", evilRec.Code)
	}
	for _, ep := range []string{"/ui/sync/peer/save", "/ui/sync/peer/delete", "/ui/sync/test", "/ui/sync/run"} {
		if unauthPost := doHTMXReq(t, h, http.MethodPost, ep, url.Values{"ssh_target": {"dev@box"}}, false); unauthPost.Code != http.StatusForbidden {
			t.Fatalf("expected 403 for non-HTMX POST %s, got %d", ep, unauthPost.Code)
		}
	}

	// Open modal
	modalRec := doHTMXReq(t, h, http.MethodGet, "/ui/modal/sync", nil, true)
	if modalRec.Code != http.StatusOK || !strings.Contains(modalRec.Body.String(), `id="syncDialog"`) {
		t.Fatalf("expected syncDialog HTML, code=%d body=%s", modalRec.Code, modalRec.Body.String())
	}

	// Reject dangerous flag-injection ssh_target
	badSave := doHTMXReq(t, h, http.MethodPost, "/ui/sync/peer/save", url.Values{
		"ssh_target": {"-oProxyCommand=sh"},
		"scope_mode": {"all"},
	}, true)
	if badSave.Code != http.StatusOK || !strings.Contains(badSave.Body.String(), "setup-alert") {
		t.Fatalf("expected inline validation error alert, code=%d", badSave.Code)
	}
	if peers, _ := deps.Memory.ListSyncPeers(context.Background()); len(peers) != 0 {
		t.Fatalf("malicious ssh_target must not be saved, got %+v", peers)
	}

	// Valid peer save with selected scopes
	form := url.Values{
		"peer_id":      {"mac-mini"},
		"ssh_target":   {"dev@mac-mini.local"},
		"remote_bin":   {"/usr/local/bin/memorize"},
		"device_label": {"Home Mini"},
		"scope_mode":   {"selected"},
		"scopes":       {"global", "project:demo"},
	}
	saveRec := doHTMXReq(t, h, http.MethodPost, "/ui/sync/peer/save", form, true)
	if saveRec.Code != http.StatusOK || !strings.Contains(saveRec.Body.String(), "mac-mini") || !strings.Contains(saveRec.Body.String(), "project:demo") {
		t.Fatalf("unexpected save response: code=%d body=%s", saveRec.Code, saveRec.Body.String())
	}
	peers, err := deps.Memory.ListSyncPeers(context.Background())
	if err != nil || len(peers) != 1 || peers[0].ScopeMode != domain.SyncScopeModeSelected || len(peers[0].Scopes) != 2 {
		t.Fatalf("unexpected saved peers in DB: %+v err=%v", peers, err)
	}

	// Delete peer
	delRec := doHTMXReq(t, h, http.MethodPost, "/ui/sync/peer/delete", url.Values{"peer_id": {"mac-mini"}}, true)
	if delRec.Code != http.StatusOK {
		t.Fatalf("delete peer failed: code=%d", delRec.Code)
	}
	if after, _ := deps.Memory.ListSyncPeers(context.Background()); len(after) != 0 {
		t.Fatalf("expected 0 peers after delete, got %+v", after)
	}
}

func TestDashboardSyncConflictBannerAndResolution(t *testing.T) {
	t.Cleanup(domain.SetMemorySyncEnabledForTest(true))
	tmp := t.TempDir()
	deps := newTestDeps(t, filepath.Join(tmp, "local.db"), noopEval{}, "", thttp.DashboardHooks{})
	remoteSvc, err := memory.Open(filepath.Join(tmp, "remote.db"), noopEval{})
	if err != nil {
		t.Fatal(err)
	}
	defer remoteSvc.Close()

	ctx := domain.WithAdminBypass(context.Background())
	if _, err := deps.Memory.CommitMemory(ctx, "Initial shared fact", "arch", "global"); err != nil {
		t.Fatal(err)
	}
	peer := domain.SyncPeer{PeerID: "remote", SSHTarget: "remote.local", ScopeMode: domain.SyncScopeModeAll}
	syncOnce := func() {
		c2sR, c2sW := io.Pipe()
		s2cR, s2cW := io.Pipe()
		errCh := make(chan error, 1)
		go func() {
			errCh <- memory.ServeSyncStream(ctx, remoteSvc, c2sR, s2cW)
			_ = c2sR.Close()
			_ = s2cW.Close()
		}()
		rw := struct {
			io.Reader
			io.Writer
		}{Reader: s2cR, Writer: c2sW}
		if _, err := deps.Memory.RunSyncStream(ctx, peer, rw); err != nil {
			t.Fatalf("RunSyncStream: %v", err)
		}
		_ = c2sW.Close()
		_ = s2cR.Close()
		if serr := <-errCh; serr != nil {
			t.Fatalf("ServeSyncStream: %v", serr)
		}
	}
	syncOnce()

	// Edit on both sides concurrently
	pLocal, _ := deps.Memory.QueryRecordsPage(ctx, domain.RecordQuery{Scope: "global", State: "active", Page: 1, PageSize: 10})
	pRemote, _ := remoteSvc.QueryRecordsPage(ctx, domain.RecordQuery{Scope: "global", State: "active", Page: 1, PageSize: 10})
	if err := deps.Memory.UpdateMemory(ctx, pLocal.Records[0].ID, "arch", "Local dashboard version", false); err != nil {
		t.Fatal(err)
	}
	if err := remoteSvc.UpdateMemory(ctx, pRemote.Records[0].ID, "arch", "Remote dashboard version", false); err != nil {
		t.Fatal(err)
	}
	syncOnce()

	h := dashboard.NewHandler(deps)

	// Memories UI renders conflict banner with both versions
	memRec := doHTMXReq(t, h, http.MethodGet, "/ui/memories", nil, true)
	body := memRec.Body.String()
	if memRec.Code != http.StatusOK || !strings.Contains(body, `id="syncConflictsBanner"`) ||
		!strings.Contains(body, "Local dashboard version") || !strings.Contains(body, "Remote dashboard version") {
		t.Fatalf("expected conflict banner in memories view, code=%d", memRec.Code)
	}

	conflicts, _ := deps.Memory.ListSyncConflicts(ctx)
	if len(conflicts) != 1 {
		t.Fatalf("expected 1 sync conflict, got %d", len(conflicts))
	}

	// Resolve from banner using use_remote
	if unauth := doHTMXReq(t, h, http.MethodPost, "/ui/sync/conflict/resolve", url.Values{
		"sync_id":  {conflicts[0].SyncID},
		"decision": {"use_remote"},
	}, false); unauth.Code != http.StatusForbidden {
		t.Fatalf("expected 403 without HX-Request, got %d", unauth.Code)
	}

	resRec := doHTMXReq(t, h, http.MethodPost, "/ui/sync/conflict/resolve", url.Values{
		"sync_id":  {conflicts[0].SyncID},
		"decision": {"use_remote"},
	}, true)
	if resRec.Code != http.StatusOK {
		t.Fatalf("conflict resolve failed: code=%d body=%s", resRec.Code, resRec.Body.String())
	}
	if strings.Contains(resRec.Body.String(), `id="syncConflictsBanner"`) {
		t.Fatalf("expected syncConflictsBanner cleared after resolution")
	}
	if !strings.Contains(resRec.Body.String(), "Remote dashboard version") {
		t.Fatalf("expected resolved memory content in updated table")
	}
	if deps.Memory.CountSyncConflicts(ctx) != 0 {
		t.Fatalf("expected 0 remaining conflicts")
	}

	// Test resolving from inside modal returns updated modal body cleanly too.
	pLocal2, _ := deps.Memory.QueryRecordsPage(ctx, domain.RecordQuery{Scope: "global", State: "active", Page: 1, PageSize: 10})
	pRemote2, _ := remoteSvc.QueryRecordsPage(ctx, domain.RecordQuery{Scope: "global", State: "active", Page: 1, PageSize: 10})
	_ = deps.Memory.UpdateMemory(ctx, pLocal2.Records[0].ID, "arch", "Diverged A2", false)
	_ = remoteSvc.UpdateMemory(ctx, pRemote2.Records[0].ID, "arch", "Diverged B2", false)
	syncOnce()
	syncOnce() // pull/push conflict triggered on divergent edits
	conflicts2, _ := deps.Memory.ListSyncConflicts(ctx)
	if len(conflicts2) != 1 {
		t.Fatalf("expected 1 new conflict for modal resolve test, got %d", len(conflicts2))
	}
	modalResolve := doHTMXReq(t, h, http.MethodPost, "/ui/sync/conflict/resolve", url.Values{
		"sync_id":  {conflicts2[0].SyncID},
		"decision": {"keep_both"},
		"from":     {"modal"},
	}, true)
	if modalResolve.Code != http.StatusOK || strings.Contains(modalResolve.Body.String(), "Diverged A2") && strings.Contains(modalResolve.Body.String(), "sync_btn_keep_both") {
		t.Fatalf("expected conflict resolved out of modal body, code=%d", modalResolve.Code)
	}
	if deps.Memory.CountSyncConflicts(ctx) != 0 {
		t.Fatalf("expected 0 remaining conflicts after modal keep_both resolve")
	}
}

func TestDashboardSyncHiddenAndBlockedWhenDisabled(t *testing.T) {
	if domain.IsMemorySyncEnabled() {
		t.Fatalf("expected IsMemorySyncEnabled() == false by default")
	}
	tmp := t.TempDir()
	deps := newTestDeps(t, filepath.Join(tmp, "dash-disabled.db"), noopEval{}, "", thttp.DashboardHooks{})
	h := dashboard.NewHandler(deps)

	homeRec := doHTMXReq(t, h, http.MethodGet, "/", nil, false)
	body := homeRec.Body.String()
	if homeRec.Code != http.StatusOK || strings.Contains(body, `id="btnOpenSync"`) || strings.Contains(body, `id="syncConflictsBanner"`) {
		t.Fatalf("expected sync controls hidden when disabled, code=%d", homeRec.Code)
	}

	modalRec := doHTMXReq(t, h, http.MethodGet, "/ui/modal/sync", nil, true)
	if modalRec.Code != http.StatusForbidden {
		t.Fatalf("expected 403 for GET /ui/modal/sync when disabled, got %d", modalRec.Code)
	}
	for _, ep := range []string{"/ui/sync/peer/save", "/ui/sync/peer/delete", "/ui/sync/test", "/ui/sync/run", "/ui/sync/conflict/resolve"} {
		if res := doHTMXReq(t, h, http.MethodPost, ep, url.Values{"ssh_target": {"dev@box"}}, true); res.Code != http.StatusForbidden {
			t.Fatalf("expected 403 for POST %s when disabled, got %d", ep, res.Code)
		}
	}
}
