package domain

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

const (
	RoleAdmin  = "admin"
	RoleMember = "member"

	DefaultServeAddr          = "0.0.0.0:18095"
	SessionCookieName         = "memorize_session"
	AccountsCookieName        = "memorize_accounts"
	MaxSwitcherAccounts       = 10
	DefaultSessionTTL         = 7 * 24 * time.Hour
	LangCookieName            = "memorize_lang"
	LangCookieMaxAge          = 365 * 24 * time.Hour
	AuthTokenPrefix           = "mem_"
	MaxRemoteRulePackDocBytes = 256 * 1024
)

type AuthPrincipal struct {
	Account       string
	Role          string
	TokenHint     string
	ServerMode    bool
	Authenticated bool
}

type authPrincipalCtxKey struct{}

func WithAuthPrincipal(ctx context.Context, p AuthPrincipal) context.Context {
	p.Account = NormalizeAccount(p.Account)
	if p.Role != "" {
		p.Role = NormalizeRole(p.Role)
	}
	return context.WithValue(ctx, authPrincipalCtxKey{}, p)
}

func AuthPrincipalFromContext(ctx context.Context) (AuthPrincipal, bool) {
	if ctx != nil {
		if v, ok := ctx.Value(authPrincipalCtxKey{}).(AuthPrincipal); ok {
			return v, true
		}
	}
	return AuthPrincipal{}, false
}

func IsServerMode(ctx context.Context) bool {
	p, ok := AuthPrincipalFromContext(ctx)
	return ok && p.ServerMode
}

func CanAccessAdminFeatures(ctx context.Context) bool {
	p, ok := AuthPrincipalFromContext(ctx)
	if !ok || !p.ServerMode {
		return true
	}
	return p.Authenticated && p.Role == RoleAdmin
}

// EffectivePrincipalAccount returns the verified account slug when running in authenticated server mode, or empty string otherwise.
func EffectivePrincipalAccount(ctx context.Context) string {
	p, ok := AuthPrincipalFromContext(ctx)
	if !ok || !p.ServerMode || !p.Authenticated {
		return ""
	}
	return p.Account
}

func IsUserPrivateScope(ctx context.Context, rawScope string) bool {
	norm := NormalizeScope(rawScope)
	return norm == "personal" || (IsServerMode(ctx) && norm == "global")
}

func CanModifyScope(ctx context.Context, rawScope string) error {
	p, ok := AuthPrincipalFromContext(ctx)
	if !ok || !p.ServerMode {
		return nil
	}
	if !p.Authenticated || p.Account == "" {
		return errors.New("authentication required")
	}
	return nil
}

func RequireAdmin(ctx context.Context) error {
	if CanAccessAdminFeatures(ctx) {
		return nil
	}
	p, ok := AuthPrincipalFromContext(ctx)
	if !ok || !p.Authenticated {
		return errors.New("authentication required")
	}
	return errors.New("permission denied: admin role required")
}

// ScopedSessionID namespaces rawSID by authenticated account in server mode so session caches,
// session_sent deltas, and turn_state rows cannot collide across teammates.
func ScopedSessionID(ctx context.Context, rawSID string) string {
	sid := strings.TrimSpace(rawSID)
	if sid == "" {
		return ""
	}
	p, ok := AuthPrincipalFromContext(ctx)
	if !ok || !p.ServerMode || p.Account == "" {
		return sid
	}
	prefix := "u:" + p.Account + ":"
	if strings.HasPrefix(sid, prefix) {
		return sid
	}
	return prefix + sid
}

func NormalizeRole(raw string) string {
	if strings.EqualFold(strings.TrimSpace(raw), RoleAdmin) {
		return RoleAdmin
	}
	return RoleMember
}

func ValidateAccountName(raw string) (string, error) {
	acc := NormalizeAccount(raw)
	if acc == "" || len(acc) > 48 {
		return "", fmt.Errorf("account name must be 1..48 characters")
	}
	for i := 0; i < len(acc); i++ {
		c := acc[i]
		isLower := c >= 'a' && c <= 'z'
		isDigit := c >= '0' && c <= '9'
		isSep := c == '.' || c == '_' || c == '-'
		if !isLower && !isDigit && !isSep {
			return "", fmt.Errorf("invalid account character %q (allowed: a-z, 0-9, ., _, -)", c)
		}
		if i == 0 && isSep {
			return "", fmt.Errorf("account name must start with a letter or digit")
		}
	}
	return acc, nil
}

type UserAccount struct {
	Account    string `json:"account"`
	Role       string `json:"role"`
	TokenHint  string `json:"token_hint"`
	Active     bool   `json:"active"`
	CreatedAt  string `json:"created_at"`
	LastSeenAt string `json:"last_seen_at,omitempty"`
}

type UserTokenIssue struct {
	User       UserAccount `json:"user"`
	PlainToken string      `json:"token"`
}

type RemoteTargetConfig struct {
	URL     string `json:"url"`
	Token   string `json:"token"`
	Account string `json:"account,omitempty"`
	Role    string `json:"role,omitempty"`
}

type HookDiffSection struct {
	Scope string `json:"scope,omitempty"`
	Label string `json:"label"`
	Body  string `json:"body"`
}

// ClientHookContext carries client-resolved repository metadata, diffs, and local docs so remote servers
// never need filesystem access to the client's machine.
type ClientHookContext struct {
	CanonicalScopes   []string                    `json:"canonical_scopes,omitempty"`
	TargetScope       string                      `json:"target_scope,omitempty"`
	TargetRelPath     string                      `json:"target_rel_path,omitempty"`
	RemoteManagedEdit bool                        `json:"remote_managed_edit,omitempty"`
	DiffSections      []HookDiffSection           `json:"diff_sections,omitempty"`
	StagedDiffs       []HookDiffSection           `json:"staged_diffs,omitempty"`
	GitHeadByScope    map[string]string           `json:"git_head_by_scope,omitempty"`
	GitCommitsByScope map[string][]GitCommitBrief `json:"git_commits_by_scope,omitempty"`
	TeamRuleDocs      map[string]string           `json:"team_rule_docs,omitempty"`
}

type clientHookCtxKey struct{}

func WithClientHookContext(ctx context.Context, cc *ClientHookContext) context.Context {
	if cc == nil {
		return ctx
	}
	return context.WithValue(ctx, clientHookCtxKey{}, cc)
}

func ClientHookContextFromContext(ctx context.Context) (*ClientHookContext, bool) {
	if ctx != nil {
		if cc, ok := ctx.Value(clientHookCtxKey{}).(*ClientHookContext); ok && cc != nil {
			return cc, true
		}
	}
	return nil, false
}

func IsFileEditTool(name string) bool {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "edit_file", "write_file", "write_to_file",
		"replace_file_content", "multi_replace_file_content",
		"edit", "write", "multiedit", "notebookedit":
		return true
	}
	return false
}

func ExtractTargetFilePath(toolIn map[string]any) string {
	for _, k := range []string{"file_path", "Path", "path", "TargetFile", "target_file", "notebook_path"} {
		if v, ok := toolIn[k]; ok {
			if s, ok := v.(string); ok && strings.TrimSpace(s) != "" {
				return strings.TrimSpace(s)
			}
		}
	}
	return ""
}

func FormatTokenHint(plain string) string {
	p := strings.TrimSpace(plain)
	if len(p) <= 12 {
		if len(p) <= 4 {
			return "****"
		}
		return p[:4] + "..."
	}
	return p[:8] + "..." + p[len(p)-4:]
}
