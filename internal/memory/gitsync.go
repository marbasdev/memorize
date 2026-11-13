package memory

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/sync/errgroup"
)

var baselineManifests = []string{
	// Code/build manifests & CONSTRAINTS.md only; static AGENTS.md/CLAUDE.md excluded.
	"CONSTRAINTS.md", "README.md",
	"go.mod", "package.json", "pyproject.toml", "Cargo.toml",
	"pom.xml", "build.gradle", "Makefile", "Dockerfile",
	".golangci.yml", "tsconfig.json", ".github/workflows",
}

func runGit(parent context.Context, dir string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(parent, 2*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...)
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

func ResolveGitRoot(dir string) string {
	dir = strings.TrimSpace(dir)
	if dir == "" {
		return ""
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return ""
	}
	home, _ := os.UserHomeDir()
	home = filepath.Clean(home)

	for cur := filepath.Clean(abs); cur != "/" && cur != "."; {
		if cur == home {
			return ""
		}
		if _, err := os.Stat(filepath.Join(cur, ".git")); err == nil {
			return cur
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			break
		}
		cur = parent
	}
	return ""
}

func fastGitHead(ctx context.Context, root string) string {
	raw, err := os.ReadFile(filepath.Join(root, ".git", "HEAD"))
	if err == nil {
		s := strings.TrimSpace(string(raw))
		if strings.HasPrefix(s, "ref:") {
			ref := strings.TrimSpace(strings.TrimPrefix(s, "ref:"))
			if refBytes, err := os.ReadFile(filepath.Join(root, ".git", filepath.FromSlash(ref))); err == nil {
				if h := strings.TrimSpace(string(refBytes)); len(h) >= 12 {
					return h[:12]
				}
			}
		} else if len(s) >= 12 {
			return s[:12]
		}
	}
	return gitRevParse(ctx, root, "HEAD")
}

func gitRevParse(ctx context.Context, root, rev string) string {
	sha, err := runGit(ctx, root, "rev-parse", "--verify", "--short=12", rev+"^{commit}")
	if err != nil {
		return ""
	}
	return sha
}

func detectHeadLayout(root string) string {
	var found []string
	for _, rel := range baselineManifests {
		if _, err := os.Stat(filepath.Join(root, rel)); err == nil {
			found = append(found, rel)
		}
	}
	entries, _ := os.ReadDir(root)
	var dirs []string
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() && !strings.HasPrefix(name, ".") && name != "node_modules" && name != "vendor" && name != "bin" {
			dirs = append(dirs, name+"/")
		}
	}
	var parts []string
	if len(found) > 0 {
		parts = append(parts, "Manifests/Docs: "+strings.Join(found, ", "))
	}
	if len(dirs) > 0 {
		parts = append(parts, "Top Directories: "+strings.Join(dirs, ", "))
	}
	if len(parts) == 0 {
		return "(standard manifests not detected; inspect root files at HEAD)"
	}
	return strings.Join(parts, "\n")
}

func (s *Service) GetSyncedCommit(ctx context.Context, rawScope string) string {
	sc := NormalizeScope(rawScope)
	var sha string
	_ = s.db.QueryRowContext(ctx, `SELECT last_commit FROM repo_sync WHERE scope = ?`, sc).Scan(&sha)
	return strings.TrimSpace(sha)
}

func (s *Service) SetSyncedCommit(ctx context.Context, rawScope, sha string) error {
	sc := NormalizeScope(rawScope)
	sha = strings.TrimSpace(sha)
	if sha == "" {
		return nil
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO repo_sync (scope, last_commit, updated_at)
		VALUES (?, ?, CURRENT_TIMESTAMP)
		ON CONFLICT(scope) DO UPDATE SET last_commit = excluded.last_commit, updated_at = CURRENT_TIMESTAMP
	`, sc, sha)
	if err == nil {
		s.mu.Lock()
		for k := range s.syncSeen {
			if strings.HasSuffix(k, "|"+sc) {
				delete(s.syncSeen, k)
			}
		}
		s.mu.Unlock()
	}
	return err
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

func (s *Service) InspectRepoSync(ctx context.Context, sessionID, rawScope, repoDir string, refresh bool) string {
	s.mu.Lock()
	autoGit := s.autoGitSync
	s.mu.Unlock()
	if autoGit {
		return s.AutoSyncRepo(ctx, sessionID, rawScope, repoDir, refresh)
	}

	sc := NormalizeScope(rawScope)
	if !strings.HasPrefix(sc, "project:") {
		return ""
	}
	root := ResolveGitRoot(repoDir)
	if root == "" {
		return ""
	}
	head := fastGitHead(ctx, root)
	if head == "" {
		return ""
	}

	last := s.GetSyncedCommit(ctx, sc)
	if last != "" {
		if last == head || gitRevParse(ctx, root, last) == head {
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
			head, root, detectHeadLayout(root), sc, head, head,
		)
	}

	if !s.shouldNotifySync(sessionID, sc, head, refresh) {
		return ""
	}
	if _, err := runGit(ctx, root, "merge-base", "--is-ancestor", last, "HEAD"); err != nil {
		return fmt.Sprintf(
			"\n**[Git History Rewritten — `%s` -> HEAD `%s`]**\n"+
				"Previous cursor (`%s`) was rewritten by rebase or force-push. Inspect current `HEAD` and reset the cursor via `commit_memory(scope=%q, synced_commit=%q)`.",
			last, head, last, sc, head,
		)
	}

	logOut, err := runGit(ctx, root, "log", "--reverse", "--format=- %h (%cs) %s", "--name-status", last+"..HEAD")
	if err != nil || logOut == "" {
		return ""
	}
	return fmt.Sprintf(
		"\n**[Incremental Git Sync — `%s..%s`]**\n%s\n"+
			"Review the commits above in chronological order. If architecture, dependencies, or conventions changed, call `commit_memory(scope=%q, candidate_facts=[...], synced_commit=%q)`; otherwise advance the cursor via `commit_memory(scope=%q, synced_commit=%q)`.",
		last, head, logOut, sc, head, sc, head,
	)
}

func (s *Service) CommitBatch(ctx context.Context, facts []string, category, rawScope, repoDir, syncedCommit string) (string, error) {
	targetScope := NormalizeScope(rawScope)
	var clean []string
	for _, f := range facts {
		if t := strings.TrimSpace(f); t != "" {
			clean = append(clean, t)
		}
	}
	sha := strings.TrimSpace(syncedCommit)
	if root := ResolveGitRoot(repoDir); root != "" {
		s.RememberRepoPath(ctx, targetScope, root)
		if sha != "" {
			if norm := gitRevParse(ctx, root, sha); norm != "" {
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
		existing, err := s.loadActiveFacts(ctx, targetScope, targetScope, "")
		if err != nil {
			return "", err
		}
		evalCtx := WithPresetCategories(ctx, s.EffectiveTaxonomyJSON(ctx))
		ev := s.evalForTask(ctx, TaskMutation)
		if ev == nil {
			return "", fmt.Errorf("no active evaluation engine configured")
		}

		type batchSlot struct {
			skipMsg string
			dec     MutationDecision
		}
		slots := make([]batchSlot, len(clean))
		seenInBatch := make(map[string]bool, len(clean))
		for i, fact := range clean {
			if reason := checkSecurityGuard(fact); reason != "" {
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
				s.recordEvalTelemetry(gctx, TaskMutation, ev, evalStart, len(existing), err)
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

func extractBaselineCandidates(_ context.Context, _ Evaluator, _ string, root string) []string {
	// Deterministic structural markdown bullet & manifest extraction (zero keyword hardcoding;
	// System 1 EvaluateMutation judges permanence/tier for each candidate without a duplicate ExtractRules LLM pass).
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

func cleanCommitSubject(subject string) string {
	s := strings.TrimSpace(subject)
	if idx := strings.Index(s, ":"); idx > 0 && idx < 28 {
		if body := strings.TrimSpace(s[idx+1:]); len(body) >= 4 {
			return body
		}
	}
	return s
}

func extractCommitRuleCandidates(ctx context.Context, _ Evaluator, _ string, root, sha, subject string) []string {
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

	filesOut, _ := runGit(ctx, root, "show", "--name-only", "--format=", sha)
	hasConstraints := false
	for _, f := range strings.Split(filesOut, "\n") {
		if strings.TrimSpace(f) == "CONSTRAINTS.md" {
			hasConstraints = true
			break
		}
	}

	var diffSnippet string
	if hasConstraints {
		diffSnippet, _ = runGit(ctx, root, "show", "--unified=0", "--format=", sha, "--", "CONSTRAINTS.md")
	}

	// Deterministic structural extraction without redundant ExtractRules LLM calls:
	// pass commit subject and added rule-doc bullets to CommitMemory so System 1 EvaluateMutation decides via Tier & IsPermanent.
	if cleaned := cleanCommitSubject(subject); cleaned != "" {
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
	sc := NormalizeScope(rawScope)
	if !strings.HasPrefix(sc, "project:") {
		return ""
	}
	root := ResolveGitRoot(repoDir)
	if root == "" {
		return ""
	}
	head := fastGitHead(ctx, root)
	if head == "" {
		return ""
	}

	last := s.GetSyncedCommit(ctx, sc)
	if last != "" {
		if last == head || gitRevParse(ctx, root, last) == head {
			return ""
		}
	}

	ev := s.evalForTask(ctx, TaskMutation)

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

	if _, err := runGit(ctx, root, "merge-base", "--is-ancestor", last, "HEAD"); err != nil {
		_ = s.SetSyncedCommit(ctx, sc, head)
		return fmt.Sprintf("\n**[Auto Git Sync — History Rewritten]** Re-anchored `synced_commit` from `%s` to HEAD `%s`.", last, head)
	}

	rawLog, err := runGit(ctx, root, "log", "--reverse", "--format=%h\t%s", last+"..HEAD")
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
	// When a large batch of commits arrives at once (e.g. git pull), inspect rule-doc diffs across
	// the entire range in one git call and bound per-commit LLM evaluation to the latest commits so
	// synchronous 8s lifecycle hooks never stall or enter timeout retry loops.
	if len(commits) > maxDirectCommits {
		olderEnd := commits[len(commits)-maxDirectCommits-1].sha
		if diffOut, err := runGit(ctx, root, "diff", "--unified=0", last+".."+olderEnd, "--", "CONSTRAINTS.md"); err == nil && diffOut != "" {
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
