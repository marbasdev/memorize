package providers

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"memorize/internal/domain"
)

func TestDraftRulePackProposalSanitizesAndValidatesAfterIndex(t *testing.T) {
	var seenPrompt string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		bodyBytes, _ := io.ReadAll(r.Body)
		seenPrompt = string(bodyBytes)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"candidates": [{
				"content": {
					"parts": [{
						"text": "{\"category\":\"database\",\"sub_category\":\"SQLite Driver\",\"title\":\"[invariant] Always compile with CGO_ENABLED=0\\nusing modernc.org/sqlite across all OS targets.\",\"after_index\":1}"
					}]
				}
			}]
		}`))
	}))
	defer srv.Close()

	t.Setenv("GEMINI_API_BASE", srv.URL)
	ev := NewGeminiAdapterConfig("test-key", "gemini-2.5-flash")
	planner, ok := any(ev).(domain.RulePackPlanner)
	if !ok {
		t.Fatalf("expected Gemini BatchEvaluator to implement domain.RulePackPlanner")
	}

	ctx := domain.WithPresetCategories(context.Background(), `{"database":["sqlite-driver"]}`)
	outline := []domain.RuleOutlineEntry{
		{RuleKey: "db-wal-mode", Category: "database", SubCategory: "sqlite-driver", Title: "Enable WAL mode."},
	}
	prop, err := planner.DraftRulePackProposal(ctx, "project:app", "CGO 없이 modernc.org/sqlite 사용", "database", "", outline)
	if err != nil {
		t.Fatalf("DraftRulePackProposal failed: %v", err)
	}
	if !strings.Contains(seenPrompt, "[1] (database/sqlite-driver) Enable WAL mode.") || strings.Contains(seenPrompt, "db-wal-mode") || !strings.Contains(seenPrompt, "CGO 없이") {
		t.Fatalf("expected indexed prompt without exposed keys, got: %s", seenPrompt)
	}
	wantKey := domain.ComputeRuleKey("database", prop.Title)
	if prop.RuleKey != wantKey || !strings.HasPrefix(prop.RuleKey, "r-") {
		t.Fatalf("unexpected hashed rule_key %q (want %q)", prop.RuleKey, wantKey)
	}
	if prop.AfterKey != "" {
		t.Fatalf("expected empty after_key when appending to category end, got %q", prop.AfterKey)
	}
	if strings.Contains(prop.Title, "`[invariant]`") || strings.Contains(prop.Title, "\n") {
		t.Fatalf("expected title sanitized to single line without tier prefix, got %q", prop.Title)
	}
}

func TestFindRedundantLocalMemoriesFiltersAndShortens(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		if n == 1 {
			longTrimmed := strings.Repeat("한국어고유규칙세부사항", 20) // > 180 runes
			payload, _ := json.Marshal(map[string]any{
				"items": []map[string]any{
					{"local_memory_id": 10, "team_rule_index": 1, "kind": "duplicate", "trimmed_content": "should be ignored", "reason": "팀 룰과 동일"},
					{"local_memory_id": 11, "team_rule_index": 2, "kind": "partial_overlap", "trimmed_content": longTrimmed, "reason": "로컬 디버그 포트 정보만 고유"},
					{"local_memory_id": 11, "team_rule_index": 2, "kind": "duplicate", "trimmed_content": "", "reason": "duplicate local id ignored"},
					{"local_memory_id": 999, "team_rule_index": 1, "kind": "duplicate", "trimmed_content": "", "reason": "unknown local id"},
					{"local_memory_id": 12, "team_rule_index": 99, "kind": "duplicate", "trimmed_content": "", "reason": "unknown team rule"},
				},
			})
			resp, _ := json.Marshal(map[string]any{
				"candidates": []map[string]any{
					{"content": map[string]any{"parts": []map[string]any{{"text": string(payload)}}}},
				},
			})
			_, _ = w.Write(resp)
			return
		}
		_, _ = w.Write([]byte(`{
			"candidates": [{
				"content": {
					"parts": [{
						"text": "{\"trimmed_content\":\"로컬 디버그 서버는 19090 포트를 사용한다.\"}"
					}]
				}
			}]
		}`))
	}))
	defer srv.Close()

	t.Setenv("GEMINI_API_BASE", srv.URL)
	ev := NewGeminiAdapterConfig("test-key", "gemini-2.5-flash")

	teamRules := []domain.TeamRule{
		{RuleKey: "db-no-cgo", Scope: "project:app", Category: "database", Pinned: true, Title: "Never enable CGO.", Why: "Static binaries.", How: "CGO_ENABLED=0"},
		{RuleKey: "api-auth", Scope: "project:app", Category: "api", Title: "Require bearer auth.", Why: "Security.", How: "Middleware"},
	}
	localFacts := []domain.Fact{
		{ID: 10, Scope: "project:app", Category: "database", Content: "CGO를 끄고 빌드한다."},
		{ID: 11, Scope: "project:app", Category: "api", Content: "Bearer auth를 쓰고 로컬 디버그 서버는 19090 포트를 사용한다."},
		{ID: 12, Scope: "project:app", Category: "testing", Content: "Unit tests run in parallel."},
	}

	out, err := ev.FindRedundantLocalMemories(context.Background(), "project:app", teamRules, localFacts)
	if err != nil {
		t.Fatalf("FindRedundantLocalMemories failed: %v", err)
	}
	if len(out) != 2 {
		t.Fatalf("expected exactly 2 validated suggestions, got %+v", out)
	}
	if out[0].LocalMemoryID != 10 || out[0].Kind != "duplicate" || out[0].TrimmedContent != "" {
		t.Fatalf("unexpected first suggestion: %+v", out[0])
	}
	if out[1].LocalMemoryID != 11 || out[1].Kind != "partial_overlap" || out[1].TrimmedContent != "로컬 디버그 서버는 19090 포트를 사용한다." {
		t.Fatalf("expected shortened partial_overlap trimmed_content, got: %+v", out[1])
	}
	if calls.Load() != 2 {
		t.Fatalf("expected 1 main call + 1 shortening retry = 2 calls, got %d", calls.Load())
	}
}
