package fabric

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"strings"
	"sync"
	"time"

	compassv1internal "github.com/RigelBuild/compass/go/internal/gen/compass/v1"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"go.opentelemetry.io/otel/metric"
)

// Unsubscribe tears down a Subscribe. Idempotent: calling it twice is safe, so a
// deferred Unsubscribe beside an explicit one is not a bug.
type Unsubscribe func()

// EventFabric is the comms/delivery event seam (§T3). It carries
// compact references on JetStream — durable at-least-once fan-out — never
// payloads; see EventRef.
type EventFabric interface {
	Publish(ctx context.Context, subject string, ref EventRef) error
	// Subscribe and SubscribeKind invoke fn serially per subscription, with a
	// ctx carrying the publisher's span context when one was propagated. fn
	// returning nil acks the event; an error or a panic redelivers it up to
	// MaxDeliver attempts, then parks it on DLQSubject.
	Subscribe(ctx context.Context, subject string, fn func(context.Context, EventRef) error) (Unsubscribe, error)
	// SubscribeKind shares one tenant-wildcard consumer across Server instances; publish remains per-tenant.
	SubscribeKind(ctx context.Context, kind EventKind, fn func(context.Context, EventRef) error) (Unsubscribe, error)
	// OnReconnect runs after each reconnect, never the initial connection; bursts may coalesce.
	// The callback never overlaps itself, and unsubscribe is idempotent.
	OnReconnect(fn func()) (Unsubscribe, error)
}

// RunnerFabric is the Server↔Runner async seam (§T3): per-Runner
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

// RoutingFabric broadcasts best-effort binding invalidations to every Server, unlike EventFabric's durable queue-group delivery.
// Core NATS has no ack, retry, or DLQ; malformed payloads are logged and dropped.
type RoutingFabric interface {
	PublishBindingChange(ctx context.Context, tenant string, b BindingChange) error
	// SubscribeBindingChanges is plain core NATS: every Server caches bindings, so a queue group would leave stale peers.
	// Unknown ops invalidate and re-read; dropping them would miss updates from newer publishers.
	SubscribeBindingChanges(ctx context.Context, fn func(BindingChange)) (Unsubscribe, error)
}

// Config configures a Fabric. Only URL is required; every other field has a
// documented default from stream.go, so the common case is
// fabric.New(ctx, fabric.Config{URL: natsURL}).
type Config struct {
	// URL is the NATS connection string. A single node and a cluster differ
	// only here (§Q3: scaling NATS is never an application mode) — a
	// comma-separated seed list is a cluster, so a ',' in a credential must be
	// percent-encoded (%2C).
	URL string

	// Name labels this connection in NATS monitoring (`nats server report
	// connections`). Defaults to "compass".
	Name string

	// Options append after fabric defaults; later options win.
	// Close observes connection status directly, while reconnect callbacks are chained so caller handlers do not disarm fabric behavior.
	Options []nats.Option

	// StreamName overrides DefaultStreamName.
	StreamName string
	// MaxDeliver counts attempts, not retries: the final failing attempt parks.
	// App and server limits enforce the same budget; all Server instances sharing a durable must use the same Config.
	MaxDeliver int
	// AckWait overrides DefaultAckWait.
	AckWait time.Duration
	// DuplicateWindow overrides DefaultDuplicateWindow.
	DuplicateWindow time.Duration
	// MaxAge overrides DefaultMaxAge.
	MaxAge time.Duration
	// Replicas overrides DefaultReplicas; set 3 against a clustered NATS.
	Replicas int

	// RunnerEventBuffer bounds the event channel; NATS drops for slow consumers rather than blocking the connection.
	RunnerEventBuffer int

	// Log carries the diagnostics that cannot be returned to a caller — a
	// dropped Runner event, a parked comms event, an async consume error. A nil
	// Log falls back to slog.Default (the house convention).
	Log *slog.Logger

	// testInactiveThreshold replaces the reap threshold. Tests only: production
	// always gets the bounded value inactiveThreshold derives.
	testInactiveThreshold time.Duration
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

// Fabric is the one NATS client: it implements EventFabric, RunnerFabric and
// RoutingFabric over a single connection, because the record gives each party
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

	// teardown stops active pumps and watchdogs when Close begins; NATS does not close their channels.
	// The closed gate rejects new work; teardown stops work already running.
	teardown chan struct{}

	// consumerClosed, when set before Subscribe, is called once a subscription's
	// consumer has fully drained and closed. Tests only: ctx-done teardown is
	// asynchronous, so this is the event that says the old consumer is gone.
	consumerClosed func()
	// beforeTerm is a test hook between DLQ publish and Term.
	beforeTerm func()
	// parkDecided is a test hook reporting each park path's decision.
	parkDecided      func(path string, published bool)
	beforeDLQPublish func() error
	getParkedMsg     func(ctx context.Context, seq uint64) (*jetstream.RawStreamMsg, error)
	parkClaimWaiting func(path string)

	// parkedSequences holds recent (durable, stream seq) park claims and their expiry;
	// activePark pins a claim while that delivery's callback is still running.
	parkedMu        sync.Mutex
	parkedSequences map[parkKey]*parkClaim
	activePark      map[parkKey]int

	// reconnectHooks is read by the hook goroutine New starts; reconnectSignal
	// wakes it. One slot, sent non-blocking, so a reconnect burst coalesces and
	// the NATS callback goroutine never waits on a hook.
	reconnectMu         sync.Mutex
	reconnectNext       uint64
	reconnectHooks      map[uint64]func()
	reconnectSignal     chan struct{}
	consumerMu          sync.RWMutex
	consumers           map[string]liveConsumer
	backlogRegistration metric.Registration
}

// Compile-time proof Fabric satisfies every seam. Cheap here, and it fails the
// build rather than a consumer's wiring if a signature drifts.
var (
	_ EventFabric   = (*Fabric)(nil)
	_ RunnerFabric  = (*Fabric)(nil)
	_ RoutingFabric = (*Fabric)(nil)
)

// New connects to NATS and returns the fabric. It does not create the JetStream
// stream; that happens lazily on the first Publish/Subscribe. ctx only feeds the
// logs of the fabric's callbacks and goroutines, so it must carry no request span.
func New(ctx context.Context, cfg Config) (*Fabric, error) {
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
		cfg:             cfg,
		log:             log,
		consumers:       make(map[string]liveConsumer),
		teardown:        make(chan struct{}),
		reconnectHooks:  make(map[uint64]func()),
		reconnectSignal: make(chan struct{}, 1),
	}

	// Read the caller's ReconnectHandler off a scratch Options so the fabric's
	// own handler, appended last, can chain it rather than be replaced by it.
	var scratch nats.Options
	for _, opt := range cfg.Options {
		if opt == nil {
			continue
		}
		if err := opt(&scratch); err != nil {
			return nil, fmt.Errorf("fabric: applying Config.Options: %w", err)
		}
	}
	callerReconnected := scratch.ReconnectedCB

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
			log.WarnContext(ctx, "fabric: nats disconnected; delivery degrades to the cursor sweep until reconnect", "error", err)
		}),
		// The Runner plane's safety argument: a stalled receiver is dropped AND
		// reported, with the cursor sweep recovering what was dropped. The default
		// options install no async error callback — so without this the one lossy path
		// drops events with no log, no metric, no error.
		nats.ErrorHandler(func(_ *nats.Conn, sub *nats.Subscription, err error) {
			if sub == nil {
				log.WarnContext(ctx, "fabric: nats async error", "error", err)
				return
			}
			log.WarnContext(ctx, "fabric: nats async error; a slow consumer drops events until it keeps up",
				"subject", sub.Subject, "dropped", subDropped(sub), "error", err)
		}),
	}, cfg.Options...)
	opts = append(opts, nats.ReconnectHandler(func(nc *nats.Conn) {
		if f.checkOpen() != nil {
			return // Close's own drain; no sweep is wanted.
		}
		// Redacted: the NATS URL can carry credentials or a token as userinfo.
		log.InfoContext(ctx, "fabric: nats reconnected", "url", redactURL(nc.ConnectedUrl()))
		f.signalReconnect()
		if callerReconnected != nil {
			callerReconnected(nc)
		}
	}))

	nc, err := nats.Connect(cfg.URL, opts...)
	if err != nil {
		// nats.go's errors can quote URL fragments (a parse error, or a DNS error
		// on a comma-split credential). With credentials, only fixed-text sentinels
		// pass, wrapped as the sentinel itself so no server or URL text rides along.
		if strings.Contains(cfg.URL, "@") {
			for _, s := range []error{nats.ErrNoServers, nats.ErrAuthorization} {
				if errors.Is(err, s) {
					return nil, fmt.Errorf("fabric: connecting to nats at %q: %w", redactURL(cfg.URL), s)
				}
			}
			return nil, fmt.Errorf("fabric: connecting to nats at %q: connect failed; cause withheld because the URL carries credentials", redactURL(cfg.URL))
		}
		return nil, fmt.Errorf("fabric: connecting to nats at %q: %w", redactURL(cfg.URL), err)
	}
	js, err := jetstream.New(nc)
	if err != nil {
		nc.Close()
		return nil, fmt.Errorf("fabric: creating jetstream context: %w", err)
	}
	f.nc, f.js = nc, js
	f.registerBacklogMetric(ctx)
	go f.runReconnectLoop(ctx)
	return f, nil
}

// redactURL hides NATS URL credentials. Every userinfo byte, in any seed-list
// entry, precedes the string's last '@', so all of it is cut. A credentialed
// seed list keeps only the hosts after that '@'.
func redactURL(raw string) string {
	at := strings.LastIndex(raw, "@")
	if at < 0 {
		return raw
	}
	scheme := "nats"
	// Only a letters-only prefix is a scheme; anything else is credential.
	if i := strings.Index(raw[:at], "://"); i > 0 && strings.Trim(raw[:i], "abcdefghijklmnopqrstuvwxyz") == "" {
		scheme = raw[:i]
	}
	return scheme + "://<redacted>@" + raw[at+1:]
}

// subDropped reports a subscription's dropped-message count for logging.
// Subscription.Dropped returns an error once the subscription is invalid, which
// is exactly the moment an error handler may run — so a failed read degrades to
// -1 rather than losing the log line that names the slow consumer.
func subDropped(sub *nats.Subscription) int {
	n, err := sub.Dropped()
	if err != nil {
		return -1
	}
	return n
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
func (f *Fabric) Close(ctx context.Context) error {
	f.closeOnce.Do(func() {
		f.closedMu.Lock()
		f.closed = true
		f.closedMu.Unlock()
		// Before the drain: the pump and the subscribe watchdogs must stop
		// consuming and hand their consumers back while the connection is
		// still usable, so the drain has something coherent to flush.
		close(f.teardown)
		if f.backlogRegistration != nil {
			if err := f.backlogRegistration.Unregister(); err != nil {
				f.log.WarnContext(ctx, "fabric: unregistering consumer backlog callback failed", "error", err)
			}
		}

		// Register the CLOSED listener BEFORE Drain: StatusChanged reports only
		// future transitions and does not replay one that already fired, so a
		// listener installed after Drain could miss the close entirely. Using the
		// connection's own status means a caller's Config.Options cannot overwrite it.
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
		// DRAINING. Wait for CLOSED so Close does not return mid-flush. Guard the
		// already-closed case first: if the connection reached CLOSED between Drain
		// and here, the transition already fired and the listener will never see it.
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
			f.log.WarnContext(ctx, "fabric: nats drain did not complete within the close timeout; shutting down anyway",
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

// OnReconnect registers fn to run after each reconnect; see
// EventFabric.OnReconnect. fn runs on the fabric's hook goroutine under a panic
// guard, never concurrently with itself, and reconnects may coalesce into one
// call. fn never runs for the initial connect. Unsubscribe is idempotent.
func (f *Fabric) OnReconnect(fn func()) (Unsubscribe, error) {
	if fn == nil {
		return nil, errors.New("fabric: OnReconnect requires a callback")
	}
	f.reconnectMu.Lock()
	defer f.reconnectMu.Unlock()
	if err := f.checkOpen(); err != nil {
		return nil, err
	}
	f.reconnectNext++
	id := f.reconnectNext
	f.reconnectHooks[id] = fn
	var once sync.Once
	return func() {
		once.Do(func() {
			f.reconnectMu.Lock()
			delete(f.reconnectHooks, id)
			f.reconnectMu.Unlock()
		})
	}, nil
}

// signalReconnect wakes the hook goroutine without blocking: if a wake-up is
// already pending, this reconnect coalesces into it.
func (f *Fabric) signalReconnect() {
	select {
	case f.reconnectSignal <- struct{}{}:
	default:
	}
}

// runReconnectLoop is the hook goroutine New starts. It keeps hooks off the
// NATS callback goroutine and exits with the fabric.
func (f *Fabric) runReconnectLoop(ctx context.Context) {
	for {
		select {
		case <-f.teardown:
			return
		case <-f.reconnectSignal:
			// select picks randomly once teardown is closed too; never sweep then.
			if f.checkOpen() != nil {
				return
			}
			f.runReconnectHooks(ctx)
		}
	}
}

// runReconnectHooks runs every hook in registration order, each under its own
// panic guard so one bad hook neither kills the process nor skips the rest.
func (f *Fabric) runReconnectHooks(ctx context.Context) {
	f.reconnectMu.Lock()
	ids := slices.Sorted(maps.Keys(f.reconnectHooks))
	hooks := make([]func(), 0, len(ids))
	for _, id := range ids {
		hooks = append(hooks, f.reconnectHooks[id])
	}
	f.reconnectMu.Unlock()
	for _, fn := range hooks {
		f.runReconnectHook(ctx, fn)
	}
}

func (f *Fabric) runReconnectHook(ctx context.Context, fn func()) {
	defer func() {
		if r := recover(); r != nil {
			f.log.ErrorContext(ctx, "fabric: reconnect hook panicked", "panic", r)
		}
	}()
	fn()
}

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
