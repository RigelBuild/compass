//go:build unix

package gateway

// gateway.go is the Runner->Server forward: the AgentGateway.Comms handler an
// in-container agent reaches over its per-container socket (socket.go). It maps
// the connection -> the container it belongs to -> the one session bound to that
// container, then forwards the call to the Server as RelayCommsCall(session_id,
// call). The Runner resolves NO account and sets NO actor: the Server resolves
// session_id -> account from its own binding and attributes in-process, fail-
// closed (transport design T3, Decision #3 / OQ-2).
//
// The socket IS the container's identity: one Gateway serves one container's
// socket, so the container name is fixed at construction, never read off the
// request. A call arriving before the container's session is bound (socket live
// at Provision, before Start mints the session) fails closed
// CodePermissionDenied — never a forward with an empty session id, never a
// bootstrap-admin-attributed side effect.

import (
	"context"
	"errors"
	"net/http"

	"connectrpc.com/connect"

	compassv1internal "github.com/sealedsecurity/compass/go/internal/gen/compass/v1"
	"github.com/sealedsecurity/compass/go/internal/gen/compass/v1/compassv1internalconnect"
)

// errNoSessionForContainer is the fail-closed cause when a comms call arrives on
// a container's socket before Start has bound a session to it. It maps to
// CodePermissionDenied — the call is refused, never forwarded with an empty
// session id and never attributed to any account.
var errNoSessionForContainer = errors.New("gateway: no live session bound to container")

// errNilRelayResult is the cause when the Server's RelayCommsCall returns a
// response with no result message — a malformed reply the gateway surfaces as
// CodeInternal rather than a success wrapping a nil result.
var errNilRelayResult = errors.New("gateway: relay returned no comms result")

// SessionForContainer resolves the one session bound to a container (1:1, fixed
// at Start, immutable thereafter — no dynamic "current-session" remap). The
// production implementation is the Runner's agentHost, which already keys its
// live session set by session id with a containerName field. No account is
// resolved anywhere on the Runner.
type SessionForContainer interface {
	Session(containerName string) (sessionID string, ok bool)
}

// CommsRelay forwards one agent-initiated comms call to the Server under the
// resolved session. It is the narrow slice of the generated RunnerServiceClient
// the gateway needs; the real client satisfies it, and a test supplies a fake.
// The Runner is a pure forwarder — it sends the session_id it structurally owns
// and the agent's request verbatim, and asserts no account.
type CommsRelay interface {
	RelayCommsCall(ctx context.Context, req *connect.Request[compassv1internal.RelayCommsCallRequest]) (*connect.Response[compassv1internal.RelayCommsCallResponse], error)
}

// Gateway implements compassv1internalconnect.AgentGatewayHandler for one
// container's socket. containerName is the identity the socket structurally
// carries; sessions resolves it to the bound session; relay forwards the call.
type Gateway struct {
	// The C1 wire (transport-consolidation record) grew AgentGateway with
	// Publish (client-stream), PostConversationFrame (unary), and Control
	// (server-stream). Their real handlers are Runner-side C2/C3 work; embedding
	// the generated Unimplemented handler makes those methods CodeUnimplemented
	// stubs so Gateway still satisfies the interface until C2/C3 override them.
	compassv1internalconnect.UnimplementedAgentGatewayHandler
	containerName string
	sessions      SessionForContainer
	relay         CommsRelay
}

// Ensure Gateway satisfies the generated handler interface at compile time.
var _ compassv1internalconnect.AgentGatewayHandler = (*Gateway)(nil)

// NewGateway builds the AgentGateway handler for the container's socket:
// containerName is bound here (the socket is that container's identity),
// sessions resolves the container to its live session, and relay forwards the
// call to the Server.
func NewGateway(containerName string, sessions SessionForContainer, relay CommsRelay) *Gateway {
	return &Gateway{containerName: containerName, sessions: sessions, relay: relay}
}

// Serve creates the per-container agent socket at path and serves this
// container's AgentGateway over it, returning the live listener. It composes the
// container's Gateway (containerName bound to the socket, sessions resolving it
// to the live session, relay forwarding to the Server) onto the owner-only Unix
// socket the SocketListener owns. Called at Provision, before `podman run`, so
// the bind-mount source is live when the container starts; the returned
// listener's Close tears the socket down at container teardown.
func Serve(ctx context.Context, path, containerName string, sessions SessionForContainer, relay CommsRelay) (*SocketListener, error) {
	mux := http.NewServeMux()
	mux.Handle(compassv1internalconnect.NewAgentGatewayHandler(NewGateway(containerName, sessions, relay)))
	return listenAgentSocket(ctx, path, mux)
}

// Comms forwards one agent-initiated comms call to the Server's RelayCommsCall
// under the session bound to this container. It fails closed
// (CodePermissionDenied) when no live session maps to the container — the socket
// is live from Provision, before Start binds the session, so a call in that
// window must never forward with an empty session id nor attribute to any
// account. The inbound deadline rides ctx into the forward.
//
// A Server-side in-band tool failure (non-member channel, bad input) rides back
// as the CommsCallError variant of the result, NOT a Connect error, so a single
// failed call never tears the transport down. A genuine transport failure
// (Server unreachable) surfaces as a Connect error, which the agent renders as
// an in-band tool error too (it never tears the turn down, N/OQ-6).
func (g *Gateway) Comms(
	ctx context.Context, req *connect.Request[compassv1internal.CommsCallRequest],
) (*connect.Response[compassv1internal.CommsCallResult], error) {
	sessionID, ok := g.sessions.Session(g.containerName)
	// An empty session id is unbound too: the resolver must never hand back a
	// live binding to the empty session, but treat "" as unbound rather than
	// forward it — the handler promises never to relay an empty session id.
	if !ok || sessionID == "" {
		return nil, connect.NewError(connect.CodePermissionDenied, errNoSessionForContainer)
	}

	resp, err := g.relay.RelayCommsCall(ctx, connect.NewRequest(&compassv1internal.RelayCommsCallRequest{
		SessionId: sessionID,
		Call:      req.Msg,
	}))
	if err != nil {
		// A transport failure on the Runner->Server leg. Surfaced as a Connect
		// error the agent renders in-band; the turn is not torn down.
		return nil, err
	}
	// A well-formed RelayCommsCallResponse always carries a result; a nil result
	// is a malformed Server response, surfaced as a Connect error (never a
	// success wrapping a nil Msg the agent would deref).
	result := resp.Msg.GetResult()
	if result == nil {
		return nil, connect.NewError(connect.CodeInternal, errNilRelayResult)
	}
	return connect.NewResponse(result), nil
}
