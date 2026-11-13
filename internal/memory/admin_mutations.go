package memory

import (
	"context"
	"fmt"
	"strings"
	"time"
)

func (s *Service) clearInMemorySessionIDs(ids ...int64) {
	s.mu.Lock()
	for _, seen := range s.sent {
		for _, id := range ids {
			delete(seen, id)
		}
	}
	s.mu.Unlock()
}

func (s *Service) clearSessionIDs(ids ...int64) {
	s.clearInMemorySessionIDs(ids...)
	for _, id := range ids {
		_, _ = s.db.Exec(`DELETE FROM session_sent WHERE memory_id = ?`, id)
	}
}

func (s *Service) checkPersonalOwnership(ctx context.Context, id int64, recScope, recAccount string) error {
	if IsAdminContext(ctx) || recScope != "personal" {
		return nil
	}
	cur := s.CurrentAccount(ctx)
	if recAccount != "" && recAccount != cur {
		return fmt.Errorf("permission denied: personal memory #%d belongs to account %q (current: %q)", id, recAccount, cur)
	}
	return nil
}

func (s *Service) loadOwnedScope(ctx context.Context, id int64) (string, string, error) {
	var scope, curAcc string
	if err := s.db.QueryRowContext(ctx, `SELECT scope, COALESCE(account, '') FROM memories WHERE id = ?`, id).Scan(&scope, &curAcc); err != nil {
		return "", "", fmt.Errorf("memory #%d not found", id)
	}
	if err := s.checkPersonalOwnership(ctx, id, scope, curAcc); err != nil {
		return "", "", err
	}
	return scope, curAcc, nil
}

func (s *Service) ForgetMemories(ctx context.Context, ids []int64) (int64, error) {
	if len(ids) == 0 {
		return 0, nil
	}
	for _, id := range ids {
		var sc, acc string
		if err := s.db.QueryRowContext(ctx, `SELECT scope, COALESCE(account, '') FROM memories WHERE id = ?`, id).Scan(&sc, &acc); err == nil {
			if err := s.checkPersonalOwnership(ctx, id, sc, acc); err != nil {
				return 0, err
			}
		}
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	var total int64
	for _, id := range ids {
		res, err := tx.ExecContext(ctx, `UPDATE memories SET active = 0 WHERE id = ? AND active = 1`, id)
		if err != nil {
			return 0, err
		}
		n, _ := res.RowsAffected()
		total += n
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	for _, id := range ids {
		_, _ = s.db.ExecContext(ctx, `INSERT OR IGNORE INTO session_sent (session_id, memory_id) SELECT session_id, memory_id FROM session_updates WHERE memory_id = ?`, id)
		_, _ = s.db.ExecContext(ctx, `DELETE FROM session_updates WHERE memory_id = ?`, id)
	}
	// Keep session_sent rows intact until each active session consumes a 1-time [REVOKED] notice
	s.clearInMemorySessionIDs(ids...)
	s.markAllTurnsMutated()
	return total, nil
}

func (s *Service) UndoMemory(ctx context.Context, id int64) (string, error) {
	var act int
	var recScope, recAcc string
	if err := s.db.QueryRowContext(ctx, `SELECT active, scope, COALESCE(account, '') FROM memories WHERE id = ?`, id).Scan(&act, &recScope, &recAcc); err != nil {
		return "", fmt.Errorf("memory #%d not found", id)
	}
	if err := s.checkPersonalOwnership(ctx, id, recScope, recAcc); err != nil {
		return "", err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT id FROM memories WHERE superseded_by = ?`, id)
	if err != nil {
		return "", err
	}
	var children []int64
	for rows.Next() {
		var cid int64
		if rows.Scan(&cid) == nil {
			children = append(children, cid)
		}
	}
	rows.Close()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer tx.Rollback()

	if act == 1 && len(children) > 0 {
		if _, err := tx.ExecContext(ctx, `UPDATE memories SET active = 0 WHERE id = ?`, id); err != nil {
			return "", err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE memories SET active = 1, superseded_by = NULL WHERE superseded_by = ?`, id); err != nil {
			return "", err
		}
		if err := tx.Commit(); err != nil {
			return "", err
		}
		s.clearSessionIDs(append(children, id)...)
		s.markAllTurnsMutated()
		return fmt.Sprintf("UNDONE OVERWRITE: deactivated #%d and restored %v", id, children), nil
	}

	if _, err := tx.ExecContext(ctx, `UPDATE memories SET active = 1, superseded_by = NULL WHERE id = ?`, id); err != nil {
		return "", err
	}
	if err := tx.Commit(); err != nil {
		return "", err
	}
	s.clearSessionIDs(id)
	s.markAllTurnsMutated()
	return fmt.Sprintf("RESTORED: memory #%d is active again", id), nil
}

func (s *Service) UpdateMemory(ctx context.Context, id int64, category, content string, pinned bool) error {
	return s.UpdateMemoryFull(ctx, id, "", category, nil, content, &pinned)
}

func (s *Service) UpdateMemoryFull(ctx context.Context, id int64, rawScope, category string, subCategory *string, content string, pinned *bool) error {
	var curScope, curAcc, curCat, curSub, curContent string
	var curPin int
	if err := s.db.QueryRowContext(ctx, `SELECT scope, COALESCE(account, ''), category, COALESCE(sub_category, ''), content, pinned FROM memories WHERE id = ?`, id).Scan(&curScope, &curAcc, &curCat, &curSub, &curContent, &curPin); err != nil {
		return fmt.Errorf("memory #%d not found", id)
	}
	if err := s.checkPersonalOwnership(ctx, id, curScope, curAcc); err != nil {
		return err
	}
	scope := curScope
	if strings.TrimSpace(rawScope) != "" {
		scope = NormalizeScope(rawScope)
	}
	acc := curAcc
	if acc == "" {
		acc = s.CurrentAccount(ctx)
	}
	cat := curCat
	sub := curSub
	if strings.TrimSpace(category) != "" {
		if strings.Contains(category, "/") && subCategory == nil {
			cat, sub = SplitCategoryAndSub(scope, category, "")
		} else {
			cat = NormalizeCategoryForScope(scope, category)
		}
	}
	if subCategory != nil {
		sub = NormalizeSubCategory(*subCategory)
	}

	txt := curContent
	if strings.TrimSpace(content) != "" {
		if reason := checkSecurityGuard(content); reason != "" {
			return fmt.Errorf("security guard: %s", reason)
		}
		txt = strings.TrimSpace(content)
	}
	pin := curPin
	if pinned != nil {
		pin = boolToInt(*pinned)
	}
	_, err := s.db.ExecContext(ctx, `UPDATE memories SET scope = ?, account = ?, category = ?, sub_category = ?, content = ?, pinned = ? WHERE id = ?`, scope, acc, cat, sub, txt, pin, id)
	if err == nil && txt != curContent {
		_, _ = s.db.ExecContext(ctx, `
			INSERT INTO session_updates (session_id, memory_id, prev_content, updated_at)
			SELECT session_id, ?, ?, CURRENT_TIMESTAMP FROM session_sent WHERE memory_id = ?
			ON CONFLICT(session_id, memory_id) DO UPDATE SET updated_at = CURRENT_TIMESTAMP
		`, id, curContent, id)
	}
	s.clearSessionIDs(id)
	s.markAllTurnsMutated()
	if err == nil {
		s.RegisterTaxonomyAppendOnly(ctx, scope, cat, sub)
	}
	if err == nil && (txt != curContent || cat != curCat || sub != curSub) {
		s.RecordMemoryMutationProgress(ctx, scope, cat, sub, "update", id, nil, txt)
	}
	return err
}

func (s *Service) UpdateMemoryAccount(ctx context.Context, id int64, rawAccount string) error {
	if _, _, err := s.loadOwnedScope(ctx, id); err != nil {
		return err
	}
	acc := NormalizeAccount(rawAccount)
	if acc == "" {
		acc = s.CurrentAccount(ctx)
	}
	_, err := s.db.ExecContext(ctx, `UPDATE memories SET account = ? WHERE id = ?`, acc, id)
	s.clearSessionIDs(id)
	return err
}

func (s *Service) UpdateMemoryTags(ctx context.Context, id int64, rawTags string) error {
	scope, _, err := s.loadOwnedScope(ctx, id)
	if err != nil {
		return err
	}
	parts := strings.FieldsFunc(rawTags, func(r rune) bool {
		return r == ',' || r == ' ' || r == '\t' || r == '\n' || r == '#'
	})
	tags := NormalizeTags(scope, parts)
	_, err = s.db.ExecContext(ctx, `UPDATE memories SET tags = ? WHERE id = ?`, strings.Join(tags, " "), id)
	s.clearSessionIDs(id)
	return err
}

func (s *Service) PurgeTombstones(ctx context.Context, scopeFilter string) (int64, error) {
	sc := strings.TrimSpace(scopeFilter)
	isAdmin := IsAdminContext(ctx)
	curAcc := s.CurrentAccount(ctx)
	// Snapshot direct superseded lineage into parent's replaces_text before deleting inactive rows
	rows, err := s.db.QueryContext(ctx, `
		SELECT superseded_by, content FROM memories
		WHERE active = 0 AND superseded_by IS NOT NULL AND (? = '' OR scope = ?)
		  AND (? = 1 OR scope != 'personal' OR account = ?)
		ORDER BY id ASC`, sc, sc, boolToInt(isAdmin), curAcc)
	if err == nil {
		byParent := make(map[int64][]string)
		for rows.Next() {
			var pid int64
			var c string
			if rows.Scan(&pid, &c) == nil && strings.TrimSpace(c) != "" {
				byParent[pid] = append(byParent[pid], strings.TrimSpace(c))
			}
		}
		rows.Close()
		for pid, items := range byParent {
			var existing string
			_ = s.db.QueryRowContext(ctx, `SELECT COALESCE(replaces_text, '') FROM memories WHERE id = ?`, pid).Scan(&existing)
			seen := make(map[string]bool)
			var merged []string
			for _, line := range strings.Split(existing, "\n") {
				if line = strings.TrimSpace(line); line != "" && !seen[line] {
					seen[line] = true
					merged = append(merged, line)
				}
			}
			for _, it := range items {
				if !seen[it] {
					seen[it] = true
					merged = append(merged, it)
				}
			}
			_, _ = s.db.ExecContext(ctx, `UPDATE memories SET replaces_text = ? WHERE id = ?`, strings.Join(merged, "\n"), pid)
		}
	}

	_, _ = s.db.ExecContext(ctx, `DELETE FROM session_sent WHERE memory_id IN (SELECT id FROM memories WHERE active = 0 AND (? = '' OR scope = ?) AND (? = 1 OR scope != 'personal' OR account = ?))`, sc, sc, boolToInt(isAdmin), curAcc)
	res, err := s.db.ExecContext(ctx, `DELETE FROM memories WHERE active = 0 AND (? = '' OR scope = ?) AND (? = 1 OR scope != 'personal' OR account = ?)`, sc, sc, boolToInt(isAdmin), curAcc)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	if n > 0 {
		s.NotifyDomainChange("memories")
	}
	return n, nil
}

func boolToInt(v bool) int {
	if v {
		return 1
	}
	return 0
}

// RecalibrateConfidence re-evaluates individual semantic confidence [0.0, 1.0] for active memories
// using the configured AI evaluator so each memory reflects its actual certainty rather than legacy fallback constants.
func (s *Service) RecalibrateConfidence(ctx context.Context, scopeFilter string) (int, error) {
	ev := s.evalForTask(ctx, TaskCalibrate)
	if ev == nil {
		return 0, fmt.Errorf("no active evaluation engine configured")
	}
	cal, ok := ev.(ConfidenceCalibrator)
	if !ok || !EvaluatorSupportsTask(ev, TaskCalibrate) {
		return 0, fmt.Errorf("active engine %q uses native System 1 confidence or does not support batch recalibration", ev.Name())
	}

	sc := strings.TrimSpace(scopeFilter)
	isAdmin := IsAdminContext(ctx)
	curAcc := s.CurrentAccount(ctx)
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, scope, COALESCE(account, ''), category, COALESCE(sub_category, ''), tags, content, pinned
		FROM memories
		WHERE active = 1 AND (? = '' OR scope = ?)
		  AND (? = 1 OR scope != 'personal' OR account = ?)
		ORDER BY scope ASC, id ASC`, sc, sc, boolToInt(isAdmin), curAcc)
	if err != nil {
		return 0, err
	}
	byScope := make(map[string][]Fact)
	var scopeOrder []string
	for rows.Next() {
		var f Fact
		var tagStr string
		var pin int
		if err := rows.Scan(&f.ID, &f.Scope, &f.Account, &f.Category, &f.SubCategory, &tagStr, &f.Content, &pin); err != nil {
			rows.Close()
			return 0, err
		}
		f.Pinned = pin == 1
		f.Tags = parseTagStr(tagStr)
		if _, seen := byScope[f.Scope]; !seen {
			scopeOrder = append(scopeOrder, f.Scope)
		}
		byScope[f.Scope] = append(byScope[f.Scope], f)
	}
	if err := rows.Close(); err != nil {
		return 0, err
	}
	if len(scopeOrder) == 0 {
		return 0, nil
	}

	updates := make(map[int64]float64)
	for _, scopeName := range scopeOrder {
		evalStart := time.Now()
		part, err := cal.CalibrateConfidence(ctx, scopeName, byScope[scopeName])
		s.recordEvalTelemetry(ctx, TaskCalibrate, ev, evalStart, len(byScope[scopeName]), err)
		if err != nil {
			return 0, err
		}
		for id, conf := range part {
			updates[id] = clamp(conf, 0.05, 1.00)
		}
	}
	if len(updates) == 0 {
		return 0, nil
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	updated := 0
	for id, conf := range updates {
		res, err := tx.ExecContext(ctx, `UPDATE memories SET confidence = ? WHERE id = ? AND active = 1`, conf, id)
		if err != nil {
			return 0, err
		}
		if n, _ := res.RowsAffected(); n > 0 {
			updated++
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	if updated > 0 {
		s.NotifyDomainChange("memories")
	}
	return updated, nil
}
