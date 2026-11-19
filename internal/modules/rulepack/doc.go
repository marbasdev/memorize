package rulepack

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"memorize/internal/domain"
)

const DefaultDocumentPreamble = `# Team Rules (` + "`memorize`" + `)

> Shared repository rules managed via Git. Mirrored read-only into local ` + "`memorize`" + ` daemons.`

var (
	ErrRuleKeyExists = errors.New("rule already exists in rule pack")
	ErrRuleNotFound  = errors.New("rule not found in rule pack")
)

type ParseError struct {
	Line    int
	Message string
}

func (e *ParseError) Error() string {
	if e.Line > 0 {
		return fmt.Sprintf(".memorize/TEAM_RULES.md line %d: %s", e.Line, e.Message)
	}
	return ".memorize/TEAM_RULES.md: " + e.Message
}

type DocumentRule struct {
	domain.TeamRule
	StartLine int
	EndLine   int
}

type DocumentSection struct {
	Category     string
	Heading      string
	StartLine    int
	IntroEndLine int
	EndLine      int
	Rules        []DocumentRule
}

type Document struct {
	Exists        bool
	Hash          string
	Lines         []string
	HadTrailingNL bool
	Sections      []DocumentSection
	Rules         []domain.TeamRule
}

func DocPath(repoRoot string) string {
	root := strings.TrimSpace(repoRoot)
	if root == "" {
		return ""
	}
	return filepath.Join(root, filepath.FromSlash(domain.RulePackRelPath))
}

func CanonicalHash(raw []byte) string {
	norm := strings.ReplaceAll(string(raw), "\r\n", "\n")
	sum := sha256.Sum256([]byte(norm))
	return hex.EncodeToString(sum[:])
}

func ParseDocument(raw []byte, scope string) (*Document, error) {
	norm := strings.ReplaceAll(string(raw), "\r\n", "\n")
	hadNL := strings.HasSuffix(norm, "\n")
	trimmedBody := strings.TrimSuffix(norm, "\n")

	var lines []string
	if trimmedBody != "" {
		lines = strings.Split(trimmedBody, "\n")
	}

	p := &docParser{
		scope: scope,
		doc: &Document{
			Exists:        true,
			Hash:          CanonicalHash(raw),
			Lines:         lines,
			HadTrailingNL: hadNL || len(lines) == 0,
		},
		seenCats:   make(map[string]int),
		seenKeys:   make(map[string]int),
		curSecIdx:  -1,
		curRuleIdx: -1,
	}
	for idx, rawLine := range lines {
		if err := p.step(idx, rawLine); err != nil {
			return nil, err
		}
	}
	if err := p.closeOpenRule(); err != nil {
		return nil, err
	}
	return p.doc, nil
}

type docParser struct {
	scope      string
	doc        *Document
	seenCats   map[string]int
	seenKeys   map[string]int
	curSecIdx  int
	curRuleIdx int
	inFence    bool
}

func (p *docParser) closeOpenRule() error {
	if p.curSecIdx < 0 || p.curRuleIdx < 0 {
		return nil
	}
	r := &p.doc.Sections[p.curSecIdx].Rules[p.curRuleIdx]
	if r.AddedAt == "" || r.AddedBy == "" {
		return &ParseError{Line: r.StartLine + 1, Message: fmt.Sprintf("rule %q is missing required '- **Added**: `author` · `YYYY-MM-DD`' field", r.RuleKey)}
	}
	if err := domain.ValidateTeamRule(r.TeamRule); err != nil {
		return &ParseError{Line: r.StartLine + 1, Message: err.Error()}
	}
	r.SortOrder = len(p.doc.Rules)
	p.doc.Rules = append(p.doc.Rules, r.TeamRule)
	p.curRuleIdx = -1
	return nil
}

func (p *docParser) advanceFenceLine(idx int) {
	if p.curSecIdx >= 0 && p.curRuleIdx < 0 {
		p.doc.Sections[p.curSecIdx].IntroEndLine = idx
	}
	if p.curSecIdx >= 0 {
		p.doc.Sections[p.curSecIdx].EndLine = idx
	}
}

func (p *docParser) step(idx int, rawLine string) error {
	trimmed := strings.TrimSpace(rawLine)
	if strings.HasPrefix(trimmed, "```") {
		p.inFence = !p.inFence
		p.advanceFenceLine(idx)
		return nil
	}
	if p.inFence {
		p.advanceFenceLine(idx)
		return nil
	}
	if strings.HasPrefix(trimmed, "## ") {
		return p.handleSectionHeading(idx, trimmed)
	}
	if strings.HasPrefix(trimmed, "### ") {
		return p.handleRuleHeading(idx, trimmed)
	}
	if handled, err := p.tryHandleCompactRule(idx, trimmed); handled || err != nil {
		return err
	}
	return p.handleSectionBodyLine(idx, trimmed)
}

func (p *docParser) handleSectionHeading(idx int, trimmed string) error {
	if err := p.closeOpenRule(); err != nil {
		return err
	}
	cat, heading, err := parseCategoryHeading(trimmed, p.scope)
	if err != nil {
		return &ParseError{Line: idx + 1, Message: err.Error()}
	}
	if prevLine, dup := p.seenCats[cat]; dup {
		return &ParseError{Line: idx + 1, Message: fmt.Sprintf("duplicate category section %q (first defined at line %d)", cat, prevLine)}
	}
	p.seenCats[cat] = idx + 1
	p.doc.Sections = append(p.doc.Sections, DocumentSection{
		Category:     cat,
		Heading:      heading,
		StartLine:    idx,
		IntroEndLine: idx,
		EndLine:      idx,
	})
	p.curSecIdx = len(p.doc.Sections) - 1
	p.curRuleIdx = -1
	return nil
}

func (p *docParser) handleRuleHeading(idx int, trimmed string) error {
	if p.curSecIdx < 0 {
		return &ParseError{Line: idx + 1, Message: "rule heading must appear inside a '## Category (`slug`)' section"}
	}
	if err := p.closeOpenRule(); err != nil {
		return err
	}
	sec := &p.doc.Sections[p.curSecIdx]
	r, err := parseRuleHeading(trimmed, p.scope, sec.Category, p.doc.Hash)
	if err != nil {
		return &ParseError{Line: idx + 1, Message: err.Error()}
	}
	if prevLine, dup := p.seenKeys[r.RuleKey]; dup {
		return &ParseError{Line: idx + 1, Message: fmt.Sprintf("duplicate rule key %q (first defined at line %d)", r.RuleKey, prevLine)}
	}
	p.seenKeys[r.RuleKey] = idx + 1
	sec.Rules = append(sec.Rules, DocumentRule{
		TeamRule:  r,
		StartLine: idx,
		EndLine:   idx,
	})
	p.curRuleIdx = len(sec.Rules) - 1
	sec.EndLine = idx
	return nil
}

func (p *docParser) tryHandleCompactRule(idx int, trimmed string) (bool, error) {
	if !strings.HasPrefix(trimmed, "- ") || strings.HasPrefix(trimmed, "- **") {
		return false, nil
	}
	curCat := ""
	if p.curSecIdx >= 0 {
		curCat = p.doc.Sections[p.curSecIdx].Category
	}
	r, isCompact, err := parseCompactRuleLine(trimmed, p.scope, curCat, p.doc.Hash)
	if !isCompact {
		return false, nil
	}
	if p.curSecIdx < 0 {
		return true, &ParseError{Line: idx + 1, Message: "rule must appear inside a '## Category (`slug`)' section"}
	}
	if cerr := p.closeOpenRule(); cerr != nil {
		return true, cerr
	}
	if err != nil {
		return true, &ParseError{Line: idx + 1, Message: err.Error()}
	}
	if prevLine, dup := p.seenKeys[r.RuleKey]; dup {
		return true, &ParseError{Line: idx + 1, Message: fmt.Sprintf("duplicate rule %q (first defined at line %d)", r.Title, prevLine)}
	}
	if verr := domain.ValidateTeamRule(r); verr != nil {
		return true, &ParseError{Line: idx + 1, Message: verr.Error()}
	}
	p.seenKeys[r.RuleKey] = idx + 1
	sec := &p.doc.Sections[p.curSecIdx]
	r.SortOrder = len(p.doc.Rules)
	sec.Rules = append(sec.Rules, DocumentRule{
		TeamRule:  r,
		StartLine: idx,
		EndLine:   idx,
	})
	p.doc.Rules = append(p.doc.Rules, r)
	sec.EndLine = idx
	p.curRuleIdx = -1
	return true, nil
}

func (p *docParser) handleSectionBodyLine(idx int, trimmed string) error {
	if p.curSecIdx < 0 {
		return nil
	}
	sec := &p.doc.Sections[p.curSecIdx]
	if p.curRuleIdx >= 0 {
		rule := &sec.Rules[p.curRuleIdx]
		if field, val, ok := parseRuleFieldLine(trimmed); ok {
			if err := applyRuleField(&rule.TeamRule, field, val); err != nil {
				return &ParseError{Line: idx + 1, Message: err.Error()}
			}
			rule.EndLine = idx
			sec.EndLine = idx
			return nil
		}
		if trimmed != "" {
			sec.EndLine = idx
		}
		return nil
	}
	if trimmed != "" {
		sec.IntroEndLine = idx
		sec.EndLine = idx
	}
	return nil
}

func parseCategoryHeading(line, scope string) (string, string, error) {
	body := strings.TrimSpace(strings.TrimPrefix(line, "## "))
	if body == "" {
		return "", "", errors.New("category heading cannot be empty")
	}
	sc := domain.NormalizeScope(scope)
	if openIdx := strings.LastIndex(body, "(`"); openIdx > 0 && strings.HasSuffix(body, "`)") {
		slugRaw := body[openIdx+2 : len(body)-2]
		cat := domain.NormalizeCategoryForScope(sc, slugRaw)
		return cat, body, nil
	}
	return domain.NormalizeCategoryForScope(sc, body), body, nil
}

func parseRuleHeading(line, scope, category, fileHash string) (domain.TeamRule, error) {
	rest := strings.TrimSpace(strings.TrimPrefix(line, "### "))
	if !strings.HasPrefix(rest, "`") {
		return domain.TeamRule{}, errors.New("rule heading must begin with backticked `rule-key`")
	}
	endKey := strings.IndexByte(rest[1:], '`')
	if endKey <= 0 {
		return domain.TeamRule{}, errors.New("unclosed backtick in rule_key")
	}
	key := strings.TrimSpace(rest[1 : 1+endKey])
	if key == "" || domain.NormalizeRuleKey(key) != key {
		return domain.TeamRule{}, fmt.Errorf("invalid rule_key %q (use lowercase [a-z0-9][a-z0-9_-]*)", key)
	}

	afterKey := strings.TrimSpace(rest[2+endKey:])
	var pinned bool
	switch {
	case strings.HasSuffix(afterKey, "`[invariant]`"):
		pinned = true
		afterKey = strings.TrimSpace(strings.TrimSuffix(afterKey, "`[invariant]`"))
	case strings.HasSuffix(afterKey, "`[rule]`"):
		pinned = false
		afterKey = strings.TrimSpace(strings.TrimSuffix(afterKey, "`[rule]`"))
	default:
		return domain.TeamRule{}, fmt.Errorf("rule %q heading must end with `[invariant]` or `[rule]`", key)
	}

	afterKey = strings.TrimSpace(strings.TrimPrefix(afterKey, "·"))
	afterKey = strings.TrimSpace(strings.TrimPrefix(afterKey, "-"))
	if afterKey == "" {
		return domain.TeamRule{}, fmt.Errorf("rule %q title cannot be empty", key)
	}

	return domain.TeamRule{
		RuleKey:  key,
		Scope:    domain.NormalizeScope(scope),
		Category: category,
		Pinned:   pinned,
		Title:    afterKey,
		FileHash: fileHash,
	}, nil
}

func parseRuleFieldLine(trimmed string) (string, string, bool) {
	if !strings.HasPrefix(trimmed, "- **") {
		return "", "", false
	}
	rest := strings.TrimPrefix(trimmed, "- **")
	end := strings.Index(rest, "**:")
	if end <= 0 {
		return "", "", false
	}
	field := strings.ToLower(strings.TrimSpace(rest[:end]))
	val := strings.TrimSpace(rest[end+3:])
	return field, val, true
}

func stripBackticks(s string) string {
	trimmed := strings.TrimSpace(s)
	if len(trimmed) >= 2 && strings.HasPrefix(trimmed, "`") && strings.HasSuffix(trimmed, "`") {
		return strings.TrimSpace(trimmed[1 : len(trimmed)-1])
	}
	return trimmed
}

func applyRuleField(r *domain.TeamRule, field, val string) error {
	switch field {
	case "subcategory", "sub_category":
		r.SubCategory = domain.NormalizeSubCategory(stripBackticks(val))
	case "why":
		r.Why = strings.TrimSpace(val)
	case "how":
		r.How = strings.TrimSpace(val)
	case "where":
		r.Where = stripBackticks(val)
	case "added":
		parts := strings.SplitN(val, "·", 2)
		if len(parts) != 2 {
			return fmt.Errorf("rule %q Added field must follow '`@author` · `YYYY-MM-DD`'", r.RuleKey)
		}
		r.AddedBy = domain.NormalizeGitHandle(stripBackticks(parts[0]))
		r.AddedAt = stripBackticks(parts[1])
	}
	return nil
}

func parseCompactRuleLine(trimmed, scope, category, fileHash string) (domain.TeamRule, bool, error) {
	if !strings.HasPrefix(trimmed, "- ") || strings.HasPrefix(trimmed, "- **") {
		return domain.TeamRule{}, false, nil
	}
	rest := strings.TrimSpace(strings.TrimPrefix(trimmed, "- "))

	if strings.HasPrefix(rest, "`") {
		if endKey := strings.IndexByte(rest[1:], '`'); endKey > 0 {
			keyRaw := strings.TrimSpace(rest[1 : 1+endKey])
			afterKey := strings.TrimSpace(rest[2+endKey:])
			var body string
			hasLegacyTier := false
			switch {
			case afterKey == "`[invariant]`" || strings.HasPrefix(afterKey, "`[invariant]` "):
				hasLegacyTier = true
				body = strings.TrimSpace(strings.TrimPrefix(afterKey, "`[invariant]`"))
			case afterKey == "`[rule]`" || strings.HasPrefix(afterKey, "`[rule]` "):
				hasLegacyTier = true
				body = strings.TrimSpace(strings.TrimPrefix(afterKey, "`[rule]`"))
			}
			if hasLegacyTier {
				key := domain.NormalizeRuleKey(keyRaw)
				if keyRaw == "" || key != keyRaw {
					return domain.TeamRule{}, true, fmt.Errorf("invalid rule_key %q (use lowercase [a-z0-9][a-z0-9_-]*)", keyRaw)
				}
				return parseCompactRuleBody(body, key, scope, category, fileHash, true)
			}
		}
	}

	const sep = " · "
	dateSep := strings.LastIndex(rest, sep)
	if dateSep <= 0 {
		return domain.TeamRule{}, false, nil
	}
	beforeDate := rest[:dateSep]
	authorSep := strings.LastIndex(beforeDate, sep)
	if authorSep <= 0 {
		return domain.TeamRule{}, false, nil
	}
	rawAuthorTok := strings.TrimSpace(beforeDate[authorSep+len(sep):])
	rawDateTok := strings.TrimSpace(rest[dateSep+len(sep):])
	if !strings.HasPrefix(rawAuthorTok, "`") || !strings.HasSuffix(rawAuthorTok, "`") ||
		!strings.HasPrefix(rawDateTok, "`") || !strings.HasSuffix(rawDateTok, "`") {
		return domain.TeamRule{}, false, nil
	}
	return parseCompactRuleBody(rest, "", scope, category, fileHash, true)
}

func parseCompactRuleBody(body, explicitKey, scope, category, fileHash string, pinned bool) (domain.TeamRule, bool, error) {
	var sub string
	if strings.HasPrefix(body, "(`") {
		if endSub := strings.Index(body[2:], "`)"); endSub > 0 {
			if cand := domain.NormalizeSubCategory(body[2 : 2+endSub]); cand != "" {
				sub = cand
				body = strings.TrimSpace(body[2+endSub+2:])
			}
		}
	}

	const sep = " · "
	dateSep := strings.LastIndex(body, sep)
	if dateSep <= 0 {
		return domain.TeamRule{}, true, errors.New("team rule must end with ' · `@author` · `YYYY-MM-DD`'")
	}
	beforeDate := body[:dateSep]
	authorSep := strings.LastIndex(beforeDate, sep)
	if authorSep <= 0 {
		return domain.TeamRule{}, true, errors.New("team rule must end with ' · `@author` · `YYYY-MM-DD`'")
	}

	title := strings.TrimSpace(beforeDate[:authorSep])
	addedBy := domain.NormalizeGitHandle(stripBackticks(beforeDate[authorSep+len(sep):]))
	addedAt := stripBackticks(body[dateSep+len(sep):])
	if _, err := time.Parse("2006-01-02", addedAt); err != nil {
		return domain.TeamRule{}, true, fmt.Errorf("team rule date %q must follow YYYY-MM-DD", addedAt)
	}
	key := explicitKey
	if key == "" {
		key = domain.ComputeRuleKey(category, title)
	}

	return domain.TeamRule{
		RuleKey:     key,
		Scope:       domain.NormalizeScope(scope),
		Category:    category,
		SubCategory: sub,
		Pinned:      pinned,
		Title:       title,
		AddedBy:     addedBy,
		AddedAt:     addedAt,
		FileHash:    fileHash,
	}, true, nil
}
