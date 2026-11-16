package sqlite

import (
	"database/sql"
	"encoding/json"
	"os"
	"strings"

	"memorize/internal/domain"
)

const cfgSyncDeviceID = "sync_device_id"

// EnsureSyncDeviceID returns the persistent local device identifier from config, creating one on first run.
func EnsureSyncDeviceID(db *sql.DB) (string, error) {
	if db == nil {
		return "", nil
	}
	var devID string
	_ = db.QueryRow(`SELECT val FROM config WHERE key = ?`, cfgSyncDeviceID).Scan(&devID)
	devID = strings.TrimSpace(devID)
	if devID != "" {
		return devID, nil
	}
	host, _ := os.Hostname()
	gen := domain.NewDeviceID(host)
	_, err := db.Exec(
		`INSERT INTO config(key, val) VALUES(?, ?) ON CONFLICT(key) DO NOTHING`,
		cfgSyncDeviceID, gen,
	)
	if err != nil {
		return "", err
	}
	if err := db.QueryRow(`SELECT val FROM config WHERE key = ?`, cfgSyncDeviceID).Scan(&devID); err != nil {
		return "", err
	}
	return strings.TrimSpace(devID), nil
}

func initSyncSchema(db *sql.DB) error {
	_, _ = db.Exec(`ALTER TABLE memories ADD COLUMN created_at DATETIME DEFAULT ''`)
	_, _ = db.Exec(`ALTER TABLE memories ADD COLUMN sync_id TEXT NOT NULL DEFAULT ''`)
	_, _ = db.Exec(`ALTER TABLE memories ADD COLUMN origin_device TEXT NOT NULL DEFAULT ''`)
	_, _ = db.Exec(`ALTER TABLE memories ADD COLUMN origin_seq INTEGER NOT NULL DEFAULT 0`)
	_, _ = db.Exec(`ALTER TABLE memories ADD COLUMN dirty INTEGER NOT NULL DEFAULT 0`)

	if _, err := db.Exec(`
		CREATE UNIQUE INDEX IF NOT EXISTS idx_memories_sync_id ON memories(sync_id) WHERE sync_id != '';
		CREATE INDEX IF NOT EXISTS idx_memories_origin ON memories(origin_device, origin_seq);
		CREATE TABLE IF NOT EXISTS memory_tx_log (
			seq INTEGER PRIMARY KEY AUTOINCREMENT,
			origin_device TEXT NOT NULL,
			origin_seq INTEGER NOT NULL,
			op TEXT NOT NULL,
			sync_id TEXT NOT NULL,
			base_origin_device TEXT NOT NULL DEFAULT '',
			base_origin_seq INTEGER NOT NULL DEFAULT 0,
			resolved_origin_device TEXT NOT NULL DEFAULT '',
			resolved_origin_seq INTEGER NOT NULL DEFAULT 0,
			scope TEXT NOT NULL DEFAULT '',
			payload_json TEXT NOT NULL DEFAULT '',
			created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
			UNIQUE(origin_device, origin_seq)
		);
		CREATE INDEX IF NOT EXISTS idx_tx_scope_seq ON memory_tx_log(scope, seq);
		CREATE INDEX IF NOT EXISTS idx_tx_scope_origin_seq ON memory_tx_log(scope, origin_device, origin_seq);
		CREATE TABLE IF NOT EXISTS sync_peers (
			peer_id TEXT PRIMARY KEY,
			ssh_target TEXT NOT NULL,
			remote_bin TEXT NOT NULL DEFAULT '',
			device_label TEXT NOT NULL DEFAULT '',
			remote_device_id TEXT NOT NULL DEFAULT '',
			scope_mode TEXT NOT NULL DEFAULT 'selected',
			scopes_json TEXT NOT NULL DEFAULT '["global"]',
			last_sync_at DATETIME,
			last_status TEXT NOT NULL DEFAULT '',
			last_error TEXT NOT NULL DEFAULT ''
		);
		CREATE TABLE IF NOT EXISTS sync_watermarks (
			peer_device_id TEXT NOT NULL,
			scope TEXT NOT NULL,
			origin_device TEXT NOT NULL,
			last_origin_seq INTEGER NOT NULL DEFAULT 0,
			PRIMARY KEY (peer_device_id, scope, origin_device)
		);
		CREATE TABLE IF NOT EXISTS sync_conflicts (
			sync_id TEXT PRIMARY KEY,
			scope TEXT NOT NULL,
			local_memory_id INTEGER NOT NULL DEFAULT 0,
			local_origin_device TEXT NOT NULL DEFAULT '',
			local_origin_seq INTEGER NOT NULL DEFAULT 0,
			local_json TEXT NOT NULL,
			remote_device_id TEXT NOT NULL DEFAULT '',
			remote_origin_device TEXT NOT NULL DEFAULT '',
			remote_origin_seq INTEGER NOT NULL DEFAULT 0,
			remote_op TEXT NOT NULL DEFAULT 'upsert',
			remote_json TEXT NOT NULL DEFAULT '',
			reason TEXT NOT NULL DEFAULT '',
			created_at DATETIME DEFAULT CURRENT_TIMESTAMP
		);
		CREATE TABLE IF NOT EXISTS sync_aliases (
			alias_sync_id TEXT PRIMARY KEY,
			canonical_sync_id TEXT NOT NULL
		);
	`); err != nil {
		return err
	}

	if !domain.IsMemorySyncEnabled() {
		return nil
	}
	devID, err := EnsureSyncDeviceID(db)
	if err != nil {
		return err
	}
	return backfillLegacyMemories(db, devID)
}

type legacyMemRow struct {
	id           int64
	syncID       string
	scope        string
	account      string
	category     string
	subCategory  string
	tags         string
	replacesText string
	content      string
	confidence   float64
	pinned       bool
	active       bool
	supersededBy sql.NullInt64
	createdAt    string
}

func backfillLegacyMemories(db *sql.DB, selfDev string) error {
	var missingCount int
	if err := db.QueryRow(`SELECT COUNT(*) FROM memories WHERE COALESCE(sync_id, '') = ''`).Scan(&missingCount); err != nil || missingCount == 0 {
		return err
	}

	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	idToSync := make(map[int64]string)
	allRows, err := tx.Query(`SELECT id, COALESCE(sync_id, '') FROM memories WHERE COALESCE(sync_id, '') != ''`)
	if err != nil {
		return err
	}
	for allRows.Next() {
		var id int64
		var sid string
		if allRows.Scan(&id, &sid) == nil && sid != "" {
			idToSync[id] = sid
		}
	}
	_ = allRows.Close()

	rows, err := tx.Query(`
		SELECT id, scope, COALESCE(account, ''), category, COALESCE(sub_category, ''),
		       COALESCE(tags, ''), COALESCE(replaces_text, ''), content, confidence,
		       pinned, active, superseded_by, COALESCE(created_at, '')
		FROM memories
		WHERE COALESCE(sync_id, '') = ''
		ORDER BY id ASC`)
	if err != nil {
		return err
	}
	var legacy []legacyMemRow
	for rows.Next() {
		var r legacyMemRow
		var pin, act int
		if err := rows.Scan(
			&r.id, &r.scope, &r.account, &r.category, &r.subCategory,
			&r.tags, &r.replacesText, &r.content, &r.confidence,
			&pin, &act, &r.supersededBy, &r.createdAt,
		); err != nil {
			_ = rows.Close()
			return err
		}
		r.pinned = pin == 1
		r.active = act == 1
		r.syncID = domain.NewSyncID()
		idToSync[r.id] = r.syncID
		legacy = append(legacy, r)
	}
	if err := rows.Close(); err != nil {
		return err
	}
	if len(legacy) == 0 {
		return nil
	}

	var maxSeq int64
	_ = tx.QueryRow(`SELECT COALESCE(MAX(origin_seq), 0) FROM memory_tx_log WHERE origin_device = ?`, selfDev).Scan(&maxSeq)

	for _, r := range legacy {
		maxSeq++
		supSync := ""
		if r.supersededBy.Valid && r.supersededBy.Int64 > 0 {
			supSync = idToSync[r.supersededBy.Int64]
		}
		snap := domain.MemorySnapshot{
			SyncID:             r.syncID,
			Scope:              domain.NormalizeScope(r.scope),
			Account:            domain.NormalizeAccount(r.account),
			Category:           r.category,
			SubCategory:        r.subCategory,
			Tags:               r.tags,
			ReplacesText:       r.replacesText,
			Content:            r.content,
			Confidence:         r.confidence,
			Pinned:             r.pinned,
			Active:             r.active,
			SupersededBySyncID: supSync,
			CreatedAt:          r.createdAt,
		}
		payload, err := json.Marshal(snap)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(
			`UPDATE memories SET sync_id = ?, origin_device = ?, origin_seq = ?, dirty = 0 WHERE id = ?`,
			r.syncID, selfDev, maxSeq, r.id,
		); err != nil {
			return err
		}
		if _, err := tx.Exec(`
			INSERT INTO memory_tx_log (
				origin_device, origin_seq, op, sync_id,
				base_origin_device, base_origin_seq, scope, payload_json
			) VALUES (?, ?, ?, ?, '', 0, ?, ?)`,
			selfDev, maxSeq, domain.SyncOpUpsert, r.syncID, snap.Scope, string(payload),
		); err != nil {
			return err
		}
	}
	return tx.Commit()
}
