package mcp

import (
	"context"
	"sync"
	"time"

	"github.com/mark3labs/mcp-go/server"

	"memorize/internal/modules/guardrail"
	"memorize/internal/modules/memory"
	"memorize/internal/modules/progress"
)

// ServerInstructions holds the protocol guidance sent to MCP clients during initialize.
const ServerInstructions = `Memorize Protocol:
1. Scope: 'project:<repo>', 'global', or 'personal' (auto-detected if omitted).
2. Query: call query_project_memory on new topics (refresh=true after compaction). Follow 'memorize://playbook/memory-curator' on Git onboarding/sync.
3. Commit Before Mutating: commit user decisions/approvals atomically (1 behavior = 1 item in candidate_facts, user's language) via commit_memory or manage_memory(update) BEFORE edits/commands; commit new choices before finishing.
4. Onboarding: if [ONBOARDING AVAILABLE], ask user first; call manage_memory(skip_onboarding) if declined, or inspect files, resolve conflicts, commit, and offer cleanup.
5. SSOT: active memories replace static rule files (CLAUDE.md/AGENTS.md) and override older turns; ignore (Replaces: ...) and [REVOKED ...].
6. Progress: use project_progress for work tracks ('planned' unstarted, 'in_progress' active, 'blocked' halted with blocker in next_step, 'done').`

// BridgeReqMeta carries per-request metadata forwarded by the stateless stdio HTTP bridge.
type BridgeReqMeta struct {
	SessionID string
	Cwd       string
	Roots     []string
}

type bridgeMetaCtxKey struct{}

// WithBridgeMeta attaches stateless stdio bridge metadata to ctx.
func WithBridgeMeta(ctx context.Context, m BridgeReqMeta) context.Context {
	return context.WithValue(ctx, bridgeMetaCtxKey{}, m)
}

// BridgeMetaFromContext retrieves stateless stdio bridge metadata attached by WithBridgeMeta.
func BridgeMetaFromContext(ctx context.Context) (BridgeReqMeta, bool) {
	if ctx != nil {
		if v, ok := ctx.Value(bridgeMetaCtxKey{}).(BridgeReqMeta); ok {
			return v, true
		}
	}
	return BridgeReqMeta{}, false
}

// Deps bundles the module services and binary version wired into MCP handlers.
type Deps struct {
	Memory    *memory.Service
	Progress  *progress.Service
	Guardrail *guardrail.Service
	Version   string
}

// NewServer constructs an MCPServer configured with Memorize resources, prompts, and tools.
func NewServer(deps Deps) *server.MCPServer {
	ver := deps.Version
	if ver == "" {
		ver = "1.0.1"
	}
	srv := server.NewMCPServer("memorize", ver,
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

// SessionPool bounds in-process MCP sessions registered for stateless HTTP bridge callers.
type SessionPool struct {
	srv      *server.MCPServer
	mu       sync.Mutex
	sessions map[string]*bridgeSessionEntry
}

// NewSessionPool allocates a bounded SessionPool attached to srv.
func NewSessionPool(srv *server.MCPServer) *SessionPool {
	return &SessionPool{
		srv:      srv,
		sessions: make(map[string]*bridgeSessionEntry),
	}
}

// GetOrCreate returns the registered InProcessSession for sid, evicting stale entries when over capacity.
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

// ServeStdio blocks serving MCP JSON-RPC over stdin/stdout.
func ServeStdio(srv *server.MCPServer) error {
	return server.ServeStdio(srv)
}
