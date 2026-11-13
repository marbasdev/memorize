package memory

import (
	"context"
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
)

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
// If multiple unrelated provider variables are set without MEMORY_PROVIDER, it returns ""
// rather than guessing.
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

// DefaultPresetCategoriesJSON defines the default category -> sub-categories taxonomy
// used by the Web Dashboard and LLM Gatekeeper when not yet customized in SQLite config.
const DefaultPresetCategoriesJSON = `{"architecture":["engine","build","api","schema","workflow"],"convention":["style","naming","workflow"],"database":["schema","query","migration"],"diet":["allergy","food"],"preference":["documentation","communication","ui"],"security":["auth","oauth2","guard"],"testing":["unit","e2e"]}`

type presetCategoriesCtxKey struct{}

func WithPresetCategories(ctx context.Context, rawJSON string) context.Context {
	return context.WithValue(ctx, presetCategoriesCtxKey{}, rawJSON)
}

func PresetCategoriesFromContext(ctx context.Context) string {
	if ctx != nil {
		if v, ok := ctx.Value(presetCategoriesCtxKey{}).(string); ok && strings.TrimSpace(v) != "" {
			return v
		}
	}
	return DefaultPresetCategoriesJSON
}

type accountCtxKey struct{}

func WithAccount(ctx context.Context, account string) context.Context {
	acc := NormalizeAccount(account)
	if acc == "" {
		return ctx
	}
	return context.WithValue(ctx, accountCtxKey{}, acc)
}

func AccountFromContext(ctx context.Context) string {
	if ctx != nil {
		if v, ok := ctx.Value(accountCtxKey{}).(string); ok && strings.TrimSpace(v) != "" {
			return NormalizeAccount(v)
		}
	}
	return ""
}

type adminBypassCtxKey struct{}

// WithAdminBypass marks a context as originating from the local operator Dashboard UI,
// allowing cross-account inspection and maintenance while MCP/CLI calls remain strictly isolated.
func WithAdminBypass(ctx context.Context) context.Context {
	return context.WithValue(ctx, adminBypassCtxKey{}, true)
}

func IsAdminContext(ctx context.Context) bool {
	if ctx != nil {
		if v, ok := ctx.Value(adminBypassCtxKey{}).(bool); ok {
			return v
		}
	}
	return false
}

func NormalizeAccount(raw string) string {
	s := strings.ToLower(strings.TrimSpace(raw))
	s = strings.ReplaceAll(s, " ", "_")
	return s
}

func DefaultAccount() string {
	for _, k := range []string{"MEMORY_ACCOUNT", "AGY_ACCOUNT", "USER", "USERNAME"} {
		if v := NormalizeAccount(os.Getenv(k)); v != "" {
			return v
		}
	}
	if u, err := user.Current(); err == nil && u != nil {
		if v := NormalizeAccount(u.Username); v != "" {
			return v
		}
	}
	return "default"
}

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

type ScoredFact struct {
	Fact
	Score      float64 `json:"score"`      // 0.0 ~ 5.0
	Confidence float64 `json:"confidence"` // 0.0 ~ 1.0
}

type MutationDecision struct {
	Category          string   `json:"category"`               // LLM-inferred domain slug (e.g. "architecture", "security", "preference")
	SubCategory       string   `json:"sub_category,omitempty"` // Optional sub-category slug
	Tags              []string `json:"tags"`                   // LLM-inferred sub-topic tags
	Tier              string   `json:"tier,omitempty"`         // "invariant" | "rule" | "preference" | "transient"
	Cardinality       string   `json:"cardinality,omitempty"`  // "exclusive" | "additive"
	IsPermanent       float64  `json:"is_permanent"`           // 0.0 ~ 1.0
	IsGlobalInvariant float64  `json:"is_global_invariant"`    // 0.0 ~ 1.0 (Pinned Core Invariant)
	Action            string   `json:"action"`                 // "INSERT" | "OVERWRITE" | "IGNORE"
	ConflictFactID    int64    `json:"conflict_fact_id"`       // Primary target ID
	ConflictFactIDs   []int64  `json:"conflict_fact_ids"`      // 1:N conflict or duplicate IDs
	Confidence        float64  `json:"confidence"`             // 0.0 ~ 1.0
}

// ApplyDecisionSemantics enforces structural consistency across the LLM's own
// schema fields (Tier, Cardinality, Action) without any hardcoded keyword rules.
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

type Evaluator interface {
	Name() string
	ScoreRelevance(ctx context.Context, currentTask string, facts []Fact) ([]ScoredFact, error)
	EvaluateMutation(ctx context.Context, scope, candidate string, existing []Fact) (MutationDecision, error)
	VerifyPlan(ctx context.Context, proposedPlan string, rules []Fact) ([]PlanViolation, error)
}

type ConsolidationProposal struct {
	SourceIDs     []int64 `json:"source_ids"`
	Category      string  `json:"category"`
	Pinned        bool    `json:"pinned"`
	MergedContent string  `json:"merged_content"`
	Reason        string  `json:"reason,omitempty"`
	Sources       []Fact  `json:"sources,omitempty"`
}

// Consolidator is an optional capability for Evaluators to synthesize multiple
// fragmented memories in the same category into one dense rule statement.
type Consolidator interface {
	ConsolidateFacts(ctx context.Context, scope, category string, facts []Fact) (string, error)
}

// ConsolidationPlanner is an optional capability for Evaluators to scan active
// memories in a scope and propose cohesive groups of overlapping/redundant items.
type ConsolidationPlanner interface {
	SuggestConsolidations(ctx context.Context, scope string, facts []Fact) ([]ConsolidationProposal, error)
}

// ConfidenceCalibrator is an optional capability for Evaluators to assess individual
// semantic certainty [0.0, 1.0] for each active memory in a scope.
type ConfidenceCalibrator interface {
	CalibrateConfidence(ctx context.Context, scope string, facts []Fact) (map[int64]float64, error)
}

type GitCommitBrief struct {
	SHA     string   `json:"sha"`
	Date    string   `json:"date"`
	Subject string   `json:"subject"`
	Files   []string `json:"files,omitempty"`
}

type CommitProgressItem struct {
	SHA         string `json:"sha"`
	Track       string `json:"track,omitempty"`
	Category    string `json:"category"`
	SubCategory string `json:"sub_category,omitempty"`
	Summary     string `json:"summary"`
}

// ProgressCommitCategorizer is an optional capability for Evaluators to map Git commits
// into concise, user-language category progress entries during on-demand Progress Sync.
type ProgressCommitCategorizer interface {
	SummarizeCommitProgress(ctx context.Context, scope, lang string, commits []GitCommitBrief) ([]CommitProgressItem, error)
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

// TaxonomyPlanner is an optional capability for Evaluators to propose non-destructive
// category/sub_category re-organization and additive new taxonomy slugs without altering memory text.
type TaxonomyPlanner interface {
	SuggestTaxonomyRebalance(ctx context.Context, scope string, facts []Fact) ([]TaxonomyRebalanceItem, error)
}

// EvalTask identifies each discrete feature/workload in memorize so users can either apply
// a single bulk engine across all features or route individual features to specialized models
// (including mixing non-generative decision models like gemini-decision-1 / Flide with text-generation models).
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

// IsDecisionOnlySpec reports whether (provider, model) is a non-generative decision engine
// (0 output tokens) that cannot synthesize free-form text.
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

// TaskCapableEvaluator lets wrapper evaluators (such as BatchEvaluator) expose
// the underlying adapter's true per-task capabilities.
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

func SafeCwdScope() string {
	wd, err := os.Getwd()
	if err != nil {
		return ""
	}
	wd = filepath.Clean(wd)
	if wd == "/" || wd == "." {
		return ""
	}
	if home, err := os.UserHomeDir(); err == nil && filepath.Clean(home) == wd {
		return ""
	}
	base := strings.ToLower(strings.TrimSpace(filepath.Base(wd)))
	if base == "" {
		return ""
	}
	return "project:" + base
}

func DefaultScope() string {
	if env := strings.TrimSpace(os.Getenv("MEMORY_DEFAULT_SCOPE")); env != "" {
		return NormalizeScope(env)
	}
	if cwdScope := SafeCwdScope(); cwdScope != "" {
		return cwdScope
	}
	return "personal"
}

func NormalizeScope(raw string) string {
	s := strings.ToLower(strings.TrimSpace(raw))
	switch s {
	case "":
		return DefaultScope()
	case "personal", "me", "private", "개인":
		return "personal"
	case "global", "common", "shared", "전역":
		return "global"
	}
	if strings.HasPrefix(s, "project:") {
		name := strings.TrimSpace(strings.TrimPrefix(s, "project:"))
		if name == "" {
			return DefaultScope()
		}
		return "project:" + name
	}
	return "project:" + s
}

func NormalizeCategoryForScope(scope, raw string) string {
	k := strings.ToLower(strings.TrimSpace(raw))
	if idx := strings.Index(k, "/"); idx > 0 {
		k = strings.TrimSpace(k[:idx])
	}
	k = strings.ReplaceAll(k, " ", "_")
	if k != "" {
		return k
	}
	if NormalizeScope(scope) == "personal" {
		return "preference"
	}
	return "general"
}

func NormalizeSubCategory(raw string) string {
	k := strings.ToLower(strings.TrimSpace(raw))
	k = strings.Trim(k, "/")
	k = strings.ReplaceAll(k, " ", "_")
	return k
}

func SplitCategoryAndSub(scope, rawCat, rawSub string) (string, string) {
	c := strings.TrimSpace(rawCat)
	sub := strings.TrimSpace(rawSub)
	if idx := strings.Index(c, "/"); idx > 0 {
		if sub == "" {
			sub = c[idx+1:]
		}
		c = c[:idx]
	}
	return NormalizeCategoryForScope(scope, c), NormalizeSubCategory(sub)
}

func NormalizeTags(_ string, raw []string) []string {
	seen := make(map[string]bool, len(raw))
	var out []string
	for _, t := range raw {
		k := strings.ToLower(strings.TrimSpace(t))
		k = strings.ReplaceAll(k, " ", "_")
		if k != "" && !seen[k] {
			seen[k] = true
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}
