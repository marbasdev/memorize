package eventbus

import (
	"sync"
	"testing"
)

func TestBusPublishSubscribeMergeAndClose(t *testing.T) {
	t.Run("nil_safety", func(t *testing.T) {
		var nilBus *Bus
		nilBus.Publish("memories")
		if nilBus.Seq() != 0 {
			t.Fatalf("expected 0 seq on nil bus")
		}
		nilBus.Close()
	})

	t.Run("publish_and_unsubscribe", func(t *testing.T) {
		b := New()
		seq0, ch, unsub := b.SubscribeDomainChanges()
		if seq0 != 0 {
			t.Fatalf("expected initial seq 0, got %d", seq0)
		}
		b.Publish("  ")
		b.Publish("memories")

		ev1 := <-ch
		ev2 := <-ch
		if ev1.Domain != "all" || ev1.Seq != 1 {
			t.Fatalf("unexpected first event: %+v", ev1)
		}
		if ev2.Domain != "memories" || ev2.Seq != 2 {
			t.Fatalf("unexpected second event: %+v", ev2)
		}
		if b.Seq() != 2 {
			t.Fatalf("expected Seq() == 2, got %d", b.Seq())
		}

		unsub()
		b.Publish("progress")
		select {
		case ev := <-ch:
			t.Fatalf("did not expect event after unsubscribe: %+v", ev)
		default:
		}
	})

	t.Run("buffer_overflow_merges_domains", func(t *testing.T) {
		b := New()
		_, ch, unsub := b.SubscribeDomainChanges()
		defer unsub()

		for range 16 {
			b.Publish("memories")
		}
		b.Publish("progress")

		seenAll := false
		for len(ch) > 0 {
			ev := <-ch
			if ev.Domain == "all" {
				seenAll = true
			}
		}
		if !seenAll {
			t.Fatalf("expected overflowing subscriber channel to coalesce differing domains to 'all'")
		}
	})

	t.Run("close_and_concurrent_race", func(t *testing.T) {
		b := New()
		_, ch, _ := b.SubscribeDomainChanges()

		var wg sync.WaitGroup
		for range 4 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for range 30 {
					b.Publish("memories")
				}
			}()
		}
		for range 2 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				_, sub, unsub := b.SubscribeDomainChanges()
				unsub()
				for range sub {
				}
			}()
		}

		b.Close()
		b.Close()
		wg.Wait()

		for range ch {
		}
		_, closedCh, unsub := b.SubscribeDomainChanges()
		defer unsub()
		if _, ok := <-closedCh; ok {
			t.Fatalf("expected SubscribeDomainChanges on closed bus to return closed channel")
		}
	})
}
