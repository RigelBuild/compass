//go:build unix

// Package delivery fans each posted message out to subscribed live agent
// sessions as a `deliver` control, timed by the author-split settle gate. It
// consumes message_posted refs from the event fabric on a goroutine rooted in
// the serve ctx; the hub's ack arm advances the durable delivery cursor.
package delivery

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
	"sync"
	"time"

	comms "github.com/RigelBuild/compass/go/internal/comms"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/metric"

	compassv1 "github.com/RigelBuild/compass/go/gen/compass/v1"
	"github.com/RigelBuild/compass/go/internal/fabric"
	compassv1internal "github.com/RigelBuild/compass/go/internal/gen/compass/v1"
	"github.com/RigelBuild/compass/go/internal/store"
)

// ControlDispatcher is the consumer's view of the RunnerHub: the send-only
// control relay (design.md:737-739). Its error return is a SYNCHRONOUS refusal
// only — no live Sessions stream / an immediate push failure — which the
// consumer treats as "no live session" and falls to the D2 sweep. A Runner-side
// ASYNC refusal (RunnerError) does not surface here; the hub observes it
// (router.complete) and the cursor stays unadvanced. runnerhub.Hub implements it
// via DispatchControl.
type ControlDispatcher interface {
	DispatchControl(ctx context.Context, sessionID string, op *compassv1internal.AgentControl) error
}

// SessionResolver resolves agent accounts to their live sessions, kept separate
// from ControlDispatcher so that stays the established dispatch-only shape.
// runnerhub.Hub implements it.
type SessionResolver interface {
	// SessionForAccount returns a live session, or false so delivery falls to the D2 sweep.
	// The context scopes cache-miss reads; denied misses also fall to the sweep.
	SessionForAccount(ctx context.Context, account store.AccountID) (sessionID string, ok bool)
	// LiveAgentSessions snapshots every live binding for lag-resync delivery.
	LiveAgentSessions() map[store.AccountID]string
}

// DeliveryReads provides the recipient sets and recovery reads for the consumer.
// *store.Store implements it.
type DeliveryReads interface { //nolint:interfacebloat // one method per store read the consumer drives; the surface is the delivery-read contract, not incidental sprawl
	// SubscribedAgents returns subscribed channel agents the author may reach.
	SubscribedAgents(ctx context.Context, channel store.ChannelID, author store.AccountID) ([]store.AccountID, error)
	IsAgentAccount(ctx context.Context, account store.AccountID) (bool, error)
	MessageByID(ctx context.Context, messageID string) (store.Message, error)
	// MessageChannel resolves the channel a message lives in through its topic
	// (topics.channel_id) — a wire/store message carries only its topic now, so
	// the fan-out resolves the channel it delivers to through this join (the
	// design record's topic->channel resolution).
	MessageChannel(ctx context.Context, messageID string) (store.ChannelID, error)
	// TopicChannelNames resolves display names; a miss yields empty names and never blocks delivery.
	TopicChannelNames(ctx context.Context, topicID string) (topicName, channelName string, err error)
	UndeliveredMessages(ctx context.Context, agent store.AccountID) (map[store.ChannelID][]store.Message, error)
	// ChannelAgentMembers returns members the author may reach and their handles;
	// out-of-reach members are absent even when they share the channel.
	ChannelAgentMembers(ctx context.Context, channel store.ChannelID, author store.AccountID) ([]store.ChannelAgentMember, error)
	// ResolveOwner scopes bare mentions to the posting author's owner namespace.
	ResolveOwner(ctx context.Context, caller store.AccountID) (store.AccountID, error)
	// SweepChannels includes subscribed, home, and mandatory channels, even without owed messages.
	// Pin sweeps need this full set to inject pins regardless of cursor.
	SweepChannels(ctx context.Context, agent store.AccountID) ([]store.ChannelID, error)
	// PinnedEntries returns entries ordered by position for cursor-independent pin delivery.
	PinnedEntries(ctx context.Context, channel store.ChannelID) ([]store.PinnedEntry, error)
	// OwedMentions returns every message owed to agent (T1), keyed by channel,
	// ascending seq — the sweepOwedMentions read (RIG-1641 T2).
	OwedMentions(ctx context.Context, agent store.AccountID) (map[store.ChannelID][]store.Message, error)
	// RecordOwedMention durably records that messageID (in channel) is owed to
	// agent — the no-loss backstop when an offline mentioned member is outside
	// the sweep set (T1). Idempotent on (agent, message_id).
	RecordOwedMention(ctx context.Context, agent store.AccountID, channel store.ChannelID, messageID string) error
	// InSweepSet reports whether agent is in channel's D2 sweep set (subscribed
	// OR home OR mandatory) — an out-of-sweep-set mentioned member has no cursor
	// backstop, so it needs a durable owed row (T2).
	InSweepSet(ctx context.Context, agent store.AccountID, channel store.ChannelID) (bool, error)
	// ClearOwedMention deletes the owed row for (agent, messageID) via the pool
	// (no txn) — sweepOwedMentions clears a permanently-unreadable owed message
	// so it stops re-logging on every start (T2).
	ClearOwedMention(ctx context.Context, agent store.AccountID, messageID string) error
	// CountOwedMentions returns the total owed_mention row count — the startup
	// visibility log (T2 observability).
	CountOwedMentions(ctx context.Context) (int, error)
	// OwedMentionAccounts lists every agent still owed a mention, for the wake
	// retried once a Runner can serve it.
	OwedMentionAccounts(ctx context.Context) ([]store.AccountID, error)
	// MarkMentionsRouted stamps messageID's settle-edge mention pass complete
	// (mentions_routed_at = now, unix ms) — the recovery scan's mark after it
	// replays a message's mention pass, and the live path's mark (T3). Idempotent:
	// the contract readers rely on is NULL vs non-NULL only (RIG-2490 T1).
	MarkMentionsRouted(ctx context.Context, messageID string) error
	// UnroutedMentionMessages returns unmarked committed messages after afterSeq in ascending order, each with its channel.
	// The caller advances a call-local cursor until a batch is short.
	UnroutedMentionMessages(ctx context.Context, afterSeq int64, limit int) ([]store.MessageWithChannel, error)
}

// settleEvent is one queued author-settle edge handed from the hub's Deliver
// goroutine (OnSessionSettled) to the consumer's ctx-rooted loop.
type settleEvent struct {
	sessionID    string
	state        compassv1.AgentSessionState
	turnSequence uint64
	// upTo bounds the commit times this edge fires. A real settle fires all; a
	// late hold's replay is bounded when no turn sequence is available.
	upTo int64
	// generation is the author's identity generation when the edge was queued.
	generation uint64
}

// startEvent is one queued session-start edge handed from the hub's Start (or
// re-enroll re-promotion) goroutine (OnSessionStarted) to the consumer's
// ctx-rooted loop, which sweeps the freshly-live session's owed messages.
type startEvent struct {
	sessionID string
	account   store.AccountID
}

// lostEvent is one lost session. A refused deliver always had a message in
// flight; errored marks a Runner-reported ERRORED, which may owe nothing.
type lostEvent struct {
	account store.AccountID
	errored bool
	// deferred marks a backoff timer firing: it wakes without adding a strike.
	deferred bool
}

// erroredBackoff spaces repeat ERRORED wakes of one account. A message that
// crashes the agent stays owed, so without it each crash re-wakes at once.
type erroredBackoff struct {
	strikes int
	last    time.Time
	pending bool
}

// The cap must stay below the reset window, or a steady crash loop would reset
// its own strikes and wake at once every time.
const (
	erroredWakeBaseDelay  = 30 * time.Second
	erroredWakeMaxDelay   = 15 * time.Minute
	erroredWakeResetAfter = 30 * time.Minute
)

// instrumentationScope is the OTel instrumentation scope for this package's
// spans AND metrics (the delivery tracer and meter both resolve from the global
// providers T2/T3 install). Shared by every otel.Tracer / otel.Meter call here.
const instrumentationScope = "github.com/RigelBuild/compass/go/internal/delivery"

// recoveryFloorInterval bounds how long a message whose fabric publish failed
// while NATS stayed up waits before a recovery pass delivers it.
const recoveryFloorInterval = 5 * time.Minute

// heldEntry records a post's turn sequence and the trace, tenant, and commit metadata.
type heldEntry struct {
	messageID    string
	traceparent  string
	tenant       store.TenantID
	atUnixMs     int64
	turnSequence uint64
	// generation is the author's identity generation at hold time.
	generation uint64
}

// Consumer consumes message_posted refs and fans posted messages out to
// subscribed live agent sessions. Safe for concurrent use: the pending-deliver
// registry, the settle queue, and the per-session dispatch gates mutate under mu;
// the fabric callback, the loop, the settle hook, and per-session dispatches all
// touch them.
type Consumer struct {
	fab      fabric.EventFabric
	st       DeliveryReads
	dispatch ControlDispatcher
	resolver SessionResolver
	log      *slog.Logger
	// agentWaker resumes offline recipients so owed work reaches them promptly.
	// Nil is safe; the cursor sweep remains the durable backstop.
	agentWaker AgentWaker

	mu sync.Mutex
	// held keeps author-session messages until settle or a terminal frame.
	// Values preserve post order; cursor sweeps recover entries stranded by a reap race.
	held map[string][]heldEntry
	// lastSettle records the latest settle timestamp and reported sequence for
	// late holds. A sequence comparison takes precedence over wall time.
	lastSettle         map[string]int64
	lastSettleSequence map[string]uint64
	lastSettleLegacy   map[string]bool
	// generation counts each session's STARTING edges; turn sequences compare
	// only within one generation because a fresh identity restarts at 1.
	generation map[string]uint64
	// generationStart is when each session's current generation began (ms).
	generationStart map[string]int64
	// fallbackLogged records sessions warned about legacy settles until teardown.
	fallbackLogged map[string]struct{}
	// settleQueue buffers author-settle edges the hook enqueues, drained by the
	// loop under its ctx. A slice (never lost) plus a buffered notify channel
	// (coalescing wakeups): the hook appends and signals without blocking Deliver.
	settleQueue []settleEvent
	// startQueue buffers session-start edges the hook enqueues, drained by the
	// loop under its ctx into the reconnect sweep (RIG-1569 T6). Same shape as
	// settleQueue: a slice (never lost) plus the shared notify wakeup, so the
	// hook appends and signals without blocking the hub's Start goroutine.
	startQueue []startEvent
	// lostQueue buffers sessions the Runner reported gone; the loop wakes each
	// account, and the woken session's start sweep redelivers what is owed.
	lostQueue []lostEvent
	// owedRewake is set when a Runner command stream attaches: a wake that failed
	// while none could serve it is retried for every agent still owed a mention.
	owedRewake bool
	// recoveryPending marks a full recovery pass (sweepAllLive + the mention
	// scan) owed by a fabric reconnect or a floor tick; the loop runs it.
	recoveryPending bool
	// notify wakes the loop when settleQueue OR startQueue grows or a recovery
	// pass is owed. Buffered(1) with a non-blocking send, so many edges between
	// drains collapse to one wakeup and the hook never blocks.
	notify chan struct{}
	// gates serializes dispatch per RECIPIENT session (design.md:212-225): a
	// session's live delivers and its reconnect-sweep re-dispatch drain through
	// one gate so a live deliver never interleaves ahead of an in-flight sweep for
	// the same session. Lazily created per session id.
	gates map[string]*sync.Mutex

	// beforeGate, when set, is called with a recipient session id right before a
	// live deliver acquires that session's dispatch gate — a TEST-ONLY seam
	// (nil in production) that lets a test deterministically observe a live
	// deliver reaching the gate while a sweep holds it (the case-6 ordering gate).
	beforeGate func(sessionID string)

	// newFloorTicker starts the recovery floor tick; a test swaps in a channel
	// it drives.
	newFloorTicker func() (<-chan time.Time, func())

	// now supplies wall-clock bounds for legacy late holds and tests.
	now func() time.Time

	// erroredWakes holds per-account ERRORED wake backoff, under mu.
	erroredWakes map[store.AccountID]*erroredBackoff
	// afterFunc schedules a deferred wake; a test swaps in a manual trigger.
	afterFunc func(time.Duration, func())

	// dispatched counts control ops by kind only; nil skips the metric.
	// Session, channel, and message labels would create high cardinality.
	dispatched metric.Int64Counter
}

// NewConsumer constructs the fan-out consumer. It takes the hub as dispatch and
// resolver (the hub is already built at server assembly); the settle edge is
// wired the other way, via hub.SetSettleSink(consumer), AFTER both exist — the
// post-construction setter that breaks the construction cycle (§2). fab is the
// event fabric Run consumes; assembly fails closed before it could be nil. A nil
// log falls back to slog.Default.
func NewConsumer(ctx context.Context, st DeliveryReads, dispatch ControlDispatcher, resolver SessionResolver, fab fabric.EventFabric, log *slog.Logger) *Consumer {
	if log == nil {
		log = slog.Default()
	}
	// Create the dispatch counter once from the global meter (providers set by
	// T2/T3). On error, leave it nil and log — a metric miss must never fail
	// consumer construction or block a delivery.
	dispatched, err := otel.Meter(instrumentationScope).Int64Counter(
		"compass.delivery.dispatched",
		metric.WithDescription("Count of live fan-out control dispatch attempts (deliver + steer), by op kind."),
	)
	if err != nil {
		log.WarnContext(ctx, "delivery: failed to create dispatch counter; delivery metrics disabled", "err", err)
		dispatched = nil
	}
	return &Consumer{
		fab:                fab,
		st:                 st,
		dispatch:           dispatch,
		resolver:           resolver,
		log:                log,
		held:               make(map[string][]heldEntry),
		lastSettle:         make(map[string]int64),
		lastSettleSequence: make(map[string]uint64),
		lastSettleLegacy:   make(map[string]bool),
		generation:         make(map[string]uint64),
		generationStart:    make(map[string]int64),
		fallbackLogged:     make(map[string]struct{}),
		notify:             make(chan struct{}, 1),
		gates:              make(map[string]*sync.Mutex),
		dispatched:         dispatched,
		newFloorTicker: func() (<-chan time.Time, func()) {
			t := time.NewTicker(recoveryFloorInterval)
			return t.C, t.Stop
		},
		now:          time.Now,
		erroredWakes: make(map[store.AccountID]*erroredBackoff),
		afterFunc: func(d time.Duration, f func()) {
			time.AfterFunc(d, f)
		},
	}
}

// SetAgentWaker wires the offline-agent wake sink (the server's resume machinery)
// AFTER both the consumer and the hub exist — the post-construction setter that
// breaks the delivery<->server construction cycle, mirroring comms.SetPresenceSource.
// Called once at server assembly before serving; no lock
// because the write happens-before the first dispatch. Nil-safe to leave unset (a
// consumer with no waker does not wake — today's behavior).
func (c *Consumer) SetAgentWaker(w AgentWaker) {
	c.agentWaker = w
}

// Run consumes message_posted refs and drains settle, start, and recovery work
// until ctx is cancelled. A publish failed after commit, or a ref parked after
// its reads kept failing, reaches live recipients via the recovery pass
// (reconnect or floor tick) and offline ones at their next session start.
func (c *Consumer) Run(ctx context.Context) error {
	// Sweeps and scans enumerate every tenant, so they run as the BYPASSRLS system
	// role; per-event work stays tenant-scoped under ctx.
	sysCtx := store.WithSystemRole(ctx)

	unsubReconnect, err := c.fab.OnReconnect(c.requestRecovery)
	if err != nil {
		return fmt.Errorf("delivery: register fabric reconnect hook: %w", err)
	}
	defer unsubReconnect()

	// Surface the durable owed-mention backlog once at start — a silently-growing
	// owed_mentions table (a wake path that never resumes) must be visible.
	if n, err := c.st.CountOwedMentions(sysCtx); err != nil {
		c.log.WarnContext(sysCtx, "delivery: count owed mentions at start", "error", err)
	} else {
		c.log.InfoContext(sysCtx, "delivery: owed mention backlog at start", "count", n)
	}
	// Scan before subscribing: callbacks run concurrently with this loop, so this
	// is the only way no event is handled before the start recovery finishes.
	c.scanMissedMentions(sysCtx)

	unsub, err := c.fab.SubscribeKind(ctx, fabric.KindMessagePosted, c.onEventRef)
	if err != nil {
		// Shutdown during stream setup is a clean stop, like cancel in the loop.
		if ctx.Err() != nil {
			return nil
		}
		return fmt.Errorf("delivery: subscribe to message_posted: %w", err)
	}
	defer unsub()

	floor, stopFloor := c.newFloorTicker()
	defer stopFloor()

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-c.notify:
			// Held delivers re-read under their own tenant, so the settle drain
			// gets ctx, never the system role (which would override the tenant).
			c.drainSettles(ctx)
			c.drainStarts(sysCtx)
			c.drainLost(sysCtx)
			c.drainOwedRewake(sysCtx)
			c.drainRecovery(sysCtx)
		case <-floor:
			c.requestRecovery()
		}
	}
}

// onEventRef handles one ref on the fabric goroutine, concurrently with Run's
// drains. A transient read error is returned before any hold or dispatch, so the
// fabric redelivers; a missing row or a deliberate skip returns nil and acks.
// A post whose author settled before the hold landed is delivered at once (hold).
func (c *Consumer) onEventRef(ctx context.Context, ref fabric.EventRef) error {
	ctx = store.WithTenant(ctx, store.TenantID(ref.Tenant))
	m, err := c.st.MessageByID(ctx, ref.RowID)
	if err != nil {
		return c.readFailure(ctx, "re-read posted message", ref.RowID, err)
	}
	return c.onMessagePosted(ctx, comms.MessageToWire(m))
}

// readFailure logs a failed pre-dispatch read. A missing row returns nil (acked),
// since redelivery cannot make it appear; any other error is returned to redeliver.
// The tenant comes from ctx, which onEventRef scoped to the ref's tenant.
func (c *Consumer) readFailure(ctx context.Context, what, messageID string, err error) error {
	tenant, _ := store.TenantFromContext(ctx)
	if errors.Is(err, store.ErrNotFound) {
		c.log.ErrorContext(ctx, "delivery: "+what+": row missing, skipping", "error", err, "message_id", messageID, "tenant", string(tenant))
		return nil
	}
	c.log.WarnContext(ctx, "delivery: "+what+": will redeliver", "error", err, "message_id", messageID, "tenant", string(tenant))
	return fmt.Errorf("delivery: %s %s: %w", what, messageID, err)
}

// requestRecovery marks a recovery pass owed and wakes the loop. It runs on the
// fabric's reconnect goroutine and the floor tick, so it never blocks or reads.
func (c *Consumer) requestRecovery() {
	c.mu.Lock()
	c.recoveryPending = true
	c.mu.Unlock()
	select {
	case c.notify <- struct{}{}:
	default:
	}
}

// drainRecovery runs an owed recovery pass. sweepAllLive is the part that
// reaches a plain deliver whose publish failed; the scan covers mentions only.
func (c *Consumer) drainRecovery(ctx context.Context) {
	c.mu.Lock()
	pending := c.recoveryPending
	c.recoveryPending = false
	c.mu.Unlock()
	if !pending {
		return
	}
	// A settle time guards a hold at any age, for example when a backlog replays
	// after an outage, so only a dead session's entry goes.
	live := c.liveSessionIDs()
	c.mu.Lock()
	for sid := range c.lastSettle {
		if _, ok := live[sid]; !ok {
			delete(c.lastSettle, sid)
			delete(c.lastSettleSequence, sid)
			delete(c.lastSettleLegacy, sid)
			delete(c.generation, sid)
			delete(c.generationStart, sid)
			delete(c.fallbackLogged, sid)
		}
	}
	// A session restarted but never settled has no lastSettle entry.
	for sid := range c.generation {
		if _, ok := live[sid]; !ok {
			delete(c.generation, sid)
			delete(c.generationStart, sid)
		}
	}
	c.pruneErroredWakes()
	c.mu.Unlock()
	c.sweepAllLive(ctx)
	c.scanMissedMentions(ctx)
}

// deliverOp carries the message and denormalized source/author names so rendering needs no roster lookup.
// Name misses yield empty strings and never block delivery.
func deliverOp(msg *compassv1.Message, fromHandle, channelName, topicName, traceparent string) *compassv1internal.AgentControl {
	return &compassv1internal.AgentControl{
		Control: &compassv1internal.AgentControl_Deliver{
			Deliver: &compassv1internal.DeliverControl{Message: msg, FromHandle: fromHandle, ChannelName: channelName, TopicName: topicName, Traceparent: traceparent},
		},
	}
}

// steerOp mirrors deliverOp for mid-turn mentions; only the recipient-side operation differs.
// It carries the same denormalized names, and name misses never block delivery.
func steerOp(msg *compassv1.Message, fromHandle, channelName, topicName, traceparent string) *compassv1internal.AgentControl {
	return &compassv1internal.AgentControl{
		Control: &compassv1internal.AgentControl_Steer{
			Steer: &compassv1internal.SteerControl{Message: msg, FromHandle: fromHandle, ChannelName: channelName, TopicName: topicName, Traceparent: traceparent},
		},
	}
}

// sourceNames resolves render-only channel and topic names; missing topics or store misses yield empty strings.
// A name miss must not block dispatch.
func (c *Consumer) sourceNames(ctx context.Context, msg *compassv1.Message) (channelName, topicName string) {
	topicID := msg.GetTopicId()
	if topicID == "" {
		return "", ""
	}
	topicName, channelName, err := c.st.TopicChannelNames(ctx, topicID)
	if err != nil {
		c.log.ErrorContext(ctx, "delivery: resolve source channel/topic names for injection",
			"error", err, "message_id", msg.GetId(), "topic_id", topicID)
		return "", ""
	}
	return channelName, topicName
}

// mentionRE matches a bare or owner-qualified handle after `@`. Group 1 captures
// the complete handle, including its optional owner qualifier.
var mentionRE = regexp.MustCompile(`(?i)@([a-z0-9][a-z0-9._-]*(?:/[a-z0-9][a-z0-9._-]*)?)`)

// reservedMentions are case-insensitive channel-wide targets.
// @everyone/@agents expand to agents; @users expands to human members.
var reservedMentions = map[string]bool{"everyone": true, "agents": true, "users": true}

// parseMentions returns distinct lowercased handles in first-appearance order.
// The full qualified spelling is the deduplication key and resolution input.
func parseMentions(text string) []string {
	matches := mentionRE.FindAllStringSubmatch(text, -1)
	if len(matches) == 0 {
		return nil
	}
	seen := make(map[string]bool, len(matches))
	var out []string
	for _, match := range matches {
		handle := strings.ToLower(match[1])
		if seen[handle] {
			continue
		}
		seen[handle] = true
		out = append(out, handle)
	}
	return out
}

// mentionHandles parses the distinct mentioned handles across all of msg's text
// blocks (design.md:516: the mention parse reads the block set). Blocks are
// scanned in order and deduped globally, so a handle mentioned in two blocks
// steers once.
func mentionHandles(msg *compassv1.Message) []string {
	blocks := msg.GetBlocks()
	if len(blocks) == 0 {
		return nil
	}
	seen := map[string]bool{}
	var out []string
	for _, b := range blocks {
		for _, h := range parseMentions(b.GetText()) {
			if seen[h] {
				continue
			}
			seen[h] = true
			out = append(out, h)
		}
	}
	return out
}

// gateFor returns the per-session dispatch gate, creating it on first use.
func (c *Consumer) gateFor(sessionID string) *sync.Mutex {
	c.mu.Lock()
	defer c.mu.Unlock()
	g, ok := c.gates[sessionID]
	if !ok {
		g = &sync.Mutex{}
		c.gates[sessionID] = g
	}
	return g
}

// storeMessageToWire re-reads through the shared mapper and resolves the channel through the message topic.
// Returning the channel separately preserves the fan-out gate for messages without a channel ID.
func (c *Consumer) storeMessageToWire(ctx context.Context, messageID string) (*compassv1.Message, store.ChannelID, store.AccountID, error) {
	m, err := c.st.MessageByID(ctx, messageID)
	if err != nil {
		return nil, "", "", err
	}
	channel, err := c.st.MessageChannel(ctx, messageID)
	if err != nil {
		return nil, "", "", err
	}
	return comms.MessageToWire(m), channel, m.AuthorAccountID, nil
}
