package progress

import (
	"path/filepath"
	"sort"
	"strings"
	"time"

	"memorize/internal/domain"
)

const maxLiveSessionCap = 64

type liveSessionEntry struct {
	session   domain.LiveSession
	startedAt time.Time
	updatedAt time.Time
}

func sanitizeLiveTarget(rawPath string) string {
	p := strings.TrimSpace(rawPath)
	if p == "" {
		return ""
	}
	base := strings.TrimSpace(filepath.Base(p))
	if base == "" || base == "." || base == "/" || domain.CheckSecurityGuard(base) != "" {
		return ""
	}
	return base
}

func isTrackableLiveSessionID(sid string) bool {
	return sid != "" &&
		!strings.HasPrefix(sid, "hook-session:") &&
		!strings.HasPrefix(sid, "stdio-") &&
		!strings.HasPrefix(sid, "bridge-")
}

func (s *Service) updateLiveSession(rawScope, sessionID, tool, targetPath string, hookCountDelta int, hookElapsed time.Duration) {
	if s == nil {
		return
	}
	sc := domain.NormalizeScope(rawScope)
	sid := strings.TrimSpace(sessionID)
	if sc == "" || !isTrackableLiveSessionID(sid) {
		return
	}
	now := time.Now()
	target := sanitizeLiveTarget(targetPath)
	tName := strings.TrimSpace(tool)
	key := strings.ToLower(sc) + "::" + sid
	elapsedMs := float64(hookElapsed.Microseconds()) / 1000.0
	if elapsedMs < 0 {
		elapsedMs = 0
	}

	s.mu.Lock()
	if s.liveSessions == nil {
		s.liveSessions = make(map[string]liveSessionEntry)
	}
	for k, v := range s.liveSessions {
		if now.Sub(v.updatedAt) > domain.LiveSessionTTL {
			delete(s.liveSessions, k)
		}
	}
	prev, exists := s.liveSessions[key]
	startedAt := now
	hookCalls := 0
	hookTotalMs := 0.0
	if exists {
		if !prev.startedAt.IsZero() {
			startedAt = prev.startedAt
		}
		hookCalls = prev.session.HookCalls
		hookTotalMs = prev.session.HookElapsedMs
		if target == "" {
			target = prev.session.Target
		}
		if tName == "" {
			tName = prev.session.Tool
		}
	}
	if hookCountDelta > 0 {
		hookCalls += hookCountDelta
		hookTotalMs += elapsedMs
	}
	s.liveSessions[key] = liveSessionEntry{
		session: domain.LiveSession{
			SessionID:     sid,
			Scope:         sc,
			Project:       domain.ProjectShortName(sc),
			Tool:          tName,
			Target:        target,
			StartedAt:     startedAt.In(time.Local).Format(time.RFC3339),
			UpdatedAt:     now.In(time.Local).Format(time.RFC3339),
			HookCalls:     hookCalls,
			HookElapsedMs: hookTotalMs,
		},
		startedAt: startedAt,
		updatedAt: now,
	}
	for len(s.liveSessions) > maxLiveSessionCap {
		oldestKey := ""
		var oldestTime time.Time
		for k, v := range s.liveSessions {
			if k == key {
				continue
			}
			if oldestKey == "" || v.updatedAt.Before(oldestTime) {
				oldestKey = k
				oldestTime = v.updatedAt
			}
		}
		if oldestKey == "" {
			break
		}
		delete(s.liveSessions, oldestKey)
	}
	s.mu.Unlock()
	s.notifyLive()
}

// TouchLiveSession updates the in-memory heartbeat for an explicit active session without touching SQLite.
func (s *Service) TouchLiveSession(rawScope, sessionID, tool, targetPath string) {
	s.updateLiveSession(rawScope, sessionID, tool, targetPath, 0, 0)
}

// RecordLiveHookElapsed accumulates in-memory hook call count and elapsed duration on an active session.
func (s *Service) RecordLiveHookElapsed(rawScope, sessionID string, elapsed time.Duration) {
	s.updateLiveSession(rawScope, sessionID, "", "", 1, elapsed)
}

// ClearLiveSession removes an explicit active session on turn/session completion.
func (s *Service) ClearLiveSession(rawScope, sessionID string) {
	if s == nil {
		return
	}
	sid := strings.TrimSpace(sessionID)
	if sid == "" {
		return
	}
	sc := strings.TrimSpace(rawScope)
	if sc != "" {
		sc = domain.NormalizeScope(sc)
	}

	removed := false
	s.mu.Lock()
	if s.liveSessions != nil {
		if sc != "" {
			key := strings.ToLower(sc) + "::" + sid
			if _, ok := s.liveSessions[key]; ok {
				delete(s.liveSessions, key)
				removed = true
			}
		} else {
			for k, v := range s.liveSessions {
				if v.session.SessionID == sid {
					delete(s.liveSessions, k)
					removed = true
				}
			}
		}
	}
	s.mu.Unlock()
	if removed {
		s.notifyLive()
	}
}

// ListLiveSessions returns non-expired active sessions sorted most-recent first.
func (s *Service) ListLiveSessions() []domain.LiveSession {
	if s == nil {
		return nil
	}
	now := time.Now()
	s.mu.Lock()
	items := make([]liveSessionEntry, 0, len(s.liveSessions))
	for k, v := range s.liveSessions {
		age := now.Sub(v.updatedAt)
		if age > domain.LiveSessionTTL {
			delete(s.liveSessions, k)
			continue
		}
		item := v
		item.session.AgeSec = max(int64(0), int64(age.Seconds()))
		if !v.startedAt.IsZero() {
			item.session.SessionAgeSec = max(int64(0), int64(now.Sub(v.startedAt).Seconds()))
		}
		items = append(items, item)
	}
	s.mu.Unlock()
	sort.Slice(items, func(i, j int) bool {
		if !items[i].updatedAt.Equal(items[j].updatedAt) {
			return items[i].updatedAt.After(items[j].updatedAt)
		}
		return items[i].session.SessionID < items[j].session.SessionID
	})
	out := make([]domain.LiveSession, len(items))
	for i, it := range items {
		out[i] = it.session
	}
	return out
}
