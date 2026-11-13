package memory

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// RememberRepoPath records the resolved Git repository root for a project:* scope
// so Dashboard Progress Sync can inspect commits even when invoked from a browser.
func (s *Service) RememberRepoPath(ctx context.Context, rawScope, repoDir string) {
	sc := NormalizeScope(rawScope)
	if !strings.HasPrefix(sc, "project:") {
		return
	}
	root := ResolveGitRoot(repoDir)
	if root == "" {
		return
	}
	_, _ = s.db.ExecContext(ctx, `
		INSERT INTO repo_sync (scope, last_commit, repo_path, updated_at)
		VALUES (?, '', ?, CURRENT_TIMESTAMP)
		ON CONFLICT(scope) DO UPDATE SET
			repo_path = excluded.repo_path,
			updated_at = CURRENT_TIMESTAMP
	`, sc, root)
}

func (s *Service) ResolveScopeRepoPath(ctx context.Context, rawScope, explicitRepo string) string {
	sc := NormalizeScope(rawScope)
	if !strings.HasPrefix(sc, "project:") {
		return ""
	}
	if r := ResolveGitRoot(explicitRepo); r != "" {
		s.RememberRepoPath(ctx, sc, r)
		return r
	}
	var stored string
	_ = s.db.QueryRowContext(ctx, `SELECT COALESCE(repo_path, '') FROM repo_sync WHERE scope = ?`, sc).Scan(&stored)
	if r := ResolveGitRoot(stored); r != "" {
		return r
	}
	projName := strings.TrimPrefix(sc, "project:")
	if wd, err := os.Getwd(); err == nil && wd != "" {
		if wdRoot := ResolveGitRoot(wd); wdRoot != "" {
			if strings.EqualFold(filepath.Base(wdRoot), projName) {
				s.RememberRepoPath(ctx, sc, wdRoot)
				return wdRoot
			}
			sib := filepath.Join(filepath.Dir(wdRoot), projName)
			if sibRoot := ResolveGitRoot(sib); sibRoot != "" && strings.EqualFold(filepath.Base(sibRoot), projName) {
				s.RememberRepoPath(ctx, sc, sibRoot)
				return sibRoot
			}
		}
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
	if d := DefaultScope(); strings.HasPrefix(d, "project:") {
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

func buildMemoryMutationSummaryAndDetails(cat, sub string, facts []Fact) (string, string, string) {
	track := EffectiveProgressTrack("", cat, sub)
	if len(facts) == 0 {
		return track, "", ""
	}
	isKO := containsCJKRune(facts[len(facts)-1].Content)
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

func (s *Service) factsForProgressIDs(ctx context.Context, memIDs []int64) []Fact {
	recs, err := s.GetRecordsByIDs(WithAdminBypass(ctx), memIDs)
	if err != nil || len(recs) == 0 {
		return nil
	}
	byID := make(map[int64]MemoryRecord, len(recs))
	for _, mr := range recs {
		byID[mr.ID] = mr
	}
	out := make([]Fact, 0, len(memIDs))
	for _, id := range memIDs {
		if mr, ok := byID[id]; ok {
			out = append(out, Fact{
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

// RecordMemoryMutationProgress automatically links project:* memory additions, overwrites,
// updates, and consolidations to a separated project_progress milestone with full expandable details.
func (s *Service) RecordMemoryMutationProgress(ctx context.Context, rawScope, category, subCategory, actionKind string, newID int64, oldIDs []int64, content string) {
	sc := NormalizeScope(rawScope)
	if !strings.HasPrefix(sc, "project:") || newID <= 0 {
		return
	}
	cat, sub := SplitCategoryAndSub(sc, category, subCategory)
	track := EffectiveProgressTrack("", cat, sub)
	isKO := containsCJKRune(content)
	fullText := strings.TrimSpace(content)

	if actionKind == "consolidate" {
		prefix := fmt.Sprintf("[Memory #%d consolidated]", newID)
		if isKO {
			prefix = fmt.Sprintf("[기억 #%d 병합 정리]", newID)
		}
		entryLine := prefix + " " + fullText
		_, _ = s.RecordProgress(ctx, ProgressInput{
			Scope:       sc,
			Track:       track,
			Category:    cat,
			SubCategory: sub,
			Status:      ProgressStatusDone,
			Summary:     entryLine,
			Details:     entryLine,
			MemoryIDs:   []int64{newID},
			Source:      "memory",
		})
		return
	}

	var recentID int64
	var recentMems string
	err := s.db.QueryRowContext(ctx, `
		SELECT id, COALESCE(memory_ids, '')
		FROM project_progress
		WHERE scope = ? AND category = ? AND COALESCE(sub_category, '') = ?
		  AND source = 'memory' AND status = 'done'
		  AND updated_at >= datetime('now', '-15 minutes')
		ORDER BY id DESC LIMIT 1
	`, sc, cat, sub).Scan(&recentID, &recentMems)
	if err == nil && recentID > 0 {
		mergedMems := parseMemoryIDsStr(recentMems)
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
			facts = []Fact{{ID: newID, Scope: sc, Category: cat, SubCategory: sub, Content: fullText}}
		}
		_, combinedSum, baseDetails := buildMemoryMutationSummaryAndDetails(cat, sub, facts)
		sid := s.ResolveEffectiveSessionID(ctx, sc, "")
		_, _ = s.db.ExecContext(ctx, `
			UPDATE project_progress
			SET track = ?,
			    session_id = CASE WHEN COALESCE(session_id, '') = '' THEN ? ELSE session_id END,
			    status = 'done',
			    summary = ?,
			    details = ?,
			    memory_ids = ?,
			    updated_at = CURRENT_TIMESTAMP
			WHERE id = ?
		`, track, sid, combinedSum, baseDetails, formatMemoryIDsStr(mergedMems), recentID)
		s.NotifyDomainChange("progress")
		return
	}

	_, sum, det := buildMemoryMutationSummaryAndDetails(cat, sub, []Fact{{
		ID: newID, Scope: sc, Category: cat, SubCategory: sub, Content: fullText,
	}})
	_, _ = s.RecordProgress(ctx, ProgressInput{
		Scope:       sc,
		Track:       track,
		Category:    cat,
		SubCategory: sub,
		Status:      ProgressStatusDone,
		Summary:     sum,
		Details:     det,
		MemoryIDs:   []int64{newID},
		Source:      "memory",
	})
}

func (s *Service) collectPendingGitCommits(ctx context.Context, scope, repoRoot, lastCommit string, maxInitial int) (string, []GitCommitBrief) {
	if repoRoot == "" {
		return "", nil
	}
	head := fastGitHead(ctx, repoRoot)
	if head == "" {
		return "", nil
	}
	existingSHAs := make(map[string]bool)
	if rows, err := s.db.QueryContext(ctx, `SELECT COALESCE(commit_sha, ''), COALESCE(details, '') FROM project_progress WHERE scope = ?`, scope); err == nil {
		for rows.Next() {
			var sha, det string
			if rows.Scan(&sha, &det) == nil {
				if clean := strings.TrimSpace(sha); len(clean) >= 7 {
					existingSHAs[clean[:7]] = true
				}
				for _, line := range strings.Split(det, "\n") {
					line = strings.TrimSpace(line)
					if strings.HasPrefix(line, "[git:") {
						if r := strings.Index(line, "]"); r > 5 {
							s7 := strings.TrimSpace(line[5:r])
							if len(s7) >= 7 {
								existingSHAs[s7[:7]] = true
							}
						}
					}
				}
			}
		}
		rows.Close()
	}

	var args []string
	if lastCommit != "" {
		if lastCommit == head || gitRevParse(ctx, repoRoot, lastCommit) == head {
			return head, nil
		}
		if _, err := runGit(ctx, repoRoot, "merge-base", "--is-ancestor", lastCommit, "HEAD"); err == nil {
			args = []string{"log", "--reverse", "-n", "15", "--format=COMMIT\t%h\t%cs\t%s", "--name-only", lastCommit + "..HEAD"}
		} else {
			args = []string{"log", "--reverse", "-n", strconv.Itoa(maxInitial), "--format=COMMIT\t%h\t%cs\t%s", "--name-only"}
		}
	} else {
		args = []string{"log", "--reverse", "-n", strconv.Itoa(maxInitial), "--format=COMMIT\t%h\t%cs\t%s", "--name-only"}
	}

	out, err := runGit(ctx, repoRoot, args...)
	if err != nil || strings.TrimSpace(out) == "" {
		return head, nil
	}

	var commits []GitCommitBrief
	var cur *GitCommitBrief
	for _, rawLine := range strings.Split(out, "\n") {
		line := strings.TrimSpace(rawLine)
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "COMMIT\t") {
			if cur != nil {
				if len(cur.SHA) < 7 || !existingSHAs[cur.SHA[:7]] {
					commits = append(commits, *cur)
				}
			}
			parts := strings.SplitN(line, "\t", 4)
			if len(parts) >= 4 {
				cur = &GitCommitBrief{
					SHA:     strings.TrimSpace(parts[1]),
					Date:    strings.TrimSpace(parts[2]),
					Subject: strings.TrimSpace(parts[3]),
				}
			} else {
				cur = nil
			}
			continue
		}
		if cur != nil && len(cur.Files) < 8 {
			cur.Files = append(cur.Files, line)
		}
	}
	if cur != nil && (len(cur.SHA) < 7 || !existingSHAs[cur.SHA[:7]]) {
		commits = append(commits, *cur)
	}
	return head, commits
}

func (s *Service) unlinkedActiveMemories(ctx context.Context, scope string) ([]Fact, error) {
	linked := make(map[int64]bool)
	if rows, err := s.db.QueryContext(ctx, `SELECT COALESCE(memory_ids, '') FROM project_progress WHERE scope = ? AND COALESCE(memory_ids, '') != ''`, scope); err == nil {
		for rows.Next() {
			var raw string
			if rows.Scan(&raw) == nil {
				for _, id := range parseMemoryIDsStr(raw) {
					linked[id] = true
				}
			}
		}
		rows.Close()
	}
	facts, err := s.loadActiveFacts(ctx, scope, scope, "")
	if err != nil {
		return nil, err
	}
	hwm := s.getProgressPrunedMemHWM(ctx, scope)
	var out []Fact
	for _, f := range facts {
		if !linked[f.ID] && f.ID > hwm {
			out = append(out, f)
		}
	}
	return out, nil
}

func (s *Service) InspectProgressSync(ctx context.Context, rawScope, explicitRepo string) ProgressSyncState {
	sc := NormalizeScope(rawScope)
	repoRoot := s.ResolveScopeRepoPath(ctx, sc, explicitRepo)
	last := s.GetSyncedCommit(ctx, sc)
	head, pending := s.collectPendingGitCommits(ctx, sc, repoRoot, last, 8)
	unlinked, _ := s.unlinkedActiveMemories(ctx, sc)

	inSync := len(unlinked) == 0 && (repoRoot == "" || (last != "" && len(pending) == 0 && last == head))
	return ProgressSyncState{
		Scope:               sc,
		RepoPath:            repoRoot,
		SyncedCommit:        last,
		HeadCommit:          head,
		PendingCommits:      pending,
		UnlinkedMemoryCount: len(unlinked),
		InSync:              inSync,
	}
}

func inferCommitCategoryFallback(scope string, c GitCommitBrief) (string, string, string, string) {
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
				cat, sub = "architecture", NormalizeSubCategory(hint)
			}
		}
	} else {
		joined := strings.ToLower(strings.Join(c.Files, " "))
		switch {
		case strings.Contains(joined, "dashboard") || strings.Contains(joined, ".html") || strings.Contains(joined, ".css"):
			cat, sub = "architecture", "dashboard"
		case strings.Contains(joined, "_test.go") || strings.Contains(joined, "/test"):
			cat, sub = "testing", "unit"
		case strings.Contains(joined, "adapter") || strings.Contains(joined, "localmodel") || strings.Contains(joined, "daemon"):
			cat, sub = "architecture", "engine"
		case strings.Contains(joined, "readme") || strings.Contains(joined, ".md"):
			cat, sub = "convention", "documentation"
		}
	}
	cat, sub = SplitCategoryAndSub(scope, cat, sub)
	track := EffectiveProgressTrack("", cat, sub)
	return track, cat, sub, cleanCommitSubject(subj)
}

func buildMemorySyncSummaryAndDetails(cat, sub, lang string, list []Fact) (string, string, string) {
	track := EffectiveProgressTrack("", cat, sub)
	if len(list) == 0 {
		return track, "", ""
	}
	isKO := lang == "ko" || containsCJKRune(list[0].Content)
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
		status string
		memIDs []int64
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, scope, category, COALESCE(sub_category, ''), COALESCE(source, ''), status, COALESCE(memory_ids, '')
		FROM project_progress
		WHERE (? = '' OR scope = ?)
		  AND source IN ('memory_sync', 'memory')
		  AND COALESCE(memory_ids, '') != ''
		  AND (COALESCE(details, '') = '' OR status = 'needs_sync')
		ORDER BY id ASC
	`, scope, scope)
	if err != nil {
		return
	}
	var targets []legacyRow
	for rows.Next() {
		var r legacyRow
		var raw string
		if rows.Scan(&r.id, &r.sc, &r.cat, &r.sub, &r.source, &r.status, &raw) == nil {
			r.memIDs = parseMemoryIDsStr(raw)
			if len(r.memIDs) > 0 {
				targets = append(targets, r)
			}
		}
	}
	rows.Close()

	type needsSyncKey struct{ sc, cat, sub string }
	type coalescedGroup struct {
		keepID    int64
		cat       string
		sub       string
		memIDs    []int64
		seenMems  map[int64]bool
		deleteIDs []int64
		rowCount  int
	}
	nsGroups := make(map[needsSyncKey]*coalescedGroup)
	var nsOrder []needsSyncKey

	for _, t := range targets {
		if t.source == "memory" && t.status == ProgressStatusNeedsSync {
			k := needsSyncKey{t.sc, t.cat, t.sub}
			g := nsGroups[k]
			if g == nil {
				g = &coalescedGroup{keepID: t.id, cat: t.cat, sub: t.sub, seenMems: map[int64]bool{}}
				nsGroups[k] = g
				nsOrder = append(nsOrder, k)
			} else {
				g.deleteIDs = append(g.deleteIDs, g.keepID)
				g.keepID = t.id
			}
			g.rowCount++
			for _, id := range t.memIDs {
				if g.seenMems[id] {
					var next []int64
					for _, x := range g.memIDs {
						if x != id {
							next = append(next, x)
						}
					}
					g.memIDs = append(next, id)
				} else {
					g.seenMems[id] = true
					g.memIDs = append(g.memIDs, id)
				}
			}
			continue
		}

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
				    status = 'done',
				    summary = ?,
				    details = ?
				WHERE id = ?
			`, track, sum, details, t.id)
		}
	}

	for _, k := range nsOrder {
		g := nsGroups[k]
		facts := s.factsForProgressIDs(ctx, g.memIDs)
		if len(facts) > 0 {
			track, sum, details := buildMemoryMutationSummaryAndDetails(g.cat, g.sub, facts)
			_, _ = s.db.ExecContext(ctx, `
				UPDATE project_progress
				SET track = CASE WHEN COALESCE(track, '') = '' THEN ? ELSE track END,
				    status = 'done',
				    summary = ?,
				    details = ?,
				    memory_ids = ?
				WHERE id = ?
			`, track, sum, details, formatMemoryIDsStr(g.memIDs), g.keepID)
		}
		for _, delID := range g.deleteIDs {
			_, _ = s.db.ExecContext(ctx, `DELETE FROM project_progress WHERE id = ?`, delID)
		}
	}

	// Retire any remaining legacy 'needs_sync' rows into completed history ('done').
	_, _ = s.db.ExecContext(ctx, `
		UPDATE project_progress
		SET status = 'done'
		WHERE (? = '' OR scope = ?) AND status = 'needs_sync'
	`, scope, scope)
}
