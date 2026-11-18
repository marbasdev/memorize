package guardrail_test

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

	// Create 3 files with ~2.1KB net diff each (~6.3KB total, exceeding the legacy 4KB Stop cut).
	// Put the violating line in the 3rd file (z_last.go) to prove tail changes are never truncated away.
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

	// 1. Whitespace-only formatting file (120 lines of indentation/blank-line churn).
	wsPath := filepath.Join(repo, "ws_only.go")
	wsBefore := "package app\n" + strings.Repeat("func keep(a int) int { return a }\n", 120)
	wsAfter := "package app\n\n" + strings.Repeat("  func keep(a int) int { return a }\n\n", 120)
	_ = os.WriteFile(wsPath, []byte(wsBefore), 0o600)

	// 2. Single oversized semantic file (>5.5KB net diff) with violation at the bottom.
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

		// ~8KB diff: requires >=3 chunks under unknown/small limit, fits in 1 chunk under 262K context.
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

		// Delay 90ms per chunk with a 1.56s context deadline -> chunk 1 runs cleanly (~1.47s left < 1.5s),
		// forcing chunk 2+ to hit the Stop chunk deadline guard deterministically without waiting seconds.
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

	// 1. Fail-open: allows cleanly, logs partial verdict, and surfaces notice once on next turn.
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

	// 2. Fail-closed: blocks Stop listing unverified file(s) and records blocked partial log.
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

	// Stage 3 files totaling > 5KB with violation in z_tail.go (well past legacy 2000B cap).
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
