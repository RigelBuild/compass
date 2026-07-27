//go:build unix

// The CompassService implementation — the server side of the compass.v1
// contract. GetServerInfo is the connect-time liveness/version probe;
// SubscribeEvents snapshots the event ring then tails live updates.
//
// The agent-session lifecycle mutators (StartAgentSession, StopAgentSession,
// ReloadAgentSession) relay to the owning Runner over the RunnerHub; a server
// built with no Runner door (hub nil, the socket-only path) returns Unavailable
// for them. GetAgentStatus is served from the Bridge board projection (the
// session snapshot), not a Runner relay — the board is the writer the RunnerHub
// feeds and the reader this service snapshots. IssueToken is served here for the
// network door (T3): it verifies the target account against the store, then
// mints a bearer token in the token store.
package server

import (
	"context"
	"errors"
	"fmt"
	"time"

	"connectrpc.com/connect"

	"github.com/sealedsecurity/compass/go/events"
	compassv1 "github.com/sealedsecurity/compass/go/gen/compass/v1"
	"github.com/sealedsecurity/compass/go/gen/compass/v1/compassv1connect"
	"github.com/sealedsecurity/compass/go/internal/auth"
	"github.com/sealedsecurity/compass/go/internal/board"
	"github.com/sealedsecurity/compass/go/internal/runnerhub"
	"github.com/sealedsecurity/compass/go/internal/store"
)

// apiVersion is the compass.v1 contract version reported by GetServerInfo.
const apiVersion = "compass.v1"

// resyncSeq is the seq on a ResyncRequired: it is a control signal, not a
// positioned event, and the client discards its cursor to reconnect at
// sinceSeq = 0, so it carries no meaningful position.
const resyncSeq uint64 = 0

// errNoRunnerHub is returned by the container-lifecycle RPCs when no Runner door
// is mounted (the socket-only path) — the RPC is Unavailable, never a panic.
var errNoRunnerHub = errors.New("no runner hub configured on this server")

// errNoCaller is returned by SubscribeAgentSession when no caller identity is in
// context — an interceptor-wiring bug (both doors must attach one). Fail closed
// with Unauthenticated rather than stream an unauthorized session.
var errNoCaller = errors.New("no caller identity in request context")

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
	// tail is the per-session SubscribeAgentSession fan-out (sessiontail.go). The
	// same instance is the RunnerHub's session-tail sink (serve.go wires both),
	// so a frame the hub relays reaches this service's stream subscribers. nil on
	// a server built with no Runner door (SubscribeAgentSession then Unavailable,
	// like the other Runner-backed RPCs).
	tail *sessionTail
}

func newService(version string, bus *events.Bus[busPayload], st *store.Store, hub *runnerhub.Hub, brd *board.Projection, tail *sessionTail) *service {
	return &service{version: version, bus: bus, store: st, hub: hub, board: brd, tail: tail}
}

// ProvisionAgentWorkspace creates the isolated per-agent container for a
// workstream by relaying to the owning Runner (Client -> Server -> RunnerHub ->
// Runner -> AgentRuntime façade); the Server holds no container-engine code. The
// request's client_request_id (when set) is the OQ6 idempotency key threaded to
// the RunnerHub as the correlation id: a timeout-retry with the same id joins
// the in-flight/completed call and returns the same container_name rather than
// provisioning a second container.
func (s *service) ProvisionAgentWorkspace(
	ctx context.Context,
	req *connect.Request[compassv1.ProvisionAgentWorkspaceRequest],
) (*connect.Response[compassv1.ProvisionAgentWorkspaceResponse], error) {
	if s.hub == nil {
		return nil, connect.NewError(connect.CodeUnavailable, errNoRunnerHub)
	}
	resp, err := s.hub.Provision(ctx, req.Msg.GetClientRequestId(), req.Msg)
	if err != nil {
		return nil, err
	}
	// Record the container_name -> agent_account_id link of the durable
	// ownership chain, only now that the Runner has created the
	// container. agent_account_id is the request field; container_name is the
	// server-minted response — so the row is rooted on a value the client cannot
	// forge. Idempotent, matching the client_request_id provision-retry contract.
	if err := s.store.RecordAgentContainer(ctx, resp.GetContainerName(), store.AccountID(req.Msg.GetAgentAccountId())); err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("recording agent container ownership: %w", err))
	}
	return connect.NewResponse(resp), nil
}

// StartAgentSession brings the first-party agent in a provisioned container
// online by relaying to the owning Runner, which starts the agent over its
// streaming-exec bridge and returns the server-side session id — the cursor for
// Stop/Reload/GetAgentStatus and the attribution id on every agent payload.
// StartAgentSessionRequest carries no client_request_id (unlike Provision), so
// the relay request id is Server-minted per call; client-retry idempotency is
// not part of Start's frozen contract. hub is nil only on a server built with no
// Runner door, where a lifecycle RPC is Unavailable rather than a panic.
func (s *service) StartAgentSession(
	ctx context.Context,
	req *connect.Request[compassv1.StartAgentSessionRequest],
) (*connect.Response[compassv1.StartAgentSessionResponse], error) {
	if s.hub == nil {
		return nil, connect.NewError(connect.CodeUnavailable, errNoRunnerHub)
	}
	resp, err := s.hub.Start(ctx, "", req.Msg)
	if err != nil {
		return nil, err
	}
	// Bind the session_id -> container_name link of the durable ownership chain
	// now that the Runner has started the session. container_name
	// is the request handle; session_id is the server-minted response. Completes
	// the chain SubscribeAgentSession resolves to authorize a subscriber.
	if err := s.store.RecordAgentSession(ctx, resp.GetSessionId(), req.Msg.GetContainerName()); err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("recording agent session ownership: %w", err))
	}
	return connect.NewResponse(resp), nil
}

// StopAgentSession deliberately kills the in-container agent and releases the
// session by relaying to the owning Runner. Idempotent per the frozen contract
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

// IssueToken mints a bearer token for an existing account. On the network door it
// is admin-only: the AdminGate classifies IssueToken as adminOnly, so only the
// bootstrap admin reaches this handler. On the shipped socket door it is served
// ungated alongside every other method — that door mounts no interceptors, since
// the 0600 socket is itself the local admin credential — so the token minted here
// is valid on the network door (no capability beyond the bootstrap-admin token
// file the same local peer already owns). It verifies the account exists in the
// store, then mints a token in the token store and returns it once; the store
// retains only its hash.
func (s *service) IssueToken(
	ctx context.Context,
	req *connect.Request[compassv1.IssueTokenRequest],
) (*connect.Response[compassv1.IssueTokenResponse], error) {
	id := store.AccountID(req.Msg.GetAccountId())
	if id == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument,
			errors.New("account_id is required"))
	}
	if _, err := s.store.GetAccount(ctx, id); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, connect.NewError(connect.CodeNotFound,
				fmt.Errorf("no account with id %s", id))
		}
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	token, err := auth.IssueAccountToken(ctx, s.store, id)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	return connect.NewResponse(&compassv1.IssueTokenResponse{Token: token}), nil
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
	return forward(ctx, sub, stream)
}

// SubscribeAgentSession streams one agent session's typed observation trace to a
// caller authorized to see it. It first resolves-and-authorizes in one step —
// RequireAgentSessionSubscriber walks the durable ownership chain (session_id ->
// container_name -> agent_account_id -> home_channel_id) and checks the caller's
// membership on that home channel, returning the SAME not-found for an unknown
// session and a non-member so neither can probe session existence. Only past
// that gate does it subscribe to the session's live tail and
// forward frames until the client disconnects, the session ends, or the
// subscriber lags past its buffer. No snapshot replay: the observation pane is a
// live tail (the deferred daemon-lifecycle reattach/resync machinery is not in
// this increment).
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

// forward drains the replay snapshot (oldest first), then forwards the live tail
// until the client disconnects, the server shuts down, or the subscriber lags
// past the ring. The lag case emits a final ResyncRequired. Both phases select
// on ctx so a shutdown or client hang-up mid-replay returns promptly rather than
// stalling graceful drain.
func forward(
	ctx context.Context,
	sub events.Subscription[busPayload],
	stream *connect.ServerStream[compassv1.SubscribeEventsResponse],
) error { //nolint:unparam // forward's result is always nil by the stream-handler contract (every path returns nil on client hang-up / clean shutdown); it is the handler signature, not dead code
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
