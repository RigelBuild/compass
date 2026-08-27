//go:build unix

// Package runnerhub is the Server side of the Server<->Runner seam (design:
// architecture-lineage). It owns the enrollment registry (which Runner is attached
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

	compassv1 "github.com/RigelBuild/compass/go/gen/compass/v1"
	compassv1internal "github.com/RigelBuild/compass/go/internal/gen/compass/v1"
	"github.com/RigelBuild/compass/go/internal/store"
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

// SessionStartSink is notified when the hub binds a live agent session at its
// StartAgentSession promotion (promoteSession) — the direct, non-bus edge the
// delivery consumer (SEA-1569 T6) subscribes to for the reconnect sweep. On a
// session start the consumer sweeps that session's owed messages
// (UndeliveredMessages) and re-dispatches them ascending-seq through the
// recipient's dispatch gate, so a message posted while the agent had no live
// session is delivered on its next start (design.md:340-346, 816-824). It is
// wired via SetSessionStartSink AFTER both the hub and the consumer exist
// (breaking the construction cycle, exactly as SetSettleSink does), and is
// nil-safe: a hub with no session-start sink is today's behavior, so every
// existing hub test is unchanged.
//
// Single-Runner MVP: a Runner re-enroll clears every binding (enroll) and each
// session re-promotes through promoteSession, so the re-enroll sweep the design
// names (design.md:341) rides the SAME promotion edge — no separate enroll-time
// hook is wired.
//
// Like SettleSink this is a DIRECT edge, not the board bus, and the sink method
// takes NO ctx: it only enqueues the start edge into the consumer's own loop and
// returns promptly, so promoteSession never blocks on the sweep's store work and
// the loop owns the serve ctx the sweep runs under.
type SessionStartSink interface {
	// OnSessionStarted reports that sessionID was bound to agent account at the
	// hub's promoteSession, called right after the binding is recorded.
	OnSessionStarted(sessionID string, account store.AccountID)
}

// SessionReapSink is notified at enroll (the Runner-reconnect teardown) of the
// set of session ids whose hub bindings were just cleared, so a consumer holding
// soft per-session state keyed by session id can drop it. The delivery consumer
// (SEA-1569 T3) subscribes to reap its held-deliver registry entries for a
// no-frame author death: such a death emits no terminal frame, so no settle edge
// ever fires fireHeld to clear the entry, and it would otherwise persist until
// process restart. The design specifies exactly this enroll-bounded reap
// (design.md:172-175). Wired via SetSessionReapSink AFTER both the hub and the
// consumer exist (breaking the construction cycle, exactly as SetSettleSink
// does), and is nil-safe: a hub with no reap sink is today's behavior, so every
// existing hub test is unchanged.
//
// Like SessionStartSink the method takes NO ctx and must return promptly: it
// only drops in-memory registry entries, never blocks the enroll goroutine on
// store work, and is called AFTER the hub releases h.mu.
type SessionReapSink interface {
	// OnSessionsReaped reports that the given session ids had their hub bindings
	// cleared at a Runner (re-)enroll. sessionIDs may be empty (a first-ever
	// enroll clears nothing).
	OnSessionsReaped(sessionIDs []string)
}

// PresenceSink is notified of the two hub-side edges the SEA-1569 T8 presence
// projection (design record D4) is fed by: a session lifecycle transition at the
// deliverSession arm (the SAME arm SettleSink rides, right after the
// LifecycleSink publish) and a session promotion at promoteSession (the
// restart-reconciliation edge — a Runner re-enroll clears bindings and each
// session re-promotes, so presence is reconstructed there). The presence
// component (internal/presence) implements it; the hub depends only on this
// narrow surface. Wired via SetPresenceSink AFTER both exist (breaking the
// construction cycle), and nil-safe: a hub with no presence sink is today's
// behavior, so every existing hub test is unchanged.
//
// Both methods only enqueue + wake the component's own loop — they must NOT
// block the hub's goroutine on store work and must NOT store the caller's ctx
// (the component's Run(ctx) owns the serve ctx). The account is resolved
// in-package by the hub before each call (presence is per-account;
// accountForSession is private and stays so).
type PresenceSink interface {
	// OnSessionLifecycle reports that account's session transitioned to state,
	// called at deliverSession right after the LifecycleSink publish.
	OnSessionLifecycle(account store.AccountID, sessionID string, state compassv1.AgentSessionState)
	// OnSessionPromoted reports that account's session (re-)promoted onto its
	// binding, called at promoteSession — the reconciliation edge.
	OnSessionPromoted(account store.AccountID, sessionID string)
}

// DeliveryStore is the durable delivery-cursor surface the hub's ack arms need:
// the comms delivery cursor (SEA-1569 T3 §6) — resolve a delivered message's
// channel and advance the per-(agent, channel) cursor on the recipient's ack —
// and the forge delivery cursor (RIG-2732 W3) — advance one subscription's
// per-subscriber delivered_revision on the agent's forge_notification_ack.
// *store.Store implements it; the hub depends only on this narrow surface
// (pattern: LifecycleSink). Wired via SetDeliveryStore after construction so no
// NewHub caller signature changes, and nil-safe: a hub with no delivery store
// simply drops delivery_ack / forge_notification_ack frames (a Deliver-only
// test hub never receives one). The forge advance rides THIS same handle rather
// than a second setter: the store that owns the comms cursor owns the forge
// cursor too, so the one SetDeliveryStore(st) boot call (sinks.go) satisfies
// both arms — RIG-2732 T7 adds no new boot wiring for it.
type DeliveryStore interface {
	// MessageChannel resolves a message id to its channel — the ack carries only
	// message_id, but AckDelivery is keyed (agent, channel, message_id).
	MessageChannel(ctx context.Context, messageID string) (store.ChannelID, error)
	// AckDelivery advances the (agent, channel) cursor across the acked message.
	AckDelivery(ctx context.Context, agent store.AccountID, channel store.ChannelID, messageID string) error
	// AdvanceForgeDeliveredRevision advances one subscription's per-subscriber
	// delivered_revision to revision, scoped to the owning agent. Zero rows
	// (unknown id, foreign agent, or unsubscribed mid-flight) -> store.ErrNotFound
	// (RIG-2732 W3; forge_subscriptions.go).
	AdvanceForgeDeliveredRevision(ctx context.Context, agent store.AccountID, subscriptionID, revision string) error
}

// TranscriptStore is the durable transcript surface the hub's commit arm writes
// a relayed transcript_entry frame to (SEA-1667 T4, the durable counterpart to
// the loss-tolerant Deliver path). *store.Store implements it; the hub depends
// only on this narrow surface (pattern: DeliveryStore). Wired via
// SetTranscriptStore after construction so no NewHub caller signature changes,
// and nil-safe: a hub with no transcript store fails a transcript commit closed
// CodeUnavailable (the durable transcript leg is not mounted — a Deliver-only
// test hub never receives one).
type TranscriptStore interface {
	// AppendTranscriptEntry persists one relayed SDK session entry at-most-once
	// on idempotencyKey; entryJSON is opaque and never parsed. The store rebases
	// lifetimeSeq onto the session's bound base and embeds the primary + safety-
	// valve flushes internally (agent_transcripts.go). An unknown session is
	// ErrInvalidArgument (the FK); a genuine (session_id, entry_seq) collision is
	// ErrConflict.
	AppendTranscriptEntry(ctx context.Context, sessionID string, lifetimeSeq uint64, checkpoint bool, entryJSON, idempotencyKey string) error
}

// TranscriptReader is the durable transcript READ surface T5's resume-body
// reconstructor (ReconstructSessionBody) reads through — the read counterpart to
// the write-only TranscriptStore. *store.Store implements it; the hub depends
// only on this narrow surface (pattern: TranscriptStore). Wired via
// SetTranscriptReader after construction so no NewHub caller signature changes,
// and nil-safe: a hub with no reader wired fails ReconstructSessionBody closed
// CodeUnavailable (the resume read leg is not mounted — a Deliver-only test hub
// never resumes). The resume read is taken as ONE atomic snapshot (the PG
// hot-tail and the safety-valve manifest together), plus a segment body by
// object key when the valve fired.
type TranscriptReader interface {
	// SessionResumeSnapshot returns the PG hot-tail (latest checkpoint if any,
	// then every later delta in entry_seq order) AND the safety_valve manifest
	// rows, taken as ONE atomic read-only snapshot so a concurrent safety-valve
	// flush cannot commit between the two reads and corrupt the reconstructed
	// body. An unknown/empty session is ErrNotFound (segments are not read once
	// the tail is empty). Empty segments means the resume never touches the
	// object store — the discriminator for a normal vs S3-fallback resume.
	SessionResumeSnapshot(ctx context.Context, sessionID string) ([]store.TranscriptEntryRow, []store.ArchiveSegmentRow, error)
	// ReadArchiveSegment fetches one safety_valve segment's verbatim-JSONL body
	// by its manifest ObjectKey. Called ONLY when the snapshot's segments are
	// non-empty (the valve fired), never on a normal resume.
	ReadArchiveSegment(ctx context.Context, objectKey string) ([]byte, error)
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
// so it does not pull the whole CommsService in (pattern: LifecycleSink). It
// is the safe Runner->Server leg: the account is resolved Server-side from the
// hub's own binding, never asserted by the Runner (transport design Decision #3
// / OQ-2, comms-tools design T2).
type CommsCaller interface {
	PostAsAccount(ctx context.Context, account store.AccountID, req *compassv1.PostMessageRequest) (*compassv1.PostMessageResponse, error)
	ListAsAccount(ctx context.Context, account store.AccountID, req *compassv1.ListMessagesRequest) (*compassv1.ListMessagesResponse, error)
	// RosterAsAccount executes an agent-initiated GetRoster under account (the
	// caller AND, when the request names no vantage, the session-resolved
	// vantage) — SEA-1721 T2.
	RosterAsAccount(ctx context.Context, account store.AccountID, req *compassv1.GetRosterRequest) (*compassv1.GetRosterResponse, error)
	// SetStatusAsAccount write-throughs the durable activity for account,
	// returning the server-truncated value that landed in the table — the write
	// half of the set_status ordered write-then-publish (SEA-1721 T2 / T3).
	SetStatusAsAccount(ctx context.Context, account store.AccountID, activity string) (string, error)
	UpdatePinnedBoardAsAccount(ctx context.Context, account store.AccountID, req *compassv1.UpdatePinnedBoardRequest) (*compassv1.UpdatePinnedBoardResponse, error)
}

// Hub is the Server-side seam: enrollment registry + command router + the
// Deliver write-through + the agent-comms session->account binding. Safe for
// concurrent use — the registry and bindings mutate under a mutex and the sinks
// are each concurrency-safe.
type Hub struct {
	lifecycle LifecycleSink
	tail      SessionTailSink
	comms     CommsCaller
	log       *slog.Logger
	// settle is the delivery consumer's settle-edge sink (SEA-1569 T3), notified
	// at deliverSession right after the LifecycleSink publish. Nil until
	// SetSettleSink wires it (after both hub and consumer exist), and read under
	// mu so the setter and the arm never race. Nil-safe: a hub with no settle
	// sink is today's behavior.
	settle SettleSink
	// sessionStart is the delivery consumer's session-start-edge sink (SEA-1569
	// T6), notified at promoteSession right after the account->session binding is
	// recorded. Nil until SetSessionStartSink wires it (after both hub and
	// consumer exist), and read under mu so the setter and promoteSession never
	// race. Nil-safe: a hub with no session-start sink is today's behavior.
	sessionStart SessionStartSink
	// reap is the delivery consumer's session-reap sink (SEA-1569 T3), notified
	// at enroll with the session ids whose bindings were just cleared, so the
	// consumer can drop any held-deliver entries a no-frame author death left
	// behind. Nil until SetSessionReapSink wires it (after both hub and consumer
	// exist), and read under mu so the setter and enroll never race. Nil-safe: a
	// hub with no reap sink is today's behavior.
	reap SessionReapSink
	// presence is the SEA-1569 T8 presence projection's sink, notified at
	// deliverSession (lifecycle transition) and promoteSession (reconciliation).
	// Nil until SetPresenceSink wires it (after both hub and the presence
	// component exist), and read under mu so the setter and the arms never race.
	// Nil-safe: a hub with no presence sink is today's behavior.
	presence PresenceSink
	// presenceSource is the T8 presence projection's READ + publish-hook edge the
	// roster leg (SEA-1721 T2) consumes: PresenceFor snapshots the enum map,
	// PublishActivity fires the set_status live event. Distinct from `presence`
	// (the write edge the hub FEEDS). Nil until SetPresenceSource wires it (after
	// both hub and the presence component exist), and read under mu so the setter
	// and the reads never race. Nil-safe: a hub with none wired reports OFFLINE
	// and drops the activity publish.
	presenceSource presenceSource
	// delivery is the durable delivery-cursor store the ack arm advances (SEA-1569
	// T3). Nil until SetDeliveryStore wires it; read under mu. Nil-safe: a hub
	// with no delivery store drops delivery_ack frames.
	delivery DeliveryStore
	// transcripts is the durable transcript store the commit arm writes a relayed
	// transcript_entry frame to (SEA-1667 T4). Nil until SetTranscriptStore wires
	// it; read under mu. Nil-safe: a hub with no transcript store fails a
	// transcript commit closed CodeUnavailable.
	transcripts TranscriptStore
	// reader is the durable transcript READ store T5's resume-body reconstructor
	// (ReconstructSessionBody) reads through. Nil until SetTranscriptReader wires
	// it; read under mu. Nil-safe: a hub with no reader fails
	// ReconstructSessionBody closed CodeUnavailable — the resume read leg is not
	// mounted.
	reader TranscriptReader
	// lifecycleCaller is the spawn/despawn execution seam RelayLifecycleCall
	// delegates a resolved lifecycle call to (spawn/despawn record T4). Nil until
	// SetLifecycleCaller wires it (after both hub and lifecycleService exist,
	// breaking their construction cycle), and read under mu so the setter and the
	// serve path never race. Nil-safe: a hub with none wired fails
	// RelayLifecycleCall closed CodeUnavailable — the lifecycle leg is not mounted.
	lifecycleCaller LifecycleCaller
	// boardCaller is the board-write execution seam RelayBoardCall delegates a
	// resolved SetIssueState call to (agent primary lifecycle T3-a). Nil until
	// SetBoardCaller wires it (after both hub and boardService exist, breaking
	// their construction cycle), and read under mu so the setter and the serve
	// path never race. Nil-safe: a hub with none wired fails RelayBoardCall
	// closed CodeUnavailable — the board write leg is not mounted.
	boardCaller BoardCaller
	// forgeCaller is the forge-write execution seam RelayForgeCall delegates a
	// resolved forge call to (Compass forge write path T4). Nil until
	// SetForgeCaller wires it (after both hub and forgeService exist, breaking
	// their construction cycle), and read under mu so the setter and the serve
	// path never race. Nil-safe: a hub with none wired fails RelayForgeCall
	// closed CodeUnavailable — the forge write leg is not mounted.
	forgeCaller ForgeCaller
	// runnerReadyHook, when set, is invoked once each time a Runner's Sessions
	// command stream attaches (fired from the Sessions handler after
	// router.attach binds the live send, on its own goroutine). It is the seam
	// the first-launch supervisor seed hangs off: Provision/Start need not just
	// an enrolled Runner but one whose command stream can actually serve a
	// command, and that stream attaches AFTER Enroll returns — firing on enroll
	// would race the attach and fail the seed's first Provision CodeUnavailable.
	// The hook itself is idempotent (it gates on an empty agent tree), so
	// re-firing on a later reconnect is a safe no-op. Nil until
	// SetRunnerReadyHook wires it; read under mu. Nil-safe: a hub with none wired
	// does nothing extra when a stream attaches.
	runnerReadyHook func()

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
	// droppedAcks counts delivery_ack frames dropped — an empty or unresolvable
	// message id, an unbound acking session, or a cursor-advance store fault. A
	// missed ack costs only a redundant redeliver on the recipient's next
	// reconnect sweep, never a teardown.
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

// NewHub constructs a hub over the two write-through sinks and the agent-comms
// caller. comms executes agent-initiated comms calls under the account a session
// resolves to (RelayCommsCall); it may be nil for a hub that never serves comms
// (e.g. a Deliver-only test), in which case RelayCommsCall fails closed. log is
// used for unknown-frame and gap diagnostics; a nil log falls back to
// slog.Default.
func NewHub(lifecycle LifecycleSink, tail SessionTailSink, comms CommsCaller, log *slog.Logger) *Hub {
	if log == nil {
		log = slog.Default()
	}
	return &Hub{
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

// SetSessionStartSink wires the delivery consumer as the hub's session-start-edge
// sink, AFTER both exist — the post-construction setter that breaks the
// consumer<->hub construction cycle, exactly as SetSettleSink does. Mirrors the
// SettleSink wiring so no NewHub caller signature changes. Called once at server
// assembly; safe to leave unset (a hub with no session-start sink is today's
// behavior).
func (h *Hub) SetSessionStartSink(sessionStart SessionStartSink) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.sessionStart = sessionStart
}

// SetSessionReapSink wires the delivery consumer as the hub's session-reap-edge
// sink, AFTER both exist — the post-construction setter that breaks the
// consumer<->hub construction cycle, exactly as SetSessionStartSink does.
// Mirrors the SettleSink wiring so no NewHub caller signature changes. Called
// once at server assembly; safe to leave unset (a hub with no reap sink is
// today's behavior).
func (h *Hub) SetSessionReapSink(reap SessionReapSink) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.reap = reap
}

// SetPresenceSink wires the SEA-1569 T8 presence component as the hub's presence
// sink, AFTER both exist — the post-construction setter that breaks the
// component<->hub construction cycle (the component takes the hub as its Status
// relay; the hub takes the component as its PresenceSink). Mirrors SetSettleSink.
// Called once at server assembly; safe to leave unset (a hub with no presence
// sink is today's behavior).
func (h *Hub) SetPresenceSink(presence PresenceSink) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.presence = presence
}

// SetRunnerReadyHook wires a callback fired once each time a Runner's Sessions
// command stream attaches — the seam the first-launch supervisor seed hangs off,
// since Provision/Start need a Runner whose command stream can serve a command,
// and that stream attaches only after Enroll returns (firing on enroll would
// race the attach). Called once at server assembly; nil-safe (a hub with none
// wired does nothing extra when a stream attaches). The hook must be idempotent:
// it fires on every attach (each reconnect), so it gates its own effect (the
// seed no-ops on a non-empty tree).
func (h *Hub) SetRunnerReadyHook(hook func()) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.runnerReadyHook = hook
}

// SetDeliveryStore wires the durable delivery-cursor store the ack arm advances,
// after construction so no NewHub caller signature changes. Called once at
// server assembly; nil-safe (a hub with no delivery store drops delivery_ack).
func (h *Hub) SetDeliveryStore(delivery DeliveryStore) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.delivery = delivery
}

// SetTranscriptStore wires the durable transcript store the commit arm writes a
// relayed transcript_entry frame to (SEA-1667 T4), after construction so no
// NewHub caller signature changes. Called once at server assembly; nil-safe (a
// hub with no transcript store fails a transcript commit closed CodeUnavailable).
// Wired under mu; read under mu.
func (h *Hub) SetTranscriptStore(transcripts TranscriptStore) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.transcripts = transcripts
}

// SetTranscriptReader wires the durable transcript READ store T5's resume-body
// reconstructor reads through (SEA-1667), after construction so no NewHub caller
// signature changes. Called once at server assembly; nil-safe (a hub with no
// reader fails ReconstructSessionBody closed CodeUnavailable). Wired under mu;
// read under mu — the exact posture SetTranscriptStore uses for the write seam.
func (h *Hub) SetTranscriptReader(reader TranscriptReader) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.reader = reader
}

// SetLifecycleCaller wires the lifecycle execution seam after construction, so
// no NewHub caller signature changes and the hub<->lifecycleService construction
// cycle (the service needs the hub for Provision/Start/Remove; the hub needs the
// service to serve RelayLifecycleCall) is broken exactly as SetSettleSink breaks
// the consumer cycle. A hub with none wired fails RelayLifecycleCall closed
// CodeUnavailable. Wired under mu; read under mu.
func (h *Hub) SetLifecycleCaller(c LifecycleCaller) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.lifecycleCaller = c
}

// SetBoardCaller wires the board-write execution seam after construction, so no
// NewHub caller signature changes and the hub<->boardService construction cycle
// (the service needs the store + issue projection; the hub needs the service to
// serve RelayBoardCall) is broken exactly as SetLifecycleCaller breaks the
// lifecycle cycle. A hub with none wired fails RelayBoardCall closed
// CodeUnavailable. Wired under mu; read under mu.
func (h *Hub) SetBoardCaller(c BoardCaller) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.boardCaller = c
}

// Deliver is the sole entry point a relayed Runner event takes into the Server.
// It records the Runner sequence (detecting gaps), classifies the frame by its
// set oneof variant, and write-throughs each variant to the surface that owns
// it: a session frame → the session-tail stream, plus its extracted lifecycle
// state → SubscribeEvents; a delivery_ack → the durable delivery cursor; a frame
// whose variant is unset or unrecognized is logged and counted, never silently
// dropped (design.md:1427-1434, agent.proto:38-39).
func (h *Hub) Deliver(ctx context.Context, ev RunnerEvent) error {
	h.recordSeq(ev.RunnerSeq)

	frame := ev.Frame
	switch f := frame.GetFrame().(type) {
	case *compassv1internal.AgentFrame_Session:
		h.deliverSession(ev.SessionID, f.Session)
		return nil
	case *compassv1internal.AgentFrame_DeliveryAck:
		h.deliverAck(ctx, ev, f.DeliveryAck)
		return nil
	case *compassv1internal.AgentFrame_ForgeNotificationAck:
		h.forgeNotificationAck(ctx, ev, f.ForgeNotificationAck)
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

// DroppedAcks reports the count of delivery_ack frames dropped — an empty or
// unresolvable message id, an unbound acking session, or a cursor-advance store
// fault. A missed ack costs only a redundant redeliver on the recipient's next
// reconnect sweep.
func (h *Hub) DroppedAcks() uint64 {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.droppedAcks
}

// FrameDiagnostics is the hub's frame-loss snapshot: every way a relayed frame
// fails to reach its surface, plus the in-transit gap flag, read together under
// one lock so the values describe the same instant. Reading the accessors one by
// one would interleave with Deliver and could report a gap without the drop that
// accompanied it.
type FrameDiagnostics struct {
	// SeenGap is true once a Runner-sequence gap was observed (in-transit loss
	// the Client bus resync recovers).
	SeenGap bool
	// UnknownFrames counts frames whose oneof variant was unset or unrecognized.
	UnknownFrames uint64
	// DroppedAcks counts delivery_ack frames dropped.
	DroppedAcks uint64
}

// FrameDiagnostics returns the frame-loss snapshot. It exists because the
// counters had no non-test reader: total frame loss was observable only by
// reading warn logs, which is no way to answer "is this relay committing
// anything". serve.go logs this snapshot on shutdown (sinks.go,
// LogFrameDiagnostics), so a run that dropped every agent turn says so once, in
// one line, rather than only in the per-frame noise.
func (h *Hub) FrameDiagnostics() FrameDiagnostics {
	h.mu.Lock()
	defer h.mu.Unlock()
	return FrameDiagnostics{
		SeenGap:       h.seenGap,
		UnknownFrames: h.unknownFrames,
		DroppedAcks:   h.droppedAcks,
	}
}

// fireRunnerReady invokes the runner-ready hook, if wired, on its OWN goroutine.
// Called by the Sessions handler right after the command stream attaches — the
// point a Runner can actually serve a Provision/Start. The goroutine is
// load-bearing, not just decoupling: the seed drives Provision→Start back
// through this hub's router down the very stream whose handler is calling this,
// so running it inline would block that handler's receive loop before it could
// serve the command, deadlocking the seed on its own transport. Reads the hook
// under h.mu (paired with SetRunnerReadyHook); fires after releasing the lock.
//
// The goroutine body recovers a panic and logs it: the hook's contract is
// "a failure is logged, not fatal" (the seed stays non-fatal to a serving
// process), and a bare panic in a goroutine would take down the whole daemon —
// every live session in the single-Runner fleet — rather than degrade to a
// logged failure. Every future ready-hook inherits this guarantee.
//
// The goroutine is fire-and-forget: not tracked by a WaitGroup or shutdown
// drain. That is safe because the hook captures the server's Serve ctx (the seed
// derives its own timeout from it), so shutdown cancellation reaches the hook's
// in-flight work rather than orphaning it. A ready-hook whose work must instead
// complete-or-abort cleanly at teardown would need these goroutines tracked in
// the server's wait group; the current hooks are logged-not-fatal, so they are
// left to race teardown by design.
func (h *Hub) fireRunnerReady() {
	h.mu.Lock()
	hook := h.runnerReadyHook
	h.mu.Unlock()
	if hook == nil {
		return
	}
	go func() {
		defer func() {
			if r := recover(); r != nil {
				h.log.Error("runner-ready hook panicked; recovered (server stays up)",
					slog.Any("panic", r))
			}
		}()
		hook()
	}()
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
	// Resolve the session's agent account from the hub's own live binding and
	// stamp it onto the published status — the DL-167 attribution join, the same
	// binding the presence arm below reads. A status published while the binding
	// is live (including the terminal STOPPED/ERRORED status, as long as Stop's
	// unbindSession has not yet dropped it) carries its account; one published
	// after a Runner reconnect cleared the maps carries none (the stated residual
	// gap). accountForSession takes h.mu; deliverSession holds no lock here.
	account, hasAccount := h.accountForSession(sessionID)
	status := &compassv1.AgentSessionStatus{SessionId: sessionID, State: state}
	if hasAccount {
		status.AgentAccountId = string(account)
	}
	h.lifecycle.PublishSessionStatus(status)
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
	// Same arm: notify the presence projection of the lifecycle transition so it
	// recomputes + republishes-on-change the session's agent presence (SEA-1569
	// T8, design.md:472-479). Reuse the account resolved above (same binding, one
	// lookup) and pass it (the sink is per-account; accountForSession stays
	// private). Read the sink under mu, nil-safe, exactly as the settle sink
	// above; the sink enqueues into the component's own loop and returns promptly
	// (no store work on Deliver). A session with no bound account (a transition
	// before Start's promote) has no presence to publish, so it is simply
	// skipped.
	h.mu.Lock()
	presence := h.presence
	h.mu.Unlock()
	if presence != nil && hasAccount {
		presence.OnSessionLifecycle(account, sessionID, state)
	}
}

// deliverAck advances the durable delivery cursor for a recipient's
// delivery_ack (SEA-1569 T3 §6): the Runner->Server receipt that a relayed
// deliver reached the session. It resolves session->agent from the hub's own
// binding (the SAME binding RelayCommsCall resolves against), resolves the
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

// forgeNotificationAck advances the durable forge delivery cursor for a
// subscriber's forge_notification_ack (RIG-2732 W3): the Runner->Server receipt
// that a ForgeNotification pushed down the session was rendered at the agent's
// turn-end flush. It resolves session->agent from the hub's own binding (the
// SAME binding deliverAck resolves against), then advances that subscription's
// delivered_revision through the delivery store. The store scopes the UPDATE to
// (id, agent_account_id), so a session bound to a DIFFERENT agent than the
// subscription's owner advances nothing (zero rows -> ErrNotFound) — the store
// resolution IS the ownership guard, exactly as MessageChannel+AckDelivery is
// for deliverAck. Fail-closed and non-fatal throughout, mirroring deliverAck:
// an unbound acking session, an empty subscription id, or a store fault
// (including the ErrNotFound of an unsubscribed-mid-flight subscription) is
// logged + counted and dropped, NEVER a stream teardown — a bad ack must not
// kill the Runner's whole event stream; the reconciliation sweep re-notifies
// from the durable gap. A nil delivery store (a Deliver-only hub) drops the ack
// silently: no cursor exists to advance.
func (h *Hub) forgeNotificationAck(ctx context.Context, ev RunnerEvent, ack *compassv1internal.ForgeNotificationAck) {
	h.mu.Lock()
	delivery := h.delivery
	h.mu.Unlock()
	if delivery == nil {
		return
	}
	subscriptionID := ack.GetSubscriptionId()
	if subscriptionID == "" {
		h.countDroppedAck(ev, "forge_notification_ack carries no subscription id")
		return
	}
	agent, ok := h.accountForSession(ev.SessionID)
	if !ok {
		h.countDroppedAck(ev, "no agent account bound to the acking session")
		return
	}
	if err := delivery.AdvanceForgeDeliveredRevision(ctx, agent, subscriptionID, ack.GetRevision()); err != nil {
		// A store fault (or the ErrNotFound of a subscription unsubscribed
		// mid-flight / owned by a different agent) advancing the cursor: log +
		// count and drop. A missed advance costs only a redundant re-notify on
		// the reconciliation sweep's next pass (the cursor stays where it was),
		// so it never justifies tearing down the relay.
		h.countDroppedAck(ev, "forge_notification_ack cursor advance failed: "+err.Error())
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

// countDroppedAck records and logs a dropped delivery_ack: the ack is dropped,
// the drop is COUNTED and LOGGED so it is observable, and the stream lives. A
// missed ack costs only a redundant redeliver on the recipient's next reconnect
// sweep, never a teardown.
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

// promotedPair is one (account -> session) binding snapshotted under h.mu so its
// terminal presence edge can be fired after the lock is released (enroll).
type promotedPair struct {
	account   store.AccountID
	sessionID string
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
	reattached = h.runner != nil
	router := newCommandRouter()
	router.log = h.log
	h.runner = &attachedRunner{id: id, subject: subject, router: router}
	// Snapshot the live (account -> session) bindings BEFORE clearing them: each
	// previously-bound account loses its live session on this re-enroll and must
	// be driven to presence OFFLINE (SEA-1569 T8). enroll emits no lifecycle
	// frames of its own, so without this a long-WORKING agent whose Runner
	// reconnected would stay WORKING in the projection forever. A first-ever
	// enroll (empty maps) snapshots nothing and fires nothing.
	offline := make([]promotedPair, 0, len(h.accountSessions))
	for account, sessionID := range h.accountSessions {
		offline = append(offline, promotedPair{account: account, sessionID: sessionID})
	}
	// Snapshot the session ids whose bindings are about to be cleared, so the
	// delivery consumer can reap any held-deliver registry entries a no-frame
	// author death left behind (SEA-1569 T3, design.md:172-175). sessionAccounts
	// is keyed by session id, and Consumer.held is keyed by that same author
	// session id, so these are exactly the keys to drop. A first-ever enroll
	// (empty map) snapshots nothing.
	reapedSessions := make([]string, 0, len(h.sessionAccounts))
	for sessionID := range h.sessionAccounts {
		reapedSessions = append(reapedSessions, sessionID)
	}
	presence := h.presence
	reap := h.reap
	clear(h.containerAccounts)
	clear(h.sessionAccounts)
	clear(h.accountSessions)
	h.mu.Unlock()

	// Fire the terminal edges AFTER releasing the lock (the sink enqueues into
	// the presence loop and returns promptly, so it must not run under h.mu) —
	// the exact lock-then-release-then-fire discipline promoteSession uses.
	// Order among distinct accounts is irrelevant. publishIfChanged dedups, so a
	// terminal frame already having driven OFFLINE makes this a no-op re-publish.
	if presence != nil {
		for _, p := range offline {
			presence.OnSessionLifecycle(p.account, p.sessionID, compassv1.AgentSessionState_AGENT_SESSION_STATE_DISCONNECTED)
		}
	}

	// Fire the reap edge AFTER releasing the lock, the same lock-then-release-
	// then-fire discipline as the presence edges above: the sink only drops
	// in-memory registry entries and returns promptly, so it must not run under
	// h.mu.
	if reap != nil {
		reap.OnSessionsReaped(reapedSessions)
	}
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
