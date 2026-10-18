package auth

import (
	"context"
	"errors"
	"log/slog"
	"strings"

	"connectrpc.com/connect/v2"

	"github.com/RigelBuild/compass/go/internal/comms"
	"github.com/RigelBuild/compass/go/internal/store"
)

var (
	errMissingAuthorization = errors.New("missing authorization header")
	errNotBearer            = errors.New("authorization header is not a bearer token")
	errInvalidToken         = errors.New("invalid or revoked token")
	errAdminOnly            = errors.New("admin-only session RPC")
)

// authorizationHeader carries the bearer credential on the network door, matching
// the token wire form ("authorization: Bearer <token>"). http.Header
// canonicalizes the key, so "Authorization" is the lookup form. The Bearer scheme
// name is matched case-insensitively (see bearerToken), not by a fixed prefix.
const authorizationHeader = "Authorization"

// callerKey stores the authenticated network-door account for this package.
// Its unexported type prevents context-key collisions.
type callerKey struct{}

// withCaller sets the account for both auth and comms readers. Without the
// comms actor, network calls would be attributed to the bootstrap admin.
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

// BearerInterceptor authenticates bearer tokens on the network door and attaches
// the caller and tenant to the server context.
func BearerInterceptor(st *store.Store) connect.ServerInterceptor {
	return func(next connect.ServerFunc) connect.ServerFunc {
		return func(ctx context.Context, spec connect.Spec, stream connect.ServerStream) error {
			var header *connect.Header
			if info, ok := connect.CallInfoForServerContext(ctx); ok {
				header = info.RequestHeader()
			}
			account, tenant, err := resolveBearer(ctx, st, header.Get(authorizationHeader))
			if err != nil {
				return err
			}
			return next(store.WithTenant(withCaller(ctx, account), tenant), spec, stream)
		}
	}
}

// AmbientIdentity attaches the socket door's fixed account to every request;
// the 0600 socket mode is the credential.
func AmbientIdentity(account store.AccountID) connect.ServerInterceptor {
	return func(next connect.ServerFunc) connect.ServerFunc {
		return func(ctx context.Context, spec connect.Spec, stream connect.ServerStream) error {
			return next(withCaller(ctx, account), spec, stream)
		}
	}
}

// resolveBearer extracts and resolves a bearer token from an Authorization
// header value, returning the caller or a CodeUnauthenticated error.
func resolveBearer(ctx context.Context, st *store.Store, header string) (store.AccountID, store.TenantID, error) {
	if header == "" {
		return store.AccountID(""), "", connect.NewError(connect.CodeUnauthenticated, errMissingAuthorization.Error()).WithCause(errMissingAuthorization)
	}
	token, ok := bearerToken(header)
	if !ok {
		return store.AccountID(""), "", connect.NewError(connect.CodeUnauthenticated, errNotBearer.Error()).WithCause(errNotBearer)
	}
	subj, err := ResolveToken(ctx, st, token, store.SubjectAccount)
	if err != nil {
		// Oracle-safe: every resolution failure — unknown, revoked, or a cross-door
		// (Runner) token — is one indistinguishable CodeUnauthenticated to the client.
		// The distinct sentinel is logged (debug) as a server-side audit signal only.
		// A store fault (ErrTokenLookupFailed) also stays Unauthenticated on this door.
		slog.DebugContext(ctx, "network door rejected bearer token", "reason", err)
		return store.AccountID(""), "", connect.NewError(connect.CodeUnauthenticated, errInvalidToken.Error()).WithCause(errInvalidToken)
	}
	// An unscoped caller would silently fall back to the bootstrap tenant; fail closed.
	if subj.Tenant == "" {
		slog.DebugContext(ctx, "network door rejected bearer token", "reason", "token has empty tenant")
		return store.AccountID(""), "", connect.NewError(connect.CodeUnauthenticated, errInvalidToken.Error()).WithCause(errInvalidToken)
	}
	return store.AccountID(subj.ID), subj.Tenant, nil
}

// bearerToken accepts a case-insensitive Bearer scheme and preserves the
// case-sensitive credential. Empty or space-containing credentials are invalid.
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
