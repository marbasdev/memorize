package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

type bridgeReqMeta struct {
	sessionID string
	cwd       string
	roots     []string
}

type bridgeMetaCtxKey struct{}

func withBridgeMeta(ctx context.Context, m bridgeReqMeta) context.Context {
	return context.WithValue(ctx, bridgeMetaCtxKey{}, m)
}

func bridgeMetaFromContext(ctx context.Context) (bridgeReqMeta, bool) {
	if ctx != nil {
		if v, ok := ctx.Value(bridgeMetaCtxKey{}).(bridgeReqMeta); ok {
			return v, true
		}
	}
	return bridgeReqMeta{}, false
}

var (
	daemonRPCClient = &http.Client{Timeout: 35 * time.Second}
	daemonHookHTTP  = &http.Client{}
)

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
		add(filepath.Join(home, ".gemini", "config", "plugins", "memorize"))
	}
	if wd, err := os.Getwd(); err == nil && wd != "" {
		add(filepath.Join(wd, "plugins", "memorize"))
	}
	return dirs
}

func resolveConfiguredHookTimeout(mode string) (time.Duration, bool) {
	if raw, hasEnv := os.LookupEnv("MEMORY_HOOK_TIMEOUT"); hasEnv {
		return parseTimeoutSpec(raw)
	}
	m := strings.TrimSpace(mode)
	if m == "" {
		return 0, false
	}
	for _, dir := range candidatePluginDirs() {
		for _, rel := range []string{"hooks.json", filepath.Join(".claude-plugin", "plugin.json")} {
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

// runStdioDaemonBridge runs a lightweight stateless stdio <-> HTTP daemon bridge.
// It holds zero SQLite locks and survives daemon hot-reloads/restarts transparently.
func runStdioDaemonBridge(addr, dbPath, modelsDir string) error {
	sid := fmt.Sprintf("stdio-%d-%d", os.Getpid(), time.Now().UnixNano()%100000)
	cwd, _ := os.Getwd()
	envAccount := ""
	for _, k := range []string{"MEMORY_ACCOUNT", "AGY_ACCOUNT"} {
		if v := strings.TrimSpace(os.Getenv(k)); v != "" {
			envAccount = v
			break
		}
	}

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
			// Handle client response to our roots/list inquiry
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
			if env.Method == "notifications/initialized" {
				if clientSupportsRoots {
					requestClientRoots()
				}
				continue
			}
			if env.Method == "notifications/roots/list_changed" {
				if clientSupportsRoots {
					requestClientRoots()
				}
				continue
			}
			// Other JSON-RPC notifications (no id) require no response
			if len(env.ID) == 0 || string(env.ID) == "null" {
				continue
			}
		}

		payload := append([]byte(nil), line...)
		var respBytes []byte
		var callErr error
		// Up to 2 attempts so an in-flight daemon hot-reload/restart is completely invisible to the IDE
		for attempt := 0; attempt < 2; attempt++ {
			if attempt > 0 {
				time.Sleep(250 * time.Millisecond)
			}
			targetAddr := resolveDaemonAddrForDB(addr, dbPath)
			if st, ok := ensureDaemonRunning(addr, dbPath, modelsDir); !ok {
				callErr = fmt.Errorf("memorize daemon unavailable on %s", targetAddr)
				continue
			} else if st != nil && st.ListenAddr != "" {
				targetAddr = st.ListenAddr
			}
			req, err := http.NewRequest(http.MethodPost, "http://"+targetAddr+"/api/daemon/mcp", bytes.NewReader(payload))
			if err != nil {
				callErr = err
				break
			}
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("X-Memorize-Session", sid)
			if cwd != "" {
				req.Header.Set("X-Memorize-Cwd", cwd)
			}
			if envAccount != "" {
				req.Header.Set("X-Memorize-Account", envAccount)
			}
			rootsMu.RLock()
			if len(cachedRoots) > 0 {
				req.Header.Set("X-Memorize-Roots", strings.Join(cachedRoots, "\t"))
			}
			rootsMu.RUnlock()

			resp, err := daemonRPCClient.Do(req)
			if err != nil {
				callErr = err
				continue
			}
			respBytes, callErr = io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusNoContent {
				callErr = nil
				respBytes = nil
				break
			}
			if resp.StatusCode == http.StatusOK && callErr == nil {
				break
			}
			callErr = fmt.Errorf("daemon mcp HTTP %d", resp.StatusCode)
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

func runHookViaDaemon(addr, dbPath, modelsDir, mode string, rawStdin []byte, procStart time.Time) (string, int, bool) {
	st, ok := ensureDaemonRunning(addr, dbPath, modelsDir)
	if !ok {
		return "", 0, false
	}
	targetAddr := addr
	if st != nil && st.ListenAddr != "" {
		targetAddr = st.ListenAddr
	}
	if strings.TrimSpace(targetAddr) == "" {
		targetAddr = resolveDaemonAddrForDB(addr, dbPath)
	}
	ctx := context.Background()
	var cancel context.CancelFunc = func() {}
	var timeoutMs int64
	if total, hasDeadline := resolveConfiguredHookTimeout(mode); hasDeadline {
		srvBudget, cliBudget, alive := remainingHookBudgets(procStart, total)
		if !alive {
			// Caller deadline is nearly exhausted; fail open cleanly before IDE SIGKILL fires.
			return "", 0, true
		}
		timeoutMs = srvBudget.Milliseconds()
		ctx, cancel = context.WithTimeout(ctx, cliBudget)
	}
	defer cancel()

	cwd, _ := os.Getwd()
	envAccount := ""
	for _, k := range []string{"MEMORY_ACCOUNT", "AGY_ACCOUNT"} {
		if v := strings.TrimSpace(os.Getenv(k)); v != "" {
			envAccount = v
			break
		}
	}
	payload := map[string]any{
		"mode":    mode,
		"stdin":   string(rawStdin),
		"cwd":     cwd,
		"account": envAccount,
	}
	if timeoutMs > 0 {
		payload["timeout_ms"] = timeoutMs
	}
	body, _ := json.Marshal(payload)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://"+targetAddr+"/api/daemon/hook", bytes.NewReader(body))
	if err != nil {
		return "", 0, false
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := daemonHookHTTP.Do(req)
	if err != nil {
		return "", 0, false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", 0, false
	}
	var out struct {
		Output   string `json:"output"`
		ExitCode int    `json:"exit_code"`
	}
	if json.NewDecoder(resp.Body).Decode(&out) != nil {
		return "", 0, false
	}
	return out.Output, out.ExitCode, true
}
