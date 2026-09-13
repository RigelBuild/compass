//go:build unix

package gateway

// The Runner->Server forward for agent-initiated lifecycle calls (spawn/despawn),
// sibling of Comms: map the socket -> container -> the one bound session, then
// forward as RelayLifecycleCall. The Runner sets NO actor — the Server resolves
// the account, fail-closed; a call before the session is bound is denied.

import (
	"context"
	"errors"

	"connectrpc.com/connect"

	compassv1internal "github.com/RigelBuild/compass/go/internal/gen/compass/v1"
)

// errNoSessionForLifecycle is the fail-closed cause when a lifecycle call
// arrives on a container's socket before Start has bound a session to it. It
// maps to CodePermissionDenied — the call is refused, never forwarded with an
// empty session id and never attributed to any account. The lifecycle twin of
// errNoSessionForContainer.
var errNoSessionForLifecycle = errors.New("gateway: no live session bound to container")

// errNilLifecycleResult is the cause when the Server's RelayLifecycleCall
// returns a response with no result message — a malformed reply the gateway
// surfaces as CodeInternal rather than a success wrapping a nil result. The
// lifecycle twin of errNilRelayResult.
var errNilLifecycleResult = errors.New("gateway: relay returned no lifecycle result")

// Lifecycle forwards one agent-initiated lifecycle call to the Server's
// RelayLifecycleCall under the session bound to this container. It fails closed
// (CodePermissionDenied) when no live session maps to the container — the socket
// is live from Provision, before Start binds the session, so a call in that
// window must never forward with an empty session id nor attribute to any
// account. The inbound deadline rides ctx into the forward.
//
// A Server-side in-band tool failure (unknown target, duplicate handle,
// self-despawn) rides back as the LifecycleCallError variant of the result, NOT
// a Connect error, so a single failed call never tears the transport down. A
// genuine transport failure (Server unreachable) surfaces as a Connect error,
// which the agent renders as an in-band tool error too. Mirrors Comms exactly.
func (g *Gateway) Lifecycle(
	ctx context.Context, req *connect.Request[compassv1internal.LifecycleCallRequest],
) (*connect.Response[compassv1internal.LifecycleCallResult], error) {
	sessionID, ok := g.sessions.Session(g.containerName)
	// An empty session id is unbound too: the resolver must never hand back a
	// live binding to the empty session, but treat "" as unbound rather than
	// forward it — the handler promises never to relay an empty session id.
	if !ok || sessionID == "" {
		return nil, connect.NewError(connect.CodePermissionDenied, errNoSessionForLifecycle)
	}

	resp, err := g.lifecycle.RelayLifecycleCall(ctx, connect.NewRequest(&compassv1internal.RelayLifecycleCallRequest{
		SessionId: sessionID,
		Call:      req.Msg,
	}))
	if err != nil {
		// A transport failure on the Runner->Server leg. Surfaced as a Connect
		// error the agent renders in-band; the turn is not torn down.
		return nil, err
	}
	// A well-formed RelayLifecycleCallResponse always carries a result; a nil
	// result is a malformed Server response, surfaced as a Connect error (never a
	// success wrapping a nil Msg the agent would deref).
	result := resp.Msg.GetResult()
	if result == nil {
		return nil, connect.NewError(connect.CodeInternal, errNilLifecycleResult)
	}
	return connect.NewResponse(result), nil
}
