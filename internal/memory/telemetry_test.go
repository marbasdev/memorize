package memory_test

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"memorize/internal/memory"
)

type telemetrySpyEval struct {
	name      string
	sleep     time.Duration
	scoreErr  error
	mutateErr error
	verifyErr error
	mutDec    memory.MutationDecision
}

func (e *telemetrySpyEval) Name() string {
	if e.name != "" {
		return e.name
	}
	return "spy:telemetry-test"
}

func (e *telemetrySpyEval) ScoreRelevance(_ context.Context, _ string, facts []memory.Fact) ([]memory.ScoredFact, error) {
	if e.sleep > 0 {
		time.Sleep(e.sleep)
	}
	if e.scoreErr != nil {
		return nil, e.scoreErr
	}
	out := make([]memory.ScoredFact, len(facts))
	for i, f := range facts {
		out[i] = memory.ScoredFact{Fact: f, Score: 4.5, Confidence: 0.9}
	}
	return out, nil
}

func (e *telemetrySpyEval) EvaluateMutation(_ context.Context, _ string, _ string, _ []memory.Fact) (memory.MutationDecision, error) {
	if e.sleep > 0 {
		time.Sleep(e.sleep)
	}
	if e.mutateErr != nil {
		return memory.MutationDecision{}, e.mutateErr
	}
	return e.mutDec, nil
}

func (e *telemetrySpyEval) VerifyPlan(_ context.Context, _ string, _ []memory.Fact) ([]memory.PlanViolation, error) {
	if e.sleep > 0 {
		time.Sleep(e.sleep)
	}
	if e.verifyErr != nil {
		return nil, e.verifyErr
	}
	return nil, nil
}

func TestEvalTelemetryTimeoutGuardrailElapsedAndLiveHookOverhead(t *testing.T) {
	ctx := context.Background()
	eval := &telemetrySpyEval{
		name:  "gemini:gemini-3.5-flash-lite",
		sleep: 3 * time.Millisecond,
		mutDec: memory.MutationDecision{
			Category:          "architecture",
			Tier:              "rule",
			Cardinality:       "additive",
			IsPermanent:       0.9,
			IsGlobalInvariant: 0.2,
			Action:            "INSERT",
			Confidence:        0.95,
		},
	}
	svc, err := memory.NewService(":memory:", eval)
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	defer svc.Close()

	repo := t.TempDir()
	scope := "project:" + strings.ToLower(filepath.Base(repo))

	// Commit 2 rules (2 mutation telemetry samples)
	if _, err := svc.CommitMemory(ctx, "Always use WAL mode for SQLite databases", "architecture", scope); err != nil {
		t.Fatalf("CommitMemory 1: %v", err)
	}
	if _, err := svc.CommitMemory(ctx, "Run gofmt on all modified Go files", "architecture", scope); err != nil {
		t.Fatalf("CommitMemory 2: %v", err)
	}

	// QuerySessionMemory (1 relevance telemetry sample evaluating 2 facts)
	if _, err := svc.QuerySessionMemory(ctx, "", "sqlite database setup", "", scope, false); err != nil {
		t.Fatalf("QuerySessionMemory: %v", err)
	}

	// VerifyPlan (1 verify telemetry sample + 1 guardrail_log entry with ElapsedMs > 0)
	if _, err := svc.VerifyPlan(ctx, "Enable WAL mode for sqlite", scope); err != nil {
		t.Fatalf("VerifyPlan: %v", err)
	}

	// Context expiring DURING VerifyPlan evaluation must still persist timeout telemetry via context.WithoutCancel
	eval.sleep = 25 * time.Millisecond
	eval.verifyErr = context.DeadlineExceeded
	expCtx, expCancel := context.WithTimeout(ctx, 10*time.Millisecond)
	defer expCancel()
	if _, _, _, vErr := svc.EvaluatePlanViolations(expCtx, "Skip WAL mode", scope, 0); vErr == nil {
		t.Fatalf("expected EvaluatePlanViolations error on expired context")
	}
	eval.sleep = 3 * time.Millisecond
	eval.verifyErr = nil

	// Non-timeout mutation error records error_count
	eval.mutateErr = errors.New("upstream 500")
	if _, mErr := svc.CommitMemory(ctx, "Broken remote rule", "architecture", scope); mErr == nil {
		t.Fatalf("expected CommitMemory error when mutateErr set")
	}
	eval.mutateErr = nil

	rep, err := svc.SummarizeEvalTelemetry(ctx, 24)
	if err != nil {
		t.Fatalf("SummarizeEvalTelemetry: %v", err)
	}
	if rep.TotalCalls != 6 || rep.TotalTimeouts != 1 || rep.TotalErrors != 1 {
		t.Fatalf("expected TotalCalls=6 TotalTimeouts=1 TotalErrors=1, got calls=%d to=%d err=%d (%+v)",
			rep.TotalCalls, rep.TotalTimeouts, rep.TotalErrors, rep.ByTask)
	}
	if rep.TotalElapsedMs <= 0 || rep.AvgElapsedMs <= 0 || rep.P95ElapsedMs <= 0 {
		t.Fatalf("expected positive total/avg/p95 elapsed ms, got total=%v avg=%v p95=%v",
			rep.TotalElapsedMs, rep.AvgElapsedMs, rep.P95ElapsedMs)
	}
	if len(rep.Engines) != 1 || rep.Engines[0].Engine != "gemini:gemini-3.5-flash-lite" || rep.Engines[0].Calls != 6 || rep.Engines[0].TimeoutCount != 1 || rep.Engines[0].ErrorCount != 1 {
		t.Fatalf("unexpected Engines aggregation: %+v", rep.Engines)
	}
	if len(rep.RecentSamples) != 6 || rep.RecentSamples[len(rep.RecentSamples)-1].Status != "error" {
		t.Fatalf("expected 6 chronological RecentSamples ending with error, got %+v", rep.RecentSamples)
	}
	mut := rep.ByTask[string(memory.TaskMutation)]
	if mut.Calls != 3 || mut.OKCount != 2 || mut.ErrorCount != 1 || mut.LastEngine != "gemini:gemini-3.5-flash-lite" {
		t.Fatalf("unexpected mutation telemetry stats: %+v", mut)
	}
	rel := rep.ByTask[string(memory.TaskRelevance)]
	if rel.Calls != 1 || rel.OKCount != 1 || rel.AvgItems != 2.0 || rel.AvgElapsedMs <= 0 {
		t.Fatalf("unexpected relevance telemetry stats: %+v", rel)
	}
	ver := rep.ByTask[string(memory.TaskVerify)]
	if ver.Calls != 2 || ver.OKCount != 1 || ver.TimeoutCount != 1 || ver.P95ElapsedMs <= 0 {
		t.Fatalf("unexpected verify telemetry stats: %+v", ver)
	}

	// Verify guardrail_logs captured positive ElapsedMs
	page, err := svc.QueryGuardrailLogsPage(ctx, memory.GuardrailLogQuery{Scope: scope, PageSize: 10})
	if err != nil {
		t.Fatalf("QueryGuardrailLogsPage: %v", err)
	}
	if len(page.Records) == 0 || page.Records[0].ElapsedMs <= 0 {
		t.Fatalf("expected guardrail_log ElapsedMs > 0, got %+v", page.Records)
	}

	// Verify in-memory LiveSession accumulates hook calls & elapsed time across start -> turn -> pretool
	sid := "sess-tel-1"
	startJSON := fmt.Sprintf(`{"conversationId":%q,"workspacePaths":[%q]}`, sid, repo)
	if _, code, err := svc.ExecuteHook(ctx, "antigravity-start", []byte(startJSON), repo); err != nil || code != 0 {
		t.Fatalf("antigravity-start failed: code=%d err=%v", code, err)
	}
	turnJSON := fmt.Sprintf(`{"conversationId":%q,"workspacePaths":[%q],"lastUserInput":"sqlite migration"}`, sid, repo)
	if _, code, err := svc.ExecuteHook(ctx, "antigravity-turn", []byte(turnJSON), repo); err != nil || code != 0 {
		t.Fatalf("antigravity-turn failed: code=%d err=%v", code, err)
	}
	pretoolJSON := fmt.Sprintf(`{"conversationId":%q,"workspacePaths":[%q],"toolCall":{"name":"edit_file","args":{"Path":%q,"Replacement":"WAL"}}}`, sid, repo, filepath.Join(repo, "db.go"))
	if _, code, err := svc.ExecuteHook(ctx, "antigravity-pretool", []byte(pretoolJSON), repo); err != nil || code != 0 {
		t.Fatalf("antigravity-pretool failed: code=%d err=%v", code, err)
	}

	live := svc.ListLiveSessions()
	if len(live) != 1 || live[0].HookCalls != 3 || live[0].HookElapsedMs <= 0 || live[0].Tool != "edit_file" || live[0].Target != "db.go" {
		t.Fatalf("expected 3 accumulated hook calls and positive elapsed ms on live session, got %+v", live)
	}

	// Verify ResetEvalTelemetry zeroes summary cleanly
	if err := svc.ResetEvalTelemetry(ctx); err != nil {
		t.Fatalf("ResetEvalTelemetry: %v", err)
	}
	afterReset, err := svc.SummarizeEvalTelemetry(ctx, 24)
	if err != nil {
		t.Fatalf("SummarizeEvalTelemetry after reset: %v", err)
	}
	if afterReset.TotalCalls != 0 {
		t.Fatalf("expected TotalCalls=0 after reset, got %d", afterReset.TotalCalls)
	}
}
