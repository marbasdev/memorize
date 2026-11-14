package progress

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"memorize/internal/domain"
	"memorize/internal/modules/memory"
)

func TestLiveSessionSyntheticIDsAndLazyTTLExpiration(t *testing.T) {
	mem, err := memory.Open(":memory:", nil)
	if err != nil {
		t.Fatalf("memory.Open: %v", err)
	}
	defer mem.Close()
	svc := New(mem)

	scope := "project:live-ttl"
	// Synthetic hook session IDs must be ignored.
	svc.TouchLiveSession(scope, "hook-session:ignored", "turn", "")
	svc.TouchLiveSession(scope, "stdio-ignored", "turn", "")
	svc.TouchLiveSession(scope, "bridge-ignored", "turn", "")
	if got := svc.ListLiveSessions(); len(got) != 0 {
		t.Fatalf("expected synthetic IDs ignored, got %+v", got)
	}

	// Real session expires lazily when updatedAt > LiveSessionTTL ago.
	svc.TouchLiveSession(scope, "conv-stale", "read_file", "/tmp/main.go")
	svc.mu.Lock()
	staleKey := strings.ToLower(scope) + "::conv-stale"
	if entry, ok := svc.liveSessions[staleKey]; ok {
		entry.updatedAt = time.Now().Add(-domain.LiveSessionTTL - time.Second)
		svc.liveSessions[staleKey] = entry
	}
	svc.mu.Unlock()
	if got := svc.ListLiveSessions(); len(got) != 0 {
		t.Fatalf("expected >LiveSessionTTL session expired lazily, got %+v", got)
	}
}

func TestProgressRecentDoneOrderingWindowAndLocalTZ(t *testing.T) {
	// Mutate time.Local deterministically inside a non-parallel test to verify UTC->Local conversion.
	origLocal := time.Local
	time.Local = time.FixedZone("KST-TEST", 9*3600)
	defer func() { time.Local = origLocal }()

	rec := domain.ProgressRecord{
		CreatedAt: "2026-11-12 20:30:00",
		UpdatedAt: "2026-11-12 20:30:00",
	}
	extractProgressDateTime(&rec)
	if rec.Date != "2026-11-13" || rec.Time != "05:30" {
		t.Fatalf("expected SQLite UTC 20:30 -> KST next day 2026-11-13 05:30, got date=%q time=%q", rec.Date, rec.Time)
	}
	reparsed, ok := ParseProgressTimestamp(rec.UpdatedAt)
	if !ok || reparsed.Hour() != 5 || reparsed.Minute() != 30 {
		t.Fatalf("expected RFC3339 UpdatedAt %q to re-parse without double timezone shift, got %v (ok=%v)", rec.UpdatedAt, reparsed, ok)
	}

	ctx := context.Background()
	mem, err := memory.Open(":memory:", nil)
	if err != nil {
		t.Fatalf("memory.Open: %v", err)
	}
	defer mem.Close()
	svc := New(mem)

	scope := "project:donewin"
	insertRow := func(track, createdExpr, updatedExpr string) {
		t.Helper()
		_, err := svc.db.ExecContext(ctx, fmt.Sprintf(`
			INSERT INTO project_progress (scope, session_id, track, category, status, summary, next_step, memory_ids, commit_sha, source, created_at, updated_at)
			VALUES (?, 'sess-win', ?, 'feature', 'done', ?, '', '', '', 'agent', %s, %s)
		`, createdExpr, updatedExpr), scope, track, "Completed "+track)
		if err != nil {
			t.Fatalf("insert %s: %v", track, err)
		}
	}

	// Row #1: created 10 days ago, completed 5 minutes ago -> MUST be #1 in RecentDone despite lowest ID!
	insertRow("old-id-recent-finish", "datetime('now', '-10 days')", "datetime('now', '-5 minutes')")
	insertRow("within-24h", "datetime('now', '-4 hours')", "datetime('now', '-3 hours')")
	insertRow("within-7d", "datetime('now', '-3 days')", "datetime('now', '-3 days')")
	insertRow("within-30d", "datetime('now', '-20 days')", "datetime('now', '-20 days')")
	insertRow("older-than-30d", "datetime('now', '-45 days')", "datetime('now', '-45 days')")

	ov24, err := svc.GetProgressOverviewWithQuery(ctx, domain.ProgressQuery{Scope: scope, DoneWindow: "24h"})
	if err != nil {
		t.Fatalf("DoneWindow 24h: %v", err)
	}
	if ov24.DoneWindow != "24h" || ov24.DoneWindowTotal != 2 || len(ov24.RecentDone) != 2 {
		t.Fatalf("expected 2 items in 24h window, got win=%q total=%d len=%d", ov24.DoneWindow, ov24.DoneWindowTotal, len(ov24.RecentDone))
	}
	if ov24.RecentDone[0].Track != "old-id-recent-finish" || ov24.RecentDone[1].Track != "within-24h" {
		t.Fatalf("expected RecentDone sorted by completion updated_at DESC, got [%s, %s]", ov24.RecentDone[0].Track, ov24.RecentDone[1].Track)
	}

	ov7, err := svc.GetProgressOverviewWithQuery(ctx, domain.ProgressQuery{Scope: scope})
	if err != nil {
		t.Fatalf("default DoneWindow: %v", err)
	}
	if ov7.DoneWindow != "7d" || ov7.DoneWindowTotal != 3 || len(ov7.RecentDone) != 3 {
		t.Fatalf("expected 3 items in default 7d window, got win=%q total=%d len=%d", ov7.DoneWindow, ov7.DoneWindowTotal, len(ov7.RecentDone))
	}

	ov30, err := svc.GetProgressOverviewWithQuery(ctx, domain.ProgressQuery{Scope: scope, DoneWindow: "30d"})
	if err != nil {
		t.Fatalf("DoneWindow 30d: %v", err)
	}
	if ov30.DoneWindowTotal != 4 || len(ov30.RecentDone) != 4 {
		t.Fatalf("expected 4 items in 30d window, got total=%d len=%d", ov30.DoneWindowTotal, len(ov30.RecentDone))
	}

	ovAll, err := svc.GetProgressOverviewWithQuery(ctx, domain.ProgressQuery{Scope: scope, DoneWindow: "all"})
	if err != nil {
		t.Fatalf("DoneWindow all: %v", err)
	}
	if ovAll.DoneWindowTotal != 5 || len(ovAll.RecentDone) != 5 {
		t.Fatalf("expected 5 items in all window, got total=%d len=%d", ovAll.DoneWindowTotal, len(ovAll.RecentDone))
	}
}
