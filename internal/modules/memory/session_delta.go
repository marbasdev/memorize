package memory

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strings"
	"time"

	"memorize/internal/domain"
)

type matchedItem struct {
	fact   domain.Fact
	suffix string
}

func (s *Service) snapshotSessionSeen(ctx context.Context, sessionID string, refresh bool) map[int64]bool {
	if sessionID == "" {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if refresh {
		s.sent[sessionID] = make(map[int64]bool)
		_, _ = s.db.ExecContext(ctx, `DELETE FROM session_sent WHERE session_id = ?`, sessionID)
		_, _ = s.db.ExecContext(ctx, `DELETE FROM session_updates WHERE session_id = ?`, sessionID)
	} else if s.sent[sessionID] == nil {
		s.sent[sessionID] = make(map[int64]bool)
		if rows, err := s.db.QueryContext(ctx, `SELECT memory_id FROM session_sent WHERE session_id = ?`, sessionID); err == nil {
			for rows.Next() {
				var mid int64
				if rows.Scan(&mid) == nil {
					s.sent[sessionID][mid] = true
				}
			}
			rows.Close()
		}
	}
	cp := make(map[int64]bool, len(s.sent[sessionID]))
	for k, v := range s.sent[sessionID] {
		if v {
			cp[k] = true
		}
	}
	return cp
}

func (s *Service) recordSessionItems(ctx context.Context, sessionID string, matched []matchedItem) (string, int) {
	var sb strings.Builder
	if sessionID == "" {
		for _, m := range matched {
			sb.WriteString(formatFactLine(m.fact, m.suffix, false))
		}
		return sb.String(), len(matched)
	}

	var newlySent []int64
	deliveredCount := 0

	s.mu.Lock()
	if s.sent[sessionID] == nil {
		s.sent[sessionID] = make(map[int64]bool)
	}
	seen := s.sent[sessionID]
	for _, m := range matched {
		if seen[m.fact.ID] {
			continue
		}
		seen[m.fact.ID] = true
		newlySent = append(newlySent, m.fact.ID)
		sb.WriteString(formatFactLine(m.fact, m.suffix, true))
		deliveredCount++
	}
	s.mu.Unlock()

	for _, mid := range newlySent {
		_, _ = s.db.ExecContext(ctx, `INSERT OR IGNORE INTO session_sent (session_id, memory_id) VALUES (?, ?)`, sessionID, mid)
	}
	return sb.String(), deliveredCount
}

func (s *Service) collectRevokedSessionItems(ctx context.Context, sessionID string, refresh bool) string {
	if sessionID == "" || refresh {
		return ""
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT m.id, m.scope, m.category, COALESCE(m.sub_category, ''), m.content
		FROM session_sent ss
		JOIN memories m ON m.id = ss.memory_id
		WHERE ss.session_id = ? AND m.active = 0 AND m.superseded_by IS NULL
		ORDER BY m.id ASC`, sessionID)
	if err != nil {
		return ""
	}
	defer rows.Close()

	var sb strings.Builder
	var revokedIDs []int64
	for rows.Next() {
		var id int64
		var sc, cat, sub, content string
		if rows.Scan(&id, &sc, &cat, &sub, &content) == nil {
			revokedIDs = append(revokedIDs, id)
			catLabel := cat
			if sub != "" {
				catLabel = cat + "/" + sub
			}
			fmt.Fprintf(&sb, "- **[REVOKED %s / %s #%d]** ~~%s~~ *(Deprecated/Deleted — ignore any earlier occurrence in this conversation)*\n", sc, catLabel, id, content)
		}
	}
	if len(revokedIDs) > 0 {
		s.mu.Lock()
		if seen := s.sent[sessionID]; seen != nil {
			for _, id := range revokedIDs {
				delete(seen, id)
			}
		}
		s.mu.Unlock()
		for _, id := range revokedIDs {
			_, _ = s.db.ExecContext(ctx, `DELETE FROM session_sent WHERE session_id = ? AND memory_id = ?`, sessionID, id)
		}
	}
	return sb.String()
}

func (s *Service) resolveSupersededRoot(ctx context.Context, startID int64) int64 {
	cur := startID
	for range 8 {
		var act int
		var next sql.NullInt64
		if err := s.db.QueryRowContext(ctx, `SELECT active, superseded_by FROM memories WHERE id = ?`, cur).Scan(&act, &next); err != nil {
			return cur
		}
		if act == 1 || !next.Valid || next.Int64 <= 0 || next.Int64 == cur {
			return cur
		}
		cur = next.Int64
	}
	return cur
}

func (s *Service) applySessionSuperseded(ctx context.Context, sessionID string, refresh bool, matched []matchedItem) string {
	for i := range matched {
		matched[i].fact.Superseded = nil
	}
	if sessionID == "" || refresh {
		return ""
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT m.id, m.scope, m.category, COALESCE(m.sub_category, ''), m.content, m.superseded_by
		FROM session_sent ss
		JOIN memories m ON m.id = ss.memory_id
		WHERE ss.session_id = ? AND m.active = 0 AND m.superseded_by IS NOT NULL
		ORDER BY m.id ASC`, sessionID)
	if err != nil {
		return ""
	}
	defer rows.Close()

	idxByID := make(map[int64]int, len(matched))
	for i, m := range matched {
		idxByID[m.fact.ID] = i
	}

	type oldItem struct {
		id      int64
		sc      string
		cat     string
		sub     string
		content string
		parent  int64
	}
	var items []oldItem
	for rows.Next() {
		var it oldItem
		if rows.Scan(&it.id, &it.sc, &it.cat, &it.sub, &it.content, &it.parent) == nil {
			items = append(items, it)
		}
	}
	rows.Close()
	if len(items) == 0 {
		return ""
	}

	var fallback strings.Builder
	var consumed []int64
	for _, it := range items {
		rootID := s.resolveSupersededRoot(ctx, it.parent)
		if idx, ok := idxByID[rootID]; ok {
			dup := false
			for _, ex := range matched[idx].fact.Superseded {
				if ex == it.content {
					dup = true
					break
				}
			}
			if !dup {
				matched[idx].fact.Superseded = append(matched[idx].fact.Superseded, it.content)
			}
		} else {
			catLabel := it.cat
			if it.sub != "" {
				catLabel = it.cat + "/" + it.sub
			}
			fmt.Fprintf(&fallback, "- **[REVOKED %s / %s #%d]** ~~%s~~ *(Superseded by #%d — ignore any earlier occurrence in this conversation)*\n",
				it.sc, catLabel, it.id, it.content, rootID)
		}
		consumed = append(consumed, it.id)
	}

	s.mu.Lock()
	if seen := s.sent[sessionID]; seen != nil {
		for _, id := range consumed {
			delete(seen, id)
		}
	}
	s.mu.Unlock()
	for _, id := range consumed {
		_, _ = s.db.ExecContext(ctx, `DELETE FROM session_sent WHERE session_id = ? AND memory_id = ?`, sessionID, id)
	}
	return fallback.String()
}

func (s *Service) pendingSessionUpdates(ctx context.Context, sessionID string, refresh bool) map[int64]string {
	if sessionID == "" || refresh {
		return nil
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT su.memory_id, su.prev_content
		FROM session_updates su
		JOIN memories m ON m.id = su.memory_id
		WHERE su.session_id = ? AND m.active = 1`, sessionID)
	if err != nil {
		return nil
	}
	defer rows.Close()
	out := make(map[int64]string)
	for rows.Next() {
		var mid int64
		var prev string
		if rows.Scan(&mid, &prev) == nil && strings.TrimSpace(prev) != "" {
			out[mid] = strings.TrimSpace(prev)
		}
	}
	return out
}

func (s *Service) consumeSessionUpdates(ctx context.Context, sessionID string, ids []int64) {
	if sessionID == "" || len(ids) == 0 {
		return
	}
	for _, id := range ids {
		_, _ = s.db.ExecContext(ctx, `DELETE FROM session_updates WHERE session_id = ? AND memory_id = ?`, sessionID, id)
	}
}

// QuerySessionMemory returns unseen relevant/pinned facts and revocation/update deltas for sessionID.
func (s *Service) QuerySessionMemory(ctx context.Context, sessionID, currentTask, category, rawScope string, refresh bool) (string, error) {
	revokedNotice := s.collectRevokedSessionItems(ctx, sessionID, refresh)
	scA, scB := ScopesFor(rawScope, true)
	var extraScopes []string
	if strings.HasPrefix(scA, "project:") && s.IncludePersonalInProject(ctx) {
		extraScopes = append(extraScopes, "personal")
	}
	facts, err := s.LoadActiveFacts(ctx, scA, scB, category, extraScopes...)
	if err != nil {
		return "", err
	}
	if len(facts) == 0 {
		if refresh && sessionID != "" {
			_, _ = s.db.ExecContext(ctx, `DELETE FROM session_updates WHERE session_id = ?`, sessionID)
		}
		revokedNotice += s.applySessionSuperseded(ctx, sessionID, refresh, nil)
		if revokedNotice != "" {
			return revokedNotice, nil
		}
		return fmt.Sprintf("No active memories found for scope [%s].", scA), nil
	}

	seen := s.snapshotSessionSeen(ctx, sessionID, refresh)
	pendingUpd := s.pendingSessionUpdates(ctx, sessionID, refresh)
	var unsentPinned, unsentCandidates []domain.Fact
	var pendingUpdatedFacts []domain.Fact
	var alreadyIDs []string
	for _, f := range facts {
		if seen[f.ID] {
			alreadyIDs = append(alreadyIDs, fmt.Sprintf("#%d", f.ID))
			continue
		}
		if _, wasUpdated := pendingUpd[f.ID]; wasUpdated {
			pendingUpdatedFacts = append(pendingUpdatedFacts, f)
			continue
		}
		if f.Pinned {
			unsentPinned = append(unsentPinned, f)
		} else {
			unsentCandidates = append(unsentCandidates, f)
		}
	}

	th := s.GetThresholds(ctx)
	var matched []matchedItem
	var consumedUpdates []int64
	for _, f := range pendingUpdatedFacts {
		prev := pendingUpd[f.ID]
		suf := fmt.Sprintf(" *(Updated #%d — replaces earlier text: %s)*", f.ID, prev)
		if f.Pinned {
			suf = " *(Core Invariant)*" + suf
		}
		matched = append(matched, matchedItem{fact: f, suffix: suf})
		consumedUpdates = append(consumedUpdates, f.ID)
	}
	for _, f := range unsentPinned {
		matched = append(matched, matchedItem{fact: f, suffix: " *(Core Invariant)*"})
	}
	hasTaskPrompt := strings.TrimSpace(currentTask) != ""
	if len(unsentCandidates) > 0 && hasTaskPrompt {
		ev := s.router.EvalForTask(ctx, domain.TaskRelevance)
		if ev == nil {
			return "", fmt.Errorf("no active evaluation engine configured")
		}
		evalStart := time.Now()
		scored, err := ev.ScoreRelevance(ctx, currentTask, unsentCandidates)
		s.router.RecordTelemetry(ctx, domain.TaskRelevance, ev, evalStart, len(unsentCandidates), err)
		if err != nil {
			return "", err
		}
		sort.Slice(scored, func(i, j int) bool {
			if scored[i].Score != scored[j].Score {
				return scored[i].Score > scored[j].Score
			}
			return scored[i].ID < scored[j].ID
		})
		for _, sf := range scored {
			if sf.Score >= th.MinRelevance && sf.Confidence >= th.MinRelConfidence {
				sfx := ""
				if sessionID == "" {
					sfx = fmt.Sprintf(" *(score: %.1f, conf: %.2f)*", sf.Score, sf.Confidence)
				}
				matched = append(matched, matchedItem{
					fact:   sf.Fact,
					suffix: sfx,
				})
			}
		}
	}

	revokedNotice += s.applySessionSuperseded(ctx, sessionID, refresh, matched)

	if len(matched) == 0 {
		if len(alreadyIDs) > 0 && revokedNotice == "" {
			return fmt.Sprintf("No new memories (%d already active in session). Pass refresh=true if context was compacted.", len(alreadyIDs)), nil
		}
		if revokedNotice != "" {
			if len(alreadyIDs) > 0 {
				return fmt.Sprintf("%s_(+%d active in session)_\n", revokedNotice, len(alreadyIDs)), nil
			}
			return revokedNotice, nil
		}
		if !hasTaskPrompt {
			return fmt.Sprintf("No active pinned memories found for scope [%s] (task rules evaluate on prompt).", scA), nil
		}
		return fmt.Sprintf("No relevant memories matched threshold (Score >= %.2f, Confidence >= %.2f).", th.MinRelevance, th.MinRelConfidence), nil
	}

	rendered, _ := s.recordSessionItems(ctx, sessionID, matched)
	s.consumeSessionUpdates(ctx, sessionID, consumedUpdates)

	var sb strings.Builder
	sb.WriteString(revokedNotice)
	sb.WriteString(rendered)
	if len(alreadyIDs) > 0 {
		fmt.Fprintf(&sb, "_(+%d active in session)_\n", len(alreadyIDs))
	}
	return sb.String(), nil
}

// RecordTurnState stores the latest user prompt for sessionID/scope to power pre-mutation reminders and session ID discovery.
func (s *Service) RecordTurnState(ctx context.Context, sessionID, rawScope, userInput string) {
	sid := strings.TrimSpace(sessionID)
	inp := strings.TrimSpace(userInput)
	if sid == "" || inp == "" {
		return
	}
	sc := domain.NormalizeScope(rawScope)
	now := time.Now().Unix()
	_, _ = s.db.ExecContext(ctx, `
		INSERT INTO turn_state (session_id, scope, last_input, mutated, gate_checked, updated_at)
		VALUES (?, ?, ?, 0, 0, ?)
		ON CONFLICT(session_id, scope) DO UPDATE SET
			mutated = CASE WHEN turn_state.last_input = excluded.last_input THEN turn_state.mutated ELSE 0 END,
			gate_checked = CASE WHEN turn_state.last_input = excluded.last_input THEN turn_state.gate_checked ELSE 0 END,
			last_input = excluded.last_input,
			updated_at = excluded.updated_at
	`, sid, sc, inp, now)
	_, _ = s.db.ExecContext(ctx, `DELETE FROM turn_state WHERE updated_at < ?`, now-7200)
}

// MarkTurnScopeMutated marks active turns in rawScope (and publishes a "memories" event) after a commit/mutation.
func (s *Service) MarkTurnScopeMutated(rawScope string) {
	sc := domain.NormalizeScope(rawScope)
	_, _ = s.db.Exec(`UPDATE turn_state SET mutated = 1 WHERE scope = ? OR ? = 'global'`, sc, sc)
	s.bus.Publish("memories")
}

// MarkAllTurnsMutated marks all active turns as mutated and publishes a "memories" change event.
func (s *Service) MarkAllTurnsMutated() {
	_, _ = s.db.Exec(`UPDATE turn_state SET mutated = 1`)
	s.bus.Publish("memories")
}

// ConsumeTurnPreMutateCandidate returns an un-checked active turn input once for pre-mutation guardrail gating.
func (s *Service) ConsumeTurnPreMutateCandidate(ctx context.Context, sessionID, rawScope, fallbackInput string) string {
	sid := strings.TrimSpace(sessionID)
	if sid == "" {
		return ""
	}
	sc := domain.NormalizeScope(rawScope)
	cutoff := time.Now().Add(-15 * time.Minute).Unix()
	var lastInput string
	var mutated, gateChecked int
	err := s.db.QueryRowContext(ctx,
		`SELECT last_input, mutated, gate_checked FROM turn_state WHERE session_id = ? AND scope = ? AND updated_at >= ?`,
		sid, sc, cutoff).Scan(&lastInput, &mutated, &gateChecked)
	if err != nil {
		if fb := strings.TrimSpace(fallbackInput); fb != "" {
			s.RecordTurnState(ctx, sid, sc, fb)
			lastInput = fb
			mutated = 0
			gateChecked = 0
		} else {
			return ""
		}
	}
	if mutated != 0 || gateChecked != 0 || strings.TrimSpace(lastInput) == "" {
		return ""
	}
	_, _ = s.db.ExecContext(ctx, `UPDATE turn_state SET gate_checked = 1 WHERE session_id = ? AND scope = ?`, sid, sc)
	return strings.TrimSpace(lastInput)
}

// IsTurnMutated reports whether commit_memory/manage_memory already mutated state in the current turn.
func (s *Service) IsTurnMutated(ctx context.Context, sessionID, rawScope string) bool {
	sid := strings.TrimSpace(sessionID)
	if sid == "" {
		return false
	}
	sc := domain.NormalizeScope(rawScope)
	var mutated int
	if err := s.db.QueryRowContext(ctx, `SELECT mutated FROM turn_state WHERE session_id = ? AND scope = ?`, sid, sc).Scan(&mutated); err != nil {
		return false
	}
	return mutated != 0
}

// ActiveTurnInput returns the active turn's last user prompt if recorded within the last 15 minutes.
func (s *Service) ActiveTurnInput(ctx context.Context, sessionID, rawScope string) string {
	sid := strings.TrimSpace(sessionID)
	if sid == "" {
		return ""
	}
	sc := domain.NormalizeScope(rawScope)
	cutoff := time.Now().Add(-15 * time.Minute).Unix()
	var lastInput string
	if err := s.db.QueryRowContext(ctx,
		`SELECT last_input FROM turn_state WHERE session_id = ? AND scope = ? AND updated_at >= ?`,
		sid, sc, cutoff).Scan(&lastInput); err != nil {
		return ""
	}
	return strings.TrimSpace(lastInput)
}
