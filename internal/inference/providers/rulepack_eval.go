package providers

import (
	"context"
	"fmt"
	"strings"

	"praxis/internal/domain"
)

const maxTrimmedRuleRunes = 180

func (e *schemaEvaluator) DraftRulePackProposal(ctx context.Context, scope, rawRuleText, sourceCat, sourceSub string, outline []domain.RuleOutlineEntry) (domain.RulePlacementProposal, error) {
	raw := strings.TrimSpace(rawRuleText)
	if raw == "" {
		return domain.RulePlacementProposal{}, fmt.Errorf("cannot draft team rule from empty input")
	}

	presets := domain.PresetCategoriesFromContext(ctx)
	var sb strings.Builder
	fmt.Fprintf(&sb, `Scope: %s
Preset Taxonomy (category -> sub_categories): %s
Source Hint: category=%q sub_category=%q

Convert the Candidate Rule below into ONE clean, self-contained single-line Team Rule entry for Markdown documentation:
1. CRITICAL: Write "title" in the EXACT SAME LANGUAGE as the Candidate Rule (e.g., Korean input -> Korean output; English input -> English output). Never translate.
2. "category": concise lowercase domain slug. Reuse an existing Preset Taxonomy or Existing Rule Outline category (or Source Hint) when applicable; if none fits, introduce a concise new category slug.
3. "sub_category": concise lowercase sub-category slug. Reuse an existing sub-category under that category from Preset Taxonomy or Existing Rule Outline (or Source Hint) when applicable, or introduce a concise new sub-category slug when a clear sub-topic exists ("" only if genuinely broad).
4. "title": ONE concise, standalone summary sentence (<= 180 chars) integrating any essential scope or reason naturally — NO separate Why/How/Where prefixes, NO [invariant]/[rule] prefixes, NO newlines.
`, scope, presets, sourceCat, sourceSub)

	sb.WriteString("\nExisting Rule Outline (current document order):\n")
	if len(outline) == 0 {
		sb.WriteString("(empty rule pack)\n")
	} else {
		for idx, item := range outline {
			sub := item.SubCategory
			if sub == "" {
				sub = "-"
			}
			fmt.Fprintf(&sb, "- [%d] (%s/%s) %s\n", idx+1, item.Category, sub, item.Title)
		}
	}
	fmt.Fprintf(&sb, "\nCandidate Rule:\n%s\n", raw)

	d := e.tr.Dialect()
	schema := d.obj(map[string]any{
		"category":     map[string]any{"type": d.t("string")},
		"sub_category": map[string]any{"type": d.t("string")},
		"title":        map[string]any{"type": d.t("string")},
	}, []string{"category", "sub_category", "title"})

	var parsed struct {
		Category    string `json:"category"`
		SubCategory string `json:"sub_category"`
		Title       string `json:"title"`
	}
	if _, err := e.tr.CallJSON(ctx, sb.String(), schema, &parsed); err != nil {
		return domain.RulePlacementProposal{}, err
	}

	catSeed := strings.TrimSpace(parsed.Category)
	if catSeed == "" {
		catSeed = sourceCat
	}
	category := domain.NormalizeCategoryForScope(scope, catSeed)
	subCat := domain.NormalizeSubCategory(parsed.SubCategory)
	if subCat == "" && sourceSub != "" {
		subCat = domain.NormalizeSubCategory(sourceSub)
	}

	title := sanitizeSingleLineRuleField(parsed.Title)
	title = strings.ReplaceAll(title, "`[invariant]`", "")
	title = strings.ReplaceAll(title, "`[rule]`", "")
	title = strings.TrimSpace(title)
	if title == "" {
		title = sanitizeSingleLineRuleField(raw)
	}

	ruleKey := domain.ComputeRuleKey(category, title)

	return domain.RulePlacementProposal{
		RuleKey:     ruleKey,
		Category:    category,
		SubCategory: subCat,
		Pinned:      true,
		Title:       title,
	}, nil
}

func (e *schemaEvaluator) FindRedundantLocalMemories(ctx context.Context, scope string, teamRules []domain.TeamRule, localFacts []domain.Fact) ([]domain.RuleDuplicateSuggestion, error) {
	if len(teamRules) == 0 || len(localFacts) == 0 {
		return nil, nil
	}

	var sb strings.Builder
	fmt.Fprintf(&sb, `Scope: %s
Compare Active Local Memories against Active Team Rules (shared team SSOT). Identify ONLY local memories that are:
1. "duplicate": the Team Rule already expresses essentially the entire local memory cleanly (safe to delete local memory), OR
2. "partial_overlap": part of the local memory duplicates a Team Rule, while another distinct local detail remains worth keeping. In this case set "trimmed_content" to ONE dense sentence (<= 165 chars) retaining ONLY the non-overlapping local detail in the EXACT SAME LANGUAGE as the local memory.
Rules:
- Omit local memories with no meaningful overlap.
- Never alter or suggest deleting Team Rules.
- Return 1-based "team_rule_index" matching the "[N]" prefix under Active Team Rules.
- Each local_memory_id may appear at most once.
- "reason": short phrase (<= 50 chars) in the same language as the local memory.

Active Team Rules:
`, scope)
	for idx, r := range teamRules {
		cat := r.Category
		if r.SubCategory != "" {
			cat += "/" + r.SubCategory
		}
		fmt.Fprintf(&sb, "- [%d] (%s) %s\n", idx+1, cat, r.DisplayContent())
	}

	localByID := make(map[int64]domain.Fact, len(localFacts))
	sb.WriteString("\nActive Local Memories:\n")
	for _, f := range localFacts {
		localByID[f.ID] = f
		tier := "general"
		if f.Pinned {
			tier = "pinned"
		}
		cat := f.Category
		if f.SubCategory != "" {
			cat += "/" + f.SubCategory
		}
		fmt.Fprintf(&sb, "- [#%d] (%s | %s) %s\n", f.ID, cat, tier, f.Content)
	}

	d := e.tr.Dialect()
	itemSchema := d.obj(map[string]any{
		"local_memory_id": map[string]any{"type": d.t("integer")},
		"team_rule_index": map[string]any{"type": d.t("integer")},
		"kind":            map[string]any{"type": d.t("string")},
		"trimmed_content": map[string]any{"type": d.t("string")},
		"reason":          map[string]any{"type": d.t("string")},
	}, []string{"local_memory_id", "team_rule_index", "kind", "trimmed_content", "reason"})
	schema := d.obj(map[string]any{
		"items": map[string]any{"type": d.t("array"), "items": itemSchema},
	}, []string{"items"})

	var parsed struct {
		Items []struct {
			LocalMemoryID  int64  `json:"local_memory_id"`
			TeamRuleIndex  int    `json:"team_rule_index"`
			Kind           string `json:"kind"`
			TrimmedContent string `json:"trimmed_content"`
			Reason         string `json:"reason"`
		} `json:"items"`
	}
	if _, err := e.tr.CallJSON(ctx, sb.String(), schema, &parsed); err != nil {
		return nil, err
	}

	seenLocal := make(map[int64]bool, len(parsed.Items))
	out := make([]domain.RuleDuplicateSuggestion, 0, len(parsed.Items))
	for _, it := range parsed.Items {
		if seenLocal[it.LocalMemoryID] {
			continue
		}
		localFact, okLocal := localByID[it.LocalMemoryID]
		if !okLocal {
			continue
		}
		if it.TeamRuleIndex < 1 || it.TeamRuleIndex > len(teamRules) {
			continue
		}
		trRule := teamRules[it.TeamRuleIndex-1]

		kind := strings.ToLower(strings.TrimSpace(it.Kind))
		trimmed := sanitizeSingleLineRuleField(it.TrimmedContent)
		switch kind {
		case "duplicate":
			trimmed = ""
		case "partial_overlap":
			if trimmed == "" {
				continue
			}
			if len([]rune(trimmed)) > maxTrimmedRuleRunes {
				trimmed = e.shortenTrimmedRule(ctx, trimmed)
			}
		default:
			continue
		}

		seenLocal[it.LocalMemoryID] = true
		out = append(out, domain.RuleDuplicateSuggestion{
			LocalMemoryID:  localFact.ID,
			LocalCategory:  localFact.Category,
			LocalContent:   localFact.Content,
			TeamRuleKey:    trRule.RuleKey,
			TeamRuleTitle:  trRule.Title,
			Kind:           kind,
			TrimmedContent: trimmed,
			Reason:         sanitizeSingleLineRuleField(it.Reason),
		})
	}
	return out, nil
}

func (e *schemaEvaluator) shortenTrimmedRule(ctx context.Context, text string) string {
	d := e.tr.Dialect()
	schema := d.obj(map[string]any{
		"trimmed_content": map[string]any{"type": d.t("string")},
	}, []string{"trimmed_content"})
	prompt := fmt.Sprintf("Shorten this remaining local rule to strictly <= 165 characters in the exact same language while preserving its distinct non-team details:\n%s", text)
	var retry struct {
		TrimmedContent string `json:"trimmed_content"`
	}
	if _, err := e.tr.CallJSON(ctx, prompt, schema, &retry); err == nil {
		if s := sanitizeSingleLineRuleField(retry.TrimmedContent); s != "" && len([]rune(s)) <= maxTrimmedRuleRunes {
			return s
		}
	}
	runes := []rune(text)
	if len(runes) > maxTrimmedRuleRunes {
		return strings.TrimSpace(string(runes[:maxTrimmedRuleRunes]))
	}
	return text
}

func sanitizeSingleLineRuleField(s string) string {
	return strings.Join(strings.Fields(strings.TrimSpace(s)), " ")
}
