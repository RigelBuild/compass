//go:build unix

// The server-side write-through sinks the RunnerHub delivers relayed agent
// events into, and the hub constructor that wires them. Deliver classifies each
// relayed frame by its set oneof and hands it to the surface that owns it
// (runnerhub/hub.go): a lifecycle transition to the board and a session-trace
// frame to the observation pane.
//
// Both sinks are real. The lifecycle sink is the Bridge board — an
// agent-session state transition fans onto SubscribeEvents, the board/liveness
// surface. The session-tail sink is the per-session fan-out backing
// SubscribeAgentSession. Agent-initiated comms calls no longer write through a
// sink: the hub executes them directly against the CommsService handler
// (RelayCommsCall), so there is no conversation write-through sink here.
package server

import (
	"context"
	"log/slog"

	"golang.org/x/sync/errgroup"

	"github.com/RigelBuild/compass/go/events"
	compassv1 "github.com/RigelBuild/compass/go/gen/compass/v1"
	"github.com/RigelBuild/compass/go/internal/board"
	"github.com/RigelBuild/compass/go/internal/comms"
	"github.com/RigelBuild/compass/go/internal/delivery"
	"github.com/RigelBuild/compass/go/internal/presence"
	"github.com/RigelBuild/compass/go/internal/runnerhub"
	"github.com/RigelBuild/compass/go/internal/store"
)

// The lifecycle sink is the Bridge board (internal/board): a session lifecycle
// transition is recorded into the board's per-session projection AND fanned onto
// SubscribeEvents, so GetAgentStatus (the snapshot) can never disagree with what
// the live stream carried. The board is a strict superset of a bus-only sink, so
// it is the one lifecycle sink — see newRunnerHub.

// newRunnerHub's session-tail sink is the real per-session fan-out (sessionTail,
// sessiontail.go): RelaySessionFrame repackages each internal frame to its
// public form and fans it to that session's live SubscribeAgentSession
// subscribers. The same *sessionTail instance is shared with the
// service (serve.go), which subscribes to it in the SubscribeAgentSession
// handler — hub is the writer, service the reader, one instance.

// newRunnerHub constructs the Server-side RunnerHub over its write-through sinks
// and the agent-comms caller: the Bridge board as the lifecycle sink (a session
// transition is recorded into the board projection and fanned onto
// SubscribeEvents), the real per-session tail sink for SubscribeAgentSession
// passed in by serve.go so the same instance is shared with the service, and
// comms — the CommsService handler, which executes an agent-initiated comms call
// under the account a session resolves to (RelayCommsCall). log carries the
// hub's gap/unknown-frame diagnostics; nil falls back to slog.Default(). st is
// the store of record: the hub's durable transcript lane (RIG-1667 T4)
// write-throughs a relayed transcript_entry to it via SetTranscriptStore, wired
// here so the one store instance backs the transcript commit path.
func newRunnerHub(st *store.Store, brd *board.Projection, tail runnerhub.SessionTailSink, commsSvc *comms.Comms, log *slog.Logger) *runnerhub.Hub {
	if log == nil {
		log = slog.Default()
	}
	hub := runnerhub.NewHub(
		brd,
		tail,
		commsSvc,
		log,
	)
	hub.SetTranscriptStore(st)
	// RIG-1667 T5: the same store backs the resume-body reconstructor's read
	// seam (SessionResumeSnapshot + ReadArchiveSegment), wired here beside the
	// write seam so the one store instance serves both legs.
	hub.SetTranscriptReader(st)
	return hub
}

// RIG-1641 T3: *lifecycleService satisfies the delivery-defined AgentWaker
// (delivery defines the narrow interface it needs; the server package implements
// it over the resume machinery). The assertion lives here in the server package,
// which imports both — so delivery never imports server.
var _ delivery.AgentWaker = (*lifecycleService)(nil)

// Compass forge write path T8: *forgeService (forge.go, the DL-050 write
// chokepoint) satisfies the T5-defined runnerhub.ForgeCaller seam the hub relays
// RelayForgeCall into. The assertion lives here in the server package — which
// imports both forgeService (unexported, same package) and runnerhub — the same
// direction the sibling assertion above is proven; T4 could not
// place it because the runnerhub interface was not importable in its isolated
// slice.
var _ runnerhub.ForgeCaller = (*forgeService)(nil)

// wireHubServiceCycles breaks the post-construction cycles between the hub and
// the account-facing services that are built before it (the hub relays through
// them, so they cannot take the hub at construction): the hub<->lifecycle cycle
// (RIG-1618 T5, RelayLifecycleCall) and the hub<->board cycle (agent primary
// lifecycle T3-a, RelayBoardCall — the board caller executes against the store +
// the issue projection). Called once at assembly before any RPC is served.
func wireHubServiceCycles(hub *runnerhub.Hub, commsSvc *comms.Comms, st *store.Store, issueBrd *board.IssueProjection) {
	hub.SetLifecycleCaller(newLifecycleService(st, hub))
	hub.SetBoardCaller(newBoardService(st, issueBrd))
	// The roster read (RIG-1721 T2) joins the hub's in-memory presence enum; the
	// hub in turn reads it from the T8 presence projection wired at
	// startPresencePublisher (hub.SetPresenceSource). comms->hub is set here (the
	// hub is stable and delegates lazily), hub->publisher when the publisher
	// starts — both before any RPC is served.
	commsSvc.SetPresenceSource(hubPresenceSource{hub})
}

// hubPresenceSource adapts *runnerhub.Hub to the comms-defined PresenceSource:
// the hub returns the enum wrapped in a runnerhub.PresenceSnapshot (leaving room
// for a later live-only attribute), while comms consumes the bare enum. The
// projection lives here in the server package, which imports both — the same
// direction the ForgeCaller assertion above is proven, so neither comms nor
// runnerhub depends on the other.
type hubPresenceSource struct{ hub *runnerhub.Hub }

func (h hubPresenceSource) PresenceFor(accountIDs []store.AccountID) map[store.AccountID]compassv1.AgentPresence {
	snaps := h.hub.PresenceFor(accountIDs)
	out := make(map[store.AccountID]compassv1.AgentPresence, len(snaps))
	for id, snap := range snaps {
		out[id] = snap.Presence
	}
	return out
}

// startDeliveryConsumer builds the RIG-1569 T3 fan-out consumer over the comms
// bus, wires the consumer<->hub construction cycle (the consumer takes hub as
// its ControlDispatcher + SessionResolver; the hub takes the consumer as its
// SettleSink AND its SessionStartSink — the reconnect sweep edge (RIG-1569 T6) —
// with st as its delivery-cursor store, the post-construction
// setters that break the cycle), and starts its bus-tail goroutine on the serve
// group rooted on gctx (so it cancels at shutdown; it also ends when the comms
// bus closes in drainDoors, so shutdown reaches it two ways).
func startDeliveryConsumer(gctx context.Context, g *errgroup.Group, commsBus *events.Bus[*compassv1.SubscribeCommsResponse], st *store.Store, hub *runnerhub.Hub, log *slog.Logger) {
	c := delivery.NewConsumer(commsBus, st, hub, hub, log)
	// The wake seam (RIG-1641 T3): a FRESH lifecycleService, not the instance
	// wireHubServiceCycles wired as the hub's LifecycleCaller. lifecycleService is
	// stateless besides its own singleflight group, and only the waker path drives
	// WakeAgent, so a second instance's group IS the wake's coalescer — sharing
	// the LifecycleCaller instance would buy nothing and couple two unrelated call
	// sites. Same (st, hub) inputs, so it runs the identical resume/start chain.
	c.SetAgentWaker(newLifecycleService(st, hub))
	hub.SetSettleSink(c)
	hub.SetSessionStartSink(c)
	hub.SetSessionReapSink(c)
	hub.SetDeliveryStore(st)
	g.Go(func() error { return c.Run(gctx) })
}

// startPresencePublisher builds the RIG-1569 T8 presence projection over the
// comms bus (it both tails and publishes onto it) + the store's open-ask read
// surface + the hub's Status relay for reconciliation, wires the
// component<->hub construction cycle (the hub takes the component as its
// PresenceSink; the component takes the hub as its Status relay — the
// post-construction setter that breaks the cycle), and starts its bus-tail
// goroutine on the serve group rooted on gctx (so it cancels at shutdown; it
// also ends when the comms bus closes in drainDoors, so shutdown reaches it two
// ways). Mirrors startDeliveryConsumer verbatim in shape.
func startPresencePublisher(gctx context.Context, g *errgroup.Group, commsBus *events.Bus[*compassv1.SubscribeCommsResponse], st *store.Store, hub *runnerhub.Hub, log *slog.Logger) {
	p := presence.NewPublisher(commsBus, st, hub, log)
	hub.SetPresenceSink(p)
	// The roster read source (RIG-1721 T2): the hub reads the enum snapshot and
	// fires the set_status activity publish through the same projection it feeds.
	hub.SetPresenceSource(p)
	g.Go(func() error { return p.Run(gctx) })
}

// startCommsBusConsumers starts both comms-bus consumers (RIG-1569): the T3
// delivery fan-out consumer and the T8 presence projection. Serve calls this one
// helper so the two starts, which share the same construction inputs (comms bus,
// store, hub, serve group, gctx), stay one statement at the call site.
func startCommsBusConsumers(gctx context.Context, g *errgroup.Group, commsBus *events.Bus[*compassv1.SubscribeCommsResponse], st *store.Store, hub *runnerhub.Hub, log *slog.Logger) {
	startDeliveryConsumer(gctx, g, commsBus, st, hub, log)
	startPresencePublisher(gctx, g, commsBus, st, hub, log)
}

// startForgeIngestLanes starts the forge webhook-ingestion lanes' background
// goroutines on the serve group: the board lane (RIG-2883) and the agent-
// notification lane (RIG-2732 T7), each contributing its webhook-arm drain and
// its reconciler sweep. Both share the App gate, so each lane is nil-or-set as a
// unit; a nil lane starts nothing. Serve calls this one helper so the four
// Run starts, which share the serve group + gctx, stay one statement at the call
// site (mirroring startCommsBusConsumers). Both Runs return nil on ctx-cancel.
func startForgeIngestLanes(gctx context.Context, g *errgroup.Group, board *boardIngestLane, notify *forgeNotifyLane) {
	if board != nil {
		g.Go(func() error { return board.arm.Run(gctx) })
		g.Go(func() error { return board.reconciler.Run(gctx) })
	}
	if notify != nil {
		g.Go(func() error { return notify.arm.Run(gctx) })
		g.Go(func() error { return notify.reconciler.Run(gctx) })
	}
}

// logFrameDiagnostics emits the hub's frame-loss snapshot as one line. Serve
// calls it on shutdown, so every run states plainly how many relayed frames
// never reached their surface.
//
// It exists because the counters had no non-test reader at all. Total frame loss
// was observable only by reading per-frame warn lines and tallying them by hand
// — which means, in practice, that a relay committing NOTHING looked exactly
// like a relay committing everything to anyone not already suspicious. That is
// the failure mode this whole classification split exists to expose, so leaving
// the numbers unreadable would have undone it.
//
// It logs at Info: the three surviving loss signals (unknown_frames,
// seen_sequence_gap, dropped_acks) are all recoverable — an unknown frame
// reached no sink, a sequence gap the Client bus resync recovers, a dropped ack
// costs only a redundant redeliver on the recipient's next reconnect sweep. This
// is the minimum that makes the counters observable — the server has no metrics
// surface to register a gauge on, and inventing one is not this change's job. A
// future metrics or diagnostics RPC reads the same FrameDiagnostics snapshot.
func logFrameDiagnostics(ctx context.Context, log *slog.Logger, hub *runnerhub.Hub) {
	d := hub.FrameDiagnostics()
	log.InfoContext(ctx, "relayed agent frame accounting",
		slog.Uint64("unknown_frames", d.UnknownFrames),
		slog.Bool("seen_sequence_gap", d.SeenGap),
		slog.Uint64("dropped_acks", d.DroppedAcks),
	)
}
