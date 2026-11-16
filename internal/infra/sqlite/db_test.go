package sqlite

import (
	"database/sql"
	"encoding/json"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"

	"memorize/internal/domain"
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

func TestInitSchemaBackfillsLegacyMemoriesSyncLog(t *testing.T) {
	t.Cleanup(domain.SetMemorySyncEnabledForTest(true))
	dbPath := filepath.Join(t.TempDir(), "legacy-mem.db")
	rawDB, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	_, err = rawDB.Exec(`
		CREATE TABLE memories (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			scope TEXT NOT NULL DEFAULT 'global',
			account TEXT NOT NULL DEFAULT 'alice',
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
		INSERT INTO memories (id, scope, account, category, content, confidence, pinned, superseded_by, active)
		VALUES
			(1, 'global', 'alice', 'convention', 'Old rule replaced by #2', 0.9, 0, 2, 0),
			(2, 'global', 'alice', 'convention', 'New rule replacing #1', 0.95, 1, NULL, 1);
	`)
	_ = rawDB.Close()
	if err != nil {
		t.Fatalf("seed legacy memories: %v", err)
	}

	db, err := Open(dbPath)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	// Running InitSchema again must be strictly idempotent without duplicating tx rows.
	if err := InitSchema(db); err != nil {
		t.Fatalf("2nd InitSchema: %v", err)
	}

	devID, err := EnsureSyncDeviceID(db)
	if err != nil || devID == "" {
		t.Fatalf("EnsureSyncDeviceID failed: %q err=%v", devID, err)
	}

	var sync1, sync2, origDev string
	var origSeq2 int64
	if err := db.QueryRow(`SELECT sync_id FROM memories WHERE id = 1`).Scan(&sync1); err != nil || sync1 == "" {
		t.Fatalf("row 1 missing sync_id: %q err=%v", sync1, err)
	}
	if err := db.QueryRow(`SELECT sync_id, origin_device, origin_seq FROM memories WHERE id = 2`).Scan(&sync2, &origDev, &origSeq2); err != nil {
		t.Fatalf("row 2 scan failed: %v", err)
	}
	if sync2 == "" || sync1 == sync2 || origDev != devID || origSeq2 != 2 {
		t.Fatalf("unexpected row 2 sync metadata: sync2=%q origDev=%q origSeq2=%d", sync2, origDev, origSeq2)
	}

	var txCount int
	if err := db.QueryRow(`SELECT COUNT(*) FROM memory_tx_log`).Scan(&txCount); err != nil || txCount != 2 {
		t.Fatalf("expected 2 backfilled tx log rows, got %d (err=%v)", txCount, err)
	}

	var payloadRaw string
	if err := db.QueryRow(`SELECT payload_json FROM memory_tx_log WHERE sync_id = ?`, sync1).Scan(&payloadRaw); err != nil {
		t.Fatalf("load tx payload for row 1: %v", err)
	}
	var snap domain.MemorySnapshot
	if err := json.Unmarshal([]byte(payloadRaw), &snap); err != nil {
		t.Fatalf("unmarshal payload: %v", err)
	}
	if snap.SupersededBySyncID != sync2 || snap.Active {
		t.Fatalf("expected forward superseded_by_sync_id=%q active=false, got %+v", sync2, snap)
	}
}

func TestInitSchemaSkipsLegacySyncBackfillWhenDisabled(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "legacy-disabled.db")
	rawDB, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	_, err = rawDB.Exec(`
		CREATE TABLE memories (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			scope TEXT NOT NULL DEFAULT 'global',
			account TEXT NOT NULL DEFAULT 'alice',
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
		INSERT INTO memories (id, scope, account, category, content, confidence, pinned, active)
		VALUES (1, 'global', 'alice', 'convention', 'Legacy rule stays untouched', 0.9, 1, 1);
	`)
	_ = rawDB.Close()
	if err != nil {
		t.Fatalf("seed legacy memories: %v", err)
	}

	db, err := Open(dbPath)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	var syncID string
	if err := db.QueryRow(`SELECT COALESCE(sync_id, '') FROM memories WHERE id = 1`).Scan(&syncID); err != nil {
		t.Fatalf("query sync_id: %v", err)
	}
	if syncID != "" {
		t.Fatalf("expected sync_id to remain empty when sync disabled, got %q", syncID)
	}
	var txCount int
	if err := db.QueryRow(`SELECT COUNT(*) FROM memory_tx_log`).Scan(&txCount); err != nil || txCount != 0 {
		t.Fatalf("expected 0 memory_tx_log rows when sync disabled, got %d (err=%v)", txCount, err)
	}
}
