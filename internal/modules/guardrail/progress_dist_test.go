package guardrail_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"memorize/internal/domain"
	"memorize/internal/inference/providers"
	"memorize/internal/modules/memory"
)

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

	ov1, err := st.prog.GetProgressOverview(ctx, scope, "", "all", repoDir)
	if err != nil {
		t.Fatalf("GetProgressOverview 1: %v", err)
	}
	if ov1.TotalCount != 1 || len(ov1.Records[0].MemoryIDs) != 2 || ov1.Records[0].Status != domain.ProgressStatusDone {
		t.Fatalf("expected 1 coalesced done progress row linking [#1 #2], got %+v", ov1.Records)
	}

	manualRec, err := st.prog.RecordProgress(ctx, domain.ProgressInput{
		Scope:       scope,
		Category:    "dashboard",
		SubCategory: "ui",
		Status:      domain.ProgressStatusInProgress,
		Summary:     "SPECIAL_PROGRESS_ONLY_MARKER_XYZ 작업 진행 중",
		NextStep:    "단위 테스트 작성",
		Source:      "agent",
	})
	if err != nil {
		t.Fatalf("RecordProgress: %v", err)
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
	ovMid, _ := st.prog.GetProgressOverview(ctx, scope, "", "all", repoDir)
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

	if err := st.prog.UpdateProgressStatus(ctx, manualRec.ID, domain.ProgressStatusDone); err != nil {
		t.Fatalf("UpdateProgressStatus: %v", err)
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

	activeRec, err := st.prog.RecordProgress(ctx, domain.ProgressInput{
		Scope:    scope,
		Track:    "대시보드 히스토리 타임라인 개편",
		Status:   domain.ProgressStatusInProgress,
		Summary:  "상단 진행 트랙 보드 및 날짜별 세로 히스토리 연동 중",
		NextStep: "트랙 클릭 필터 검증",
		Source:   "agent",
	})
	if err != nil {
		t.Fatalf("RecordProgress active track: %v", err)
	}
	ovFinal, err := st.prog.GetProgressOverview(ctx, scope, "", "all", repoDir)
	if err != nil {
		t.Fatalf("GetProgressOverview final: %v", err)
	}
	if len(ovFinal.ActiveTracks) != 1 || ovFinal.ActiveTracks[0].LatestID != activeRec.ID || ovFinal.ActiveTracks[0].Track != "대시보드 히스토리 타임라인 개편" {
		t.Fatalf("expected only ongoing track in ActiveTracks, got %+v", ovFinal.ActiveTracks)
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
	filteredOv, err := st.prog.GetProgressOverviewByTrack(ctx, scope, "대시보드 히스토리 타임라인 개편", "", "all", repoDir)
	if err != nil || len(filteredOv.Records) != 1 || filteredOv.Records[0].ID != activeRec.ID {
		t.Fatalf("expected track filter to isolate 1 record, got %+v (%v)", filteredOv.Records, err)
	}

	if _, err := st.prog.RecordProgress(ctx, domain.ProgressInput{
		Scope:   scope,
		Track:   "대시보드 히스토리 타임라인 개편",
		Status:  domain.ProgressStatusPlanned,
		Summary: "모바일 반응형 점검",
		Source:  "agent",
	}); err != nil {
		t.Fatalf("RecordProgress planned: %v", err)
	}
	longRule := "진행 현황 히스토리 및 상단 3분할 상태 보드에서는 긴 기억 원문이 추가되거나 수정되더라도 말줄임표 없이 끝까지 원문 전체가 온전하게 표시되어야 하며 어떠한 경우에도 문장 끝이 잘리지 않아야 한다 (검증 마커: FULL_TAIL_OK_999)"
	if _, err := st.mem.CommitMemoryFull(ctx, longRule, "architecture", "dashboard", "", scope); err != nil {
		t.Fatalf("CommitMemoryFull longRule: %v", err)
	}

	for i := 1; i <= 12; i++ {
		if _, err := st.prog.RecordProgress(ctx, domain.ProgressInput{
			Scope:   scope,
			Track:   fmt.Sprintf("완료 트랙 #%d", i),
			Status:  domain.ProgressStatusDone,
			Summary: fmt.Sprintf("자동 기록 완료 마일스톤 %d", i),
			Source:  "agent",
		}); err != nil {
			t.Fatalf("RecordProgress milestone %d: %v", i, err)
		}
	}

	page1, err := st.prog.GetProgressOverviewPaged(ctx, scope, "", "", "all", repoDir, 1, 10)
	if err != nil {
		t.Fatalf("GetProgressOverviewPaged page 1: %v", err)
	}
	page2, err := st.prog.GetProgressOverviewPaged(ctx, scope, "", "", "all", repoDir, 2, 10)
	if err != nil {
		t.Fatalf("GetProgressOverviewPaged page 2: %v", err)
	}
	if len(page1.Records) != 10 || page1.TotalPages < 2 || len(page2.Records) == 0 {
		t.Fatalf("expected paginated history (page1=10, page2>0, totalPages>=2), got page1=%d page2=%d totalPages=%d",
			len(page1.Records), len(page2.Records), page1.TotalPages)
	}
	if len(page2.DoingTracks) != 1 || len(page2.PlannedTracks) != 1 || len(page2.NextStepItems) != 1 || len(page2.RecentDone) != 5 {
		t.Fatalf("expected top 3-lane board to preserve full-scope Doing(1)/Planned(1)/Next(1)/RecentDone(5) on page 2, got doing=%d planned=%d next=%d recentDone=%d",
			len(page2.DoingTracks), len(page2.PlannedTracks), len(page2.NextStepItems), len(page2.RecentDone))
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
	if !strings.Contains(startOut, "PROJECT PROGRESS STATE") || !strings.Contains(startOut, "상단 진행 트랙 보드 및 날짜별 세로 히스토리 연동 중") {
		t.Fatalf("expected SessionStart hook to inject ongoing work track state for seamless resume, got: %s", startOut)
	}
	afterHookSync := st.prog.InspectProgressSync(ctx, scope, repoDir)
	if !afterHookSync.InSync || len(afterHookSync.PendingCommits) != 0 {
		t.Fatalf("expected SessionStart hook to auto-sync new Git commit into project_progress, got %+v", afterHookSync)
	}
}

func TestParallelGuardrailHookAndConcurrentCommitBatch(t *testing.T) {
	ctx := context.Background()

	var (
		barrierOn     atomic.Bool
		mutCalls      atomic.Int32
		evalEntered   = make(chan struct{}, 1)
		verifyEntered = make(chan struct{}, 1)
		bothEntered   = make(chan struct{})
		once          sync.Once
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
			if barrierOn.Load() {
				select {
				case verifyEntered <- struct{}{}:
				default:
				}
				select {
				case <-evalEntered:
					once.Do(func() { close(bothEntered) })
				case <-time.After(500 * time.Millisecond):
				}
			}
			content = `{"items":[]}`
		case strings.Contains(prompt, "Current Task"):
			content = `{"items":[{"id":1,"score":4.5,"confidence":0.93}]}`
		default:
			mutCalls.Add(1)
			if barrierOn.Load() {
				select {
				case evalEntered <- struct{}{}:
				default:
				}
				select {
				case <-verifyEntered:
					once.Do(func() { close(bothEntered) })
				case <-time.After(500 * time.Millisecond):
				}
			}
			content = `{"category":"architecture","sub_category":"core","tags":[],"tier":"invariant","cardinality":"singleton","is_permanent":0.93,"is_global_invariant":0.91,"confidence":0.94,"action":"INSERT","conflict_fact_ids":[]}`
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

	barrierOn.Store(true)
	guardPayload, _ := json.Marshal(map[string]any{
		"session_id": "sess-parallel",
		"cwd":        workDir,
		"tool_name":  "Bash",
		"tool_input": map[string]any{
			"command": "CGO_ENABLED=1 go build ./...",
		},
	})
	guardOut, _, err := st.gr.ExecuteHook(ctx, "claude-pretool", guardPayload, workDir)
	if err != nil {
		t.Fatalf("ExecuteHook claude-pretool: %v", err)
	}
	barrierOn.Store(false)

	select {
	case <-bothEntered:
	default:
		t.Fatalf("expected EvaluateMutation and VerifyPlan HTTP requests to execute concurrently in guardrail hook")
	}
	if !strings.Contains(guardOut, "PRE-MUTATE GATE") {
		t.Fatalf("expected PRE-MUTATE GATE block on un-committed rule, got:\n%s", guardOut)
	}

	// Part B: Verify CommitBatch evaluates multiple unique candidates concurrently while skipping duplicates in batch
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
	if mutCalls.Load() != 4 {
		t.Fatalf("expected 4 unique EvaluateMutation calls (skipping 1 duplicate in batch), got %d", mutCalls.Load())
	}
	if !strings.Contains(res, "SKIPPED (duplicate in batch") {
		t.Fatalf("expected duplicate-in-batch notice in CommitBatch output, got:\n%s", res)
	}
	recs, _ := batchSvc.ListRecords(ctx, "project:batch")
	if len(recs) != 4 {
		t.Fatalf("expected 4 committed records, got %d", len(recs))
	}
}

type guardrailAuditEval struct {
	violate bool
}

func (e *guardrailAuditEval) Name() string { return "guardrail-audit-eval" }
func (e *guardrailAuditEval) ScoreRelevance(_ context.Context, _ string, facts []domain.Fact) ([]domain.ScoredFact, error) {
	out := make([]domain.ScoredFact, len(facts))
	for i, f := range facts {
		out[i] = domain.ScoredFact{Fact: f, Score: 4.5, Confidence: 0.92}
	}
	return out, nil
}
func (e *guardrailAuditEval) EvaluateMutation(_ context.Context, _ string, _ string, _ []domain.Fact) (domain.MutationDecision, error) {
	return domain.MutationDecision{
		Category:          "architecture",
		Tier:              "invariant",
		IsPermanent:       0.95,
		IsGlobalInvariant: 0.92,
		Action:            "INSERT",
		Confidence:        0.96,
	}, nil
}
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
	eval := &guardrailAuditEval{}
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
