package memory

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"strings"

	"memorize/internal/domain"
	"memorize/internal/infra/sqlite"
)

// SelfDeviceID returns this machine's persistent sync device ID and human-readable host label.
func (s *Service) SelfDeviceID(ctx context.Context) (string, string) {
	if !domain.IsMemorySyncEnabled() || s == nil || s.db == nil {
		return "", ""
	}
	devID, _ := sqlite.EnsureSyncDeviceID(s.db)
	cfg := s.GetConfigMap(ctx)
	label := strings.TrimSpace(cfg["sync_device_label"])
	if label == "" {
		if host, err := os.Hostname(); err == nil {
			label = strings.TrimSpace(host)
		}
	}
	if label == "" {
		label = devID
	}
	return devID, label
}

func recordMemoryTxInTx(ctx context.Context, tx *sql.Tx, selfDev string, memID int64, op, resolvedOriginDev string, resolvedOriginSeq int64) error {
	if !domain.IsMemorySyncEnabled() || tx == nil || memID <= 0 {
		return nil
	}
	if strings.TrimSpace(selfDev) == "" {
		selfDev = ensureDeviceIDFallback(tx)
	}

	var (
		syncID       string
		curOriginDev string
		curOriginSeq int64
		scope        string
		account      string
		category     string
		subCategory  string
		tags         string
		replacesText string
		content      string
		confidence   float64
		pinnedInt    int
		activeInt    int
		supBy        sql.NullInt64
		createdAt    string
	)
	err := tx.QueryRowContext(ctx, `
		SELECT COALESCE(sync_id, ''), COALESCE(origin_device, ''), origin_seq,
		       scope, COALESCE(account, ''), category, COALESCE(sub_category, ''),
		       COALESCE(tags, ''), COALESCE(replaces_text, ''), content, confidence,
		       pinned, active, superseded_by, COALESCE(created_at, '')
		FROM memories WHERE id = ?`, memID).Scan(
		&syncID, &curOriginDev, &curOriginSeq,
		&scope, &account, &category, &subCategory,
		&tags, &replacesText, &content, &confidence,
		&pinnedInt, &activeInt, &supBy, &createdAt,
	)
	if err != nil {
		return err
	}

	baseDev := strings.TrimSpace(curOriginDev)
	baseSeq := curOriginSeq
	if strings.TrimSpace(syncID) == "" {
		syncID = domain.NewSyncID()
		baseDev = ""
		baseSeq = 0
	}

	var supSyncID string
	if supBy.Valid && supBy.Int64 > 0 {
		_ = tx.QueryRowContext(ctx, `SELECT COALESCE(sync_id, '') FROM memories WHERE id = ?`, supBy.Int64).Scan(&supSyncID)
		supSyncID = strings.TrimSpace(supSyncID)
	}

	var nextSeq int64
	_ = tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(origin_seq), 0) FROM memory_tx_log WHERE origin_device = ?`, selfDev).Scan(&nextSeq)
	nextSeq++

	normScope := domain.NormalizeScope(scope)
	var payloadJSON string
	if op != domain.SyncOpPurge {
		op = domain.SyncOpUpsert
		if _, err := tx.ExecContext(ctx,
			`UPDATE memories SET sync_id = ?, origin_device = ?, origin_seq = ?, dirty = 1 WHERE id = ?`,
			syncID, selfDev, nextSeq, memID,
		); err != nil {
			return err
		}
		snap := domain.MemorySnapshot{
			SyncID:             syncID,
			Scope:              normScope,
			Account:            domain.NormalizeAccount(account),
			Category:           category,
			SubCategory:        subCategory,
			Tags:               tags,
			ReplacesText:       replacesText,
			Content:            content,
			Confidence:         confidence,
			Pinned:             pinnedInt == 1,
			Active:             activeInt == 1,
			SupersededBySyncID: supSyncID,
			CreatedAt:          createdAt,
		}
		b, err := json.Marshal(snap)
		if err != nil {
			return err
		}
		payloadJSON = string(b)
	}

	_, err = tx.ExecContext(ctx, `
		INSERT INTO memory_tx_log (
			origin_device, origin_seq, op, sync_id,
			base_origin_device, base_origin_seq,
			resolved_origin_device, resolved_origin_seq,
			scope, payload_json
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		selfDev, nextSeq, op, syncID,
		baseDev, baseSeq,
		strings.TrimSpace(resolvedOriginDev), resolvedOriginSeq,
		normScope, payloadJSON,
	)
	return err
}

func ensureDeviceIDFallback(tx *sql.Tx) string {
	var devID string
	_ = tx.QueryRow(`SELECT val FROM config WHERE key = 'sync_device_id'`).Scan(&devID)
	if devID = strings.TrimSpace(devID); devID != "" {
		return devID
	}
	host, _ := os.Hostname()
	devID = domain.NewDeviceID(host)
	_, _ = tx.Exec(`INSERT INTO config(key, val) VALUES('sync_device_id', ?) ON CONFLICT(key) DO NOTHING`, devID)
	_ = tx.QueryRow(`SELECT val FROM config WHERE key = 'sync_device_id'`).Scan(&devID)
	return strings.TrimSpace(devID)
}
