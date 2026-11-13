package memory

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"
)

type CleanupPlan struct {
	Scope          string                  `json:"scope"`
	CategoryFilter string                  `json:"category_filter,omitempty"`
	SourceLabel    string                  `json:"source"`
	Proposals      []ConsolidationProposal `json:"proposals"`
	RebalanceItems []TaxonomyRebalanceItem `json:"rebalance_items"`
}

func isValidTaxonomySlug(s string) bool {
	s = strings.TrimSpace(s)
	if len(s) < 2 || len(s) > 32 || s == UncategorizedSubKey {
		return false
	}
	for _, r := range s {
		if unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_' || r == '-' {
			continue
		}
		return false
	}
	return true
}

func parsePresetTaxonomyMap(rawJSON string) map[string]map[string]bool {
	src := strings.TrimSpace(rawJSON)
	if src == "" {
		src = DefaultPresetCategoriesJSON
	}
	var parsed map[string][]string
	if err := json.Unmarshal([]byte(src), &parsed); err != nil || len(parsed) == 0 {
		_ = json.Unmarshal([]byte(DefaultPresetCategoriesJSON), &parsed)
	}
	out := make(map[string]map[string]bool, len(parsed))
	for rawCat, subs := range parsed {
		c := strings.ToLower(strings.TrimSpace(rawCat))
		if c == "" {
			continue
		}
		if out[c] == nil {
			out[c] = make(map[string]bool)
		}
		for _, rawSub := range subs {
			sc := NormalizeSubCategory(rawSub)
			if sc != "" {
				out[c][sc] = true
			}
		}
	}
	return out
}

func encodePresetTaxonomyMap(m map[string]map[string]bool) string {
	clean := make(map[string][]string, len(m))
	for c, subs := range m {
		if c == "" {
			continue
		}
		list := make([]string, 0, len(subs))
		for s := range subs {
			if s != "" {
				list = append(list, s)
			}
		}
		sort.Strings(list)
		clean[c] = list
	}
	b, err := json.Marshal(clean)
	if err != nil {
		return DefaultPresetCategoriesJSON
	}
	return string(b)
}

// EffectiveTaxonomyJSON merges configured preset_categories with all active categories and
// sub-categories currently stored in SQLite so LLM classification reuses consistent slugs.
func (s *Service) EffectiveTaxonomyJSON(ctx context.Context) string {
	tax := parsePresetTaxonomyMap(s.GetConfigMap(ctx)["preset_categories"])
	rows, err := s.db.QueryContext(ctx, `
		SELECT DISTINCT LOWER(TRIM(category)), LOWER(TRIM(COALESCE(sub_category, '')))
		FROM memories
		WHERE active = 1 AND TRIM(category) != ''
	`)
	if err == nil {
		for rows.Next() {
			var cat, sub string
			if rows.Scan(&cat, &sub) == nil && isValidTaxonomySlug(cat) && cat != "general" {
				if tax[cat] == nil {
					tax[cat] = make(map[string]bool)
				}
				if isValidTaxonomySlug(sub) {
					tax[cat][sub] = true
				}
			}
		}
		_ = rows.Close()
	}
	return encodePresetTaxonomyMap(tax)
}

// RegisterTaxonomyAppendOnly adds a newly used category and/or sub-category into config.preset_categories
// strictly append-only (never deleting any existing category or sub-category).
func (s *Service) RegisterTaxonomyAppendOnly(ctx context.Context, rawScope, rawCat, rawSub string) {
	cat, sub := SplitCategoryAndSub(rawScope, rawCat, rawSub)
	if !isValidTaxonomySlug(cat) || cat == "general" {
		return
	}
	cfg := s.GetConfigMap(ctx)
	tax := parsePresetTaxonomyMap(cfg["preset_categories"])

	changed := false
	if tax[cat] == nil {
		tax[cat] = make(map[string]bool)
		changed = true
	}
	if sub != "" && isValidTaxonomySlug(sub) && !tax[cat][sub] {
		tax[cat][sub] = true
		changed = true
	}
	if !changed {
		return
	}
	_ = s.SaveConfig(ctx, map[string]string{
		"preset_categories": encodePresetTaxonomyMap(tax),
	})
}

// SuggestTaxonomyRebalance analyzes active memories in rawScope (and optional categoryFilter)
// and proposes non-destructive category/sub_category adjustments (never altering memory text or deleting items).
func (s *Service) SuggestTaxonomyRebalance(ctx context.Context, rawScope, categoryFilter string) ([]TaxonomyRebalanceItem, string, error) {
	targetScope := NormalizeScope(rawScope)
	facts, err := s.loadActiveFacts(ctx, targetScope, targetScope, strings.TrimSpace(categoryFilter))
	if err != nil {
		return nil, "", err
	}
	if len(facts) == 0 {
		return nil, s.ProviderName(), nil
	}

	presetTax := parsePresetTaxonomyMap(s.GetConfigMap(ctx)["preset_categories"])
	ev := s.evalForTask(ctx, TaskTaxonomy)
	sourceLabel := "lexical-taxonomy"
	var rawItems []TaxonomyRebalanceItem
	usedAI := false

	if EvaluatorSupportsTask(ev, TaskTaxonomy) {
		if planner, ok := ev.(TaxonomyPlanner); ok {
			pctx := WithPresetCategories(ctx, s.EffectiveTaxonomyJSON(ctx))
			evalStart := time.Now()
			proposed, perr := planner.SuggestTaxonomyRebalance(pctx, targetScope, facts)
			s.recordEvalTelemetry(pctx, TaskTaxonomy, ev, evalStart, len(facts), perr)
			if perr == nil {
				rawItems = proposed
				usedAI = true
				sourceLabel = ev.Name()
			}
		}
	}
	if !usedAI {
		rawItems = suggestLexicalSubCategories(targetScope, facts, presetTax)
	}

	byID := make(map[int64]Fact, len(facts))
	for _, f := range facts {
		byID[f.ID] = f
	}
	seen := make(map[int64]bool, len(rawItems))
	var out []TaxonomyRebalanceItem
	for _, it := range rawItems {
		if seen[it.ID] {
			continue
		}
		f, ok := byID[it.ID]
		if !ok {
			continue
		}
		newCat, newSub := SplitCategoryAndSub(targetScope, it.NewCategory, it.NewSubCategory)
		if !isValidTaxonomySlug(newCat) {
			newCat = f.Category
		}
		if newSub != "" && !isValidTaxonomySlug(newSub) {
			newSub = f.SubCategory
		}
		if newCat == f.Category && newSub == f.SubCategory {
			continue
		}
		seen[it.ID] = true
		isNew := presetTax[newCat] == nil || (newSub != "" && !presetTax[newCat][newSub])
		out = append(out, TaxonomyRebalanceItem{
			ID:             f.ID,
			OldCategory:    f.Category,
			OldSubCategory: f.SubCategory,
			NewCategory:    newCat,
			NewSubCategory: newSub,
			IsNewTaxonomy:  isNew,
			Reason:         strings.TrimSpace(it.Reason),
			Pinned:         f.Pinned,
			Content:        f.Content,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, sourceLabel, nil
}

func suggestLexicalSubCategories(targetScope string, facts []Fact, presetTax map[string]map[string]bool) []TaxonomyRebalanceItem {
	// Collect token centroids for existing sub-categories within the same category
	type subCentroid struct {
		cat    string
		sub    string
		tokens map[string]bool
	}
	var centroids []subCentroid
	byKey := map[string]map[string]bool{}
	for _, f := range facts {
		if f.SubCategory == "" {
			continue
		}
		k := f.Category + "/" + f.SubCategory
		if byKey[k] == nil {
			byKey[k] = map[string]bool{}
		}
		for tok := range tokenizeSimilaritySet(f) {
			byKey[k][tok] = true
		}
	}
	for k, toks := range byKey {
		parts := strings.SplitN(k, "/", 2)
		if len(parts) == 2 {
			centroids = append(centroids, subCentroid{cat: parts[0], sub: parts[1], tokens: toks})
		}
	}

	var out []TaxonomyRebalanceItem
	for _, f := range facts {
		if f.SubCategory != "" {
			continue
		}
		fToks := tokenizeSimilaritySet(f)
		bestSub := ""
		bestScore := 0.0
		for _, c := range centroids {
			if c.cat != f.Category {
				continue
			}
			sc := jaccardOverlap(fToks, c.tokens)
			if sc > bestScore {
				bestScore = sc
				bestSub = c.sub
			}
		}
		if bestSub == "" && presetTax[f.Category] != nil {
			low := strings.ToLower(f.Content)
			for candSub := range presetTax[f.Category] {
				if strings.Contains(low, candSub) {
					bestSub = candSub
					break
				}
			}
		}
		if bestSub != "" {
			out = append(out, TaxonomyRebalanceItem{
				ID:             f.ID,
				NewCategory:    NormalizeCategoryForScope(targetScope, f.Category),
				NewSubCategory: bestSub,
				Reason:         "미분류 항목 하위카테고리 매칭",
			})
		}
	}
	return out
}

// ApplyTaxonomyRebalance updates ONLY category and sub_category for approved memory IDs
// without modifying content, confidence, pin state, or deleting any record, and appends new taxonomy slugs.
func (s *Service) ApplyTaxonomyRebalance(ctx context.Context, rawScope string, items []TaxonomyRebalanceItem) (int, error) {
	if len(items) == 0 {
		return 0, fmt.Errorf("select at least 1 taxonomy adjustment to apply")
	}
	targetScope := NormalizeScope(rawScope)
	updated := 0
	for _, it := range items {
		if it.ID <= 0 {
			continue
		}
		newCat, newSub := SplitCategoryAndSub(targetScope, it.NewCategory, it.NewSubCategory)
		if !isValidTaxonomySlug(newCat) {
			continue
		}
		res, err := s.db.ExecContext(ctx, `
			UPDATE memories
			SET category = ?, sub_category = ?
			WHERE id = ? AND scope = ? AND active = 1
		`, newCat, newSub, it.ID, targetScope)
		if err != nil {
			return updated, err
		}
		if n, _ := res.RowsAffected(); n > 0 {
			updated++
			s.RegisterTaxonomyAppendOnly(ctx, targetScope, newCat, newSub)
		}
	}
	if updated > 0 {
		s.markTurnScopeMutated(targetScope)
	}
	return updated, nil
}

// SuggestCleanupPlanFiltered runs AI merge analysis and AI category/sub-category rebalance
// concurrently and returns a unified, disjoint cleanup plan for rawScope (and optional categoryFilter).
func (s *Service) SuggestCleanupPlanFiltered(ctx context.Context, rawScope, categoryFilter string) (CleanupPlan, error) {
	targetScope := NormalizeScope(rawScope)
	catFilter := strings.TrimSpace(categoryFilter)

	var (
		wg        sync.WaitGroup
		proposals []ConsolidationProposal
		mergeSrc  string
		mergeErr  error
		rebItems  []TaxonomyRebalanceItem
		rebSrc    string
		rebErr    error
	)

	wg.Add(2)
	go func() {
		defer wg.Done()
		proposals, mergeSrc, mergeErr = s.SuggestConsolidationPlanFiltered(ctx, targetScope, catFilter)
	}()
	go func() {
		defer wg.Done()
		rebItems, rebSrc, rebErr = s.SuggestTaxonomyRebalance(ctx, targetScope, catFilter)
	}()
	wg.Wait()

	if mergeErr != nil {
		return CleanupPlan{}, mergeErr
	}
	if rebErr != nil {
		return CleanupPlan{}, rebErr
	}

	mergedIDs := make(map[int64]bool)
	for _, p := range proposals {
		for _, id := range p.SourceIDs {
			mergedIDs[id] = true
		}
	}
	var disjointReb []TaxonomyRebalanceItem
	for _, it := range rebItems {
		if !mergedIDs[it.ID] {
			disjointReb = append(disjointReb, it)
		}
	}

	srcLabel := mergeSrc
	if (srcLabel == "" || srcLabel == "heuristic") && rebSrc != "" && rebSrc != "lexical-taxonomy" {
		srcLabel = rebSrc
	}
	if srcLabel == "" {
		srcLabel = s.ProviderName()
	}

	return CleanupPlan{
		Scope:          targetScope,
		CategoryFilter: catFilter,
		SourceLabel:    srcLabel,
		Proposals:      proposals,
		RebalanceItems: disjointReb,
	}, nil
}

// ApplyCleanupPlan executes approved merge proposals and non-destructive category adjustments in one batch.
func (s *Service) ApplyCleanupPlan(ctx context.Context, rawScope string, proposals []ConsolidationProposal, items []TaxonomyRebalanceItem) ([]int64, int, int, error) {
	if len(proposals) == 0 && len(items) == 0 {
		return nil, 0, 0, fmt.Errorf("select at least 1 merge group or category adjustment to apply")
	}
	targetScope := NormalizeScope(rawScope)
	superseded := make(map[int64]bool)

	var newIDs []int64
	mergedSrcCnt := 0
	if len(proposals) > 0 {
		var err error
		newIDs, mergedSrcCnt, err = s.ApplyConsolidationPlan(ctx, targetScope, proposals)
		if err != nil {
			return newIDs, mergedSrcCnt, 0, err
		}
		for _, p := range proposals {
			for _, id := range p.SourceIDs {
				superseded[id] = true
			}
		}
	}

	var remaining []TaxonomyRebalanceItem
	for _, it := range items {
		if !superseded[it.ID] {
			remaining = append(remaining, it)
		}
	}
	rebCnt := 0
	if len(remaining) > 0 {
		var err error
		rebCnt, err = s.ApplyTaxonomyRebalance(ctx, targetScope, remaining)
		if err != nil {
			return newIDs, mergedSrcCnt, rebCnt, err
		}
	}
	return newIDs, mergedSrcCnt, rebCnt, nil
}
