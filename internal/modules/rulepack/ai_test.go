package rulepack

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"memorize/internal/domain"
	"memorize/internal/infra/sqlite"
	"memorize/internal/modules/memory"
)

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
	sqlite.RememberRepoPath(ctx, memSvc.DB(), "project:teamapp", repoDir)

	// Seed 2 local memories: one general workflow rule to promote, one database rule with partial overlap.
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

	// Set dbFact pinned=true to verify partial_overlap trim preserves category/sub_category/pinned
	pinTrue := true
	if err := memSvc.UpdateMemoryFull(ctx, dbFact.ID, "", "", nil, dbFact.Content, &pinTrue); err != nil {
		t.Fatal(err)
	}

	// 1. Draft & Promote local memory via AI RulePackPlanner and publish cleanly
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

	// Verify outline passes indexed rules and DraftProposalFromText derives deterministic content hash key
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

	// Verify session delivery + Mirror change invalidates session_sent
	q1, err := memSvc.QuerySessionMemory(ctx, "sess-ai-1", "test go packages", "", "project:teamapp", false)
	if err != nil || !strings.Contains(q1, "[TEAM project:teamapp/testing/go]") || strings.Contains(q1, raceKey) {
		t.Fatalf("expected initial Team Rule query delivery without exposed hash key, got err=%v out:\n%s", err, q1)
	}

	// Simulate teammate updating title of race rule and syncing + adding a conflicting rule
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

	// Make mutation evaluator flag modernc rule against dbFact (sync scan) and against new conflicting team rules (CAS check)
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

	// Next turn MUST re-deliver updated testing rule because applyMirrorTx called InvalidateSessionIDs
	q2, err := memSvc.QuerySessionMemory(ctx, "sess-ai-1", "test go packages", "", "project:teamapp", false)
	if err != nil || !strings.Contains(q2, "Run go test -race before every PR merge") {
		t.Fatalf("expected updated Team Rule to re-deliver after mirror diff invalidation, got err=%v out:\n%s", err, q2)
	}

	// Verify DefaultConflictEvaluator pauses PublishWithDefaults on semantic conflict without pushing
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

	// Resolve pending conflict with mark_updated so modernc rule stays active for duplicate trim test
	if err := rpSvc.ResolveConflict(ctx, "project:teamapp", dbKey, "mark_updated", ""); err != nil {
		t.Fatal(err)
	}

	// 2. Redundancy scan & ApplyDuplicateCleanup (delete testFact, trim dbFact keeping pinned/sub_category)
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
