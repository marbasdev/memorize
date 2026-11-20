package http

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/mark3labs/mcp-go/server"

	"praxis/internal/domain"
	"praxis/internal/modules/guardrail"
	"praxis/internal/modules/memory"
	transportmcp "praxis/internal/transport/mcp"
)

type DaemonDeps struct {
	ServerMode    bool
	DB            *sql.DB
	Memory        *memory.Service
	Guardrail     *guardrail.Service
	MCPServer     *server.MCPServer
	Dashboard     http.Handler
	BuildStatusFn func() DaemonStatus
	ApplyPortsFn  func(ctx context.Context, daemonPort, ggufPort int) (PortApplyResponse, error)
	RestartFn     func() (string, error)
	StopFn        func()
}

func resolveDaemonAuth(deps DaemonDeps, r *http.Request) (domain.AuthPrincipal, bool) {
	if !deps.ServerMode {
		return domain.AuthPrincipal{ServerMode: false}, true
	}
	if deps.DB == nil {
		return domain.AuthPrincipal{}, false
	}
	return AuthenticateHTTPRequest(deps.DB, r)
}

func attachAuthContext(ctx context.Context, deps DaemonDeps, p domain.AuthPrincipal, fallbackAccount string) context.Context {
	if deps.ServerMode {
		ctx = domain.WithAuthPrincipal(ctx, p)
		if p.Account != "" {
			ctx = domain.WithAccount(ctx, p.Account)
		}
		return ctx
	}
	if acc := strings.TrimSpace(fallbackAccount); acc != "" {
		ctx = domain.WithAccount(ctx, acc)
	}
	return ctx
}

func NewDaemonMux(deps DaemonDeps) http.Handler {
	mux := http.NewServeMux()

	var streamHTTP http.Handler
	var pool *transportmcp.SessionPool
	if deps.MCPServer != nil {
		streamHTTP = server.NewStreamableHTTPServer(deps.MCPServer)
		pool = transportmcp.NewSessionPool(deps.MCPServer)
	}

	requireDaemonAdmin := func(w http.ResponseWriter, r *http.Request) bool {
		if !IsJSONPost(r, deps.ServerMode) {
			http.Error(w, `{"error":"forbidden"}`, http.StatusForbidden)
			return false
		}
		if !deps.ServerMode {
			return true
		}
		p, ok := resolveDaemonAuth(deps, r)
		if !ok {
			http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
			return false
		}
		if p.Role != domain.RoleAdmin {
			http.Error(w, `{"error":"admin role required"}`, http.StatusForbidden)
			return false
		}
		return true
	}

	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
			return
		}
		if deps.DB != nil {
			pingCtx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
			err := deps.DB.PingContext(pingCtx)
			cancel()
			if err != nil {
				WriteJSON(w, http.StatusServiceUnavailable, map[string]any{"ok": false})
				return
			}
		}
		if r.Method == http.MethodHead {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			return
		}
		WriteJSON(w, http.StatusOK, map[string]any{"ok": true})
	})

	mux.Handle("/mcp", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !IsAllowedOrigin(r, deps.ServerMode) {
			http.Error(w, `{"error":"non-local request blocked"}`, http.StatusForbidden)
			return
		}
		if r.Method == http.MethodPost {
			if !hasJSONContentType(r) {
				http.Error(w, `{"error":"Content-Type application/json required"}`, http.StatusUnsupportedMediaType)
				return
			}
		} else if r.Method != http.MethodGet && r.Method != http.MethodDelete {
			http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
			return
		}
		p, ok := resolveDaemonAuth(deps, r)
		if !ok {
			http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
			return
		}
		if streamHTTP == nil {
			http.Error(w, `{"error":"mcp server unavailable"}`, http.StatusServiceUnavailable)
			return
		}
		reqCtx := attachAuthContext(r.Context(), deps, p, r.Header.Get("X-Praxis-Account"))
		streamHTTP.ServeHTTP(w, r.WithContext(reqCtx))
	}))

	mux.HandleFunc("/api/daemon/status", func(w http.ResponseWriter, r *http.Request) {
		if !IsAllowedOrigin(r, deps.ServerMode) {
			http.Error(w, `{"error":"non-local request blocked"}`, http.StatusForbidden)
			return
		}
		p, ok := resolveDaemonAuth(deps, r)
		if !ok {
			http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
			return
		}
		var st DaemonStatus
		if deps.BuildStatusFn != nil {
			st = deps.BuildStatusFn()
		}
		if deps.ServerMode {
			st.ServerMode = true
			st.Account = p.Account
			st.Role = p.Role
			if p.Role != domain.RoleAdmin {
				st.PID = 0
				st.ListenAddr = ""
				st.DBPath = ""
				st.ModelsDir = ""
				st.LogPath = ""
				st.GGUFEndpoint = ""
			}
		}
		WriteJSON(w, http.StatusOK, st)
	})

	mux.HandleFunc("/api/daemon/port", func(w http.ResponseWriter, r *http.Request) {
		if !requireDaemonAdmin(w, r) {
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
		if !requireDaemonAdmin(w, r) {
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
		if !requireDaemonAdmin(w, r) {
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
		if !IsJSONPost(r, deps.ServerMode) {
			http.Error(w, `{"error":"forbidden"}`, http.StatusForbidden)
			return
		}
		p, ok := resolveDaemonAuth(deps, r)
		if !ok {
			http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
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
		sid := strings.TrimSpace(r.Header.Get("X-Praxis-Session"))
		if sid == "" {
			sid = "bridge-default"
		}
		var roots []string
		if rawRoots := strings.TrimSpace(r.Header.Get("X-Praxis-Roots")); rawRoots != "" {
			for _, part := range strings.Split(rawRoots, "\t") {
				if part = strings.TrimSpace(part); part != "" {
					roots = append(roots, part)
				}
			}
		}
		var scopeHints map[string]string
		if rawHints := strings.TrimSpace(r.Header.Get("X-Praxis-Scope-Hints")); rawHints != "" {
			_ = json.Unmarshal([]byte(rawHints), &scopeHints)
		}
		ctx := attachAuthContext(r.Context(), deps, p, r.Header.Get("X-Praxis-Account"))
		ctx = transportmcp.WithBridgeMeta(ctx, transportmcp.BridgeReqMeta{
			SessionID:  sid,
			Cwd:        strings.TrimSpace(r.Header.Get("X-Praxis-Cwd")),
			Roots:      roots,
			ScopeHints: scopeHints,
		})
		scopedSID := domain.ScopedSessionID(ctx, sid)
		ctx = deps.MCPServer.WithContext(ctx, pool.GetOrCreate(scopedSID))

		rpcResp := deps.MCPServer.HandleMessage(ctx, json.RawMessage(rawMsg))
		w.Header().Set("Content-Type", "application/json")
		if rpcResp == nil {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		_ = json.NewEncoder(w).Encode(rpcResp)
	})

	mux.HandleFunc("/api/daemon/hook", func(w http.ResponseWriter, r *http.Request) {
		if !IsJSONPost(r, deps.ServerMode) {
			http.Error(w, `{"error":"forbidden"}`, http.StatusForbidden)
			return
		}
		p, ok := resolveDaemonAuth(deps, r)
		if !ok {
			http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
			return
		}
		if deps.Guardrail == nil {
			http.Error(w, `{"error":"guardrail unavailable"}`, http.StatusServiceUnavailable)
			return
		}
		var req DaemonHookRequest
		if err := json.NewDecoder(io.LimitReader(r.Body, 4<<20)).Decode(&req); err != nil {
			http.Error(w, `{"error":"invalid hook json"}`, http.StatusBadRequest)
			return
		}
		ctx := r.Context()
		var cancel context.CancelFunc = func() {}
		if req.TimeoutMs > 0 {
			ctx, cancel = context.WithTimeout(ctx, time.Duration(req.TimeoutMs)*time.Millisecond)
		}
		defer cancel()
		ctx = attachAuthContext(ctx, deps, p, req.Account)
		if req.ClientCtx != nil {
			ctx = domain.WithClientHookContext(ctx, req.ClientCtx)
		}
		fallbackCwd := req.Cwd
		if deps.ServerMode {
			fallbackCwd = ""
		}
		out, code, _ := deps.Guardrail.ExecuteHook(ctx, req.Mode, []byte(req.Stdin), fallbackCwd)
		WriteJSON(w, http.StatusOK, map[string]any{
			"output":    out,
			"exit_code": code,
		})
	})

	mux.HandleFunc("/api/daemon/export", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
			return
		}
		if !IsAllowedOrigin(r, deps.ServerMode) {
			http.Error(w, `{"error":"non-local request blocked"}`, http.StatusForbidden)
			return
		}
		p, ok := resolveDaemonAuth(deps, r)
		if !ok {
			http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
			return
		}
		if deps.Memory == nil {
			http.Error(w, `{"error":"memory service unavailable"}`, http.StatusServiceUnavailable)
			return
		}
		q := r.URL.Query()
		var ids []int64
		if rawIDs := strings.TrimSpace(q.Get("ids")); rawIDs != "" {
			for _, part := range strings.Split(rawIDs, ",") {
				if id, err := strconv.ParseInt(strings.TrimSpace(part), 10, 64); err == nil && id > 0 {
					ids = append(ids, id)
				}
			}
		}
		rq := domain.RecordQuery{
			Scope:       strings.TrimSpace(q.Get("scope")),
			Account:     strings.TrimSpace(q.Get("account")),
			State:       strings.TrimSpace(q.Get("state")),
			Category:    strings.TrimSpace(q.Get("category")),
			SubCategory: strings.TrimSpace(q.Get("sub_category")),
			Search:      q.Get("q"),
		}
		reqCtx := attachAuthContext(r.Context(), deps, p, r.Header.Get("X-Praxis-Account"))
		bundle, err := deps.Memory.ExportBundle(reqCtx, rq, ids)
		if err != nil {
			WriteErr(w, http.StatusInternalServerError, err)
			return
		}
		if q.Get("download") == "1" {
			w.Header().Set("Content-Disposition", `attachment; filename="praxis-bundle.json"`)
		}
		WriteJSON(w, http.StatusOK, bundle)
	})

	mux.HandleFunc("/api/daemon/import", PostJSONMode(deps.ServerMode, func(w http.ResponseWriter, r *http.Request) {
		p, ok := resolveDaemonAuth(deps, r)
		if !ok {
			http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
			return
		}
		if deps.Memory == nil {
			http.Error(w, `{"error":"memory service unavailable"}`, http.StatusServiceUnavailable)
			return
		}
		var req struct {
			Bundle  domain.MemoryBundle        `json:"bundle"`
			Options domain.BundleImportOptions `json:"options"`
		}
		if err := json.NewDecoder(io.LimitReader(r.Body, 4<<20)).Decode(&req); err != nil {
			http.Error(w, `{"error":"invalid bundle import json"}`, http.StatusBadRequest)
			return
		}
		reqCtx := attachAuthContext(r.Context(), deps, p, r.Header.Get("X-Praxis-Account"))
		res, err := deps.Memory.ImportBundle(reqCtx, req.Bundle, req.Options)
		if err != nil {
			WriteErr(w, http.StatusBadRequest, err)
			return
		}
		WriteJSON(w, http.StatusOK, res)
	}))

	if deps.Dashboard != nil {
		mux.Handle("/", deps.Dashboard)
	}
	return SecurityHeadersMiddleware(mux)
}
