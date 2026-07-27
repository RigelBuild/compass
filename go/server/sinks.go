//go:build unix

// The server-side write-through sinks the RunnerHub delivers relayed agent
// events into, and the hub constructor that wires them. Deliver classifies each
// relayed frame by its set oneof and hands it to the surface that owns it
// (runnerhub/hub.go): a lifecycle transition to the board, a conversation frame
// to comms, a session-trace frame to the observation pane.
//
// T4 wires the lifecycle sink for real — an agent-session state transition fans
// onto SubscribeEvents, the board/liveness surface the T4 acceptance path tails
// (provision a workspace, observe lifecycle on SubscribeEvents). The conversation
// and session-tail sinks are minimal in T4 by the frozen design
// (compass-0.6 §T4/§T5): the agent that emits conversation and trace frames is
// the first-party agent built in T5, and the conversation write-through
// additionally needs a session→channel mapping that lands with T5's store
// schema. Until then no agent produces those frames on this path; a frame that
// nonetheless arrives is observed (logged) rather than dropped, and T5
// substitutes the real comms write-through and the SubscribeAgentSession stream
// behind the same interfaces without touching the hub.
package server

import (
	"context"
	"log/slog"

	compassv1 "github.com/sealedsecurity/compass/go/gen/compass/v1"
	"github.com/sealedsecurity/compass/go/internal/board"
	"github.com/sealedsecurity/compass/go/internal/runnerhub"
)

// The lifecycle sink is the Bridge board (internal/board): a session lifecycle
// transition is recorded into the board's per-session projection AND fanned onto
// SubscribeEvents, so GetAgentStatus (the snapshot) can never disagree with what
// the live stream carried. The board is a strict superset of a bus-only sink, so
// it is the one lifecycle sink — see newRunnerHub.

// observedConversationSink is the T4-minimal ConversationSink. The real
// write-through commits an agent conversation frame to comms Message rows +
// SubscribeComms; it needs the session→channel mapping T5's store adds and a
// first-party agent (T5) to emit the frames, so no agent produces conversation
// frames on this path in T4. A frame that nonetheless arrives is logged (never
// silently dropped) so a contract skew is observable; T5 replaces this with the
// comms write-through behind the same interface.
type observedConversationSink struct {
	log *slog.Logger
}

func (s observedConversationSink) PostAgentMessage(_ context.Context, sessionID string, _ *compassv1.MessagePosted, updated *compassv1.MessageUpdated) error {
	kind := "posted"
	if updated != nil {
		kind = "updated"
	}
	s.log.Debug("conversation frame relayed before the T5 comms write-through is wired; observed, not committed",
		slog.String("session_id", sessionID), slog.String("kind", kind))
	return nil
}

// newRunnerHub's session-tail sink is the real per-session fan-out (sessionTail,
// sessiontail.go): RelaySessionFrame repackages each internal frame to its
// public form and fans it to that session's live SubscribeAgentSession
// subscribers. The same *sessionTail instance is shared with the
// service (serve.go), which subscribes to it in the SubscribeAgentSession
// handler — hub is the writer, service the reader, one instance.

// newRunnerHub constructs the Server-side RunnerHub over its write-through sinks
// and the agent-comms caller: the Bridge board as the lifecycle sink (a session
// transition is recorded into the board projection and fanned onto
// SubscribeEvents), the minimal conversation sink (see the file doc), the real
// per-session tail sink for SubscribeAgentSession passed in by serve.go so the
// same instance is shared with the service, and comms — the CommsService handler,
// which executes an agent-initiated comms call under the account a session
// resolves to (RelayCommsCall). log carries the sinks' observed-frame diagnostics
// and the hub's gap/unknown-frame warnings; nil falls back to slog.Default().
func newRunnerHub(brd *board.Projection, tail runnerhub.SessionTailSink, comms runnerhub.CommsCaller, log *slog.Logger) *runnerhub.Hub {
	if log == nil {
		log = slog.Default()
	}
	return runnerhub.NewHub(
		observedConversationSink{log: log},
		brd,
		tail,
		comms,
		log,
	)
}
