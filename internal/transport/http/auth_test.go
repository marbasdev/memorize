package http_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"memorize/internal/domain"
	"memorize/internal/infra/sqlite"
	"memorize/internal/modules/guardrail"
	"memorize/internal/modules/memory"
	"memorize/internal/modules/progress"
	thttp "memorize/internal/transport/http"
	transportmcp "memorize/internal/transport/mcp"
)

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
	return domain.MutationDecision{Action: "INSERT", Category: "convention", IsPermanent: 0.9, Confidence: 0.9}, nil
}
func (noopEval) VerifyPlan(_ context.Context, _ string, _ []domain.Fact) ([]domain.PlanViolation, error) {
	return nil, nil
}

func TestIsAllowedOriginModes(t *testing.T) {
	// Local solo mode blocks external host headers.
	reqExt := httptest.NewRequest(http.MethodGet, "http://team.example.com:18095/", nil)
	reqExt.Host = "team.example.com:18095"
	if thttp.IsAllowedOrigin(reqExt, false) {
		t.Fatalf("expected solo mode to reject non-loopback host")
	}
	if !thttp.IsAllowedOrigin(reqExt, true) {
		t.Fatalf("expected server mode to accept non-loopback host without cross-site Origin")
	}

	// Cross-site Sec-Fetch-Site blocked in both modes.
	reqCross := httptest.NewRequest(http.MethodPost, "http://team.example.com:18095/ui/test", nil)
	reqCross.Host = "team.example.com:18095"
	reqCross.Header.Set("Sec-Fetch-Site", "cross-site")
	if thttp.IsAllowedOrigin(reqCross, true) {
		t.Fatalf("expected cross-site Sec-Fetch-Site blocked in server mode")
	}

	// Matching Origin or X-Forwarded-Host allowed; mismatched Origin blocked.
	reqSameOrig := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:18095/login", nil)
	reqSameOrig.Host = "127.0.0.1:18095"
	reqSameOrig.Header.Set("Origin", "https://team.company.com")
	if thttp.IsAllowedOrigin(reqSameOrig, true) {
		t.Fatalf("expected mismatched Origin rejected without X-Forwarded-Host")
	}
	reqSameOrig.Header.Set("X-Forwarded-Host", "team.company.com")
	if !thttp.IsAllowedOrigin(reqSameOrig, true) {
		t.Fatalf("expected matching X-Forwarded-Host accepted in server mode")
	}
}

func TestDaemonMuxServerAuthAndAntiSpoofing(t *testing.T) {
	ctx := context.Background()
	mem, err := memory.Open(":memory:", noopEval{})
	if err != nil {
		t.Fatalf("memory.Open: %v", err)
	}
	t.Cleanup(func() { _ = mem.Close() })
	prog := progress.New(mem)
	gr := guardrail.New(mem, prog)

	adminIss, _, err := sqlite.EnsureBootstrapAdmin(ctx, mem.DB(), "hostadmin", "")
	if err != nil {
		t.Fatal(err)
	}
	aliceIss, err := sqlite.CreateUser(ctx, mem.DB(), "alice", domain.RoleMember)
	if err != nil {
		t.Fatal(err)
	}

	mcpSrv := transportmcp.NewServer(transportmcp.Deps{
		Version:   "test",
		Memory:    mem,
		Progress:  prog,
		Guardrail: gr,
	})

	h := thttp.NewDaemonMux(thttp.DaemonDeps{
		ServerMode: true,
		DB:         mem.DB(),
		Memory:     mem,
		Guardrail:  gr,
		MCPServer:  mcpSrv,
		BuildStatusFn: func() thttp.DaemonStatus {
			return thttp.DaemonStatus{
				Running:    true,
				PID:        999,
				Version:    "test",
				ListenAddr: "0.0.0.0:18095",
				DBPath:     "/secret/server/memories.db",
				LogPath:    "/secret/server/daemon.log",
				Provider:   "mock",
			}
		},
	})

	// Unauthenticated requests fail with 401.
	for _, path := range []string{"/api/daemon/status", "/mcp"} {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "http://team.local:18095"+path, nil)
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("GET %s expected 401, got %d", path, rec.Code)
		}
	}

	// Member status redacts server host paths; admin status preserves them.
	recMemSt := httptest.NewRecorder()
	reqMemSt := httptest.NewRequest(http.MethodGet, "http://team.local:18095/api/daemon/status", nil)
	reqMemSt.Header.Set("Authorization", "Bearer "+aliceIss.PlainToken)
	h.ServeHTTP(recMemSt, reqMemSt)
	if recMemSt.Code != http.StatusOK {
		t.Fatalf("member status expected 200, got %d", recMemSt.Code)
	}
	var stAlice thttp.DaemonStatus
	_ = json.NewDecoder(recMemSt.Body).Decode(&stAlice)
	if !stAlice.ServerMode || stAlice.Account != "alice" || stAlice.DBPath != "" || stAlice.LogPath != "" {
		t.Fatalf("expected member status redacted with account=alice, got %+v", stAlice)
	}

	recAdmSt := httptest.NewRecorder()
	reqAdmSt := httptest.NewRequest(http.MethodGet, "http://team.local:18095/api/daemon/status", nil)
	reqAdmSt.Header.Set("Authorization", "Bearer "+adminIss.PlainToken)
	h.ServeHTTP(recAdmSt, reqAdmSt)
	var stAdm thttp.DaemonStatus
	_ = json.NewDecoder(recAdmSt.Body).Decode(&stAdm)
	if stAdm.DBPath == "" || stAdm.Account != "hostadmin" {
		t.Fatalf("expected admin status to keep DBPath & account, got %+v", stAdm)
	}

	// Member blocked from /api/daemon/port, /restart, /stop (403).
	for _, path := range []string{"/api/daemon/port", "/api/daemon/restart", "/api/daemon/stop"} {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "http://team.local:18095"+path, strings.NewReader(`{}`))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+aliceIss.PlainToken)
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("member POST %s expected 403, got %d", path, rec.Code)
		}
	}

	// Anti-spoofing on /api/daemon/mcp: Alice attempts X-Memorize-Account=bob & args.account=bob on personal scope.
	rpcBody := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"commit_memory","arguments":{"candidate_fact":"Prefer dark mode","scope":"personal","account":"bob","category":"preference"}}}`
	recMCP := httptest.NewRecorder()
	reqMCP := httptest.NewRequest(http.MethodPost, "http://team.local:18095/api/daemon/mcp", bytes.NewBufferString(rpcBody))
	reqMCP.Header.Set("Content-Type", "application/json")
	reqMCP.Header.Set("Authorization", "Bearer "+aliceIss.PlainToken)
	reqMCP.Header.Set("X-Memorize-Account", "bob")
	h.ServeHTTP(recMCP, reqMCP)
	if recMCP.Code != http.StatusOK {
		t.Fatalf("member MCP call failed: %d %s", recMCP.Code, recMCP.Body.String())
	}

	ctxAlice := domain.WithAuthPrincipal(ctx, domain.AuthPrincipal{Account: "alice", Role: domain.RoleMember, ServerMode: true, Authenticated: true})
	ctxBob := domain.WithAuthPrincipal(ctx, domain.AuthPrincipal{Account: "bob", Role: domain.RoleMember, ServerMode: true, Authenticated: true})
	alicePage, _ := mem.QueryRecordsPage(ctxAlice, domain.RecordQuery{Scope: "personal", State: "all"})
	bobPage, _ := mem.QueryRecordsPage(ctxBob, domain.RecordQuery{Scope: "personal", State: "all"})
	if len(alicePage.Records) != 1 || alicePage.Records[0].Account != "alice" || len(bobPage.Records) != 0 {
		t.Fatalf("anti-spoofing failed: alice=%+v bob=%+v", alicePage.Records, bobPage.Records)
	}
}
