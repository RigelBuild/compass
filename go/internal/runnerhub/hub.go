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
	"sync/atomic"

	"connectrpc.com/connect"

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
	// IdempotencyKey is the agent-minted key for a durable conversation frame
	// (the durable PostConversationFrame path), populated from
	// PublishEventsRequest.idempotency_key. Empty for trace/session frames. The
	// Runner's gateway carries it (SEA-1364 C2); the hub threads it into the
	// ConversationSink so the conversation write-through commits KEYED at the
	// comms store's (author_account_id, client_request_id) unique constraint —
	// the frozen at-most-once invariant. The sink-ack swap (SEA-1561) later
	// retires this Publish-spine Deliver path for the CommitConversationFrame
	// unary, at which point the key rides that unary instead.
	IdempotencyKey string
}

// ConversationSink write-throughs a durable conversation frame (an agent text
// reply or an ask) to the comms store of record and the comms event bus — the
// same write-through PostMessage performs, so a conversation the agent emits is
// indistinguishable downstream from one a human posted. The comms package
// implements it; the hub depends only on this narrow surface so it does not pull
// the whole CommsService in.
//
// The sink takes the RESOLVED account, not just the session id: the hub owns the
// session->account binding and resolves it once at the Deliver site, exactly as
// RelayCommsCall does (relay_comms.go). Handing the sink a session id to resolve
// for itself would put the binding in two places, which is how attribution
// drifts — so the account is resolved here and passed, and the sink never looks
// a session up.
type ConversationSink interface {
	// PostAgentMessage commits a conversation posted/updated event to the store
	// and fans it out on the comms bus, under account — the agent account the
	// hub resolved the frame's session to. Exactly one of posted/updated is
	// non-nil: posted is a new message (MessagePosted), updated an edit to one
	// being composed (MessageUpdated). sessionID is carried for diagnostics
	// only; account is the authority.
	//
	// ERROR VOCABULARY — REQUIRED OF EVERY IMPLEMENTATION. Deliver classifies a
	// returned error by its CONNECT CODE alone (isFrameRefusal,
	// isContractDefect), so an implementation MUST return connect errors whose
	// codes carry the intended meaning — in practice by routing every store
	// error through the comms package's edgeError mapping, exactly as a human
	// caller's RPC does. The code decides the frame's fate:
	//
	//   - NotFound / InvalidArgument → a per-frame refusal: the frame is
	//     dropped and counted, and the relay stream carries on.
	//   - FailedPrecondition → a contract defect: also dropped and counted, but
	//     against a separate counter and logged as a systemic misconfiguration,
	//     because a defect afflicts every frame rather than this one.
	//   - anything else, INCLUDING an unmapped bare error → an infrastructure
	//     fault: it ENDS the Runner's PublishEvents stream so the relay retries.
	//
	// That last clause is the fail-safe and it is deliberate: connect.CodeOf
	// reports CodeUnknown for a plain errors.New, so a sink that forgets the
	// mapping tears the stream down loudly instead of having its errors silently
	// reinterpreted as "the frame was bad". Never return a bare error to mean a
	// refusal — say it with a code.
	PostAgentMessage(ctx context.Context, account store.AccountID, sessionID string, idempotencyKey string, msg *compassv1.MessagePosted, updated *compassv1.MessageUpdated) error
}

// LifecycleSink publishes an extracted agent-session lifecycle transition onto
// SubscribeEvents (the board/liveness surface). The server's CompassService bus
// implements it; a session frame carrying a non-UNSPECIFIED AgentSessionState
// extracts to one AgentSessionStatus here.
type LifecycleSink interface {
	// PublishSessionStatus fans an AgentSessionStatus onto SubscribeEvents.
	PublishSessionStatus(status *compassv1.AgentSessionStatus)
}

// SettleSink is notified of an agent session's lifecycle transition at the SAME
// hub arm that extracts it for the LifecycleSink (deliverSession) — the direct,
// non-bus edge the delivery consumer (SEA-1569 T3) subscribes to for the
// author's turn-settle (design.md:155-160). The consumer holds agent-authored
// messages until their author's session settles (WORKING->READY) or reaches a
// terminal state, then fires the held delivers from the message's current
// (settled) blocks. It is wired via SetSettleSink AFTER both the hub and the
// consumer exist (breaking the construction cycle), and is nil-safe: a hub with
// no settle sink is today's behavior, so every existing hub test is unchanged.
//
// A DIRECT edge, not the board bus: the settle signal fires through the board
// bus for the liveness surface, but the delivery consumer tails the COMMS bus, a
// different bus. Routing the settle edge through the board stream would couple
// two bus spaces; the hub calling the sink directly at the arm keeps the edge
// where the design puts it (design.md:76-84 of the T3 brief).
type SettleSink interface {
	// OnSessionSettled reports that sessionID transitioned to state, called at
	// the hub's deliverSession arm right after the LifecycleSink publish.
	OnSessionSettled(sessionID string, state compassv1.AgentSessionState)
}

// DeliveryStore is the durable delivery-cursor surface the hub's ack arm needs
// (SEA-1569 T3 §6): resolve a delivered message's channel and advance the
// per-(agent, channel) cursor on the recipient's ack. *store.Store implements
// it; the hub depends only on this narrow surface (pattern: ConversationSink).
// Wired via SetDeliveryStore after construction so no NewHub caller signature
// changes, and nil-safe: a hub with no delivery store simply drops delivery_ack
// frames (a Deliver-only test hub never receives one).
type DeliveryStore interface {
	// MessageChannel resolves a message id to its channel — the ack carries only
	// message_id, but AckDelivery is keyed (agent, channel, message_id).
	MessageChannel(ctx context.Context, messageID string) (store.ChannelID, error)
	// AckDelivery advances the (agent, channel) cursor across the acked message.
	AckDelivery(ctx context.Context, agent store.AccountID, channel store.ChannelID, messageID string) error
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
	// CommitAgentPostKeyed / CommitAgentUpdateKeyed serve CommitConversationFrame
	// AND the Deliver-path ConversationSink — the DURABLE, at-most-once commit.
	// They take the agent-minted idempotency_key the Runner forwards and thread it
	// (POST) or deliberately do not (UPDATE, idempotent by replacement — see the
	// comms implementation) so a retried frame commits at most once.
	// CommitAgentUpdateKeyed is a thin pass-through to the unkeyed CommitAgentUpdate
	// (which stays the live update implementation); CommitAgentPostKeyed posts
	// directly, so the unkeyed CommitAgentPost survives only as a test helper
	// (agent_caller.go).
	CommitAgentPostKeyed(ctx context.Context, account store.AccountID, posted *compassv1.MessagePosted, idempotencyKey string) (*compassv1.PostMessageResponse, error)
	CommitAgentUpdateKeyed(ctx context.Context, account store.AccountID, updated *compassv1.MessageUpdated, idempotencyKey string) (*compassv1.MessageUpdated, error)
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
	// settle is the delivery consumer's settle-edge sink (SEA-1569 T3), notified
	// at deliverSession right after the LifecycleSink publish. Nil until
	// SetSettleSink wires it (after both hub and consumer exist), and read under
	// mu so the setter and the arm never race. Nil-safe: a hub with no settle
	// sink is today's behavior.
	settle SettleSink
	// delivery is the durable delivery-cursor store the ack arm advances (SEA-1569
	// T3). Nil until SetDeliveryStore wires it; read under mu. Nil-safe: a hub
	// with no delivery store drops delivery_ack frames.
	delivery DeliveryStore

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
	// accountSessions is the REVERSE of sessionAccounts (account -> live
	// session_id), maintained wherever sessionAccounts is so the two never drift:
	// promoteSession adds, unbindSession removes, enroll clears. The delivery
	// consumer (SEA-1569 T3) resolves a subscribed agent account to its live
	// session to dispatch a deliver — the reverse direction RelayCommsCall never
	// needs. Single-Runner MVP: an account has at most one live session, so this
	// is a plain 1:1 map; a future multi-session-per-agent change would widen the
	// value to a set.
	accountSessions map[store.AccountID]string
	// lastSeq is the highest RunnerSeq Deliver has accepted, for gap detection.
	lastSeq uint64
	// seenGap records whether a sequence gap was ever observed (in-transit
	// loss), surfaced for the board/diagnostics.
	seenGap bool
	// unknownFrames counts frames whose oneof variant was unset or unrecognized
	// — logged and counted, never silently dropped (agent.proto:38-39).
	unknownFrames uint64
	// refusedFrames counts conversation frames the write-through refused as ONE
	// bad frame — an unresolvable session, or a per-frame rejection from the
	// comms layer (cross-account, revoked member, malformed frame). Each is a
	// non-fatal drop: logged and counted, never silently swallowed, and never a
	// stream teardown (that is reserved for store/transaction faults).
	refusedFrames uint64
	// contractDefects counts conversation frames dropped because the agent and
	// the Server disagree about the frame's SHAPE — a relayed update carrying no
	// message id, or a session bound to a non-agent account. These are wiring
	// skew, not per-frame garbage: unlike a refusal, EVERY frame carries the
	// same defect, so a non-zero count means the relay is committing nothing at
	// all. Counted separately from refusedFrames precisely so total loss cannot
	// masquerade as a healthy relay refusing the occasional frame.
	contractDefects uint64
	// droppedAcks counts delivery_ack frames dropped — an empty or unresolvable
	// message id, an unbound acking session, or a cursor-advance store fault. A
	// delivery_ack is NOT a conversation frame, so it stays out of refusedFrames:
	// folding it in would muddy that bucket and mislead an operator scanning for
	// authz boundaries. A missed ack costs only a redundant redeliver on the
	// recipient's next reconnect sweep, never a teardown.
	droppedAcks uint64
	// secretsVersion is the monotonic set-change token minted for the
	// SecretsVersion signal (mintSecretsVersion). An atomic counter, not a
	// content hash: see mintSecretsVersion for the load-bearing invariant.
	secretsVersion atomic.Uint64
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
		accountSessions:   make(map[store.AccountID]string),
	}
}

// SetSettleSink wires the delivery consumer as the hub's settle-edge sink,
// AFTER both exist — the post-construction setter that breaks the consumer<->hub
// construction cycle (the consumer takes the hub as its ControlDispatcher, the
// hub takes the consumer as its SettleSink). Mirrors the LifecycleSink wiring
// pattern but as a setter, so no NewHub caller signature changes. Called once at
// server assembly; safe to leave unset (a hub with no settle sink is today's
// behavior).
func (h *Hub) SetSettleSink(settle SettleSink) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.settle = settle
}

// SetDeliveryStore wires the durable delivery-cursor store the ack arm advances,
// after construction so no NewHub caller signature changes. Called once at
// server assembly; nil-safe (a hub with no delivery store drops delivery_ack).
func (h *Hub) SetDeliveryStore(delivery DeliveryStore) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.delivery = delivery
}

// Deliver is the sole entry point a relayed Runner event takes into the Server.
// It records the Runner sequence (detecting gaps), classifies the frame by its
// set oneof variant, and write-throughs each variant to the surface that owns
// it: a conversation frame → the comms store + bus; a session frame → the
// session-tail stream, plus its extracted lifecycle state → SubscribeEvents; a
// frame whose variant is unset or unrecognized is logged and counted, never
// silently dropped (design.md:1427-1434, agent.proto:38-39).
//
// A returned error ENDS the Runner's PublishEvents stream (handler.go:99-104),
// so only a fault the relay should retry is returned: a per-frame refusal is a
// non-fatal drop (deliverConversation), never a teardown.
func (h *Hub) Deliver(ctx context.Context, ev RunnerEvent) error {
	h.recordSeq(ev.RunnerSeq)

	frame := ev.Frame
	switch f := frame.GetFrame().(type) {
	case *compassv1internal.AgentFrame_ConversationPosted:
		return h.deliverConversation(ctx, ev, f.ConversationPosted, nil)
	case *compassv1internal.AgentFrame_ConversationUpdated:
		return h.deliverConversation(ctx, ev, nil, f.ConversationUpdated)
	case *compassv1internal.AgentFrame_Session:
		h.deliverSession(ev.SessionID, f.Session)
		return nil
	case *compassv1internal.AgentFrame_DeliveryAck:
		h.deliverAck(ctx, ev, f.DeliveryAck)
		return nil
	default:
		// Unset or unrecognized oneof — the "unknown frame". Log + count so a
		// contract skew (a new variant the Server does not yet handle) is
		// observable, but never drop it silently.
		h.countUnknown(ev)
		return nil
	}
}

// isFrameRefusal reports whether err is a per-frame rejection rather than an
// infrastructure fault. The write-through surfaces comms/store errors through
// the same edgeError mapping a human caller gets, so the Connect code IS the
// classification: NotFound is the D9 collapse (an unknown message, a foreign
// one, or one whose channel membership was revoked) and InvalidArgument a
// malformed frame (empty block set, an ask missing its immutable id). Neither is
// retriable and neither implicates the transport. Everything else — notably
// CodeInternal from a failed transaction — is a real fault the relay should
// retry, so it is NOT swallowed.
func isFrameRefusal(err error) bool {
	switch connect.CodeOf(err) {
	case connect.CodeNotFound, connect.CodeInvalidArgument:
		return true
	default:
		return false
	}
}

// isContractDefect reports whether err is a structural skew between the relay's
// ends rather than one bad frame: CodeFailedPrecondition is the vocabulary a
// sink uses for a request that is impossible as posed, not merely refused — a
// session bound to a NON-AGENT account (comms/agent_caller.go homeChannel), which
// no retry and no other frame can fix.
//
// The distinction matters because the two buckets answer different operator
// questions. A rising refusedFrames says "an authz boundary is being hit"; a
// rising contractDefects says "this relay is committing nothing, go fix the
// wiring". Collapsing them, as the pre-review code did, made those two states
// indistinguishable.
func isContractDefect(err error) bool {
	return connect.CodeOf(err) == connect.CodeFailedPrecondition
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

// RefusedFrames reports the count of conversation frames the write-through
// refused — an unresolvable session, or a per-frame rejection from the comms
// layer. Non-zero is meaningful: a healthy relay refuses nothing, so a rising
// count is either a real authz boundary being hit or a wiring skew worth
// investigating (the diagnostic sibling of UnknownFrames).
func (h *Hub) RefusedFrames() uint64 {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.refusedFrames
}

// ContractDefects reports the count of conversation frames dropped because the
// relay's two ends disagree about the frame's shape — an id-less relayed update,
// or a session bound to a non-agent account. It reads differently from
// RefusedFrames on purpose: a refusal is one bad frame, a defect is a broken
// relay, and on the current base EVERY agent turn is a defect (the agent emits
// id-less updates and no hop stamps an id — see deliverConversation's shape
// check). So a defect count that tracks the frame count means nothing is
// committing at all.
func (h *Hub) ContractDefects() uint64 {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.contractDefects
}

// DroppedAcks reports the count of delivery_ack frames dropped — an empty or
// unresolvable message id, an unbound acking session, or a cursor-advance store
// fault. Kept out of RefusedFrames on purpose: an ack is not a conversation
// frame, and a missed ack costs only a redundant redeliver on the recipient's
// next reconnect sweep, so folding it into the conversation-frame bucket would
// mislead an operator reading that count.
func (h *Hub) DroppedAcks() uint64 {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.droppedAcks
}

// FrameDiagnostics is the hub's frame-loss snapshot: every way a relayed frame
// fails to reach its surface, plus the in-transit gap flag, read together under
// one lock so the four values describe the same instant. Reading the accessors
// one by one would interleave with Deliver and could report a gap without the
// drop that accompanied it.
type FrameDiagnostics struct {
	// SeenGap is true once a Runner-sequence gap was observed (in-transit loss
	// the Client bus resync recovers).
	SeenGap bool
	// UnknownFrames counts frames whose oneof variant was unset or unrecognized.
	UnknownFrames uint64
	// RefusedFrames counts conversation frames refused as one bad frame.
	RefusedFrames uint64
	// ContractDefects counts conversation frames dropped for relay wiring skew.
	// Non-zero and rising is the loud one: the relay is committing nothing.
	ContractDefects uint64
	// DroppedAcks counts delivery_ack frames dropped — separate from
	// RefusedFrames because an ack is not a conversation frame.
	DroppedAcks uint64
}

// FrameDiagnostics returns the frame-loss snapshot. It exists because the three
// counters had no non-test reader: total frame loss was observable only by
// reading warn logs, which is no way to answer "is this relay committing
// anything". serve.go logs this snapshot on shutdown (sinks.go,
// LogFrameDiagnostics), so a run that dropped every agent turn says so once, in
// one line, rather than only in the per-frame noise.
func (h *Hub) FrameDiagnostics() FrameDiagnostics {
	h.mu.Lock()
	defer h.mu.Unlock()
	return FrameDiagnostics{
		SeenGap:         h.seenGap,
		UnknownFrames:   h.unknownFrames,
		RefusedFrames:   h.refusedFrames,
		ContractDefects: h.contractDefects,
		DroppedAcks:     h.droppedAcks,
	}
}

// deliverConversation resolves the frame's session to its agent account and
// write-throughs the frame under that account, classifying any failure.
//
// Resolution is the SAME binding RelayCommsCall resolves against
// (accountForSession, relay_comms.go:78) and it happens HERE, once, rather than
// inside the sink: the hub owns the session->account binding, and a second
// resolution site is how attribution drifts.
//
// Fail closed on an unbound session. The account is the agent's IDENTITY on a
// durable, fanned-out row — there is no safe default, so an unresolvable session
// is refused outright rather than committed under a fallback. This mirrors
// errNoActor (comms/agent_caller.go): a missing actor is a hard refusal
// precisely so a wiring bug cannot quietly attribute an agent's words to the
// bootstrap admin. The refusal is a DROP, not a teardown — one frame the Server
// cannot attribute must not kill the Runner's whole event stream.
//
// KNOWN CONSEQUENCE, deliberate — do not "fix" it with a fallback. enroll()
// (:269) calls clear(h.sessionAccounts) on every Runner re-enroll, because
// session ids are Runner-minted and a restarted Runner can re-mint an id still
// bound here; clearing is what stops the new session's words being attributed to
// the previous account. So a frame in flight across a Runner reconnect resolves
// to nothing and lands on this refusal path. That is the ruled behavior (OQ-2,
// ratified). Reintroducing any fallback here — a default account, a retained
// stale binding — would reopen exactly the misattribution the clear() exists to
// close. The right fix is Runner-side session resume, which is another lane's
// work; it re-binds the session and this path then resolves normally.
func (h *Hub) deliverConversation(
	ctx context.Context,
	ev RunnerEvent,
	posted *compassv1.MessagePosted,
	updated *compassv1.MessageUpdated,
) error {
	account, ok := h.accountForSession(ev.SessionID)
	if !ok {
		h.countRefused(ev, "no agent account bound to the frame's session")
		return nil
	}
	// Frame-shape check BEFORE the sink. An update names the row it edits by
	// message.id, so an id-less one addresses nothing and there is no write to
	// attempt — the comms layer would reject it as InvalidArgument, which is
	// indistinguishable here from an ordinary malformed frame. Checking the shape
	// at the seam is what lets the hub name the real cause, and this is the
	// SOLE frame shape the production agent emits today (see contractDefect's
	// doc and contract_defect_test.go), so misclassifying it would report 100%
	// frame loss as routine refusals.
	if updated != nil && updated.GetMessage().GetId() == "" {
		h.countContractDefect(ev, "relayed conversation_updated carries no message.id, so it addresses no row and nothing can commit")
		return nil
	}
	if err := h.conversation.PostAgentMessage(ctx, account, ev.SessionID, ev.IdempotencyKey, posted, updated); err != nil {
		switch {
		case isContractDefect(err):
			// Not this frame's fault: the relay is wired wrong (a session bound
			// to a non-agent account, a Server-side dispatch skew). Still a
			// drop, because every frame carries the same defect and a teardown
			// would kill the relay outright — but counted and logged as the
			// systemic fault it is.
			h.countContractDefect(ev, err.Error())
			return nil
		case isFrameRefusal(err):
			// One frame the comms layer rejected — a cross-account or revoked
			// -member edit, or a malformed frame. It is specific to this frame,
			// so retrying the relay would only replay the same rejection: drop
			// and count it, and let the stream carry the next frame.
			h.countRefused(ev, err.Error())
			return nil
		}
		// Not frame-specific (a store or transaction fault), or unclassified
		// (an error that never met edgeError, which connect.CodeOf reports as
		// CodeUnknown): end the stream so the Runner retries the relay rather
		// than losing the frame. An error the Server cannot classify is never
		// assumed to be the frame's fault.
		return err
	}
	return nil
}

// deliverSession routes a session frame to the observation-pane tail and, when
// the frame carries a lifecycle transition, extracts the AgentSessionStatus onto
// SubscribeEvents. A session frame can carry a trace event, a lifecycle
// transition, or both; UNSPECIFIED means "trace only, no transition".
func (h *Hub) deliverSession(sessionID string, sf *compassv1internal.SessionFrame) {
	h.tail.RelaySessionFrame(sessionID, sf)
	state := sf.GetState()
	if state == compassv1.AgentSessionState_AGENT_SESSION_STATE_UNSPECIFIED {
		return
	}
	h.lifecycle.PublishSessionStatus(&compassv1.AgentSessionStatus{
		SessionId: sessionID,
		State:     state,
	})
	// Same arm, right after the lifecycle publish: notify the delivery consumer
	// of the author's settle edge so it can fire any agent-authored messages held
	// for this session (SEA-1569 T3 §2, design.md:155-160). Read the sink under
	// mu so the setter and this arm never race; nil-safe (a hub with no settle
	// sink is today's behavior). The sink enqueues into the consumer's own loop
	// and returns promptly — it does not block Deliver on store work.
	h.mu.Lock()
	settle := h.settle
	h.mu.Unlock()
	if settle != nil {
		settle.OnSessionSettled(sessionID, state)
	}
}

// deliverAck advances the durable delivery cursor for a recipient's
// delivery_ack (SEA-1569 T3 §6): the Runner->Server receipt that a relayed
// deliver reached the session. It resolves session->agent from the hub's own
// binding (the SAME binding deliverConversation resolves against), resolves the
// acked message's channel through the delivery store (the ack carries only
// message_id, but AckDelivery is keyed (agent, channel, message_id)), then
// advances the cursor. Fail-closed and non-fatal throughout, mirroring the
// existing frame handlers: an unbound session, an unknown/foreign message, or a
// store fault is logged + counted and dropped, NEVER a stream teardown — a bad
// ack must not kill the Runner's whole event stream. A nil delivery store (a
// Deliver-only hub) drops the ack silently: no cursor exists to advance.
func (h *Hub) deliverAck(ctx context.Context, ev RunnerEvent, ack *compassv1internal.DeliveryAck) {
	h.mu.Lock()
	delivery := h.delivery
	h.mu.Unlock()
	if delivery == nil {
		return
	}
	messageID := ack.GetMessageId()
	if messageID == "" {
		h.countDroppedAck(ev, "delivery_ack carries no message id")
		return
	}
	agent, ok := h.accountForSession(ev.SessionID)
	if !ok {
		h.countDroppedAck(ev, "no agent account bound to the acking session")
		return
	}
	// MessageChannel only resolves ANY message's channel; it is NOT the
	// membership/owed clamp. store.AckDelivery is the clamp: it resolves
	// messageID WHERE id=$1 AND channel_id=$2 and only UPDATEs an existing
	// seeded cursor (never inserts), so the message must resolve for this
	// (agent, channel) there or the ack is a no-op — a future reader must not
	// mistake MessageChannel for the guard.
	channel, err := delivery.MessageChannel(ctx, messageID)
	if err != nil {
		// An unknown or foreign message id: fail-closed no-op, never a teardown.
		// A fabricated id cannot advance a cursor; the resolution IS the guard.
		h.countDroppedAck(ev, "delivery_ack for an unresolvable message: "+err.Error())
		return
	}
	if err := delivery.AckDelivery(ctx, agent, channel, messageID); err != nil {
		// A store fault advancing the cursor: log + count and drop. A missed ack
		// costs only a redundant redeliver on the recipient's next reconnect
		// sweep (the cursor stays where it was), so it never justifies tearing
		// down the relay.
		h.countDroppedAck(ev, "delivery_ack cursor advance failed: "+err.Error())
		return
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

// countRefused records and logs a refused conversation frame — the write-through
// counterpart of countUnknown, and the same posture: the frame is dropped, but
// the drop is COUNTED and LOGGED so it is observable. A refusal is never silent,
// because silence here would hide both a real authz boundary being hit and a
// binding/wiring skew that is losing an agent's words.
func (h *Hub) countRefused(ev RunnerEvent, reason string) {
	h.mu.Lock()
	h.refusedFrames++
	n := h.refusedFrames
	h.mu.Unlock()
	h.log.Warn("refused relayed conversation frame (dropped, stream continues)",
		slog.String("session_id", ev.SessionID),
		slog.Uint64("runner_seq", ev.RunnerSeq),
		slog.String("reason", reason),
		slog.Uint64("refused_total", n))
}

// countDroppedAck records and logs a dropped delivery_ack — the ack-arm
// counterpart of countRefused, and the same non-fatal posture: the ack is
// dropped, the drop is COUNTED and LOGGED so it is observable, and the stream
// lives. Kept distinct from countRefused because a delivery_ack is not a
// conversation frame: folding it into refusedFrames would muddy that bucket and
// emit a misleading "conversation frame" line. A missed ack costs only a
// redundant redeliver on the recipient's next reconnect sweep, never a teardown.
func (h *Hub) countDroppedAck(ev RunnerEvent, reason string) {
	h.mu.Lock()
	h.droppedAcks++
	n := h.droppedAcks
	h.mu.Unlock()
	h.log.Warn("dropped delivery ack (stream continues)",
		slog.String("session_id", ev.SessionID),
		slog.Uint64("runner_seq", ev.RunnerSeq),
		slog.String("reason", reason),
		slog.Uint64("dropped_ack_total", n))
}

// countContractDefect records and logs a conversation frame dropped for relay
// wiring skew. Same non-fatal posture as countRefused — the frame is dropped and
// the stream lives — but a deliberately louder line, because the two say very
// different things to whoever reads them.
//
// countRefused's line means "one frame was rejected"; a reader can reasonably
// ignore it. THIS line means the relay's two ends disagree structurally, so every
// frame is being lost and no retry, redeploy, or next frame will change that.
// The message therefore states the consequence outright rather than naming the
// frame: an operator scanning logs must not have to infer total data loss from a
// counter they were never told to watch.
//
// Error, not Warn: a refusal is expected traffic on a healthy system, a contract
// defect never is. It stays non-fatal all the same — returning an error here
// would end the Runner's PublishEvents stream on the FIRST agent turn, since on
// the current base every turn carries the defect.
func (h *Hub) countContractDefect(ev RunnerEvent, reason string) {
	h.mu.Lock()
	h.contractDefects++
	n := h.contractDefects
	h.mu.Unlock()
	h.log.Error("agent conversation relay is misconfigured: NOTHING is being committed to comms",
		slog.String("session_id", ev.SessionID),
		slog.Uint64("runner_seq", ev.RunnerSeq),
		slog.String("defect", reason),
		slog.Uint64("contract_defect_total", n))
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
	router := newCommandRouter()
	router.log = h.log
	h.runner = &attachedRunner{id: id, subject: subject, router: router}
	clear(h.containerAccounts)
	clear(h.sessionAccounts)
	clear(h.accountSessions)
	return reattached
}

// routerFor returns the attached Runner's command router and its id, or an error
// when no Runner is enrolled (a session command with no Runner to serve it). The
// id travels out with the router so a caller that must attribute the call to a
// Runner (Provision, recording an agent's durable placement) names the Runner
// that actually served it, rather than re-reading the registry afterwards and
// racing a re-enroll onto the wrong id.
func (h *Hub) routerFor(sessionID string) (*commandRouter, string, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.runner == nil {
		return nil, "", fmt.Errorf("no runner enrolled to serve session %q", sessionID)
	}
	return h.runner.router, h.runner.id, nil
}
