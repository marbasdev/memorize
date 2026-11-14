package dashboard

import (
	"context"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"memorize/internal/domain"
)

type ssrGuardrailLogsData struct {
	Lang       string
	OOB        bool
	Query      domain.GuardrailLogQuery
	Scopes     []string
	Page       domain.GuardrailLogPage
	Retention  domain.RetentionPolicy
	PageNums   []string
	PageSizes  []int
	PageInfo   string
	IsFiltered bool
}

func parseGuardrailQuery(q url.Values) domain.GuardrailLogQuery {
	st := strings.ToLower(strings.TrimSpace(q.Get("gr_state")))
	if st == "" {
		st = strings.ToLower(strings.TrimSpace(q.Get("state")))
	}
	if st == "" {
		st = "all"
	}
	src := strings.TrimSpace(q.Get("gr_source"))
	if src == "" {
		src = strings.TrimSpace(q.Get("source"))
	}
	sc := strings.TrimSpace(q.Get("gr_scope"))
	if _, hasGrScope := q["gr_scope"]; !hasGrScope {
		sc = strings.TrimSpace(q.Get("scope"))
	}
	page, _ := strconv.Atoi(firstNonEmptyStr(q.Get("gr_page"), q.Get("page")))
	pageSize, _ := strconv.Atoi(firstNonEmptyStr(q.Get("gr_page_size"), q.Get("page_size")))
	if page <= 0 {
		page = 1
	}
	if pageSize <= 0 {
		pageSize = 10
	}
	return domain.GuardrailLogQuery{
		Scope:    sc,
		State:    st,
		Source:   src,
		Page:     page,
		PageSize: pageSize,
	}
}

func firstNonEmptyStr(vals ...string) string {
	for _, v := range vals {
		if t := strings.TrimSpace(v); t != "" {
			return t
		}
	}
	return ""
}

func formatGuardrailPageInfo(lang string, p domain.GuardrailLogPage, isFiltered bool) string {
	if p.FilteredCount == 0 {
		if lang == "ko" {
			if isFiltered && p.TotalCount > 0 {
				return fmt.Sprintf("조건 일치 기록 0건 (스코프 전체 %d건)", p.TotalCount)
			}
			return "검증 기록 0건"
		}
		if isFiltered && p.TotalCount > 0 {
			return fmt.Sprintf("0 matching logs (out of %d)", p.TotalCount)
		}
		return "0 verification logs"
	}
	start := (p.Page-1)*p.PageSize + 1
	end := min(p.FilteredCount, p.Page*p.PageSize)
	if lang == "ko" {
		if isFiltered && p.FilteredCount != p.TotalCount {
			return fmt.Sprintf("필터 결과 %d건 중 %d–%d 표시 (전체 %d건)", p.FilteredCount, start, end, p.TotalCount)
		}
		return fmt.Sprintf("총 %d건 중 %d–%d 표시", p.FilteredCount, start, end)
	}
	if isFiltered && p.FilteredCount != p.TotalCount {
		return fmt.Sprintf("Showing %d–%d of %d (out of %d)", start, end, p.FilteredCount, p.TotalCount)
	}
	return fmt.Sprintf("Showing %d–%d of %d", start, end, p.FilteredCount)
}

func resolveKnownScopes(ctx context.Context, d Deps, extra ...string) []string {
	facets, _ := d.Memory.GetDashboardFacets(ctx)
	scopes := facets.Scopes
	if len(scopes) == 0 {
		scopes = []string{"global", "personal"}
	}
	for _, ex := range extra {
		ex = strings.TrimSpace(ex)
		if ex == "" {
			continue
		}
		found := false
		for _, s := range scopes {
			if s == ex {
				found = true
				break
			}
		}
		if !found {
			scopes = append([]string{ex}, scopes...)
		}
	}
	return scopes
}

func buildGuardrailLogsView(ctx context.Context, d Deps, lang string, q url.Values, oob bool) (ssrGuardrailLogsData, error) {
	gq := parseGuardrailQuery(q)
	pageRes, err := d.Guardrail.QueryGuardrailLogsPage(ctx, gq)
	if err != nil {
		return ssrGuardrailLogsData{}, err
	}
	scopes := resolveKnownScopes(ctx, d, gq.Scope)
	isFiltered := (gq.State != "" && gq.State != "all") || gq.Source != ""
	return ssrGuardrailLogsData{
		Lang:       lang,
		OOB:        oob,
		Query:      gq,
		Scopes:     scopes,
		Page:       pageRes,
		Retention:  d.Guardrail.GetRetentionPolicy(ctx),
		PageNums:   buildPageNums(pageRes.Page, pageRes.TotalPages),
		PageSizes:  []int{10, 20, 50},
		PageInfo:   formatGuardrailPageInfo(lang, pageRes, isFiltered),
		IsFiltered: isFiltered,
	}, nil
}

func buildConfHistSectionData(ctx context.Context, d Deps, lang, calScope string, scopes []string) ssrCalibrateData {
	sc := strings.TrimSpace(calScope)
	if len(scopes) == 0 {
		scopes = resolveKnownScopes(ctx, d, sc)
	}
	stats, _ := d.Memory.GetDashboardStatsForScope(ctx, sc)
	return ssrCalibrateData{
		Lang:            lang,
		Thresholds:      d.Memory.GetThresholds(ctx),
		HistScope:       sc,
		HistActiveCount: stats.ActiveCount,
		Histogram:       buildHistogramData(stats.ConfidenceBins, false),
		Scopes:          scopes,
	}
}

// buildGuardrailsView builds SSR state for the Guardrails tab (audit logs and plan violation checker).
func buildGuardrailsView(ctx context.Context, d Deps, lang string, q url.Values, scopes []string) ssrGuardrailsData {
	if len(scopes) == 0 {
		scopes = resolveKnownScopes(ctx, d)
	}
	grQuery := url.Values{}
	for k, v := range q {
		if strings.HasPrefix(k, "gr_") {
			grQuery[k] = v
		}
	}
	grLogs, _ := buildGuardrailLogsView(ctx, d, lang, grQuery, false)
	if len(grLogs.Scopes) > 0 {
		scopes = grLogs.Scopes
	}
	return ssrGuardrailsData{
		Lang:          lang,
		Scopes:        scopes,
		GuardrailLogs: grLogs,
	}
}

// buildCalibrateView builds SSR state for the Calibration & Test tab (thresholds, confidence histogram, relevance probe).
func buildCalibrateView(ctx context.Context, d Deps, lang string, q url.Values, scopes []string) ssrCalibrateData {
	calScope := strings.TrimSpace(q.Get("cal_scope"))
	return buildConfHistSectionData(ctx, d, lang, calScope, scopes)
}

func registerSSRCalibrateRoutes(mux *http.ServeMux, d Deps) {
	mux.HandleFunc("/ui/guardrails", getHX(func(w http.ResponseWriter, r *http.Request) {
		lang := requestLang(r)
		view := buildGuardrailsView(r.Context(), d, lang, r.URL.Query(), nil)
		writeHTMLTemplate(w, "guardrails_section", view)
	}))

	mux.HandleFunc("/ui/calibrate", getHX(func(w http.ResponseWriter, r *http.Request) {
		lang := requestLang(r)
		view := buildCalibrateView(r.Context(), d, lang, r.URL.Query(), nil)
		writeHTMLTemplate(w, "calibrate_section", view)
	}))

	mux.HandleFunc("/ui/calibrate/histogram", getHX(func(w http.ResponseWriter, r *http.Request) {
		lang := requestLang(r)
		q := r.URL.Query()
		calScope := q.Get("cal_scope")
		if _, ok := q["cal_scope"]; !ok {
			calScope = q.Get("scope")
		}
		view := buildConfHistSectionData(r.Context(), d, lang, calScope, nil)
		writeHTMLTemplate(w, "conf_hist_section", view)
	}))

	mux.HandleFunc("/ui/guardrail-logs", getHX(func(w http.ResponseWriter, r *http.Request) {
		lang := requestLang(r)
		view, err := buildGuardrailLogsView(r.Context(), d, lang, r.URL.Query(), false)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeHTMLTemplate(w, "guardrail_logs_section", view)
	}))

	mux.HandleFunc("/ui/guardrail-logs/retention", postHX(func(w http.ResponseWriter, r *http.Request) {
		lang := requestLang(r)
		days, err := strconv.Atoi(strings.TrimSpace(r.FormValue("retention_days")))
		if err != nil {
			http.Error(w, "invalid retention_days", http.StatusBadRequest)
			return
		}
		pol, deleted, err := d.Guardrail.SetGuardrailRetentionPolicy(r.Context(), days, nil)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		msg := fmt.Sprintf("Guardrail log policy saved (%s)", formatRetentionLabel(lang, pol.GuardrailDays))
		if deleted > 0 {
			msg = fmt.Sprintf("Saved (%s) · cleaned %d expired logs", formatRetentionLabel(lang, pol.GuardrailDays), deleted)
		}
		if lang == "ko" {
			msg = fmt.Sprintf("가드레일 기록 보관 정책 저장 (%s)", formatRetentionLabel(lang, pol.GuardrailDays))
			if deleted > 0 {
				msg = fmt.Sprintf("보관 정책 저장 (%s) · 만료 기록 %d건 자동 정리", formatRetentionLabel(lang, pol.GuardrailDays), deleted)
			}
		}
		if r.Form == nil {
			r.Form = url.Values{}
		}
		r.Form.Set("gr_page", "1")
		view, err := buildGuardrailLogsView(r.Context(), d, lang, r.Form, false)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		setHXTriggers(w, msg, false, false)
		writeHTMLTemplate(w, "guardrail_logs_section", view)
	}))

	mux.HandleFunc("/ui/guardrail-logs/cleanup", postHX(func(w http.ResponseWriter, r *http.Request) {
		lang := requestLang(r)
		sc := strings.TrimSpace(r.FormValue("gr_scope"))
		if _, has := r.Form["gr_scope"]; !has {
			sc = strings.TrimSpace(r.FormValue("scope"))
		}
		days := 7
		if raw := strings.TrimSpace(r.FormValue("older_than_days")); raw != "" {
			v, err := strconv.Atoi(raw)
			if err != nil || v < 0 {
				http.Error(w, "invalid older_than_days", http.StatusBadRequest)
				return
			}
			days = v
		}
		incBlocked := strings.TrimSpace(r.FormValue("include_blocked")) == "1"
		deleted, err := d.Guardrail.CleanupGuardrailLogs(r.Context(), sc, days, incBlocked)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		msg := fmt.Sprintf("Cleaned %d guardrail logs", deleted)
		if !incBlocked {
			msg = fmt.Sprintf("Cleaned %d PASS logs (blocked violations preserved)", deleted)
		}
		if lang == "ko" {
			if deleted == 0 {
				msg = "선택한 조건에 정리할 가드레일 기록이 없습니다"
			} else if incBlocked {
				msg = fmt.Sprintf("가드레일 검증 기록 %d건 정리 완료 (위반 · 차단 포함)", deleted)
			} else {
				msg = fmt.Sprintf("가드레일 통과(PASS) 기록 %d건 정리 완료 (위반 · 차단 이력 보존)", deleted)
			}
		}
		if r.Form == nil {
			r.Form = url.Values{}
		}
		r.Form.Set("gr_page", "1")
		view, err := buildGuardrailLogsView(r.Context(), d, lang, r.Form, false)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		setHXTriggers(w, msg, false, false)
		writeHTMLTemplate(w, "guardrail_logs_section", view)
	}))

	mux.HandleFunc("/ui/thresholds", postHX(func(w http.ResponseWriter, r *http.Request) {
		lang := requestLang(r)
		ctx := r.Context()
		var target domain.Thresholds
		toast := "Thresholds saved"
		if lang == "ko" {
			toast = "판단 임계값 저장 완료"
		}
		if r.FormValue("reset") == "1" {
			target = domain.DefaultThresholds()
			toast = "Thresholds reset"
			if lang == "ko" {
				toast = "임계값 기본값 복원 완료"
			}
		} else {
			pf := func(k string, def float64) float64 {
				if v, err := strconv.ParseFloat(strings.TrimSpace(r.FormValue(k)), 64); err == nil {
					return v
				}
				return def
			}
			cur := d.Memory.GetThresholds(ctx)
			target = domain.Thresholds{
				MinPermanent:     pf("min_permanent", cur.MinPermanent),
				MinConfidence:    pf("min_confidence", cur.MinConfidence),
				PinInvariant:     pf("pin_invariant", cur.PinInvariant),
				MinRelevance:     pf("min_relevance", cur.MinRelevance),
				MinRelConfidence: pf("min_rel_confidence", cur.MinRelConfidence),
			}
		}
		saved, err := d.Memory.SaveThresholds(ctx, target)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		calScope := r.FormValue("cal_scope")
		view := buildConfHistSectionData(ctx, d, lang, calScope, nil)
		view.Thresholds = saved
		setHXTriggers(w, toast, false, false)
		writeHTMLTemplate(w, "thresholds_card", view)
	}))

	mux.HandleFunc("/ui/calibrate-confidence", postHX(func(w http.ResponseWriter, r *http.Request) {
		ctx := domain.WithAdminBypass(r.Context())
		lang := requestLang(r)
		calScope := r.FormValue("cal_scope")
		if _, hasCal := r.Form["cal_scope"]; !hasCal {
			calScope = r.FormValue("scope")
		}
		updated, err := d.Memory.RecalibrateConfidence(ctx, calScope)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		scopeLabel := calScope
		if strings.TrimSpace(scopeLabel) == "" {
			if lang == "ko" {
				scopeLabel = "전체 스코프"
			} else {
				scopeLabel = "all scopes"
			}
		}
		toast := fmt.Sprintf("Recalibrated %d active memories (%s)", updated, scopeLabel)
		if lang == "ko" {
			toast = fmt.Sprintf("[%s] 활성 기억 %d건 개별 확신도 재산정 완료", scopeLabel, updated)
		}
		view := buildConfHistSectionData(ctx, d, lang, calScope, nil)
		body, err := renderSSRTemplate("conf_hist_section", view)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if memView, mErr := buildMemoriesView(r.Context(), d, lang, r.Form, true); mErr == nil {
			if memBody, rErr := renderSSRTemplate("memories_section", memView); rErr == nil {
				body = append(body, []byte("\n<div class=\"card\" id=\"memoriesSection\" hx-swap-oob=\"innerHTML\">")...)
				body = append(body, memBody...)
				body = append(body, []byte("</div>")...)
			}
		}
		setHXTriggers(w, toast, false, false)
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store, no-cache, must-revalidate")
		_, _ = w.Write(body)
	}))

	mux.HandleFunc("/ui/probe", postHX(func(w http.ResponseWriter, r *http.Request) {
		lang := requestLang(r)
		res, err := d.Memory.DiagnoseRelevance(r.Context(), r.FormValue("scope"), r.FormValue("query"))
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		passCnt := len(res.Pinned)
		scored := make([]ssrScoredProbeItem, 0, len(res.Scored))
		for _, s := range res.Scored {
			ok := s.Score >= res.Thresholds.MinRelevance && s.Confidence >= res.Thresholds.MinRelConfidence
			if ok {
				passCnt++
			}
			pct := math.Min(100, math.Max(2, (s.Score/5.0)*100.0))
			scored = append(scored, ssrScoredProbeItem{
				ID:         s.ID,
				Content:    s.Content,
				Score:      s.Score,
				Confidence: s.Confidence,
				Pass:       ok,
				Pct:        pct,
			})
		}
		writeHTMLTemplate(w, "probe_results", ssrProbeResultsData{
			Lang:       lang,
			ElapsedMs:  res.ElapsedMs,
			PassCount:  passCnt,
			TotalCount: len(res.Pinned) + len(res.Scored),
			Pinned:     res.Pinned,
			Scored:     scored,
		})
	}))

	mux.HandleFunc("/ui/verify", postHX(func(w http.ResponseWriter, r *http.Request) {
		lang := requestLang(r)
		ctx := domain.WithVerifySource(r.Context(), "ui:verify")
		out, err := d.Guardrail.VerifyPlan(ctx, r.FormValue("plan"), r.FormValue("scope"))
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		grLogs, _ := buildGuardrailLogsView(r.Context(), d, lang, r.Form, true)
		writeHTMLTemplate(w, "verify_results", ssrVerifyResultsData{
			Passed:        strings.HasPrefix(out, "PASS"),
			Output:        out,
			GuardrailLogs: grLogs,
		})
	}))
}
