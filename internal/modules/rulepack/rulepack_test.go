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

	runGitTest(t, devADir, "pull", "--ff-only", "origin", "main")
	if _, err := os.Stat(filepath.Join(devADir, "staged_code.txt")); err == nil {
		t.Fatal("staged code leaked into origin/main!")
	}
	if _, err := os.Stat(filepath.Join(devADir, "wip_commit.txt")); err == nil {
		t.Fatal("unpushed feature branch commit leaked into origin/main!")
	}

	stA, err := svcA.SyncTurn(ctx, "project:repo", devADir)
	if err != nil || !stA.Changed || stA.RuleCount != 2 {
		t.Fatalf("expected Dev A SyncTurn to pick up both rules cleanly, got status=%+v err=%v", stA, err)
	}
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

}

func TestSyncAndResolveConflict_ProtectedFromLocalDeleteAndParseError(t *testing.T) {
	_, devADir, _ := initTestRepoWithOrigin(t)
	mem, svc := openTestRulePackService(t)
	ctx := context.Background()

	_, err := mem.CommitBatch(ctx, []string{"Use tabs for Go formatting"}, "convention", "project:repo", devADir, "")
	if err != nil {
		t.Fatal(err)
	}
	page, err := mem.QueryRecordsPage(ctx, domain.RecordQuery{Scope: "project:repo", State: "active", Page: 1, PageSize: 20})
	if err != nil || len(page.Records) == 0 {
		t.Fatalf("expected local memory record, err=%v len=%d", err, len(page.Records))
	}
	localID := page.Records[0].ID

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

	if err := svc.ResolveConflict(ctx, "project:repo", convKey, "local_override", "local editor override"); err != nil {
		t.Fatal(err)
	}
	facts, _ := svc.ListTeamRuleFacts(ctx, "project:repo")
	if len(facts) != 0 {
		t.Fatalf("expected overridden team rule excluded from active evaluation facts, got %+v", facts)
	}

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

	runGitTest(t, devBDir, "checkout", "-b", "feature/ui")
	if err := os.WriteFile(filepath.Join(devBDir, "ui.txt"), []byte("wip staged\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGitTest(t, devBDir, "add", "ui.txt")

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

type aiRuleEval struct {
	stubRuleEval
	mutateFn     func(scope, cand string, existing []domain.Fact) domain.MutationDecision
	draftFn      func(scope, raw, cat, sub string, outline []domain.RuleOutlineEntry) domain.RulePlacementProposal
	redundancyFn func(scope string, teamRules []domain.TeamRule, localFacts []domain.Fact) []domain.RuleDuplicateSuggestion
}

func (a *aiRuleEval) EvaluateMutation(_ context.Context, scope, cand string, existing []domain.Fact) (domain.MutationDecision, error) {
	if a.mutateFn != nil {
		return a.mutateFn(scope, cand, existing), nil
	}
	return a.stubRuleEval.EvaluateMutation(context.Background(), scope, cand, existing)
}

func (a *aiRuleEval) DraftRulePackProposal(_ context.Context, scope, rawRuleText, sourceCat, sourceSub string, outline []domain.RuleOutlineEntry) (domain.RulePlacementProposal, error) {
	if a.draftFn != nil {
		return a.draftFn(scope, rawRuleText, sourceCat, sourceSub, outline), nil
	}
	cat := domain.NormalizeCategoryForScope(scope, sourceCat)
	return domain.RulePlacementProposal{
		RuleKey:  domain.ComputeRuleKey(cat, rawRuleText),
		Category: cat,
		Pinned:   true,
		Title:    rawRuleText,
	}, nil
}

func (a *aiRuleEval) FindRedundantLocalMemories(_ context.Context, scope string, teamRules []domain.TeamRule, localFacts []domain.Fact) ([]domain.RuleDuplicateSuggestion, error) {
	if a.redundancyFn != nil {
		return a.redundancyFn(scope, teamRules, localFacts), nil
	}
	return nil, nil
}

func TestRulePackAIWorkflowConflictsDedupeAndSessionInvalidation(t *testing.T) {
	_, repoDir, _ := initTestRepoWithOrigin(t)
	runGitTest(t, repoDir, "config", "user.name", "Alice Reviewer")

	ev := &aiRuleEval{}
	dbPath := filepath.Join(t.TempDir(), "mem.db")
	memSvc, err := memory.Open(dbPath, ev)
	if err != nil {
		t.Fatalf("open memory: %v", err)
	}
	defer memSvc.Close()

	rpSvc := New(memSvc)
	memSvc.AttachExtraFactsProvider(rpSvc)

	ctx := context.Background()
	memSvc.RememberRepoPath(ctx, "project:teamapp", repoDir)

	ev.mutateFn = func(_, _ string, _ []domain.Fact) domain.MutationDecision {
		return domain.MutationDecision{Category: "workflow", Tier: "rule", Cardinality: "additive", IsPermanent: 0.9, Action: "INSERT", Confidence: 0.95}
	}
	if _, err := memSvc.CommitMemoryFull(ctx, "Always run race detector before pushing PRs", "testing", "go", "", "project:teamapp"); err != nil {
		t.Fatal(err)
	}
	if _, err := memSvc.CommitMemoryFull(ctx, "Use modernc sqlite without CGO; local debug port 19090", "database", "sqlite", "", "project:teamapp"); err != nil {
		t.Fatal(err)
	}

	localFacts, err := memSvc.LoadActiveFacts(ctx, "project:teamapp", "project:teamapp", "")
	if err != nil || len(localFacts) != 2 {
		t.Fatalf("expected 2 local facts, got %d err=%v", len(localFacts), err)
	}
	testFact := localFacts[0]
	dbFact := localFacts[1]

	pinTrue := true
	if err := memSvc.UpdateMemoryFull(ctx, dbFact.ID, "", "", nil, dbFact.Content, &pinTrue); err != nil {
		t.Fatal(err)
	}

	ev.draftFn = func(scope, raw, cat, sub string, _ []domain.RuleOutlineEntry) domain.RulePlacementProposal {
		return domain.RulePlacementProposal{
			Category:    cat,
			SubCategory: sub,
			Title:       raw,
		}
	}
	prop, src, err := rpSvc.PromoteLocalMemory(ctx, testFact.ID, "project:teamapp", repoDir)
	if err != nil {
		t.Fatalf("PromoteLocalMemory failed: %v", err)
	}
	if src.ID != testFact.ID || prop.AddedBy != "@Alice-Reviewer" {
		t.Fatalf("unexpected promote metadata: src=%+v prop=%+v", src, prop)
	}

	pubRes, err := rpSvc.PublishWithDefaults(ctx, PublishInput{
		Scope:         "project:teamapp",
		RepoDir:       repoDir,
		Proposal:      prop,
		LocalMemoryID: testFact.ID,
	}, false)
	if err != nil {
		t.Fatalf("PublishWithDefaults failed: %v", err)
	}
	if pubRes.Status != "published" {
		t.Fatalf("expected published status, got %+v", pubRes)
	}
	raceKey := pubRes.Rule.RuleKey

	ev.draftFn = func(scope, raw, _, _ string, outline []domain.RuleOutlineEntry) domain.RulePlacementProposal {
		if len(outline) != 1 || outline[0].RuleKey != raceKey {
			t.Fatalf("expected existing outline in DraftRulePackProposal, got %+v", outline)
		}
		return domain.RulePlacementProposal{
			Category: "database",
			Title:    "Use modernc.org/sqlite without CGO.",
		}
	}
	propDraft, err := rpSvc.DraftProposalFromText(ctx, "project:teamapp", repoDir, "Use modernc.org/sqlite without CGO.", "database", "sqlite")
	if err != nil {
		t.Fatalf("DraftProposalFromText failed: %v", err)
	}
	wantDraftKey := domain.ComputeRuleKey("database", "Use modernc.org/sqlite without CGO.")
	if propDraft.RuleKey != wantDraftKey {
		t.Fatalf("expected computed rule key %q, got %q", wantDraftKey, propDraft.RuleKey)
	}

	q1, err := memSvc.QuerySessionMemory(ctx, "sess-ai-1", "test go packages", "", "project:teamapp", false)
	if err != nil || !strings.Contains(q1, "[TEAM project:teamapp/testing/go]") || strings.Contains(q1, raceKey) {
		t.Fatalf("expected initial Team Rule query delivery without exposed hash key, got err=%v out:\n%s", err, q1)
	}

	rawDisk, err := os.ReadFile(DocPath(repoDir))
	if err != nil {
		t.Fatal(err)
	}
	edited := strings.Replace(string(rawDisk), "Always run race detector before pushing PRs", "Run go test -race before every PR merge", 1)
	updatedRaceKey := domain.ComputeRuleKey("testing", "Run go test -race before every PR merge")
	updatedDocBytes, err := InsertRule([]byte(edited), "project:teamapp", domain.RulePlacementProposal{
		Category: "database",
		Title:    "Use modernc.org/sqlite without CGO",
		AddedBy:  "Bob Teammate",
		AddedAt:  "2026-11-14",
	})
	if err != nil {
		t.Fatal(err)
	}
	dbKey := domain.ComputeRuleKey("database", "Use modernc.org/sqlite without CGO")
	if err := os.WriteFile(DocPath(repoDir), updatedDocBytes, 0644); err != nil {
		t.Fatal(err)
	}
	runGitTest(t, repoDir, "add", domain.RulePackRelPath)
	runGitTest(t, repoDir, "commit", "-m", "teammate adds modernc rule")
	runGitTest(t, repoDir, "push", "origin", "main")

	ev.mutateFn = func(_, cand string, existing []domain.Fact) domain.MutationDecision {
		if strings.Contains(cand, "modernc.org/sqlite") && len(existing) > 0 {
			for _, f := range existing {
				if f.ID == dbFact.ID || f.RuleKey == dbKey {
					return domain.MutationDecision{
						Action:          "OVERWRITE",
						ConflictFactID:  f.ID,
						ConflictFactIDs: []int64{f.ID},
						Cardinality:     "exclusive",
						Confidence:      0.92,
					}
				}
			}
		}
		return domain.MutationDecision{Action: "INSERT", Confidence: 0.9}
	}

	syncSt, conflictBanner, err := rpSvc.SyncTurnAndScanConflicts(ctx, "project:teamapp", repoDir)
	if err != nil {
		t.Fatalf("SyncTurnAndScanConflicts failed: %v", err)
	}
	if !syncSt.Changed || syncSt.PendingConflicts != 1 || !strings.Contains(conflictBanner, "Use modernc.org/sqlite without CGO") || strings.Contains(conflictBanner, dbKey) {
		t.Fatalf("expected 1 detected conflict banner without internal hash, got status=%+v banner=%q", syncSt, conflictBanner)
	}

	q2, err := memSvc.QuerySessionMemory(ctx, "sess-ai-1", "test go packages", "", "project:teamapp", false)
	if err != nil || !strings.Contains(q2, "Run go test -race before every PR merge") {
		t.Fatalf("expected updated Team Rule to re-deliver after mirror diff invalidation, got err=%v out:\n%s", err, q2)
	}

	pubConflict, err := rpSvc.PublishWithDefaults(ctx, PublishInput{
		Scope:   "project:teamapp",
		RepoDir: repoDir,
		Proposal: domain.RulePlacementProposal{
			Category: "database",
			Title:    "Use modernc.org/sqlite with CGO enabled",
		},
	}, false)
	if err != nil {
		t.Fatalf("PublishWithDefaults conflict check failed: %v", err)
	}
	if pubConflict.Status != "conflict_pending" || !strings.Contains(pubConflict.ConflictReason, "Use modernc.org/sqlite without CGO") {
		t.Fatalf("expected PublishWithDefaults to stop with conflict_pending citing rule title, got %+v", pubConflict)
	}

	if err := rpSvc.ResolveConflict(ctx, "project:teamapp", dbKey, "mark_updated", ""); err != nil {
		t.Fatal(err)
	}

	ev.redundancyFn = func(scope string, teamRules []domain.TeamRule, localFacts []domain.Fact) []domain.RuleDuplicateSuggestion {
		return []domain.RuleDuplicateSuggestion{
			{
				LocalMemoryID: testFact.ID,
				LocalCategory: testFact.Category,
				LocalContent:  testFact.Content,
				TeamRuleKey:   updatedRaceKey,
				TeamRuleTitle: "Run go test -race before every PR merge",
				Kind:          "duplicate",
				Reason:        "Covered by team testing rule",
			},
			{
				LocalMemoryID:  dbFact.ID,
				LocalCategory:  dbFact.Category,
				LocalContent:   dbFact.Content,
				TeamRuleKey:    dbKey,
				TeamRuleTitle:  "Use modernc.org/sqlite without CGO",
				Kind:           "partial_overlap",
				TrimmedContent: "Local debug server listens on port 19090",
				Reason:         "Retain unique local debug port",
			},
		}
	}

	sugs, srcEngine, err := rpSvc.DetectRedundantLocalMemories(ctx, "project:teamapp")
	if err != nil || len(sugs) != 2 || srcEngine != "stub" {
		t.Fatalf("expected 2 redundancy suggestions from stub, got len=%d src=%q err=%v", len(sugs), srcEngine, err)
	}

	cleanRes, err := rpSvc.ApplyDuplicateCleanup(ctx, "project:teamapp", []DuplicateCleanupAction{
		{LocalMemoryID: testFact.ID, Action: "delete"},
		{LocalMemoryID: dbFact.ID, Action: "trim", TrimmedContent: sugs[1].TrimmedContent},
	})
	if err != nil {
		t.Fatalf("ApplyDuplicateCleanup failed: %v", err)
	}
	if len(cleanRes.DeletedIDs) != 1 || cleanRes.DeletedIDs[0] != testFact.ID || len(cleanRes.TrimmedIDs) != 1 || cleanRes.TrimmedIDs[0] != dbFact.ID {
		t.Fatalf("unexpected cleanup result: %+v", cleanRes)
	}

	remaining, err := memSvc.LoadActiveFacts(ctx, "project:teamapp", "project:teamapp", "")
	if err != nil || len(remaining) != 1 {
		t.Fatalf("expected 1 remaining local fact, got %+v err=%v", remaining, err)
	}
	if remaining[0].Content != "Local debug server listens on port 19090" || !remaining[0].Pinned || remaining[0].SubCategory != "sqlite" {
		t.Fatalf("expected trimmed fact to preserve pinned=true & sub_category=sqlite, got %+v", remaining[0])
	}
}
