package guardrail_test

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"memorize/internal/domain"
	"memorize/internal/modules/guardrail"
	"memorize/internal/modules/memory"
	"memorize/internal/modules/progress"
)

type autoScoreEval struct {
	stubEval
	scoreCalls atomic.Int32
}

func (a *autoScoreEval) ScoreRelevance(ctx context.Context, task string, facts []domain.Fact) ([]domain.ScoredFact, error) {
	a.scoreCalls.Add(1)
	return a.stubEval.ScoreRelevance(ctx, task, facts)
}

func TestZeroTouchAutomationSuite(t *testing.T) {
	ctx := context.Background()
	dbFile := filepath.Join(t.TempDir(), "autotouch.db")

	eval := &stubEval{
		dec: domain.MutationDecision{
			Category:          "security",
			Tags:              []string{"auth"},
			Tier:              "invariant",
			Cardinality:       "additive",
			IsPermanent:       0.95,
			IsGlobalInvariant: 0.92,
			Action:            "INSERT",
			Confidence:        0.94,
		},
	}
	st := newTestStack(t, dbFile, eval)
	st.mem.SetAutoGitSync(true)

	repo := t.TempDir()
	gitRun := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", repo}, args...)...)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v failed: %v\n%s", args, err, string(out))
		}
		return strings.TrimSpace(string(out))
	}
	writeAndCommit := func(rel, body, msg string) string {
		t.Helper()
		full := filepath.Join(repo, rel)
		_ = os.MkdirAll(filepath.Dir(full), 0o755)
		if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
			t.Fatalf("write %s: %v", rel, err)
		}
		gitRun("add", ".")
		gitRun("commit", "-m", msg)
		return gitRun("rev-parse", "--short=12", "HEAD")
	}

	gitRun("init")
	gitRun("config", "user.email", "dev@example.com")
	gitRun("config", "user.name", "Dev")

	_ = writeAndCommit("AGENTS.md", "# Legacy Agents Rules\n- LEGACY_IGNORED_AGENTS_RULE must never be scraped\n", "docs: add legacy agents file")
	_ = writeAndCommit("CLAUDE.md", "# Legacy Claude Rules\n- LEGACY_IGNORED_CLAUDE_RULE must never be scraped\n", "docs: add legacy claude file")
	head0 := writeAndCommit("CONSTRAINTS.md", "# Project Constraints\n- Never use CGO in Go binaries\n- Always enforce TLS 1.3 on external endpoints\n", "chore: initial manifest")
	scope := "project:" + strings.ToLower(filepath.Base(repo))

	autoMsg0 := st.mem.InspectRepoSync(ctx, "sess-auto", scope, repo, false)
	if !strings.Contains(autoMsg0, "Auto Git Baseline Onboarding") || st.mem.GetSyncedCommit(ctx, scope) != head0 {
		t.Fatalf("expected autonomous baseline onboarding at %s, got msg=%q cursor=%q", head0, autoMsg0, st.mem.GetSyncedCommit(ctx, scope))
	}
	baseRecs, _ := st.mem.ListRecords(ctx, scope)
	for _, r := range baseRecs {
		if strings.Contains(r.Content, "LEGACY_IGNORED_") {
			t.Fatalf("expected AGENTS.md/CLAUDE.md rules to be ignored, but stored: %+v", r)
		}
	}

	eval.dec.Tier = "transient"
	_ = writeAndCommit("AGENTS.md", "# Legacy Agents Rules\n- LEGACY_IGNORED_DIFF_RULE must never be extracted\n", "docs: update agents instructions")
	head1 := writeAndCommit("README.md", "# Updated Docs\n", "docs: fix typo in readme")
	autoMsg1 := st.mem.InspectRepoSync(ctx, "sess-auto", scope, repo, false)
	if !strings.Contains(autoMsg1, "0 rule changes") || st.mem.GetSyncedCommit(ctx, scope) != head1 {
		t.Fatalf("expected fast-forward on ordinary commit to %s, got msg=%q cursor=%q", head1, autoMsg1, st.mem.GetSyncedCommit(ctx, scope))
	}
	diffRecs, _ := st.mem.ListRecords(ctx, scope)
	for _, r := range diffRecs {
		if strings.Contains(r.Content, "LEGACY_IGNORED_") {
			t.Fatalf("expected AGENTS.md commit diff rules to be ignored, but stored: %+v", r)
		}
	}

	eval.dec.Tier = "invariant"
	eval.dec.IsPermanent = 0.95
	head2 := writeAndCommit("internal/auth.go", "package internal\n", "refactor(auth): replace legacy tokens with WebAuthn passkeys")
	autoMsg2 := st.mem.InspectRepoSync(ctx, "sess-auto", scope, repo, false)
	if !strings.Contains(autoMsg2, "applied 1 rule update") || st.mem.GetSyncedCommit(ctx, scope) != head2 {
		t.Fatalf("expected autonomous rule commit at %s, got msg=%q cursor=%q", head2, autoMsg2, st.mem.GetSyncedCommit(ctx, scope))
	}

	// Cross-process session delta hook roundtrip (Antigravity camelCase contract)
	startPayload := fmt.Sprintf(`{"conversationId":"agy-sess-1","workspacePaths":[%q],"lastUserInput":"implement auth handler"}`, repo)
	hookStartOut, code, err := st.gr.ExecuteHook(ctx, "antigravity-start", []byte(startPayload), "")
	if err != nil || code != 0 || !strings.Contains(hookStartOut, "Never use CGO") || !strings.Contains(hookStartOut, "systemMessage") {
		t.Fatalf("expected antigravity-start hook to auto-inject active rules, got code=%d out=%s", code, hookStartOut)
	}

	mem2, err := memory.Open(dbFile, eval)
	if err != nil {
		t.Fatalf("memory.Open(mem2) failed: %v", err)
	}
	defer mem2.Close()
	mem2.SetAutoGitSync(true)
	gr2 := guardrail.New(mem2, progress.New(mem2))

	turnDupOut, code, _ := gr2.ExecuteHook(ctx, "antigravity-turn", []byte(startPayload), "")
	if code != 0 || turnDupOut != "{}" {
		t.Fatalf("expected cross-process antigravity-turn hook to suppress already-sent memories with '{}', got code=%d out=%s", code, turnDupOut)
	}

	_, _ = mem2.CommitMemory(ctx, "모든 DB 쿼리는 context.WithTimeout을 강제한다", "database", scope)
	turnDeltaOut, code, _ := gr2.ExecuteHook(ctx, "antigravity-turn", []byte(startPayload), "")
	if code != 0 || !strings.Contains(turnDeltaOut, "systemMessage") || !strings.Contains(turnDeltaOut, "context.WithTimeout") || strings.Contains(turnDeltaOut, "Never use CGO") {
		t.Fatalf("expected antigravity-turn to inject only delta memory, got out=%s", turnDeltaOut)
	}

	// Count-based consolidation (no forced background auto-consolidation on commit)
	eval.dec.Tier = "rule"
	eval.dec.IsGlobalInvariant = 0.20
	eval.dec.Category = "convention"
	_, _ = st.mem.CommitMemory(ctx, "에러 로그는 구조화된 slog 패키지를 사용한다", "convention", "project:cons")
	_, _ = st.mem.CommitMemory(ctx, "모든 에러 반환 시 fmt.Errorf로 문맥을 래핑한다", "convention", "project:cons")
	_, _ = st.mem.CommitMemory(ctx, "패닉 대신 명시적 error 반환을 원칙으로 한다", "convention", "project:cons")
	consCommitOut, err := st.mem.CommitMemory(ctx, "외부 API 타임아웃은 기본 5초로 설정한다", "convention", "project:cons")
	if err != nil || strings.Contains(consCommitOut, "Auto-Consolidated") {
		t.Fatalf("expected commit NOT to force background auto-consolidation, got: %s (%v)", consCommitOut, err)
	}
	preRecs, _ := st.mem.ListRecords(ctx, "project:cons")
	if len(preRecs) != 4 {
		t.Fatalf("expected all 4 convention rules to remain separate before manual merge, got %d", len(preRecs))
	}

	_, targets, err := st.mem.SelectConsolidationTargets(ctx, "project:cons", "convention", false, 3)
	if err != nil || len(targets) != 3 {
		t.Fatalf("expected SelectConsolidationTargets(3) to return 3 items, got len=%d (%v)", len(targets), err)
	}
	var targetIDs []int64
	for _, f := range targets {
		targetIDs = append(targetIDs, f.ID)
	}
	synth, err := st.mem.SynthesizeMergedContent(ctx, "project:cons", "convention", targetIDs)
	if err != nil || strings.TrimSpace(synth) == "" {
		t.Fatalf("SynthesizeMergedContent failed: %q (%v)", synth, err)
	}
	if _, err := st.mem.ConsolidateMemories(ctx, "project:cons", "convention", targetIDs, synth, false); err != nil {
		t.Fatalf("ConsolidateMemories count=3 failed: %v", err)
	}
	postRecs, _ := st.mem.ListRecords(ctx, "project:cons")
	activeCons := 0
	for _, r := range postRecs {
		if r.Active {
			activeCons++
		}
	}
	if activeCons != 2 {
		t.Fatalf("expected 2 active rules after merging 3 of 4, got %d", activeCons)
	}

	// PreToolUse Guardrail Hook
	readHookOut, readCode, _ := st.gr.ExecuteHook(ctx, "claude-pretool",
		[]byte(fmt.Sprintf(`{"cwd":%q,"tool_name":"Read","tool_input":{"file_path":"main.go"}}`, repo)), repo)
	if readCode != 0 || readHookOut != "{}" {
		t.Fatalf("expected Read tool to pass guardrail with code 0, got code=%d out=%s", readCode, readHookOut)
	}

	eval.violations = []domain.PlanViolation{
		{
			Fact:        domain.Fact{ID: 1, Scope: scope, Category: "architecture", Content: "Never use CGO in Go binaries"},
			Probability: 0.94,
			Confidence:  0.95,
		},
	}
	blockHookOut, blockCode, _ := st.gr.ExecuteHook(ctx, "claude-pretool",
		[]byte(fmt.Sprintf(`{"cwd":%q,"tool_name":"Bash","tool_input":{"command":"CGO_ENABLED=1 go build ./..."}}`, repo)), repo)
	if blockCode != 2 || !strings.Contains(blockHookOut, `"decision":"block"`) || !strings.Contains(blockHookOut, "Never use CGO") {
		t.Fatalf("expected violating Bash tool to be blocked (code 2), got code=%d out=%s", blockCode, blockHookOut)
	}

	agyReadOut, agyReadCode, _ := st.gr.ExecuteHook(ctx, "antigravity-pretool",
		[]byte(fmt.Sprintf(`{"workspacePaths":[%q],"toolCall":{"name":"read_file","args":{"Path":"main.go"}}}`, repo)), "")
	if agyReadCode != 0 || agyReadOut != `{"decision":"allow"}` {
		t.Fatalf("expected antigravity-pretool read_file to allow, got code=%d out=%s", agyReadCode, agyReadOut)
	}
	agyBlockOut, agyBlockCode, _ := st.gr.ExecuteHook(ctx, "antigravity-pretool",
		[]byte(fmt.Sprintf(`{"workspacePaths":[%q],"toolCall":{"name":"run_command","args":{"CommandLine":"CGO_ENABLED=1 go build ./..."}}}`, repo)), "")
	if agyBlockCode != 0 || !strings.Contains(agyBlockOut, `"decision":"deny"`) || !strings.Contains(agyBlockOut, "Never use CGO") {
		t.Fatalf("expected antigravity-pretool to return decision=deny, got code=%d out=%s", agyBlockCode, agyBlockOut)
	}
}

func TestSessionDeltaCompressionAndColdStart(t *testing.T) {
	ctx := context.Background()
	eval := &autoScoreEval{}
	st := newTestStack(t, "", eval)

	eval.dec = domain.MutationDecision{
		Category:          "architecture",
		Tier:              "invariant",
		IsPermanent:       0.95,
		IsGlobalInvariant: 0.92,
		Tags:              []string{"sql", "safety"},
		Action:            "INSERT",
		Confidence:        0.95,
	}
	if _, err := st.mem.CommitMemory(ctx, "모든 SQLite DB 파일은 0600 권한을 유지한다", "", "project:slim"); err != nil {
		t.Fatalf("CommitMemory pinned: %v", err)
	}

	eval.dec = domain.MutationDecision{
		Category:          "convention",
		Tier:              "rule",
		IsPermanent:       0.90,
		IsGlobalInvariant: 0.20,
		Tags:              []string{"api", "http"},
		Action:            "INSERT",
		Confidence:        0.90,
	}
	if _, err := st.mem.CommitMemory(ctx, "HTTP 핸들러는 X-Content-Type-Options: nosniff 헤더를 설정한다", "", "project:slim"); err != nil {
		t.Fatalf("CommitMemory rule 1: %v", err)
	}
	if _, err := st.mem.CommitMemory(ctx, "JSON 에러 응답은 민감한 내부 경로를 노출하지 않는다", "", "project:slim"); err != nil {
		t.Fatalf("CommitMemory rule 2: %v", err)
	}

	coldOut, coldCode, err := st.gr.ExecuteHook(ctx, "claude-start", []byte(`{"session_id":"slim-sess-1","cwd":"/tmp/slim"}`), "project:slim")
	if err != nil || coldCode != 0 {
		t.Fatalf("claude-start cold hook failed: code=%d err=%v", coldCode, err)
	}
	if eval.scoreCalls.Load() != 0 {
		t.Fatalf("expected 0 ScoreRelevance calls on empty-prompt SessionStart, got %d", eval.scoreCalls.Load())
	}
	if !strings.Contains(coldOut, "0600 권한") || strings.Contains(coldOut, "nosniff") {
		t.Fatalf("expected cold SessionStart to deliver only pinned invariant, got:\n%s", coldOut)
	}

	eval.scoreCalls.Store(0)
	turnOut, err := st.mem.QuerySessionMemory(ctx, "slim-sess-1", "HTTP API 응답 보안 검토", "", "project:slim", false)
	if err != nil {
		t.Fatalf("QuerySessionMemory turn failed: %v", err)
	}
	if eval.scoreCalls.Load() != 1 {
		t.Fatalf("expected 1 ScoreRelevance call on task turn, got %d", eval.scoreCalls.Load())
	}
	if !strings.Contains(turnOut, "nosniff") || strings.Contains(turnOut, "score:") || strings.Contains(turnOut, "[api, http]") {
		t.Fatalf("expected active session delta without score/tag fluff, got:\n%s", turnOut)
	}
	if !strings.Contains(turnOut, "(+1 active in session)") || strings.Contains(turnOut, "#1,") {
		t.Fatalf("expected count-only active summary instead of ID enumeration, got:\n%s", turnOut)
	}

	emptyDelta, err := st.mem.QuerySessionMemory(ctx, "slim-sess-1", "같은 주제 이어서 진행", "", "project:slim", false)
	if err != nil || !strings.HasPrefix(emptyDelta, "No new memories (3 already active in session)") {
		t.Fatalf("expected compact 'No new memories (3 already active in session)' message, got: %q (%v)", emptyDelta, err)
	}

	probeOut, err := st.mem.QueryMemory(ctx, "HTTP 보안", "", "project:slim")
	if err != nil || !strings.Contains(probeOut, "score: 4.5") {
		t.Fatalf("expected stateless QueryMemory probe to retain score diagnostics, got:\n%s (%v)", probeOut, err)
	}
}

func TestMultiWorkspaceHookZeroInferenceAndSoftPreferenceGuard(t *testing.T) {
	ctx := context.Background()

	// 1. Soft preference never blocks VerifyPlan
	prefEval := &stubEval{
		dec: domain.MutationDecision{
			Category:          "convention",
			Tags:              []string{"golang"},
			Tier:              "preference",
			Cardinality:       "additive",
			IsPermanent:       0.90,
			IsGlobalInvariant: 0.92,
			Action:            "INSERT",
			Confidence:        0.90,
		},
		violations: []domain.PlanViolation{{Fact: domain.Fact{ID: 1}, Probability: 0.95, Confidence: 0.95}},
	}
	prefStack := newTestStack(t, "", prefEval)
	_, _ = prefStack.mem.CommitMemory(ctx, "프로그래밍 언어 golang 선호", "", "global")
	vOut, _ := prefStack.gr.VerifyPlan(ctx, "Python 스크립트 작성", "global")
	if !strings.HasPrefix(vOut, "PASS:") {
		t.Fatalf("expected soft preferences not to block VerifyPlan, got: %s", vOut)
	}

	// 2. Multi-workspace hook support & zero-inference fast paths
	fastEval := &autoScoreEval{
		stubEval: stubEval{
			dec: domain.MutationDecision{
				Category:          "architecture",
				Tier:              "rule",
				IsPermanent:       0.95,
				IsGlobalInvariant: 0.90,
				Action:            "INSERT",
				Confidence:        0.95,
			},
			violations: []domain.PlanViolation{
				{Fact: domain.Fact{ID: 1, Category: "architecture", Content: "필수규칙: 모든 API 응답은 JSON 형식을 사용한다"}, Probability: 0.95, Confidence: 0.95},
			},
		},
	}
	st := newTestStack(t, "", fastEval)

	rootDir := t.TempDir()
	repoA := filepath.Join(rootDir, "alpha-app")
	repoB := filepath.Join(rootDir, "beta-api")
	_ = os.MkdirAll(repoA, 0o755)
	_ = os.MkdirAll(repoB, 0o755)

	_, _ = st.mem.CommitMemory(ctx, "alpha-app must use React 19 server actions", "architecture/frontend", "project:alpha-app")
	_, _ = st.mem.CommitMemory(ctx, "beta-api must use gRPC with mTLS", "architecture/backend", "project:beta-api")

	multiStartPayload := fmt.Sprintf(`{"conversationId":"multi-ws-1","workspacePaths":[%q,%q],"lastUserInput":"update frontend and backend"}`, repoA, repoB)
	multiOut, multiCode, err := st.gr.ExecuteHook(ctx, "antigravity-start", []byte(multiStartPayload), "")
	if err != nil || multiCode != 0 {
		t.Fatalf("multi-workspace antigravity-start failed: code=%d err=%v", multiCode, err)
	}
	if !strings.Contains(multiOut, "React 19") || !strings.Contains(multiOut, "gRPC with mTLS") || !strings.Contains(multiOut, "Multiple workspaces are active") {
		t.Fatalf("expected multi-workspace hook output to include both alpha-app and beta-api memories plus multi-workspace notice, got: %s", multiOut)
	}

	// Repeat session turn makes 0 ScoreRelevance calls when all memories were already sent
	fastEval2 := &autoScoreEval{stubEval: fastEval.stubEval}
	st2 := newTestStack(t, "", fastEval2)
	_, _ = st2.mem.CommitMemory(ctx, "필수규칙: 모든 API 응답은 JSON 형식을 사용한다", "architecture", "project:fast")
	_, _ = st2.mem.QuerySessionMemory(ctx, "fast-sess", "API 작업", "", "project:fast", false)
	callsAfterFirst := fastEval2.scoreCalls.Load()
	_, _ = st2.mem.QuerySessionMemory(ctx, "fast-sess", "다음 턴 작업", "", "project:fast", false)
	if fastEval2.scoreCalls.Load() != callsAfterFirst {
		t.Fatalf("expected 0 additional ScoreRelevance calls when all memories already sent in session, got %d -> %d", callsAfterFirst, fastEval2.scoreCalls.Load())
	}

	// Read-only shell commands & unchanged Target lines in edit_file skip VerifyPlan inference
	diagOut, diagCode, _ := st2.gr.ExecuteHook(ctx, "antigravity-pretool",
		[]byte(`{"workspacePaths":["/tmp/fast"],"toolCall":{"name":"run_command","args":{"CommandLine":"go test ./... && git status"}}}`), "")
	if diagCode != 0 || diagOut != `{"decision":"allow"}` {
		t.Fatalf("expected read-only shell command to allow immediately, got code=%d out=%s", diagCode, diagOut)
	}
	ctxEditOut, ctxEditCode, _ := st2.gr.ExecuteHook(ctx, "antigravity-pretool",
		[]byte(`{"workspacePaths":["/tmp/fast"],"toolCall":{"name":"edit_file","args":{"Path":"handler.go","Target":"// 필수규칙 JSON API\noldVar := 1","Replacement":"// 필수규칙 JSON API\nnewVar := 2"}}}`), "")
	if ctxEditCode != 0 || ctxEditOut != `{"decision":"allow"}` {
		t.Fatalf("expected unchanged Target context line in edit_file not to trigger VerifyPlan overlap, got code=%d out=%s", ctxEditCode, ctxEditOut)
	}
}
