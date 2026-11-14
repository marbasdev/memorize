package guardrail

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

// SetGuardrailRetentionPolicy updates automatic TTL for guardrail audit logs and immediately sweeps expired entries.
func (s *Service) SetGuardrailRetentionPolicy(ctx context.Context, days int, includeBlocked *bool) (domain.RetentionPolicy, int, error) {
	norm := domain.NormalizeRetentionDays(days)
	pairs := map[string]string{
		config.CfgGuardrailRetentionDays: strconv.Itoa(norm),
	}
	if includeBlocked != nil {
		if *includeBlocked {
			pairs[config.CfgGuardrailIncludeBlocked] = "1"
		} else {
			pairs[config.CfgGuardrailIncludeBlocked] = "0"
		}
	}
	if err := s.mem.SaveConfig(ctx, pairs); err != nil {
		return s.GetRetentionPolicy(ctx), 0, err
	}
	pol := s.GetRetentionPolicy(ctx)
	deleted := 0
	if pol.GuardrailDays > 0 {
		deleted, _ = s.CleanupGuardrailLogs(ctx, "", pol.GuardrailDays, pol.GuardrailIncludeBlocked)
	}
	return pol, deleted, nil
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
		sc := domain.NormalizeScope(rawScope)
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
		s.notifyGuardrailsChange()
	}
	return int(n), nil
}

// MaybeRunAutoRetention runs automatic TTL sweeps at most once per hour for both progress history and guardrail logs.
func (s *Service) MaybeRunAutoRetention(ctx context.Context) {
	if s == nil {
		return
	}
	if s.prog != nil {
		s.prog.MaybeRunAutoRetention(ctx)
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
	if pol.GuardrailDays > 0 {
		_, _ = s.CleanupGuardrailLogs(ctx, "", pol.GuardrailDays, pol.GuardrailIncludeBlocked)
	}
}
