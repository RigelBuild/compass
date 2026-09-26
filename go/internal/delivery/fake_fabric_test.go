//go:build unix

package delivery

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/RigelBuild/compass/go/internal/fabric"
	otelx "github.com/RigelBuild/compass/go/internal/otel"
	"github.com/RigelBuild/compass/go/internal/store"
)

// testTenant is the tenant the fake-store tests publish their refs under.
const testTenant store.TenantID = "tenant-1"

// fakeFabric is an in-memory fabric.EventFabric. Publish queues a ref the way
// the stream retains one, so a ref published before SubscribeKind is delivered
// once the consumer subscribes. One goroutine runs the callback serially, as the
// fabric promises, and fireReconnect runs the OnReconnect hooks. A callback
// error is recorded and redelivered until maxDeliver attempts, then dropped.
type fakeFabric struct {
	queue      chan fakeEvent
	subscribed chan struct{}
	subOnce    sync.Once
	// beforeSubscribe, when set, runs at the top of SubscribeKind so a test can
	// observe what Run finished before it subscribed.
	beforeSubscribe func()
	maxDeliver      int

	mu       sync.Mutex
	hooks    map[int]func()
	nextHook int
	acked    []string
	ackSig   chan struct{}
	failures map[string][]error
}

// fakeEvent is one queued publish: the ref plus the publisher's traceparent,
// which the real fabric carries in a message header, and the attempts so far.
type fakeEvent struct {
	ref         fabric.EventRef
	traceparent string
	attempts    int
}

func newFakeFabric() *fakeFabric {
	return &fakeFabric{
		queue:      make(chan fakeEvent, 1024),
		subscribed: make(chan struct{}),
		maxDeliver: fabric.DefaultMaxDeliver,
		hooks:      map[int]func(){},
		ackSig:     make(chan struct{}, 1024),
		failures:   map[string][]error{},
	}
}

// fakeFabricOf returns the fake fabric a test consumer was built over.
func fakeFabricOf(c *Consumer) *fakeFabric {
	return c.fab.(*fakeFabric)
}

func (f *fakeFabric) Publish(ctx context.Context, _ string, ref fabric.EventRef) error {
	select {
	case f.queue <- fakeEvent{ref: ref, traceparent: otelx.Traceparent(ctx)}:
		return nil
	default:
		return errors.New("fake fabric: publish queue full")
	}
}

// Subscribe is the concrete-subject read side, which the consumer must not use.
func (f *fakeFabric) Subscribe(context.Context, string, func(context.Context, fabric.EventRef) error) (fabric.Unsubscribe, error) {
	return nil, errors.New("fake fabric: the delivery consumer subscribes by kind only")
}

func (f *fakeFabric) SubscribeKind(ctx context.Context, kind fabric.EventKind, fn func(context.Context, fabric.EventRef) error) (fabric.Unsubscribe, error) {
	if kind != fabric.KindMessagePosted {
		return nil, fmt.Errorf("fake fabric: unexpected kind %q", kind)
	}
	if f.beforeSubscribe != nil {
		f.beforeSubscribe()
	}
	// The real fabric's stream setup fails on a cancelled ctx.
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("fake fabric: ensuring stream: %w", err)
	}
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		// Nak'd events go here and run before the next queued one, like an
		// immediate redelivery; the goroutine owns the slice, so it needs no lock.
		var redeliver []fakeEvent
		for {
			var ev fakeEvent
			if len(redeliver) > 0 {
				// A retry still honours teardown, as the real consumer's drain does.
				select {
				case <-ctx.Done():
					return
				case <-stop:
					return
				default:
				}
				ev, redeliver = redeliver[0], redeliver[1:]
			} else {
				select {
				case <-ctx.Done():
					return
				case <-stop:
					return
				case ev = <-f.queue:
				}
			}
			// The real fabric extracts the header's span onto the subscription ctx.
			ev.attempts++
			if err := fn(otelx.ContextWithTraceparent(ctx, ev.traceparent), ev.ref); err != nil {
				if f.fail(ev, err) {
					redeliver = append(redeliver, ev)
				}
				continue
			}
			f.ack(ev.ref.RowID)
		}
	}()
	f.subOnce.Do(func() { close(f.subscribed) })
	var once sync.Once
	return func() {
		once.Do(func() {
			close(stop)
			<-done
		})
	}, nil
}

func (f *fakeFabric) OnReconnect(fn func()) (fabric.Unsubscribe, error) {
	if fn == nil {
		return nil, errors.New("fake fabric: OnReconnect requires a callback")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nextHook++
	id := f.nextHook
	f.hooks[id] = fn
	return func() {
		f.mu.Lock()
		defer f.mu.Unlock()
		delete(f.hooks, id)
	}, nil
}

// fireReconnect runs every registered hook, as the fabric does after it logs a
// NATS reconnect.
func (f *fakeFabric) fireReconnect() {
	f.mu.Lock()
	hooks := make([]func(), 0, len(f.hooks))
	for _, fn := range f.hooks {
		hooks = append(hooks, fn)
	}
	f.mu.Unlock()
	for _, fn := range hooks {
		fn()
	}
}

// ack records that the callback for rowID returned, which is what makes the
// real fabric ack the message.
func (f *fakeFabric) ack(rowID string) {
	f.mu.Lock()
	f.acked = append(f.acked, rowID)
	f.mu.Unlock()
	signalObserved(f.ackSig)
}

// fail records a callback error and reports whether to redeliver (a Nak): true
// until maxDeliver attempts are spent, after which the event is dropped (parked).
func (f *fakeFabric) fail(ev fakeEvent, err error) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failures[ev.ref.RowID] = append(f.failures[ev.ref.RowID], err)
	return ev.attempts < f.maxDeliver
}

// failuresFor returns the callback errors recorded for rowID, in order.
func (f *fakeFabric) failuresFor(rowID string) []error {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.failures[rowID])
}

// waitAcked blocks until the callback for rowID has returned, or fails at the
// deadline.
func (f *fakeFabric) waitAcked(t *testing.T, rowID string) {
	t.Helper()
	deadline := time.After(testTimeout)
	for {
		f.mu.Lock()
		acked := slices.Contains(f.acked, rowID)
		f.mu.Unlock()
		if acked {
			return
		}
		select {
		case <-f.ackSig:
		case <-deadline:
			t.Fatalf("ref %q never acked: its callback did not return", rowID)
		}
	}
}

// waitSubscribed blocks until Run has subscribed, which it does only after its
// start scan completes.
func (f *fakeFabric) waitSubscribed(t *testing.T) {
	t.Helper()
	select {
	case <-f.subscribed:
	case <-time.After(testTimeout):
		t.Fatal("consumer never subscribed to message_posted")
	}
}

// publishRef publishes messageID's message_posted ref under ctx, as comms does
// after the commit; a span on ctx reaches the consumer's callback.
func publishRef(t *testing.T, ctx context.Context, c *Consumer, tenant store.TenantID, messageID string) {
	t.Helper()
	ref := fabric.EventRef{Tenant: string(tenant), Kind: fabric.KindMessagePosted, RowID: messageID}
	subject, err := fabric.CommsSubject(ref.Tenant, ref.Kind)
	if err != nil {
		t.Fatalf("CommsSubject: %v", err)
	}
	if err := c.fab.Publish(ctx, subject, ref); err != nil {
		t.Fatalf("Publish %s: %v", messageID, err)
	}
}

// publishPosted publishes the ref of an already-committed message, with no span.
func publishPosted(t *testing.T, c *Consumer, messageID string) {
	t.Helper()
	publishRef(t, context.Background(), c, testTenant, messageID)
}

// postMessage commits m to the fake store, then publishes its ref: the comms
// post path.
func postMessage(t *testing.T, c *Consumer, reads *fakeReads, m store.Message) {
	t.Helper()
	reads.seedMessage(m)
	publishPosted(t, c, string(m.ID))
}
