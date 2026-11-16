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

func upsertSyncConflictInTx(ctx context.Context, tx *sql.Tx, syncID, remoteDevID string, localRow *localRowMeta, e domain.MemoryTxEntry, reason string) error {
	localBytes, err := json.Marshal(localRow.snap)
	if err != nil {
		return err
	}
	var remoteBytes []byte
	if e.Snapshot != nil {
		remoteBytes, err = json.Marshal(e.Snapshot)
		if err != nil {
			return err
		}
	}
	_, err = tx.ExecContext(ctx, `
		INSERT INTO sync_conflicts (
			sync_id, scope, local_memory_id, local_origin_device, local_origin_seq, local_json,
			remote_device_id, remote_origin_device, remote_origin_seq, remote_op, remote_json, reason
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(sync_id) DO UPDATE SET
			local_memory_id = excluded.local_memory_id,
			local_origin_device = excluded.local_origin_device,
			local_origin_seq = excluded.local_origin_seq,
			local_json = excluded.local_json,
			remote_device_id = excluded.remote_device_id,
			remote_origin_device = excluded.remote_origin_device,
			remote_origin_seq = excluded.remote_origin_seq,
			remote_op = excluded.remote_op,
			remote_json = excluded.remote_json,
			reason = excluded.reason,
			created_at = CURRENT_TIMESTAMP`,
		syncID, localRow.snap.Scope, localRow.id, localRow.originDev, localRow.originSeq, string(localBytes),
		remoteDevID, e.OriginDevice, e.OriginSeq, e.Op, string(remoteBytes), reason,
	)
	return err
}

// ResolveSyncConflict resolves one pending sync conflict ("keep_local", "use_remote", or "keep_both")
// and emits a local resolution transaction carrying the acknowledged remote origin so both peers converge without re-conflicting.
func (s *Service) ResolveSyncConflict(ctx context.Context, syncID, decision string) (string, error) {
	if !domain.IsMemorySyncEnabled() {
		return "", domain.ErrMemorySyncDisabled
	}
	if s == nil || s.db == nil {
		return "", errors.New("memory service unavailable")
	}
	sid := strings.TrimSpace(syncID)
	if sid == "" {
		return "", errors.New("sync_id is required")
	}
	dec := strings.ToLower(strings.TrimSpace(decision))

	var touched []int64
	var msg string

	if err := s.withTx(ctx, func(tx *sql.Tx) error {
		var (
			c          domain.SyncConflict
			localJSON  string
			remoteJSON string
		)
		err := tx.QueryRowContext(ctx, `
			SELECT sync_id, scope, local_memory_id, local_origin_device, local_origin_seq, local_json,
			       remote_device_id, remote_origin_device, remote_origin_seq, remote_op, remote_json
			FROM sync_conflicts WHERE sync_id = ?`, sid).Scan(
			&c.SyncID, &c.Scope, &c.LocalMemoryID, &c.LocalOriginDevice, &c.LocalOriginSeq, &localJSON,
			&c.RemoteDeviceID, &c.RemoteOriginDevice, &c.RemoteOriginSeq, &c.RemoteOp, &remoteJSON,
		)
		if err != nil {
			return fmt.Errorf("conflict %q not found", sid)
		}
		_ = json.Unmarshal([]byte(localJSON), &c.LocalSnapshot)
		if strings.TrimSpace(remoteJSON) != "" {
			_ = json.Unmarshal([]byte(remoteJSON), &c.RemoteSnapshot)
		}

		selfDev := ensureDeviceIDFallback(tx)
		localRow, err := loadLocalRowBySyncID(ctx, tx, sid)
		if err != nil {
			return err
		}

		switch dec {
		case domain.SyncResolveKeepLocal:
			if localRow != nil {
				if err := recordMemoryTxInTx(ctx, tx, selfDev, localRow.id, domain.SyncOpUpsert, c.RemoteOriginDevice, c.RemoteOriginSeq); err != nil {
					return err
				}
				touched = append(touched, localRow.id)
			}
			msg = "Kept local memory and queued resolution for next sync"

		case domain.SyncResolveUseRemote:
			id, err := applyRemoteOverLocalInTx(ctx, tx, selfDev, sid, localRow, c)
			if err != nil {
				return err
			}
			if id > 0 {
				touched = append(touched, id)
			}
			msg = "Adopted remote memory version"

		case domain.SyncResolveKeepBoth:
			if c.RemoteOp == domain.SyncOpPurge {
				if localRow != nil {
					if err := recordMemoryTxInTx(ctx, tx, selfDev, localRow.id, domain.SyncOpUpsert, c.RemoteOriginDevice, c.RemoteOriginSeq); err != nil {
						return err
					}
					touched = append(touched, localRow.id)
				}
				msg = "Kept local memory active"
			} else {
				if localRow != nil {
					if _, err := applyRemoteOverLocalInTx(ctx, tx, selfDev, sid, localRow, c); err != nil {
						return err
					}
					touched = append(touched, localRow.id)
				}
				newSyncID := domain.NewSyncID()
				ls := c.LocalSnapshot
				if strings.TrimSpace(ls.Content) != "" {
					res, ierr := tx.ExecContext(ctx, `
						INSERT INTO memories (
							scope, account, category, sub_category, tags, replaces_text,
							content, confidence, pinned, active,
							sync_id, origin_device, origin_seq, dirty
						) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, '', 0, 1)`,
						domain.NormalizeScope(ls.Scope), domain.NormalizeAccount(ls.Account), ls.Category, ls.SubCategory, ls.Tags, ls.ReplacesText,
						ls.Content, ls.Confidence, boolToInt(ls.Pinned), boolToInt(ls.Active), newSyncID,
					)
					if ierr != nil {
						return ierr
					}
					dupID, _ := res.LastInsertId()
					if err := recordMemoryTxInTx(ctx, tx, selfDev, dupID, domain.SyncOpUpsert, "", 0); err != nil {
						return err
					}
					touched = append(touched, dupID)
				}
				msg = "Kept both memories as separate records"
			}

		default:
			return fmt.Errorf("unknown sync conflict decision %q (use 'keep_local', 'use_remote', or 'keep_both')", decision)
		}

		_, err = tx.ExecContext(ctx, `DELETE FROM sync_conflicts WHERE sync_id = ?`, sid)
		return err
	}); err != nil {
		return "", err
	}
	if len(touched) > 0 {
		s.clearSessionIDs(touched...)
		s.MarkAllTurnsMutated()
	}
	s.bus.Publish("memories")
	return msg, nil
}

func applyRemoteOverLocalInTx(ctx context.Context, tx *sql.Tx, selfDev, syncID string, localRow *localRowMeta, c domain.SyncConflict) (int64, error) {
	if c.RemoteOp == domain.SyncOpPurge {
		if localRow != nil {
			if err := recordMemoryTxInTx(ctx, tx, selfDev, localRow.id, domain.SyncOpPurge, c.RemoteOriginDevice, c.RemoteOriginSeq); err != nil {
				return 0, err
			}
			_, _ = tx.ExecContext(ctx, `DELETE FROM session_sent WHERE memory_id = ?`, localRow.id)
			_, _ = tx.ExecContext(ctx, `DELETE FROM memories WHERE id = ?`, localRow.id)
			return localRow.id, nil
		}
		return 0, nil
	}
	rs := c.RemoteSnapshot
	rs.Scope = domain.NormalizeScope(rs.Scope)
	rs.Account = domain.NormalizeAccount(rs.Account)
	if localRow == nil {
		return 0, nil
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE memories
		SET scope = ?, account = ?, category = ?, sub_category = ?, tags = ?, replaces_text = ?,
		    content = ?, confidence = ?, pinned = ?, active = ?
		WHERE id = ?`,
		rs.Scope, rs.Account, rs.Category, rs.SubCategory, rs.Tags, rs.ReplacesText,
		rs.Content, rs.Confidence, boolToInt(rs.Pinned), boolToInt(rs.Active),
		localRow.id,
	); err != nil {
		return 0, err
	}
	if strings.TrimSpace(rs.SupersededBySyncID) == "" {
		_, _ = tx.ExecContext(ctx, `UPDATE memories SET superseded_by = NULL WHERE id = ?`, localRow.id)
	} else {
		var pid int64
		if tx.QueryRowContext(ctx, `SELECT id FROM memories WHERE sync_id = ?`, rs.SupersededBySyncID).Scan(&pid) == nil && pid > 0 {
			_, _ = tx.ExecContext(ctx, `UPDATE memories SET superseded_by = ? WHERE id = ?`, pid, localRow.id)
		}
	}
	if err := recordMemoryTxInTx(ctx, tx, selfDev, localRow.id, domain.SyncOpUpsert, c.RemoteOriginDevice, c.RemoteOriginSeq); err != nil {
		return 0, err
	}
	return localRow.id, nil
}
