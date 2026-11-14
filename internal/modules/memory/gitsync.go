package memory

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/sync/errgroup"

	"memorize/internal/domain"
	"memorize/internal/infra/git"
	"memorize/internal/infra/sqlite"
)

// GetSyncedCommit returns the recorded Git commit cursor for rawScope.
func (s *Service) GetSyncedCommit(ctx context.Context, rawScope string) string {
	return sqlite.GetSyncedCommit(ctx, s.db, rawScope)
}

// SetSyncedCommit updates the recorded Git commit cursor for rawScope and clears session sync dedupe state.
func (s *Service) SetSyncedCommit(ctx context.Context, rawScope, sha string) error {
	if err := sqlite.SetSyncedCommit(ctx, s.db, rawScope, sha); err != nil {
		return err
	}
	sc := domain.NormalizeScope(rawScope)
	s.mu.Lock()
	for k := range s.syncSeen {
		if strings.HasSuffix(k, "|"+sc) {
			delete(s.syncSeen, k)
		}
	}
	s.mu.Unlock()
	return nil
}

func (s *Service) shouldNotifySync(sessionID, scope, head string, refresh bool) bool {
	if sessionID == "" {
		return true
	}
	key := sessionID + "|" + scope
	s.mu.Lock()
	defer s.mu.Unlock()
	if !refresh && s.syncSeen[key] == head {
		return false
	}
	s.syncSeen[key] = head
	return true
}

// InspectRepoSync detects unsynced baseline or incremental Git commits for project:* scopes
// (running autonomously when AutoGitSync is enabled, or emitting agent instructions otherwise).
func (s *Service) InspectRepoSync(ctx context.Context, sessionID, rawScope, repoDir string, refresh bool) string {
	if s.IsAutoGitSyncEnabled() {
		return s.AutoSyncRepo(ctx, sessionID, rawScope, repoDir, refresh)
	}

	sc := domain.NormalizeScope(rawScope)
	if !strings.HasPrefix(sc, "project:") {
		return ""
	}
	root := git.ResolveRoot(repoDir)
	if root == "" {
		return ""
	}
	head := git.FastHead(ctx, root)
	if head == "" {
		return ""
	}

	last := s.GetSyncedCommit(ctx, sc)
	if last != "" {
		if last == head || git.RevParse(ctx, root, last) == head {
			return ""
		}
	}

	if last == "" {
		var memCount int
		_ = s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM memories WHERE scope = ?`, sc).Scan(&memCount)
		if memCount > 0 {
			_ = s.SetSyncedCommit(ctx, sc, head)
			return ""
		}
		if !s.shouldNotifySync(sessionID, sc, head, refresh) {
			return ""
		}
		return fmt.Sprintf(
			"\n**[Git Baseline Onboarding — HEAD `%s`]**\n"+
				"Repo: `%s`\n%s\n"+
				"No synced commit recorded yet. Do not scan historical commits; inspect current `HEAD` manifests/docs and call `commit_memory(scope=%q, candidate_facts=[...], synced_commit=%q)` once to anchor the baseline (or pass `candidate_facts=[]` and `synced_commit=%q` if no lasting rules exist).",
			head, root, git.DetectHeadLayout(root), sc, head, head,
		)
	}

	if !s.shouldNotifySync(sessionID, sc, head, refresh) {
		return ""
	}
	if _, err := git.Run(ctx, root, "merge-base", "--is-ancestor", last, "HEAD"); err != nil {
		return fmt.Sprintf(
			"\n**[Git History Rewritten — `%s` -> HEAD `%s`]**\n"+
				"Previous cursor (`%s`) was rewritten by rebase or force-push. Inspect current `HEAD` and reset the cursor via `commit_memory(scope=%q, synced_commit=%q)`.",
			last, head, last, sc, head,
		)
	}

	logOut, err := git.Run(ctx, root, "log", "--reverse", "--format=- %h (%cs) %s", "--name-status", last+"..HEAD")
	if err != nil || logOut == "" {
		return ""
	}
	return fmt.Sprintf(
		"\n**[Incremental Git Sync — `%s..%s`]**\n%s\n"+
			"Review the commits above in chronological order. If architecture, dependencies, or conventions changed, call `commit_memory(scope=%q, candidate_facts=[...], synced_commit=%q)`; otherwise advance the cursor via `commit_memory(scope=%q, synced_commit=%q)`.",
		last, head, logOut, sc, head, sc, head,
	)
}

// CommitBatch evaluates and commits one or more candidate facts concurrently (up to 4 in-flight) and optionally updates synced_commit.
func (s *Service) CommitBatch(ctx context.Context, facts []string, category, rawScope, repoDir, syncedCommit string) (string, error) {
	targetScope := domain.NormalizeScope(rawScope)
	var clean []string
	for _, f := range facts {
		if t := strings.TrimSpace(f); t != "" {
			clean = append(clean, t)
		}
	}
	sha := strings.TrimSpace(syncedCommit)
	if root := git.ResolveRoot(repoDir); root != "" {
		sqlite.RememberRepoPath(ctx, s.db, targetScope, root)
		if sha != "" {
			if norm := git.RevParse(ctx, root, sha); norm != "" {
				sha = norm
			}
		}
	}
	if len(clean) == 0 && sha == "" {
		return "", fmt.Errorf("candidate_fact, candidate_facts, or synced_commit is required")
	}

	var lines []string
	if len(clean) == 1 {
		res, err := s.CommitMemory(ctx, clean[0], category, targetScope)
		if err != nil {
			return "", err
		}
		lines = append(lines, res)
	} else if len(clean) > 1 {
		existing, err := s.LoadActiveFacts(ctx, targetScope, targetScope, "")
		if err != nil {
			return "", err
		}
		evalCtx := domain.WithPresetCategories(ctx, s.EffectiveTaxonomyJSON(ctx))
		ev := s.router.EvalForTask(ctx, domain.TaskMutation)
		if ev == nil {
			return "", fmt.Errorf("no active evaluation engine configured")
		}

		type batchSlot struct {
			skipMsg string
			dec     domain.MutationDecision
		}
		slots := make([]batchSlot, len(clean))
		seenInBatch := make(map[string]bool, len(clean))
		for i, fact := range clean {
			if reason := domain.CheckSecurityGuard(fact); reason != "" {
				slots[i].skipMsg = fmt.Sprintf("REJECTED (security guard: %s)", reason)
				continue
			}
			key := strings.ToLower(strings.TrimSpace(fact))
			if seenInBatch[key] {
				slots[i].skipMsg = fmt.Sprintf("SKIPPED (duplicate in batch for %s)", targetScope)
				continue
			}
			seenInBatch[key] = true
		}

		g, gctx := errgroup.WithContext(evalCtx)
		g.SetLimit(4)
		for i, fact := range clean {
			if slots[i].skipMsg != "" {
				continue
			}
			idx, cand := i, fact
			g.Go(func() error {
				evalStart := time.Now()
				d, err := ev.EvaluateMutation(gctx, targetScope, cand, existing)
				s.router.RecordTelemetry(gctx, domain.TaskMutation, ev, evalStart, len(existing), err)
				if err != nil {
					return err
				}
				slots[idx].dec = d
				return nil
			})
		}
		if err := g.Wait(); err != nil {
			return "", err
		}

		for i, fact := range clean {
			if slots[i].skipMsg != "" {
				lines = append(lines, slots[i].skipMsg)
				continue
			}
			res, err := s.applyEvaluatedCommit(ctx, targetScope, fact, category, "", "", slots[i].dec)
			if err != nil {
				return "", err
			}
			lines = append(lines, res)
		}
	}
	if sha != "" {
		if err := s.SetSyncedCommit(ctx, targetScope, sha); err != nil {
			return "", err
		}
		lines = append(lines, fmt.Sprintf("SYNCED (scope=%s, last_commit=%s)", targetScope, sha))
	}
	return strings.Join(lines, "\n"), nil
}

func cleanMarkdownRuleLine(raw string) string {
	line := strings.TrimSpace(raw)
	for _, p := range []string{"- ", "* ", "• "} {
		if strings.HasPrefix(line, p) {
			line = strings.TrimSpace(strings.TrimPrefix(line, p))
			break
		}
	}
	line = strings.ReplaceAll(line, "**", "")
	line = strings.ReplaceAll(line, "`", "")
	line = strings.TrimSpace(line)
	if len(line) < 8 || len(line) > 220 {
		return ""
	}
	return line
}

func extractBaselineCandidates(_ context.Context, _ domain.Evaluator, _ string, root string) []string {
	seen := make(map[string]bool)
	var out []string
	add := func(s string) {
		s = strings.TrimSpace(s)
		k := strings.ToLower(s)
		if s != "" && !seen[k] && len(out) < 10 {
			seen[k] = true
			out = append(out, s)
		}
	}

	if b, err := os.ReadFile(filepath.Join(root, "CONSTRAINTS.md")); err == nil {
		inCode := false
		for _, rawLine := range strings.Split(string(b), "\n") {
			trimmed := strings.TrimSpace(rawLine)
			if strings.HasPrefix(trimmed, "```") {
				inCode = !inCode
				continue
			}
			if inCode {
				continue
			}
			if strings.HasPrefix(trimmed, "- ") || strings.HasPrefix(trimmed, "* ") || strings.HasPrefix(trimmed, "• ") {
				if rule := cleanMarkdownRuleLine(trimmed); rule != "" {
					add(rule)
				}
			}
		}
	}
	return out
}

func extractCommitRuleCandidates(ctx context.Context, _ domain.Evaluator, _ string, root, sha, subject string) []string {
	seen := make(map[string]bool)
	var out []string
	add := func(s string) {
		s = strings.TrimSpace(s)
		k := strings.ToLower(s)
		if s != "" && !seen[k] {
			seen[k] = true
			out = append(out, s)
		}
	}

	filesOut, _ := git.Run(ctx, root, "show", "--name-only", "--format=", sha)
	hasConstraints := false
	for _, f := range strings.Split(filesOut, "\n") {
		if strings.TrimSpace(f) == "CONSTRAINTS.md" {
			hasConstraints = true
			break
		}
	}

	var diffSnippet string
	if hasConstraints {
		diffSnippet, _ = git.Run(ctx, root, "show", "--unified=0", "--format=", sha, "--", "CONSTRAINTS.md")
	}

	if cleaned := git.CleanCommitSubject(subject); cleaned != "" {
		add(cleaned)
	}
	for _, line := range strings.Split(diffSnippet, "\n") {
		if strings.HasPrefix(line, "+") && !strings.HasPrefix(line, "+++") {
			added := strings.TrimSpace(strings.TrimPrefix(line, "+"))
			if strings.HasPrefix(added, "- ") || strings.HasPrefix(added, "* ") || strings.HasPrefix(added, "• ") {
				if rule := cleanMarkdownRuleLine(added); rule != "" {
					add(rule)
				}
			}
		}
	}
	return out
}

// AutoSyncRepo autonomously onboards HEAD baseline rules or processes post-baseline commits
// and advances the synced_commit cursor without requiring agent or human tool calls.
func (s *Service) AutoSyncRepo(ctx context.Context, sessionID, rawScope, repoDir string, refresh bool) string {
	sc := domain.NormalizeScope(rawScope)
	if !strings.HasPrefix(sc, "project:") {
		return ""
	}
	root := git.ResolveRoot(repoDir)
	if root == "" {
		return ""
	}
	head := git.FastHead(ctx, root)
	if head == "" {
		return ""
	}

	last := s.GetSyncedCommit(ctx, sc)
	if last != "" {
		if last == head || git.RevParse(ctx, root, last) == head {
			return ""
		}
	}

	ev := s.router.EvalForTask(ctx, domain.TaskMutation)

	if last == "" {
		var memCount int
		_ = s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM memories WHERE scope = ?`, sc).Scan(&memCount)
		if memCount > 0 {
			_ = s.SetSyncedCommit(ctx, sc, head)
			return ""
		}
		candidates := extractBaselineCandidates(ctx, ev, sc, root)
		applied := 0
		for _, cand := range candidates {
			res, err := s.CommitMemory(ctx, cand, "", sc)
			if err == nil && (strings.HasPrefix(res, "COMMITTED") || strings.HasPrefix(res, "OVERWRITTEN")) {
				applied++
			}
		}
		_ = s.SetSyncedCommit(ctx, sc, head)
		return fmt.Sprintf("\n**[Auto Git Baseline Onboarding — HEAD `%s`]** Extracted %d baseline rule(s) from repository manifests and anchored cursor at `%s`.", head, applied, head)
	}

	if _, err := git.Run(ctx, root, "merge-base", "--is-ancestor", last, "HEAD"); err != nil {
		_ = s.SetSyncedCommit(ctx, sc, head)
		return fmt.Sprintf("\n**[Auto Git Sync — History Rewritten]** Re-anchored `synced_commit` from `%s` to HEAD `%s`.", last, head)
	}

	rawLog, err := git.Run(ctx, root, "log", "--reverse", "--format=%h\t%s", last+"..HEAD")
	if err != nil || rawLog == "" {
		_ = s.SetSyncedCommit(ctx, sc, head)
		return ""
	}

	lines := strings.Split(rawLog, "\n")
	type commitEntry struct {
		sha  string
		subj string
	}
	var commits []commitEntry
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, "\t", 2)
		subj := ""
		if len(parts) > 1 {
			subj = parts[1]
		}
		commits = append(commits, commitEntry{sha: parts[0], subj: subj})
	}
	commitCount := len(commits)
	if commitCount == 0 {
		_ = s.SetSyncedCommit(context.Background(), sc, head)
		return ""
	}

	applied := 0
	const maxDirectCommits = 5
	evalCommits := commits
	if len(commits) > maxDirectCommits {
		olderEnd := commits[len(commits)-maxDirectCommits-1].sha
		if diffOut, err := git.Run(ctx, root, "diff", "--unified=0", last+".."+olderEnd, "--", "CONSTRAINTS.md"); err == nil && diffOut != "" {
			seenDoc := make(map[string]bool)
			for _, dl := range strings.Split(diffOut, "\n") {
				if strings.HasPrefix(dl, "+") && !strings.HasPrefix(dl, "+++") {
					added := strings.TrimSpace(strings.TrimPrefix(dl, "+"))
					if strings.HasPrefix(added, "- ") || strings.HasPrefix(added, "* ") || strings.HasPrefix(added, "• ") {
						if rule := cleanMarkdownRuleLine(added); rule != "" && !seenDoc[strings.ToLower(rule)] {
							seenDoc[strings.ToLower(rule)] = true
							if ctx.Err() == nil {
								res, err := s.CommitMemory(ctx, rule, "", sc)
								if err == nil && (strings.HasPrefix(res, "COMMITTED") || strings.HasPrefix(res, "OVERWRITTEN")) {
									applied++
								}
							}
						}
					}
				}
			}
		}
		_ = s.SetSyncedCommit(context.Background(), sc, olderEnd)
		evalCommits = commits[len(commits)-maxDirectCommits:]
	}

	lastDone := ""
	for _, ce := range evalCommits {
		if ctx.Err() != nil {
			break
		}
		for _, cand := range extractCommitRuleCandidates(ctx, ev, sc, root, ce.sha, ce.subj) {
			if ctx.Err() != nil {
				break
			}
			res, err := s.CommitMemory(ctx, cand, "", sc)
			if err == nil && (strings.HasPrefix(res, "COMMITTED") || strings.HasPrefix(res, "OVERWRITTEN")) {
				applied++
			}
		}
		if ctx.Err() == nil {
			lastDone = ce.sha
			_ = s.SetSyncedCommit(context.Background(), sc, ce.sha)
		}
	}
	if ctx.Err() == nil {
		lastDone = head
		_ = s.SetSyncedCommit(context.Background(), sc, head)
	}
	if lastDone == "" {
		return ""
	}
	if applied > 0 {
		return fmt.Sprintf("\n**[Auto Incremental Git Sync — `%s..%s`]** Automatically applied %d rule update(s) across %d commit(s) and advanced cursor to `%s`.", last, lastDone, applied, commitCount, lastDone)
	}
	return fmt.Sprintf("\n**[Auto Incremental Git Sync — `%s..%s`]** Verified %d ordinary commit(s) (0 rule changes) and fast-forwarded cursor to `%s`.", last, lastDone, commitCount, lastDone)
}
