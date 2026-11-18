package dashboard

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"sort"
	"strconv"
	"strings"

	"memorize/internal/domain"
	"memorize/internal/inference/discovery"
)

func buildPageNums(cur, total int) []string {
	if total <= 7 {
		out := make([]string, total)
		for i := 0; i < total; i++ {
			out[i] = strconv.Itoa(i + 1)
		}
		return out
	}
	out := []string{"1"}
	left := max(2, cur-1)
	right := min(total-1, cur+1)
	if left > 2 {
		out = append(out, "...")
	}
	for p := left; p <= right; p++ {
		out = append(out, strconv.Itoa(p))
	}
	if right < total-1 {
		out = append(out, "...")
	}
	out = append(out, strconv.Itoa(total))
	return out
}

func formatPageInfo(lang string, page domain.RecordPage, isFiltered bool) string {
	if page.FilteredRecords == 0 {
		if lang == "ko" {
			if isFiltered {
				return fmt.Sprintf("조건에 맞는 기억 0건 (전체 %d건)", page.TotalRecords)
			}
			return "기억 0건"
		}
		if isFiltered {
			return fmt.Sprintf("0 matching memories (filtered from %d)", page.TotalRecords)
		}
		return "0 memories"
	}
	start := (page.Page-1)*page.PageSize + 1
	end := min(page.FilteredRecords, page.Page*page.PageSize)
	if lang == "ko" {
		if isFiltered {
			return fmt.Sprintf("검색 결과 %d건 중 %d–%d 표시 (전체 %d건)", page.FilteredRecords, start, end, page.TotalRecords)
		}
		return fmt.Sprintf("총 %d건 중 %d–%d 표시", page.TotalRecords, start, end)
	}
	if isFiltered {
		return fmt.Sprintf("Showing %d–%d of %d (filtered from %d)", start, end, page.FilteredRecords, page.TotalRecords)
	}
	return fmt.Sprintf("Showing %d–%d of %d", start, end, page.TotalRecords)
}

func mergeCategoriesAndSubs(presetJSON string, facets domain.DashboardFacets) ([]string, map[string][]string) {
	var presetMap map[string][]string
	_ = json.Unmarshal([]byte(discovery.NormalizePresetCategoriesJSON(presetJSON)), &presetMap)
	catSet := map[string]bool{}
	subMap := map[string]map[string]bool{}
	for c, subs := range presetMap {
		catSet[c] = true
		k := strings.ToLower(c)
		if subMap[k] == nil {
			subMap[k] = map[string]bool{}
		}
		for _, s := range subs {
			subMap[k][s] = true
		}
	}
	for _, c := range facets.Categories {
		catSet[c] = true
	}
	for cKey, subs := range facets.SubCategories {
		if subMap[cKey] == nil {
			subMap[cKey] = map[string]bool{}
		}
		for _, s := range subs {
			subMap[cKey][s] = true
		}
	}
	cats := make([]string, 0, len(catSet))
	for c := range catSet {
		if c != "" {
			cats = append(cats, c)
		}
	}
	sort.Strings(cats)

	outSubs := make(map[string][]string, len(subMap))
	for k, set := range subMap {
		list := make([]string, 0, len(set))
		for s := range set {
			if s != "" {
				list = append(list, s)
			}
		}
		sort.Strings(list)
		outSubs[k] = list
	}
	return cats, outSubs
}

func buildMemoriesView(ctx context.Context, d Deps, lang string, q url.Values, oob bool) (ssrMemoriesData, error) {
	rq := parseRecordQuery(q)
	if rq.State == "" {
		rq.State = "active"
	}
	if rq.PageSize <= 0 {
		rq.PageSize = 20
	}
	stats, err := d.Memory.GetDashboardStatsForScope(ctx, rq.Scope)
	if err != nil {
		return ssrMemoriesData{}, err
	}
	dist, _ := d.Memory.GetCategoryDistribution(ctx, rq.Scope)
	facets, err := d.Memory.GetDashboardFacets(ctx)
	if err != nil {
		return ssrMemoriesData{}, err
	}
	pageRes, err := d.Memory.QueryRecordsPage(ctx, rq)
	if err != nil {
		return ssrMemoriesData{}, err
	}
	cfg := d.Memory.GetConfigMap(ctx)
	cats, allSubMap := mergeCategoriesAndSubs(cfg["preset_categories"], facets)

	var subList []string
	if rq.Category != "" {
		subList = allSubMap[strings.ToLower(rq.Category)]
	} else {
		seen := map[string]bool{}
		for _, list := range allSubMap {
			for _, s := range list {
				if !seen[s] {
					seen[s] = true
					subList = append(subList, s)
				}
			}
		}
		sort.Strings(subList)
	}

	var activeIDs []int64
	for _, r := range pageRes.Records {
		if r.Active {
			activeIDs = append(activeIDs, r.ID)
		}
	}

	hasCustomFilter := rq.Scope != "" || rq.SessionID != "" || rq.Account != "" ||
		rq.Category != "" || rq.SubCategory != "" || strings.TrimSpace(rq.Search) != "" ||
		(rq.State != "" && rq.State != "active")
	isFiltered := hasCustomFilter || rq.State != "all"

	scopes := facets.Scopes
	if len(scopes) == 0 {
		scopes = []string{"global", "personal"}
	}

	return ssrMemoriesData{
		Lang:             lang,
		OOB:              oob,
		ServerMode:       domain.IsServerMode(ctx),
		CanAdmin:         domain.CanAccessAdminFeatures(ctx),
		Stats:            stats,
		GeneralCount:     max(0, stats.ActiveCount-stats.PinnedCount),
		Histogram:        buildHistogramData(stats.ConfidenceBins, oob),
		Distribution:     dist,
		Query:            rq,
		HasCustomFilter:  hasCustomFilter,
		IncludePersonal:  cfg["include_personal_in_project"] == "1" || (cfg["include_personal_in_project"] == "" && discovery.EnvOrDefault("", "MEMORY_INCLUDE_PERSONAL", "0") == "1"),
		CurrentAccount:   d.Memory.CurrentAccount(ctx),
		Scopes:           scopes,
		Sessions:         facets.Sessions,
		Accounts:         facets.Accounts,
		Categories:       cats,
		SubCategories:    subList,
		AllSubCategories: allSubMap,
		Page:             pageRes,
		PageActiveIDs:    activeIDs,
		PageNums:         buildPageNums(pageRes.Page, pageRes.TotalPages),
		PageSizes:        []int{10, 20, 50, 100},
		PageInfo:         formatPageInfo(lang, pageRes, isFiltered),
		HighlightTerms:   extractHighlightTerms(rq.Search),
	}, nil
}
