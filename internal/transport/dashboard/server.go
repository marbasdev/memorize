package dashboard

import (
	"embed"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"memorize/internal/domain"
	"memorize/internal/inference/localmodel"
	"memorize/internal/modules/guardrail"
	"memorize/internal/modules/memory"
	"memorize/internal/modules/progress"
	thttp "memorize/internal/transport/http"
)

//go:embed templates/*.html assets/styles.css assets/ui.js assets/theme.js assets/vendor/*
var embeddedFS embed.FS

// Deps supplies the module services and optional daemon hooks required to serve the dashboard UI.
type Deps struct {
	Memory     *memory.Service
	Progress   *progress.Service
	Guardrail  *guardrail.Service
	LocalModel *localmodel.Manager
	Hooks      thttp.DashboardHooks
}

// NewHandler builds the dashboard HTTP handler backed by the supplied module services and hooks.
func NewHandler(d Deps) http.Handler {
	if d.LocalModel == nil {
		d.LocalModel = localmodel.NewManager("")
	}
	if d.Memory != nil {
		if d.Progress == nil {
			d.Progress = progress.New(d.Memory)
		}
		if d.Guardrail == nil {
			d.Guardrail = guardrail.New(d.Memory, d.Progress)
		}
	}
	mux := http.NewServeMux()
	registerSSRRoutes(mux, d)
	registerAssetRoute(mux, d.Hooks)
	registerHandoverAPIRoutes(mux, d.Hooks)
	registerRecordsExportRoute(mux, d.Memory)
	registerRetentionAPIRoutes(mux, d)
	return thttp.SecurityHeadersMiddleware(mux)
}

func isDevHotReloadEnabled() bool {
	return strings.TrimSpace(os.Getenv("MEMORIZE_DEV_HOTRELOAD")) == "1"
}

func currentBinModTime(hooks thttp.DashboardHooks) int64 {
	if hooks.BinMtimeFn != nil {
		return hooks.BinMtimeFn()
	}
	exe, err := os.Executable()
	if err != nil {
		return 0
	}
	info, err := os.Stat(exe)
	if err != nil {
		return 0
	}
	return info.ModTime().UnixNano()
}

func resolveLiveAsset(rel string, hooksOpt ...thttp.DashboardHooks) ([]byte, bool) {
	if isDevHotReloadEnabled() {
		var hooks thttp.DashboardHooks
		if len(hooksOpt) > 0 {
			hooks = hooksOpt[0]
		}
		var candidates []string
		if wd, err := os.Getwd(); err == nil {
			candidates = append(candidates, filepath.Join(wd, "internal", "transport", "dashboard", filepath.FromSlash(rel)))
		}
		if _, callerFile, _, ok := runtime.Caller(0); ok && filepath.IsAbs(callerFile) {
			candidates = append(candidates, filepath.Join(filepath.Dir(callerFile), filepath.FromSlash(rel)))
		}
		binMtime := currentBinModTime(hooks)
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

func parseRecordQuery(q url.Values) domain.RecordQuery {
	page, _ := strconv.Atoi(strings.TrimSpace(q.Get("page")))
	pageSize, _ := strconv.Atoi(strings.TrimSpace(q.Get("page_size")))
	return domain.RecordQuery{
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

func registerAssetRoute(mux *http.ServeMux, hooks thttp.DashboardHooks) {
	mux.HandleFunc("/assets/", func(w http.ResponseWriter, r *http.Request) {
		if !thttp.IsAllowedLocalOrigin(r) {
			thttp.WriteJSON(w, http.StatusForbidden, map[string]any{"error": "non-local host blocked"})
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
		data, ok := resolveLiveAsset("assets/"+name, hooks)
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", ctype)
		w.Header().Set("Cache-Control", "no-store, no-cache, must-revalidate")
		_, _ = w.Write(data)
	})
}

func registerHandoverAPIRoutes(mux *http.ServeMux, hooks thttp.DashboardHooks) {
	mux.HandleFunc("/api/dashboard/meta", func(w http.ResponseWriter, r *http.Request) {
		if !thttp.IsAllowedLocalOrigin(r) {
			thttp.WriteJSON(w, http.StatusForbidden, map[string]any{"error": "cross-origin request blocked"})
			return
		}
		thttp.WriteJSON(w, http.StatusOK, map[string]any{
			"pid":       os.Getpid(),
			"bin_mtime": currentBinModTime(hooks),
			"can_yield": hooks.YieldFn != nil,
		})
	})

	mux.HandleFunc("/api/dashboard/yield", thttp.PostJSON(func(w http.ResponseWriter, r *http.Request) {
		if hooks.YieldFn == nil {
			thttp.WriteJSON(w, http.StatusBadRequest, map[string]any{"error": "yield not supported on this handler"})
			return
		}
		thttp.WriteJSON(w, http.StatusOK, map[string]any{"yielded": true, "pid": os.Getpid()})
		go func() {
			time.Sleep(25 * time.Millisecond)
			_ = hooks.YieldFn()
		}()
	}))
}

func registerRecordsExportRoute(mux *http.ServeMux, mem *memory.Service) {
	mux.HandleFunc("/api/records", func(w http.ResponseWriter, r *http.Request) {
		if !thttp.IsAllowedLocalOrigin(r) {
			thttp.WriteJSON(w, http.StatusForbidden, map[string]any{"error": "cross-origin request blocked"})
			return
		}
		ctx := r.Context()
		rq := parseRecordQuery(r.URL.Query())
		if r.URL.Query().Get("ids_only") == "1" {
			ids, err := mem.QueryMatchingActiveIDs(ctx, rq, 5000)
			if err != nil {
				thttp.WriteErr(w, http.StatusInternalServerError, err)
				return
			}
			thttp.WriteJSON(w, http.StatusOK, map[string]any{"ids": ids})
			return
		}
		pageRes, err := mem.QueryRecordsPage(ctx, rq)
		if err != nil {
			thttp.WriteErr(w, http.StatusInternalServerError, err)
			return
		}
		thttp.WriteJSON(w, http.StatusOK, pageRes)
	})
}
