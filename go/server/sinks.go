//go:build unix

// The server-side write-through sinks the RunnerHub delivers relayed agent events
// into, and the hub constructor that wires them. Deliver classifies each frame by its
// oneof: a lifecycle transition to the board, a session-trace frame to the pane.
// Agent-initiated comms calls run directly against the CommsService handler, not here.
package server

import (
	"context"
	"log/slog"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/RigelBuild/compass/go/events"
	compassv1 "github.com/RigelBuild/compass/go/gen/compass/v1"
	"github.com/RigelBuild/compass/go/internal/board"
	"github.com/RigelBuild/compass/go/internal/comms"
	"github.com/RigelBuild/compass/go/internal/delivery"
	"github.com/RigelBuild/compass/go/internal/fabric"
	"github.com/RigelBuild/compass/go/internal/presence"
	"github.com/RigelBuild/compass/go/internal/runnerhub"
	"github.com/RigelBuild/compass/go/internal/store"
	"github.com/RigelBuild/compass/go/internal/usage"
)

// The Bridge board is the lifecycle sink: it records per-session state and
// publishes events, keeping GetAgentStatus consistent with SubscribeEvents.

// The sessionTail sink repackages Runner frames for live SubscribeAgentSession
// subscribers. RunnerHub writes it; the service reads the same instance.

// newRunnerHub constructs the server RunnerHub over its write-through sinks.
// It stores durable transcripts; a nil logger uses slog.Default().
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
	hub.SetSessionBlobStore(st)
	// The Runner binds a resumed lifetime's rebase base through the same store.
	hub.SetLifetimeBinder(st)
	// The durable store backs session bindings; without a RoutingFabric, hub
	// writes keep its in-memory cache current on this single-server path.
	hub.SetSessionBindingStore(st)
	// The same store reads archived transcript segments for resume reconstruction.
	hub.SetTranscriptReader(st)
	hub.SetSessionEndSink(sessionEndArchiver{st: st})
	return hub
}

// lifecycleService satisfies the delivery.AgentWaker seam; the assertion lives
// here so delivery does not import the server package.
var _ delivery.AgentWaker = (*lifecycleService)(nil)

// forgeService satisfies runnerhub.ForgeCaller; the assertion lives here because
// this package imports both the service and hub interface.
var _ runnerhub.ForgeCaller = (*forgeService)(nil)

// wireHubServiceCycles installs callers after hub construction and before RPCs.
func wireHubServiceCycles(hub *runnerhub.Hub, commsSvc *comms.Comms, st *store.Store, issueBrd *board.IssueProjection) {
	hub.SetLifecycleCaller(newLifecycleService(st, hub, commsSvc))
	hub.SetBoardCaller(newBoardService(st, issueBrd))
	// Connect the roster read to the hub here; the publisher supplies it before
	// any RPC is served.
	commsSvc.SetPresenceSource(hubPresenceSource{hub})
}

// hubPresenceSource adapts *runnerhub.Hub to the comms-defined PresenceSource: the
// hub returns the enum wrapped in a runnerhub.PresenceSnapshot while comms consumes
// the bare enum. It lives here in the server package (which imports both), so
// neither comms nor runnerhub depends on the other.
type hubPresenceSource struct{ hub *runnerhub.Hub }

func (h hubPresenceSource) PresenceFor(accountIDs []store.AccountID) map[store.AccountID]compassv1.AgentPresence {
	snaps := h.hub.PresenceFor(accountIDs)
	out := make(map[store.AccountID]compassv1.AgentPresence, len(snaps))
	for id, snap := range snaps {
		out[id] = snap.Presence
	}
	return out
}

// startDeliveryConsumer builds the fan-out consumer over the event fabric, wires
// the consumer<->hub construction cycle (consumer takes hub as ControlDispatcher/
// SessionResolver; hub takes consumer as SettleSink and SessionStartSink), and
// starts its Run on gctx, which ends at shutdown.
func startDeliveryConsumer(gctx context.Context, g *errgroup.Group, fab fabric.EventFabric, st *store.Store, hub *runnerhub.Hub, log *slog.Logger) {
	c := delivery.NewConsumer(gctx, st, hub, hub, fab, log)
	// The wake seam (RIG-1641 T3): a FRESH lifecycleService, not the instance
	// wireHubServiceCycles wired. lifecycleService is stateless besides its own
	// singleflight group, and only the waker path drives WakeAgent, so this
	// instance's group IS the wake's coalescer. nil dmOpener: the waker never spawns.
	c.SetAgentWaker(newLifecycleService(st, hub, nil))
	hub.SetSettleSink(c)
	hub.SetSessionStartSink(c)
	hub.SetSessionReapSink(c)
	hub.SetSessionLostSink(c)
	hub.SetRunnerReadySink(c)
	hub.SetDeliveryStore(st)
	g.Go(func() error { return c.Run(gctx) })
}

// startPresencePublisher wires the bus-backed presence projection to the hub
// and runs its tail in the serve group so shutdown cancels it.
func startPresencePublisher(gctx context.Context, g *errgroup.Group, commsBus *events.Bus[*compassv1.SubscribeCommsResponse], st *store.Store, hub *runnerhub.Hub, log *slog.Logger) {
	p := presence.NewPublisher(commsBus, st, hub, log)
	hub.SetPresenceSink(p)
	// The roster read source (RIG-1721 T2): the hub reads the enum snapshot and
	// fires the set_status activity publish through the same projection it feeds.
	hub.SetPresenceSource(p)
	g.Go(func() error { return p.Run(gctx) })
}

// startCommsConsumers starts both comms consumers: delivery fan-out on the event
// fabric and the presence projection on the comms bus. They share the store,
// hub, serve group, and gctx, so Serve starts them in one statement.
func startCommsConsumers(gctx context.Context, g *errgroup.Group, commsBus *events.Bus[*compassv1.SubscribeCommsResponse], fab fabric.EventFabric, st *store.Store, hub *runnerhub.Hub, log *slog.Logger) {
	startDeliveryConsumer(gctx, g, fab, st, hub, log)
	startPresencePublisher(gctx, g, commsBus, st, hub, log)
}

// startForgeIngestLanes starts each non-nil lane's arm+reconciler on the serve group; the token
// broker shares the board lane's App gate; Linear notify lanes gate independently.
func startForgeIngestLanes(gctx context.Context, g *errgroup.Group, board *boardIngestLane, gitCredentials *gitCredentialBroker, notify ...*forgeNotifyLane) {
	if board != nil {
		g.Go(func() error { return board.arm.Run(gctx) })
		g.Go(func() error { return board.reconciler.Run(gctx) })
	}
	startGitCredentialRefresh(gctx, g, gitCredentials)
	for _, lane := range notify {
		if lane == nil {
			continue
		}
		g.Go(func() error { return lane.arm.Run(gctx) })
		g.Go(func() error { return lane.reconciler.Run(gctx) })
	}
}

// startUsageSweepers starts the token- and compute-usage retention prune and the
// orphaned compute interval close; binding changes close intervals inline.
func startUsageSweepers(gctx context.Context, g *errgroup.Group, st *store.Store, retention time.Duration, log *slog.Logger) {
	retain := usage.NewRetentionSweeper(usage.NewPostgres(st), usage.RetentionConfig{Retention: retention, Log: log})
	closer := usage.NewComputeUsageSweeper(computeUsageCloser{st: st}, log)
	g.Go(func() error { return retain.Run(gctx) })
	g.Go(func() error { return closer.Run(gctx) })
}

type computeUsageCloser struct{ st *store.Store }

func (c computeUsageCloser) CloseOrphanedComputeIntervals(ctx context.Context) (int64, error) {
	return c.st.CloseOrphanedComputeIntervals(store.WithSystemRole(ctx))
}

// logFrameDiagnostics reports frame-loss counters on shutdown, distinguishing
// an empty relay commit from one that delivered everything. Remaining loss
// signals are recoverable; Info is sufficient because the server has no metrics
// surface.
func logFrameDiagnostics(ctx context.Context, log *slog.Logger, hub *runnerhub.Hub) {
	d := hub.FrameDiagnostics()
	log.InfoContext(ctx, "relayed agent frame accounting",
		slog.Uint64("unknown_frames", d.UnknownFrames),
		slog.Bool("seen_sequence_gap", d.SeenGap),
		slog.Uint64("dropped_acks", d.DroppedAcks),
	)
}
