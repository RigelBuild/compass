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

	"connectrpc.com/connect/v2"
	"google.golang.org/protobuf/proto"

	"github.com/RigelBuild/compass/go/events"
	compassv1 "github.com/RigelBuild/compass/go/gen/compass/v1"
	"github.com/RigelBuild/compass/go/gen/compass/v1/compassv1connect"
	"github.com/RigelBuild/compass/go/internal/auth"
	"github.com/RigelBuild/compass/go/internal/board"
	compassv1internal "github.com/RigelBuild/compass/go/internal/gen/compass/v1"
	"github.com/RigelBuild/compass/go/internal/otel"
	"github.com/RigelBuild/compass/go/internal/runnerhub"
	"github.com/RigelBuild/compass/go/internal/store"
)

// apiVersion is the compass.v1 contract version reported by GetServerInfo.
const apiVersion = "compass.v1"

// resyncSeq marks a control signal, not a positioned event; clients discard
// their cursor and reconnect at sinceSeq = 0.
const resyncSeq uint64 = 0

// rollbackStopTimeout bounds cleanup when recording a started session fails;
// an unresponsive Runner could otherwise stall the RPC and graceful shutdown.
var rollbackStopTimeout = 30 * time.Second

// errNoRunnerHub is returned by the container-lifecycle RPCs when no Runner door
// is mounted (the socket-only path) — the RPC is Unavailable, never a panic.
var errNoRunnerHub = errors.New("no runner hub configured on this server")

// errNoCaller marks missing interceptor identity on SubscribeAgentSession;
// fail closed rather than stream an unauthorized session.
var errNoCaller = errors.New("no caller identity in request context")

// errNoSessionID means the Runner started without returning an addressable
// session, so cleanup can only log the failure.
var errNoSessionID = errors.New("runner returned no session id to stop")

// busPayload is the event type. Since generated oneof interfaces are
// unexported, the bus carries the response payload and stamps envelope fields
// onto a copy at the stream edge.
type busPayload = *compassv1.SubscribeEventsResponse

type accountRenamePublisher interface {
	PublishAccountRename(ctx context.Context, account store.Account, channelIDs []store.ChannelID)
}

// service implements compass.v1. The store backs IssueToken; the optional
// runner hub routes container lifecycle calls and is nil on socket-only servers.
type service struct {
	compassv1connect.UnimplementedCompassServiceHandler
	version string
	rev     string
	bus     *events.Bus[busPayload]
	store   *store.Store
	hub     *runnerhub.Hub
	// board is the Bridge projection used by GetAgentStatus and the lifecycle sink.
	board *board.Projection
	// issueBrd is the server-authoritative projection for ListBoardIssues,
	// shared with ingestion and the live event tail.
	issueBrd *board.IssueProjection
	// tail shares the RunnerHub's per-session fan-out; serve.go wires the same
	// instance to both. nil without a Runner door, making the RPC unavailable.
	tail           *sessionTail
	accountRenamer accountRenamePublisher
	adminID        store.AccountID
	// refLinkRules provides the configured forge-link rules; nil returns none.
	refLinkRules *refLinkRuleSource
	// signaler notifies live sessions after config writes so Runners re-fetch.
	// It is optional when no Runner door is mounted.
	signaler configSignaler
	// spawnMu protects the composite memo; retries join the same Provision→Start.
	spawnMu sync.Mutex
	spawns  map[spawnKey]*spawnCall
	// scheduleAfter schedules delayed memo eviction; nil uses time.AfterFunc.
	// Tests override it to drive eviction without wall time.
	scheduleAfter func(time.Duration, func()) *time.Timer
}

func newService(version string, bus *events.Bus[busPayload], st *store.Store, hub *runnerhub.Hub, brd *board.Projection, issueBrd *board.IssueProjection, tail *sessionTail) *service {
	// Assign through a concrete-nil guard so socket-only servers retain a nil
	// interface and the handler's nil check works.
	var sig configSignaler
	if hub != nil {
		sig = hub
	}
	return &service{version: version, bus: bus, store: st, hub: hub, board: brd, issueBrd: issueBrd, tail: tail, signaler: sig}
}

// ProvisionAgentWorkspace provisions an isolated per-agent container through
// the owning Runner. A client_request_id makes retries join the same call and
// return the original container rather than provisioning another.
func (s *service) ProvisionAgentWorkspace(
	ctx context.Context,
	req *compassv1.ProvisionAgentWorkspaceRequest,
) (*compassv1.ProvisionAgentWorkspaceResponse, error) {
	if s.hub == nil {
		return nil, connect.NewError(connect.CodeUnavailable, errNoRunnerHub.Error()).WithCause(errNoRunnerHub)
	}
	acc, err := resolveQualifiedAgent(ctx, s.store, req.GetAgentHandle())
	if err != nil {
		return nil, err
	}
	resp, err := s.provisionAgent(ctx, acc, req)
	if err != nil {
		return nil, err
	}
	return resp, nil
}

// StartAgentSession relays a fresh start, then records session ownership.
// The request has no client_request_id, so the relay ID is server-minted.
// A server without a Runner hub returns Unavailable.
func (s *service) StartAgentSession(
	ctx context.Context,
	req *compassv1.StartAgentSessionRequest,
) (*compassv1.StartAgentSessionResponse, error) {
	if s.hub == nil {
		return nil, connect.NewError(connect.CodeUnavailable, errNoRunnerHub.Error()).WithCause(errNoRunnerHub)
	}
	// Resume authorizes and reconstructs before the Runner call; its existing
	// session ownership is reused. A fresh start mints and records a new ID.
	if resumeID := req.GetResumeSessionId(); resumeID != "" {
		resp, err := s.startResumeSession(ctx, resumeID, req)
		if err != nil {
			return nil, err
		}
		return resp, nil
	}

	resp, err := s.hub.Start(ctx, "", req)
	if err != nil {
		return nil, err
	}
	// Record the durable session-to-agent ownership after Runner start. If either
	// store step fails, the live session cannot self-heal and must be stopped.
	agentAccountID, err := s.store.AgentForContainer(ctx, req.GetContainerName())
	if err != nil {
		return nil, s.abandonStartedSession(ctx, req.GetContainerName(), resp.GetSessionId(),
			fmt.Errorf("resolving agent for container: %w", err))
	}
	if err := s.store.RecordAgentSession(ctx, resp.GetSessionId(), agentAccountID); err != nil {
		return nil, s.abandonStartedSession(ctx, req.GetContainerName(), resp.GetSessionId(),
			fmt.Errorf("recording agent session ownership: %w", err))
	}
	return resp, nil
}

// StopAgentSession relays an idempotent stop to the owning Runner. Unknown or
// already-stopped sessions succeed under the established contract.
func (s *service) StopAgentSession(
	ctx context.Context,
	req *compassv1.StopAgentSessionRequest,
) (*compassv1.StopAgentSessionResponse, error) {
	if s.hub == nil {
		return nil, connect.NewError(connect.CodeUnavailable, errNoRunnerHub.Error()).WithCause(errNoRunnerHub)
	}
	resp, err := s.hub.Stop(ctx, "", req)
	if err != nil {
		return nil, err
	}
	// Stop already killed the agent; archive failure must not turn success into failure.
	archiveSessionEnd(ctx, s.store, req.GetSessionId())
	return resp, nil
}

// SkipBatchWindow fires the agent's pending idle batch immediately. The owner
// or an admin must authorize before the control is relayed.
func (s *service) SkipBatchWindow(
	ctx context.Context,
	req *compassv1.SkipBatchWindowRequest,
) (*compassv1.SkipBatchWindowResponse, error) {
	if s.hub == nil {
		return nil, connect.NewError(connect.CodeUnavailable, errNoRunnerHub.Error()).WithCause(errNoRunnerHub)
	}
	caller, ok := auth.CallerFrom(ctx)
	if !ok {
		return nil, connect.NewError(connect.CodeUnauthenticated, errNoCaller.Error()).WithCause(errNoCaller)
	}
	sessionID := req.GetSessionId()
	if sessionID == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, "session_id is required")
	}
	if err := s.store.RequireAgentSessionOwner(ctx, caller, sessionID); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, connect.Errorf(connect.CodeNotFound, "agent session %q", sessionID)
		}
		if errors.Is(err, store.ErrInvalidArgument) {
			return nil, connect.NewError(connect.CodeInvalidArgument, err.Error()).WithCause(err)
		}
		return nil, connect.NewError(connect.CodeInternal, err.Error()).WithCause(err)
	}
	if err := s.hub.DispatchControl(ctx, sessionID, &compassv1internal.AgentControl{
		Control: &compassv1internal.AgentControl_StartNow{
			StartNow: &compassv1internal.StartNowControl{},
		},
	}); err != nil {
		return nil, connect.NewError(connect.CodeUnavailable, err.Error()).WithCause(err)
	}
	return &compassv1.SkipBatchWindowResponse{}, nil
}

// ReloadAgentSession tears down the current agent exec and starts a fresh one
// against the same container, reusing the session id so the board entry is
// continuous — relayed to the owning Runner, which owns the teardown-then-start.
func (s *service) ReloadAgentSession(
	ctx context.Context,
	req *compassv1.ReloadAgentSessionRequest,
) (*compassv1.ReloadAgentSessionResponse, error) {
	if s.hub == nil {
		return nil, connect.NewError(connect.CodeUnavailable, errNoRunnerHub.Error()).WithCause(errNoRunnerHub)
	}
	resp, err := s.hub.Reload(ctx, "", req)
	if err != nil {
		return nil, err
	}
	return resp, nil
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
	req *compassv1.RemoveAgentWorkspaceRequest,
) (*compassv1.RemoveAgentWorkspaceResponse, error) {
	if s.hub == nil {
		return nil, connect.NewError(connect.CodeUnavailable, errNoRunnerHub.Error()).WithCause(errNoRunnerHub)
	}
	resp, err := s.hub.Remove(ctx, req.GetClientRequestId(), req)
	if err != nil {
		return nil, err
	}
	if err := s.store.DeleteAgentPlacement(ctx, req.GetContainerName()); err != nil {
		wrappedErr := fmt.Errorf("releasing agent placement: %w", err)
		return nil, connect.NewError(connect.CodeInternal, wrappedErr.Error()).WithCause(wrappedErr)
	}
	return resp, nil
}

// GetAgentStatus returns the Bridge board's snapshot of agent-session state: one
// session when session_id is set, else every live session. It reads the
// Server-side projection, not a live relay to the Runner. A nil board (never the
// real serving path — serve.go always builds one) answers empty rather than
// panicking.
func (s *service) GetAgentStatus(
	_ context.Context,
	req *compassv1.GetAgentStatusRequest,
) (*compassv1.GetAgentStatusResponse, error) {
	enrolled := s.hub != nil && s.hub.RunnerEnrolled()
	if s.board == nil {
		return &compassv1.GetAgentStatusResponse{RunnerEnrolled: &enrolled}, nil
	}
	return &compassv1.GetAgentStatusResponse{
		Statuses:       s.board.Snapshot(req.GetSessionId()),
		RunnerEnrolled: &enrolled,
	}, nil
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
	_ *compassv1.ListBoardIssuesRequest,
) (*compassv1.ListBoardIssuesResponse, error) {
	if s.issueBrd == nil {
		return &compassv1.ListBoardIssuesResponse{}, nil
	}
	return &compassv1.ListBoardIssuesResponse{
		Issues: s.issueBrd.Snapshot(),
	}, nil
}

// SearchIssues searches the tenant-scoped board corpus and returns wire issues
// in relevance order. Empty queries are rejected by the store as invalid.
func (s *service) SearchIssues(
	ctx context.Context,
	req *compassv1.SearchIssuesRequest,
) (*compassv1.SearchIssuesResponse, error) {
	issues, err := s.store.SearchIssues(ctx, req.GetQuery(), req.GetLimit())
	if err != nil {
		if errors.Is(err, store.ErrInvalidArgument) {
			return nil, connect.NewError(connect.CodeInvalidArgument, err.Error()).WithCause(err)
		}
		return nil, connect.NewError(connect.CodeInternal, err.Error()).WithCause(err)
	}
	out, err := s.issueBrd.IssueToProtoWithPrs(ctx, issues...)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err.Error()).WithCause(err)
	}
	return &compassv1.SearchIssuesResponse{Issues: out}, nil
}

// GetServerInfo is the connect-time liveness, version, and Runner enrollment probe.
func (s *service) GetServerInfo(
	_ context.Context,
	_ *compassv1.GetServerInfoRequest,
) (*compassv1.GetServerInfoResponse, error) {
	var enrolledRunners []*compassv1.EnrolledRunner
	if s.hub != nil {
		enrolledRunners = s.hub.EnrolledRunners()
	}
	return &compassv1.GetServerInfoResponse{
		Version:         s.version,
		ApiVersion:      apiVersion,
		Rev:             s.rev,
		EnrolledRunners: enrolledRunners,
	}, nil
}

// WhoAmI reflects the caller's own account id, resolved server-side from its
// authenticated credential (DL-111) — embedded (socket) yields the ambient-admin
// identity, native-client (network door) the bearer's subject. The id is never a
// client-supplied field; it comes solely from the caller identity the door's
// interceptors attached. A missing caller means an interceptor-wiring bug (both
// doors must attach one): fail closed with Unauthenticated.
func (s *service) WhoAmI(
	ctx context.Context,
	_ *compassv1.WhoAmIRequest,
) (*compassv1.WhoAmIResponse, error) {
	caller, ok := auth.CallerFrom(ctx)
	if !ok {
		return nil, connect.NewError(connect.CodeUnauthenticated, errNoCaller.Error()).WithCause(errNoCaller)
	}
	return &compassv1.WhoAmIResponse{AccountId: string(caller)}, nil
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
	req *compassv1.IssueTokenRequest,
) (*compassv1.IssueTokenResponse, error) {
	raw := req.GetAccountHandle()
	if raw == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument,
			"account_handle is required")
	}
	// The admin door is not visibility-scoped (no D9 clip): the admin may name any account.
	var acc store.Account
	var err error
	if qh := store.ParseQualifiedHandle(raw); qh.Qualified() {
		acc, err = resolveQualifiedAgent(ctx, s.store, raw)
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
		return nil, connect.Errorf(connect.CodePermissionDenied,
			"account %q is the system account and cannot be issued a token", raw)
	}
	token, err := auth.IssueAccountToken(ctx, s.store, id)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err.Error()).WithCause(err)
	}
	return &compassv1.IssueTokenResponse{Token: token}, nil
}

// RenameAccount changes an account's handle through the admin-only operator door.
func (s *service) RenameAccount(
	ctx context.Context,
	req *compassv1.RenameAccountRequest,
) (*compassv1.RenameAccountResponse, error) {
	raw := req.GetAccountHandle()
	newHandle := req.GetNewHandle()
	if raw == "" || newHandle == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, "account_handle and new_handle are required")
	}
	qh := store.ParseQualifiedHandle(raw)
	if qh.Malformed() {
		return nil, connect.NewError(connect.CodeInvalidArgument, "account_handle has invalid qualified-handle syntax")
	}
	var acc store.Account
	var err error
	if qh.Qualified() {
		acc, err = resolveQualifiedAgent(ctx, s.store, raw)
	} else if acc, err = s.store.UserByHandle(ctx, raw); err != nil {
		err = handleLookupError(raw, err)
	}
	if err != nil {
		return nil, err
	}
	if acc.System != nil {
		return nil, connect.Errorf(connect.CodePermissionDenied,
			"account %q is the system account and cannot be renamed", raw)
	}

	if acc.ID == s.adminID || isAdminRootSupervisor(acc, s.adminID) {
		return nil, connect.Errorf(connect.CodeFailedPrecondition,
			"account %q is a protected bootstrap account", raw)
	}
	renamed, rekeyedChannels, err := s.store.RenameHandle(ctx, acc.ID, newHandle)
	if err != nil {
		switch {
		case errors.Is(err, store.ErrInvalidArgument):
			return nil, connect.NewError(connect.CodeInvalidArgument, err.Error()).WithCause(err)
		case errors.Is(err, store.ErrConflict):
			return nil, connect.NewError(connect.CodeAlreadyExists, err.Error()).WithCause(err)
		case errors.Is(err, store.ErrNotFound):
			return nil, connect.NewError(connect.CodeNotFound, err.Error()).WithCause(err)
		default:
			return nil, connect.NewError(connect.CodeInternal, err.Error()).WithCause(err)
		}
	}
	if s.accountRenamer != nil {
		s.accountRenamer.PublishAccountRename(ctx, renamed, rekeyedChannels)
	}
	handle := renamed.Handle
	if renamed.Agent != nil {
		owner, err := s.store.GetAccount(ctx, renamed.Agent.OwnerUserID)
		if err != nil {
			return nil, connect.NewError(connect.CodeInternal, err.Error()).WithCause(err)
		}
		handle = owner.Handle + "/" + renamed.Handle
	}
	return &compassv1.RenameAccountResponse{
		AccountId: string(renamed.ID),
		Handle:    handle,
	}, nil
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
	req *compassv1.RevokeTokenRequest,
) (*compassv1.RevokeTokenResponse, error) {
	token := req.GetToken()
	if token == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument,
			"token is required")
	}
	if err := auth.RevokeToken(ctx, s.store, token); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, connect.NewError(connect.CodeNotFound,
				"no such token")
		}
		return nil, connect.NewError(connect.CodeInternal, err.Error()).WithCause(err)
	}
	return &compassv1.RevokeTokenResponse{}, nil
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
	req *compassv1.SubscribeEventsRequest,
	stream compassv1connect.CompassServiceSubscribeEventsServerStream,
) error {
	ctx = otel.WithoutSpan(ctx)
	sub, err := s.bus.Subscribe(req.GetSinceSeq(), req.GetInstanceEpoch())
	if err != nil {
		if errors.Is(err, events.ErrBufferUnderflow) {
			// Cursor can't be served gap-free: answer with a single terminal
			// ResyncRequired so the client re-snapshots at sinceSeq = 0.
			return stream.Send(resyncRequired(s.bus.InstanceEpoch()))
		}
		return connect.NewError(connect.CodeInternal, err.Error()).WithCause(err)
	}
	// Free the fan-out slot on every exit path (client hang-up, lag, shutdown,
	// send error) rather than leaking it until an overrun or bus close. Safe
	// after the bus has already closed the channel — Cancel is idempotent.
	defer sub.Cancel()
	// On a fresh subscribe (since_seq==0) send one leading snapshot-boundary frame
	// before the tail: the ordering marker a client pairs with a ListBoardIssues
	// read (register -> boundary -> tail, gap-free). The board is unversioned in v1
	// so the boundary carries snapshot_seq=0; the client unions the read id-keyed.
	if req.GetSinceSeq() == 0 {
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
	req *compassv1.SubscribeAgentSessionRequest,
	stream compassv1connect.CompassServiceSubscribeAgentSessionServerStream,
) error {
	ctx = otel.WithoutSpan(ctx)
	if s.tail == nil {
		return connect.NewError(connect.CodeUnavailable, errNoRunnerHub.Error()).WithCause(errNoRunnerHub)
	}
	caller, ok := auth.CallerFrom(ctx)
	if !ok {
		// No caller in context is a door wiring bug (an interceptor must attach
		// one on both doors); fail closed rather than stream unauthorized.
		return connect.NewError(connect.CodeUnauthenticated, errNoCaller.Error()).WithCause(errNoCaller)
	}
	sessionID := req.GetSessionId()
	if err := s.store.RequireAgentSessionSubscriber(ctx, caller, sessionID); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			// Unknown session OR non-member — indistinguishable by contract, so
			// the caller cannot tell "exists but forbidden" from "does not exist".
			return connect.Errorf(connect.CodeNotFound, "agent session %q", sessionID)
		}
		return connect.NewError(connect.CodeInternal, err.Error()).WithCause(err)
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

func (s *service) withAccountRenamer(p accountRenamePublisher, adminID store.AccountID) *service {
	s.accountRenamer = p
	s.adminID = adminID
	return s
}

// withRev sets the build git rev GetServerInfo reports; only Serve stamps it.
func (s *service) withRev(rev string) *service {
	s.rev = rev
	return s
}

// provisionAgent relays a Provision for the resolved agent acc and records its
// placement.
func (s *service) provisionAgent(
	ctx context.Context,
	acc store.Account,
	msg *compassv1.ProvisionAgentWorkspaceRequest,
) (*compassv1.ProvisionAgentWorkspaceResponse, error) {
	relay := proto.CloneOf(msg)
	// SERVER-AUTHORITATIVE persona and role: overwrite whatever the client sent
	// with the store's values, so a caller cannot inject a system or role prompt.
	relay.Persona = acc.Agent.Persona
	relay.Role = acc.Agent.Role
	resp, runnerID, err := s.hub.Provision(ctx, relay.GetClientRequestId(), acc.ID, relay)
	if err != nil {
		return nil, err
	}
	// Record durable placement after Provision so the container mapping survives
	// Server restarts and Runner reattach recovery.
	if err := s.store.RecordAgentPlacement(ctx, acc.ID, runnerID, resp.GetContainerName()); err != nil {
		wrappedErr := fmt.Errorf("recording agent placement: %w", err)
		return nil, connect.NewError(connect.CodeInternal, wrappedErr.Error()).WithCause(wrappedErr)
	}
	return resp, nil
}

// resolveQualifiedAgent resolves an `owner/agent` handle without a session owner.
// Every miss returns the same NotFound naming the submitted handle.
func resolveQualifiedAgent(ctx context.Context, st *store.Store, raw string) (store.Account, error) {
	qh := store.ParseQualifiedHandle(raw)
	if !qh.Qualified() || qh.Malformed() {
		return store.Account{}, handleNotFound(raw)
	}
	owner, err := st.UserByHandle(ctx, qh.Owner)
	if err != nil {
		return store.Account{}, handleLookupError(raw, err)
	}
	acc, err := st.AgentByHandle(ctx, owner.ID, qh.Handle)
	if err != nil {
		return store.Account{}, handleLookupError(raw, err)
	}
	return acc, nil
}

// handleNotFound is the admin door's single miss for a submitted handle. It
// names the handle as sent and never a resolved id.
func handleNotFound(raw string) error {
	return connect.Errorf(connect.CodeNotFound, "no account with handle %q", raw)
}

// handleLookupError maps a store resolver error: a miss or a malformed segment
// (an empty owner or agent part) is handleNotFound, anything else is Internal.
func handleLookupError(raw string, err error) error {
	if errors.Is(err, store.ErrNotFound) || errors.Is(err, store.ErrInvalidArgument) {
		return handleNotFound(raw)
	}
	wrappedErr := fmt.Errorf("resolving handle %q: %w", raw, err)
	return connect.NewError(connect.CodeInternal, wrappedErr.Error()).WithCause(wrappedErr)
}

// startResumeSession authorizes before Runner calls, reconstructs the durable
// transcript, then relays the request with its body in the internal envelope.
func (s *service) startResumeSession(
	ctx context.Context,
	resumeSessionID string,
	req *compassv1.StartAgentSessionRequest,
) (*compassv1.StartAgentSessionResponse, error) {
	caller, ok := auth.CallerFrom(ctx)
	if !ok {
		// Missing door identity is a wiring bug; fail closed before resume.
		return nil, connect.NewError(connect.CodeUnauthenticated, errNoCaller.Error()).WithCause(errNoCaller)
	}
	if err := s.store.RequireAgentSessionSubscriber(ctx, caller, resumeSessionID); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			// Hide unknown sessions and non-members as NotFound.
			return nil, connect.Errorf(connect.CodeNotFound, "agent session %q", resumeSessionID)
		}
		wrappedErr := fmt.Errorf("authorizing resume: %w", err)
		return nil, connect.NewError(connect.CodeInternal, wrappedErr.Error()).WithCause(wrappedErr)
	}
	body, err := s.hub.ReconstructSessionBody(ctx, resumeSessionID)
	if err != nil {
		return nil, err
	}
	return s.hub.StartResume(ctx, "", req, body)
}

// abandonStartedSession best-effort stops a started session whose ownership
// could not be recorded. Bounded context.WithoutCancel cleanup names the session
// for an operator if Stop fails; caller cancellation cannot stop it.
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
	wrappedErr := fmt.Errorf("%w (started session %q was rolled back)", cause, sessionID)
	return connect.NewError(connect.CodeInternal, wrappedErr.Error()).WithCause(wrappedErr)
}

// forward drains replay then the live tail until disconnect, shutdown, or lag;
// lag emits ResyncRequired and cancellation interrupts replay.
func forward(
	ctx context.Context,
	sub events.Subscription[busPayload],
	stream compassv1connect.CompassServiceSubscribeEventsServerStream,
) error {
	for _, event := range sub.Replay {
		select {
		case <-ctx.Done():
			return nil // client hung up or server shutting down mid-replay
		default:
		}
		if err := stream.Send(toResponse(event)); err != nil {
			slog.DebugContext(ctx, "event stream replay send stopped", "err", err)
			return nil // The client hung up mid-replay; ending the stream cleanly is the contract.
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
					if err := stream.Send(resyncRequired(sub.Epoch)); err != nil {
						slog.DebugContext(ctx, "event stream resync send stopped", "err", err)
					}
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

// resyncRequired is the final control frame when the server cannot serve a
// cursor gap-free.
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

// snapshotBoundary starts a since_seq==0 subscription. Seq=0 and no payload
// distinguish it from positioned events and ResyncRequired. The unversioned
// board requires clients to union the snapshot with the full tail; epoch selects
// the bus sequence space.
func snapshotBoundary(instanceEpoch uint64) *compassv1.SubscribeEventsResponse {
	return &compassv1.SubscribeEventsResponse{
		Seq:           resyncSeq,
		AtUnixMs:      time.Now().UnixMilli(),
		InstanceEpoch: instanceEpoch,
		SnapshotSeq:   0,
	}
}
