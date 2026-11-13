package memory

import (
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const (
	// LiveSessionTTL expires abandoned in-memory sessions lazily on read with no background sweeper.
	LiveSessionTTL    = 90 * time.Second
	maxLiveSessionCap = 64
)

// LiveSession represents an ephemeral active IDE/MCP session kept strictly in memory.
type LiveSession struct {
	SessionID     string  `json:"session_id"`
	Scope         string  `json:"scope"`
	Project       string  `json:"project"`
	Tool          string  `json:"tool,omitempty"`
	Target        string  `json:"target,omitempty"`
	StartedAt     string  `json:"started_at,omitempty"`
	UpdatedAt     string  `json:"updated_at"`
	AgeSec        int64   `json:"age_sec"`
	SessionAgeSec int64   `json:"session_age_sec,omitempty"`
	HookCalls     int     `json:"hook_calls,omitempty"`
	HookElapsedMs float64 `json:"hook_elapsed_ms,omitempty"`
	startedAt     time.Time
	updatedAt     time.Time
}

func sanitizeLiveTarget(rawPath string) string {
	p := strings.TrimSpace(rawPath)
	if p == "" {
		return ""
	}
	base := strings.TrimSpace(filepath.Base(p))
	if base == "" || base == "." || base == "/" || checkSecurityGuard(base) != "" {
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
	sc := NormalizeScope(rawScope)
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
		s.liveSessions = make(map[string]LiveSession)
	}
	for k, v := range s.liveSessions {
		if now.Sub(v.updatedAt) > LiveSessionTTL {
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
		hookCalls = prev.HookCalls
		hookTotalMs = prev.HookElapsedMs
		if target == "" {
			target = prev.Target
		}
		if tName == "" {
			tName = prev.Tool
		}
	}
	if hookCountDelta > 0 {
		hookCalls += hookCountDelta
		hookTotalMs += elapsedMs
	}
	s.liveSessions[key] = LiveSession{
		SessionID:     sid,
		Scope:         sc,
		Project:       ProjectShortName(sc),
		Tool:          tName,
		Target:        target,
		StartedAt:     startedAt.In(time.Local).Format(time.RFC3339),
		UpdatedAt:     now.In(time.Local).Format(time.RFC3339),
		HookCalls:     hookCalls,
		HookElapsedMs: hookTotalMs,
		startedAt:     startedAt,
		updatedAt:     now,
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
	s.NotifyLive()
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
		sc = NormalizeScope(sc)
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
				if v.SessionID == sid {
					delete(s.liveSessions, k)
					removed = true
				}
			}
		}
	}
	s.mu.Unlock()
	if removed {
		s.NotifyLive()
	}
}

// ListLiveSessions returns non-expired active sessions sorted most-recent first.
func (s *Service) ListLiveSessions() []LiveSession {
	if s == nil {
		return nil
	}
	now := time.Now()
	s.mu.Lock()
	out := make([]LiveSession, 0, len(s.liveSessions))
	for k, v := range s.liveSessions {
		age := now.Sub(v.updatedAt)
		if age > LiveSessionTTL {
			delete(s.liveSessions, k)
			continue
		}
		item := v
		item.AgeSec = max(int64(0), int64(age.Seconds()))
		if !v.startedAt.IsZero() {
			item.SessionAgeSec = max(int64(0), int64(now.Sub(v.startedAt).Seconds()))
		}
		out = append(out, item)
	}
	s.mu.Unlock()
	sort.Slice(out, func(i, j int) bool {
		if !out[i].updatedAt.Equal(out[j].updatedAt) {
			return out[i].updatedAt.After(out[j].updatedAt)
		}
		return out[i].SessionID < out[j].SessionID
	})
	return out
}
