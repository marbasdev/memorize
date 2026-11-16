package guardrail

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"memorize/internal/domain"
	"memorize/internal/infra/git"
)

const (
	maxTurnBaselineBytes = 128 * 1024
	maxPerFileDiffBytes  = 1200
	maxBatchDiffBytes    = 4000
)

type pendingTurnEdit struct {
	SessionID       string
	Scope           string
	FilePath        string
	RepoDir         string
	TurnInput       string
	BaselineExists  int
	BaselineContent string
	FallbackSnippet string
}

func resolveCanonicalToolPath(rawPath, repoDir, cwd string) string {
	p := strings.TrimSpace(rawPath)
	if p == "" {
		return ""
	}
	if !filepath.IsAbs(p) {
		if base := domain.FirstNonEmpty(repoDir, cwd); base != "" {
			p = filepath.Join(base, p)
		}
	}
	return filepath.Clean(p)
}

func captureFileBaseline(path string) (exists int, content string) {
	if strings.HasPrefix(path, "_unnamed:") {
		return -1, ""
	}
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return 0, ""
	}
	if err != nil || len(data) > maxTurnBaselineBytes || !utf8.Valid(data) || bytes.IndexByte(data, 0) >= 0 {
		return -1, ""
	}
	return 1, string(data)
}

func (s *Service) recordPendingTurnEdit(ctx context.Context, sid, scope, repoDir, toolName string, ev NormalizedHookEvent, snippet string) {
	if strings.TrimSpace(sid) == "" || s.db == nil {
		return
	}
	sc := domain.NormalizeScope(scope)
	targetPath := resolveCanonicalToolPath(ev.TargetPath, repoDir, ev.Cwd)
	if targetPath == "" {
		targetPath = "_unnamed:" + strings.ToLower(strings.TrimSpace(toolName))
	}

	var turnInp string
	if ev.SessionID != "" {
		turnInp = s.mem.ActiveTurnInput(ctx, ev.SessionID, sc)
	}
	turnInp = domain.FirstNonEmpty(turnInp, ev.TurnPrompt)

	now := time.Now().Unix()
	if turnInp != "" {
		_, _ = s.db.ExecContext(ctx,
			`DELETE FROM turn_pending_edits WHERE session_id = ? AND scope = ? AND turn_input != '' AND turn_input != ?`,
			sid, sc, turnInp)
	}

	baselineExists, baselineContent := captureFileBaseline(targetPath)
	fallbackSnippet := ""
	if baselineExists == -1 {
		fallbackSnippet = truncateUTF8Bytes(strings.TrimSpace(snippet), maxPerFileDiffBytes)
	}

	_, _ = s.db.ExecContext(ctx, `
		INSERT INTO turn_pending_edits (
			session_id, scope, file_path, repo_dir, turn_input,
			baseline_exists, baseline_content, fallback_snippet, updated_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(session_id, scope, file_path) DO UPDATE SET
			updated_at = excluded.updated_at,
			repo_dir = CASE WHEN excluded.repo_dir != '' THEN excluded.repo_dir ELSE turn_pending_edits.repo_dir END,
			turn_input = CASE WHEN excluded.turn_input != '' THEN excluded.turn_input ELSE turn_pending_edits.turn_input END,
			fallback_snippet = CASE
				WHEN turn_pending_edits.baseline_exists = -1 AND excluded.fallback_snippet != '' THEN
					CASE
						WHEN turn_pending_edits.fallback_snippet = '' THEN excluded.fallback_snippet
						ELSE substr(turn_pending_edits.fallback_snippet || char(10) || excluded.fallback_snippet, 1, 2000)
					END
				ELSE turn_pending_edits.fallback_snippet
			END
	`, sid, sc, targetPath, strings.TrimSpace(repoDir), turnInp, baselineExists, baselineContent, fallbackSnippet, now)

	_, _ = s.db.ExecContext(ctx, `DELETE FROM turn_pending_edits WHERE updated_at < ?`, now-7200)
}

func (s *Service) clearPendingTurnEdits(ctx context.Context, sid string) {
	if strings.TrimSpace(sid) == "" || s.db == nil {
		return
	}
	_, _ = s.db.ExecContext(ctx, `DELETE FROM turn_pending_edits WHERE session_id = ?`, sid)
}

func (s *Service) loadPendingTurnEdits(ctx context.Context, sid string) ([]pendingTurnEdit, error) {
	if strings.TrimSpace(sid) == "" || s.db == nil {
		return nil, nil
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT session_id, scope, file_path, repo_dir, turn_input, baseline_exists, baseline_content, fallback_snippet
		FROM turn_pending_edits
		WHERE session_id = ?
		ORDER BY updated_at ASC, file_path ASC
	`, sid)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []pendingTurnEdit
	for rows.Next() {
		var e pendingTurnEdit
		if err := rows.Scan(&e.SessionID, &e.Scope, &e.FilePath, &e.RepoDir, &e.TurnInput, &e.BaselineExists, &e.BaselineContent, &e.FallbackSnippet); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// formatChangedHunks emits only added/removed line hunks between oldRaw and newRaw
// using unique matching anchors (patience LIS) in O(m+n) time and space, separating
// disjoint edited regions with "...".
func formatChangedHunks(oldRaw, newRaw string, maxBytes int) string {
	oldMid, newMid := trimChangedLineEdges(oldRaw, newRaw)
	m, n := len(oldMid), len(newMid)
	if m == 0 || n == 0 {
		return formatEdgeDiff(oldMid, newMid, "+ ", maxBytes)
	}

	cntO, cntN, posN := make(map[string]int, m), make(map[string]int, n), make(map[string]int, n)
	for _, l := range oldMid {
		cntO[l]++
	}
	for j, l := range newMid {
		cntN[l]++
		posN[l] = j
	}
	type anchor struct{ o, n int }
	var anchors []anchor
	for i, l := range oldMid {
		if cntO[l] == 1 && cntN[l] == 1 {
			anchors = append(anchors, anchor{i, posN[l]})
		}
	}
	if len(anchors) == 0 {
		return formatEdgeDiff(oldMid, newMid, "+ ", maxBytes)
	}

	// Longest strictly increasing anchor chain so a single moved line cannot collapse surrounding matches.
	tails := make([]int, 0, len(anchors))
	prev := make([]int, len(anchors))
	for i, a := range anchors {
		pos := sort.Search(len(tails), func(k int) bool { return anchors[tails[k]].n >= a.n })
		prev[i] = -1
		if pos > 0 {
			prev[i] = tails[pos-1]
		}
		if pos == len(tails) {
			tails = append(tails, i)
		} else {
			tails[pos] = i
		}
	}
	var kept []anchor
	for idx := tails[len(tails)-1]; idx >= 0; idx = prev[idx] {
		kept = append(kept, anchors[idx])
	}
	for l, r := 0, len(kept)-1; l < r; l, r = l+1, r-1 {
		kept[l], kept[r] = kept[r], kept[l]
	}

	type hunk struct{ oldLines, newLines []string }
	var hunks []hunk
	prevO, prevN := 0, 0
	for _, a := range kept {
		if a.o < prevO || a.n < prevN {
			continue
		}
		aO, aN := a.o, a.n
		for aO > prevO && aN > prevN && oldMid[aO-1] == newMid[aN-1] {
			aO--
			aN--
		}
		if prevO < aO || prevN < aN {
			hunks = append(hunks, hunk{oldMid[prevO:aO], newMid[prevN:aN]})
		}
		bO, bN := a.o+1, a.n+1
		for bO < m && bN < n && oldMid[bO] == newMid[bN] {
			bO++
			bN++
		}
		prevO, prevN = bO, bN
	}
	if prevO < m || prevN < n {
		hunks = append(hunks, hunk{oldMid[prevO:], newMid[prevN:]})
	}
	if len(hunks) == 0 {
		return ""
	}

	perCap := maxBytes
	if len(hunks) > 1 && maxBytes > 0 {
		perCap = max(200, (maxBytes-len(hunks)*8)/len(hunks))
	}
	var sb strings.Builder
	for i, h := range hunks {
		rem, add := formatDiffLines("- ", h.oldLines), formatDiffLines("+ ", h.newLines)
		if perCap > 0 && len(rem)+len(add)+1 > perCap {
			if rem != "" && add != "" {
				rem = truncateUTF8Bytes(rem, max(64, (perCap-8)/2))
				add = truncateUTF8Bytes(add, perCap-len(rem)-4)
			} else {
				rem, add = truncateUTF8Bytes(rem, perCap), truncateUTF8Bytes(add, perCap)
			}
		}
		if i > 0 {
			sb.WriteString("\n...\n")
		}
		sb.WriteString(rem)
		if rem != "" && add != "" {
			sb.WriteByte('\n')
		}
		sb.WriteString(add)
	}
	return truncateUTF8Bytes(sb.String(), maxBytes)
}

func displayEditPath(filePath, repoDir, fallbackCwd string) string {
	base := domain.FirstNonEmpty(repoDir, fallbackCwd)
	if base != "" && filepath.IsAbs(filePath) {
		if rel, err := filepath.Rel(base, filePath); err == nil && !strings.HasPrefix(rel, "..") {
			return filepath.ToSlash(rel)
		}
	}
	return filepath.ToSlash(filePath)
}

func diffPendingTurnEdit(ctx context.Context, e pendingTurnEdit, displayPath string) string {
	fallback := func() string {
		if e.FallbackSnippet == "" {
			return ""
		}
		return fmt.Sprintf("File: %s\n%s", displayPath, truncateUTF8Bytes(e.FallbackSnippet, maxPerFileDiffBytes))
	}
	if strings.HasPrefix(e.FilePath, "_unnamed:") {
		return fallback()
	}
	data, err := os.ReadFile(e.FilePath)
	if os.IsNotExist(err) {
		if e.BaselineExists == 1 && strings.TrimSpace(e.BaselineContent) != "" {
			return fmt.Sprintf("File: %s (deleted)\nRemoved:\n%s", displayPath, truncateUTF8Bytes(e.BaselineContent, maxPerFileDiffBytes))
		}
		return ""
	}
	if err != nil {
		return fallback()
	}

	if e.BaselineExists == -1 {
		if e.RepoDir != "" {
			if gDiff := strings.TrimSpace(git.DiffFile(ctx, e.RepoDir, e.FilePath)); gDiff != "" {
				return fmt.Sprintf("File: %s\n%s", displayPath, truncateUTF8Bytes(gDiff, maxPerFileDiffBytes))
			}
		}
		return fallback()
	}

	if !utf8.Valid(data) || bytes.IndexByte(data, 0) >= 0 {
		if f := fallback(); f != "" {
			return f
		}
		return fmt.Sprintf("File: %s (binary file modified)", displayPath)
	}

	cur := string(data)
	if e.BaselineExists == 1 && cur == e.BaselineContent {
		return ""
	}
	oldText := e.BaselineContent
	if e.BaselineExists == 0 {
		oldText = ""
	}
	diff := formatChangedHunks(oldText, cur, maxPerFileDiffBytes)
	if strings.TrimSpace(diff) == "" {
		return ""
	}
	return fmt.Sprintf("File: %s\n%s", displayPath, diff)
}

func formatBatchToolLabel(n int) string {
	if n == 1 {
		return "turn-batch (1 file)"
	}
	return fmt.Sprintf("turn-batch (%d files)", n)
}

type stopScopeBatch struct {
	scope string
	edits []pendingTurnEdit
}

func groupPendingEditsByScope(edits []pendingTurnEdit) []stopScopeBatch {
	scopeIdx := make(map[string]int)
	var batches []stopScopeBatch
	for _, e := range edits {
		idx, ok := scopeIdx[e.Scope]
		if !ok {
			idx = len(batches)
			scopeIdx[e.Scope] = idx
			batches = append(batches, stopScopeBatch{scope: e.Scope})
		}
		batches[idx].edits = append(batches[idx].edits, e)
	}
	return batches
}

func (s *Service) runStopHook(ctx context.Context, ev NormalizedHookEvent, codec AgentCodec, sid, fallbackCwd string) (string, int, error) {
	if ev.shouldSkipStopHook() {
		s.clearPendingTurnEdits(ctx, sid)
		return codec.FormatEmpty()
	}

	edits, err := s.loadPendingTurnEdits(ctx, sid)
	if err != nil || len(edits) == 0 {
		return codec.FormatEmpty()
	}
	s.clearPendingTurnEdits(ctx, sid)

	var allBlocking []domain.PlanViolation
	var failClosedScopes []string
	var touchedFiles []string
	seenTouched := make(map[string]bool)
	cwd := domain.FirstNonEmpty(ev.Cwd, fallbackCwd)

	for _, b := range groupPendingEditsByScope(edits) {
		blocking, failClosed := s.evaluateStopScopeBatch(ctx, ev, sid, cwd, b, seenTouched, &touchedFiles)
		allBlocking = append(allBlocking, blocking...)
		if failClosed {
			failClosedScopes = append(failClosedScopes, b.scope)
		}
	}

	if len(allBlocking) == 0 && len(failClosedScopes) == 0 {
		return codec.FormatEmpty()
	}
	return codec.FormatStopContinue(formatStopBlockingMessage(allBlocking, failClosedScopes, touchedFiles))
}

func (s *Service) evaluateStopScopeBatch(
	ctx context.Context,
	ev NormalizedHookEvent,
	sid, fallbackDir string,
	b stopScopeBatch,
	seenTouched map[string]bool,
	touchedFiles *[]string,
) ([]domain.PlanViolation, bool) {
	var sections, labels []string
	scopeTurnInput := ""
	for _, e := range b.edits {
		if scopeTurnInput == "" && e.TurnInput != "" {
			scopeTurnInput = e.TurnInput
		}
		label := displayEditPath(e.FilePath, e.RepoDir, fallbackDir)
		diff := diffPendingTurnEdit(ctx, e, label)
		if diff == "" {
			continue
		}
		sections = append(sections, diff)
		labels = append(labels, label)
		if !seenTouched[label] {
			seenTouched[label] = true
			*touchedFiles = append(*touchedFiles, label)
		}
	}
	if len(sections) == 0 {
		return nil, false
	}

	evalEvent := ev
	if evalEvent.TurnPrompt == "" && scopeTurnInput != "" {
		evalEvent.TurnPrompt = scopeTurnInput
	}
	// Ensure evaluateGuardrailViolations can load ActiveTurnInput when explicitSID was derived.
	if evalEvent.SessionID == "" {
		evalEvent.SessionID = sid
	}

	batchPlan := fmt.Sprintf("Turn edited files (%s):\n%s",
		strings.Join(labels, ", "),
		truncateUTF8Bytes(strings.Join(sections, "\n\n"), maxBatchDiffBytes))
	toolLabel := formatBatchToolLabel(len(sections))
	start := time.Now()
	candidateCount, blocking, verifyErr := s.evaluateGuardrailViolations(ctx, batchPlan, evalEvent, b.scope, 0.80)
	blocked := s.recordVerifyOutcomeLog(ctx, b.scope, ev.Mode, toolLabel, candidateCount, batchPlan, blocking, verifyErr, domain.ElapsedMsSince(start))
	if verifyErr != nil {
		return nil, blocked
	}
	if !blocked {
		return nil, false
	}
	return blocking, false
}

func formatStopBlockingMessage(allBlocking []domain.PlanViolation, failClosedScopes, touchedFiles []string) string {
	var sb strings.Builder
	if len(allBlocking) > 0 {
		sb.WriteString("[MEMORIZE GUARDRAIL — FIX BEFORE FINISHING] Your file edits in this turn violate active project rules:\n")
		for _, v := range allBlocking {
			fmt.Fprintf(&sb, "- [%s / %s %s] %s (violation prob: %.0f%%)\n",
				v.Scope, v.Category, formatViolationRef(v.Fact), v.Content, v.Probability*100)
		}
		if len(touchedFiles) > 0 {
			fmt.Fprintf(&sb, "Touched files: %s\n", strings.Join(touchedFiles, ", "))
		}
		sb.WriteString("Fix the violating edits directly in those files (or update an intentionally superseded rule via manage_memory/commit_memory) before ending your response.")
	} else {
		fmt.Fprintf(&sb, "[MEMORIZE GUARDRAIL — FIX BEFORE FINISHING] Stop batch verification failed under fail-closed policy (%s).", strings.Join(failClosedScopes, ", "))
	}
	return strings.TrimSpace(sb.String())
}
