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

	"connectrpc.com/connect"

	"github.com/RigelBuild/compass/go/internal/auth"
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
var errUnauthenticated = connect.NewError(connect.CodeUnauthenticated, errors.New("unauthenticated"))

// errLookupUnavailable is the fixed store-fault response; the cause can name DB hosts, so it stays server-side.
var errLookupUnavailable = connect.NewError(connect.CodeUnavailable, errors.New("credential check unavailable"))

// authenticate extracts the bearer token from the request header, resolves it as
// a SubjectRunner token, and returns a context carrying the subject. Any
// credential failure — missing/malformed header, not-found, revoked, wrong kind —
// returns errUnauthenticated with no distinguishing detail; a store fault returns
// errLookupUnavailable.
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
		return nil, errUnauthenticated
	}
	return withRunnerSubject(ctx, subj), nil
}

// bearerAuth holds the resolver the interceptors authenticate through.
type bearerAuth struct {
	resolve TokenResolver
}

// unaryInterceptor authenticates a unary call (Enroll) before it reaches the
// handler, setting the Runner subject on the context it forwards.
func (b *bearerAuth) unaryInterceptor() connect.UnaryInterceptorFunc {
	return func(next connect.UnaryFunc) connect.UnaryFunc {
		return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
			authed, err := b.authenticate(ctx, req.Header())
			if err != nil {
				return nil, err
			}
			return next(authed, req)
		}
	}
}

// streamInterceptor authenticates a streaming call (Sessions bidi, PublishEvents
// client-stream) at connect, before the handler runs, setting the Runner subject
// on the stream's context.
func (b *bearerAuth) streamInterceptor() connect.Interceptor {
	return &streamAuth{auth: b}
}

// streamAuth is the streaming half of the bearer interceptor. It authenticates
// the handler side (the Runner dials in, so the server always terminates the
// stream) and passes client-side calls through untouched.
type streamAuth struct {
	auth *bearerAuth
}

// WrapUnary passes unary calls through — the unary path is handled by
// unaryInterceptor; a streamAuth used in a stream-only chain must still satisfy
// the full Interceptor interface.
func (s *streamAuth) WrapUnary(next connect.UnaryFunc) connect.UnaryFunc {
	return next
}

// WrapStreamingClient passes client-side streaming through — this door only
// terminates server-side streams (the Runner is the client).
func (s *streamAuth) WrapStreamingClient(next connect.StreamingClientFunc) connect.StreamingClientFunc {
	return next
}

// WrapStreamingHandler authenticates the incoming stream via its request header,
// then serves it on a context carrying the Runner subject.
func (s *streamAuth) WrapStreamingHandler(next connect.StreamingHandlerFunc) connect.StreamingHandlerFunc {
	return func(ctx context.Context, conn connect.StreamingHandlerConn) error {
		authed, err := s.auth.authenticate(ctx, conn.RequestHeader())
		if err != nil {
			return err
		}
		return next(authed, conn)
	}
}
