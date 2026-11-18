package sqlite

import (
	"database/sql"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"
)

func TestInitSchemaDropsLegacyProgressStatusColumn(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "legacy.db")
	rawDB, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}

	// Seed a legacy project_progress table and status-referencing indexes before sqlite.Open runs InitSchema.
	legacyDDL := `
	CREATE TABLE project_progress (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		scope TEXT NOT NULL,
		category TEXT NOT NULL DEFAULT 'general',
		sub_category TEXT NOT NULL DEFAULT '',
		track TEXT NOT NULL DEFAULT '',
		status TEXT NOT NULL DEFAULT 'done',
		summary TEXT NOT NULL,
		details TEXT NOT NULL DEFAULT '',
		next_step TEXT NOT NULL DEFAULT '',
		source TEXT NOT NULL DEFAULT 'agent',
		commit_hash TEXT NOT NULL DEFAULT '',
		memory_ids TEXT NOT NULL DEFAULT '',
		created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ', 'now')),
		updated_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ', 'now'))
	);
	CREATE INDEX idx_progress_scope_cat ON project_progress(scope, category, status, id DESC);
	CREATE INDEX idx_progress_scope_track ON project_progress(scope, track, status, id DESC);
	INSERT INTO project_progress (scope, category, track, status, summary, source)
	VALUES
		('project:demo', 'backend', 'auth', 'done', 'Initial auth work', 'agent'),
		('project:demo', 'backend', 'api', 'in_progress', 'Legacy in-progress row', 'agent');
	`
	if _, err := rawDB.Exec(legacyDDL); err != nil {
		_ = rawDB.Close()
		t.Fatalf("seed legacy schema: %v", err)
	}
	_ = rawDB.Close()

	db, err := Open(dbPath)
	if err != nil {
		t.Fatalf("Open (with InitSchema migration): %v", err)
	}
	defer db.Close()

	if err := InitSchema(db); err != nil {
		t.Fatalf("InitSchema (idempotent 2nd): %v", err)
	}

	rows, err := db.Query(`PRAGMA table_info(project_progress)`)
	if err != nil {
		t.Fatalf("PRAGMA table_info: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var cid int
		var name, colType string
		var notNull, pk int
		var dflt any
		if err := rows.Scan(&cid, &name, &colType, &notNull, &dflt, &pk); err != nil {
			t.Fatalf("scan table_info: %v", err)
		}
		if name == "status" {
			t.Fatalf("expected status column to be dropped from project_progress")
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("table_info rows: %v", err)
	}

	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM project_progress WHERE scope = 'project:demo'`).Scan(&count); err != nil {
		t.Fatalf("count surviving rows: %v", err)
	}
	if count != 2 {
		t.Fatalf("expected 2 pre-existing rows to survive migration, got %d", count)
	}

	if _, err := db.Exec(`INSERT INTO project_progress (scope, category, track, summary, source) VALUES ('project:demo', 'tests', 'sqlite', 'Post-migration insert', 'agent')`); err != nil {
		t.Fatalf("insert without status column: %v", err)
	}

	var idxCount int
	if err := db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type = 'index' AND tbl_name = 'project_progress' AND name IN ('idx_progress_scope_cat', 'idx_progress_scope_track')`).Scan(&idxCount); err != nil {
		t.Fatalf("check recreated indexes: %v", err)
	}
	if idxCount != 2 {
		t.Fatalf("expected 2 recreated progress indexes, got %d", idxCount)
	}
}
