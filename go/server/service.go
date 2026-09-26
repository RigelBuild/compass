//go:build unix

// The CompassService implementation — server side of the compass.v1 contract.
// GetServerInfo probes liveness; SubscribeEvents snapshots the ring then tails.
// The agent-session lifecycle mutators relay to the owning Runner (Unavailable on
// the socket-only path). GetAgentStatus is served from the board; IssueToken mints.
package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"

	"github.com/RigelBuild/compass/go/events"
	compassv1 "github.com/RigelBuild/compass/go/gen/compass/v1"
	"github.com/RigelBuild/compass/go/gen/compass/v1/compassv1connect"
	"github.com/RigelBuild/compass/go/internal/auth"
	"github.com/RigelBuild/compass/go/internal/board"
	"github.com/RigelBuild/compass/go/internal/runnerhub"
	"github.com/RigelBuild/compass/go/internal/store"
)

// apiVersion is the compass.v1 contract version reported by GetServerInfo.
const apiVersion = "compass.v1"

// resyncSeq is the seq on a ResyncRequired: it is a control signal, not a
// positioned event, and the client discards its cursor to reconnect at
// sinceSeq = 0, so it carries no meaningful position.
const resyncSeq uint64 = 0

// rollbackStopTimeout bounds the best-effort Stop in abandonStartedSession. The
// runnerhub dispatch path (internal/runnerhub) has NO deadline of its own — a
// wedged-but-connected Runner that accepts Stop but never answers its result
// would otherwise hang StartAgentSession (and stall graceful-shutdown drain)
// forever. A package var rather than a const only so a test can shorten it; it
// is never reassigned in production.
var rollbackStopTimeout = 30 * time.Second

// errNoRunnerHub is returned by the container-lifecycle RPCs when no Runner door
// is mounted (the socket-only path) — the RPC is Unavailable, never a panic.
var errNoRunnerHub = errors.New("no runner hub configured on this server")

// errNoCaller is returned by SubscribeAgentSession when no caller identity is in
// context — an interceptor-wiring bug (both doors must attach one). Fail closed
// with Unauthenticated rather than stream an unauthorized session.
var errNoCaller = errors.New("no caller identity in request context")

// errNoSessionID is the abandon-path cause when a Runner returned success for a
// Start but no session id: there is nothing to Stop, and nothing the caller
// could ever address, so the roll-back reduces to logging it loudly.
var errNoSessionID = errors.New("runner returned no session id to stop")

// busPayload is the bus's event type. Go's generated oneof interface is
// unexported, so the bus carries the whole response message with only its
// Payload oneof set at publish time — the bus stamps Seq/AtUnixMs/InstanceEpoch
// onto a copy at the stream edge.
type busPayload = *compassv1.SubscribeEventsResponse

// service is the server's compass.v1 service. The event bus and the store of
// record are shared by pointer and the version is a small owned string. The
// store backs IssueToken: it verifies the target account against the store, then
// mints a bearer token whose hash the store persists. The runner hub (when set)
// routes the container-lifecycle RPCs to the owning Runner; hub is nil on the
// socket-only path that mounts no Runner door, and a lifecycle RPC then returns
// Unavailable rather than panicking.
type service struct {
	compassv1connect.UnimplementedCompassServiceHandler
	version string
	bus     *events.Bus[busPayload]
	store   *store.Store
	hub     *runnerhub.Hub
	// board is the Bridge board projection GetAgentStatus reads its snapshot from.
	// The same instance is the RunnerHub's lifecycle sink (serve.go wires both),
	// so the snapshot reflects exactly the transitions fanned onto SubscribeEvents.
	board *board.Projection
	// issueBrd is the Server-authoritative board issue projection ListBoardIssues
	// reads its Snapshot from. The same instance is the ingestion sink (part 3's
	// issueSink) and the issue=16 live fan-out onto SubscribeEvents, so the board
	// the handler re-snapshots is exactly the state the tail unions against.
	issueBrd *board.IssueProjection
	// tail is the per-session SubscribeAgentSession fan-out (sessiontail.go). The
	// same instance is the RunnerHub's session-tail sink (serve.go wires both),
	// so a frame the hub relays reaches this service's stream subscribers. nil on
	// a server built with no Runner door (SubscribeAgentSession then Unavailable,
	// like the other Runner-backed RPCs).
	tail *sessionTail
	// signaler emits a ConfigVersion signal to every live session after a
	// successful config write (PutAgentConfig / DeleteAgentConfig), so live
	// Runners re-fetch the bundle. *runnerhub.Hub satisfies it; the service
	// depends on the narrow interface (not the concrete hub) so a test drives the
	// emit path with a recorder, mirroring secretsService's secretsSignaler. nil
	// on a server built with no Runner door (socket-only) — a write then completes
	// without a signal, there being no live session to notify.
	signaler configSignaler
	// spawnMu guards spawns, the SpawnAgent composite-span memo (spawn.go). A
	// spawn keyed by (agent_account_id, client_request_id) is registered before
	// its Provision→Start runs and settled after, so a retry (concurrent or
	// sequential) joins the original rather than re-provisioning — the end-to-end
	// idempotency the three lower dedup primitives do not compose for a completed
	// retry. A settled success is evicted after spawnMemoTTL, bounding the map.
	spawnMu sync.Mutex
	spawns  map[spawnKey]*spawnCall
	// scheduleAfter schedules the delayed memo eviction; nil uses time.AfterFunc.
	// A test overrides it to drive eviction deterministically without wall time.
	scheduleAfter func(time.Duration, func()) *time.Timer
}

func newService(version string, bus *events.Bus[busPayload], st *store.Store, hub *runnerhub.Hub, brd *board.Projection, issueBrd *board.IssueProjection, tail *sessionTail) *service {
	// A nil *runnerhub.Hub must land in the interface field as a nil interface,
	// not a non-nil interface wrapping a nil pointer, so the handler's nil check
	// fires on the socket-only path. Assign through the concrete-nil guard.
	var sig configSignaler
	if hub != nil {
		sig = hub
	}
	return &service{version: version, bus: bus, store: st, hub: hub, board: brd, issueBrd: issueBrd, tail: tail, signaler: sig}
}

// ProvisionAgentWorkspace creates the isolated per-agent container for a
// workstream by relaying to the owning Runner (Client -> Server -> RunnerHub ->
// Runner -> AgentRuntime façade); the Server holds no container-engine code. The
// agent is named `owner/agent` and resolved here; the Runner receives the
// resolved account id. The request's client_request_id (when set) is the OQ6
// idempotency key threaded to the RunnerHub as the correlation id: a
// timeout-retry with the same id joins the in-flight/completed call and returns
// the same container_name rather than provisioning a second container.
func (s *service) ProvisionAgentWorkspace(
	ctx context.Context,
	req *connect.Request[compassv1.ProvisionAgentWorkspaceRequest],
) (*connect.Response[compassv1.ProvisionAgentWorkspaceResponse], error) {
	if s.hub == nil {
		return nil, connect.NewError(connect.CodeUnavailable, errNoRunnerHub)
	}
	acc, err := s.resolveQualifiedAgent(ctx, req.Msg.GetAgentHandle())
	if err != nil {
		return nil, err
	}
	resp, err := s.provisionAgent(ctx, acc, req.Msg)
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(resp), nil
}

// StartAgentSession brings the first-party agent in a provisioned container
// online by relaying to the owning Runner, which starts the agent over its
// streaming-exec bridge and returns the server-side session id — the cursor for
// Stop/Reload/GetAgentStatus and the attribution id on every agent payload.
// StartAgentSessionRequest carries no client_request_id (unlike Provision), so
// the relay request id is Server-minted per call; client-retry idempotency is
// not part of Start's established contract. hub is nil only on a server built with no
// Runner door, where a lifecycle RPC is Unavailable rather than a panic.
func (s *service) StartAgentSession(
	ctx context.Context,
	req *connect.Request[compassv1.StartAgentSessionRequest],
) (*connect.Response[compassv1.StartAgentSessionResponse], error) {
	if s.hub == nil {
		return nil, connect.NewError(connect.CodeUnavailable, errNoRunnerHub)
	}
	// The resume branch: a non-empty resume_session_id relays through the resume
	// handoff (authz-gate, bind lifetime, reconstruct body, attach to the INTERNAL
	// envelope), all BEFORE any Runner call. A resume REUSES the logical id as the
	// live id, so its ownership row already exists and was already authorized — it
	// does NOT re-record ownership. A fresh start mints a new id and records below.
	if resumeID := req.Msg.GetResumeSessionId(); resumeID != "" {
		resp, err := s.startResumeSession(ctx, resumeID, req.Msg)
		if err != nil {
			return nil, err
		}
		return connect.NewResponse(resp), nil
	}

	resp, err := s.hub.Start(ctx, "", req.Msg)
	if err != nil {
		return nil, err
	}
	// Complete the durable ownership chain now that the Runner started the session:
	// session_id -> agent_account_id, from the placement recorded at Provision (a
	// durable read, correct across restart). Either store step can fail AFTER the
	// Runner started, and unlike Provision this does NOT self-heal, so we tear it down.
	agentAccountID, err := s.store.AgentForContainer(ctx, req.Msg.GetContainerName())
	if err != nil {
		return nil, s.abandonStartedSession(ctx, req.Msg.GetContainerName(), resp.GetSessionId(),
			fmt.Errorf("resolving agent for container: %w", err))
	}
	if err := s.store.RecordAgentSession(ctx, resp.GetSessionId(), agentAccountID); err != nil {
		return nil, s.abandonStartedSession(ctx, req.Msg.GetContainerName(), resp.GetSessionId(),
			fmt.Errorf("recording agent session ownership: %w", err))
	}
	return connect.NewResponse(resp), nil
}

// StopAgentSession deliberately kills the in-container agent and releases the
// session by relaying to the owning Runner. Idempotent per the established contract
// (compass.proto): stopping an unknown/already-stopped session succeeds, since
// the Runner returns success for a session it no longer holds.
func (s *service) StopAgentSession(
	ctx context.Context,
	req *connect.Request[compassv1.StopAgentSessionRequest],
) (*connect.Response[compassv1.StopAgentSessionResponse], error) {
	if s.hub == nil {
		return nil, connect.NewError(connect.CodeUnavailable, errNoRunnerHub)
	}
	resp, err := s.hub.Stop(ctx, "", req.Msg)
	if err != nil {
		return nil, err
	}
	// RIG-1667 T4 session-end flush: archive the remaining hot-tail as one session_end
	// segment so history is COMPLETE for analytics (not pruned, never read on resume).
	// BEST-EFFORT: the Stop relay already killed the agent, so a flush failure must
	// NEVER convert a successful Stop into a failure.
	sessionID := req.Msg.GetSessionId()
	if maxSeq, seqErr := s.store.SessionMaxEntrySeq(ctx, sessionID); seqErr != nil {
		slog.ErrorContext(ctx, "session-end transcript flush skipped: could not read max entry seq",
			"session_id", sessionID, "error", seqErr)
	} else if maxSeq > 0 {
		// maxSeq == 0 means no transcript rows: nothing to archive, skip the flush.
		if flushErr := s.store.FlushSuperseded(ctx, sessionID, maxSeq, store.SegmentKindSessionEnd); flushErr != nil {
			slog.ErrorContext(ctx, "session-end transcript flush failed; Stop still succeeded",
				"session_id", sessionID, "upto_entry_seq", maxSeq, "error", flushErr)
		}
	}
	return connect.NewResponse(resp), nil
}

// ReloadAgentSession tears down the current agent exec and starts a fresh one
// against the same container, reusing the session id so the board entry is
// continuous — relayed to the owning Runner, which owns the teardown-then-start.
func (s *service) ReloadAgentSession(
	ctx context.Context,
	req *connect.Request[compassv1.ReloadAgentSessionRequest],
) (*connect.Response[compassv1.ReloadAgentSessionResponse], error) {
	if s.hub == nil {
		return nil, connect.NewError(connect.CodeUnavailable, errNoRunnerHub)
	}
	resp, err := s.hub.Reload(ctx, "", req.Msg)
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(resp), nil
}

// RemoveAgentWorkspace tears down a provisioned per-agent container — the
// operator-door teardown counterpart to ProvisionAgentWorkspace — by relaying to
// the owning Runner, then releasing the durable placement so the container name
// is free for a future provision. This is the ADMIN-gated operator door (the
// AdminGate classifies RemoveAgentWorkspace as adminOnly); the agent-facing
// despawn reaches teardown through lifecycleService.DespawnAsAccount, not here.
// Idempotent end to end per the established contract (compass.proto): the Runner
// returns success for an unknown/already-removed container, and
// DeleteAgentPlacement treats an absent row as success — so a retried Remove
// succeeds. hub is nil only on a server built with no Runner door, where the RPC
// is Unavailable rather than a panic.
func (s *service) RemoveAgentWorkspace(
	ctx context.Context,
	req *connect.Request[compassv1.RemoveAgentWorkspaceRequest],
) (*connect.Response[compassv1.RemoveAgentWorkspaceResponse], error) {
	if s.hub == nil {
		return nil, connect.NewError(connect.CodeUnavailable, errNoRunnerHub)
	}
	resp, err := s.hub.Remove(ctx, req.Msg.GetClientRequestId(), req.Msg)
	if err != nil {
		return nil, err
	}
	if err := s.store.DeleteAgentPlacement(ctx, req.Msg.GetContainerName()); err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("releasing agent placement: %w", err))
	}
	return connect.NewResponse(resp), nil
}

// GetAgentStatus returns the Bridge board's snapshot of agent-session state: one
// session when session_id is set, else every live session. It reads the
// Server-side projection, not a live relay to the Runner. A nil board (never the
// real serving path — serve.go always builds one) answers empty rather than
// panicking.
func (s *service) GetAgentStatus(
	_ context.Context,
	req *connect.Request[compassv1.GetAgentStatusRequest],
) (*connect.Response[compassv1.GetAgentStatusResponse], error) {
	if s.board == nil {
		return connect.NewResponse(&compassv1.GetAgentStatusResponse{}), nil
	}
	return connect.NewResponse(&compassv1.GetAgentStatusResponse{
		Statuses: s.board.Snapshot(req.Msg.GetSessionId()),
	}), nil
}

// ListBoardIssues returns the whole issue board (all states incl. ARCHIVED,
// sorted by id) from the Server-authoritative IssueProjection snapshot — the
// connect-time re-snapshot a client reads once and then unions, id-keyed, with
// the SubscribeEvents live tail. The board is unversioned in v1: the request's
// snapshot_seq is RESERVED and IGNORED, the handler always returns the current
// Snapshot(). A nil projection (never the real serving path — serve.go always
// builds one) answers empty rather than panicking, mirroring GetAgentStatus.
func (s *service) ListBoardIssues(
	_ context.Context,
	_ *connect.Request[compassv1.ListBoardIssuesRequest],
) (*connect.Response[compassv1.ListBoardIssuesResponse], error) {
	if s.issueBrd == nil {
		return connect.NewResponse(&compassv1.ListBoardIssuesResponse{}), nil
	}
	return connect.NewResponse(&compassv1.ListBoardIssuesResponse{
		Issues: s.issueBrd.Snapshot(),
	}), nil
}

// GetServerInfo is the connect-time liveness/version probe.
func (s *service) GetServerInfo(
	_ context.Context,
	_ *connect.Request[compassv1.GetServerInfoRequest],
) (*connect.Response[compassv1.GetServerInfoResponse], error) {
	return connect.NewResponse(&compassv1.GetServerInfoResponse{
		Version:    s.version,
		ApiVersion: apiVersion,
	}), nil
}

// WhoAmI reflects the caller's own account id, resolved server-side from its
// authenticated credential (DL-111) — embedded (socket) yields the ambient-admin
// identity, native-client (network door) the bearer's subject. The id is never a
// client-supplied field; it comes solely from the caller identity the door's
// interceptors attached. A missing caller means an interceptor-wiring bug (both
// doors must attach one): fail closed with Unauthenticated.
func (s *service) WhoAmI(
	ctx context.Context,
	_ *connect.Request[compassv1.WhoAmIRequest],
) (*connect.Response[compassv1.WhoAmIResponse], error) {
	caller, ok := auth.CallerFrom(ctx)
	if !ok {
		return nil, connect.NewError(connect.CodeUnauthenticated, errNoCaller)
	}
	return connect.NewResponse(&compassv1.WhoAmIResponse{AccountId: string(caller)}), nil
}

// IssueToken mints a bearer token for an existing account. On the network door it
// is admin-only: the AdminGate classifies IssueToken as adminOnly, so only the
// bootstrap admin reaches this handler. On the shipped socket door it is served
// ungated alongside every other method — that door mounts no interceptors, since
// the 0600 socket is itself the local admin credential — so the token minted here
// is valid on the network door (no capability beyond the bootstrap-admin token
// file the same local peer already owns). The account is a bare user handle or an
// `owner/agent` handle; the token store retains only the token's hash.
func (s *service) IssueToken(
	ctx context.Context,
	req *connect.Request[compassv1.IssueTokenRequest],
) (*connect.Response[compassv1.IssueTokenResponse], error) {
	raw := req.Msg.GetAccountHandle()
	if raw == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument,
			errors.New("account_handle is required"))
	}
	// The admin door is not visibility-scoped (no D9 clip): the admin may name any account.
	var acc store.Account
	var err error
	if qh := store.ParseQualifiedHandle(raw); qh.Handle != qh.Raw {
		acc, err = s.resolveQualifiedAgent(ctx, raw)
	} else if acc, err = s.store.UserByHandle(ctx, raw); err != nil {
		err = handleLookupError(raw, err)
	}
	if err != nil {
		return nil, err
	}
	id := acc.ID
	// The system account (@compass) is structurally not authenticatable: it is a
	// server-internal sender, never a token subject. Refuse here rather than
	// relying on the admin gate alone — defense in depth on the token-minting door.
	if acc.System != nil {
		return nil, connect.NewError(connect.CodePermissionDenied,
			fmt.Errorf("account %q is the system account and cannot be issued a token", raw))
	}
	token, err := auth.IssueAccountToken(ctx, s.store, id)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	return connect.NewResponse(&compassv1.IssueTokenResponse{Token: token}), nil
}

// RevokeToken withdraws a bearer token by its presented plaintext (network door,
// admin-gated by classifyProcedure). The CLI sends the plaintext over the
// authenticated door and this handler hashes it (auth.RevokeToken → hashToken):
// the server is the only party that hashes, and the store keys tokens by hash,
// never plaintext. Revocation is per-hash (this one token), not per-account.
// An empty token is a client error (InvalidArgument). A token that was never
// issued is NotFound; an already-revoked token is a no-op success (the store
// returns nil), so a repeat revoke is not an error.
func (s *service) RevokeToken(
	ctx context.Context,
	req *connect.Request[compassv1.RevokeTokenRequest],
) (*connect.Response[compassv1.RevokeTokenResponse], error) {
	token := req.Msg.GetToken()
	if token == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument,
			errors.New("token is required"))
	}
	if err := auth.RevokeToken(ctx, s.store, token); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, connect.NewError(connect.CodeNotFound,
				errors.New("no such token"))
		}
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	return connect.NewResponse(&compassv1.RevokeTokenResponse{}), nil
}

// SubscribeEvents snapshots the event ring then tails live updates until the
// client disconnects, the server shuts down, or the subscriber lags past the
// ring — the lag case emits a final ResyncRequired so the client re-snapshots.
//
// connect-go drives the stream inside this handler: it blocks until the handler
// returns. Both the replay drain and the live tail select on ctx (cancelled
// when the client hangs up), and the bus closing its Live channel is what lets a
// held-open stream end on server shutdown.
func (s *service) SubscribeEvents(
	ctx context.Context,
	req *connect.Request[compassv1.SubscribeEventsRequest],
	stream *connect.ServerStream[compassv1.SubscribeEventsResponse],
) error {
	sub, err := s.bus.Subscribe(req.Msg.GetSinceSeq(), req.Msg.GetInstanceEpoch())
	if err != nil {
		if errors.Is(err, events.ErrBufferUnderflow) {
			// Cursor can't be served gap-free: answer with a single terminal
			// ResyncRequired so the client re-snapshots at sinceSeq = 0.
			return stream.Send(resyncRequired(s.bus.InstanceEpoch()))
		}
		return connect.NewError(connect.CodeInternal, err)
	}
	// Free the fan-out slot on every exit path (client hang-up, lag, shutdown,
	// send error) rather than leaking it until an overrun or bus close. Safe
	// after the bus has already closed the channel — Cancel is idempotent.
	defer sub.Cancel()
	// On a fresh subscribe (since_seq==0) send one leading snapshot-boundary frame
	// before the tail: the ordering marker a client pairs with a ListBoardIssues
	// read (register -> boundary -> tail, gap-free). The board is unversioned in v1
	// so the boundary carries snapshot_seq=0; the client unions the read id-keyed.
	if req.Msg.GetSinceSeq() == 0 {
		if err := stream.Send(snapshotBoundary(s.bus.InstanceEpoch())); err != nil {
			return err
		}
	}
	return forward(ctx, sub, stream)
}

// SubscribeAgentSession streams one agent session's typed observation trace to a
// caller authorized to see it. It first resolves-and-authorizes in one step —
// RequireAgentSessionSubscriber walks the durable ownership chain (session_id ->
// agent_account_id -> home_channel_id) and checks the caller's membership on
// that home channel, returning the SAME not-found for an unknown session and a
// non-member so neither can probe session existence. Only past
// that gate does it subscribe to the session's live tail. It then sends one
// leading registration-ack frame (session_id only, no event, no transition)
// the instant the subscriber is registered server-side — mirroring
// SubscribeEvents' snapshotBoundary / SubscribeComms' commsSnapshotBoundary —
// so a caller's OpenSessionTail RoundTrip unblocks on REGISTRATION, before any
// relayed frame. This is still a live tail, not a snapshot: the ack carries no
// history (no replay/resync/reattach; the deferred daemon-lifecycle machinery
// is not in this increment). After the ack it forwards frames until the client
// disconnects, the session ends, or the subscriber lags past its buffer.
func (s *service) SubscribeAgentSession(
	ctx context.Context,
	req *connect.Request[compassv1.SubscribeAgentSessionRequest],
	stream *connect.ServerStream[compassv1.AgentSessionFrame],
) error {
	if s.tail == nil {
		return connect.NewError(connect.CodeUnavailable, errNoRunnerHub)
	}
	caller, ok := auth.CallerFrom(ctx)
	if !ok {
		// No caller in context is a door wiring bug (an interceptor must attach
		// one on both doors); fail closed rather than stream unauthorized.
		return connect.NewError(connect.CodeUnauthenticated, errNoCaller)
	}
	sessionID := req.Msg.GetSessionId()
	if err := s.store.RequireAgentSessionSubscriber(ctx, caller, sessionID); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			// Unknown session OR non-member — indistinguishable by contract, so
			// the caller cannot tell "exists but forbidden" from "does not exist".
			return connect.NewError(connect.CodeNotFound, fmt.Errorf("agent session %q", sessionID))
		}
		return connect.NewError(connect.CodeInternal, err)
	}
	sub := s.tail.subscribe(sessionID)
	// Free the fan-out slot on every exit path (client hang-up, session end,
	// lag-drop). Safe after the tail has already closed the channel on a
	// lag-drop — unsubscribe is a no-op once the sub is gone.
	defer s.tail.unsubscribe(sessionID, sub)
	// Leading registration-ack: flush one zero-payload frame (session_id only) to
	// unblock the client's OpenSessionTail RoundTrip on REGISTRATION rather than the
	// first relayed frame. Makes "open the tail before the post" a true
	// happens-before, closing the idle-session drop where an injection races subscribe().
	if err := stream.Send(&compassv1.AgentSessionFrame{SessionId: sessionID}); err != nil {
		return nil
	}
	for {
		select {
		case <-ctx.Done():
			return nil // client hung up or server shutting down
		case frame, ok := <-sub.ch:
			if !ok {
				return nil // lag-drop or session teardown: end the stream cleanly
			}
			if err := stream.Send(frame); err != nil {
				// Client hung up: a clean end on send error IS the contract.
				return nil
			}
		}
	}
}

// provisionAgent relays a Provision for the resolved agent acc and records its
// placement.
func (s *service) provisionAgent(
	ctx context.Context,
	acc store.Account,
	msg *compassv1.ProvisionAgentWorkspaceRequest,
) (*compassv1.ProvisionAgentWorkspaceResponse, error) {
	relay := proto.CloneOf(msg)
	// The Runner hop carries the resolved account id: the Runner, its container
	// bind, and the hub's dedup key all read agent_handle as an id.
	relay.AgentHandle = string(acc.ID)
	// SERVER-AUTHORITATIVE persona and role: overwrite whatever the client sent
	// with the store's values, so a caller cannot inject a system or role prompt.
	relay.Persona = acc.Agent.Persona
	relay.Role = acc.Agent.Role
	resp, runnerID, err := s.hub.Provision(ctx, relay.GetClientRequestId(), relay)
	if err != nil {
		return nil, err
	}
	// Record the agent's durable PLACEMENT — which Runner and container name — only
	// now that the Runner created the container. Idempotent upsert. This is what makes
	// the container->account mapping survive a Server restart, and what reattach
	// recovery reads to name agents stranded by a Runner restart.
	if err := s.store.RecordAgentPlacement(ctx, acc.ID, runnerID, resp.GetContainerName()); err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("recording agent placement: %w", err))
	}
	return resp, nil
}

// resolveQualifiedAgent resolves an `owner/agent` handle on the admin door, which
// has no agent session to default a bare handle's owner from. A bare handle and
// every miss return the same NotFound naming the submitted handle.
func (s *service) resolveQualifiedAgent(ctx context.Context, raw string) (store.Account, error) {
	qh := store.ParseQualifiedHandle(raw)
	if !qh.Qualified() {
		return store.Account{}, handleNotFound(raw)
	}
	owner, err := s.store.UserByHandle(ctx, qh.Owner)
	if err != nil {
		return store.Account{}, handleLookupError(raw, err)
	}
	acc, err := s.store.AgentByHandle(ctx, owner.ID, qh.Handle)
	if err != nil {
		return store.Account{}, handleLookupError(raw, err)
	}
	return acc, nil
}

// handleNotFound is the admin door's single miss for a submitted handle. It
// names the handle as sent and never a resolved id.
func handleNotFound(raw string) error {
	return connect.NewError(connect.CodeNotFound, fmt.Errorf("no account with handle %q", raw))
}

// handleLookupError maps a store resolver error: a miss or a malformed segment
// (an empty owner or agent part) is handleNotFound, anything else is Internal.
func handleLookupError(raw string, err error) error {
	if errors.Is(err, store.ErrNotFound) || errors.Is(err, store.ErrInvalidArgument) {
		return handleNotFound(raw)
	}
	return connect.NewError(connect.CodeInternal, fmt.Errorf("resolving handle %q: %w", raw, err))
}

// startResumeSession is the resume leg of StartAgentSession (T6, RIG-1667). It
// runs BEFORE any Runner call for the parts that must: (1) authorize the caller
// on the resumed session via RequireAgentSessionSubscriber — an unknown or
// foreign resume_session_id is one indistinguishable NotFound (the
// not-found/forbidden merge, D9), so a caller holding a foreign id cannot probe
// existence and NO Start is ever relayed; (2) BindLifetime write-once to
// snapshot the entry_seq rebase base for the new lifetime onto the stable
// logical session, so the new lifetime's agent-stamped frames rebase onto the
// stored maximum (a re-resume re-reads the same max — idempotent); (3)
// reconstruct the resume body from the durable transcript (T5); (4) relay the
// verbatim public start request with the reconstructed body attached to the
// INTERNAL envelope (hub.StartResume). The public request carries only the
// authz-checked resume_session_id — no locator, no body a client could forge.
func (s *service) startResumeSession(
	ctx context.Context,
	resumeSessionID string,
	req *compassv1.StartAgentSessionRequest,
) (*compassv1.StartAgentSessionResponse, error) {
	caller, ok := auth.CallerFrom(ctx)
	if !ok {
		// No caller in context is a door-wiring bug (an interceptor must attach
		// one on both doors); fail closed rather than resume unauthorized.
		return nil, connect.NewError(connect.CodeUnauthenticated, errNoCaller)
	}
	if err := s.store.RequireAgentSessionSubscriber(ctx, caller, resumeSessionID); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			// Unknown session OR non-member — indistinguishable by contract, so
			// the caller cannot tell "exists but forbidden" from "does not exist".
			return nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("agent session %q", resumeSessionID))
		}
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("authorizing resume: %w", err))
	}
	// Snapshot the rebase base for the new lifetime BEFORE the Runner emits any
	// frame for it. Write-once within a lifetime; a re-resume re-reads the same
	// stored max, so this is safe to call on every resume.
	if _, err := s.store.BindLifetime(ctx, resumeSessionID); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			// TOCTOU: the session vanished between the authz check and the bind.
			// Surface the same indistinguishable NotFound as the authz branch
			// above, not a 500.
			return nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("agent session %q", resumeSessionID))
		}
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("binding resume lifetime: %w", err))
	}
	body, err := s.hub.ReconstructSessionBody(ctx, resumeSessionID)
	if err != nil {
		return nil, err
	}
	return s.hub.StartResume(ctx, "", req, body)
}

// abandonStartedSession tears down a session StartAgentSession already started
// on the Runner but could not record, and returns the Internal error to fail
// with. Best-effort: the Stop's own failure must not mask the original cause,
// which is why the returned error NAMES the session id — a session the store
// never recorded and Stop could not kill is only reapable by an operator who
// can see its id.
//
// The teardown runs on context.WithoutCancel(ctx): the caller's context may
// already be cancelled (a client that gave up is one plausible reason the store
// write failed at all), and the session is live regardless — the same reasoning
// AgentRuntime.Launch uses to remove a half-provisioned container
// (internal/runtime/agent.go). The dispatch path has no independent deadline, so
// the Stop is bounded here by rollbackStopTimeout — StartAgentSession returns
// within that bound even against a Runner that accepts Stop but never answers.
func (s *service) abandonStartedSession(ctx context.Context, containerName, sessionID string, cause error) error {
	stopErr := errNoSessionID
	if sessionID != "" {
		stopCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), rollbackStopTimeout)
		defer cancel()
		_, stopErr = s.hub.Stop(stopCtx, "", &compassv1.StopAgentSessionRequest{SessionId: sessionID})
	}
	if stopErr != nil {
		slog.ErrorContext(ctx, "started agent session could not be recorded or stopped; it is running unreapable",
			"session_id", sessionID, "container_name", containerName, "cause", cause, "stop_error", stopErr)
	} else {
		slog.ErrorContext(ctx, "started agent session could not be recorded; stopped it to avoid stranding",
			"session_id", sessionID, "container_name", containerName, "cause", cause)
	}
	return connect.NewError(connect.CodeInternal, fmt.Errorf("%w (started session %q was rolled back)", cause, sessionID))
}

// forward drains the replay snapshot (oldest first), then forwards the live tail
// until the client disconnects, the server shuts down, or the subscriber lags
// past the ring. The lag case emits a final ResyncRequired. Both phases select
// on ctx so a shutdown or client hang-up mid-replay returns promptly rather than
// stalling graceful drain.
func forward(
	ctx context.Context,
	sub events.Subscription[busPayload],
	stream *connect.ServerStream[compassv1.SubscribeEventsResponse],
) error {
	for _, event := range sub.Replay {
		select {
		case <-ctx.Done():
			return nil // client hung up or server shutting down mid-replay
		default:
		}
		if err := stream.Send(toResponse(event)); err != nil {
			return nil //nolint:nilerr // client hung up mid-replay: ending the stream cleanly on a send error IS the contract, so the non-nil send error is intentionally not propagated
		}
	}

	for {
		select {
		case <-ctx.Done():
			return nil // client hung up or server shutting down
		case event, ok := <-sub.Live:
			if !ok {
				// Live closed: an overrun (emit terminal resync) vs a clean bus
				// shutdown (end silently).
				if sub.Lagged() {
					_ = stream.Send(resyncRequired(sub.Epoch))
				}
				return nil
			}
			if err := stream.Send(toResponse(event)); err != nil {
				return nil //nolint:nilerr // client hung up: ending the stream cleanly on a send error IS the contract, so the non-nil send error is intentionally not propagated
			}
		}
	}
}

// toResponse maps the bus's Stamped envelope onto the concrete SubscribeEvents
// response at the stream edge: the seq/at_unix_ms/instance_epoch transfer from
// the envelope and the payload oneof comes from the stamped message.
func toResponse(event events.Stamped[busPayload]) *compassv1.SubscribeEventsResponse {
	return &compassv1.SubscribeEventsResponse{
		Seq:           event.Seq,
		AtUnixMs:      event.AtUnixMS,
		InstanceEpoch: event.InstanceEpoch,
		Payload:       event.Payload.GetPayload(),
	}
}

// resyncRequired is the typed resync signal: the last event the server sends
// before it closes a stream whose cursor it can no longer serve gap-free.
func resyncRequired(instanceEpoch uint64) *compassv1.SubscribeEventsResponse {
	return &compassv1.SubscribeEventsResponse{
		Seq:           resyncSeq,
		AtUnixMs:      time.Now().UnixMilli(),
		InstanceEpoch: instanceEpoch,
		Payload: &compassv1.SubscribeEventsResponse_ResyncRequired{
			ResyncRequired: &compassv1.ResyncRequired{},
		},
	}
}

// snapshotBoundary is the leading control frame a since_seq==0 SubscribeEvents
// subscribe receives before any tail event: the subscribe-first ordering marker
// a client pairs with a ListBoardIssues read (register -> boundary -> tail,
// gap-free), mirroring commsSnapshotBoundary. It is a control frame, not a
// positioned event — Seq=0, no payload — so a client discriminates it from a
// positioned event by its zero seq and absent payload, and from the terminal
// resync (also Seq=0) by payload: this frame has none, the resync carries a
// ResyncRequired. The board is unversioned in v1, so snapshot_seq is 0: the
// client unions the read with the full tail id-keyed to close the window, not a
// seq bound. instance_epoch is the bus epoch so a client pairs the boundary
// with the seq space it belongs to.
func snapshotBoundary(instanceEpoch uint64) *compassv1.SubscribeEventsResponse {
	return &compassv1.SubscribeEventsResponse{
		Seq:           resyncSeq,
		AtUnixMs:      time.Now().UnixMilli(),
		InstanceEpoch: instanceEpoch,
		SnapshotSeq:   0,
	}
}
