package fabric

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/nats-io/nats.go"
)

// PublishBindingChange announces on core NATS that one session's binding
// changed, so every Server holding a cached entry for it invalidates.
//
// Core NATS, not JetStream, by design (§T4): "core NATS at-most-once suffices
// because a dropped invalidation degrades to a cache-miss re-read". A stream
// here would buy durability for a message whose whole content is "go ask
// Postgres" — a second store of state Postgres already owns — and would drag in
// the queue-group consumer semantics that are exactly wrong for this plane (see
// RoutingFabric).
//
// tenant is the subject's tenant token, and b.Tenant must equal it. The
// redundancy is deliberate: the read side subscribes tenant-wildcard and its
// callback never sees the subject, so the payload's tenant is the receiver's
// only scope — checking them equal here is what stops a caller from publishing
// tenant-a's change on tenant-b's subject, which would tell a receiver to
// invalidate the wrong tenant's cache entry.
//
// nc.Publish is fire-and-forget, so a nil return means "handed to the NATS
// client", not "every Server got it" — nothing on this plane can promise the
// latter. The flush against the caller's ctx is what makes the error real: it
// surfaces a connection that cannot take the write, which is the failure a
// caller can act on. The same reasoning as SendCommand's.
func (f *Fabric) PublishBindingChange(ctx context.Context, tenant string, b BindingChange) error {
	if err := f.checkOpen(); err != nil {
		return err
	}
	if err := b.valid(); err != nil {
		return err
	}
	subject, err := RoutingBindingSubject(tenant)
	if err != nil {
		return err
	}
	if b.Tenant != tenant {
		return fmt.Errorf("fabric: binding change for tenant %q was published under tenant %q", b.Tenant, tenant)
	}
	data, err := b.encode()
	if err != nil {
		return err
	}
	if err := f.nc.Publish(subject, data); err != nil {
		return fmt.Errorf("fabric: publishing binding change %s/%s to %q: %w", b.Tenant, b.SessionID, subject, err)
	}
	if err := f.flush(ctx); err != nil {
		return fmt.Errorf("fabric: flushing binding change %s/%s to %q: %w", b.Tenant, b.SessionID, subject, err)
	}
	return nil
}

// SubscribeBindingChanges drives fn for every binding change on every tenant
// until the returned Unsubscribe is called, ctx is done, or the Fabric is
// closed — whichever comes first.
//
// NO QUEUE GROUP, and that is the contract, not an omission: each Server keeps
// its own binding cache, so each must see every invalidation. A queue group
// would deliver each change to exactly one Server and leave the others serving
// a stale binding with no error, no gap and no missing ack to reveal it.
// RunnerFabric.Events does join a queue group (RunnerEventsQueue) for the
// opposite reason: a Runner event is work, and work must be done once.
//
// The subscribe is tenant-wildcard (RoutingBindingWildcardSubject) because a
// Server's cache spans every tenant it has resolved a session for, so one
// subscription is what the cache needs and tenant creation stays a Postgres
// insert.
//
// A malformed payload is logged and DROPPED — this plane has no ack to withhold,
// no Nak to send and no DLQ to park on, so surviving is the only option that
// keeps invalidation flowing for every other session. A panicking fn is
// recovered for the same reason: fn runs on the NATS dispatcher goroutine, so
// letting it panic would take the process down.
func (f *Fabric) SubscribeBindingChanges(ctx context.Context, fn func(BindingChange)) (Unsubscribe, error) {
	if err := f.checkOpen(); err != nil {
		return nil, err
	}
	if fn == nil {
		return nil, errors.New("fabric: SubscribeBindingChanges requires a callback")
	}
	subject := RoutingBindingWildcardSubject()

	// nc.Subscribe, NOT nc.QueueSubscribe. See the seam's contract: a queue
	// group here silently breaks cache coherence.
	sub, err := f.nc.Subscribe(subject, func(msg *nats.Msg) {
		f.handleBindingChange(ctx, msg, fn)
	})
	if err != nil {
		return nil, fmt.Errorf("fabric: subscribing %q: %w", subject, err)
	}
	// Flush so this returns only once the server has registered the interest.
	// Load-bearing on core NATS in a way it is not on JetStream: the server
	// DROPS a message with no matching interest, so a caller that subscribed
	// and then triggered a binding change would race its own first
	// invalidation and never learn it was lost. Same argument as Events'.
	if err := f.flush(ctx); err != nil {
		if derr := sub.Unsubscribe(); derr != nil {
			f.log.WarnContext(ctx, "fabric: unsubscribing after a failed flush", "subject", subject, "error", derr)
		}
		return nil, fmt.Errorf("fabric: establishing the binding-change subscription on %q: %w", subject, err)
	}

	// One teardown path, reached from the caller's Unsubscribe, from ctx being
	// done, or from the fabric closing, and run at most once — so the watchdog
	// goroutine always exits and the subscription is never torn down twice.
	//
	// f.teardown is load-bearing, not a duplicate of ctx.Done(): a Close with
	// an uncancelled ctx (a Server whose root context outlives the fabric —
	// the ordinary shutdown shape) would otherwise leak this goroutine.
	//
	// Drain rather than Unsubscribe, mirroring the runner-events pump: it lets
	// NATS deliver what it has already accepted for this subject before the
	// interest goes away, so an invalidation this process already had in hand
	// still reaches the cache. An already-closed connection is the expected
	// shutdown outcome, not a failure — Close closes f.teardown before
	// nc.Drain(), and the connection-level drain reclaims every subscription
	// itself, so a later sub.Drain() reporting ErrConnectionClosed describes a
	// subscription that WAS drained.
	var once sync.Once
	done := make(chan struct{})
	stop := func() {
		once.Do(func() {
			if err := sub.Drain(); err != nil && !errors.Is(err, nats.ErrConnectionClosed) {
				f.log.WarnContext(ctx, "fabric: draining the binding-change subscription failed",
					"subject", subject, "error", err)
			}
			close(done)
		})
	}
	go func() {
		select {
		case <-ctx.Done():
			stop()
		case <-f.teardown:
			stop()
		case <-done:
		}
	}()
	return stop, nil
}

// handleBindingChange runs one delivery: decode, cross-check the payload's
// tenant against the subject it arrived on, then invoke fn under a panic guard.
// Split out of SubscribeBindingChanges so the drop decision is readable on its
// own — and so it is visibly the WHOLE of it: there is no ack, no
// retry-or-park branch and no DLQ republish after this point, because core NATS
// offers nowhere to put a message it could not process.
func (f *Fabric) handleBindingChange(ctx context.Context, msg *nats.Msg, fn func(BindingChange)) {
	b, err := decodeBindingChange(msg.Data)
	if err != nil {
		// Best-effort plane: one malformed publish must not stop invalidation
		// for every other session. Surfaced, never silent — the log line is the
		// only trace this message will ever leave.
		f.log.ErrorContext(ctx, "fabric: dropping an undecodable binding change",
			"subject", msg.Subject, "error", err)
		return
	}
	// The payload's tenant must match the subject's, exactly as handleEvent
	// checks a comms ref. The read side subscribes tenant-wildcard and fn never
	// sees the subject, so b.Tenant is the receiver's ONLY scope — and until
	// OQ-3 lands per-tenant NATS authorization, any client that can reach the
	// client port can publish any subject. Without this, a publish of
	// {tenant: victim} on the attacker's own subject would hand a subscriber a
	// victim-scoped change, and an "unbound" op is a stale NEGATIVE the
	// receiver drops outright rather than a re-read that would self-correct.
	//
	// Dropped, not parked: this plane has no dead-letter subject, and a
	// mismatch is as unprocessable as an undecodable payload.
	want, subjErr := RoutingBindingSubject(b.Tenant)
	if subjErr != nil {
		f.log.ErrorContext(ctx, "fabric: dropping a binding change whose tenant is not a valid subject token",
			"subject", msg.Subject, "error", subjErr)
		return
	}
	if msg.Subject != want {
		f.log.ErrorContext(ctx, "fabric: dropping a binding change delivered on a foreign tenant's subject",
			"subject", msg.Subject, "claimed_tenant", b.Tenant, "claimed_subject", want, "session_id", b.SessionID)
		return
	}
	if err := invokeBindingChange(fn, b); err != nil {
		// A failed callback is a DROP here, not a redelivery: the cache entry
		// stays stale until the next change or a cache miss re-reads Postgres,
		// which is the arbiter. Logged, because a subscriber panicking is a bug
		// even though this plane tolerates it.
		f.log.ErrorContext(ctx, "fabric: a binding-change subscriber failed; the invalidation is dropped",
			"subject", msg.Subject, "session_id", b.SessionID, "error", err)
	}
}

// invokeBindingChange calls fn, converting a panic into an error. fn is consumer
// code running on the fabric's dispatcher goroutine, so letting it panic would
// take the process down over one bad cache entry.
func invokeBindingChange(fn func(BindingChange), b BindingChange) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("fabric: binding-change subscriber panicked handling %s/%s: %v", b.Tenant, b.SessionID, r)
		}
	}()
	fn(b)
	return nil
}
