//go:build unix

// Package delivery is the Server-side notification fan-out consumer (RIG-1569
// T3, design record D1). It tails the in-process comms event bus and, for each
// posted message, resolves the subscribed agent sessions and dispatches a
// `deliver` control down the existing Sessions relay to each live recipient —
// timed by the author-split settle gate — while the durable per-(agent, channel)
// delivery cursor is advanced only when the recipient acks (the ack arm lives in
// the RunnerHub; this package is the trigger + dispatch side).
//
// It mirrors comms.SubscribeComms's bus-tail discipline (subscribe.go) but runs
// its OWN long-lived goroutine started in server assembly, not a per-request
// handler: Run(ctx) roots the loop on the serve-scoped ctx, and the loop ends
// when that ctx is cancelled or the bus closes (rule://go-thread-context — the
// ctx passed to Run IS the goroutine's root; the loop never mints a fresh one,
// and the settle hook hands its work back to this loop rather than storing a
// ctx).
package delivery

import (
	"context"
	"log/slog"
	"regexp"
	"strings"
	"sync"

	comms "github.com/RigelBuild/compass/go/internal/comms"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/metric"

	"github.com/RigelBuild/compass/go/events"
	compassv1 "github.com/RigelBuild/compass/go/gen/compass/v1"
	compassv1internal "github.com/RigelBuild/compass/go/internal/gen/compass/v1"
	otelx "github.com/RigelBuild/compass/go/internal/otel"
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
	// when the account has no live session (deliver falls to the D2 sweep).
	SessionForAccount(account store.AccountID) (sessionID string, ok bool)
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

// heldEntry is one pending-deliver registry element: the held message id plus
// the W3C traceparent captured at hold time (the author's live-dispatch ctx), so
// the deliver fired when the author settles re-links to the publisher's trace
// across the goroutine boundary. Empty traceparent ⇒ empty on the wire.
type heldEntry struct {
	messageID   string
	traceparent string
}

// Consumer tails the comms bus and fans posted messages out to subscribed live
// agent sessions. Safe for concurrent use: the pending-deliver registry, the
// settle queue, and the per-session dispatch gates mutate under mu; the bus loop,
// the settle hook, and per-session dispatches all touch them.
type Consumer struct {
	bus      *events.Bus[*compassv1.SubscribeCommsResponse]
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
	// still streams is HELD here until that author's session settles
	// (WORKING->READY) or reaches a terminal frame. The value is the ordered set
	// of message ids held for that author, in post order, so a settle fires them
	// ascending. A no-frame author death (no settle edge ever enqueues) leaves
	// its entry here until it is reaped: the reap happens in-process on the next
	// Runner (re-)enroll via the hub's SessionReapSink (OnSessionsReaped), which
	// drops the entry for every session id whose hub binding enroll just cleared.
	// So the common no-frame death is reaped at that next enroll rather than
	// persisting until process restart. The reap is best-effort, NOT a hard
	// bound: the reaped set is exactly the session ids bound at enroll time, and a
	// no-frame-dead session is never re-promoted (its id, once cleared, never
	// re-enters the hub's session map), so a narrow race can still strand one
	// entry until process restart — a Deliver that resolved the author LIVE an
	// instant before enroll cleared the maps can hold(sess) just AFTER that
	// enroll's reap, re-adding the dead session's entry; because that id never
	// re-enrolls, no later enroll reaps it. Delivery correctness (no-loss) is
	// unaffected either way — only the reap (a leak bound, not the delivery
	// guarantee) is best-effort: the recipient still receives the message via the
	// reconnect cursor sweep, independent of this registry.
	held map[string][]heldEntry
	// settleQueue buffers author-settle edges the hook enqueues, drained by the
	// loop under its ctx. A slice (never lost) plus a buffered notify channel
	// (coalescing wakeups): the hook appends and signals without blocking Deliver.
	settleQueue []settleEvent
	// startQueue buffers session-start edges the hook enqueues, drained by the
	// loop under its ctx into the reconnect sweep (RIG-1569 T6). Same shape as
	// settleQueue: a slice (never lost) plus the shared notify wakeup, so the
	// hook appends and signals without blocking the hub's Start goroutine.
	startQueue []startEvent
	// notify wakes the loop when settleQueue OR startQueue grows. Buffered(1)
	// with a non-blocking send, so many edges between drains collapse to one
	// wakeup and the hook never blocks.
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

	// afterResubscribe, when set, is called right after the lag-overrun branch
	// re-subscribes to the bus and before it resumes the tail — a TEST-ONLY seam
	// (nil in production) that lets a test observe that the fresh subscription is
	// live, so a post-sweep publish is guaranteed to land on the new tail rather
	// than racing into the (deliberately un-drained) replay snapshot.
	afterResubscribe func()

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
// post-construction setter that breaks the construction cycle (§2). A nil log
// falls back to slog.Default.
func NewConsumer(bus *events.Bus[*compassv1.SubscribeCommsResponse], st DeliveryReads, dispatch ControlDispatcher, resolver SessionResolver, log *slog.Logger) *Consumer {
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
		bus:        bus,
		st:         st,
		dispatch:   dispatch,
		resolver:   resolver,
		log:        log,
		held:       make(map[string][]heldEntry),
		notify:     make(chan struct{}, 1),
		gates:      make(map[string]*sync.Mutex),
		dispatched: dispatched,
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

// Run tails the comms bus and dispatches until ctx is cancelled (serve shutdown)
// or the bus closes. It mirrors comms.forwardComms: drain the replay snapshot
// oldest-first, then select on ctx, the live tail, and the settle queue. On the
// live channel closing, sub.Lagged() distinguishes an overrun — re-subscribe
// then run the D2 recipient sweep, not a loss (design.md:227-231) — from a clean
// bus shutdown (end silently). ctx threads from the serve group into every store
// read and dispatch below; the loop never re-roots it.
func (c *Consumer) Run(ctx context.Context) error {
	// N5/OQ-4: the fan-out consumer is a cross-tenant background loop (it tails
	// EVERY tenant's posted messages, sweeps EVERY agent's owed set, and scans
	// the whole messages table). It must run under the BYPASSRLS system role, not
	// the tenant-scoped request path — a request-path (fail-closed) scope would
	// see zero rows and halt delivery fleet-wide. Marking the root ctx here
	// propagates the system role into every store call the loop makes (replay,
	// live dispatch, the settle/start drains, sweeps, and scanMissedMentions),
	// since the loop threads this ctx and never re-roots it.
	ctx = store.WithSystemRole(ctx)
	sub, err := c.bus.Subscribe(0, c.bus.InstanceEpoch())
	if err != nil {
		// A fresh subscription at since_seq=0 on a live bus cannot underflow; any
		// error here is a genuine subscribe fault the caller should see.
		return err
	}
	// Closure over sub (not defer sub.Cancel()): the lagged branch reassigns sub
	// to a fresh subscription, and only a closure cancels whichever one is
	// current at return, not the value captured at defer time.
	defer func() { sub.Cancel() }()

	// Surface the durable owed-mention backlog once at start — a silently-growing
	// owed_mentions table (a wake path that never resumes) must be visible.
	if n, err := c.st.CountOwedMentions(ctx); err != nil {
		c.log.WarnContext(ctx, "delivery: count owed mentions at start", "error", err)
	} else {
		c.log.InfoContext(ctx, "delivery: owed mention backlog at start", "count", n)
	}

	// Recover the committed-but-unmarked mention set from durable state before
	// draining replay: mirrors the subscribe-first/sweep-second seam ordering
	// (see the overrun branch below), so the scan and the replay+live tail
	// together cover every message with no loss (RIG-2490 T3).
	c.scanMissedMentions(ctx)

	for _, event := range sub.Replay {
		select {
		case <-ctx.Done():
			return nil
		default:
		}
		c.handleEvent(otelx.ContextWithTraceparent(ctx, event.Traceparent), event.Payload)
	}

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-c.notify:
			c.drainSettles(ctx)
			c.drainStarts(ctx)
		case event, ok := <-sub.Live:
			if !ok {
				if sub.Lagged() {
					// Overrun: bus events were dropped. Not a loss — the cursor
					// defines exactly what is undelivered, so RE-SUBSCRIBE, then
					// sweep every live recipient (design.md:227-231): a resync is
					// a latency blip, never a stop, exactly as SubscribeComms
					// clients treat one (re-subscribe and carry on).
					//
					// Subscribe FIRST, sweep SECOND — the order closes a
					// delivery seam. The post path commits the store row BEFORE
					// publishing MessagePosted (comms.go:270-271), so a message M
					// committed+published in the window between an owed-read and
					// the fresh Subscribe's lock-acquire would be missed by a
					// sweep-first order: absent from the already-read owed set,
					// only in the fresh Replay (deliberately not drained), and
					// NOT on a Live that predates its registration
					// (events.go:216-219). Subscribing first makes the fresh Live
					// cover [T_sub, ∞) and the sweep cover [0, T_sweep] with
					// T_sweep ≥ T_sub, so a boundary message lands on BOTH. The
					// only cost is a benign double-deliver of the thin boundary
					// set, which at-least-once deliver-ack already tolerates (the
					// per-recipient dispatch gate + the store cursor's
					// above_seqs/duplicate-ack no-op dedupe). No seam, no loss.
					fresh, err := c.bus.Subscribe(0, c.bus.InstanceEpoch())
					if err != nil {
						// A fresh subscription at since_seq=0 on a live bus cannot
						// underflow; any error here is a genuine subscribe fault
						// the caller should see (mirrors the top-of-Run subscribe).
						c.log.ErrorContext(ctx, "delivery: re-subscribe after bus-lag overrun", "error", err)
						return err
					}
					// Cancel the old lagged sub and adopt the fresh one; the
					// deferred closure then cancels whichever is current at
					// return. The fresh Replay is deliberately NOT drained — the
					// sweep below is the replay-equivalent for the owed set, and
					// re-draining the retained ring would double-deliver every
					// message in it (the boundary overlap above is the tolerated
					// exception, not a re-drain).
					sub.Cancel()
					sub = fresh
					c.sweepAllLive(ctx)
					// Recover the committed-but-unmarked mention set dropped in
					// the overrun window: the scan/replay overlap is the same
					// tolerated at-least-once boundary set the sweep above
					// absorbs (RIG-2490 T3).
					c.scanMissedMentions(ctx)
					if c.afterResubscribe != nil {
						c.afterResubscribe()
					}
					continue
				}
				return nil
			}
			c.handleEvent(otelx.ContextWithTraceparent(ctx, event.Traceparent), event.Payload)
		}
	}
}

// handleEvent routes one comms bus payload. A MessagePosted is the delivery
// trigger; a MessageUpdated grows an agent-authored message's block set but is
// NOT itself a trigger (the settle edge fires the held deliver, re-reading the
// then-current blocks); every other variant is ignored.
func (c *Consumer) handleEvent(ctx context.Context, resp *compassv1.SubscribeCommsResponse) {
	posted := resp.GetMessagePosted()
	if posted == nil {
		return
	}
	c.onMessagePosted(ctx, posted.GetMessage())
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
