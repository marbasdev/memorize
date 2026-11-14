package progress

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"memorize/internal/domain"
	"memorize/internal/infra/config"
)

// GetRetentionPolicy returns the active retention policy from SQLite config with safe defaults.
func (s *Service) GetRetentionPolicy(ctx context.Context) domain.RetentionPolicy {
	return config.LoadRetentionPolicy(ctx, s.db)
}

// SetProgressRetentionDays updates automatic TTL for completed progress history and immediately sweeps expired entries.
func (s *Service) SetProgressRetentionDays(ctx context.Context, days int) (domain.RetentionPolicy, int, error) {
	norm := domain.NormalizeRetentionDays(days)
	if err := s.mem.SaveConfig(ctx, map[string]string{
		config.CfgProgressRetentionDays: strconv.Itoa(norm),
	}); err != nil {
		return s.GetRetentionPolicy(ctx), 0, err
	}
	deleted := 0
	if norm > 0 {
		deleted, _ = s.CleanupProgressHistory(ctx, "", norm)
	}
	return s.GetRetentionPolicy(ctx), deleted, nil
}

func (s *Service) getProgressPrunedMemHWM(ctx context.Context, scope string) int64 {
	sc := domain.NormalizeScope(scope)
	var raw string
	_ = s.db.QueryRowContext(ctx, `SELECT val FROM config WHERE key = ?`, config.CfgProgressPrunedMemHWM+sc).Scan(&raw)
	v, _ := strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
	return v
}

// CleanupProgressHistory deletes only completed ('done') progress entries matching scope and age cutoff
// (olderThanDays == 0 deletes all completed entries; >0 deletes entries older than N days),
// protecting active ('in_progress', 'blocked', 'planned') tracks and recording a memory watermark
// so pruned historical memories are not re-created on subsequent Progress/Git syncs.
func (s *Service) CleanupProgressHistory(ctx context.Context, rawScope string, olderThanDays int) (int, error) {
	if olderThanDays < 0 {
		return 0, fmt.Errorf("older_than_days must be >= 0")
	}
	conds := []string{`status = 'done'`}
	var args []any
	if strings.TrimSpace(rawScope) != "" {
		sc := domain.NormalizeScope(rawScope)
		conds = append(conds, `scope = ?`)
		args = append(args, sc)
	}
	if olderThanDays > 0 {
		conds = append(conds, `datetime(COALESCE(NULLIF(updated_at, ''), created_at)) <= datetime('now', ?)`)
		args = append(args, fmt.Sprintf("-%d days", olderThanDays))
	}
	whereSQL := strings.Join(conds, " AND ")

	rows, err := s.db.QueryContext(ctx, `
		SELECT scope, COALESCE(memory_ids, ''), COALESCE(commit_sha, '')
		FROM project_progress
		WHERE `+whereSQL+`
		ORDER BY id ASC`, args...)
	if err != nil {
		return 0, err
	}

	maxMemByScope := make(map[string]int64)
	latestSHAByScope := make(map[string]string)
	candidateCount := 0
	for rows.Next() {
		var sc, memRaw, sha string
		if rows.Scan(&sc, &memRaw, &sha) == nil {
			candidateCount++
			for _, id := range ParseMemoryIDsStr(memRaw) {
				if id > maxMemByScope[sc] {
					maxMemByScope[sc] = id
				}
			}
			if cleanSHA := strings.TrimSpace(sha); cleanSHA != "" {
				latestSHAByScope[sc] = cleanSHA
			}
		}
	}
	rows.Close()
	if candidateCount == 0 {
		return 0, nil
	}

	cfgUpdates := make(map[string]string)
	for sc, maxID := range maxMemByScope {
		if maxID > s.getProgressPrunedMemHWM(ctx, sc) {
			cfgUpdates[config.CfgProgressPrunedMemHWM+sc] = strconv.FormatInt(maxID, 10)
		}
	}
	if len(cfgUpdates) > 0 {
		_ = s.mem.SaveConfig(ctx, cfgUpdates)
	}
	for sc, sha := range latestSHAByScope {
		if sha != "" && s.mem.GetSyncedCommit(ctx, sc) == "" {
			_ = s.mem.SetSyncedCommit(ctx, sc, sha)
		}
	}

	res, err := s.db.ExecContext(ctx, `DELETE FROM project_progress WHERE `+whereSQL, args...)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	if n > 0 {
		s.notifyProgressChange()
	}
	return int(n), nil
}

// MaybeRunAutoRetention runs automatic completed-progress TTL sweeps at most once per hour.
func (s *Service) MaybeRunAutoRetention(ctx context.Context) {
	if s == nil {
		return
	}
	now := time.Now().Unix()
	prev := s.lastRetentionSweep.Load()
	if prev > 0 && now-prev < 3600 {
		return
	}
	if !s.lastRetentionSweep.CompareAndSwap(prev, now) {
		return
	}
	pol := s.GetRetentionPolicy(ctx)
	if pol.ProgressDays > 0 {
		_, _ = s.CleanupProgressHistory(ctx, "", pol.ProgressDays)
	}
}
