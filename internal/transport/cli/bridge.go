package cli

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"memorize/internal/domain"
	"memorize/internal/infra/config"
	"memorize/internal/infra/git"
	thttp "memorize/internal/transport/http"
)

type ensureDaemonFn func(explicitAddr, dbPath, modelsDir string) (*thttp.DaemonStatus, bool)

func parseTimeoutSpec(raw string) (time.Duration, bool) {
	s := strings.TrimSpace(raw)
	if s == "" || s == "0" || strings.EqualFold(s, "off") || strings.EqualFold(s, "none") {
		return 0, false
	}
	if d, err := time.ParseDuration(s); err == nil {
		return d, d > 0
	}
	if sec, err := strconv.ParseFloat(s, 64); err == nil && sec > 0 {
		return time.Duration(sec * float64(time.Second)), true
	}
	return 0, false
}

func commandMatchesHookMode(cmdStr, mode string) bool {
	fields := strings.Fields(cmdStr)
	for i := 0; i < len(fields); i++ {
		tok := strings.Trim(fields[i], `"'`)
		if (tok == "-hook" || tok == "--hook") && i+1 < len(fields) {
			if strings.Trim(fields[i+1], `"'`) == mode {
				return true
			}
		}
		if strings.TrimPrefix(strings.TrimPrefix(tok, "--hook="), "-hook=") == mode && strings.Contains(tok, "hook=") {
			return true
		}
	}
	return false
}

func findHookTimeoutInJSON(rawJSON []byte, mode string) (time.Duration, bool) {
	var root any
	if json.Unmarshal(rawJSON, &root) != nil {
		return 0, false
	}
	var walk func(v any) (time.Duration, bool)
	walk = func(v any) (time.Duration, bool) {
		switch node := v.(type) {
		case map[string]any:
			if cmdStr, ok := node["command"].(string); ok && commandMatchesHookMode(cmdStr, mode) {
				switch tv := node["timeout"].(type) {
				case float64:
					if tv > 0 {
						return time.Duration(tv * float64(time.Second)), true
					}
				case string:
					if d, ok := parseTimeoutSpec(tv); ok {
						return d, true
					}
				}
				return 0, false
			}
			for _, child := range node {
				if d, ok := walk(child); ok {
					return d, true
				}
			}
		case []any:
			for _, item := range node {
				if d, ok := walk(item); ok {
					return d, true
				}
			}
		}
		return 0, false
	}
	return walk(root)
}

func candidatePluginDirs() []string {
	seen := make(map[string]bool)
	var dirs []string
	add := func(p string) {
		p = strings.TrimSpace(p)
		if p == "" {
			return
		}
		if abs, err := filepath.Abs(p); err == nil {
			p = abs
		}
		if !seen[p] {
			seen[p] = true
			dirs = append(dirs, p)
		}
	}
	add(os.Getenv("MEMORY_PLUGIN_DIR"))
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		for _, spec := range domain.AllAgentSpecs() {
			for _, rel := range spec.PluginDirRelPaths {
				add(filepath.Join(home, rel))
			}
		}
	}
	if wd, err := os.Getwd(); err == nil && wd != "" {
		add(filepath.Join(wd, "plugins", "memorize"))
	}
	return dirs
}

func agentManifestRelPaths() []string {
	seen := make(map[string]bool)
	var rels []string
	for _, spec := range domain.AllAgentSpecs() {
		for _, rel := range spec.ManifestRelPaths {
			if rel != "" && !seen[rel] {
				seen[rel] = true
				rels = append(rels, rel)
			}
		}
	}
	return rels
}

func resolveConfiguredHookTimeout(mode string) (time.Duration, bool) {
	if raw, hasEnv := os.LookupEnv("MEMORY_HOOK_TIMEOUT"); hasEnv {
		return parseTimeoutSpec(raw)
	}
	m := strings.TrimSpace(mode)
	if m == "" {
		return 0, false
	}
	manifests := agentManifestRelPaths()
	for _, dir := range candidatePluginDirs() {
		for _, rel := range manifests {
			data, err := os.ReadFile(filepath.Join(dir, rel))
			if err != nil {
				continue
			}
			if d, ok := findHookTimeoutInJSON(data, m); ok {
				return d, true
			}
		}
	}
	return 0, false
}

func remainingHookBudgets(procStart time.Time, total time.Duration) (serverBudget, clientBudget time.Duration, ok bool) {
	remaining := total - time.Since(procStart)
	if remaining <= 100*time.Millisecond {
		return 0, 0, false
	}
	margin := remaining / 8
	if remaining < 600*time.Millisecond {
		margin = remaining / 4
	} else {
		if margin < 150*time.Millisecond {
			margin = 150 * time.Millisecond
		}
		if margin > 800*time.Millisecond {
			margin = 800 * time.Millisecond
		}
	}
	if remaining <= margin {
		return 0, 0, false
	}
	return remaining - margin, remaining - margin/2, true
}

func parseLocalRootPath(raw string) string {
	s := strings.TrimSpace(raw)
	if s == "" {
		return ""
	}
	u, err := url.Parse(s)
	p := s
	if err == nil && (u.Scheme == "file" || u.Scheme == "") && u.Path != "" {
		p = u.Path
	}
	if len(p) >= 3 && p[0] == '/' && ((p[1] >= 'a' && p[1] <= 'z') || (p[1] >= 'A' && p[1] <= 'Z')) && p[2] == ':' {
		p = p[1:]
	}
	return strings.TrimRight(p, "/\\")
}

func resolveClientScopeAndRoot(ctx context.Context, rawPath string) (string, string) {
	p := parseLocalRootPath(rawPath)
	if p == "" {
		return "", ""
	}
	if sc, root := git.CanonicalScope(ctx, p); sc != "" {
		return sc, root
	}
	clean := filepath.Clean(p)
	home, _ := os.UserHomeDir()
	if clean != "." && clean != filepath.Dir(clean) && filepath.VolumeName(clean) != clean && clean != filepath.Clean(home) {
		if base := strings.ToLower(domain.CanonicalPathBase(clean)); base != "" {
			return "project:" + base, ""
		}
	}
	return "", ""
}

func buildClientScopeHints(ctx context.Context, cwd string, roots []string) map[string]string {
	hints := make(map[string]string)
	add := func(raw string) {
		localPath := parseLocalRootPath(raw)
		if localPath == "" {
			return
		}
		sc, root := resolveClientScopeAndRoot(ctx, localPath)
		if sc == "" {
			return
		}
		hints[strings.TrimSpace(raw)] = sc
		hints[localPath] = sc
		if root != "" {
			hints[root] = sc
		}
	}
	for _, rt := range roots {
		add(rt)
	}
	if strings.TrimSpace(cwd) != "" {
		add(cwd)
	}
	if len(hints) == 0 {
		return nil
	}
	return hints
}

func resolveRemoteForCandidates(dataDir string, paths ...string) (domain.RemoteTargetConfig, bool) {
	for _, raw := range paths {
		p := parseLocalRootPath(raw)
		if p == "" {
			continue
		}
		if target, ok := config.ResolveRemoteTarget(dataDir, p); ok {
			return target, true
		}
	}
	return config.ResolveRemoteTarget(dataDir, "")
}

func hookBudgetContext(procStart time.Time, mode string) (context.Context, context.CancelFunc, int64, bool) {
	ctx := context.Background()
	var cancel context.CancelFunc = func() {}
	var timeoutMs int64
	if total, hasDeadline := resolveConfiguredHookTimeout(mode); hasDeadline {
		srvBudget, cliBudget, alive := remainingHookBudgets(procStart, total)
		if !alive {
			return ctx, cancel, 0, false
		}
		timeoutMs = srvBudget.Milliseconds()
		ctx, cancel = context.WithTimeout(ctx, cliBudget)
	}
	return ctx, cancel, timeoutMs, true
}

// runStdioDaemonBridge runs a lightweight stateless stdio <-> HTTP bridge
// targeting either a bound remote team server or the unified local daemon.
func runStdioDaemonBridge(addr, dbPath, modelsDir string, ensureFn ensureDaemonFn) error {
	sid := fmt.Sprintf("stdio-%d-%d", os.Getpid(), time.Now().UnixNano()%100000)
	cwd, _ := os.Getwd()
	dataDir := resolveClientDataDir(dbPath)
	envAccount := domain.FirstNonEmpty(os.Getenv("MEMORY_ACCOUNT"), os.Getenv("AGY_ACCOUNT"))

	var outMu sync.Mutex
	writeLine := func(b []byte) error {
		outMu.Lock()
		defer outMu.Unlock()
		b = bytes.TrimSpace(b)
		if len(b) == 0 {
			return nil
		}
		_, err := os.Stdout.Write(append(b, '\n'))
		return err
	}

	var rootsMu sync.RWMutex
	var cachedRoots []string
	clientSupportsRoots := false

	requestClientRoots := func() {
		_ = writeLine([]byte(`{"jsonrpc":"2.0","id":"memorize-bridge-roots","method":"roots/list"}`))
	}

	scanner := bufio.NewScanner(os.Stdin)
	scanner.Buffer(make([]byte, 64*1024), 8<<20)

	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}
		var env struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params struct {
				Capabilities struct {
					Roots json.RawMessage `json:"roots"`
				} `json:"capabilities"`
			} `json:"params"`
			Result struct {
				Roots []struct {
					URI string `json:"uri"`
				} `json:"roots"`
			} `json:"result"`
		}
		if json.Unmarshal(line, &env) == nil {
			if env.Method == "" && string(env.ID) == `"memorize-bridge-roots"` {
				var uris []string
				for _, rt := range env.Result.Roots {
					if u := strings.TrimSpace(rt.URI); u != "" {
						uris = append(uris, u)
					}
				}
				rootsMu.Lock()
				cachedRoots = uris
				rootsMu.Unlock()
				continue
			}
			if env.Method == "initialize" && len(env.Params.Capabilities.Roots) > 0 && string(env.Params.Capabilities.Roots) != "null" {
				clientSupportsRoots = true
			}
			if env.Method == "notifications/initialized" || env.Method == "notifications/roots/list_changed" {
				if clientSupportsRoots {
					requestClientRoots()
				}
				continue
			}
			// Other JSON-RPC notifications (no id) require no response.
			if len(env.ID) == 0 || string(env.ID) == "null" {
				continue
			}
		}

		payload := append([]byte(nil), line...)
		rootsMu.RLock()
		rootsCopy := append([]string(nil), cachedRoots...)
		rootsMu.RUnlock()

		scopeHints := buildClientScopeHints(context.Background(), cwd, rootsCopy)
		candPaths := append(append([]string(nil), rootsCopy...), cwd)
		remote, hasRemote := resolveRemoteForCandidates(dataDir, candPaths...)

		var respBytes []byte
		var callErr error
		if hasRemote {
			for attempt := 0; attempt < 2; attempt++ {
				if attempt > 0 {
					time.Sleep(150 * time.Millisecond)
				}
				var noContent bool
				respBytes, noContent, callErr = thttp.PostDaemonMCPWithAuth(remote.URL, remote.Token, sid, cwd, "", rootsCopy, scopeHints, payload)
				if callErr == nil {
					if noContent {
						respBytes = nil
					}
					break
				}
			}
		} else {
			// Up to 2 attempts so an in-flight local daemon hot-reload/restart is invisible to the IDE.
			for attempt := 0; attempt < 2; attempt++ {
				if attempt > 0 {
					time.Sleep(250 * time.Millisecond)
				}
				targetAddr := thttp.ResolveDaemonAddrForDB(addr, dbPath)
				if ensureFn == nil {
					callErr = fmt.Errorf("memorize daemon unavailable on %s", targetAddr)
					break
				}
				if st, ok := ensureFn(addr, dbPath, modelsDir); !ok {
					callErr = fmt.Errorf("memorize daemon unavailable on %s", targetAddr)
					continue
				} else if st != nil && st.ListenAddr != "" {
					targetAddr = st.ListenAddr
				}
				var noContent bool
				respBytes, noContent, callErr = thttp.PostDaemonMCPWithAuth(targetAddr, "", sid, cwd, envAccount, rootsCopy, scopeHints, payload)
				if callErr == nil {
					if noContent {
						respBytes = nil
					}
					break
				}
			}
		}

		if callErr != nil {
			errReply, _ := json.Marshal(map[string]any{
				"jsonrpc": "2.0",
				"id":      env.ID,
				"error": map[string]any{
					"code":    -32603,
					"message": callErr.Error(),
				},
			})
			_ = writeLine(errReply)
			continue
		}
		if len(respBytes) > 0 {
			_ = writeLine(respBytes)
		}
	}
	return scanner.Err()
}

type hookInputPeek struct {
	SessionID      string
	Cwd            string
	WorkspacePaths []string
	Scope          string
	ToolName       string
	TargetPath     string
}

func peekHookInput(rawStdin []byte) hookInputPeek {
	trimmed := strings.TrimSpace(string(rawStdin))
	if !strings.HasPrefix(trimmed, "{") {
		return hookInputPeek{}
	}
	var p struct {
		SessionID           string         `json:"session_id"`
		ConversationID      string         `json:"conversation_id"`
		ConversationIDCamel string         `json:"conversationId"`
		Cwd                 string         `json:"cwd"`
		Workspace           string         `json:"workspace"`
		WorkspacePaths      []string       `json:"workspacePaths"`
		WorkspaceRoots      []string       `json:"workspace_roots"`
		Scope               string         `json:"scope"`
		ToolName            string         `json:"tool_name"`
		Tool                string         `json:"tool"`
		ToolInput           map[string]any `json:"tool_input"`
		Input               map[string]any `json:"input"`
		ToolCall            *struct {
			Name string         `json:"name"`
			Args map[string]any `json:"args"`
		} `json:"toolCall"`
	}
	if json.Unmarshal([]byte(trimmed), &p) != nil {
		return hookInputPeek{}
	}
	toolIn := p.ToolInput
	if len(toolIn) == 0 {
		toolIn = p.Input
	}
	if len(toolIn) == 0 && p.ToolCall != nil {
		toolIn = p.ToolCall.Args
	}
	toolName := domain.FirstNonEmpty(p.ToolName, p.Tool)
	if toolName == "" && p.ToolCall != nil {
		toolName = strings.TrimSpace(p.ToolCall.Name)
	}
	var paths []string
	paths = append(paths, p.WorkspacePaths...)
	paths = append(paths, p.WorkspaceRoots...)
	return hookInputPeek{
		SessionID:      domain.FirstNonEmpty(p.SessionID, p.ConversationID, p.ConversationIDCamel),
		Cwd:            domain.FirstNonEmpty(p.Cwd, p.Workspace),
		WorkspacePaths: paths,
		Scope:          strings.TrimSpace(p.Scope),
		ToolName:       toolName,
		TargetPath:     domain.ExtractTargetFilePath(toolIn),
	}
}

func resolveRemoteTargetForHook(dataDir string, rawStdin []byte) (domain.RemoteTargetConfig, bool) {
	peek := peekHookInput(rawStdin)
	var candidates []string
	candidates = append(candidates, peek.WorkspacePaths...)
	if peek.Cwd != "" {
		candidates = append(candidates, peek.Cwd)
	}
	if peek.TargetPath != "" {
		candidates = append(candidates, filepath.Dir(peek.TargetPath))
	}
	if wd, err := os.Getwd(); err == nil && wd != "" {
		candidates = append(candidates, wd)
	}
	return resolveRemoteForCandidates(dataDir, candidates...)
}

func clientGitSyncPath(dataDir string) string {
	if dataDir == "" {
		return ""
	}
	return filepath.Join(dataDir, "client-git-sync.json")
}

func loadClientGitHeads(dataDir string) map[string]string {
	p := clientGitSyncPath(dataDir)
	if p == "" {
		return map[string]string{}
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return map[string]string{}
	}
	var m map[string]string
	if json.Unmarshal(b, &m) != nil || m == nil {
		return map[string]string{}
	}
	return m
}

func saveClientGitHeads(dataDir string, heads map[string]string) {
	p := clientGitSyncPath(dataDir)
	if p == "" || len(heads) == 0 {
		return
	}
	_ = os.MkdirAll(filepath.Dir(p), 0o700)
	b, err := json.MarshalIndent(heads, "", "  ")
	if err != nil {
		return
	}
	tmp := p + ".tmp"
	if os.WriteFile(tmp, append(b, '\n'), 0o600) == nil {
		_ = os.Rename(tmp, p)
	}
}

func effectiveClientHookSessionID(explicitSID, primaryScope string) string {
	if sid := strings.TrimSpace(explicitSID); sid != "" {
		return sid
	}
	return "hook-session:" + primaryScope
}

func clientHookDisplayPath(absTarget, relTarget string) string {
	if strings.TrimSpace(relTarget) != "" {
		return relTarget
	}
	return filepath.ToSlash(strings.TrimSpace(absTarget))
}

func isStopBlockedOutput(out string, code int) bool {
	if code != 0 {
		return true
	}
	trimmed := strings.TrimSpace(out)
	if trimmed == "" {
		return false
	}
	var m map[string]any
	if json.Unmarshal([]byte(trimmed), &m) == nil {
		if dec, ok := m["decision"].(string); ok && strings.EqualFold(strings.TrimSpace(dec), "block") {
			return true
		}
		if cont, ok := m["continue"].(bool); ok && !cont {
			return true
		}
		if hookOut, ok := m["hookSpecificOutput"].(map[string]any); ok {
			if pd, ok := hookOut["permissionDecision"].(string); ok && strings.EqualFold(strings.TrimSpace(pd), "deny") {
				return true
			}
		}
	}
	return false
}

func buildClientHookContext(ctx context.Context, remote domain.RemoteTargetConfig, dataDir, mode string, peek hookInputPeek, fallbackCwd string) (*domain.ClientHookContext, map[string]string, string) {
	_, phase, valid := domain.ParseHookMode(mode)
	if !valid {
		return nil, nil, ""
	}

	cc := &domain.ClientHookContext{
		TeamRuleDocs:      make(map[string]string),
		GitHeadByScope:    make(map[string]string),
		GitCommitsByScope: make(map[string][]domain.GitCommitBrief),
	}
	repoByScope := make(map[string]string)
	seenScope := make(map[string]bool)

	addWorkspace := func(raw string) {
		sc, root := resolveClientScopeAndRoot(ctx, raw)
		if sc == "" {
			return
		}
		if !seenScope[sc] {
			seenScope[sc] = true
			cc.CanonicalScopes = append(cc.CanonicalScopes, sc)
		}
		if root != "" && repoByScope[sc] == "" {
			repoByScope[sc] = root
		}
	}

	if peek.Scope != "" {
		sc := domain.NormalizeScope(peek.Scope)
		seenScope[sc] = true
		cc.CanonicalScopes = append(cc.CanonicalScopes, sc)
		if root := git.ResolveRoot(domain.FirstNonEmpty(peek.Cwd, fallbackCwd)); root != "" {
			repoByScope[sc] = root
		}
	}
	for _, wp := range peek.WorkspacePaths {
		addWorkspace(wp)
	}
	if len(cc.CanonicalScopes) == 0 {
		addWorkspace(domain.FirstNonEmpty(peek.Cwd, fallbackCwd))
	}
	if len(cc.CanonicalScopes) == 0 {
		def := domain.DefaultScope()
		cc.CanonicalScopes = []string{def}
	}

	primaryScope := cc.CanonicalScopes[0]
	targetScope := primaryScope
	targetRepo := repoByScope[primaryScope]
	targetAbs := ""
	if peek.TargetPath != "" {
		tp := peek.TargetPath
		cwd := domain.FirstNonEmpty(peek.Cwd, fallbackCwd)
		if !filepath.IsAbs(tp) && cwd != "" {
			tp = filepath.Join(cwd, tp)
		}
		targetAbs = filepath.Clean(tp)
		matched := false
		for sc, root := range repoByScope {
			if root == "" {
				continue
			}
			cleanRoot := filepath.Clean(root)
			if targetAbs == cleanRoot || strings.HasPrefix(targetAbs, cleanRoot+string(filepath.Separator)) {
				targetScope = sc
				targetRepo = root
				matched = true
				break
			}
		}
		if !matched {
			if sc, root := resolveClientScopeAndRoot(ctx, filepath.Dir(targetAbs)); sc != "" {
				targetScope = sc
				targetRepo = root
			}
		}
		if targetRepo != "" {
			if rel, err := filepath.Rel(targetRepo, targetAbs); err == nil && !strings.HasPrefix(rel, "..") {
				cc.TargetRelPath = filepath.ToSlash(rel)
			}
		}
		if cc.TargetRelPath == "" {
			cc.TargetRelPath = filepath.ToSlash(targetAbs)
		}
	}
	cc.TargetScope = targetScope

	if phase == domain.HookPhaseSessionStart || phase == domain.HookPhaseTurn || phase == domain.HookPhasePreTool || phase == domain.HookPhasePreCommit {
		for _, sc := range cc.CanonicalScopes {
			root := repoByScope[sc]
			if root == "" {
				continue
			}
			if rawMD, err := os.ReadFile(filepath.Join(root, ".memorize", "TEAM_RULES.md")); err == nil && len(rawMD) > 0 {
				if len(rawMD) > 256*1024 {
					rawMD = rawMD[:256*1024]
				}
				cc.TeamRuleDocs[sc] = string(rawMD)
			}
		}
	}

	pendingHeads := make(map[string]string)
	if phase == domain.HookPhaseSessionStart || phase == domain.HookPhaseTurn {
		savedHeads := loadClientGitHeads(dataDir)
		for _, sc := range cc.CanonicalScopes {
			root := repoByScope[sc]
			if root == "" {
				continue
			}
			curHead := git.FastHead(ctx, root)
			if curHead == "" {
				continue
			}
			cacheKey := remote.URL + "|" + sc
			prevHead := savedHeads[cacheKey]
			cc.GitHeadByScope[sc] = curHead
			if prevHead == curHead {
				continue
			}
			head, commits := git.CollectPendingCommits(ctx, root, prevHead, 10, nil)
			if head != "" {
				cc.GitHeadByScope[sc] = head
				pendingHeads[cacheKey] = head
			}
			if len(commits) > 0 {
				cc.GitCommitsByScope[sc] = commits
			}
		}
	}

	effSID := effectiveClientHookSessionID(peek.SessionID, targetScope)
	switch phase {
	case domain.HookPhasePreTool:
		if domain.IsFileEditTool(peek.ToolName) && targetAbs != "" {
			dispPath := clientHookDisplayPath(targetAbs, cc.TargetRelPath)
			config.RecordClientTurnBaseline(dataDir, effSID, targetScope, targetAbs, dispPath)
			cc.RemoteManagedEdit = true
		}
	case domain.HookPhaseStop:
		cc.DiffSections = config.ConsumeClientTurnDiffs(dataDir, effSID, false)
		cc.RemoteManagedEdit = len(cc.DiffSections) > 0
	case domain.HookPhasePreCommit:
		if targetRepo != "" {
			if staged, err := git.StagedDiffFiles(ctx, targetRepo); err == nil {
				for _, f := range staged {
					cc.StagedDiffs = append(cc.StagedDiffs, domain.HookDiffSection{
						Scope: targetScope,
						Label: f.Path,
						Body:  f.Body,
					})
				}
			}
		}
	}

	if len(cc.TeamRuleDocs) == 0 {
		cc.TeamRuleDocs = nil
	}
	if len(cc.GitHeadByScope) == 0 {
		cc.GitHeadByScope = nil
	}
	if len(cc.GitCommitsByScope) == 0 {
		cc.GitCommitsByScope = nil
	}
	return cc, pendingHeads, effSID
}

func runHookViaRemote(remote domain.RemoteTargetConfig, dataDir, mode string, rawStdin []byte, procStart time.Time) (string, int, bool) {
	ctx, cancel, timeoutMs, alive := hookBudgetContext(procStart, mode)
	if !alive {
		return "", 0, true
	}
	defer cancel()

	cwd, _ := os.Getwd()
	peek := peekHookInput(rawStdin)
	cc, pendingHeads, effSID := buildClientHookContext(ctx, remote, dataDir, mode, peek, cwd)

	out, code, err := thttp.PostDaemonHookWithAuth(ctx, remote.URL, remote.Token, mode, rawStdin, cwd, "", timeoutMs, cc)
	if err != nil {
		return "", 0, true
	}
	if len(pendingHeads) > 0 {
		heads := loadClientGitHeads(dataDir)
		for k, v := range pendingHeads {
			heads[k] = v
		}
		saveClientGitHeads(dataDir, heads)
	}
	_, phase, _ := domain.ParseHookMode(mode)
	if phase == domain.HookPhaseStop && effSID != "" && !isStopBlockedOutput(out, code) {
		_ = config.ConsumeClientTurnDiffs(dataDir, effSID, true)
	}
	return out, code, true
}

func runHookViaDaemon(addr, dbPath, modelsDir, mode string, rawStdin []byte, procStart time.Time, ensureFn ensureDaemonFn) (string, int, bool) {
	st, ok := ensureFn(addr, dbPath, modelsDir)
	if !ok {
		return "", 0, false
	}
	targetAddr := addr
	if st != nil && st.ListenAddr != "" {
		targetAddr = st.ListenAddr
	}
	if strings.TrimSpace(targetAddr) == "" {
		targetAddr = thttp.ResolveDaemonAddrForDB(addr, dbPath)
	}
	ctx, cancel, timeoutMs, alive := hookBudgetContext(procStart, mode)
	if !alive {
		// Caller deadline is nearly exhausted; fail open cleanly before IDE SIGKILL fires.
		return "", 0, true
	}
	defer cancel()

	cwd, _ := os.Getwd()
	envAccount := domain.FirstNonEmpty(os.Getenv("MEMORY_ACCOUNT"), os.Getenv("AGY_ACCOUNT"))
	return thttp.PostDaemonHook(ctx, targetAddr, mode, rawStdin, cwd, envAccount, timeoutMs)
}
