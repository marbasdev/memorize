package rulepack

import (
	"fmt"
	"strings"

	"memorize/internal/domain"
)

func (d *Document) Outline() []domain.RuleOutlineEntry {
	if d == nil || len(d.Rules) == 0 {
		return nil
	}
	out := make([]domain.RuleOutlineEntry, len(d.Rules))
	for i, r := range d.Rules {
		out[i] = domain.RuleOutlineEntry{
			RuleKey:     r.RuleKey,
			Category:    r.Category,
			SubCategory: r.SubCategory,
			Title:       r.Title,
		}
	}
	return out
}

func FormatRule(r domain.TeamRule) []string {
	subPart := ""
	if sub := domain.NormalizeSubCategory(r.SubCategory); sub != "" {
		subPart = fmt.Sprintf("(`%s`) ", sub)
	}
	title := strings.TrimSpace(strings.NewReplacer("\r\n", " ", "\n", " ", "\r", " ").Replace(r.Title))
	author := domain.NormalizeGitHandle(r.AddedBy)
	if author == "" {
		author = strings.TrimSpace(r.AddedBy)
	}
	return []string{
		fmt.Sprintf("- %s%s · `%s` · `%s`", subPart, title, author, strings.TrimSpace(r.AddedAt)),
	}
}

func FormatSectionHeading(cat string) string {
	slug := domain.NormalizeCategoryForScope("project:team", cat)
	return fmt.Sprintf("## %s (`%s`)", domain.FormatCategoryHeadingTitle(slug), slug)
}

func serializeLines(lines []string) []byte {
	if len(lines) == 0 {
		return nil
	}
	return []byte(strings.Join(lines, "\n") + "\n")
}

func spliceInsert(lines []string, idx int, extra []string) []string {
	if idx < 0 {
		idx = 0
	}
	if idx > len(lines) {
		idx = len(lines)
	}
	out := make([]string, 0, len(lines)+len(extra))
	out = append(out, lines[:idx]...)
	out = append(out, extra...)
	out = append(out, lines[idx:]...)
	return out
}

func spliceRemove(lines []string, start, end int, trimAdjacentBlank bool) []string {
	if trimAdjacentBlank {
		if end+1 < len(lines) && strings.TrimSpace(lines[end+1]) == "" {
			end++
		} else if start > 0 && strings.TrimSpace(lines[start-1]) == "" {
			start--
		}
	}
	out := make([]string, 0, len(lines)-(end-start+1))
	out = append(out, lines[:start]...)
	out = append(out, lines[end+1:]...)
	return out
}

func lastNonBlankBefore(lines []string, limit int) int {
	if limit > len(lines) {
		limit = len(lines)
	}
	for i := limit - 1; i >= 0; i-- {
		if strings.TrimSpace(lines[i]) != "" {
			return i
		}
	}
	return -1
}

func InsertRule(raw []byte, scope string, prop domain.RulePlacementProposal) ([]byte, error) {
	rule := prop.ToTeamRule(scope)
	if err := domain.ValidateTeamRule(rule); err != nil {
		return nil, err
	}
	ruleLines := FormatRule(rule)

	if strings.TrimSpace(string(raw)) == "" {
		lines := append([]string{}, strings.Split(DefaultDocumentPreamble, "\n")...)
		lines = append(lines, "", FormatSectionHeading(rule.Category), "")
		lines = append(lines, ruleLines...)
		return serializeLines(lines), nil
	}

	doc, err := ParseDocument(raw, scope)
	if err != nil {
		return nil, err
	}
	for _, existing := range doc.Rules {
		if existing.RuleKey == rule.RuleKey {
			return nil, fmt.Errorf("%w: %s", ErrRuleKeyExists, rule.RuleKey)
		}
	}

	for _, sec := range doc.Sections {
		if sec.Category == rule.Category {
			return insertIntoExistingSection(doc.Lines, scope, sec, ruleLines), nil
		}
	}

	return insertNewSection(doc, rule.Category, ruleLines), nil
}

func insertIntoExistingSection(lines []string, scope string, sec DocumentSection, ruleLines []string) []byte {
	afterLine := pickRuleInsertionLine(sec)
	insertAt := afterLine + 1
	var block []string
	switch {
	case afterLine >= 0 && afterLine < len(lines) && isCompactDocRuleLine(lines[afterLine], scope):
		block = ruleLines
	case insertAt < len(lines) && strings.TrimSpace(lines[insertAt]) == "" &&
		insertAt+1 < len(lines) && isCompactDocRuleLine(lines[insertAt+1], scope):
		insertAt++
		block = ruleLines
	default:
		block = append([]string{""}, ruleLines...)
	}
	return serializeLines(spliceInsert(lines, insertAt, block))
}

func insertNewSection(doc *Document, category string, ruleLines []string) []byte {
	newRank := domain.CanonicalCategoryRank(category)
	secHeading := FormatSectionHeading(category)
	for _, sec := range doc.Sections {
		secRank := domain.CanonicalCategoryRank(sec.Category)
		if secRank > newRank || (secRank == 100 && newRank == 100 && sec.Category > category) {
			block := append([]string{secHeading, ""}, ruleLines...)
			block = append(block, "")
			return serializeLines(spliceInsert(doc.Lines, sec.StartLine, block))
		}
	}

	lastNB := lastNonBlankBefore(doc.Lines, len(doc.Lines))
	block := append([]string{"", secHeading, ""}, ruleLines...)
	return serializeLines(spliceInsert(doc.Lines, lastNB+1, block))
}

func isCompactDocRuleLine(line, scope string) bool {
	_, ok, _ := parseCompactRuleLine(strings.TrimSpace(line), scope, "general", "")
	return ok
}

func pickRuleInsertionLine(sec DocumentSection) int {
	if len(sec.Rules) == 0 {
		return sec.IntroEndLine
	}
	return sec.Rules[len(sec.Rules)-1].EndLine
}

func ReplaceRuleByKey(raw []byte, scope, targetKey string, updated domain.TeamRule) ([]byte, error) {
	sc := domain.NormalizeScope(scope)
	target := domain.NormalizeRuleKey(targetKey)
	updated.Category = domain.NormalizeCategoryForScope(sc, updated.Category)
	updated.SubCategory = domain.NormalizeSubCategory(updated.SubCategory)
	updated.Title = strings.Join(strings.Fields(strings.TrimSpace(updated.Title)), " ")
	if domain.NormalizeRuleKey(updated.RuleKey) == "" {
		updated.RuleKey = domain.ComputeRuleKey(updated.Category, updated.Title)
	} else {
		updated.RuleKey = domain.NormalizeRuleKey(updated.RuleKey)
	}
	updated.AddedBy = domain.NormalizeGitHandle(updated.AddedBy)
	updated.Pinned = true
	if err := domain.ValidateTeamRule(updated); err != nil {
		return nil, err
	}

	doc, err := ParseDocument(raw, sc)
	if err != nil {
		return nil, err
	}
	for _, existing := range doc.Rules {
		if existing.RuleKey != target && existing.RuleKey == updated.RuleKey {
			return nil, fmt.Errorf("%w: %s", ErrRuleKeyExists, updated.Title)
		}
	}
	for _, sec := range doc.Sections {
		for _, r := range sec.Rules {
			if r.RuleKey != target {
				continue
			}
			if r.Category != updated.Category {
				removed, err := RemoveRule(raw, sc, target)
				if err != nil {
					return nil, err
				}
				return InsertRule(removed, sc, domain.RulePlacementProposal{
					RuleKey:     updated.RuleKey,
					Category:    updated.Category,
					SubCategory: updated.SubCategory,
					Pinned:      true,
					Title:       updated.Title,
					AddedBy:     updated.AddedBy,
					AddedAt:     updated.AddedAt,
				})
			}
			newLines := FormatRule(updated)
			out := make([]string, 0, len(doc.Lines)-((r.EndLine-r.StartLine)+1)+len(newLines))
			out = append(out, doc.Lines[:r.StartLine]...)
			out = append(out, newLines...)
			out = append(out, doc.Lines[r.EndLine+1:]...)
			return serializeLines(out), nil
		}
	}
	return nil, fmt.Errorf("%w: %s", ErrRuleNotFound, targetKey)
}

func RemoveRule(raw []byte, scope, key string) ([]byte, error) {
	sc := domain.NormalizeScope(scope)
	doc, err := ParseDocument(raw, sc)
	if err != nil {
		return nil, err
	}
	target := domain.NormalizeRuleKey(key)
	for _, sec := range doc.Sections {
		for _, r := range sec.Rules {
			if r.RuleKey != target {
				continue
			}
			if len(sec.Rules) == 1 && sec.IntroEndLine == sec.StartLine {
				return serializeLines(spliceRemove(doc.Lines, sec.StartLine, r.EndLine, true)), nil
			}
			start, end := r.StartLine, r.EndLine
			isCompact := start == end && isCompactDocRuleLine(doc.Lines[start], sc)
			hasPrevCompact := isCompact && start > 0 && isCompactDocRuleLine(doc.Lines[start-1], sc)
			hasNextCompact := isCompact && end+1 < len(doc.Lines) && isCompactDocRuleLine(doc.Lines[end+1], sc)
			return serializeLines(spliceRemove(doc.Lines, start, end, !hasPrevCompact && !hasNextCompact)), nil
		}
	}
	return nil, fmt.Errorf("%w: %s", ErrRuleNotFound, key)
}
