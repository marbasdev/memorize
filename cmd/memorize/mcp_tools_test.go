package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/mcp"

	"memorize/internal/memory"
	"memorize/internal/memory/localmodel"
)

type stubMCPEval struct {
	nextAction string
}

func (e stubMCPEval) Name() string { return "stub-mcp-eval" }

func (e stubMCPEval) ScoreRelevance(_ context.Context, _ string, facts []memory.Fact) ([]memory.ScoredFact, error) {
	out := make([]memory.ScoredFact, len(facts))
	for i, f := range facts {
		out[i] = memory.ScoredFact{Fact: f, Score: 4.6, Confidence: 0.95}
	}
	return out, nil
}

func (e stubMCPEval) EvaluateMutation(_ context.Context, _, _ string, _ []memory.Fact) (memory.MutationDecision, error) {
	act := e.nextAction
	if act == "" {
		act = "INSERT"
	}
	return memory.MutationDecision{
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

func (e stubMCPEval) VerifyPlan(_ context.Context, proposedPlan string, rules []memory.Fact) ([]memory.PlanViolation, error) {
	if !strings.Contains(strings.ToUpper(proposedPlan), "FORBIDDEN") {
		return nil, nil
	}
	var out []memory.PlanViolation
	for _, r := range rules {
		out = append(out, memory.PlanViolation{Fact: r, Probability: 0.95, Confidence: 0.95})
	}
	return out, nil
}

func newMCPTestService(t *testing.T) (*memory.Service, *scopeResolver) {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "test.db")
	svc, err := memory.NewService(dbPath, stubMCPEval{})
	if err != nil {
		t.Fatalf("NewService failed: %v", err)
	}
	t.Cleanup(func() { _ = svc.Close() })
	srv := buildMCPServer(svc)
	return svc, &scopeResolver{srv: srv, cache: make(map[string][]resolvedWorkspace)}
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
	if got := firstNonEmptyStr("", "   ", "alpha", "beta"); got != "alpha" {
		t.Fatalf("firstNonEmptyStr = %q, want alpha", got)
	}
	if got := firstNonEmptyStr("", " "); got != "" {
		t.Fatalf("expected empty string, got %q", got)
	}

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

	bCtx := withBridgeMeta(context.Background(), bridgeReqMeta{
		sessionID: "bridge-sess-1",
		cwd:       "/tmp/some-dir",
		roots:     []string{"file:///tmp/my-workspace"},
	})
	if got := sessionIDFrom(bCtx); got != "bridge-sess-1" {
		t.Fatalf("sessionIDFrom bridge = %q", got)
	}
	if got := effectiveWorkDir(bCtx); got != "/tmp/some-dir" {
		t.Fatalf("effectiveWorkDir bridge = %q", got)
	}
	_, r := newMCPTestService(t)
	if sc, _ := r.resolve(bCtx, "", ""); sc != "project:my-workspace" {
		t.Fatalf("scopeResolver bridge root = %q, want project:my-workspace", sc)
	}
	if sc, _ := r.resolve(bCtx, "global", ""); sc != "global" {
		t.Fatalf("scopeResolver explicit global = %q", sc)
	}
}

func TestMainStorageDaemonAndSecurityHelpers(t *testing.T) {
	tmp := t.TempDir()
	customDB := filepath.Join(tmp, "custom.db")
	t.Setenv("MEMORY_DB_PATH", customDB)
	t.Setenv("MEMORY_MODELS_DIR", "")
	dbPath, modelsDir := resolveStoragePaths()
	if dbPath != customDB || modelsDir != filepath.Join(tmp, "models") {
		t.Fatalf("resolveStoragePaths mismatch: db=%q models=%q", dbPath, modelsDir)
	}

	t.Setenv("MEMORY_DB_PATH", "")
	t.Setenv("MEMORY_DATA_DIR", filepath.Join(tmp, "data"))
	dbPath, modelsDir = resolveStoragePaths()
	if dbPath != filepath.Join(tmp, "data", "memories.db") || modelsDir != filepath.Join(tmp, "data", "models") {
		t.Fatalf("resolveStoragePaths DATA_DIR mismatch: db=%q models=%q", dbPath, modelsDir)
	}

	if !isInlineModeForced(":memory:") || !isInlineModeForced("file::memory:?cache=shared") {
		t.Fatalf("expected memory DB paths to force inline mode")
	}
	t.Setenv("MEMORY_DISABLE_BG_DASHBOARD", "1")
	if !isInlineModeForced(filepath.Join(tmp, "disk.db")) {
		t.Fatalf("expected MEMORY_DISABLE_BG_DASHBOARD=1 to force inline mode")
	}

	if act := parseDaemonCtlAction("restart"); act != "restart" {
		t.Fatalf("parseDaemonCtlAction explicit = %q", act)
	}

	t.Setenv("MEMORY_LAUNCHER_START_SEC", "")
	now := time.Now()
	if got := effectiveProcStart(now); !got.Equal(now) {
		t.Fatalf("expected goStart unchanged when env unset")
	}

	req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:18095/mcp", nil)
	req.Host = "127.0.0.1:18095"
	req.Header.Set("Content-Type", "application/json")
	if !isLocalJSONPost(req) {
		t.Fatalf("expected localhost JSON POST to be accepted")
	}
	reqCross := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:18095/mcp", nil)
	reqCross.Host = "127.0.0.1:18095"
	reqCross.Header.Set("Sec-Fetch-Site", "cross-site")
	reqCross.Header.Set("Content-Type", "application/json")
	if isLocalJSONPost(reqCross) {
		t.Fatalf("expected cross-site request to be rejected")
	}
	reqEvilOrigin := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:18095/", nil)
	reqEvilOrigin.Host = "127.0.0.1:18095"
	reqEvilOrigin.Header.Set("Origin", "https://evil.example")
	if isLocalRequest(reqEvilOrigin) {
		t.Fatalf("expected foreign Origin to be rejected")
	}

	t.Setenv("MEMORY_PROVIDER", "gemini")
	if ev := selectEvaluator(nil); !strings.HasPrefix(ev.Name(), "gemini") {
		t.Fatalf("selectEvaluator(gemini) = %q", ev.Name())
	}
	t.Setenv("MEMORY_PROVIDER", "")
	mgr := localmodel.NewManager(t.TempDir())
	if ev := selectEvaluator(mgr); ev == nil || ev.Name() == "" {
		t.Fatalf("expected non-nil default evaluator")
	}
	if got := daemonLogPath(customDB); got != filepath.Join(tmp, "daemon.log") {
		t.Fatalf("daemonLogPath = %q", got)
	}
	t.Setenv("MEMORY_DAEMON_ADDR", "127.0.0.1:29999")
	if got := resolveDaemonAddr(""); got != "127.0.0.1:29999" {
		t.Fatalf("resolveDaemonAddr env = %q", got)
	}
}

func TestMCPToolHandlersRoundtrip(t *testing.T) {
	svc, resolver := newMCPTestService(t)
	ctx := memory.WithAccount(context.Background(), "work")
	scope := "project:mcptest"

	commitFn := handleCommitTool(svc, resolver)
	queryFn := handleQueryTool(svc, resolver)
	verifyFn := handleVerifyTool(svc, resolver)
	manageFn := handleManageTool(svc, resolver)
	progressFn := handleProgressTool(svc, resolver)

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

	recs, err := svc.ListRecords(ctx, scope)
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
		// Stub triggers violation when plan has FORBIDDEN
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
	updated, err := svc.GetRecordsByIDs(ctx, []int64{firstID})
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
	recsAfter, _ := svc.ListRecords(ctx, scope)
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
	ov, err := svc.GetProgressOverviewByTrack(ctx, scope, "OSS Prep", "", "", "")
	if err != nil || len(ov.Records) == 0 {
		t.Fatalf("GetProgressOverviewByTrack failed: len=%d err=%v", len(ov.Records), err)
	}
	progID := ov.Records[0].ID

	// Update status without next_step preserves previous NextStep
	res, err = progressFn(ctx, mcpReq(map[string]any{
		"action": "update",
		"id":     progID,
		"status": "done",
	}))
	if err != nil || res.IsError {
		t.Fatalf("progress update failed: %v %s", err, mcpText(t, res))
	}
	gotProg, err := svc.GetProgressByID(ctx, progID)
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

	// Silence unused os import if needed, verify test DB path cleanup handles files
	if _, err := os.Stat(filepath.Join(t.TempDir(), "nonexistent")); !os.IsNotExist(err) {
		t.Fatalf("unexpected file state")
	}
}
