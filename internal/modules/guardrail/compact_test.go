package guardrail_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"memorize/internal/domain"
)

type compactSpyEval struct {
	lastQueryTask      string
	lastCandidateCount int
	queryCalls         int
}

func (e *compactSpyEval) Name() string { return "gemini:compact-spy" }

func (e *compactSpyEval) ScoreRelevance(_ context.Context, currentTask string, candidates []domain.Fact) ([]domain.ScoredFact, error) {
	e.queryCalls++
	e.lastQueryTask = currentTask
	e.lastCandidateCount = len(candidates)
	out := make([]domain.ScoredFact, 0, len(candidates))
	lower := strings.ToLower(currentTask)
	for _, c := range candidates {
		score := 0.2
		if strings.Contains(lower, "sqlite") && strings.Contains(strings.ToLower(c.Content), "sqlite") {
			score = 9.0
		}
		out = append(out, domain.ScoredFact{Fact: c, Score: score, Confidence: 0.95})
	}
	return out, nil
}

func (e *compactSpyEval) EvaluateMutation(_ context.Context, _, _ string, _ []domain.Fact) (domain.MutationDecision, error) {
	return domain.MutationDecision{
		Category:    "architecture",
		Tier:        "rule",
		Cardinality: "additive",
		IsPermanent: 0.95,
		Action:      "INSERT",
		Confidence:  0.95,
	}, nil
}

func (e *compactSpyEval) VerifyPlan(_ context.Context, _ string, _ []domain.Fact) ([]domain.PlanViolation, error) {
	return nil, nil
}

func TestClaudePostCompactAndSessionStartCompactAutoRestoresRules(t *testing.T) {
	ctx := context.Background()
	eval := &compactSpyEval{}
	st := newTestStack(t, ":memory:", eval)

	repo := t.TempDir()
	scope := "project:" + strings.ToLower(filepath.Base(repo))
	if _, err := st.mem.CommitMemory(ctx, "Always enable SQLite WAL mode for concurrent reads", "architecture", scope); err != nil {
		t.Fatalf("CommitMemory 1: %v", err)
	}
	if _, err := st.mem.CommitMemory(ctx, "Format SQL migrations with uppercase keywords", "architecture", scope); err != nil {
		t.Fatalf("CommitMemory 2: %v", err)
	}

	sid := "claude-compact-sess"
	turn1Payload, _ := json.Marshal(map[string]any{
		"session_id": sid,
		"cwd":        repo,
		"prompt":     "Configure SQLite persistence",
	})
	out1, code, err := st.gr.ExecuteHook(ctx, "claude-turn", turn1Payload, repo)
	if err != nil || code != 0 || !strings.Contains(out1, "SQLite WAL mode") {
		t.Fatalf("turn 1 should inject SQLite rule: code=%d err=%v out=%q", code, err, out1)
	}
	if eval.lastCandidateCount != 2 {
		t.Fatalf("expected 2 candidates on turn 1, got %d", eval.lastCandidateCount)
	}

	turn2Payload, _ := json.Marshal(map[string]any{
		"session_id": sid,
		"cwd":        repo,
		"prompt":     "Check SQLite WAL setting again",
	})
	out2, code, err := st.gr.ExecuteHook(ctx, "claude-turn", turn2Payload, repo)
	if err != nil || code != 0 || out2 != "" {
		t.Fatalf("repeat turn 2 should return empty delta: code=%d err=%v out=%q", code, err, out2)
	}
	if eval.lastCandidateCount != 1 {
		t.Fatalf("expected delivered SQLite rule to be pre-filtered leaving 1 unsent candidate, got %d", eval.lastCandidateCount)
	}

	compactPayload, _ := json.Marshal(map[string]any{
		"session_id":      sid,
		"cwd":             repo,
		"hook_event_name": "PostCompact",
		"compact_summary": "Refactored SQLite storage layer and connection pool.",
	})
	if outC, codeC, errC := st.gr.ExecuteHook(ctx, "claude-compact", compactPayload, repo); errC != nil || codeC != 0 || outC != "" {
		t.Fatalf("claude-compact failed: code=%d err=%v out=%q", codeC, errC, outC)
	}

	startCompactPayload, _ := json.Marshal(map[string]any{
		"session_id":      sid,
		"cwd":             repo,
		"hook_event_name": "SessionStart",
		"source":          "compact",
	})
	outRestore, codeR, errR := st.gr.ExecuteHook(ctx, "claude-start", startCompactPayload, repo)
	if errR != nil || codeR != 0 {
		t.Fatalf("claude-start after compact failed: code=%d err=%v", codeR, errR)
	}
	if eval.lastCandidateCount != 2 {
		t.Fatalf("expected PostCompact reset to re-evaluate all 2 candidates, got %d", eval.lastCandidateCount)
	}
	if !strings.Contains(outRestore, "Post-Compact Restored") || !strings.Contains(outRestore, "SQLite WAL mode") {
		t.Fatalf("expected Post-Compact Restored output with SQLite WAL rule, got:\n%s", outRestore)
	}
	if !strings.Contains(eval.lastQueryTask, "Refactored SQLite storage layer") {
		t.Fatalf("expected ScoreRelevance to receive PostCompact summary, got %q", eval.lastQueryTask)
	}
}

func TestAntigravityTranscriptCheckpointTriggersAutoPostCompactRecovery(t *testing.T) {
	ctx := context.Background()
	eval := &compactSpyEval{}
	st := newTestStack(t, ":memory:", eval)

	repo := t.TempDir()
	scope := "project:" + strings.ToLower(filepath.Base(repo))
	if _, err := st.mem.CommitMemory(ctx, "Always enable SQLite WAL mode for concurrent reads", "architecture", scope); err != nil {
		t.Fatalf("CommitMemory: %v", err)
	}

	transcriptPath := filepath.Join(t.TempDir(), "transcript.jsonl")
	initialLine := `{"step_index":0,"source":"USER","type":"USER_INPUT","status":"DONE","content":"hello"}` + "\n"
	if err := os.WriteFile(transcriptPath, []byte(initialLine), 0o600); err != nil {
		t.Fatal(err)
	}

	sid := "agy-compact-sess"
	startPayload, _ := json.Marshal(map[string]any{
		"conversationId": sid,
		"workspacePaths": []string{repo},
		"transcriptPath": transcriptPath,
	})
	if _, code, err := st.gr.ExecuteHook(ctx, "antigravity-start", startPayload, repo); err != nil || code != 0 {
		t.Fatalf("antigravity-start failed: code=%d err=%v", code, err)
	}

	turn1, _ := json.Marshal(map[string]any{
		"conversationId": sid,
		"workspacePaths": []string{repo},
		"transcriptPath": transcriptPath,
		"lastUserInput":  "Inspect sqlite layer",
	})
	out1, code1, err1 := st.gr.ExecuteHook(ctx, "antigravity-turn", turn1, repo)
	if err1 != nil || code1 != 0 || !strings.Contains(out1, "SQLite WAL mode") {
		t.Fatalf("expected SQLite rule on turn 1: code=%d err=%v out=%s", code1, err1, out1)
	}

	f, err := os.OpenFile(transcriptPath, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = f.WriteString(`{"step_index":1,"source":"MODEL","type":"PLANNER_RESPONSE","status":"DONE","content":"working"}` + "\n")
	_ = f.Close()

	turn2, _ := json.Marshal(map[string]any{
		"conversationId": sid,
		"workspacePaths": []string{repo},
		"transcriptPath": transcriptPath,
		"lastUserInput":  "Continue implementation",
	})
	out2, _, err2 := st.gr.ExecuteHook(ctx, "antigravity-turn", turn2, repo)
	if err2 != nil || out2 != "{}" {
		t.Fatalf("expected '{}' before compaction, got %q (err=%v)", out2, err2)
	}

	ckptLine, _ := json.Marshal(map[string]any{
		"step_index": 12,
		"source":     "SYSTEM",
		"type":       "CHECKPOINT",
		"status":     "DONE",
		"created_at": "2026-11-14T00:15:00Z",
		"content":    "# Resuming from a compaction\n\nYou are continuing work.\n\n# Task Overview\nRefactoring sqlite journal storage and connection setup.",
	})
	f2, err := os.OpenFile(transcriptPath, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = fmt.Fprintln(f2, string(ckptLine))
	_ = f2.Close()

	turn3, _ := json.Marshal(map[string]any{
		"conversationId": sid,
		"workspacePaths": []string{repo},
		"transcriptPath": transcriptPath,
		"lastUserInput":  "Proceed to step 2",
	})
	out3, code3, err3 := st.gr.ExecuteHook(ctx, "antigravity-turn", turn3, repo)
	if err3 != nil || code3 != 0 {
		t.Fatalf("post-checkpoint turn failed: code=%d err=%v", code3, err3)
	}
	if !strings.Contains(out3, "Post-Compact Restored") || !strings.Contains(out3, "SQLite WAL mode") {
		t.Fatalf("expected automatic post-compact restoration in antigravity-turn, got:\n%s", out3)
	}
	if !strings.Contains(eval.lastQueryTask, "Refactoring sqlite journal storage") || !strings.Contains(eval.lastQueryTask, "Proceed to step 2") {
		t.Fatalf("expected query task to combine summary + prompt, got %q", eval.lastQueryTask)
	}

	turn4, _ := json.Marshal(map[string]any{
		"conversationId": sid,
		"workspacePaths": []string{repo},
		"transcriptPath": transcriptPath,
		"lastUserInput":  "Run checks",
	})
	out4, _, err4 := st.gr.ExecuteHook(ctx, "antigravity-turn", turn4, repo)
	if err4 != nil || out4 != "{}" {
		t.Fatalf("expected normal empty delta on subsequent turn after recovery, got %q (err=%v)", out4, err4)
	}
}
