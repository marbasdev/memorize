package http_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

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
	if !thttp.IsLocalJSONPost(req) {
		t.Fatalf("expected localhost JSON POST to be accepted")
	}

	reqCross := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:18095/mcp", nil)
	reqCross.Host = "127.0.0.1:18095"
	reqCross.Header.Set("Sec-Fetch-Site", "cross-site")
	reqCross.Header.Set("Content-Type", "application/json")
	if thttp.IsLocalJSONPost(reqCross) {
		t.Fatalf("expected cross-site request to be rejected")
	}

	reqEvilOrigin := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:18095/", nil)
	reqEvilOrigin.Host = "127.0.0.1:18095"
	reqEvilOrigin.Header.Set("Origin", "https://evil.example")
	if thttp.IsAllowedLocalOrigin(reqEvilOrigin) {
		t.Fatalf("expected foreign Origin to be rejected")
	}

	t.Setenv("MEMORY_DAEMON_ADDR", "127.0.0.1:29999")
	if got := thttp.ResolveDaemonAddr(""); got != "127.0.0.1:29999" {
		t.Fatalf("ResolveDaemonAddr env = %q", got)
	}

	// End-to-end mux check for /api/daemon/status, /api/daemon/mcp, /api/daemon/hook, and DNS rebinding Host guard
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

	// /api/daemon/mcp tools/list via bridge endpoint
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

	// /api/daemon/hook read-only pretool
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
