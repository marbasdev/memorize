package guardrail_test

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"memorize/internal/domain"
)

type offlineFailingEval struct {
	failRemote bool
}

func (offlineFailingEval) Name() string { return "offline-failing" }

func (e *offlineFailingEval) ScoreRelevance(_ context.Context, _ string, _ []domain.Fact) ([]domain.ScoredFact, error) {
	if e.failRemote {
		return nil, errors.New("network unreachable: offline")
	}
	return nil, nil
}

func (e *offlineFailingEval) EvaluateMutation(_ context.Context, _, _ string, _ []domain.Fact) (domain.MutationDecision, error) {
	if e.failRemote {
		return domain.MutationDecision{}, errors.New("network unreachable: offline")
	}
	return domain.MutationDecision{
		Category:    "architecture",
		Tier:        "rule",
		Cardinality: "additive",
		IsPermanent: 0.9,
		Action:      "INSERT",
		Confidence:  0.95,
	}, nil
}

func (e *offlineFailingEval) VerifyPlan(_ context.Context, _ string, _ []domain.Fact) ([]domain.PlanViolation, error) {
	if e.failRemote {
		return nil, errors.New("network unreachable: offline")
	}
	return nil, nil
}

func TestLiveSessionsTurnToolStopZeroHeuristicsAndNoDBLeak(t *testing.T) {
	ctx := context.Background()
	eval := &offlineFailingEval{}
	st := newTestStack(t, ":memory:", eval)

	repo := t.TempDir()
	scope := "project:" + strings.ToLower(filepath.Base(repo))
	sid := "conv-live-1"

	// Commit one fact so QuerySessionMemory triggers remote evaluator, then force offline failure.
	if _, err := st.mem.CommitMemory(ctx, "모든 SQLite 변경은 WAL 모드로 수행한다", "architecture", scope); err != nil {
		t.Fatalf("CommitMemory: %v", err)
	}
	eval.failRemote = true

	// 1. User chat turn marks ephemeral in-memory live session without scraping prompt or writing to SQLite.
	secretTurn := fmt.Sprintf(`{"conversationId":%q,"workspacePaths":[%q],"lastUserInput":"API 키 sk-proj-secret1234567890abcdef 확인"}`, sid, repo)
	if _, code, err := st.gr.ExecuteHook(ctx, "antigravity-turn", []byte(secretTurn), ""); err != nil || code != 0 {
		t.Fatalf("antigravity-turn failed: code=%d err=%v", code, err)
	}
	live := st.prog.ListLiveSessions()
	if len(live) != 1 || live[0].SessionID != sid || live[0].Tool != "turn" || live[0].Target != "" {
		t.Fatalf("expected 1 ephemeral turn session without prompt scraping, got %+v", live)
	}
	ov, err := st.prog.GetProgressOverviewWithQuery(ctx, domain.ProgressQuery{Scope: scope})
	if err != nil {
		t.Fatalf("GetProgressOverviewWithQuery: %v", err)
	}
	var nonMemoryRows int
	if err := st.mem.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM project_progress WHERE source != 'memory'`).Scan(&nonMemoryRows); err != nil {
		t.Fatalf("count non-memory progress rows: %v", err)
	}
	if nonMemoryRows != 0 || len(ov.DoingTracks) != 0 {
		t.Fatalf("expected zero non-memory project_progress rows from chat turn, got nonMemory=%d doing=%+v", nonMemoryRows, ov.DoingTracks)
	}

	// 2. PreToolUse updates tool and sanitized basename target on the same live session.
	pretoolPayload := fmt.Sprintf(`{"conversationId":%q,"workspacePaths":[%q],"toolCall":{"name":"edit_file","args":{"Path":%q}}}`,
		sid, repo, filepath.Join(repo, "hook.go"))
	if _, code, err := st.gr.ExecuteHook(ctx, "antigravity-pretool", []byte(pretoolPayload), ""); err != nil || code != 0 {
		t.Fatalf("antigravity-pretool failed: code=%d err=%v", code, err)
	}
	live = st.prog.ListLiveSessions()
	if len(live) != 1 || live[0].Tool != "edit_file" || live[0].Target != "hook.go" {
		t.Fatalf("expected edit_file + basename target hook.go, got %+v", live)
	}

	// 3. Intermediate stop with fullyIdle=false keeps live session active.
	midStop := fmt.Sprintf(`{"conversationId":%q,"workspacePaths":[%q],"fullyIdle":false}`, sid, repo)
	if _, _, err := st.gr.ExecuteHook(ctx, "antigravity-stop", []byte(midStop), ""); err != nil {
		t.Fatalf("antigravity-stop non-idle failed: %v", err)
	}
	if got := st.prog.ListLiveSessions(); len(got) != 1 {
		t.Fatalf("expected non-idle stop to preserve live session, got %+v", got)
	}

	// 4. Final idle stop clears live session immediately.
	idleStop := fmt.Sprintf(`{"conversationId":%q,"workspacePaths":[%q],"fullyIdle":true}`, sid, repo)
	if _, _, err := st.gr.ExecuteHook(ctx, "antigravity-stop", []byte(idleStop), ""); err != nil {
		t.Fatalf("antigravity-stop idle failed: %v", err)
	}
	if got := st.prog.ListLiveSessions(); len(got) != 0 {
		t.Fatalf("expected idle stop to clear live session immediately, got %+v", got)
	}

	// 5. claude-end clears live session immediately without leaving orphan DB rows.
	st.prog.TouchLiveSession(scope, "conv-end-2", "write_to_file", filepath.Join(repo, "README.md"))
	endPayload := fmt.Sprintf(`{"session_id":"conv-end-2","workspacePaths":[%q]}`, repo)
	if _, _, err := st.gr.ExecuteHook(ctx, "claude-end", []byte(endPayload), ""); err != nil {
		t.Fatalf("claude-end failed: %v", err)
	}
	if got := st.prog.ListLiveSessions(); len(got) != 0 {
		t.Fatalf("expected claude-end to clear live session, got %+v", got)
	}

	var finalNonMemory int
	if err := st.mem.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM project_progress WHERE source != 'memory'`).Scan(&finalNonMemory); err != nil {
		t.Fatalf("final count non-memory progress rows: %v", err)
	}
	if finalNonMemory != 0 {
		t.Fatalf("expected project_progress DB untouched by live hooks, got %d non-memory rows", finalNonMemory)
	}
}
