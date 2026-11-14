package memory

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"
	"unicode"

	"memorize/internal/domain"
)

// TopActiveScope returns the scope with the highest number of active memories, or "" if none exist.
func (s *Service) TopActiveScope(ctx context.Context) string {
	isAdmin := domain.IsAdminContext(ctx)
	curAcc := s.CurrentAccount(ctx)
	var sc string
	_ = s.db.QueryRowContext(ctx, `
		SELECT scope FROM memories
		WHERE active = 1 AND (? = 1 OR scope != 'personal' OR account = ?)
		GROUP BY scope
		ORDER BY COUNT(*) DESC, scope ASC
		LIMIT 1`, boolToInt(isAdmin), curAcc).Scan(&sc)
	return strings.TrimSpace(sc)
}

// SuggestConsolidationPlan scans active memories in rawScope using the configured AI evaluator
// (or a deterministic structural/lexical similarity fallback) and returns validated, disjoint merge proposals.
func (s *Service) SuggestConsolidationPlan(ctx context.Context, rawScope string) ([]domain.ConsolidationProposal, string, error) {
	return s.SuggestConsolidationPlanFiltered(ctx, rawScope, "")
}

// SuggestConsolidationPlanFiltered scans active memories in rawScope matching categoryFilter for consolidation candidates.
func (s *Service) SuggestConsolidationPlanFiltered(ctx context.Context, rawScope, categoryFilter string) ([]domain.ConsolidationProposal, string, error) {
	targetScope := domain.NormalizeScope(rawScope)
	facts, err := s.LoadActiveFacts(ctx, targetScope, targetScope, strings.TrimSpace(categoryFilter))
	if err != nil {
		return nil, "", err
	}
	if len(facts) < 2 {
		return nil, s.ProviderName(), nil
	}

	ev := s.router.EvalForTask(ctx, domain.TaskConsolidate)
	sourceLabel := "heuristic"
	var rawProposals []domain.ConsolidationProposal
	usedAI := false

	if domain.EvaluatorSupportsTask(ev, domain.TaskConsolidate) {
		if planner, ok := ev.(domain.ConsolidationPlanner); ok {
			pctx := domain.WithPresetCategories(ctx, s.EffectiveTaxonomyJSON(ctx))
			evalStart := time.Now()
			groups, perr := planner.SuggestConsolidations(pctx, targetScope, facts)
			s.router.RecordTelemetry(pctx, domain.TaskConsolidate, ev, evalStart, len(facts), perr)
			if perr == nil {
				rawProposals = groups
				usedAI = true
				sourceLabel = ev.Name()
			}
		}
	}
	if !usedAI {
		rawProposals = s.suggestHeuristicConsolidations(ctx, targetScope, facts)
	}

	clean := s.sanitizeConsolidationProposals(ctx, targetScope, facts, rawProposals)
	return clean, sourceLabel, nil
}

func (s *Service) sanitizeConsolidationProposals(ctx context.Context, targetScope string, facts []domain.Fact, raw []domain.ConsolidationProposal) []domain.ConsolidationProposal {
	byID := make(map[int64]domain.Fact, len(facts))
	for _, f := range facts {
		byID[f.ID] = f
	}
	usedIDs := make(map[int64]bool)
	var out []domain.ConsolidationProposal

	for _, g := range raw {
		var ids []int64
		var sources []domain.Fact
		pinnedAny := false
		for _, id := range g.SourceIDs {
			if usedIDs[id] {
				continue
			}
			f, ok := byID[id]
			if !ok {
				continue
			}
			ids = append(ids, id)
			sources = append(sources, f)
			if f.Pinned {
				pinnedAny = true
			}
		}
		if len(ids) < 2 {
			continue
		}
		sort.Slice(sources, func(i, j int) bool { return sources[i].ID < sources[j].ID })
		for i := range sources {
			ids[i] = sources[i].ID
		}

		cat := strings.TrimSpace(g.Category)
		if cat == "" {
			cat = sources[0].Category
			if sources[0].SubCategory != "" {
				cat += "/" + sources[0].SubCategory
			}
		} else if !strings.Contains(cat, "/") && sources[0].SubCategory != "" {
			normCat := domain.NormalizeCategoryForScope(targetScope, cat)
			if normCat == sources[0].Category {
				cat = normCat + "/" + sources[0].SubCategory
			}
		}

		merged := strings.TrimSpace(g.MergedContent)
		if merged == "" || len([]rune(merged)) > domain.MaxConsolidatedRunes || domain.CheckSecurityGuard(merged) != "" {
			if synth, err := s.SynthesizeMergedContent(ctx, targetScope, cat, ids); err == nil {
				merged = strings.TrimSpace(synth)
			}
		}
		if merged == "" || len([]rune(merged)) > domain.MaxConsolidatedRunes || domain.CheckSecurityGuard(merged) != "" {
			continue
		}

		for _, id := range ids {
			usedIDs[id] = true
		}
		out = append(out, domain.ConsolidationProposal{
			SourceIDs:     ids,
			Category:      cat,
			Pinned:        pinnedAny,
			MergedContent: merged,
			Reason:        strings.TrimSpace(g.Reason),
			Sources:       sources,
		})
	}
	return out
}

func tokenizeSimilaritySet(f domain.Fact) map[string]bool {
	set := make(map[string]bool)
	for _, t := range f.Tags {
		if t != "" {
			set["tag:"+t] = true
		}
	}
	if f.SubCategory != "" {
		set["sub:"+f.SubCategory] = true
	}
	words := strings.FieldsFunc(strings.ToLower(f.Content), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsNumber(r)
	})
	for _, w := range words {
		if len([]rune(w)) >= 2 {
			set[w] = true
		}
	}
	return set
}

func jaccardOverlap(a, b map[string]bool) float64 {
	if len(a) == 0 || len(b) == 0 {
		return 0
	}
	inter := 0
	for k := range a {
		if b[k] {
			inter++
		}
	}
	union := len(a) + len(b) - inter
	if union <= 0 {
		return 0
	}
	return float64(inter) / float64(union)
}

func (s *Service) suggestHeuristicConsolidations(ctx context.Context, targetScope string, facts []domain.Fact) []domain.ConsolidationProposal {
	type bucketKey struct {
		cat    string
		pinned bool
	}
	buckets := make(map[bucketKey][]domain.Fact)
	var keys []bucketKey
	for _, f := range facts {
		k := bucketKey{cat: f.Category, pinned: f.Pinned}
		if _, seen := buckets[k]; !seen {
			keys = append(keys, k)
		}
		buckets[k] = append(buckets[k], f)
	}

	var out []domain.ConsolidationProposal
	for _, k := range keys {
		list := buckets[k]
		if len(list) < 2 {
			continue
		}
		toks := make([]map[string]bool, len(list))
		for i, f := range list {
			toks[i] = tokenizeSimilaritySet(f)
		}
		used := make([]bool, len(list))
		for i := 0; i < len(list); i++ {
			if used[i] {
				continue
			}
			groupIdx := []int{i}
			ids := []int64{list[i].ID}
			for j := i + 1; j < len(list) && len(groupIdx) < 4; j++ {
				if used[j] {
					continue
				}
				sameSub := list[i].SubCategory != "" && list[i].SubCategory == list[j].SubCategory
				sim := jaccardOverlap(toks[i], toks[j])
				if !sameSub && sim < 0.15 {
					continue
				}
				tryIDs := append(append([]int64(nil), ids...), list[j].ID)
				merged, err := s.SynthesizeMergedContent(ctx, targetScope, k.cat, tryIDs)
				if err != nil || len([]rune(strings.TrimSpace(merged))) > domain.MaxConsolidatedRunes {
					continue
				}
				groupIdx = append(groupIdx, j)
				ids = tryIDs
			}
			if len(ids) >= 2 {
				merged, err := s.SynthesizeMergedContent(ctx, targetScope, k.cat, ids)
				if err == nil && len([]rune(strings.TrimSpace(merged))) <= domain.MaxConsolidatedRunes {
					for _, gi := range groupIdx {
						used[gi] = true
					}
					cat := k.cat
					if list[i].SubCategory != "" {
						cat += "/" + list[i].SubCategory
					}
					out = append(out, domain.ConsolidationProposal{
						SourceIDs:     ids,
						Category:      cat,
						Pinned:        k.pinned,
						MergedContent: strings.TrimSpace(merged),
						Reason:        cat,
					})
				}
			}
		}
	}
	return out
}

// ApplyConsolidationPlan executes multiple approved consolidation groups in rawScope.
func (s *Service) ApplyConsolidationPlan(ctx context.Context, rawScope string, proposals []domain.ConsolidationProposal) ([]int64, int, error) {
	if len(proposals) == 0 {
		return nil, 0, fmt.Errorf("select at least 1 consolidation group to execute")
	}
	targetScope := domain.NormalizeScope(rawScope)
	seenSource := make(map[int64]bool)
	var valid []domain.ConsolidationProposal
	for _, p := range proposals {
		var ids []int64
		for _, id := range p.SourceIDs {
			if id > 0 && !seenSource[id] {
				seenSource[id] = true
				ids = append(ids, id)
			}
		}
		txt := strings.TrimSpace(p.MergedContent)
		if len(ids) < 2 || txt == "" {
			continue
		}
		p.SourceIDs = ids
		p.MergedContent = txt
		valid = append(valid, p)
	}
	if len(valid) == 0 {
		return nil, 0, fmt.Errorf("no valid consolidation groups selected")
	}

	newIDs := make([]int64, 0, len(valid))
	mergedSources := 0
	for _, p := range valid {
		nid, err := s.ConsolidateMemories(ctx, targetScope, p.Category, p.SourceIDs, p.MergedContent, p.Pinned)
		if err != nil {
			return newIDs, mergedSources, err
		}
		newIDs = append(newIDs, nid)
		mergedSources += len(p.SourceIDs)
	}
	return newIDs, mergedSources, nil
}

// ConsolidateMemories merges sourceIDs into a single concise memory entry within MaxConsolidatedRunes.
func (s *Service) ConsolidateMemories(ctx context.Context, rawScope, category string, sourceIDs []int64, mergedContent string, pinned bool) (int64, error) {
	mergedContent = strings.TrimSpace(mergedContent)
	if len(sourceIDs) < 2 || mergedContent == "" {
		return 0, fmt.Errorf("select at least 2 memories and provide merged content")
	}
	if runeLen := len([]rune(mergedContent)); runeLen > domain.MaxConsolidatedRunes {
		return 0, fmt.Errorf("consolidated memory exceeds %d characters (%d); merge only similar rules into a concise single statement or keep distinct rules separate", domain.MaxConsolidatedRunes, runeLen)
	}
	if reason := domain.CheckSecurityGuard(mergedContent); reason != "" {
		return 0, fmt.Errorf("security guard: %s", reason)
	}
	targetScope := domain.NormalizeScope(rawScope)
	cat, subCat := domain.SplitCategoryAndSub(targetScope, category, "")
	pin := boolToInt(pinned)

	tagSet := make(map[string]bool)
	acc := ""
	mergedConf := 0.0
	for _, id := range sourceIDs {
		var ts, sc, srcScope, srcAcc string
		var srcConf float64
		if err := s.db.QueryRowContext(ctx, `SELECT scope, tags, COALESCE(sub_category, ''), COALESCE(account, ''), confidence FROM memories WHERE id = ?`, id).Scan(&srcScope, &ts, &sc, &srcAcc, &srcConf); err != nil {
			return 0, fmt.Errorf("memory #%d not found", id)
		}
		if err := s.checkPersonalOwnership(ctx, id, srcScope, srcAcc); err != nil {
			return 0, err
		}
		if srcConf > mergedConf {
			mergedConf = srcConf
		}
		for _, t := range parseTagStr(ts) {
			tagSet[t] = true
		}
		if subCat == "" && sc != "" {
			subCat = sc
		}
		if acc == "" && srcAcc != "" {
			acc = srcAcc
		}
	}
	if mergedConf <= 0 {
		mergedConf = 0.90
	}
	mergedConf = clamp(mergedConf, 0.80, 1.00)
	if acc == "" {
		acc = s.CurrentAccount(ctx)
	}
	var rawTags []string
	for t := range tagSet {
		rawTags = append(rawTags, t)
	}
	tagStr := strings.Join(domain.NormalizeTags(targetScope, rawTags), " ")

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	res, err := tx.ExecContext(ctx,
		`INSERT INTO memories (scope, account, category, sub_category, tags, content, confidence, pinned, active) VALUES (?, ?, ?, ?, ?, ?, ?, ?, 1)`,
		targetScope, acc, cat, subCat, tagStr, mergedContent, mergedConf, pin)
	if err != nil {
		return 0, err
	}
	newID, _ := res.LastInsertId()
	for _, oldID := range sourceIDs {
		if _, err := tx.ExecContext(ctx, `UPDATE memories SET active = 0, superseded_by = ? WHERE id = ?`, newID, oldID); err != nil {
			return 0, err
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	for _, oldID := range sourceIDs {
		_, _ = s.db.ExecContext(ctx, `INSERT OR IGNORE INTO session_sent (session_id, memory_id) SELECT session_id, memory_id FROM session_updates WHERE memory_id = ?`, oldID)
		_, _ = s.db.ExecContext(ctx, `DELETE FROM session_updates WHERE memory_id = ?`, oldID)
	}
	// Keep SQLite session_sent rows intact so sessions that previously saw sourceIDs get a 1-time (Replaces: ...) notice.
	s.clearInMemorySessionIDs(sourceIDs...)
	s.MarkTurnScopeMutated(targetScope)
	s.RegisterTaxonomyAppendOnly(ctx, targetScope, cat, subCat)
	s.emitMutation(ctx, MutationEvent{
		Scope:       targetScope,
		Category:    cat,
		SubCategory: subCat,
		ActionKind:  "consolidate",
		MemoryID:    newID,
		TargetIDs:   sourceIDs,
		Content:     mergedContent,
	})
	return newID, nil
}

func synthesizeFallbackMerge(facts []domain.Fact) string {
	seen := make(map[string]bool, len(facts))
	var parts []string
	for _, f := range facts {
		txt := strings.TrimSpace(strings.TrimRight(strings.TrimSpace(f.Content), ".。;"))
		key := strings.ToLower(txt)
		if txt != "" && !seen[key] {
			seen[key] = true
			parts = append(parts, txt)
		}
	}
	if len(parts) == 0 && len(facts) > 0 {
		return strings.TrimSpace(facts[0].Content)
	}
	return strings.Join(parts, "; ")
}

// SelectConsolidationTargets picks up to count active memories in rawScope matching category and pinned state.
func (s *Service) SelectConsolidationTargets(ctx context.Context, rawScope, category string, pinned bool, count int) (string, []domain.Fact, error) {
	if count < 2 {
		return "", nil, fmt.Errorf("consolidation count must be at least 2")
	}
	targetScope := domain.NormalizeScope(rawScope)
	facts, err := s.LoadActiveFacts(ctx, targetScope, targetScope, "")
	if err != nil {
		return "", nil, err
	}

	var matched []domain.Fact
	cat := strings.TrimSpace(category)
	if cat != "" {
		cat = domain.NormalizeCategoryForScope(targetScope, cat)
		for _, f := range facts {
			if f.Category == cat && f.Pinned == pinned {
				matched = append(matched, f)
			}
		}
	} else {
		byCat := make(map[string][]domain.Fact)
		for _, f := range facts {
			if f.Pinned == pinned {
				byCat[f.Category] = append(byCat[f.Category], f)
			}
		}
		for c, list := range byCat {
			if len(list) > len(matched) || (len(list) == len(matched) && c < cat) {
				cat = c
				matched = list
			}
		}
	}

	if len(matched) < 2 {
		return cat, nil, fmt.Errorf("fewer than 2 active memories match the selected criteria")
	}
	if len(matched) > count {
		matched = matched[:count]
	}
	return cat, matched, nil
}

// SynthesizeMergedContent generates a single merged sentence for sourceIDs without mutating SQLite.
func (s *Service) SynthesizeMergedContent(ctx context.Context, rawScope, category string, sourceIDs []int64) (string, error) {
	if len(sourceIDs) < 2 {
		return "", fmt.Errorf("select at least 2 memories")
	}
	targetScope := domain.NormalizeScope(rawScope)
	facts, err := s.LoadActiveFacts(ctx, targetScope, targetScope, "")
	if err != nil {
		return "", err
	}
	byID := make(map[int64]domain.Fact, len(facts))
	for _, f := range facts {
		byID[f.ID] = f
	}
	var chunk []domain.Fact
	for _, id := range sourceIDs {
		if f, ok := byID[id]; ok {
			chunk = append(chunk, f)
		}
	}
	if len(chunk) < 2 {
		return "", fmt.Errorf("fewer than 2 active memories found in scope %s", targetScope)
	}
	cat := strings.TrimSpace(category)
	if cat == "" {
		cat = chunk[0].Category
	} else {
		cat = domain.NormalizeCategoryForScope(targetScope, cat)
	}

	var merged string
	if ev := s.router.EvalForTask(ctx, domain.TaskConsolidate); domain.EvaluatorSupportsTask(ev, domain.TaskConsolidate) {
		if cons, ok := ev.(domain.Consolidator); ok {
			evalStart := time.Now()
			var cerr error
			merged, cerr = cons.ConsolidateFacts(ctx, targetScope, cat, chunk)
			s.router.RecordTelemetry(ctx, domain.TaskConsolidate, ev, evalStart, len(chunk), cerr)
		}
	}
	merged = strings.TrimSpace(merged)
	if merged == "" || domain.CheckSecurityGuard(merged) != "" {
		merged = synthesizeFallbackMerge(chunk)
	}
	return merged, nil
}

// ConsolidateByCount merges up to count active memories in the specified scope/category/pinned group.
func (s *Service) ConsolidateByCount(ctx context.Context, rawScope, category string, pinned bool, count int, customContent string) (int64, []int64, string, error) {
	targetScope := domain.NormalizeScope(rawScope)
	cat, matched, err := s.SelectConsolidationTargets(ctx, targetScope, category, pinned, count)
	if err != nil {
		return 0, nil, "", err
	}
	ids := make([]int64, len(matched))
	for i, f := range matched {
		ids[i] = f.ID
	}
	merged := strings.TrimSpace(customContent)
	if merged == "" {
		merged, err = s.SynthesizeMergedContent(ctx, targetScope, cat, ids)
		if err != nil {
			return 0, nil, "", err
		}
	}
	newID, err := s.ConsolidateMemories(ctx, targetScope, cat, ids, merged, pinned)
	if err != nil {
		return 0, nil, "", err
	}
	return newID, ids, merged, nil
}
