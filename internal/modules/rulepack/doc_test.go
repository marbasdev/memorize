package rulepack

import (
	"errors"
	"strings"
	"testing"

	"memorize/internal/domain"
)

func TestInsertRule_ScaffoldAndRoundTrip(t *testing.T) {
	first := domain.RulePlacementProposal{
		Category:    "architecture",
		SubCategory: "workflow",
		Title:       "Keep acyclic unidirectional package layering",
		AddedBy:     "@marbas",
		AddedAt:     "2026-11-14",
	}
	raw, err := InsertRule(nil, "project:memorize", first)
	if err != nil {
		t.Fatalf("InsertRule scaffold failed: %v", err)
	}
	text := string(raw)
	if !strings.Contains(text, "# Team Rules (`memorize`)") ||
		!strings.Contains(text, "## Architecture (`architecture`)") ||
		!strings.Contains(text, "- (`workflow`) Keep acyclic unidirectional package layering · `@marbas` · `2026-11-14`") {
		t.Fatalf("unexpected generated markdown:\n%s", text)
	}
	if strings.Contains(text, "[invariant]") || strings.Contains(text, "[rule]") || strings.Contains(text, "r-") {
		t.Fatalf("expected no visible key or tier tags in markdown:\n%s", text)
	}

	doc, err := ParseDocument(raw, "project:memorize")
	if err != nil {
		t.Fatalf("ParseDocument round-trip failed: %v", err)
	}
	if !doc.Exists || len(doc.Rules) != 1 {
		t.Fatalf("expected 1 rule, got exists=%v len=%d", doc.Exists, len(doc.Rules))
	}
	got := doc.Rules[0]
	wantKey := domain.ComputeRuleKey("architecture", "Keep acyclic unidirectional package layering")
	if got.RuleKey != wantKey || !got.Pinned || got.SubCategory != "workflow" || got.Title != "Keep acyclic unidirectional package layering" || got.AddedBy != "@marbas" || got.AddedAt != "2026-11-14" {
		t.Fatalf("unexpected parsed rule: %+v", got)
	}
}

func TestInsertRule_SurgicalOrderingAndBytePreservation(t *testing.T) {
	initial := strings.Join([]string{
		"# Team Rules (`memorize`)",
		"",
		"> Custom team intro note preserved verbatim.",
		"",
		"## Architecture (`architecture`)",
		"",
		"> Keep modules decoupled.",
		"",
		"### `arch-one` · First legacy rule `[invariant]`",
		"- **Subcategory**: `workflow`",
		"- **Why**: Keeps boundaries clean",
		"- **How**: Check imports",
		"- **Added**: `Minho Kim` · `2026-11-10`",
		"",
		"### `arch-two` · Second legacy rule `[rule]`",
		"- **Subcategory**: `ui`",
		"- **Why**: Keeps CSS simple",
		"- **How**: Keep under 800 lines",
		"- **Added**: `@alice` · `2026-11-11`",
		"",
		"- `arch-legacy` `[rule]` Legacy compact bullet stays readable · `@kim` · `2026-11-12`",
		"",
		"## Testing (`testing`)",
		"",
		"### `test-race` · Run race detector on Unix CI `[rule]`",
		"- **Why**: Catches concurrency bugs",
		"- **How**: Run go test -race",
		"- **Added**: `@bob` · `2026-11-12`",
		"",
	}, "\n")

	// 1) Inserting Security places ## Security between ## Architecture and ## Testing (canonical rank),
	//    leaving all existing prose and legacy rules untouched verbatim.
	withSec, err := InsertRule([]byte(initial), "project:demo", domain.RulePlacementProposal{
		Category: "security",
		Title:    "Bind daemon strictly to loopback",
		AddedBy:  "@sec",
		AddedAt:  "2026-11-14",
	})
	if err != nil {
		t.Fatalf("insert security failed: %v", err)
	}
	secText := string(withSec)
	archIdx := strings.Index(secText, "## Architecture (`architecture`)")
	secIdx := strings.Index(secText, "## Security (`security`)")
	testIdx := strings.Index(secText, "## Testing (`testing`)")
	if !(archIdx < secIdx && secIdx < testIdx) {
		t.Fatalf("expected Security between Architecture and Testing:\n%s", secText)
	}
	if !strings.Contains(secText, "> Custom team intro note preserved verbatim.") ||
		!strings.Contains(secText, "> Keep modules decoupled.") {
		t.Fatalf("existing intro notes were mutated")
	}

	// 2) Inserting into Architecture always appends at the bottom of the Architecture section.
	withSub, err := InsertRule(withSec, "project:demo", domain.RulePlacementProposal{
		Category:    "architecture",
		SubCategory: "ui",
		Title:       "Keep custom templates under 800 lines",
		AddedBy:     "@marbas",
		AddedAt:     "2026-11-14",
	})
	if err != nil {
		t.Fatalf("insert ui subcategory failed: %v", err)
	}
	subText := string(withSub)
	idxLeg := strings.Index(subText, "Legacy compact bullet stays readable")
	idxUI2 := strings.Index(subText, "- (`ui`) Keep custom templates under 800 lines · `@marbas` · `2026-11-14`")
	idxSec := strings.Index(subText, "## Security (`security`)")
	if !(idxLeg < idxUI2 && idxUI2 < idxSec) {
		t.Fatalf("expected new rule appended at bottom of Architecture section:\n%s", subText)
	}

	// 3) Inserting another rule in Architecture appends after the previous last Architecture rule.
	withAfter, err := InsertRule(withSub, "project:demo", domain.RulePlacementProposal{
		Category: "architecture",
		Title:    "Follow-up rule at bottom of architecture",
		AddedBy:  "@alice",
		AddedAt:  "2026-11-14",
	})
	if err != nil {
		t.Fatalf("insert bottom rule failed: %v", err)
	}
	afterText := string(withAfter)
	pPrev := strings.Index(afterText, "- (`ui`) Keep custom templates under 800 lines · `@marbas` · `2026-11-14`")
	pLast := strings.Index(afterText, "- Follow-up rule at bottom of architecture · `@alice` · `2026-11-14`")
	pSec := strings.Index(afterText, "## Security (`security`)")
	if !(pPrev < pLast && pLast < pSec) {
		t.Fatalf("expected follow-up rule appended at bottom of architecture:\n%s", afterText)
	}

	doc, err := ParseDocument(withAfter, "project:demo")
	if err != nil {
		t.Fatalf("re-parse final doc failed: %v", err)
	}
	if len(doc.Rules) != 7 {
		t.Fatalf("expected 7 parsed rules (legacy + new), got %d", len(doc.Rules))
	}
	if doc.Rules[4].Title != "Follow-up rule at bottom of architecture" || doc.Rules[0].AddedBy != "@Minho-Kim" {
		t.Fatalf("unexpected order or git handle normalization: %+v", doc.Outline())
	}
}

func TestParseAndSecurityValidation(t *testing.T) {
	// Duplicate rules (same category + normalized title) rejected with ParseError.
	dupDoc := strings.Join([]string{
		"## Architecture (`architecture`)",
		"",
		"- Keep layers clean · `@u` · `2026-11-14`",
		"- Keep   layers clean · `@v` · `2026-11-14`",
	}, "\n")
	if _, err := ParseDocument([]byte(dupDoc), "project:demo"); err == nil {
		t.Fatal("expected duplicate rule parse error")
	}

	// Non-rule intro bullets inside a section are preserved harmlessly without failing parse.
	withNotes := strings.Join([]string{
		"## Architecture (`architecture`)",
		"",
		"- General module overview note (not a team rule bullet).",
		"",
		"- Keep acyclic layers · `@u` · `2026-11-14`",
	}, "\n")
	docNotes, err := ParseDocument([]byte(withNotes), "project:demo")
	if err != nil || len(docNotes.Rules) != 1 || docNotes.Sections[0].IntroEndLine != 2 {
		t.Fatalf("expected intro bullet preserved and 1 rule parsed, got doc=%+v err=%v", docNotes, err)
	}

	// Secret detection blocks export.
	_, err = InsertRule(nil, "project:demo", domain.RulePlacementProposal{
		Category: "security",
		Title:    "Never embed AIzaSyDummySecretValue12345678901234",
		AddedBy:  "@u",
		AddedAt:  "2026-11-14",
	})
	if err == nil || !strings.Contains(err.Error(), "rejected") {
		t.Fatalf("expected security guard rejection, got %v", err)
	}

	// Duplicate insert error uses ErrRuleKeyExists sentinel.
	base, err := InsertRule(nil, "project:demo", domain.RulePlacementProposal{
		Category: "testing",
		Title:    "Rule one",
		AddedBy:  "@u",
		AddedAt:  "2026-11-14",
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = InsertRule(base, "project:demo", domain.RulePlacementProposal{
		Category: "testing",
		Title:    "Rule one",
		AddedBy:  "@u",
		AddedAt:  "2026-11-14",
	})
	if !errors.Is(err, ErrRuleKeyExists) {
		t.Fatalf("expected ErrRuleKeyExists, got %v", err)
	}
}

func TestReplaceAndRemoveRuleAndCRLFHash(t *testing.T) {
	base, err := InsertRule(nil, "project:demo", domain.RulePlacementProposal{
		Category: "database",
		Title:    "Wrap schema changes in a transaction",
		AddedBy:  "@db",
		AddedAt:  "2026-11-14",
	})
	if err != nil {
		t.Fatal(err)
	}
	base, err = InsertRule(base, "project:demo", domain.RulePlacementProposal{
		Category: "database",
		Title:    "Enable SQLite WAL mode",
		AddedBy:  "@db",
		AddedAt:  "2026-11-14",
	})
	if err != nil {
		t.Fatal(err)
	}

	crlf := []byte(strings.ReplaceAll(string(base), "\n", "\r\n"))
	if CanonicalHash(base) != CanonicalHash(crlf) {
		t.Fatal("expected identical canonical SHA-256 across LF and CRLF")
	}

	firstKey := domain.ComputeRuleKey("database", "Wrap schema changes in a transaction")
	updated, err := ReplaceRuleByKey(base, "project:demo", firstKey, domain.TeamRule{
		Category: "database",
		Title:    "Wrap all DDL migrations cleanly",
		AddedBy:  "@db",
		AddedAt:  "2026-11-14",
	})
	if err != nil {
		t.Fatalf("ReplaceRuleByKey failed: %v", err)
	}
	if !strings.Contains(string(updated), "Wrap all DDL migrations cleanly · `@db` · `2026-11-14`\n- Enable SQLite WAL mode · `@db` · `2026-11-14`") {
		t.Fatalf("expected in-place tight bullet update, got:\n%s", updated)
	}

	// Moving category via ReplaceRuleByKey removes empty source or keeps other rule and inserts in new section.
	walKey := domain.ComputeRuleKey("database", "Enable SQLite WAL mode")
	moved, err := ReplaceRuleByKey(updated, "project:demo", walKey, domain.TeamRule{
		Category:    "architecture",
		SubCategory: "storage",
		Title:       "Enable SQLite WAL mode by default",
		AddedBy:     "@arch",
		AddedAt:     "2026-11-14",
	})
	if err != nil {
		t.Fatalf("cross-category ReplaceRuleByKey failed: %v", err)
	}
	if !strings.Contains(string(moved), "## Architecture (`architecture`)") || !strings.Contains(string(moved), "- (`storage`) Enable SQLite WAL mode by default · `@arch` · `2026-11-14`") {
		t.Fatalf("unexpected moved output:\n%s", moved)
	}

	ddlKey := domain.ComputeRuleKey("database", "Wrap all DDL migrations cleanly")
	removed, err := RemoveRule(moved, "project:demo", ddlKey)
	if err != nil {
		t.Fatalf("RemoveRule failed: %v", err)
	}
	// Pruning empty ## Database section when its only rule is deleted without intro prose.
	if strings.Contains(string(removed), "## Database (`database`)") {
		t.Fatalf("expected empty Database heading to be pruned after deleting its only rule:\n%s", removed)
	}
	doc, err := ParseDocument(removed, "project:demo")
	if err != nil {
		t.Fatalf("ParseDocument after RemoveRule failed: %v", err)
	}
	if len(doc.Rules) != 1 || doc.Rules[0].Title != "Enable SQLite WAL mode by default" {
		t.Fatalf("expected 1 rule left after removal, got %+v", doc.Rules)
	}
}
