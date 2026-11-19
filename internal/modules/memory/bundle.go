package memory

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strings"
	"time"

	"memorize/internal/domain"
)

const (
	maxBundleExportRecords = 5000
	maxBundleImportItems   = 2000
	bundleMergeChunkSize   = 20
)

func normalizeBundleDedupKey(content string) string {
	return strings.Join(strings.Fields(strings.ToLower(content)), " ")
}

func (s *Service) ExportBundle(ctx context.Context, q domain.RecordQuery, ids []int64) (domain.MemoryBundle, error) {
	out := domain.MemoryBundle{
		Format:     domain.MemoryBundleFormat,
		ExportedAt: time.Now().UTC().Format(time.RFC3339),
		Items:      []domain.MemoryBundleItem{},
	}
	if s == nil || s.db == nil {
		return out, fmt.Errorf("memory service is unavailable")
	}

	var recs []domain.MemoryRecord
	if len(ids) > 0 {
		for start := 0; start < len(ids); start += 500 {
			end := min(start+500, len(ids))
			chunk, err := s.GetRecordsByIDs(ctx, ids[start:end])
			if err != nil {
				return out, err
			}
			for _, r := range chunk {
				if r.Active {
					recs = append(recs, r)
				}
			}
		}
	} else {
		switch strings.ToLower(strings.TrimSpace(q.State)) {
		case "pinned", "general", "unpinned":
		default:
			q.State = "active"
		}
		q.PageSize = 500
		for page := 1; len(recs) < maxBundleExportRecords; page++ {
			q.Page = page
			res, err := s.QueryRecordsPage(ctx, q)
			if err != nil {
				return out, err
			}
			recs = append(recs, res.Records...)
			if page >= res.TotalPages || len(res.Records) == 0 {
				break
			}
		}
		if len(recs) > maxBundleExportRecords {
			recs = recs[:maxBundleExportRecords]
		}
	}

	sort.Slice(recs, func(i, j int) bool { return recs[i].ID < recs[j].ID })
	out.Items = make([]domain.MemoryBundleItem, 0, len(recs))
	for _, r := range recs {
		out.Items = append(out.Items, domain.MemoryBundleItem{
			Scope:       r.Scope,
			Category:    r.Category,
			SubCategory: r.SubCategory,
			Tags:        append([]string(nil), r.Tags...),
			Content:     r.Content,
			Confidence:  r.Confidence,
			Pinned:      r.Pinned,
		})
	}
	return out, nil
}

type preparedBundleItem struct {
	index       int
	scope       string
	category    string
	subCategory string
	tags        []string
	content     string
	confidence  float64
	pinned      bool
}

func (s *Service) ImportBundle(ctx context.Context, bundle domain.MemoryBundle, opts domain.BundleImportOptions) (domain.BundleImportResult, error) {
	mode := strings.ToLower(strings.TrimSpace(opts.Mode))
	if mode == "" {
		mode = "merge"
	}
	if mode != "exact" && mode != "merge" {
		return domain.BundleImportResult{}, fmt.Errorf("invalid import mode %q (expected 'merge' or 'exact')", opts.Mode)
	}
	if s == nil || s.db == nil {
		return domain.BundleImportResult{}, fmt.Errorf("memory service is unavailable")
	}
	if len(bundle.Items) > maxBundleImportItems {
		return domain.BundleImportResult{}, fmt.Errorf("bundle exceeds max %d items (got %d)", maxBundleImportItems, len(bundle.Items))
	}

	res := domain.BundleImportResult{
		DryRun: opts.DryRun,
		Mode:   mode,
		Total:  len(bundle.Items),
		Items:  make([]domain.BundleImportItemResult, len(bundle.Items)),
	}
	if len(bundle.Items) == 0 {
		return res, nil
	}

	overrideScope := ""
	if raw := strings.TrimSpace(opts.TargetScope); raw != "" {
		overrideScope = domain.NormalizeScope(raw)
	}

	var scopeOrder []string
	byScope := make(map[string][]preparedBundleItem)
	for i, raw := range bundle.Items {
		content := strings.TrimSpace(raw.Content)
		sc := overrideScope
		if sc == "" {
			sc = domain.NormalizeScope(raw.Scope)
		}
		cat, sub := domain.SplitCategoryAndSub(sc, raw.Category, raw.SubCategory)
		res.Items[i] = domain.BundleImportItemResult{
			Index:       i,
			Scope:       sc,
			Category:    cat,
			SubCategory: sub,
			Content:     content,
			Pinned:      raw.Pinned,
		}
		if content == "" {
			res.Items[i].Action = "skip_invalid"
			res.Items[i].Reason = "empty content"
			continue
		}
		if err := domain.CanModifyScope(ctx, sc); err != nil {
			return res, err
		}
		if reason := domain.CheckSecurityGuard(content); reason != "" {
			res.Items[i].Action = "skip_invalid"
			res.Items[i].Reason = "security guard: " + reason
			continue
		}
		conf := raw.Confidence
		if conf <= 0 || conf > 1 {
			conf = 0.90
		}
		if _, exists := byScope[sc]; !exists {
			scopeOrder = append(scopeOrder, sc)
		}
		byScope[sc] = append(byScope[sc], preparedBundleItem{
			index:       i,
			scope:       sc,
			category:    cat,
			subCategory: sub,
			tags:        domain.NormalizeTags(sc, raw.Tags),
			content:     content,
			confidence:  conf,
			pinned:      raw.Pinned,
		})
	}

	activeByScope := make(map[string][]domain.Fact, len(scopeOrder))
	pendingByScope := make(map[string][]preparedBundleItem, len(scopeOrder))
	for _, sc := range scopeOrder {
		existing, err := s.LoadActiveFacts(ctx, sc, sc, "")
		if err != nil {
			return res, err
		}
		activeByScope[sc] = existing
		seen := make(map[string]bool, len(existing))
		for _, f := range existing {
			seen[normalizeBundleDedupKey(f.Content)] = true
		}
		for _, it := range byScope[sc] {
			key := normalizeBundleDedupKey(it.content)
			if seen[key] {
				res.Items[it.index].Action = "skip_exact"
				res.Items[it.index].Reason = "exact match already exists"
				continue
			}
			seen[key] = true
			pendingByScope[sc] = append(pendingByScope[sc], it)
		}
	}

	var pendingCount int
	for _, sc := range scopeOrder {
		pendingCount += len(pendingByScope[sc])
	}
	if pendingCount > 0 {
		if mode == "exact" {
			if err := s.applyBundleExact(ctx, scopeOrder, pendingByScope, opts.DryRun, &res); err != nil {
				return res, err
			}
		} else {
			if err := s.applyBundleMerge(ctx, scopeOrder, pendingByScope, activeByScope, opts.DryRun, &res); err != nil {
				return res, err
			}
		}
	}

	for _, item := range res.Items {
		switch item.Action {
		case "insert":
			res.Inserted++
		case "overwrite":
			res.Overwritten++
		default:
			res.Skipped++
		}
	}
	return res, nil
}

func (s *Service) applyBundleExact(ctx context.Context, scopeOrder []string, pendingByScope map[string][]preparedBundleItem, dryRun bool, res *domain.BundleImportResult) error {
	acc := s.CurrentAccount(ctx)
	for _, sc := range scopeOrder {
		items := pendingByScope[sc]
		if len(items) == 0 {
			continue
		}
		if dryRun {
			for _, it := range items {
				res.Items[it.index].Action = "insert"
			}
			continue
		}
		newIDs := make([]int64, len(items))
		if err := s.withTx(ctx, func(tx *sql.Tx) error {
			for idx, it := range items {
				pin := 0
				if it.pinned {
					pin = 1
				}
				r, err := tx.ExecContext(ctx,
					`INSERT INTO memories (scope, account, category, sub_category, tags, content, confidence, pinned, active) VALUES (?, ?, ?, ?, ?, ?, ?, ?, 1)`,
					it.scope, acc, it.category, it.subCategory, strings.Join(it.tags, " "), it.content, it.confidence, pin)
				if err != nil {
					return err
				}
				id, _ := r.LastInsertId()
				newIDs[idx] = id
			}
			return nil
		}); err != nil {
			return err
		}
		for idx, it := range items {
			res.Items[it.index].Action = "insert"
			res.Items[it.index].NewID = newIDs[idx]
			s.RegisterTaxonomyAppendOnly(ctx, it.scope, it.category, it.subCategory)
			s.emitMutation(ctx, MutationEvent{
				Scope:       it.scope,
				Category:    it.category,
				SubCategory: it.subCategory,
				ActionKind:  "commit",
				MemoryID:    newIDs[idx],
				Content:     it.content,
			})
		}
		s.MarkTurnScopeMutatedCtx(ctx, sc)
	}
	return nil
}

func (s *Service) applyBundleMerge(ctx context.Context, scopeOrder []string, pendingByScope map[string][]preparedBundleItem, activeByScope map[string][]domain.Fact, dryRun bool, res *domain.BundleImportResult) error {
	ev := s.router.EvalForTask(ctx, domain.TaskMutation)
	if ev == nil {
		return fmt.Errorf("no active evaluation engine configured for AI merge (use mode=exact for deterministic import)")
	}
	res.Engine = ev.Name()
	evalCtx := domain.WithPresetCategories(ctx, s.EffectiveTaxonomyJSON(ctx))
	acc := s.CurrentAccount(ctx)
	th := s.GetThresholds(ctx)
	privSQL, privArgs := s.privateScopeSQL(ctx, "", false)

	for _, sc := range scopeOrder {
		items := pendingByScope[sc]
		if len(items) == 0 {
			continue
		}
		activeFacts := append([]domain.Fact(nil), activeByScope[sc]...)
		activeByID := make(map[int64]domain.Fact, len(activeFacts))
		for _, f := range activeFacts {
			activeByID[f.ID] = f
		}
		scopeMutated := false

		for start := 0; start < len(items); start += bundleMergeChunkSize {
			end := min(start+bundleMergeChunkSize, len(items))
			chunk := items[start:end]
			cands := make([]string, len(chunk))
			for i, it := range chunk {
				cands[i] = it.content
			}

			var decs []domain.MutationDecision
			evalStart := time.Now()
			if bm, ok := ev.(domain.BatchMutationEvaluator); ok {
				var err error
				decs, err = bm.EvaluateMutations(evalCtx, sc, cands, activeFacts)
				s.router.RecordTelemetry(ctx, domain.TaskMutation, ev, evalStart, len(activeFacts), err)
				if err != nil {
					return err
				}
			} else {
				decs = make([]domain.MutationDecision, len(chunk))
				for i, c := range cands {
					d, err := ev.EvaluateMutation(evalCtx, sc, c, activeFacts)
					s.router.RecordTelemetry(ctx, domain.TaskMutation, ev, evalStart, len(activeFacts), err)
					if err != nil {
						return err
					}
					decs[i] = d
				}
			}

			for i, it := range chunk {
				var dec domain.MutationDecision
				if i < len(decs) {
					dec = decs[i]
				}
				domain.ApplyDecisionSemantics(&dec)

				rawTargets := dec.ConflictFactIDs
				if len(rawTargets) == 0 && dec.ConflictFactID > 0 {
					rawTargets = []int64{dec.ConflictFactID}
				}
				var validTargets []int64
				seenTgt := make(map[int64]bool, len(rawTargets))
				anyTargetPinned := false
				for _, tid := range rawTargets {
					if f, ok := activeByID[tid]; ok && !seenTgt[tid] {
						seenTgt[tid] = true
						validTargets = append(validTargets, tid)
						if f.Pinned {
							anyTargetPinned = true
						}
					}
				}

				if dec.Action == "IGNORE" {
					res.Items[it.index].Action = "skip_duplicate"
					res.Items[it.index].TargetIDs = validTargets
					if len(validTargets) > 0 {
						res.Items[it.index].Reason = fmt.Sprintf("semantic duplicate of %v", validTargets)
					} else {
						res.Items[it.index].Reason = "skipped by Gatekeeper (IGNORE)"
					}
					continue
				}

				isOverwrite := dec.Action == "OVERWRITE" && len(validTargets) > 0
				pinned := it.pinned || anyTargetPinned || dec.IsGlobalInvariant >= th.PinInvariant
				res.Items[it.index].Pinned = pinned
				if isOverwrite {
					res.Items[it.index].Action = "overwrite"
					res.Items[it.index].TargetIDs = validTargets
					res.Items[it.index].Reason = fmt.Sprintf("supersedes %v", validTargets)
				} else {
					res.Items[it.index].Action = "insert"
				}
				if dryRun {
					continue
				}

				pinInt := 0
				if pinned {
					pinInt = 1
				}
				var newID int64
				if err := s.withTx(ctx, func(tx *sql.Tx) error {
					r, err := tx.ExecContext(ctx,
						`INSERT INTO memories (scope, account, category, sub_category, tags, content, confidence, pinned, active) VALUES (?, ?, ?, ?, ?, ?, ?, ?, 1)`,
						it.scope, acc, it.category, it.subCategory, strings.Join(it.tags, " "), it.content, it.confidence, pinInt)
					if err != nil {
						return err
					}
					newID, _ = r.LastInsertId()
					if isOverwrite {
						for _, oldID := range validTargets {
							qArgs := append([]any{newID, oldID, sc}, privArgs...)
							if _, err := tx.ExecContext(ctx,
								`UPDATE memories SET active = 0, superseded_by = ?
								 WHERE id = ? AND scope = ? AND `+privSQL+` AND active = 1`,
								qArgs...); err != nil {
								return err
							}
						}
					}
					return nil
				}); err != nil {
					return err
				}

				scopeMutated = true
				res.Items[it.index].NewID = newID
				actionKind := "commit"
				if isOverwrite {
					actionKind = "overwrite"
					s.clearInMemorySessionIDs(validTargets...)
					removed := make(map[int64]bool, len(validTargets))
					for _, tid := range validTargets {
						delete(activeByID, tid)
						removed[tid] = true
					}
					filtered := activeFacts[:0]
					for _, f := range activeFacts {
						if !removed[f.ID] {
							filtered = append(filtered, f)
						}
					}
					activeFacts = filtered
				}
				newFact := domain.Fact{
					ID:          newID,
					Scope:       it.scope,
					Account:     acc,
					Category:    it.category,
					SubCategory: it.subCategory,
					Tags:        it.tags,
					Content:     it.content,
					Pinned:      pinned,
				}
				activeFacts = append(activeFacts, newFact)
				activeByID[newID] = newFact
				s.RegisterTaxonomyAppendOnly(ctx, it.scope, it.category, it.subCategory)
				s.emitMutation(ctx, MutationEvent{
					Scope:       it.scope,
					Category:    it.category,
					SubCategory: it.subCategory,
					ActionKind:  actionKind,
					MemoryID:    newID,
					TargetIDs:   validTargets,
					Content:     it.content,
				})
			}
		}
		if scopeMutated {
			s.MarkTurnScopeMutatedCtx(ctx, sc)
		}
	}
	return nil
}
