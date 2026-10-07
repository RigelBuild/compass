package fabric

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	otelx "github.com/RigelBuild/compass/go/internal/otel"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"go.opentelemetry.io/otel/trace"
)

// traceparentHeader is the W3C key, lowercase because nats-server 2.11/2.12
// lowercase it in place and NATS headers are case-sensitive.
const traceparentHeader = "traceparent"

// reapProbeBudget caps the consumer-existence check a pull error triggers; a
// probe that times out is retried on the next missed heartbeat.
const reapProbeBudget = 2 * time.Second

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
	// The subject must be exactly the one the ref itself names: a head-only check
	// would let a caller publish tenant-a's ref on tenant-b's subject, the
	// cross-tenant read the EventRef invariant prevents. Deriving the wanted subject
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
	msg := &nats.Msg{Subject: subject, Data: data, Header: nats.Header{}}
	if tp := otelx.Traceparent(ctx); tp != "" {
		msg.Header.Set(traceparentHeader, tp)
	}
	if _, err := f.js.PublishMsg(ctx, msg, jetstream.WithMsgID(ref.msgID())); err != nil {
		return fmt.Errorf("fabric: publishing %s/%s to %q: %w", ref.Kind, ref.RowID, subject, err)
	}
	return nil
}

// Subscribe drives fn for every event on subject until the returned Unsubscribe
// is called, ctx is done, or the Fabric is closed — whichever comes first. Every
// one of those three paths DRAINS the consumer, so an event this process had
// already claimed is processed and acked rather than discarded. Callbacks run
// serially per subscription.
//
// The consumer is a DURABLE pull consumer named from the subject, so every
// Server instance on that subject shares one consumer: each event is claimed by
// exactly one instance (§Q3's queue-group semantics), and a restart resumes from
// the consumer's position instead of replaying or skipping. Because that
// consumer is shared, every instance subscribing to a subject must run the same
// fabric Config — see Config.MaxDeliver.
//
// Acking is explicit and follows fn: fn returning nil acks, and fn returning an
// error or panicking is a failure. A panic is recovered, so one subscriber can
// neither take down the process nor silently ack an unprocessed event. A
// failure Naks for immediate redelivery until NumDelivered
// reaches MaxDeliver — total ATTEMPTS, not retries — at which point the message
// is parked on DLQSubject and Term'd. An undecodable payload is parked
// immediately: redelivering it can never succeed. A callback that outlives
// AckWait on every attempt is dropped by the server at MaxDeliver; the fabric
// parks it from the consumer's MAX_DELIVERIES advisory instead (no Term).
func (f *Fabric) Subscribe(ctx context.Context, subject string, fn func(context.Context, EventRef) error) (Unsubscribe, error) {
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
// closed. Callbacks run serially per subscription. It is the delivery plane's
// cross-tenant fan-in path: the delivery consumer is a per-Server singleton,
// while each event is published on a concrete compass.<tenant>.comms.<kind>.
//
// Identical in every other respect to Subscribe — one DURABLE queue-group
// consumer (durableName hashes the wildcard subject to its own name, distinct
// from any concrete-tenant consumer, so each matching event is claimed by
// exactly one Server instance), the same explicit ack / Nak-to-MaxDeliver /
// park-on-DLQSubject semantics (including the advisory park of a message the
// server dropped at MaxDeliver), and the same drain on all three teardown
// paths. Wildcard and concrete consumers are independent durables; see
// SUBJECTS.md's "Its own durable consumer" property when migrating callers.
//
// The wildcard is on the TENANT token only: kind is concrete and validated, so
// a SubscribeKind(KindMessagePosted) receives message_posted for every tenant
// and nothing else. Subscribe keeps its strict concrete-subject grammar — a
// wildcard subject cannot be reached through it.
func (f *Fabric) SubscribeKind(ctx context.Context, kind EventKind, fn func(context.Context, EventRef) error) (Unsubscribe, error) {
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
func (f *Fabric) subscribeSubject(ctx context.Context, subject string, fn func(context.Context, EventRef) error) (Unsubscribe, error) {
	stream, err := f.ensureStream(ctx)
	if err != nil {
		return nil, err
	}
	// The consumer is shared and durable, so consumerConfig's values come from
	// a Config every instance on this subject must agree on (see
	// Config.MaxDeliver).
	cfg := f.cfg.consumerConfig(subject)
	cons, err := stream.CreateOrUpdateConsumer(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("fabric: creating consumer for %q on %s: %w", subject, f.cfg.streamName(), err)
	}

	// Consume dispatches serially today; the lock makes the promised serial
	// callback a fabric guarantee rather than a nats.go internal.
	var callbackMu sync.Mutex
	// The server reaps the durable (InactiveThreshold) when this client could
	// not pull, e.g. a long partition. reapDetector reports it and the
	// supervisor below recreates the durable rather than go silent.
	reaped := make(chan struct{}, 1)
	consume := func(cons jetstream.Consumer) (jetstream.ConsumeContext, error) {
		detect := reapDetector(ctx, cons, reaped)
		return cons.Consume(func(msg jetstream.Msg) {
			callbackMu.Lock()
			defer callbackMu.Unlock()
			f.handleEvent(ctx, msg, fn)
		}, jetstream.ConsumeErrHandler(func(_ jetstream.ConsumeContext, err error) {
			detect(err)
			// Transient pull errors are the library's to retry; surfacing them is
			// the only thing this side can do, and swallowing them would hide a
			// consumer wedged for good.
			f.log.WarnContext(ctx, "fabric: consume error", "subject", subject, "error", err)
		}))
	}
	cc, err := consume(cons)
	if err != nil {
		return nil, fmt.Errorf("fabric: consuming %q: %w", subject, err)
	}
	durable := durableName(subject)
	f.trackConsumer(durable, cons)
	advisory, err := f.parkOnMaxDeliveries(ctx, subject)
	if err != nil {
		cc.Stop()
		f.untrackConsumer(durable)
		return nil, err
	}

	// One teardown path (Unsubscribe, ctx done, or fabric closing), run once. Drain,
	// not Stop: Stop DISCARDS the buffer, and on a durable shared consumer those
	// claimed-not-acked events only return after AckWait (a silent stall). Drain runs
	// them through fn and acks first, as the durability contract requires.
	var ccMu sync.Mutex
	var stopped bool // under ccMu; a recreate racing stop must not install a new cc
	var once sync.Once
	done := make(chan struct{})
	stop := func() {
		once.Do(func() {
			ccMu.Lock()
			stopped = true
			cur := cc
			ccMu.Unlock()
			cur.Drain()
			if hook := f.consumerClosed; hook != nil {
				go func() {
					<-cur.Closed()
					hook()
				}()
			}
			f.untrackConsumer(durable)
			if err := advisory.Unsubscribe(); err != nil && !errors.Is(err, nats.ErrConnectionClosed) && !errors.Is(err, nats.ErrConnectionDraining) && !errors.Is(err, nats.ErrBadSubscription) {
				f.log.WarnContext(ctx, "fabric: unsubscribing the max-deliveries advisory failed", "subject", subject, "error", err)
			}
			close(done)
		})
	}
	// tryRecreate makes one attempt to replace a reaped consumer. ok=false with
	// a nil err means stop won the race and the subscription is over.
	tryRecreate := func() (ok bool, err error) {
		next, err := stream.CreateOrUpdateConsumer(ctx, cfg)
		if err != nil {
			return false, err
		}
		nextCC, err := consume(next)
		if err != nil {
			return false, err
		}
		ccMu.Lock()
		defer ccMu.Unlock()
		if stopped {
			nextCC.Stop()
			return false, nil
		}
		// A probe-detected reap leaves the old Consume running against the
		// recreated durable; drain it so only cc pulls and stop() reaches all.
		cc.Drain()
		cc = nextCC
		f.untrackConsumer(durable)
		f.trackConsumer(durable, next)
		return true, nil
	}
	recreate := func() bool {
		return f.retryUntil(ctx, done, subject, tryRecreate)
	}
	go func() {
		for {
			select {
			case <-reaped:
				if recreate() {
					continue
				}
				stop()
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
			return
		}
	}()
	return stop, nil
}

// retryUntil runs attempt every AckWait until it succeeds or the subscription
// ends; it reports whether the reaped consumer was replaced.
func (f *Fabric) retryUntil(ctx context.Context, done <-chan struct{}, subject string, attempt func() (bool, error)) bool {
	retry := time.NewTicker(f.cfg.ackWait())
	defer retry.Stop()
	for {
		ok, err := attempt()
		if ok {
			f.log.WarnContext(ctx, "fabric: recreated a consumer the server reaped", "subject", subject)
			return true
		}
		if err == nil {
			return false
		}
		f.log.ErrorContext(ctx, "fabric: recreating a reaped consumer failed; retrying", "subject", subject, "error", err)
		select {
		case <-retry.C:
		case <-ctx.Done():
			return false
		case <-f.teardown:
			return false
		case <-done:
			return false
		}
	}
}

// reapDetector returns a consume-error hook that signals reaped once cons is
// gone. The server's 409 reaches only a pull waiting at the delete; a pull to
// an already-deleted durable sees no-responders or a missed heartbeat instead,
// so those trigger one bounded Info probe off the handler goroutine.
func reapDetector(ctx context.Context, cons jetstream.Consumer, reaped chan<- struct{}) func(error) {
	signal := func() {
		select {
		case reaped <- struct{}{}:
		default:
		}
	}
	var probing atomic.Bool
	return func(err error) {
		switch {
		case errors.Is(err, jetstream.ErrConsumerDeleted):
			signal()
		case errors.Is(err, nats.ErrNoResponders), errors.Is(err, jetstream.ErrNoHeartbeat):
			if !probing.CompareAndSwap(false, true) {
				return
			}
			go func() {
				defer probing.Store(false)
				probeCtx, cancel := context.WithTimeout(ctx, reapProbeBudget)
				defer cancel()
				if _, err := cons.Info(probeCtx); errors.Is(err, jetstream.ErrConsumerNotFound) {
					signal()
				}
			}()
		}
	}
}

// handleEvent runs one delivery: decode, invoke fn under a panic guard, then ack
// on nil or retry/park on an error. Split out of Subscribe so the ack/park
// decision is readable on its own.
func (f *Fabric) handleEvent(ctx context.Context, msg jetstream.Msg, fn func(context.Context, EventRef) error) {
	ref, decodeErr := decodeEventRef(msg.Data())
	if decodeErr != nil {
		// Unparseable: no number of redeliveries changes the bytes.
		f.park(ctx, msg, decodeErr)
		return
	}
	// The ref's tenant must match the subject it arrived on. COMPASS_COMMS is shared
	// with no per-tenant authorization yet (OQ-3), so a client can put arbitrary
	// bytes on any subject; a cross-tenant ref would hand a subscriber a foreign row
	// (EventRef re-reads under ref.Tenant, not the subject). A mismatch parks.
	want, subjErr := CommsSubject(ref.Tenant, ref.Kind)
	if subjErr != nil {
		f.park(ctx, msg, subjErr)
		return
	}
	if got := msg.Subject(); got != want {
		f.park(ctx, msg, fmt.Errorf("fabric: event ref %s/%s names subject %q but was delivered on %q", ref.Tenant, ref.Kind, want, got))
		return
	}
	// A message buffered behind a slow callback has already spent part of its
	// AckWait; resetting it here lines the server's timer up with the deadline
	// below, so a ctx-respecting final attempt parks before the server drops it.
	if err := msg.InProgress(); err != nil {
		f.log.WarnContext(ctx, "fabric: resetting ack_wait before the callback failed", "subject", msg.Subject(), "error", err)
	}
	if md, err := msg.Metadata(); err == nil && md != nil {
		key := parkKey{durable: md.Consumer, seq: md.Sequence.Stream}
		f.activeParkAttempt(key, 1)
		defer f.activeParkAttempt(key, -1)
	}
	// Detached from ctx so an event drained after Subscribe's ctx ends still runs
	// live; the 0.9 margin lets the final attempt's Term reach the server first.
	// The subscriber's span is stripped so only the publisher's trace carries.
	base := trace.ContextWithSpanContext(context.WithoutCancel(ctx), trace.SpanContext{})
	deliveryCtx, cancel := context.WithTimeout(otelx.ContextWithTraceparent(base, traceparent(msg.Headers())), f.cfg.ackWait()*9/10)
	err := invoke(deliveryCtx, fn, ref)
	cancel()
	if err != nil {
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

// invoke calls fn and returns its error, converting a panic into an error. A
// subscriber callback is consumer code running on the fabric's goroutine:
// letting it panic would take the process down, and recovering without failing
// the message would ack an event nobody processed.
func invoke(ctx context.Context, fn func(context.Context, EventRef) error, ref EventRef) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("fabric: subscriber panicked handling %s/%s: %v", ref.Kind, ref.RowID, r)
		}
	}()
	return fn(ctx, ref)
}

// traceparent reads the trace header in any case: a server may have rewritten
// the canonical key a publisher sent.
func traceparent(h nats.Header) string {
	for k, v := range h {
		if strings.EqualFold(k, traceparentHeader) && len(v) > 0 {
			return v[0]
		}
	}
	return ""
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
// A late final-attempt Term also draws the max-deliveries advisory; both paths claim
// (durable, stream seq) and only one publishes (lifetime in SUBJECTS.md).
//
// The reason on the wire is sanitized and bounded (see sanitizeReason); the
// full cause goes to the log, which has no wire limit.
func (f *Fabric) park(ctx context.Context, msg jetstream.Msg, cause error) {
	md, err := msg.Metadata()
	reason := sanitizeReason(cause.Error())
	key := parkKey{}
	hasKey := err == nil && md != nil
	if hasKey {
		key = parkKey{durable: md.Consumer, seq: md.Sequence.Stream}
	}
	var claim *parkClaim
	published := false
	shouldPublish := true
	if hasKey {
		claim, shouldPublish, err = f.claimPark(ctx, key, "callback:"+key.durable)
		if err != nil {
			shouldPublish = false
			f.log.WarnContext(ctx, "fabric: waiting for park claim failed", "subject", msg.Subject(), "error", err)
		}
	}
	if shouldPublish {
		var publishErr error
		reason, publishErr = f.publishDLQ(ctx, msg.Subject(), msg.Data(), cause)
		published = publishErr == nil
		if hasKey {
			if !f.finishParkClaim(key, claim, published) && !published {
				f.log.WarnContext(ctx, "fabric: park claim changed before publish failure was recorded", "subject", msg.Subject(), "stream_seq", key.seq)
			}
		}
		if hook := f.beforeTerm; hook != nil {
			hook()
		}
	} else {
		f.log.DebugContext(ctx, "fabric: event already claimed for dead-letter parking",
			"subject", msg.Subject(), "stream_seq", key.seq)
	}
	if err := msg.TermWithReason(reason); err != nil {
		f.log.ErrorContext(ctx, "fabric: terminating a parked message failed; it may redeliver until max_deliver",
			"subject", msg.Subject(), "error", err)
	}
	if hasKey {
		f.notifyParkDecided("callback:"+key.durable, published)
	}
}

type parkKey struct {
	durable string
	seq     uint64
}

// parkClaim is one DLQ park attempt; done closes when its owner publishes or gives up.
type parkClaim struct {
	done      chan struct{}
	inFlight  bool
	published bool
	expires   time.Time
}

// activeParkAttempt prevents pruning a claim while its callback is still running.
func (f *Fabric) activeParkAttempt(key parkKey, delta int) {
	f.parkedMu.Lock()
	defer f.parkedMu.Unlock()
	if f.activePark == nil {
		f.activePark = make(map[parkKey]int)
	}
	f.activePark[key] += delta
	if delta < 0 && f.activePark[key] == 0 {
		delete(f.activePark, key)
		if claim := f.parkedSequences[key]; claim != nil && !claim.inFlight {
			claim.expires = time.Now().Add(f.cfg.maxAge() + 2*f.cfg.ackWait())
		}
	}
}

// claimPark reserves a key or waits for its owner's publish result.
func (f *Fabric) claimPark(ctx context.Context, key parkKey, path string) (*parkClaim, bool, error) {
	for {
		now := time.Now()
		f.parkedMu.Lock()
		if f.parkedSequences == nil {
			f.parkedSequences = make(map[parkKey]*parkClaim)
		}
		for seen, old := range f.parkedSequences {
			if f.activePark[seen] == 0 && !old.inFlight && now.After(old.expires) {
				delete(f.parkedSequences, seen)
			}
		}
		claim := f.parkedSequences[key]
		if claim == nil {
			claim = &parkClaim{done: make(chan struct{}), inFlight: true}
			f.parkedSequences[key] = claim
			f.parkedMu.Unlock()
			return claim, true, nil
		}
		f.parkedMu.Unlock()
		if hook := f.parkClaimWaiting; hook != nil {
			hook(path)
		}
		// Prefer a settled claim over ctx: drained events run after Subscribe's ctx ends.
		select {
		case <-claim.done:
		case <-ctx.Done():
			select {
			case <-claim.done:
			default:
				return claim, false, ctx.Err()
			}
		}
		f.parkedMu.Lock()
		published := claim.published
		if published && f.parkedSequences[key] == claim {
			claim.expires = time.Now().Add(f.cfg.maxAge() + 2*f.cfg.ackWait())
		}
		f.parkedMu.Unlock()
		if published {
			return claim, false, nil
		}
	}
}

// finishParkClaim publishes the owner's result and wakes every contender.
func (f *Fabric) finishParkClaim(key parkKey, claim *parkClaim, published bool) bool {
	f.parkedMu.Lock()
	defer f.parkedMu.Unlock()
	if claim == nil || f.parkedSequences[key] != claim || !claim.inFlight {
		return false
	}
	claim.inFlight = false
	claim.published = published
	if published {
		claim.expires = time.Now().Add(f.cfg.maxAge() + 2*f.cfg.ackWait())
	} else {
		delete(f.parkedSequences, key)
	}
	close(claim.done)
	return true
}

func (f *Fabric) notifyParkDecided(path string, published bool) {
	if f.parkDecided != nil {
		f.parkDecided(path, published)
	}
}

// publishDLQ writes one DLQ record and returns its sanitized reason and publish error.
func (f *Fabric) publishDLQ(ctx context.Context, subject string, data []byte, cause error) (string, error) {
	f.log.ErrorContext(ctx, "fabric: parking event on the dlq",
		"subject", subject, "dlq_subject", DLQSubject, "error", cause)
	if hook := f.beforeDLQPublish; hook != nil {
		if err := hook(); err != nil {
			return sanitizeReason(cause.Error()), err
		}
	}
	dlq := nats.NewMsg(DLQSubject)
	dlq.Data = data
	dlq.Header.Set(dlqHeaderSubject, subject)
	reason := sanitizeReason(cause.Error())
	dlq.Header.Set(dlqHeaderReason, reason)
	if err := f.nc.PublishMsg(dlq); err != nil {
		f.log.ErrorContext(ctx, "fabric: publishing to the dlq failed",
			"subject", subject, "error", err)
		return reason, err
	}
	return reason, nil
}

// maxDeliveriesAdvisory is the server's notice that a consumer gave up on a
// message after MaxDeliver attempts. Only the fields the park needs.
type maxDeliveriesAdvisory struct {
	StreamSeq  uint64 `json:"stream_seq"`
	Deliveries uint64 `json:"deliveries"`
}

// parkOnMaxDeliveries parks what the server drops at MaxDeliver. A callback that
// returns nil but outlives AckWait every time is never Nak'd, so retryOrPark never
// sees it; only this advisory does. A Term'd park emits no such advisory.
func (f *Fabric) parkOnMaxDeliveries(ctx context.Context, subject string) (*nats.Subscription, error) {
	// A private handle: the shared one from ensureStream has its cached info
	// rewritten by Info(), which races this callback's GetMsg.
	stream, err := f.js.Stream(ctx, f.cfg.streamName())
	if err != nil {
		return nil, fmt.Errorf("fabric: opening stream for the max-deliveries advisory: %w", err)
	}
	advisorySubject := "$JS.EVENT.ADVISORY.CONSUMER.MAX_DELIVERIES." + f.cfg.streamName() + "." + durableName(subject)
	// Queue group: every instance on this consumer hears the advisory; one parks it.
	sub, err := f.nc.QueueSubscribe(advisorySubject, durableName(subject), func(m *nats.Msg) {
		var adv maxDeliveriesAdvisory
		if err := json.Unmarshal(m.Data, &adv); err != nil {
			f.log.WarnContext(ctx, "fabric: undecodable max-deliveries advisory", "subject", subject, "error", err)
			return
		}
		key := parkKey{durable: durableName(subject), seq: adv.StreamSeq}
		path := "advisory:" + durableName(subject)
		claim, owner, claimErr := f.claimPark(ctx, key, path)
		if claimErr != nil {
			f.notifyParkDecided(path, false)
			return
		}
		if !owner {
			f.notifyParkDecided(path, false)
			return
		}
		// Bounded so a stalled fetch cannot hold the claim in flight and park its waiters.
		getCtx, cancelGet := context.WithTimeout(context.WithoutCancel(ctx), f.cfg.ackWait())
		getParkedMsg := f.getParkedMsg
		if getParkedMsg == nil {
			getParkedMsg = func(ctx context.Context, seq uint64) (*jetstream.RawStreamMsg, error) {
				return stream.GetMsg(ctx, seq)
			}
		}
		raw, err := getParkedMsg(getCtx, adv.StreamSeq)
		cancelGet()
		if err != nil {
			f.finishParkClaim(key, claim, false)
			f.log.WarnContext(ctx, "fabric: event dropped at max_deliver is no longer in the stream; not parked",
				"subject", subject, "stream_seq", adv.StreamSeq, "error", err)
			f.notifyParkDecided(path, false)
			return
		}
		_, publishErr := f.publishDLQ(ctx, raw.Subject, raw.Data,
			fmt.Errorf("fabric: dropped by the server after %d delivery attempts (callback outlived ack_wait)", adv.Deliveries))
		published := publishErr == nil
		f.finishParkClaim(key, claim, published)
		f.notifyParkDecided(path, published)
	})
	if err != nil {
		return nil, fmt.Errorf("fabric: subscribing to max-deliveries advisory for %q: %w", subject, err)
	}
	return sub, nil
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
