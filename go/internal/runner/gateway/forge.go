//go:build unix

package gateway

// The Runner->Server forward for agent-initiated forge calls (AgentGateway.Forge),
// sibling of Comms and Lifecycle: map the socket -> container -> the one bound
// session, then forward as RelayForgeCall. The Runner sets NO actor — the Server
// resolves the account, fail-closed; a call before the session is bound is denied.

import (
	"context"
	"errors"

	"connectrpc.com/connect"

	compassv1internal "github.com/RigelBuild/compass/go/internal/gen/compass/v1"
)

// errNoSessionForForge is the fail-closed cause when a forge call arrives on a
// container's socket before Start has bound a session to it. It maps to
// CodePermissionDenied — the call is refused, never forwarded with an empty
// session id and never attributed to any account. The forge twin of
// errNoSessionForLifecycle.
var errNoSessionForForge = errors.New("gateway: no live session bound to container")

// errNilForgeResult is the cause when the Server's RelayForgeCall returns a
// response with no result message — a malformed reply the gateway surfaces as
// CodeInternal rather than a success wrapping a nil result. The forge twin of
// errNilLifecycleResult.
var errNilForgeResult = errors.New("gateway: relay returned no forge result")

// Forge forwards one agent-initiated forge call to the Server's RelayForgeCall
// under the session bound to this container. It fails closed
// (CodePermissionDenied) when no live session maps to the container — the socket
// is live from Provision, before Start binds the session, so a call in that
// window must never forward with an empty session id nor attribute to any
// account. The inbound deadline rides ctx into the forward.
//
// A Server-side in-band tool failure (unknown coordinate, rate limit, bad input)
// rides back as the ForgeCallError variant of the result, NOT a Connect error,
// so a single failed call never tears the transport down. A genuine transport
// failure (Server unreachable) surfaces as a Connect error, which the agent
// renders as an in-band tool error too. Mirrors Lifecycle exactly.
func (g *Gateway) Forge(
	ctx context.Context, req *connect.Request[compassv1internal.ForgeCallRequest],
) (*connect.Response[compassv1internal.ForgeCallResult], error) {
	sessionID, ok := g.sessions.Session(g.containerName)
	// An empty session id is unbound too: the resolver must never hand back a
	// live binding to the empty session, but treat "" as unbound rather than
	// forward it — the handler promises never to relay an empty session id.
	if !ok || sessionID == "" {
		return nil, connect.NewError(connect.CodePermissionDenied, errNoSessionForForge)
	}

	resp, err := g.forge.RelayForgeCall(ctx, connect.NewRequest(&compassv1internal.RelayForgeCallRequest{
		SessionId: sessionID,
		Call:      req.Msg,
	}))
	if err != nil {
		// A transport failure on the Runner->Server leg. Surfaced as a Connect
		// error the agent renders in-band; the turn is not torn down.
		return nil, err
	}
	// A well-formed RelayForgeCallResponse always carries a result; a nil result
	// is a malformed Server response, surfaced as a Connect error (never a
	// success wrapping a nil Msg the agent would deref).
	result := resp.Msg.GetResult()
	if result == nil {
		return nil, connect.NewError(connect.CodeInternal, errNilForgeResult)
	}
	return connect.NewResponse(result), nil
}
