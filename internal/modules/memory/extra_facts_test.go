package memory_test

import (
	"context"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"memorize/internal/domain"
	"memorize/internal/modules/memory"
)

type stubExtraFactsProvider struct {
	mu    sync.Mutex
	facts []domain.Fact
}

func (p *stubExtraFactsProvider) LoadExtraFacts(_ context.Context, scopeA, scopeB, _ string) ([]domain.Fact, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	var out []domain.Fact
	for _, f := range p.facts {
		if f.Scope == scopeA || f.Scope == scopeB {
			out = append(out, f)
		}
	}
	return out, nil
}

func TestExtraFactsMergedReadOnlyAndSessionInvalidation(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "mem.db")
	ev := &stubEval{}
	svc, err := memory.Open(dbPath, ev)
	if err != nil {
		t.Fatalf("open memory service: %v", err)
	}
	defer svc.Close()

	provider := &stubExtraFactsProvider{
		facts: []domain.Fact{
			{
				ID:       1000000001,
				Scope:    "project:app",
				Origin:   domain.RuleOriginTeam,
				RuleKey:  "db-no-cgo",
				Category: "database",
				Content:  "Never enable CGO — Why: Static builds | How: CGO_ENABLED=0",
				Pinned:   true,
			},
			{
				ID:       1000000002,
				Scope:    "project:app",
				Origin:   domain.RuleOriginTeam,
				RuleKey:  "api-json-err",
				Category: "api",
				Content:  "Always return structured JSON errors",
				Pinned:   false,
			},
		},
	}
	svc.AttachExtraFactsProvider(provider)

	ctx := context.Background()
	out1, err := svc.QuerySessionMemory(ctx, "sess-1", "implement error response", "", "project:app", false)
	if err != nil {
		t.Fatalf("first QuerySessionMemory: %v", err)
	}
	if !strings.Contains(out1, "[TEAM project:app/database]") || !strings.Contains(out1, "Never enable CGO") {
		t.Fatalf("expected team rule rendered with TEAM prefix, got:\n%s", out1)
	}
	if !strings.Contains(out1, "[TEAM project:app/api]") {
		t.Fatalf("expected second team rule rendered with TEAM prefix, got:\n%s", out1)
	}

	// Second turn in same session must deduplicate via session_sent
	out2, err := svc.QuerySessionMemory(ctx, "sess-1", "implement error response", "", "project:app", false)
	if err != nil {
		t.Fatalf("second QuerySessionMemory: %v", err)
	}
	if !strings.Contains(out2, "No new memories") {
		t.Fatalf("expected already-delivered team rules to be deduplicated on turn 2, got:\n%s", out2)
	}

	// Updating and invalidating a Team Rule ID must re-deliver it cleanly on the next turn
	provider.mu.Lock()
	provider.facts[0].Content = "Updated Team Rule Content"
	provider.mu.Unlock()
	svc.InvalidateSessionIDs(ctx, 1000000001)

	out3, err := svc.QuerySessionMemory(ctx, "sess-1", "next step", "", "project:app", false)
	if err != nil {
		t.Fatalf("third QuerySessionMemory: %v", err)
	}
	if !strings.Contains(out3, "Updated Team Rule Content") {
		t.Fatalf("expected invalidated team rule to re-deliver with updated content, got:\n%s", out3)
	}

	// CommitMemoryFull MUST NOT expose Team Rules to mutation/overwrite evaluation
	ev.dec = domain.MutationDecision{Action: "INSERT", Confidence: 0.9}
	_, err = svc.CommitMemoryFull(ctx, "Always use parameterized queries", "database", "", "", "project:app")
	if err != nil {
		t.Fatalf("CommitMemoryFull: %v", err)
	}
	ev.mu.Lock()
	seenInMutation := append([]domain.Fact(nil), ev.lastFacts...)
	ev.mu.Unlock()
	for _, f := range seenInMutation {
		if f.IsTeam() || f.ID >= 1000000000 {
			t.Fatalf("CommitMemoryFull leaked team rule into mutation candidates: %+v", f)
		}
	}
}
