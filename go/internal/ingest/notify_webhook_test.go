package ingest

import (
	"context"
	"sync"
	"testing"

	"github.com/RigelBuild/compass/go/internal/forge"
)

// fakeEventRouter is a scripted eventRouter that records every routed event so a
// test can assert what the drain handed to Route. errFirst returns errBoom only
// on the first Route call (then succeeds), so a test can prove the drain
// survives a per-event error.
type fakeEventRouter struct {
	mu       sync.Mutex
	routed   []forge.ForgeEvent
	served   int
	errFirst bool
	notify   chan struct{}
}

func (r *fakeEventRouter) Route(_ context.Context, ev forge.ForgeEvent) error {
	r.mu.Lock()
	r.served++
	first := r.served == 1
	r.routed = append(r.routed, ev)
	notify := r.notify
	r.mu.Unlock()
	if notify != nil {
		notify <- struct{}{}
	}
	if r.errFirst && first {
		return errBoom
	}
	return nil
}

func (r *fakeEventRouter) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.routed)
}

func notifyEvent(number uint64) forge.ForgeEvent {
	return forge.ForgeEvent{Repo: "owner/repo", Number: number, Change: changeUpdate}
}

// TestNotifyWebhookArmRoutesEnqueuedEvent: an enqueued event reaches Route with
// the drain running, and Run returns nil on cancel. Gated on the router's notify
// channel, never a clock.
func TestNotifyWebhookArmRoutesEnqueuedEvent(t *testing.T) {
	r := &fakeEventRouter{notify: make(chan struct{}, 1)}
	arm := NewNotifyWebhookArm(r, NotifyArmConfig{QueueSize: 16})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- arm.Run(ctx) }()

	ev := notifyEvent(1)
	arm.Enqueue(context.Background(), ev)
	<-r.notify // gates on Route being called (bounded by the goroutine, no sleep)

	if r.count() != 1 {
		t.Fatalf("routed = %d, want 1", r.count())
	}
	if got := r.routed[0]; got.Repo != "owner/repo" || got.Number != 1 {
		t.Fatalf("routed event = %+v, want owner/repo #1", got)
	}

	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Run returned %v, want nil on cancel", err)
	}
}

// TestNotifyWebhookArmEnqueueNeverBlocksAndDrops: with a queue of capacity 1 and
// no drain running, three Enqueues never block and at least one is dropped.
func TestNotifyWebhookArmEnqueueNeverBlocksAndDrops(t *testing.T) {
	r := &fakeEventRouter{}
	arm := NewNotifyWebhookArm(r, NotifyArmConfig{QueueSize: 1})

	done := make(chan struct{})
	go func() {
		for i := range 3 {
			arm.Enqueue(context.Background(), notifyEvent(uint64(i+1)))
		}
		close(done)
	}()
	<-done // gates on all three Enqueues returning; hangs the test if any blocks

	if arm.Dropped() < 1 {
		t.Fatalf("Dropped() = %d, want >= 1 (queue cap 1, 3 enqueued)", arm.Dropped())
	}
}

// TestNotifyWebhookArmRouteErrorDoesNotStopDrain: the first event's Route errors;
// the drain logs-and-continues, so the second event is still routed, and Run
// exits nil on cancel.
func TestNotifyWebhookArmRouteErrorDoesNotStopDrain(t *testing.T) {
	r := &fakeEventRouter{errFirst: true, notify: make(chan struct{}, 2)}
	arm := NewNotifyWebhookArm(r, NotifyArmConfig{QueueSize: 16})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- arm.Run(ctx) }()

	arm.Enqueue(context.Background(), notifyEvent(1))
	arm.Enqueue(context.Background(), notifyEvent(2))
	<-r.notify // first Route (errors)
	<-r.notify // second Route (drain survived the first error)

	if r.count() != 2 {
		t.Fatalf("routed = %d, want 2 (drain survived the first error)", r.count())
	}
	if got := r.routed[1]; got.Number != 2 {
		t.Fatalf("second routed Number = %d, want 2", got.Number)
	}

	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Run returned %v, want nil on cancel", err)
	}
}
