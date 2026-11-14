package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"memorize/internal/infra/config"
	"memorize/internal/infra/git"
)

func resolveSelfBinary() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		return resolved, nil
	}
	return exe, nil
}

func installTarget(target string) error {
	switch strings.ToLower(strings.TrimSpace(target)) {
	case "git-hook", "git":
		exePath, err := resolveSelfBinary()
		if err != nil {
			return fmt.Errorf("resolve executable path: %w", err)
		}
		home, err := os.UserHomeDir()
		if err != nil {
			return fmt.Errorf("resolve home directory: %w", err)
		}
		return installGitHook(exePath, home)
	default:
		return fmt.Errorf("unknown install target %q (expected: git-hook; use plugin install commands for Antigravity/Claude)", target)
	}
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
	dbPath := strings.TrimSpace(os.Getenv("MEMORY_DB_PATH"))
	if dbPath == "" {
		dataDir := config.DefaultDataDir()
		if dataDir == "" {
			dataDir = filepath.Join(home, ".memorize")
		}
		dbPath = filepath.Join(dataDir, "memories.db")
	}

	hookPath := filepath.Join(root, ".git", "hooks", "pre-commit")
	if info, lerr := os.Lstat(hookPath); lerr == nil && (info.Mode()&os.ModeSymlink) != 0 {
		return fmt.Errorf("refusing to overwrite symlink pre-commit hook at %s", hookPath)
	}
	if existing, err := os.ReadFile(hookPath); err == nil && !strings.Contains(string(existing), "memorize") {
		return fmt.Errorf("existing pre-commit hook found at %s (refusing to overwrite non-memorize hook)", hookPath)
	}
	if err := os.MkdirAll(filepath.Dir(hookPath), 0o755); err != nil {
		return err
	}
	script := fmt.Sprintf("#!/usr/bin/env bash\n# Managed by memorize\nexport MEMORY_DB_PATH=%s\nexec %s -hook git-precommit\n",
		shellSingleQuote(dbPath),
		shellSingleQuote(exePath),
	)
	if err := os.WriteFile(hookPath, []byte(script), 0o755); err != nil {
		return err
	}
	fmt.Printf("[ok] Installed Git pre-commit guardrail hook at %s\n", hookPath)
	return nil
}
