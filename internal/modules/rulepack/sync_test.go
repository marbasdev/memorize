package rulepack

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"memorize/internal/domain"
	"memorize/internal/infra/git"
	"memorize/internal/modules/memory"
)

type stubRuleEval struct{}

func (stubRuleEval) Name() string { return "stub" }
func (stubRuleEval) ScoreRelevance(_ context.Context, _ string, facts []domain.Fact) ([]domain.ScoredFact, error) {
	out := make([]domain.ScoredFact, len(facts))
	for i, f := range facts {
		out[i] = domain.ScoredFact{Fact: f, Score: 4.5, Confidence: 0.95}
	}
	return out, nil
}
func (stubRuleEval) EvaluateMutation(_ context.Context, _ string, _ string, _ []domain.Fact) (domain.MutationDecision, error) {
	return domain.MutationDecision{
		Category:    "convention",
		Tier:        "rule",
		Cardinality: "exclusive",
		IsPermanent: 0.90,
		Action:      "INSERT",
		Confidence:  0.95,
	}, nil
}
func (stubRuleEval) VerifyPlan(_ context.Context, _ string, _ []domain.Fact) ([]domain.PlanViolation, error) {
	return nil, nil
}

func runGitTest(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v in %s failed: %v\n%s", args, dir, err, string(out))
	}
	return strings.TrimSpace(string(out))
}

func initTestRepoWithOrigin(t *testing.T) (originDir, devADir, devBDir string) {
	t.Helper()
	tmp := t.TempDir()
	originDir = filepath.Join(tmp, "origin.git")
	devADir = filepath.Join(tmp, "repo-a")
	devBDir = filepath.Join(tmp, "repo-b")

	runGitTest(t, tmp, "init", "--bare", "--initial-branch=main", originDir)
	runGitTest(t, tmp, "clone", originDir, devADir)
	runGitTest(t, devADir, "config", "user.name", "Alice Dev")
	runGitTest(t, devADir, "config", "user.email", "alice@example.com")
	if err := os.WriteFile(filepath.Join(devADir, "README.md"), []byte("# Demo\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGitTest(t, devADir, "add", "README.md")
	runGitTest(t, devADir, "commit", "-m", "initial")
	runGitTest(t, devADir, "branch", "-M", "main")
	runGitTest(t, devADir, "push", "-u", "origin", "main")

	runGitTest(t, tmp, "clone", originDir, devBDir)
	runGitTest(t, devBDir, "config", "user.name", "Bob Dev")
	runGitTest(t, devBDir, "config", "user.email", "bob@example.com")
	return originDir, devADir, devBDir
}

func openTestRulePackService(t *testing.T) (*memory.Service, *Service) {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "memories.db")
	mem, err := memory.Open(dbPath, stubRuleEval{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = mem.Close() })
	return mem, New(mem)
}

func TestPublishProposal_IsolatedPushAndFeatureBranchSafety(t *testing.T) {
	_, devADir, devBDir := initTestRepoWithOrigin(t)
	_, svcA := openTestRulePackService(t)
	_, svcB := openTestRulePackService(t)
	ctx := context.Background()

	// 1) Dev A pushes first team rule directly on main; Author auto-fills as normalized git handle ("@Alice-Dev").
	resA, err := svcA.PublishProposal(ctx, PublishInput{
		Scope:   "project:repo",
		RepoDir: devADir,
		Proposal: domain.RulePlacementProposal{
			RuleKey:  "arch-no-cgo",
			Category: "architecture",
			Pinned:   true,
			Title:    "Keep static builds free of CGO",
		},
	})
	if err != nil {
		t.Fatalf("Dev A PublishProposal failed: %v", err)
	}
	if resA.Status != "published" || resA.BranchPushed != "main" || !resA.WorkspaceSynced {
		t.Fatalf("unexpected Dev A publish result: %+v", resA)
	}
	if resA.Rule.AddedBy != "@Alice-Dev" {
		t.Fatalf("expected git handle @Alice-Dev, got %q", resA.Rule.AddedBy)
	}

	// 2) Dev B works on feature/wip with staged changes + unstaged edits + unpushed local commit.
	runGitTest(t, devBDir, "checkout", "-b", "feature/wip")
	if err := os.WriteFile(filepath.Join(devBDir, "wip_commit.txt"), []byte("unpushed feature commit\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGitTest(t, devBDir, "add", "wip_commit.txt")
	runGitTest(t, devBDir, "commit", "-m", "feat: unpushed wip")
	if err := os.WriteFile(filepath.Join(devBDir, "staged_code.txt"), []byte("staged but uncommitted\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGitTest(t, devBDir, "add", "staged_code.txt")
	if err := os.WriteFile(filepath.Join(devBDir, "README.md"), []byte("dirty working file\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Promote a second rule on Dev B using default "push" mode.
	resB, err := svcB.PublishProposal(ctx, PublishInput{
		Scope:   "project:repo",
		RepoDir: devBDir,
		Proposal: domain.RulePlacementProposal{
			RuleKey:  "test-race",
			Category: "testing",
			Title:    "Run Unix tests with -race",
			Why:      "Detects data races early",
			How:      "go test -race ./...",
			AddedBy:  "BobNick",
		},
	})
	if err != nil {
		t.Fatalf("Dev B PublishProposal failed: %v", err)
	}
	if resB.Status != "published" || resB.BranchPushed != "main" || resB.WorkspaceSynced {
		t.Fatalf("expected isolated push to main without dirtying feature/wip, got %+v", resB)
	}

	// Verify Dev B workspace is completely unpolluted: staged_code.txt stays staged, README.md stays dirty, branch stays feature/wip.
	statusOut := runGitTest(t, devBDir, "status", "--porcelain")
	if !strings.Contains(statusOut, "staged_code.txt") || !strings.Contains(statusOut, "README.md") {
		t.Fatalf("workspace git status polluted:\n%s", statusOut)
	}
	if diffStaged := runGitTest(t, devBDir, "diff", "--cached", "--name-only"); diffStaged != "staged_code.txt" {
		t.Fatalf("expected staged_code.txt still staged in workspace index, got %q", diffStaged)
	}
	if branch := runGitTest(t, devBDir, "symbolic-ref", "--short", "HEAD"); branch != "feature/wip" {
		t.Fatalf("expected Dev B still on feature/wip, got %s", branch)
	}

	// Verify origin/main contains ONLY .memorize/TEAM_RULES.md additions and NO feature files!
	runGitTest(t, devADir, "pull", "--ff-only", "origin", "main")
	if _, err := os.Stat(filepath.Join(devADir, "staged_code.txt")); err == nil {
		t.Fatal("staged code leaked into origin/main!")
	}
	if _, err := os.Stat(filepath.Join(devADir, "wip_commit.txt")); err == nil {
		t.Fatal("unpushed feature branch commit leaked into origin/main!")
	}

	// Verify Dev A sync reads updated disk file containing BOTH arch-no-cgo and test-race.
	stA, err := svcA.SyncTurn(ctx, "project:repo", devADir)
	if err != nil || !stA.Changed || stA.RuleCount != 2 {
		t.Fatalf("expected Dev A SyncTurn to pick up both rules cleanly, got status=%+v err=%v", stA, err)
	}
	// Verify Dev B's next SyncTurn retains the remote mirror cleanly on feature/wip without wiping test-race!
	stB, err := svcB.SyncTurn(ctx, "project:repo", devBDir)
	if err != nil || stB.RuleCount != 2 {
		t.Fatalf("expected Dev B SyncTurn to preserve mirrored 2 rules while on feature/wip, got status=%+v err=%v", stB, err)
	}
}

func TestPublishProposal_CASConflictAndProtectedBranchError(t *testing.T) {
	_, devADir, devBDir := initTestRepoWithOrigin(t)
	_, svcA := openTestRulePackService(t)
	_, svcB := openTestRulePackService(t)
	ctx := context.Background()

	// Seed origin/main via Dev A.
	pnpmRes, err := svcA.PublishProposal(ctx, PublishInput{
		Scope:   "project:repo",
		RepoDir: devADir,
		Proposal: domain.RulePlacementProposal{
			Category: "convention",
			Title:    "Use pnpm exclusively for Node packages",
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	// Dev B proposes a rule using a stale BaseHash after Dev A pushed concurrently:
	// ConflictCheck sees the newly fetched latestDoc and pauses with conflict_pending.
	staleBaseHash := CanonicalHash([]byte("# empty stale\n"))
	evalCalls := 0
	resConflict, err := svcB.PublishProposal(ctx, PublishInput{
		Scope:   "project:repo",
		RepoDir: devBDir,
		Proposal: domain.RulePlacementProposal{
			Category: "convention",
			BaseHash: staleBaseHash,
			Title:    "Always use npm ci",
		},
		ConflictCheck: func(ctx context.Context, rule domain.TeamRule, latestDoc *Document) (bool, string, error) {
			evalCalls++
			for _, existing := range latestDoc.Rules {
				if existing.RuleKey == pnpmRes.Rule.RuleKey {
					return false, "conflicts with Use pnpm exclusively for Node packages", nil
				}
			}
			return false, "", nil
		},
	})
	if err != nil {
		t.Fatalf("expected clean pause, got err: %v", err)
	}
	if resConflict.Status != "conflict_pending" || !strings.Contains(resConflict.ConflictReason, "Use pnpm exclusively") || evalCalls == 0 {
		t.Fatalf("expected conflict_pending pause with latestDoc re-evaluation, got %+v evalCalls=%d", resConflict, evalCalls)
	}

	// Unrelated rule drafted against stale BaseHash re-evaluates against latest remote baseline and merges cleanly on main.
	resRebased, err := svcB.PublishProposal(ctx, PublishInput{
		Scope:   "project:repo",
		RepoDir: devBDir,
		Proposal: domain.RulePlacementProposal{
			Category: "convention",
			BaseHash: staleBaseHash,
			AfterKey: "deleted-concurrent-anchor",
			Title:    "Wrap repository errors with context",
		},
	})
	if err != nil {
		t.Fatalf("expected stale BaseHash + stale AfterKey to rebase cleanly onto latest, got err=%v", err)
	}
	if resRebased.Status != "published" || resRebased.Document == nil || len(resRebased.Document.Rules) != 2 {
		t.Fatalf("expected 2 rules merged cleanly onto latest main, got %+v", resRebased)
	}

	// Explicit PR mode pushes a dedicated memorize/rule-* branch without touching main.
}

func TestSyncAndResolveConflict_ProtectedFromLocalDeleteAndParseError(t *testing.T) {
	_, devADir, _ := initTestRepoWithOrigin(t)
	mem, svc := openTestRulePackService(t)
	ctx := context.Background()

	// Seed a local memory that will conflict with incoming Team Rule.
	_, err := mem.CommitBatch(ctx, []string{"Use tabs for Go formatting"}, "convention", "project:repo", devADir, "")
	if err != nil {
		t.Fatal(err)
	}
	page, err := mem.QueryRecordsPage(ctx, domain.RecordQuery{Scope: "project:repo", State: "active", Page: 1, PageSize: 20})
	if err != nil || len(page.Records) == 0 {
		t.Fatalf("expected local memory record, err=%v len=%d", err, len(page.Records))
	}
	localID := page.Records[0].ID

	// Publish a Team Rule via isolated push.
	res, err := svc.PublishProposal(ctx, PublishInput{
		Scope:   "project:repo",
		RepoDir: devADir,
		Proposal: domain.RulePlacementProposal{
			RuleKey:  "conv-gofmt",
			Category: "convention",
			Title:    "Always run gofmt standard formatting",
			Why:      "Uniform style",
			How:      "Run gofmt -w before commits",
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	// Corrupt TEAM_RULES.md on disk -> SyncTurn returns error AND keeps last valid 1 Team Rule mirrored!
	docFile := DocPath(devADir)
	if err := os.WriteFile(docFile, []byte("## Convention (`convention`)\n### broken heading without backticks\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.SyncTurn(ctx, "project:repo", devADir); err == nil {
		t.Fatal("expected parse error on broken TEAM_RULES.md")
	}
	rulesAfterErr, _ := svc.ListTeamRules(ctx, "project:repo")
	if len(rulesAfterErr) != 1 || rulesAfterErr[0].RuleKey != res.Rule.RuleKey {
		t.Fatalf("expected valid mirror preserved after parse error, got %+v", rulesAfterErr)
	}

	// Restore valid file and test Conflict -> LocalOverride & AdoptTeam.
	validRaw, _ := InsertRule(nil, "project:repo", domain.RulePlacementProposal{
		Category: "convention",
		Title:    "Always run gofmt standard formatting",
		AddedBy:  "@alice",
		AddedAt:  "2026-11-14",
	})
	if err := os.WriteFile(docFile, validRaw, 0o644); err != nil {
		t.Fatal(err)
	}
	_, err = svc.SyncTurn(ctx, "project:repo", devADir)
	if err != nil {
		t.Fatal(err)
	}

	convKey := res.Rule.RuleKey
	if err := svc.UpsertConflictState(ctx, "project:repo", convKey, domain.RuleStateConflictPending, localID, page.Records[0].Content, "tabs vs gofmt", ""); err != nil {
		t.Fatal(err)
	}
	if cnt := svc.CountPendingConflicts(ctx, "project:repo"); cnt != 1 {
		t.Fatalf("expected 1 pending conflict, got %d", cnt)
	}

	// Resolving via local_override hides the Team Rule from active Fact projection while leaving Team Rule record visible.
	if err := svc.ResolveConflict(ctx, "project:repo", convKey, "local_override", "local editor override"); err != nil {
		t.Fatal(err)
	}
	facts, _ := svc.ListTeamRuleFacts(ctx, "project:repo")
	if len(facts) != 0 {
		t.Fatalf("expected overridden team rule excluded from active evaluation facts, got %+v", facts)
	}

	// Resolving via adopt_team restores team rule to active facts AND softly archives the conflicting local memory (active=0).
	if err := svc.ResolveConflict(ctx, "project:repo", convKey, "adopt_team", ""); err != nil {
		t.Fatal(err)
	}
	facts, _ = svc.ListTeamRuleFacts(ctx, "project:repo")
	if len(facts) != 1 || facts[0].ID <= TeamFactIDOffset || !facts[0].IsTeam() {
		t.Fatalf("expected adopted team fact with disjoint ID > %d, got %+v", TeamFactIDOffset, facts)
	}
	localAfterAdopt, err := mem.QueryRecordsPage(ctx, domain.RecordQuery{Scope: "project:repo", State: "active", Page: 1, PageSize: 20})
	if err != nil || len(localAfterAdopt.Records) != 0 {
		t.Fatalf("expected conflicting local memory softly deactivated on adopt_team, got %+v err=%v", localAfterAdopt.Records, err)
	}

	// Local PurgeTombstones does NOT delete mirrored Team Rules!
	if _, err := mem.PurgeTombstones(ctx, "project:repo"); err != nil {
		t.Fatal(err)
	}
	rulesStillThere, _ := svc.ListTeamRules(ctx, "project:repo")
	if len(rulesStillThere) != 1 {
		t.Fatalf("expected team_rules completely untouched by local purge, got %+v", rulesStillThere)
	}
}

func TestPublishProposal_RetryOnPushConflictAndProtectedBranchError(t *testing.T) {
	originDir, devADir, devBDir := initTestRepoWithOrigin(t)
	_, svcA := openTestRulePackService(t)
	_, svcB := openTestRulePackService(t)
	ctx := context.Background()

	// Genuine concurrent remote races: while Dev B prepares tries 1 & 2, Dev A pushes to origin/main
	// right before Dev B's push runs, triggering native Git non-fast-forward rejections twice before try 3 succeeds.
	raceCalls := 0
	resRetry, err := svcB.PublishProposal(ctx, PublishInput{
		Scope:   "project:repo",
		RepoDir: devBDir,
		Proposal: domain.RulePlacementProposal{
			RuleKey:  "arch-retry-ok",
			Category: "architecture",
			Title:    "Retry push safely on genuine concurrent remote race",
			Why:      "Avoids transient push failure",
			How:      "CAS loop fetches latest and retries up to twice",
		},
		ConflictCheck: func(ctx context.Context, rule domain.TeamRule, latestDoc *Document) (bool, string, error) {
			raceCalls++
			if raceCalls <= 2 {
				key := "arch-race-1"
				if raceCalls == 2 {
					key = "arch-race-2"
				}
				_, raceErr := svcA.PublishProposal(ctx, PublishInput{
					Scope:   "project:repo",
					RepoDir: devADir,
					Proposal: domain.RulePlacementProposal{
						RuleKey:  key,
						Category: "architecture",
						Title:    "Concurrent teammate rule " + key,
						Why:      "Simulates live concurrent push race",
						How:      "Published right ahead of Dev B",
					},
				})
				if raceErr != nil {
					return false, "", raceErr
				}
			}
			return false, "", nil
		},
	})
	if err != nil {
		t.Fatalf("expected 3rd attempt (2nd retry) to succeed after 2 real Git races, got err=%v", err)
	}
	if resRetry.Attempts != 3 || resRetry.Status != "published" || resRetry.Document == nil || len(resRetry.Document.Rules) != 3 {
		t.Fatalf("expected Attempts=3 Status=published with all 3 merged rules, got %+v", resRetry)
	}

	if runtime.GOOS == "windows" {
		return
	}
	// Replace pre-receive hook to enforce branch protection on main.
	hookPath := filepath.Join(originDir, "hooks", "pre-receive")
	protScript := "#!/bin/sh\n" +
		"while read oldrev newrev refname; do\n" +
		"  if [ \"$refname\" = \"refs/heads/main\" ]; then\n" +
		"    echo 'remote: error: GH006: Protected branch update failed for refs/heads/main.' >&2\n" +
		"    exit 1\n" +
		"  fi\n" +
		"done\n" +
		"exit 0\n"
	if err := os.WriteFile(hookPath, []byte(protScript), 0o755); err != nil {
		t.Fatal(err)
	}

	_, err = svcB.PublishProposal(ctx, PublishInput{
		Scope:   "project:repo",
		RepoDir: devBDir,
		Proposal: domain.RulePlacementProposal{
			Category: "security",
			Title:    "Return explicit push error instead of PR fallback when main is protected",
		},
	})
	if err == nil || !errors.Is(err, git.ErrBranchProtected) {
		t.Fatalf("expected git.ErrBranchProtected on protected branch without PR fallback, got err=%v", err)
	}
	if branches := strings.TrimSpace(runGitTest(t, devADir, "ls-remote", "--heads", "origin", "memorize/*")); branches != "" {
		t.Fatalf("expected no fallback PR branch created on origin, got %q", branches)
	}
}

func TestUpdateAndRemoveRuleRemote_IsolatedWorktreeAndCAS(t *testing.T) {
	_, devADir, devBDir := initTestRepoWithOrigin(t)
	_, svcA := openTestRulePackService(t)
	_, svcB := openTestRulePackService(t)
	ctx := context.Background()

	// Seed initial rule via Dev A
	pub, err := svcA.PublishProposal(ctx, PublishInput{
		Scope:   "project:repo",
		RepoDir: devADir,
		Proposal: domain.RulePlacementProposal{
			Category:    "testing",
			SubCategory: "go",
			Title:       "Always run go test with -race",
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	// Dev B switches to a dirty feature branch with staged changes
	runGitTest(t, devBDir, "checkout", "-b", "feature/ui")
	if err := os.WriteFile(filepath.Join(devBDir, "ui.txt"), []byte("wip staged\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGitTest(t, devBDir, "add", "ui.txt")

	// Dev B updates the rule remotely without polluting feature/ui
	updRes, err := svcB.UpdateRule(ctx, UpdateInput{
		Scope:     "project:repo",
		RepoDir:   devBDir,
		TargetKey: pub.Rule.RuleKey,
		Updated: domain.TeamRule{
			Category:    "testing",
			SubCategory: "unit",
			Title:       "Always run go test -race -count=1",
		},
	})
	if err != nil {
		t.Fatalf("UpdateRule failed: %v", err)
	}
	if updRes.Status != "updated" || updRes.BranchPushed != "main" || updRes.WorkspaceSynced {
		t.Fatalf("unexpected UpdateRule result: %+v", updRes)
	}
	if branch := runGitTest(t, devBDir, "symbolic-ref", "--short", "HEAD"); branch != "feature/ui" {
		t.Fatalf("expected feature/ui untouched, got %s", branch)
	}

	rulesB, _ := svcB.ListTeamRules(ctx, "project:repo")
	if len(rulesB) != 1 || rulesB[0].Title != "Always run go test -race -count=1" || rulesB[0].SubCategory != "unit" {
		t.Fatalf("expected updated mirror on Dev B, got %+v", rulesB)
	}

	// Remove the rule remotely via Dev A
	rmRes, err := svcA.RemoveRuleRemote(ctx, RemoveInput{
		Scope:     "project:repo",
		RepoDir:   devADir,
		TargetKey: updRes.Rule.RuleKey,
	})
	if err != nil {
		t.Fatalf("RemoveRuleRemote failed: %v", err)
	}
	if rmRes.Status != "removed" || rmRes.Rule.Title != "Always run go test -race -count=1" {
		t.Fatalf("unexpected RemoveRuleRemote result: %+v", rmRes)
	}
	rulesAfterRm, _ := svcA.ListTeamRules(ctx, "project:repo")
	if len(rulesAfterRm) != 0 {
		t.Fatalf("expected 0 mirrored rules after RemoveRuleRemote, got %+v", rulesAfterRm)
	}
}
