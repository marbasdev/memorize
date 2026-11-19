package mcp

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"memorize/internal/domain"
	"memorize/internal/infra/sqlite"
	"memorize/internal/modules/guardrail"
	"memorize/internal/modules/memory"
	"memorize/internal/modules/progress"
	"memorize/internal/modules/rulepack"
)

type stubMCPTeamEval struct {
	stubMCPEval
}

func (s *stubMCPTeamEval) DraftRulePackProposal(_ context.Context, scope, rawRuleText, sourceCat, sourceSub string, _ []domain.RuleOutlineEntry) (domain.RulePlacementProposal, error) {
	cat := sourceCat
	if cat == "" {
		cat = "workflow"
	}
	normCat := domain.NormalizeCategoryForScope(scope, cat)
	return domain.RulePlacementProposal{
		RuleKey:     domain.ComputeRuleKey(normCat, rawRuleText),
		Category:    normCat,
		SubCategory: sourceSub,
		Pinned:      true,
		Title:       rawRuleText,
	}, nil
}

func (s *stubMCPTeamEval) FindRedundantLocalMemories(_ context.Context, _ string, teamRules []domain.TeamRule, localFacts []domain.Fact) ([]domain.RuleDuplicateSuggestion, error) {
	if len(teamRules) == 0 || len(localFacts) == 0 {
		return nil, nil
	}
	return []domain.RuleDuplicateSuggestion{
		{
			LocalMemoryID:  localFacts[0].ID,
			LocalCategory:  localFacts[0].Category,
			LocalContent:   localFacts[0].Content,
			TeamRuleKey:    teamRules[0].RuleKey,
			TeamRuleTitle:  teamRules[0].Title,
			Kind:           "partial_overlap",
			TrimmedContent: "Keep local debug flag enabled",
			Reason:         "Partial overlap with team rule",
		},
	}, nil
}

func runGitMCPTest(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v failed: %v\n%s", args, err, string(out))
	}
}

func TestMCPTeamRulesToolWorkflow(t *testing.T) {
	tmp := t.TempDir()
	bareDir := filepath.Join(tmp, "origin.git")
	runGitMCPTest(t, tmp, "init", "--bare", bareDir)

	repoDir := filepath.Join(tmp, "teamrepo")
	runGitMCPTest(t, tmp, "clone", bareDir, repoDir)
	runGitMCPTest(t, repoDir, "config", "user.name", "Nick MCP")
	runGitMCPTest(t, repoDir, "config", "user.email", "nick@example.com")
	runGitMCPTest(t, repoDir, "checkout", "-B", "main")
	if err := os.WriteFile(filepath.Join(repoDir, "README.md"), []byte("ok\n"), 0644); err != nil {
		t.Fatal(err)
	}
	runGitMCPTest(t, repoDir, "add", "README.md")
	runGitMCPTest(t, repoDir, "commit", "-m", "init")
	runGitMCPTest(t, repoDir, "push", "-u", "origin", "main")

	dbPath := filepath.Join(tmp, "mcp_team.db")
	mem, err := memory.Open(dbPath, &stubMCPTeamEval{})
	if err != nil {
		t.Fatalf("memory.Open: %v", err)
	}
	defer mem.Close()

	prog := progress.New(mem)
	rp := rulepack.New(mem)
	mem.AttachExtraFactsProvider(rp)
	gr := guardrail.New(mem, prog)
	gr.AttachRulePack(rp)

	deps := Deps{Memory: mem, Progress: prog, Guardrail: gr, RulePack: rp, Version: "test"}
	srv := NewServer(deps)
	resolver := NewScopeResolver(srv)
	handler := handleTeamRuleTool(deps, resolver)

	ctx := context.Background()
	sqlite.RememberRepoPath(ctx, mem.DB(), "project:teamrepo", repoDir)

	// 1. Configure git handle and git mode via set_config
	cfgRes, err := handler(ctx, mcpReq(map[string]any{
		"action":    "set_config",
		"scope":     "project:teamrepo",
		"repo_path": repoDir,
		"author":    "Nick Author",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(mcpText(t, cfgRes), "TEAM_RULE_CONFIG_UPDATED") {
		t.Fatalf("unexpected set_config output: %s", mcpText(t, cfgRes))
	}

	// 2. Add a team rule directly from chat prompt (concise single-line AI draft + isolated push)
	addRes, err := handler(ctx, mcpReq(map[string]any{
		"action":    "add",
		"scope":     "project:teamrepo",
		"repo_path": repoDir,
		"content":   "Always keep UI templates below 800 lines",
		"category":  "architecture",
	}))
	if err != nil {
		t.Fatal(err)
	}
	addOut := mcpText(t, addRes)
	if !strings.Contains(addOut, "TEAM_RULE_PUBLISHED") || strings.Contains(addOut, "rule_key=") {
		t.Fatalf("expected TEAM_RULE_PUBLISHED without exposed rule_key, got:\n%s", addOut)
	}

	// 3. List team rules shows numbered single-line summary + normalized git handle @Nick-Author without key/tier tags
	listRes, err := handler(ctx, mcpReq(map[string]any{
		"action":    "list",
		"scope":     "project:teamrepo",
		"repo_path": repoDir,
	}))
	if err != nil {
		t.Fatal(err)
	}
	listOut := mcpText(t, listRes)
	if !strings.Contains(listOut, "1. (architecture)") || !strings.Contains(listOut, "@Nick-Author") || !strings.Contains(listOut, "Always keep UI templates below 800 lines") ||
		strings.Contains(listOut, "[invariant]") || strings.Contains(listOut, "[rule]") {
		t.Fatalf("unexpected list output:\n%s", listOut)
	}

	// 3b. Update team rule #1 cleanly in place
	updRes, err := handler(ctx, mcpReq(map[string]any{
		"action":       "update",
		"scope":        "project:teamrepo",
		"repo_path":    repoDir,
		"id":           float64(1),
		"sub_category": "ui",
		"content":      "Keep custom UI templates strictly below 800 lines",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(mcpText(t, updRes), "TEAM_RULE_UPDATED") {
		t.Fatalf("expected TEAM_RULE_UPDATED, got:\n%s", mcpText(t, updRes))
	}

	// 4. Commit a local rule, promote a second local rule, detect & clean partial_overlap
	if _, err := mem.CommitMemoryFull(ctx, "Keep UI templates small and use local debug flag", "architecture", "", "", "project:teamrepo"); err != nil {
		t.Fatal(err)
	}
	locals, err := mem.LoadActiveFacts(ctx, "project:teamrepo", "project:teamrepo", "")
	if err != nil || len(locals) == 0 {
		t.Fatalf("expected 1 local memory, got %+v err=%v", locals, err)
	}
	localID := locals[0].ID

	dupRes, err := handler(ctx, mcpReq(map[string]any{
		"action":    "duplicates",
		"scope":     "project:teamrepo",
		"repo_path": repoDir,
	}))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(mcpText(t, dupRes), "partial_overlap") {
		t.Fatalf("expected duplicates detection output, got:\n%s", mcpText(t, dupRes))
	}

	cleanRes, err := handler(ctx, mcpReq(map[string]any{
		"action":    "clean_duplicates",
		"scope":     "project:teamrepo",
		"repo_path": repoDir,
		"ids":       []any{float64(localID)},
	}))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(mcpText(t, cleanRes), "DUPLICATES_CLEANED") {
		t.Fatalf("expected DUPLICATES_CLEANED, got:\n%s", mcpText(t, cleanRes))
	}

	// 5. Promote remaining trimmed local memory
	promRes, err := handler(ctx, mcpReq(map[string]any{
		"action":       "promote",
		"scope":        "project:teamrepo",
		"repo_path":    repoDir,
		"id":           float64(localID),
		"retire_local": true,
	}))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(mcpText(t, promRes), "TEAM_RULE_PUBLISHED") {
		t.Fatalf("expected promote publish, got:\n%s", mcpText(t, promRes))
	}

	// Remove rule #2 cleanly via isolated push
	rmRes, err := handler(ctx, mcpReq(map[string]any{
		"action":    "remove",
		"scope":     "project:teamrepo",
		"repo_path": repoDir,
		"id":        float64(2),
	}))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(mcpText(t, rmRes), "TEAM_RULE_REMOVED") {
		t.Fatalf("expected TEAM_RULE_REMOVED, got:\n%s", mcpText(t, rmRes))
	}

	// Sync check runs cleanly
	syncRes, err := handler(ctx, mcpReq(map[string]any{
		"action":    "sync",
		"scope":     "project:teamrepo",
		"repo_path": repoDir,
	}))
	if err != nil {
		t.Fatal(err)
	}
	syncOut := mcpText(t, syncRes)
	if !strings.Contains(syncOut, "TEAM_RULES_SYNCED") || !strings.Contains(syncOut, "rules=1") {
		t.Fatalf("expected TEAM_RULES_SYNCED with 1 remaining rule, got:\n%s", syncOut)
	}

	// In ServerMode, git-based sync/publish actions are blocked against host filesystem while list remains allowed.
	srvMemberCtx := domain.WithAuthPrincipal(context.Background(), domain.AuthPrincipal{
		Account:       "alice",
		Role:          domain.RoleMember,
		ServerMode:    true,
		Authenticated: true,
	})
	for _, blockedAct := range []string{"sync", "add", "promote", "update", "remove"} {
		res, callErr := handler(srvMemberCtx, mcpReq(map[string]any{
			"action":    blockedAct,
			"scope":     "project:teamrepo",
			"repo_path": repoDir,
			"content":   "Should not mutate host git",
			"id":        float64(1),
		}))
		if callErr != nil || !res.IsError {
			t.Fatalf("expected ServerMode team_rules(action=%q) to return tool error, got err=%v res=%+v", blockedAct, callErr, res)
		}
	}
	srvListRes, err := handler(srvMemberCtx, mcpReq(map[string]any{
		"action": "list",
		"scope":  "project:teamrepo",
	}))
	if err != nil || srvListRes.IsError {
		t.Fatalf("expected ServerMode team_rules(action=list) to succeed cleanly: err=%v res=%+v", err, srvListRes)
	}
}
