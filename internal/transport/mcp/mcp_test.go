package mcp

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"

	"praxis/internal/domain"
	"praxis/internal/modules/guardrail"
	"praxis/internal/modules/memory"
	"praxis/internal/modules/progress"
	"praxis/internal/modules/rulepack"
)

type stubMCPEval struct {
	nextAction string
}

func (e stubMCPEval) Name() string { return "stub-mcp-eval" }

func (e stubMCPEval) ScoreRelevance(_ context.Context, _ string, facts []domain.Fact) ([]domain.ScoredFact, error) {
	out := make([]domain.ScoredFact, len(facts))
	for i, f := range facts {
		out[i] = domain.ScoredFact{Fact: f, Score: 4.6, Confidence: 0.95}
	}
	return out, nil
}

func (e stubMCPEval) EvaluateMutation(_ context.Context, _, _ string, _ []domain.Fact) (domain.MutationDecision, error) {
	act := e.nextAction
	if act == "" {
		act = "INSERT"
	}
	return domain.MutationDecision{
		Category:          "architecture",
		SubCategory:       "testing",
		Tags:              []string{"rule"},
		Tier:              "rule",
		IsPermanent:       0.9,
		IsGlobalInvariant: 0.92,
		Action:            act,
		Confidence:        0.95,
	}, nil
}

func (e stubMCPEval) VerifyPlan(_ context.Context, proposedPlan string, rules []domain.Fact) ([]domain.PlanViolation, error) {
	if !strings.Contains(strings.ToUpper(proposedPlan), "FORBIDDEN") {
		return nil, nil
	}
	var out []domain.PlanViolation
	for _, r := range rules {
		out = append(out, domain.PlanViolation{Fact: r, Probability: 0.95, Confidence: 0.95})
	}
	return out, nil
}

func newMCPTestDeps(t *testing.T) (Deps, *ScopeResolver) {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "test.db")
	mem, err := memory.Open(dbPath, stubMCPEval{})
	if err != nil {
		t.Fatalf("memory.Open failed: %v", err)
	}
	t.Cleanup(func() { _ = mem.Close() })
	prog := progress.New(mem)
	gr := guardrail.New(mem, prog)
	deps := Deps{Memory: mem, Progress: prog, Guardrail: gr, Version: "test"}
	srv := NewServer(deps)
	return deps, NewScopeResolver(srv)
}

func mcpReq(args map[string]any) mcp.CallToolRequest {
	return mcp.CallToolRequest{
		Params: mcp.CallToolParams{
			Arguments: args,
		},
	}
}

func mcpText(t *testing.T, res *mcp.CallToolResult) string {
	t.Helper()
	if res == nil || len(res.Content) == 0 {
		t.Fatalf("empty MCP tool result: %+v", res)
	}
	tc, ok := mcp.AsTextContent(res.Content[0])
	if !ok {
		t.Fatalf("expected TextContent, got %T", res.Content[0])
	}
	return tc.Text
}

func TestMCPArgAndScopeHelpers(t *testing.T) {
	if v, ok := toInt64(float64(42)); !ok || v != 42 {
		t.Fatalf("toInt64(float64(42)) = %d, %v", v, ok)
	}
	if v, ok := toInt64(int(7)); !ok || v != 7 {
		t.Fatalf("toInt64(int(7)) = %d, %v", v, ok)
	}
	if v, ok := toInt64(int64(9)); !ok || v != 9 {
		t.Fatalf("toInt64(int64(9)) = %d, %v", v, ok)
	}
	if v, ok := toInt64(" 15 "); !ok || v != 15 {
		t.Fatalf("toInt64(' 15 ') = %d, %v", v, ok)
	}
	if _, ok := toInt64(-3); ok {
		t.Fatalf("expected negative int64 to be rejected")
	}

	single, ids := parseIDs(mcpReq(map[string]any{"ids": []any{float64(10), "20", 0, -1}}))
	if single != 10 || len(ids) != 2 || ids[0] != 10 || ids[1] != 20 {
		t.Fatalf("parseIDs mismatch: single=%d ids=%v", single, ids)
	}
	single, ids = parseIDs(mcpReq(map[string]any{"id": int64(5)}))
	if single != 5 || len(ids) != 1 || ids[0] != 5 {
		t.Fatalf("parseIDs single fallback mismatch: single=%d ids=%v", single, ids)
	}

	memIDs, present := parseMemoryIDListArg(mcpReq(map[string]any{}))
	if present || memIDs != nil {
		t.Fatalf("expected missing memory_ids to return nil, false")
	}
	memIDs, present = parseMemoryIDListArg(mcpReq(map[string]any{"memory_ids": []any{float64(3), float64(4)}}))
	if !present || len(memIDs) != 2 || memIDs[0] != 3 || memIDs[1] != 4 {
		t.Fatalf("parseMemoryIDListArg mismatch: present=%v ids=%v", present, memIDs)
	}

	if got := combineCategory("architecture", "api"); got != "architecture/api" {
		t.Fatalf("combineCategory = %q", got)
	}
	if got := combineCategory("", "hooks"); got != "general/hooks" {
		t.Fatalf("combineCategory empty cat = %q", got)
	}
	if got := combineCategory("security/auth", "ignored"); got != "security/auth" {
		t.Fatalf("combineCategory slash = %q", got)
	}

	if sc, p := parseRootURI("file:///Users/dev/DemoRepo/"); sc != "project:demorepo" || p != "/Users/dev/DemoRepo" {
		t.Fatalf("parseRootURI mismatch: sc=%q p=%q", sc, p)
	}
	if sc, p := parseRootURI("file:///C:/Users/dev/DemoRepo/"); sc != "project:demorepo" || p != "C:/Users/dev/DemoRepo" {
		t.Fatalf("parseRootURI windows URI mismatch: sc=%q p=%q", sc, p)
	}
	if sc, p := parseRootURI(`C:\Users\dev\DemoRepo\`); sc != "project:demorepo" || p != `C:\Users\dev\DemoRepo` {
		t.Fatalf("parseRootURI windows raw path mismatch: sc=%q p=%q", sc, p)
	}
	if sc, _ := parseRootURI("/"); sc != "" {
		t.Fatalf("expected empty scope for root /, got %q", sc)
	}
	if sc, _ := parseRootURI("file:///C:/"); sc != "" {
		t.Fatalf("expected empty scope for Windows drive root file:///C:/, got %q", sc)
	}
	if sc, _ := parseRootURI(`C:\`); sc != "" {
		t.Fatalf("expected empty scope for Windows drive root C:\\, got %q", sc)
	}

	bCtx := WithBridgeMeta(context.Background(), BridgeReqMeta{
		SessionID: "bridge-sess-1",
		Cwd:       "/tmp/some-dir",
		Roots:     []string{"file:///tmp/my-workspace"},
	})
	if got := sessionIDFrom(bCtx); got != "bridge-sess-1" {
		t.Fatalf("sessionIDFrom bridge = %q", got)
	}
	if got := effectiveWorkDir(bCtx); got != "/tmp/some-dir" {
		t.Fatalf("effectiveWorkDir bridge = %q", got)
	}
	_, r := newMCPTestDeps(t)
	if sc, _ := r.Resolve(bCtx, "", ""); sc != "project:my-workspace" {
		t.Fatalf("ScopeResolver bridge root = %q, want project:my-workspace", sc)
	}
	if sc, _ := r.Resolve(bCtx, "global", ""); sc != "global" {
		t.Fatalf("ScopeResolver explicit global = %q", sc)
	}
}

func TestMCPToolHandlersRoundtrip(t *testing.T) {
	deps, resolver := newMCPTestDeps(t)
	ctx := domain.WithAccount(context.Background(), "work")
	scope := "project:mcptest"

	commitFn := handleCommitTool(deps.Memory, deps.Progress, resolver)
	queryFn := handleQueryTool(deps.Memory, deps.Progress, deps.RulePack, resolver)
	verifyFn := handleVerifyTool(deps.Guardrail, deps.Progress, resolver)
	manageFn := handleManageTool(deps.Memory, deps.Guardrail, deps.Progress, resolver)
	progressFn := handleProgressTool(deps.Progress, resolver)

	res, err := commitFn(ctx, mcpReq(map[string]any{
		"scope":           scope,
		"category":        "architecture",
		"sub_category":    "testing",
		"candidate_fact":  "Always validate loopback Origin headers on JSON mutations.",
		"candidate_facts": []any{"Never expose internal credentials in public docs."},
	}))
	if err != nil || res.IsError {
		t.Fatalf("commit failed: err=%v res=%s", err, mcpText(t, res))
	}

	page, err := deps.Memory.QueryRecordsPage(ctx, domain.RecordQuery{Scope: scope, State: "all", PageSize: 100})
	if err != nil || len(page.Records) < 2 {
		t.Fatalf("expected >= 2 committed records, got len=%d err=%v", len(page.Records), err)
	}
	firstID, secondID := page.Records[0].ID, page.Records[1].ID

	res, err = queryFn(ctx, mcpReq(map[string]any{
		"scope":        scope,
		"session_id":   "test-sess-1",
		"current_task": "Refactor dashboard templates",
		"refresh":      true,
	}))
	if err != nil || res.IsError {
		t.Fatalf("query failed: err=%v res=%s", err, mcpText(t, res))
	}

	res, err = verifyFn(ctx, mcpReq(map[string]any{
		"scope":         scope,
		"proposed_plan": "Split oversized Go template nicely.",
	}))
	if err != nil || res.IsError {
		t.Fatalf("verify clean failed: err=%v res=%s", err, mcpText(t, res))
	}
	res, err = verifyFn(ctx, mcpReq(map[string]any{
		"scope":         scope,
		"proposed_plan": "FORBIDDEN: dump API secret into template",
	}))
	if err != nil {
		t.Fatalf("verify forbidden err: %v", err)
	}
	if !strings.Contains(strings.ToUpper(mcpText(t, res)), "VIOLAT") && !strings.Contains(strings.ToUpper(mcpText(t, res)), "WARN") {
		t.Fatalf("expected violation output on forbidden plan, got: %s", mcpText(t, res))
	}

	res, err = manageFn(ctx, mcpReq(map[string]any{
		"action": "set_pin",
		"id":     firstID,
		"pinned": true,
	}))
	if err != nil || res.IsError {
		t.Fatalf("set_pin failed: %v %s", err, mcpText(t, res))
	}
	res, err = manageFn(ctx, mcpReq(map[string]any{
		"action":       "update",
		"id":           firstID,
		"sub_category": "modular",
		"account":      "oss",
	}))
	if err != nil || res.IsError {
		t.Fatalf("manage update failed: %v %s", err, mcpText(t, res))
	}
	updated, err := deps.Memory.GetRecordsByIDs(ctx, []int64{firstID})
	if err != nil || len(updated) != 1 || !updated[0].Pinned || updated[0].SubCategory != "modular" || updated[0].Account != "oss" {
		t.Fatalf("unexpected updated record: %+v err=%v", updated, err)
	}

	res, err = manageFn(ctx, mcpReq(map[string]any{
		"action":   "consolidate",
		"scope":    scope,
		"category": "architecture/testing",
		"ids":      []any{float64(firstID), float64(secondID)},
		"content":  "Validate loopback Origin headers and keep internal credentials private.",
		"pinned":   true,
	}))
	if err != nil || res.IsError {
		t.Fatalf("consolidate failed: %v %s", err, mcpText(t, res))
	}
	afterPage, _ := deps.Memory.QueryRecordsPage(ctx, domain.RecordQuery{Scope: scope, State: "active", PageSize: 100})
	var consID int64
	for _, r := range afterPage.Records {
		if r.Active && strings.Contains(r.Content, "Validate loopback Origin headers") {
			consID = r.ID
		}
	}
	if consID == 0 {
		t.Fatalf("consolidated record not found in active records: %+v", afterPage.Records)
	}
	if res, _ = manageFn(ctx, mcpReq(map[string]any{"action": "undo", "id": consID})); res.IsError {
		t.Fatalf("undo failed: %s", mcpText(t, res))
	}
	if res, _ = manageFn(ctx, mcpReq(map[string]any{"action": "forget", "ids": []any{float64(firstID)}})); res.IsError {
		t.Fatalf("forget failed: %s", mcpText(t, res))
	}
	if res, _ = manageFn(ctx, mcpReq(map[string]any{"action": "purge", "scope": scope})); res.IsError {
		t.Fatalf("purge failed: %s", mcpText(t, res))
	}
	if res, _ = manageFn(ctx, mcpReq(map[string]any{"action": "skip_onboarding", "scope": scope})); res.IsError {
		t.Fatalf("skip_onboarding failed: %s", mcpText(t, res))
	}
	if res, _ = manageFn(ctx, mcpReq(map[string]any{"action": "reset_onboarding", "scope": scope})); res.IsError {
		t.Fatalf("reset_onboarding failed: %s", mcpText(t, res))
	}
	if res, _ = manageFn(ctx, mcpReq(map[string]any{"action": "bogus"})); !res.IsError {
		t.Fatalf("expected unknown manage action to fail")
	}

	res, err = progressFn(ctx, mcpReq(map[string]any{
		"action":       "record",
		"scope":        scope,
		"track":        "OSS Prep",
		"category":     "architecture",
		"sub_category": "testing",
		"summary":      "Expand command handler tests",
		"details":      "Line 1\nLine 2",
		"next_step":    "Run coverage check",
		"memory_ids":   []any{float64(secondID)},
	}))
	if err != nil || res.IsError {
		t.Fatalf("progress record failed: %v %s", err, mcpText(t, res))
	}
	ov, err := deps.Progress.GetProgressOverviewByTrack(ctx, scope, "OSS Prep", "", "")
	if err != nil || len(ov.Records) == 0 {
		t.Fatalf("GetProgressOverviewByTrack failed: len=%d err=%v", len(ov.Records), err)
	}
	progID := ov.Records[0].ID

	res, err = progressFn(ctx, mcpReq(map[string]any{
		"action": "update",
		"id":     progID,
	}))
	if err != nil || res.IsError {
		t.Fatalf("progress update failed: %v %s", err, mcpText(t, res))
	}
	gotProg, err := deps.Progress.GetProgressByID(ctx, progID)
	if err != nil || gotProg.NextStep != "Run coverage check" {
		t.Fatalf("unexpected progress after update: %+v err=%v", gotProg, err)
	}

	res, err = progressFn(ctx, mcpReq(map[string]any{
		"action": "list",
		"scope":  scope,
		"track":  "OSS Prep",
	}))
	if err != nil || res.IsError || !strings.Contains(mcpText(t, res), "OSS Prep") {
		t.Fatalf("progress list mismatch: err=%v out=%s", err, mcpText(t, res))
	}
	if res, _ = progressFn(ctx, mcpReq(map[string]any{"action": "delete", "id": progID})); res.IsError {
		t.Fatalf("progress delete failed: %s", mcpText(t, res))
	}
	if res, _ = progressFn(ctx, mcpReq(map[string]any{"action": "unknown"})); !res.IsError {
		t.Fatalf("expected unknown progress action error")
	}

	if _, err := os.Stat(filepath.Join(t.TempDir(), "nonexistent")); !os.IsNotExist(err) {
		t.Fatalf("unexpected file state")
	}
}

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
	mem.RememberRepoPath(ctx, "project:teamrepo", repoDir)

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

	addRes, err := handler(ctx, mcpReq(map[string]any{
		"action":    "add",
		"scope":     "project:teamrepo",
		"repo_path": repoDir,
		"content":   "Always run static builds with CGO_ENABLED=0",
		"category":  "architecture",
	}))
	if err != nil {
		t.Fatal(err)
	}
	addOut := mcpText(t, addRes)
	if !strings.Contains(addOut, "TEAM_RULE_PUBLISHED") || strings.Contains(addOut, "rule_key=") {
		t.Fatalf("expected TEAM_RULE_PUBLISHED without exposed rule_key, got:\n%s", addOut)
	}

	listRes, err := handler(ctx, mcpReq(map[string]any{
		"action":    "list",
		"scope":     "project:teamrepo",
		"repo_path": repoDir,
	}))
	if err != nil {
		t.Fatal(err)
	}
	listOut := mcpText(t, listRes)
	if !strings.Contains(listOut, "1. (architecture)") || !strings.Contains(listOut, "@Nick-Author") || !strings.Contains(listOut, "Always run static builds with CGO_ENABLED=0") ||
		strings.Contains(listOut, "[invariant]") || strings.Contains(listOut, "[rule]") {
		t.Fatalf("unexpected list output:\n%s", listOut)
	}

	updRes, err := handler(ctx, mcpReq(map[string]any{
		"action":       "update",
		"scope":        "project:teamrepo",
		"repo_path":    repoDir,
		"id":           float64(1),
		"sub_category": "ui",
		"content":      "Keep static builds strictly CGO_ENABLED=0 across OS targets",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(mcpText(t, updRes), "TEAM_RULE_UPDATED") {
		t.Fatalf("expected TEAM_RULE_UPDATED, got:\n%s", mcpText(t, updRes))
	}

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
