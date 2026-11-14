package http

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/mark3labs/mcp-go/server"

	"memorize/internal/domain"
	"memorize/internal/modules/guardrail"
	transportmcp "memorize/internal/transport/mcp"
)

// DaemonDeps wires unified daemon HTTP routes to MCP, Guardrail, Dashboard, and process control callbacks.
type DaemonDeps struct {
	Guardrail     *guardrail.Service
	MCPServer     *server.MCPServer
	Dashboard     http.Handler
	BuildStatusFn func() DaemonStatus
	ApplyPortsFn  func(ctx context.Context, daemonPort, ggufPort int) (PortApplyResponse, error)
	RestartFn     func() (string, error)
	StopFn        func()
}

// NewDaemonMux constructs the root HTTP handler combining /mcp, /api/daemon/*, and the dashboard handler.
func NewDaemonMux(deps DaemonDeps) http.Handler {
	mux := http.NewServeMux()

	var streamHTTP http.Handler
	var pool *transportmcp.SessionPool
	if deps.MCPServer != nil {
		streamHTTP = server.NewStreamableHTTPServer(deps.MCPServer)
		pool = transportmcp.NewSessionPool(deps.MCPServer)
	}

	mux.Handle("/mcp", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !IsAllowedLocalOrigin(r) {
			http.Error(w, `{"error":"non-local request blocked"}`, http.StatusForbidden)
			return
		}
		if r.Method == http.MethodPost {
			ct := strings.ToLower(strings.TrimSpace(r.Header.Get("Content-Type")))
			if !strings.HasPrefix(ct, "application/json") {
				http.Error(w, `{"error":"Content-Type application/json required"}`, http.StatusUnsupportedMediaType)
				return
			}
		} else if r.Method != http.MethodGet && r.Method != http.MethodDelete {
			http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
			return
		}
		if streamHTTP == nil {
			http.Error(w, `{"error":"mcp server unavailable"}`, http.StatusServiceUnavailable)
			return
		}
		streamHTTP.ServeHTTP(w, r)
	}))

	mux.HandleFunc("/api/daemon/status", func(w http.ResponseWriter, r *http.Request) {
		if !IsAllowedLocalOrigin(r) {
			http.Error(w, `{"error":"non-local request blocked"}`, http.StatusForbidden)
			return
		}
		var st DaemonStatus
		if deps.BuildStatusFn != nil {
			st = deps.BuildStatusFn()
		}
		WriteJSON(w, http.StatusOK, st)
	})

	mux.HandleFunc("/api/daemon/port", func(w http.ResponseWriter, r *http.Request) {
		if !IsLocalJSONPost(r) {
			http.Error(w, `{"error":"forbidden"}`, http.StatusForbidden)
			return
		}
		var req PortApplyRequest
		if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&req); err != nil {
			http.Error(w, `{"error":"invalid port request json"}`, http.StatusBadRequest)
			return
		}
		dp, gp := req.DaemonPort, req.GGUFPort
		if req.Reset {
			dp, gp = domain.DefaultDaemonPort, domain.DefaultGGUFPort
		}
		if deps.ApplyPortsFn == nil {
			WriteJSON(w, http.StatusNotImplemented, PortApplyResponse{OK: false, Error: "port reconfiguration unavailable"})
			return
		}
		res, err := deps.ApplyPortsFn(r.Context(), dp, gp)
		if err != nil {
			WriteJSON(w, http.StatusBadRequest, PortApplyResponse{OK: false, Error: err.Error()})
			return
		}
		WriteJSON(w, http.StatusOK, res)
	})

	mux.HandleFunc("/api/daemon/restart", func(w http.ResponseWriter, r *http.Request) {
		if !IsLocalJSONPost(r) {
			http.Error(w, `{"error":"forbidden"}`, http.StatusForbidden)
			return
		}
		var nextBin string
		var err error
		if deps.RestartFn != nil {
			nextBin, err = deps.RestartFn()
		}
		if err != nil {
			WriteJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
		WriteJSON(w, http.StatusOK, map[string]any{
			"restarting": true,
			"old_pid":    os.Getpid(),
			"binary":     nextBin,
		})
	})

	mux.HandleFunc("/api/daemon/stop", func(w http.ResponseWriter, r *http.Request) {
		if !IsLocalJSONPost(r) {
			http.Error(w, `{"error":"forbidden"}`, http.StatusForbidden)
			return
		}
		WriteJSON(w, http.StatusOK, map[string]any{
			"stopping": true,
			"pid":      os.Getpid(),
		})
		if deps.StopFn != nil {
			deps.StopFn()
		}
	})

	mux.HandleFunc("/api/daemon/mcp", func(w http.ResponseWriter, r *http.Request) {
		if !IsLocalJSONPost(r) {
			http.Error(w, `{"error":"forbidden"}`, http.StatusForbidden)
			return
		}
		if deps.MCPServer == nil || pool == nil {
			http.Error(w, `{"error":"mcp bridge unavailable"}`, http.StatusServiceUnavailable)
			return
		}
		rawMsg, err := io.ReadAll(io.LimitReader(r.Body, 4<<20))
		if err != nil || len(bytes.TrimSpace(rawMsg)) == 0 {
			http.Error(w, `{"error":"empty rpc payload"}`, http.StatusBadRequest)
			return
		}
		sid := strings.TrimSpace(r.Header.Get("X-Memorize-Session"))
		if sid == "" {
			sid = "bridge-default"
		}
		var roots []string
		if rawRoots := strings.TrimSpace(r.Header.Get("X-Memorize-Roots")); rawRoots != "" {
			for _, part := range strings.Split(rawRoots, "\t") {
				if part = strings.TrimSpace(part); part != "" {
					roots = append(roots, part)
				}
			}
		}
		ctx := r.Context()
		if acc := strings.TrimSpace(r.Header.Get("X-Memorize-Account")); acc != "" {
			ctx = domain.WithAccount(ctx, acc)
		}
		ctx = transportmcp.WithBridgeMeta(ctx, transportmcp.BridgeReqMeta{
			SessionID: sid,
			Cwd:       strings.TrimSpace(r.Header.Get("X-Memorize-Cwd")),
			Roots:     roots,
		})
		ctx = deps.MCPServer.WithContext(ctx, pool.GetOrCreate(sid))

		rpcResp := deps.MCPServer.HandleMessage(ctx, json.RawMessage(rawMsg))
		w.Header().Set("Content-Type", "application/json")
		if rpcResp == nil {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		_ = json.NewEncoder(w).Encode(rpcResp)
	})

	mux.HandleFunc("/api/daemon/hook", func(w http.ResponseWriter, r *http.Request) {
		if !IsLocalJSONPost(r) {
			http.Error(w, `{"error":"forbidden"}`, http.StatusForbidden)
			return
		}
		if deps.Guardrail == nil {
			http.Error(w, `{"error":"guardrail unavailable"}`, http.StatusServiceUnavailable)
			return
		}
		var req struct {
			Mode      string `json:"mode"`
			Stdin     string `json:"stdin"`
			Cwd       string `json:"cwd"`
			Account   string `json:"account"`
			TimeoutMs int64  `json:"timeout_ms,omitempty"`
		}
		if err := json.NewDecoder(io.LimitReader(r.Body, 2<<20)).Decode(&req); err != nil {
			http.Error(w, `{"error":"invalid hook json"}`, http.StatusBadRequest)
			return
		}
		ctx := r.Context()
		var cancel context.CancelFunc = func() {}
		if req.TimeoutMs > 0 {
			ctx, cancel = context.WithTimeout(ctx, time.Duration(req.TimeoutMs)*time.Millisecond)
		}
		defer cancel()
		if strings.TrimSpace(req.Account) != "" {
			ctx = domain.WithAccount(ctx, req.Account)
		}
		out, code, _ := deps.Guardrail.ExecuteHook(ctx, req.Mode, []byte(req.Stdin), req.Cwd)
		WriteJSON(w, http.StatusOK, map[string]any{
			"output":    out,
			"exit_code": code,
		})
	})

	if deps.Dashboard != nil {
		mux.Handle("/", deps.Dashboard)
	}
	return SecurityHeadersMiddleware(mux)
}
