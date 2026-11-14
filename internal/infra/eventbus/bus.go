package eventbus

import (
	"strings"
	"sync"
	"sync/atomic"

	"memorize/internal/domain"
)

// Bus coordinates non-blocking monotonic change notifications for SSE subscribers.
type Bus struct {
	mu         sync.Mutex
	subs       map[chan uint64]struct{}
	domainSubs map[chan domain.ChangeEvent]struct{}
	seq        atomic.Uint64
}

// New constructs an initialized Bus.
func New() *Bus {
	return &Bus{
		subs:       make(map[chan uint64]struct{}),
		domainSubs: make(map[chan domain.ChangeEvent]struct{}),
	}
}

// PublishLive emits an ephemeral live-session ping without triggering full DB reloads.
func (b *Bus) PublishLive() {
	if b == nil {
		return
	}
	seq := b.seq.Add(1)
	b.mu.Lock()
	for ch := range b.domainSubs {
		ev := domain.ChangeEvent{Seq: seq, Domain: "live"}
		select {
		case ch <- ev:
		default:
		}
	}
	b.mu.Unlock()
}

// Publish increments the monotonic state sequence and wakes SSE subscribers for dom.
func (b *Bus) Publish(dom string) {
	if b == nil {
		return
	}
	d := strings.TrimSpace(dom)
	if d == "" {
		d = "all"
	}
	seq := b.seq.Add(1)
	b.mu.Lock()
	for ch := range b.subs {
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
	for ch := range b.domainSubs {
		ev := domain.ChangeEvent{Seq: seq, Domain: d}
		select {
		case ch <- ev:
		default:
			select {
			case prev := <-ch:
				ev.Domain = domain.MergeEventDomains(prev.Domain, d)
			default:
			}
			select {
			case ch <- ev:
			default:
			}
		}
	}
	b.mu.Unlock()
}

// PublishAll broadcasts a global ("all") state mutation notification.
func (b *Bus) PublishAll() {
	b.Publish("all")
}

// Seq returns the current monotonic state change sequence counter.
func (b *Bus) Seq() uint64 {
	if b == nil {
		return 0
	}
	return b.seq.Load()
}

// SubscribeChanges registers a buffered change-notification channel and returns (initialSeq, ch, unsubscribe).
func (b *Bus) SubscribeChanges() (uint64, <-chan uint64, func()) {
	ch := make(chan uint64, 1)
	b.mu.Lock()
	if b.subs == nil {
		b.subs = make(map[chan uint64]struct{})
	}
	b.subs[ch] = struct{}{}
	cur := b.seq.Load()
	b.mu.Unlock()

	unsub := func() {
		b.mu.Lock()
		delete(b.subs, ch)
		b.mu.Unlock()
	}
	return cur, ch, unsub
}

// SubscribeDomainChanges registers a buffered domain-aware change channel and returns (initialSeq, ch, unsubscribe).
func (b *Bus) SubscribeDomainChanges() (uint64, <-chan domain.ChangeEvent, func()) {
	ch := make(chan domain.ChangeEvent, 4)
	b.mu.Lock()
	if b.domainSubs == nil {
		b.domainSubs = make(map[chan domain.ChangeEvent]struct{})
	}
	b.domainSubs[ch] = struct{}{}
	cur := b.seq.Load()
	b.mu.Unlock()

	unsub := func() {
		b.mu.Lock()
		delete(b.domainSubs, ch)
		b.mu.Unlock()
	}
	return cur, ch, unsub
}
