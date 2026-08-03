//go:build unix

// The server-side write-through sinks the RunnerHub delivers relayed agent
// events into, and the hub constructor that wires them. Deliver classifies each
// relayed frame by its set oneof and hands it to the surface that owns it
// (runnerhub/hub.go): a lifecycle transition to the board, a conversation frame
// to comms, a session-trace frame to the observation pane.
//
// All three sinks are real. The lifecycle sink is the Bridge board — an
// agent-session state transition fans onto SubscribeEvents, the board/liveness
// surface. The conversation sink is the comms write-through (commsConversationSink
// below), committing a relayed conversation frame to durable Message rows +
// SubscribeComms. The session-tail sink is the per-session fan-out backing
// SubscribeAgentSession.
package server

import (
	"context"
	"errors"
	"log/slog"

	"connectrpc.com/connect"

	"golang.org/x/sync/errgroup"

	"github.com/sealedsecurity/compass/go/events"
	compassv1 "github.com/sealedsecurity/compass/go/gen/compass/v1"
	"github.com/sealedsecurity/compass/go/internal/board"
	"github.com/sealedsecurity/compass/go/internal/comms"
	"github.com/sealedsecurity/compass/go/internal/delivery"
	"github.com/sealedsecurity/compass/go/internal/presence"
	"github.com/sealedsecurity/compass/go/internal/runnerhub"
	"github.com/sealedsecurity/compass/go/internal/store"
)

// The lifecycle sink is the Bridge board (internal/board): a session lifecycle
// transition is recorded into the board's per-session projection AND fanned onto
// SubscribeEvents, so GetAgentStatus (the snapshot) can never disagree with what
// the live stream carried. The board is a strict superset of a bus-only sink, so
// it is the one lifecycle sink — see newRunnerHub.

// commsConversationSink is the real conversation write-through: a relayed
// conversation frame becomes a durable comms Message row and fans out on
// SubscribeComms, so an agent's turn is indistinguishable downstream from a
// human's post (SEA-1364 T3).
//
// It is a thin adapter, deliberately. All the work lives in the CommsService
// handler's *AsAccount family (internal/comms/agent_caller.go): a post delegates
// to the same PostMessage path a human takes, and an update goes through the
// authorizing store update that requires channel membership AND authorship. So
// there is no server-package authz to drift from the comms package's — this type
// only maps the sink interface onto those two calls.
//
// The account arrives already resolved: the hub owns the session->account
// binding and resolves it at the Deliver site, so this sink never looks a
// session up (runnerhub/hub.go, ConversationSink).
type commsConversationSink struct {
	comms *comms.Comms
}

// PostAgentMessage commits one relayed conversation frame under account.
//
// ERROR VOCABULARY. The hub classifies what this returns by its CONNECT CODE
// alone (runnerhub/hub.go, ConversationSink) — NotFound/InvalidArgument drop the
// frame as a refusal, FailedPrecondition drops it as a contract defect, anything
// else ends the Runner's relay stream. This method satisfies that contract by
// construction: both delegates return errors already mapped through the comms
// package's edgeError, and the one error minted here
// (errNoConversationVariant) picks its code deliberately. Never return a bare
// error from here — connect.CodeOf reports CodeUnknown for one, which tears the
// stream down.
//
// A frame with neither variant set cannot happen: Deliver dispatches on the
// oneof and only ever passes exactly one. The unset case is therefore a guard,
// returning an error rather than panicking or reporting a silent success
// (rule://go-no-panic-in-lib).
func (s commsConversationSink) PostAgentMessage(
	ctx context.Context,
	account store.AccountID,
	_ string,
	idempotencyKey string,
	posted *compassv1.MessagePosted,
	updated *compassv1.MessageUpdated,
) error {
	switch {
	case posted != nil:
		_, err := s.comms.CommitAgentPostKeyed(ctx, account, posted, idempotencyKey)
		return err
	case updated != nil:
		_, err := s.comms.CommitAgentUpdateKeyed(ctx, account, updated, idempotencyKey)
		return err
	default:
		return errNoConversationVariant
	}
}

// errNoConversationVariant is the cause for a conversation write-through called
// with neither variant set. Unreachable through Deliver's dispatch, so it can
// only fire on a SERVER-SIDE wiring defect — the frame is well-formed and the
// dispatch is what broke.
//
// CodeInternal, so the hub ends the relay stream. It was CodeInvalidArgument,
// which classified a Server bug as a routine droppable frame: the Server would
// have silently discarded relayed turns because of its own defect, hidden among
// the refusals a healthy relay is expected to produce. An unreachable branch
// firing is exactly the case worth a loud teardown. It is deliberately not
// CodeFailedPrecondition either — that bucket is for a skew the relay keeps
// serving through, and this one must not be served through.
var errNoConversationVariant = connect.NewError(
	connect.CodeInternal,
	errors.New("server: conversation frame has neither a posted nor an updated variant"),
)

// newRunnerHub's session-tail sink is the real per-session fan-out (sessionTail,
// sessiontail.go): RelaySessionFrame repackages each internal frame to its
// public form and fans it to that session's live SubscribeAgentSession
// subscribers. The same *sessionTail instance is shared with the
// service (serve.go), which subscribes to it in the SubscribeAgentSession
// handler — hub is the writer, service the reader, one instance.

// newRunnerHub constructs the Server-side RunnerHub over its write-through sinks
// and the agent-comms caller: the Bridge board as the lifecycle sink (a session
// transition is recorded into the board projection and fanned onto
// SubscribeEvents), the comms write-through as the conversation sink (a relayed
// conversation frame becomes a durable Message row + SubscribeComms), the real
// per-session tail sink for SubscribeAgentSession passed in by serve.go so the
// same instance is shared with the service, and comms — the CommsService handler,
// which executes an agent-initiated comms call under the account a session
// resolves to (RelayCommsCall). The one CommsService instance serves both comms
// legs: the conversation write-through and RelayCommsCall. log carries the hub's
// gap/unknown/refused-frame diagnostics; nil falls back to slog.Default(). st is
// the store of record: the hub's durable transcript lane (SEA-1667 T4)
// write-throughs a relayed transcript_entry to it via SetTranscriptStore, wired
// here so the one store instance backs the transcript commit path.
func newRunnerHub(st *store.Store, brd *board.Projection, tail runnerhub.SessionTailSink, commsSvc *comms.Comms, log *slog.Logger) *runnerhub.Hub {
	if log == nil {
		log = slog.Default()
	}
	hub := runnerhub.NewHub(
		commsConversationSink{comms: commsSvc},
		brd,
		tail,
		commsSvc,
		log,
	)
	hub.SetTranscriptStore(st)
	return hub
}

// startDeliveryConsumer builds the SEA-1569 T3 fan-out consumer over the comms
// bus, wires the consumer<->hub construction cycle (the consumer takes hub as
// its ControlDispatcher + SessionResolver; the hub takes the consumer as its
// SettleSink AND its SessionStartSink — the reconnect sweep edge (SEA-1569 T6) —
// with st as its delivery-cursor store, the post-construction
// setters that break the cycle), and starts its bus-tail goroutine on the serve
// group rooted on gctx (so it cancels at shutdown; it also ends when the comms
// bus closes in drainDoors, so shutdown reaches it two ways).
func startDeliveryConsumer(gctx context.Context, g *errgroup.Group, commsBus *events.Bus[*compassv1.SubscribeCommsResponse], st *store.Store, hub *runnerhub.Hub, log *slog.Logger) {
	c := delivery.NewConsumer(commsBus, st, hub, hub, log)
	hub.SetSettleSink(c)
	hub.SetSessionStartSink(c)
	hub.SetDeliveryStore(st)
	g.Go(func() error { return c.Run(gctx) })
}

// startPresencePublisher builds the SEA-1569 T8 presence projection over the
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
	g.Go(func() error { return p.Run(gctx) })
}

// startCommsBusConsumers starts both comms-bus consumers (SEA-1569): the T3
// delivery fan-out consumer and the T8 presence projection. Serve calls this one
// helper so the two starts, which share the same construction inputs (comms bus,
// store, hub, serve group, gctx), stay one statement at the call site.
func startCommsBusConsumers(gctx context.Context, g *errgroup.Group, commsBus *events.Bus[*compassv1.SubscribeCommsResponse], st *store.Store, hub *runnerhub.Hub, log *slog.Logger) {
	startDeliveryConsumer(gctx, g, commsBus, st, hub, log)
	startPresencePublisher(gctx, g, commsBus, st, hub, log)
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
// A clean run logs at Info and reads as four zeros. Any contract defect flips it
// to Error and says the relay is misconfigured, because on the current base a
// non-zero defect count means every agent turn was lost (runnerhub/hub.go,
// ContractDefects). This is the minimum that makes the counters observable — the
// server has no metrics surface to register a gauge on, and inventing one is not
// this change's job. A future metrics or diagnostics RPC reads the same
// FrameDiagnostics snapshot.
func logFrameDiagnostics(ctx context.Context, log *slog.Logger, hub *runnerhub.Hub) {
	d := hub.FrameDiagnostics()
	attrs := []any{
		slog.Uint64("contract_defects", d.ContractDefects),
		slog.Uint64("refused_frames", d.RefusedFrames),
		slog.Uint64("unknown_frames", d.UnknownFrames),
		slog.Bool("seen_sequence_gap", d.SeenGap),
	}
	if d.ContractDefects > 0 {
		log.ErrorContext(ctx, "relayed agent frames were dropped for relay misconfiguration: agent conversation was NOT committed to comms", attrs...)
		return
	}
	log.InfoContext(ctx, "relayed agent frame accounting", attrs...)
}
