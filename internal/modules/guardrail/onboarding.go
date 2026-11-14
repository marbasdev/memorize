package guardrail

import (
	"context"
	"fmt"
	"strings"
	"time"

	"memorize/internal/domain"
	"memorize/internal/infra/config"
)

// GetOnboardingStatus reports "pending", "skipped", or "completed" for rawScope.
func (s *Service) GetOnboardingStatus(ctx context.Context, rawScope string) string {
	sc := domain.NormalizeScope(rawScope)
	var st string
	if err := s.db.QueryRowContext(ctx, `SELECT status FROM project_onboarding WHERE scope = ?`, sc).Scan(&st); err == nil && strings.TrimSpace(st) != "" {
		return strings.TrimSpace(st)
	}
	var count int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM memories WHERE scope = ?`, sc).Scan(&count); err == nil && count > 0 {
		return "completed"
	}
	return "pending"
}

// SkipOnboarding marks a cold-start project scope as skipped so onboarding offers cease.
func (s *Service) SkipOnboarding(ctx context.Context, rawScope string) error {
	sc := domain.NormalizeScope(rawScope)
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO project_onboarding (scope, status, updated_at)
		VALUES (?, 'skipped', CURRENT_TIMESTAMP)
		ON CONFLICT(scope) DO UPDATE SET status = 'skipped', updated_at = CURRENT_TIMESTAMP
	`, sc)
	if err == nil && s.mem != nil {
		s.mem.MarkTurnScopeMutated(sc)
	}
	return err
}

// ResetOnboarding clears the skipped/completed state and session cooldown for rawScope.
func (s *Service) ResetOnboarding(ctx context.Context, rawScope string) error {
	sc := domain.NormalizeScope(rawScope)
	if _, err := s.db.ExecContext(ctx, `DELETE FROM project_onboarding WHERE scope = ?`, sc); err != nil {
		return err
	}
	_, _ = s.db.ExecContext(ctx, `DELETE FROM onboarding_prompts WHERE scope = ?`, sc)
	return nil
}

func (s *Service) tryClaimSessionNoticeCooldown(ctx context.Context, sessionID, noticeScope string, ttlSeconds int64) bool {
	sid := strings.TrimSpace(sessionID)
	if sid == "" {
		sid = "hook-session:" + noticeScope
	}
	now := time.Now().Unix()
	cutoff := now - ttlSeconds
	var prev int64
	if err := s.db.QueryRowContext(ctx,
		`SELECT updated_at FROM onboarding_prompts WHERE session_id = ? AND scope = ? AND updated_at >= ?`,
		sid, noticeScope, cutoff).Scan(&prev); err == nil && prev > 0 {
		return false
	}
	_, _ = s.db.ExecContext(ctx, `
		INSERT INTO onboarding_prompts (session_id, scope, updated_at)
		VALUES (?, ?, ?)
		ON CONFLICT(session_id, scope) DO UPDATE SET updated_at = excluded.updated_at
	`, sid, noticeScope, now)
	return true
}

// ConsumeEngineSetupOffer returns a once-per-session notice when no inference engine is ready,
// pointing the user directly to the Web Console Engines tab without running doomed LLM calls.
func (s *Service) ConsumeEngineSetupOffer(ctx context.Context, sessionID, reason string) string {
	if !s.tryClaimSessionNoticeCooldown(ctx, sessionID, "__engine_setup__", 3600) {
		return ""
	}
	detail := strings.TrimSpace(reason)
	if detail == "" {
		detail = "no active inference engine configured"
	}
	setupURL := strings.TrimRight(config.ActiveDaemonURL(s.dbPath), "/") + "/#engines"
	return fmt.Sprintf("[MEMORIZE ENGINE SETUP REQUIRED] Inference engine is not ready (%s). Do NOT run commit_memory or rule onboarding yet; guide the user to open %s (Web Console -> Engines tab) to download Local GGUF in 1 click or connect Ollama/LM Studio, Gemini, or System 1.", detail, setupURL)
}

// ConsumeOnboardingOffer checks with zero LLM/file overhead whether a cold-start project:<repo>
// has 0 stored memories and pending onboarding, emitting a 1-time prompt per session.
func (s *Service) ConsumeOnboardingOffer(ctx context.Context, sessionID, rawScope string) string {
	sc := domain.NormalizeScope(rawScope)
	if !strings.HasPrefix(sc, "project:") {
		return ""
	}
	if s.GetOnboardingStatus(ctx, sc) != "pending" {
		return ""
	}
	if !s.tryClaimSessionNoticeCooldown(ctx, sessionID, sc, 6*3600) {
		return ""
	}
	return fmt.Sprintf("[ONBOARDING AVAILABLE for %s] 0 project memories stored. Briefly ask the user whether to run initial rule onboarding: if declined, call manage_memory(action=\"skip_onboarding\"); if accepted, inspect key repo files—if conflicting implementations exist, ask the user which pattern takes priority BEFORE calling commit_memory, then offer to refactor the violating files.", sc)
}
