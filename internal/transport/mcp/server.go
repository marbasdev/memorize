package mcp

import (
	"context"
	"sync"
	"time"

	"github.com/mark3labs/mcp-go/server"

	"praxis/internal/modules/guardrail"
	"praxis/internal/modules/memory"
	"praxis/internal/modules/progress"
	"praxis/internal/modules/rulepack"
)

const ServerInstructions = `Praxis Protocol:
1. Scope: 'project:<repo>', 'global', or 'personal' (auto-detected if omitted).
2. Query: call query_project_memory on new topics (refresh=true after compaction). Follow 'praxis://playbook/memory-curator' on Git onboarding/sync.
3. Commit Before Mutating: commit user decisions/approvals atomically (1 behavior = 1 item in candidate_facts, user's language) via commit_memory or manage_memory(update) BEFORE edits/commands; commit new choices before finishing.
4. Onboarding: if [ONBOARDING AVAILABLE], ask user first; call manage_memory(skip_onboarding) if declined, or inspect files, resolve conflicts, commit, and offer cleanup.
5. SSOT & Evolution: active rules replace static rule files (CLAUDE.md/AGENTS.md) and older turns; ignore (Replaces: ...) and [REVOKED ...]. Never self-revoke rules silently, and never compromise a clearly better design just to appease an outdated rule—propose updating/forgetting that #ID (or team rule) to the user and update it upon approval.
6. Progress: optionally call project_progress(action="record") after completing notable tasks or milestones to log completed work in the activity feed.
7. Team Rules: use team_rules for shared .praxis/TEAM_RULES.md rules (list/sync/add/promote/update/remove/resolve_conflict/duplicates); NEVER hand-edit TEAM_RULES.md so workspace code stays unmixed.
8. Verify Plan: call verify_plan_against_memory once before multi-file refactors; shell commands gate PreToolUse while file edits batch-verify on Stop.
9. Trial-and-Error Discovery: when a command/build/test fails from an undocumented repo/env constraint (not typos or syntax slips) and you verify a fix, ask in 1 line at reply end whether to save that lesson and call commit_memory only if approved.`

type BridgeReqMeta struct {
	SessionID  string
	Cwd        string
	Roots      []string
	ScopeHints map[string]string
}

type bridgeMetaCtxKey struct{}

func WithBridgeMeta(ctx context.Context, m BridgeReqMeta) context.Context {
	return context.WithValue(ctx, bridgeMetaCtxKey{}, m)
}

func BridgeMetaFromContext(ctx context.Context) (BridgeReqMeta, bool) {
	if ctx != nil {
		if v, ok := ctx.Value(bridgeMetaCtxKey{}).(BridgeReqMeta); ok {
			return v, true
		}
	}
	return BridgeReqMeta{}, false
}

type Deps struct {
	Memory    *memory.Service
	Progress  *progress.Service
	Guardrail *guardrail.Service
	RulePack  *rulepack.Service
	Version   string
}

func NewServer(deps Deps) *server.MCPServer {
	ver := deps.Version
	if ver == "" {
		ver = "1.3.4"
	}
	if deps.Memory != nil && deps.RulePack == nil {
		deps.RulePack = rulepack.New(deps.Memory)
	}
	srv := server.NewMCPServer("praxis", ver,
		server.WithToolCapabilities(false),
		server.WithResourceCapabilities(false, false),
		server.WithPromptCapabilities(false),
		server.WithRoots(),
		server.WithInstructions(ServerInstructions),
	)
	resolver := NewScopeResolver(srv)
	RegisterResourcesAndPrompts(srv)
	RegisterTools(srv, deps, resolver)
	return srv
}

type bridgeSessionEntry struct {
	sess     *server.InProcessSession
	lastSeen time.Time
}

const (
	maxBridgeSessions = 256
	bridgeIdleTTL     = 6 * time.Hour
)

type SessionPool struct {
	srv      *server.MCPServer
	mu       sync.Mutex
	sessions map[string]*bridgeSessionEntry
}

func NewSessionPool(srv *server.MCPServer) *SessionPool {
	return &SessionPool{
		srv:      srv,
		sessions: make(map[string]*bridgeSessionEntry),
	}
}

func (p *SessionPool) GetOrCreate(sid string) *server.InProcessSession {
	now := time.Now()
	p.mu.Lock()
	defer p.mu.Unlock()
	if entry, ok := p.sessions[sid]; ok {
		entry.lastSeen = now
		return entry.sess
	}
	if len(p.sessions) >= maxBridgeSessions {
		var oldestKey string
		var oldestTime time.Time
		for k, v := range p.sessions {
			if now.Sub(v.lastSeen) > bridgeIdleTTL {
				p.srv.UnregisterSession(context.Background(), k)
				delete(p.sessions, k)
				continue
			}
			if oldestKey == "" || v.lastSeen.Before(oldestTime) {
				oldestKey = k
				oldestTime = v.lastSeen
			}
		}
		if len(p.sessions) >= maxBridgeSessions && oldestKey != "" {
			p.srv.UnregisterSession(context.Background(), oldestKey)
			delete(p.sessions, oldestKey)
		}
	}
	s := server.NewInProcessSession(sid, nil)
	s.Initialize()
	_ = p.srv.RegisterSession(context.Background(), s)
	p.sessions[sid] = &bridgeSessionEntry{sess: s, lastSeen: now}
	return s
}

func ServeStdio(srv *server.MCPServer) error {
	return server.ServeStdio(srv)
}
