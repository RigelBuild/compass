//go:build unix

package runnerhub

// OQ7 cross-door authentication (the RunnerService side). The two mandatory
// contracts:
//   1. NO ORACLE: an account-subject token, an unknown token, and a revoked
//      token all fail with the identical bare CodeUnauthenticated — the client
//      cannot distinguish the three causes.
//   2. A valid SubjectRunner token is accepted and its subject is set on ctx.
//
// The reject side is exercised on every RPC path (Enroll unary + Sessions/
// PublishEvents streaming + RelayCommsCall unary) over the real generated client
// and the mounted door, so the whole interceptor chain is proven, not just the
// pure authenticate().

import (
	"context"
	"errors"
	"io"
	"testing"

	"connectrpc.com/connect"

	compassv1internal "github.com/sealedsecurity/compass/go/internal/gen/compass/v1"
	"github.com/sealedsecurity/compass/go/internal/store"
)

// authenticate() collapses every failure cause to the identical bare
// Unauthenticated with no distinguishing detail — the no-oracle contract. A bug
// that leaked the store sentinel (ErrNotFound vs ErrTokenRevoked vs wrong-kind)
// into the client-visible error would let a caller probe which tokens exist.
func TestAuthenticateNoOracleAcrossCauses(t *testing.T) {
	resolver := &fakeResolver{tokens: map[string]resolverEntry{
		"account-tok": {subj: store.Subject{Kind: store.SubjectAccount, ID: "acct-1"}},
		"revoked-tok": {subj: store.Subject{Kind: store.SubjectRunner, ID: "runner-1"}, revoked: true},
		// "unknown-tok" deliberately absent → ErrNotFound.
	}}
	b := &bearerAuth{resolve: resolver.resolve}

	cases := []struct {
		name  string
		token string
	}{
		{"account subject (wrong kind)", "account-tok"},
		{"unknown token", "unknown-tok"},
		{"revoked token", "revoked-tok"},
		{"missing bearer scheme", "not-bearer-see-below"}, // handled specially below
	}

	var refErr error
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var header staticHeader
			if tc.name == "missing bearer scheme" {
				header = staticHeader{"Authorization": "Token account-tok"} // no "Bearer " prefix
			} else {
				header = bearerHeader(tc.token)
			}
			_, err := b.authenticate(context.Background(), header)
			if err == nil {
				t.Fatalf("%s authenticated, want Unauthenticated", tc.name)
			}
			if got := connect.CodeOf(err); got != connect.CodeUnauthenticated {
				t.Fatalf("%s code = %v, want Unauthenticated", tc.name, got)
			}
			// The no-oracle assertion: every cause yields the byte-identical
			// error message. Compare each against the first.
			if i == 0 {
				refErr = err
			} else if err.Error() != refErr.Error() {
				t.Fatalf("%s error = %q, differs from reference %q — that is an oracle distinguishing the cause",
					tc.name, err.Error(), refErr.Error())
			}
			// And it must not leak the underlying store sentinel.
			if errors.Is(err, store.ErrNotFound) || errors.Is(err, store.ErrTokenRevoked) {
				t.Fatalf("%s error wraps a store sentinel (%v) — the cause leaks to the client", tc.name, err)
			}
		})
	}
}

// A valid SubjectRunner token is accepted and its subject is set on the returned
// context, so the handler can read it. A bug that dropped the subject would fail
// the defense-in-depth check in Enroll.
func TestAuthenticateAcceptsRunnerTokenAndSetsSubject(t *testing.T) {
	want := store.Subject{Kind: store.SubjectRunner, ID: "runner-7"}
	resolver := &fakeResolver{tokens: map[string]resolverEntry{"good-tok": {subj: want}}}
	b := &bearerAuth{resolve: resolver.resolve}

	ctx, err := b.authenticate(context.Background(), bearerHeader("good-tok"))
	if err != nil {
		t.Fatalf("authenticate(valid runner token) = %v, want nil", err)
	}
	got, ok := runnerSubjectFrom(ctx)
	if !ok {
		t.Fatal("no runner subject on the authenticated context; the door must set it")
	}
	if got != want {
		t.Fatalf("context subject = %+v, want %+v", got, want)
	}
}

// End-to-end over the mounted door: an account token is rejected Unauthenticated
// on EVERY RunnerService RPC path (Enroll unary, Sessions bidi, PublishEvents
// client-stream, RelayCommsCall unary). Each RPC opens through the real generated
// client, so the interceptor wiring — unary and streaming — is proven, not just
// authenticate().
func TestAccountTokenRejectedOnEveryRPCPath(t *testing.T) {
	hub := newHubOnly()
	resolver := &fakeResolver{tokens: map[string]resolverEntry{
		"account-tok": {subj: store.Subject{Kind: store.SubjectAccount, ID: "acct-1"}},
		"runner-tok":  {subj: store.Subject{Kind: store.SubjectRunner, ID: "runner-1"}},
	}}
	url := newMountedH2CServer(t, hub, resolver.resolve)

	rejectClient := newRawRunnerClient(t, url, "account-tok")

	t.Run("enroll", func(t *testing.T) {
		_, err := rejectClient.Enroll(context.Background(), connect.NewRequest(&compassv1internal.EnrollRequest{RunnerId: "runner-1"}))
		assertUnauthenticated(t, err)
	})

	t.Run("sessions", func(t *testing.T) {
		stream := rejectClient.Sessions(context.Background())
		// The interceptor rejects at connect (before the handler). Per the
		// connect-go contract, if the server has already closed when Send runs,
		// Send returns an io.EOF-wrapped error and the authoritative status is
		// retrieved via Receive; if Send wins the race it returns nil and the
		// status surfaces on Receive directly. Either way the real code is on
		// Receive, so always drain there — asserting on Send's return is racy
		// (it can be a transport-level Unknown on the closed-stream write).
		if err := stream.Send(&compassv1internal.SessionsRequest{}); err != nil && !errors.Is(err, io.EOF) {
			t.Fatalf("Send = %v, want nil or io.EOF-wrapped", err)
		}
		_, err := stream.Receive()
		assertUnauthenticated(t, err)
	})

	t.Run("publish events", func(t *testing.T) {
		stream := rejectClient.PublishEvents(context.Background())
		// Same reject-race as sessions: the authoritative code is on
		// CloseAndReceive, not Send's closed-stream write.
		if err := stream.Send(&compassv1internal.PublishEventsRequest{}); err != nil && !errors.Is(err, io.EOF) {
			t.Fatalf("Send = %v, want nil or io.EOF-wrapped", err)
		}
		_, err := stream.CloseAndReceive()
		assertUnauthenticated(t, err)
	})

	t.Run("relay comms call", func(t *testing.T) {
		_, err := rejectClient.RelayCommsCall(context.Background(), connect.NewRequest(&compassv1internal.RelayCommsCallRequest{}))
		assertUnauthenticated(t, err)
	})

	t.Run("relay comms call", func(t *testing.T) {
		_, err := rejectClient.RelayCommsCall(context.Background(), connect.NewRequest(&compassv1internal.RelayCommsCallRequest{}))
		assertUnauthenticated(t, err)
	})

	t.Run("relay comms call", func(t *testing.T) {
		_, err := rejectClient.RelayCommsCall(context.Background(), connect.NewRequest(&compassv1internal.RelayCommsCallRequest{}))
		assertUnauthenticated(t, err)
	})
}

// The positive wire path: a SubjectRunner token is accepted end-to-end — Enroll
// succeeds through the mounted door. Proves the door does not reject everything.
func TestRunnerTokenAcceptedOverWire(t *testing.T) {
	hub := newHubOnly()
	resolver := &fakeResolver{tokens: map[string]resolverEntry{
		"runner-tok": {subj: store.Subject{Kind: store.SubjectRunner, ID: "runner-1"}},
	}}
	url := newMountedH2CServer(t, hub, resolver.resolve)
	client := newRawRunnerClient(t, url, "runner-tok")

	resp, err := client.Enroll(context.Background(), connect.NewRequest(&compassv1internal.EnrollRequest{RunnerId: "runner-1"}))
	if err != nil {
		t.Fatalf("Enroll with a valid runner token = %v, want success", err)
	}
	if resp.Msg.GetReattached() {
		t.Fatal("first Enroll reattached = true, want false")
	}
}

// A missing/empty bearer credential over the wire is Unauthenticated too — the
// no-credential path, distinct from a bad credential but the same client-visible
// code.
func TestMissingTokenRejectedOverWire(t *testing.T) {
	hub := newHubOnly()
	resolver := &fakeResolver{tokens: map[string]resolverEntry{}}
	url := newMountedH2CServer(t, hub, resolver.resolve)
	client := newRawRunnerClient(t, url, "") // no token stamped

	_, err := client.Enroll(context.Background(), connect.NewRequest(&compassv1internal.EnrollRequest{RunnerId: "runner-1"}))
	assertUnauthenticated(t, err)
}

// assertUnauthenticated fails unless err carries CodeUnauthenticated.
func assertUnauthenticated(t *testing.T, err error) {
	t.Helper()
	if err == nil {
		t.Fatal("call succeeded, want Unauthenticated")
	}
	if got := connect.CodeOf(err); got != connect.CodeUnauthenticated {
		t.Fatalf("code = %v, want Unauthenticated", got)
	}
}
