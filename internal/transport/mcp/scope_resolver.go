package mcp

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"path"
	"path/filepath"
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

func (r *ScopeResolver) sessionRoots(ctx context.Context) []resolvedWorkspace {
	if bm, ok := BridgeMetaFromContext(ctx); ok && len(bm.Roots) > 0 {
		var roots []resolvedWorkspace
		seen := make(map[string]bool, len(bm.Roots))
		for _, uri := range bm.Roots {
			if sc, dir := parseRootURI(uri); sc != "" && !seen[sc] {
				seen[sc] = true
				roots = append(roots, resolvedWorkspace{
					scope:   sc,
					repoDir: git.ResolveRoot(dir),
				})
			}
		}
		if len(roots) > 0 {
			return roots
		}
	}

	sid := sessionIDFrom(ctx)
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
				var roots []resolvedWorkspace
				seen := make(map[string]bool, len(res.Roots))
				for _, root := range res.Roots {
					if sc, dir := parseRootURI(root.URI); sc != "" && !seen[sc] {
						seen[sc] = true
						roots = append(roots, resolvedWorkspace{
							scope:   sc,
							repoDir: git.ResolveRoot(dir),
						})
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
	wd, _ := os.Getwd()
	return wd
}

// Resolve returns the normalized scope and matching Git repository root for the request.
func (r *ScopeResolver) Resolve(ctx context.Context, explicitScope, explicitRepo string) (string, string) {
	repoDir := git.ResolveRoot(explicitRepo)
	wd := effectiveWorkDir(ctx)
	if s := strings.TrimSpace(explicitScope); s != "" {
		norm := domain.NormalizeScope(s)
		if repoDir == "" && strings.HasPrefix(norm, "project:") {
			for _, ws := range r.sessionRoots(ctx) {
				if ws.scope == norm && ws.repoDir != "" {
					repoDir = ws.repoDir
					break
				}
			}
			if repoDir == "" && wd != "" {
				wdRoot := git.ResolveRoot(wd)
				if wdRoot != "" && "project:"+strings.ToLower(strings.TrimSpace(filepath.Base(wdRoot))) == norm {
					repoDir = wdRoot
				}
			}
		}
		return norm, repoDir
	}
	if repoDir != "" {
		base := strings.ToLower(strings.TrimSpace(filepath.Base(repoDir)))
		if base != "" && base != "." && base != "/" {
			return "project:" + base, repoDir
		}
	}

	if roots := r.sessionRoots(ctx); len(roots) > 0 {
		return roots[0].scope, roots[0].repoDir
	}
	if wd != "" {
		repoDir = git.ResolveRoot(wd)
		if envSc := strings.TrimSpace(os.Getenv("MEMORY_SCOPE")); envSc == "" {
			target := repoDir
			if target == "" {
				target = wd
			}
			base := strings.ToLower(strings.TrimSpace(filepath.Base(target)))
			if base != "" && base != "." && base != "/" {
				return "project:" + base, repoDir
			}
		}
	}
	return domain.DefaultScope(), repoDir
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
