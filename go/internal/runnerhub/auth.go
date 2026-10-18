//go:build unix

// RunnerService door auth: a bearer-token interceptor that Kind-gates the
// presented token to SubjectRunner on every RPC. Any other token (account,
// revoked, not-found) collapses to a bare CodeUnauthenticated — no oracle,
// fail-closed; distinct store sentinels are for server-side logging only.
// A store fault is not a verdict: it returns a fixed Unavailable so the Runner retries.
package runnerhub

import (
	"context"
	"errors"
	"log/slog"
	"strings"

	"connectrpc.com/connect/v2"

	"github.com/RigelBuild/compass/go/internal/auth"
	"github.com/RigelBuild/compass/go/internal/bodydeadline"
	"github.com/RigelBuild/compass/go/internal/store"
)

// TokenResolver is the shared credential-resolution seam: sha256 the presented
// token, resolve it in the store, and Kind-gate it against want. It mirrors
// auth.ResolveToken(ctx, st, presented, want) with the store closed over. Returns
// the resolved subject, a credential sentinel the door collapses to
// Unauthenticated, or an auth.ErrTokenLookupFailed-wrapped store fault.
type TokenResolver func(ctx context.Context, presented string, want store.SubjectKind) (store.Subject, error)

// runnerSubjectKey carries the authenticated Runner subject on the request
// context. Unexported so only this package sets or reads it — a Runner's
// identity can never be spoofed through a request field.
type runnerSubjectKey struct{}

// runnerSubjectFrom returns the authenticated Runner subject set by the door
// interceptor, or (zero, false) when none is set (an unauthenticated path).
func runnerSubjectFrom(ctx context.Context) (store.Subject, bool) {
	subj, ok := ctx.Value(runnerSubjectKey{}).(store.Subject)
	return subj, ok
}

// withRunnerSubject returns ctx carrying the authenticated Runner subject.
func withRunnerSubject(ctx context.Context, subj store.Subject) context.Context {
	return context.WithValue(ctx, runnerSubjectKey{}, subj)
}

// bearerPrefix is the Authorization scheme the token rides under, matching the
// account door (compass.proto:246 "authorization: Bearer <token>").
const bearerPrefix = "Bearer "

// errUnauthenticated is the single opaque error every credential failure maps to —
// no detail distinguishes not-found, revoked, or wrong-kind to the client (no
// oracle). The distinct store sentinels are logged server-side only.
var errUnauthenticated = connect.NewError(connect.CodeUnauthenticated, "unauthenticated")

// errLookupUnavailable is the fixed store-fault response; the cause can name DB hosts, so it stays server-side.
var errLookupUnavailable = connect.NewError(connect.CodeUnavailable, "credential check unavailable")

// errKeysUnavailable is the fixed response when a projected token names a
// registered cluster whose keys are unusable; retryable, unlike a bad token.
var errKeysUnavailable = connect.NewError(connect.CodeUnavailable, "runner cluster keys unavailable")

// authenticate resolves the bearer token as SubjectRunner and returns its subject in context.
// Credential failures share errUnauthenticated; store faults return errLookupUnavailable.
func (b *bearerAuth) authenticate(ctx context.Context, header interface{ Get(key string) string }) (context.Context, error) {
	raw := header.Get("Authorization")
	if !strings.HasPrefix(raw, bearerPrefix) {
		return nil, errUnauthenticated
	}
	token := strings.TrimPrefix(raw, bearerPrefix)
	if token == "" {
		return nil, errUnauthenticated
	}
	subj, err := b.resolve(ctx, token, store.SubjectRunner)
	if err != nil {
		// The client learns only that it is not authenticated, never which; a store
		// fault is not a credential verdict, so it is retryable Unavailable instead.
		if errors.Is(err, auth.ErrTokenLookupFailed) {
			slog.WarnContext(ctx, "runner credential check unavailable", "error", err)
			return nil, errLookupUnavailable
		}
		if errors.Is(err, auth.ErrKeysUnavailable) {
			slog.WarnContext(ctx, "runner cluster keys unavailable", "error", err)
			return nil, errKeysUnavailable
		}
		return nil, errUnauthenticated
	}
	return withRunnerSubject(ctx, subj), nil
}

// bearerAuth holds the resolver the interceptors authenticate through.
type bearerAuth struct {
	resolve TokenResolver
}

// interceptor authenticates every RunnerService call before releasing its body deadline.
func (b *bearerAuth) interceptor() connect.ServerInterceptor {
	return func(next connect.ServerFunc) connect.ServerFunc {
		return func(ctx context.Context, spec connect.Spec, stream connect.ServerStream) error {
			var header *connect.Header
			if info, ok := connect.CallInfoForServerContext(ctx); ok {
				header = info.RequestHeader()
			}
			authed, err := b.authenticate(ctx, header)
			if err != nil {
				return err
			}
			bodydeadline.Release(authed)
			return next(authed, spec, stream)
		}
	}
}
