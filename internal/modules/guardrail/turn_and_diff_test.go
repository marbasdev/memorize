package guardrail

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"memorize/internal/domain"
)

func TestDecodeHookEventAndCodecs(t *testing.T) {
	ev := decodeHookEvent("antigravity-pretool", []byte(`{
		"conversationId": "conv-123",
		"workspacePaths": ["/tmp/ws1"],
		"lastUserInput": "always use strict types",
		"toolCall": {"name": "run_command", "args": {"CommandLine": "rm -rf build"}}
	}`))
	if !ev.Valid || ev.Agent != "antigravity" || ev.Phase != domain.HookPhasePreTool {
		t.Fatalf("unexpected parsed meta: %+v", ev)
	}
	if ev.SessionID != "conv-123" || ev.ToolName != "run_command" || ev.TurnPrompt != "always use strict types" {
		t.Fatalf("unexpected normalized fields: %+v", ev)
	}

	out, code, _ := codecForAgent("claude").FormatDenyPreTool("blocked")
	if code != 2 {
		t.Fatalf("expected claude pretool deny exit code 2, got %d", code)
	}
	var payload map[string]string
	if json.Unmarshal([]byte(out), &payload) != nil || payload["decision"] != "block" {
		t.Fatalf("unexpected claude deny body: %s", out)
	}

	outA, codeA, _ := codecForAgent("antigravity").FormatDenyPreTool("blocked")
	if codeA != 0 {
		t.Fatalf("expected antigravity pretool deny exit code 0, got %d", codeA)
	}
	if json.Unmarshal([]byte(outA), &payload) != nil || payload["decision"] != "deny" {
		t.Fatalf("unexpected antigravity deny body: %s", outA)
	}
}

type compactSpyEval struct {
	stubEval
	lastQueryTask      string
	lastCandidateCount int
	queryCalls         int
}

func newCompactSpyEval() *compactSpyEval {
	return &compactSpyEval{
		stubEval: stubEval{
			dec: domain.MutationDecision{
				Category:    "architecture",
				Tier:        "rule",
				Cardinality: "additive",
				IsPermanent: 0.95,
				Action:      "INSERT",
				Confidence:  0.95,
			},
		},
	}
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

func TestClaudePostCompactAndSessionStartCompactAutoRestoresRules(t *testing.T) {
	ctx := context.Background()
	eval := newCompactSpyEval()
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
	eval := newCompactSpyEval()
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

type contextAwareVerifyEval struct {
	stubEval
	ctxLimit      int
	ruleBatchSize int
	capturedPlans []string
	violateSubstr string
	delayPerCall  time.Duration
}

func (c *contextAwareVerifyEval) InputTokenLimit(_ context.Context) int { return c.ctxLimit }
func (c *contextAwareVerifyEval) RuleBatchSize() int                    { return c.ruleBatchSize }

func (c *contextAwareVerifyEval) VerifyPlan(ctx context.Context, plan string, rules []domain.Fact) ([]domain.PlanViolation, error) {
	c.capturedPlans = append(c.capturedPlans, plan)
	if c.delayPerCall > 0 {
		select {
		case <-time.After(c.delayPerCall):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if c.violateSubstr != "" && strings.Contains(plan, c.violateSubstr) && len(rules) > 0 {
		return []domain.PlanViolation{{Fact: rules[0], Probability: 0.95, Confidence: 0.93}}, nil
	}
	return nil, nil
}

func TestStopBatchChunksLargeMultiFileDiffWithoutTruncation(t *testing.T) {
	ctx := context.Background()
	eval := &contextAwareVerifyEval{
		violateSubstr: "TAIL_VIOLATION_MARKER",
		stubEval: stubEval{
			dec: domain.MutationDecision{
				Category:    "convention",
				Tier:        "rule",
				IsPermanent: 0.95,
				Action:      "INSERT",
				Confidence:  0.95,
			},
		},
	}
	st := newTestStack(t, ":memory:", eval)

	repo := t.TempDir()
	scope := "project:" + strings.ToLower(filepath.Base(repo))
	if _, err := st.mem.CommitMemory(ctx, "Never emit TAIL_VIOLATION_MARKER in source files", "convention", scope); err != nil {
		t.Fatal(err)
	}
	eval.dec = domain.MutationDecision{Action: "IGNORE"}

	files := []struct {
		name string
		body string
	}{
		{"a_first.go", "package app\n" + strings.Repeat("func f1(x int) int { return x + 1 }\n", 60)},
		{"b_mid.go", "package app\n" + strings.Repeat("func f2(x int) int { return x + 2 }\n", 60)},
		{"z_last.go", "package app\n" + strings.Repeat("func f3(x int) int { return x + 3 }\n", 55) + "func bad() { _ = \"TAIL_VIOLATION_MARKER\" }\n"},
	}

	for _, f := range files {
		p := filepath.Join(repo, f.name)
		if err := os.WriteFile(p, []byte("package app\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		pre, _ := json.Marshal(map[string]any{
			"session_id": "sess-chunk-multi",
			"cwd":        repo,
			"tool_name":  "Write",
			"tool_input": map[string]any{"file_path": p, "content": f.body},
		})
		if out, code, err := st.gr.ExecuteHook(ctx, "claude-pretool", pre, repo); err != nil || code != 0 || out != "{}" {
			t.Fatalf("pretool write %s failed: code=%d out=%q err=%v", f.name, code, out, err)
		}
		if err := os.WriteFile(p, []byte(f.body), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	stopPayload, _ := json.Marshal(map[string]any{"session_id": "sess-chunk-multi", "cwd": repo})
	stopOut, stopCode, err := st.gr.ExecuteHook(ctx, "claude-stop", stopPayload, repo)
	if err != nil || stopCode != 0 || !strings.Contains(stopOut, `"decision":"block"`) || !strings.Contains(stopOut, "TAIL_VIOLATION_MARKER") {
		t.Fatalf("expected chunked Stop to catch violation in z_last.go beyond 4KB, got code=%d out=%q err=%v", stopCode, stopOut, err)
	}
	if len(eval.capturedPlans) < 2 {
		t.Fatalf("expected multi-file >4KB diff to be split across >=2 chunks, got %d", len(eval.capturedPlans))
	}
	joined := strings.Join(eval.capturedPlans, "\n")
	for _, f := range files {
		if !strings.Contains(joined, "File: "+f.name) {
			t.Fatalf("expected %s to appear in evaluated chunks, got:\n%s", f.name, joined)
		}
	}
}

func TestStopBatchSplitsOversizedFileAndCompactsWhitespaceNoise(t *testing.T) {
	ctx := context.Background()
	eval := &contextAwareVerifyEval{
		violateSubstr: "OVERSIZED_BOTTOM_RULE_BREAK",
		stubEval: stubEval{
			dec: domain.MutationDecision{
				Category:    "architecture",
				Tier:        "rule",
				IsPermanent: 0.95,
				Action:      "INSERT",
				Confidence:  0.95,
			},
		},
	}
	st := newTestStack(t, ":memory:", eval)

	repo := t.TempDir()
	scope := "project:" + strings.ToLower(filepath.Base(repo))
	if _, err := st.mem.CommitMemory(ctx, "Never include OVERSIZED_BOTTOM_RULE_BREAK", "architecture", scope); err != nil {
		t.Fatal(err)
	}
	eval.dec = domain.MutationDecision{Action: "IGNORE"}

	wsPath := filepath.Join(repo, "ws_only.go")
	wsBefore := "package app\n" + strings.Repeat("func keep(a int) int { return a }\n", 120)
	wsAfter := "package app\n\n" + strings.Repeat("  func keep(a int) int { return a }\n\n", 120)
	_ = os.WriteFile(wsPath, []byte(wsBefore), 0o600)

	hugePath := filepath.Join(repo, "huge.go")
	_ = os.WriteFile(hugePath, []byte("package app\n"), 0o600)

	for _, p := range []string{wsPath, hugePath} {
		pre, _ := json.Marshal(map[string]any{
			"session_id": "sess-oversized-ws",
			"cwd":        repo,
			"tool_name":  "Edit",
			"tool_input": map[string]any{"file_path": p},
		})
		_, _, _ = st.gr.ExecuteHook(ctx, "claude-pretool", pre, repo)
	}

	var hugeLines strings.Builder
	hugeLines.WriteString("package app\n")
	for i := 0; i < 150; i++ {
		fmt.Fprintf(&hugeLines, "func step_%03d(v int) int { return v + %d }\n", i, i)
	}
	hugeLines.WriteString("func tail() { _ = \"OVERSIZED_BOTTOM_RULE_BREAK\" }\n")
	_ = os.WriteFile(wsPath, []byte(wsAfter), 0o600)
	_ = os.WriteFile(hugePath, []byte(hugeLines.String()), 0o600)

	stopPayload, _ := json.Marshal(map[string]any{"session_id": "sess-oversized-ws", "cwd": repo})
	stopOut, _, err := st.gr.ExecuteHook(ctx, "claude-stop", stopPayload, repo)
	if err != nil || !strings.Contains(stopOut, `"decision":"block"`) {
		t.Fatalf("expected oversized single-file violation at bottom to be caught, got out=%q err=%v", stopOut, err)
	}
	joined := strings.Join(eval.capturedPlans, "\n")
	if !strings.Contains(joined, "File: ws_only.go (whitespace/formatting only)") {
		t.Fatalf("expected whitespace-only file to collapse into 1 summary line, got:\n%s", joined)
	}
	if strings.Contains(joined, "+  func keep(a int)") {
		t.Fatalf("expected pure whitespace diff lines not to inflate prompt chunks")
	}
	if !strings.Contains(joined, "File: huge.go (part 1/") || !strings.Contains(joined, "OVERSIZED_BOTTOM_RULE_BREAK") {
		t.Fatalf("expected huge.go to split cleanly into numbered parts preserving tail, got:\n%s", joined)
	}
}

func TestDynamicDiffBudgetScalesWithKnownContextLimit(t *testing.T) {
	ctx := context.Background()

	runWithLimit := func(ctxLimit int) int {
		eval := &contextAwareVerifyEval{
			ctxLimit:      ctxLimit,
			ruleBatchSize: 20,
			stubEval: stubEval{
				dec: domain.MutationDecision{
					Category:    "convention",
					Tier:        "rule",
					IsPermanent: 0.95,
					Action:      "INSERT",
					Confidence:  0.95,
				},
			},
		}
		st := newTestStack(t, ":memory:", eval)
		repo := t.TempDir()
		scope := "project:" + strings.ToLower(filepath.Base(repo))
		_, _ = st.mem.CommitMemory(ctx, "Keep modules concise", "convention", scope)
		eval.dec = domain.MutationDecision{Action: "IGNORE"}

		p := filepath.Join(repo, "mod.go")
		_ = os.WriteFile(p, []byte("package mod\n"), 0o600)
		pre, _ := json.Marshal(map[string]any{
			"session_id": "sess-budget",
			"cwd":        repo,
			"tool_name":  "Edit",
			"tool_input": map[string]any{"file_path": p},
		})
		_, _, _ = st.gr.ExecuteHook(ctx, "claude-pretool", pre, repo)

		var body strings.Builder
		body.WriteString("package mod\n")
		for i := 0; i < 220; i++ {
			fmt.Fprintf(&body, "func fn_%03d(x int) int { return x + %d }\n", i, i)
		}
		_ = os.WriteFile(p, []byte(body.String()), 0o600)

		stopPayload, _ := json.Marshal(map[string]any{"session_id": "sess-budget", "cwd": repo})
		_, _, _ = st.gr.ExecuteHook(ctx, "claude-stop", stopPayload, repo)
		return len(eval.capturedPlans)
	}

	unknownChunks := runWithLimit(0)
	largeChunks := runWithLimit(262144)
	if unknownChunks < 3 {
		t.Fatalf("expected unknown context limit to split 8KB diff into >=3 safe chunks, got %d", unknownChunks)
	}
	if largeChunks != 1 {
		t.Fatalf("expected 262K context model to evaluate 8KB diff in 1 chunk, got %d", largeChunks)
	}
}

func TestStopDeadlinePartialVerificationFailOpenNoticeAndFailClosed(t *testing.T) {
	setupAndRun := func(t *testing.T, failClosed bool) (string, *testStack, string) {
		t.Helper()
		if failClosed {
			t.Setenv("MEMORY_GUARDRAIL_FAIL_CLOSED", "1")
		} else {
			t.Setenv("MEMORY_GUARDRAIL_FAIL_CLOSED", "0")
		}

		eval := &contextAwareVerifyEval{
			delayPerCall: 90 * time.Millisecond,
			stubEval: stubEval{
				dec: domain.MutationDecision{
					Category:    "convention",
					Tier:        "rule",
					IsPermanent: 0.95,
					Action:      "INSERT",
					Confidence:  0.95,
				},
			},
		}
		st := newTestStack(t, ":memory:", eval)
		repo := t.TempDir()
		scope := "project:" + strings.ToLower(filepath.Base(repo))
		_, _ = st.mem.CommitMemory(context.Background(), "Keep functions short", "convention", scope)
		eval.dec = domain.MutationDecision{Action: "IGNORE"}

		for i, name := range []string{"first.go", "second.go"} {
			p := filepath.Join(repo, name)
			_ = os.WriteFile(p, []byte("package app\n"), 0o600)
			pre, _ := json.Marshal(map[string]any{
				"session_id": "sess-partial",
				"cwd":        repo,
				"tool_name":  "Edit",
				"tool_input": map[string]any{"file_path": p},
			})
			_, _, _ = st.gr.ExecuteHook(context.Background(), "claude-pretool", pre, repo)

			var b strings.Builder
			b.WriteString("package app\n")
			for j := 0; j < 110; j++ {
				fmt.Fprintf(&b, "func p%d_%03d() int { return %d }\n", i, j, j)
			}
			_ = os.WriteFile(p, []byte(b.String()), 0o600)
		}

		stopCtx, cancel := context.WithTimeout(context.Background(), 1560*time.Millisecond)
		defer cancel()
		stopPayload, _ := json.Marshal(map[string]any{"session_id": "sess-partial", "cwd": repo})
		out, _, err := st.gr.ExecuteHook(stopCtx, "claude-stop", stopPayload, repo)
		if err != nil {
			t.Fatalf("ExecuteHook stop err=%v", err)
		}
		return out, st, repo
	}

	outOpen, stOpen, repoOpen := setupAndRun(t, false)
	if outOpen != "" {
		t.Fatalf("expected fail-open partial Stop to yield cleanly, got %q", outOpen)
	}
	scopeOpen := "project:" + strings.ToLower(filepath.Base(repoOpen))
	pageOpen, err := stOpen.gr.QueryGuardrailLogsPage(context.Background(), domain.GuardrailLogQuery{Scope: scopeOpen, Source: "stop", Page: 1, PageSize: 10})
	if err != nil || pageOpen.TotalCount != 1 || pageOpen.Records[0].Verdict != domain.GuardrailVerdictPartial || pageOpen.Records[0].Blocked {
		t.Fatalf("expected unblocked partial guardrail log, got err=%v page=%+v", err, pageOpen)
	}
	if !strings.Contains(pageOpen.Records[0].Details, "second.go") {
		t.Fatalf("expected partial log details to list unverified second.go, got %q", pageOpen.Records[0].Details)
	}

	turnNext, _ := json.Marshal(map[string]any{"session_id": "sess-partial", "cwd": repoOpen, "prompt": "continue"})
	turnOut1, _, _ := stOpen.gr.ExecuteHook(context.Background(), "claude-turn", turnNext, repoOpen)
	if !strings.Contains(turnOut1, "[MEMORIZE GUARDRAIL NOTICE]") || !strings.Contains(turnOut1, "second.go") {
		t.Fatalf("expected next turn to receive one-shot partial verification notice, got %q", turnOut1)
	}
	turnOut2, _, _ := stOpen.gr.ExecuteHook(context.Background(), "claude-turn", turnNext, repoOpen)
	if strings.Contains(turnOut2, "[MEMORIZE GUARDRAIL NOTICE]") {
		t.Fatalf("expected partial notice to clear after first delivery, got %q", turnOut2)
	}

	outClosed, stClosed, repoClosed := setupAndRun(t, true)
	if !strings.Contains(outClosed, `"decision":"block"`) || !strings.Contains(outClosed, "second.go") {
		t.Fatalf("expected fail-closed partial Stop to block listing second.go, got %q", outClosed)
	}
	scopeClosed := "project:" + strings.ToLower(filepath.Base(repoClosed))
	pageClosed, err := stClosed.gr.QueryGuardrailLogsPage(context.Background(), domain.GuardrailLogQuery{Scope: scopeClosed, Source: "stop", Page: 1, PageSize: 10})
	if err != nil || pageClosed.TotalCount != 1 || pageClosed.Records[0].Verdict != domain.GuardrailVerdictPartial || !pageClosed.Records[0].Blocked {
		t.Fatalf("expected blocked partial guardrail log under fail-closed, got err=%v page=%+v", err, pageClosed)
	}
}

func TestGitPreCommitEvaluatesAllStagedFilesWithout2KBCap(t *testing.T) {
	ctx := context.Background()
	eval := &contextAwareVerifyEval{
		violateSubstr: "PRECOMMIT_TAIL_FORBIDDEN",
		stubEval: stubEval{
			dec: domain.MutationDecision{
				Category:    "security",
				Tier:        "rule",
				IsPermanent: 0.95,
				Action:      "INSERT",
				Confidence:  0.95,
			},
		},
	}
	st := newTestStack(t, ":memory:", eval)

	repo := t.TempDir()
	runGit := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", repo}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v failed: %v\n%s", args, err, string(out))
		}
	}
	runGit("init")
	runGit("config", "user.name", "Test")
	runGit("config", "user.email", "test@example.com")
	_ = os.WriteFile(filepath.Join(repo, "README.md"), []byte("init\n"), 0o600)
	runGit("add", "README.md")
	runGit("commit", "-m", "init")

	scope := "project:" + strings.ToLower(filepath.Base(repo))
	if _, err := st.mem.CommitMemory(ctx, "Never commit PRECOMMIT_TAIL_FORBIDDEN", "security", scope); err != nil {
		t.Fatal(err)
	}
	eval.dec = domain.MutationDecision{Action: "IGNORE"}

	_ = os.WriteFile(filepath.Join(repo, "a_head.go"), []byte("package main\n"+strings.Repeat("func a() int { return 1 }\n", 80)), 0o600)
	_ = os.WriteFile(filepath.Join(repo, "b_mid.go"), []byte("package main\n"+strings.Repeat("func b() int { return 2 }\n", 80)), 0o600)
	_ = os.WriteFile(filepath.Join(repo, "z_tail.go"), []byte("package main\nfunc bad() { _ = \"PRECOMMIT_TAIL_FORBIDDEN\" }\n"), 0o600)
	runGit("add", "a_head.go", "b_mid.go", "z_tail.go")

	out, code, err := st.gr.ExecuteHook(ctx, "git-precommit", []byte("{}"), repo)
	if err != nil || code != 1 || !strings.Contains(out, "PRECOMMIT_TAIL_FORBIDDEN") {
		t.Fatalf("expected git-precommit to evaluate all staged chunks past 2KB and block z_tail.go, got code=%d out=%q err=%v", code, out, err)
	}
	if len(eval.capturedPlans) < 2 {
		t.Fatalf("expected staged diff >4KB to be chunked across >=2 VerifyPlan calls, got %d", len(eval.capturedPlans))
	}
}

type batchVerifyEval struct {
	stubEval
	capturedPlans []string
	violate       bool
}

func (b *batchVerifyEval) VerifyPlan(_ context.Context, plan string, rules []domain.Fact) ([]domain.PlanViolation, error) {
	b.capturedPlans = append(b.capturedPlans, plan)
	if b.violate && len(rules) > 0 {
		return []domain.PlanViolation{
			{Fact: rules[0], Probability: 0.94, Confidence: 0.92},
		}, nil
	}
	return nil, nil
}

func TestStopBatchEvaluatesMultipleFilesOnceAndIsolatesPreExistingDirtyLines(t *testing.T) {
	ctx := context.Background()
	eval := &batchVerifyEval{
		stubEval: stubEval{
			dec: domain.MutationDecision{
				Category:    "architecture",
				Tier:        "rule",
				IsPermanent: 0.95,
				Action:      "INSERT",
				Confidence:  0.95,
			},
		},
	}
	st := newTestStack(t, ":memory:", eval)

	repo := t.TempDir()
	scope := "project:" + strings.ToLower(filepath.Base(repo))
	if _, err := st.mem.CommitMemory(ctx, "Never call panic() in production handlers", "architecture", scope); err != nil {
		t.Fatal(err)
	}
	eval.dec = domain.MutationDecision{Action: "IGNORE"}

	fileA := filepath.Join(repo, "a.go")
	fileB := filepath.Join(repo, "b.go")
	preExistingA := "package main\nfunc oldLegacy() { panic(\"preexisting-before-turn\") }\nfunc keepMiddle() {}\nfunc okFooter() {}\n"
	if err := os.WriteFile(fileA, []byte(preExistingA), 0o600); err != nil {
		t.Fatal(err)
	}

	turnPayload, _ := json.Marshal(map[string]any{
		"session_id": "claude-batch-1",
		"cwd":        repo,
		"prompt":     "Update footer in a.go and create b.go",
	})
	_, _, _ = st.gr.ExecuteHook(ctx, "claude-turn", turnPayload, repo)

	for _, editArgs := range []struct {
		tool  string
		input map[string]any
	}{
		{"Edit", map[string]any{"file_path": fileA, "old_string": "okFooter() {}", "new_string": "okFooter() { panic(\"temp\") }"}},
		{"Edit", map[string]any{"file_path": fileA, "old_string": "okFooter() { panic(\"temp\") }", "new_string": "okFooter() { return }"}},
		{"Write", map[string]any{"file_path": fileB, "content": "package main\nfunc b() {}\n"}},
	} {
		payload, _ := json.Marshal(map[string]any{
			"session_id": "claude-batch-1",
			"cwd":        repo,
			"tool_name":  editArgs.tool,
			"tool_input": editArgs.input,
		})
		out, code, err := st.gr.ExecuteHook(ctx, "claude-pretool", payload, repo)
		if err != nil || code != 0 || out != "{}" {
			t.Fatalf("expected file edit PreToolUse to allow immediately, got code=%d out=%q err=%v", code, out, err)
		}
	}

	if len(eval.capturedPlans) != 0 {
		t.Fatalf("expected 0 VerifyPlan calls during PreToolUse file edits, got %d", len(eval.capturedPlans))
	}

	_ = os.WriteFile(fileA, []byte("package main\nfunc oldLegacy() { panic(\"preexisting-before-turn\") }\nfunc keepMiddle() {}\nfunc okFooter() { return }\n"), 0o600)
	_ = os.WriteFile(fileB, []byte("package main\nfunc b() {}\n"), 0o600)

	stopPayload, _ := json.Marshal(map[string]any{
		"session_id":       "claude-batch-1",
		"cwd":              repo,
		"stop_hook_active": false,
	})
	stopOut, stopCode, err := st.gr.ExecuteHook(ctx, "claude-stop", stopPayload, repo)
	if err != nil || stopCode != 0 || stopOut != "" {
		t.Fatalf("expected clean Claude Stop batch to pass silently, got code=%d out=%q err=%v", stopCode, stopOut, err)
	}
	if len(eval.capturedPlans) != 1 {
		t.Fatalf("expected exactly 1 VerifyPlan call across 3 edits at Stop, got %d", len(eval.capturedPlans))
	}
	plan := eval.capturedPlans[0]
	if !strings.Contains(plan, "+ func okFooter() { return }") || !strings.Contains(plan, "+ func b() {}") {
		t.Fatalf("expected Stop plan to contain net diff of a.go and b.go, got:\n%s", plan)
	}
	if strings.Contains(plan, "preexisting-before-turn") || strings.Contains(plan, "panic(\"temp\")") {
		t.Fatalf("expected Stop plan to exclude pre-existing dirty line and collapsed temp edit, got:\n%s", plan)
	}

	eval.violate = true
	turn2, _ := json.Marshal(map[string]any{
		"session_id": "claude-batch-1",
		"cwd":        repo,
		"prompt":     "Add helper to b.go",
	})
	_, _, _ = st.gr.ExecuteHook(ctx, "claude-turn", turn2, repo)
	editB, _ := json.Marshal(map[string]any{
		"session_id": "claude-batch-1",
		"cwd":        repo,
		"tool_name":  "Edit",
		"tool_input": map[string]any{"file_path": fileB, "old_string": "func b() {}", "new_string": "func b() { panic(\"bad\") }"},
	})
	_, _, _ = st.gr.ExecuteHook(ctx, "claude-pretool", editB, repo)
	_ = os.WriteFile(fileB, []byte("package main\nfunc b() { panic(\"bad\") }\n"), 0o600)

	blockOut, blockCode, err := st.gr.ExecuteHook(ctx, "claude-stop", stopPayload, repo)
	if err != nil || blockCode != 0 || !strings.Contains(blockOut, `"decision":"block"`) || !strings.Contains(blockOut, "Never call panic()") {
		t.Fatalf("expected Claude Stop to return decision=block with reason, got code=%d out=%q err=%v", blockCode, blockOut, err)
	}

	page, err := st.gr.QueryGuardrailLogsPage(ctx, domain.GuardrailLogQuery{Scope: scope, Source: "stop", Page: 1, PageSize: 10})
	if err != nil || page.TotalCount != 2 || len(page.Records) != 2 {
		t.Fatalf("expected 2 stop logs in audit query, got err=%v page=%+v", err, page)
	}
}

func TestStopGuardsAndNoOpEditsSkipLooping(t *testing.T) {
	ctx := context.Background()
	eval := &batchVerifyEval{
		violate: true,
		stubEval: stubEval{
			dec: domain.MutationDecision{
				Category:    "architecture",
				Tier:        "rule",
				IsPermanent: 0.95,
				Action:      "INSERT",
				Confidence:  0.95,
			},
		},
	}
	st := newTestStack(t, ":memory:", eval)

	repo := t.TempDir()
	scope := "project:" + strings.ToLower(filepath.Base(repo))
	_, _ = st.mem.CommitMemory(ctx, "Always return wrapped errors", "architecture", scope)
	eval.dec = domain.MutationDecision{Action: "IGNORE"}

	target := filepath.Join(repo, "svc.go")
	_ = os.WriteFile(target, []byte("package svc\nfunc run() {}\n"), 0o600)
	editPayload := fmt.Sprintf(`{"conversationId":"conv-stop-guards","workspacePaths":[%q],"lastUserInput":"Refactor svc","toolCall":{"name":"edit_file","args":{"Path":%q,"Replacement":"bad"}}}`,
		repo, target)

	_, _, _ = st.gr.ExecuteHook(ctx, "antigravity-pretool", []byte(editPayload), "")
	noOpStop := fmt.Sprintf(`{"conversationId":"conv-stop-guards","workspacePaths":[%q],"terminationReason":"NO_TOOL_CALL"}`, repo)
	out, code, _ := st.gr.ExecuteHook(ctx, "antigravity-stop", []byte(noOpStop), "")
	if code != 0 || out != `{}` || len(eval.capturedPlans) != 0 {
		t.Fatalf("expected unchanged disk file at Stop to skip VerifyPlan, got code=%d calls=%d out=%s", code, len(eval.capturedPlans), out)
	}

	_, _, _ = st.gr.ExecuteHook(ctx, "antigravity-pretool", []byte(editPayload), "")
	_ = os.WriteFile(target, []byte("package svc\nfunc run() { println(1) }\n"), 0o600)
	canceledStop := fmt.Sprintf(`{"conversationId":"conv-stop-guards","workspacePaths":[%q],"terminationReason":"USER_CANCELED"}`, repo)
	out, code, _ = st.gr.ExecuteHook(ctx, "antigravity-stop", []byte(canceledStop), "")
	if code != 0 || out != `{}` || len(eval.capturedPlans) != 0 {
		t.Fatalf("expected USER_CANCELED Stop to skip evaluation and yield, got code=%d calls=%d out=%s", code, len(eval.capturedPlans), out)
	}
	out, code, _ = st.gr.ExecuteHook(ctx, "antigravity-stop", []byte(noOpStop), "")
	if code != 0 || out != `{}` || len(eval.capturedPlans) != 0 {
		t.Fatalf("expected cleared pending edits after USER_CANCELED, got code=%d calls=%d out=%s", code, len(eval.capturedPlans), out)
	}

	_, _, _ = st.gr.ExecuteHook(ctx, "claude-pretool", []byte(fmt.Sprintf(`{"session_id":"conv-stop-guards","cwd":%q,"tool_name":"Edit","tool_input":{"file_path":%q}}`, repo, target)), repo)
	_ = os.WriteFile(target, []byte("package svc\nfunc run() { println(2) }\n"), 0o600)
	activeStop := fmt.Sprintf(`{"session_id":"conv-stop-guards","cwd":%q,"stop_hook_active":true}`, repo)
	cOut, cCode, _ := st.gr.ExecuteHook(ctx, "claude-stop", []byte(activeStop), repo)
	if cCode != 0 || cOut != "" || len(eval.capturedPlans) != 0 {
		t.Fatalf("expected stop_hook_active=true to bypass re-blocking, got code=%d calls=%d out=%q", cCode, len(eval.capturedPlans), cOut)
	}
}

func TestStopBatchSeparatesDisjointHunksAndOmitsUnchangedMiddle(t *testing.T) {
	ctx := context.Background()
	eval := &batchVerifyEval{
		stubEval: stubEval{
			dec: domain.MutationDecision{
				Category:    "architecture",
				Tier:        "rule",
				IsPermanent: 0.95,
				Action:      "INSERT",
				Confidence:  0.95,
			},
		},
	}
	st := newTestStack(t, ":memory:", eval)

	repo := t.TempDir()
	scope := "project:" + strings.ToLower(filepath.Base(repo))
	if _, err := st.mem.CommitMemory(ctx, "Never call panic() in production handlers", "architecture", scope); err != nil {
		t.Fatal(err)
	}
	eval.dec = domain.MutationDecision{Action: "IGNORE"}

	filePath := filepath.Join(repo, "multi.go")
	before := strings.Join([]string{
		"package main",
		"func top() {",
		"\treturn",
		"}",
		"",
		"func unchangedMiddle() {",
		"\t// UNCHANGED_SENTINEL_LINE",
		"}",
		"",
		"func bottom() {",
		"\treturn",
		"}",
		"",
	}, "\n")
	after := strings.Join([]string{
		"package main",
		"func top() {",
		"\tx := 1",
		"}",
		"",
		"func unchangedMiddle() {",
		"\t// UNCHANGED_SENTINEL_LINE",
		"}",
		"",
		"func bottom() {",
		"\ty := 2",
		"}",
		"",
	}, "\n")
	if err := os.WriteFile(filePath, []byte(before), 0o600); err != nil {
		t.Fatal(err)
	}

	prePayload, _ := json.Marshal(map[string]any{
		"session_id": "claude-hunk-1",
		"cwd":        repo,
		"tool_name":  "Edit",
		"tool_input": map[string]any{"file_path": filePath, "old_string": "\treturn", "new_string": "\tx := 1"},
	})
	if _, code, err := st.gr.ExecuteHook(ctx, "claude-pretool", prePayload, repo); err != nil || code != 0 {
		t.Fatalf("pretool failed: code=%d err=%v", code, err)
	}
	if err := os.WriteFile(filePath, []byte(after), 0o600); err != nil {
		t.Fatal(err)
	}

	stopPayload, _ := json.Marshal(map[string]any{"session_id": "claude-hunk-1", "cwd": repo})
	if out, code, err := st.gr.ExecuteHook(ctx, "claude-stop", stopPayload, repo); err != nil || code != 0 || out != "" {
		t.Fatalf("expected clean Stop pass, got code=%d out=%q err=%v", code, out, err)
	}
	if len(eval.capturedPlans) != 1 {
		t.Fatalf("expected 1 VerifyPlan call, got %d", len(eval.capturedPlans))
	}
	plan := eval.capturedPlans[0]
	for _, want := range []string{"+ \tx := 1", "+ \ty := 2", "\n...\n", "- \treturn"} {
		if !strings.Contains(plan, want) {
			t.Fatalf("expected Stop plan to contain %q, got:\n%s", want, plan)
		}
	}
	if strings.Contains(plan, "UNCHANGED_SENTINEL_LINE") {
		t.Fatalf("expected unchanged middle lines to be omitted from multi-hunk diff, got:\n%s", plan)
	}
}

func TestStopReverifiesContinuationAndCatchesNewViolations(t *testing.T) {
	ctx := context.Background()
	eval := &contextAwareVerifyEval{
		violateSubstr: "BAD_CALL",
		stubEval: stubEval{
			dec: domain.MutationDecision{
				Category:    "architecture",
				Tier:        "rule",
				IsPermanent: 0.95,
				Action:      "INSERT",
				Confidence:  0.95,
			},
		},
	}
	st := newTestStack(t, ":memory:", eval)

	repo := t.TempDir()
	scope := "project:" + strings.ToLower(filepath.Base(repo))
	if _, err := st.mem.CommitMemory(ctx, "Never invoke BAD_CALL in handlers", "architecture", scope); err != nil {
		t.Fatal(err)
	}
	eval.dec = domain.MutationDecision{Action: "IGNORE"}

	fileA := filepath.Join(repo, "a.go")
	fileB := filepath.Join(repo, "b.go")
	_ = os.WriteFile(fileA, []byte("package app\nfunc a() {}\n"), 0o600)
	_ = os.WriteFile(fileB, []byte("package app\nfunc b() {}\n"), 0o600)

	turnStart, _ := json.Marshal(map[string]any{"session_id": "sess-reverify", "cwd": repo, "prompt": "update a and b"})
	_, _, _ = st.gr.ExecuteHook(ctx, "claude-turn", turnStart, repo)

	for _, f := range []string{fileA, fileB} {
		pre, _ := json.Marshal(map[string]any{
			"session_id": "sess-reverify",
			"cwd":        repo,
			"tool_name":  "Edit",
			"tool_input": map[string]any{"file_path": f},
		})
		_, _, _ = st.gr.ExecuteHook(ctx, "claude-pretool", pre, repo)
	}
	_ = os.WriteFile(fileA, []byte("package app\nfunc a() { BAD_CALL() }\n"), 0o600)
	_ = os.WriteFile(fileB, []byte("package app\nfunc b() { ok() }\n"), 0o600)

	stop1, _ := json.Marshal(map[string]any{"session_id": "sess-reverify", "cwd": repo, "stop_hook_active": false})
	out1, _, _ := st.gr.ExecuteHook(ctx, "claude-stop", stop1, repo)
	if !strings.Contains(out1, `"decision":"block"`) {
		t.Fatalf("expected Stop #1 to block BAD_CALL in a.go, got %q", out1)
	}

	for _, f := range []string{fileA, fileB} {
		pre, _ := json.Marshal(map[string]any{
			"session_id": "sess-reverify",
			"cwd":        repo,
			"tool_name":  "Edit",
			"tool_input": map[string]any{"file_path": f},
		})
		_, _, _ = st.gr.ExecuteHook(ctx, "claude-pretool", pre, repo)
	}
	_ = os.WriteFile(fileA, []byte("package app\nfunc a() { fixed() }\n"), 0o600)
	_ = os.WriteFile(fileB, []byte("package app\nfunc b() { BAD_CALL() }\n"), 0o600)

	stopCont, _ := json.Marshal(map[string]any{"session_id": "sess-reverify", "cwd": repo, "stop_hook_active": true})
	out2, _, _ := st.gr.ExecuteHook(ctx, "claude-stop", stopCont, repo)
	if !strings.Contains(out2, `"decision":"block"`) {
		t.Fatalf("expected Stop #2 continuation to re-verify and catch new BAD_CALL in b.go, got %q", out2)
	}

	_ = os.WriteFile(fileB, []byte("package app\nfunc b() { ok() }\n"), 0o600)
	out3, _, _ := st.gr.ExecuteHook(ctx, "claude-stop", stopCont, repo)
	if out3 != "" {
		t.Fatalf("expected Stop #3 after full fix to pass cleanly, got %q", out3)
	}
	page, err := st.gr.QueryGuardrailLogsPage(ctx, domain.GuardrailLogQuery{Scope: scope, Source: "stop", Page: 1, PageSize: 10})
	if err != nil || page.TotalCount != 3 || page.BlockedCount != 2 || page.PassCount != 1 {
		t.Fatalf("expected 3 Stop log entries (2 blocks + 1 pass), got err=%v page=%+v", err, page)
	}
	if len(eval.capturedPlans) != 5 {
		t.Fatalf("expected 5 VerifyPlan calls (2x Phase1+2 on blocks + 1x clean Phase1), got %d", len(eval.capturedPlans))
	}
}

func TestStopReverifyCapYieldsAndWarnsNextTurn(t *testing.T) {
	ctx := context.Background()
	eval := &contextAwareVerifyEval{
		violateSubstr: "PERSISTENT_VIOLATION",
		stubEval: stubEval{
			dec: domain.MutationDecision{
				Category:    "architecture",
				Tier:        "rule",
				IsPermanent: 0.95,
				Action:      "INSERT",
				Confidence:  0.95,
			},
		},
	}
	st := newTestStack(t, ":memory:", eval)

	repo := t.TempDir()
	scope := "project:" + strings.ToLower(filepath.Base(repo))
	if _, err := st.mem.CommitMemory(ctx, "Never use PERSISTENT_VIOLATION", "architecture", scope); err != nil {
		t.Fatal(err)
	}
	eval.dec = domain.MutationDecision{Action: "IGNORE"}

	target := filepath.Join(repo, "loop.go")
	_ = os.WriteFile(target, []byte("package app\nfunc f() {}\n"), 0o600)
	pre, _ := json.Marshal(map[string]any{
		"session_id": "sess-cap",
		"cwd":        repo,
		"tool_name":  "Edit",
		"tool_input": map[string]any{"file_path": target},
	})
	_, _, _ = st.gr.ExecuteHook(ctx, "claude-pretool", pre, repo)
	_ = os.WriteFile(target, []byte("package app\nfunc f() { PERSISTENT_VIOLATION() }\n"), 0o600)

	stopInit, _ := json.Marshal(map[string]any{"session_id": "sess-cap", "cwd": repo, "stop_hook_active": false})
	stopCont, _ := json.Marshal(map[string]any{"session_id": "sess-cap", "cwd": repo, "stop_hook_active": true})

	if out1, _, _ := st.gr.ExecuteHook(ctx, "claude-stop", stopInit, repo); !strings.Contains(out1, `"decision":"block"`) {
		t.Fatalf("expected Stop #1 block, got %q", out1)
	}
	if out2, _, _ := st.gr.ExecuteHook(ctx, "claude-stop", stopCont, repo); !strings.Contains(out2, `"decision":"block"`) {
		t.Fatalf("expected Stop #2 block, got %q", out2)
	}
	if out3, _, _ := st.gr.ExecuteHook(ctx, "claude-stop", stopCont, repo); out3 != "" {
		t.Fatalf("expected Stop #3 at retry cap to yield cleanly, got %q", out3)
	}

	page, err := st.gr.QueryGuardrailLogsPage(ctx, domain.GuardrailLogQuery{Scope: scope, Source: "stop", Page: 1, PageSize: 10})
	if err != nil || page.TotalCount != 3 || page.Records[0].Blocked {
		t.Fatalf("expected 3 stop logs with final cap-yield record Blocked=false, got err=%v page=%+v", err, page)
	}
	if !strings.Contains(page.Records[0].Details, "retry cap reached") {
		t.Fatalf("expected retry cap note in final log details, got %q", page.Records[0].Details)
	}

	nextTurn, _ := json.Marshal(map[string]any{"session_id": "sess-cap", "cwd": repo, "prompt": "next task"})
	turnOut, _, _ := st.gr.ExecuteHook(ctx, "claude-turn", nextTurn, repo)
	if !strings.Contains(turnOut, "[MEMORIZE GUARDRAIL NOTICE]") || !strings.Contains(turnOut, "PERSISTENT_VIOLATION") {
		t.Fatalf("expected next turn to receive unresolved Stop retry cap warning, got %q", turnOut)
	}
}
