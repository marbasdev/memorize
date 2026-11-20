package cli

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"praxis/internal/infra/config"
	"praxis/internal/infra/git"
)

type installOptions struct {
	project    bool
	configPath string
	uninstall  bool
}

func resolveSelfBinary() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	resolved := exe
	if r, rerr := filepath.EvalSymlinks(exe); rerr == nil {
		resolved = r
	}
	if abs, aerr := filepath.Abs(resolved); aerr == nil {
		resolved = abs
	}
	base := filepath.Base(resolved)
	// Prefer the unversioned launcher link (~/.praxis/bin/praxis) so upgrades that prune
	// old praxis-<version> binaries never break registered MCP configs or git hooks.
	if strings.HasPrefix(base, "praxis-") {
		stableName := "praxis"
		if runtime.GOOS == "windows" && strings.HasSuffix(strings.ToLower(base), ".exe") {
			stableName = "praxis.exe"
		}
		candidate := filepath.Join(filepath.Dir(resolved), stableName)
		if info, serr := os.Stat(candidate); serr == nil && !info.IsDir() {
			return candidate, nil
		}
	}
	return resolved, nil
}

func runInstallCLI(args []string, uninstall bool) int {
	verb := "install"
	if uninstall {
		verb = "uninstall"
	}
	fs := flag.NewFlagSet("praxis "+verb, flag.ContinueOnError)
	project := fs.Bool("project", false, "write project-scoped MCP config (.cursor/mcp.json in current Git root)")
	cfgPath := fs.String("config", "", "explicit JSON config file path override")
	if err := fs.Parse(reorderFlagsBeforePositional(args, map[string]bool{"config": true})); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	pos := fs.Args()
	if len(pos) != 1 {
		fmt.Fprintf(os.Stderr, "usage: praxis %s <cursor|windsurf|claude-desktop|git-hook> [--project] [--config <path>]\n", verb)
		return 2
	}
	err := applyInstallTarget(pos[0], installOptions{
		project:    *project,
		configPath: strings.TrimSpace(*cfgPath),
		uninstall:  uninstall,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s failed: %v\n", verb, err)
		return 1
	}
	return 0
}

func installTarget(target string) error {
	return applyInstallTarget(target, installOptions{})
}

func applyInstallTarget(rawTarget string, opts installOptions) error {
	target := strings.ToLower(strings.TrimSpace(rawTarget))
	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("resolve home directory: %w", err)
	}

	switch target {
	case "git-hook", "git":
		if opts.project || opts.configPath != "" {
			return fmt.Errorf("--project and --config are not supported for target %q", rawTarget)
		}
		if opts.uninstall {
			return uninstallGitHook()
		}
		exePath, err := resolveSelfBinary()
		if err != nil {
			return fmt.Errorf("resolve executable path: %w", err)
		}
		return installGitHook(exePath, home)

	case "cursor", "windsurf", "claude-desktop", "claude_desktop", "claudedesktop", "mcp":
		wd, err := os.Getwd()
		if err != nil {
			return err
		}
		cfgPath, err := resolveMCPConfigPath(target, home, wd, opts.project, opts.configPath)
		if err != nil {
			return err
		}
		if opts.uninstall {
			return uninstallMCPServerEntry(cfgPath)
		}
		exePath, err := resolveSelfBinary()
		if err != nil {
			return fmt.Errorf("resolve executable path: %w", err)
		}
		return installMCPServerEntry(cfgPath, exePath)

	default:
		return fmt.Errorf("unknown target %q (expected: cursor, windsurf, claude-desktop, git-hook; use native plugin install for Antigravity/Claude Code)", rawTarget)
	}
}

func claudeDesktopConfigPath(goos, home string, getenv func(string) string) string {
	switch goos {
	case "darwin":
		return filepath.Join(home, "Library", "Application Support", "Claude", "claude_desktop_config.json")
	case "windows":
		if appData := strings.TrimSpace(getenv("APPDATA")); appData != "" {
			return filepath.Join(appData, "Claude", "claude_desktop_config.json")
		}
		return filepath.Join(home, "AppData", "Roaming", "Claude", "claude_desktop_config.json")
	default:
		if xdg := strings.TrimSpace(getenv("XDG_CONFIG_HOME")); xdg != "" {
			return filepath.Join(xdg, "Claude", "claude_desktop_config.json")
		}
		return filepath.Join(home, ".config", "Claude", "claude_desktop_config.json")
	}
}

func resolveMCPConfigPath(target, home, cwd string, projectScope bool, explicitConfig string) (string, error) {
	if explicitConfig != "" {
		if explicitConfig == "~" {
			return home, nil
		}
		if strings.HasPrefix(explicitConfig, "~/") || strings.HasPrefix(explicitConfig, `~\`) {
			explicitConfig = filepath.Join(home, explicitConfig[2:])
		}
		return filepath.Clean(explicitConfig), nil
	}

	switch target {
	case "cursor":
		if projectScope {
			baseDir := git.ResolveRoot(cwd)
			if baseDir == "" {
				baseDir = cwd
			}
			return filepath.Join(baseDir, ".cursor", "mcp.json"), nil
		}
		return filepath.Join(home, ".cursor", "mcp.json"), nil
	case "windsurf":
		if projectScope {
			return "", fmt.Errorf("--project is only supported for target \"cursor\" (or pass --config <path>)")
		}
		return filepath.Join(home, ".codeium", "windsurf", "mcp_config.json"), nil
	case "claude-desktop", "claude_desktop", "claudedesktop":
		if projectScope {
			return "", fmt.Errorf("--project is only supported for target \"cursor\" (or pass --config <path>)")
		}
		return claudeDesktopConfigPath(runtime.GOOS, home, os.Getenv), nil
	case "mcp":
		return "", fmt.Errorf("target \"mcp\" requires --config <path> (or specify cursor, windsurf, claude-desktop, git-hook)")
	default:
		return "", fmt.Errorf("unsupported MCP target %q", target)
	}
}

func resolveWritableConfigPath(cfgPath string) string {
	if info, err := os.Lstat(cfgPath); err == nil && (info.Mode()&os.ModeSymlink) != 0 {
		if realPath, rerr := filepath.EvalSymlinks(cfgPath); rerr == nil {
			return realPath
		}
	}
	return cfgPath
}

func loadExistingJSONObject(path string) (map[string]any, os.FileMode, bool, error) {
	info, statErr := os.Stat(path)
	if statErr != nil {
		if errors.Is(statErr, os.ErrNotExist) {
			return map[string]any{}, 0o600, false, nil
		}
		return nil, 0, false, statErr
	}
	perm := info.Mode().Perm()
	if perm == 0 {
		perm = 0o600
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, 0, false, err
	}
	if strings.TrimSpace(string(raw)) == "" {
		return map[string]any{}, perm, true, nil
	}
	var root map[string]any
	if err := json.Unmarshal(raw, &root); err != nil || root == nil {
		if err == nil {
			err = fmt.Errorf("top-level JSON value must be an object")
		}
		return nil, 0, true, fmt.Errorf("invalid JSON in %s (refusing to overwrite): %w", path, err)
	}
	return root, perm, true, nil
}

func writeJSONObjectAtomically(path string, root map[string]any, perm os.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	encoded, err := json.MarshalIndent(root, "", "  ")
	if err != nil {
		return err
	}
	encoded = append(encoded, '\n')

	tmp, err := os.CreateTemp(dir, ".praxis-mcp-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.Remove(tmpName)
		}
	}()
	if _, err := tmp.Write(encoded); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Chmod(perm); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		return err
	}
	cleanup = false
	return nil
}

func installMCPServerEntry(cfgPath, exePath string) error {
	writePath := resolveWritableConfigPath(cfgPath)
	root, perm, _, err := loadExistingJSONObject(writePath)
	if err != nil {
		return err
	}

	var servers map[string]any
	if rawServers, exists := root["mcpServers"]; exists && rawServers != nil {
		var ok bool
		servers, ok = rawServers.(map[string]any)
		if !ok {
			return fmt.Errorf("unexpected mcpServers type in %s (expected JSON object)", cfgPath)
		}
	}
	if servers == nil {
		servers = map[string]any{}
	}

	entry := map[string]any{}
	if prev, ok := servers["praxis"].(map[string]any); ok {
		for k, v := range prev {
			entry[k] = v
		}
	}
	entry["command"] = exePath
	entry["args"] = []string{}
	servers["praxis"] = entry
	root["mcpServers"] = servers

	if err := writeJSONObjectAtomically(writePath, root, perm); err != nil {
		return err
	}
	fmt.Printf("[ok] Registered praxis MCP server in %s\n", cfgPath)
	return nil
}

func uninstallMCPServerEntry(cfgPath string) error {
	writePath := resolveWritableConfigPath(cfgPath)
	root, perm, exists, err := loadExistingJSONObject(writePath)
	if err != nil {
		return err
	}
	if !exists {
		fmt.Printf("[ok] Praxis MCP entry already absent at %s\n", cfgPath)
		return nil
	}

	rawServers, hasServers := root["mcpServers"]
	if !hasServers || rawServers == nil {
		fmt.Printf("[ok] Praxis MCP entry already absent at %s\n", cfgPath)
		return nil
	}
	servers, ok := rawServers.(map[string]any)
	if !ok {
		return fmt.Errorf("unexpected mcpServers type in %s (expected JSON object)", cfgPath)
	}
	if _, hasPraxis := servers["praxis"]; !hasPraxis {
		fmt.Printf("[ok] Praxis MCP entry already absent at %s\n", cfgPath)
		return nil
	}

	delete(servers, "praxis")
	root["mcpServers"] = servers
	if err := writeJSONObjectAtomically(writePath, root, perm); err != nil {
		return err
	}
	fmt.Printf("[ok] Removed praxis MCP server from %s\n", cfgPath)
	return nil
}

func shellSingleQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'"'"'`) + "'"
}

func installGitHook(exePath, home string) error {
	wd, err := os.Getwd()
	if err != nil {
		return err
	}
	root := git.ResolveRoot(wd)
	if root == "" {
		return fmt.Errorf("not inside a Git repository: %s", wd)
	}
	dbPath := strings.TrimSpace(os.Getenv("PRAXIS_DB_PATH"))
	if dbPath == "" {
		dataDir := config.DefaultDataDir()
		if dataDir == "" {
			dataDir = filepath.Join(home, ".praxis")
		}
		dbPath = filepath.Join(dataDir, "memories.db")
	}

	hookPath := filepath.Join(root, ".git", "hooks", "pre-commit")
	if info, lerr := os.Lstat(hookPath); lerr == nil && (info.Mode()&os.ModeSymlink) != 0 {
		return fmt.Errorf("refusing to overwrite symlink pre-commit hook at %s", hookPath)
	}
	if existing, err := os.ReadFile(hookPath); err == nil && !strings.Contains(string(existing), "praxis") {
		return fmt.Errorf("existing pre-commit hook found at %s (refusing to overwrite non-praxis hook)", hookPath)
	}
	if err := os.MkdirAll(filepath.Dir(hookPath), 0o755); err != nil {
		return err
	}
	script := fmt.Sprintf("#!/usr/bin/env bash\n# Managed by praxis\nexport PRAXIS_DB_PATH=%s\nexec %s -hook git-precommit\n",
		shellSingleQuote(dbPath),
		shellSingleQuote(exePath),
	)
	if err := os.WriteFile(hookPath, []byte(script), 0o755); err != nil {
		return err
	}
	fmt.Printf("[ok] Installed Git pre-commit guardrail hook at %s\n", hookPath)
	return nil
}

func uninstallGitHook() error {
	wd, err := os.Getwd()
	if err != nil {
		return err
	}
	root := git.ResolveRoot(wd)
	if root == "" {
		return fmt.Errorf("not inside a Git repository: %s", wd)
	}
	hookPath := filepath.Join(root, ".git", "hooks", "pre-commit")
	info, lerr := os.Lstat(hookPath)
	if lerr != nil {
		if errors.Is(lerr, os.ErrNotExist) {
			fmt.Printf("[ok] Git pre-commit hook already absent at %s\n", hookPath)
			return nil
		}
		return lerr
	}
	if (info.Mode() & os.ModeSymlink) != 0 {
		return fmt.Errorf("refusing to remove symlink pre-commit hook at %s", hookPath)
	}
	existing, err := os.ReadFile(hookPath)
	if err != nil {
		return err
	}
	if !strings.Contains(string(existing), "Managed by praxis") {
		return fmt.Errorf("existing pre-commit hook at %s is not managed by praxis (refusing to remove)", hookPath)
	}
	if err := os.Remove(hookPath); err != nil {
		return err
	}
	fmt.Printf("[ok] Removed Git pre-commit guardrail hook at %s\n", hookPath)
	return nil
}
