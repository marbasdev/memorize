package main

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

	"memorize/internal/memory"
)

func firstNonEmptyStr(vals ...string) string {
	for _, v := range vals {
		if s := strings.TrimSpace(v); s != "" {
			return s
		}
	}
	return ""
}

func sessionIDFrom(ctx context.Context) string {
	if bm, ok := bridgeMetaFromContext(ctx); ok && bm.sessionID != "" {
		return bm.sessionID
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

type scopeResolver struct {
	srv   *server.MCPServer
	mu    sync.Mutex
	cache map[string][]resolvedWorkspace
}

func (r *scopeResolver) sessionRoots(ctx context.Context) []resolvedWorkspace {
	if bm, ok := bridgeMetaFromContext(ctx); ok && len(bm.roots) > 0 {
		var roots []resolvedWorkspace
		seen := make(map[string]bool, len(bm.roots))
		for _, uri := range bm.roots {
			if sc, dir := parseRootURI(uri); sc != "" && !seen[sc] {
				seen[sc] = true
				roots = append(roots, resolvedWorkspace{
					scope:   sc,
					repoDir: memory.ResolveGitRoot(dir),
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

	// Ask MCP client for active workspace roots ONLY if client declared 'roots' capability in initialize
	if cs := server.ClientSessionFromContext(ctx); cs != nil {
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
							repoDir: memory.ResolveGitRoot(dir),
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
	if bm, ok := bridgeMetaFromContext(ctx); ok && strings.TrimSpace(bm.cwd) != "" {
		return strings.TrimSpace(bm.cwd)
	}
	wd, _ := os.Getwd()
	return wd
}

func (r *scopeResolver) resolve(ctx context.Context, explicitScope, explicitRepo string) (string, string) {
	repoDir := memory.ResolveGitRoot(explicitRepo)
	wd := effectiveWorkDir(ctx)
	if s := strings.TrimSpace(explicitScope); s != "" {
		norm := memory.NormalizeScope(s)
		if repoDir == "" && strings.HasPrefix(norm, "project:") {
			for _, ws := range r.sessionRoots(ctx) {
				if ws.scope == norm && ws.repoDir != "" {
					repoDir = ws.repoDir
					break
				}
			}
			if repoDir == "" && wd != "" {
				wdRoot := memory.ResolveGitRoot(wd)
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
		repoDir = memory.ResolveGitRoot(wd)
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
	return memory.DefaultScope(), repoDir
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
