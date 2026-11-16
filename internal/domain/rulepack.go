package domain

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"time"
)

const (
	// RulePackRelPath is the repository-relative path of the shared declarative Team Rule Pack document.
	RulePackRelPath = ".memorize/TEAM_RULES.md"

	RuleOriginLocal = "local"
	RuleOriginTeam  = "team"

	RuleGitModePush = "push"

	RuleStateAdopted         = "adopted"
	RuleStateLocalOverride   = "local_override"
	RuleStateConflictPending = "conflict_pending"
)

// TeamRule represents one declarative team-agreed rule stored in .memorize/TEAM_RULES.md and cached in memory.
type TeamRule struct {
	RuleKey           string `json:"rule_key"`
	Scope             string `json:"scope,omitempty"`
	Category          string `json:"category"`
	SubCategory       string `json:"sub_category,omitempty"`
	Pinned            bool   `json:"pinned"`
	Title             string `json:"title"`
	Why               string `json:"why"`
	How               string `json:"how"`
	Where             string `json:"where,omitempty"`
	AddedBy           string `json:"added_by"`
	AddedAt           string `json:"added_at"`
	FileHash          string `json:"file_hash,omitempty"`
	SortOrder         int    `json:"sort_order,omitempty"`
	Resolution        string `json:"resolution,omitempty"`
	LocalOverrideNote string `json:"local_override_note,omitempty"`
}

// DisplayContent builds a compact evaluator-ready rule statement combining What, Why, and How.
func (r TeamRule) DisplayContent() string {
	var b strings.Builder
	b.WriteString(strings.TrimSpace(r.Title))
	if why := strings.TrimSpace(r.Why); why != "" {
		b.WriteString(" — Why: ")
		b.WriteString(why)
	}
	if how := strings.TrimSpace(r.How); how != "" {
		b.WriteString(" | How: ")
		b.WriteString(how)
	}
	if where := strings.TrimSpace(r.Where); where != "" {
		b.WriteString(" | Where: ")
		b.WriteString(where)
	}
	return b.String()
}

// ToFact projects a TeamRule into a read-only domain.Fact for evaluation and guardrail checks.
func (r TeamRule) ToFact(virtualID int64) Fact {
	return Fact{
		ID:          virtualID,
		Scope:       r.Scope,
		Origin:      RuleOriginTeam,
		RuleKey:     r.RuleKey,
		Category:    r.Category,
		SubCategory: r.SubCategory,
		Content:     r.DisplayContent(),
		Pinned:      true,
	}
}

// RulePlacementProposal holds an AI-summarized Team Rule entry plus internal surgical placement hints.
type RulePlacementProposal struct {
	RuleKey     string `json:"rule_key,omitempty"`
	Category    string `json:"category"`
	SubCategory string `json:"sub_category,omitempty"`
	Pinned      bool   `json:"pinned,omitempty"`
	Title       string `json:"title"`
	Why         string `json:"why,omitempty"`
	How         string `json:"how,omitempty"`
	Where       string `json:"where,omitempty"`
	AddedBy     string `json:"added_by"`
	AddedAt     string `json:"added_at"`
	AfterKey    string `json:"after_key,omitempty"`
	BaseHash    string `json:"base_hash,omitempty"`
}

// ToTeamRule converts a placement proposal into a normalized TeamRule bound to scope.
func (p RulePlacementProposal) ToTeamRule(scope string) TeamRule {
	sc := NormalizeScope(scope)
	cat := NormalizeCategoryForScope(sc, p.Category)
	title := cleanSingleLine(p.Title)
	key := ComputeRuleKey(cat, title)
	addedBy := NormalizeGitHandle(p.AddedBy)
	if addedBy == "" {
		addedBy = cleanSingleLine(p.AddedBy)
	}
	return TeamRule{
		RuleKey:     key,
		Scope:       sc,
		Category:    cat,
		SubCategory: NormalizeSubCategory(p.SubCategory),
		Pinned:      true,
		Title:       title,
		Why:         cleanSingleLine(p.Why),
		How:         cleanSingleLine(p.How),
		Where:       cleanSingleLine(p.Where),
		AddedBy:     addedBy,
		AddedAt:     strings.TrimSpace(p.AddedAt),
	}
}

// RuleOutlineEntry is an internal-key and title index item used for surgical insertion ordering.
type RuleOutlineEntry struct {
	RuleKey     string `json:"rule_key"`
	Category    string `json:"category"`
	SubCategory string `json:"sub_category,omitempty"`
	Pinned      bool   `json:"pinned,omitempty"`
	Title       string `json:"title"`
}

// RulePackConflict records a policy clash between an incoming TeamRule and an existing Local Memory.
type RulePackConflict struct {
	Scope         string   `json:"scope"`
	RuleKey       string   `json:"rule_key"`
	TeamRule      TeamRule `json:"team_rule"`
	LocalMemoryID int64    `json:"local_memory_id"`
	LocalContent  string   `json:"local_content"`
	Reason        string   `json:"reason,omitempty"`
	Status        string   `json:"status"`
	UpdatedAt     string   `json:"updated_at,omitempty"`
}

// RuleDuplicateSuggestion describes a local memory redundant with or partially overlapping active Team Rules.
type RuleDuplicateSuggestion struct {
	LocalMemoryID  int64  `json:"local_memory_id"`
	LocalCategory  string `json:"local_category"`
	LocalContent   string `json:"local_content"`
	TeamRuleKey    string `json:"team_rule_key"`
	TeamRuleTitle  string `json:"team_rule_title"`
	Kind           string `json:"kind"` // "duplicate" | "partial_overlap"
	TrimmedContent string `json:"trimmed_content,omitempty"`
	Reason         string `json:"reason,omitempty"`
}

func cleanSingleLine(s string) string {
	lines := strings.Fields(strings.TrimSpace(s))
	return strings.Join(lines, " ")
}

// NormalizeRuleKey canonicalizes a string into a safe lowercase kebab/snake rule slug ([a-z0-9][a-z0-9_-]*).
func NormalizeRuleKey(raw string) string {
	s := strings.ToLower(strings.TrimSpace(raw))
	var b strings.Builder
	b.Grow(len(s))
	lastDash := false
	for _, r := range s {
		switch {
		case (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9'):
			b.WriteRune(r)
			lastDash = false
		case r == '-' || r == '_' || r == ' ' || r == '/' || r == '.':
			if b.Len() > 0 && !lastDash {
				if r == '_' {
					b.WriteByte('_')
				} else {
					b.WriteByte('-')
				}
				lastDash = true
			}
		}
	}
	out := strings.Trim(b.String(), "-_")
	if len(out) > 64 {
		out = strings.TrimRight(out[:64], "-_")
	}
	return out
}

// ComputeRuleKey derives a deterministic, language-agnostic internal identifier
// from canonical category and normalized single-line title.
func ComputeRuleKey(category, title string) string {
	cat := NormalizeCategoryForScope("", category)
	normTitle := strings.ToLower(cleanSingleLine(title))
	sum := sha256.Sum256([]byte(cat + "\x00" + normTitle))
	return "r-" + hex.EncodeToString(sum[:5])
}

// SuggestRuleKey generates a deterministic internal rule key from category and seed text.
func SuggestRuleKey(category, seed string) string {
	catPrefix := NormalizeRuleKey(category)
	slug := NormalizeRuleKey(seed)
	if slug != "" {
		if catPrefix != "" && !strings.HasPrefix(slug, catPrefix+"-") {
			combined := catPrefix + "-" + slug
			if len(combined) <= 64 {
				return combined
			}
		}
		return slug
	}
	sum := sha256.Sum256([]byte(strings.TrimSpace(category) + "|" + strings.TrimSpace(seed)))
	shortHex := hex.EncodeToString(sum[:4])
	if catPrefix == "" {
		catPrefix = "rule"
	}
	return NormalizeRuleKey(catPrefix + "-" + shortHex)
}

// NormalizeGitHandle formats an author attribution string into a clean Git '@handle' (no spaces/backticks/'·').
func NormalizeGitHandle(raw string) string {
	cleaned := strings.NewReplacer("`", "", "·", "", "\r", " ", "\n", " ").Replace(strings.TrimSpace(raw))
	cleaned = strings.TrimLeft(cleaned, "@")
	fields := strings.Fields(cleaned)
	if len(fields) == 0 {
		return ""
	}
	handle := strings.Join(fields, "-")
	handle = strings.TrimLeft(handle, "@")
	if handle == "" {
		return ""
	}
	return "@" + handle
}

// ValidateTeamRule checks formatting, completeness, inline-backtick safety, and security guards on r.
func ValidateTeamRule(r TeamRule) error {
	key := strings.TrimSpace(r.RuleKey)
	if key == "" || NormalizeRuleKey(key) != key {
		return fmt.Errorf("invalid rule_key %q (must match [a-z0-9][a-z0-9_-]{0,63})", r.RuleKey)
	}
	if strings.Contains(key, "`") {
		return fmt.Errorf("rule_key cannot contain backticks")
	}
	cat := strings.TrimSpace(r.Category)
	if cat == "" {
		return fmt.Errorf("rule %q category cannot be empty", key)
	}
	if strings.Contains(cat, "`") || strings.Contains(r.SubCategory, "`") {
		return fmt.Errorf("rule %q category/sub_category cannot contain backticks", key)
	}
	title := strings.TrimSpace(r.Title)
	if title == "" {
		return fmt.Errorf("rule %q title cannot be empty", key)
	}
	if strings.Contains(title, "`[invariant]`") || strings.Contains(title, "`[rule]`") {
		return fmt.Errorf("rule %q title must not embed tier tags", key)
	}
	if strings.Contains(r.Where, "`") {
		return fmt.Errorf("rule %q where pattern cannot contain backticks", key)
	}
	author := strings.TrimSpace(r.AddedBy)
	if author == "" {
		return fmt.Errorf("rule %q requires Added Git author (@handle)", key)
	}
	if strings.Contains(author, "`") || strings.Contains(author, "·") {
		return fmt.Errorf("rule %q added_by cannot contain backticks or '·'", key)
	}
	addedAt := strings.TrimSpace(r.AddedAt)
	if _, err := time.Parse("2006-01-02", addedAt); err != nil {
		return fmt.Errorf("rule %q added_at %q must follow YYYY-MM-DD", key, r.AddedAt)
	}
	for _, field := range []struct {
		name, val string
	}{
		{"title", title},
		{"why", r.Why},
		{"how", r.How},
		{"where", r.Where},
		{"added_by", author},
	} {
		if reason := CheckSecurityGuard(field.val); reason != "" {
			return fmt.Errorf("rule %q %s rejected: %s", key, field.name, reason)
		}
	}
	return nil
}

var canonicalCategoryRanks = map[string]int{
	"architecture": 10,
	"security":     20,
	"database":     30,
	"api":          40,
	"testing":      50,
	"convention":   60,
	"workflow":     70,
	"preference":   80,
	"general":      90,
}

// CanonicalCategoryRank returns the fixed top-level section ordering priority for Markdown Rule Pack generation.
func CanonicalCategoryRank(cat string) int {
	k := strings.ToLower(strings.TrimSpace(cat))
	if r, ok := canonicalCategoryRanks[k]; ok {
		return r
	}
	return 100
}

// FormatCategoryHeadingTitle converts a category slug into a readable Markdown heading title.
func FormatCategoryHeadingTitle(slug string) string {
	k := strings.ToLower(strings.TrimSpace(slug))
	if k == "" {
		return "General"
	}
	if k == "api" {
		return "API"
	}
	parts := strings.Fields(strings.NewReplacer("_", " ", "-", " ").Replace(k))
	for i, p := range parts {
		if len(p) > 0 {
			parts[i] = strings.ToUpper(p[:1]) + p[1:]
		}
	}
	return strings.Join(parts, " ")
}
