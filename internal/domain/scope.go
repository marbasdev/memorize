package domain

import (
	"context"
	"os"
	"os/user"
	"path/filepath"
	"sort"
	"strings"
	"unicode"
)

// DefaultPresetCategoriesJSON defines the default category -> sub-categories taxonomy
// used by the Web Dashboard and LLM Gatekeeper when not yet customized in SQLite config.
const DefaultPresetCategoriesJSON = `{"architecture":["engine","build","api","schema","workflow"],"convention":["style","naming","workflow"],"database":["schema","query","migration"],"diet":["allergy","food"],"preference":["documentation","communication","ui"],"security":["auth","oauth2","guard"],"testing":["unit","e2e"]}`

type presetCategoriesCtxKey struct{}

// WithPresetCategories attaches customized category taxonomy JSON to ctx.
func WithPresetCategories(ctx context.Context, rawJSON string) context.Context {
	return context.WithValue(ctx, presetCategoriesCtxKey{}, rawJSON)
}

// PresetCategoriesFromContext extracts preset taxonomy JSON from ctx, falling back to DefaultPresetCategoriesJSON.
func PresetCategoriesFromContext(ctx context.Context) string {
	if ctx != nil {
		if v, ok := ctx.Value(presetCategoriesCtxKey{}).(string); ok && strings.TrimSpace(v) != "" {
			return v
		}
	}
	return DefaultPresetCategoriesJSON
}

type accountCtxKey struct{}

// WithAccount binds a normalized account name to ctx for personal-scope isolation.
func WithAccount(ctx context.Context, account string) context.Context {
	acc := NormalizeAccount(account)
	if acc == "" {
		return ctx
	}
	return context.WithValue(ctx, accountCtxKey{}, acc)
}

// AccountFromContext extracts the normalized account name from ctx if present.
func AccountFromContext(ctx context.Context) string {
	if ctx != nil {
		if v, ok := ctx.Value(accountCtxKey{}).(string); ok && strings.TrimSpace(v) != "" {
			return NormalizeAccount(v)
		}
	}
	return ""
}

type adminBypassCtxKey struct{}

// WithAdminBypass marks a context as originating from the local operator Dashboard UI,
// allowing cross-account inspection and maintenance while MCP/CLI calls remain strictly isolated.
func WithAdminBypass(ctx context.Context) context.Context {
	return context.WithValue(ctx, adminBypassCtxKey{}, true)
}

// IsAdminContext reports whether ctx carries dashboard operator bypass privileges.
func IsAdminContext(ctx context.Context) bool {
	if ctx != nil {
		if v, ok := ctx.Value(adminBypassCtxKey{}).(bool); ok {
			return v
		}
	}
	return false
}

type progressSessionCtxKey struct{}

// WithSessionID attaches an IDE / MCP conversation session ID to ctx so progress mutations can record it automatically.
func WithSessionID(ctx context.Context, sessionID string) context.Context {
	return context.WithValue(ctx, progressSessionCtxKey{}, strings.TrimSpace(sessionID))
}

// SessionIDFromContext extracts the attached session ID from ctx if present.
func SessionIDFromContext(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	if v, ok := ctx.Value(progressSessionCtxKey{}).(string); ok {
		return strings.TrimSpace(v)
	}
	return ""
}

type verifySourceCtxKey struct{}

// WithVerifySource tags a VerifyPlan call with its caller source (e.g. "ui:verify" vs default "mcp:verify_plan").
func WithVerifySource(ctx context.Context, src string) context.Context {
	return context.WithValue(ctx, verifySourceCtxKey{}, strings.TrimSpace(src))
}

// VerifySourceFromContext returns the caller source for VerifyPlan, defaulting to "mcp:verify_plan".
func VerifySourceFromContext(ctx context.Context) string {
	if ctx != nil {
		if v, ok := ctx.Value(verifySourceCtxKey{}).(string); ok && strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return "mcp:verify_plan"
}

// NormalizeAccount canonicalizes account identifiers into lowercase underscore-separated slugs.
func NormalizeAccount(raw string) string {
	s := strings.ToLower(strings.TrimSpace(raw))
	s = strings.ReplaceAll(s, " ", "_")
	return s
}

// DefaultAccount resolves the default personal account name from environment or OS user.
func DefaultAccount() string {
	for _, k := range []string{"MEMORY_ACCOUNT", "AGY_ACCOUNT", "USER", "USERNAME"} {
		if v := NormalizeAccount(os.Getenv(k)); v != "" {
			return v
		}
	}
	if u, err := user.Current(); err == nil && u != nil {
		if v := NormalizeAccount(u.Username); v != "" {
			return v
		}
	}
	return "default"
}

// SafeCwdScope derives a project scope slug from the current working directory if safe.
func SafeCwdScope() string {
	wd, err := os.Getwd()
	if err != nil {
		return ""
	}
	wd = filepath.Clean(wd)
	if wd == "/" || wd == "." {
		return ""
	}
	if home, err := os.UserHomeDir(); err == nil && filepath.Clean(home) == wd {
		return ""
	}
	base := strings.ToLower(strings.TrimSpace(filepath.Base(wd)))
	if base == "" {
		return ""
	}
	return "project:" + base
}

// DefaultScope resolves the fallback scope from MEMORY_DEFAULT_SCOPE, CWD, or "personal".
func DefaultScope() string {
	if env := strings.TrimSpace(os.Getenv("MEMORY_DEFAULT_SCOPE")); env != "" {
		return NormalizeScope(env)
	}
	if cwdScope := SafeCwdScope(); cwdScope != "" {
		return cwdScope
	}
	return "personal"
}

// NormalizeScope canonicalizes user-provided scope strings into "personal", "global", or "project:<slug>".
func NormalizeScope(raw string) string {
	s := strings.ToLower(strings.TrimSpace(raw))
	switch s {
	case "":
		return DefaultScope()
	case "personal", "me", "private", "개인":
		return "personal"
	case "global", "common", "shared", "전역":
		return "global"
	}
	if strings.HasPrefix(s, "project:") {
		name := strings.TrimSpace(strings.TrimPrefix(s, "project:"))
		if name == "" {
			return DefaultScope()
		}
		return "project:" + name
	}
	return "project:" + s
}

// NormalizeCategoryForScope canonicalizes top-level category slugs with scope defaults.
func NormalizeCategoryForScope(scope, raw string) string {
	k := strings.ToLower(strings.TrimSpace(raw))
	if idx := strings.Index(k, "/"); idx > 0 {
		k = strings.TrimSpace(k[:idx])
	}
	k = strings.ReplaceAll(k, " ", "_")
	if k != "" {
		return k
	}
	if NormalizeScope(scope) == "personal" {
		return "preference"
	}
	return "general"
}

// NormalizeSubCategory canonicalizes sub-category slugs.
func NormalizeSubCategory(raw string) string {
	k := strings.ToLower(strings.TrimSpace(raw))
	k = strings.Trim(k, "/")
	k = strings.ReplaceAll(k, " ", "_")
	return k
}

// SplitCategoryAndSub separates combined "cat/sub" strings into normalized (category, sub_category).
func SplitCategoryAndSub(scope, rawCat, rawSub string) (string, string) {
	c := strings.TrimSpace(rawCat)
	sub := strings.TrimSpace(rawSub)
	if idx := strings.Index(c, "/"); idx > 0 {
		if sub == "" {
			sub = c[idx+1:]
		}
		c = c[:idx]
	}
	return NormalizeCategoryForScope(scope, c), NormalizeSubCategory(sub)
}

// NormalizeTags deduplicates, lowercases, and sorts tag slugs.
func NormalizeTags(_ string, raw []string) []string {
	seen := make(map[string]bool, len(raw))
	var out []string
	for _, t := range raw {
		k := strings.ToLower(strings.TrimSpace(t))
		k = strings.ReplaceAll(k, " ", "_")
		if k != "" && !seen[k] {
			seen[k] = true
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}

// IsCJKRune reports whether r is a Hangul or Han rune.
func IsCJKRune(r rune) bool {
	return unicode.Is(unicode.Hangul, r) || unicode.Is(unicode.Han, r)
}

// ContainsCJKRune reports whether s contains at least one Hangul or Han character.
func ContainsCJKRune(s string) bool {
	for _, r := range s {
		if IsCJKRune(r) {
			return true
		}
	}
	return false
}
