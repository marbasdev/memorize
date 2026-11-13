package memory

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"
)

const (
	DefaultProgressRetentionDays  = 0
	DefaultGuardrailRetentionDays = 0

	cfgProgressRetentionDays   = "progress_retention_days"
	cfgGuardrailRetentionDays  = "guardrail_retention_days"
	cfgGuardrailIncludeBlocked = "guardrail_retention_include_blocked"
	cfgProgressPrunedMemHWM    = "progress_pruned_mem_hwm:"
)

// RetentionPolicy defines automatic TTL retention settings for completed progress history and guardrail audit logs.
type RetentionPolicy struct {
	ProgressDays            int  `json:"progress_days"`             // 0 = keep forever; >0 = auto-prune done entries older than N days
	GuardrailDays           int  `json:"guardrail_days"`            // 0 = keep forever; >0 = auto-prune logs older than N days
	GuardrailIncludeBlocked bool `json:"guardrail_include_blocked"` // false = auto-prune PASS logs only (protect blocked violations)
}

// NormalizeRetentionDays clamps retention days to 0 (keep forever) or [1..3650].
func NormalizeRetentionDays(days int) int {
	if days <= 0 {
		return 0
	}
	if days > 3650 {
		return 3650
	}
	return days
}

// GetRetentionPolicy returns the active retention policy from config with safe defaults.
func (s *Service) GetRetentionPolicy(ctx context.Context) RetentionPolicy {
	cfg := s.GetConfigMap(ctx)
	progDays := DefaultProgressRetentionDays
	if raw, ok := cfg[cfgProgressRetentionDays]; ok && strings.TrimSpace(raw) != "" {
		if v, err := strconv.Atoi(strings.TrimSpace(raw)); err == nil {
			progDays = NormalizeRetentionDays(v)
		}
	}
	grDays := DefaultGuardrailRetentionDays
	if raw, ok := cfg[cfgGuardrailRetentionDays]; ok && strings.TrimSpace(raw) != "" {
		if v, err := strconv.Atoi(strings.TrimSpace(raw)); err == nil {
			grDays = NormalizeRetentionDays(v)
		}
	}
	return RetentionPolicy{
		ProgressDays:            progDays,
		GuardrailDays:           grDays,
		GuardrailIncludeBlocked: cfg[cfgGuardrailIncludeBlocked] == "1",
	}
}

// SetProgressRetentionDays updates automatic TTL for completed progress history and immediately sweeps expired entries.
func (s *Service) SetProgressRetentionDays(ctx context.Context, days int) (RetentionPolicy, int, error) {
	norm := NormalizeRetentionDays(days)
	if err := s.SaveConfig(ctx, map[string]string{
		cfgProgressRetentionDays: strconv.Itoa(norm),
	}); err != nil {
		return s.GetRetentionPolicy(ctx), 0, err
	}
	deleted := 0
	if norm > 0 {
		deleted, _ = s.CleanupProgressHistory(ctx, "", norm)
	}
	return s.GetRetentionPolicy(ctx), deleted, nil
}

// SetGuardrailRetentionPolicy updates automatic TTL for guardrail audit logs and immediately sweeps expired entries.
func (s *Service) SetGuardrailRetentionPolicy(ctx context.Context, days int, includeBlocked *bool) (RetentionPolicy, int, error) {
	norm := NormalizeRetentionDays(days)
	pairs := map[string]string{
		cfgGuardrailRetentionDays: strconv.Itoa(norm),
	}
	if includeBlocked != nil {
		if *includeBlocked {
			pairs[cfgGuardrailIncludeBlocked] = "1"
		} else {
			pairs[cfgGuardrailIncludeBlocked] = "0"
		}
	}
	if err := s.SaveConfig(ctx, pairs); err != nil {
		return s.GetRetentionPolicy(ctx), 0, err
	}
	pol := s.GetRetentionPolicy(ctx)
	deleted := 0
	if pol.GuardrailDays > 0 {
		deleted, _ = s.CleanupGuardrailLogs(ctx, "", pol.GuardrailDays, pol.GuardrailIncludeBlocked)
	}
	return pol, deleted, nil
}

func (s *Service) getProgressPrunedMemHWM(ctx context.Context, scope string) int64 {
	sc := NormalizeScope(scope)
	var raw string
	_ = s.db.QueryRowContext(ctx, `SELECT val FROM config WHERE key = ?`, cfgProgressPrunedMemHWM+sc).Scan(&raw)
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
		sc := NormalizeScope(rawScope)
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
			for _, id := range parseMemoryIDsStr(memRaw) {
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
			cfgUpdates[cfgProgressPrunedMemHWM+sc] = strconv.FormatInt(maxID, 10)
		}
	}
	if len(cfgUpdates) > 0 {
		_ = s.SaveConfig(ctx, cfgUpdates)
	}
	for sc, sha := range latestSHAByScope {
		if sha != "" && s.GetSyncedCommit(ctx, sc) == "" {
			_ = s.SetSyncedCommit(ctx, sc, sha)
		}
	}

	res, err := s.db.ExecContext(ctx, `DELETE FROM project_progress WHERE `+whereSQL, args...)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	if n > 0 {
		s.NotifyDomainChange("progress")
	}
	return int(n), nil
}

// CleanupGuardrailLogs deletes guardrail verification audit logs matching scope and age cutoff
// (olderThanDays == 0 deletes all ages; >0 deletes entries older than N days).
// When includeBlocked is false, only PASS entries (blocked = 0) are removed and violation/blocked audit logs are preserved.
func (s *Service) CleanupGuardrailLogs(ctx context.Context, rawScope string, olderThanDays int, includeBlocked bool) (int, error) {
	if olderThanDays < 0 {
		return 0, fmt.Errorf("older_than_days must be >= 0")
	}
	var conds []string
	var args []any
	if !includeBlocked {
		conds = append(conds, `blocked = 0`)
	}
	if strings.TrimSpace(rawScope) != "" {
		sc := NormalizeScope(rawScope)
		conds = append(conds, `scope = ?`)
		args = append(args, sc)
	}
	if olderThanDays > 0 {
		conds = append(conds, `datetime(created_at) <= datetime('now', ?)`)
		args = append(args, fmt.Sprintf("-%d days", olderThanDays))
	}
	whereSQL := "1 = 1"
	if len(conds) > 0 {
		whereSQL = strings.Join(conds, " AND ")
	}
	res, err := s.db.ExecContext(ctx, `DELETE FROM guardrail_logs WHERE `+whereSQL, args...)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	if n > 0 {
		s.NotifyDomainChange("guardrails")
	}
	return int(n), nil
}

// MaybeRunAutoRetention runs automatic TTL sweeps at most once per hour so daemon startup and session hooks stay zero-overhead.
func (s *Service) MaybeRunAutoRetention(ctx context.Context) {
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
	if pol.GuardrailDays > 0 {
		_, _ = s.CleanupGuardrailLogs(ctx, "", pol.GuardrailDays, pol.GuardrailIncludeBlocked)
	}
}
