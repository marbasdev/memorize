package memory_test

import (
	"context"
	"path/filepath"
	"strings"
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
	g1 := commitAndID(t, ctxAdmin, svc, "Shared global rule #1", "architecture", "global")
	alicePers := commitAndID(t, ctxAlice, svc, "Alice personal draft note", "preference", "personal")

	// 1. Server mode blocks moving shared project:* or global rules into personal via UpdateMemoryFull.
	for name, tc := range map[string]struct {
		ctx context.Context
		id  int64
	}{
		"member_project_to_personal": {ctx: ctxAlice, id: p1},
		"admin_project_to_personal":  {ctx: ctxAdmin, id: p1},
		"admin_global_to_personal":   {ctx: ctxAdmin, id: g1},
	} {
		if err := svc.UpdateMemoryFull(tc.ctx, tc.id, "personal", "", nil, "", nil); err == nil || !strings.Contains(err.Error(), "personal") {
			t.Fatalf("%s: expected shared->personal move rejected in ServerMode, got err=%v", name, err)
		}
	}

	// Moving personal -> project:* remains permitted cleanly.
	if err := svc.UpdateMemoryFull(ctxAlice, alicePers, "project:teamapp", "", nil, "", nil); err != nil {
		t.Fatalf("expected personal -> project move allowed: %v", err)
	}

	// ConsolidateMemories also blocks merging shared rules into personal and blocks member deactivating global.
	if _, err := svc.ConsolidateMemories(ctxAlice, "personal", "architecture", []int64{p1, p2}, "Cannot privatize team rules", true); err == nil {
		t.Fatalf("expected ConsolidateMemories moving project rules to personal rejected")
	}
	if _, err := svc.ConsolidateMemories(ctxAlice, "project:teamapp", "architecture", []int64{p1, g1}, "Member cannot retire global", true); err == nil {
		t.Fatalf("expected ConsolidateMemories by member containing global child rejected")
	}

	// Solo mode (!ServerMode) retains full scope move flexibility.
	soloID := commitAndID(t, ctxSolo, svc, "Solo move test rule", "architecture", "project:solo")
	if err := svc.UpdateMemoryFull(ctxSolo, soloID, "personal", "", nil, "", nil); err != nil {
		t.Fatalf("expected solo mode shared -> personal move still permitted: %v", err)
	}

	// 2. UndoMemory cannot restore global or another user's personal superseded children.
	aliceChild := commitAndID(t, ctxAlice, svc, "Alice secret child note", "preference", "personal")
	parentProj := commitAndID(t, ctxAdmin, svc, "Merged parent project rule", "architecture", "project:teamapp")
	if _, err := svc.DB().Exec(`UPDATE memories SET active = 0, superseded_by = ? WHERE id IN (?, ?)`, parentProj, aliceChild, g1); err != nil {
		t.Fatalf("seed superseded children: %v", err)
	}

	if _, err := svc.UndoMemory(ctxBob, parentProj); err == nil {
		t.Fatalf("expected Bob UndoMemory on parent superseding Alice personal / global child to fail")
	}
	// Verify atomic preservation: parent stayed active=1, children stayed active=0.
	recs, err := svc.GetRecordsByIDs(ctxAdmin, []int64{parentProj, g1})
	if err != nil || len(recs) != 2 {
		t.Fatalf("GetRecordsByIDs: %v", err)
	}
	for _, r := range recs {
		if r.ID == parentProj && !r.Active {
			t.Fatalf("expected parentProj to stay active after rejected UndoMemory")
		}
		if r.ID == g1 && r.Active {
			t.Fatalf("expected g1 to remain inactive after rejected UndoMemory")
		}
	}
	aliceRecs, err := svc.GetRecordsByIDs(ctxAlice, []int64{aliceChild})
	if err != nil || len(aliceRecs) != 1 || aliceRecs[0].Active {
		t.Fatalf("expected aliceChild to remain inactive after rejected Bob UndoMemory, got %+v (err=%v)", aliceRecs, err)
	}
}
