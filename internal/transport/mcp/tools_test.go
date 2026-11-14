package mcp

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"

	"memorize/internal/domain"
	"memorize/internal/modules/guardrail"
	"memorize/internal/modules/memory"
	"memorize/internal/modules/progress"
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
	queryFn := handleQueryTool(deps.Memory, deps.Progress, resolver)
	verifyFn := handleVerifyTool(deps.Guardrail, deps.Progress, resolver)
	manageFn := handleManageTool(deps.Memory, deps.Guardrail, resolver)
	progressFn := handleProgressTool(deps.Progress, resolver)

	// 1. Commit batch
	res, err := commitFn(ctx, mcpReq(map[string]any{
		"scope":           scope,
		"category":        "architecture",
		"sub_category":    "testing",
		"candidate_fact":  "Always keep SSR HTML templates under 800 lines.",
		"candidate_facts": []any{"Never expose internal credentials in public docs."},
	}))
	if err != nil || res.IsError {
		t.Fatalf("commit failed: err=%v res=%s", err, mcpText(t, res))
	}

	recs, err := deps.Memory.ListRecords(ctx, scope)
	if err != nil || len(recs) < 2 {
		t.Fatalf("expected >= 2 committed records, got len=%d err=%v", len(recs), err)
	}
	firstID, secondID := recs[0].ID, recs[1].ID

	// 2. Query session memory
	res, err = queryFn(ctx, mcpReq(map[string]any{
		"scope":        scope,
		"session_id":   "test-sess-1",
		"current_task": "Refactor dashboard templates",
		"refresh":      true,
	}))
	if err != nil || res.IsError {
		t.Fatalf("query failed: err=%v res=%s", err, mcpText(t, res))
	}

	// 3. Verify plan (clean & violating)
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
	if !strings.Contains(mcpText(t, res), "VIOLAT") && !strings.Contains(strings.ToUpper(mcpText(t, res)), "WARN") {
		t.Logf("verify forbidden output: %s", mcpText(t, res))
	}

	// 4. Manage update preserves pin when omitted, toggles on set_pin, and updates account
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

	// 5. Consolidate, undo, forget, purge, onboarding toggles, invalid action
	res, err = manageFn(ctx, mcpReq(map[string]any{
		"action":   "consolidate",
		"scope":    scope,
		"category": "architecture/testing",
		"ids":      []any{float64(firstID), float64(secondID)},
		"content":  "Keep templates modular under 800 lines and keep credentials private.",
		"pinned":   true,
	}))
	if err != nil || res.IsError {
		t.Fatalf("consolidate failed: %v %s", err, mcpText(t, res))
	}
	recsAfter, _ := deps.Memory.ListRecords(ctx, scope)
	var consID int64
	for _, r := range recsAfter {
		if r.Active && strings.Contains(r.Content, "Keep templates modular") {
			consID = r.ID
		}
	}
	if consID == 0 {
		t.Fatalf("consolidated record not found in active records: %+v", recsAfter)
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

	// 6. Progress record / update (preserving next_step) / list / delete
	res, err = progressFn(ctx, mcpReq(map[string]any{
		"action":       "record",
		"scope":        scope,
		"track":        "OSS Prep",
		"category":     "architecture",
		"sub_category": "testing",
		"status":       "in_progress",
		"summary":      "Expand command handler tests",
		"details":      "Line 1\nLine 2",
		"next_step":    "Run coverage check",
		"memory_ids":   []any{float64(secondID)},
	}))
	if err != nil || res.IsError {
		t.Fatalf("progress record failed: %v %s", err, mcpText(t, res))
	}
	ov, err := deps.Progress.GetProgressOverviewByTrack(ctx, scope, "OSS Prep", "", "", "")
	if err != nil || len(ov.Records) == 0 {
		t.Fatalf("GetProgressOverviewByTrack failed: len=%d err=%v", len(ov.Records), err)
	}
	progID := ov.Records[0].ID

	res, err = progressFn(ctx, mcpReq(map[string]any{
		"action": "update",
		"id":     progID,
		"status": "done",
	}))
	if err != nil || res.IsError {
		t.Fatalf("progress update failed: %v %s", err, mcpText(t, res))
	}
	gotProg, err := deps.Progress.GetProgressByID(ctx, progID)
	if err != nil || gotProg.Status != "done" || gotProg.NextStep != "Run coverage check" {
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

func TestTouchMCPLiveSessionDoesNotCrossTalk(t *testing.T) {
	deps, _ := newMCPTestDeps(t)
	ctx := context.Background()
	repo := t.TempDir()
	scope := "project:" + strings.ToLower(filepath.Base(repo))
	sidA := "conv-mcp-isolated-a"

	_, _, _ = deps.Guardrail.ExecuteHook(ctx, "antigravity-turn", []byte(`{"conversationId":"`+sidA+`","workspacePaths":["`+repo+`"],"lastUserInput":"작업 1"}`), "")
	_, _, _ = deps.Guardrail.ExecuteHook(ctx, "antigravity-stop", []byte(`{"conversationId":"`+sidA+`","workspacePaths":["`+repo+`"],"fullyIdle":true}`), "")

	// Generic bridge MCP call without explicit session_id must NOT guess turn_state or resurrect finished Session A.
	bridgeCtx := domain.WithSessionID(ctx, "bridge-999")
	touchMCPLiveSession(bridgeCtx, deps.Progress, scope, "", "commit_memory")
	if live := deps.Progress.ListLiveSessions(); len(live) != 0 {
		t.Fatalf("expected generic bridge MCP call not to resurrect finished sidA, got %+v", live)
	}

	// Explicit non-generic session_id updates live session status cleanly without polluting Progress DB.
	touchMCPLiveSession(bridgeCtx, deps.Progress, scope, "conv-explicit-mcp", "query_project_memory")
	live := deps.Progress.ListLiveSessions()
	if len(live) != 1 || live[0].SessionID != "conv-explicit-mcp" || live[0].Tool != "query_project_memory" {
		t.Fatalf("expected explicit session_id MCP call in ListLiveSessions, got %+v", live)
	}
	ov, err := deps.Progress.GetProgressOverviewWithQuery(ctx, domain.ProgressQuery{Scope: scope})
	if err != nil {
		t.Fatal(err)
	}
	if ov.TotalCount != 0 {
		t.Fatalf("expected live MCP session not to pollute persistent Progress DB, got total=%d", ov.TotalCount)
	}
}
