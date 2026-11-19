package http_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
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

func TestSecurityGuardsAndDaemonMux(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:18095/mcp", nil)
	req.Host = "127.0.0.1:18095"
	req.Header.Set("Content-Type", "application/json")
	if !thttp.IsJSONPost(req, false) {
		t.Fatalf("expected localhost JSON POST to be accepted")
	}

	reqCross := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:18095/mcp", nil)
	reqCross.Host = "127.0.0.1:18095"
	reqCross.Header.Set("Sec-Fetch-Site", "cross-site")
	reqCross.Header.Set("Content-Type", "application/json")
	if thttp.IsJSONPost(reqCross, false) {
		t.Fatalf("expected cross-site request to be rejected")
	}

	reqEvilOrigin := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:18095/", nil)
	reqEvilOrigin.Host = "127.0.0.1:18095"
	reqEvilOrigin.Header.Set("Origin", "https://evil.example")
	if thttp.IsAllowedOrigin(reqEvilOrigin, false) {
		t.Fatalf("expected foreign Origin to be rejected")
	}

	t.Setenv("MEMORY_DAEMON_ADDR", "127.0.0.1:29999")
	if got := thttp.ResolveDaemonAddrForDB("", ""); got != "127.0.0.1:29999" {
		t.Fatalf("ResolveDaemonAddrForDB env = %q", got)
	}

	dbPath := filepath.Join(t.TempDir(), "http.db")
	mem, err := memory.Open(dbPath, nil)
	if err != nil {
		t.Fatalf("memory.Open: %v", err)
	}
	defer mem.Close()
	prog := progress.New(mem)
	gr := guardrail.New(mem, prog)
	mcpSrv := transportmcp.NewServer(transportmcp.Deps{Memory: mem, Progress: prog, Guardrail: gr, Version: "test"})

	mux := thttp.NewDaemonMux(thttp.DaemonDeps{
		Guardrail: gr,
		MCPServer: mcpSrv,
		BuildStatusFn: func() thttp.DaemonStatus {
			return thttp.DaemonStatus{Running: true, PID: 123, Version: "test", DBPath: dbPath}
		},
	})

	rebindReq := httptest.NewRequest(http.MethodGet, "http://evil.local/api/daemon/status", nil)
	rebindReq.Host = "evil.local:18095"
	rebindRec := httptest.NewRecorder()
	mux.ServeHTTP(rebindRec, rebindReq)
	if rebindRec.Code != http.StatusForbidden {
		t.Fatalf("expected DNS rebinding Host to fail with 403, got %d", rebindRec.Code)
	}

	stReq := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:18095/api/daemon/status", nil)
	stReq.Host = "127.0.0.1:18095"
	stRec := httptest.NewRecorder()
	mux.ServeHTTP(stRec, stReq)
	if stRec.Code != http.StatusOK || !strings.Contains(stRec.Body.String(), `"running":true`) {
		t.Fatalf("unexpected /api/daemon/status response: %d %s", stRec.Code, stRec.Body.String())
	}
	if stRec.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Fatalf("expected SecurityHeadersMiddleware nosniff header")
	}

	rpcReq := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:18095/api/daemon/mcp",
		bytes.NewReader([]byte(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)))
	rpcReq.Host = "127.0.0.1:18095"
	rpcReq.Header.Set("Content-Type", "application/json")
	rpcReq.Header.Set("X-Memorize-Session", "http-test-sess")
	rpcRec := httptest.NewRecorder()
	mux.ServeHTTP(rpcRec, rpcReq)
	if rpcRec.Code != http.StatusOK || !strings.Contains(rpcRec.Body.String(), "query_project_memory") {
		t.Fatalf("unexpected /api/daemon/mcp response: %d %s", rpcRec.Code, rpcRec.Body.String())
	}

	hookBody, _ := json.Marshal(map[string]any{
		"mode":  "claude-pretool",
		"stdin": `{"cwd":"/tmp/demo","tool_name":"Read","tool_input":{"file_path":"a.go"}}`,
	})
	hookReq := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:18095/api/daemon/hook", bytes.NewReader(hookBody))
	hookReq.Host = "127.0.0.1:18095"
	hookReq.Header.Set("Content-Type", "application/json")
	hookRec := httptest.NewRecorder()
	mux.ServeHTTP(hookRec, hookReq)
	if hookRec.Code != http.StatusOK || !strings.Contains(hookRec.Body.String(), `"exit_code":0`) {
		t.Fatalf("unexpected /api/daemon/hook response: %d %s", hookRec.Code, hookRec.Body.String())
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
	return domain.MutationDecision{Action: "INSERT", Category: "convention", IsPermanent: 0.9, Confidence: 0.9}, nil
}
func (noopEval) VerifyPlan(_ context.Context, _ string, _ []domain.Fact) ([]domain.PlanViolation, error) {
	return nil, nil
}

func TestIsAllowedOriginModes(t *testing.T) {
	reqExt := httptest.NewRequest(http.MethodGet, "http://team.example.com:18095/", nil)
	reqExt.Host = "team.example.com:18095"
	if thttp.IsAllowedOrigin(reqExt, false) {
		t.Fatalf("expected solo mode to reject non-loopback host")
	}
	if !thttp.IsAllowedOrigin(reqExt, true) {
		t.Fatalf("expected server mode to accept non-loopback host without cross-site Origin")
	}

	reqCross := httptest.NewRequest(http.MethodPost, "http://team.example.com:18095/ui/test", nil)
	reqCross.Host = "team.example.com:18095"
	reqCross.Header.Set("Sec-Fetch-Site", "cross-site")
	if thttp.IsAllowedOrigin(reqCross, true) {
		t.Fatalf("expected cross-site Sec-Fetch-Site blocked in server mode")
	}

	reqSameOrig := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:18095/login", nil)
	reqSameOrig.RemoteAddr = "127.0.0.1:54321"
	reqSameOrig.Host = "127.0.0.1:18095"
	reqSameOrig.Header.Set("Origin", "https://team.company.com")
	if thttp.IsAllowedOrigin(reqSameOrig, true) {
		t.Fatalf("expected mismatched Origin rejected without X-Forwarded-Host")
	}
	reqSameOrig.Header.Set("X-Forwarded-Host", "team.company.com")
	if !thttp.IsAllowedOrigin(reqSameOrig, true) {
		t.Fatalf("expected matching X-Forwarded-Host from trusted proxy accepted in server mode")
	}

	reqUntrustedProxy := httptest.NewRequest(http.MethodPost, "http://team.example.com:18095/login", nil)
	reqUntrustedProxy.RemoteAddr = "203.0.113.9:54321"
	reqUntrustedProxy.Host = "team.example.com:18095"
	reqUntrustedProxy.Header.Set("Origin", "https://evil.example.com")
	reqUntrustedProxy.Header.Set("X-Forwarded-Host", "evil.example.com")
	if thttp.IsAllowedOrigin(reqUntrustedProxy, true) {
		t.Fatalf("expected X-Forwarded-Host from untrusted public IP to be ignored")
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

	for _, path := range []string{"/api/daemon/status", "/mcp"} {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "http://team.local:18095"+path, nil)
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("GET %s expected 401, got %d", path, rec.Code)
		}
	}

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
