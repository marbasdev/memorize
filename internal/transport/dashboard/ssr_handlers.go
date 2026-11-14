package dashboard

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"unicode/utf16"

	"memorize/internal/domain"
	"memorize/internal/inference/discovery"
	"memorize/internal/infra/config"
	thttp "memorize/internal/transport/http"
)

// postHX enforces local Origin/Host/Sec-Fetch-Site checks AND requires the custom HX-Request header,
// preventing drive-by Simple-POST form CSRF against localhost without requiring client JSON assembly.
func postHX(fn http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !thttp.IsAllowedLocalOrigin(r) {
			thttp.WriteJSON(w, http.StatusForbidden, map[string]any{"error": "cross-origin request blocked"})
			return
		}
		if r.Method != http.MethodPost {
			thttp.WriteJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "POST required"})
			return
		}
		if r.Header.Get("HX-Request") != "true" {
			thttp.WriteJSON(w, http.StatusForbidden, map[string]any{"error": "HX-Request header required"})
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
		_ = r.ParseForm()
		fn(w, r)
	}
}

func getHX(fn http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !thttp.IsAllowedLocalOrigin(r) {
			thttp.WriteJSON(w, http.StatusForbidden, map[string]any{"error": "cross-origin request blocked"})
			return
		}
		fn(w, r)
	}
}

// escapeJSONASCII converts non-ASCII runes in marshaled JSON to \uXXXX sequences
// so HTTP response headers (HX-Trigger) never suffer Latin-1 mojibake in browsers.
func escapeJSONASCII(raw []byte) string {
	s := string(raw)
	var sb strings.Builder
	sb.Grow(len(raw) + 32)
	for _, r := range s {
		if r <= 0x7F {
			sb.WriteByte(byte(r))
			continue
		}
		if r <= 0xFFFF {
			fmt.Fprintf(&sb, `\u%04x`, r)
			continue
		}
		r1, r2 := utf16.EncodeRune(r)
		fmt.Fprintf(&sb, `\u%04x\u%04x`, r1, r2)
	}
	return sb.String()
}

func setHXTriggers(w http.ResponseWriter, toast string, closeModal, clearSel bool) {
	ev := map[string]any{}
	if strings.TrimSpace(toast) != "" {
		ev["memorizeToast"] = toast
	}
	if closeModal {
		ev["memorizeCloseModal"] = true
	}
	if clearSel {
		ev["memorizeClearSel"] = true
	}
	if len(ev) > 0 {
		if b, err := json.Marshal(ev); err == nil {
			w.Header().Set("HX-Trigger", escapeJSONASCII(b))
		}
	}
}

func writeHTMLTemplate(w http.ResponseWriter, name string, data any) {
	body, err := renderSSRTemplate(name, data)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store, no-cache, must-revalidate")
	_, _ = w.Write(body)
}

func parseFormIDs(vals url.Values) []int64 {
	seen := map[int64]bool{}
	var out []int64
	add := func(raw string) {
		for _, part := range strings.FieldsFunc(raw, func(r rune) bool { return r == ',' || r == ' ' }) {
			if id, err := strconv.ParseInt(strings.TrimSpace(part), 10, 64); err == nil && id > 0 && !seen[id] {
				seen[id] = true
				out = append(out, id)
			}
		}
	}
	for _, raw := range vals["ids"] {
		add(raw)
	}
	if len(out) == 0 {
		add(vals.Get("id"))
	}
	return out
}

func renderMemoriesAfterMutation(w http.ResponseWriter, r *http.Request, d Deps, lang string, toast string, closeModal, clearSel bool) {
	memView, err := buildMemoriesView(r.Context(), d, lang, r.Form, true)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	setHXTriggers(w, toast, closeModal, clearSel)
	writeHTMLTemplate(w, "memories_section", memView)
}

func renderEnginesSection(w http.ResponseWriter, r *http.Request, d Deps, lang string, q url.Values, overrideKey, overrideURL string) {
	view := buildEnginesView(r.Context(), d, lang, q, overrideKey, overrideURL)
	view.OOB = true
	writeHTMLTemplate(w, "engines_section", view)
}

func registerSSRRoutes(mux *http.ServeMux, d Deps) {
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		if !thttp.IsAllowedLocalOrigin(r) {
			thttp.WriteJSON(w, http.StatusForbidden, map[string]any{"error": "non-local host blocked"})
			return
		}
		ctx := r.Context()
		lang := requestLang(r)
		memView, err := buildMemoriesView(ctx, d, lang, r.URL.Query(), false)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		progView, _ := buildProgressView(ctx, d, lang, r.URL.Query())
		grView := buildGuardrailsView(ctx, d, lang, r.URL.Query(), memView.Scopes)
		engView := buildEnginesView(ctx, d, lang, r.URL.Query(), "", "")
		calView := buildCalibrateView(ctx, d, lang, r.URL.Query(), memView.Scopes)
		writeHTMLTemplate(w, "layout", ssrPageData{
			Lang:       lang,
			Memories:   memView,
			Progress:   progView,
			Guardrails: grView,
			Engines:    engView,
			Calibrate:  calView,
		})
	})

	mux.HandleFunc("/ui/lang", postHX(func(w http.ResponseWriter, r *http.Request) {
		lang := normalizeLang(r.FormValue("lang"))
		http.SetCookie(w, &http.Cookie{
			Name:     "memorize_lang",
			Value:    lang,
			Path:     "/",
			MaxAge:   365 * 24 * 3600,
			HttpOnly: true,
			SameSite: http.SameSiteLaxMode,
		})
		w.Header().Set("HX-Refresh", "true")
		w.WriteHeader(http.StatusOK)
	}))

	mux.HandleFunc("/ui/memories", getHX(func(w http.ResponseWriter, r *http.Request) {
		lang := requestLang(r)
		memView, err := buildMemoriesView(r.Context(), d, lang, r.URL.Query(), true)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeHTMLTemplate(w, "memories_section", memView)
	}))

	mux.HandleFunc("/ui/personal-toggle", postHX(func(w http.ResponseWriter, r *http.Request) {
		val := "0"
		msg := "Personal memories isolated from projects"
		if r.FormValue("enabled") == "1" {
			val = "1"
			msg = "Personal memories enabled in projects"
		}
		if err := d.Memory.SaveConfig(domain.WithAdminBypass(r.Context()), map[string]string{"include_personal_in_project": val}); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		setHXTriggers(w, msg, false, false)
		w.WriteHeader(http.StatusOK)
	}))

	mux.HandleFunc("/ui/memories/commit", postHX(func(w http.ResponseWriter, r *http.Request) {
		lang := requestLang(r)
		cand := strings.TrimSpace(r.FormValue("candidate"))
		if cand == "" {
			http.Error(w, "candidate cannot be empty", http.StatusBadRequest)
			return
		}
		ctx := r.Context()
		if acc := strings.TrimSpace(r.FormValue("new_account")); acc != "" {
			ctx = domain.WithAccount(ctx, acc)
		}
		out, err := d.Memory.CommitMemoryFull(ctx, cand, r.FormValue("new_category"), r.FormValue("new_sub_category"), r.FormValue("new_tags"), r.FormValue("new_scope"))
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		renderMemoriesAfterMutation(w, r, d, lang, out, false, false)
	}))

	mux.HandleFunc("/ui/memories/forget", postHX(func(w http.ResponseWriter, r *http.Request) {
		lang := requestLang(r)
		ids := parseFormIDs(r.Form)
		if len(ids) == 0 {
			http.Error(w, "no memory IDs selected", http.StatusBadRequest)
			return
		}
		n, err := d.Memory.ForgetMemories(domain.WithAdminBypass(r.Context()), ids)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		renderMemoriesAfterMutation(w, r, d, lang, fmt.Sprintf("Deactivated %d item(s)", n), true, true)
	}))

	mux.HandleFunc("/ui/memories/undo", postHX(func(w http.ResponseWriter, r *http.Request) {
		lang := requestLang(r)
		id, _ := strconv.ParseInt(strings.TrimSpace(r.FormValue("id")), 10, 64)
		msg, err := d.Memory.UndoMemory(domain.WithAdminBypass(r.Context()), id)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		renderMemoriesAfterMutation(w, r, d, lang, msg, true, false)
	}))

	mux.HandleFunc("/ui/memories/update", postHX(func(w http.ResponseWriter, r *http.Request) {
		lang := requestLang(r)
		ctx := domain.WithAdminBypass(r.Context())
		id, _ := strconv.ParseInt(strings.TrimSpace(r.FormValue("id")), 10, 64)
		subCat := strings.TrimSpace(r.FormValue("sub_category"))
		pinned := r.FormValue("pinned") == "1"
		if err := d.Memory.UpdateMemoryFull(ctx, id, r.FormValue("scope"), r.FormValue("category"), &subCat, r.FormValue("content"), &pinned); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if err := d.Memory.UpdateMemoryAccount(ctx, id, r.FormValue("account")); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if err := d.Memory.UpdateMemoryTags(ctx, id, r.FormValue("tags")); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		renderMemoriesAfterMutation(w, r, d, lang, fmt.Sprintf("Updated #%d", id), true, false)
	}))

	mux.HandleFunc("/ui/memories/purge", postHX(func(w http.ResponseWriter, r *http.Request) {
		lang := requestLang(r)
		n, err := d.Memory.PurgeTombstones(domain.WithAdminBypass(r.Context()), r.FormValue("scope"))
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		renderMemoriesAfterMutation(w, r, d, lang, fmt.Sprintf("Purged %d tombstones", n), false, false)
	}))

	mux.HandleFunc("/ui/modal/detail", getHX(func(w http.ResponseWriter, r *http.Request) {
		lang := requestLang(r)
		ctx := r.Context()
		id, _ := strconv.ParseInt(strings.TrimSpace(r.URL.Query().Get("id")), 10, 64)
		recs, err := d.Memory.GetRecordsByIDs(ctx, []int64{id})
		if err != nil || len(recs) == 0 {
			http.Error(w, "memory not found", http.StatusNotFound)
			return
		}
		facets, _ := d.Memory.GetDashboardFacets(ctx)
		cats, allSubMap := mergeCategoriesAndSubs(d.Memory.GetConfigMap(ctx)["preset_categories"], facets)
		scopes := facets.Scopes
		if len(scopes) == 0 {
			scopes = []string{"global", "personal"}
		}
		writeHTMLTemplate(w, "modal_detail", ssrDetailModalData{
			Lang:             lang,
			Record:           recs[0],
			Scopes:           scopes,
			Categories:       cats,
			AllSubCategories: allSubMap,
		})
	}))

	registerSSRConsolidateRoutes(mux, d)
	registerSSRProgressRoutes(mux, d)
	registerSSRPortRoutes(mux, d)

	mux.HandleFunc("/ui/modal/categories", getHX(func(w http.ResponseWriter, r *http.Request) {
		lang := requestLang(r)
		presets := discovery.NormalizePresetCategoriesJSON(d.Memory.GetConfigMap(r.Context())["preset_categories"])
		writeHTMLTemplate(w, "modal_categories", ssrCategoriesModalData{
			Lang:        lang,
			PresetsJSON: presets,
		})
	}))

	mux.HandleFunc("/ui/categories/save", postHX(func(w http.ResponseWriter, r *http.Request) {
		lang := requestLang(r)
		val := discovery.NormalizePresetCategoriesJSON(r.FormValue("preset_categories"))
		if err := d.Memory.SaveConfig(domain.WithAdminBypass(r.Context()), map[string]string{"preset_categories": val}); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		renderMemoriesAfterMutation(w, r, d, lang, "Category presets saved", true, false)
	}))

	mux.HandleFunc("/ui/engines", getHX(func(w http.ResponseWriter, r *http.Request) {
		renderEnginesSection(w, r, d, requestLang(r), r.URL.Query(), "", "")
	}))

	mux.HandleFunc("/ui/engines/download-box", getHX(func(w http.ResponseWriter, r *http.Request) {
		lang := requestLang(r)
		view := buildEnginesView(r.Context(), d, lang, r.URL.Query(), "", "")
		if !view.LocalModels.Download.Active {
			if b, err := json.Marshal(map[string]any{"memorizeRefreshEngines": true}); err == nil {
				w.Header().Set("HX-Trigger", escapeJSONASCII(b))
			}
		}
		writeHTMLTemplate(w, "engine_download_box", view)
	}))

	mux.HandleFunc("/ui/engines/discover-gemini", postHX(func(w http.ResponseWriter, r *http.Request) {
		lang := requestLang(r)
		q := url.Values{"view": {"gemini"}, "discover_gemini": {"1"}, "gtier": {r.FormValue("tier")}}
		_, _, apiErr := discovery.DiscoverGeminiModels(r.Context(), d.Memory.GetConfigMap(r.Context()), r.FormValue("gemini_api_key"))
		if apiErr != "" {
			setHXTriggers(w, apiErr, false, false)
		}
		renderEnginesSection(w, r, d, lang, q, r.FormValue("gemini_api_key"), "")
	}))

	mux.HandleFunc("/ui/engines/discover-jev", postHX(func(w http.ResponseWriter, r *http.Request) {
		lang := requestLang(r)
		q := url.Values{"view": {"jev"}, "discover_jev": {"1"}}
		disc := discovery.DiscoverSystemOneEndpoint(r.Context(), d.Memory.GetConfigMap(r.Context()), r.FormValue("jev_api_url"), r.FormValue("typesafe_api_key"))
		if disc.APIError != "" {
			setHXTriggers(w, disc.APIError, false, false)
		}
		renderEnginesSection(w, r, d, lang, q, r.FormValue("typesafe_api_key"), r.FormValue("jev_api_url"))
	}))

	mux.HandleFunc("/ui/engines/save", postHX(func(w http.ResponseWriter, r *http.Request) {
		lang := requestLang(r)
		prov := strings.ToLower(strings.TrimSpace(r.FormValue("provider")))
		pairs := map[string]string{"active_provider": prov}
		for _, k := range []string{"gguf_endpoint", "gemini_model", "jev_api_url", "jev_model", "local_model_url", "local_model_name"} {
			if v := strings.TrimSpace(r.FormValue(k)); v != "" {
				pairs[k] = v
			}
		}
		for _, secretKey := range []string{"gemini_api_key", "typesafe_api_key", "local_api_key"} {
			if v := strings.TrimSpace(r.FormValue(secretKey)); v != "" {
				pairs[secretKey] = v
			}
		}
		if _, hasGen := r.Form["gen_provider"]; hasGen {
			if err := discovery.ValidateAndApplyRoutingPairs(r.FormValue("gen_provider"), r.FormValue("gen_model"), true, nil, pairs); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
		}
		if err := d.Memory.SaveConfig(r.Context(), pairs); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		msg := fmt.Sprintf("Saved & applied engine: %s", strings.ToUpper(prov))
		if lang == "ko" {
			msg = fmt.Sprintf("추론 엔진 저장 완료: %s", strings.ToUpper(prov))
		}
		setHXTriggers(w, msg, false, false)
		q := url.Values{"view": {prov}}
		renderEnginesSection(w, r, d, lang, q, "", "")
	}))

	mux.HandleFunc("/ui/engines/routing", postHX(func(w http.ResponseWriter, r *http.Request) {
		lang := requestLang(r)
		_ = r.ParseForm()
		mode := strings.TrimSpace(r.FormValue("mode"))
		pairs := make(map[string]string)
		if mode == "reset_all" {
			for _, spec := range domain.AllEvalTasks() {
				pairs[spec.ProviderKey] = ""
				pairs[spec.ModelKey] = ""
			}
		} else {
			routes := make(map[domain.EvalTask][2]string, len(domain.AllEvalTasks()))
			for _, spec := range domain.AllEvalTasks() {
				p := strings.TrimSpace(r.FormValue(spec.ProviderKey))
				m := strings.TrimSpace(r.FormValue(spec.ModelKey))
				if p == "" && strings.HasPrefix(strings.ToLower(m), "gemini-decision") {
					// Keep explicit decision-only model submission intact so validation rejects it on generative tasks cleanly.
				} else if p == "" {
					m = ""
				}
				routes[spec.Task] = [2]string{p, m}
			}
			_, applyGen := r.Form["gen_provider"]
			if err := discovery.ValidateAndApplyRoutingPairs(r.FormValue("gen_provider"), r.FormValue("gen_model"), applyGen, routes, pairs); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
		}
		if err := d.Memory.SaveConfig(r.Context(), pairs); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		msg := "Feature model routing saved"
		if mode == "reset_all" {
			msg = "All features reset to bulk default engine"
			if lang == "ko" {
				msg = "모든 기능을 일괄 기본 엔진으로 되돌렸습니다"
			}
		} else if lang == "ko" {
			msg = "기능별 모델 라우팅을 저장했습니다"
		}
		setHXTriggers(w, msg, false, false)
		q := url.Values{}
		if v := strings.TrimSpace(r.FormValue("view")); v != "" {
			q.Set("view", v)
		}
		if tw := strings.TrimSpace(r.FormValue("tw")); tw != "" {
			q.Set("tw", tw)
		}
		renderEnginesSection(w, r, d, lang, q, "", "")
	}))

	mux.HandleFunc("/ui/engines/telemetry/reset", postHX(func(w http.ResponseWriter, r *http.Request) {
		lang := requestLang(r)
		if err := d.Memory.Router().ResetTelemetry(r.Context()); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		msg := "Evaluator telemetry reset"
		if lang == "ko" {
			msg = "추론 엔진 텔레메트리를 초기화했습니다"
		}
		setHXTriggers(w, msg, false, false)
		q := url.Values{}
		if v := strings.TrimSpace(r.FormValue("view")); v != "" {
			q.Set("view", v)
		}
		if tw := strings.TrimSpace(r.FormValue("tw")); tw != "" {
			q.Set("tw", tw)
		}
		renderEnginesSection(w, r, d, lang, q, "", "")
	}))

	mux.HandleFunc("/ui/models/download", postHX(func(w http.ResponseWriter, r *http.Request) {
		lang := requestLang(r)
		action := strings.TrimSpace(r.FormValue("action"))
		var toast string
		switch action {
		case "cancel":
			d.LocalModel.CancelDownload()
			toast = "Download cancelled"
		case "runtime":
			if err := d.LocalModel.StartRuntimeInstall(); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			toast = "Installing llama-server runtime..."
		default:
			fname, err := d.LocalModel.StartModelDownload(r.FormValue("preset_id"), r.FormValue("custom_url"))
			if err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			toast = "Downloading " + fname
		}
		setHXTriggers(w, toast, false, false)
		renderEnginesSection(w, r, d, lang, url.Values{"view": {"gguf"}}, "", "")
	}))

	mux.HandleFunc("/ui/models/server", postHX(func(w http.ResponseWriter, r *http.Request) {
		lang := requestLang(r)
		action := strings.TrimSpace(r.FormValue("action"))
		model := strings.TrimSpace(r.FormValue("model"))
		var toast string
		switch action {
		case "start":
			port, _ := strconv.Atoi(strings.TrimSpace(r.FormValue("port")))
			if port < 0 || port > 65535 {
				port = 0
			}
			if port == 0 {
				port = config.LoadPortConfig(d.Memory.DBPath()).GGUFPort
			}
			ep, err := d.LocalModel.StartServer(r.Context(), model, port)
			if err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			if r.FormValue("activate") == "1" {
				_ = d.Memory.SaveConfig(domain.WithAdminBypass(r.Context()), map[string]string{
					"active_provider": "gguf",
					"gguf_endpoint":   ep,
					"gguf_model":      model,
				})
			} else {
				_ = config.SyncManagedGGUFEndpoint(r.Context(), d.Memory.DB(), d.Memory.DBPath(), ep)
			}
			toast = "Activated " + d.Memory.ProviderName()
		case "stop":
			if err := d.LocalModel.StopServer(); err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			toast = "Local server stopped"
		case "delete":
			if err := d.LocalModel.DeleteModel(model); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			toast = "Deleted " + model
		default:
			http.Error(w, "unknown server action", http.StatusBadRequest)
			return
		}
		setHXTriggers(w, toast, false, false)
		renderEnginesSection(w, r, d, lang, url.Values{"view": {"gguf"}}, "", "")
	}))

	registerSSRCalibrateRoutes(mux, d)
}
