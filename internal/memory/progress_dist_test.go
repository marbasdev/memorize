package memory_test

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

	"memorize/internal/memory"
	"memorize/internal/memory/adapters"
)

type progressSyncEval struct {
	stubEval
}

func (p *progressSyncEval) SummarizeCommitProgress(_ context.Context, _ string, _ string, commits []memory.GitCommitBrief) ([]memory.CommitProgressItem, error) {
	out := make([]memory.CommitProgressItem, 0, len(commits))
	for _, c := range commits {
		out = append(out, memory.CommitProgressItem{
			SHA:         c.SHA,
			Category:    "architecture",
			SubCategory: "dashboard",
			Summary:     "대시보드 진행상황 동기화 구현: " + c.Subject,
		})
	}
	return out, nil
}

func (p *progressSyncEval) SuggestConsolidations(_ context.Context, _ string, facts []memory.Fact) ([]memory.ConsolidationProposal, error) {
	if len(facts) < 2 {
		return nil, nil
	}
	ids := []int64{facts[0].ID, facts[1].ID}
	return []memory.ConsolidationProposal{
		{
			SourceIDs:     ids,
			Category:      facts[0].Category,
			MergedContent: "통합된 카테고리 규칙 요약",
			Reason:        "동일 카테고리 중복 통합",
		},
	}, nil
}

func (p *progressSyncEval) SuggestTaxonomyRebalance(_ context.Context, _ string, facts []memory.Fact) ([]memory.TaxonomyRebalanceItem, error) {
	var out []memory.TaxonomyRebalanceItem
	for _, f := range facts {
		if f.SubCategory == "" {
			out = append(out, memory.TaxonomyRebalanceItem{
				ID:             f.ID,
				NewCategory:    f.Category,
				NewSubCategory: "observability",
				Reason:         "미분류 항목 의미 기반 세부 분류 배정",
			})
		}
	}
	return out, nil
}

func TestCategoryDistributionAndScopeStats(t *testing.T) {
	ctx := context.Background()
	eval := &progressSyncEval{}
	eval.dec = memory.MutationDecision{
		IsPermanent:       0.95,
		IsGlobalInvariant: 0.90,
		Action:            "INSERT",
		Confidence:        0.93,
	}

	svc, err := memory.NewService(":memory:", eval)
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	defer svc.Close()

	// 1. Primary category ("architecture": 6 active rules, including 1 unclassified sub_category) + shared sub-category ("api")
	for i := 1; i <= 6; i++ {
		sub := "core"
		if i == 5 {
			sub = ""
		} else if i == 6 {
			sub = "api"
		}
		if _, err := svc.CommitMemoryFull(ctx, fmt.Sprintf("아키텍처 불변 설계 원칙 #%d", i), "architecture", sub, "", "project:dist"); err != nil {
			t.Fatalf("CommitMemoryFull arch %d: %v", i, err)
		}
	}

	// 2. Secondary category ("convention": 3 sub-categories with 1 rule each)
	for _, sub := range []string{"naming", "imports", "comments"} {
		if _, err := svc.CommitMemoryFull(ctx, "코드 작성 컨벤션 원칙 "+sub, "convention", sub, "", "project:dist"); err != nil {
			t.Fatalf("CommitMemoryFull convention/%s: %v", sub, err)
		}
	}

	// 3. Cross-category duplicate sub-category + brand-new category/sub-category auto-registration ("telemetry/api")
	if _, err := svc.CommitMemoryFull(ctx, "텔레메트리 API 수집 불변 원칙", "telemetry", "api", "", "project:dist"); err != nil {
		t.Fatalf("CommitMemoryFull telemetry/api: %v", err)
	}

	// 4. Superseded/forgotten history in "testing" (1 active rule, 3 forgotten rules)
	var tombIDs []int64
	for i := 1; i <= 4; i++ {
		if _, err := svc.CommitMemoryFull(ctx, fmt.Sprintf("단위 테스트 격리 불변 원칙 #%d", i), "testing", "unit", "", "project:dist"); err != nil {
			t.Fatalf("CommitMemoryFull testing %d: %v", i, err)
		}
		if i <= 3 {
			recs, _ := svc.ListRecords(ctx, "project:dist")
			tombIDs = append(tombIDs, recs[0].ID)
		}
	}
	if _, err := svc.ForgetMemories(memory.WithAdminBypass(ctx), tombIDs); err != nil {
		t.Fatalf("ForgetMemories: %v", err)
	}

	// 5. Separate scope ("project:other": 2 active rules) to verify scope-isolated stats
	for i := 1; i <= 2; i++ {
		if _, err := svc.CommitMemoryFull(ctx, fmt.Sprintf("타 프로젝트 스키마 불변 원칙 #%d", i), "database", "schema", "", "project:other"); err != nil {
			t.Fatalf("CommitMemoryFull other %d: %v", i, err)
		}
	}

	distStats, err := svc.GetDashboardStatsForScope(ctx, "project:dist")
	if err != nil {
		t.Fatalf("GetDashboardStatsForScope(project:dist): %v", err)
	}
	if distStats.ActiveCount != 11 || distStats.TombCount != 3 || distStats.TotalCount != 14 {
		t.Fatalf("expected project:dist stats active=11 tomb=3 total=14, got %+v", distStats)
	}

	otherStats, err := svc.GetDashboardStatsForScope(ctx, "project:other")
	if err != nil || otherStats.ActiveCount != 2 || otherStats.TotalCount != 2 {
		t.Fatalf("expected project:other stats active=2 total=2, got %+v (%v)", otherStats, err)
	}

	globalStats, err := svc.GetDashboardStatsForScope(ctx, "")
	if err != nil || globalStats.ActiveCount != 13 || globalStats.TombCount != 3 {
		t.Fatalf("expected global stats active=13 tomb=3, got %+v (%v)", globalStats, err)
	}

	// Verify 2-level proportional distribution + unclassified bucket (__none__)
	dist, err := svc.GetCategoryDistribution(ctx, "project:dist")
	if err != nil {
		t.Fatalf("GetCategoryDistribution: %v", err)
	}
	if dist.TotalActive != 11 || dist.TotalTomb != 3 || dist.UncategorizedTotal != 1 {
		t.Fatalf("unexpected distribution totals: %+v", dist)
	}
	if len(dist.DuplicateSubCategories) != 1 || dist.DuplicateSubCategories[0] != "api" {
		t.Fatalf("expected duplicate sub-category ['api'], got %v", dist.DuplicateSubCategories)
	}

	byCat := map[string]memory.CategoryDistItem{}
	totalCatAreaPct := 0.0
	for _, c := range dist.Categories {
		if c.W <= 0 || c.H <= 0 {
			t.Fatalf("expected 2D treemap W>0 and H>0 for category %s, got %+v", c.Category, c)
		}
		totalCatAreaPct += (c.W * c.H) / 100.0
		byCat[c.Category] = c
	}
	if totalCatAreaPct < 99.0 || totalCatAreaPct > 101.0 {
		t.Fatalf("expected 2D category treemap areas to partition 100%% of fixed canvas, got %.2f%%", totalCatAreaPct)
	}
	archDist := byCat["architecture"]
	convDist := byCat["convention"]
	if archDist.ActiveCount != 6 || archDist.UncategorizedCount != 1 || archDist.FlexWeight != 6 {
		t.Fatalf("expected architecture active=6 uncat=1 flex=6, got %+v", archDist)
	}
	if archDist.W*archDist.H <= convDist.W*convDist.H {
		t.Fatalf("expected architecture 2D area (%.1f) > convention 2D area (%.1f)", archDist.W*archDist.H, convDist.W*convDist.H)
	}
	foundUncatTile := false
	subAreaPct := 0.0
	for _, s := range archDist.SubItems {
		if s.W <= 0 || s.H <= 0 {
			t.Fatalf("expected 2D treemap W>0 and H>0 for sub-item %s, got %+v", s.SubCategory, s)
		}
		subAreaPct += (s.W * s.H) / 100.0
		if s.IsUncategorized && s.SubCategory == memory.UncategorizedSubKey && s.ActiveCount == 1 {
			foundUncatTile = true
		}
	}
	if !foundUncatTile || subAreaPct < 99.0 || subAreaPct > 101.0 {
		t.Fatalf("expected unclassified sub-tile (__none__) and 100%% sub-area partition (got %.2f%%): %+v", subAreaPct, archDist.SubItems)
	}

	// Verify filtering by SubCategory == "__none__" returns only unclassified records
	uncatPage, err := svc.QueryRecordsPage(ctx, memory.RecordQuery{
		Scope:       "project:dist",
		Category:    "architecture",
		SubCategory: memory.UncategorizedSubKey,
		State:       "active",
		Page:        1,
		PageSize:    20,
	})
	if err != nil || len(uncatPage.Records) != 1 || uncatPage.Records[0].SubCategory != "" {
		t.Fatalf("expected 1 unclassified architecture record for sub_category=__none__, got %+v (%v)", uncatPage.Records, err)
	}
	origUncat := uncatPage.Records[0]

	// Verify append-only preset_categories registration preserved defaults and added "telemetry":["api"]
	var savedTax map[string][]string
	if err := json.Unmarshal([]byte(svc.GetConfigMap(ctx)["preset_categories"]), &savedTax); err != nil {
		t.Fatalf("unmarshal preset_categories: %v", err)
	}
	if len(savedTax["architecture"]) == 0 || len(savedTax["security"]) == 0 {
		t.Fatalf("expected existing default preset categories to remain intact (zero deletion), got %+v", savedTax)
	}
	if len(savedTax["telemetry"]) != 1 || savedTax["telemetry"][0] != "api" {
		t.Fatalf("expected new category telemetry/api appended to preset_categories, got %+v", savedTax["telemetry"])
	}

	// Verify non-destructive AI taxonomy rebalance updates sub_category without altering content or pin state
	rebItems, _, err := svc.SuggestTaxonomyRebalance(memory.WithAdminBypass(ctx), "project:dist", "architecture")
	if err != nil || len(rebItems) != 1 || rebItems[0].ID != origUncat.ID || rebItems[0].NewSubCategory != "observability" || !rebItems[0].IsNewTaxonomy {
		t.Fatalf("expected 1 taxonomy rebalance suggestion for #%d -> architecture/observability, got %+v (%v)", origUncat.ID, rebItems, err)
	}
	appliedCnt, err := svc.ApplyTaxonomyRebalance(memory.WithAdminBypass(ctx), "project:dist", rebItems)
	if err != nil || appliedCnt != 1 {
		t.Fatalf("ApplyTaxonomyRebalance failed: count=%d err=%v", appliedCnt, err)
	}
	afterRecs, err := svc.GetRecordsByIDs(ctx, []int64{origUncat.ID})
	if err != nil || len(afterRecs) != 1 {
		t.Fatalf("GetRecordsByIDs after rebalance: %v", err)
	}
	if afterRecs[0].Content != origUncat.Content || afterRecs[0].Pinned != origUncat.Pinned || afterRecs[0].SubCategory != "observability" {
		t.Fatalf("expected non-destructive sub_category update only, before=%+v after=%+v", origUncat, afterRecs[0])
	}

	// Category-filtered AI consolidation plan only inspects the requested category
	proposals, _, err := svc.SuggestConsolidationPlanFiltered(memory.WithAdminBypass(ctx), "project:dist", "convention")
	if err != nil || len(proposals) != 1 || !strings.HasPrefix(proposals[0].Category, "convention") {
		t.Fatalf("expected 1 convention-scoped proposal, got %+v (%v)", proposals, err)
	}
}

func TestSeparatedProjectProgressLifecycleAndGitSync(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	ctx := context.Background()
	eval := &progressSyncEval{}
	eval.dec = memory.MutationDecision{
		IsPermanent:       0.95,
		IsGlobalInvariant: 0.90,
		Action:            "INSERT",
		Confidence:        0.92,
	}

	svc, err := memory.NewService(":memory:", eval)
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	defer svc.Close()

	// Create temporary Git repository with 2 commits
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
	svc.RememberRepoPath(ctx, scope, repoDir)
	_ = svc.SetSyncedCommit(ctx, scope, baseHead)

	// 1. Two rapid commits in the same category/sub_category coalesce into 1 completed memory-linked progress row (status=done, zero needs_sync)
	if _, err := svc.CommitMemoryFull(ctx, "순수 Go SQLite 드라이버만 사용한다", "architecture", "storage", "", scope); err != nil {
		t.Fatalf("Commit 1: %v", err)
	}
	if _, err := svc.CommitMemoryFull(ctx, "단일 연결 WAL 모드로 잠금 경합을 방지한다", "architecture", "storage", "", scope); err != nil {
		t.Fatalf("Commit 2: %v", err)
	}

	ov1, err := svc.GetProgressOverview(ctx, scope, "", "all", repoDir)
	if err != nil {
		t.Fatalf("GetProgressOverview 1: %v", err)
	}
	if ov1.TotalCount != 1 || len(ov1.Records[0].MemoryIDs) != 2 || ov1.Records[0].Status != memory.ProgressStatusDone {
		t.Fatalf("expected 1 coalesced done progress row linking [#1 #2], got %+v", ov1.Records)
	}

	// 2. Agent progress record is completely isolated from memories prompt injection & VerifyActionPlan guardrails
	manualRec, err := svc.RecordProgress(ctx, memory.ProgressInput{
		Scope:       scope,
		Category:    "dashboard",
		SubCategory: "ui",
		Status:      memory.ProgressStatusInProgress,
		Summary:     "SPECIAL_PROGRESS_ONLY_MARKER_XYZ 작업 진행 중",
		NextStep:    "단위 테스트 작성",
		Source:      "agent",
	})
	if err != nil {
		t.Fatalf("RecordProgress: %v", err)
	}

	memPrompt, err := svc.QuerySessionMemory(ctx, "sess-prog-check", "SPECIAL_PROGRESS_ONLY_MARKER_XYZ 확인", "", scope, false)
	if err != nil {
		t.Fatalf("QuerySessionMemory: %v", err)
	}
	if strings.Contains(memPrompt, "SPECIAL_PROGRESS_ONLY_MARKER_XYZ") {
		t.Fatalf("expected project_progress to NEVER leak into session memory prompt, got:\n%s", memPrompt)
	}

	verifyOut, err := svc.VerifyPlan(ctx, "edit SPECIAL_PROGRESS_ONLY_MARKER_XYZ", scope)
	if err != nil || !strings.HasPrefix(verifyOut, "PASS:") {
		t.Fatalf("expected VerifyPlan to ignore project_progress entries, got %q (%v)", verifyOut, err)
	}

	// 3. Delete coalesced progress row and add 2 more rules (4 total in architecture/storage) + a Git commit to verify full details without truncation
	if err := svc.DeleteProgress(ctx, ov1.Records[0].ID); err != nil {
		t.Fatalf("DeleteProgress: %v", err)
	}
	if _, err := svc.CommitMemoryFull(ctx, "세 번째 스토리지 마이그레이션 불변 원칙", "architecture", "storage", "", scope); err != nil {
		t.Fatalf("Commit 3: %v", err)
	}
	if _, err := svc.CommitMemoryFull(ctx, "네 번째 백업 스냅샷 무결성 검증 원칙", "architecture", "storage", "", scope); err != nil {
		t.Fatalf("Commit 4: %v", err)
	}
	ovMid, _ := svc.GetProgressOverview(ctx, scope, "", "all", repoDir)
	for _, r := range ovMid.Records {
		if r.Source == "memory" {
			_ = svc.DeleteProgress(ctx, r.ID)
		}
	}

	_ = os.WriteFile(filepath.Join(repoDir, "dashboard.html"), []byte("<div>progress</div>\n"), 0o644)
	runGitCmd("add", ".")
	runGitCmd("commit", "-m", "feat(dashboard): add progress tab")

	preSync := svc.InspectProgressSync(ctx, scope, repoDir)
	if preSync.InSync || preSync.UnlinkedMemoryCount != 4 || len(preSync.PendingCommits) != 1 {
		t.Fatalf("expected InSync=false with 4 unlinked memories and 1 pending commit, got %+v", preSync)
	}

	// Close agent item to done before verifying SyncProjectProgress
	if err := svc.UpdateProgressStatus(ctx, manualRec.ID, memory.ProgressStatusDone); err != nil {
		t.Fatalf("UpdateProgressStatus: %v", err)
	}

	syncRes, err := svc.SyncProjectProgress(ctx, scope, repoDir, "ko")
	if err != nil {
		t.Fatalf("SyncProjectProgress: %v", err)
	}
	if syncRes.SyncedMemories != 4 || syncRes.SyncedCommits != 1 {
		t.Fatalf("expected SyncedMemories=4 SyncedCommits=1, got %+v", syncRes)
	}

	postSync := svc.InspectProgressSync(ctx, scope, repoDir)
	if !postSync.InSync || postSync.UnlinkedMemoryCount != 0 || len(postSync.PendingCommits) != 0 {
		t.Fatalf("expected InSync=true after SyncProjectProgress, got %+v", postSync)
	}

	// 4. Verify active work tracks vs completed timeline & full 4-item expandable DetailsLines
	activeRec, err := svc.RecordProgress(ctx, memory.ProgressInput{
		Scope:    scope,
		Track:    "대시보드 히스토리 타임라인 개편",
		Status:   memory.ProgressStatusInProgress,
		Summary:  "상단 진행 트랙 보드 및 날짜별 세로 히스토리 연동 중",
		NextStep: "트랙 클릭 필터 검증",
		Source:   "agent",
	})
	if err != nil {
		t.Fatalf("RecordProgress active track: %v", err)
	}
	ovFinal, err := svc.GetProgressOverview(ctx, scope, "", "all", repoDir)
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
	filteredOv, err := svc.GetProgressOverviewByTrack(ctx, scope, "대시보드 히스토리 타임라인 개편", "", "all", repoDir)
	if err != nil || len(filteredOv.Records) != 1 || filteredOv.Records[0].ID != activeRec.ID {
		t.Fatalf("expected track filter to isolate 1 record, got %+v (%v)", filteredOv.Records, err)
	}

	// 5. Verify 3-lane read-only board (DoingTracks / PlannedTracks + NextStepItems / RecentDone),
	// zero ellipsis truncation on >150-rune memory commits, SessionStart active-progress & Git auto-sync, and pagination.
	if _, err := svc.RecordProgress(ctx, memory.ProgressInput{
		Scope:   scope,
		Track:   "대시보드 히스토리 타임라인 개편",
		Status:  memory.ProgressStatusPlanned,
		Summary: "모바일 반응형 점검",
		Source:  "agent",
	}); err != nil {
		t.Fatalf("RecordProgress planned: %v", err)
	}
	longRule := "진행 현황 히스토리 및 상단 3분할 상태 보드에서는 긴 기억 원문이 추가되거나 수정되더라도 말줄임표 없이 끝까지 원문 전체가 온전하게 표시되어야 하며 어떠한 경우에도 문장 끝이 잘리지 않아야 한다 (검증 마커: FULL_TAIL_OK_999)"
	if _, err := svc.CommitMemoryFull(ctx, longRule, "architecture", "dashboard", "", scope); err != nil {
		t.Fatalf("CommitMemoryFull longRule: %v", err)
	}

	// Add 12 completed milestones to verify pagination across page 1 and page 2 (pageSize=10)
	for i := 1; i <= 12; i++ {
		if _, err := svc.RecordProgress(ctx, memory.ProgressInput{
			Scope:   scope,
			Track:   fmt.Sprintf("완료 트랙 #%d", i),
			Status:  memory.ProgressStatusDone,
			Summary: fmt.Sprintf("자동 기록 완료 마일스톤 %d", i),
			Source:  "agent",
		}); err != nil {
			t.Fatalf("RecordProgress milestone %d: %v", i, err)
		}
	}

	page1, err := svc.GetProgressOverviewPaged(ctx, scope, "", "", "all", repoDir, 1, 10)
	if err != nil {
		t.Fatalf("GetProgressOverviewPaged page 1: %v", err)
	}
	page2, err := svc.GetProgressOverviewPaged(ctx, scope, "", "", "all", repoDir, 2, 10)
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

	// Verify SessionStart hook injects ongoing work tracks AND auto-syncs new Git commits into project_progress
	_ = os.WriteFile(filepath.Join(repoDir, "auto.go"), []byte("package main\n// auto\n"), 0o644)
	runGitCmd("add", ".")
	runGitCmd("commit", "-m", "feat(progress): auto sync on session start")
	startPayload := fmt.Sprintf(`{"conversationId":"sess-next-window","workspacePaths":[%q],"lastUserInput":"이어서 진행하자"}`, repoDir)
	startOut, startCode, err := svc.ExecuteHook(ctx, "antigravity-start", []byte(startPayload), "")
	if err != nil || startCode != 0 {
		t.Fatalf("ExecuteHook antigravity-start: code=%d err=%v", startCode, err)
	}
	if !strings.Contains(startOut, "PROJECT PROGRESS STATE") || !strings.Contains(startOut, "상단 진행 트랙 보드 및 날짜별 세로 히스토리 연동 중") {
		t.Fatalf("expected SessionStart hook to inject ongoing work track state for seamless resume, got: %s", startOut)
	}
	afterHookSync := svc.InspectProgressSync(ctx, scope, repoDir)
	if !afterHookSync.InSync || len(afterHookSync.PendingCommits) != 0 {
		t.Fatalf("expected SessionStart hook to auto-sync new Git commit into project_progress, got %+v", afterHookSync)
	}
}

func TestUpdateMemoryFullSessionInlineNotice(t *testing.T) {
	ctx := context.Background()
	eval := &stubEval{
		dec: memory.MutationDecision{
			Category:          "architecture",
			IsPermanent:       0.95,
			IsGlobalInvariant: 0.90,
			Action:            "INSERT",
			Confidence:        0.94,
		},
	}
	svc, err := memory.NewService(":memory:", eval)
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	defer svc.Close()

	if _, err := svc.CommitMemory(ctx, "API timeout defaults to 5s", "architecture", "project:edit"); err != nil {
		t.Fatalf("CommitMemory: %v", err)
	}

	// Turn 1: Session receives initial text
	t1, err := svc.QuerySessionMemory(ctx, "sess-edit", "configure HTTP client", "", "project:edit", false)
	if err != nil || !strings.Contains(t1, "API timeout defaults to 5s") {
		t.Fatalf("Turn 1 expected initial memory, got:\n%s (%v)", t1, err)
	}

	// Two consecutive in-place updates before session's next turn must preserve earliest prev_content ("5s")
	adminCtx := memory.WithAdminBypass(ctx)
	if err := svc.UpdateMemoryFull(adminCtx, 1, "project:edit", "architecture", nil, "API timeout defaults to 15s", nil); err != nil {
		t.Fatalf("UpdateMemoryFull 1: %v", err)
	}
	if err := svc.UpdateMemoryFull(adminCtx, 1, "project:edit", "architecture", nil, "API timeout defaults to 20s", nil); err != nil {
		t.Fatalf("UpdateMemoryFull 2: %v", err)
	}

	// Turn 2 for existing session: receives 1-time inline update notice referencing original "5s"
	t2, err := svc.QuerySessionMemory(ctx, "sess-edit", "configure HTTP client", "", "project:edit", false)
	if err != nil {
		t.Fatalf("Turn 2 QuerySessionMemory: %v", err)
	}
	if !strings.Contains(t2, "API timeout defaults to 20s") || !strings.Contains(t2, "Updated #1 — replaces earlier text: API timeout defaults to 5s") {
		t.Fatalf("expected Turn 2 to include updated text + earliest prev_content notice, got:\n%s", t2)
	}

	// Turn 3 for existing session: notice was consumed once, so deduplicated 1-liner is returned
	t3, err := svc.QuerySessionMemory(ctx, "sess-edit", "configure HTTP client", "", "project:edit", false)
	if err != nil || !strings.HasPrefix(t3, "No new memories") {
		t.Fatalf("expected Turn 3 to be deduplicated after consuming update notice, got:\n%s", t3)
	}

	// Fresh session: receives clean latest text without inline update notice
	tFresh, err := svc.QuerySessionMemory(ctx, "sess-fresh", "configure HTTP client", "", "project:edit", false)
	if err != nil || !strings.Contains(tFresh, "API timeout defaults to 20s") || strings.Contains(tFresh, "Updated #1") {
		t.Fatalf("expected fresh session to receive clean text without Updated notice, got:\n%s", tFresh)
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

	adapter := adapters.NewOpenAICompatibleAdapter(llmSrv.URL+"/v1", "qwen3:4b", "")
	svc, err := memory.NewService(":memory:", adapter)
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	defer svc.Close()

	workDir := filepath.Join(t.TempDir(), "conc")
	_ = os.MkdirAll(workDir, 0o755)

	if _, err := svc.CommitMemory(ctx, "Never enable CGO in Go build commands", "architecture", "project:conc"); err != nil {
		t.Fatalf("seed rule: %v", err)
	}

	turnPayload, _ := json.Marshal(map[string]any{
		"session_id": "sess-parallel",
		"cwd":        workDir,
		"prompt":     "앞으로 모든 API 응답에는 반드시 X-Trace-ID 헤더를 포함해줘 그리고 빌드 실행해줘",
	})
	if _, _, err := svc.ExecuteHook(ctx, "claude-turn", turnPayload, workDir); err != nil {
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
	guardOut, _, err := svc.ExecuteHook(ctx, "claude-pretool", guardPayload, workDir)
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

	// Part B: Verify CommitBatch evaluates multiple unique candidates concurrently via OpenAICompatibleAdapter while skipping duplicates in batch
	mutCalls.Store(0)
	batchSvc, err := memory.NewService(":memory:", adapter)
	if err != nil {
		t.Fatalf("NewService batch: %v", err)
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
func (e *guardrailAuditEval) ScoreRelevance(_ context.Context, _ string, facts []memory.Fact) ([]memory.ScoredFact, error) {
	out := make([]memory.ScoredFact, len(facts))
	for i, f := range facts {
		out[i] = memory.ScoredFact{Fact: f, Score: 4.5, Confidence: 0.92}
	}
	return out, nil
}
func (e *guardrailAuditEval) EvaluateMutation(_ context.Context, _ string, _ string, _ []memory.Fact) (memory.MutationDecision, error) {
	return memory.MutationDecision{
		Category:          "architecture",
		Tier:              "invariant",
		IsPermanent:       0.95,
		IsGlobalInvariant: 0.92,
		Action:            "INSERT",
		Confidence:        0.96,
	}, nil
}
func (e *guardrailAuditEval) VerifyPlan(_ context.Context, _ string, rules []memory.Fact) ([]memory.PlanViolation, error) {
	if !e.violate || len(rules) == 0 {
		return nil, nil
	}
	return []memory.PlanViolation{
		{Fact: rules[0], Probability: 0.96, Confidence: 0.95},
	}, nil
}
func (e *guardrailAuditEval) SynthesizeConsolidation(_ context.Context, _ string, _ string, facts []memory.Fact) (string, error) {
	if len(facts) > 0 {
		return facts[0].Content, nil
	}
	return "", nil
}

func TestGuardrailLogsAuditPaginationAndIsolation(t *testing.T) {
	ctx := context.Background()
	eval := &guardrailAuditEval{}
	svc, err := memory.NewService(":memory:", eval)
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	defer svc.Close()

	if _, err := svc.CommitMemory(ctx, "CGO_ENABLED=0 순수 Go 바이너리 빌드만 허용하며 C 바인딩을 금지한다", "architecture", "project:guard"); err != nil {
		t.Fatalf("CommitMemory: %v", err)
	}

	// 1. PreToolUse violation block records GuardrailVerdictRuleViolation + redacts accidental secrets
	eval.violate = true
	hookPayload := []byte(`{"tool_name":"run_command","tool_input":{"command":"CGO_ENABLED=1 go build -ldflags sk-1234567890abcdefghijklmnop"},"cwd":"/tmp/guard"}`)
	outDeny, _, err := svc.ExecuteHook(ctx, "antigravity-pretool", hookPayload, "project:guard")
	if err != nil || !strings.Contains(outDeny, `"decision":"deny"`) {
		t.Fatalf("expected antigravity-pretool deny, got %q (%v)", outDeny, err)
	}

	// 2. PreToolUse pass (overlapping rule checked, 0 violations) records GuardrailVerdictPass
	eval.violate = false
	passPayload := []byte(`{"tool_name":"run_command","tool_input":{"command":"CGO_ENABLED=0 go build ./cmd/memorize"},"cwd":"/tmp/guard"}`)
	outAllow, _, err := svc.ExecuteHook(ctx, "antigravity-pretool", passPayload, "project:guard")
	if err != nil || !strings.Contains(outAllow, `"decision":"allow"`) {
		t.Fatalf("expected antigravity-pretool allow, got %q (%v)", outAllow, err)
	}

	// 3. VerifyPlan violation + 10 extra entries to verify SQLite LIMIT/OFFSET pagination
	eval.violate = true
	for i := 1; i <= 11; i++ {
		vOut, vErr := svc.VerifyPlan(ctx, fmt.Sprintf("GUARDRAIL_ONLY_SECRET_MARKER_%d: CGO_ENABLED=1 빌드 전환 시도", i), "project:guard")
		if vErr != nil || !strings.Contains(vOut, "Violations Detected") {
			t.Fatalf("VerifyPlan %d expected violation, got %q (%v)", i, vOut, vErr)
		}
	}

	page1, err := svc.QueryGuardrailLogsPage(ctx, memory.GuardrailLogQuery{
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

	page2, err := svc.QueryGuardrailLogsPage(ctx, memory.GuardrailLogQuery{
		Scope:    "project:guard",
		State:    "all",
		Page:     2,
		PageSize: 10,
	})
	if err != nil || len(page2.Records) != 3 {
		t.Fatalf("expected 3 records on page 2, got %+v (%v)", page2, err)
	}
	oldest := page2.Records[len(page2.Records)-1]
	if oldest.Verdict != memory.GuardrailVerdictRuleViolation || strings.Contains(oldest.PlanSnippet, "sk-123456") || !strings.Contains(oldest.PlanSnippet, "[REDACTED]") {
		t.Fatalf("expected secret redaction and rule_violation on oldest PreToolUse log, got %+v", oldest)
	}

	// 4. Filter by source="pretool" and state="pass"
	prePass, err := svc.QueryGuardrailLogsPage(ctx, memory.GuardrailLogQuery{
		Scope:    "project:guard",
		State:    "pass",
		Source:   "pretool",
		Page:     1,
		PageSize: 10,
	})
	if err != nil || prePass.FilteredCount != 1 || prePass.Records[0].Verdict != memory.GuardrailVerdictPass {
		t.Fatalf("expected 1 pretool pass entry, got %+v (%v)", prePass, err)
	}

	// 5. Strict SSOT isolation: QuerySessionMemory must never surface guardrail_logs text
	qOut, err := svc.QuerySessionMemory(ctx, "sess-guard-iso", "GUARDRAIL_ONLY_SECRET_MARKER", "", "project:guard", true)
	if err != nil {
		t.Fatalf("QuerySessionMemory: %v", err)
	}
	if strings.Contains(qOut, "GUARDRAIL_ONLY_SECRET_MARKER") {
		t.Fatalf("guardrail_logs leaked into QuerySessionMemory: %s", qOut)
	}
}
