package guardrail_test

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	"memorize/internal/domain"
	_ "modernc.org/sqlite"
)

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

	// 1. Default retention policy must be 0 (keep forever)
	if domain.DefaultProgressRetentionDays != 0 || domain.DefaultGuardrailRetentionDays != 0 {
		t.Fatalf("expected DefaultProgressRetentionDays=0 and DefaultGuardrailRetentionDays=0")
	}
	defPol := st.prog.GetRetentionPolicy(ctx)
	if defPol.ProgressDays != 0 || defPol.GuardrailDays != 0 || defPol.GuardrailIncludeBlocked {
		t.Fatalf("expected default forever (0d) retention policy, got: %+v", defPol)
	}

	// 2. Commit 2 memories (auto-linked into activity row) + add 2 older tracks + 1 recent track
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

	// Backdate the memory-linked row AND the two older tracks to -15 days
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

	// Clean history older than 7 days: must delete all 3 -15d rows while preserving the recent entry!
	deletedProg, err := st.prog.CleanupProgressHistory(ctx, scope, 7)
	if err != nil {
		t.Fatalf("CleanupProgressHistory(7): %v", err)
	}
	if deletedProg != 3 {
		t.Fatalf("expected 3 old rows deleted, got %d", deletedProg)
	}

	ovAfter, err := st.prog.GetProgressOverview(ctx, scope, "", "")
	if err != nil {
		t.Fatalf("GetProgressOverview: %v", err)
	}
	if ovAfter.TotalCount != 1 {
		t.Fatalf("expected 1 remaining recent row, got %d: %+v", ovAfter.TotalCount, ovAfter.Records)
	}
	if ovAfter.Records[0].ID != recentDone.ID || ovAfter.Records[0].ID == oldTrackA.ID || ovAfter.Records[0].ID == oldTrackB.ID {
		t.Fatalf("expected only recent record %d preserved, got %+v", recentDone.ID, ovAfter.Records)
	}

	// Watermark protection: pruned active memories #1 and #2 must NOT show up as unlinked or resurrect on SyncProjectProgress!
	syncState := st.prog.InspectProgressSync(ctx, scope, "")
	if syncState.UnlinkedMemoryCount != 0 || !syncState.InSync {
		t.Fatalf("expected watermark to prevent pruned memories from appearing unlinked, got %+v", syncState)
	}
	syncRes, err := st.prog.SyncProjectProgress(ctx, scope, "", "ko")
	if err != nil || syncRes.SyncedMemories != 0 {
		t.Fatalf("expected SyncProjectProgress to resurrect 0 pruned memories, got %+v (%v)", syncRes, err)
	}

	// A brand-new unlinked memory (ID > watermark) MUST still be detected and synced!
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

	// 3. Guardrail logs cleanup: PASS-only default vs includeBlocked=true
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

	// 4. Auto-TTL sweep when SetProgressRetentionDays(7) is saved
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
