package sqlite

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strings"

	"memorize/internal/domain"
	"memorize/internal/infra/git"
)

// GetSyncedCommit returns the recorded last_commit cursor for rawScope in repo_sync.
func GetSyncedCommit(ctx context.Context, db *sql.DB, rawScope string) string {
	sc := domain.NormalizeScope(rawScope)
	var sha string
	_ = db.QueryRowContext(ctx, `SELECT last_commit FROM repo_sync WHERE scope = ?`, sc).Scan(&sha)
	return strings.TrimSpace(sha)
}

// SetSyncedCommit upserts the synced commit cursor for rawScope in repo_sync.
func SetSyncedCommit(ctx context.Context, db *sql.DB, rawScope, sha string) error {
	sc := domain.NormalizeScope(rawScope)
	sha = strings.TrimSpace(sha)
	if sha == "" {
		return nil
	}
	_, err := db.ExecContext(ctx, `
		INSERT INTO repo_sync (scope, last_commit, updated_at)
		VALUES (?, ?, CURRENT_TIMESTAMP)
		ON CONFLICT(scope) DO UPDATE SET last_commit = excluded.last_commit, updated_at = CURRENT_TIMESTAMP
	`, sc, sha)
	return err
}

// RememberRepoPath records the resolved Git repository root for a project:* scope.
func RememberRepoPath(ctx context.Context, db *sql.DB, rawScope, repoDir string) {
	sc := domain.NormalizeScope(rawScope)
	if !strings.HasPrefix(sc, "project:") {
		return
	}
	root := git.ResolveRoot(repoDir)
	if root == "" {
		return
	}
	_, _ = db.ExecContext(ctx, `
		INSERT INTO repo_sync (scope, last_commit, repo_path, updated_at)
		VALUES (?, '', ?, CURRENT_TIMESTAMP)
		ON CONFLICT(scope) DO UPDATE SET
			repo_path = excluded.repo_path,
			updated_at = CURRENT_TIMESTAMP
	`, sc, root)
}

// ResolveScopeRepoPath resolves the Git root path for rawScope from explicitRepo, repo_sync, or CWD siblings.
func ResolveScopeRepoPath(ctx context.Context, db *sql.DB, rawScope, explicitRepo string) string {
	sc := domain.NormalizeScope(rawScope)
	if !strings.HasPrefix(sc, "project:") {
		return ""
	}
	if r := git.ResolveRoot(explicitRepo); r != "" {
		RememberRepoPath(ctx, db, sc, r)
		return r
	}
	var stored string
	_ = db.QueryRowContext(ctx, `SELECT COALESCE(repo_path, '') FROM repo_sync WHERE scope = ?`, sc).Scan(&stored)
	if r := git.ResolveRoot(stored); r != "" {
		return r
	}
	projName := strings.TrimPrefix(sc, "project:")
	if wd, err := os.Getwd(); err == nil && wd != "" {
		if wdRoot := git.ResolveRoot(wd); wdRoot != "" {
			if strings.EqualFold(filepath.Base(wdRoot), projName) {
				RememberRepoPath(ctx, db, sc, wdRoot)
				return wdRoot
			}
			sib := filepath.Join(filepath.Dir(wdRoot), projName)
			if sibRoot := git.ResolveRoot(sib); sibRoot != "" && strings.EqualFold(filepath.Base(sibRoot), projName) {
				RememberRepoPath(ctx, db, sc, sibRoot)
				return sibRoot
			}
		}
	}
	return ""
}
