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
	// SessionForAccount returns the live session bound to account, or ok=false
	// when the account has no live session (deliver falls to the D2 sweep). ctx
	// is threaded so the hub's read-through binding cache (RIG-3108) can scope a
	// cache-miss table read: under the consumer's system-role ctx the read-through
	// is refused and the miss falls to the sweep, exactly this method's contract.
	SessionForAccount(ctx context.Context, account store.AccountID) (sessionID string, ok bool)
	// LiveAgentSessions snapshots every live (account -> session) binding — the
	// set the lag-resync sweep iterates so it redelivers to every live recipient.
	LiveAgentSessions() map[store.AccountID]string
}

// DeliveryReads is the store surface the consumer reads: subscriber resolution,
// the author agent/human split, the settled-message re-read, the sweep, the
// mention→steer routing set (channel agent members + handle resolution, D5), and
// the RIG-1641 owed-mention arm (record/read/clear/count + the sweep-set
// predicate). *store.Store implements it.
type DeliveryReads interface { //nolint:interfacebloat // one method per store read the consumer drives; the surface is the delivery-read contract, not incidental sprawl
	SubscribedAgents(ctx context.Context, channel store.ChannelID, author store.AccountID) ([]store.AccountID, error)
	IsAgentAccount(ctx context.Context, account store.AccountID) (bool, error)
	MessageByID(ctx context.Context, messageID string) (store.Message, error)
	// MessageChannel resolves the channel a message lives in through its topic
	// (topics.channel_id) — a wire/store message carries only its topic now, so
	// the fan-out resolves the channel it delivers to through this join (the
	// frozen record's topic->channel resolution).
	MessageChannel(ctx context.Context, messageID string) (store.ChannelID, error)
	// TopicChannelNames resolves a message's topic id to its topic name and the
	// name of the channel it lives in (topics.channel_id -> channels.name) — the
	// source-name denormalization stamped onto the deliver/steer control so the
	// recipient renders the source channel+topic without a roster lookup (RIG-2956
	// T0). An unknown topic id is store.ErrNotFound, which the caller logs and
	// treats as empty names — never a delivery block, exactly as GetAccount's
	// from_handle miss degrades.
	TopicChannelNames(ctx context.Context, topicID string) (topicName, channelName string, err error)
	UndeliveredMessages(ctx context.Context, agent store.AccountID) (map[store.ChannelID][]store.Message, error)
	// ChannelAgentMembers resolves every agent MEMBER of a channel (subscribe
	// state irrelevant), author excluded — the mention→steer routing set (D5,
	// design.md:526-527), distinct from SubscribedAgents' deliver set.
	ChannelAgentMembers(ctx context.Context, channel store.ChannelID, author store.AccountID) ([]store.AccountID, error)
	// AgentByHandle resolves a bare mention handle to its agent account within
	// owner's namespace (RIG-2751 handle cutover: agent handles are per-owner);
	// the caller passes the posting author's owner, since a mention is a bare
	// handle in the author's own namespace. An unknown, wrong-owner, or non-agent
	// (human) handle is store.ErrNotFound (a mention no-op, D5).
	AgentByHandle(ctx context.Context, owner store.AccountID, handle string) (store.Account, error)
	// ResolveOwner resolves the posting author to the owner-user namespace its
	// bare mentions resolve in (an agent author → its owner_user_id, a user
	// author → itself).
	ResolveOwner(ctx context.Context, caller store.AccountID) (store.AccountID, error)
	// SweepChannels resolves the D1 disjunct channel set an agent sweeps: every
	// subscribed channel, PLUS its home channel, PLUS any mandatory_subscription
	// channel it is a member of (T4 policy) — the pin sweep's channel
	// enumeration (design.md T7). Distinct from UndeliveredMessages, whose map
	// omits channels with no owed messages: the pin sweep must visit every
	// subscribed channel to inject its current pins regardless of cursor.
	SweepChannels(ctx context.Context, agent store.AccountID) ([]store.ChannelID, error)
	// PinnedEntries returns a channel's pinned board ordered by position (the
	// channel_pins store, T6). The pin sweep dispatches a deliver for each pinned
	// message regardless of cursor position (design.md T7).
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
	// GetAccount resolves an account by id, used to denormalize the author's
	// handle onto the deliver/steer control (RIG-2486 T1 from_handle). An unknown
	// id is store.ErrNotFound, which the caller logs and treats as an empty
	// handle — never a delivery block.
	GetAccount(ctx context.Context, id store.AccountID) (store.Account, error)
	// MarkMentionsRouted stamps messageID's settle-edge mention pass complete
	// (mentions_routed_at = now, unix ms) — the recovery scan's mark after it
	// replays a message's mention pass, and the live path's mark (T3). Idempotent:
	// the contract readers rely on is NULL vs non-NULL only (RIG-2490 T1).
	MarkMentionsRouted(ctx context.Context, messageID string) error
	// UnroutedMentionMessages returns committed messages whose settle-edge mention
	// pass never completed (mentions_routed_at IS NULL) AND whose seq is > afterSeq,
	// ascending seq, each with its channel resolved — the recovery scan read
	// (RIG-2490 T1). limit bounds one batch; the caller loops, advancing afterSeq
	// (a scan-LOCAL, never-persisted cursor) to the last returned seq until a batch
	// is short.
	UnroutedMentionMessages(ctx context.Context, afterSeq int64, limit int) ([]store.MessageWithChannel, error)
}

// settleEvent is one queued author-settle edge handed from the hub's Deliver
// goroutine (OnSessionSettled) to the consumer's ctx-rooted loop.
type settleEvent struct {
	sessionID string
	state     compassv1.AgentSessionState
	// upTo bounds the commit times this edge fires. A real settle fires all; a
	// late hold's replay fires only its settled turn, not a still-streaming one.
	upTo int64
}

// startEvent is one queued session-start edge handed from the hub's Start (or
// re-enroll re-promotion) goroutine (OnSessionStarted) to the consumer's
// ctx-rooted loop, which sweeps the freshly-live session's owed messages.
type startEvent struct {
	sessionID string
	account   store.AccountID
}

// instrumentationScope is the OTel instrumentation scope for this package's
// spans AND metrics (the delivery tracer and meter both resolve from the global
// providers T2/T3 install). Shared by every otel.Tracer / otel.Meter call here.
const instrumentationScope = "github.com/RigelBuild/compass/go/internal/delivery"

// recoveryFloorInterval bounds how long a message whose fabric publish failed
// while NATS stayed up waits before a recovery pass delivers it.
const recoveryFloorInterval = 5 * time.Minute

// heldEntry is one pending-deliver registry element: the held message id plus
// the W3C traceparent and tenant captured at hold time, so the deliver fired when
// the author settles re-links to the publisher's trace and re-reads under the
// message's own tenant. Empty traceparent ⇒ empty on the wire.
type heldEntry struct {
	messageID   string
	traceparent string
	tenant      store.TenantID
	atUnixMs    int64 // commit time, matched against settleEvent.upTo
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
	// agentWaker best-effort resumes an offline recipient's session so an owed
	// mention or subscribed deliver reaches it promptly (RIG-1641 T3). Set once
	// at assembly via SetAgentWaker, AFTER both the consumer and the hub exist
	// (the server package implements it over the resume machinery). Nil-safe: a
	// consumer with no waker wired does not wake — today's behavior. T3 defines
	// and wires this seam; no routing path calls it yet (that is T2/T4).
	agentWaker AgentWaker

	mu sync.Mutex
	// held is the pending-deliver registry (design.md:157-168), keyed by the
	// AUTHOR's live session id: an agent-authored message posted while its author
	// still streams is HELD here until that session settles (WORKING->READY) or
	// reaches a terminal frame. Values are in post order, so a settle fires them
	// ascending. A no-frame author death never settles; its entry waits for the
	// next-enroll reap (OnSessionsReaped), and a reap race can strand one entry.
	// The sweeps skip only messages held for a LIVE author, so the cursor sweep
	// still delivers a stranded entry.
	held map[string][]heldEntry
	// lastSettle maps an author session id to the unix ms of its latest settle
	// edge, so a message whose hold lost the race with that settle fires at once.
	// The recovery pass drops entries of dead sessions; the reap drops them too.
	lastSettle map[string]int64
	// settleQueue buffers author-settle edges the hook enqueues, drained by the
	// loop under its ctx. A slice (never lost) plus a buffered notify channel
	// (coalescing wakeups): the hook appends and signals without blocking Deliver.
	settleQueue []settleEvent
	// startQueue buffers session-start edges the hook enqueues, drained by the
	// loop under its ctx into the reconnect sweep (RIG-1569 T6). Same shape as
	// settleQueue: a slice (never lost) plus the shared notify wakeup, so the
	// hook appends and signals without blocking the hub's Start goroutine.
	startQueue []startEvent
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

	// now stamps and prunes lastSettle; a test swaps in a fixed clock.
	now func() time.Time

	// dispatched counts control dispatches (deliver + steer), labelled only by
	// op kind (compass.op.kind = steer|deliver). Created ONCE at NewConsumer from
	// the global meter; nil when meter construction failed, in which case the
	// increment is skipped (a metric miss never blocks a delivery). NEVER
	// labelled per-session/channel/message — that is a cardinality hazard.
	dispatched metric.Int64Counter
}

// NewConsumer constructs the fan-out consumer. It takes the hub as dispatch and
// resolver (the hub is already built at server assembly); the settle edge is
// wired the other way, via hub.SetSettleSink(consumer), AFTER both exist — the
// post-construction setter that breaks the construction cycle (§2). fab is the
// event fabric Run consumes; assembly fails closed before it could be nil. A nil
// log falls back to slog.Default.
func NewConsumer(st DeliveryReads, dispatch ControlDispatcher, resolver SessionResolver, fab fabric.EventFabric, log *slog.Logger) *Consumer {
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
		log.Warn("delivery: failed to create dispatch counter; delivery metrics disabled", "err", err)
		dispatched = nil
	}
	return &Consumer{
		fab:        fab,
		st:         st,
		dispatch:   dispatch,
		resolver:   resolver,
		log:        log,
		held:       make(map[string][]heldEntry),
		lastSettle: make(map[string]int64),
		notify:     make(chan struct{}, 1),
		gates:      make(map[string]*sync.Mutex),
		dispatched: dispatched,
		newFloorTicker: func() (<-chan time.Time, func()) {
			t := time.NewTicker(recoveryFloorInterval)
			return t.C, t.Stop
		},
		now: time.Now,
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
		}
	}
	c.mu.Unlock()
	c.sweepAllLive(ctx)
	c.scanMissedMentions(ctx)
}

// deliverOp wraps a wire message in the AgentControl deliver op the relay carries
// (§5 command envelope; agent.proto deliver = 3). fromHandle is the author's
// handle denormalized onto the control so the agent emits the SessionInjection
// observation's from_handle without a roster lookup on the injection path
// (RIG-2486 T1); empty when the author handle could not be resolved (logged at
// the resolve site — a handle miss never blocks a delivery). channelName and
// topicName are the source channel+topic names denormalized the same way so the
// agent renders "Channel <name> › topic <name>:" without a roster lookup
// (RIG-2956 T0); each is empty on a resolve miss, which never blocks a delivery.
func deliverOp(msg *compassv1.Message, fromHandle, channelName, topicName, traceparent string) *compassv1internal.AgentControl {
	return &compassv1internal.AgentControl{
		Control: &compassv1internal.AgentControl_Deliver{
			Deliver: &compassv1internal.DeliverControl{Message: msg, FromHandle: fromHandle, ChannelName: channelName, TopicName: topicName, Traceparent: traceparent},
		},
	}
}

// steerOp wraps a wire message in the AgentControl steer op the relay carries
// (mirror of deliverOp; agent.proto steer = 2). D5 routes an `@`-mention to a
// channel agent member as a steer (mid-turn interrupt) rather than a deliver
// (turn-end coalesced) — the only deliver-vs-steer difference is recipient-side
// (design.md:558-562). SteerControl carries the same single first-party Message
// as DeliverControl (DL-073), plus the same denormalized author from_handle
// (RIG-2486 T1) and source channel+topic names (RIG-2956 T0) — each empty on a
// resolve miss, which never blocks a delivery.
func steerOp(msg *compassv1.Message, fromHandle, channelName, topicName, traceparent string) *compassv1internal.AgentControl {
	return &compassv1internal.AgentControl{
		Control: &compassv1internal.AgentControl_Steer{
			Steer: &compassv1internal.SteerControl{Message: msg, FromHandle: fromHandle, ChannelName: channelName, TopicName: topicName, Traceparent: traceparent},
		},
	}
}

// sourceNames resolves a wire message's topic id to its source channel name and
// topic name — the values denormalized onto the deliver/steer control so the
// agent renders the source without a roster lookup (RIG-2956 T0). A missing
// topic id or a store miss is logged and yields empty names: the names are a
// render signal, never a delivery precondition, so a name miss must not block
// the dispatch — the exact log-and-continue posture authorHandle applies to the
// from_handle.
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

// authorHandle resolves a wire message's author account id to its handle — the
// value denormalized onto the deliver/steer control as the SessionInjection
// from_handle (RIG-2486 T1). A missing author id or a store miss is logged and
// yields an empty handle: the from_handle is an observation signal, never a
// delivery precondition, so a handle miss must not block the dispatch (matches
// the log-and-continue posture the mention/subscriber resolvers already use).
func (c *Consumer) authorHandle(ctx context.Context, msg *compassv1.Message) string {
	author := store.AccountID(msg.GetAuthorAccountId())
	if author == "" {
		return ""
	}
	acc, err := c.st.GetAccount(ctx, author)
	if err != nil {
		c.log.ErrorContext(ctx, "delivery: resolve author handle for injection from_handle",
			"error", err, "message_id", msg.GetId(), "author", string(author))
		return ""
	}
	return acc.Handle
}

// mentionRE matches one `@`-mention token: `@` then a handle. The handle is
// [a-z0-9][a-z0-9._-]* — the leading char must be a letter/digit, so a bare `@`
// or `@.` does not match. This is the client grammar ported verbatim for parity
// (apps/ui/src/comms.ts:265 MENTION_RE = /@([a-z0-9][a-z0-9._-]*)/gi); keep the
// two grammars in sync. Parity is GRAMMAR-level only: the server routes from raw
// block text, so an @handle inside a code span or link label DOES route here,
// whereas the client renderer never chips a mention inside code or a link label
// (MarkdownText.tsx:389-398). Go RE2 has no inline /i, so the (?i) prefix
// carries the flag; no backrefs are needed. Group 1 is the handle without `@`.
var mentionRE = regexp.MustCompile(`(?i)@([a-z0-9][a-z0-9._-]*)`)

// reservedMentions are the broadcast ping targets that expand to a channel's
// member sets server-side (apps/ui/src/comms-stub.ts:182 RESERVED_MENTIONS).
// Matched case-insensitively against the lowercased handle (comms.ts:279-281).
// For steer routing, @everyone / @agents expand to the channel's agent members;
// @users expands to human members (no agent session to steer) (design.md:532-534).
var reservedMentions = map[string]bool{"everyone": true, "agents": true, "users": true}

// parseMentions returns the distinct lowercased handles mentioned in text, in
// first-appearance order. Lowercasing folds the case-insensitive grammar into a
// single dedup key for both reserved-ping matching and handle resolution
// (account handles are stored lowercase). The `@` is stripped; group 1 is the
// handle.
func parseMentions(text string) []string {
	matches := mentionRE.FindAllStringSubmatch(text, -1)
	if len(matches) == 0 {
		return nil
	}
	seen := make(map[string]bool, len(matches))
	var out []string
	for _, m := range matches {
		h := strings.ToLower(m[1])
		if seen[h] {
			continue
		}
		seen[h] = true
		out = append(out, h)
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

// storeMessageToWire re-reads a message from the store, maps it to the wire shape
// via the ONE store->wire mapper (comms.MessageToWire), and resolves the channel
// it lives in through its topic — the settled-block re-read the settle gate and
// the no-live-author / sweep paths dispatch from (design.md:158-161), never a
// stale in-memory copy. The channel is returned alongside because a wire/store
// message no longer carries it: the fan-out gate needs the channel, resolved
// through topics.channel_id (the frozen record's topic->channel resolution).
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
