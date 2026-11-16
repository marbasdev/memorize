package memory_test

import (
	"context"
	"database/sql"
	"errors"
	"io"
	"path/filepath"
	"strings"
	"testing"

	_ "modernc.org/sqlite"

	"memorize/internal/domain"
	isqlite "memorize/internal/infra/sqlite"
	"memorize/internal/modules/memory"
)

type duplexRW struct {
	io.Reader
	io.Writer
}

func openSyncPeerServices(t *testing.T) (*memory.Service, *memory.Service, *stubEval, *stubEval) {
	t.Helper()
	t.Cleanup(domain.SetMemorySyncEnabledForTest(true))
	dir := t.TempDir()
	evalA := &stubEval{dec: domain.MutationDecision{IsPermanent: 0.95, Action: "INSERT", Confidence: 0.9}}
	evalB := &stubEval{dec: domain.MutationDecision{IsPermanent: 0.95, Action: "INSERT", Confidence: 0.9}}
	svcA, err := memory.Open(filepath.Join(dir, "peerA.db"), evalA)
	if err != nil {
		t.Fatalf("open peerA: %v", err)
	}
	t.Cleanup(func() { _ = svcA.Close() })
	svcB, err := memory.Open(filepath.Join(dir, "peerB.db"), evalB)
	if err != nil {
		t.Fatalf("open peerB: %v", err)
	}
	t.Cleanup(func() { _ = svcB.Close() })
	return svcA, svcB, evalA, evalB
}

func runPipeSync(t *testing.T, ctx context.Context, initiator *memory.Service, peer domain.SyncPeer, responder *memory.Service) domain.SyncRunReport {
	t.Helper()
	c2sR, c2sW := io.Pipe()
	s2cR, s2cW := io.Pipe()
	srvErrCh := make(chan error, 1)
	go func() {
		err := memory.ServeSyncStream(ctx, responder, c2sR, s2cW)
		_ = c2sR.Close()
		_ = s2cW.Close()
		srvErrCh <- err
	}()
	report, err := initiator.RunSyncStream(ctx, peer, duplexRW{Reader: s2cR, Writer: c2sW})
	_ = c2sW.Close()
	_ = s2cR.Close()
	if err != nil {
		t.Fatalf("RunSyncStream failed: %v", err)
	}
	if srvErr := <-srvErrCh; srvErr != nil {
		t.Fatalf("ServeSyncStream failed: %v", srvErr)
	}
	return report
}

func listContentsByScope(t *testing.T, svc *memory.Service, scope string) map[string]domain.MemoryRecord {
	t.Helper()
	ctx := domain.WithAdminBypass(context.Background())
	page, err := svc.QueryRecordsPage(ctx, domain.RecordQuery{
		Scope:    scope,
		State:    "active",
		Page:     1,
		PageSize: 200,
	})
	if err != nil {
		t.Fatalf("QueryRecordsPage(%s): %v", scope, err)
	}
	out := make(map[string]domain.MemoryRecord, len(page.Records))
	for _, r := range page.Records {
		out[r.Content] = r
	}
	return out
}

func TestSyncLiveMutationTxLogCoverage(t *testing.T) {
	t.Cleanup(domain.SetMemorySyncEnabledForTest(true))
	ctx := domain.WithAdminBypass(context.Background())
	dir := t.TempDir()
	eval := &stubEval{}
	svc, err := memory.Open(filepath.Join(dir, "txlog.db"), eval)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer svc.Close()

	// 1. Commit insert + boost on IGNORE
	eval.dec = domain.MutationDecision{Category: "arch", IsPermanent: 0.95, Action: "INSERT", Confidence: 0.85}
	if _, err := svc.CommitMemory(ctx, "Fact One for sync", "arch", "global"); err != nil {
		t.Fatalf("commit 1: %v", err)
	}
	eval.dec = domain.MutationDecision{Category: "arch", IsPermanent: 0.95, Action: "INSERT", Confidence: 0.85}
	if _, err := svc.CommitMemory(ctx, "Fact Two for sync", "arch", "global"); err != nil {
		t.Fatalf("commit 2: %v", err)
	}

	recs := listContentsByScope(t, svc, "global")
	id1 := recs["Fact One for sync"].ID
	id2 := recs["Fact Two for sync"].ID

	// IGNORE boost (confidence increase)
	eval.dec = domain.MutationDecision{Action: "IGNORE", ConflictFactID: id1, Confidence: 0.95}
	if _, err := svc.CommitMemory(ctx, "Fact One repeated", "arch", "global"); err != nil {
		t.Fatalf("ignore boost: %v", err)
	}

	// OVERWRITE id2 -> id3
	eval.dec = domain.MutationDecision{Category: "arch", IsPermanent: 0.95, Action: "OVERWRITE", ConflictFactID: id2, Confidence: 0.92}
	if _, err := svc.CommitMemory(ctx, "Fact Two v2 updated", "arch", "global"); err != nil {
		t.Fatalf("overwrite: %v", err)
	}

	// UpdateMemoryFull, UpdateMemoryAccount, UpdateMemoryTags
	pinTrue := true
	sub := "testing"
	if err := svc.UpdateMemoryFull(ctx, id1, "global", "arch", &sub, "Fact One refined", &pinTrue); err != nil {
		t.Fatalf("UpdateMemoryFull: %v", err)
	}
	if err := svc.UpdateMemoryAccount(ctx, id1, "work"); err != nil {
		t.Fatalf("UpdateMemoryAccount: %v", err)
	}
	if err := svc.UpdateMemoryTags(ctx, id1, "go,sync"); err != nil {
		t.Fatalf("UpdateMemoryTags: %v", err)
	}

	// Consolidate two active memories
	eval.dec = domain.MutationDecision{Category: "arch", IsPermanent: 0.95, Action: "INSERT", Confidence: 0.90}
	if _, err := svc.CommitMemory(ctx, "Mergeable rule alpha", "arch", "global"); err != nil {
		t.Fatalf("commit alpha: %v", err)
	}
	if _, err := svc.CommitMemory(ctx, "Mergeable rule beta", "arch", "global"); err != nil {
		t.Fatalf("commit beta: %v", err)
	}
	recs = listContentsByScope(t, svc, "global")
	mergedID, err := svc.ConsolidateMemories(ctx, "global", "arch", []int64{recs["Mergeable rule alpha"].ID, recs["Mergeable rule beta"].ID}, "Unified rule alpha+beta", false)
	if err != nil {
		t.Fatalf("ConsolidateMemories: %v", err)
	}

	// ApplyTaxonomyRebalance
	if _, err := svc.ApplyTaxonomyRebalance(ctx, "global", []domain.TaxonomyRebalanceItem{
		{ID: mergedID, NewCategory: "security", NewSubCategory: "auth"},
	}); err != nil {
		t.Fatalf("ApplyTaxonomyRebalance: %v", err)
	}

	// Forget, Undo, and Purge
	if _, err := svc.ForgetMemories(ctx, []int64{mergedID}); err != nil {
		t.Fatalf("ForgetMemories: %v", err)
	}
	if _, err := svc.UndoMemory(ctx, mergedID); err != nil {
		t.Fatalf("UndoMemory: %v", err)
	}
	if _, err := svc.ForgetMemories(ctx, []int64{mergedID}); err != nil {
		t.Fatalf("ForgetMemories 2: %v", err)
	}
	purged, err := svc.PurgeTombstones(ctx, "global")
	if err != nil || purged == 0 {
		t.Fatalf("PurgeTombstones: purged=%d err=%v", purged, err)
	}

	entries, err := svc.ExportSyncDeltas(ctx, domain.SyncScopeModeAll, nil, nil)
	if err != nil {
		t.Fatalf("ExportSyncDeltas: %v", err)
	}
	if len(entries) < 12 {
		t.Fatalf("expected >= 12 TX log entries covering all mutations, got %d", len(entries))
	}
	hasPurge := false
	hasTagUpsert := false
	for _, e := range entries {
		if e.OriginDevice == "" || e.OriginSeq <= 0 || e.SyncID == "" {
			t.Fatalf("invalid tx entry metadata: %+v", e)
		}
		if e.Op == domain.SyncOpPurge {
			hasPurge = true
		}
		if e.Snapshot != nil && e.Snapshot.Tags == "go sync" {
			hasTagUpsert = true
		}
	}
	if !hasPurge || !hasTagUpsert {
		t.Fatalf("missing expected ops in tx log: hasPurge=%v hasTagUpsert=%v", hasPurge, hasTagUpsert)
	}
}

func TestSyncBidirectionalAndInitialDedup(t *testing.T) {
	ctx := domain.WithAdminBypass(context.Background())
	svcA, svcB, evalA, evalB := openSyncPeerServices(t)
	devB, _ := svcB.SelfDeviceID(ctx)

	evalA.dec = domain.MutationDecision{Category: "arch", IsPermanent: 0.95, Action: "INSERT", Confidence: 0.9}
	_, _ = svcA.CommitMemory(ctx, "Only on Peer A", "arch", "global")
	_, _ = svcA.CommitMemory(ctx, "Shared exact duplicate rule", "arch", "global")

	evalB.dec = domain.MutationDecision{Category: "arch", IsPermanent: 0.95, Action: "INSERT", Confidence: 0.9}
	_, _ = svcB.CommitMemory(ctx, "Only on Peer B", "arch", "global")
	_, _ = svcB.CommitMemory(ctx, "Shared exact duplicate rule", "arch", "global")

	peer, err := svcA.UpsertSyncPeer(ctx, domain.SyncPeer{
		PeerID:    "peer-b",
		SSHTarget: "peer-b.local",
		ScopeMode: domain.SyncScopeModeAll,
	})
	if err != nil {
		t.Fatalf("UpsertSyncPeer: %v", err)
	}

	rep1 := runPipeSync(t, ctx, svcA, peer, svcB)
	if rep1.PulledApplied != 1 || rep1.PulledDeduped != 1 || rep1.PullConflicts != 0 {
		t.Fatalf("unexpected pull stats: %+v", rep1)
	}
	if rep1.PushedApplied != 1 || rep1.PushedConflicts != 0 {
		t.Fatalf("unexpected push stats: %+v", rep1)
	}
	if rep1.RemoteDeviceID != devB {
		t.Fatalf("expected RemoteDeviceID %q, got %q", devB, rep1.RemoteDeviceID)
	}

	// Both peers must have exactly 3 active memories and no duplicate rows.
	recsA := listContentsByScope(t, svcA, "global")
	recsB := listContentsByScope(t, svcB, "global")
	if len(recsA) != 3 || len(recsB) != 3 {
		t.Fatalf("expected 3 memories on both peers, got A=%d B=%d", len(recsA), len(recsB))
	}

	// Updating the previously deduped row on Peer A must propagate via alias without creating a 4th row or conflict on Peer B.
	sharedAID := recsA["Shared exact duplicate rule"].ID
	if err := svcA.UpdateMemory(ctx, sharedAID, "arch", "Shared exact duplicate rule v2", false); err != nil {
		t.Fatalf("UpdateMemory on deduped row: %v", err)
	}

	// Second sync with updated peer RemoteDeviceID must apply only that update and finish clean.
	savedPeer, err := svcA.GetSyncPeer(ctx, "peer-b")
	if err != nil {
		t.Fatalf("GetSyncPeer: %v", err)
	}
	rep2 := runPipeSync(t, ctx, svcA, *savedPeer, svcB)
	if rep2.PushedApplied != 1 || rep2.PushedConflicts != 0 {
		t.Fatalf("expected second sync to push 1 clean update without conflict, got %+v", rep2)
	}
	recsB2 := listContentsByScope(t, svcB, "global")
	if len(recsB2) != 3 {
		t.Fatalf("expected 3 memories after aliased update, got %d", len(recsB2))
	}
	if _, ok := recsB2["Shared exact duplicate rule v2"]; !ok {
		t.Fatalf("expected aliased update to land on Peer B, got %v", recsB2)
	}

	// Third sync must be a complete zero-transfer no-op.
	savedPeer, _ = svcA.GetSyncPeer(ctx, "peer-b")
	rep3 := runPipeSync(t, ctx, svcA, *savedPeer, svcB)
	if rep3.PulledApplied != 0 || rep3.PulledDeduped != 0 || rep3.PushedSent != 0 || rep3.PushedApplied != 0 {
		t.Fatalf("expected third sync to transfer zero entries, got %+v", rep3)
	}
}

func TestSyncScopeFilteringAndExpansionWithoutMissingOldEntries(t *testing.T) {
	ctx := domain.WithAdminBypass(context.Background())
	svcA, svcB, evalA, _ := openSyncPeerServices(t)

	evalA.dec = domain.MutationDecision{Category: "arch", IsPermanent: 0.95, Action: "INSERT", Confidence: 0.9}
	_, _ = svcA.CommitMemory(ctx, "Global stays excluded initially", "arch", "global")
	_, _ = svcA.CommitMemory(ctx, "Alpha memory old", "arch", "project:alpha")
	_, _ = svcA.CommitMemory(ctx, "Beta memory old", "arch", "project:beta")

	// Sync only project:alpha first.
	peerAlpha, err := svcB.UpsertSyncPeer(ctx, domain.SyncPeer{
		PeerID:    "peer-a",
		SSHTarget: "peer-a.local",
		ScopeMode: domain.SyncScopeModeSelected,
		Scopes:    []string{"project:alpha"},
	})
	if err != nil {
		t.Fatalf("UpsertSyncPeer: %v", err)
	}
	rep1 := runPipeSync(t, ctx, svcB, peerAlpha, svcA)
	if rep1.PulledApplied != 1 {
		t.Fatalf("expected 1 memory pulled for project:alpha, got %+v", rep1)
	}
	if got := listContentsByScope(t, svcB, "project:beta"); len(got) != 0 {
		t.Fatalf("expected 0 project:beta memories before expansion, got %d", len(got))
	}

	// Expand scope filter to include project:beta; older project:beta entry must still sync.
	peerExpanded, err := svcB.UpsertSyncPeer(ctx, domain.SyncPeer{
		PeerID:    "peer-a",
		SSHTarget: "peer-a.local",
		ScopeMode: domain.SyncScopeModeSelected,
		Scopes:    []string{"project:alpha", "project:beta"},
	})
	if err != nil {
		t.Fatalf("UpsertSyncPeer expand: %v", err)
	}
	rep2 := runPipeSync(t, ctx, svcB, peerExpanded, svcA)
	if rep2.PulledApplied != 1 {
		t.Fatalf("expected 1 older project:beta memory after scope expansion, got %+v", rep2)
	}
	if got := listContentsByScope(t, svcB, "project:beta"); len(got) != 1 {
		t.Fatalf("expected 1 project:beta memory after expansion, got %d", len(got))
	}
	if got := listContentsByScope(t, svcB, "global"); len(got) != 0 {
		t.Fatalf("expected global memory still excluded, got %d", len(got))
	}
}

func TestSyncConflictDetectionAndResolutions(t *testing.T) {
	cases := []struct {
		name     string
		decision string
		verify   func(t *testing.T, svcA, svcB *memory.Service)
	}{
		{
			name:     "keep_local",
			decision: domain.SyncResolveKeepLocal,
			verify: func(t *testing.T, svcA, svcB *memory.Service) {
				a := listContentsByScope(t, svcA, "global")
				b := listContentsByScope(t, svcB, "global")
				if len(a) != 1 || len(b) != 1 {
					t.Fatalf("expected 1 row each, got A=%d B=%d", len(a), len(b))
				}
				if _, ok := a["Edited on Peer A"]; !ok {
					t.Fatalf("expected Peer A edit to win, A=%v", a)
				}
				if _, ok := b["Edited on Peer A"]; !ok {
					t.Fatalf("expected Peer B to converge to Peer A edit, B=%v", b)
				}
			},
		},
		{
			name:     "use_remote",
			decision: domain.SyncResolveUseRemote,
			verify: func(t *testing.T, svcA, svcB *memory.Service) {
				a := listContentsByScope(t, svcA, "global")
				b := listContentsByScope(t, svcB, "global")
				if len(a) != 1 || len(b) != 1 {
					t.Fatalf("expected 1 row each, got A=%d B=%d", len(a), len(b))
				}
				if _, ok := a["Edited on Peer B"]; !ok {
					t.Fatalf("expected Peer B edit adopted on A, A=%v", a)
				}
				if _, ok := b["Edited on Peer B"]; !ok {
					t.Fatalf("expected Peer B to keep Peer B edit, B=%v", b)
				}
			},
		},
		{
			name:     "keep_both",
			decision: domain.SyncResolveKeepBoth,
			verify: func(t *testing.T, svcA, svcB *memory.Service) {
				a := listContentsByScope(t, svcA, "global")
				b := listContentsByScope(t, svcB, "global")
				if len(a) != 2 || len(b) != 2 {
					t.Fatalf("expected 2 rows each for keep_both, got A=%d B=%d", len(a), len(b))
				}
				if _, ok := a["Edited on Peer A"]; !ok {
					t.Fatalf("missing Peer A variant on A: %v", a)
				}
				if _, ok := a["Edited on Peer B"]; !ok {
					t.Fatalf("missing Peer B variant on A: %v", a)
				}
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := domain.WithAdminBypass(context.Background())
			svcA, svcB, evalA, _ := openSyncPeerServices(t)

			evalA.dec = domain.MutationDecision{Category: "arch", IsPermanent: 0.95, Action: "INSERT", Confidence: 0.9}
			_, _ = svcA.CommitMemory(ctx, "Shared original fact", "arch", "global")

			peer, _ := svcA.UpsertSyncPeer(ctx, domain.SyncPeer{
				PeerID:    "peer-b",
				SSHTarget: "peer-b.local",
				ScopeMode: domain.SyncScopeModeAll,
			})
			_ = runPipeSync(t, ctx, svcA, peer, svcB)

			// Edit same logical memory on both peers concurrently before next sync.
			idA := listContentsByScope(t, svcA, "global")["Shared original fact"].ID
			idB := listContentsByScope(t, svcB, "global")["Shared original fact"].ID
			if err := svcA.UpdateMemory(ctx, idA, "arch", "Edited on Peer A", false); err != nil {
				t.Fatalf("update A: %v", err)
			}
			if err := svcB.UpdateMemory(ctx, idB, "arch", "Edited on Peer B", false); err != nil {
				t.Fatalf("update B: %v", err)
			}

			// Sync triggers conflict on both peers.
			savedA, _ := svcA.GetSyncPeer(ctx, "peer-b")
			rep := runPipeSync(t, ctx, svcA, *savedA, svcB)
			if rep.PullConflicts != 1 || rep.PushedConflicts != 1 {
				t.Fatalf("expected 1 pull and 1 push conflict, got %+v", rep)
			}
			if svcA.CountSyncConflicts(ctx) != 1 || svcB.CountSyncConflicts(ctx) != 1 {
				t.Fatalf("expected 1 conflict on both peers, got A=%d B=%d",
					svcA.CountSyncConflicts(ctx), svcB.CountSyncConflicts(ctx))
			}

			// Resolve on Peer A and re-sync; Peer B's pending conflict must also clear via resolved_origin ancestor match.
			conflicts, err := svcA.ListSyncConflicts(ctx)
			if err != nil || len(conflicts) != 1 {
				t.Fatalf("ListSyncConflicts: len=%d err=%v", len(conflicts), err)
			}
			if _, err := svcA.ResolveSyncConflict(ctx, conflicts[0].SyncID, tc.decision); err != nil {
				t.Fatalf("ResolveSyncConflict(%s): %v", tc.decision, err)
			}

			savedA, _ = svcA.GetSyncPeer(ctx, "peer-b")
			repAfter := runPipeSync(t, ctx, svcA, *savedA, svcB)
			if repAfter.PullConflicts != 0 || repAfter.PushedConflicts != 0 {
				t.Fatalf("expected zero conflicts on convergence sync, got %+v", repAfter)
			}
			if svcA.CountSyncConflicts(ctx) != 0 || svcB.CountSyncConflicts(ctx) != 0 {
				t.Fatalf("expected 0 remaining conflicts, got A=%d B=%d",
					svcA.CountSyncConflicts(ctx), svcB.CountSyncConflicts(ctx))
			}
			tc.verify(t, svcA, svcB)
		})
	}
}

func TestSyncPurgeConflictAndLegacyDBMigrationConvergence(t *testing.T) {
	t.Cleanup(domain.SetMemorySyncEnabledForTest(true))
	ctx := domain.WithAdminBypass(context.Background())
	dir := t.TempDir()
	dbPathA := filepath.Join(dir, "legacyA.db")

	// Seed a legacy pre-sync DB on Peer A without sync columns, then upgrade via memory.Open.
	rawDB, err := sql.Open("sqlite", dbPathA)
	if err != nil {
		t.Fatalf("open raw legacy db: %v", err)
	}
	_, err = rawDB.Exec(`
		CREATE TABLE memories (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			scope TEXT NOT NULL,
			account TEXT DEFAULT '',
			category TEXT NOT NULL,
			sub_category TEXT DEFAULT '',
			tags TEXT DEFAULT '',
			replaces_text TEXT DEFAULT '',
			content TEXT NOT NULL,
			confidence REAL NOT NULL DEFAULT 0.9,
			pinned INTEGER NOT NULL DEFAULT 0,
			active INTEGER NOT NULL DEFAULT 1,
			superseded_by INTEGER REFERENCES memories(id)
		);
		INSERT INTO memories (scope, category, content) VALUES ('global', 'arch', 'Migrated legacy rule');
	`)
	_ = rawDB.Close()
	if err != nil {
		t.Fatalf("seed legacy db: %v", err)
	}

	svcA, err := memory.Open(dbPathA, &stubEval{})
	if err != nil {
		t.Fatalf("upgrade peerA: %v", err)
	}
	defer svcA.Close()
	svcB, err := memory.Open(filepath.Join(dir, "freshB.db"), &stubEval{})
	if err != nil {
		t.Fatalf("open peerB: %v", err)
	}
	defer svcB.Close()

	// Migrated legacy row should sync cleanly into Peer B.
	peer, _ := svcB.UpsertSyncPeer(ctx, domain.SyncPeer{
		PeerID:    "legacy-a",
		SSHTarget: "legacy-a.local",
		ScopeMode: domain.SyncScopeModeAll,
	})
	rep := runPipeSync(t, ctx, svcB, peer, svcA)
	if rep.PulledApplied != 1 {
		t.Fatalf("expected migrated legacy row pulled into B, got %+v", rep)
	}

	// Deactivate + Purge on Peer B propagates tombstone deletion cleanly when Peer A wasn't concurrently edited.
	idB := listContentsByScope(t, svcB, "global")["Migrated legacy rule"].ID
	if _, err := svcB.ForgetMemories(ctx, []int64{idB}); err != nil {
		t.Fatalf("forget on B: %v", err)
	}
	if _, err := svcB.PurgeTombstones(ctx, "global"); err != nil {
		t.Fatalf("purge on B: %v", err)
	}
	savedB, _ := svcB.GetSyncPeer(ctx, "legacy-a")
	_ = runPipeSync(t, ctx, svcB, *savedB, svcA)
	if got := listContentsByScope(t, svcA, "global"); len(got) != 0 {
		t.Fatalf("expected purged row removed on Peer A, got %d", len(got))
	}

	// Verify DeleteSyncPeer and ListKnownSyncScopes.
	if err := svcB.DeleteSyncPeer(ctx, "legacy-a"); err != nil {
		t.Fatalf("DeleteSyncPeer: %v", err)
	}
	if peers, _ := svcB.ListSyncPeers(ctx); len(peers) != 0 {
		t.Fatalf("expected 0 peers after delete, got %d", len(peers))
	}
	if scopes := svcB.ListKnownSyncScopes(ctx); len(scopes) < 2 {
		t.Fatalf("expected >= 2 known scopes (global, personal), got %v", scopes)
	}
	_ = isqlite.Open
}

func TestSyncDisabledByDefaultSkipsTxLogAndBlocksSync(t *testing.T) {
	if domain.IsMemorySyncEnabled() {
		t.Fatalf("expected IsMemorySyncEnabled() == false by default")
	}
	ctx := domain.WithAdminBypass(context.Background())
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "disabled-sync.db")
	eval := &stubEval{dec: domain.MutationDecision{Category: "arch", IsPermanent: 0.95, Action: "INSERT", Confidence: 0.9}}
	svc, err := memory.Open(dbPath, eval)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer svc.Close()

	if _, err := svc.CommitMemory(ctx, "Fact stored while sync disabled", "arch", "global"); err != nil {
		t.Fatalf("commit: %v", err)
	}
	recs := listContentsByScope(t, svc, "global")
	id := recs["Fact stored while sync disabled"].ID
	if err := svc.UpdateMemory(ctx, id, "arch", "Fact updated while sync disabled", true); err != nil {
		t.Fatalf("update: %v", err)
	}
	if _, err := svc.ForgetMemories(ctx, []int64{id}); err != nil {
		t.Fatalf("forget: %v", err)
	}
	if _, err := svc.PurgeTombstones(ctx, "global"); err != nil {
		t.Fatalf("purge: %v", err)
	}

	raw, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open raw: %v", err)
	}
	defer raw.Close()
	var txCnt int
	if err := raw.QueryRow(`SELECT COUNT(*) FROM memory_tx_log`).Scan(&txCnt); err != nil || txCnt != 0 {
		t.Fatalf("expected 0 memory_tx_log rows when disabled, got %d (err=%v)", txCnt, err)
	}

	if _, err := svc.SyncWithPeer(ctx, "peer-x"); !errors.Is(err, domain.ErrMemorySyncDisabled) {
		t.Fatalf("expected ErrMemorySyncDisabled from SyncWithPeer, got %v", err)
	}
	if _, _, err := svc.HandleSyncHandshake(ctx, domain.SyncWireFrame{Type: "hello"}); !errors.Is(err, domain.ErrMemorySyncDisabled) {
		t.Fatalf("expected ErrMemorySyncDisabled from HandleSyncHandshake, got %v", err)
	}
	if _, err := svc.RunSyncStream(ctx, domain.SyncPeer{PeerID: "p", SSHTarget: "p.local"}, duplexRW{Reader: io.LimitReader(nil, 0), Writer: io.Discard}); !errors.Is(err, domain.ErrMemorySyncDisabled) {
		t.Fatalf("expected ErrMemorySyncDisabled from RunSyncStream, got %v", err)
	}
	if err := memory.ServeSyncStream(ctx, svc, io.LimitReader(nil, 0), io.Discard); !errors.Is(err, domain.ErrMemorySyncDisabled) {
		t.Fatalf("expected ErrMemorySyncDisabled from ServeSyncStream, got %v", err)
	}
	if probe := svc.TestSyncTarget(ctx, "p.local", ""); probe.OK || !strings.Contains(probe.Error, domain.ErrMemorySyncDisabled.Error()) {
		t.Fatalf("expected disabled probe error, got %+v", probe)
	}
}
