package feed

import (
	"context"
	"sync"
	"testing"
	"time"
)

func TestPublishWakesAllWaiters(t *testing.T) {
	n := NewNotifier()
	const waiters = 5
	var started, woke sync.WaitGroup
	started.Add(waiters)
	woke.Add(waiters)
	for range waiters {
		go func() {
			ready := n.Ready()
			started.Done()
			<-ready
			woke.Done()
		}()
	}
	started.Wait()
	n.Publish()

	done := make(chan struct{})
	go func() { woke.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("one Publish did not wake all waiters")
	}
	// The next generation is a fresh, open channel.
	select {
	case <-n.Ready():
		t.Fatal("new Ready channel already closed")
	default:
	}
}

func TestWaitStopsOnCancel(t *testing.T) {
	n := NewNotifier()
	ctx, cancel := context.WithCancel(context.Background())
	out := make(chan time.Duration, 1)
	go func() {
		start := time.Now()
		select {
		case <-n.Ready():
		case <-ctx.Done():
		}
		out <- time.Since(start)
	}()
	time.Sleep(20 * time.Millisecond)
	cancel()
	select {
	case d := <-out:
		if d > time.Second {
			t.Fatalf("waiter returned %s after cancel", d)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("waiter leaked: still blocked after cancel")
	}
}

func TestNoLostWakeup(t *testing.T) {
	// Ready taken before Publish must fire even if the wait starts later.
	n := NewNotifier()
	ready := n.Ready()
	n.Publish()
	select {
	case <-ready:
	case <-time.After(time.Second):
		t.Fatal("publish between check and wait was lost")
	}
}

func TestConcurrentPublishAndWait(t *testing.T) {
	n := NewNotifier()
	var wg sync.WaitGroup
	for range 50 {
		wg.Go(func() { n.Publish() })
		wg.Go(func() {
			select {
			case <-n.Ready():
			case <-time.After(time.Millisecond):
			}
		})
	}
	wg.Wait()
}
