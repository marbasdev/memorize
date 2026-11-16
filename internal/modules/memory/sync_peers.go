package memory

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"memorize/internal/domain"
)

// ListSyncPeers returns all saved SSH sync peer configurations ordered by peer_id.
func (s *Service) ListSyncPeers(ctx context.Context) ([]domain.SyncPeer, error) {
	if !domain.IsMemorySyncEnabled() || s == nil || s.db == nil {
		return nil, nil
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT peer_id, ssh_target, COALESCE(remote_bin, ''), COALESCE(device_label, ''),
		       COALESCE(remote_device_id, ''), scope_mode, COALESCE(scopes_json, '[]'),
		       COALESCE(last_sync_at, ''), COALESCE(last_status, ''), COALESCE(last_error, '')
		FROM sync_peers
		ORDER BY peer_id ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.SyncPeer
	for rows.Next() {
		var p domain.SyncPeer
		var scopesRaw string
		if err := rows.Scan(
			&p.PeerID, &p.SSHTarget, &p.RemoteBin, &p.DeviceLabel,
			&p.RemoteDeviceID, &p.ScopeMode, &scopesRaw,
			&p.LastSyncAt, &p.LastStatus, &p.LastError,
		); err != nil {
			return nil, err
		}
		var scopes []string
		_ = json.Unmarshal([]byte(scopesRaw), &scopes)
		p.ScopeMode, p.Scopes = domain.NormalizeSyncScopes(p.ScopeMode, scopes)
		out = append(out, p)
	}
	return out, rows.Err()
}

// GetSyncPeer fetches a single saved peer configuration by peer_id (or matching ssh_target).
func (s *Service) GetSyncPeer(ctx context.Context, peerIDOrTarget string) (*domain.SyncPeer, error) {
	key := strings.TrimSpace(peerIDOrTarget)
	if key == "" || s == nil || s.db == nil {
		return nil, errors.New("peer_id is required")
	}
	peers, err := s.ListSyncPeers(ctx)
	if err != nil {
		return nil, err
	}
	for i := range peers {
		if peers[i].PeerID == key || peers[i].SSHTarget == key {
			p := peers[i]
			return &p, nil
		}
	}
	return nil, fmt.Errorf("sync peer %q not found", key)
}

// UpsertSyncPeer creates or updates a saved SSH peer configuration and its selected scope filter.
func (s *Service) UpsertSyncPeer(ctx context.Context, peer domain.SyncPeer) (domain.SyncPeer, error) {
	if !domain.IsMemorySyncEnabled() {
		return peer, domain.ErrMemorySyncDisabled
	}
	if s == nil || s.db == nil {
		return peer, errors.New("memory service unavailable")
	}
	spec, err := domain.ParseSyncTarget(peer.SSHTarget)
	if err != nil {
		return peer, err
	}
	cleanBin, err := domain.ValidateRemoteBin(peer.RemoteBin)
	if err != nil {
		return peer, err
	}
	peer.SSHTarget = strings.TrimSpace(peer.SSHTarget)
	peer.RemoteBin = cleanBin
	peer.DeviceLabel = strings.TrimSpace(peer.DeviceLabel)
	peer.PeerID = strings.TrimSpace(peer.PeerID)
	if peer.PeerID == "" {
		peer.PeerID = spec.HostSpec
	}
	peer.ScopeMode, peer.Scopes = domain.NormalizeSyncScopes(peer.ScopeMode, peer.Scopes)
	scopesBytes, _ := json.Marshal(peer.Scopes)

	_, err = s.db.ExecContext(ctx, `
		INSERT INTO sync_peers (
			peer_id, ssh_target, remote_bin, device_label, scope_mode, scopes_json
		) VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(peer_id) DO UPDATE SET
			ssh_target = excluded.ssh_target,
			remote_bin = excluded.remote_bin,
			device_label = CASE WHEN excluded.device_label != '' THEN excluded.device_label ELSE sync_peers.device_label END,
			scope_mode = excluded.scope_mode,
			scopes_json = excluded.scopes_json`,
		peer.PeerID, peer.SSHTarget, peer.RemoteBin, peer.DeviceLabel, peer.ScopeMode, string(scopesBytes),
	)
	if err != nil {
		return peer, err
	}
	return peer, nil
}

// DeleteSyncPeer removes a saved SSH peer configuration.
func (s *Service) DeleteSyncPeer(ctx context.Context, peerID string) error {
	if !domain.IsMemorySyncEnabled() {
		return domain.ErrMemorySyncDisabled
	}
	if s == nil || s.db == nil {
		return nil
	}
	_, err := s.db.ExecContext(ctx, `DELETE FROM sync_peers WHERE peer_id = ?`, strings.TrimSpace(peerID))
	return err
}

func (s *Service) recordSyncPeerOutcome(ctx context.Context, peerID, remoteDevID, remoteLabel, statusText, errText string) {
	if s == nil || s.db == nil || strings.TrimSpace(peerID) == "" {
		return
	}
	_, _ = s.db.ExecContext(ctx, `
		UPDATE sync_peers
		SET remote_device_id = CASE WHEN ? != '' THEN ? ELSE remote_device_id END,
		    device_label = CASE WHEN device_label = '' AND ? != '' THEN ? ELSE device_label END,
		    last_sync_at = CASE WHEN ? = '' THEN CURRENT_TIMESTAMP ELSE last_sync_at END,
		    last_status = ?,
		    last_error = ?
		WHERE peer_id = ?`,
		remoteDevID, remoteDevID,
		remoteLabel, remoteLabel,
		errText,
		strings.TrimSpace(statusText), strings.TrimSpace(errText),
		strings.TrimSpace(peerID),
	)
}

// ListSyncConflicts returns all unresolved synchronization conflicts awaiting user resolution.
func (s *Service) ListSyncConflicts(ctx context.Context) ([]domain.SyncConflict, error) {
	if !domain.IsMemorySyncEnabled() || s == nil || s.db == nil {
		return nil, nil
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT sync_id, scope, local_memory_id, local_origin_device, local_origin_seq,
		       local_json, remote_device_id, remote_origin_device, remote_origin_seq,
		       remote_op, remote_json, reason, COALESCE(created_at, '')
		FROM sync_conflicts
		ORDER BY created_at DESC, sync_id ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.SyncConflict
	for rows.Next() {
		var c domain.SyncConflict
		var localJSON, remoteJSON string
		if err := rows.Scan(
			&c.SyncID, &c.Scope, &c.LocalMemoryID, &c.LocalOriginDevice, &c.LocalOriginSeq,
			&localJSON, &c.RemoteDeviceID, &c.RemoteOriginDevice, &c.RemoteOriginSeq,
			&c.RemoteOp, &remoteJSON, &c.Reason, &c.CreatedAt,
		); err != nil {
			return nil, err
		}
		_ = json.Unmarshal([]byte(localJSON), &c.LocalSnapshot)
		if strings.TrimSpace(remoteJSON) != "" {
			_ = json.Unmarshal([]byte(remoteJSON), &c.RemoteSnapshot)
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// CountSyncConflicts returns the count of unresolved synchronization conflicts.
func (s *Service) CountSyncConflicts(ctx context.Context) int {
	if !domain.IsMemorySyncEnabled() || s == nil || s.db == nil {
		return 0
	}
	var cnt int
	_ = s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM sync_conflicts`).Scan(&cnt)
	return cnt
}

// ListKnownSyncScopes returns sorted union of all scopes seen in local memories, team rules, or tx log.
func (s *Service) ListKnownSyncScopes(ctx context.Context) []string {
	seen := map[string]bool{"global": true, "personal": true}
	if s != nil && s.db != nil {
		for _, q := range []string{
			`SELECT DISTINCT scope FROM memories WHERE COALESCE(scope, '') != ''`,
			`SELECT DISTINCT scope FROM memory_tx_log WHERE COALESCE(scope, '') != ''`,
			`SELECT DISTINCT scope FROM repo_sync WHERE COALESCE(scope, '') != ''`,
		} {
			if rows, err := s.db.QueryContext(ctx, q); err == nil {
				for rows.Next() {
					var sc string
					if rows.Scan(&sc) == nil && strings.TrimSpace(sc) != "" {
						seen[domain.NormalizeScope(sc)] = true
					}
				}
				_ = rows.Close()
			}
		}
	}
	out := make([]string, 0, len(seen))
	for sc := range seen {
		out = append(out, sc)
	}
	sort.Slice(out, func(i, j int) bool {
		ri, rj := scopeSortRank(out[i]), scopeSortRank(out[j])
		if ri != rj {
			return ri < rj
		}
		return out[i] < out[j]
	})
	return out
}

func scopeSortRank(sc string) int {
	switch sc {
	case "global":
		return 0
	case "personal":
		return 1
	default:
		return 2
	}
}

// loadEffectiveWatermarks merges recorded peer watermarks with max local TX seqs per (scope, origin_device)
// so this node never asks any peer to re-send events it already holds locally.
func (s *Service) loadEffectiveWatermarks(ctx context.Context, peerDevID string, scopes []string) (domain.SyncWatermarkMap, error) {
	wm := make(domain.SyncWatermarkMap)
	if s == nil || s.db == nil {
		return wm, nil
	}
	allowed := scopeSet(scopes)
	merge := func(sc, orig string, seq int64) {
		if seq <= 0 || sc == "" || orig == "" {
			return
		}
		if len(allowed) > 0 && !allowed[sc] {
			return
		}
		if wm[sc] == nil {
			wm[sc] = make(map[string]int64)
		}
		if seq > wm[sc][orig] {
			wm[sc][orig] = seq
		}
	}

	if strings.TrimSpace(peerDevID) != "" {
		rows, err := s.db.QueryContext(ctx, `
			SELECT scope, origin_device, last_origin_seq
			FROM sync_watermarks
			WHERE peer_device_id = ?`, peerDevID)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var sc, orig string
			var seq int64
			if rows.Scan(&sc, &orig, &seq) == nil {
				merge(domain.NormalizeScope(sc), orig, seq)
			}
		}
		_ = rows.Close()
	}

	rows, err := s.db.QueryContext(ctx, `
		SELECT scope, origin_device, MAX(origin_seq)
		FROM memory_tx_log
		GROUP BY scope, origin_device`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var sc, orig string
		var seq int64
		if rows.Scan(&sc, &orig, &seq) == nil {
			merge(domain.NormalizeScope(sc), orig, seq)
		}
	}
	_ = rows.Close()
	return wm, nil
}

func scopeSet(scopes []string) map[string]bool {
	if len(scopes) == 0 {
		return nil
	}
	m := make(map[string]bool, len(scopes))
	for _, sc := range scopes {
		if n := domain.NormalizeScope(sc); n != "" {
			m[n] = true
		}
	}
	return m
}

func resolveAliasInTx(ctx context.Context, tx *sql.Tx, syncID string) string {
	cur := strings.TrimSpace(syncID)
	for range 8 {
		var next string
		err := tx.QueryRowContext(ctx, `SELECT canonical_sync_id FROM sync_aliases WHERE alias_sync_id = ?`, cur).Scan(&next)
		if err != nil || strings.TrimSpace(next) == "" || next == cur {
			break
		}
		cur = strings.TrimSpace(next)
	}
	return cur
}
