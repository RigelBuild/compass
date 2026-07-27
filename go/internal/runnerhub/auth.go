//go:build unix

// The RunnerService door's authentication: a bearer-token interceptor that
// authenticates the Runner-subject token on every Enroll/Sessions/PublishEvents
// call and rejects anything that is not a live SubjectRunner token with a bare
// CodeUnauthenticated.
//
// OQ7 (go-toolchain-default.md:1410-1423): the security check itself is the
// SHARED auth.ResolveToken helper (sha256 → ResolveTokenHash → Kind gate), built
// once in the T3 network-door lane and consumed here verbatim — this door only
// asks it for a SubjectRunner token. An account token that reaches this door
// fails the Kind gate → Unauthenticated: that IS the RunnerService side of the
// two mandatory cross-door rejection tests. not-found / revoked / wrong-kind all
// map to the same bare Unauthenticated (no oracle); the distinct sentinels are
// for server-side logging only (compass ruling).
//
// The resolver is injected as TokenResolver rather than imported directly so the
// handler compiles and tests run before the T3 lane lands: a test drives a fake
// resolver, and on the T3 rebase the binding is
// `func(ctx, p, w) (store.Subject, error) { return auth.ResolveToken(ctx, st, p, w) }`.
package runnerhub

import (
	"context"
	"errors"
	"strings"

	"connectrpc.com/connect"

	"github.com/sealedsecurity/compass/go/internal/store"
)

// TokenResolver is the shared credential-resolution seam: sha256 the presented
// token, resolve it in the store, and Kind-gate it against want. It mirrors the
// T3 lane's exported auth.ResolveToken(ctx, st, presented, want) with the store
// closed over. Returns the resolved subject, or a store sentinel (ErrNotFound /
// ErrTokenRevoked / a wrong-kind error) the door collapses to Unauthenticated.
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

// errUnauthenticated is the single opaque error every auth failure maps to — no
// detail distinguishes not-found, revoked, or wrong-kind to the client (no
// oracle). The distinct store sentinels are logged server-side only.
var errUnauthenticated = connect.NewError(connect.CodeUnauthenticated, errors.New("unauthenticated"))

// authenticate extracts the bearer token from the request header, resolves it as
// a SubjectRunner token, and returns a context carrying the subject. Any
// failure — missing/malformed header, not-found, revoked, wrong kind — returns
// errUnauthenticated with no distinguishing detail.
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
		// A store sentinel (ErrNotFound / ErrTokenRevoked / wrong-kind) collapses
		// to a bare Unauthenticated: the client learns only that it is not
		// authenticated, never which. The resolver logs the distinct cause.
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
