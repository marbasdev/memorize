package sqlite

import (
	"context"
	"database/sql"
	"strings"
	"time"

	_ "modernc.org/sqlite"

	"memorize/internal/domain"
	"memorize/internal/infra/config"
)

func dropProgressStatusColumnIfPresent(db *sql.DB) {
	rows, err := db.Query(`PRAGMA table_info(project_progress)`)
	if err != nil {
		return
	}
	hasStatus := false
	for rows.Next() {
		var (
			cid     int
			name    string
			colType string
			notNull int
			dfltVal sql.NullString
			pk      int
		)
		if rows.Scan(&cid, &name, &colType, &notNull, &dfltVal, &pk) == nil && strings.EqualFold(name, "status") {
			hasStatus = true
			break
		}
	}
	_ = rows.Close()
	if !hasStatus {
		return
	}
	_, _ = db.Exec(`DROP INDEX IF EXISTS idx_progress_scope_cat`)
	_, _ = db.Exec(`DROP INDEX IF EXISTS idx_progress_scope_track`)
	_, _ = db.Exec(`ALTER TABLE project_progress DROP COLUMN status`)
}

const maxDiskOpenConns = 4

func isMemoryDSN(dbPath string) bool {
	s := strings.TrimSpace(strings.ToLower(dbPath))
	return s == "" || s == ":memory:" || strings.HasPrefix(s, "file::memory:") || strings.Contains(s, "mode=memory")
}

func buildSQLiteDSN(dbPath string) string {
	if isMemoryDSN(dbPath) {
		return dbPath
	}
	sep := "?"
	if strings.Contains(dbPath, "?") {
		sep = "&"
	}
	return dbPath + sep + "_busy_timeout=10000&_journal_mode=WAL&_synchronous=NORMAL&_txlock=immediate"
}

// Open initializes a WAL SQLite database with 0600 file permissions, schema migrations,
// and concurrent WAL read pooling (immediate write locking + 10s busy_timeout) on disk-backed DBs.
func Open(dbPath string) (*sql.DB, error) {
	config.EnsurePrivateDBFile(dbPath)
	db, err := sql.Open("sqlite", buildSQLiteDSN(dbPath))
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if err := InitSchema(db); err != nil {
		_ = db.Close()
		return nil, err
	}
	if !isMemoryDSN(dbPath) {
		db.SetMaxOpenConns(maxDiskOpenConns)
		db.SetMaxIdleConns(maxDiskOpenConns)
	}
	config.RestrictDBPermissions(dbPath)
	return db, nil
}

// InitSchema applies WAL/busy_timeout pragmas, creates all memorize tables/indexes, and runs idempotent migrations.
func InitSchema(db *sql.DB) error {
	if _, err := db.Exec(`
		PRAGMA journal_mode=WAL;
		PRAGMA busy_timeout=5000;
		CREATE TABLE IF NOT EXISTS memories (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			scope TEXT NOT NULL DEFAULT 'global',
			account TEXT NOT NULL DEFAULT '',
			category TEXT NOT NULL,
			sub_category TEXT NOT NULL DEFAULT '',
			tags TEXT NOT NULL DEFAULT '',
			content TEXT NOT NULL,
			confidence REAL NOT NULL,
			pinned INTEGER NOT NULL DEFAULT 0,
			superseded_by INTEGER,
			active INTEGER NOT NULL DEFAULT 1,
			created_at DATETIME DEFAULT CURRENT_TIMESTAMP
		);
	`); err != nil {
		return err
	}
	_, _ = db.Exec(`ALTER TABLE memories ADD COLUMN scope TEXT NOT NULL DEFAULT 'global'`)
	_, _ = db.Exec(`ALTER TABLE memories ADD COLUMN account TEXT NOT NULL DEFAULT ''`)
	_, _ = db.Exec(`ALTER TABLE memories ADD COLUMN sub_category TEXT NOT NULL DEFAULT ''`)
	_, _ = db.Exec(`ALTER TABLE memories ADD COLUMN tags TEXT NOT NULL DEFAULT ''`)
	_, _ = db.Exec(`ALTER TABLE memories ADD COLUMN replaces_text TEXT NOT NULL DEFAULT ''`)
	_, _ = db.Exec(`UPDATE memories SET account = ? WHERE COALESCE(account, '') = ''`, domain.DefaultAccount())
	_, _ = db.Exec(`
		DROP TRIGGER IF EXISTS memories_ai;
		DROP TRIGGER IF EXISTS memories_ad;
		DROP TRIGGER IF EXISTS memories_au;
		DROP TABLE IF EXISTS memories_fts;
		CREATE INDEX IF NOT EXISTS idx_memories_scope ON memories(scope, active, pinned);
		CREATE INDEX IF NOT EXISTS idx_memories_account ON memories(scope, account, active);
		CREATE INDEX IF NOT EXISTS idx_memories_superseded ON memories(scope, active, superseded_by);
		CREATE INDEX IF NOT EXISTS idx_memories_list_order ON memories(active DESC, pinned DESC, id DESC);
		CREATE INDEX IF NOT EXISTS idx_memories_recent_order ON memories(active DESC, id DESC);
		CREATE INDEX IF NOT EXISTS idx_memories_parent ON memories(superseded_by) WHERE superseded_by IS NOT NULL;
		CREATE TABLE IF NOT EXISTS repo_sync (
			scope TEXT PRIMARY KEY,
			last_commit TEXT NOT NULL,
			updated_at DATETIME DEFAULT CURRENT_TIMESTAMP
		);
		CREATE TABLE IF NOT EXISTS settings (
			key TEXT PRIMARY KEY,
			val REAL NOT NULL
		);
		CREATE TABLE IF NOT EXISTS config (
			key TEXT PRIMARY KEY,
			val TEXT NOT NULL
		);
		CREATE TABLE IF NOT EXISTS session_sent (
			session_id TEXT NOT NULL,
			memory_id INTEGER NOT NULL,
			updated_at DATETIME DEFAULT CURRENT_TIMESTAMP,
			PRIMARY KEY (session_id, memory_id)
		);
		CREATE TABLE IF NOT EXISTS turn_state (
			session_id TEXT NOT NULL,
			scope TEXT NOT NULL,
			last_input TEXT NOT NULL DEFAULT '',
			mutated INTEGER NOT NULL DEFAULT 0,
			gate_checked INTEGER NOT NULL DEFAULT 0,
			partial_notice TEXT NOT NULL DEFAULT '',
			stop_retry INTEGER NOT NULL DEFAULT 0,
			updated_at INTEGER NOT NULL DEFAULT 0,
			PRIMARY KEY (session_id, scope)
		);
		CREATE TABLE IF NOT EXISTS project_onboarding (
			scope TEXT PRIMARY KEY,
			status TEXT NOT NULL,
			updated_at DATETIME DEFAULT CURRENT_TIMESTAMP
		);
		CREATE TABLE IF NOT EXISTS onboarding_prompts (
			session_id TEXT NOT NULL,
			scope TEXT NOT NULL,
			updated_at INTEGER NOT NULL DEFAULT 0,
			PRIMARY KEY (session_id, scope)
		);
		CREATE TABLE IF NOT EXISTS session_updates (
			session_id TEXT NOT NULL,
			memory_id INTEGER NOT NULL,
			prev_content TEXT NOT NULL,
			updated_at DATETIME DEFAULT CURRENT_TIMESTAMP,
			PRIMARY KEY (session_id, memory_id)
		);
		CREATE TABLE IF NOT EXISTS project_progress (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			scope TEXT NOT NULL,
			session_id TEXT NOT NULL DEFAULT '',
			track TEXT NOT NULL DEFAULT '',
			category TEXT NOT NULL,
			sub_category TEXT NOT NULL DEFAULT '',
			summary TEXT NOT NULL,
			details TEXT NOT NULL DEFAULT '',
			next_step TEXT NOT NULL DEFAULT '',
			memory_ids TEXT NOT NULL DEFAULT '',
			commit_sha TEXT NOT NULL DEFAULT '',
			source TEXT NOT NULL DEFAULT 'agent',
			created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
			updated_at DATETIME DEFAULT CURRENT_TIMESTAMP
		);
		CREATE INDEX IF NOT EXISTS idx_progress_scope_commit ON project_progress(scope, commit_sha);
		CREATE TABLE IF NOT EXISTS guardrail_logs (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			scope TEXT NOT NULL DEFAULT 'global',
			source TEXT NOT NULL DEFAULT '',
			tool_name TEXT NOT NULL DEFAULT '',
			verdict TEXT NOT NULL DEFAULT 'pass',
			blocked INTEGER NOT NULL DEFAULT 0,
			rule_count INTEGER NOT NULL DEFAULT 0,
			violated_ids TEXT NOT NULL DEFAULT '',
			plan_snippet TEXT NOT NULL DEFAULT '',
			details TEXT NOT NULL DEFAULT '',
			elapsed_ms REAL NOT NULL DEFAULT 0,
			created_at DATETIME DEFAULT CURRENT_TIMESTAMP
		);
		CREATE INDEX IF NOT EXISTS idx_guardrail_logs_scope_id ON guardrail_logs(scope, blocked, id DESC);
		CREATE TABLE IF NOT EXISTS eval_telemetry (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			task TEXT NOT NULL,
			engine TEXT NOT NULL DEFAULT '',
			elapsed_ms REAL NOT NULL DEFAULT 0,
			item_count INTEGER NOT NULL DEFAULT 0,
			status TEXT NOT NULL DEFAULT 'ok',
			created_at DATETIME DEFAULT CURRENT_TIMESTAMP
		);
		CREATE INDEX IF NOT EXISTS idx_eval_telemetry_task_id ON eval_telemetry(task, id DESC);
		CREATE TABLE IF NOT EXISTS session_compacts (
			session_id TEXT PRIMARY KEY,
			last_offset INTEGER NOT NULL DEFAULT 0,
			last_marker TEXT NOT NULL DEFAULT '',
			pending_summary TEXT NOT NULL DEFAULT '',
			pending INTEGER NOT NULL DEFAULT 0,
			updated_at INTEGER NOT NULL
		);
		CREATE TABLE IF NOT EXISTS team_rule_states (
			scope TEXT NOT NULL,
			rule_key TEXT NOT NULL,
			status TEXT NOT NULL DEFAULT 'adopted',
			local_memory_id INTEGER NOT NULL DEFAULT 0,
			local_content TEXT NOT NULL DEFAULT '',
			conflict_reason TEXT NOT NULL DEFAULT '',
			override_note TEXT NOT NULL DEFAULT '',
			updated_at DATETIME DEFAULT CURRENT_TIMESTAMP,
			PRIMARY KEY (scope, rule_key)
		);
		CREATE TABLE IF NOT EXISTS team_rule_sync (
			scope TEXT PRIMARY KEY,
			file_hash TEXT NOT NULL DEFAULT '',
			remote_branch TEXT NOT NULL DEFAULT '',
			disk_baseline_hash TEXT NOT NULL DEFAULT '',
			last_error TEXT NOT NULL DEFAULT '',
			updated_at DATETIME DEFAULT CURRENT_TIMESTAMP
		);
		CREATE TABLE IF NOT EXISTS session_sent_rules (
			session_id TEXT NOT NULL,
			scope TEXT NOT NULL,
			rule_key TEXT NOT NULL,
			file_hash TEXT NOT NULL DEFAULT '',
			updated_at DATETIME DEFAULT CURRENT_TIMESTAMP,
			PRIMARY KEY (session_id, scope, rule_key)
		);
		CREATE TABLE IF NOT EXISTS turn_pending_edits (
			session_id TEXT NOT NULL,
			scope TEXT NOT NULL,
			file_path TEXT NOT NULL,
			repo_dir TEXT NOT NULL DEFAULT '',
			turn_input TEXT NOT NULL DEFAULT '',
			baseline_exists INTEGER NOT NULL DEFAULT 0,
			baseline_content TEXT NOT NULL DEFAULT '',
			fallback_snippet TEXT NOT NULL DEFAULT '',
			updated_at INTEGER NOT NULL DEFAULT 0,
			PRIMARY KEY (session_id, scope, file_path)
		);
		CREATE TABLE IF NOT EXISTS users (
			account TEXT PRIMARY KEY,
			role TEXT NOT NULL DEFAULT 'member',
			token_hash TEXT NOT NULL UNIQUE,
			token_hint TEXT NOT NULL DEFAULT '',
			active INTEGER NOT NULL DEFAULT 1,
			created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
			last_seen_at DATETIME
		);
		CREATE TABLE IF NOT EXISTS user_repo_sync (
			scope TEXT NOT NULL,
			account TEXT NOT NULL,
			last_commit TEXT NOT NULL DEFAULT '',
			updated_at DATETIME DEFAULT CURRENT_TIMESTAMP,
			PRIMARY KEY (scope, account)
		);
		CREATE TABLE IF NOT EXISTS internal_secrets (
			key TEXT PRIMARY KEY,
			val TEXT NOT NULL
		);
	`)
	_, _ = db.Exec(`
		INSERT INTO internal_secrets (key, val)
		SELECT key, val FROM config WHERE key = ?
		ON CONFLICT(key) DO NOTHING
	`, internalSecretAuthSession)
	_, _ = db.Exec(`DELETE FROM config WHERE key = ?`, internalSecretAuthSession)
	_, _ = db.Exec(`ALTER TABLE guardrail_logs ADD COLUMN elapsed_ms REAL NOT NULL DEFAULT 0`)
	_, _ = db.Exec(`ALTER TABLE guardrail_logs ADD COLUMN account TEXT NOT NULL DEFAULT ''`)
	_, _ = db.Exec(`ALTER TABLE guardrail_logs ADD COLUMN is_private INTEGER NOT NULL DEFAULT 0`)
	_, _ = db.Exec(`CREATE INDEX IF NOT EXISTS idx_guardrail_logs_account_id ON guardrail_logs(account, id DESC)`)
	_, _ = db.Exec(`ALTER TABLE project_progress ADD COLUMN track TEXT NOT NULL DEFAULT ''`)
	_, _ = db.Exec(`ALTER TABLE project_progress ADD COLUMN details TEXT NOT NULL DEFAULT ''`)
	_, _ = db.Exec(`ALTER TABLE project_progress ADD COLUMN session_id TEXT NOT NULL DEFAULT ''`)
	_, _ = db.Exec(`ALTER TABLE project_progress ADD COLUMN account TEXT NOT NULL DEFAULT ''`)
	_, _ = db.Exec(`ALTER TABLE eval_telemetry ADD COLUMN account TEXT NOT NULL DEFAULT ''`)
	dropProgressStatusColumnIfPresent(db)
	_, _ = db.Exec(`CREATE INDEX IF NOT EXISTS idx_progress_scope_cat ON project_progress(scope, category, id DESC)`)
	_, _ = db.Exec(`CREATE INDEX IF NOT EXISTS idx_progress_scope_track ON project_progress(scope, track, id DESC)`)
	_, _ = db.Exec(`ALTER TABLE repo_sync ADD COLUMN repo_path TEXT NOT NULL DEFAULT ''`)
	_, _ = db.Exec(`ALTER TABLE turn_state ADD COLUMN partial_notice TEXT NOT NULL DEFAULT ''`)
	_, _ = db.Exec(`ALTER TABLE turn_state ADD COLUMN stop_retry INTEGER NOT NULL DEFAULT 0`)
	_, _ = db.Exec(`ALTER TABLE session_sent ADD COLUMN updated_at DATETIME DEFAULT CURRENT_TIMESTAMP`)
	SweepTransientSessionTables(context.Background(), db)
	_, _ = db.Exec(`DELETE FROM project_progress WHERE source = 'session_auto'`)
	return nil
}

func SweepTransientSessionTables(ctx context.Context, db *sql.DB) {
	if db == nil {
		return
	}
	now := time.Now()
	_, _ = db.ExecContext(ctx, `DELETE FROM session_sent WHERE updated_at < datetime('now', '-2 days')`)
	_, _ = db.ExecContext(ctx, `DELETE FROM session_sent_rules WHERE updated_at < datetime('now', '-2 days')`)
	_, _ = db.ExecContext(ctx, `DELETE FROM session_updates WHERE updated_at < datetime('now', '-2 days')`)
	_, _ = db.ExecContext(ctx, `DELETE FROM turn_state WHERE updated_at < ?`, now.Add(-domain.TurnBaselineTTL).Unix())
	_, _ = db.ExecContext(ctx, `DELETE FROM turn_pending_edits WHERE updated_at < ?`, now.Add(-domain.TurnBaselineTTL).Unix())
	_, _ = db.ExecContext(ctx, `DELETE FROM onboarding_prompts WHERE updated_at < ?`, now.Add(-6*time.Hour).Unix())
	_, _ = db.ExecContext(ctx, `DELETE FROM session_compacts WHERE updated_at < ?`, now.Add(-48*time.Hour).Unix())
}

func WithTx(ctx context.Context, db *sql.DB, fn func(tx *sql.Tx) error) error {
	if db == nil {
		return sql.ErrConnDone
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit()
}
