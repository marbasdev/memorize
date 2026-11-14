package domain

const (
	GuardrailVerdictPass          = "pass"
	GuardrailVerdictRuleViolation = "rule_violation"
	GuardrailVerdictPreMutateGate = "pre_mutate_gate"
	GuardrailVerdictPlanViolation = "plan_violation"
)

// GuardrailLogInput records a single guardrail or plan verification audit event.
type GuardrailLogInput struct {
	Scope       string
	Source      string
	ToolName    string
	Verdict     string
	Blocked     bool
	RuleCount   int
	ViolatedIDs []int64
	PlanSnippet string
	Details     string
	ElapsedMs   float64
}

// GuardrailLogRecord represents one stored row in guardrail_logs.
type GuardrailLogRecord struct {
	ID          int64   `json:"id"`
	Scope       string  `json:"scope"`
	Source      string  `json:"source"`
	ToolName    string  `json:"tool_name"`
	Verdict     string  `json:"verdict"`
	Blocked     bool    `json:"blocked"`
	RuleCount   int     `json:"rule_count"`
	ViolatedIDs []int64 `json:"violated_ids,omitempty"`
	PlanSnippet string  `json:"plan_snippet"`
	Details     string  `json:"details,omitempty"`
	ElapsedMs   float64 `json:"elapsed_ms,omitempty"`
	CreatedAt   string  `json:"created_at"`
}

// GuardrailLogQuery filters guardrail audit logs.
type GuardrailLogQuery struct {
	Scope    string
	State    string // "all", "blocked", "pass"
	Source   string // "", "pretool", "git-precommit", "verify"
	Page     int
	PageSize int
}

// GuardrailLogPage returns a paginated guardrail audit log result set.
type GuardrailLogPage struct {
	Records       []GuardrailLogRecord `json:"records"`
	TotalCount    int                  `json:"total_count"`
	BlockedCount  int                  `json:"blocked_count"`
	PassCount     int                  `json:"pass_count"`
	FilteredCount int                  `json:"filtered_count"`
	Page          int                  `json:"page"`
	PageSize      int                  `json:"page_size"`
	TotalPages    int                  `json:"total_pages"`
}
