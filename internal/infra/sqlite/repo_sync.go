package sqlite

import (
	"context"
	"database/sql"
	"strings"

	"praxis/internal/domain"
)

func GetSyncedCommit(ctx context.Context, db *sql.DB, rawScope string) string {
	sc := domain.NormalizeScope(rawScope)
	var sha string
	if p, ok := domain.AuthPrincipalFromContext(ctx); ok && p.ServerMode && p.Account != "" {
		_ = db.QueryRowContext(ctx, `SELECT last_commit FROM user_repo_sync WHERE scope = ? AND account = ?`, sc, p.Account).Scan(&sha)
		if strings.TrimSpace(sha) != "" {
			return strings.TrimSpace(sha)
		}
	}
	_ = db.QueryRowContext(ctx, `SELECT last_commit FROM repo_sync WHERE scope = ?`, sc).Scan(&sha)
	return strings.TrimSpace(sha)
}

func SetSyncedCommit(ctx context.Context, db *sql.DB, rawScope, sha string) error {
	sc := domain.NormalizeScope(rawScope)
	sha = strings.TrimSpace(sha)
	if sha == "" {
		return nil
	}
	if p, ok := domain.AuthPrincipalFromContext(ctx); ok && p.ServerMode && p.Account != "" {
		_, err := db.ExecContext(ctx, `
			INSERT INTO user_repo_sync (scope, account, last_commit, updated_at)
			VALUES (?, ?, ?, CURRENT_TIMESTAMP)
			ON CONFLICT(scope, account) DO UPDATE SET last_commit = excluded.last_commit, updated_at = CURRENT_TIMESTAMP
		`, sc, p.Account, sha)
		return err
	}
	_, err := db.ExecContext(ctx, `
		INSERT INTO repo_sync (scope, last_commit, updated_at)
		VALUES (?, ?, CURRENT_TIMESTAMP)
		ON CONFLICT(scope) DO UPDATE SET last_commit = excluded.last_commit, updated_at = CURRENT_TIMESTAMP
	`, sc, sha)
	return err
}

// SetStoredRepoPath persists an already-resolved local Git repository root for a project:* scope.
// In authenticated team server mode, client repository paths are never persisted on the host DB.
func SetStoredRepoPath(ctx context.Context, db *sql.DB, rawScope, resolvedRoot string) {
	if db == nil || domain.IsServerMode(ctx) {
		return
	}
	sc := domain.NormalizeScope(rawScope)
	root := strings.TrimSpace(resolvedRoot)
	if !strings.HasPrefix(sc, "project:") || root == "" {
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

func GetStoredRepoPath(ctx context.Context, db *sql.DB, rawScope string) string {
	if db == nil || domain.IsServerMode(ctx) {
		return ""
	}
	sc := domain.NormalizeScope(rawScope)
	if !strings.HasPrefix(sc, "project:") {
		return ""
	}
	var stored string
	_ = db.QueryRowContext(ctx, `SELECT COALESCE(repo_path, '') FROM repo_sync WHERE scope = ?`, sc).Scan(&stored)
	return strings.TrimSpace(stored)
}

func ListSyncedProjectScopes(ctx context.Context, db *sql.DB) []string {
	if db == nil {
		return nil
	}
	rows, err := db.QueryContext(ctx, `SELECT DISTINCT scope FROM repo_sync WHERE scope LIKE 'project:%'`)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var sc string
		if rows.Scan(&sc) == nil && strings.HasPrefix(sc, "project:") {
			out = append(out, sc)
		}
	}
	return out
}
