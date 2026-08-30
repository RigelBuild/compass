package ingest

// The notify webhook arm (RIG-2732 T7, design.md pipeline step 1/4): the notify
// consumer behind the one GitHub ingress that the board arm also sits behind.
// The webhook handler verifies, acks 200, and fans each accepted event to this
// arm's Enqueue (a non-blocking channel try-send, github_webhook.go:44-51 — the
// 200 ack is never on the downstream latency path), and a single drain goroutine
// hands each dequeued event to the in-package NotifyRouter (step 4, the notify
// hot path). Unlike the board arm this does NOT hydrate/coalesce/target-gate: the
// router does its own store loads and subscriber match, so the drain is a plain
// dequeue-then-Route, log-and-continue on a per-event error.

import (
	"context"
	"expvar"
	"log/slog"
	"sync/atomic"

	"github.com/RigelBuild/compass/go/internal/forge"
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
	queue   chan forge.ForgeEvent
	log     *slog.Logger
	dropped atomic.Int64
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
		queue:  make(chan forge.ForgeEvent, size),
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
func (a *NotifyWebhookArm) Enqueue(_ context.Context, ev forge.ForgeEvent) {
	select {
	case a.queue <- ev:
	default:
		a.dropped.Add(1)
		notifyWebhookDrops.Add(1)
		a.log.Warn("notify webhook: queue full, dropping event (reconciler heals)",
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
		case ev := <-a.queue:
			if err := a.router.Route(ctx, ev); err != nil {
				a.log.Warn("notify route failed",
					"err", err, "repo", ev.Repo, "number", ev.Number, "change", ev.Change)
			}
		}
	}
}
