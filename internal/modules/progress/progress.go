package progress

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"memorize/internal/domain"
)

func formatMemoryIDsStr(ids []int64) string {
	seen := make(map[int64]bool, len(ids))
	var clean []int64
	for _, id := range ids {
		if id > 0 && !seen[id] {
			seen[id] = true
			clean = append(clean, id)
		}
	}
	sort.Slice(clean, func(i, j int) bool { return clean[i] < clean[j] })
	parts := make([]string, len(clean))
	for i, id := range clean {
		parts[i] = strconv.FormatInt(id, 10)
	}
	return strings.Join(parts, " ")
}

// ParseMemoryIDsStr extracts positive, deduplicated memory IDs from whitespace/punctuation-delimited text.
func ParseMemoryIDsStr(raw string) []int64 {
	seen := make(map[int64]bool)
	var out []int64
	for _, tok := range strings.FieldsFunc(raw, func(r rune) bool {
		return r == ' ' || r == ',' || r == '#' || r == ';' || r == '\t'
	}) {
		if id, err := strconv.ParseInt(strings.TrimSpace(tok), 10, 64); err == nil && id > 0 && !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	return out
}

func splitDetailLines(details, summary string) []string {
	var lines []string
	for _, raw := range strings.Split(details, "\n") {
		if line := strings.TrimSpace(raw); line != "" {
			lines = append(lines, line)
		}
	}
	if len(lines) == 1 && lines[0] == strings.TrimSpace(summary) {
		return nil
	}
	return lines
}

// ParseProgressTimestamp parses SQLite CURRENT_TIMESTAMP (UTC) or ISO/RFC3339 strings into local time.
func ParseProgressTimestamp(raw string) (time.Time, bool) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return time.Time{}, false
	}
	for _, layout := range []string{
		time.RFC3339Nano,
		time.RFC3339,
		"2006-01-02 15:04:05Z07:00",
		"2006-01-02 15:04:05-07:00",
	} {
		if t, err := time.Parse(layout, s); err == nil {
			return t.In(time.Local), true
		}
	}
	for _, utcLayout := range []string{
		"2006-01-02 15:04:05",
		"2006-01-02 15:04",
	} {
		if t, err := time.ParseInLocation(utcLayout, s, time.UTC); err == nil {
			return t.In(time.Local), true
		}
	}
	return time.Time{}, false
}

func extractProgressDateTime(r *domain.ProgressRecord) {
	sum := strings.TrimSpace(r.Summary)
	if r.Source == "git_sync" && len(sum) >= 12 && sum[0] == '(' && sum[11] == ')' && sum[5] == '-' && sum[8] == '-' {
		r.Date = sum[1:11]
	}
	ts := strings.TrimSpace(r.UpdatedAt)
	if ts == "" {
		ts = strings.TrimSpace(r.CreatedAt)
	}
	if localT, ok := ParseProgressTimestamp(ts); ok {
		r.UpdatedAt = localT.Format(time.RFC3339)
		if r.Date == "" {
			r.Date = localT.Format("2006-01-02")
		}
		r.Time = localT.Format("15:04")
		return
	}
	if len(ts) >= 10 && r.Date == "" {
		r.Date = ts[:10]
	}
	if len(ts) >= 16 {
		r.Time = ts[11:16]
	}
	if r.Date == "" {
		r.Date = "recent"
	}
}

// RecordProgress validates and inserts a new entry into project_progress.
func (s *Service) RecordProgress(ctx context.Context, in domain.ProgressInput) (domain.ProgressRecord, error) {
	summary := strings.TrimSpace(in.Summary)
	if summary == "" {
		return domain.ProgressRecord{}, fmt.Errorf("progress summary cannot be empty")
	}
	nextStep := strings.TrimSpace(in.NextStep)
	details := strings.TrimSpace(in.Details)
	trackRaw := strings.TrimSpace(in.Track)
	if reason := domain.CheckSecurityGuard(trackRaw + "\n" + summary + "\n" + details + "\n" + nextStep); reason != "" {
		return domain.ProgressRecord{}, fmt.Errorf("security guard: %s", reason)
	}
	sc := domain.NormalizeScope(in.Scope)
	rawCat := strings.TrimSpace(in.Category)
	if rawCat == "" && trackRaw != "" {
		rawCat = "general"
	}
	cat, sub := domain.SplitCategoryAndSub(sc, rawCat, in.SubCategory)
	track := domain.EffectiveProgressTrack(trackRaw, cat, sub)
	status := domain.NormalizeProgressStatus(in.Status)
	src := strings.TrimSpace(in.Source)
	if src == "" {
		src = "agent"
	}
	sha := strings.TrimSpace(in.CommitSHA)
	if len(sha) > 12 {
		sha = sha[:12]
	}
	sid := strings.TrimSpace(in.SessionID)
	if sid == "" && src != "git_sync" && src != "memory_sync" {
		sid = s.mem.ResolveEffectiveSessionID(ctx, sc, "")
	}
	memStr := formatMemoryIDsStr(in.MemoryIDs)

	res, err := s.db.ExecContext(ctx, `
		INSERT INTO project_progress (scope, session_id, track, category, sub_category, status, summary, details, next_step, memory_ids, commit_sha, source, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)
	`, sc, sid, track, cat, sub, status, summary, details, nextStep, memStr, sha, src)
	if err != nil {
		return domain.ProgressRecord{}, err
	}
	s.notifyProgressChange()
	id, _ := res.LastInsertId()
	return s.GetProgressByID(ctx, id)
}

// GetProgressByID loads one project_progress entry by primary key.
func (s *Service) GetProgressByID(ctx context.Context, id int64) (domain.ProgressRecord, error) {
	var r domain.ProgressRecord
	var memStr string
	err := s.db.QueryRowContext(ctx, `
		SELECT id, scope, COALESCE(session_id, ''), COALESCE(track, ''), category, COALESCE(sub_category, ''), status,
		       summary, COALESCE(details, ''), COALESCE(next_step, ''),
		       COALESCE(memory_ids, ''), COALESCE(commit_sha, ''), COALESCE(source, 'manual'),
		       COALESCE(created_at, ''), COALESCE(updated_at, '')
		FROM project_progress WHERE id = ?
	`, id).Scan(&r.ID, &r.Scope, &r.SessionID, &r.Track, &r.Category, &r.SubCategory, &r.Status,
		&r.Summary, &r.Details, &r.NextStep, &memStr, &r.CommitSHA, &r.Source, &r.CreatedAt, &r.UpdatedAt)
	if err != nil {
		return domain.ProgressRecord{}, fmt.Errorf("progress #%d not found", id)
	}
	r.ProjectName = domain.ProjectShortName(r.Scope)
	r.Track = domain.EffectiveProgressTrack(r.Track, r.Category, r.SubCategory)
	r.MemoryIDs = ParseMemoryIDsStr(memStr)
	r.Kind = domain.ProgressWorkKind(r.Source, r.CommitSHA, r.MemoryIDs)
	r.DetailsLines = splitDetailLines(r.Details, r.Summary)
	extractProgressDateTime(&r)
	return r, nil
}

// UpdateProgress updates mutable fields of an existing project_progress entry.
func (s *Service) UpdateProgress(ctx context.Context, id int64, in domain.ProgressInput) error {
	cur, err := s.GetProgressByID(ctx, id)
	if err != nil {
		return err
	}
	sc := cur.Scope
	if strings.TrimSpace(in.Scope) != "" {
		sc = domain.NormalizeScope(in.Scope)
	}
	sid := cur.SessionID
	if strings.TrimSpace(in.SessionID) != "" {
		sid = strings.TrimSpace(in.SessionID)
	} else if sid == "" && cur.Source != "git_sync" && cur.Source != "memory_sync" {
		if csid := s.mem.ResolveEffectiveSessionID(ctx, sc, ""); csid != "" {
			sid = csid
		}
	}
	cat, sub := cur.Category, cur.SubCategory
	if strings.TrimSpace(in.Category) != "" || strings.TrimSpace(in.SubCategory) != "" {
		rawCat := in.Category
		if strings.TrimSpace(rawCat) == "" {
			rawCat = cur.Category
		}
		cat, sub = domain.SplitCategoryAndSub(sc, rawCat, in.SubCategory)
	}
	track := cur.Track
	if strings.TrimSpace(in.Track) != "" {
		track = strings.TrimSpace(in.Track)
	} else if strings.TrimSpace(in.Category) != "" || strings.TrimSpace(in.SubCategory) != "" {
		if cur.Track == domain.EffectiveProgressTrack("", cur.Category, cur.SubCategory) {
			track = domain.EffectiveProgressTrack("", cat, sub)
		}
	}
	status := cur.Status
	if strings.TrimSpace(in.Status) != "" {
		status = domain.NormalizeProgressStatus(in.Status)
	}
	summary := cur.Summary
	if strings.TrimSpace(in.Summary) != "" {
		summary = strings.TrimSpace(in.Summary)
	}
	details := cur.Details
	if in.Details != "" {
		details = strings.TrimSpace(in.Details)
	}
	nextStep := strings.TrimSpace(in.NextStep)
	if reason := domain.CheckSecurityGuard(track + "\n" + summary + "\n" + details + "\n" + nextStep); reason != "" {
		return fmt.Errorf("security guard: %s", reason)
	}
	memStr := formatMemoryIDsStr(cur.MemoryIDs)
	if in.MemoryIDs != nil {
		memStr = formatMemoryIDsStr(in.MemoryIDs)
	}
	sha := cur.CommitSHA
	if strings.TrimSpace(in.CommitSHA) != "" {
		sha = strings.TrimSpace(in.CommitSHA)
		if len(sha) > 12 {
			sha = sha[:12]
		}
	}
	_, err = s.db.ExecContext(ctx, `
		UPDATE project_progress
		SET scope = ?, session_id = ?, track = ?, category = ?, sub_category = ?, status = ?, summary = ?, details = ?, next_step = ?,
		    memory_ids = ?, commit_sha = ?, updated_at = CURRENT_TIMESTAMP
		WHERE id = ?
	`, sc, sid, track, cat, sub, status, summary, details, nextStep, memStr, sha, id)
	if err == nil {
		s.notifyProgressChange()
	}
	return err
}

// UpdateProgressStatus transitions the lifecycle status of a progress entry.
func (s *Service) UpdateProgressStatus(ctx context.Context, id int64, rawStatus string) error {
	cur, err := s.GetProgressByID(ctx, id)
	if err != nil {
		return err
	}
	st := domain.NormalizeProgressStatus(rawStatus)
	sid := s.mem.ResolveEffectiveSessionID(ctx, cur.Scope, "")
	_, err = s.db.ExecContext(ctx, `
		UPDATE project_progress
		SET status = ?,
		    session_id = CASE WHEN COALESCE(session_id, '') = '' AND COALESCE(source, '') NOT IN ('git_sync', 'memory_sync') AND ? != '' THEN ? ELSE session_id END,
		    updated_at = CURRENT_TIMESTAMP
		WHERE id = ?
	`, st, sid, sid, id)
	if err == nil {
		s.notifyProgressChange()
	}
	return err
}

// DeleteProgress removes one project_progress entry by ID.
func (s *Service) DeleteProgress(ctx context.Context, id int64) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM project_progress WHERE id = ?`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("progress #%d not found", id)
	}
	s.notifyProgressChange()
	return nil
}

func progressStatusPriority(status string) int {
	switch status {
	case domain.ProgressStatusInProgress:
		return 1
	case domain.ProgressStatusBlocked:
		return 2
	case domain.ProgressStatusNeedsSync:
		return 3
	case domain.ProgressStatusPlanned:
		return 4
	default:
		return 5
	}
}

// GetProgressOverview returns the progress board/timeline for rawScope filtered by category, status, and repo.
func (s *Service) GetProgressOverview(ctx context.Context, rawScope, categoryFilter, statusFilter, explicitRepo string) (domain.ProgressOverview, error) {
	return s.GetProgressOverviewByTrack(ctx, rawScope, "", categoryFilter, statusFilter, explicitRepo)
}

// GetProgressOverviewByTrack returns a track-filtered progress overview (up to 200 items).
func (s *Service) GetProgressOverviewByTrack(ctx context.Context, rawScope, trackFilter, categoryFilter, statusFilter, explicitRepo string) (domain.ProgressOverview, error) {
	return s.GetProgressOverviewPaged(ctx, rawScope, trackFilter, categoryFilter, statusFilter, explicitRepo, 1, 200)
}

// GetProgressOverviewPaged returns a paginated progress overview for rawScope (defaulting to TopProjectScope when empty).
func (s *Service) GetProgressOverviewPaged(ctx context.Context, rawScope, trackFilter, categoryFilter, statusFilter, explicitRepo string, page, pageSize int) (domain.ProgressOverview, error) {
	sc := strings.TrimSpace(rawScope)
	if sc == "" {
		sc = s.TopProjectScope(ctx)
	}
	return s.GetProgressOverviewWithQuery(ctx, domain.ProgressQuery{
		Scope:        sc,
		Track:        trackFilter,
		Category:     categoryFilter,
		Status:       statusFilter,
		ExplicitRepo: explicitRepo,
		Page:         page,
		PageSize:     pageSize,
	})
}
