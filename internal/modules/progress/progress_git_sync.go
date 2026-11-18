package progress

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"memorize/internal/domain"
	"memorize/internal/infra/git"
)

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

func truncateRunes(s string, maxRunes int) string {
	r := []rune(strings.TrimSpace(s))
	if maxRunes <= 0 || len(r) <= maxRunes {
		return string(r)
	}
	if maxRunes <= 3 {
		return string(r[:maxRunes])
	}
	return strings.TrimSpace(string(r[:maxRunes-3])) + "..."
}

func formatCommitGroupSummary(date, track, lang string, sums []string) string {
	if len(sums) == 0 {
		return ""
	}
	isKO := strings.EqualFold(strings.TrimSpace(lang), "ko") || domain.ContainsCJKRune(track)
	if !isKO {
		for _, s := range sums {
			if domain.ContainsCJKRune(s) {
				isKO = true
				break
			}
		}
	}

	var body string
	switch len(sums) {
	case 1:
		body = strings.TrimSpace(sums[0])
	case 2:
		s0, s1 := strings.TrimSpace(sums[0]), strings.TrimSpace(sums[1])
		if len([]rune(s0))+len([]rune(s1))+3 <= 140 {
			body = s0 + " · " + s1
		} else if isKO {
			body = fmt.Sprintf("%s 외 1건 (2개 커밋)", truncateRunes(s1, 100))
		} else {
			body = fmt.Sprintf("%s (+1 more, 2 commits)", truncateRunes(s1, 100))
		}
	default:
		latest := truncateRunes(sums[len(sums)-1], 80)
		if isKO {
			body = fmt.Sprintf("[%s] %s 외 %d건 요약 (%d개 커밋)", track, latest, len(sums)-1, len(sums))
		} else {
			body = fmt.Sprintf("[%s] %s (+%d more, %d commits)", track, latest, len(sums)-1, len(sums))
		}
	}

	if d := strings.TrimSpace(date); d != "" {
		return fmt.Sprintf("(%s) %s", d, body)
	}
	return body
}

func formatCommitDetailLine(c domain.GitCommitBrief, sum string) string {
	line := fmt.Sprintf("[git:%s] %s", c.SHA, sum)
	if len(c.Files) > 0 {
		line += fmt.Sprintf(" (%s)", strings.Join(c.Files, ", "))
	}
	return line
}

func (s *Service) coalesceAndRecordCommitMilestones(
	ctx context.Context,
	sc, lang string,
	commits []domain.GitCommitBrief,
	aiMapped map[string]domain.CommitProgressItem,
) int {
	if len(commits) == 0 {
		return 0
	}
	type resolvedCommit struct {
		commit domain.GitCommitBrief
		track  string
		cat    string
		sub    string
		sum    string
	}
	resolved := make([]resolvedCommit, 0, len(commits))
	type msKey struct{ date, bucket string }
	trackBuckets := make(map[msKey]bool)

	for _, c := range commits {
		track, cat, sub, sum := inferCommitCategoryFallback(sc, c)
		if it, ok := aiMapped[c.SHA]; ok && strings.TrimSpace(it.Summary) != "" {
			cat, sub = domain.SplitCategoryAndSub(sc, it.Category, it.SubCategory)
			sum = strings.TrimSpace(it.Summary)
			if strings.TrimSpace(it.Track) != "" {
				track = strings.TrimSpace(it.Track)
			} else {
				track = domain.EffectiveProgressTrack("", cat, sub)
			}
		}
		resolved = append(resolved, resolvedCommit{
			commit: c,
			track:  track,
			cat:    cat,
			sub:    sub,
			sum:    sum,
		})
		trackBuckets[msKey{date: c.Date, bucket: strings.ToLower(track)}] = true
	}

	// When syncing many commits scattered across >4 micro-tracks, roll up by (date, category)
	// so the activity timeline summarizes milestones rather than exploding into per-commit cards.
	rollByCategory := len(commits) >= 5 && len(trackBuckets) > 4

	msMap := make(map[msKey]*commitMilestone)
	var msOrder []msKey
	for _, rc := range resolved {
		bucketTrack := rc.track
		bucketSub := rc.sub
		if rollByCategory {
			bucketTrack = rc.cat
			bucketSub = ""
		}
		key := msKey{date: rc.commit.Date, bucket: strings.ToLower(bucketTrack)}
		ms := msMap[key]
		if ms == nil {
			ms = &commitMilestone{
				date:  rc.commit.Date,
				track: bucketTrack,
				cat:   rc.cat,
				sub:   bucketSub,
			}
			msMap[key] = ms
			msOrder = append(msOrder, key)
		}
		ms.headSHA = rc.commit.SHA
		ms.sums = append(ms.sums, rc.sum)
		ms.details = append(ms.details, formatCommitDetailLine(rc.commit, rc.sum))
		ms.count++
	}

	synced := 0
	for _, key := range msOrder {
		ms := msMap[key]
		combinedSum := formatCommitGroupSummary(ms.date, ms.track, lang, ms.sums)
		if _, err := s.RecordProgress(ctx, domain.ProgressInput{
			Scope:       sc,
			Track:       ms.track,
			Category:    ms.cat,
			SubCategory: ms.sub,
			Summary:     combinedSum,
			Details:     strings.Join(ms.details, "\n"),
			CommitSHA:   ms.headSHA,
			Source:      "git_sync",
		}); err == nil {
			synced += ms.count
		}
	}
	return synced
}

// AutoSyncProgressCommits performs fast, deterministic (<5ms, zero-LLM) incremental Git commit sync
// into project_progress on SessionStart/PreInvocation turns.
func (s *Service) AutoSyncProgressCommits(ctx context.Context, rawScope, repoDir string) int {
	sc := domain.NormalizeScope(rawScope)
	if !strings.HasPrefix(sc, "project:") {
		return 0
	}
	root := s.ResolveScopeRepoPath(ctx, sc, repoDir)
	if root == "" {
		return 0
	}
	lastCommit := s.mem.GetSyncedCommit(ctx, sc)
	head, pending := s.collectPendingGitCommits(ctx, sc, root, lastCommit, 10)
	return s.recordAutoProgressCommits(ctx, sc, head, pending)
}

// AutoSyncProgressCommitsFromPayload ingests client-collected Git commits into project_progress
// and advances the caller's per-user commit cursor without touching host server disk.
func (s *Service) AutoSyncProgressCommitsFromPayload(ctx context.Context, rawScope, head string, commits []domain.GitCommitBrief) int {
	sc := domain.NormalizeScope(rawScope)
	if !strings.HasPrefix(sc, "project:") || len(commits) == 0 {
		return 0
	}
	existingSHAs := s.loadExistingCommitSHAs(ctx, sc)
	var pending []domain.GitCommitBrief
	for _, c := range commits {
		sha := strings.TrimSpace(c.SHA)
		if len(sha) >= 7 && existingSHAs[sha[:7]] {
			continue
		}
		pending = append(pending, c)
	}
	return s.recordAutoProgressCommits(ctx, sc, strings.TrimSpace(head), pending)
}

func (s *Service) loadExistingCommitSHAs(ctx context.Context, scope string) map[string]bool {
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
						if end := strings.IndexByte(line[5:], ']'); end > 0 {
							tok := strings.TrimSpace(line[5 : 5+end])
							if len(tok) >= 7 {
								existingSHAs[tok[:7]] = true
							}
						}
					}
				}
			}
		}
		_ = rows.Close()
	}
	return existingSHAs
}

func (s *Service) recordAutoProgressCommits(ctx context.Context, sc, head string, pending []domain.GitCommitBrief) int {
	if head == "" || len(pending) == 0 {
		return 0
	}
	synced := s.coalesceAndRecordCommitMilestones(ctx, sc, "", pending, nil)
	if synced > 0 {
		_ = s.mem.SetSyncedCommit(ctx, sc, head)
	}
	return synced
}

func isValidYYYYMMDD(s string) bool {
	if len(s) != 10 || s[4] != '-' || s[7] != '-' {
		return false
	}
	_, err := time.Parse("2006-01-02", s)
	return err == nil
}

func matchPendingCommitSHA(rawSHA string, pending []domain.GitCommitBrief, used map[string]bool) (domain.GitCommitBrief, bool) {
	q := strings.ToLower(strings.TrimSpace(rawSHA))
	if q == "" {
		return domain.GitCommitBrief{}, false
	}
	for _, c := range pending {
		if used[c.SHA] {
			continue
		}
		cl := strings.ToLower(c.SHA)
		if cl == q || (len(q) >= 7 && (strings.HasPrefix(cl, q) || strings.HasPrefix(q, cl))) {
			return c, true
		}
	}
	return domain.GitCommitBrief{}, false
}

// SyncProjectProgress synchronizes unlinked active memories and unsynced Git commits (last_commit..HEAD)
// into work-track milestones with full expandable details and advances the repo_sync cursor.
func (s *Service) SyncProjectProgress(ctx context.Context, rawScope, explicitRepo, lang string) (domain.ProgressSyncResult, error) {
	sc := domain.NormalizeScope(rawScope)
	s.upgradeLegacyMemorySyncRows(ctx, sc)
	state := s.InspectProgressSync(ctx, sc, explicitRepo)
	res := domain.ProgressSyncResult{
		Scope:      sc,
		HeadCommit: state.HeadCommit,
	}

	// 1. Baseline / incremental sync of unlinked active memories grouped by category/sub_category work track
	unlinked, err := s.unlinkedActiveMemories(ctx, sc)
	if err == nil && len(unlinked) > 0 {
		type grpKey struct{ cat, sub string }
		groups := make(map[grpKey][]domain.Fact)
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
			if _, err := s.RecordProgress(ctx, domain.ProgressInput{
				Scope:       sc,
				Track:       track,
				Category:    k.cat,
				SubCategory: k.sub,
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

	// 2. Sync pending Git commits, synthesizing cohesive milestones when multiple commits accumulate.
	if len(state.PendingCommits) > 0 {
		ev := s.router.EvalForTask(ctx, domain.TaskProgress)
		pctx := domain.WithPresetCategories(ctx, s.mem.EffectiveTaxonomyJSON(ctx))
		covered := make(map[string]bool, len(state.PendingCommits))

		if len(state.PendingCommits) >= 3 && domain.EvaluatorSupportsTask(ev, domain.TaskProgress) {
			if ms, ok := ev.(domain.ProgressMilestoneSummarizer); ok {
				evalStart := time.Now()
				milestones, merr := ms.SummarizeCommitMilestones(pctx, sc, lang, state.PendingCommits)
				s.router.RecordTelemetry(pctx, domain.TaskProgress, ev, evalStart, len(state.PendingCommits), merr)
				if merr == nil && len(milestones) > 0 {
					for _, m := range milestones {
						sumText := strings.TrimSpace(m.Summary)
						if sumText == "" {
							continue
						}
						var matched []domain.GitCommitBrief
						var detLines []string
						latestDate := ""
						headSHA := ""
						for _, rawSHA := range m.CommitSHAs {
							if c, found := matchPendingCommitSHA(rawSHA, state.PendingCommits, covered); found {
								covered[c.SHA] = true
								matched = append(matched, c)
								detLines = append(detLines, formatCommitDetailLine(c, git.CleanCommitSubject(c.Subject)))
								headSHA = c.SHA
								if c.Date > latestDate {
									latestDate = c.Date
								}
							}
						}
						if len(matched) == 0 {
							continue
						}
						cat, sub := domain.SplitCategoryAndSub(sc, m.Category, m.SubCategory)
						track := domain.EffectiveProgressTrack(m.Track, cat, sub)
						date := strings.TrimSpace(m.Date)
						if !isValidYYYYMMDD(date) {
							date = latestDate
						}
						if date != "" && !(len(sumText) >= 12 && sumText[0] == '(' && sumText[11] == ')' && isValidYYYYMMDD(sumText[1:11])) {
							sumText = fmt.Sprintf("(%s) %s", date, sumText)
						}
						if _, rerr := s.RecordProgress(ctx, domain.ProgressInput{
							Scope:       sc,
							Track:       track,
							Category:    cat,
							SubCategory: sub,
							Summary:     sumText,
							Details:     strings.Join(detLines, "\n"),
							CommitSHA:   headSHA,
							Source:      "git_sync",
						}); rerr == nil {
							res.SyncedCommits += len(matched)
						}
					}
				}
			}
		}

		var remaining []domain.GitCommitBrief
		for _, c := range state.PendingCommits {
			if !covered[c.SHA] {
				remaining = append(remaining, c)
			}
		}
		if len(remaining) > 0 {
			var aiMapped map[string]domain.CommitProgressItem
			if domain.EvaluatorSupportsTask(ev, domain.TaskProgress) {
				if catz, ok := ev.(domain.ProgressCommitCategorizer); ok {
					evalStart := time.Now()
					items, aerr := catz.SummarizeCommitProgress(pctx, sc, lang, remaining)
					s.router.RecordTelemetry(pctx, domain.TaskProgress, ev, evalStart, len(remaining), aerr)
					if aerr == nil {
						aiMapped = make(map[string]domain.CommitProgressItem, len(items))
						for _, it := range items {
							aiMapped[strings.TrimSpace(it.SHA)] = it
						}
					}
				}
			}
			res.SyncedCommits += s.coalesceAndRecordCommitMilestones(ctx, sc, lang, remaining, aiMapped)
		}
	}

	if state.RepoPath != "" && state.HeadCommit != "" {
		_ = s.mem.SetSyncedCommit(ctx, sc, state.HeadCommit)
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
		if d := domain.DefaultScope(); strings.HasPrefix(d, "project:") {
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
func (s *Service) InspectAllProjectsProgressSync(ctx context.Context) domain.ProgressSyncState {
	scopes := s.distinctProjectScopes(ctx)
	agg := domain.ProgressSyncState{
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
func (s *Service) SyncAllProjectsProgress(ctx context.Context, lang string) (domain.ProgressSyncResult, error) {
	scopes := s.distinctProjectScopes(ctx)
	var total domain.ProgressSyncResult
	for _, sc := range scopes {
		r, err := s.SyncProjectProgress(ctx, sc, "", lang)
		if err != nil {
			return total, err
		}
		total.SyncedMemories += r.SyncedMemories
		total.SyncedCommits += r.SyncedCommits
		if total.HeadCommit == "" && r.HeadCommit != "" {
			total.HeadCommit = r.HeadCommit
		}
	}
	return total, nil
}
