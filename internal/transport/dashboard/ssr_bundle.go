package dashboard

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"praxis/internal/domain"
	thttp "praxis/internal/transport/http"
)

type ssrImportModalData struct {
	Lang        string
	Scopes      []string
	TargetScope string
	Mode        string
	BundleJSON  string
	Preview     *domain.BundleImportResult
	Error       string
}

func registerSSRBundleRoutes(mux *http.ServeMux, d Deps) {
	mux.HandleFunc("/ui/memories/export", getHX(func(w http.ResponseWriter, r *http.Request) {
		if d.Memory == nil {
			http.Error(w, "memory service unavailable", http.StatusServiceUnavailable)
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
		rq := parseRecordQuery(q)
		bundle, err := d.Memory.ExportBundle(r.Context(), rq, ids)
		if err != nil {
			thttp.WriteErr(w, http.StatusInternalServerError, err)
			return
		}
		w.Header().Set("Content-Disposition", `attachment; filename="praxis-bundle.json"`)
		thttp.WriteJSON(w, http.StatusOK, bundle)
	}))

	mux.HandleFunc("/ui/modal/import", getHX(func(w http.ResponseWriter, r *http.Request) {
		lang := requestLang(r)
		facets, _ := d.Memory.GetDashboardFacets(r.Context())
		scopes := facets.Scopes
		if len(scopes) == 0 {
			scopes = []string{"global", "personal"}
		}
		writeHTMLTemplate(w, "modal_import", ssrImportModalData{
			Lang:        lang,
			Scopes:      scopes,
			TargetScope: strings.TrimSpace(r.URL.Query().Get("scope")),
			Mode:        "merge",
		})
	}))

	parseImportForm := func(r *http.Request) (domain.MemoryBundle, domain.BundleImportOptions, string, error) {
		rawJSON := strings.TrimSpace(r.FormValue("bundle_json"))
		if rawJSON == "" {
			return domain.MemoryBundle{}, domain.BundleImportOptions{}, rawJSON, fmt.Errorf("paste or select a memory bundle JSON file first")
		}
		var bundle domain.MemoryBundle
		if err := json.Unmarshal([]byte(rawJSON), &bundle); err != nil {
			return domain.MemoryBundle{}, domain.BundleImportOptions{}, rawJSON, fmt.Errorf("invalid bundle JSON: %w", err)
		}
		if len(bundle.Items) == 0 {
			return domain.MemoryBundle{}, domain.BundleImportOptions{}, rawJSON, fmt.Errorf("bundle contains 0 memory items")
		}
		mode := strings.ToLower(strings.TrimSpace(r.FormValue("mode")))
		if mode != "exact" && mode != "merge" {
			mode = "merge"
		}
		opts := domain.BundleImportOptions{
			Mode:        mode,
			TargetScope: strings.TrimSpace(r.FormValue("target_scope")),
		}
		return bundle, opts, rawJSON, nil
	}

	mux.HandleFunc("/ui/memories/import/preview", postHX(func(w http.ResponseWriter, r *http.Request) {
		lang := requestLang(r)
		bundle, opts, rawJSON, err := parseImportForm(r)
		if err != nil {
			writeHTMLTemplate(w, "import_preview_box", ssrImportModalData{Lang: lang, BundleJSON: rawJSON, Error: err.Error()})
			return
		}
		opts.DryRun = true
		res, err := d.Memory.ImportBundle(r.Context(), bundle, opts)
		if err != nil {
			writeHTMLTemplate(w, "import_preview_box", ssrImportModalData{Lang: lang, BundleJSON: rawJSON, Error: err.Error()})
			return
		}
		writeHTMLTemplate(w, "import_preview_box", ssrImportModalData{
			Lang:        lang,
			TargetScope: opts.TargetScope,
			Mode:        opts.Mode,
			Preview:     &res,
		})
	}))

	mux.HandleFunc("/ui/memories/import/apply", postHX(func(w http.ResponseWriter, r *http.Request) {
		lang := requestLang(r)
		bundle, opts, _, err := parseImportForm(r)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		opts.DryRun = false
		res, err := d.Memory.ImportBundle(r.Context(), bundle, opts)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		toast := langText(lang,
			fmt.Sprintf("Imported bundle: %d added, %d merged/overwritten, %d skipped", res.Inserted, res.Overwritten, res.Skipped),
			fmt.Sprintf("번들 가져오기 완료: 추가 %d건, 병합·대체 %d건, 스킵 %d건", res.Inserted, res.Overwritten, res.Skipped))
		setHXTriggers(w, toast, true, true)
		memData, err := buildMemoriesView(r.Context(), d, lang, r.Form, true)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeHTMLTemplate(w, "memories_section", memData)
	}))
}
