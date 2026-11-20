package progress

import (
	"context"
	"fmt"
	"strings"

	"praxis/internal/domain"
	"praxis/internal/infra/git"
)

func (s *Service) RememberRepoPath(ctx context.Context, rawScope, repoDir string) {
	if s != nil && s.mem != nil {
		s.mem.RememberRepoPath(ctx, rawScope, repoDir)
	}
}

func (s *Service) ResolveScopeRepoPath(ctx context.Context, rawScope, explicitRepo string) string {
	if s != nil && s.mem != nil {
		return s.mem.ResolveScopeRepoPath(ctx, rawScope, explicitRepo)
	}
	return ""
}

func (s *Service) TopProjectScope(ctx context.Context) string {
	var sc string
	_ = s.db.QueryRowContext(ctx, `
		SELECT scope FROM (
			SELECT scope, COUNT(*) AS cnt FROM memories WHERE active = 1 AND scope LIKE 'project:%' GROUP BY scope
			UNION ALL
			SELECT scope, COUNT(*) AS cnt FROM project_progress WHERE scope LIKE 'project:%' GROUP BY scope
		)
		GROUP BY scope
		ORDER BY SUM(cnt) DESC, scope ASC
		LIMIT 1
	`).Scan(&sc)
	if strings.TrimSpace(sc) != "" {
		return strings.TrimSpace(sc)
	}
	if d := domain.DefaultScope(); strings.HasPrefix(d, "project:") {
		return d
	}
	return ""
}

func formatHashIDs(ids []int64) string {
	parts := make([]string, 0, len(ids))
	seen := make(map[int64]bool, len(ids))
	for _, id := range ids {
		if id > 0 && !seen[id] {
			seen[id] = true
			parts = append(parts, fmt.Sprintf("#%d", id))
		}
	}
	return strings.Join(parts, ", ")
}

func buildMemoryMutationSummaryAndDetails(cat, sub string, facts []domain.Fact) (string, string, string) {
	track := domain.EffectiveProgressTrack("", cat, sub)
	if len(facts) == 0 {
		return track, "", ""
	}
	isKO := domain.ContainsCJKRune(facts[len(facts)-1].Content)
	ids := make([]int64, len(facts))
	detLines := make([]string, len(facts))
	for i, f := range facts {
		ids[i] = f.ID
		detLines[i] = fmt.Sprintf("#%d: %s", f.ID, strings.TrimSpace(f.Content))
	}
	details := strings.Join(detLines, "\n")
	latest := strings.TrimSpace(facts[len(facts)-1].Content)
	if len(facts) == 1 {
		if isKO {
			return track, fmt.Sprintf("[기억 #%d 반영] %s", facts[0].ID, latest), details
		}
		return track, fmt.Sprintf("[Memory #%d recorded] %s", facts[0].ID, latest), details
	}
	if isKO {
		return track, fmt.Sprintf("[%s 기억 %d건 반영 (%s)] 최신: %s", track, len(facts), formatHashIDs(ids), latest), details
	}
	return track, fmt.Sprintf("[%s: %d memories recorded (%s)] Latest: %s", track, len(facts), formatHashIDs(ids), latest), details
}

func (s *Service) factsForProgressIDs(ctx context.Context, memIDs []int64) []domain.Fact {
	recs, err := s.mem.GetRecordsByIDs(domain.WithAdminBypass(ctx), memIDs)
	if err != nil || len(recs) == 0 {
		return nil
	}
	byID := make(map[int64]domain.MemoryRecord, len(recs))
	for _, mr := range recs {
		byID[mr.ID] = mr
	}
	out := make([]domain.Fact, 0, len(memIDs))
	for _, id := range memIDs {
		if mr, ok := byID[id]; ok {
			out = append(out, domain.Fact{
				ID:          mr.ID,
				Scope:       mr.Scope,
				Category:    mr.Category,
				SubCategory: mr.SubCategory,
				Content:     mr.Content,
			})
		}
	}
	return out
}

func (s *Service) RecordMemoryMutationProgress(ctx context.Context, rawScope, category, subCategory, actionKind string, newID int64, oldIDs []int64, content string) {
	sc := domain.NormalizeScope(rawScope)
	if !strings.HasPrefix(sc, "project:") || newID <= 0 {
		return
	}
	cat, sub := domain.SplitCategoryAndSub(sc, category, subCategory)
	track := domain.EffectiveProgressTrack("", cat, sub)
	isKO := domain.ContainsCJKRune(content)
	fullText := strings.TrimSpace(content)

	if actionKind == "consolidate" {
		prefix := fmt.Sprintf("[Memory #%d consolidated]", newID)
		if isKO {
			prefix = fmt.Sprintf("[기억 #%d 병합 정리]", newID)
		}
		entryLine := prefix + " " + fullText
		_, _ = s.RecordProgress(ctx, domain.ProgressInput{
			Scope:       sc,
			Track:       track,
			Category:    cat,
			SubCategory: sub,
			Summary:     entryLine,
			Details:     entryLine,
			MemoryIDs:   []int64{newID},
			Source:      "memory",
		})
		return
	}

	var recentID int64
	var recentMems string
	accFilter := domain.EffectivePrincipalAccount(ctx)
	err := s.db.QueryRowContext(ctx, `
		SELECT id, COALESCE(memory_ids, '')
		FROM project_progress
		WHERE scope = ? AND category = ? AND COALESCE(sub_category, '') = ?
		  AND source = 'memory'
		  AND (? = '' OR account = ?)
		  AND updated_at >= datetime('now', '-15 minutes')
		ORDER BY id DESC LIMIT 1
	`, sc, cat, sub, accFilter, accFilter).Scan(&recentID, &recentMems)
	if err == nil && recentID > 0 {
		mergedMems := ParseMemoryIDsStr(recentMems)
		hasNew := false
		for _, id := range mergedMems {
			if id == newID {
				hasNew = true
				break
			}
		}
		if !hasNew {
			mergedMems = append(mergedMems, newID)
		} else {
			var reordered []int64
			for _, id := range mergedMems {
				if id != newID {
					reordered = append(reordered, id)
				}
			}
			mergedMems = append(reordered, newID)
		}
		facts := s.factsForProgressIDs(ctx, mergedMems)
		if len(facts) == 0 {
			facts = []domain.Fact{{ID: newID, Scope: sc, Category: cat, SubCategory: sub, Content: fullText}}
		}
		_, combinedSum, baseDetails := buildMemoryMutationSummaryAndDetails(cat, sub, facts)
		sid := s.mem.ResolveEffectiveSessionID(ctx, sc, "")
		_, _ = s.db.ExecContext(ctx, `
			UPDATE project_progress
			SET track = ?,
			    session_id = CASE WHEN COALESCE(session_id, '') = '' THEN ? ELSE session_id END,
			    summary = ?,
			    details = ?,
			    memory_ids = ?,
			    updated_at = CURRENT_TIMESTAMP
			WHERE id = ?
		`, track, sid, combinedSum, baseDetails, formatMemoryIDsStr(mergedMems), recentID)
		s.notifyProgressChange()
		return
	}

	_, sum, det := buildMemoryMutationSummaryAndDetails(cat, sub, []domain.Fact{{
		ID: newID, Scope: sc, Category: cat, SubCategory: sub, Content: fullText,
	}})
	_, _ = s.RecordProgress(ctx, domain.ProgressInput{
		Scope:       sc,
		Track:       track,
		Category:    cat,
		SubCategory: sub,
		Summary:     sum,
		Details:     det,
		MemoryIDs:   []int64{newID},
		Source:      "memory",
	})
}

func (s *Service) collectPendingGitCommits(ctx context.Context, scope, repoRoot, lastCommit string, maxInitial int) (string, []domain.GitCommitBrief) {
	return git.CollectPendingCommits(ctx, repoRoot, lastCommit, maxInitial, s.loadExistingCommitSHAs(ctx, scope))
}

func (s *Service) unlinkedActiveMemories(ctx context.Context, scope string) ([]domain.Fact, error) {
	linked := make(map[int64]bool)
	strictPrivate := 0
	if domain.IsServerMode(ctx) && domain.IsUserPrivateScope(ctx, scope) {
		strictPrivate = 1
	}
	curAcc := domain.EffectivePrincipalAccount(ctx)
	if rows, err := s.db.QueryContext(ctx, `
		SELECT COALESCE(memory_ids, '') FROM project_progress
		WHERE scope = ? AND COALESCE(memory_ids, '') != ''
		  AND (? = 0 OR COALESCE(account, '') = ?)`, scope, strictPrivate, curAcc); err == nil {
		for rows.Next() {
			var raw string
			if rows.Scan(&raw) == nil {
				for _, id := range ParseMemoryIDsStr(raw) {
					linked[id] = true
				}
			}
		}
		rows.Close()
	}
	facts, err := s.mem.LoadActiveFacts(ctx, scope, scope, "")
	if err != nil {
		return nil, err
	}
	hwm := s.getProgressPrunedMemHWM(ctx, scope)
	var out []domain.Fact
	for _, f := range facts {
		if !linked[f.ID] && f.ID > hwm {
			out = append(out, f)
		}
	}
	return out, nil
}

func (s *Service) InspectProgressSync(ctx context.Context, rawScope, explicitRepo string) domain.ProgressSyncState {
	sc := domain.NormalizeScope(rawScope)
	repoRoot := s.ResolveScopeRepoPath(ctx, sc, explicitRepo)
	last := s.mem.GetSyncedCommit(ctx, sc)
	head, pending := s.collectPendingGitCommits(ctx, sc, repoRoot, last, 8)
	unlinked, _ := s.unlinkedActiveMemories(ctx, sc)

	inSync := len(unlinked) == 0 && (repoRoot == "" || (last != "" && len(pending) == 0 && last == head))
	return domain.ProgressSyncState{
		Scope:               sc,
		RepoPath:            repoRoot,
		SyncedCommit:        last,
		HeadCommit:          head,
		PendingCommits:      pending,
		UnlinkedMemoryCount: len(unlinked),
		InSync:              inSync,
	}
}

func inferCommitCategoryFallback(scope string, c domain.GitCommitBrief) (string, string, string, string) {
	subj := strings.TrimSpace(c.Subject)
	cat, sub := "architecture", "workflow"
	if l := strings.Index(subj, "("); l > 0 {
		if r := strings.Index(subj[l:], ")"); r > 1 {
			hint := strings.ToLower(strings.TrimSpace(subj[l+1 : l+r]))
			if idx := strings.Index(hint, ","); idx > 0 {
				hint = strings.TrimSpace(hint[:idx])
			}
			switch hint {
			case "dashboard", "ui", "ssr":
				cat, sub = "architecture", "dashboard"
			case "db", "sqlite", "schema", "database":
				cat, sub = "database", "schema"
			case "test", "tests", "e2e":
				cat, sub = "testing", "unit"
			case "auth", "security", "csrf":
				cat, sub = "security", "guard"
			case "ci", "release", "build":
				cat, sub = "architecture", "build"
			default:
				cat, sub = "architecture", domain.NormalizeSubCategory(hint)
			}
		}
	} else {
		joined := strings.ToLower(strings.Join(c.Files, " "))
		switch {
		case strings.Contains(joined, "dashboard") || strings.Contains(joined, ".html") || strings.Contains(joined, ".css"):
			cat, sub = "architecture", "dashboard"
		case strings.Contains(joined, "_test.go") || strings.Contains(joined, "/test"):
			cat, sub = "testing", "unit"
		case strings.Contains(joined, "adapter") || strings.Contains(joined, "inference") || strings.Contains(joined, "localmodel") || strings.Contains(joined, "daemon"):
			cat, sub = "architecture", "engine"
		case strings.Contains(joined, "readme") || strings.Contains(joined, ".md"):
			cat, sub = "convention", "documentation"
		}
	}
	cat, sub = domain.SplitCategoryAndSub(scope, cat, sub)
	track := domain.EffectiveProgressTrack("", cat, sub)
	return track, cat, sub, git.CleanCommitSubject(subj)
}

func buildMemorySyncSummaryAndDetails(cat, sub, lang string, list []domain.Fact) (string, string, string) {
	track := domain.EffectiveProgressTrack("", cat, sub)
	if len(list) == 0 {
		return track, "", ""
	}
	isKO := lang == "ko" || domain.ContainsCJKRune(list[0].Content)
	ids := make([]int64, len(list))
	detLines := make([]string, len(list))
	for i, f := range list {
		ids[i] = f.ID
		detLines[i] = fmt.Sprintf("#%d: %s", f.ID, strings.TrimSpace(f.Content))
	}
	details := strings.Join(detLines, "\n")

	var sum string
	if len(list) == 1 {
		if isKO {
			sum = fmt.Sprintf("[기억 #%d 동기화] %s", list[0].ID, strings.TrimSpace(list[0].Content))
		} else {
			sum = fmt.Sprintf("[Synced Memory #%d] %s", list[0].ID, strings.TrimSpace(list[0].Content))
		}
		return track, sum, details
	}

	if len(list) <= 2 {
		parts := make([]string, len(list))
		for i, f := range list {
			parts[i] = fmt.Sprintf("#%d %s", f.ID, strings.TrimSpace(f.Content))
		}
		if isKO {
			sum = fmt.Sprintf("[활성 기억 %d건 동기화] %s", len(list), strings.Join(parts, " / "))
		} else {
			sum = fmt.Sprintf("[Synced %d active memories] %s", len(list), strings.Join(parts, " / "))
		}
		return track, sum, details
	}

	if isKO {
		sum = fmt.Sprintf("[%s 활성 기억 %d건 기준선 동기화] 연결 규칙: %s (펼쳐서 전체 원문 확인)", track, len(list), formatHashIDs(ids))
	} else {
		sum = fmt.Sprintf("[%s baseline sync: %d active memories] Linked: %s (expand for full list)", track, len(list), formatHashIDs(ids))
	}
	return track, sum, details
}

func (s *Service) upgradeLegacyMemorySyncRows(ctx context.Context, scope string) {
	type legacyRow struct {
		id     int64
		sc     string
		cat    string
		sub    string
		source string
		memIDs []int64
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, scope, category, COALESCE(sub_category, ''), COALESCE(source, ''), COALESCE(memory_ids, '')
		FROM project_progress
		WHERE (? = '' OR scope = ?)
		  AND source IN ('memory_sync', 'memory')
		  AND COALESCE(memory_ids, '') != ''
		  AND COALESCE(details, '') = ''
		ORDER BY id ASC
	`, scope, scope)
	if err != nil {
		return
	}
	var targets []legacyRow
	for rows.Next() {
		var r legacyRow
		var raw string
		if rows.Scan(&r.id, &r.sc, &r.cat, &r.sub, &r.source, &raw) == nil {
			r.memIDs = ParseMemoryIDsStr(raw)
			if len(r.memIDs) > 0 {
				targets = append(targets, r)
			}
		}
	}
	rows.Close()

	for _, t := range targets {
		facts := s.factsForProgressIDs(ctx, t.memIDs)
		if len(facts) == 0 {
			continue
		}
		var track, sum, details string
		if t.source == "memory" {
			track, sum, details = buildMemoryMutationSummaryAndDetails(t.cat, t.sub, facts)
		} else {
			track, sum, details = buildMemorySyncSummaryAndDetails(t.cat, t.sub, "", facts)
		}
		if sum != "" {
			_, _ = s.db.ExecContext(ctx, `
				UPDATE project_progress
				SET track = CASE WHEN COALESCE(track, '') = '' THEN ? ELSE track END,
				    summary = ?,
				    details = ?
				WHERE id = ?
			`, track, sum, details, t.id)
		}
	}
}
