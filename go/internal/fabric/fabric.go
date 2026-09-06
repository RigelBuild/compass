package fabric

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	compassv1internal "github.com/RigelBuild/compass/go/internal/gen/compass/v1"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

// Unsubscribe tears down a Subscribe. Idempotent: calling it twice is safe, so a
// deferred Unsubscribe beside an explicit one is not a bug.
type Unsubscribe func()

// EventFabric is the comms/delivery event seam (frozen, §T3). It carries
// compact references on JetStream — durable at-least-once fan-out — never
// payloads; see EventRef.
type EventFabric interface {
	Publish(ctx context.Context, subject string, ref EventRef) error
	Subscribe(ctx context.Context, subject string, fn func(EventRef)) (Unsubscribe, error)
}

// RunnerFabric is the Server↔Runner async seam (frozen, §T3): per-Runner
// command push out, queue-grouped event fan-in back. It rides core NATS
// (best-effort); a command to an offline Runner is recovered by the
// delivery-cursor sweep, not by a stream.
//
// The typed request/reply legs (enrollment, the unary Relay* calls,
// FetchSecrets, the bulk FetchAgentConfig pull) are NOT here — they stay on the
// Connect/gRPC edge (§Q3, OQ-5 Variant B), which keeps deadline propagation and
// typed proto errors.
type RunnerFabric interface {
	SendCommand(ctx context.Context, runnerID string, cmd *compassv1internal.SessionsResponse) error
	Events(ctx context.Context) (<-chan RunnerEvent, error)
}

// Config configures a Fabric. Only URL is required; every other field has a
// documented default from stream.go, so the common case is
// fabric.New(fabric.Config{URL: natsURL}).
type Config struct {
	// URL is the NATS connection string. A single node and a cluster differ
	// only here (§Q3: scaling NATS is never an application mode) — a
	// comma-separated seed list is a cluster.
	URL string

	// Name labels this connection in NATS monitoring (`nats server report
	// connections`). Defaults to "compass".
	Name string

	// Options are extra nats.Options appended after the fabric's own, so a
	// caller can add credentials or TLS without this package growing a field per
	// auth mechanism. A later option wins over an earlier one.
	//
	// The fabric reserves the CONNECTION LIFECYCLE for its own shutdown
	// coordination: Close observes the connection's status directly rather than
	// through a nats.ClosedHandler, so a caller may add its own ClosedHandler
	// (a monitoring hook, say) without disarming Close's drain wait. Replacing
	// the fabric's DisconnectErrHandler/ReconnectHandler is likewise safe —
	// those are log-only, and a caller that replaces them loses the fabric's
	// outage diagnostics, nothing more.
	Options []nats.Option

	// StreamName overrides DefaultStreamName.
	StreamName string
	// MaxDeliver overrides DefaultMaxDeliver. It is the total number of
	// delivery ATTEMPTS, not retries: MaxDeliver=1 parks on the first failure
	// with no retry at all, and MaxDeliver=5 means the fifth failing attempt
	// parks.
	//
	// The budget is enforced twice, by design (§Q3): the app-level check in
	// retryOrPark parks at the budget, and the consumer's server-side
	// MaxDeliver is the backstop for the case where the app-level check cannot
	// run (unreadable metadata — which already parks). Both derive from THIS
	// field, so every Server instance subscribing to a given subject MUST run
	// the same fabric Config: the durable consumer is shared, so a divergent
	// MaxDeliver would make the shared consumer's server-side budget
	// flip-flop with whichever instance last ran CreateOrUpdateConsumer. One
	// stack config for the whole deployment (RIG-2861 topology) is what
	// guarantees it; the fabric deliberately does not probe for drift.
	MaxDeliver int
	// AckWait overrides DefaultAckWait.
	AckWait time.Duration
	// DuplicateWindow overrides DefaultDuplicateWindow.
	DuplicateWindow time.Duration
	// MaxAge overrides DefaultMaxAge.
	MaxAge time.Duration
	// Replicas overrides DefaultReplicas; set 3 against a clustered NATS.
	Replicas int

	// RunnerEventBuffer is the capacity of the channel Events returns. A
	// consumer slower than the Runner event rate makes the core-NATS
	// subscription's own buffer the backstop — NATS drops for a slow consumer
	// rather than blocking the connection, which is the intended best-effort
	// semantic. Defaults to 256.
	RunnerEventBuffer int

	// Log carries the diagnostics that cannot be returned to a caller — a
	// dropped Runner event, a parked comms event, an async consume error. A nil
	// Log falls back to slog.Default (the house convention).
	Log *slog.Logger
}

func (c Config) streamName() string {
	if c.StreamName != "" {
		return c.StreamName
	}
	return DefaultStreamName
}

func (c Config) maxDeliver() int {
	if c.MaxDeliver > 0 {
		return c.MaxDeliver
	}
	return DefaultMaxDeliver
}

// deliveryBudget is maxDeliver as the unsigned count JetStream reports in
// MsgMetadata.NumDelivered. maxDeliver is an int only because ConsumerConfig
// takes one; the guard makes the widening provably wrap-free rather than
// relying on the accessor's own invariant holding forever.
func (c Config) deliveryBudget() uint64 {
	n := c.maxDeliver()
	if n <= 0 {
		return DefaultMaxDeliver
	}
	return uint64(n)
}

func (c Config) ackWait() time.Duration {
	if c.AckWait > 0 {
		return c.AckWait
	}
	return DefaultAckWait
}

func (c Config) duplicateWindow() time.Duration {
	if c.DuplicateWindow > 0 {
		return c.DuplicateWindow
	}
	return DefaultDuplicateWindow
}

func (c Config) maxAge() time.Duration {
	if c.MaxAge > 0 {
		return c.MaxAge
	}
	return DefaultMaxAge
}

func (c Config) replicas() int {
	if c.Replicas > 0 {
		return c.Replicas
	}
	return DefaultReplicas
}

func (c Config) runnerEventBuffer() int {
	if c.RunnerEventBuffer > 0 {
		return c.RunnerEventBuffer
	}
	return defaultRunnerEventBuffer
}

func (c Config) logger() *slog.Logger {
	if c.Log != nil {
		return c.Log
	}
	return slog.Default()
}

// Fabric is the one NATS client: it implements both EventFabric and
// RunnerFabric over a single connection, because the record gives each party
// exactly one ("Each Runner holds ONE fabric connection … each Server
// likewise"). Safe for concurrent use.
type Fabric struct {
	cfg Config
	log *slog.Logger

	nc *nats.Conn
	js jetstream.JetStream

	// streamMu guards the lazily-ensured stream handle (see ensureStream).
	streamMu sync.Mutex
	stream   jetstream.Stream

	// closeOnce makes Close idempotent, and closed gates new work so a
	// Subscribe racing a Close cannot register a consumer on a dying
	// connection.
	closeOnce sync.Once
	closeErr  error
	closedMu  sync.RWMutex
	closed    bool

	// teardown is closed by Close at the START of shutdown, before the drain.
	// It is the signal the runner-events pump and every Subscribe watchdog
	// select on, so a Close with an uncancelled subscribe/Events context still
	// tears them down — nats.go closes neither a ChanSubscription's channel nor
	// a ConsumeContext's buffer on connection close, so relying on those to
	// unblock is a leaked goroutine and, for Events, a channel that never
	// closes under a ranging consumer.
	//
	// Distinct from the closed/closedMu fail-closed gate above and not a
	// substitute for it: that gate answers "may I start new work", this one
	// answers "stop the work already running".
	teardown chan struct{}
}

// Compile-time proof Fabric satisfies both frozen seams. Cheap here, and it
// fails the build rather than a consumer's wiring if a signature drifts.
var (
	_ EventFabric  = (*Fabric)(nil)
	_ RunnerFabric = (*Fabric)(nil)
)

// New connects to NATS and returns the fabric. It does not create the JetStream
// stream — that happens lazily on the first Publish/Subscribe, because the
// frozen signature carries no context and rooting a fresh one here would sever
// the caller's cancellation chain.
func New(cfg Config) (*Fabric, error) {
	if cfg.URL == "" {
		return nil, errors.New("fabric: Config.URL is required (the NATS connection string)")
	}
	name := cfg.Name
	if name == "" {
		name = "compass"
	}
	log := cfg.logger()

	// f is captured by the connection handlers below so they can tell an
	// unplanned disconnect (a real outage the operator must see) from the drain
	// Close performs (expected, and noise if logged). It is assigned before
	// nats.Connect can fire either.
	f := &Fabric{
		cfg:      cfg,
		log:      log,
		teardown: make(chan struct{}),
	}

	// Reconnect forever: NATS being briefly unreachable is an outage to ride
	// out, not a reason to abandon the connection — the record's degrade path is
	// "fabric outage degrades to sweep-recovered delivery", which requires the
	// client to come back on its own.
	opts := append([]nats.Option{
		nats.Name(name),
		nats.MaxReconnects(-1),
		nats.DisconnectErrHandler(func(_ *nats.Conn, err error) {
			if f.checkOpen() != nil {
				return // Close's own drain.
			}
			log.Warn("fabric: nats disconnected; delivery degrades to the cursor sweep until reconnect", "error", err)
		}),
		nats.ReconnectHandler(func(nc *nats.Conn) {
			log.Info("fabric: nats reconnected", "url", nc.ConnectedUrl())
		}),
	}, cfg.Options...)

	nc, err := nats.Connect(cfg.URL, opts...)
	if err != nil {
		return nil, fmt.Errorf("fabric: connecting to nats at %q: %w", cfg.URL, err)
	}
	js, err := jetstream.New(nc)
	if err != nil {
		nc.Close()
		return nil, fmt.Errorf("fabric: creating jetstream context: %w", err)
	}
	f.nc, f.js = nc, js
	return f, nil
}

// Close drains and closes the connection: Drain flushes pending publishes and
// lets in-flight subscription callbacks finish before the socket goes away, so a
// shutdown does not lose an already-published event. It also tears down the
// work this fabric started — the runner-events pump and every live Subscribe —
// even when their contexts are still uncancelled, because nats.go leaves both a
// ChanSubscription's channel and a ConsumeContext's buffer open on connection
// close and neither would ever unblock on its own.
//
// Close returns only once the drain has completed (bounded by closeTimeout), so
// a caller that returns from Close can rely on the flush having happened.
// Idempotent — the first call's result is returned to every caller.
func (f *Fabric) Close() error {
	f.closeOnce.Do(func() {
		f.closedMu.Lock()
		f.closed = true
		f.closedMu.Unlock()
		// Before the drain: the pump and the subscribe watchdogs must stop
		// consuming and hand their consumers back while the connection is
		// still usable, so the drain has something coherent to flush.
		close(f.teardown)

		// Register the CLOSED listener BEFORE Drain: StatusChanged reports only
		// future transitions and does not replay one that already fired, so a
		// listener installed after Drain could miss the close entirely. Using
		// the connection's own status (not a nats.ClosedHandler option) means a
		// caller's Config.Options cannot overwrite the drain-completion signal.
		closed := f.nc.StatusChanged(nats.CLOSED)
		defer f.nc.RemoveStatusListener(closed)

		if err := f.nc.Drain(); err != nil {
			f.closeErr = fmt.Errorf("fabric: draining nats connection: %w", err)
			// Drain refused (already closed, or the connection is gone); close
			// outright so the socket and its goroutines are not leaked. That
			// also drives the connection to CLOSED, so there is nothing left
			// worth waiting for on this path.
			f.nc.Close()
			return
		}
		// Drain is asynchronous: it returns as soon as the connection enters
		// DRAINING. Wait for CLOSED so Close does not return mid-flush. Guard
		// the already-closed case first: if the connection reached CLOSED
		// between Drain and here, the transition has already fired and the
		// listener will never see it.
		if f.nc.IsClosed() {
			return
		}
		select {
		case <-closed:
		case <-time.After(closeTimeout):
			// The drain WAS initiated and continues in the background; the
			// bound only exists so a wedged server cannot hang shutdown
			// forever, which would be a worse failure than an unconfirmed
			// flush.
			f.log.Warn("fabric: nats drain did not complete within the close timeout; shutting down anyway",
				"timeout", closeTimeout)
		}
	})
	return f.closeErr
}

// closeTimeout bounds how long Close waits for the asynchronous drain to reach
// a closed connection. Generous, because the wait is what makes Close's
// no-lost-publish contract real; bounded, because a wedged or vanished server
// must not hang a process's shutdown.
const closeTimeout = 10 * time.Second

// flushTimeout bounds a flush whose caller's context carries no deadline. A
// flush is a round-trip to the server, and nats.go refuses a context without
// one; a Publish/Events call whose ctx is a process-lifetime context still
// needs the write confirmed in bounded time rather than hanging until shutdown.
const flushTimeout = 10 * time.Second

// flush round-trips the connection so a preceding core-NATS publish or
// subscribe is known to have reached the server. It derives from the caller's
// ctx — never re-roots — adding a deadline only when ctx has none, so
// cancellation still propagates.
func (f *Fabric) flush(ctx context.Context) error {
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, flushTimeout)
		defer cancel()
	}
	return f.nc.FlushWithContext(ctx)
}

// errClosed is returned by any operation attempted after Close.
var errClosed = errors.New("fabric: closed")

// checkOpen refuses work on a closed fabric. Fail-closed: a Publish that
// silently no-ops after shutdown would look like a delivered event.
func (f *Fabric) checkOpen() error {
	f.closedMu.RLock()
	defer f.closedMu.RUnlock()
	if f.closed {
		return errClosed
	}
	return nil
}
