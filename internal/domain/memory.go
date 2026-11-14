package domain

import "strings"

const (
	MaxConsolidatedRunes = 180
	UncategorizedSubKey  = "__none__"

	DefaultProgressRetentionDays  = 0
	DefaultGuardrailRetentionDays = 0
)

// Thresholds configures read/write gate thresholds for memory ingestion and retrieval.
type Thresholds struct {
	MinPermanent     float64 `json:"min_permanent"`
	MinConfidence    float64 `json:"min_confidence"`
	PinInvariant     float64 `json:"pin_invariant"`
	MinRelevance     float64 `json:"min_relevance"`
	MinRelConfidence float64 `json:"min_rel_confidence"`
}

// DefaultThresholds returns the default memory gate thresholds.
func DefaultThresholds() Thresholds {
	return Thresholds{
		MinPermanent:     0.75, // Write Gate
		MinConfidence:    0.80, // Write Gate
		PinInvariant:     0.88, // Write Gate
		MinRelevance:     3.5,  // Read Gate
		MinRelConfidence: 0.75, // Read Gate
	}
}

// Fact represents an active stored memory entry passed to inference evaluators.
type Fact struct {
	ID          int64    `json:"id"`
	Scope       string   `json:"scope"`
	Account     string   `json:"account,omitempty"`
	Category    string   `json:"category"`
	SubCategory string   `json:"sub_category,omitempty"`
	Tags        []string `json:"tags,omitempty"`
	Content     string   `json:"content"`
	Pinned      bool     `json:"pinned,omitempty"`
	Superseded  []string `json:"superseded,omitempty"`
}

// ScoredFact pairs a retrieved Fact with evaluator relevance and confidence scores.
type ScoredFact struct {
	Fact
	Score      float64 `json:"score"`      // 0.0 ~ 5.0
	Confidence float64 `json:"confidence"` // 0.0 ~ 1.0
}

// MutationDecision captures the gatekeeper verdict for committing a candidate fact.
type MutationDecision struct {
	Category          string   `json:"category"`
	SubCategory       string   `json:"sub_category,omitempty"`
	Tags              []string `json:"tags"`
	Tier              string   `json:"tier,omitempty"`        // "invariant" | "rule" | "preference" | "transient"
	Cardinality       string   `json:"cardinality,omitempty"` // "exclusive" | "additive"
	IsPermanent       float64  `json:"is_permanent"`          // 0.0 ~ 1.0
	IsGlobalInvariant float64  `json:"is_global_invariant"`   // 0.0 ~ 1.0 (Pinned Core Invariant)
	Action            string   `json:"action"`                // "INSERT" | "OVERWRITE" | "IGNORE"
	ConflictFactID    int64    `json:"conflict_fact_id"`      // Primary target ID
	ConflictFactIDs   []int64  `json:"conflict_fact_ids"`     // 1:N conflict or duplicate IDs
	Confidence        float64  `json:"confidence"`            // 0.0 ~ 1.0
}

// ApplyDecisionSemantics enforces structural consistency across the LLM's own
// schema fields (Tier, Cardinality, Action) without hardcoded keyword rules.
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

// PlanViolation represents a rule conflict flagged during plan verification.
type PlanViolation struct {
	Fact
	Probability float64 `json:"probability"` // 0.0 ~ 1.0
	Confidence  float64 `json:"confidence"`  // 0.0 ~ 1.0
}

// ConsolidationProposal describes one recommended merge of overlapping active memories.
type ConsolidationProposal struct {
	SourceIDs     []int64 `json:"source_ids"`
	Category      string  `json:"category"`
	Pinned        bool    `json:"pinned"`
	MergedContent string  `json:"merged_content"`
	Reason        string  `json:"reason,omitempty"`
	Sources       []Fact  `json:"sources,omitempty"`
}

// TaxonomyRebalanceItem describes a proposed non-destructive category/sub-category relabeling.
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

// CleanupPlan combines AI consolidation and taxonomy rebalancing recommendations.
type CleanupPlan struct {
	Scope          string                  `json:"scope"`
	CategoryFilter string                  `json:"category_filter,omitempty"`
	SourceLabel    string                  `json:"source"`
	Proposals      []ConsolidationProposal `json:"proposals"`
	RebalanceItems []TaxonomyRebalanceItem `json:"rebalance_items"`
}

// MemoryRecord represents a full stored row from the memories table.
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

// DashboardStats summarizes memory counts and confidence buckets for a scope.
type DashboardStats struct {
	TotalCount     int   `json:"total_count"`
	ActiveCount    int   `json:"active_count"`
	PinnedCount    int   `json:"pinned_count"`
	TombCount      int   `json:"tomb_count"`
	ConfidenceBins []int `json:"confidence_bins"`
}

// SessionSummary aggregates per-session delivery/progress counts.
type SessionSummary struct {
	SessionID string `json:"session_id"`
	Scope     string `json:"scope,omitempty"`
	Count     int    `json:"count"`
	UpdatedAt string `json:"updated_at,omitempty"`
}

// DashboardFacets provides filter dropdown values for the web console.
type DashboardFacets struct {
	Scopes        []string            `json:"scopes"`
	Accounts      []string            `json:"accounts"`
	Categories    []string            `json:"categories"`
	SubCategories map[string][]string `json:"sub_categories"`
	Sessions      []SessionSummary    `json:"sessions"`
}

// RecordQuery filters paginated memory records.
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

// RecordPage holds a paginated memory query result.
type RecordPage struct {
	Records             []MemoryRecord `json:"records"`
	TotalRecords        int            `json:"total_records"`
	FilteredRecords     int            `json:"filtered_records"`
	FilteredActiveCount int            `json:"filtered_active_records"`
	Page                int            `json:"page"`
	PageSize            int            `json:"page_size"`
	TotalPages          int            `json:"total_pages"`
}

// ProbeResult captures diagnostic relevance evaluation timings and scores.
type ProbeResult struct {
	Provider   string       `json:"provider"`
	ElapsedMs  float64      `json:"elapsed_ms"`
	Thresholds Thresholds   `json:"thresholds"`
	Pinned     []Fact       `json:"pinned"`
	Scored     []ScoredFact `json:"scored"`
}

// SubCategoryDistItem holds treemap metrics for one sub-category block.
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

// CategoryDistItem holds treemap metrics for one category block.
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

// ScopeDistribution represents the 2D category/sub-category distribution of memories in a scope.
type ScopeDistribution struct {
	Scope                  string             `json:"scope"`
	TotalActive            int                `json:"total_active"`
	TotalPinned            int                `json:"total_pinned"`
	TotalTomb              int                `json:"total_tomb"`
	UncategorizedTotal     int                `json:"uncategorized_total"`
	DuplicateSubCategories []string           `json:"duplicate_sub_categories,omitempty"`
	Categories             []CategoryDistItem `json:"categories"`
}

// RetentionPolicy defines automatic TTL retention settings for completed progress history and guardrail audit logs.
type RetentionPolicy struct {
	ProgressDays            int  `json:"progress_days"`             // 0 = keep forever; >0 = auto-prune done entries older than N days
	GuardrailDays           int  `json:"guardrail_days"`            // 0 = keep forever; >0 = auto-prune logs older than N days
	GuardrailIncludeBlocked bool `json:"guardrail_include_blocked"` // false = auto-prune PASS logs only (protect blocked violations)
}

// NormalizeRetentionDays clamps retention days to 0 (keep forever) or [1..3650].
func NormalizeRetentionDays(days int) int {
	if days <= 0 {
		return 0
	}
	if days > 3650 {
		return 3650
	}
	return days
}
