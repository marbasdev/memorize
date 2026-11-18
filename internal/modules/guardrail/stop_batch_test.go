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
	// Normal implementation prompts do not establish new durable rules, so Pre-Mutate Gate ignores them.
	eval.dec = domain.MutationDecision{Action: "IGNORE"}

	// Pre-existing dirty lines before this turn must NOT be included in this turn's Stop net diff.
	fileA := filepath.Join(repo, "a.go")
	fileB := filepath.Join(repo, "b.go")
	preExistingA := "package main\nfunc oldLegacy() { panic(\"preexisting-before-turn\") }\nfunc keepMiddle() {}\nfunc okFooter() {}\n"
	if err := os.WriteFile(fileA, []byte(preExistingA), 0o600); err != nil {
		t.Fatal(err)
	}

	// Turn starts cleanly.
	turnPayload, _ := json.Marshal(map[string]any{
		"session_id": "claude-batch-1",
		"cwd":        repo,
		"prompt":     "Update footer in a.go and create b.go",
	})
	_, _, _ = st.gr.ExecuteHook(ctx, "claude-turn", turnPayload, repo)

	// Edit #1 introduces temporary panic in a.go, Edit #2 fixes it before Stop, Edit #3 creates b.go.
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

	// 0 VerifyPlan calls occurred during the 3 file edits.
	if len(eval.capturedPlans) != 0 {
		t.Fatalf("expected 0 VerifyPlan calls during PreToolUse file edits, got %d", len(eval.capturedPlans))
	}

	// Simulate final net edits landed on disk (edit #2 fixed temporary panic; pre-existing oldLegacy untouched).
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

	// Verify next turn with violating edit blocks Claude Stop with {"decision":"block","reason":...} and logs source='stop'.
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

	// 1. Aborted / no-op edit (disk unchanged) must skip VerifyPlan at Stop.
	_, _, _ = st.gr.ExecuteHook(ctx, "antigravity-pretool", []byte(editPayload), "")
	noOpStop := fmt.Sprintf(`{"conversationId":"conv-stop-guards","workspacePaths":[%q],"terminationReason":"NO_TOOL_CALL"}`, repo)
	out, code, _ := st.gr.ExecuteHook(ctx, "antigravity-stop", []byte(noOpStop), "")
	if code != 0 || out != `{}` || len(eval.capturedPlans) != 0 {
		t.Fatalf("expected unchanged disk file at Stop to skip VerifyPlan, got code=%d calls=%d out=%s", code, len(eval.capturedPlans), out)
	}

	// 2. USER_CANCELED / ERROR terminations must allow cleanly without calling VerifyPlan and clear pending edits.
	_, _, _ = st.gr.ExecuteHook(ctx, "antigravity-pretool", []byte(editPayload), "")
	_ = os.WriteFile(target, []byte("package svc\nfunc run() { println(1) }\n"), 0o600)
	canceledStop := fmt.Sprintf(`{"conversationId":"conv-stop-guards","workspacePaths":[%q],"terminationReason":"USER_CANCELED"}`, repo)
	out, code, _ = st.gr.ExecuteHook(ctx, "antigravity-stop", []byte(canceledStop), "")
	if code != 0 || out != `{}` || len(eval.capturedPlans) != 0 {
		t.Fatalf("expected USER_CANCELED Stop to skip evaluation and yield, got code=%d calls=%d out=%s", code, len(eval.capturedPlans), out)
	}
	// Verify pending edits were cleared so an empty follow-up NO_TOOL_CALL does not replay canceled edits.
	out, code, _ = st.gr.ExecuteHook(ctx, "antigravity-stop", []byte(noOpStop), "")
	if code != 0 || out != `{}` || len(eval.capturedPlans) != 0 {
		t.Fatalf("expected cleared pending edits after USER_CANCELED, got code=%d calls=%d out=%s", code, len(eval.capturedPlans), out)
	}

	// 3. stop_hook_active=true / continuationActive=true must exit without forcing another loop.
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

	// Stop #1: catches BAD_CALL in a.go and keeps baselines for re-verification.
	stop1, _ := json.Marshal(map[string]any{"session_id": "sess-reverify", "cwd": repo, "stop_hook_active": false})
	out1, _, _ := st.gr.ExecuteHook(ctx, "claude-stop", stop1, repo)
	if !strings.Contains(out1, `"decision":"block"`) {
		t.Fatalf("expected Stop #1 to block BAD_CALL in a.go, got %q", out1)
	}

	// Continuation fix: fixes a.go, but introduces BAD_CALL in b.go.
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

	// Stop #2 (stop_hook_active=true): must re-verify and catch BAD_CALL in b.go.
	stopCont, _ := json.Marshal(map[string]any{"session_id": "sess-reverify", "cwd": repo, "stop_hook_active": true})
	out2, _, _ := st.gr.ExecuteHook(ctx, "claude-stop", stopCont, repo)
	if !strings.Contains(out2, `"decision":"block"`) {
		t.Fatalf("expected Stop #2 continuation to re-verify and catch new BAD_CALL in b.go, got %q", out2)
	}

	// Second fix resolves b.go cleanly -> Stop #3 passes.
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

	// Stop #1 and #2 block (up to maxStopReverifyAttempts=2).
	if out1, _, _ := st.gr.ExecuteHook(ctx, "claude-stop", stopInit, repo); !strings.Contains(out1, `"decision":"block"`) {
		t.Fatalf("expected Stop #1 block, got %q", out1)
	}
	if out2, _, _ := st.gr.ExecuteHook(ctx, "claude-stop", stopCont, repo); !strings.Contains(out2, `"decision":"block"`) {
		t.Fatalf("expected Stop #2 block, got %q", out2)
	}
	// Stop #3 hits retry cap -> final check still fails, yields "" without infinite loop, and warns next turn.
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
