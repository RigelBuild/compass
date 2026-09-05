package fabric

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

// Publish sends ref to subject on JetStream, returning only once the server has
// acked it into the stream — so a Publish that returns nil means the event is
// stored, and one that returns an error is genuinely unpublished and the caller
// can leave its cursor unadvanced.
//
// WithMsgID(ref.msgID()) gives publish-side dedup: two Servers publishing the
// same logical change, or a retry of a publish whose ack was lost, collapse to
// one stored message inside the stream's duplicate window.
func (f *Fabric) Publish(ctx context.Context, subject string, ref EventRef) error {
	if err := f.checkOpen(); err != nil {
		return err
	}
	if err := ref.valid(); err != nil {
		return err
	}
	// The subject must be exactly the one the ref itself names. A head-only
	// check would let a caller publish tenant-a's ref on tenant-b's subject:
	// the subscriber that claims it is scoped to tenant-b but the ref tells it
	// to re-read a tenant-a row, which is the cross-tenant read the EventRef
	// invariant (eventref.go) exists to prevent. Deriving the wanted subject
	// from the ref also subsumes the whole-subject grammar check.
	want, err := CommsSubject(ref.Tenant, ref.Kind)
	if err != nil {
		return err
	}
	if subject != want {
		return fmt.Errorf("fabric: event ref %s/%s is for subject %q but was published on %q", ref.Tenant, ref.Kind, want, subject)
	}
	if _, err := f.ensureStream(ctx); err != nil {
		return err
	}
	data, err := ref.encode()
	if err != nil {
		return err
	}
	if _, err := f.js.Publish(ctx, subject, data, jetstream.WithMsgID(ref.msgID())); err != nil {
		return fmt.Errorf("fabric: publishing %s/%s to %q: %w", ref.Kind, ref.RowID, subject, err)
	}
	return nil
}

// Subscribe drives fn for every event on subject until the returned Unsubscribe
// is called, ctx is done, or the Fabric is closed — whichever comes first. Every
// one of those three paths DRAINS the consumer, so an event this process had
// already claimed is processed and acked rather than discarded.
//
// The consumer is a DURABLE pull consumer named from the subject, so every
// Server instance on that subject shares one consumer: each event is claimed by
// exactly one instance (§Q3's queue-group semantics), and a restart resumes from
// the consumer's position instead of replaying or skipping. Because that
// consumer is shared, every instance subscribing to a subject must run the same
// fabric Config — see Config.MaxDeliver.
//
// Acking is explicit and follows fn: fn returning normally acks, and fn
// panicking is recovered and treated as a failure (a panic in one subscriber
// must not take down the process — and must not silently ack an unprocessed
// event either). A failure Naks for immediate redelivery until NumDelivered
// reaches MaxDeliver — total ATTEMPTS, not retries — at which point the message
// is parked on DLQSubject and Term'd. An undecodable payload is parked
// immediately: redelivering it can never succeed.
func (f *Fabric) Subscribe(ctx context.Context, subject string, fn func(EventRef)) (Unsubscribe, error) {
	if err := f.checkOpen(); err != nil {
		return nil, err
	}
	if fn == nil {
		return nil, fmt.Errorf("fabric: Subscribe(%q) requires a callback", subject)
	}
	if err := validCommsSubject(subject); err != nil {
		return nil, err
	}
	return f.subscribeSubject(ctx, subject, fn)
}

// SubscribeKind drives fn for every event of one kind ACROSS EVERY TENANT,
// until the returned Unsubscribe is called, ctx is done, or the Fabric is
// closed. It is the delivery plane's cross-tenant fan-in path: the delivery
// consumer is a per-Server singleton serving all tenants, while each event is
// published on a concrete compass.<tenant>.comms.<kind>, so the one consumer
// subscribes on the tenant-wildcard subject CommsWildcardSubject builds.
//
// Identical in every other respect to Subscribe — one DURABLE queue-group
// consumer (durableName hashes the wildcard subject to its own name, distinct
// from any concrete-tenant consumer, so each matching event is claimed by
// exactly one Server instance), the same explicit ack / Nak-to-MaxDeliver /
// park-on-DLQSubject semantics, and the same drain on all three teardown
// paths. Wildcard and concrete consumers are independent durables; see
// SUBJECTS.md's "Its own durable consumer" property when migrating callers.
//
// The wildcard is on the TENANT token only: kind is concrete and validated, so
// a SubscribeKind(KindMessagePosted) receives message_posted for every tenant
// and nothing else. Subscribe keeps its strict concrete-subject grammar — a
// wildcard subject cannot be reached through it.
func (f *Fabric) SubscribeKind(ctx context.Context, kind EventKind, fn func(EventRef)) (Unsubscribe, error) {
	if err := f.checkOpen(); err != nil {
		return nil, err
	}
	if fn == nil {
		return nil, fmt.Errorf("fabric: SubscribeKind(%q) requires a callback", kind)
	}
	subject, err := CommsWildcardSubject(kind)
	if err != nil {
		return nil, err
	}
	return f.subscribeSubject(ctx, subject, fn)
}

// subscribeSubject is the shared body of Subscribe and SubscribeKind: it
// registers the durable consumer on an ALREADY-VALIDATED subject and wires its
// teardown. Split out so each public entry point owns its own subject
// validation — Subscribe's strict concrete-only grammar, SubscribeKind's
// tenant-wildcard builder — and neither can reach the other's.
//
// It performs no validation of its own: subject must come from
// validCommsSubject or CommsWildcardSubject.
func (f *Fabric) subscribeSubject(ctx context.Context, subject string, fn func(EventRef)) (Unsubscribe, error) {
	stream, err := f.ensureStream(ctx)
	if err != nil {
		return nil, err
	}
	// The consumer is shared and durable, so consumerConfig's values come from
	// a Config every instance on this subject must agree on (see
	// Config.MaxDeliver).
	cons, err := stream.CreateOrUpdateConsumer(ctx, f.cfg.consumerConfig(subject))
	if err != nil {
		return nil, fmt.Errorf("fabric: creating consumer for %q on %s: %w", subject, f.cfg.streamName(), err)
	}

	cc, err := cons.Consume(func(msg jetstream.Msg) {
		f.handleEvent(ctx, msg, fn)
	}, jetstream.ConsumeErrHandler(func(_ jetstream.ConsumeContext, err error) {
		// Transient pull errors are the library's to retry; surfacing them is
		// the only thing this side can do, and swallowing them would hide a
		// consumer wedged for good.
		f.log.WarnContext(ctx, "fabric: consume error", "subject", subject, "error", err)
	}))
	if err != nil {
		return nil, fmt.Errorf("fabric: consuming %q: %w", subject, err)
	}

	// One teardown path, reached from the caller's Unsubscribe, from ctx being
	// done, or from the fabric closing, and run at most once — so the watchdog
	// goroutine always exits and the consumer is never torn down twice.
	//
	// Drain, not Stop: Stop DISCARDS whatever the pull consumer has already
	// buffered (up to its prefetch), and on a durable shared consumer those
	// messages are claimed-not-acked, so they only come back to anyone after
	// AckWait — a silent multi-second stall for events this process had already
	// accepted. Drain runs them through fn and acks them first. That is the
	// right choice for all three teardown paths: the durability contract says a
	// claimed event is not silently dropped, and a cancelled context on
	// shutdown does not change that.
	var once sync.Once
	done := make(chan struct{})
	stop := func() {
		once.Do(func() {
			cc.Drain()
			close(done)
		})
	}
	go func() {
		select {
		case <-ctx.Done():
			stop()
		case <-f.teardown:
			// Close alone must tear this down: nats.go does not close a
			// ConsumeContext's buffer when the connection closes, so without
			// this case a Close with an uncancelled ctx leaks this goroutine
			// and the consumer with it.
			stop()
		case <-done:
		}
	}()
	return stop, nil
}

// handleEvent runs one delivery: decode, invoke fn under a panic guard, then ack
// or park. Split out of Subscribe so the ack/park decision is readable on its
// own.
func (f *Fabric) handleEvent(ctx context.Context, msg jetstream.Msg, fn func(EventRef)) {
	ref, decodeErr := decodeEventRef(msg.Data())
	if decodeErr != nil {
		// Unparseable: no number of redeliveries changes the bytes.
		f.park(ctx, msg, decodeErr)
		return
	}
	if err := invoke(fn, ref); err != nil {
		f.retryOrPark(ctx, msg, err)
		return
	}
	if err := msg.Ack(); err != nil {
		// The event WAS processed; a lost ack costs a redelivery, which the
		// subscriber's Postgres re-read makes idempotent. Log, never park.
		f.log.WarnContext(ctx, "fabric: acking delivered event failed; it will be redelivered",
			"subject", msg.Subject(), "kind", string(ref.Kind), "row_id", ref.RowID, "error", err)
	}
}

// invoke calls fn, converting a panic into an error. A subscriber callback is
// consumer code running on the fabric's goroutine: letting it panic would take
// the process down, and recovering without failing the message would ack an
// event nobody processed.
func invoke(fn func(EventRef), ref EventRef) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("fabric: subscriber panicked handling %s/%s: %v", ref.Kind, ref.RowID, r)
		}
	}()
	fn(ref)
	return nil
}

// retryOrPark Naks a failed delivery for another attempt, or parks it once the
// attempt budget is spent. Reading NumDelivered from the message metadata (not a
// local counter) is what makes the budget hold across Server instances and
// restarts — the count is the server's.
func (f *Fabric) retryOrPark(ctx context.Context, msg jetstream.Msg, cause error) {
	md, err := msg.Metadata()
	if err != nil {
		// No metadata means no attempt count, so the budget cannot be enforced;
		// park rather than risk redelivering a poison message forever.
		f.park(ctx, msg, fmt.Errorf("%w (and its metadata was unreadable: %w)", cause, err))
		return
	}
	if md.NumDelivered >= f.cfg.deliveryBudget() {
		f.park(ctx, msg, fmt.Errorf("%w (after %d delivery attempts)", cause, md.NumDelivered))
		return
	}
	f.log.WarnContext(ctx, "fabric: event handling failed; redelivering",
		"subject", msg.Subject(), "attempt", md.NumDelivered, "max_deliver", f.cfg.maxDeliver(), "error", cause)
	if err := msg.Nak(); err != nil {
		// AckWait still expires and redelivers, so this is a latency cost, not
		// a lost event.
		f.log.WarnContext(ctx, "fabric: nak failed; redelivery waits for ack_wait",
			"subject", msg.Subject(), "error", err)
	}
}

// park implements the dead-letter pattern JetStream has no native support for:
// republish the raw payload to DLQSubject, then Term the message so the server
// stops redelivering it.
//
// The payload goes out verbatim over CORE NATS, with the original subject and
// the reason in headers. Core rather than JetStream because the DLQ is a
// diagnostic tap, not a recovery path — recovery is always the Postgres row —
// and because a DLQ publish that itself needed a stream would need its own DLQ.
//
// Term is issued even if the DLQ publish fails: leaving a poison message
// redelivering forever is the worse failure, and the reason is logged either
// way.
//
// The reason on the wire is sanitized and bounded (see sanitizeReason); the
// full cause goes to the log, which has no wire limit.
func (f *Fabric) park(ctx context.Context, msg jetstream.Msg, cause error) {
	f.log.ErrorContext(ctx, "fabric: parking event on the dlq",
		"subject", msg.Subject(), "dlq_subject", DLQSubject, "error", cause)

	dlq := nats.NewMsg(DLQSubject)
	dlq.Data = msg.Data()
	dlq.Header.Set(dlqHeaderSubject, msg.Subject())
	reason := sanitizeReason(cause.Error())
	dlq.Header.Set(dlqHeaderReason, reason)
	if err := f.nc.PublishMsg(dlq); err != nil {
		f.log.ErrorContext(ctx, "fabric: publishing to the dlq failed; terminating the message anyway",
			"subject", msg.Subject(), "error", err)
	}
	if err := msg.TermWithReason(reason); err != nil {
		f.log.ErrorContext(ctx, "fabric: terminating a parked message failed; it may redeliver until max_deliver",
			"subject", msg.Subject(), "error", err)
	}
}

// DLQ message headers, so a consumer of DLQSubject knows what the payload was
// and why it parked without parsing a log line.
const (
	dlqHeaderSubject = "Compass-Original-Subject"
	dlqHeaderReason  = "Compass-Park-Reason"
)

// maxParkReason bounds the reason written to the DLQ header and the +TERM ack
// body. Both are wire-protocol fields under the server's max-payload ceiling,
// and jetstream's TermWithReason applies no sanitization of its own, so an
// unbounded or newline-bearing reason could make the park itself fail — the
// worst possible place to fail, since the alternative is a poison message
// redelivering forever.
const maxParkReason = 256

// sanitizeReason strips CR/LF, which would corrupt the +TERM ack line and the
// header, and bounds the length. A subscriber panic embeds an arbitrary consumer
// value in the cause, so neither property can be assumed. The full, untruncated
// cause still reaches the ErrorContext log.
//
// Truncation is on a byte boundary and can split a trailing rune; this is a
// diagnostic string read by an operator, not something that is parsed, so a
// mangled last character is an acceptable price for a hard byte bound.
func sanitizeReason(s string) string {
	s = strings.NewReplacer("\r", " ", "\n", " ").Replace(s)
	if len(s) > maxParkReason {
		s = s[:maxParkReason]
	}
	return s
}
