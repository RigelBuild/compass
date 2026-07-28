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
	"sync"

	"connectrpc.com/connect"
	"github.com/hashicorp/golang-lru/v2/expirable"

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

// maxAgentMessageBytes bounds every AgentGateway message the socket handler reads
// (Comms, Publish, PostConversationFrame). Retiring the stdout scanner also
// retired its 4 MiB line cap (relay.go:145), which was the only per-message size
// bound on the agent→Runner hop; connect-go imposes none unless WithReadMaxBytes
// is set, so without this a compromised in-container agent could stream one
// arbitrarily large message the Runner buffers in memory. Set to a small multiple
// of the retired 4 MiB, chosen once here (Global Constraints, transport-
// consolidation record). A message past it is a Connect stream/unary error routed
// to the agent's reconnect path, not an OOM.
const maxAgentMessageBytes = 16 * 1024 * 1024

// committedKeysMax bounds the advisory idempotency fast-path cache
// (Gateway.committedKeys). A durable conversation frame carries a distinct
// idempotency_key, so an unbounded set would grow one entry per durable message
// for the socket's whole life — a leak proportional to conversation volume, and
// a lever an in-container agent could pull to exhaust Runner memory with many
// small distinct-key frames (the aggregate-memory analogue of the per-message
// maxAgentMessageBytes bound). Sized generously so a normal session's recent
// keys all stay resident (the fast-path stays effective for realistic retry
// windows); an evicted key costs one redundant re-forward the store dedups on
// its own at-most-once boundary, never a correctness loss.
const committedKeysMax = 16384

// errNoSessionForPublish / errNoSessionForConversation are the fail-closed causes
// when a telemetry frame arrives on a container's socket before Start binds a
// session (the socket is live from Provision). Both map to CodePermissionDenied —
// the frame is never forwarded under an empty session id, exactly as Comms fails
// closed.
var (
	errNoSessionForPublish      = errors.New("gateway: no live session bound to container for publish")
	errNoSessionForConversation = errors.New("gateway: no live session bound to container for conversation frame")
)

// errNotConversationFrame is the cause when PostConversationFrame receives a
// frame whose variant is not conversation_posted / conversation_updated — the
// durable unary carries only durable conversation frames (CodeInvalidArgument).
var errNotConversationFrame = errors.New("gateway: PostConversationFrame requires a conversation_posted or conversation_updated frame")

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
	// The transport-consolidation record grew AgentGateway with Publish
	// (client-stream), PostConversationFrame (unary), and Control
	// (server-stream). This change overrides Publish + PostConversationFrame on
	// *Gateway; Control is still served by the embedded Unimplemented handler
	// until the control lane overrides it. Embedding keeps Gateway satisfying the
	// interface across the telemetry-ingest/control-lane split.
	compassv1internalconnect.UnimplementedAgentGatewayHandler
	containerName string
	sessions      SessionForContainer
	relay         CommsRelay
	// events forwards trace/session + durable conversation frames up
	// PublishEvents. control routes the two control-plane ack frames off the
	// Publish stream to the control lane; the default is a no-op until the control
	// lane injects the real sender.
	events  EventRelay
	control ControlRouter
	// baseCtx is the socket's lifetime context — it lives as long as the socket
	// server (Serve creates it; SocketListener.Close cancels it at container
	// teardown), NOT any one agent request. The shared upstream PublishEvents
	// stream is opened against it (acquirePublisher), so the stream's life is the
	// socket's, never a single handler invocation: binding it to a unary
	// PostConversationFrame's request context would tear the shared stream down
	// the instant that unary returned (net/http cancels a request context on
	// handler return), wedging every later forward. Stored on the struct because
	// the handler methods that lazily open the stream receive only their own
	// request contexts; there is no other channel for the socket-lifetime scope.
	//nolint:containedctx // socket-lifetime scope for the shared upstream stream; the per-request handler ctx is the wrong lifetime (see field doc)
	baseCtx context.Context
	// pub is the ordered per-session publisher shared by Publish and
	// PostConversationFrame so both stamp one monotonic RunnerSeq and send on one
	// upstream stream (allocation order == emission order, so concurrent traffic
	// across the two paths opens no false hub gap). It is created lazily by
	// whichever handler first forwards a frame, guarded by pubMu for that init
	// race, for the swap back to nil when Publish closes the stream, and for the
	// session-change reset (a publisher bound to a prior session is replaced when
	// the container rebinds to a new session across Stop→Start).
	pubMu sync.Mutex
	pub   *sessionPublisher
	// committedKeys is the advisory in-process at-most-once fast-path for durable
	// PostConversationFrame idempotency: a key seen committed here short-circuits
	// a retry without a redundant upstream forward. It is NOT the durability
	// boundary — that is the atomic commit at the comms Message store keyed on the
	// same idempotency_key (store AppendMessage clientRequestID), which survives a
	// Runner crash that loses this cache. Bounded (LRU, committedKeysMax) so a
	// long-lived session emitting many distinct-key durable frames cannot grow it
	// without limit — an evicted key merely costs one redundant re-forward the
	// store dedups, never a correctness loss. The LRU is internally synchronized,
	// so it needs no separate mutex.
	committedKeys *expirable.LRU[string, struct{}]
}

// Ensure Gateway satisfies the generated handler interface at compile time.
var _ compassv1internalconnect.AgentGatewayHandler = (*Gateway)(nil)

// NewGateway builds the AgentGateway handler for the container's socket:
// containerName is bound here (the socket is that container's identity),
// sessions resolves the container to its live session, relay forwards a comms
// call to the Server, and events forwards telemetry/durable frames up
// PublishEvents. The control router defaults to a no-op; SetControlRouter
// injects the real one once the control lane is wired.
//
// baseCtx is the socket's lifetime context (Serve owns it, SocketListener.Close
// cancels it): the shared upstream PublishEvents stream is opened against it so
// the stream outlives any one agent request. A caller with no distinct socket
// scope (a hermetic test) passes context.Background().
func NewGateway(baseCtx context.Context, containerName string, sessions SessionForContainer, relay CommsRelay, events EventRelay) *Gateway {
	return &Gateway{
		baseCtx:       baseCtx,
		containerName: containerName,
		sessions:      sessions,
		relay:         relay,
		events:        events,
		control:       noopControlRouter{},
		// ttl=0: no expiry, a pure size-bounded LRU (committedKeysMax). The cache
		// is advisory, so eviction is safe — it never drops the store's boundary.
		committedKeys: expirable.NewLRU[string, struct{}](committedKeysMax, nil, 0),
	}
}

// SetControlRouter injects the control lane as the target of ack routing. Called
// during Gateway wiring once the control lane exists; until then the no-op
// default stands. Not safe for concurrent use with a live Publish stream — set
// it at construction/wiring, before Serve accepts calls.
func (g *Gateway) SetControlRouter(r ControlRouter) {
	g.control = r
}

// Serve creates the per-container agent socket at path and serves this
// container's AgentGateway over it, returning the live listener. It composes the
// container's Gateway (containerName bound to the socket, sessions resolving it
// to the live session, relay + events forwarding to the Server) onto the
// owner-only Unix socket the SocketListener owns, with an explicit ReadMaxBytes
// bound on every method (Global Constraints: a large agent-buffered message is a
// stream/unary error, not an OOM). Called at Provision, before `podman run`, so
// the bind-mount source is live when the container starts; the returned
// listener's Close tears the socket down at container teardown.
func Serve(ctx context.Context, path, containerName string, sessions SessionForContainer, relay CommsRelay, events EventRelay) (*SocketListener, error) {
	// The socket-lifetime context: it outlives any one agent request and is
	// cancelled when the listener closes at container teardown (listenAgentSocket
	// hands socketCancel to the listener). The shared upstream PublishEvents
	// stream is opened against it, so a PostConversationFrame unary that first
	// opens the stream does not tie the stream's life to its own request.
	socketCtx, socketCancel := context.WithCancel(ctx)
	mux := http.NewServeMux()
	mux.Handle(compassv1internalconnect.NewAgentGatewayHandler(
		NewGateway(socketCtx, containerName, sessions, relay, events),
		connect.WithReadMaxBytes(maxAgentMessageBytes),
	))
	l, err := listenAgentSocket(ctx, path, mux, socketCancel)
	if err != nil {
		socketCancel()
		return nil, err
	}
	return l, nil
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
