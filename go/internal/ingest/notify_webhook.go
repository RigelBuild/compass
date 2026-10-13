package ingest

// The notify webhook arm (RIG-2732 T7): the notify consumer behind the one GitHub
// ingress the board arm also sits behind. The handler verifies, acks 200, and
// fans each accepted event to Enqueue (a non-blocking try-send, off the ack path).

// A drain goroutine hands each event to the in-package NotifyRouter. Unlike the
// board arm it does NOT hydrate/coalesce — the router does its own store loads —
// so the drain is a plain dequeue-then-Route.

import (
	"context"
	"expvar"
	"log/slog"
	"sync/atomic"

	"go.opentelemetry.io/otel"

	"github.com/RigelBuild/compass/go/internal/forge"
	otelx "github.com/RigelBuild/compass/go/internal/otel"
)

// notifyWebhookDrops is the exported queue-full/drop metric: a sustained
// queue-full silently degrades the notify hot path (the reconcile sweep is the
// heal ceiling), so the drop is scrapeable for an alerting threshold — a
// counter+Warn alone is not enough to notice the degradation.
var notifyWebhookDrops = expvar.NewInt("compass_notify_webhook_drops")

// defaultNotifyQueueSize is the bounded drain-queue depth. Sized to absorb a
// normal event burst between Route calls; a sustained overflow drops with the
// metric+Warn and is healed by the reconcile sweep.
const defaultNotifyQueueSize = 1024

// eventRouter is the per-event route seam, satisfied structurally by
// *NotifyRouter (in this package). Defined locally so the arm depends on no
// concrete router type on the drain path and a test can inject a fake.
type eventRouter interface {
	Route(ctx context.Context, ev forge.ForgeEvent) error
}

// NotifyArmConfig configures the notify webhook arm.
type NotifyArmConfig struct {
	// QueueSize is the bounded drain-queue depth; <= 0 uses defaultNotifyQueueSize.
	QueueSize int
	// Log is the arm logger; nil uses slog.Default().
	Log *slog.Logger
}

// NotifyWebhookArm consumes forge events from the webhook ingress and routes
// each through the shared NotifyRouter's notify hot path.
type NotifyWebhookArm struct {
	router  eventRouter
	queue   chan queuedEvent
	log     *slog.Logger
	dropped atomic.Int64
}

// queuedEvent carries the enqueuer's trace across the queue, so the routed
// event's span joins the webhook delivery that accepted it.
type queuedEvent struct {
	ev          forge.ForgeEvent
	traceparent string
}

// NewNotifyWebhookArm returns an arm that routes each accepted event through r.
func NewNotifyWebhookArm(r eventRouter, cfg NotifyArmConfig) *NotifyWebhookArm {
	log := cfg.Log
	if log == nil {
		log = slog.Default()
	}
	size := cfg.QueueSize
	if size <= 0 {
		size = defaultNotifyQueueSize
	}
	return &NotifyWebhookArm{
		router: r,
		queue:  make(chan queuedEvent, size),
		log:    log,
	}
}

// Dropped reports the number of events this arm dropped on a full queue (the
// same fact published to the notifyWebhookDrops expvar). Test-observable.
func (a *NotifyWebhookArm) Dropped() int64 { return a.dropped.Load() }

// Enqueue satisfies server.ForgeEventSink's contract (github_webhook.go:44-51):
// it MUST NOT block. It channel try-sends; a full queue DROPS the event with the
// drop metric + a Warn (the reconcile sweep heals it). Unlike the board arm it
// applies no kind/change filter — the router handles all forge kinds.
func (a *NotifyWebhookArm) Enqueue(ctx context.Context, ev forge.ForgeEvent) {
	select {
	case a.queue <- queuedEvent{ev: ev, traceparent: otelx.Traceparent(ctx)}:
	default:
		a.dropped.Add(1)
		notifyWebhookDrops.Add(1)
		a.log.WarnContext(ctx, "notify webhook: queue full, dropping event (reconciler heals)",
			"repo", ev.Repo, "number", ev.Number, "change", ev.Change)
	}
}

// Run drains the queue until ctx is cancelled — then it returns nil (clean
// shutdown, driver.go:95-99 idiom). Each dequeued event is routed through the
// NotifyRouter; a per-event route error is logged and the drain CONTINUES (a bad
// event must not kill the drain — the reconcile sweep heals any durable gap).
func (a *NotifyWebhookArm) Run(ctx context.Context) error {
	for {
		select {
		case <-ctx.Done():
			return nil
		case q := <-a.queue:
			a.route(ctx, q)
		}
	}
}

// route runs one dequeued event under a span parented by its enqueuer's trace;
// an event enqueued with no span gets a root.
func (a *NotifyWebhookArm) route(ctx context.Context, q queuedEvent) {
	ctx, span := otel.Tracer(instrumentationScope).Start(
		otelx.ContextWithTraceparent(otelx.WithoutSpan(ctx), q.traceparent), "ingest.notify.route")
	defer span.End()
	ev := q.ev
	if err := a.router.Route(ctx, ev); err != nil {
		a.log.WarnContext(ctx, "notify route failed",
			"err", err, "repo", ev.Repo, "number", ev.Number, "change", ev.Change)
	}
}
