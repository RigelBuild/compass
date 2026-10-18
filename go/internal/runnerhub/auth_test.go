//go:build unix

package runnerhub

// Runner authentication rejects account, unknown, and revoked tokens with the same bare status.
// A Runner token succeeds and sets its subject on context.
// Tests exercise unary and streaming RPCs through the mounted door.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync/atomic"
	"testing"

	"connectrpc.com/connect/v2"

	"github.com/RigelBuild/compass/go/internal/auth"
	"github.com/RigelBuild/compass/go/internal/bodydeadline"
	compassv1internal "github.com/RigelBuild/compass/go/internal/gen/compass/v1"
	"github.com/RigelBuild/compass/go/internal/gen/compass/v1/compassv1internalconnect"
	"github.com/RigelBuild/compass/go/internal/store"
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
			if errors.Is(err, auth.ErrTokenLookupFailed) {
				t.Fatalf("%s error wraps lookup-failure sentinel", tc.name)
			}
		})
	}
}

func TestAuthenticateLookupFailureIsUnavailable(t *testing.T) {
	cause := errors.New("connection failed with db-password-secret")
	lookupErr := fmt.Errorf("%w: %w", auth.ErrTokenLookupFailed, cause)
	var presented string
	b := &bearerAuth{resolve: func(_ context.Context, token string, _ store.SubjectKind) (store.Subject, error) {
		presented = token
		return store.Subject{}, lookupErr
	}}
	handlerCalled := false
	server := connect.NewServer(b.interceptor())
	const procedure = "/compass.v1.RunnerService/Enroll"
	server.Register(connect.Method{
		Spec: connect.Spec{Procedure: procedure, StreamType: connect.StreamTypeUnary},
		Handler: func(context.Context, connect.Spec, connect.ServerStream) error {
			handlerCalled = true
			return nil
		},
	})
	var info connect.CallInfo
	info.RequestHeader().Set("Authorization", "Bearer runner-tok")
	err := server.Call(context.Background(), procedure, &info, nil)
	if err == nil {
		t.Fatal("Enroll during resolver failure succeeded, want Unavailable")
	}
	if got := connect.CodeOf(err); got != connect.CodeUnavailable {
		t.Fatalf("code = %v, want Unavailable", got)
	}
	if ce, ok := errors.AsType[*connect.Error](err); !ok || ce.Message() != "credential check unavailable" {
		t.Fatalf("error = %v, want fixed message %q", err, "credential check unavailable")
	}
	if strings.Contains(err.Error(), cause.Error()) {
		t.Fatalf("wire error %q contains lookup cause", err)
	}
	if presented != "runner-tok" {
		t.Fatalf("resolver token = %q, want runner-tok", presented)
	}
	if handlerCalled {
		t.Fatal("Enroll handler ran despite resolver failure")
	}
}

// Unusable cluster keys are an outage, not a verdict: the Runner must retry,
// with a fixed message and no cause on the wire.
func TestAuthenticateKeysUnavailableIsUnavailable(t *testing.T) {
	cause := fmt.Errorf("runner cluster %q: %w", "prod-eks", auth.ErrKeysUnavailable)
	b := &bearerAuth{resolve: func(context.Context, string, store.SubjectKind) (store.Subject, error) {
		return store.Subject{}, cause
	}}
	_, err := b.authenticate(t.Context(), bearerHeader("a.b.c"))
	if got := connect.CodeOf(err); got != connect.CodeUnavailable {
		t.Fatalf("code = %v, want Unavailable", got)
	}
	if ce, ok := errors.AsType[*connect.Error](err); !ok || ce.Message() != "runner cluster keys unavailable" {
		t.Fatalf("error = %v, want fixed message", err)
	}
	if strings.Contains(err.Error(), "prod-eks") {
		t.Fatalf("wire error %q leaks the cluster name", err)
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

// TestInterceptorReleasesBodyDeadlineOnlyAfterAuthentication checks release ordering.
func TestInterceptorReleasesBodyDeadlineOnlyAfterAuthentication(t *testing.T) {
	want := store.Subject{Kind: store.SubjectRunner, ID: "runner-7"}
	resolver := &fakeResolver{tokens: map[string]resolverEntry{"good-tok": {subj: want}}}
	interceptor := (&bearerAuth{resolve: resolver.resolve}).interceptor()

	t.Run("successful authentication", func(t *testing.T) {
		var releases atomic.Int32
		ctx := bodydeadline.WithRelease(t.Context(), func(context.Context) { releases.Add(1) })
		server := connect.NewServer(interceptor)
		server.Register(connect.Method{
			Spec: connect.Spec{Procedure: "/compass.v1.RunnerService/Enroll", StreamType: connect.StreamTypeUnary},
			Handler: func(context.Context, connect.Spec, connect.ServerStream) error {
				if got := releases.Load(); got != 1 {
					t.Fatalf("release calls before handler = %d, want 1", got)
				}
				return nil
			},
		})
		var info connect.CallInfo
		info.RequestHeader().Set("Authorization", "Bearer good-tok")
		if err := server.Call(ctx, "/compass.v1.RunnerService/Enroll", &info, nil); err != nil {
			t.Fatalf("authenticated Enroll = %v, want nil", err)
		}
		if got := releases.Load(); got != 1 {
			t.Fatalf("release calls = %d, want 1", got)
		}
	})

	t.Run("failed authentication", func(t *testing.T) {
		var releases atomic.Int32
		ctx := bodydeadline.WithRelease(t.Context(), func(context.Context) { releases.Add(1) })
		server := connect.NewServer(interceptor)
		server.Register(connect.Method{
			Spec: connect.Spec{Procedure: "/compass.v1.RunnerService/Enroll", StreamType: connect.StreamTypeUnary},
			Handler: func(context.Context, connect.Spec, connect.ServerStream) error {
				t.Fatal("handler ran despite failed authentication")
				return nil
			},
		})
		var info connect.CallInfo
		info.RequestHeader().Set("Authorization", "Bearer unknown-tok")
		err := server.Call(ctx, "/compass.v1.RunnerService/Enroll", &info, nil)
		if got := connect.CodeOf(err); got != connect.CodeUnauthenticated {
			t.Fatalf("failed auth code = %v, want Unauthenticated", got)
		}
		if got := releases.Load(); got != 0 {
			t.Fatalf("release calls = %d, want 0", got)
		}
	})
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
		_, err := rejectClient.Enroll(context.Background(), &compassv1internal.EnrollRequest{RunnerId: "runner-1"})
		assertUnauthenticated(t, err)
	})

	t.Run("sessions", func(t *testing.T) {
		stream, err := rejectClient.Sessions(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		// The interceptor can reject at connect before the handler. Send may return nil or EOF,
		// while Receive carries the authoritative status; checking only Send is racy.
		if err := stream.Send(&compassv1internal.SessionsRequest{}); err != nil && !errors.Is(err, io.EOF) {
			t.Fatalf("Send = %v, want nil or io.EOF-wrapped", err)
		}
		_, err = stream.Receive()
		assertUnauthenticated(t, err)
	})

	t.Run("publish events", func(t *testing.T) {
		stream, err := rejectClient.PublishEvents(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		// Same reject-race as sessions: the authoritative code is on
		// CloseAndReceive, not Send's closed-stream write.
		if err := stream.Send(&compassv1internal.PublishEventsRequest{}); err != nil && !errors.Is(err, io.EOF) {
			t.Fatalf("Send = %v, want nil or io.EOF-wrapped", err)
		}
		_, err = stream.CloseAndReceive()
		assertUnauthenticated(t, err)
	})

	t.Run("relay comms call", func(t *testing.T) {
		_, err := rejectClient.RelayCommsCall(context.Background(), &compassv1internal.RelayCommsCallRequest{})
		assertUnauthenticated(t, err)
	})

	t.Run("commit conversation frame", func(t *testing.T) {
		_, err := rejectClient.CommitConversationFrame(context.Background(), &compassv1internal.CommitConversationFrameRequest{})
		assertUnauthenticated(t, err)
	})
}

func TestSessionBlobRPCsRejectNonRunnerSubject(t *testing.T) {
	h := newHubOnly()
	resolver := &fakeResolver{tokens: map[string]resolverEntry{
		"account-tok": {subj: store.Subject{Kind: store.SubjectAccount, ID: "account-1"}},
	}}
	url := newMountedH2CServer(t, h, resolver.resolve)
	client := newRawRunnerClient(t, url, "account-tok")
	_, err := client.RelaySessionBlob(context.Background(), &compassv1internal.RelaySessionBlobRequest{})
	assertUnauthenticated(t, err)
	stream, err := client.FetchSessionBlobs(context.Background(), &compassv1internal.FetchSessionBlobsRequest{})
	if err != nil {
		assertUnauthenticated(t, err)
		return
	}
	_, err = stream.Receive()
	assertUnauthenticated(t, err)
	if closeErr := stream.Close(); closeErr != nil {
		t.Errorf("close FetchSessionBlobs stream: %v", closeErr)
	}
}

// Direct handler calls also enforce SubjectRunner, independent of the door.
func TestSessionBlobHandlersRejectAccountSubject(t *testing.T) {
	handler := NewHandler(newHubOnly(), nil, nil)
	ctx := withRunnerSubject(context.Background(), store.Subject{Kind: store.SubjectAccount, ID: "account-1"})
	_, err := handler.RelaySessionBlob(ctx, &compassv1internal.RelaySessionBlobRequest{})
	assertUnauthenticated(t, err)
	err = handler.FetchSessionBlobs(ctx, &compassv1internal.FetchSessionBlobsRequest{}, compassv1internalconnect.RunnerServiceFetchSessionBlobsServerStream{})
	assertUnauthenticated(t, err)
}

func TestSessionBlobRPCsRegisteredOverWire(t *testing.T) {
	h := newHubOnly()
	url := newMountedH2CServer(t, h, runnerResolverForFetch().resolve)
	client := newRawRunnerClient(t, url, "runner-tok")
	_, err := client.RelaySessionBlob(context.Background(), &compassv1internal.RelaySessionBlobRequest{})
	if got := connect.CodeOf(err); got != connect.CodeUnavailable {
		t.Fatalf("RelaySessionBlob without store = %v, want Unavailable (err %v)", got, err)
	}
	stream, err := client.FetchSessionBlobs(context.Background(), &compassv1internal.FetchSessionBlobsRequest{})
	if err != nil {
		if got := connect.CodeOf(err); got != connect.CodeUnavailable {
			t.Fatalf("FetchSessionBlobs without store = %v, want Unavailable (err %v)", got, err)
		}
		return
	}
	_, err = stream.Receive()
	if err == nil {
		t.Fatal("FetchSessionBlobs without store unexpectedly received a frame")
	}
	if closeErr := stream.Close(); closeErr != nil {
		t.Errorf("close FetchSessionBlobs stream: %v", closeErr)
	}
	if got := connect.CodeOf(err); got != connect.CodeUnavailable {
		t.Fatalf("FetchSessionBlobs without store = %v, want Unavailable (err %v)", got, err)
	}
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

	resp, err := client.Enroll(context.Background(), &compassv1internal.EnrollRequest{RunnerId: "runner-1"})
	if err != nil {
		t.Fatalf("Enroll with a valid runner token = %v, want success", err)
	}
	if resp.GetReattached() {
		t.Fatal("first Enroll reattached = true, want false")
	}
}

// A projected-token Runner cannot compute its own ID, so it enrolls with an
// empty runner_id and must learn the authenticated subject's ID from the reply.
func TestEnrollReturnsSubjectRunnerID(t *testing.T) {
	hub := newHubOnly()
	const id = "prod-eks/ip-10-0-1-5_ec2_internal"
	resolver := &fakeResolver{tokens: map[string]resolverEntry{
		"projected-tok": {subj: store.Subject{Kind: store.SubjectRunner, ID: id}},
	}}
	url := newMountedH2CServer(t, hub, resolver.resolve)
	client := newRawRunnerClient(t, url, "projected-tok")

	resp, err := client.Enroll(t.Context(), &compassv1internal.EnrollRequest{})
	if err != nil {
		t.Fatalf("Enroll with empty runner_id = %v, want success", err)
	}
	if got := resp.GetRunnerId(); got != id {
		t.Fatalf("EnrollResponse.runner_id = %q, want %q", got, id)
	}
}

// A non-empty runner_id must still match the token subject: valid Runner
// credentials cannot enroll under another Runner's identity.
func TestEnrollMismatchedRunnerIDIsUnauthenticated(t *testing.T) {
	hub := newHubOnly()
	resolver := &fakeResolver{tokens: map[string]resolverEntry{
		"runner-tok": {subj: store.Subject{Kind: store.SubjectRunner, ID: "prod/node-a"}},
	}}
	url := newMountedH2CServer(t, hub, resolver.resolve)
	client := newRawRunnerClient(t, url, "runner-tok")

	_, err := client.Enroll(t.Context(), &compassv1internal.EnrollRequest{RunnerId: "prod/node-b"})
	if got := connect.CodeOf(err); got != connect.CodeUnauthenticated {
		t.Fatalf("Enroll with mismatched runner_id = code %v (%v), want Unauthenticated", got, err)
	}
}

func TestEnrollFaultedDurableReapReturnsUnavailableOverWire(t *testing.T) {
	hub := newHubOnly()
	bindings := newFakeBindingStore()
	reap := &fakeSessionReapSink{}
	presence := &fakePresenceSink{}
	hub.SetSessionBindingStore(bindings)
	hub.SetSessionReapSink(reap)
	hub.SetPresenceSink(presence)

	reattached, err := hub.enroll(context.Background(), "runner-1", runnerSubject(), 0, 0)
	if err != nil || reattached {
		t.Fatalf("priming Enroll = (%v, %v), want fresh success", reattached, err)
	}
	bindSession(hub, "sess-live")

	reapErr := errors.New("private durable store detail")
	bindings.mu.Lock()
	bindings.deleteForRunnerErr = reapErr
	bindings.mu.Unlock()

	resolver := &fakeResolver{tokens: map[string]resolverEntry{
		"runner-tok": {subj: store.Subject{Kind: store.SubjectRunner, ID: "runner-1"}},
	}}
	url := newMountedH2CServer(t, hub, resolver.resolve)
	client := newRawRunnerClient(t, url, "runner-tok")
	_, err = client.Enroll(context.Background(), &compassv1internal.EnrollRequest{RunnerId: "runner-1"})
	if err == nil {
		t.Fatal("Enroll with a durable reap fault succeeded, want Unavailable")
	}
	if got := connect.CodeOf(err); got != connect.CodeUnavailable {
		t.Fatalf("Enroll error code = %v, want Unavailable: %v", got, err)
	}
	if strings.Contains(err.Error(), reapErr.Error()) {
		t.Fatalf("Enroll error leaked internal store detail: %v", err)
	}
	lifecycle := presence.lifecycleSnapshot()
	if len(lifecycle) != 1 || lifecycle[0].sessionID != "sess-live" || lifecycle[0].account != testAgentAccount {
		t.Fatalf("presence lifecycle callbacks = %+v, want sess-live disconnected before Enroll returns", lifecycle)
	}
	reapCalls := reap.snapshot()
	if len(reapCalls) != 2 || len(reapCalls[1]) != 1 || reapCalls[1][0] != "sess-live" {
		t.Fatalf("session reap callbacks = %+v, want sess-live reaped before Enroll returns", reapCalls)
	}
	if account, ok := hub.accountForSession(context.Background(), "sess-live"); ok {
		t.Fatalf("faulted Enroll resurrected stale row for %q", account)
	}
	reattached, err = hub.enroll(context.Background(), "runner-1", runnerSubject(), 0, 0)
	if !reattached || !errors.Is(err, reapErr) {
		t.Fatalf("faulted Hub.enroll = (%v, %v), want reattached and original reap error", reattached, err)
	}
}

func TestSuccessfulDurableReapEnrollsNormallyOverWire(t *testing.T) {
	hub := newHubOnly()
	bindings := newFakeBindingStore()
	hub.SetSessionBindingStore(bindings)
	resolver := &fakeResolver{tokens: map[string]resolverEntry{
		"runner-tok": {subj: store.Subject{Kind: store.SubjectRunner, ID: "runner-1"}},
	}}
	url := newMountedH2CServer(t, hub, resolver.resolve)
	client := newRawRunnerClient(t, url, "runner-tok")

	first, err := client.Enroll(context.Background(), &compassv1internal.EnrollRequest{RunnerId: "runner-1"})
	if err != nil || first.GetReattached() {
		t.Fatalf("first Enroll = (%v, %v), want fresh success", first, err)
	}
	second, err := client.Enroll(context.Background(), &compassv1internal.EnrollRequest{RunnerId: "runner-1"})
	if err != nil || !second.GetReattached() {
		t.Fatalf("second Enroll = (%v, %v), want successful reattached response", second, err)
	}
}

func TestResolverFaultReturnsUnavailableOverWire(t *testing.T) {
	hub := newHubOnly()
	const secretCause = "db-password-secret"
	resolve := func(context.Context, string, store.SubjectKind) (store.Subject, error) {
		return store.Subject{}, fmt.Errorf("%w: %s", auth.ErrTokenLookupFailed, secretCause)
	}
	url := newMountedH2CServer(t, hub, resolve)
	client := newRawRunnerClient(t, url, "runner-tok")

	_, err := client.Enroll(context.Background(), &compassv1internal.EnrollRequest{RunnerId: "runner-1"})
	if err == nil {
		t.Fatal("Enroll during resolver failure succeeded, want Unavailable")
	}
	if got := connect.CodeOf(err); got != connect.CodeUnavailable {
		t.Fatalf("Enroll code = %v, want Unavailable", got)
	}
	if strings.Contains(err.Error(), secretCause) {
		t.Fatalf("Enroll error %q contains resolver cause", err)
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

	_, err := client.Enroll(context.Background(), &compassv1internal.EnrollRequest{RunnerId: "runner-1"})
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
