package adapters

import (
	"context"
	"errors"
	"os"
	"strconv"
	"sync"

	"golang.org/x/sync/errgroup"

	"memorize/internal/memory"
)

func envInt(key string, fallback int) int {
	if v, err := strconv.Atoi(os.Getenv(key)); err == nil && v > 0 {
		return v
	}
	return fallback
}

// BatchEvaluator wraps any Evaluator to perform 100% exhaustive evaluation
// across arbitrarily many active memories via bounded parallel chunks.
type BatchEvaluator struct {
	name        string
	base        memory.Evaluator
	batchSize   int
	concurrency int
}

func NewBatchEvaluator(name string, base memory.Evaluator, defaultBatch, defaultConc int) *BatchEvaluator {
	return &BatchEvaluator{
		name:        name,
		base:        base,
		batchSize:   envInt("MEMORY_BATCH_SIZE", defaultBatch),
		concurrency: envInt("MEMORY_CONCURRENCY", defaultConc),
	}
}

func (b *BatchEvaluator) Name() string { return b.name }

// SupportsTask reports the true per-task capabilities of the underlying base evaluator.
func (b *BatchEvaluator) SupportsTask(task memory.EvalTask) bool {
	return memory.EvaluatorSupportsTask(b.base, task)
}

func (b *BatchEvaluator) chunkFacts(facts []memory.Fact) [][]memory.Fact {
	var batches [][]memory.Fact
	for i := 0; i < len(facts); i += b.batchSize {
		end := min(i+b.batchSize, len(facts))
		batches = append(batches, facts[i:end])
	}
	return batches
}

func (b *BatchEvaluator) ScoreRelevance(ctx context.Context, currentTask string, facts []memory.Fact) ([]memory.ScoredFact, error) {
	if len(facts) == 0 {
		return nil, nil
	}
	if len(facts) <= b.batchSize {
		return b.base.ScoreRelevance(ctx, currentTask, facts)
	}
	batches := b.chunkFacts(facts)
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(b.concurrency)

	var mu sync.Mutex
	out := make([]memory.ScoredFact, 0, len(facts))
	for _, batch := range batches {
		chunk := batch
		g.Go(func() error {
			part, err := b.base.ScoreRelevance(gctx, currentTask, chunk)
			if err != nil {
				return err
			}
			mu.Lock()
			out = append(out, part...)
			mu.Unlock()
			return nil
		})
	}
	return out, g.Wait()
}

func decisionIDs(d memory.MutationDecision) []int64 {
	if len(d.ConflictFactIDs) > 0 {
		return d.ConflictFactIDs
	}
	if d.ConflictFactID > 0 {
		return []int64{d.ConflictFactID}
	}
	return nil
}

func (b *BatchEvaluator) EvaluateMutation(ctx context.Context, scope, candidate string, existing []memory.Fact) (memory.MutationDecision, error) {
	if len(existing) <= b.batchSize {
		return b.base.EvaluateMutation(ctx, scope, candidate, existing)
	}
	batches := b.chunkFacts(existing)
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(b.concurrency)

	partials := make([]memory.MutationDecision, len(batches))
	for i, batch := range batches {
		idx, chunk := i, batch
		g.Go(func() error {
			d, err := b.base.EvaluateMutation(gctx, scope, candidate, chunk)
			if err != nil {
				return err
			}
			partials[idx] = d
			return nil
		})
	}
	if err := g.Wait(); err != nil {
		return memory.MutationDecision{}, err
	}

	merged := partials[0]
	merged.Action = "INSERT"
	merged.ConflictFactID = 0
	merged.ConflictFactIDs = nil

	var overwrites, duplicates []int64
	var bestOverwriteConf, bestDupConf float64
	for _, p := range partials {
		ids := decisionIDs(p)
		switch p.Action {
		case "IGNORE":
			if len(ids) > 0 {
				duplicates = append(duplicates, ids...)
				if p.Confidence > bestDupConf {
					bestDupConf = p.Confidence
				}
			}
		case "OVERWRITE":
			if len(ids) > 0 {
				overwrites = append(overwrites, ids...)
				if p.Confidence >= bestOverwriteConf {
					bestOverwriteConf = p.Confidence
					merged.ConflictFactID = ids[0]
				}
			}
		}
	}
	if len(duplicates) > 0 {
		merged.Action = "IGNORE"
		merged.ConflictFactID = duplicates[0]
		merged.ConflictFactIDs = duplicates
		merged.Confidence = bestDupConf
	} else if len(overwrites) > 0 {
		merged.Action = "OVERWRITE"
		merged.ConflictFactIDs = overwrites
		merged.Confidence = bestOverwriteConf
	}
	return merged, nil
}

func (b *BatchEvaluator) VerifyPlan(ctx context.Context, proposedPlan string, rules []memory.Fact) ([]memory.PlanViolation, error) {
	if len(rules) == 0 {
		return nil, nil
	}
	if len(rules) <= b.batchSize {
		return b.base.VerifyPlan(ctx, proposedPlan, rules)
	}
	batches := b.chunkFacts(rules)
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(b.concurrency)

	var mu sync.Mutex
	var out []memory.PlanViolation
	for _, batch := range batches {
		chunk := batch
		g.Go(func() error {
			part, err := b.base.VerifyPlan(gctx, proposedPlan, chunk)
			if err != nil {
				return err
			}
			mu.Lock()
			out = append(out, part...)
			mu.Unlock()
			return nil
		})
	}
	return out, g.Wait()
}

func (b *BatchEvaluator) ConsolidateFacts(ctx context.Context, scope, category string, facts []memory.Fact) (string, error) {
	if c, ok := b.base.(memory.Consolidator); ok {
		return c.ConsolidateFacts(ctx, scope, category, facts)
	}
	return "", nil
}

func (b *BatchEvaluator) SuggestConsolidations(ctx context.Context, scope string, facts []memory.Fact) ([]memory.ConsolidationProposal, error) {
	p, ok := b.base.(memory.ConsolidationPlanner)
	if !ok || len(facts) < 2 {
		return nil, nil
	}
	const maxPlanBatch = 60
	if len(facts) <= maxPlanBatch {
		return p.SuggestConsolidations(ctx, scope, facts)
	}
	// Group by category first so related memories stay together within chunks <= maxPlanBatch.
	byCat := make(map[string][]memory.Fact)
	var catOrder []string
	for _, f := range facts {
		if _, seen := byCat[f.Category]; !seen {
			catOrder = append(catOrder, f.Category)
		}
		byCat[f.Category] = append(byCat[f.Category], f)
	}
	var chunks [][]memory.Fact
	var cur []memory.Fact
	for _, c := range catOrder {
		list := byCat[c]
		for len(list) > maxPlanBatch {
			if len(cur) > 0 {
				chunks = append(chunks, cur)
				cur = nil
			}
			chunks = append(chunks, list[:maxPlanBatch])
			list = list[maxPlanBatch:]
		}
		if len(cur)+len(list) > maxPlanBatch && len(cur) >= 2 {
			chunks = append(chunks, cur)
			cur = nil
		}
		cur = append(cur, list...)
	}
	if len(cur) >= 2 {
		chunks = append(chunks, cur)
	}

	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(b.concurrency)
	var mu sync.Mutex
	var out []memory.ConsolidationProposal
	for _, ch := range chunks {
		chunk := ch
		g.Go(func() error {
			part, err := p.SuggestConsolidations(gctx, scope, chunk)
			if err != nil {
				return err
			}
			mu.Lock()
			out = append(out, part...)
			mu.Unlock()
			return nil
		})
	}
	return out, g.Wait()
}

func (b *BatchEvaluator) CalibrateConfidence(ctx context.Context, scope string, facts []memory.Fact) (map[int64]float64, error) {
	cal, ok := b.base.(memory.ConfidenceCalibrator)
	if !ok {
		return nil, errors.New("active evaluator does not support confidence calibration")
	}
	if len(facts) <= b.batchSize {
		return cal.CalibrateConfidence(ctx, scope, facts)
	}
	chunks := b.chunkFacts(facts)
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(b.concurrency)
	var mu sync.Mutex
	out := make(map[int64]float64, len(facts))
	for _, ch := range chunks {
		chunk := ch
		g.Go(func() error {
			part, err := cal.CalibrateConfidence(gctx, scope, chunk)
			if err != nil {
				return err
			}
			mu.Lock()
			for k, v := range part {
				out[k] = v
			}
			mu.Unlock()
			return nil
		})
	}
	return out, g.Wait()
}

func (b *BatchEvaluator) SummarizeCommitProgress(ctx context.Context, scope, lang string, commits []memory.GitCommitBrief) ([]memory.CommitProgressItem, error) {
	if catz, ok := b.base.(memory.ProgressCommitCategorizer); ok {
		return catz.SummarizeCommitProgress(ctx, scope, lang, commits)
	}
	return nil, nil
}

func (b *BatchEvaluator) SuggestTaxonomyRebalance(ctx context.Context, scope string, facts []memory.Fact) ([]memory.TaxonomyRebalanceItem, error) {
	tp, ok := b.base.(memory.TaxonomyPlanner)
	if !ok || len(facts) == 0 {
		return nil, nil
	}
	const maxTaxBatch = 60
	if len(facts) <= maxTaxBatch {
		return tp.SuggestTaxonomyRebalance(ctx, scope, facts)
	}
	var out []memory.TaxonomyRebalanceItem
	for i := 0; i < len(facts); i += maxTaxBatch {
		end := min(len(facts), i+maxTaxBatch)
		part, err := tp.SuggestTaxonomyRebalance(ctx, scope, facts[i:end])
		if err != nil {
			return nil, err
		}
		out = append(out, part...)
	}
	return out, nil
}
