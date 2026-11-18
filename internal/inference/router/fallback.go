package router

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"memorize/internal/domain"
)

type cooldownTracker struct {
	mu    sync.RWMutex
	until map[string]time.Time
}

func newCooldownTracker() *cooldownTracker {
	return &cooldownTracker{
		until: make(map[string]time.Time),
	}
}

func (c *cooldownTracker) isCoolingDown(name string) bool {
	if c == nil || name == "" {
		return false
	}
	c.mu.RLock()
	exp, ok := c.until[name]
	c.mu.RUnlock()
	return ok && time.Now().Before(exp)
}

func (c *cooldownTracker) clear(name string) {
	if c == nil || name == "" {
		return
	}
	c.mu.Lock()
	delete(c.until, name)
	c.mu.Unlock()
}

func (c *cooldownTracker) markFailure(name string, err error) {
	if c == nil || name == "" || err == nil {
		return
	}
	msg := strings.ToLower(err.Error())
	d := 30 * time.Second
	switch {
	case strings.Contains(msg, "status 429") || strings.Contains(msg, "quota") || strings.Contains(msg, "rate"):
		d = 2 * time.Minute
	case strings.Contains(msg, "status 503") || strings.Contains(msg, "status 404") || strings.Contains(msg, "unreachable") || strings.Contains(msg, "connection refused"):
		d = 1 * time.Minute
	}
	c.mu.Lock()
	if c.until == nil {
		c.until = make(map[string]time.Time)
	}
	c.until[name] = time.Now().Add(d)
	c.mu.Unlock()
}

// fallbackEvaluator wraps a primary task evaluator with a circuit-breaker cooldown
// and automatic fallback to a secondary evaluator when the primary errors or is rate-limited.
type fallbackEvaluator struct {
	primary  domain.Evaluator
	fallback domain.Evaluator
	tracker  *cooldownTracker

	mu       sync.RWMutex
	usedName string
}

func newFallbackEvaluator(primary, fallback domain.Evaluator, tracker *cooldownTracker) domain.Evaluator {
	if primary == nil {
		return fallback
	}
	if fallback == nil || fallback.Name() == primary.Name() {
		return primary
	}
	return &fallbackEvaluator{
		primary:  primary,
		fallback: fallback,
		tracker:  tracker,
	}
}

func (f *fallbackEvaluator) setUsed(name string) {
	f.mu.Lock()
	f.usedName = name
	f.mu.Unlock()
}

func (f *fallbackEvaluator) Name() string {
	f.mu.RLock()
	used := f.usedName
	f.mu.RUnlock()
	if used != "" {
		return used
	}
	if f.primary != nil {
		return f.primary.Name()
	}
	if f.fallback != nil {
		return f.fallback.Name()
	}
	return "none"
}

func (f *fallbackEvaluator) SupportsTask(task domain.EvalTask) bool {
	return domain.EvaluatorSupportsTask(f.primary, task)
}

// InputTokenLimit returns the safe verified context token ceiling across active primary and fallback evaluators.
func (f *fallbackEvaluator) InputTokenLimit(ctx context.Context) int {
	if f == nil {
		return 0
	}
	if f.primary != nil && f.fallback != nil && f.tracker.isCoolingDown(f.primary.Name()) {
		return domain.EvaluatorInputTokenLimit(ctx, f.fallback)
	}
	pLim := domain.EvaluatorInputTokenLimit(ctx, f.primary)
	if f.fallback == nil || f.fallback.Name() == f.primary.Name() {
		return pLim
	}
	fLim := domain.EvaluatorInputTokenLimit(ctx, f.fallback)
	if pLim <= 0 || fLim <= 0 {
		return 0
	}
	return min(pLim, fLim)
}

// RuleBatchSize returns the conservative per-call rule chunk limit across primary and fallback evaluators.
func (f *fallbackEvaluator) RuleBatchSize() int {
	if f == nil {
		return 0
	}
	if f.primary != nil && f.fallback != nil && f.tracker.isCoolingDown(f.primary.Name()) {
		return domain.EvaluatorRuleBatchSize(f.fallback)
	}
	pBatch := domain.EvaluatorRuleBatchSize(f.primary)
	if f.fallback == nil || f.fallback.Name() == f.primary.Name() {
		return pBatch
	}
	fBatch := domain.EvaluatorRuleBatchSize(f.fallback)
	if pBatch <= 0 {
		return fBatch
	}
	if fBatch > 0 && fBatch > pBatch {
		return fBatch
	}
	return pBatch
}

func runWithFallback[T any](
	ctx context.Context,
	f *fallbackEvaluator,
	task domain.EvalTask,
	call func(ev domain.Evaluator) (T, error),
) (T, error) {
	primaryOK := f.primary != nil && domain.EvaluatorSupportsTask(f.primary, task)
	fallbackOK := f.fallback != nil && domain.EvaluatorSupportsTask(f.fallback, task) &&
		(f.primary == nil || f.fallback.Name() != f.primary.Name())

	if primaryOK && (!fallbackOK || !f.tracker.isCoolingDown(f.primary.Name())) {
		f.setUsed(f.primary.Name())
		res, err := call(f.primary)
		if err == nil {
			f.tracker.clear(f.primary.Name())
			return res, nil
		}
		if ctx == nil || ctx.Err() == nil {
			f.tracker.markFailure(f.primary.Name(), err)
		}
		if !fallbackOK || (ctx != nil && ctx.Err() != nil) {
			return res, err
		}
	}

	if fallbackOK {
		f.setUsed(f.fallback.Name())
		return call(f.fallback)
	}

	var zero T
	return zero, fmt.Errorf("evaluator %s does not support task %s", f.Name(), task)
}

func (f *fallbackEvaluator) ScoreRelevance(ctx context.Context, currentTask string, facts []domain.Fact) ([]domain.ScoredFact, error) {
	return runWithFallback(ctx, f, domain.TaskRelevance, func(ev domain.Evaluator) ([]domain.ScoredFact, error) {
		return ev.ScoreRelevance(ctx, currentTask, facts)
	})
}

func (f *fallbackEvaluator) EvaluateMutation(ctx context.Context, scope, candidate string, existing []domain.Fact) (domain.MutationDecision, error) {
	return runWithFallback(ctx, f, domain.TaskMutation, func(ev domain.Evaluator) (domain.MutationDecision, error) {
		return ev.EvaluateMutation(ctx, scope, candidate, existing)
	})
}

func (f *fallbackEvaluator) EvaluateMutations(ctx context.Context, scope string, candidates []string, existing []domain.Fact) ([]domain.MutationDecision, error) {
	return runWithFallback(ctx, f, domain.TaskMutation, func(ev domain.Evaluator) ([]domain.MutationDecision, error) {
		if bm, ok := ev.(domain.BatchMutationEvaluator); ok {
			return bm.EvaluateMutations(ctx, scope, candidates, existing)
		}
		out := make([]domain.MutationDecision, len(candidates))
		for i, c := range candidates {
			dec, err := ev.EvaluateMutation(ctx, scope, c, existing)
			if err != nil {
				return nil, err
			}
			out[i] = dec
		}
		return out, nil
	})
}

func (f *fallbackEvaluator) VerifyPlan(ctx context.Context, proposedPlan string, rules []domain.Fact) ([]domain.PlanViolation, error) {
	return runWithFallback(ctx, f, domain.TaskVerify, func(ev domain.Evaluator) ([]domain.PlanViolation, error) {
		return ev.VerifyPlan(ctx, proposedPlan, rules)
	})
}

func (f *fallbackEvaluator) CalibrateConfidence(ctx context.Context, scope string, facts []domain.Fact) (map[int64]float64, error) {
	return runWithFallback(ctx, f, domain.TaskCalibrate, func(ev domain.Evaluator) (map[int64]float64, error) {
		c, ok := ev.(domain.ConfidenceCalibrator)
		if !ok {
			return nil, fmt.Errorf("evaluator %s does not support confidence calibration", ev.Name())
		}
		return c.CalibrateConfidence(ctx, scope, facts)
	})
}

func (f *fallbackEvaluator) ConsolidateFacts(ctx context.Context, scope, category string, facts []domain.Fact) (string, error) {
	return runWithFallback(ctx, f, domain.TaskConsolidate, func(ev domain.Evaluator) (string, error) {
		c, ok := ev.(domain.Consolidator)
		if !ok {
			return "", fmt.Errorf("evaluator %s does not support consolidation", ev.Name())
		}
		return c.ConsolidateFacts(ctx, scope, category, facts)
	})
}

func (f *fallbackEvaluator) SuggestConsolidations(ctx context.Context, scope string, facts []domain.Fact) ([]domain.ConsolidationProposal, error) {
	return runWithFallback(ctx, f, domain.TaskConsolidate, func(ev domain.Evaluator) ([]domain.ConsolidationProposal, error) {
		p, ok := ev.(domain.ConsolidationPlanner)
		if !ok {
			return nil, fmt.Errorf("evaluator %s does not support consolidation planning", ev.Name())
		}
		return p.SuggestConsolidations(ctx, scope, facts)
	})
}

func (f *fallbackEvaluator) SummarizeCommitProgress(ctx context.Context, scope, lang string, commits []domain.GitCommitBrief) ([]domain.CommitProgressItem, error) {
	return runWithFallback(ctx, f, domain.TaskProgress, func(ev domain.Evaluator) ([]domain.CommitProgressItem, error) {
		p, ok := ev.(domain.ProgressCommitCategorizer)
		if !ok {
			return nil, fmt.Errorf("evaluator %s does not support progress categorization", ev.Name())
		}
		return p.SummarizeCommitProgress(ctx, scope, lang, commits)
	})
}

func (f *fallbackEvaluator) SummarizeCommitMilestones(ctx context.Context, scope, lang string, commits []domain.GitCommitBrief) ([]domain.GitMilestoneSummary, error) {
	return runWithFallback(ctx, f, domain.TaskProgress, func(ev domain.Evaluator) ([]domain.GitMilestoneSummary, error) {
		if p, ok := ev.(domain.ProgressMilestoneSummarizer); ok {
			return p.SummarizeCommitMilestones(ctx, scope, lang, commits)
		}
		if _, ok := ev.(domain.ProgressCommitCategorizer); ok {
			return nil, nil
		}
		return nil, fmt.Errorf("evaluator %s does not support progress milestones", ev.Name())
	})
}

func (f *fallbackEvaluator) SuggestTaxonomyRebalance(ctx context.Context, scope string, facts []domain.Fact) ([]domain.TaxonomyRebalanceItem, error) {
	return runWithFallback(ctx, f, domain.TaskTaxonomy, func(ev domain.Evaluator) ([]domain.TaxonomyRebalanceItem, error) {
		p, ok := ev.(domain.TaxonomyPlanner)
		if !ok {
			return nil, fmt.Errorf("evaluator %s does not support taxonomy rebalance", ev.Name())
		}
		return p.SuggestTaxonomyRebalance(ctx, scope, facts)
	})
}

func (f *fallbackEvaluator) DraftRulePackProposal(ctx context.Context, scope, rawRuleText, sourceCat, sourceSub string, outline []domain.RuleOutlineEntry) (domain.RulePlacementProposal, error) {
	return runWithFallback(ctx, f, domain.TaskConsolidate, func(ev domain.Evaluator) (domain.RulePlacementProposal, error) {
		p, ok := ev.(domain.RulePackPlanner)
		if !ok {
			return domain.RulePlacementProposal{}, fmt.Errorf("evaluator %s does not support rule pack planning", ev.Name())
		}
		return p.DraftRulePackProposal(ctx, scope, rawRuleText, sourceCat, sourceSub, outline)
	})
}

func (f *fallbackEvaluator) FindRedundantLocalMemories(ctx context.Context, scope string, teamRules []domain.TeamRule, localFacts []domain.Fact) ([]domain.RuleDuplicateSuggestion, error) {
	return runWithFallback(ctx, f, domain.TaskConsolidate, func(ev domain.Evaluator) ([]domain.RuleDuplicateSuggestion, error) {
		p, ok := ev.(domain.RulePackPlanner)
		if !ok {
			return nil, fmt.Errorf("evaluator %s does not support rule pack duplicate analysis", ev.Name())
		}
		return p.FindRedundantLocalMemories(ctx, scope, teamRules, localFacts)
	})
}
