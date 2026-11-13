package memory

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type offlineFailingEval struct {
	failRemote bool
}

func (offlineFailingEval) Name() string { return "offline-failing" }

func (e *offlineFailingEval) ScoreRelevance(_ context.Context, _ string, _ []Fact) ([]ScoredFact, error) {
	if e.failRemote {
		return nil, errors.New("network unreachable: offline")
	}
	return nil, nil
}

func (e *offlineFailingEval) EvaluateMutation(_ context.Context, _, _ string, _ []Fact) (MutationDecision, error) {
	if e.failRemote {
		return MutationDecision{}, errors.New("network unreachable: offline")
	}
	return MutationDecision{
		Category:    "architecture",
		Tier:        "rule",
		Cardinality: "additive",
		IsPermanent: 0.9,
		Action:      "INSERT",
		Confidence:  0.95,
	}, nil
}

func (e *offlineFailingEval) VerifyPlan(_ context.Context, _ string, _ []Fact) ([]PlanViolation, error) {
	if e.failRemote {
		return nil, errors.New("network unreachable: offline")
	}
	return nil, nil
}

func TestLiveSessionsTurnToolStopZeroHeuristicsAndNoDBLeak(t *testing.T) {
	ctx := context.Background()
	eval := &offlineFailingEval{}
	svc, err := NewService(":memory:", eval)
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	defer svc.Close()

	repo := t.TempDir()
	scope := "project:" + strings.ToLower(filepath.Base(repo))
	sid := "conv-live-1"

	// Commit one fact so QuerySessionMemory triggers remote evaluator, then force offline failure.
	if _, err := svc.CommitMemory(ctx, "모든 SQLite 변경은 WAL 모드로 수행한다", "architecture", scope); err != nil {
		t.Fatalf("CommitMemory: %v", err)
	}
	eval.failRemote = true

	// 1. User chat turn marks ephemeral in-memory live session without scraping prompt or writing to SQLite.
	secretTurn := fmt.Sprintf(`{"conversationId":%q,"workspacePaths":[%q],"lastUserInput":"API 키 sk-proj-secret1234567890abcdef 확인"}`, sid, repo)
	if _, code, err := svc.ExecuteHook(ctx, "antigravity-turn", []byte(secretTurn), ""); err != nil || code != 0 {
		t.Fatalf("antigravity-turn failed: code=%d err=%v", code, err)
	}
	live := svc.ListLiveSessions()
	if len(live) != 1 || live[0].SessionID != sid || live[0].Tool != "turn" || live[0].Target != "" {
		t.Fatalf("expected 1 ephemeral turn session without prompt scraping, got %+v", live)
	}
	ov, err := svc.GetProgressOverviewWithQuery(ctx, ProgressQuery{Scope: scope})
	if err != nil {
		t.Fatalf("GetProgressOverviewWithQuery: %v", err)
	}
	var nonMemoryRows int
	if err := svc.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM project_progress WHERE source != 'memory'`).Scan(&nonMemoryRows); err != nil {
		t.Fatalf("count non-memory progress rows: %v", err)
	}
	if nonMemoryRows != 0 || len(ov.DoingTracks) != 0 {
		t.Fatalf("expected zero non-memory project_progress rows from chat turn, got nonMemory=%d doing=%+v", nonMemoryRows, ov.DoingTracks)
	}

	// 2. PreToolUse updates tool and sanitized basename target on the same live session.
	pretoolPayload := fmt.Sprintf(`{"conversationId":%q,"workspacePaths":[%q],"toolCall":{"name":"edit_file","args":{"Path":%q}}}`,
		sid, repo, filepath.Join(repo, "hook.go"))
	if _, code, err := svc.ExecuteHook(ctx, "antigravity-pretool", []byte(pretoolPayload), ""); err != nil || code != 0 {
		t.Fatalf("antigravity-pretool failed: code=%d err=%v", code, err)
	}
	live = svc.ListLiveSessions()
	if len(live) != 1 || live[0].Tool != "edit_file" || live[0].Target != "hook.go" {
		t.Fatalf("expected edit_file + basename target hook.go, got %+v", live)
	}

	// 3. Intermediate stop with fullyIdle=false keeps live session active.
	midStop := fmt.Sprintf(`{"conversationId":%q,"workspacePaths":[%q],"fullyIdle":false}`, sid, repo)
	if _, _, err := svc.ExecuteHook(ctx, "antigravity-stop", []byte(midStop), ""); err != nil {
		t.Fatalf("antigravity-stop non-idle failed: %v", err)
	}
	if got := svc.ListLiveSessions(); len(got) != 1 {
		t.Fatalf("expected non-idle stop to preserve live session, got %+v", got)
	}

	// 4. Final idle stop clears live session immediately.
	idleStop := fmt.Sprintf(`{"conversationId":%q,"workspacePaths":[%q],"fullyIdle":true}`, sid, repo)
	if _, _, err := svc.ExecuteHook(ctx, "antigravity-stop", []byte(idleStop), ""); err != nil {
		t.Fatalf("antigravity-stop idle failed: %v", err)
	}
	if got := svc.ListLiveSessions(); len(got) != 0 {
		t.Fatalf("expected idle stop to clear live session immediately, got %+v", got)
	}

	// 5. claude-end clears live session immediately without leaving orphan DB rows.
	svc.TouchLiveSession(scope, "conv-end-2", "write_to_file", filepath.Join(repo, "README.md"))
	endPayload := fmt.Sprintf(`{"session_id":"conv-end-2","workspacePaths":[%q]}`, repo)
	if _, _, err := svc.ExecuteHook(ctx, "claude-end", []byte(endPayload), ""); err != nil {
		t.Fatalf("claude-end failed: %v", err)
	}
	if got := svc.ListLiveSessions(); len(got) != 0 {
		t.Fatalf("expected claude-end to clear live session, got %+v", got)
	}

	// 6. Synthetic hook session IDs are ignored and stale live entries expire lazily on read (>90s).
	svc.TouchLiveSession(scope, "hook-session:ignored", "turn", "")
	svc.TouchLiveSession(scope, "stdio-ignored", "turn", "")
	svc.TouchLiveSession(scope, "conv-stale", "read_file", filepath.Join(repo, "main.go"))
	svc.mu.Lock()
	staleKey := strings.ToLower(scope) + "::conv-stale"
	if entry, ok := svc.liveSessions[staleKey]; ok {
		entry.updatedAt = time.Now().Add(-LiveSessionTTL - time.Second)
		svc.liveSessions[staleKey] = entry
	}
	svc.mu.Unlock()
	if got := svc.ListLiveSessions(); len(got) != 0 {
		t.Fatalf("expected synthetic IDs ignored and >LiveSessionTTL session expired lazily, got %+v", got)
	}

	var finalNonMemory int
	if err := svc.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM project_progress WHERE source != 'memory'`).Scan(&finalNonMemory); err != nil {
		t.Fatalf("final count non-memory progress rows: %v", err)
	}
	if finalNonMemory != 0 {
		t.Fatalf("expected project_progress DB untouched by live hooks, got %d non-memory rows", finalNonMemory)
	}
}

func TestProgressRecentDoneOrderingWindowAndLocalTZ(t *testing.T) {
	// Mutate time.Local deterministically inside a non-parallel test to verify UTC->Local conversion.
	origLocal := time.Local
	time.Local = time.FixedZone("KST-TEST", 9*3600)
	defer func() { time.Local = origLocal }()

	rec := ProgressRecord{
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
	svc, err := NewService(":memory:", nil)
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	defer svc.Close()

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
	// Row #2: completed 3 hours ago
	insertRow("within-24h", "datetime('now', '-4 hours')", "datetime('now', '-3 hours')")
	// Row #3: completed 3 days ago
	insertRow("within-7d", "datetime('now', '-3 days')", "datetime('now', '-3 days')")
	// Row #4: completed 20 days ago
	insertRow("within-30d", "datetime('now', '-20 days')", "datetime('now', '-20 days')")
	// Row #5: completed 45 days ago
	insertRow("older-than-30d", "datetime('now', '-45 days')", "datetime('now', '-45 days')")

	ov24, err := svc.GetProgressOverviewWithQuery(ctx, ProgressQuery{Scope: scope, DoneWindow: "24h"})
	if err != nil {
		t.Fatalf("DoneWindow 24h: %v", err)
	}
	if ov24.DoneWindow != "24h" || ov24.DoneWindowTotal != 2 || len(ov24.RecentDone) != 2 {
		t.Fatalf("expected 2 items in 24h window, got win=%q total=%d len=%d", ov24.DoneWindow, ov24.DoneWindowTotal, len(ov24.RecentDone))
	}
	if ov24.RecentDone[0].Track != "old-id-recent-finish" || ov24.RecentDone[1].Track != "within-24h" {
		t.Fatalf("expected RecentDone sorted by completion updated_at DESC, got [%s, %s]", ov24.RecentDone[0].Track, ov24.RecentDone[1].Track)
	}

	// Default ("") normalizes to 7d (3 items)
	ov7, err := svc.GetProgressOverviewWithQuery(ctx, ProgressQuery{Scope: scope})
	if err != nil {
		t.Fatalf("default DoneWindow: %v", err)
	}
	if ov7.DoneWindow != "7d" || ov7.DoneWindowTotal != 3 || len(ov7.RecentDone) != 3 {
		t.Fatalf("expected 3 items in default 7d window, got win=%q total=%d len=%d", ov7.DoneWindow, ov7.DoneWindowTotal, len(ov7.RecentDone))
	}

	ov30, err := svc.GetProgressOverviewWithQuery(ctx, ProgressQuery{Scope: scope, DoneWindow: "30d"})
	if err != nil {
		t.Fatalf("DoneWindow 30d: %v", err)
	}
	if ov30.DoneWindowTotal != 4 || len(ov30.RecentDone) != 4 {
		t.Fatalf("expected 4 items in 30d window, got total=%d len=%d", ov30.DoneWindowTotal, len(ov30.RecentDone))
	}

	ovAll, err := svc.GetProgressOverviewWithQuery(ctx, ProgressQuery{Scope: scope, DoneWindow: "all"})
	if err != nil {
		t.Fatalf("DoneWindow all: %v", err)
	}
	if ovAll.DoneWindowTotal != 5 || len(ovAll.RecentDone) != 5 {
		t.Fatalf("expected 5 items in all window, got total=%d len=%d", ovAll.DoneWindowTotal, len(ovAll.RecentDone))
	}
}
