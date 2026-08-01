//go:build unix

// Package delivery is the Server-side notification fan-out consumer (SEA-1569
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
	"sync"

	comms "github.com/sealedsecurity/compass/go/internal/comms"

	"github.com/sealedsecurity/compass/go/events"
	compassv1 "github.com/sealedsecurity/compass/go/gen/compass/v1"
	compassv1internal "github.com/sealedsecurity/compass/go/internal/gen/compass/v1"
	"github.com/sealedsecurity/compass/go/internal/store"
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
// from ControlDispatcher so that stays the frozen dispatch-only shape.
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
// the author agent/human split, the settled-message re-read, and the sweep.
// *store.Store implements it.
type DeliveryReads interface {
	SubscribedAgents(ctx context.Context, channel store.ChannelID, author store.AccountID) ([]store.AccountID, error)
	IsAgentAccount(ctx context.Context, account store.AccountID) (bool, error)
	MessageByID(ctx context.Context, messageID string) (store.Message, error)
	UndeliveredMessages(ctx context.Context, agent store.AccountID) (map[store.ChannelID][]store.Message, error)
}

// settleEvent is one queued author-settle edge handed from the hub's Deliver
// goroutine (OnSessionSettled) to the consumer's ctx-rooted loop.
type settleEvent struct {
	sessionID string
	state     compassv1.AgentSessionState
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

	mu sync.Mutex
	// held is the pending-deliver registry (design.md:157-168), keyed by the
	// AUTHOR's live session id: an agent-authored message posted while its author
	// still streams is HELD here until that author's session settles
	// (WORKING->READY) or reaches a terminal frame. The value is the ordered set
	// of message ids held for that author, in post order, so a settle fires them
	// ascending.
	held map[string][]string
	// settleQueue buffers author-settle edges the hook enqueues, drained by the
	// loop under its ctx. A slice (never lost) plus a buffered notify channel
	// (coalescing wakeups): the hook appends and signals without blocking Deliver.
	settleQueue []settleEvent
	// notify wakes the loop when settleQueue grows. Buffered(1) with a
	// non-blocking send, so many settles between drains collapse to one wakeup and
	// the hook never blocks.
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
	return &Consumer{
		bus:      bus,
		st:       st,
		dispatch: dispatch,
		resolver: resolver,
		log:      log,
		held:     make(map[string][]string),
		notify:   make(chan struct{}, 1),
		gates:    make(map[string]*sync.Mutex),
	}
}

// Run tails the comms bus and dispatches until ctx is cancelled (serve shutdown)
// or the bus closes. It mirrors comms.forwardComms: drain the replay snapshot
// oldest-first, then select on ctx, the live tail, and the settle queue. On the
// live channel closing, sub.Lagged() distinguishes an overrun — run the D2
// recipient sweep, not a loss (design.md:227-231) — from a clean bus shutdown
// (end silently). ctx threads from the serve group into every store read and
// dispatch below; the loop never re-roots it.
func (c *Consumer) Run(ctx context.Context) error {
	sub, err := c.bus.Subscribe(0, c.bus.InstanceEpoch())
	if err != nil {
		// A fresh subscription at since_seq=0 on a live bus cannot underflow; any
		// error here is a genuine subscribe fault the caller should see.
		return err
	}
	defer sub.Cancel()

	for _, event := range sub.Replay {
		select {
		case <-ctx.Done():
			return nil
		default:
		}
		c.handleEvent(ctx, event.Payload)
	}

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-c.notify:
			c.drainSettles(ctx)
		case event, ok := <-sub.Live:
			if !ok {
				if sub.Lagged() {
					// Overrun: bus events were dropped. Not a loss — the cursor
					// defines exactly what is undelivered, so sweep every live
					// recipient rather than re-snapshotting (design.md:227-231).
					c.sweepAllLive(ctx)
				}
				return nil
			}
			c.handleEvent(ctx, event.Payload)
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
// (§5 command envelope; agent.proto deliver = 3).
func deliverOp(msg *compassv1.Message) *compassv1internal.AgentControl {
	return &compassv1internal.AgentControl{
		Control: &compassv1internal.AgentControl_Deliver{
			Deliver: &compassv1internal.DeliverControl{Message: msg},
		},
	}
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

// storeMessageToWire re-reads a message from the store and maps it to the wire
// shape via the ONE store->wire mapper (comms.MessageToWire) — the settled-block
// re-read the settle gate and the no-live-author / sweep paths dispatch from
// (design.md:158-161), never a stale in-memory copy.
func (c *Consumer) storeMessageToWire(ctx context.Context, messageID string) (*compassv1.Message, store.Message, error) {
	m, err := c.st.MessageByID(ctx, messageID)
	if err != nil {
		return nil, store.Message{}, err
	}
	return comms.MessageToWire(m), m, nil
}
