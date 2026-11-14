package memory

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"time"
)

const (
	maxCompactSummaryRunes = 1500
	maxTranscriptScanBytes = 512 * 1024
)

type transcriptCompactLine struct {
	Type             string `json:"type"`
	Subtype          string `json:"subtype"`
	IsCompactSummary bool   `json:"isCompactSummary"`
	StepIndex        int64  `json:"step_index"`
	CreatedAt        string `json:"created_at"`
	Content          any    `json:"content"`
	Message          struct {
		Content any `json:"content"`
	} `json:"message"`
}

func cleanCompactionSummary(raw string) string {
	s := strings.TrimSpace(raw)
	if s == "" {
		return ""
	}
	if strings.HasPrefix(s, "# Resuming from a compaction") {
		if idx := strings.Index(s, "\n# "); idx > 0 {
			s = strings.TrimSpace(s[idx+1:])
		} else if idx := strings.Index(s, "\n\n"); idx > 0 {
			s = strings.TrimSpace(s[idx+2:])
		}
	}
	runes := []rune(s)
	if len(runes) > maxCompactSummaryRunes {
		s = strings.TrimSpace(string(runes[:maxCompactSummaryRunes]))
	}
	return s
}

func extractCompactLineText(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case []any:
		var parts []string
		for _, item := range x {
			if m, ok := item.(map[string]any); ok {
				if txt, ok := m["text"].(string); ok && strings.TrimSpace(txt) != "" {
					parts = append(parts, txt)
				}
			}
		}
		return strings.Join(parts, "\n")
	default:
		return ""
	}
}

func scanTranscriptCompactionWindow(transcriptPath string, prevOffset int64) (alignedEnd int64, marker string, summary string, createdAt string, ok bool) {
	p := strings.TrimSpace(transcriptPath)
	if p == "" {
		return prevOffset, "", "", "", false
	}
	info, err := os.Stat(p)
	if err != nil || info.IsDir() {
		return prevOffset, "", "", "", false
	}
	fileSize := info.Size()
	if fileSize == 0 {
		return 0, "", "", "", true
	}
	if prevOffset < 0 || fileSize < prevOffset {
		prevOffset = 0
	}
	if fileSize == prevOffset {
		return prevOffset, "", "", "", true
	}

	startOff := prevOffset
	snappedMidLine := false
	if fileSize-startOff > maxTranscriptScanBytes {
		startOff = fileSize - maxTranscriptScanBytes
		snappedMidLine = true
	}

	f, err := os.Open(p)
	if err != nil {
		return prevOffset, "", "", "", false
	}
	defer f.Close()

	if _, err := f.Seek(startOff, io.SeekStart); err != nil {
		return prevOffset, "", "", "", false
	}
	chunk, err := io.ReadAll(io.LimitReader(f, maxTranscriptScanBytes+1024))
	if err != nil {
		return prevOffset, "", "", "", false
	}
	lastNL := bytes.LastIndexByte(chunk, '\n')
	if lastNL < 0 {
		return prevOffset, "", "", "", true
	}
	alignedEnd = startOff + int64(lastNL+1)
	linesChunk := chunk[:lastNL]
	offsetCursor := startOff
	if snappedMidLine {
		if firstNL := bytes.IndexByte(linesChunk, '\n'); firstNL >= 0 {
			linesChunk = linesChunk[firstNL+1:]
			offsetCursor += int64(firstNL + 1)
		} else {
			return alignedEnd, "", "", "", true
		}
	}

	for _, rawLine := range bytes.Split(linesChunk, []byte{'\n'}) {
		lineLen := int64(len(rawLine)) + 1
		trimmed := bytes.TrimSpace(rawLine)
		if len(trimmed) > 0 && trimmed[0] == '{' {
			var entry transcriptCompactLine
			if json.Unmarshal(trimmed, &entry) == nil {
				switch {
				case strings.EqualFold(entry.Type, "CHECKPOINT"):
					marker = fmt.Sprintf("checkpoint:%d:%s", entry.StepIndex, strings.TrimSpace(entry.CreatedAt))
					createdAt = strings.TrimSpace(entry.CreatedAt)
					if txt := cleanCompactionSummary(extractCompactLineText(entry.Content)); txt != "" {
						summary = txt
					}
				case entry.IsCompactSummary:
					if marker == "" {
						marker = fmt.Sprintf("claude-compact-summary:%s:%d", strings.TrimSpace(entry.CreatedAt), offsetCursor)
					}
					if ts := strings.TrimSpace(entry.CreatedAt); ts != "" {
						createdAt = ts
					}
					txt := cleanCompactionSummary(extractCompactLineText(entry.Content))
					if txt == "" {
						txt = cleanCompactionSummary(extractCompactLineText(entry.Message.Content))
					}
					if txt != "" {
						summary = txt
					}
				case strings.EqualFold(entry.Type, "system") && strings.EqualFold(entry.Subtype, "compact_boundary"):
					marker = fmt.Sprintf("compact-boundary:%s:%d", strings.TrimSpace(entry.CreatedAt), offsetCursor)
					createdAt = strings.TrimSpace(entry.CreatedAt)
				}
			}
		}
		offsetCursor += lineLen
	}
	return alignedEnd, marker, summary, createdAt, true
}

func (s *Service) clearSessionDelivered(ctx context.Context, sessionID string) {
	if s == nil || sessionID == "" {
		return
	}
	s.mu.Lock()
	s.sent[sessionID] = make(map[int64]bool)
	s.mu.Unlock()
	if s.db != nil {
		_, _ = s.db.ExecContext(ctx, `DELETE FROM session_sent WHERE session_id = ?`, sessionID)
		_, _ = s.db.ExecContext(ctx, `DELETE FROM session_updates WHERE session_id = ?`, sessionID)
	}
}

func (s *Service) upsertSessionCompactState(ctx context.Context, sessionID string, offset int64, marker, pendingSummary string, pending bool) {
	if s == nil || s.db == nil || sessionID == "" {
		return
	}
	pendInt := 0
	if pending {
		pendInt = 1
	}
	_, _ = s.db.ExecContext(ctx, `
		INSERT INTO session_compacts (session_id, last_offset, last_marker, pending_summary, pending, updated_at)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(session_id) DO UPDATE SET
			last_offset = excluded.last_offset,
			last_marker = CASE WHEN excluded.last_marker <> '' THEN excluded.last_marker ELSE session_compacts.last_marker END,
			pending_summary = excluded.pending_summary,
			pending = excluded.pending,
			updated_at = excluded.updated_at`,
		sessionID, offset, marker, pendingSummary, pendInt, time.Now().Unix())
}

// RecordPostCompact records a compaction summary from an explicit compact hook (e.g. Claude Code PostCompact)
// and clears session_sent immediately so the next SessionStart or turn restores active rules cleanly.
func (s *Service) RecordPostCompact(ctx context.Context, sessionID, rawSummary, transcriptPath string) {
	if s == nil || sessionID == "" {
		return
	}
	s.clearSessionDelivered(ctx, sessionID)
	summary := cleanCompactionSummary(rawSummary)
	alignedEnd, marker, logSummary, _, ok := scanTranscriptCompactionWindow(transcriptPath, 0)
	if summary == "" && logSummary != "" {
		summary = logSummary
	}
	if !ok {
		alignedEnd = 0
	}
	s.upsertSessionCompactState(ctx, sessionID, alignedEnd, marker, summary, true)
}

func (s *Service) shouldRefreshInitialCheckpoint(ctx context.Context, sessionID, createdAt string) bool {
	if s == nil || s.db == nil || sessionID == "" {
		return false
	}
	var maxSentStr sql.NullString
	err := s.db.QueryRowContext(ctx, `SELECT MAX(updated_at) FROM session_sent WHERE session_id = ?`, sessionID).Scan(&maxSentStr)
	if err != nil || !maxSentStr.Valid || strings.TrimSpace(maxSentStr.String) == "" {
		return false
	}
	if strings.TrimSpace(createdAt) == "" {
		return true
	}
	ckptTime, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(createdAt))
	if err != nil {
		return true
	}
	sentTime, err := time.Parse("2006-01-02 15:04:05", strings.TrimSpace(maxSentStr.String))
	if err != nil {
		return true
	}
	return !ckptTime.Before(sentTime.Add(-2 * time.Second))
}

// InspectCompactionForHook detects or consumes session compaction state for both start and turn hooks.
// On compaction, it returns compacted=true along with an optional compacted summary snippet for relevance scoring.
func (s *Service) InspectCompactionForHook(ctx context.Context, sessionID, transcriptPath, source string, isStart bool) (compacted bool, summary string) {
	if s == nil || s.db == nil || sessionID == "" {
		return false, ""
	}
	srcCompact := strings.EqualFold(strings.TrimSpace(source), "compact")

	var prevOffset int64
	var prevMarker, pendingSummary string
	var pendingFlag int
	err := s.db.QueryRowContext(ctx, `
		SELECT last_offset, last_marker, pending_summary, pending
		FROM session_compacts WHERE session_id = ?`, sessionID).
		Scan(&prevOffset, &prevMarker, &pendingSummary, &pendingFlag)
	hasRow := err == nil

	if pendingFlag != 0 {
		summary = strings.TrimSpace(pendingSummary)
		compacted = true
	}
	if srcCompact {
		compacted = true
	}

	alignedEnd, marker, logSummary, createdAt, ok := scanTranscriptCompactionWindow(transcriptPath, prevOffset)
	if !ok {
		alignedEnd = prevOffset
	}

	if isStart {
		if compacted && summary == "" && logSummary != "" {
			summary = logSummary
		}
		nextMarker := prevMarker
		if marker != "" {
			nextMarker = marker
		}
		s.upsertSessionCompactState(ctx, sessionID, alignedEnd, nextMarker, "", false)
		return compacted, summary
	}

	newMarker := marker != "" && marker != prevMarker
	if newMarker && (hasRow || s.shouldRefreshInitialCheckpoint(ctx, sessionID, createdAt)) {
		compacted = true
	}
	if compacted && summary == "" && logSummary != "" {
		summary = logSummary
	}
	nextMarker := prevMarker
	if marker != "" {
		nextMarker = marker
	}
	s.upsertSessionCompactState(ctx, sessionID, alignedEnd, nextMarker, "", false)
	return compacted, summary
}
