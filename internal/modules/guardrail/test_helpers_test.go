package guardrail_test

import (
	"context"
	"testing"

	"memorize/internal/domain"
	"memorize/internal/modules/guardrail"
	"memorize/internal/modules/memory"
	"memorize/internal/modules/progress"
)

type testStack struct {
	mem  *memory.Service
	prog *progress.Service
	gr   *guardrail.Service
}

func newTestStack(t *testing.T, dbPath string, eval domain.Evaluator) *testStack {
	t.Helper()
	if dbPath == "" {
		dbPath = ":memory:"
	}
	mem, err := memory.Open(dbPath, eval)
	if err != nil {
		t.Fatalf("memory.Open(%q): %v", dbPath, err)
	}
	t.Cleanup(func() { _ = mem.Close() })
	prog := progress.New(mem)
	gr := guardrail.New(mem, prog)
	return &testStack{mem: mem, prog: prog, gr: gr}
}

type stubEval struct {
	dec        domain.MutationDecision
	violations []domain.PlanViolation
}

func (s *stubEval) Name() string { return "stub" }

func (s *stubEval) ScoreRelevance(_ context.Context, _ string, facts []domain.Fact) ([]domain.ScoredFact, error) {
	out := make([]domain.ScoredFact, 0, len(facts))
	for _, f := range facts {
		out = append(out, domain.ScoredFact{Fact: f, Score: 4.5, Confidence: 0.9})
	}
	return out, nil
}

func (s *stubEval) EvaluateMutation(_ context.Context, scope, _ string, _ []domain.Fact) (domain.MutationDecision, error) {
	return s.dec, nil
}

func (s *stubEval) VerifyPlan(_ context.Context, _ string, _ []domain.Fact) ([]domain.PlanViolation, error) {
	return s.violations, nil
}

type progressSyncEval struct {
	stubEval
}

func (p *progressSyncEval) SummarizeCommitProgress(_ context.Context, _ string, _ string, commits []domain.GitCommitBrief) ([]domain.CommitProgressItem, error) {
	out := make([]domain.CommitProgressItem, 0, len(commits))
	for _, c := range commits {
		out = append(out, domain.CommitProgressItem{
			SHA:         c.SHA,
			Category:    "architecture",
			SubCategory: "dashboard",
			Summary:     "대시보드 진행상황 동기화 구현: " + c.Subject,
		})
	}
	return out, nil
}

func (p *progressSyncEval) SuggestConsolidations(_ context.Context, _ string, facts []domain.Fact) ([]domain.ConsolidationProposal, error) {
	if len(facts) < 2 {
		return nil, nil
	}
	ids := []int64{facts[0].ID, facts[1].ID}
	return []domain.ConsolidationProposal{
		{
			SourceIDs:     ids,
			Category:      facts[0].Category,
			MergedContent: "통합된 카테고리 규칙 요약",
			Reason:        "동일 카테고리 중복 통합",
		},
	}, nil
}

func (p *progressSyncEval) SuggestTaxonomyRebalance(_ context.Context, _ string, facts []domain.Fact) ([]domain.TaxonomyRebalanceItem, error) {
	var out []domain.TaxonomyRebalanceItem
	for _, f := range facts {
		if f.SubCategory == "" {
			out = append(out, domain.TaxonomyRebalanceItem{
				ID:             f.ID,
				NewCategory:    f.Category,
				NewSubCategory: "observability",
				Reason:         "미분류 항목 의미 기반 세부 분류 배정",
			})
		}
	}
	return out, nil
}
