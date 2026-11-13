package memory

import (
	"context"
	"strings"
	"time"
)

type progressSessionCtxKey struct{}

// WithSessionID attaches an IDE / MCP conversation session ID to ctx so progress mutations can record it automatically.
func WithSessionID(ctx context.Context, sessionID string) context.Context {
	return context.WithValue(ctx, progressSessionCtxKey{}, strings.TrimSpace(sessionID))
}

// SessionIDFromContext extracts the attached session ID from ctx if present.
func SessionIDFromContext(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	if v, ok := ctx.Value(progressSessionCtxKey{}).(string); ok {
		return strings.TrimSpace(v)
	}
	return ""
}

// ResolveEffectiveSessionID resolves the active conversation/session ID for a progress record:
// explicit input wins, followed by non-generic context session IDs, active IDE hook turn_state
// conversation IDs for the scope, and finally generic MCP stdio/bridge session IDs.
func (s *Service) ResolveEffectiveSessionID(ctx context.Context, rawScope, explicit string) string {
	if exp := strings.TrimSpace(explicit); exp != "" {
		return exp
	}
	ctxSID := SessionIDFromContext(ctx)
	if ctxSID != "" && !strings.HasPrefix(ctxSID, "stdio-") && !strings.HasPrefix(ctxSID, "bridge-") {
		return ctxSID
	}
	sc := NormalizeScope(rawScope)
	var turnSID string
	cutoff := time.Now().Add(-2 * time.Hour).Unix()
	_ = s.db.QueryRowContext(ctx, `
		SELECT session_id FROM turn_state
		WHERE scope = ? AND updated_at >= ? AND COALESCE(session_id, '') != ''
		ORDER BY updated_at DESC LIMIT 1
	`, sc, cutoff).Scan(&turnSID)
	if strings.TrimSpace(turnSID) != "" {
		return strings.TrimSpace(turnSID)
	}
	return ctxSID
}

// ChangeEvent describes a state mutation with its domain ("progress", "memories", "guardrails", or "all").
type ChangeEvent struct {
	Seq    uint64
	Domain string
}

func mergeEventDomains(a, b string) string {
	da := strings.TrimSpace(a)
	db := strings.TrimSpace(b)
	if da == "" || da == "live" {
		return db
	}
	if db == "" || db == "live" || da == db {
		return da
	}
	return "all"
}

// NotifyLive wakes SSE subscribers with an ephemeral live-session ping without triggering full-tab DB reloads.
func (s *Service) NotifyLive() {
	seq := s.eventSeq.Add(1)
	s.eventMu.Lock()
	for ch := range s.domainEventSubs {
		ev := ChangeEvent{Seq: seq, Domain: "live"}
		select {
		case ch <- ev:
		default:
		}
	}
	s.eventMu.Unlock()
}

// NotifyDomainChange increments the monotonic state sequence and wakes all SSE subscribers without blocking.
func (s *Service) NotifyDomainChange(domain string) {
	dom := strings.TrimSpace(domain)
	if dom == "" {
		dom = "all"
	}
	seq := s.eventSeq.Add(1)
	s.eventMu.Lock()
	for ch := range s.eventSubs {
		select {
		case ch <- seq:
		default:
			select {
			case <-ch:
			default:
			}
			select {
			case ch <- seq:
			default:
			}
		}
	}
	for ch := range s.domainEventSubs {
		ev := ChangeEvent{Seq: seq, Domain: dom}
		select {
		case ch <- ev:
		default:
			select {
			case prev := <-ch:
				ev.Domain = mergeEventDomains(prev.Domain, dom)
			default:
			}
			select {
			case ch <- ev:
			default:
			}
		}
	}
	s.eventMu.Unlock()
}

// NotifyChange increments the monotonic state sequence and wakes all SSE subscribers without blocking.
func (s *Service) NotifyChange() {
	s.NotifyDomainChange("all")
}

// EventSeq returns the current monotonic state change sequence counter.
func (s *Service) EventSeq() uint64 {
	return s.eventSeq.Load()
}

// SubscribeChanges registers a buffered change-notification channel and returns (initialSeq, ch, unsubscribe).
func (s *Service) SubscribeChanges() (uint64, <-chan uint64, func()) {
	ch := make(chan uint64, 1)
	s.eventMu.Lock()
	if s.eventSubs == nil {
		s.eventSubs = make(map[chan uint64]struct{})
	}
	s.eventSubs[ch] = struct{}{}
	cur := s.eventSeq.Load()
	s.eventMu.Unlock()

	unsub := func() {
		s.eventMu.Lock()
		delete(s.eventSubs, ch)
		s.eventMu.Unlock()
	}
	return cur, ch, unsub
}

// SubscribeDomainChanges registers a buffered domain-aware change channel and returns (initialSeq, ch, unsubscribe).
func (s *Service) SubscribeDomainChanges() (uint64, <-chan ChangeEvent, func()) {
	ch := make(chan ChangeEvent, 4)
	s.eventMu.Lock()
	if s.domainEventSubs == nil {
		s.domainEventSubs = make(map[chan ChangeEvent]struct{})
	}
	s.domainEventSubs[ch] = struct{}{}
	cur := s.eventSeq.Load()
	s.eventMu.Unlock()

	unsub := func() {
		s.eventMu.Lock()
		delete(s.domainEventSubs, ch)
		s.eventMu.Unlock()
	}
	return cur, ch, unsub
}
