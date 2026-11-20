package dashboard

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"

	"praxis/internal/domain"
)

type ssrProgressData struct {
	Lang           string
	ServerMode     bool
	CanAdmin       bool
	Scope          string
	Scopes         []string
	KindFilter     string
	TrackFilter    string
	CategoryFilter string
	DoneWindow     string
	Overview       domain.ProgressOverview
	KindAllTotal   int
	WorkTotal      int
	MemoryTotal    int
	GitTotal       int
	RetentionDays  int // Auto-TTL retention days for progress history
	PageSizes      []int
	PageNums       []string
	PageInfo       string
}

func orderedProgressScopes(ctx context.Context, d Deps, facets domain.DashboardFacets) ([]string, string) {
	seen := map[string]bool{}
	var projects, others []string
	for _, sc := range facets.Scopes {
		sc = strings.TrimSpace(sc)
		if sc == "" || seen[sc] {
			continue
		}
		seen[sc] = true
		if strings.HasPrefix(sc, "project:") {
			projects = append(projects, sc)
		} else {
			others = append(others, sc)
		}
	}
	if topProj := d.Progress.TopProjectScope(ctx); topProj != "" && !seen[topProj] {
		seen[topProj] = true
		projects = append([]string{topProj}, projects...)
	}
	sort.Strings(projects)
	sort.Strings(others)
	all := append(projects, others...)
	if len(all) == 0 {
		all = []string{"global"}
	}
	defScope := d.Progress.TopProjectScope(ctx)
	if defScope == "" {
		defScope = all[0]
	}
	return all, defScope
}

func formatProgressPageInfo(lang string, ov domain.ProgressOverview, isFiltered bool) string {
	if ov.FilteredCount == 0 {
		if lang == "ko" {
			if isFiltered {
				return fmt.Sprintf("조건에 맞는 기록 0건 (전체 %d건)", ov.TotalCount)
			}
			return "진행 기록 0건"
		}
		if isFiltered {
			return fmt.Sprintf("0 matching entries (filtered from %d)", ov.TotalCount)
		}
		return "0 entries"
	}
	start := (ov.Page-1)*ov.PageSize + 1
	end := min(ov.FilteredCount, ov.Page*ov.PageSize)
	if lang == "ko" {
		if isFiltered {
			return fmt.Sprintf("필터 결과 %d건 중 %d–%d 표시 (전체 %d건)", ov.FilteredCount, start, end, ov.TotalCount)
		}
		return fmt.Sprintf("총 %d건 중 %d–%d 표시", ov.TotalCount, start, end)
	}
	if isFiltered {
		return fmt.Sprintf("Showing %d–%d of %d (filtered from %d)", start, end, ov.FilteredCount, ov.TotalCount)
	}
	return fmt.Sprintf("Showing %d–%d of %d", start, end, ov.TotalCount)
}

func buildProgressView(ctx context.Context, d Deps, lang string, q url.Values) (ssrProgressData, error) {
	facets, err := d.Memory.GetDashboardFacets(ctx)
	if err != nil {
		return ssrProgressData{}, err
	}
	scopes, _ := orderedProgressScopes(ctx, d, facets)
	rawSc := strings.TrimSpace(q.Get("scope"))
	sc := ""
	if rawSc != "" && !strings.EqualFold(rawSc, "all") {
		sc = domain.NormalizeScope(rawSc)
	}
	if sc != "" {
		hasScope := false
		for _, s := range scopes {
			if s == sc {
				hasScope = true
				break
			}
		}
		if !hasScope {
			scopes = append([]string{sc}, scopes...)
		}
	}

	kindF := strings.ToLower(strings.TrimSpace(q.Get("kind")))
	if kindF == "all" {
		kindF = ""
	}
	trkF := strings.TrimSpace(q.Get("track"))
	catF := strings.TrimSpace(q.Get("category"))
	doneWin := domain.NormalizeDoneWindow(q.Get("done_window"))

	page, _ := strconv.Atoi(strings.TrimSpace(q.Get("page")))
	if page <= 0 {
		page = 1
	}
	pageSize, _ := strconv.Atoi(strings.TrimSpace(q.Get("page_size")))
	switch pageSize {
	case 10, 20, 50:
	default:
		pageSize = 10
	}

	ov, err := d.Progress.GetProgressOverviewWithQuery(ctx, domain.ProgressQuery{
		Scope:      sc,
		Kind:       kindF,
		Track:      trkF,
		Category:   catF,
		DoneWindow: doneWin,
		Page:       page,
		PageSize:   pageSize,
	})
	if err != nil {
		return ssrProgressData{}, err
	}
	isFiltered := kindF != "" || trkF != "" || catF != ""
	workCnt := ov.KindCounts[domain.ProgressKindWork]
	memCnt := ov.KindCounts[domain.ProgressKindMemory]
	gitCnt := ov.KindCounts[domain.ProgressKindGit]

	return ssrProgressData{
		Lang:           lang,
		ServerMode:     domain.IsServerMode(ctx),
		CanAdmin:       domain.CanAccessAdminFeatures(ctx),
		Scope:          sc,
		Scopes:         scopes,
		KindFilter:     kindF,
		TrackFilter:    trkF,
		CategoryFilter: catF,
		DoneWindow:     doneWin,
		Overview:       ov,
		KindAllTotal:   workCnt + memCnt + gitCnt,
		WorkTotal:      workCnt,
		MemoryTotal:    memCnt,
		GitTotal:       gitCnt,
		RetentionDays:  d.Progress.GetRetentionPolicy(ctx).ProgressDays,
		PageSizes:      []int{10, 20, 50},
		PageNums:       buildPageNums(ov.Page, ov.TotalPages),
		PageInfo:       formatProgressPageInfo(lang, ov, isFiltered),
	}, nil
}

func formatRetentionLabel(lang string, days int) string {
	if days <= 0 {
		if lang == "ko" {
			return "영구 보관"
		}
		return "forever"
	}
	if lang == "ko" {
		return fmt.Sprintf("%d일 자동 보관", days)
	}
	return fmt.Sprintf("%dd auto-retention", days)
}

func renderProgressAfterMutation(w http.ResponseWriter, r *http.Request, d Deps, lang, toast string, q url.Values) {
	view, err := buildProgressView(r.Context(), d, lang, q)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	setHXTriggers(w, toast, false, false)
	writeHTMLTemplate(w, "progress_section", view)
}

func registerSSRProgressRoutes(mux *http.ServeMux, d Deps) {
	mux.HandleFunc("/ui/progress", getHX(func(w http.ResponseWriter, r *http.Request) {
		lang := requestLang(r)
		view, err := buildProgressView(r.Context(), d, lang, r.URL.Query())
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeHTMLTemplate(w, "progress_section", view)
	}))

	mux.HandleFunc("/ui/progress/retention", postHX(requireAdminRoute(func(w http.ResponseWriter, r *http.Request) {
		lang := requestLang(r)
		days, err := strconv.Atoi(strings.TrimSpace(r.FormValue("retention_days")))
		if err != nil {
			http.Error(w, "invalid retention_days", http.StatusBadRequest)
			return
		}
		pol, deleted, err := d.Progress.SetProgressRetentionDays(r.Context(), days)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		msg := formatRetentionToast(lang, "Progress history policy saved", "작업 히스토리 보관 정책 저장", pol.ProgressDays, deleted)
		r.Form.Set("page", "1")
		renderProgressAfterMutation(w, r, d, lang, msg, r.Form)
	})))

	mux.HandleFunc("/ui/progress/cleanup", postHX(requireAdminRoute(func(w http.ResponseWriter, r *http.Request) {
		lang := requestLang(r)
		sc := strings.TrimSpace(r.FormValue("scope"))
		days := 7
		if raw := strings.TrimSpace(r.FormValue("older_than_days")); raw != "" {
			v, err := strconv.Atoi(raw)
			if err != nil || v < 0 {
				http.Error(w, "invalid older_than_days", http.StatusBadRequest)
				return
			}
			days = v
		}
		deleted, err := d.Progress.CleanupProgressHistory(r.Context(), sc, days)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		koMsg := fmt.Sprintf("진행 히스토리 %d건 정리 완료", deleted)
		if deleted == 0 {
			koMsg = "선택한 기간 이전에 정리할 기록이 없습니다"
		}
		msg := langText(lang, fmt.Sprintf("Cleaned %d progress history entries", deleted), koMsg)
		r.Form.Set("page", "1")
		renderProgressAfterMutation(w, r, d, lang, msg, r.Form)
	})))

	mux.HandleFunc("/ui/progress/sync", postHX(requireAdminRoute(func(w http.ResponseWriter, r *http.Request) {
		lang := requestLang(r)
		sc := strings.TrimSpace(r.FormValue("scope"))
		var (
			res domain.ProgressSyncResult
			err error
		)
		if sc == "" || strings.EqualFold(sc, "all") {
			res, err = d.Progress.SyncAllProjectsProgress(r.Context(), lang)
			r.Form.Set("scope", "")
		} else {
			res, err = d.Progress.SyncProjectProgress(r.Context(), sc, "", lang)
			if err == nil {
				r.Form.Set("scope", res.Scope)
			}
		}
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		msg := langText(lang,
			fmt.Sprintf("Synced (%d memories, %d commits)", res.SyncedMemories, res.SyncedCommits),
			fmt.Sprintf("동기화 완료 (기억 %d건 · 커밋 %d건 반영)", res.SyncedMemories, res.SyncedCommits),
		)
		renderProgressAfterMutation(w, r, d, lang, msg, r.Form)
	})))
}
