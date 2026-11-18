package domain

import (
	"strings"
)

const (
	ProgressKindWork   = "work"
	ProgressKindMemory = "memory"
	ProgressKindGit    = "git"
)

// ProgressWorkKind classifies a progress entry into work, memory, or git.
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

// ProjectShortName strips the "project:" prefix for compact display.
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

// EffectiveProgressTrack resolves a human-readable work stream / topic name.
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

// GitCommitBrief summarizes one Git commit passed to progress categorization.
type GitCommitBrief struct {
	SHA     string   `json:"sha"`
	Date    string   `json:"date"`
	Subject string   `json:"subject"`
	Files   []string `json:"files,omitempty"`
}

// CommitProgressItem holds a categorized summary for a Git commit.
type CommitProgressItem struct {
	SHA         string `json:"sha"`
	Track       string `json:"track,omitempty"`
	Category    string `json:"category"`
	SubCategory string `json:"sub_category,omitempty"`
	Summary     string `json:"summary"`
}

// GitMilestoneSummary holds a cohesive synthesized milestone spanning one or more Git commits.
type GitMilestoneSummary struct {
	Track       string   `json:"track"`
	Category    string   `json:"category"`
	SubCategory string   `json:"sub_category,omitempty"`
	Date        string   `json:"date,omitempty"`
	Summary     string   `json:"summary"`
	CommitSHAs  []string `json:"commit_shas"`
}

// ProgressRecord represents one persisted entry in project_progress.
type ProgressRecord struct {
	ID           int64    `json:"id"`
	Scope        string   `json:"scope"`
	Account      string   `json:"account,omitempty"`
	ProjectName  string   `json:"project_name"`
	SessionID    string   `json:"session_id,omitempty"`
	Kind         string   `json:"kind"`
	Track        string   `json:"track"`
	Category     string   `json:"category"`
	SubCategory  string   `json:"sub_category,omitempty"`
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

// ProgressInput captures fields to insert or update in project_progress.
type ProgressInput struct {
	Scope       string
	Account     string
	SessionID   string
	Track       string
	Category    string
	SubCategory string
	Summary     string
	Details     string
	NextStep    string
	MemoryIDs   []int64
	CommitSHA   string
	Source      string
}

// ProgressTrackSummary aggregates activity counts for one work track.
type ProgressTrackSummary struct {
	Scope             string `json:"scope"`
	ProjectName       string `json:"project_name"`
	SessionID         string `json:"session_id,omitempty"`
	Kind              string `json:"kind"`
	Track             string `json:"track"`
	Category          string `json:"category,omitempty"`
	SubCategory       string `json:"sub_category,omitempty"`
	LatestID          int64  `json:"latest_id"`
	LatestSummary     string `json:"latest_summary"`
	LatestNextStep    string `json:"latest_next_step,omitempty"`
	LatestUpdatedAt   string `json:"latest_updated_at"`
	TotalCount        int    `json:"total_count"`
	LinkedMemoryCount int    `json:"linked_memory_count"`
}

// ProgressDateGroup groups progress records under one YYYY-MM-DD header.
type ProgressDateGroup struct {
	Date    string           `json:"date"`
	Records []ProgressRecord `json:"records"`
}

// ProgressCategorySummary summarizes progress by category.
type ProgressCategorySummary struct {
	Category          string   `json:"category"`
	SubCategories     []string `json:"sub_categories,omitempty"`
	TotalCount        int      `json:"total_count"`
	LatestSummary     string   `json:"latest_summary"`
	LatestNextStep    string   `json:"latest_next_step,omitempty"`
	LatestUpdatedAt   string   `json:"latest_updated_at"`
	LinkedMemoryCount int      `json:"linked_memory_count"`
}

// ProgressSyncState reports repo_sync alignment and pending Git commits for a project.
type ProgressSyncState struct {
	Scope               string           `json:"scope"`
	RepoPath            string           `json:"repo_path,omitempty"`
	SyncedCommit        string           `json:"synced_commit,omitempty"`
	HeadCommit          string           `json:"head_commit,omitempty"`
	PendingCommits      []GitCommitBrief `json:"pending_commits,omitempty"`
	UnlinkedMemoryCount int              `json:"unlinked_memory_count"`
	InSync              bool             `json:"in_sync"`
}

// ProgressSyncResult reports the items processed during SyncProjectProgress.
type ProgressSyncResult struct {
	Scope          string `json:"scope"`
	HeadCommit     string `json:"head_commit,omitempty"`
	SyncedMemories int    `json:"synced_memories"`
	SyncedCommits  int    `json:"synced_commits"`
}

// ProgressProjectSummary aggregates recent activity counts and flow for one project scope.
type ProgressProjectSummary struct {
	Scope           string           `json:"scope"`
	ProjectName     string           `json:"project_name"`
	LatestUpdatedAt string           `json:"latest_updated_at"`
	TotalCount      int              `json:"total_count"`
	WorkCount       int              `json:"work_count"`
	MemoryCount     int              `json:"memory_count"`
	GitCount        int              `json:"git_count"`
	RecentHistory   []ProgressRecord `json:"recent_history,omitempty"`
}

// ProgressQuery filters progress activity board and timeline queries.
type ProgressQuery struct {
	Scope        string
	Account      string
	Kind         string
	SessionID    string
	Track        string
	Category     string
	DoneWindow   string
	ExplicitRepo string
	Page         int
	PageSize     int
}

// ProgressOverview holds the complete dashboard/MCP payload for project progress.
type ProgressOverview struct {
	Scope           string                    `json:"scope"`
	TotalCount      int                       `json:"total_count"`
	FilteredCount   int                       `json:"filtered_count"`
	Page            int                       `json:"page"`
	PageSize        int                       `json:"page_size"`
	TotalPages      int                       `json:"total_pages"`
	KindCounts      map[string]int            `json:"kind_counts,omitempty"`
	Projects        []ProgressProjectSummary  `json:"projects"`
	RecentHistory   []ProgressRecord          `json:"recent_history"`
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
