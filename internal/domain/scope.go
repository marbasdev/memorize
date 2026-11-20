package domain

import (
	"context"
	"os"
	"os/user"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"unicode"
)

const DefaultPresetCategoriesJSON = `{"architecture":["engine","build","api","schema","workflow"],"convention":["style","naming","workflow"],"database":["schema","query","migration"],"diet":["allergy","food"],"preference":["documentation","communication","ui"],"security":["auth","oauth2","guard"],"testing":["unit","e2e"]}`

type presetCategoriesCtxKey struct{}

func WithPresetCategories(ctx context.Context, rawJSON string) context.Context {
	return context.WithValue(ctx, presetCategoriesCtxKey{}, rawJSON)
}

func PresetCategoriesFromContext(ctx context.Context) string {
	if ctx != nil {
		if v, ok := ctx.Value(presetCategoriesCtxKey{}).(string); ok && strings.TrimSpace(v) != "" {
			return v
		}
	}
	return DefaultPresetCategoriesJSON
}

type accountCtxKey struct{}

func WithAccount(ctx context.Context, account string) context.Context {
	acc := NormalizeAccount(account)
	if acc == "" {
		return ctx
	}
	return context.WithValue(ctx, accountCtxKey{}, acc)
}

func AccountFromContext(ctx context.Context) string {
	if ctx != nil {
		if v, ok := ctx.Value(accountCtxKey{}).(string); ok && strings.TrimSpace(v) != "" {
			return NormalizeAccount(v)
		}
	}
	return ""
}

type adminBypassCtxKey struct{}

func WithAdminBypass(ctx context.Context) context.Context {
	return context.WithValue(ctx, adminBypassCtxKey{}, true)
}

func IsAdminContext(ctx context.Context) bool {
	if ctx != nil {
		if v, ok := ctx.Value(adminBypassCtxKey{}).(bool); ok {
			return v
		}
	}
	return false
}

type progressSessionCtxKey struct{}

func WithSessionID(ctx context.Context, sessionID string) context.Context {
	return context.WithValue(ctx, progressSessionCtxKey{}, strings.TrimSpace(sessionID))
}

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

func WithVerifySource(ctx context.Context, src string) context.Context {
	return context.WithValue(ctx, verifySourceCtxKey{}, strings.TrimSpace(src))
}

func VerifySourceFromContext(ctx context.Context) string {
	if ctx != nil {
		if v, ok := ctx.Value(verifySourceCtxKey{}).(string); ok && strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return "mcp:verify_plan"
}

func NormalizeAccount(raw string) string {
	s := strings.ToLower(strings.TrimSpace(raw))
	s = strings.ReplaceAll(s, " ", "_")
	return s
}

func DefaultAccount() string {
	for _, k := range []string{"PRAXIS_ACCOUNT", "AGY_ACCOUNT", "USER", "USERNAME"} {
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

func DefaultScope() string {
	if env := strings.TrimSpace(os.Getenv("PRAXIS_DEFAULT_SCOPE")); env != "" {
		return NormalizeScope(env)
	}
	if cwdScope := SafeCwdScope(); cwdScope != "" {
		return cwdScope
	}
	return "personal"
}

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

func NormalizeSubCategory(raw string) string {
	k := strings.ToLower(strings.TrimSpace(raw))
	k = strings.Trim(k, "/")
	k = strings.ReplaceAll(k, " ", "_")
	return k
}

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

func IsCJKRune(r rune) bool {
	return unicode.Is(unicode.Hangul, r) || unicode.Is(unicode.Han, r)
}

func ContainsCJKRune(s string) bool {
	for _, r := range s {
		if IsCJKRune(r) {
			return true
		}
	}
	return false
}

func CanonicalPathBase(rawPath string) string {
	s := strings.TrimSpace(strings.ReplaceAll(rawPath, "\\", "/"))
	s = strings.TrimRight(s, "/")
	if s == "" || (len(s) == 2 && s[1] == ':') {
		return ""
	}
	b := path.Base(s)
	if b == "" || b == "." || b == "/" {
		return ""
	}
	return b
}

func CanonicalProjectSlugFromRemote(rawURL string) string {
	u := strings.TrimSpace(rawURL)
	if u == "" {
		return ""
	}
	u = strings.TrimRight(u, "/")
	if !strings.Contains(u, "://") {
		if idx := strings.LastIndex(u, ":"); idx >= 0 && idx+1 < len(u) {
			u = u[idx+1:]
		}
	}
	base := CanonicalPathBase(u)
	base = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(base)), ".git")
	base = strings.ReplaceAll(base, " ", "-")
	return base
}
