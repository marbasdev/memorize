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

// Open initializes a single-writer WAL SQLite connection with 0600 file permissions and schema migrations.
func Open(dbPath string) (*sql.DB, error) {
	config.EnsurePrivateDBFile(dbPath)
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if err := InitSchema(db); err != nil {
		_ = db.Close()
		return nil, err
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
	`)
	_, _ = db.Exec(`ALTER TABLE guardrail_logs ADD COLUMN elapsed_ms REAL NOT NULL DEFAULT 0`)
	_, _ = db.Exec(`ALTER TABLE project_progress ADD COLUMN track TEXT NOT NULL DEFAULT ''`)
	_, _ = db.Exec(`ALTER TABLE project_progress ADD COLUMN details TEXT NOT NULL DEFAULT ''`)
	_, _ = db.Exec(`ALTER TABLE project_progress ADD COLUMN session_id TEXT NOT NULL DEFAULT ''`)
	dropProgressStatusColumnIfPresent(db)
	_, _ = db.Exec(`CREATE INDEX IF NOT EXISTS idx_progress_scope_cat ON project_progress(scope, category, id DESC)`)
	_, _ = db.Exec(`CREATE INDEX IF NOT EXISTS idx_progress_scope_track ON project_progress(scope, track, id DESC)`)
	_, _ = db.Exec(`ALTER TABLE repo_sync ADD COLUMN repo_path TEXT NOT NULL DEFAULT ''`)
	_, _ = db.Exec(`ALTER TABLE session_sent ADD COLUMN updated_at DATETIME DEFAULT CURRENT_TIMESTAMP`)
	_, _ = db.Exec(`DELETE FROM session_sent WHERE updated_at < datetime('now', '-2 days')`)
	_, _ = db.Exec(`DELETE FROM session_sent_rules WHERE updated_at < datetime('now', '-2 days')`)
	_, _ = db.Exec(`DELETE FROM session_updates WHERE updated_at < datetime('now', '-2 days')`)
	_, _ = db.Exec(`DELETE FROM turn_state WHERE updated_at < ?`, time.Now().Add(-2*time.Hour).Unix())
	_, _ = db.Exec(`DELETE FROM turn_pending_edits WHERE updated_at < ?`, time.Now().Add(-2*time.Hour).Unix())
	_, _ = db.Exec(`DELETE FROM onboarding_prompts WHERE updated_at < ?`, time.Now().Add(-6*time.Hour).Unix())
	_, _ = db.Exec(`DELETE FROM session_compacts WHERE updated_at < ?`, time.Now().Add(-48*time.Hour).Unix())
	_, _ = db.Exec(`DELETE FROM project_progress WHERE source = 'session_auto'`)
	if err := initSyncSchema(db); err != nil {
		return err
	}
	return nil
}

// WithTx executes fn inside a SQLite transaction, rolling back on error or panic and committing on success.
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
