package domain

import (
	"sort"
	"strings"
	"time"
)

const (
	MaxConsolidatedRunes = 180
	UncategorizedSubKey  = "__none__"

	DefaultProgressRetentionDays  = 0
	DefaultGuardrailRetentionDays = 0

	TurnBaselineTTL = 2 * time.Hour
	TurnInputTTL    = 15 * time.Minute
)

type Thresholds struct {
	MinPermanent     float64 `json:"min_permanent"`
	MinConfidence    float64 `json:"min_confidence"`
	PinInvariant     float64 `json:"pin_invariant"`
	MinRelevance     float64 `json:"min_relevance"`
	MinRelConfidence float64 `json:"min_rel_confidence"`
}

func DefaultThresholds() Thresholds {
	return Thresholds{
		MinPermanent:     0.75, // Write Gate
		MinConfidence:    0.80, // Write Gate
		PinInvariant:     0.88, // Write Gate
		MinRelevance:     3.5,  // Read Gate
		MinRelConfidence: 0.75, // Read Gate
	}
}

type Fact struct {
	ID          int64    `json:"id"`
	Scope       string   `json:"scope"`
	Account     string   `json:"account,omitempty"`
	Origin      string   `json:"origin,omitempty"`   // ""/"local" or "team"
	RuleKey     string   `json:"rule_key,omitempty"` // Stable Team Rule Pack identifier
	Category    string   `json:"category"`
	SubCategory string   `json:"sub_category,omitempty"`
	Tags        []string `json:"tags,omitempty"`
	Content     string   `json:"content"`
	Pinned      bool     `json:"pinned,omitempty"`
	Superseded  []string `json:"superseded,omitempty"`
}

func (f Fact) IsTeam() bool {
	return f.Origin == RuleOriginTeam || f.RuleKey != ""
}

type ScoredFact struct {
	Fact
	Score      float64 `json:"score"`      // 0.0 ~ 5.0
	Confidence float64 `json:"confidence"` // 0.0 ~ 1.0
}

type MutationDecision struct {
	Category          string   `json:"category"`
	SubCategory       string   `json:"sub_category,omitempty"`
	Tags              []string `json:"tags"`
	Tier              string   `json:"tier,omitempty"`        // "invariant" | "rule" | "preference" | "transient"
	Cardinality       string   `json:"cardinality,omitempty"` // "exclusive" | "additive"
	IsPermanent       float64  `json:"is_permanent"`          // 0.0 ~ 1.0
	IsGlobalInvariant float64  `json:"is_global_invariant"`   // 0.0 ~ 1.0 (Pinned Core Rule)
	Action            string   `json:"action"`                // "INSERT" | "OVERWRITE" | "IGNORE"
	ConflictFactID    int64    `json:"conflict_fact_id"`      // Primary target ID
	ConflictFactIDs   []int64  `json:"conflict_fact_ids"`     // 1:N conflict or duplicate IDs
	Confidence        float64  `json:"confidence"`            // 0.0 ~ 1.0
}

func ApplyDecisionSemantics(dec *MutationDecision) {
	switch strings.ToLower(strings.TrimSpace(dec.Tier)) {
	case "preference":
		dec.Category = "preference"
		if dec.IsGlobalInvariant > 0.25 {
			dec.IsGlobalInvariant = 0.25
		}
	case "transient":
		if dec.IsPermanent > 0.40 {
			dec.IsPermanent = 0.25
		}
	case "invariant":
		if dec.IsGlobalInvariant < 0.88 {
			dec.IsGlobalInvariant = 0.90
		}
	}
	if strings.EqualFold(strings.TrimSpace(dec.Cardinality), "additive") && dec.Action == "OVERWRITE" {
		dec.Action = "INSERT"
		dec.ConflictFactID = 0
		dec.ConflictFactIDs = nil
	}
}

type PlanViolation struct {
	Fact
	Probability float64 `json:"probability"` // 0.0 ~ 1.0
	Confidence  float64 `json:"confidence"`  // 0.0 ~ 1.0
}

type ConsolidationProposal struct {
	SourceIDs     []int64 `json:"source_ids"`
	Category      string  `json:"category"`
	Pinned        bool    `json:"pinned"`
	MergedContent string  `json:"merged_content"`
	Reason        string  `json:"reason,omitempty"`
	Sources       []Fact  `json:"sources,omitempty"`
}

type TaxonomyRebalanceItem struct {
	ID             int64  `json:"id"`
	OldCategory    string `json:"old_category"`
	OldSubCategory string `json:"old_sub_category,omitempty"`
	NewCategory    string `json:"new_category"`
	NewSubCategory string `json:"new_sub_category,omitempty"`
	IsNewTaxonomy  bool   `json:"is_new_taxonomy,omitempty"`
	Reason         string `json:"reason,omitempty"`
	Pinned         bool   `json:"pinned,omitempty"`
	Content        string `json:"content,omitempty"`
}

type CleanupPlan struct {
	Scope          string                  `json:"scope"`
	CategoryFilter string                  `json:"category_filter,omitempty"`
	SourceLabel    string                  `json:"source"`
	Proposals      []ConsolidationProposal `json:"proposals"`
	RebalanceItems []TaxonomyRebalanceItem `json:"rebalance_items"`
}

type MemoryRecord struct {
	ID           int64    `json:"id"`
	Scope        string   `json:"scope"`
	Account      string   `json:"account"`
	Category     string   `json:"category"`
	SubCategory  string   `json:"sub_category"`
	Tags         []string `json:"tags"`
	Content      string   `json:"content"`
	Confidence   float64  `json:"confidence"`
	Pinned       bool     `json:"pinned"`
	Active       bool     `json:"active"`
	SupersededBy *int64   `json:"superseded_by"`
	Replaces     []string `json:"replaces,omitempty"`
	CreatedAt    string   `json:"created_at"`
}

type DashboardStats struct {
	TotalCount     int   `json:"total_count"`
	ActiveCount    int   `json:"active_count"`
	PinnedCount    int   `json:"pinned_count"`
	TombCount      int   `json:"tomb_count"`
	ConfidenceBins []int `json:"confidence_bins"`
}

type SessionSummary struct {
	SessionID string `json:"session_id"`
	Scope     string `json:"scope,omitempty"`
	Count     int    `json:"count"`
	UpdatedAt string `json:"updated_at,omitempty"`
}

type DashboardFacets struct {
	Scopes        []string            `json:"scopes"`
	Accounts      []string            `json:"accounts"`
	Categories    []string            `json:"categories"`
	SubCategories map[string][]string `json:"sub_categories"`
	Sessions      []SessionSummary    `json:"sessions"`
}

type RecordQuery struct {
	Scope       string
	SessionID   string
	Account     string
	State       string
	Category    string
	SubCategory string
	Search      string
	Page        int
	PageSize    int
}

type RecordPage struct {
	Records             []MemoryRecord `json:"records"`
	TotalRecords        int            `json:"total_records"`
	FilteredRecords     int            `json:"filtered_records"`
	FilteredActiveCount int            `json:"filtered_active_records"`
	Page                int            `json:"page"`
	PageSize            int            `json:"page_size"`
	TotalPages          int            `json:"total_pages"`
}

type ProbeResult struct {
	Provider   string       `json:"provider"`
	ElapsedMs  float64      `json:"elapsed_ms"`
	Thresholds Thresholds   `json:"thresholds"`
	Pinned     []Fact       `json:"pinned"`
	Scored     []ScoredFact `json:"scored"`
}

type SubCategoryDistItem struct {
	SubCategory         string  `json:"sub_category"`
	IsUncategorized     bool    `json:"is_uncategorized,omitempty"`
	ActiveCount         int     `json:"active_count"`
	PinnedCount         int     `json:"pinned_count"`
	GeneralCount        int     `json:"general_count"`
	SharePct            float64 `json:"share_pct"`
	FlexWeight          int     `json:"flex_weight"`
	X                   float64 `json:"x"`
	Y                   float64 `json:"y"`
	W                   float64 `json:"w"`
	H                   float64 `json:"h"`
	DuplicateAcrossCats bool    `json:"duplicate_across_cats,omitempty"`
}

type CategoryDistItem struct {
	Category           string                `json:"category"`
	ActiveCount        int                   `json:"active_count"`
	PinnedCount        int                   `json:"pinned_count"`
	GeneralCount       int                   `json:"general_count"`
	UncategorizedCount int                   `json:"uncategorized_count"`
	TombCount          int                   `json:"tomb_count"`
	SharePct           float64               `json:"share_pct"`
	FlexWeight         int                   `json:"flex_weight"`
	X                  float64               `json:"x"`
	Y                  float64               `json:"y"`
	W                  float64               `json:"w"`
	H                  float64               `json:"h"`
	SubItems           []SubCategoryDistItem `json:"sub_items,omitempty"`
}

type ScopeDistribution struct {
	Scope                  string             `json:"scope"`
	TotalActive            int                `json:"total_active"`
	TotalPinned            int                `json:"total_pinned"`
	TotalTomb              int                `json:"total_tomb"`
	UncategorizedTotal     int                `json:"uncategorized_total"`
	DuplicateSubCategories []string           `json:"duplicate_sub_categories,omitempty"`
	Categories             []CategoryDistItem `json:"categories"`
}

type RetentionPolicy struct {
	ProgressDays            int  `json:"progress_days"`             // 0 = keep forever; >0 = auto-prune done entries older than N days
	GuardrailDays           int  `json:"guardrail_days"`            // 0 = keep forever; >0 = auto-prune logs older than N days
	GuardrailIncludeBlocked bool `json:"guardrail_include_blocked"` // false = auto-prune PASS logs only (protect blocked violations)
}

func NormalizeRetentionDays(days int) int {
	if days <= 0 {
		return 0
	}
	if days > 3650 {
		return 3650
	}
	return days
}

func FactPriorityRank(f Fact) int {
	if f.IsTeam() {
		return 0
	}
	sc := NormalizeScope(f.Scope)
	switch {
	case strings.HasPrefix(sc, "project:"):
		return 1
	case sc == "global":
		return 2
	default:
		return 3
	}
}

func SortFactsByPrefixTier(facts []Fact) {
	if len(facts) < 2 {
		return
	}
	sort.SliceStable(facts, func(i, j int) bool {
		return FactPriorityRank(facts[i]) < FactPriorityRank(facts[j])
	})
}

const MemoryBundleFormat = "praxis.bundle.v1"

type MemoryBundleItem struct {
	Scope       string   `json:"scope"`
	Category    string   `json:"category"`
	SubCategory string   `json:"sub_category,omitempty"`
	Tags        []string `json:"tags,omitempty"`
	Content     string   `json:"content"`
	Confidence  float64  `json:"confidence,omitempty"`
	Pinned      bool     `json:"pinned,omitempty"`
}

type MemoryBundle struct {
	Format     string             `json:"format"`
	ExportedAt string             `json:"exported_at"`
	Items      []MemoryBundleItem `json:"items"`
}

type BundleImportOptions struct {
	Mode        string `json:"mode,omitempty"`         // "merge" (AI dedup/overwrite) or "exact" (deterministic exact dedup)
	TargetScope string `json:"target_scope,omitempty"` // optional scope override for all imported items
	DryRun      bool   `json:"dry_run,omitempty"`
}

type BundleImportItemResult struct {
	Index       int     `json:"index"`
	Scope       string  `json:"scope"`
	Category    string  `json:"category"`
	SubCategory string  `json:"sub_category,omitempty"`
	Content     string  `json:"content"`
	Pinned      bool    `json:"pinned,omitempty"`
	Action      string  `json:"action"` // "insert", "overwrite", "skip_exact", "skip_duplicate", "skip_invalid"
	Reason      string  `json:"reason,omitempty"`
	TargetIDs   []int64 `json:"target_ids,omitempty"`
	NewID       int64   `json:"new_id,omitempty"`
}

type BundleImportResult struct {
	DryRun      bool                     `json:"dry_run"`
	Mode        string                   `json:"mode"`
	Engine      string                   `json:"engine,omitempty"`
	Total       int                      `json:"total"`
	Inserted    int                      `json:"inserted"`
	Overwritten int                      `json:"overwritten"`
	Skipped     int                      `json:"skipped"`
	Items       []BundleImportItemResult `json:"items"`
}
