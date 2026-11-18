package mcp

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"path"
	"strings"
	"sync"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"memorize/internal/domain"
	"memorize/internal/infra/git"
)

func sessionIDFrom(ctx context.Context) string {
	if bm, ok := BridgeMetaFromContext(ctx); ok && bm.SessionID != "" {
		return bm.SessionID
	}
	if cs := server.ClientSessionFromContext(ctx); cs != nil && cs.SessionID() != "" && cs.SessionID() != "stdio" {
		return cs.SessionID()
	}
	return fmt.Sprintf("stdio-%d", os.Getpid())
}

type resolvedWorkspace struct {
	scope   string
	repoDir string
}

// ScopeResolver maps explicit arguments, MCP roots, or working directories into project scopes.
type ScopeResolver struct {
	srv   *server.MCPServer
	mu    sync.Mutex
	cache map[string][]resolvedWorkspace
}

// NewScopeResolver creates a ScopeResolver backed by srv for MCP roots discovery.
func NewScopeResolver(srv *server.MCPServer) *ScopeResolver {
	return &ScopeResolver{
		srv:   srv,
		cache: make(map[string][]resolvedWorkspace),
	}
}

func hintScopeFor(hints map[string]string, keys ...string) string {
	if len(hints) == 0 {
		return ""
	}
	for _, k := range keys {
		trimmed := strings.TrimSpace(k)
		if trimmed == "" {
			continue
		}
		for _, cand := range []string{trimmed, strings.TrimRight(trimmed, "/\\")} {
			if sc, ok := hints[cand]; ok && strings.TrimSpace(sc) != "" {
				return domain.NormalizeScope(sc)
			}
		}
	}
	return ""
}

func resolveRootEntry(ctx context.Context, rawURI string, hints map[string]string, serverMode bool) (resolvedWorkspace, bool) {
	parsedScope, cleanPath := parseRootURI(rawURI)
	if sc := hintScopeFor(hints, rawURI, cleanPath); sc != "" {
		repoDir := ""
		if !serverMode && cleanPath != "" {
			repoDir = git.ResolveRoot(cleanPath)
		}
		return resolvedWorkspace{scope: sc, repoDir: repoDir}, true
	}
	if !serverMode && cleanPath != "" {
		if sc, root := git.CanonicalScope(ctx, cleanPath); sc != "" {
			return resolvedWorkspace{scope: sc, repoDir: root}, true
		}
	}
	if parsedScope == "" {
		return resolvedWorkspace{}, false
	}
	repoDir := ""
	if !serverMode && cleanPath != "" {
		repoDir = git.ResolveRoot(cleanPath)
	}
	return resolvedWorkspace{scope: parsedScope, repoDir: repoDir}, true
}

func (r *ScopeResolver) sessionRoots(ctx context.Context) []resolvedWorkspace {
	serverMode := domain.IsServerMode(ctx)
	if bm, ok := BridgeMetaFromContext(ctx); ok && len(bm.Roots) > 0 {
		var roots []resolvedWorkspace
		seen := make(map[string]bool, len(bm.Roots))
		for _, uri := range bm.Roots {
			if ws, ok := resolveRootEntry(ctx, uri, bm.ScopeHints, serverMode); ok && !seen[ws.scope] {
				seen[ws.scope] = true
				roots = append(roots, ws)
			}
		}
		if len(roots) > 0 {
			return roots
		}
	}

	sid := domain.ScopedSessionID(ctx, sessionIDFrom(ctx))
	r.mu.Lock()
	cached, ok := r.cache[sid]
	r.mu.Unlock()
	if ok && len(cached) > 0 {
		return cached
	}

	if cs := server.ClientSessionFromContext(ctx); cs != nil && r.srv != nil {
		sci, hasInfo := cs.(server.SessionWithClientInfo)
		_, supportsRoots := cs.(server.SessionWithRoots)
		if hasInfo && supportsRoots && sci.GetClientCapabilities().Roots != nil {
			rctx, cancel := context.WithTimeout(ctx, 250*time.Millisecond)
			res, err := r.srv.RequestRoots(rctx, mcp.ListRootsRequest{})
			cancel()
			if err == nil && res != nil && len(res.Roots) > 0 {
				var hints map[string]string
				if bm, hasBM := BridgeMetaFromContext(ctx); hasBM {
					hints = bm.ScopeHints
				}
				var roots []resolvedWorkspace
				seen := make(map[string]bool, len(res.Roots))
				for _, root := range res.Roots {
					if ws, ok := resolveRootEntry(ctx, root.URI, hints, serverMode); ok && !seen[ws.scope] {
						seen[ws.scope] = true
						roots = append(roots, ws)
					}
				}
				if len(roots) > 0 {
					r.mu.Lock()
					if len(r.cache) >= 256 {
						clear(r.cache)
					}
					r.cache[sid] = roots
					r.mu.Unlock()
					return roots
				}
			}
		}
	}
	return nil
}

func effectiveWorkDir(ctx context.Context) string {
	if bm, ok := BridgeMetaFromContext(ctx); ok && strings.TrimSpace(bm.Cwd) != "" {
		return strings.TrimSpace(bm.Cwd)
	}
	if domain.IsServerMode(ctx) {
		return ""
	}
	wd, _ := os.Getwd()
	return wd
}

// Resolve returns the normalized scope and matching Git repository root for the request.
func (r *ScopeResolver) Resolve(ctx context.Context, explicitScope, explicitRepo string) (string, string) {
	serverMode := domain.IsServerMode(ctx)
	var hints map[string]string
	if bm, ok := BridgeMetaFromContext(ctx); ok {
		hints = bm.ScopeHints
	}
	wd := effectiveWorkDir(ctx)

	if s := strings.TrimSpace(explicitScope); s != "" {
		norm := domain.NormalizeScope(s)
		if serverMode {
			return norm, ""
		}
		repoDir := git.ResolveRoot(explicitRepo)
		if repoDir == "" && strings.HasPrefix(norm, "project:") {
			for _, ws := range r.sessionRoots(ctx) {
				if ws.scope == norm && ws.repoDir != "" {
					repoDir = ws.repoDir
					break
				}
			}
			if repoDir == "" && wd != "" {
				if sc, wdRoot := git.CanonicalScope(ctx, wd); sc == norm && wdRoot != "" {
					repoDir = wdRoot
				}
			}
		}
		return norm, repoDir
	}

	if explicitRepo := strings.TrimSpace(explicitRepo); explicitRepo != "" {
		if sc := hintScopeFor(hints, explicitRepo); sc != "" {
			if serverMode {
				return sc, ""
			}
			return sc, git.ResolveRoot(explicitRepo)
		}
		if serverMode {
			if base := strings.ToLower(domain.CanonicalPathBase(explicitRepo)); base != "" {
				return "project:" + base, ""
			}
		} else {
			if sc, repoDir := git.CanonicalScope(ctx, explicitRepo); sc != "" {
				return sc, repoDir
			}
			repoDir := git.ResolveRoot(explicitRepo)
			if repoDir != "" {
				if base := strings.ToLower(domain.CanonicalPathBase(repoDir)); base != "" {
					return "project:" + base, repoDir
				}
			}
		}
	}

	if roots := r.sessionRoots(ctx); len(roots) > 0 {
		return roots[0].scope, roots[0].repoDir
	}
	if wd != "" {
		if sc := hintScopeFor(hints, wd); sc != "" {
			if serverMode {
				return sc, ""
			}
			return sc, git.ResolveRoot(wd)
		}
		if serverMode {
			if base := strings.ToLower(domain.CanonicalPathBase(wd)); base != "" {
				return "project:" + base, ""
			}
		} else if os.Getenv("MEMORY_SCOPE") == "" {
			if sc, repoDir := git.CanonicalScope(ctx, wd); sc != "" {
				return sc, repoDir
			}
			repoDir := git.ResolveRoot(wd)
			target := repoDir
			if target == "" {
				target = wd
			}
			if base := strings.ToLower(domain.CanonicalPathBase(target)); base != "" {
				return "project:" + base, repoDir
			}
		}
	}
	return domain.DefaultScope(), ""
}

func isASCIILetter(b byte) bool {
	return (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z')
}

func hasWindowsDrivePrefix(p string) bool {
	return len(p) >= 2 && isASCIILetter(p[0]) && p[1] == ':' && (len(p) == 2 || p[2] == '/' || p[2] == '\\')
}

func parseRootURI(rawURI string) (string, string) {
	u, err := url.Parse(rawURI)
	p := rawURI
	if err == nil && (u.Scheme == "file" || u.Scheme == "") && u.Path != "" {
		p = u.Path
	}
	if len(p) >= 3 && p[0] == '/' && hasWindowsDrivePrefix(p[1:]) {
		p = p[1:]
	}
	cleanPath := strings.TrimRight(p, "/\\")
	normPath := strings.ReplaceAll(cleanPath, "\\", "/")
	if len(normPath) == 2 && hasWindowsDrivePrefix(normPath) {
		return "", ""
	}
	base := strings.ToLower(strings.TrimSpace(path.Base(normPath)))
	if base == "" || base == "." || base == "/" {
		return "", ""
	}
	return "project:" + base, cleanPath
}
