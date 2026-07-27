package auth

import (
	"context"

	"connectrpc.com/connect"

	compassv1connect "github.com/sealedsecurity/compass/go/gen/compass/v1/compassv1connect"
	"github.com/sealedsecurity/compass/go/internal/store"
)

// privilege is the access class of a CompassService or CommsService RPC on the
// network door: a sealed sum (//sumtype:decl) so every switch over it — check()
// below — must handle each class exhaustively or gochecksumtype fails the build.
// The sum does NOT police procedure coverage (classifyProcedure switches over a
// string, which gochecksumtype cannot see); classify_exhaustive_test enforces
// that every generated procedure is classified. See classifyProcedure.
//
//sumtype:decl
type privilege interface{ isPrivilege() }

// adminOnly marks an RPC reachable on the network door only by the bootstrap
// admin. These are the privileged CompassService agent-session RPCs (and token
// issuance): they were written for the single-user socket trust boundary and take
// no account argument, so on the shared network door a non-admin account must not
// reach them.
type adminOnly struct{}

func (adminOnly) isPrivilege() {}

// authenticatedOpen marks an RPC reachable on the network door by any
// authenticated account: the connect-time probe and every CommsService method,
// which carry their own per-account authorization in the service body.
type authenticatedOpen struct{}

func (authenticatedOpen) isPrivilege() {}

// classifyProcedure maps a connect procedure path to its network-door access
// class, returning ok=false when the path is not a recognized generated
// procedure. The switch enumerates every generated CompassService and
// CommsService procedure; classify_exhaustive_test ranges the proto service
// descriptors and fails if any generated procedure is missing here, so a newly
// added RPC reddens CI until it is classified. gochecksumtype cannot police this
// coverage — the switch is over a string, not the privilege sum. An unrecognized
// path (ok=false) is treated as adminOnly — fail closed, never admit an unknown
// method as open.
func classifyProcedure(procedure string) (privilege, bool) {
	switch procedure {
	// Privileged CompassService agent-session RPCs + token issuance: admin only.
	case compassv1connect.CompassServiceProvisionAgentWorkspaceProcedure,
		compassv1connect.CompassServiceStartAgentSessionProcedure,
		compassv1connect.CompassServiceStopAgentSessionProcedure,
		compassv1connect.CompassServiceReloadAgentSessionProcedure,
		compassv1connect.CompassServiceGetAgentStatusProcedure,
		compassv1connect.CompassServiceIssueTokenProcedure:
		return adminOnly{}, true

	// The connect-time probe and the two event streams: open to any authenticated
	// account. SubscribeAgentSession carries its own per-account authorization in
	// the handler (session-ownership → home-channel membership), like the other
	// open streams, so the network-door gate must let its intended non-admin
	// channel members through to that check.
	case compassv1connect.CompassServiceGetServerInfoProcedure,
		compassv1connect.CompassServiceSubscribeEventsProcedure,
		compassv1connect.CompassServiceSubscribeAgentSessionProcedure:
		return authenticatedOpen{}, true

	// Every CommsService method: open to any authenticated account (the service
	// body enforces per-account authorization).
	case compassv1connect.CommsServiceCreateUserProcedure,
		compassv1connect.CommsServiceCreateAgentProcedure,
		compassv1connect.CommsServiceListAccountsProcedure,
		compassv1connect.CommsServiceCreateChannelGroupProcedure,
		compassv1connect.CommsServiceListChannelGroupsProcedure,
		compassv1connect.CommsServiceCreateChannelProcedure,
		compassv1connect.CommsServiceListChannelsProcedure,
		compassv1connect.CommsServiceUpdateChannelMembersProcedure,
		compassv1connect.CommsServiceOpenAgentWorkspaceProcedure,
		compassv1connect.CommsServiceListMessagesProcedure,
		compassv1connect.CommsServicePostMessageProcedure,
		compassv1connect.CommsServiceRespondToAskProcedure,
		compassv1connect.CommsServiceSearchMessagesProcedure,
		compassv1connect.CommsServiceSubscribeCommsProcedure:
		return authenticatedOpen{}, true

	default:
		// Fail closed: an unrecognized path (not a generated procedure) is gated
		// to admin and reported unclassified (ok=false). classify_exhaustive_test
		// guarantees no generated procedure reaches here; this guards only a
		// runtime path that is not a generated procedure at all.
		return adminOnly{}, false
	}
}

// AdminGate is the method-level authorization guard over the network door's
// privileged CompassService session RPCs — distinct from BearerInterceptor's
// authentication. BearerInterceptor runs first (outer) and injects the caller;
// this gate reads that caller and, for an adminOnly procedure, rejects any
// account other than the bootstrap admin with CodePermissionDenied. Open
// procedures pass through untouched.
//
// Unlike the Rust tower Layer this re-realizes, it is a plain connect interceptor:
// connect exposes the RPC method path on Spec().Procedure inside the interceptor,
// so the method-aware gate needs no lower-level middleware.
type AdminGate struct {
	admin store.AccountID
}

// NewAdminGate returns an admin gate parameterized by the bootstrap-admin
// account, mirroring AmbientIdentity; the network door hands the same admin id to
// both.
func NewAdminGate(admin store.AccountID) *AdminGate {
	return &AdminGate{admin: admin}
}

// WrapUnary enforces the gate on unary RPCs.
func (g *AdminGate) WrapUnary(next connect.UnaryFunc) connect.UnaryFunc {
	return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
		if err := g.check(ctx, req.Spec().Procedure); err != nil {
			return nil, err
		}
		return next(ctx, req)
	}
}

// WrapStreamingClient is a pass-through: the gate guards inbound handler calls,
// not the server's own outbound clients.
func (g *AdminGate) WrapStreamingClient(next connect.StreamingClientFunc) connect.StreamingClientFunc {
	return next
}

// WrapStreamingHandler enforces the gate on streaming RPCs before the handler
// runs.
func (g *AdminGate) WrapStreamingHandler(next connect.StreamingHandlerFunc) connect.StreamingHandlerFunc {
	return func(ctx context.Context, conn connect.StreamingHandlerConn) error {
		if err := g.check(ctx, conn.Spec().Procedure); err != nil {
			return err
		}
		return next(ctx, conn)
	}
}

// check rejects a non-admin caller on an adminOnly procedure. It runs after
// BearerInterceptor, so a missing caller means the gated request never cleared
// authentication — treated as permission denied (the authn layer already
// rejected it as unauthenticated; this is defense in depth).
func (g *AdminGate) check(ctx context.Context, procedure string) error {
	class, _ := classifyProcedure(procedure)
	switch class.(type) {
	case adminOnly:
		caller, ok := CallerFrom(ctx)
		if !ok || caller != g.admin {
			return connect.NewError(connect.CodePermissionDenied, errAdminOnly)
		}
		return nil
	case authenticatedOpen:
		return nil
	}
	// Fail closed: privilege is a sealed two-variant sum, so this fallthrough is
	// unreachable today, but an authorization gate must deny — not admit — any
	// class it does not explicitly recognize.
	return connect.NewError(connect.CodePermissionDenied, errAdminOnly)
}
