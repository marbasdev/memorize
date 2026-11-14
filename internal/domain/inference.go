package domain

import (
	"context"
	"fmt"
	"os"
	"runtime"
	"strings"
	"time"
)

// ElapsedMsSince returns milliseconds elapsed since start, floored at zero.
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

// DefaultProvider returns the platform-appropriate default inference provider:
// "local" (OpenAI-compatible Custom Local URL) on Windows where built-in GGUF llama-server
// is disabled, and "gguf" on macOS/Linux.
func DefaultProvider() string {
	if runtime.GOOS == "windows" {
		return "local"
	}
	return "gguf"
}

// ResolveExplicitEnvProvider returns an active provider when MEMORY_PROVIDER is set
// OR when exactly one provider-specific environment variable is explicitly configured.
func ResolveExplicitEnvProvider() string {
	if explicit := NormalizeProviderID(os.Getenv("MEMORY_PROVIDER")); explicit != "" {
		return explicit
	}
	var matched []string
	if strings.TrimSpace(os.Getenv("GEMINI_API_KEY")) != "" {
		matched = append(matched, "gemini")
	}
	if strings.TrimSpace(os.Getenv("TYPESAFE_API_KEY")) != "" ||
		strings.TrimSpace(os.Getenv("FLIDE_API_KEY")) != "" ||
		strings.TrimSpace(os.Getenv("JEV_API_URL")) != "" {
		matched = append(matched, "jev")
	}
	if strings.TrimSpace(os.Getenv("LOCAL_MODEL_URL")) != "" {
		matched = append(matched, "local")
	}
	if strings.TrimSpace(os.Getenv("LOCAL_GGUF_URL")) != "" {
		matched = append(matched, "gguf")
	}
	if len(matched) == 1 {
		return matched[0]
	}
	return ""
}

// Evaluator is the core inference contract for relevance scoring, mutation gatekeeping, and plan verification.
type Evaluator interface {
	Name() string
	ScoreRelevance(ctx context.Context, currentTask string, facts []Fact) ([]ScoredFact, error)
	EvaluateMutation(ctx context.Context, scope, candidate string, existing []Fact) (MutationDecision, error)
	VerifyPlan(ctx context.Context, proposedPlan string, rules []Fact) ([]PlanViolation, error)
}

// Consolidator synthesizes multiple fragmented memories into one dense rule statement.
type Consolidator interface {
	ConsolidateFacts(ctx context.Context, scope, category string, facts []Fact) (string, error)
}

// ConsolidationPlanner proposes cohesive groups of overlapping or redundant memories.
type ConsolidationPlanner interface {
	SuggestConsolidations(ctx context.Context, scope string, facts []Fact) ([]ConsolidationProposal, error)
}

// ConfidenceCalibrator assesses individual semantic certainty [0.0, 1.0] for active memories.
type ConfidenceCalibrator interface {
	CalibrateConfidence(ctx context.Context, scope string, facts []Fact) (map[int64]float64, error)
}

// ProgressCommitCategorizer maps Git commits into concise progress entries during Progress Sync.
type ProgressCommitCategorizer interface {
	SummarizeCommitProgress(ctx context.Context, scope, lang string, commits []GitCommitBrief) ([]CommitProgressItem, error)
}

// TaxonomyPlanner proposes non-destructive category/sub_category reorganizations.
type TaxonomyPlanner interface {
	SuggestTaxonomyRebalance(ctx context.Context, scope string, facts []Fact) ([]TaxonomyRebalanceItem, error)
}

// EvalTask identifies each discrete workload in memorize for specialized model routing.
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

// EvalTaskSpec describes a routable memorize feature and whether it requires text generation.
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

// AllEvalTasks returns the canonical ordered list of routable feature specs.
func AllEvalTasks() []EvalTaskSpec {
	out := make([]EvalTaskSpec, len(evalTaskCatalog))
	copy(out, evalTaskCatalog)
	return out
}

// FindEvalTask looks up a task specification by ID.
func FindEvalTask(task EvalTask) (EvalTaskSpec, bool) {
	for _, spec := range evalTaskCatalog {
		if spec.Task == task {
			return spec, true
		}
	}
	return EvalTaskSpec{}, false
}

// NormalizeProviderID canonicalizes provider identifiers (e.g. flide -> jev).
func NormalizeProviderID(provider string) string {
	p := strings.ToLower(strings.TrimSpace(provider))
	if p == "flide" {
		return "jev"
	}
	return p
}

// IsDecisionOnlySpec reports whether (provider, model) is a non-generative decision engine.
func IsDecisionOnlySpec(provider, model string) bool {
	p := NormalizeProviderID(provider)
	m := strings.ToLower(strings.TrimSpace(model))
	if p == "jev" {
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
	switch p {
	case "gguf", "gemini", "jev", "local":
	default:
		return fmt.Errorf("unknown provider %q for task %s", provider, task)
	}
	return nil
}

// TaskCapableEvaluator lets wrapper evaluators expose underlying per-task capabilities.
type TaskCapableEvaluator interface {
	SupportsTask(task EvalTask) bool
}

// EvaluatorSupportsTask reports whether ev can genuinely execute the given EvalTask.
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
		return c1 || c2
	case TaskProgress:
		_, ok := ev.(ProgressCommitCategorizer)
		return ok
	case TaskTaxonomy:
		_, ok := ev.(TaxonomyPlanner)
		return ok
	default:
		return true
	}
}

// EvalTaskStats summarizes observed runtime latency and outcomes for one EvalTask.
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

// EvalEngineStats summarizes observed runtime latency and outcomes for one provider:model engine.
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

// EvalTelemetrySample represents a single recent evaluator invocation for sparkline visualization.
type EvalTelemetrySample struct {
	ID        int64    `json:"id"`
	Task      EvalTask `json:"task"`
	Engine    string   `json:"engine"`
	ElapsedMs float64  `json:"elapsed_ms"`
	ItemCount int      `json:"item_count"`
	Status    string   `json:"status"`
	CreatedAt string   `json:"created_at"`
}

// EvalTelemetryReport aggregates measured per-task and per-engine inference performance across a lookback window.
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
