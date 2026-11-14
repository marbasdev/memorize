package memory_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"memorize/internal/domain"
	"memorize/internal/modules/memory"
)

type distStubEval struct {
	dec domain.MutationDecision
}

func (s *distStubEval) Name() string { return "dist-stub" }

func (s *distStubEval) ScoreRelevance(_ context.Context, _ string, facts []domain.Fact) ([]domain.ScoredFact, error) {
	out := make([]domain.ScoredFact, 0, len(facts))
	for _, f := range facts {
		out = append(out, domain.ScoredFact{Fact: f, Score: 4.5, Confidence: 0.9})
	}
	return out, nil
}

func (s *distStubEval) EvaluateMutation(_ context.Context, _, _ string, _ []domain.Fact) (domain.MutationDecision, error) {
	return s.dec, nil
}

func (s *distStubEval) VerifyPlan(_ context.Context, _ string, _ []domain.Fact) ([]domain.PlanViolation, error) {
	return nil, nil
}

func (s *distStubEval) SuggestConsolidations(_ context.Context, _ string, facts []domain.Fact) ([]domain.ConsolidationProposal, error) {
	if len(facts) < 2 {
		return nil, nil
	}
	return []domain.ConsolidationProposal{
		{
			SourceIDs:     []int64{facts[0].ID, facts[1].ID},
			Category:      facts[0].Category,
			MergedContent: "통합된 카테고리 규칙 요약",
			Reason:        "동일 카테고리 중복 통합",
		},
	}, nil
}

func (s *distStubEval) SuggestTaxonomyRebalance(_ context.Context, _ string, facts []domain.Fact) ([]domain.TaxonomyRebalanceItem, error) {
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

func TestCategoryDistributionAndScopeStats(t *testing.T) {
	ctx := context.Background()
	eval := &distStubEval{
		dec: domain.MutationDecision{
			IsPermanent:       0.95,
			IsGlobalInvariant: 0.90,
			Action:            "INSERT",
			Confidence:        0.93,
		},
	}

	svc, err := memory.Open(":memory:", eval)
	if err != nil {
		t.Fatalf("memory.Open: %v", err)
	}
	defer svc.Close()

	for i := 1; i <= 6; i++ {
		sub := "core"
		if i == 5 {
			sub = ""
		} else if i == 6 {
			sub = "api"
		}
		if _, err := svc.CommitMemoryFull(ctx, fmt.Sprintf("아키텍처 불변 설계 원칙 #%d", i), "architecture", sub, "", "project:dist"); err != nil {
			t.Fatalf("CommitMemoryFull arch %d: %v", i, err)
		}
	}

	for _, sub := range []string{"naming", "imports", "comments"} {
		if _, err := svc.CommitMemoryFull(ctx, "코드 작성 컨벤션 원칙 "+sub, "convention", sub, "", "project:dist"); err != nil {
			t.Fatalf("CommitMemoryFull convention/%s: %v", sub, err)
		}
	}

	if _, err := svc.CommitMemoryFull(ctx, "텔레메트리 API 수집 불변 원칙", "telemetry", "api", "", "project:dist"); err != nil {
		t.Fatalf("CommitMemoryFull telemetry/api: %v", err)
	}

	var tombIDs []int64
	for i := 1; i <= 4; i++ {
		if _, err := svc.CommitMemoryFull(ctx, fmt.Sprintf("단위 테스트 격리 불변 원칙 #%d", i), "testing", "unit", "", "project:dist"); err != nil {
			t.Fatalf("CommitMemoryFull testing %d: %v", i, err)
		}
		if i <= 3 {
			recs, _ := svc.ListRecords(ctx, "project:dist")
			tombIDs = append(tombIDs, recs[0].ID)
		}
	}
	if _, err := svc.ForgetMemories(domain.WithAdminBypass(ctx), tombIDs); err != nil {
		t.Fatalf("ForgetMemories: %v", err)
	}

	for i := 1; i <= 2; i++ {
		if _, err := svc.CommitMemoryFull(ctx, fmt.Sprintf("타 프로젝트 스키마 불변 원칙 #%d", i), "database", "schema", "", "project:other"); err != nil {
			t.Fatalf("CommitMemoryFull other %d: %v", i, err)
		}
	}

	distStats, err := svc.GetDashboardStatsForScope(ctx, "project:dist")
	if err != nil {
		t.Fatalf("GetDashboardStatsForScope(project:dist): %v", err)
	}
	if distStats.ActiveCount != 11 || distStats.TombCount != 3 || distStats.TotalCount != 14 {
		t.Fatalf("expected project:dist stats active=11 tomb=3 total=14, got %+v", distStats)
	}

	otherStats, err := svc.GetDashboardStatsForScope(ctx, "project:other")
	if err != nil || otherStats.ActiveCount != 2 || otherStats.TotalCount != 2 {
		t.Fatalf("expected project:other stats active=2 total=2, got %+v (%v)", otherStats, err)
	}

	globalStats, err := svc.GetDashboardStatsForScope(ctx, "")
	if err != nil || globalStats.ActiveCount != 13 || globalStats.TombCount != 3 {
		t.Fatalf("expected global stats active=13 tomb=3, got %+v (%v)", globalStats, err)
	}

	dist, err := svc.GetCategoryDistribution(ctx, "project:dist")
	if err != nil {
		t.Fatalf("GetCategoryDistribution: %v", err)
	}
	if dist.TotalActive != 11 || dist.TotalTomb != 3 || dist.UncategorizedTotal != 1 {
		t.Fatalf("unexpected distribution totals: %+v", dist)
	}
	if len(dist.DuplicateSubCategories) != 1 || dist.DuplicateSubCategories[0] != "api" {
		t.Fatalf("expected duplicate sub-category ['api'], got %v", dist.DuplicateSubCategories)
	}

	byCat := map[string]domain.CategoryDistItem{}
	totalCatAreaPct := 0.0
	for _, c := range dist.Categories {
		if c.W <= 0 || c.H <= 0 {
			t.Fatalf("expected 2D treemap W>0 and H>0 for category %s, got %+v", c.Category, c)
		}
		totalCatAreaPct += (c.W * c.H) / 100.0
		byCat[c.Category] = c
	}
	if totalCatAreaPct < 99.0 || totalCatAreaPct > 101.0 {
		t.Fatalf("expected 2D category treemap areas to partition 100%% of fixed canvas, got %.2f%%", totalCatAreaPct)
	}
	archDist := byCat["architecture"]
	convDist := byCat["convention"]
	if archDist.ActiveCount != 6 || archDist.UncategorizedCount != 1 || archDist.FlexWeight != 6 {
		t.Fatalf("expected architecture active=6 uncat=1 flex=6, got %+v", archDist)
	}
	if archDist.W*archDist.H <= convDist.W*convDist.H {
		t.Fatalf("expected architecture 2D area (%.1f) > convention 2D area (%.1f)", archDist.W*archDist.H, convDist.W*convDist.H)
	}
	foundUncatTile := false
	subAreaPct := 0.0
	for _, s := range archDist.SubItems {
		if s.W <= 0 || s.H <= 0 {
			t.Fatalf("expected 2D treemap W>0 and H>0 for sub-item %s, got %+v", s.SubCategory, s)
		}
		subAreaPct += (s.W * s.H) / 100.0
		if s.IsUncategorized && s.SubCategory == domain.UncategorizedSubKey && s.ActiveCount == 1 {
			foundUncatTile = true
		}
	}
	if !foundUncatTile || subAreaPct < 99.0 || subAreaPct > 101.0 {
		t.Fatalf("expected unclassified sub-tile (__none__) and 100%% sub-area partition (got %.2f%%): %+v", subAreaPct, archDist.SubItems)
	}

	uncatPage, err := svc.QueryRecordsPage(ctx, domain.RecordQuery{
		Scope:       "project:dist",
		Category:    "architecture",
		SubCategory: domain.UncategorizedSubKey,
		State:       "active",
		Page:        1,
		PageSize:    20,
	})
	if err != nil || len(uncatPage.Records) != 1 || uncatPage.Records[0].SubCategory != "" {
		t.Fatalf("expected 1 unclassified architecture record for sub_category=__none__, got %+v (%v)", uncatPage.Records, err)
	}
	origUncat := uncatPage.Records[0]

	var savedTax map[string][]string
	if err := json.Unmarshal([]byte(svc.GetConfigMap(ctx)["preset_categories"]), &savedTax); err != nil {
		t.Fatalf("unmarshal preset_categories: %v", err)
	}
	if len(savedTax["architecture"]) == 0 || len(savedTax["security"]) == 0 {
		t.Fatalf("expected existing default preset categories to remain intact (zero deletion), got %+v", savedTax)
	}
	if len(savedTax["telemetry"]) != 1 || savedTax["telemetry"][0] != "api" {
		t.Fatalf("expected new category telemetry/api appended to preset_categories, got %+v", savedTax["telemetry"])
	}

	rebItems, _, err := svc.SuggestTaxonomyRebalance(domain.WithAdminBypass(ctx), "project:dist", "architecture")
	if err != nil || len(rebItems) != 1 || rebItems[0].ID != origUncat.ID || rebItems[0].NewSubCategory != "observability" || !rebItems[0].IsNewTaxonomy {
		t.Fatalf("expected 1 taxonomy rebalance suggestion for #%d -> architecture/observability, got %+v (%v)", origUncat.ID, rebItems, err)
	}
	appliedCnt, err := svc.ApplyTaxonomyRebalance(domain.WithAdminBypass(ctx), "project:dist", rebItems)
	if err != nil || appliedCnt != 1 {
		t.Fatalf("ApplyTaxonomyRebalance failed: count=%d err=%v", appliedCnt, err)
	}
	afterRecs, err := svc.GetRecordsByIDs(ctx, []int64{origUncat.ID})
	if err != nil || len(afterRecs) != 1 {
		t.Fatalf("GetRecordsByIDs after rebalance: %v", err)
	}
	if afterRecs[0].Content != origUncat.Content || afterRecs[0].Pinned != origUncat.Pinned || afterRecs[0].SubCategory != "observability" {
		t.Fatalf("expected non-destructive sub_category update only, before=%+v after=%+v", origUncat, afterRecs[0])
	}

	proposals, _, err := svc.SuggestConsolidationPlanFiltered(domain.WithAdminBypass(ctx), "project:dist", "convention")
	if err != nil || len(proposals) != 1 || !strings.HasPrefix(proposals[0].Category, "convention") {
		t.Fatalf("expected 1 convention-scoped proposal, got %+v (%v)", proposals, err)
	}
}

func TestUpdateMemoryFullSessionInlineNotice(t *testing.T) {
	ctx := context.Background()
	eval := &distStubEval{
		dec: domain.MutationDecision{
			Category:          "architecture",
			IsPermanent:       0.95,
			IsGlobalInvariant: 0.90,
			Action:            "INSERT",
			Confidence:        0.94,
		},
	}
	svc, err := memory.Open(":memory:", eval)
	if err != nil {
		t.Fatalf("memory.Open: %v", err)
	}
	defer svc.Close()

	if _, err := svc.CommitMemory(ctx, "API timeout defaults to 5s", "architecture", "project:edit"); err != nil {
		t.Fatalf("CommitMemory: %v", err)
	}

	t1, err := svc.QuerySessionMemory(ctx, "sess-edit", "configure HTTP client", "", "project:edit", false)
	if err != nil || !strings.Contains(t1, "API timeout defaults to 5s") {
		t.Fatalf("Turn 1 expected initial memory, got:\n%s (%v)", t1, err)
	}

	adminCtx := domain.WithAdminBypass(ctx)
	if err := svc.UpdateMemoryFull(adminCtx, 1, "project:edit", "architecture", nil, "API timeout defaults to 15s", nil); err != nil {
		t.Fatalf("UpdateMemoryFull 1: %v", err)
	}
	if err := svc.UpdateMemoryFull(adminCtx, 1, "project:edit", "architecture", nil, "API timeout defaults to 20s", nil); err != nil {
		t.Fatalf("UpdateMemoryFull 2: %v", err)
	}

	t2, err := svc.QuerySessionMemory(ctx, "sess-edit", "configure HTTP client", "", "project:edit", false)
	if err != nil {
		t.Fatalf("Turn 2 QuerySessionMemory: %v", err)
	}
	if !strings.Contains(t2, "API timeout defaults to 20s") || !strings.Contains(t2, "Updated #1 — replaces earlier text: API timeout defaults to 5s") {
		t.Fatalf("expected Turn 2 to include updated text + earliest prev_content notice, got:\n%s", t2)
	}

	t3, err := svc.QuerySessionMemory(ctx, "sess-edit", "configure HTTP client", "", "project:edit", false)
	if err != nil || !strings.HasPrefix(t3, "No new memories") {
		t.Fatalf("expected Turn 3 to be deduplicated after consuming update notice, got:\n%s", t3)
	}

	tFresh, err := svc.QuerySessionMemory(ctx, "sess-fresh", "configure HTTP client", "", "project:edit", false)
	if err != nil || !strings.Contains(tFresh, "API timeout defaults to 20s") || strings.Contains(tFresh, "Updated #1") {
		t.Fatalf("expected fresh session to receive clean text without Updated notice, got:\n%s", tFresh)
	}
}

func TestMultiAccountPersonalOwnershipAndAdminBypass(t *testing.T) {
	eval := &distStubEval{
		dec: domain.MutationDecision{
			Category:          "diet",
			Tier:              "invariant",
			IsPermanent:       0.95,
			IsGlobalInvariant: 0.92,
			Action:            "INSERT",
			Confidence:        0.94,
		},
	}
	svc, err := memory.Open(":memory:", eval)
	if err != nil {
		t.Fatalf("memory.Open: %v", err)
	}
	defer svc.Close()

	ctxAlice := domain.WithAccount(context.Background(), "alice")
	ctxBob := domain.WithAccount(context.Background(), "bob")

	if _, err := svc.CommitMemory(ctxAlice, "앨리스는 땅콩 알레르기가 있다", "diet", "personal"); err != nil {
		t.Fatalf("Alice commit: %v", err)
	}

	if _, err := svc.ForgetMemories(ctxBob, []int64{1}); err == nil {
		t.Fatalf("expected Bob ForgetMemories on Alice's personal memory to fail")
	}
	if err := svc.UpdateMemoryFull(ctxBob, 1, "", "", nil, "조작된 내용", nil); err == nil {
		t.Fatalf("expected Bob UpdateMemoryFull on Alice's personal memory to fail")
	}

	if err := svc.UpdateMemoryFull(ctxAlice, 1, "", "", nil, "앨리스는 중증 땅콩 알레르기가 있다", nil); err != nil {
		t.Fatalf("Alice UpdateMemoryFull failed: %v", err)
	}

	ctxAdmin := domain.WithAdminBypass(ctxBob)
	if err := svc.UpdateMemoryFull(ctxAdmin, 1, "", "", nil, "앨리스는 땅콩·호두 알레르기가 있다", nil); err != nil {
		t.Fatalf("Admin UpdateMemoryFull failed: %v", err)
	}

	recs, _ := svc.ListRecords(ctxAdmin, "personal")
	if len(recs) != 1 || recs[0].Account != "alice" || !recs[0].Pinned {
		t.Fatalf("expected account='alice' and Pinned=true preserved after admin edit, got %+v", recs)
	}
}

func TestAutoSyncRepoLargeCommitBatchBoundedExecution(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	ctx := context.Background()
	eval := &distStubEval{
		dec: domain.MutationDecision{
			Category:    "architecture",
			Tier:        "transient",
			IsPermanent: 0.20,
			Action:      "IGNORE",
			Confidence:  0.90,
		},
	}
	svc, err := memory.Open(":memory:", eval)
	if err != nil {
		t.Fatalf("memory.Open: %v", err)
	}
	defer svc.Close()
	svc.SetAutoGitSync(true)

	repo := t.TempDir()
	gitRun := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", repo}, args...)...)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v failed: %v\n%s", args, err, string(out))
		}
		return strings.TrimSpace(string(out))
	}
	writeAndCommit := func(rel, body, msg string) string {
		t.Helper()
		full := filepath.Join(repo, rel)
		_ = os.MkdirAll(filepath.Dir(full), 0o755)
		_ = os.WriteFile(full, []byte(body), 0o644)
		gitRun("add", ".")
		gitRun("commit", "-m", msg)
		return gitRun("rev-parse", "--short=12", "HEAD")
	}

	gitRun("init")
	gitRun("config", "user.email", "dev@example.com")
	gitRun("config", "user.name", "Dev")

	head0 := writeAndCommit("README.md", "# App\n", "chore: init")
	scope := "project:" + strings.ToLower(filepath.Base(repo))
	_ = svc.SetSyncedCommit(ctx, scope, head0)

	eval.dec = domain.MutationDecision{
		Category:    "architecture",
		Tier:        "rule",
		IsPermanent: 0.92,
		Action:      "INSERT",
		Confidence:  0.93,
	}
	_ = writeAndCommit("CONSTRAINTS.md", "# Constraints\n- All SQLite connections must enable WAL mode\n", "docs: add sqlite constraint")
	var latest string
	for i := 1; i <= 7; i++ {
		latest = writeAndCommit("main.go", fmt.Sprintf("package main // %d\n", i), fmt.Sprintf("chore: step %d", i))
	}

	msg := svc.AutoSyncRepo(ctx, "sess-batch", scope, repo, false)
	if svc.GetSyncedCommit(ctx, scope) != latest || !strings.Contains(msg, "8 commit(s)") {
		t.Fatalf("expected AutoSyncRepo to advance cursor to %s across 8 commits, got cursor=%q msg=%q", latest, svc.GetSyncedCommit(ctx, scope), msg)
	}
}
