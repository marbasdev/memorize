package dashboard

import (
	"context"
	"embed"
	"encoding/json"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"memorize/internal/memory"
	"memorize/internal/memory/localmodel"
)

//go:embed templates/*.html assets/styles.css assets/ui.js assets/theme.js assets/vendor/*
var embeddedFS embed.FS

func isDevHotReloadEnabled() bool {
	return strings.TrimSpace(os.Getenv("MEMORIZE_DEV_HOTRELOAD")) == "1"
}

func resolveLiveAsset(rel string) ([]byte, bool) {
	if isDevHotReloadEnabled() {
		var candidates []string
		if wd, err := os.Getwd(); err == nil {
			candidates = append(candidates, filepath.Join(wd, "internal", "memory", "dashboard", filepath.FromSlash(rel)))
		}
		if _, callerFile, _, ok := runtime.Caller(0); ok && filepath.IsAbs(callerFile) {
			candidates = append(candidates, filepath.Join(filepath.Dir(callerFile), filepath.FromSlash(rel)))
		}
		binMtime := currentBinaryModTime()
		for _, p := range candidates {
			info, err := os.Stat(p)
			if err != nil || info.IsDir() {
				continue
			}
			if binMtime == 0 || info.ModTime().UnixNano() >= binMtime {
				if data, err := os.ReadFile(p); err == nil && len(data) > 0 {
					return data, true
				}
			}
		}
	}
	data, err := embeddedFS.ReadFile(rel)
	if err != nil {
		return nil, false
	}
	return data, true
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, err error) {
	writeJSON(w, code, map[string]any{"error": err.Error()})
}

func isLoopbackHost(rawHost string) bool {
	h := strings.TrimSpace(rawHost)
	if h == "" {
		return false
	}
	if hostOnly, _, err := net.SplitHostPort(h); err == nil {
		h = hostOnly
	}
	h = strings.Trim(strings.ToLower(h), "[]")
	return h == "127.0.0.1" || h == "localhost" || h == "::1"
}

// SecurityHeadersMiddleware attaches defensive HTTP security headers on local console responses.
func SecurityHeadersMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Content-Security-Policy",
			"default-src 'self'; script-src 'self' 'unsafe-eval'; style-src 'self' 'unsafe-inline'; img-src 'self' data:; connect-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'self'")
		next.ServeHTTP(w, r)
	})
}

func isAllowedLocalOrigin(r *http.Request) bool {
	if strings.EqualFold(strings.TrimSpace(r.Header.Get("Sec-Fetch-Site")), "cross-site") {
		return false
	}
	if !isLoopbackHost(r.Host) {
		return false
	}
	orig := strings.TrimSpace(r.Header.Get("Origin"))
	if orig == "" {
		return true
	}
	u, err := url.Parse(orig)
	if err != nil {
		return false
	}
	host := strings.ToLower(u.Hostname())
	return host == "127.0.0.1" || host == "localhost" || host == "::1"
}

// postJSON enforces POST + application/json + local Origin check to prevent drive-by browser CSRF against localhost.
func postJSON(fn http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !isAllowedLocalOrigin(r) {
			writeJSON(w, http.StatusForbidden, map[string]any{"error": "cross-origin request blocked"})
			return
		}
		if r.Method != http.MethodPost {
			writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "POST required"})
			return
		}
		ct := strings.ToLower(strings.TrimSpace(r.Header.Get("Content-Type")))
		if !strings.HasPrefix(ct, "application/json") {
			writeJSON(w, http.StatusUnsupportedMediaType, map[string]any{"error": "Content-Type application/json required"})
			return
		}
		fn(w, r)
	}
}

func decodeJSON(w http.ResponseWriter, r *http.Request, dst any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	if err := json.NewDecoder(r.Body).Decode(dst); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return false
	}
	return true
}

func parseRecordQuery(q url.Values) memory.RecordQuery {
	page, _ := strconv.Atoi(strings.TrimSpace(q.Get("page")))
	pageSize, _ := strconv.Atoi(strings.TrimSpace(q.Get("page_size")))
	return memory.RecordQuery{
		Scope:       strings.TrimSpace(q.Get("scope")),
		SessionID:   strings.TrimSpace(q.Get("session_id")),
		Account:     strings.TrimSpace(q.Get("account")),
		State:       strings.TrimSpace(q.Get("state")),
		Category:    strings.TrimSpace(q.Get("category")),
		SubCategory: strings.TrimSpace(q.Get("sub_category")),
		Search:      q.Get("q"),
		Page:        page,
		PageSize:    pageSize,
	}
}

func NewHandler(svc *memory.Service, mgrOpt ...*localmodel.Manager) http.Handler {
	return NewHandlerWithYield(svc, nil, mgrOpt...)
}

type DaemonPortState struct {
	PID            int    `json:"pid"`
	ListenAddr     string `json:"listen_addr"`
	UptimeSec      int64  `json:"uptime_sec"`
	ConfiguredPort int    `json:"configured_daemon_port"`
	ActivePort     int    `json:"active_daemon_port"`
	Fallback       bool   `json:"daemon_fallback"`
}

type DaemonPortApplyResult struct {
	ListenAddr       string `json:"listen_addr"`
	ActiveDaemonPort int    `json:"active_daemon_port"`
	DaemonFallback   bool   `json:"daemon_fallback"`
	Restarting       bool   `json:"restarting"`
	NewURL           string `json:"new_url"`
	ActiveGGUFPort   int    `json:"active_gguf_port"`
	GGUFEndpoint     string `json:"gguf_endpoint"`
	GGUFFallback     bool   `json:"gguf_fallback"`
	GGUFRunning      bool   `json:"gguf_running"`
}

type ServerHooks struct {
	YieldFn           func() error
	DaemonStatusFn    func() DaemonPortState
	ApplyDaemonPortFn func(ctx context.Context, daemonPort, ggufPort int) (DaemonPortApplyResult, error)
}

func NewHandlerWithYield(svc *memory.Service, yieldFn func() error, mgrOpt ...*localmodel.Manager) http.Handler {
	var mgr *localmodel.Manager
	if len(mgrOpt) > 0 && mgrOpt[0] != nil {
		mgr = mgrOpt[0]
	}
	return NewHandlerWithHooks(svc, mgr, ServerHooks{YieldFn: yieldFn})
}

func NewHandlerWithHooks(svc *memory.Service, mgr *localmodel.Manager, hooks ServerHooks) http.Handler {
	if mgr == nil {
		mgr = localmodel.NewManager("")
	}
	mux := http.NewServeMux()
	registerSSRRoutes(mux, svc, mgr, hooks)
	registerAssetRoute(mux)
	registerHandoverAPIRoutes(mux, hooks.YieldFn)
	registerRecordsExportRoute(mux, svc)
	registerRetentionAPIRoutes(mux, svc)
	return SecurityHeadersMiddleware(mux)
}

func registerAssetRoute(mux *http.ServeMux) {
	mux.HandleFunc("/assets/", func(w http.ResponseWriter, r *http.Request) {
		if !isAllowedLocalOrigin(r) {
			writeJSON(w, http.StatusForbidden, map[string]any{"error": "non-local host blocked"})
			return
		}
		name := strings.TrimPrefix(r.URL.Path, "/assets/")
		var ctype string
		switch name {
		case "styles.css":
			ctype = "text/css; charset=utf-8"
		case "ui.js", "theme.js", "vendor/htmx.min.js", "vendor/alpine.min.js":
			ctype = "application/javascript; charset=utf-8"
		default:
			http.NotFound(w, r)
			return
		}
		data, ok := resolveLiveAsset("assets/" + name)
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", ctype)
		w.Header().Set("Cache-Control", "no-store, no-cache, must-revalidate")
		_, _ = w.Write(data)
	})
}

func registerHandoverAPIRoutes(mux *http.ServeMux, yieldFn func() error) {
	mux.HandleFunc("/api/dashboard/meta", func(w http.ResponseWriter, r *http.Request) {
		if !isAllowedLocalOrigin(r) {
			writeJSON(w, http.StatusForbidden, map[string]any{"error": "cross-origin request blocked"})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"pid":       os.Getpid(),
			"bin_mtime": currentBinaryModTime(),
			"can_yield": yieldFn != nil,
		})
	})

	mux.HandleFunc("/api/dashboard/yield", postJSON(func(w http.ResponseWriter, r *http.Request) {
		if yieldFn == nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "yield not supported on this handler"})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"yielded": true, "pid": os.Getpid()})
		go func() {
			time.Sleep(25 * time.Millisecond)
			_ = yieldFn()
		}()
	}))
}

func registerRecordsExportRoute(mux *http.ServeMux, svc *memory.Service) {
	mux.HandleFunc("/api/records", func(w http.ResponseWriter, r *http.Request) {
		if !isAllowedLocalOrigin(r) {
			writeJSON(w, http.StatusForbidden, map[string]any{"error": "cross-origin request blocked"})
			return
		}
		ctx := r.Context()
		rq := parseRecordQuery(r.URL.Query())
		if r.URL.Query().Get("ids_only") == "1" {
			ids, err := svc.QueryMatchingActiveIDs(ctx, rq, 5000)
			if err != nil {
				writeErr(w, http.StatusInternalServerError, err)
				return
			}
			writeJSON(w, http.StatusOK, map[string]any{"ids": ids})
			return
		}
		pageRes, err := svc.QueryRecordsPage(ctx, rq)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, err)
			return
		}
		writeJSON(w, http.StatusOK, pageRes)
	})
}
