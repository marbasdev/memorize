package memory

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"
)

// AutoSyncProgressCommits performs fast, deterministic (<5ms, zero-LLM) incremental Git commit sync
// into project_progress on SessionStart when a project scope already has an anchored synced_commit.
func (s *Service) AutoSyncProgressCommits(ctx context.Context, rawScope, repoDir string) int {
	sc := NormalizeScope(rawScope)
	if !strings.HasPrefix(sc, "project:") {
		return 0
	}
	root := s.ResolveScopeRepoPath(ctx, sc, repoDir)
	if root == "" {
		return 0
	}
	lastCommit := s.GetSyncedCommit(ctx, sc)
	if lastCommit == "" {
		return 0
	}
	head, pending := s.collectPendingGitCommits(ctx, sc, root, lastCommit, 10)
	if head == "" || len(pending) == 0 {
		return 0
	}

	type commitMilestone struct {
		date    string
		track   string
		cat     string
		sub     string
		headSHA string
		sums    []string
		details []string
		count   int
	}
	type msKey struct{ date, track string }
	msMap := make(map[msKey]*commitMilestone)
	var msOrder []msKey

	for _, c := range pending {
		track, cat, sub, sum := inferCommitCategoryFallback(sc, c)
		key := msKey{date: c.Date, track: strings.ToLower(track)}
		ms := msMap[key]
		if ms == nil {
			ms = &commitMilestone{date: c.Date, track: track, cat: cat, sub: sub}
			msMap[key] = ms
			msOrder = append(msOrder, key)
		}
		ms.headSHA = c.SHA
		ms.sums = append(ms.sums, sum)
		detLine := fmt.Sprintf("[git:%s] %s", c.SHA, sum)
		if len(c.Files) > 0 {
			detLine += fmt.Sprintf(" (%s)", strings.Join(c.Files, ", "))
		}
		ms.details = append(ms.details, detLine)
		ms.count++
	}

	synced := 0
	for _, key := range msOrder {
		ms := msMap[key]
		combinedSum := strings.Join(ms.sums, " · ")
		if ms.date != "" {
			combinedSum = fmt.Sprintf("(%s) %s", ms.date, combinedSum)
		}
		if _, err := s.RecordProgress(ctx, ProgressInput{
			Scope:       sc,
			Track:       ms.track,
			Category:    ms.cat,
			SubCategory: ms.sub,
			Status:      ProgressStatusDone,
			Summary:     combinedSum,
			Details:     strings.Join(ms.details, "\n"),
			CommitSHA:   ms.headSHA,
			Source:      "git_sync",
		}); err == nil {
			synced += ms.count
		}
	}
	if synced > 0 {
		_ = s.SetSyncedCommit(ctx, sc, head)
	}
	return synced
}

// SyncProjectProgress synchronizes unlinked active memories and unsynced Git commits (last_commit..HEAD)
// into work-track milestones with full expandable details and advances the repo_sync cursor.
func (s *Service) SyncProjectProgress(ctx context.Context, rawScope, explicitRepo, lang string) (ProgressSyncResult, error) {
	sc := NormalizeScope(rawScope)
	s.upgradeLegacyMemorySyncRows(ctx, sc)
	state := s.InspectProgressSync(ctx, sc, explicitRepo)
	res := ProgressSyncResult{
		Scope:      sc,
		HeadCommit: state.HeadCommit,
	}

	// 1. Baseline / incremental sync of unlinked active memories grouped by category/sub_category work track
	unlinked, err := s.unlinkedActiveMemories(ctx, sc)
	if err == nil && len(unlinked) > 0 {
		type grpKey struct{ cat, sub string }
		groups := make(map[grpKey][]Fact)
		var order []grpKey
		for _, f := range unlinked {
			k := grpKey{f.Category, f.SubCategory}
			if _, ok := groups[k]; !ok {
				order = append(order, k)
			}
			groups[k] = append(groups[k], f)
		}
		for _, k := range order {
			list := groups[k]
			ids := make([]int64, len(list))
			for i, f := range list {
				ids[i] = f.ID
			}
			track, sum, details := buildMemorySyncSummaryAndDetails(k.cat, k.sub, lang, list)
			if _, err := s.RecordProgress(ctx, ProgressInput{
				Scope:       sc,
				Track:       track,
				Category:    k.cat,
				SubCategory: k.sub,
				Status:      ProgressStatusDone,
				Summary:     sum,
				Details:     details,
				MemoryIDs:   ids,
				CommitSHA:   state.HeadCommit,
				Source:      "memory_sync",
			}); err == nil {
				res.SyncedMemories += len(list)
			}
		}
	}

	// 2. Sync pending Git commits, coalescing same-date same-track commits into clean milestones
	if len(state.PendingCommits) > 0 {
		var aiMapped map[string]CommitProgressItem
		if ev := s.evalForTask(ctx, TaskProgress); EvaluatorSupportsTask(ev, TaskProgress) {
			if catz, ok := ev.(ProgressCommitCategorizer); ok {
				pctx := WithPresetCategories(ctx, s.EffectiveTaxonomyJSON(ctx))
				evalStart := time.Now()
				items, aerr := catz.SummarizeCommitProgress(pctx, sc, lang, state.PendingCommits)
				s.recordEvalTelemetry(pctx, TaskProgress, ev, evalStart, len(state.PendingCommits), aerr)
				if aerr == nil {
					aiMapped = make(map[string]CommitProgressItem, len(items))
					for _, it := range items {
						aiMapped[strings.TrimSpace(it.SHA)] = it
					}
				}
			}
		}

		type commitMilestone struct {
			date    string
			track   string
			cat     string
			sub     string
			headSHA string
			sums    []string
			details []string
			count   int
		}
		type msKey struct{ date, track string }
		msMap := make(map[msKey]*commitMilestone)
		var msOrder []msKey

		for _, c := range state.PendingCommits {
			track, cat, sub, sum := inferCommitCategoryFallback(sc, c)
			if it, ok := aiMapped[c.SHA]; ok && strings.TrimSpace(it.Summary) != "" {
				cat, sub = SplitCategoryAndSub(sc, it.Category, it.SubCategory)
				sum = strings.TrimSpace(it.Summary)
				if strings.TrimSpace(it.Track) != "" {
					track = strings.TrimSpace(it.Track)
				} else {
					track = EffectiveProgressTrack("", cat, sub)
				}
			}
			key := msKey{date: c.Date, track: strings.ToLower(track)}
			ms := msMap[key]
			if ms == nil {
				ms = &commitMilestone{date: c.Date, track: track, cat: cat, sub: sub}
				msMap[key] = ms
				msOrder = append(msOrder, key)
			}
			ms.headSHA = c.SHA
			ms.sums = append(ms.sums, sum)
			detLine := fmt.Sprintf("[git:%s] %s", c.SHA, sum)
			if len(c.Files) > 0 {
				detLine += fmt.Sprintf(" (%s)", strings.Join(c.Files, ", "))
			}
			ms.details = append(ms.details, detLine)
			ms.count++
		}

		for _, key := range msOrder {
			ms := msMap[key]
			combinedSum := strings.Join(ms.sums, " · ")
			if ms.date != "" {
				combinedSum = fmt.Sprintf("(%s) %s", ms.date, combinedSum)
			}
			if _, err := s.RecordProgress(ctx, ProgressInput{
				Scope:       sc,
				Track:       ms.track,
				Category:    ms.cat,
				SubCategory: ms.sub,
				Status:      ProgressStatusDone,
				Summary:     combinedSum,
				Details:     strings.Join(ms.details, "\n"),
				CommitSHA:   ms.headSHA,
				Source:      "git_sync",
			}); err == nil {
				res.SyncedCommits += ms.count
			}
		}
	}

	// 3. Resolve memory-triggered 'needs_sync' items when repository sync completes at HEAD
	if state.HeadCommit != "" && (res.SyncedCommits > 0 || state.SyncedCommit != state.HeadCommit) {
		if r, err := s.db.ExecContext(ctx, `
			UPDATE project_progress
			SET status = 'done',
			    commit_sha = CASE WHEN COALESCE(commit_sha, '') = '' THEN ? ELSE commit_sha END,
			    updated_at = CURRENT_TIMESTAMP
			WHERE scope = ? AND status = 'needs_sync'
		`, state.HeadCommit, sc); err == nil {
			n, _ := r.RowsAffected()
			res.ResolvedSyncs = int(n)
		}
	}

	if state.RepoPath != "" && state.HeadCommit != "" {
		_ = s.SetSyncedCommit(ctx, sc, state.HeadCommit)
		s.RememberRepoPath(ctx, sc, state.RepoPath)
	}
	return res, nil
}

func (s *Service) distinctProjectScopes(ctx context.Context) []string {
	seen := map[string]bool{}
	rows, err := s.db.QueryContext(ctx, `
		SELECT DISTINCT scope FROM memories WHERE active = 1 AND scope LIKE 'project:%'
		UNION
		SELECT DISTINCT scope FROM project_progress WHERE scope LIKE 'project:%'
		UNION
		SELECT DISTINCT scope FROM repo_sync WHERE scope LIKE 'project:%'
	`)
	if err == nil {
		for rows.Next() {
			var sc string
			if rows.Scan(&sc) == nil {
				sc = strings.TrimSpace(sc)
				if strings.HasPrefix(sc, "project:") {
					seen[sc] = true
				}
			}
		}
		rows.Close()
	}
	if len(seen) == 0 {
		if d := DefaultScope(); strings.HasPrefix(d, "project:") {
			seen[d] = true
		}
	}
	out := make([]string, 0, len(seen))
	for sc := range seen {
		out = append(out, sc)
	}
	sort.Strings(out)
	return out
}

// InspectAllProjectsProgressSync aggregates sync status across all recorded project:* scopes
// for the All-Projects unified Progress board.
func (s *Service) InspectAllProjectsProgressSync(ctx context.Context) ProgressSyncState {
	scopes := s.distinctProjectScopes(ctx)
	agg := ProgressSyncState{
		Scope:  "",
		InSync: true,
	}
	for _, sc := range scopes {
		st := s.InspectProgressSync(ctx, sc, "")
		agg.UnlinkedMemoryCount += st.UnlinkedMemoryCount
		if len(st.PendingCommits) > 0 {
			agg.PendingCommits = append(agg.PendingCommits, st.PendingCommits...)
		}
		if !st.InSync {
			agg.InSync = false
		}
		if agg.HeadCommit == "" && st.HeadCommit != "" {
			agg.HeadCommit = st.HeadCommit
			agg.RepoPath = st.RepoPath
		}
	}
	return agg
}

// SyncAllProjectsProgress runs SyncProjectProgress across all recorded project:* scopes.
func (s *Service) SyncAllProjectsProgress(ctx context.Context, lang string) (ProgressSyncResult, error) {
	scopes := s.distinctProjectScopes(ctx)
	var total ProgressSyncResult
	for _, sc := range scopes {
		r, err := s.SyncProjectProgress(ctx, sc, "", lang)
		if err != nil {
			return total, err
		}
		total.SyncedMemories += r.SyncedMemories
		total.SyncedCommits += r.SyncedCommits
		total.ResolvedSyncs += r.ResolvedSyncs
		if total.HeadCommit == "" && r.HeadCommit != "" {
			total.HeadCommit = r.HeadCommit
		}
	}
	return total, nil
}
