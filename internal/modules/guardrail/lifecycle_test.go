package guardrail

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"memorize/internal/domain"
	"memorize/internal/inference/providers"
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
	basePage, _ := st.mem.QueryRecordsPage(ctx, domain.RecordQuery{Scope: scope, State: "all", PageSize: 100})
	for _, r := range basePage.Records {
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
	diffPage, _ := st.mem.QueryRecordsPage(ctx, domain.RecordQuery{Scope: scope, State: "all", PageSize: 100})
	for _, r := range diffPage.Records {
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
	gr2 := New(mem2, progress.New(mem2))

	turnDupOut, code, _ := gr2.ExecuteHook(ctx, "antigravity-turn", []byte(startPayload), "")
	if code != 0 || turnDupOut != "{}" {
		t.Fatalf("expected cross-process antigravity-turn hook to suppress already-sent memories with '{}', got code=%d out=%s", code, turnDupOut)
	}

	_, _ = mem2.CommitMemory(ctx, "모든 DB 쿼리는 context.WithTimeout을 강제한다", "database", scope)
	turnDeltaOut, code, _ := gr2.ExecuteHook(ctx, "antigravity-turn", []byte(startPayload), "")
	if code != 0 || !strings.Contains(turnDeltaOut, "systemMessage") || !strings.Contains(turnDeltaOut, "context.WithTimeout") || strings.Contains(turnDeltaOut, "Never use CGO") {
		t.Fatalf("expected antigravity-turn to inject only delta memory, got out=%s", turnDeltaOut)
	}

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
	prePage, _ := st.mem.QueryRecordsPage(ctx, domain.RecordQuery{Scope: "project:cons", State: "all", PageSize: 50})
	if len(prePage.Records) != 4 {
		t.Fatalf("expected all 4 convention rules to remain separate before manual merge, got %d", len(prePage.Records))
	}

	activeFacts, err := st.mem.LoadActiveFacts(ctx, "project:cons", "project:cons", "convention")
	if err != nil || len(activeFacts) != 4 {
		t.Fatalf("expected 4 active convention facts, got len=%d (%v)", len(activeFacts), err)
	}
	targetIDs := []int64{activeFacts[0].ID, activeFacts[1].ID, activeFacts[2].ID}
	synth, err := st.mem.SynthesizeMergedContent(ctx, "project:cons", "convention", targetIDs)
	if err != nil || strings.TrimSpace(synth) == "" {
		t.Fatalf("SynthesizeMergedContent failed: %q (%v)", synth, err)
	}
	if _, err := st.mem.ConsolidateMemories(ctx, "project:cons", "convention", targetIDs, synth, false); err != nil {
		t.Fatalf("ConsolidateMemories count=3 failed: %v", err)
	}
	postPage, _ := st.mem.QueryRecordsPage(ctx, domain.RecordQuery{Scope: "project:cons", State: "active", PageSize: 50})
	if len(postPage.Records) != 2 {
		t.Fatalf("expected 2 active rules after merging 3 of 4, got %d", len(postPage.Records))
	}

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

	probeOut, err := st.mem.QuerySessionMemory(ctx, "", "HTTP 보안", "", "project:slim", true)
	if err != nil || !strings.Contains(probeOut, "score: 4.5") {
		t.Fatalf("expected stateless QueryMemory probe to retain score diagnostics, got:\n%s (%v)", probeOut, err)
	}
}

func TestMultiWorkspaceHookZeroInferenceAndSoftPreferenceGuard(t *testing.T) {
	ctx := context.Background()

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

	fastEval2 := &autoScoreEval{stubEval: fastEval.stubEval}
	st2 := newTestStack(t, "", fastEval2)
	_, _ = st2.mem.CommitMemory(ctx, "필수규칙: 모든 API 응답은 JSON 형식을 사용한다", "architecture", "project:fast")
	_, _ = st2.mem.QuerySessionMemory(ctx, "fast-sess", "API 작업", "", "project:fast", false)
	callsAfterFirst := fastEval2.scoreCalls.Load()
	_, _ = st2.mem.QuerySessionMemory(ctx, "fast-sess", "다음 턴 작업", "", "project:fast", false)
	if fastEval2.scoreCalls.Load() != callsAfterFirst {
		t.Fatalf("expected 0 additional ScoreRelevance calls when all memories already sent in session, got %d -> %d", callsAfterFirst, fastEval2.scoreCalls.Load())
	}

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

func TestSeparatedProjectProgressLifecycleAndGitSync(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	ctx := context.Background()
	eval := &progressSyncEval{}
	eval.dec = domain.MutationDecision{
		IsPermanent:       0.95,
		IsGlobalInvariant: 0.90,
		Action:            "INSERT",
		Confidence:        0.92,
	}

	st := newTestStack(t, ":memory:", eval)

	repoDir := filepath.Join(t.TempDir(), "prog-demo")
	if err := os.MkdirAll(repoDir, 0o755); err != nil {
		t.Fatalf("mkdir repo: %v", err)
	}
	runGitCmd := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = repoDir
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=Test",
			"GIT_AUTHOR_EMAIL=test@example.com",
			"GIT_COMMITTER_NAME=Test",
			"GIT_COMMITTER_EMAIL=test@example.com",
		)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v failed: %v\n%s", args, err, string(out))
		}
	}
	runGitCmd("init")
	_ = os.WriteFile(filepath.Join(repoDir, "main.go"), []byte("package main\n"), 0o644)
	runGitCmd("add", ".")
	runGitCmd("commit", "-m", "feat(core): bootstrap service")
	baseHeadBytes, _ := exec.Command("git", "-C", repoDir, "rev-parse", "--short=12", "HEAD").Output()
	baseHead := strings.TrimSpace(string(baseHeadBytes))

	scope := "project:prog-demo"
	st.prog.RememberRepoPath(ctx, scope, repoDir)
	_ = st.mem.SetSyncedCommit(ctx, scope, baseHead)

	if _, err := st.mem.CommitMemoryFull(ctx, "순수 Go SQLite 드라이버만 사용한다", "architecture", "storage", "", scope); err != nil {
		t.Fatalf("Commit 1: %v", err)
	}
	if _, err := st.mem.CommitMemoryFull(ctx, "단일 연결 WAL 모드로 잠금 경합을 방지한다", "architecture", "storage", "", scope); err != nil {
		t.Fatalf("Commit 2: %v", err)
	}

	ov1, err := st.prog.GetProgressOverviewByTrack(ctx, scope, "", "", repoDir)
	if err != nil {
		t.Fatalf("GetProgressOverview 1: %v", err)
	}
	if ov1.TotalCount != 1 || len(ov1.Records[0].MemoryIDs) != 2 {
		t.Fatalf("expected 1 coalesced activity row linking [#1 #2], got %+v", ov1.Records)
	}

	manualRec, err := st.prog.RecordProgress(ctx, domain.ProgressInput{
		Scope:       scope,
		Category:    "dashboard",
		SubCategory: "ui",
		Summary:     "SPECIAL_PROGRESS_ONLY_MARKER_XYZ 작업 완료 기록",
		NextStep:    "단위 테스트 작성",
		Source:      "agent",
	})
	if err != nil {
		t.Fatalf("RecordProgress: %v", err)
	}
	if manualRec.ID == 0 {
		t.Fatalf("expected valid activity record ID, got %+v", manualRec)
	}

	memPrompt, err := st.mem.QuerySessionMemory(ctx, "sess-prog-check", "SPECIAL_PROGRESS_ONLY_MARKER_XYZ 확인", "", scope, false)
	if err != nil {
		t.Fatalf("QuerySessionMemory: %v", err)
	}
	if strings.Contains(memPrompt, "SPECIAL_PROGRESS_ONLY_MARKER_XYZ") {
		t.Fatalf("expected project_progress to NEVER leak into session memory prompt, got:\n%s", memPrompt)
	}

	verifyOut, err := st.gr.VerifyPlan(ctx, "edit SPECIAL_PROGRESS_ONLY_MARKER_XYZ", scope)
	if err != nil || !strings.HasPrefix(verifyOut, "PASS:") {
		t.Fatalf("expected VerifyPlan to ignore project_progress entries, got %q (%v)", verifyOut, err)
	}

	if err := st.prog.DeleteProgress(ctx, ov1.Records[0].ID); err != nil {
		t.Fatalf("DeleteProgress: %v", err)
	}
	if _, err := st.mem.CommitMemoryFull(ctx, "세 번째 스토리지 마이그레이션 불변 원칙", "architecture", "storage", "", scope); err != nil {
		t.Fatalf("Commit 3: %v", err)
	}
	if _, err := st.mem.CommitMemoryFull(ctx, "네 번째 백업 스냅샷 무결성 검증 원칙", "architecture", "storage", "", scope); err != nil {
		t.Fatalf("Commit 4: %v", err)
	}
	ovMid, _ := st.prog.GetProgressOverviewByTrack(ctx, scope, "", "", repoDir)
	for _, r := range ovMid.Records {
		if r.Source == "memory" {
			_ = st.prog.DeleteProgress(ctx, r.ID)
		}
	}

	_ = os.WriteFile(filepath.Join(repoDir, "dashboard.html"), []byte("<div>progress</div>\n"), 0o644)
	runGitCmd("add", ".")
	runGitCmd("commit", "-m", "feat(dashboard): add progress tab")

	preSync := st.prog.InspectProgressSync(ctx, scope, repoDir)
	if preSync.InSync || preSync.UnlinkedMemoryCount != 4 || len(preSync.PendingCommits) != 1 {
		t.Fatalf("expected InSync=false with 4 unlinked memories and 1 pending commit, got %+v", preSync)
	}

	syncRes, err := st.prog.SyncProjectProgress(ctx, scope, repoDir, "ko")
	if err != nil {
		t.Fatalf("SyncProjectProgress: %v", err)
	}
	if syncRes.SyncedMemories != 4 || syncRes.SyncedCommits != 1 {
		t.Fatalf("expected SyncedMemories=4 SyncedCommits=1, got %+v", syncRes)
	}

	postSync := st.prog.InspectProgressSync(ctx, scope, repoDir)
	if !postSync.InSync || postSync.UnlinkedMemoryCount != 0 || len(postSync.PendingCommits) != 0 {
		t.Fatalf("expected InSync=true after SyncProjectProgress, got %+v", postSync)
	}

	activityRec, err := st.prog.RecordProgress(ctx, domain.ProgressInput{
		Scope:    scope,
		Track:    "대시보드 히스토리 타임라인 개편",
		Summary:  "상단 활동 요약 카드 및 날짜별 세로 히스토리 연동 완료",
		NextStep: "트랙 클릭 필터 검증",
		Source:   "agent",
	})
	if err != nil {
		t.Fatalf("RecordProgress activity track: %v", err)
	}
	ovFinal, err := st.prog.GetProgressOverviewByTrack(ctx, scope, "", "", repoDir)
	if err != nil {
		t.Fatalf("GetProgressOverview final: %v", err)
	}
	if len(ovFinal.RecentHistory) == 0 || ovFinal.RecentHistory[0].ID != activityRec.ID || ovFinal.RecentHistory[0].Track != "대시보드 히스토리 타임라인 개편" {
		t.Fatalf("expected latest recorded activity at head of RecentHistory, got %+v", ovFinal.RecentHistory)
	}
	if len(ovFinal.AllTracks) < 3 || len(ovFinal.Timeline) == 0 {
		t.Fatalf("expected AllTracks >= 3 and non-empty date-grouped Timeline, got tracks=%d timeline=%d", len(ovFinal.AllTracks), len(ovFinal.Timeline))
	}
	foundFullDetails := false
	for _, r := range ovFinal.Records {
		if r.Source == "memory_sync" && len(r.DetailsLines) == 4 && strings.Contains(r.Details, "네 번째 백업 스냅샷 무결성 검증 원칙") {
			foundFullDetails = true
		}
	}
	if !foundFullDetails {
		t.Fatalf("expected memory_sync record to preserve all 4 memories in DetailsLines without truncation, got %+v", ovFinal.Records)
	}
	filteredOv, err := st.prog.GetProgressOverviewByTrack(ctx, scope, "대시보드 히스토리 타임라인 개편", "", repoDir)
	if err != nil || len(filteredOv.Records) != 1 || filteredOv.Records[0].ID != activityRec.ID {
		t.Fatalf("expected track filter to isolate 1 record, got %+v (%v)", filteredOv.Records, err)
	}

	if _, err := st.prog.RecordProgress(ctx, domain.ProgressInput{
		Scope:   scope,
		Track:   "모바일 반응형 점검",
		Summary: "모바일 반응형 점검 완료",
		Source:  "agent",
	}); err != nil {
		t.Fatalf("RecordProgress second track: %v", err)
	}
	longRule := "진행 현황 히스토리 및 상단 활동 요약 카드에서는 긴 기억 원문이 추가되거나 수정되더라도 말줄임표 없이 끝까지 원문 전체가 온전하게 표시되어야 하며 어떠한 경우에도 문장 끝이 잘리지 않아야 한다 (검증 마커: FULL_TAIL_OK_999)"
	if _, err := st.mem.CommitMemoryFull(ctx, longRule, "architecture", "dashboard", "", scope); err != nil {
		t.Fatalf("CommitMemoryFull longRule: %v", err)
	}

	for i := 1; i <= 12; i++ {
		if _, err := st.prog.RecordProgress(ctx, domain.ProgressInput{
			Scope:   scope,
			Track:   fmt.Sprintf("완료 트랙 #%d", i),
			Summary: fmt.Sprintf("자동 기록 완료 마일스톤 %d", i),
			Source:  "agent",
		}); err != nil {
			t.Fatalf("RecordProgress milestone %d: %v", i, err)
		}
	}

	page1, err := st.prog.GetProgressOverviewPaged(ctx, scope, "", "", repoDir, 1, 10)
	if err != nil {
		t.Fatalf("GetProgressOverviewPaged page 1: %v", err)
	}
	page2, err := st.prog.GetProgressOverviewPaged(ctx, scope, "", "", repoDir, 2, 10)
	if err != nil {
		t.Fatalf("GetProgressOverviewPaged page 2: %v", err)
	}
	if len(page1.Records) != 10 || page1.TotalPages < 2 || len(page2.Records) == 0 {
		t.Fatalf("expected paginated history (page1=10, page2>0, totalPages>=2), got page1=%d page2=%d totalPages=%d",
			len(page1.Records), len(page2.Records), page1.TotalPages)
	}
	if len(page2.RecentHistory) != 5 || len(page2.Projects) != 1 || len(page2.Projects[0].RecentHistory) != 5 {
		t.Fatalf("expected top project board to preserve RecentHistory(5)/Projects(1)/Projects[0].RecentHistory(5) on page 2, got recentHistory=%d projects=%d projRecent=%d",
			len(page2.RecentHistory), len(page2.Projects), len(page2.Projects[0].RecentHistory))
	}
	for _, r := range append(page1.Records, page2.Records...) {
		if strings.Contains(r.Summary, "…") {
			t.Fatalf("expected zero ellipsis '…' across all progress summaries, found in #%d: %q", r.ID, r.Summary)
		}
	}

	_ = os.WriteFile(filepath.Join(repoDir, "auto.go"), []byte("package main\n// auto\n"), 0o644)
	runGitCmd("add", ".")
	runGitCmd("commit", "-m", "feat(progress): auto sync on session start")
	startPayload := fmt.Sprintf(`{"conversationId":"sess-next-window","workspacePaths":[%q],"lastUserInput":"이어서 진행하자"}`, repoDir)
	startOut, startCode, err := st.gr.ExecuteHook(ctx, "antigravity-start", []byte(startPayload), "")
	if err != nil || startCode != 0 {
		t.Fatalf("ExecuteHook antigravity-start: code=%d err=%v", startCode, err)
	}
	if strings.Contains(startOut, "PROJECT PROGRESS STATE") {
		t.Fatalf("expected SessionStart hook NOT to inject active progress state under activity-feed model, got: %s", startOut)
	}
	afterHookSync := st.prog.InspectProgressSync(ctx, scope, repoDir)
	if !afterHookSync.InSync || len(afterHookSync.PendingCommits) != 0 {
		t.Fatalf("expected SessionStart hook to auto-sync new Git commit into project_progress, got %+v", afterHookSync)
	}
}

func TestParallelGuardrailHookAndConcurrentCommitBatch(t *testing.T) {
	ctx := context.Background()

	var (
		mutCalls    atomic.Int32
		verifyCalls atomic.Int32
		failOnce    atomic.Bool
	)

	llmSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Messages []struct {
				Content string `json:"content"`
			} `json:"messages"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		prompt := ""
		if len(req.Messages) > 0 {
			prompt = req.Messages[len(req.Messages)-1].Content
		}

		var content string
		switch {
		case strings.Contains(prompt, "Proposed Plan"):
			verifyCalls.Add(1)
			content = `{"a_verdict":"COMPLIES: ok","violations":[]}`
		case strings.Contains(prompt, "Current Task"):
			content = `{"items":[{"id":1,"score":4.5,"confidence":0.93}]}`
		case strings.Contains(prompt, "\nCandidates:\n"):
			mutCalls.Add(1)
			content = `{"items":[
				{"index":0,"category":"architecture","sub_category":"core","tags":[],"tier":"invariant","cardinality":"exclusive","is_permanent":0.93,"is_global_invariant":0.91,"confidence":0.94,"action":"INSERT","conflict_fact_ids":[]},
				{"index":1,"category":"security","sub_category":"auth","tags":[],"tier":"invariant","cardinality":"exclusive","is_permanent":0.93,"is_global_invariant":0.91,"confidence":0.94,"action":"INSERT","conflict_fact_ids":[]},
				{"index":2,"category":"database","sub_category":"sql","tags":[],"tier":"invariant","cardinality":"exclusive","is_permanent":0.93,"is_global_invariant":0.91,"confidence":0.94,"action":"INSERT","conflict_fact_ids":[]},
				{"index":3,"category":"testing","sub_category":"unit","tags":[],"tier":"invariant","cardinality":"exclusive","is_permanent":0.93,"is_global_invariant":0.91,"confidence":0.94,"action":"INSERT","conflict_fact_ids":[]}
			]}`
		default:
			mutCalls.Add(1)
			if failOnce.CompareAndSwap(true, false) {
				w.WriteHeader(http.StatusServiceUnavailable)
				_, _ = w.Write([]byte(`{"error":{"message":"transient error"}}`))
				return
			}
			content = `{"category":"architecture","sub_category":"core","tags":[],"tier":"invariant","cardinality":"exclusive","is_permanent":0.93,"is_global_invariant":0.91,"confidence":0.94,"action":"INSERT","conflict_fact_ids":[]}`
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"choices":[{"message":{"content":%q}}]}`, content)
	}))
	defer llmSrv.Close()

	adapter := providers.NewOpenAICompatibleAdapter(llmSrv.URL+"/v1", "qwen3:4b", "")
	st := newTestStack(t, ":memory:", adapter)

	workDir := filepath.Join(t.TempDir(), "conc")
	_ = os.MkdirAll(workDir, 0o755)

	if _, err := st.mem.CommitMemory(ctx, "Never enable CGO in Go build commands", "architecture", "project:conc"); err != nil {
		t.Fatalf("seed rule: %v", err)
	}

	turnPayload, _ := json.Marshal(map[string]any{
		"session_id": "sess-parallel",
		"cwd":        workDir,
		"prompt":     "앞으로 모든 API 응답에는 반드시 X-Trace-ID 헤더를 포함해줘 그리고 빌드 실행해줘",
	})
	if _, _, err := st.gr.ExecuteHook(ctx, "claude-turn", turnPayload, workDir); err != nil {
		t.Fatalf("ExecuteHook claude-turn: %v", err)
	}

	guardPayload, _ := json.Marshal(map[string]any{
		"session_id": "sess-parallel",
		"cwd":        workDir,
		"tool_name":  "Bash",
		"tool_input": map[string]any{
			"command": "CGO_ENABLED=1 go build ./...",
		},
	})
	failOnce.Store(true)
	_, _, _ = st.gr.ExecuteHook(ctx, "claude-pretool", guardPayload, workDir)

	guardOut, _, err := st.gr.ExecuteHook(ctx, "claude-pretool", guardPayload, workDir)
	if err != nil {
		t.Fatalf("ExecuteHook claude-pretool: %v", err)
	}
	if !strings.Contains(guardOut, "PRE-MUTATE GATE") {
		t.Fatalf("expected PRE-MUTATE GATE block on un-committed rule after retry, got:\n%s", guardOut)
	}

	mutCalls.Store(0)
	batchSvc, err := memory.Open(":memory:", adapter)
	if err != nil {
		t.Fatalf("memory.Open batch: %v", err)
	}
	defer batchSvc.Close()

	res, err := batchSvc.CommitBatch(ctx, []string{
		"첫 번째 독립 아키텍처 불변 규칙",
		"두 번째 독립 보안 검증 규칙",
		"세 번째 독립 데이터베이스 규칙",
		"두 번째 독립 보안 검증 규칙",
		"네 번째 독립 테스트 격리 규칙",
	}, "architecture", "project:batch", "", "")
	if err != nil {
		t.Fatalf("CommitBatch: %v", err)
	}
	if mutCalls.Load() != 1 {
		t.Fatalf("expected 1 batched EvaluateMutations call for 4 unique candidates (skipping 1 duplicate in batch), got %d", mutCalls.Load())
	}
	if !strings.Contains(res, "SKIPPED (duplicate in batch") {
		t.Fatalf("expected duplicate-in-batch notice in CommitBatch output, got:\n%s", res)
	}
	batchPage, _ := batchSvc.QueryRecordsPage(ctx, domain.RecordQuery{Scope: "project:batch", State: "all", PageSize: 50})
	if len(batchPage.Records) != 4 {
		t.Fatalf("expected 4 committed records, got %d", len(batchPage.Records))
	}
}

type guardrailAuditEval struct {
	stubEval
	violate bool
}

func (e *guardrailAuditEval) Name() string { return "guardrail-audit-eval" }
func (e *guardrailAuditEval) VerifyPlan(_ context.Context, _ string, rules []domain.Fact) ([]domain.PlanViolation, error) {
	if !e.violate || len(rules) == 0 {
		return nil, nil
	}
	return []domain.PlanViolation{
		{Fact: rules[0], Probability: 0.96, Confidence: 0.95},
	}, nil
}
func (e *guardrailAuditEval) SynthesizeConsolidation(_ context.Context, _ string, _ string, facts []domain.Fact) (string, error) {
	if len(facts) > 0 {
		return facts[0].Content, nil
	}
	return "", nil
}

func TestGuardrailLogsAuditPaginationAndIsolation(t *testing.T) {
	ctx := context.Background()
	eval := &guardrailAuditEval{
		stubEval: stubEval{
			dec: domain.MutationDecision{
				Category:          "architecture",
				Tier:              "invariant",
				IsPermanent:       0.95,
				IsGlobalInvariant: 0.92,
				Action:            "INSERT",
				Confidence:        0.96,
			},
		},
	}
	st := newTestStack(t, ":memory:", eval)

	if _, err := st.mem.CommitMemory(ctx, "CGO_ENABLED=0 순수 Go 바이너리 빌드만 허용하며 C 바인딩을 금지한다", "architecture", "project:guard"); err != nil {
		t.Fatalf("CommitMemory: %v", err)
	}

	eval.violate = true
	hookPayload := []byte(`{"tool_name":"run_command","tool_input":{"command":"CGO_ENABLED=1 go build -ldflags sk-1234567890abcdefghijklmnop"},"cwd":"/tmp/guard"}`)
	outDeny, _, err := st.gr.ExecuteHook(ctx, "antigravity-pretool", hookPayload, "project:guard")
	if err != nil || !strings.Contains(outDeny, `"decision":"deny"`) {
		t.Fatalf("expected antigravity-pretool deny, got %q (%v)", outDeny, err)
	}

	eval.violate = false
	passPayload := []byte(`{"tool_name":"run_command","tool_input":{"command":"CGO_ENABLED=0 go build ./cmd/memorize"},"cwd":"/tmp/guard"}`)
	outAllow, _, err := st.gr.ExecuteHook(ctx, "antigravity-pretool", passPayload, "project:guard")
	if err != nil || !strings.Contains(outAllow, `"decision":"allow"`) {
		t.Fatalf("expected antigravity-pretool allow, got %q (%v)", outAllow, err)
	}

	eval.violate = true
	for i := 1; i <= 11; i++ {
		vOut, vErr := st.gr.VerifyPlan(ctx, fmt.Sprintf("GUARDRAIL_ONLY_SECRET_MARKER_%d: CGO_ENABLED=1 빌드 전환 시도", i), "project:guard")
		if vErr != nil || !strings.Contains(vOut, "Violations Detected") {
			t.Fatalf("VerifyPlan %d expected violation, got %q (%v)", i, vOut, vErr)
		}
	}

	page1, err := st.gr.QueryGuardrailLogsPage(ctx, domain.GuardrailLogQuery{
		Scope:    "project:guard",
		State:    "all",
		Page:     1,
		PageSize: 10,
	})
	if err != nil {
		t.Fatalf("QueryGuardrailLogsPage page 1: %v", err)
	}
	if page1.TotalCount != 13 || page1.BlockedCount != 12 || page1.PassCount != 1 || page1.TotalPages != 2 || len(page1.Records) != 10 {
		t.Fatalf("unexpected page 1 metrics: %+v", page1)
	}

	page2, err := st.gr.QueryGuardrailLogsPage(ctx, domain.GuardrailLogQuery{
		Scope:    "project:guard",
		State:    "all",
		Page:     2,
		PageSize: 10,
	})
	if err != nil || len(page2.Records) != 3 {
		t.Fatalf("expected 3 records on page 2, got %+v (%v)", page2, err)
	}
	oldest := page2.Records[len(page2.Records)-1]
	if oldest.Verdict != domain.GuardrailVerdictRuleViolation || strings.Contains(oldest.PlanSnippet, "sk-123456") || !strings.Contains(oldest.PlanSnippet, "[REDACTED]") {
		t.Fatalf("expected secret redaction and rule_violation on oldest PreToolUse log, got %+v", oldest)
	}

	prePass, err := st.gr.QueryGuardrailLogsPage(ctx, domain.GuardrailLogQuery{
		Scope:    "project:guard",
		State:    "pass",
		Source:   "pretool",
		Page:     1,
		PageSize: 10,
	})
	if err != nil || prePass.FilteredCount != 1 || prePass.Records[0].Verdict != domain.GuardrailVerdictPass {
		t.Fatalf("expected 1 pretool pass entry, got %+v (%v)", prePass, err)
	}

	qOut, err := st.mem.QuerySessionMemory(ctx, "sess-guard-iso", "GUARDRAIL_ONLY_SECRET_MARKER", "", "project:guard", true)
	if err != nil {
		t.Fatalf("QuerySessionMemory: %v", err)
	}
	if strings.Contains(qOut, "GUARDRAIL_ONLY_SECRET_MARKER") {
		t.Fatalf("guardrail_logs leaked into QuerySessionMemory: %s", qOut)
	}
}

func TestRetentionAndCleanupProgressAndGuardrailLogs(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "retention.db")
	eval := &progressSyncEval{}
	eval.dec = domain.MutationDecision{
		IsPermanent:       0.95,
		IsGlobalInvariant: 0.90,
		Action:            "INSERT",
		Confidence:        0.92,
	}
	st := newTestStack(t, dbPath, eval)

	if domain.DefaultProgressRetentionDays != 0 || domain.DefaultGuardrailRetentionDays != 0 {
		t.Fatalf("expected DefaultProgressRetentionDays=0 and DefaultGuardrailRetentionDays=0")
	}
	defPol := st.prog.GetRetentionPolicy(ctx)
	if defPol.ProgressDays != 0 || defPol.GuardrailDays != 0 || defPol.GuardrailIncludeBlocked {
		t.Fatalf("expected default forever (0d) retention policy, got: %+v", defPol)
	}

	scope := "project:ret"
	if _, err := st.mem.CommitMemoryFull(ctx, "첫 번째 보관 정책 불변 규칙", "architecture", "storage", "", scope); err != nil {
		t.Fatalf("Commit 1: %v", err)
	}
	if _, err := st.mem.CommitMemoryFull(ctx, "두 번째 보관 정책 불변 규칙", "architecture", "storage", "", scope); err != nil {
		t.Fatalf("Commit 2: %v", err)
	}
	oldTrackA, err := st.prog.RecordProgress(ctx, domain.ProgressInput{
		Scope:    scope,
		Track:    "오래된 작업 트랙 A",
		Summary:  "15일 전 완료된 작업 A",
		NextStep: "후속 검증 메모",
	})
	if err != nil {
		t.Fatalf("RecordProgress oldTrackA: %v", err)
	}
	oldTrackB, err := st.prog.RecordProgress(ctx, domain.ProgressInput{
		Scope:   scope,
		Track:   "오래된 작업 트랙 B",
		Summary: "15일 전 완료된 작업 B",
	})
	if err != nil {
		t.Fatalf("RecordProgress oldTrackB: %v", err)
	}
	recentDone, err := st.prog.RecordProgress(ctx, domain.ProgressInput{
		Scope:   scope,
		Track:   "오늘 완료 트랙",
		Summary: "방금 완료된 최신 작업",
	})
	if err != nil {
		t.Fatalf("RecordProgress recent done: %v", err)
	}

	rawDB, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer rawDB.Close()

	if _, err := rawDB.ExecContext(ctx, `
		UPDATE project_progress
		SET created_at = datetime('now', '-15 days'), updated_at = datetime('now', '-15 days')
		WHERE id != ?`, recentDone.ID); err != nil {
		t.Fatalf("backdate project_progress: %v", err)
	}

	deletedProg, err := st.prog.CleanupProgressHistory(ctx, scope, 7)
	if err != nil {
		t.Fatalf("CleanupProgressHistory(7): %v", err)
	}
	if deletedProg != 3 {
		t.Fatalf("expected 3 old rows deleted, got %d", deletedProg)
	}

	ovAfter, err := st.prog.GetProgressOverviewByTrack(ctx, scope, "", "", "")
	if err != nil {
		t.Fatalf("GetProgressOverview: %v", err)
	}
	if ovAfter.TotalCount != 1 {
		t.Fatalf("expected 1 remaining recent row, got %d: %+v", ovAfter.TotalCount, ovAfter.Records)
	}
	if ovAfter.Records[0].ID != recentDone.ID || ovAfter.Records[0].ID == oldTrackA.ID || ovAfter.Records[0].ID == oldTrackB.ID {
		t.Fatalf("expected only recent record %d preserved, got %+v", recentDone.ID, ovAfter.Records)
	}

	syncState := st.prog.InspectProgressSync(ctx, scope, "")
	if syncState.UnlinkedMemoryCount != 0 || !syncState.InSync {
		t.Fatalf("expected watermark to prevent pruned memories from appearing unlinked, got %+v", syncState)
	}
	syncRes, err := st.prog.SyncProjectProgress(ctx, scope, "", "ko")
	if err != nil || syncRes.SyncedMemories != 0 {
		t.Fatalf("expected SyncProjectProgress to resurrect 0 pruned memories, got %+v (%v)", syncRes, err)
	}

	res3, err := rawDB.ExecContext(ctx, `
		INSERT INTO memories (scope, account, category, sub_category, tags, content, confidence, pinned, active)
		VALUES (?, 'tester', 'architecture', 'storage', '', '새로 추가된 세 번째 규칙', 0.92, 0, 1)`, scope)
	if err != nil {
		t.Fatalf("insert new unlinked memory: %v", err)
	}
	newMemID, _ := res3.LastInsertId()
	if newMemID <= 2 {
		t.Fatalf("expected newMemID > 2, got %d", newMemID)
	}
	syncStateNew := st.prog.InspectProgressSync(ctx, scope, "")
	if syncStateNew.UnlinkedMemoryCount != 1 {
		t.Fatalf("expected 1 new unlinked memory above watermark, got %+v", syncStateNew)
	}

	oldPassID, _ := st.gr.RecordGuardrailLog(ctx, domain.GuardrailLogInput{
		Scope:       scope,
		Source:      "antigravity-pretool",
		ToolName:    "edit_file",
		Verdict:     domain.GuardrailVerdictPass,
		Blocked:     false,
		RuleCount:   4,
		PlanSnippet: "old pass log",
	})
	oldBlockedID, _ := st.gr.RecordGuardrailLog(ctx, domain.GuardrailLogInput{
		Scope:       scope,
		Source:      "antigravity-pretool",
		ToolName:    "run_command",
		Verdict:     domain.GuardrailVerdictRuleViolation,
		Blocked:     true,
		RuleCount:   4,
		ViolatedIDs: []int64{1},
		PlanSnippet: "old blocked violation",
		Details:     "violation detail",
	})
	recentPassID, _ := st.gr.RecordGuardrailLog(ctx, domain.GuardrailLogInput{
		Scope:       scope,
		Source:      "mcp:verify_plan",
		ToolName:    "verify_plan",
		Verdict:     domain.GuardrailVerdictPass,
		Blocked:     false,
		RuleCount:   4,
		PlanSnippet: "recent pass log",
	})
	if _, err := rawDB.ExecContext(ctx, `
		UPDATE guardrail_logs SET created_at = datetime('now', '-10 days') WHERE id IN (?, ?)`,
		oldPassID, oldBlockedID); err != nil {
		t.Fatalf("backdate guardrail_logs: %v", err)
	}

	delGrPass, err := st.gr.CleanupGuardrailLogs(ctx, scope, 7, false)
	if err != nil || delGrPass != 1 {
		t.Fatalf("expected 1 old PASS log deleted, got %d (%v)", delGrPass, err)
	}
	grAfterPass, err := st.gr.QueryGuardrailLogsPage(ctx, domain.GuardrailLogQuery{Scope: scope, State: "all", Page: 1, PageSize: 10})
	if err != nil || grAfterPass.TotalCount != 2 || grAfterPass.BlockedCount != 1 || grAfterPass.PassCount != 1 {
		t.Fatalf("expected 1 blocked + 1 recent pass remaining, got %+v (%v)", grAfterPass, err)
	}
	_ = recentPassID

	delGrBlocked, err := st.gr.CleanupGuardrailLogs(ctx, scope, 7, true)
	if err != nil || delGrBlocked != 1 {
		t.Fatalf("expected 1 old blocked log deleted with includeBlocked=true, got %d (%v)", delGrBlocked, err)
	}
	grFinal, _ := st.gr.QueryGuardrailLogsPage(ctx, domain.GuardrailLogQuery{Scope: scope, State: "all", Page: 1, PageSize: 10})
	if grFinal.TotalCount != 1 || grFinal.PassCount != 1 || grFinal.BlockedCount != 0 {
		t.Fatalf("expected only recent pass log left, got %+v", grFinal)
	}

	if _, err := rawDB.ExecContext(ctx, `
		UPDATE project_progress SET created_at = datetime('now', '-8 days'), updated_at = datetime('now', '-8 days')
		WHERE id = ?`, recentDone.ID); err != nil {
		t.Fatalf("backdate recentDone: %v", err)
	}
	polSaved, autoDeleted, err := st.prog.SetProgressRetentionDays(ctx, 7)
	if err != nil || polSaved.ProgressDays != 7 || autoDeleted != 1 {
		t.Fatalf("expected SetProgressRetentionDays(7) to auto-delete 1 expired done entry, got pol=%+v deleted=%d err=%v", polSaved, autoDeleted, err)
	}
}

type telemetrySpyEval struct {
	name      string
	sleep     time.Duration
	scoreErr  error
	mutateErr error
	verifyErr error
	mutDec    domain.MutationDecision
}

func (e *telemetrySpyEval) Name() string {
	if e.name != "" {
		return e.name
	}
	return "spy:telemetry-test"
}

func (e *telemetrySpyEval) ScoreRelevance(_ context.Context, _ string, facts []domain.Fact) ([]domain.ScoredFact, error) {
	if e.sleep > 0 {
		time.Sleep(e.sleep)
	}
	if e.scoreErr != nil {
		return nil, e.scoreErr
	}
	out := make([]domain.ScoredFact, len(facts))
	for i, f := range facts {
		out[i] = domain.ScoredFact{Fact: f, Score: 4.5, Confidence: 0.9}
	}
	return out, nil
}

func (e *telemetrySpyEval) EvaluateMutation(_ context.Context, _ string, _ string, _ []domain.Fact) (domain.MutationDecision, error) {
	if e.sleep > 0 {
		time.Sleep(e.sleep)
	}
	if e.mutateErr != nil {
		return domain.MutationDecision{}, e.mutateErr
	}
	return e.mutDec, nil
}

func (e *telemetrySpyEval) VerifyPlan(_ context.Context, _ string, _ []domain.Fact) ([]domain.PlanViolation, error) {
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
		mutDec: domain.MutationDecision{
			Category:          "architecture",
			Tier:              "rule",
			Cardinality:       "additive",
			IsPermanent:       0.9,
			IsGlobalInvariant: 0.2,
			Action:            "INSERT",
			Confidence:        0.95,
		},
	}
	st := newTestStack(t, ":memory:", eval)

	repo := t.TempDir()
	scope := "project:" + strings.ToLower(filepath.Base(repo))

	if _, err := st.mem.CommitMemory(ctx, "Always use WAL mode for SQLite databases", "architecture", scope); err != nil {
		t.Fatalf("CommitMemory 1: %v", err)
	}
	if _, err := st.mem.CommitMemory(ctx, "Run gofmt on all modified Go files", "architecture", scope); err != nil {
		t.Fatalf("CommitMemory 2: %v", err)
	}

	if _, err := st.mem.QuerySessionMemory(ctx, "", "sqlite database setup", "", scope, false); err != nil {
		t.Fatalf("QuerySessionMemory: %v", err)
	}

	if _, err := st.gr.VerifyPlan(ctx, "Enable WAL mode for sqlite", scope); err != nil {
		t.Fatalf("VerifyPlan: %v", err)
	}

	eval.sleep = 25 * time.Millisecond
	eval.verifyErr = context.DeadlineExceeded
	expCtx, expCancel := context.WithTimeout(ctx, 10*time.Millisecond)
	defer expCancel()
	if _, _, _, vErr := st.gr.EvaluatePlanViolations(expCtx, "Skip WAL mode", scope, 0); vErr == nil {
		t.Fatalf("expected EvaluatePlanViolations error on expired context")
	}
	eval.sleep = 3 * time.Millisecond
	eval.verifyErr = nil

	eval.mutateErr = errors.New("upstream 500")
	if _, mErr := st.mem.CommitMemory(ctx, "Broken remote rule", "architecture", scope); mErr == nil {
		t.Fatalf("expected CommitMemory error when mutateErr set")
	}
	eval.mutateErr = nil

	rep, err := st.mem.Router().SummarizeTelemetry(ctx, 24)
	if err != nil {
		t.Fatalf("SummarizeTelemetry: %v", err)
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
	mut := rep.ByTask[string(domain.TaskMutation)]
	if mut.Calls != 3 || mut.OKCount != 2 || mut.ErrorCount != 1 || mut.LastEngine != "gemini:gemini-3.5-flash-lite" {
		t.Fatalf("unexpected mutation telemetry stats: %+v", mut)
	}
	rel := rep.ByTask[string(domain.TaskRelevance)]
	if rel.Calls != 1 || rel.OKCount != 1 || rel.AvgItems != 2.0 || rel.AvgElapsedMs <= 0 {
		t.Fatalf("unexpected relevance telemetry stats: %+v", rel)
	}
	ver := rep.ByTask[string(domain.TaskVerify)]
	if ver.Calls != 2 || ver.OKCount != 1 || ver.TimeoutCount != 1 || ver.P95ElapsedMs <= 0 {
		t.Fatalf("unexpected verify telemetry stats: %+v", ver)
	}

	page, err := st.gr.QueryGuardrailLogsPage(ctx, domain.GuardrailLogQuery{Scope: scope, PageSize: 10})
	if err != nil {
		t.Fatalf("QueryGuardrailLogsPage: %v", err)
	}
	if len(page.Records) == 0 || page.Records[0].ElapsedMs <= 0 {
		t.Fatalf("expected guardrail_log ElapsedMs > 0, got %+v", page.Records)
	}

	if err := st.mem.Router().ResetTelemetry(ctx); err != nil {
		t.Fatalf("ResetTelemetry: %v", err)
	}
	afterReset, err := st.mem.Router().SummarizeTelemetry(ctx, 24)
	if err != nil {
		t.Fatalf("SummarizeTelemetry after reset: %v", err)
	}
	if afterReset.TotalCalls != 0 {
		t.Fatalf("expected TotalCalls=0 after reset, got %d", afterReset.TotalCalls)
	}
}
