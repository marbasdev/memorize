package memory

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	ProgressStatusPlanned    = "planned"
	ProgressStatusInProgress = "in_progress"
	ProgressStatusNeedsSync  = "needs_sync"
	ProgressStatusBlocked    = "blocked"
	ProgressStatusDone       = "done"

	ProgressKindWork   = "work"
	ProgressKindMemory = "memory"
	ProgressKindGit    = "git"
)

func ProgressWorkKind(source, commitSHA string, memoryIDs []int64) string {
	switch strings.ToLower(strings.TrimSpace(source)) {
	case "git_sync", "git":
		return ProgressKindGit
	case "memory", "memory_sync":
		return ProgressKindMemory
	default:
		if strings.TrimSpace(commitSHA) != "" && len(memoryIDs) == 0 && source != "agent" {
			return ProgressKindGit
		}
		return ProgressKindWork
	}
}

func ProjectShortName(scope string) string {
	sc := strings.TrimSpace(scope)
	if strings.HasPrefix(sc, "project:") {
		return strings.TrimPrefix(sc, "project:")
	}
	if sc == "" {
		return "global"
	}
	return sc
}

func NormalizeProgressStatus(raw string) string {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "planned", "todo", "pending", "예정":
		return ProgressStatusPlanned
	case "in_progress", "in-progress", "working", "doing", "active", "진행중", "진행":
		return ProgressStatusInProgress
	case "blocked", "hold", "보류", "막힘":
		return ProgressStatusBlocked
	case "done", "completed", "complete", "완료", "needs_sync", "needs-sync", "sync", "review", "확인필요":
		return ProgressStatusDone
	default:
		return ProgressStatusInProgress
	}
}

// EffectiveProgressTrack resolves a human-readable work stream / topic name.
// Explicit track titles take priority; otherwise category + sub_category form a distinct work stream.
func EffectiveProgressTrack(track, category, subCategory string) string {
	if t := strings.TrimSpace(track); t != "" {
		return t
	}
	cat := strings.TrimSpace(category)
	sub := strings.TrimSpace(subCategory)
	if cat != "" && sub != "" && !strings.EqualFold(cat, sub) {
		return cat + " / " + sub
	}
	if sub != "" {
		return sub
	}
	if cat != "" {
		return cat
	}
	return "general"
}

type ProgressRecord struct {
	ID           int64    `json:"id"`
	Scope        string   `json:"scope"`
	ProjectName  string   `json:"project_name"`
	SessionID    string   `json:"session_id,omitempty"`
	Kind         string   `json:"kind"`
	Track        string   `json:"track"`
	Category     string   `json:"category"`
	SubCategory  string   `json:"sub_category,omitempty"`
	Status       string   `json:"status"`
	Summary      string   `json:"summary"`
	Details      string   `json:"details,omitempty"`
	DetailsLines []string `json:"details_lines,omitempty"`
	NextStep     string   `json:"next_step,omitempty"`
	MemoryIDs    []int64  `json:"memory_ids,omitempty"`
	CommitSHA    string   `json:"commit_sha,omitempty"`
	Source       string   `json:"source"`
	Date         string   `json:"date,omitempty"`
	Time         string   `json:"time,omitempty"`
	CreatedAt    string   `json:"created_at"`
	UpdatedAt    string   `json:"updated_at"`
}

type ProgressInput struct {
	Scope       string
	SessionID   string
	Track       string
	Category    string
	SubCategory string
	Status      string
	Summary     string
	Details     string
	NextStep    string
	MemoryIDs   []int64
	CommitSHA   string
	Source      string
}

type ProgressTrackSummary struct {
	Scope             string  `json:"scope"`
	ProjectName       string  `json:"project_name"`
	SessionID         string  `json:"session_id,omitempty"`
	Kind              string  `json:"kind"`
	Track             string  `json:"track"`
	Category          string  `json:"category,omitempty"`
	SubCategory       string  `json:"sub_category,omitempty"`
	Status            string  `json:"status"`
	LatestID          int64   `json:"latest_id"`
	LatestSummary     string  `json:"latest_summary"`
	LatestNextStep    string  `json:"latest_next_step,omitempty"`
	LatestUpdatedAt   string  `json:"latest_updated_at"`
	TotalCount        int     `json:"total_count"`
	DoneCount         int     `json:"done_count"`
	InProgressCount   int     `json:"in_progress_count"`
	NeedsSyncCount    int     `json:"needs_sync_count"`
	PlannedCount      int     `json:"planned_count"`
	BlockedCount      int     `json:"blocked_count"`
	ActiveCount       int     `json:"active_count"`
	DonePct           float64 `json:"done_pct"`
	LinkedMemoryCount int     `json:"linked_memory_count"`
}

type ProgressDateGroup struct {
	Date    string           `json:"date"`
	Records []ProgressRecord `json:"records"`
}

type ProgressCategorySummary struct {
	Category          string   `json:"category"`
	SubCategories     []string `json:"sub_categories,omitempty"`
	TotalCount        int      `json:"total_count"`
	DoneCount         int      `json:"done_count"`
	InProgressCount   int      `json:"in_progress_count"`
	NeedsSyncCount    int      `json:"needs_sync_count"`
	PlannedCount      int      `json:"planned_count"`
	BlockedCount      int      `json:"blocked_count"`
	DonePct           float64  `json:"done_pct"`
	LatestStatus      string   `json:"latest_status"`
	LatestSummary     string   `json:"latest_summary"`
	LatestNextStep    string   `json:"latest_next_step,omitempty"`
	LatestUpdatedAt   string   `json:"latest_updated_at"`
	LinkedMemoryCount int      `json:"linked_memory_count"`
}

type ProgressSyncState struct {
	Scope               string           `json:"scope"`
	RepoPath            string           `json:"repo_path,omitempty"`
	SyncedCommit        string           `json:"synced_commit,omitempty"`
	HeadCommit          string           `json:"head_commit,omitempty"`
	PendingCommits      []GitCommitBrief `json:"pending_commits,omitempty"`
	UnlinkedMemoryCount int              `json:"unlinked_memory_count"`
	InSync              bool             `json:"in_sync"`
}

type ProgressSyncResult struct {
	Scope          string `json:"scope"`
	HeadCommit     string `json:"head_commit,omitempty"`
	SyncedMemories int    `json:"synced_memories"`
	SyncedCommits  int    `json:"synced_commits"`
	ResolvedSyncs  int    `json:"resolved_syncs"`
}

type ProgressNextStepItem struct {
	ID            int64  `json:"id"`
	Scope         string `json:"scope"`
	ProjectName   string `json:"project_name"`
	SessionID     string `json:"session_id,omitempty"`
	Kind          string `json:"kind"`
	Track         string `json:"track"`
	Status        string `json:"status"`
	NextStep      string `json:"next_step"`
	ParentSummary string `json:"parent_summary"`
	UpdatedAt     string `json:"updated_at"`
}

type ProgressQuery struct {
	Scope        string
	Kind         string
	SessionID    string
	Track        string
	Category     string
	Status       string
	DoneWindow   string
	ExplicitRepo string
	Page         int
	PageSize     int
}

// NormalizeDoneWindow normalizes the time window for the Recently Completed lane.
func NormalizeDoneWindow(raw string) string {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "24h", "1d", "today":
		return "24h"
	case "30d", "30":
		return "30d"
	case "all", "0":
		return "all"
	default:
		return "7d"
	}
}

type ProgressOverview struct {
	Scope           string                    `json:"scope"`
	TotalCount      int                       `json:"total_count"`
	FilteredCount   int                       `json:"filtered_count"`
	Page            int                       `json:"page"`
	PageSize        int                       `json:"page_size"`
	TotalPages      int                       `json:"total_pages"`
	StatusCounts    map[string]int            `json:"status_counts"`
	KindCounts      map[string]int            `json:"kind_counts,omitempty"`
	Sessions        []SessionSummary          `json:"sessions,omitempty"`
	ActiveTracks    []ProgressTrackSummary    `json:"active_tracks"`
	DoingTracks     []ProgressTrackSummary    `json:"doing_tracks"`
	PlannedTracks   []ProgressTrackSummary    `json:"planned_tracks"`
	NextStepItems   []ProgressNextStepItem    `json:"next_step_items"`
	RecentDone      []ProgressRecord          `json:"recent_done"`
	DoneWindow      string                    `json:"done_window,omitempty"`
	DoneWindowTotal int                       `json:"done_window_total,omitempty"`
	DoneWindowStart string                    `json:"done_window_start,omitempty"`
	DoneWindowEnd   string                    `json:"done_window_end,omitempty"`
	AllTracks       []ProgressTrackSummary    `json:"all_tracks"`
	Categories      []ProgressCategorySummary `json:"categories"`
	Timeline        []ProgressDateGroup       `json:"timeline"`
	Records         []ProgressRecord          `json:"records"`
	Sync            ProgressSyncState         `json:"sync"`
}

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

func parseMemoryIDsStr(raw string) []int64 {
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

func extractProgressDateTime(r *ProgressRecord) {
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

func (s *Service) RecordProgress(ctx context.Context, in ProgressInput) (ProgressRecord, error) {
	summary := strings.TrimSpace(in.Summary)
	if summary == "" {
		return ProgressRecord{}, fmt.Errorf("progress summary cannot be empty")
	}
	nextStep := strings.TrimSpace(in.NextStep)
	details := strings.TrimSpace(in.Details)
	trackRaw := strings.TrimSpace(in.Track)
	if reason := checkSecurityGuard(trackRaw + "\n" + summary + "\n" + details + "\n" + nextStep); reason != "" {
		return ProgressRecord{}, fmt.Errorf("security guard: %s", reason)
	}
	sc := NormalizeScope(in.Scope)
	rawCat := strings.TrimSpace(in.Category)
	if rawCat == "" && trackRaw != "" {
		rawCat = "general"
	}
	cat, sub := SplitCategoryAndSub(sc, rawCat, in.SubCategory)
	track := EffectiveProgressTrack(trackRaw, cat, sub)
	status := NormalizeProgressStatus(in.Status)
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
		sid = s.ResolveEffectiveSessionID(ctx, sc, "")
	}
	memStr := formatMemoryIDsStr(in.MemoryIDs)

	res, err := s.db.ExecContext(ctx, `
		INSERT INTO project_progress (scope, session_id, track, category, sub_category, status, summary, details, next_step, memory_ids, commit_sha, source, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)
	`, sc, sid, track, cat, sub, status, summary, details, nextStep, memStr, sha, src)
	if err != nil {
		return ProgressRecord{}, err
	}
	s.NotifyDomainChange("progress")
	id, _ := res.LastInsertId()
	return s.GetProgressByID(ctx, id)
}

func (s *Service) GetProgressByID(ctx context.Context, id int64) (ProgressRecord, error) {
	var r ProgressRecord
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
		return ProgressRecord{}, fmt.Errorf("progress #%d not found", id)
	}
	r.ProjectName = ProjectShortName(r.Scope)
	r.Track = EffectiveProgressTrack(r.Track, r.Category, r.SubCategory)
	r.MemoryIDs = parseMemoryIDsStr(memStr)
	r.Kind = ProgressWorkKind(r.Source, r.CommitSHA, r.MemoryIDs)
	r.DetailsLines = splitDetailLines(r.Details, r.Summary)
	extractProgressDateTime(&r)
	return r, nil
}

func (s *Service) UpdateProgress(ctx context.Context, id int64, in ProgressInput) error {
	cur, err := s.GetProgressByID(ctx, id)
	if err != nil {
		return err
	}
	sc := cur.Scope
	if strings.TrimSpace(in.Scope) != "" {
		sc = NormalizeScope(in.Scope)
	}
	sid := cur.SessionID
	if strings.TrimSpace(in.SessionID) != "" {
		sid = strings.TrimSpace(in.SessionID)
	} else if sid == "" && cur.Source != "git_sync" && cur.Source != "memory_sync" {
		if csid := s.ResolveEffectiveSessionID(ctx, sc, ""); csid != "" {
			sid = csid
		}
	}
	cat, sub := cur.Category, cur.SubCategory
	if strings.TrimSpace(in.Category) != "" || strings.TrimSpace(in.SubCategory) != "" {
		rawCat := in.Category
		if strings.TrimSpace(rawCat) == "" {
			rawCat = cur.Category
		}
		cat, sub = SplitCategoryAndSub(sc, rawCat, in.SubCategory)
	}
	track := cur.Track
	if strings.TrimSpace(in.Track) != "" {
		track = strings.TrimSpace(in.Track)
	} else if strings.TrimSpace(in.Category) != "" || strings.TrimSpace(in.SubCategory) != "" {
		if cur.Track == EffectiveProgressTrack("", cur.Category, cur.SubCategory) {
			track = EffectiveProgressTrack("", cat, sub)
		}
	}
	status := cur.Status
	if strings.TrimSpace(in.Status) != "" {
		status = NormalizeProgressStatus(in.Status)
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
	if reason := checkSecurityGuard(track + "\n" + summary + "\n" + details + "\n" + nextStep); reason != "" {
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
		s.NotifyDomainChange("progress")
	}
	return err
}

func (s *Service) UpdateProgressStatus(ctx context.Context, id int64, rawStatus string) error {
	cur, err := s.GetProgressByID(ctx, id)
	if err != nil {
		return err
	}
	st := NormalizeProgressStatus(rawStatus)
	sid := s.ResolveEffectiveSessionID(ctx, cur.Scope, "")
	_, err = s.db.ExecContext(ctx, `
		UPDATE project_progress
		SET status = ?,
		    session_id = CASE WHEN COALESCE(session_id, '') = '' AND COALESCE(source, '') NOT IN ('git_sync', 'memory_sync') AND ? != '' THEN ? ELSE session_id END,
		    updated_at = CURRENT_TIMESTAMP
		WHERE id = ?
	`, st, sid, sid, id)
	if err == nil {
		s.NotifyDomainChange("progress")
	}
	return err
}

func (s *Service) DeleteProgress(ctx context.Context, id int64) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM project_progress WHERE id = ?`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("progress #%d not found", id)
	}
	s.NotifyDomainChange("progress")
	return nil
}

func progressStatusPriority(status string) int {
	switch status {
	case ProgressStatusInProgress:
		return 1
	case ProgressStatusBlocked:
		return 2
	case ProgressStatusNeedsSync:
		return 3
	case ProgressStatusPlanned:
		return 4
	default:
		return 5
	}
}

func (s *Service) GetProgressOverview(ctx context.Context, rawScope, categoryFilter, statusFilter, explicitRepo string) (ProgressOverview, error) {
	return s.GetProgressOverviewByTrack(ctx, rawScope, "", categoryFilter, statusFilter, explicitRepo)
}

func (s *Service) GetProgressOverviewByTrack(ctx context.Context, rawScope, trackFilter, categoryFilter, statusFilter, explicitRepo string) (ProgressOverview, error) {
	return s.GetProgressOverviewPaged(ctx, rawScope, trackFilter, categoryFilter, statusFilter, explicitRepo, 1, 200)
}

func (s *Service) GetProgressOverviewPaged(ctx context.Context, rawScope, trackFilter, categoryFilter, statusFilter, explicitRepo string, page, pageSize int) (ProgressOverview, error) {
	sc := strings.TrimSpace(rawScope)
	if sc == "" {
		sc = s.TopProjectScope(ctx)
	}
	return s.GetProgressOverviewWithQuery(ctx, ProgressQuery{
		Scope:        sc,
		Track:        trackFilter,
		Category:     categoryFilter,
		Status:       statusFilter,
		ExplicitRepo: explicitRepo,
		Page:         page,
		PageSize:     pageSize,
	})
}
