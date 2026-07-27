//go:build unix

// Package runnerhub is the Server side of the Server<->Runner seam (design
// compass-0.6 §T4). It owns the enrollment registry (which Runner is attached
// and under what token subject), the session-command router (a client-facing
// session RPC → the owning Runner's Sessions stream, request-id correlated),
// and Deliver — the sole entry point relayed Runner events take into the Server,
// write-through to the surfaces that own them.
//
// Deliver is fed by the RunnerService PublishEvents handler (serve.go mounts the
// handler; the handler calls Deliver per frame). Keeping Deliver the one seam
// means a future brokered transport replaces only what feeds it — the write-
// through, the registry, and the router are transport-agnostic (design.md:
// 1347-1352).
package runnerhub

import (
	"context"
	"fmt"
	"log/slog"
	"sync"

	compassv1 "github.com/sealedsecurity/compass/go/gen/compass/v1"
	compassv1internal "github.com/sealedsecurity/compass/go/internal/gen/compass/v1"
	"github.com/sealedsecurity/compass/go/internal/store"
)

// RunnerEvent is one relayed agent event as it enters the Server: the frame the
// Runner published, plus the Runner-assigned sequence and the session it belongs
// to. It is the domain form of a PublishEvents frame, decoupling Deliver's
// write-through from the wire message so the seam is the only place the two meet.
type RunnerEvent struct {
	// RunnerSeq is the Runner-assigned monotonic sequence across the Runner's
	// whole event stream. A gap in the sequence the hub observes is in-transit
	// loss (OQ6 Runner-sequenced, go-toolchain-default.md:1389-1392).
	RunnerSeq uint64
	// SessionID is the Server-side session id the frame belongs to.
	SessionID string
	// Frame is the relayed agent stdout frame, verbatim.
	Frame *compassv1internal.AgentFrame
}

// ConversationSink write-throughs a durable conversation frame (an agent text
// reply or an ask) to the comms store of record and the comms event bus — the
// same write-through PostMessage performs, so a conversation the agent emits is
// indistinguishable downstream from one a human posted. The comms package
// implements it; the hub depends only on this narrow surface so it does not pull
// the whole CommsService in.
type ConversationSink interface {
	// PostAgentMessage commits a conversation posted/updated event to the store
	// and fans it out on the comms bus. posted distinguishes a new message
	// (MessagePosted) from an update to one being composed (MessageUpdated).
	PostAgentMessage(ctx context.Context, sessionID string, msg *compassv1.MessagePosted, updated *compassv1.MessageUpdated) error
}

// LifecycleSink publishes an extracted agent-session lifecycle transition onto
// SubscribeEvents (the board/liveness surface). The server's CompassService bus
// implements it; a session frame carrying a non-UNSPECIFIED AgentSessionState
// extracts to one AgentSessionStatus here.
type LifecycleSink interface {
	// PublishSessionStatus fans an AgentSessionStatus onto SubscribeEvents.
	PublishSessionStatus(status *compassv1.AgentSessionStatus)
}

// SessionTailSink relays an opaque OMP-native session frame to the dedicated
// session-tail stream (the observation pane). In T4 this is a minimal sink so a
// session frame's trace body is never dropped; T5 wires SubscribeAgentSession
// behind the same interface (design.md:521-526, 1479-1482). The hub depends on
// the interface, not the concrete stream, so T5 substitutes without touching the
// hub.
type SessionTailSink interface {
	// RelaySessionFrame forwards one opaque session frame for the session's
	// observation-pane tail.
	RelaySessionFrame(sessionID string, frame *compassv1internal.SessionFrame)
}

// CommsCaller executes an agent-initiated comms call as a resolved agent
// account. The comms package implements it over the same PostMessage /
// ListMessages handler paths a human caller takes, so authz (D9), idempotency,
// and event fan-out are identical — the hub depends only on this narrow surface
// so it does not pull the whole CommsService in (pattern: ConversationSink). It
// is the safe Runner->Server leg: the account is resolved Server-side from the
// hub's own binding, never asserted by the Runner (transport design Decision #3
// / OQ-2, comms-tools design T2).
type CommsCaller interface {
	PostAsAccount(ctx context.Context, account store.AccountID, req *compassv1.PostMessageRequest) (*compassv1.PostMessageResponse, error)
	ListAsAccount(ctx context.Context, account store.AccountID, req *compassv1.ListMessagesRequest) (*compassv1.ListMessagesResponse, error)
}

// Hub is the Server-side seam: enrollment registry + command router + the
// Deliver write-through + the agent-comms session->account binding. Safe for
// concurrent use — the registry and bindings mutate under a mutex and the sinks
// are each concurrency-safe.
type Hub struct {
	conversation ConversationSink
	lifecycle    LifecycleSink
	tail         SessionTailSink
	comms        CommsCaller
	log          *slog.Logger

	mu sync.Mutex
	// runner is the single attached Runner (single-Runner MVP, OQ6
	// go-toolchain-default.md:1392). A second enrollment re-attaches rather than
	// registering a second entry.
	runner *attachedRunner
	// containerAccounts binds a provisioned container_name to the agent account
	// it was provisioned for (recorded at Provision, from the request's
	// agent_account_id). Start promotes the entry to sessionAccounts under the
	// minted session_id; it lives here only for the Provision..Start window.
	containerAccounts map[string]store.AccountID
	// sessionAccounts binds a live session_id to its agent account — the
	// authoritative map RelayCommsCall resolves against. An entry exists only
	// while the session is live: Start adds it, Stop removes it, and a Runner
	// reconnect (enroll re-attach) drops ALL of them, so a re-minted id under a
	// fresh Runner session fails closed (CodeNotFound) rather than inheriting a
	// stale account (OQ-2, ratified). Single-Runner MVP: every binding belongs
	// to the one enrolled Runner, so reconnect clears the whole map.
	sessionAccounts map[string]store.AccountID
	// lastSeq is the highest RunnerSeq Deliver has accepted, for gap detection.
	lastSeq uint64
	// seenGap records whether a sequence gap was ever observed (in-transit
	// loss), surfaced for the board/diagnostics.
	seenGap bool
	// unknownFrames counts frames whose oneof variant was unset or unrecognized
	// — logged and counted, never silently dropped (agent.proto:38-39).
	unknownFrames uint64
}

// attachedRunner is one enrolled Runner: its id, its authenticated token
// subject, and the command router that reaches its live Sessions stream.
type attachedRunner struct {
	id      string
	subject store.Subject
	router  *commandRouter
}

// NewHub constructs a hub over the three write-through sinks and the agent-comms
// caller. comms executes agent-initiated comms calls under the account a session
// resolves to (RelayCommsCall); it may be nil for a hub that never serves comms
// (e.g. a Deliver-only test), in which case RelayCommsCall fails closed. log is
// used for unknown-frame and gap diagnostics; a nil log falls back to
// slog.Default.
func NewHub(conversation ConversationSink, lifecycle LifecycleSink, tail SessionTailSink, comms CommsCaller, log *slog.Logger) *Hub {
	if log == nil {
		log = slog.Default()
	}
	return &Hub{
		conversation:      conversation,
		lifecycle:         lifecycle,
		tail:              tail,
		comms:             comms,
		log:               log,
		containerAccounts: make(map[string]store.AccountID),
		sessionAccounts:   make(map[string]store.AccountID),
	}
}

// Deliver is the sole entry point a relayed Runner event takes into the Server.
// It records the Runner sequence (detecting gaps), classifies the frame by its
// set oneof variant, and write-throughs each variant to the surface that owns
// it: a conversation frame → the comms store + bus; a session frame → the
// session-tail stream, plus its extracted lifecycle state → SubscribeEvents; a
// frame whose variant is unset or unrecognized is logged and counted, never
// silently dropped (design.md:1427-1434, agent.proto:38-39).
func (h *Hub) Deliver(ctx context.Context, ev RunnerEvent) error {
	h.recordSeq(ev.RunnerSeq)

	frame := ev.Frame
	switch f := frame.GetFrame().(type) {
	case *compassv1internal.AgentFrame_ConversationPosted:
		return h.conversation.PostAgentMessage(ctx, ev.SessionID, f.ConversationPosted, nil)
	case *compassv1internal.AgentFrame_ConversationUpdated:
		return h.conversation.PostAgentMessage(ctx, ev.SessionID, nil, f.ConversationUpdated)
	case *compassv1internal.AgentFrame_Session:
		h.deliverSession(ev.SessionID, f.Session)
		return nil
	default:
		// Unset or unrecognized oneof — the "unknown frame". Log + count so a
		// contract skew (a new variant the Server does not yet handle) is
		// observable, but never drop it silently.
		h.countUnknown(ev)
		return nil
	}
}

// SeenGap reports whether Deliver ever observed a Runner-sequence gap. For the
// board/diagnostics; a gap means in-transit loss the Client bus resync recovers.
func (h *Hub) SeenGap() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.seenGap
}

// UnknownFrames reports the count of unset/unrecognized frames Deliver has seen.
func (h *Hub) UnknownFrames() uint64 {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.unknownFrames
}

// deliverSession routes a session frame to the observation-pane tail and, when
// the frame carries a lifecycle transition, extracts the AgentSessionStatus onto
// SubscribeEvents. A session frame can carry a trace event, a lifecycle
// transition, or both; UNSPECIFIED means "trace only, no transition".
func (h *Hub) deliverSession(sessionID string, sf *compassv1internal.SessionFrame) {
	h.tail.RelaySessionFrame(sessionID, sf)
	if state := sf.GetState(); state != compassv1.AgentSessionState_AGENT_SESSION_STATE_UNSPECIFIED {
		h.lifecycle.PublishSessionStatus(&compassv1.AgentSessionStatus{
			SessionId: sessionID,
			State:     state,
		})
	}
}

// recordSeq advances the accepted-sequence high-water mark and flags a gap when
// the observed seq is not exactly one past the last (in-transit loss). The first
// event (lastSeq == 0) establishes the baseline without flagging.
func (h *Hub) recordSeq(seq uint64) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.lastSeq != 0 && seq > h.lastSeq+1 {
		h.seenGap = true
		h.log.Warn("runner event sequence gap",
			slog.Uint64("expected", h.lastSeq+1), slog.Uint64("got", seq))
	}
	if seq > h.lastSeq {
		h.lastSeq = seq
	}
}

// countUnknown records and logs an unknown frame.
func (h *Hub) countUnknown(ev RunnerEvent) {
	h.mu.Lock()
	h.unknownFrames++
	n := h.unknownFrames
	h.mu.Unlock()
	h.log.Warn("unknown runner frame (unset or unrecognized variant)",
		slog.String("session_id", ev.SessionID),
		slog.Uint64("runner_seq", ev.RunnerSeq),
		slog.Uint64("unknown_total", n))
}

// enroll registers (or re-attaches) a Runner under its authenticated subject,
// returning whether it re-attached an existing Runner (OQ6 duplicate enrollment:
// a second enrollment re-attaches the same Runner rather than registering a
// second, single-Runner MVP, go-toolchain-default.md:1392).
//
// Enroll drops ALL agent-comms bindings (OQ-2, ratified). session_id /
// container_name are Runner-minted, so a restarted Runner could re-mint an id
// still bound in the hub and a later comms call would run under the old
// account's scope. Clearing every binding on (re-)enroll closes that: a
// re-minted id under the fresh Runner session resolves CodeNotFound until it is
// bound anew, never a stale account. Single-Runner MVP — every binding belongs
// to the one enrolled Runner, so a reconnect clears the whole map.
func (h *Hub) enroll(id string, subject store.Subject) (reattached bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	reattached = h.runner != nil
	h.runner = &attachedRunner{id: id, subject: subject, router: newCommandRouter()}
	clear(h.containerAccounts)
	clear(h.sessionAccounts)
	return reattached
}

// router returns the attached Runner's command router, or an error when no
// Runner is enrolled (a session command with no Runner to serve it).
func (h *Hub) routerFor(sessionID string) (*commandRouter, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.runner == nil {
		return nil, fmt.Errorf("no runner enrolled to serve session %q", sessionID)
	}
	return h.runner.router, nil
}
