package domain

import (
	"context"
	"fmt"
	"runtime"
	"strings"
	"time"
)

func ElapsedMsSince(start time.Time) float64 {
	if start.IsZero() {
		return 0
	}
	ms := float64(time.Since(start).Microseconds()) / 1000.0
	if ms < 0 {
		return 0
	}
	return ms
}

func DefaultProvider() string {
	if runtime.GOOS == "windows" {
		return "local"
	}
	return "gguf"
}

type Evaluator interface {
	Name() string
	ScoreRelevance(ctx context.Context, currentTask string, facts []Fact) ([]ScoredFact, error)
	EvaluateMutation(ctx context.Context, scope, candidate string, existing []Fact) (MutationDecision, error)
	VerifyPlan(ctx context.Context, proposedPlan string, rules []Fact) ([]PlanViolation, error)
}

type BatchMutationEvaluator interface {
	EvaluateMutations(ctx context.Context, scope string, candidates []string, existing []Fact) ([]MutationDecision, error)
}

type Consolidator interface {
	ConsolidateFacts(ctx context.Context, scope, category string, facts []Fact) (string, error)
}

type ConsolidationPlanner interface {
	SuggestConsolidations(ctx context.Context, scope string, facts []Fact) ([]ConsolidationProposal, error)
}

type ConfidenceCalibrator interface {
	CalibrateConfidence(ctx context.Context, scope string, facts []Fact) (map[int64]float64, error)
}

type ProgressCommitCategorizer interface {
	SummarizeCommitProgress(ctx context.Context, scope, lang string, commits []GitCommitBrief) ([]CommitProgressItem, error)
}

type ProgressMilestoneSummarizer interface {
	SummarizeCommitMilestones(ctx context.Context, scope, lang string, commits []GitCommitBrief) ([]GitMilestoneSummary, error)
}

type TaxonomyPlanner interface {
	SuggestTaxonomyRebalance(ctx context.Context, scope string, facts []Fact) ([]TaxonomyRebalanceItem, error)
}

type RulePackPlanner interface {
	DraftRulePackProposal(ctx context.Context, scope, rawRuleText, sourceCat, sourceSub string, outline []RuleOutlineEntry) (RulePlacementProposal, error)
	FindRedundantLocalMemories(ctx context.Context, scope string, teamRules []TeamRule, localFacts []Fact) ([]RuleDuplicateSuggestion, error)
}

type EvalTask string

const (
	TaskRelevance   EvalTask = "relevance"
	TaskMutation    EvalTask = "mutation"
	TaskVerify      EvalTask = "verify"
	TaskCalibrate   EvalTask = "calibrate"
	TaskConsolidate EvalTask = "consolidate"
	TaskProgress    EvalTask = "progress"
	TaskTaxonomy    EvalTask = "taxonomy"
)

type EvalTaskSpec struct {
	Task               EvalTask `json:"task"`
	RequiresGenerative bool     `json:"requires_generative"`
	ProviderKey        string   `json:"provider_key"`
	ModelKey           string   `json:"model_key"`
}

var evalTaskCatalog = []EvalTaskSpec{
	{Task: TaskRelevance, RequiresGenerative: false, ProviderKey: "task_relevance_provider", ModelKey: "task_relevance_model"},
	{Task: TaskMutation, RequiresGenerative: false, ProviderKey: "task_mutation_provider", ModelKey: "task_mutation_model"},
	{Task: TaskVerify, RequiresGenerative: false, ProviderKey: "task_verify_provider", ModelKey: "task_verify_model"},
	{Task: TaskCalibrate, RequiresGenerative: false, ProviderKey: "task_calibrate_provider", ModelKey: "task_calibrate_model"},
	{Task: TaskConsolidate, RequiresGenerative: true, ProviderKey: "task_consolidate_provider", ModelKey: "task_consolidate_model"},
	{Task: TaskProgress, RequiresGenerative: true, ProviderKey: "task_progress_provider", ModelKey: "task_progress_model"},
	{Task: TaskTaxonomy, RequiresGenerative: true, ProviderKey: "task_taxonomy_provider", ModelKey: "task_taxonomy_model"},
}

func AllEvalTasks() []EvalTaskSpec {
	out := make([]EvalTaskSpec, len(evalTaskCatalog))
	copy(out, evalTaskCatalog)
	return out
}

func FindEvalTask(task EvalTask) (EvalTaskSpec, bool) {
	for _, spec := range evalTaskCatalog {
		if spec.Task == task {
			return spec, true
		}
	}
	return EvalTaskSpec{}, false
}

func IsDecisionOnlySpec(provider, model string) bool {
	p := NormalizeProviderID(provider)
	m := strings.ToLower(strings.TrimSpace(model))
	if spec, ok := FindProviderSpec(p); ok && spec.DecisionOnly {
		return true
	}
	return (p == "" || p == "gemini") && strings.HasPrefix(m, "gemini-decision")
}

// ValidateTaskRoute ensures non-generative decision models are never bound to text-generation tasks.
func ValidateTaskRoute(task EvalTask, provider, model string) error {
	spec, ok := FindEvalTask(task)
	if !ok {
		return fmt.Errorf("unknown evaluation task: %q", task)
	}
	p := NormalizeProviderID(provider)
	if spec.RequiresGenerative && IsDecisionOnlySpec(p, model) {
		name := strings.TrimSpace(model)
		if p != "" && name != "" {
			name = p + ":" + name
		} else if name == "" {
			name = p
		}
		return fmt.Errorf("task %q requires a text-generation model and cannot use decision-only engine %q", task, name)
	}
	if p == "" {
		return nil
	}
	if _, ok := FindProviderSpec(p); !ok {
		return fmt.Errorf("unknown provider %q for task %s", provider, task)
	}
	return nil
}

type TaskCapableEvaluator interface {
	SupportsTask(task EvalTask) bool
}

type ContextWindowProvider interface {
	InputTokenLimit(ctx context.Context) int
}

type RuleBatchSizeProvider interface {
	RuleBatchSize() int
}

func EvaluatorInputTokenLimit(ctx context.Context, ev Evaluator) int {
	if p, ok := ev.(ContextWindowProvider); ok && ev != nil {
		return p.InputTokenLimit(ctx)
	}
	return 0
}

func EvaluatorRuleBatchSize(ev Evaluator) int {
	if p, ok := ev.(RuleBatchSizeProvider); ok && ev != nil {
		return p.RuleBatchSize()
	}
	return 0
}

func EvaluatorSupportsTask(ev Evaluator, task EvalTask) bool {
	if ev == nil {
		return false
	}
	if tc, ok := ev.(TaskCapableEvaluator); ok {
		return tc.SupportsTask(task)
	}
	switch task {
	case TaskCalibrate:
		_, ok := ev.(ConfidenceCalibrator)
		return ok
	case TaskConsolidate:
		_, c1 := ev.(Consolidator)
		_, c2 := ev.(ConsolidationPlanner)
		_, c3 := ev.(RulePackPlanner)
		return c1 || c2 || c3
	case TaskProgress:
		_, c1 := ev.(ProgressCommitCategorizer)
		_, c2 := ev.(ProgressMilestoneSummarizer)
		return c1 || c2
	case TaskTaxonomy:
		_, ok := ev.(TaxonomyPlanner)
		return ok
	default:
		return true
	}
}

type EvalTaskStats struct {
	Task           EvalTask `json:"task"`
	Calls          int      `json:"calls"`
	OKCount        int      `json:"ok_count"`
	TimeoutCount   int      `json:"timeout_count"`
	ErrorCount     int      `json:"error_count"`
	AvgElapsedMs   float64  `json:"avg_elapsed_ms"`
	P95ElapsedMs   float64  `json:"p95_elapsed_ms"`
	MaxElapsedMs   float64  `json:"max_elapsed_ms"`
	TotalElapsedMs float64  `json:"total_elapsed_ms"`
	AvgItems       float64  `json:"avg_items"`
	LastEngine     string   `json:"last_engine,omitempty"`
	LastElapsedMs  float64  `json:"last_elapsed_ms"`
	LastStatus     string   `json:"last_status,omitempty"`
	LastAt         string   `json:"last_at,omitempty"`
}

type EvalEngineStats struct {
	Engine         string  `json:"engine"`
	Calls          int     `json:"calls"`
	OKCount        int     `json:"ok_count"`
	TimeoutCount   int     `json:"timeout_count"`
	ErrorCount     int     `json:"error_count"`
	AvgElapsedMs   float64 `json:"avg_elapsed_ms"`
	P95ElapsedMs   float64 `json:"p95_elapsed_ms"`
	MaxElapsedMs   float64 `json:"max_elapsed_ms"`
	TotalElapsedMs float64 `json:"total_elapsed_ms"`
	AvgItems       float64 `json:"avg_items"`
}

type EvalTelemetrySample struct {
	ID        int64    `json:"id"`
	Task      EvalTask `json:"task"`
	Engine    string   `json:"engine"`
	ElapsedMs float64  `json:"elapsed_ms"`
	ItemCount int      `json:"item_count"`
	Status    string   `json:"status"`
	CreatedAt string   `json:"created_at"`
}

type EvalTelemetryReport struct {
	WindowHours    int                      `json:"window_hours"`
	TotalCalls     int                      `json:"total_calls"`
	TotalTimeouts  int                      `json:"total_timeouts"`
	TotalErrors    int                      `json:"total_errors"`
	TotalElapsedMs float64                  `json:"total_elapsed_ms"`
	AvgElapsedMs   float64                  `json:"avg_elapsed_ms"`
	P95ElapsedMs   float64                  `json:"p95_elapsed_ms"`
	Tasks          []EvalTaskStats          `json:"tasks"`
	ByTask         map[string]EvalTaskStats `json:"by_task"`
	Engines        []EvalEngineStats        `json:"engines"`
	RecentSamples  []EvalTelemetrySample    `json:"recent_samples"`
}
