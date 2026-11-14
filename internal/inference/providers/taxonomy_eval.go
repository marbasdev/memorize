package providers

import (
	"context"
	"fmt"
	"strings"

	"memorize/internal/domain"
)

// SuggestTaxonomyRebalance runs on-demand AI re-categorization to suggest semantic
// category and sub_category assignments (reusing existing slugs or adding concise new slugs, never deleting or editing content).
func (e *schemaEvaluator) SuggestTaxonomyRebalance(ctx context.Context, scope string, facts []domain.Fact) ([]domain.TaxonomyRebalanceItem, error) {
	if len(facts) == 0 {
		return nil, nil
	}
	presets := domain.PresetCategoriesFromContext(ctx)
	var sb strings.Builder
	fmt.Fprintf(&sb, `Scope: %s
Current Taxonomy (category -> sub_categories): %s
During on-demand AI re-categorization, inspect the active memories below and return semantic category / sub_category recommendations ONLY for items whose classification should be refined:
1. Assign an appropriate sub_category to unclassified items (where current sub_category is "-"), fix mismatched category/sub_category pairs, and unify synonymous or scattered sub_category slugs under a consistent slug.
2. Reuse existing categories and sub-categories when they fit; introduce a concise lowercase ASCII slug when distinct topics warrant a new category or sub_category (append-only, never deleting taxonomy).
3. Omit items whose current category and sub_category already fit well.
4. Write "reason" concisely in the same language as the memory text.

Active Memories:
`, scope, presets)
	for _, f := range facts {
		sub := f.SubCategory
		if sub == "" {
			sub = "-"
		}
		fmt.Fprintf(&sb, "- [#%d] (%s / %s) %s\n", f.ID, f.Category, sub, f.Content)
	}

	d := e.tr.Dialect()
	itemSchema := d.obj(map[string]any{
		"id":               map[string]any{"type": d.t("integer")},
		"new_category":     map[string]any{"type": d.t("string")},
		"new_sub_category": map[string]any{"type": d.t("string")},
		"reason":           map[string]any{"type": d.t("string")},
	}, []string{"id", "new_category", "new_sub_category", "reason"})
	schema := d.obj(map[string]any{
		"items": map[string]any{"type": d.t("array"), "items": itemSchema},
	}, []string{"items"})

	var parsed struct {
		Items []domain.TaxonomyRebalanceItem `json:"items"`
	}
	if _, err := e.tr.CallJSON(ctx, sb.String(), schema, &parsed); err != nil {
		return nil, err
	}
	return parsed.Items, nil
}
