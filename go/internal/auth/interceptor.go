package auth

import (
	"context"
	"log/slog"
	"strings"

	"connectrpc.com/connect"

	"github.com/RigelBuild/compass/go/internal/comms"
	"github.com/RigelBuild/compass/go/internal/store"
)

// authorizationHeader carries the bearer credential on the network door, matching
// the frozen token wire form ("authorization: Bearer <token>"). http.Header
// canonicalizes the key, so "Authorization" is the lookup form. The Bearer scheme
// name is matched case-insensitively (see bearerToken), not by a fixed prefix.
const authorizationHeader = "Authorization"

// callerKey is the unexported context key under which the network door's bearer
// interceptor stores the authenticated caller for the auth layer's own reader
// (AdminGate). Unexported so only this package can set it and no other package's
// context value can collide.
type callerKey struct{}

// withCaller returns a child context carrying account as the authenticated caller
// for both readers on the network door: the auth AdminGate reads it via
// CallerFrom (callerKey), and the comms service handlers read it via
// comms.actorFromContext — the frozen T3 seam (comms/context.go), set through
// comms.WithActor. Setting both is load-bearing: without the comms.WithActor
// half, an authenticated comms RPC on the network door would fall back to the
// bootstrap-admin identity (comms attributes the admin when no actor is set),
// silently attributing every network caller to the admin rather than the real
// token holder.
func withCaller(ctx context.Context, account store.AccountID) context.Context {
	ctx = context.WithValue(ctx, callerKey{}, account)
	return comms.WithActor(ctx, account)
}

// CallerFrom extracts the authenticated caller the network door's bearer
// interceptor attached to ctx, or ok=false when none was set (a request that
// reached an admin-gated handler without clearing the bearer door is a server
// wiring bug). The AdminGate reads the caller identity through this.
func CallerFrom(ctx context.Context) (store.AccountID, bool) {
	account, ok := ctx.Value(callerKey{}).(store.AccountID)
	return account, ok
}

// BearerInterceptor is the bearer-token interceptor for the network door. It
// reads "authorization: Bearer <token>", resolves the token against the store of
// record, and attaches the caller to the request context — or rejects the request
// with CodeUnauthenticated when the header is missing, malformed (not a Bearer
// value), or names a token the store doesn't hold (or one issued for a non-account
// subject). The scheme name is matched case-insensitively (Bearer/bearer/BEARER)
// per RFC 7235 §2.1, while the token itself stays case-sensitive (base64url is
// case-significant).
//
// Returned as a UnaryInterceptorFunc; the streaming door is covered by
// BearerStreamInterceptor, which shares the same resolution.
func BearerInterceptor(st *store.Store) connect.UnaryInterceptorFunc {
	return func(next connect.UnaryFunc) connect.UnaryFunc {
		return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
			account, err := resolveBearer(ctx, st, req.Header().Get(authorizationHeader))
			if err != nil {
				return nil, err
			}
			return next(withCaller(ctx, account), req)
		}
	}
}

// BearerStreamInterceptor is the streaming-handler variant of BearerInterceptor:
// same header resolution, applied to the streaming door so a streaming RPC also
// carries the resolved caller in its context. connect drives it via
// WrapStreamingHandler; the client-stream leg is a pass-through (the network door
// authenticates inbound handler calls, not the server's own outbound clients).
func BearerStreamInterceptor(st *store.Store) connect.Interceptor {
	return streamAuth{resolve: func(ctx context.Context, header string) (store.AccountID, error) {
		return resolveBearer(ctx, st, header)
	}}
}

// AmbientIdentity is the socket door's caller interceptor: it attaches a fixed
// account to every request without a token, because the 0600 socket's mode is
// itself the credential (only the owning user can connect). Handlers then read
// the caller uniformly via CallerFrom on both doors — the network door resolves
// it from a bearer token, the socket door from this ambient identity. Pair it
// with AmbientStreamInterceptor: installing only the unary form leaves streaming
// RPCs (SubscribeAgentSession) with no caller in context, so CallerFrom returns
// false and the handler fail-closes — a silent authorization gap, not a compile
// error.
func AmbientIdentity(account store.AccountID) connect.UnaryInterceptorFunc {
	return func(next connect.UnaryFunc) connect.UnaryFunc {
		return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
			return next(withCaller(ctx, account), req)
		}
	}
}

// AmbientStreamInterceptor is the streaming-handler variant of AmbientIdentity:
// it attaches the same fixed account to a streaming RPC's context so a stream
// (SubscribeAgentSession) carries the ambient caller on the socket door. The
// client-stream leg is a pass-through (the door authenticates inbound handler
// calls, not the server's own outbound clients).
func AmbientStreamInterceptor(account store.AccountID) connect.Interceptor {
	return ambientStream{account: account}
}

// ambientStream carries the fixed account for AmbientStreamInterceptor's
// WrapStreamingHandler; the unary and client-stream legs are pass-throughs.
type ambientStream struct {
	account store.AccountID
}

func (a ambientStream) WrapUnary(next connect.UnaryFunc) connect.UnaryFunc { return next }

func (a ambientStream) WrapStreamingClient(next connect.StreamingClientFunc) connect.StreamingClientFunc {
	return next
}

func (a ambientStream) WrapStreamingHandler(next connect.StreamingHandlerFunc) connect.StreamingHandlerFunc {
	return func(ctx context.Context, conn connect.StreamingHandlerConn) error {
		return next(withCaller(ctx, a.account), conn)
	}
}

// resolveBearer extracts and resolves the bearer token from an Authorization
// header value, returning the caller or a CodeUnauthenticated error. Shared by
// the unary and streaming bearer interceptors so both doors reject identically.
func resolveBearer(ctx context.Context, st *store.Store, header string) (store.AccountID, error) {
	if header == "" {
		return store.AccountID(""), connect.NewError(connect.CodeUnauthenticated, errMissingAuthorization)
	}
	token, ok := bearerToken(header)
	if !ok {
		return store.AccountID(""), connect.NewError(connect.CodeUnauthenticated, errNotBearer)
	}
	subj, err := ResolveToken(ctx, st, token, store.SubjectAccount)
	if err != nil {
		// Oracle-safe: every resolution failure — unknown, revoked, or a
		// cross-door (Runner) token — is one indistinguishable CodeUnauthenticated
		// to the client (errInvalidToken), so the response never reveals which.
		// The distinct sentinel is logged (debug) as a server-side audit signal
		// only; it never reaches the wire.
		slog.DebugContext(ctx, "network door rejected bearer token", "reason", err)
		return store.AccountID(""), connect.NewError(connect.CodeUnauthenticated, errInvalidToken)
	}
	// The account door's trivial typed wrap on the shared resolver's Subject.
	return store.AccountID(subj.ID), nil
}

// bearerToken extracts the token from an Authorization header value, accepting
// the Bearer scheme case-insensitively (RFC 7235 §2.1) followed by one or more
// spaces, and returning the credential verbatim (case-sensitive — base64url is
// case-significant). Returns ok=false for any non-bearer value, an empty
// credential, or a credential carrying an interior space (a base64url token never
// contains one, so that signals a malformed header).
func bearerToken(header string) (string, bool) {
	scheme, rest, found := strings.Cut(header, " ")
	if !found || !strings.EqualFold(scheme, "bearer") {
		return "", false
	}
	token := strings.TrimLeft(rest, " ")
	if token == "" || strings.Contains(token, " ") {
		return "", false
	}
	return token, true
}
