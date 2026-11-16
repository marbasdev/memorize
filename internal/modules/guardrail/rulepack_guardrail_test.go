package guardrail_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"memorize/internal/domain"
	"memorize/internal/modules/rulepack"
)

type teamVerifyEval struct {
	stubEval
	verifyFn func(plan string, rules []domain.Fact) []domain.PlanViolation
}

func (e *teamVerifyEval) VerifyPlan(_ context.Context, plan string, rules []domain.Fact) ([]domain.PlanViolation, error) {
	if e.verifyFn != nil {
		return e.verifyFn(plan, rules), nil
	}
	return nil, nil
}

func TestGuardrailSyncsAndEnforcesTeamRulePack(t *testing.T) {
	tmp := t.TempDir()
	repoDir := filepath.Join(tmp, "teamrepo")
	if err := os.MkdirAll(filepath.Join(repoDir, ".git"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(repoDir, ".memorize"), 0755); err != nil {
		t.Fatal(err)
	}

	ev := &teamVerifyEval{}
	stack := newTestStack(t, filepath.Join(tmp, "mem.db"), ev)
	rp := rulepack.New(stack.mem)
	stack.mem.AttachExtraFactsProvider(rp)
	stack.gr.AttachRulePack(rp)

	ctx := context.Background()
	scope := "project:teamrepo"

	// Commit a local conflicting memory first
	ev.dec = domain.MutationDecision{Action: "INSERT", Category: "database", Cardinality: "exclusive", IsPermanent: 0.9, Confidence: 0.95}
	if _, err := stack.mem.CommitMemoryFull(ctx, "Use go-sqlite3 CGO driver in dev builds", "database", "sqlite", "", scope); err != nil {
		t.Fatal(err)
	}
	locals, err := stack.mem.LoadActiveFacts(ctx, scope, scope, "")
	if err != nil || len(locals) != 1 {
		t.Fatalf("expected 1 local fact, got %+v err=%v", locals, err)
	}
	localID := locals[0].ID

	// Write a declarative TEAM_RULES.md containing a conflicting team rule
	docBytes, err := rulepack.InsertRule(nil, scope, domain.RulePlacementProposal{
		Category: "database",
		Title:    "Always use pure-Go modernc.org/sqlite with CGO_ENABLED=0",
		AddedBy:  "Alice",
		AddedAt:  "2026-11-14",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repoDir, domain.RulePackRelPath), docBytes, 0644); err != nil {
		t.Fatal(err)
	}

	// Simulate conflict detected during SessionStart turn sync
	ev.dec = domain.MutationDecision{
		Action:          "OVERWRITE",
		ConflictFactID:  localID,
		ConflictFactIDs: []int64{localID},
		Cardinality:     "exclusive",
		Confidence:      0.94,
	}

	startPayload, _ := json.Marshal(map[string]any{
		"session_id": "sess-team-hook",
		"cwd":        repoDir,
	})
	hookOut, code, err := stack.gr.ExecuteHook(ctx, "claude-start", startPayload, repoDir)
	if err != nil || code != 0 {
		t.Fatalf("claude-start RunHook failed: code=%d err=%v out=%s", code, err, hookOut)
	}
	if !strings.Contains(hookOut, "[TEAM project:teamrepo/database]") || !strings.Contains(hookOut, "TEAM RULE CONFLICT DETECTED") {
		t.Fatalf("expected hook output to deliver TEAM rule and conflict notice, got:\n%s", hookOut)
	}
	if strings.Contains(hookOut, "TEAM:r-") {
		t.Fatalf("did not expect internal hash key exposed in hook output, got:\n%s", hookOut)
	}

	teamKey := domain.ComputeRuleKey("database", "Always use pure-Go modernc.org/sqlite with CGO_ENABLED=0")

	// VerifyPlan must include Team Rules and format violation cleanly with TEAM marker
	ev.verifyFn = func(plan string, rules []domain.Fact) []domain.PlanViolation {
		for _, r := range rules {
			if r.IsTeam() && r.RuleKey == teamKey && strings.Contains(plan, "CGO_ENABLED=1") {
				return []domain.PlanViolation{{Fact: r, Probability: 0.96, Confidence: 0.95}}
			}
		}
		return nil
	}
	vOut, err := stack.gr.VerifyPlan(ctx, "Run CGO_ENABLED=1 with modernc.org/sqlite", scope)
	if err != nil {
		t.Fatalf("VerifyPlan failed: %v", err)
	}
	if !strings.Contains(vOut, "database TEAM]") || !strings.Contains(vOut, "modernc.org/sqlite") {
		t.Fatalf("expected VerifyPlan violation to cite TEAM rule cleanly, got:\n%s", vOut)
	}

	// PreToolUse guardrail must block a bash command contradicting the Team Rule
	toolPayload, _ := json.Marshal(map[string]any{
		"session_id": "sess-team-hook",
		"cwd":        repoDir,
		"tool_name":  "Bash",
		"tool_input": map[string]any{
			"command": "CGO_ENABLED=1 go test ./internal/infra/sqlite/...",
		},
	})
	// Mark turn as already mutated so pre-mutate gate doesn't intercept before rule verification
	stack.mem.MarkTurnScopeMutated(scope)
	gOut, gCode, err := stack.gr.ExecuteHook(ctx, "claude-pretool", toolPayload, repoDir)
	if err != nil || gCode != 2 {
		t.Fatalf("claude-pretool RunHook expected exit code 2 (block): code=%d err=%v out=%s", gCode, err, gOut)
	}
	if !strings.Contains(gOut, "block") || !strings.Contains(gOut, "database TEAM") {
		t.Fatalf("expected PreToolUse guardrail block mentioning TEAM rule, got:\n%s", gOut)
	}

	// Resolve conflict via local_override -> Team Rule must stop blocking
	if err := rp.ResolveConflict(ctx, scope, teamKey, "local_override", "Testing override"); err != nil {
		t.Fatalf("ResolveConflict failed: %v", err)
	}
	vAfterOverride, err := stack.gr.VerifyPlan(ctx, "Run CGO_ENABLED=1 with modernc.org/sqlite", scope)
	if err != nil {
		t.Fatalf("VerifyPlan after local_override failed: %v", err)
	}
	if strings.Contains(vAfterOverride, "modernc.org/sqlite") {
		t.Fatalf("expected local_override to stop enforcing overridden TEAM rule, got:\n%s", vAfterOverride)
	}
}
