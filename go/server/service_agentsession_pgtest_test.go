//go:build pgtest && unix

package server

// Store-gated handler contracts for SubscribeAgentSession: the
// three branches that reach RequireAgentSessionSubscriber and therefore need a
// real Postgres behind the authz gate. Driven through the production network-door
// interceptor chain (bearer + admin gate) over a real connect client so the
// handler reads a genuine caller identity the same way the shipped door supplies
// it — the caller-injection convention networkDoorHandler already uses; this test
// does not invent a new path. Behind `pgtest && unix` via the shared pgtest
// harness (SKIP when no runtime). The two store-free branches (nil tail, no
// caller) live in service_agentsession_test.go on the default lane.
//
// No replay/snapshot is exercised: the handler has no replay phase
// (service.go:276 "No snapshot replay: live tail"), so delivery is asserted only
// for frames relayed AFTER the subscription is live.

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"testing"
	"time"

	"connectrpc.com/connect"

	"github.com/sealedsecurity/compass/go/events"
	compassv1 "github.com/sealedsecurity/compass/go/gen/compass/v1"
	"github.com/sealedsecurity/compass/go/gen/compass/v1/compassv1connect"
	"github.com/sealedsecurity/compass/go/internal/auth"
	"github.com/sealedsecurity/compass/go/internal/pgtest"
	"github.com/sealedsecurity/compass/go/internal/store"
)

// agentSessionFixture seeds one full ownership chain (owner user -> agent ->
// container -> session) plus a bootstrap admin (the AdminGate's admin param) and
// an unrelated outsider, and stands up a service whose network door mounts the
// real bearer + admin-gate chain. It returns the wired client, the live session
// tail (to relay frames), the recorded session id, and bearer tokens for the
// authorized owner (a member of the agent's home channel, seeded at CreateAgent)
// and the outsider (a member of nothing relevant). Skips when no Postgres runtime
// is available.
type agentSessionFixture struct {
	client     compassv1connect.CompassServiceClient
	tail       *sessionTail
	sessionID  string
	ownerToken string
	otherToken string
}

func newAgentSessionFixture(t *testing.T) agentSessionFixture {
	t.Helper()
	ctx := context.Background()
	st, err := store.Open(ctx, pgtest.RequireDSN(t))
	if err != nil {
		t.Fatalf("store Open: %v", err)
	}
	t.Cleanup(st.Close)

	admin, err := st.BootstrapAdmin(ctx, store.NewUser{Handle: "admin", DisplayName: "admin"})
	if err != nil {
		t.Fatalf("BootstrapAdmin: %v", err)
	}
	owner, err := st.CreateUser(ctx, store.NewUser{Handle: "owner", DisplayName: "owner"})
	if err != nil {
		t.Fatalf("CreateUser(owner): %v", err)
	}
	outsider, err := st.CreateUser(ctx, store.NewUser{Handle: "outsider", DisplayName: "outsider"})
	if err != nil {
		t.Fatalf("CreateUser(outsider): %v", err)
	}
	agent, err := st.CreateAgent(ctx, owner.ID, store.NewAgent{Handle: "agent", DisplayName: "agent"})
	if err != nil {
		t.Fatalf("CreateAgent: %v", err)
	}
	const sessionID = "sess-1"
	if err := st.RecordAgentSession(ctx, sessionID, agent.ID); err != nil {
		t.Fatalf("RecordAgentSession: %v", err)
	}

	ownerTok, err := auth.IssueAccountToken(ctx, st, owner.ID)
	if err != nil {
		t.Fatalf("IssueAccountToken(owner): %v", err)
	}
	otherTok, err := auth.IssueAccountToken(ctx, st, outsider.ID)
	if err != nil {
		t.Fatalf("IssueAccountToken(outsider): %v", err)
	}

	bus := events.NewBus[busPayload]()
	t.Cleanup(bus.Close)
	tail := newSessionTail()
	svc := newService("test", bus, st, nil, nil, nil, tail)
	url := newH2CTestServerWithInterceptors(t, svc,
		auth.BearerInterceptor(st),
		auth.BearerStreamInterceptor(st),
		auth.NewAdminGate(admin.ID),
	)
	return agentSessionFixture{
		client:     newH2CClient(t, url),
		tail:       tail,
		sessionID:  sessionID,
		ownerToken: ownerTok,
		otherToken: otherTok,
	}
}

// subscribeErr opens a SubscribeAgentSession stream carrying the given bearer for
// the given session id and returns the connect.Error the door surfaces on the
// first Receive. A rejection (authz gate before any Send) surfaces there; a
// delivered frame is a contract violation for the reject cases. It never sleeps —
// recvAgentFrameOrTimeout is the deadline safety net.
func subscribeErr(t *testing.T, client compassv1connect.CompassServiceClient, bearer, sessionID string) *connect.Error {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()
	req := connect.NewRequest(&compassv1.SubscribeAgentSessionRequest{SessionId: sessionID})
	req.Header().Set("Authorization", "Bearer "+bearer)
	stream, err := client.SubscribeAgentSession(ctx, req)
	if err == nil {
		defer func() { _ = stream.Close() }()
		if recvAgentFrameOrTimeout(t, stream) {
			t.Fatalf("a rejected SubscribeAgentSession stream delivered a frame: %+v", stream.Msg())
		}
		err = stream.Err()
	}
	var ce *connect.Error
	if !errors.As(err, &ce) {
		t.Fatalf("expected a connect.Error, got %v", err)
	}
	return ce
}

// waitTailSubscribers event-gates on the tail's subscriber count for sessionID
// reaching want. It is a monotonic readiness gate (runtime.Gosched yields to the
// handler goroutine between probes; timeAfter is the deadline safety net), never
// a wall-clock sleep — the handler registers its subscriber synchronously once
// authz passes, so the count advances the instant the handler reaches the tail.
func waitTailSubscribers(t *testing.T, tail *sessionTail, sessionID string, want int) {
	t.Helper()
	if !tailSubscribersReach(tail, sessionID, want, timeAfter()) {
		tail.mu.Lock()
		n := len(tail.subs[sessionID])
		tail.mu.Unlock()
		t.Fatalf("tail subscriber count for %q = %d, never reached %d", sessionID, n, want)
	}
}

// tailSubscribersReach polls (yielding, never sleeping) until the tail's
// subscriber count for sessionID equals want or the deadline fires, returning
// whether it reached want. Safe to call off the test goroutine (no t.Fatal), so
// the delivery test can gate a concurrent relay on the handler having
// registered its subscriber.
func tailSubscribersReach(tail *sessionTail, sessionID string, want int, deadline <-chan time.Time) bool {
	for {
		tail.mu.Lock()
		n := len(tail.subs[sessionID])
		tail.mu.Unlock()
		if n == want {
			return true
		}
		select {
		case <-deadline:
			return false
		default:
			runtime.Gosched()
		}
	}
}

// TestSubscribeAgentSessionNotFoundParity pins branch 3 (service.go:294-299): a
// caller that fails RequireAgentSessionSubscriber with store.ErrNotFound —
// whether the session is UNKNOWN or exists but the caller is NOT a member — is
// refused with CodeNotFound, never PermissionDenied (which would confirm the
// session exists). The message is a pure template of the presented session_id
// (`agent session %q`), carrying no bit that distinguishes "unknown" from
// "forbidden": asserting the exact message for both the foreign-real and the
// unknown case proves the merge (D9). A two-step resolve-then-check that returned
// a distinct forbidden error, or a message that leaked the reason, reddens this.
func TestSubscribeAgentSessionNotFoundParity(t *testing.T) {
	f := newAgentSessionFixture(t)

	// The outsider is not a member of the agent's home channel: the real session
	// resolves but membership fails → ErrNotFound.
	foreignReal := subscribeErr(t, f.client, f.otherToken, f.sessionID)
	// A never-recorded session: the chain resolves nothing → ErrNotFound.
	const ghostID = "does-not-exist"
	unknown := subscribeErr(t, f.client, f.otherToken, ghostID)

	if foreignReal.Code() != connect.CodeNotFound {
		t.Fatalf("foreign real session code = %v, want CodeNotFound (never PermissionDenied)", foreignReal.Code())
	}
	if unknown.Code() != connect.CodeNotFound {
		t.Fatalf("unknown session code = %v, want CodeNotFound", unknown.Code())
	}
	// The message is `agent session %q` templated on the input id and nothing
	// else — so it reveals only what the caller already presented, never whether
	// the session exists. Assert the exact form for both cases.
	if got, want := foreignReal.Message(), fmt.Sprintf("agent session %q", f.sessionID); got != want {
		t.Fatalf("foreign-session message = %q, want %q (must carry only the id, no forbidden/unknown leak)", got, want)
	}
	if got, want := unknown.Message(), fmt.Sprintf("agent session %q", ghostID); got != want {
		t.Fatalf("unknown-session message = %q, want %q (must carry only the id)", got, want)
	}
}

// TestSubscribeAgentSessionDeliversLiveFrameToAuthorizedMember pins branch 4
// (service.go:302-318) and branch 5 (service.go:307-310 + the defer at :306): an
// authorized home-channel member subscribes, a frame relayed AFTER the
// subscription is live reaches the client stream faithfully repackaged, and
// cancelling the client context ends the handler cleanly and frees the fan-out
// slot. Deterministic: delivery is event-gated on the tail registering the
// subscriber (no relay before a subscriber exists — that would be a silent
// no-op), and the clean-exit half is gated on the slot count returning to zero.
func TestSubscribeAgentSessionDeliversLiveFrameToAuthorizedMember(t *testing.T) {
	f := newAgentSessionFixture(t)

	// connect's server-stream client call blocks in RoundTrip until the handler
	// flushes response headers, which happens on its FIRST Send — i.e. only once a
	// frame has been relayed. The handler registers its tail subscriber during
	// that blocked call, so the relay must run concurrently: gate it on the
	// subscriber appearing (no relay before a subscriber exists — that would be a
	// silent no-op), then relay. This unblocks the client's subscribe. No sleep;
	// the gate is the readiness signal and timeAfter is the deadline safety net.
	go func() {
		if tailSubscribersReach(f.tail, f.sessionID, 1, timeAfter()) {
			f.tail.RelaySessionFrame(f.sessionID, traceFrame("live-trace"))
		}
	}()

	ctx, cancel := context.WithCancel(context.Background())
	req := connect.NewRequest(&compassv1.SubscribeAgentSessionRequest{SessionId: f.sessionID})
	req.Header().Set("Authorization", "Bearer "+f.ownerToken)
	stream, err := f.client.SubscribeAgentSession(ctx, req)
	if err != nil {
		cancel()
		t.Fatalf("authorized owner SubscribeAgentSession: %v", err)
	}

	if !recvAgentFrameOrTimeout(t, stream) {
		cancel()
		t.Fatalf("authorized subscribe delivered no frame: first Receive = false, err = %v", stream.Err())
	}
	got := stream.Msg()
	if got.GetSessionId() != f.sessionID {
		cancel()
		t.Fatalf("frame session_id = %q, want %q (stamped from the routing key)", got.GetSessionId(), f.sessionID)
	}
	if text := got.GetEvent().GetAssistantText().GetText(); text != "live-trace" {
		cancel()
		t.Fatalf("frame trace text = %q, want live-trace (the relayed payload, faithfully repackaged)", text)
	}

	// Branch 5: client hang-up. Cancelling the client context makes the handler's
	// ctx.Done fire; it returns nil (clean end) and the deferred unsubscribe
	// frees the slot. Gate on the slot returning to zero — the observable proof
	// the handler exited its loop and ran the defer, no leak.
	cancel()
	_ = stream.Close()
	waitTailSubscribers(t, f.tail, f.sessionID, 0)
}
