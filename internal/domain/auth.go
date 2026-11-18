package domain

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

const (
	// RoleAdmin identifies the host administrator with engine, threshold, global-rule, and user management access.
	RoleAdmin = "admin"
	// RoleMember identifies a standard team collaborator with project and personal memory access.
	RoleMember = "member"

	// DefaultServeAddr is the default network bind address used by `memorize serve`.
	DefaultServeAddr = "0.0.0.0:18095"
	// SessionCookieName is the HttpOnly session cookie used by dashboard web login in server mode.
	SessionCookieName = "memorize_session"
	// DefaultSessionTTL is the unified lifetime applied to both signed session payloads and browser session cookies.
	DefaultSessionTTL = 7 * 24 * time.Hour
	// LangCookieName stores the user's selected dashboard language preference.
	LangCookieName = "memorize_lang"
	// LangCookieMaxAge defines how long the dashboard language preference cookie persists.
	LangCookieMaxAge = 365 * 24 * time.Hour
	// AuthTokenPrefix identifies plaintext team server access tokens before hashing.
	AuthTokenPrefix = "mem_"
	// MaxRemoteRulePackDocBytes caps per-hook TEAM_RULES.md payload snapshots sent from thin clients.
	MaxRemoteRulePackDocBytes = 256 * 1024
)

// AuthPrincipal describes the authenticated caller identity and server mode policy for the current request.
type AuthPrincipal struct {
	Account       string
	Role          string
	TokenHint     string
	ServerMode    bool
	Authenticated bool
}

type authPrincipalCtxKey struct{}

// WithAuthPrincipal attaches an AuthPrincipal to ctx.
func WithAuthPrincipal(ctx context.Context, p AuthPrincipal) context.Context {
	p.Account = NormalizeAccount(p.Account)
	if p.Role != "" {
		p.Role = NormalizeRole(p.Role)
	}
	return context.WithValue(ctx, authPrincipalCtxKey{}, p)
}

// AuthPrincipalFromContext extracts the AuthPrincipal from ctx if present.
func AuthPrincipalFromContext(ctx context.Context) (AuthPrincipal, bool) {
	if ctx != nil {
		if v, ok := ctx.Value(authPrincipalCtxKey{}).(AuthPrincipal); ok {
			return v, true
		}
	}
	return AuthPrincipal{}, false
}

// IsServerMode reports whether ctx is executing inside authenticated multi-user team server mode.
func IsServerMode(ctx context.Context) bool {
	p, ok := AuthPrincipalFromContext(ctx)
	return ok && p.ServerMode
}

// CanAccessAdminFeatures reports whether ctx holds operator/admin authority.
// Local solo mode (ServerMode == false) always allows full local console access;
// team server mode requires an authenticated admin principal.
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

// CanModifyScope verifies whether the caller is allowed to mutate rules in scope.
func CanModifyScope(ctx context.Context, rawScope string) error {
	p, ok := AuthPrincipalFromContext(ctx)
	if !ok || !p.ServerMode {
		return nil
	}
	if !p.Authenticated || p.Account == "" {
		return errors.New("authentication required")
	}
	if NormalizeScope(rawScope) == "global" && p.Role != RoleAdmin {
		return errors.New("permission denied: only admins can modify global rules")
	}
	return nil
}

// RequireAdmin verifies admin authority when running in server mode.
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

// NormalizeRole canonicalizes a user role into "admin" or "member".
func NormalizeRole(raw string) string {
	if strings.EqualFold(strings.TrimSpace(raw), RoleAdmin) {
		return RoleAdmin
	}
	return RoleMember
}

// ValidateAccountName normalizes and validates a server user account slug.
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

// UserAccount represents one registered team server account stored in SQLite users.
type UserAccount struct {
	Account    string `json:"account"`
	Role       string `json:"role"`
	TokenHint  string `json:"token_hint"`
	Active     bool   `json:"active"`
	CreatedAt  string `json:"created_at"`
	LastSeenAt string `json:"last_seen_at,omitempty"`
}

// UserTokenIssue pairs a UserAccount with its newly minted plaintext token (returned once on create/rotate).
type UserTokenIssue struct {
	User       UserAccount `json:"user"`
	PlainToken string      `json:"token"`
}

// RemoteTargetConfig binds a local repository (or default fallback) to a remote memorize server.
type RemoteTargetConfig struct {
	URL     string `json:"url"`
	Token   string `json:"token"`
	Account string `json:"account,omitempty"`
	Role    string `json:"role,omitempty"`
}

// HookDiffSection carries one client-computed file diff section inside remote Thin Client hook requests.
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

// WithClientHookContext attaches client-resolved Thin Client metadata to ctx.
func WithClientHookContext(ctx context.Context, cc *ClientHookContext) context.Context {
	if cc == nil {
		return ctx
	}
	return context.WithValue(ctx, clientHookCtxKey{}, cc)
}

// ClientHookContextFromContext retrieves client-resolved Thin Client metadata from ctx if present.
func ClientHookContextFromContext(ctx context.Context) (*ClientHookContext, bool) {
	if ctx != nil {
		if cc, ok := ctx.Value(clientHookCtxKey{}).(*ClientHookContext); ok && cc != nil {
			return cc, true
		}
	}
	return nil, false
}

// IsFileEditTool reports whether tool name performs direct file writes/edits (deferred to Stop verification).
func IsFileEditTool(name string) bool {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "edit_file", "write_file", "write_to_file",
		"replace_file_content", "multi_replace_file_content",
		"edit", "write", "multiedit", "notebookedit":
		return true
	}
	return false
}

// ExtractTargetFilePath extracts the file path argument from a coding agent's tool input payload.
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

// FormatTokenHint returns a masked display preview for a bearer token without exposing the secret.
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
