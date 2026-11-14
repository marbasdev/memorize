package guardrail

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	"memorize/internal/domain"
	"memorize/internal/inference/router"
	"memorize/internal/infra/eventbus"
	"memorize/internal/modules/memory"
	"memorize/internal/modules/progress"
)

// Service owns PreToolUse/pre-commit guardrails, explicit plan verification,
// audit logging, cold-start onboarding prompts, and guardrail log retention.
type Service struct {
	mem                *memory.Service
	prog               *progress.Service
	db                 *sql.DB
	dbPath             string
	router             *router.Router
	bus                *eventbus.Bus
	lastRetentionSweep atomic.Int64
}

// New constructs a guardrail Service layered over mem and an optional prog tracker.
func New(mem *memory.Service, prog *progress.Service) *Service {
	var (
		db     *sql.DB
		dbPath string
		rtr    *router.Router
		bus    *eventbus.Bus
	)
	if mem != nil {
		db = mem.DB()
		dbPath = mem.DBPath()
		rtr = mem.Router()
		bus = mem.Bus()
	}
	if bus == nil {
		bus = eventbus.New()
	}
	return &Service{
		mem:    mem,
		prog:   prog,
		db:     db,
		dbPath: dbPath,
		router: rtr,
		bus:    bus,
	}
}

// Memory returns the underlying memory Service.
func (s *Service) Memory() *memory.Service {
	return s.mem
}

// Progress returns the linked progress Service if attached.
func (s *Service) Progress() *progress.Service {
	return s.prog
}

// CheckEngineReadiness reports whether the active inference engine is ready to serve verification and hook calls.
func (s *Service) CheckEngineReadiness(ctx context.Context) (bool, string) {
	if s == nil || s.router == nil {
		return false, "no inference engine configured"
	}
	return s.router.CheckReadiness(ctx)
}

func (s *Service) notifyGuardrailsChange() {
	if s != nil && s.bus != nil {
		s.bus.Publish("guardrails")
	}
}

// filterEnforceableRules excludes soft unpinned preferences, which act as tie-breaker hints rather than blocking constraints.
func filterEnforceableRules(loaded []domain.Fact) []domain.Fact {
	var rules []domain.Fact
	for _, r := range loaded {
		if r.Category == "preference" && !r.Pinned {
			continue
		}
		rules = append(rules, r)
	}
	return rules
}

func filterAndSortViolations(violations []domain.PlanViolation, minProb float64) []domain.PlanViolation {
	var filtered []domain.PlanViolation
	for _, v := range violations {
		if minProb <= 0 || v.Probability >= minProb {
			filtered = append(filtered, v)
		}
	}
	sort.Slice(filtered, func(i, j int) bool {
		if filtered[i].Probability != filtered[j].Probability {
			return filtered[i].Probability > filtered[j].Probability
		}
		return filtered[i].ID < filtered[j].ID
	})
	return filtered
}

// EvaluatePlanViolations loads all active enforceable rules (excluding soft unpinned preferences),
// evaluates whether proposedPlan violates them, filters by minProb, and sorts descending by probability.
func (s *Service) EvaluatePlanViolations(ctx context.Context, proposedPlan, rawScope string, minProb float64) (string, int, []domain.PlanViolation, error) {
	scA, scB := memory.ScopesFor(rawScope, true)
	loaded, err := s.mem.LoadActiveFacts(ctx, scA, scB, "")
	if err != nil {
		return scA, 0, nil, err
	}
	rules := filterEnforceableRules(loaded)
	if len(rules) == 0 {
		return scA, 0, nil, nil
	}
	ev := s.router.EvalForTask(ctx, domain.TaskVerify)
	if ev == nil {
		return scA, len(rules), nil, fmt.Errorf("no active evaluation engine configured")
	}
	evalStart := time.Now()
	violations, err := ev.VerifyPlan(ctx, proposedPlan, rules)
	s.router.RecordTelemetry(ctx, domain.TaskVerify, ev, evalStart, len(rules), err)
	if err != nil {
		return scA, len(rules), nil, err
	}
	return scA, len(rules), filterAndSortViolations(violations, minProb), nil
}

// VerifyPlan evaluates proposedPlan against active scope+global enforceable rules and records an audit log entry.
func (s *Service) VerifyPlan(ctx context.Context, proposedPlan string, scope ...string) (string, error) {
	rawScope := ""
	if len(scope) > 0 {
		rawScope = scope[0]
	}
	t0 := time.Now()
	scA, ruleCount, violations, err := s.EvaluatePlanViolations(ctx, proposedPlan, rawScope, 0)
	if err != nil {
		return "", err
	}
	if ruleCount == 0 {
		return fmt.Sprintf("PASS: No active rules in scope [%s] to verify against.", scA), nil
	}
	elapsedMs := domain.ElapsedMsSince(t0)
	src := domain.VerifySourceFromContext(ctx)
	if len(violations) == 0 {
		_, _ = s.RecordGuardrailLog(ctx, domain.GuardrailLogInput{
			Scope:       scA,
			Source:      src,
			ToolName:    "verify_plan",
			Verdict:     domain.GuardrailVerdictPass,
			Blocked:     false,
			RuleCount:   ruleCount,
			PlanSnippet: proposedPlan,
			ElapsedMs:   elapsedMs,
		})
		return "PASS: Proposed plan complies with all active memories.", nil
	}
	vIDs, vDetails := formatViolationDetails(violations)
	_, _ = s.RecordGuardrailLog(ctx, domain.GuardrailLogInput{
		Scope:       scA,
		Source:      src,
		ToolName:    "verify_plan",
		Verdict:     domain.GuardrailVerdictPlanViolation,
		Blocked:     true,
		RuleCount:   ruleCount,
		ViolatedIDs: vIDs,
		PlanSnippet: proposedPlan,
		Details:     vDetails,
		ElapsedMs:   elapsedMs,
	})
	var sb strings.Builder
	sb.WriteString("**Plan Memory Violations Detected:**\n")
	for _, v := range violations {
		fmt.Fprintf(&sb, "- **[%s / %s #%d]** %s *(violation prob: %.0f%%, conf: %.2f)*\n",
			v.Scope, v.Category, v.ID, v.Content, v.Probability*100, v.Confidence)
	}
	return sb.String(), nil
}
