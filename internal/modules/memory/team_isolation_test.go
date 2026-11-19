package memory_test

import (
	"context"
	"path/filepath"
	"testing"

	"memorize/internal/domain"
	"memorize/internal/modules/memory"
)

func commitAndID(t *testing.T, ctx context.Context, svc *memory.Service, content, cat, scope string) int64 {
	t.Helper()
	if _, err := svc.CommitMemoryFull(ctx, content, cat, "", "", scope); err != nil {
		t.Fatalf("CommitMemoryFull(%q, %s): %v", content, scope, err)
	}
	var id int64
	if err := svc.DB().QueryRowContext(ctx, `SELECT id FROM memories WHERE scope = ? AND content = ? ORDER BY id DESC LIMIT 1`, scope, content).Scan(&id); err != nil || id <= 0 {
		t.Fatalf("lookup committed memory ID (%q): id=%d err=%v", content, id, err)
	}
	return id
}

func TestServerModeSharedToPersonalRestrictionAndUndoIsolation(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "mem.db")
	svc, err := memory.Open(dbPath, &stubEval{
		dec: domain.MutationDecision{Action: "INSERT", Category: "architecture", IsPermanent: 0.95, Confidence: 0.95},
	})
	if err != nil {
		t.Fatalf("memory.Open: %v", err)
	}
	defer svc.Close()

	ctxAdmin := domain.WithAuthPrincipal(context.Background(), domain.AuthPrincipal{
		Account:       "admin",
		Role:          domain.RoleAdmin,
		ServerMode:    true,
		Authenticated: true,
	})
	ctxAlice := domain.WithAuthPrincipal(context.Background(), domain.AuthPrincipal{
		Account:       "alice",
		Role:          domain.RoleMember,
		ServerMode:    true,
		Authenticated: true,
	})
	ctxBob := domain.WithAuthPrincipal(context.Background(), domain.AuthPrincipal{
		Account:       "bob",
		Role:          domain.RoleMember,
		ServerMode:    true,
		Authenticated: true,
	})
	ctxSolo := context.Background()

	p1 := commitAndID(t, ctxAlice, svc, "Shared project rule #1", "architecture", "project:teamapp")
	p2 := commitAndID(t, ctxAlice, svc, "Shared project rule #2", "architecture", "project:teamapp")
	aliceGlobal := commitAndID(t, ctxAlice, svc, "Alice cross-project dev rule #1", "architecture", "global")
	adminGlobal := commitAndID(t, ctxAdmin, svc, "Admin cross-project dev rule #1", "architecture", "global")
	alicePers := commitAndID(t, ctxAlice, svc, "Alice personal draft note", "preference", "personal")

	// 1. In ServerMode, member & admin each isolate their own global & personal rules,
	//    while moving shared project:* memories into personal OR global is rejected.
	for name, tc := range map[string]struct {
		ctx   context.Context
		id    int64
		scope string
	}{
		"member_project_to_personal": {ctx: ctxAlice, id: p1, scope: "personal"},
		"member_project_to_global":   {ctx: ctxAlice, id: p1, scope: "global"},
		"admin_project_to_personal":  {ctx: ctxAdmin, id: p1, scope: "personal"},
		"admin_project_to_global":    {ctx: ctxAdmin, id: p1, scope: "global"},
	} {
		if err := svc.UpdateMemoryFull(tc.ctx, tc.id, tc.scope, "", nil, "", nil); err == nil {
			t.Fatalf("%s: expected shared->%s move rejected in ServerMode", name, tc.scope)
		}
	}

	// Each user's personal <-> global <-> project moves work for their own rows, while other accounts are blocked.
	if err := svc.UpdateMemoryFull(ctxAlice, alicePers, "global", "", nil, "", nil); err != nil {
		t.Fatalf("expected Alice own personal -> global move allowed: %v", err)
	}
	if err := svc.UpdateMemoryFull(ctxAlice, alicePers, "project:teamapp", "", nil, "", nil); err != nil {
		t.Fatalf("expected Alice own global -> project move allowed: %v", err)
	}
	if err := svc.UpdateMemoryFull(ctxBob, aliceGlobal, "project:teamapp", "", nil, "", nil); err == nil {
		t.Fatalf("expected Bob mutating Alice global memory to fail in ServerMode")
	}
	if err := svc.UpdateMemoryFull(ctxAdmin, aliceGlobal, "project:teamapp", "", nil, "", nil); err == nil {
		t.Fatalf("expected Admin mutating Alice global memory to fail in ServerMode")
	}
	if pageBob, err := svc.QueryRecordsPage(ctxBob, domain.RecordQuery{Scope: "global", State: "all"}); err != nil || len(pageBob.Records) != 0 {
		t.Fatalf("expected Bob to see 0 other-user global records, got %+v (err=%v)", pageBob.Records, err)
	}
	if pageAdmin, err := svc.QueryRecordsPage(ctxAdmin, domain.RecordQuery{Scope: "global", State: "all"}); err != nil || len(pageAdmin.Records) != 1 || pageAdmin.Records[0].ID != adminGlobal {
		t.Fatalf("expected Admin to see only Admin's global record #%d, got %+v (err=%v)", adminGlobal, pageAdmin.Records, err)
	}

	// ConsolidateMemories blocks merging shared project rules into personal/global,
	// and blocks a member consolidating another account's private global rule.
	if _, err := svc.ConsolidateMemories(ctxAlice, "personal", "architecture", []int64{p1, p2}, "Cannot privatize team rules", true); err == nil {
		t.Fatalf("expected ConsolidateMemories moving project rules to personal rejected")
	}
	if _, err := svc.ConsolidateMemories(ctxAlice, "global", "architecture", []int64{p1, p2}, "Cannot privatize team rules", true); err == nil {
		t.Fatalf("expected ConsolidateMemories moving project rules to global rejected")
	}
	if _, err := svc.ConsolidateMemories(ctxAlice, "project:teamapp", "architecture", []int64{p1, adminGlobal}, "Cannot include admin personal global", true); err == nil {
		t.Fatalf("expected ConsolidateMemories containing another account global child rejected")
	}

	// Solo mode (!ServerMode) retains full scope move flexibility.
	soloID := commitAndID(t, ctxSolo, svc, "Solo move test rule", "architecture", "project:solo")
	if err := svc.UpdateMemoryFull(ctxSolo, soloID, "personal", "", nil, "", nil); err != nil {
		t.Fatalf("expected solo mode shared -> personal move still permitted: %v", err)
	}

	// 2. UndoMemory cannot restore another user's personal or global superseded children.
	aliceChild := commitAndID(t, ctxAlice, svc, "Alice secret child note", "preference", "personal")
	parentProj := commitAndID(t, ctxAdmin, svc, "Merged parent project rule", "architecture", "project:teamapp")
	if _, err := svc.DB().Exec(`UPDATE memories SET active = 0, superseded_by = ? WHERE id IN (?, ?)`, parentProj, aliceChild, adminGlobal); err != nil {
		t.Fatalf("seed superseded children: %v", err)
	}

	if _, err := svc.UndoMemory(ctxBob, parentProj); err == nil {
		t.Fatalf("expected Bob UndoMemory on parent superseding Alice personal / Admin global child to fail")
	}
	// Verify atomic preservation: parent stayed active=1, children stayed active=0.
	recs, err := svc.GetRecordsByIDs(ctxAdmin, []int64{parentProj, adminGlobal})
	if err != nil || len(recs) != 2 {
		t.Fatalf("GetRecordsByIDs: %v", err)
	}
	for _, r := range recs {
		if r.ID == parentProj && !r.Active {
			t.Fatalf("expected parentProj to stay active after rejected UndoMemory")
		}
		if r.ID == adminGlobal && r.Active {
			t.Fatalf("expected adminGlobal to remain inactive after rejected UndoMemory")
		}
	}
	aliceRecs, err := svc.GetRecordsByIDs(ctxAlice, []int64{aliceChild})
	if err != nil || len(aliceRecs) != 1 || aliceRecs[0].Active {
		t.Fatalf("expected aliceChild to remain inactive after rejected Bob UndoMemory, got %+v (err=%v)", aliceRecs, err)
	}
}
