package eventbus

import (
	"strings"
	"sync"
	"sync/atomic"

	"memorize/internal/domain"
)

type Bus struct {
	mu         sync.Mutex
	closed     bool
	domainSubs map[chan domain.ChangeEvent]struct{}
	seq        atomic.Uint64
}

func New() *Bus {
	return &Bus{
		domainSubs: make(map[chan domain.ChangeEvent]struct{}),
	}
}

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
	if b.closed {
		b.mu.Unlock()
		return
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

func (b *Bus) Seq() uint64 {
	if b == nil {
		return 0
	}
	return b.seq.Load()
}

func (b *Bus) SubscribeDomainChanges() (uint64, <-chan domain.ChangeEvent, func()) {
	ch := make(chan domain.ChangeEvent, 16)
	b.mu.Lock()
	cur := b.seq.Load()
	if b.closed {
		close(ch)
		b.mu.Unlock()
		return cur, ch, func() {}
	}
	if b.domainSubs == nil {
		b.domainSubs = make(map[chan domain.ChangeEvent]struct{})
	}
	b.domainSubs[ch] = struct{}{}
	b.mu.Unlock()

	unsub := func() {
		b.mu.Lock()
		delete(b.domainSubs, ch)
		b.mu.Unlock()
	}
	return cur, ch, unsub
}

func (b *Bus) Close() {
	if b == nil {
		return
	}
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return
	}
	b.closed = true
	for ch := range b.domainSubs {
		delete(b.domainSubs, ch)
		close(ch)
	}
	b.mu.Unlock()
}
