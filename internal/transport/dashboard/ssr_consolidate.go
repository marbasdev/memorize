package dashboard

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"

	"memorize/internal/domain"
)

func resolveConsolidateScope(ctx context.Context, d Deps, q url.Values, scopes []string) string {
	for _, key := range []string{"merge_scope", "scope"} {
		if sc := strings.TrimSpace(q.Get(key)); sc != "" {
			return sc
		}
	}
	if top := d.Memory.TopActiveScope(domain.WithAdminBypass(ctx)); top != "" {
		return top
	}
	if len(scopes) > 0 {
		return scopes[0]
	}
	return "global"
}

func buildConsolidateModalView(ctx context.Context, d Deps, lang string, q url.Values, manualIDs []int64) (ssrConsolidateModalData, error) {
	facets, err := d.Memory.GetDashboardFacets(ctx)
	if err != nil {
		return ssrConsolidateModalData{}, err
	}
	scopes := facets.Scopes
	if len(scopes) == 0 {
		scopes = []string{"global"}
	}

	if len(manualIDs) >= 2 {
		recs, err := d.Memory.GetRecordsByIDs(ctx, manualIDs)
		if err != nil {
			return ssrConsolidateModalData{}, err
		}
		var active []domain.MemoryRecord
		picked := map[int64]bool{}
		pinnedAny := false
		for _, r := range recs {
			if r.Active {
				active = append(active, r)
				picked[r.ID] = true
				if r.Pinned {
					pinnedAny = true
				}
			}
		}
		sort.Slice(active, func(i, j int) bool { return active[i].ID < active[j].ID })
		if len(active) < 2 {
			return ssrConsolidateModalData{}, fmt.Errorf("select at least 2 active memories")
		}
		return ssrConsolidateModalData{
			Lang:        lang,
			ManualMode:  true,
			Scope:       active[0].Scope,
			Scopes:      scopes,
			Count:       len(active),
			MaxCount:    max(2, len(active)),
			Category:    active[0].Category,
			Pinned:      pinnedAny,
			Candidates:  active,
			PickedIDs:   picked,
			PickedCount: len(active),
		}, nil
	}

	sc := resolveConsolidateScope(ctx, d, q, scopes)
	hasScope := false
	for _, s := range scopes {
		if s == sc {
			hasScope = true
			break
		}
	}
	if !hasScope && sc != "" {
		scopes = append([]string{sc}, scopes...)
	}

	mode := strings.ToLower(strings.TrimSpace(q.Get("mode")))
	catFilter := strings.TrimSpace(q.Get("merge_category"))
	if catFilter == "" {
		catFilter = strings.TrimSpace(q.Get("category"))
	}
	if mode != "manual" {
		return ssrConsolidateModalData{
			Lang:     lang,
			AIMode:   true,
			Scope:    sc,
			Scopes:   scopes,
			Category: catFilter,
		}, nil
	}

	page, err := d.Memory.QueryRecordsPage(ctx, domain.RecordQuery{
		Scope:    sc,
		State:    "active",
		Page:     1,
		PageSize: 500,
	})
	if err != nil {
		return ssrConsolidateModalData{}, err
	}
	activeRecs := append([]domain.MemoryRecord(nil), page.Records...)
	sort.Slice(activeRecs, func(i, j int) bool { return activeRecs[i].ID < activeRecs[j].ID })

	bucketMap := map[string][]domain.MemoryRecord{}
	for _, r := range activeRecs {
		pinKey := "0"
		if r.Pinned {
			pinKey = "1"
		}
		key := r.Category + "|" + pinKey
		bucketMap[key] = append(bucketMap[key], r)
	}
	type pair struct {
		key  string
		list []domain.MemoryRecord
	}
	var pairs []pair
	for k, list := range bucketMap {
		if len(list) >= 2 {
			pairs = append(pairs, pair{k, list})
		}
	}
	sort.Slice(pairs, func(i, j int) bool {
		if len(pairs[i].list) != len(pairs[j].list) {
			return len(pairs[i].list) > len(pairs[j].list)
		}
		return pairs[i].key < pairs[j].key
	})

	out := ssrConsolidateModalData{
		Lang:      lang,
		Scope:     sc,
		Scopes:    scopes,
		Count:     2,
		MaxCount:  2,
		PickedIDs: map[int64]bool{},
	}
	if len(pairs) == 0 {
		return out, nil
	}

	selectedKey := strings.TrimSpace(q.Get("bucket"))
	foundIdx := 0
	for i, p := range pairs {
		parts := strings.SplitN(p.key, "|", 2)
		tierLbl := "Rule"
		if len(parts) == 2 && parts[1] == "1" {
			tierLbl = "Pinned"
		}
		out.Buckets = append(out.Buckets, ssrMergeBucket{
			Key:   p.key,
			Label: fmt.Sprintf("%s · %s (%d)", parts[0], tierLbl, len(p.list)),
		})
		if p.key == selectedKey {
			foundIdx = i
		}
	}
	chosen := pairs[foundIdx]
	out.SelectedBucket = chosen.key
	parts := strings.SplitN(chosen.key, "|", 2)
	out.Category = parts[0]
	out.Pinned = len(parts) == 2 && parts[1] == "1"
	out.Candidates = chosen.list
	out.MaxCount = max(2, len(chosen.list))

	cnt, _ := strconv.Atoi(strings.TrimSpace(q.Get("count")))
	if cnt < 2 {
		cnt = min(len(chosen.list), 3)
	}
	if cnt > len(chosen.list) {
		cnt = len(chosen.list)
	}
	out.Count = cnt
	for i := 0; i < cnt; i++ {
		out.PickedIDs[chosen.list[i].ID] = true
	}
	out.PickedCount = cnt
	return out, nil
}

func summarizeMergeCategories(proposals []domain.ConsolidationProposal) []ssrMergeCatSummary {
	if len(proposals) == 0 {
		return nil
	}
	byCat := make(map[string]*ssrMergeCatSummary)
	for _, p := range proposals {
		cat := strings.TrimSpace(p.Category)
		if cat == "" {
			cat = "general"
		}
		item := byCat[cat]
		if item == nil {
			item = &ssrMergeCatSummary{Category: cat}
			byCat[cat] = item
		}
		item.GroupCount++
		srcCnt := len(p.Sources)
		if srcCnt == 0 {
			srcCnt = len(p.SourceIDs)
		}
		item.SourceCount += srcCnt
	}
	out := make([]ssrMergeCatSummary, 0, len(byCat))
	for _, item := range byCat {
		out = append(out, *item)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].SourceCount != out[j].SourceCount {
			return out[i].SourceCount > out[j].SourceCount
		}
		return out[i].Category < out[j].Category
	})
	return out
}

func summarizeRebalanceCategories(items []domain.TaxonomyRebalanceItem) []ssrRebalanceCatSummary {
	if len(items) == 0 {
		return nil
	}
	counts := make(map[string]int)
	for _, it := range items {
		cat := strings.TrimSpace(it.NewCategory)
		if cat == "" {
			cat = strings.TrimSpace(it.OldCategory)
		}
		if cat == "" {
			cat = "general"
		}
		counts[cat]++
	}
	out := make([]ssrRebalanceCatSummary, 0, len(counts))
	for c, n := range counts {
		out = append(out, ssrRebalanceCatSummary{Category: c, Count: n})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Count != out[j].Count {
			return out[i].Count > out[j].Count
		}
		return out[i].Category < out[j].Category
	})
	return out
}

func parseRebalanceFormItems(r *http.Request) []domain.TaxonomyRebalanceItem {
	var items []domain.TaxonomyRebalanceItem
	for _, idxStr := range r.Form["item_idx"] {
		idxStr = strings.TrimSpace(idxStr)
		if idxStr == "" {
			continue
		}
		id, err := strconv.ParseInt(strings.TrimSpace(r.FormValue("item_"+idxStr+"_id")), 10, 64)
		if err != nil || id <= 0 {
			continue
		}
		items = append(items, domain.TaxonomyRebalanceItem{
			ID:             id,
			NewCategory:    strings.TrimSpace(r.FormValue("item_" + idxStr + "_cat")),
			NewSubCategory: strings.TrimSpace(r.FormValue("item_" + idxStr + "_sub")),
		})
	}
	return items
}

func registerSSRConsolidateRoutes(mux *http.ServeMux, d Deps) {
	mux.HandleFunc("/ui/modal/consolidate", getHX(func(w http.ResponseWriter, r *http.Request) {
		lang := requestLang(r)
		view, err := buildConsolidateModalView(r.Context(), d, lang, r.URL.Query(), nil)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		writeHTMLTemplate(w, "modal_consolidate", view)
	}))

	mux.HandleFunc("/ui/modal/consolidate-selected", postHX(func(w http.ResponseWriter, r *http.Request) {
		lang := requestLang(r)
		ids := parseFormIDs(r.Form)
		view, err := buildConsolidateModalView(r.Context(), d, lang, r.Form, ids)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		writeHTMLTemplate(w, "modal_consolidate", view)
	}))

	mux.HandleFunc("/ui/consolidate/suggest", postHX(func(w http.ResponseWriter, r *http.Request) {
		lang := requestLang(r)
		ctx := domain.WithAdminBypass(r.Context())
		sc := strings.TrimSpace(r.FormValue("merge_scope"))
		if sc == "" {
			sc = d.Memory.TopActiveScope(ctx)
		}
		catFilter := strings.TrimSpace(r.FormValue("merge_category"))
		plan, err := d.Memory.SuggestCleanupPlanFiltered(ctx, sc, catFilter)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		writeHTMLTemplate(w, "consolidate_ai_plan", ssrConsolidateModalData{
			Lang:                  lang,
			AIMode:                true,
			Scope:                 sc,
			Category:              catFilter,
			Proposals:             plan.Proposals,
			CatSummaries:          summarizeMergeCategories(plan.Proposals),
			RebalanceItems:        plan.RebalanceItems,
			RebalanceCatSummaries: summarizeRebalanceCategories(plan.RebalanceItems),
			TotalActionCount:      len(plan.Proposals) + len(plan.RebalanceItems),
			SourceLabel:           plan.SourceLabel,
		})
	}))

	mux.HandleFunc("/ui/taxonomy/suggest", postHX(func(w http.ResponseWriter, r *http.Request) {
		lang := requestLang(r)
		ctx := domain.WithAdminBypass(r.Context())
		sc := strings.TrimSpace(r.FormValue("merge_scope"))
		if sc == "" {
			sc = d.Memory.TopActiveScope(ctx)
		}
		catFilter := strings.TrimSpace(r.FormValue("merge_category"))
		items, srcLabel, err := d.Memory.SuggestTaxonomyRebalance(ctx, sc, catFilter)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		writeHTMLTemplate(w, "rebalance_ai_plan", ssrConsolidateModalData{
			Lang:           lang,
			RebalanceMode:  true,
			Scope:          sc,
			Category:       catFilter,
			RebalanceItems: items,
			SourceLabel:    srcLabel,
		})
	}))

	mux.HandleFunc("/ui/taxonomy/apply", postHX(func(w http.ResponseWriter, r *http.Request) {
		lang := requestLang(r)
		ctx := domain.WithAdminBypass(r.Context())
		sc := strings.TrimSpace(r.FormValue("merge_scope"))
		items := parseRebalanceFormItems(r)
		updated, err := d.Memory.ApplyTaxonomyRebalance(ctx, sc, items)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		toast := fmt.Sprintf("Updated categories for %d memory(ies)", updated)
		if lang == "ko" {
			toast = fmt.Sprintf("AI 분류 정리 완료 (%d건 카테고리 반영)", updated)
		}
		renderMemoriesAfterMutation(w, r, d, lang, toast, true, true)
	}))

	mux.HandleFunc("/ui/consolidate/apply-plan", postHX(func(w http.ResponseWriter, r *http.Request) {
		lang := requestLang(r)
		ctx := domain.WithAdminBypass(r.Context())
		sc := strings.TrimSpace(r.FormValue("merge_scope"))
		var proposals []domain.ConsolidationProposal
		for _, idxStr := range r.Form["group_idx"] {
			idxStr = strings.TrimSpace(idxStr)
			if idxStr == "" {
				continue
			}
			ids := parseFormIDs(url.Values{"ids": {r.FormValue("group_" + idxStr + "_ids")}})
			cat := strings.TrimSpace(r.FormValue("group_" + idxStr + "_cat"))
			pinned := r.FormValue("group_"+idxStr+"_pinned") == "1"
			content := strings.TrimSpace(r.FormValue("group_" + idxStr + "_content"))
			if len(ids) >= 2 && content != "" {
				proposals = append(proposals, domain.ConsolidationProposal{
					SourceIDs:     ids,
					Category:      cat,
					Pinned:        pinned,
					MergedContent: content,
				})
			}
		}
		rebItems := parseRebalanceFormItems(r)
		newIDs, srcCnt, rebCnt, err := d.Memory.ApplyCleanupPlan(ctx, sc, proposals, rebItems)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		var toast string
		if lang == "ko" {
			switch {
			case len(newIDs) > 0 && rebCnt > 0:
				toast = fmt.Sprintf("AI 정리 완료: 병합 %d그룹(%d->%d건) · 재분류 %d건", len(newIDs), srcCnt, len(newIDs), rebCnt)
			case rebCnt > 0:
				toast = fmt.Sprintf("AI 재분류 완료 (%d건 카테고리 반영)", rebCnt)
			default:
				toast = fmt.Sprintf("AI 추천 %d개 그룹 병합 완료 (%d건 -> %d건)", len(newIDs), srcCnt, len(newIDs))
			}
		} else {
			switch {
			case len(newIDs) > 0 && rebCnt > 0:
				toast = fmt.Sprintf("Applied %d merge group(s) (%d->%d) & %d category update(s)", len(newIDs), srcCnt, len(newIDs), rebCnt)
			case rebCnt > 0:
				toast = fmt.Sprintf("Updated categories for %d memory(ies)", rebCnt)
			default:
				toast = fmt.Sprintf("Merged %d AI group(s) (%d -> %d)", len(newIDs), srcCnt, len(newIDs))
			}
		}
		renderMemoriesAfterMutation(w, r, d, lang, toast, true, true)
	}))

	mux.HandleFunc("/ui/consolidate/preview", postHX(func(w http.ResponseWriter, r *http.Request) {
		lang := requestLang(r)
		ids := parseFormIDs(r.Form)
		if len(ids) < 2 {
			http.Error(w, "select at least 2 memories", http.StatusBadRequest)
			return
		}
		text, err := d.Memory.SynthesizeMergedContent(domain.WithAdminBypass(r.Context()), r.FormValue("merge_scope"), r.FormValue("merge_category"), ids)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		writeHTMLTemplate(w, "consolidate_draft_field", ssrConsolidateModalData{
			Lang:      lang,
			DraftText: text,
		})
	}))

	mux.HandleFunc("/ui/consolidate/execute", postHX(func(w http.ResponseWriter, r *http.Request) {
		lang := requestLang(r)
		ctx := domain.WithAdminBypass(r.Context())
		ids := parseFormIDs(r.Form)
		if len(ids) < 2 {
			http.Error(w, "select at least 2 memories", http.StatusBadRequest)
			return
		}
		sc := r.FormValue("merge_scope")
		cat := r.FormValue("merge_category")
		pinned := r.FormValue("merge_pinned") == "1"
		content := strings.TrimSpace(r.FormValue("content"))
		if content == "" {
			synth, err := d.Memory.SynthesizeMergedContent(ctx, sc, cat, ids)
			if err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			content = synth
		}
		newID, err := d.Memory.ConsolidateMemories(ctx, sc, cat, ids, content, pinned)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		renderMemoriesAfterMutation(w, r, d, lang, fmt.Sprintf("Merged %d -> #%d", len(ids), newID), true, true)
	}))
}
