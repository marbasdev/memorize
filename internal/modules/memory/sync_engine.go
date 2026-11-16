package memory

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"memorize/internal/domain"
)

// HandleSyncHandshake processes a peer's hello/watermark frame and exports matching pull deltas.
func (s *Service) HandleSyncHandshake(ctx context.Context, req domain.SyncWireFrame) (domain.SyncWireFrame, []domain.MemoryTxEntry, error) {
	if !domain.IsMemorySyncEnabled() {
		return domain.SyncWireFrame{}, nil, domain.ErrMemorySyncDisabled
	}
	if s == nil || s.db == nil {
		return domain.SyncWireFrame{}, nil, errors.New("memory service unavailable")
	}
	selfDev, selfLabel := s.SelfDeviceID(ctx)
	if req.ProtocolVersion != 0 && req.ProtocolVersion != domain.SyncProtocolVer {
		return domain.SyncWireFrame{}, nil, fmt.Errorf("unsupported sync protocol version %d", req.ProtocolVersion)
	}
	if req.ProbeOnly {
		return domain.SyncWireFrame{
			Type:            "hello_ack",
			ProtocolVersion: domain.SyncProtocolVer,
			DeviceID:        selfDev,
			DeviceLabel:     selfLabel,
		}, nil, nil
	}
	mode, scopes := domain.NormalizeSyncScopes(req.ScopeMode, req.Scopes)
	wm, err := s.loadEffectiveWatermarks(ctx, req.DeviceID, scopes)
	if err != nil {
		return domain.SyncWireFrame{}, nil, err
	}
	pullEntries, err := s.ExportSyncDeltas(ctx, mode, scopes, req.Watermarks)
	if err != nil {
		return domain.SyncWireFrame{}, nil, err
	}
	ack := domain.SyncWireFrame{
		Type:            "hello_ack",
		ProtocolVersion: domain.SyncProtocolVer,
		DeviceID:        selfDev,
		DeviceLabel:     selfLabel,
		ScopeMode:       mode,
		MatchedScopes:   scopes,
		Watermarks:      wm,
	}
	return ack, pullEntries, nil
}

// ExportSyncDeltas returns TX entries for mode/scopes strictly newer than clientWatermarks.
func (s *Service) ExportSyncDeltas(ctx context.Context, scopeMode string, scopes []string, clientWatermarks domain.SyncWatermarkMap) ([]domain.MemoryTxEntry, error) {
	if s == nil || s.db == nil {
		return nil, nil
	}
	mode, cleanScopes := domain.NormalizeSyncScopes(scopeMode, scopes)
	if mode == domain.SyncScopeModeSelected && len(cleanScopes) == 0 {
		return nil, nil
	}
	allowed := scopeSet(cleanScopes)
	rows, err := s.db.QueryContext(ctx, `
		SELECT seq, origin_device, origin_seq, op, sync_id,
		       COALESCE(base_origin_device, ''), base_origin_seq,
		       COALESCE(resolved_origin_device, ''), resolved_origin_seq,
		       scope, COALESCE(payload_json, ''), COALESCE(created_at, '')
		FROM memory_tx_log
		ORDER BY seq ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []domain.MemoryTxEntry
	for rows.Next() {
		var (
			e          domain.MemoryTxEntry
			payloadRaw string
		)
		if err := rows.Scan(
			&e.Seq, &e.OriginDevice, &e.OriginSeq, &e.Op, &e.SyncID,
			&e.BaseOriginDevice, &e.BaseOriginSeq,
			&e.ResolvedOriginDevice, &e.ResolvedOriginSeq,
			&e.Scope, &payloadRaw, &e.CreatedAt,
		); err != nil {
			return nil, err
		}
		e.Scope = domain.NormalizeScope(e.Scope)
		if mode == domain.SyncScopeModeSelected && !allowed[e.Scope] {
			continue
		}
		if e.OriginSeq <= watermarkFor(clientWatermarks, e.Scope, e.OriginDevice) {
			continue
		}
		if strings.TrimSpace(payloadRaw) != "" {
			var snap domain.MemorySnapshot
			if json.Unmarshal([]byte(payloadRaw), &snap) == nil {
				e.Snapshot = &snap
			}
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

func watermarkFor(wm domain.SyncWatermarkMap, scope, origin string) int64 {
	if wm == nil {
		return 0
	}
	if byOrigin, ok := wm[scope]; ok {
		return byOrigin[origin]
	}
	return 0
}

type localRowMeta struct {
	id        int64
	syncID    string
	originDev string
	originSeq int64
	snap      domain.MemorySnapshot
}

func loadLocalRowBySyncID(ctx context.Context, tx *sql.Tx, syncID string) (*localRowMeta, error) {
	var (
		m       localRowMeta
		pinInt  int
		actInt  int
		supBy   sql.NullInt64
		supSync string
	)
	err := tx.QueryRowContext(ctx, `
		SELECT id, sync_id, COALESCE(origin_device, ''), origin_seq,
		       scope, COALESCE(account, ''), category, COALESCE(sub_category, ''),
		       COALESCE(tags, ''), COALESCE(replaces_text, ''), content, confidence,
		       pinned, active, superseded_by, COALESCE(created_at, '')
		FROM memories WHERE sync_id = ?`, syncID).Scan(
		&m.id, &m.syncID, &m.originDev, &m.originSeq,
		&m.snap.Scope, &m.snap.Account, &m.snap.Category, &m.snap.SubCategory,
		&m.snap.Tags, &m.snap.ReplacesText, &m.snap.Content, &m.snap.Confidence,
		&pinInt, &actInt, &supBy, &m.snap.CreatedAt,
	)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	if supBy.Valid && supBy.Int64 > 0 {
		_ = tx.QueryRowContext(ctx, `SELECT COALESCE(sync_id, '') FROM memories WHERE id = ?`, supBy.Int64).Scan(&supSync)
	}
	m.snap.SyncID = m.syncID
	m.snap.Pinned = pinInt == 1
	m.snap.Active = actInt == 1
	m.snap.SupersededBySyncID = strings.TrimSpace(supSync)
	return &m, nil
}

func findActiveExactDupInTx(ctx context.Context, tx *sql.Tx, snap domain.MemorySnapshot) (*localRowMeta, error) {
	if !snap.Active {
		return nil, nil
	}
	wantKey := domain.NormalizeDedupKey(snap.Scope, snap.Account, snap.Content)
	rows, err := tx.QueryContext(ctx, `
		SELECT id, COALESCE(sync_id, ''), COALESCE(origin_device, ''), origin_seq,
		       scope, COALESCE(account, ''), category, COALESCE(sub_category, ''),
		       COALESCE(tags, ''), COALESCE(replaces_text, ''), content, confidence,
		       pinned, active, COALESCE(created_at, '')
		FROM memories
		WHERE active = 1 AND scope = ? AND COALESCE(account, '') = ? AND COALESCE(sync_id, '') != ''
		ORDER BY id ASC`, domain.NormalizeScope(snap.Scope), domain.NormalizeAccount(snap.Account))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var m localRowMeta
		var pinInt, actInt int
		if err := rows.Scan(
			&m.id, &m.syncID, &m.originDev, &m.originSeq,
			&m.snap.Scope, &m.snap.Account, &m.snap.Category, &m.snap.SubCategory,
			&m.snap.Tags, &m.snap.ReplacesText, &m.snap.Content, &m.snap.Confidence,
			&pinInt, &actInt, &m.snap.CreatedAt,
		); err != nil {
			return nil, err
		}
		m.snap.SyncID = m.syncID
		m.snap.Pinned = pinInt == 1
		m.snap.Active = actInt == 1
		if domain.NormalizeDedupKey(m.snap.Scope, m.snap.Account, m.snap.Content) == wantKey {
			return &m, nil
		}
	}
	return nil, rows.Err()
}

func matchesOriginAncestor(localDev string, localSeq int64, e domain.MemoryTxEntry) bool {
	if localDev == e.BaseOriginDevice && localSeq == e.BaseOriginSeq {
		return true
	}
	if strings.TrimSpace(e.ResolvedOriginDevice) != "" &&
		localDev == strings.TrimSpace(e.ResolvedOriginDevice) &&
		localSeq == e.ResolvedOriginSeq {
		return true
	}
	return false
}

// ApplySyncDeltas replays incoming TX entries from remoteDevID inside one ACID transaction,
// handling two-pass supersede linking, exact-content initial dedup, and concurrent edit conflict isolation.
func (s *Service) ApplySyncDeltas(ctx context.Context, remoteDevID string, entries []domain.MemoryTxEntry) (domain.SyncApplyStats, error) {
	var stats domain.SyncApplyStats
	if !domain.IsMemorySyncEnabled() {
		return stats, domain.ErrMemorySyncDisabled
	}
	if s == nil || s.db == nil || len(entries) == 0 {
		return stats, nil
	}
	selfDev, _ := s.SelfDeviceID(ctx)

	var touchedIDs []int64
	var pendingSupersede []struct{ childSyncID, parentSyncID string }
	var newTax [][3]string
	wmAdvance := make(domain.SyncWatermarkMap)

	noteWatermark := func(scope, origDev string, origSeq int64) {
		if scope == "" || origDev == "" || origSeq <= 0 {
			return
		}
		if wmAdvance[scope] == nil {
			wmAdvance[scope] = make(map[string]int64)
		}
		if origSeq > wmAdvance[scope][origDev] {
			wmAdvance[scope][origDev] = origSeq
		}
	}

	if err := s.withTx(ctx, func(tx *sql.Tx) error {
		for _, e := range entries {
			normScope := domain.NormalizeScope(e.Scope)
			e.Scope = normScope
			if err := insertRemoteTxLogInTx(ctx, tx, e); err != nil {
				return err
			}
			noteWatermark(normScope, e.OriginDevice, e.OriginSeq)

			if e.OriginDevice == selfDev {
				stats.Skipped++
				continue
			}
			canonicalSyncID := resolveAliasInTx(ctx, tx, e.SyncID)
			localRow, err := loadLocalRowBySyncID(ctx, tx, canonicalSyncID)
			if err != nil {
				return err
			}

			if e.Op == domain.SyncOpPurge {
				if localRow == nil {
					stats.Skipped++
					continue
				}
				if localRow.originDev == e.OriginDevice && localRow.originSeq == e.OriginSeq {
					stats.Skipped++
					continue
				}
				if matchesOriginAncestor(localRow.originDev, localRow.originSeq, e) {
					_, _ = tx.ExecContext(ctx, `DELETE FROM session_sent WHERE memory_id = ?`, localRow.id)
					_, _ = tx.ExecContext(ctx, `DELETE FROM session_updates WHERE memory_id = ?`, localRow.id)
					if _, err := tx.ExecContext(ctx, `DELETE FROM memories WHERE id = ?`, localRow.id); err != nil {
						return err
					}
					_, _ = tx.ExecContext(ctx, `DELETE FROM sync_conflicts WHERE sync_id = ?`, canonicalSyncID)
					touchedIDs = append(touchedIDs, localRow.id)
					stats.Applied++
					continue
				}
				if err := upsertSyncConflictInTx(ctx, tx, canonicalSyncID, remoteDevID, localRow, e, "concurrent local change vs remote permanent purge"); err != nil {
					return err
				}
				stats.Conflicts++
				continue
			}

			if e.Snapshot == nil {
				stats.Skipped++
				continue
			}
			snap := *e.Snapshot
			snap.SyncID = canonicalSyncID
			snap.Scope = domain.NormalizeScope(snap.Scope)
			snap.Account = domain.NormalizeAccount(snap.Account)

			if localRow == nil {
				dupRow, err := findActiveExactDupInTx(ctx, tx, snap)
				if err != nil {
					return err
				}
				if dupRow != nil {
					winningSyncID := canonicalSyncID
					losingSyncID := dupRow.syncID
					if dupRow.syncID < canonicalSyncID {
						winningSyncID = dupRow.syncID
						losingSyncID = canonicalSyncID
					}
					if _, err := tx.ExecContext(ctx,
						`INSERT OR IGNORE INTO sync_aliases (alias_sync_id, canonical_sync_id) VALUES (?, ?)`,
						losingSyncID, winningSyncID,
					); err != nil {
						return err
					}
					mergedPin := boolToInt(dupRow.snap.Pinned || snap.Pinned)
					mergedConf := dupRow.snap.Confidence
					if snap.Confidence > mergedConf {
						mergedConf = snap.Confidence
					}
					if winningSyncID == canonicalSyncID {
						_, _ = tx.ExecContext(ctx,
							`UPDATE memories SET sync_id = ?, origin_device = ?, origin_seq = ?, dirty = 0, pinned = ?, confidence = ? WHERE id = ?`,
							canonicalSyncID, e.OriginDevice, e.OriginSeq, mergedPin, mergedConf, dupRow.id,
						)
					} else {
						_, _ = tx.ExecContext(ctx,
							`UPDATE memories SET pinned = ?, confidence = ? WHERE id = ?`,
							mergedPin, mergedConf, dupRow.id,
						)
					}
					_, _ = tx.ExecContext(ctx, `DELETE FROM sync_conflicts WHERE sync_id IN (?, ?)`, winningSyncID, losingSyncID)
					touchedIDs = append(touchedIDs, dupRow.id)
					stats.Deduped++
					continue
				}

				createdAt := strings.TrimSpace(snap.CreatedAt)
				var res sql.Result
				if createdAt != "" {
					res, err = tx.ExecContext(ctx, `
						INSERT INTO memories (
							scope, account, category, sub_category, tags, replaces_text,
							content, confidence, pinned, active,
							sync_id, origin_device, origin_seq, dirty, created_at
						) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 0, ?)`,
						snap.Scope, snap.Account, snap.Category, snap.SubCategory, snap.Tags, snap.ReplacesText,
						snap.Content, snap.Confidence, boolToInt(snap.Pinned), boolToInt(snap.Active),
						canonicalSyncID, e.OriginDevice, e.OriginSeq, createdAt,
					)
				} else {
					res, err = tx.ExecContext(ctx, `
						INSERT INTO memories (
							scope, account, category, sub_category, tags, replaces_text,
							content, confidence, pinned, active,
							sync_id, origin_device, origin_seq, dirty
						) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 0)`,
						snap.Scope, snap.Account, snap.Category, snap.SubCategory, snap.Tags, snap.ReplacesText,
						snap.Content, snap.Confidence, boolToInt(snap.Pinned), boolToInt(snap.Active),
						canonicalSyncID, e.OriginDevice, e.OriginSeq,
					)
				}
				if err != nil {
					return err
				}
				newID, _ := res.LastInsertId()
				touchedIDs = append(touchedIDs, newID)
				if snap.SupersededBySyncID != "" {
					pendingSupersede = append(pendingSupersede, struct{ childSyncID, parentSyncID string }{canonicalSyncID, snap.SupersededBySyncID})
				}
				newTax = append(newTax, [3]string{snap.Scope, snap.Category, snap.SubCategory})
				stats.Applied++
				continue
			}

			// Local row exists with canonicalSyncID.
			if localRow.originDev == e.OriginDevice && localRow.originSeq == e.OriginSeq {
				stats.Skipped++
				continue
			}
			if domain.SnapshotsEquivalent(localRow.snap, snap) {
				if _, err := tx.ExecContext(ctx,
					`UPDATE memories SET origin_device = ?, origin_seq = ?, dirty = 0 WHERE id = ?`,
					e.OriginDevice, e.OriginSeq, localRow.id,
				); err != nil {
					return err
				}
				_, _ = tx.ExecContext(ctx, `DELETE FROM sync_conflicts WHERE sync_id = ?`, canonicalSyncID)
				stats.Deduped++
				continue
			}
			if matchesOriginAncestor(localRow.originDev, localRow.originSeq, e) {
				if _, err := tx.ExecContext(ctx, `
					UPDATE memories
					SET scope = ?, account = ?, category = ?, sub_category = ?, tags = ?, replaces_text = ?,
					    content = ?, confidence = ?, pinned = ?, active = ?,
					    origin_device = ?, origin_seq = ?, dirty = 0
					WHERE id = ?`,
					snap.Scope, snap.Account, snap.Category, snap.SubCategory, snap.Tags, snap.ReplacesText,
					snap.Content, snap.Confidence, boolToInt(snap.Pinned), boolToInt(snap.Active),
					e.OriginDevice, e.OriginSeq, localRow.id,
				); err != nil {
					return err
				}
				pendingSupersede = append(pendingSupersede, struct{ childSyncID, parentSyncID string }{canonicalSyncID, snap.SupersededBySyncID})
				_, _ = tx.ExecContext(ctx, `DELETE FROM sync_conflicts WHERE sync_id = ?`, canonicalSyncID)
				touchedIDs = append(touchedIDs, localRow.id)
				newTax = append(newTax, [3]string{snap.Scope, snap.Category, snap.SubCategory})
				stats.Applied++
				continue
			}

			if err := upsertSyncConflictInTx(ctx, tx, canonicalSyncID, remoteDevID, localRow, e, "concurrent modification on both devices"); err != nil {
				return err
			}
			stats.Conflicts++
		}

		// Pass 2: resolve superseded_by FK links now that all rows in this batch exist.
		for _, link := range pendingSupersede {
			childID := resolveAliasInTx(ctx, tx, link.childSyncID)
			if strings.TrimSpace(link.parentSyncID) == "" {
				_, _ = tx.ExecContext(ctx, `UPDATE memories SET superseded_by = NULL WHERE sync_id = ?`, childID)
				continue
			}
			parentSync := resolveAliasInTx(ctx, tx, link.parentSyncID)
			var parentID int64
			if err := tx.QueryRowContext(ctx, `SELECT id FROM memories WHERE sync_id = ?`, parentSync).Scan(&parentID); err == nil && parentID > 0 {
				_, _ = tx.ExecContext(ctx, `UPDATE memories SET superseded_by = ? WHERE sync_id = ?`, parentID, childID)
			}
		}

		if strings.TrimSpace(remoteDevID) != "" {
			for sc, byOrig := range wmAdvance {
				for origDev, seq := range byOrig {
					if _, err := tx.ExecContext(ctx, `
						INSERT INTO sync_watermarks (peer_device_id, scope, origin_device, last_origin_seq)
						VALUES (?, ?, ?, ?)
						ON CONFLICT(peer_device_id, scope, origin_device) DO UPDATE SET
							last_origin_seq = MAX(sync_watermarks.last_origin_seq, excluded.last_origin_seq)`,
						remoteDevID, sc, origDev, seq,
					); err != nil {
						return err
					}
				}
			}
		}
		return nil
	}); err != nil {
		return stats, err
	}

	if len(touchedIDs) > 0 {
		s.clearSessionIDs(touchedIDs...)
		s.MarkAllTurnsMutated()
	}
	for _, t := range newTax {
		if t[1] != "" {
			s.RegisterTaxonomyAppendOnly(ctx, t[0], t[1], t[2])
		}
	}
	if stats.Applied > 0 || stats.Deduped > 0 || stats.Conflicts > 0 {
		s.bus.Publish("memories")
	}
	return stats, nil
}

func insertRemoteTxLogInTx(ctx context.Context, tx *sql.Tx, e domain.MemoryTxEntry) error {
	var payloadJSON string
	if e.Snapshot != nil {
		b, err := json.Marshal(e.Snapshot)
		if err != nil {
			return err
		}
		payloadJSON = string(b)
	}
	_, err := tx.ExecContext(ctx, `
		INSERT OR IGNORE INTO memory_tx_log (
			origin_device, origin_seq, op, sync_id,
			base_origin_device, base_origin_seq,
			resolved_origin_device, resolved_origin_seq,
			scope, payload_json
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		e.OriginDevice, e.OriginSeq, e.Op, e.SyncID,
		e.BaseOriginDevice, e.BaseOriginSeq,
		e.ResolvedOriginDevice, e.ResolvedOriginSeq,
		domain.NormalizeScope(e.Scope), payloadJSON,
	)
	return err
}
