//go:build pgtest && unix

package server

// Store-gated handler contracts for SubscribeAgentSession: the three branches that
// reach RequireAgentSessionSubscriber and so need a real Postgres behind the authz
// gate. Driven through the production network-door chain so the handler reads a genuine
// caller identity. No replay phase, so delivery is asserted only for post-sub frames.

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"testing"
	"time"

	"connectrpc.com/connect/v2"

	"github.com/RigelBuild/compass/go/events"
	compassv1 "github.com/RigelBuild/compass/go/gen/compass/v1"
	"github.com/RigelBuild/compass/go/gen/compass/v1/compassv1connect"
	"github.com/RigelBuild/compass/go/internal/auth"
	"github.com/RigelBuild/compass/go/internal/pgtest"
	"github.com/RigelBuild/compass/go/internal/store"
)

// Seeds owner and outsider identities through production auth gates; returns
// the client, session tail, and both bearer tokens.
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

// Opens a stream and returns its rejection on first Receive; rejections send
// no frame, and Receive has a deadline guard.
func subscribeErr(t *testing.T, client compassv1connect.CompassServiceClient, bearer, sessionID string) *connect.Error {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()
	req := &compassv1.SubscribeAgentSessionRequest{SessionId: sessionID}
	ctx, info := connect.NewClientContext(ctx)
	info.RequestHeader().Set("Authorization", "Bearer "+bearer)
	stream, err := client.SubscribeAgentSession(ctx, req)
	if err == nil {
		defer func() { _ = stream.Close() }()
		frame, receiveErr := receiveAgentFrameOrTimeout(t, stream)
		if receiveErr == nil {
			t.Fatalf("a rejected SubscribeAgentSession stream delivered a frame: %+v", frame)
		}
		err = receiveErr
	}
	ce, ok := errors.AsType[*connect.Error](err)
	if !ok {
		t.Fatalf("expected a connect.Error, got %v", err)
	}
	return ce
}

// Waits on the monotonic subscriber count; authz registers synchronously and
// the deadline bounds probes.
func waitTailSubscribers(t *testing.T, tail *sessionTail, sessionID string, want int) {
	t.Helper()
	if !tailSubscribersReach(tail, sessionID, want, timeAfter()) {
		tail.mu.Lock()
		n := len(tail.subs[sessionID])
		tail.mu.Unlock()
		t.Fatalf("tail subscriber count for %q = %d, never reached %d", sessionID, n, want)
	}
}

// Reads subscriber counts for concurrent relays waiting on handler registration.
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

// Unknown sessions and non-members both fail closed with CodeNotFound and the
// same session-id template. The message must not reveal whether the session exists.
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

// Verifies the registration ack precedes a relayed frame and cancellation frees
// the fan-out slot. Both delivery and cleanup are event-gated on subscriber count.
func TestSubscribeAgentSessionDeliversLiveFrameToAuthorizedMember(t *testing.T) {
	f := newAgentSessionFixture(t)

	// The client call waits for the handler's first Send, so relay must run
	// concurrently. Wait until the handler registers before relaying; this
	// avoids a silent no-op and uses a deadline instead of sleep.
	go func() {
		if tailSubscribersReach(f.tail, f.sessionID, 1, timeAfter()) {
			f.tail.RelaySessionFrame(f.sessionID, traceFrame("live-trace"))
		}
	}()

	ctx, cancel := context.WithCancel(context.Background())
	req := &compassv1.SubscribeAgentSessionRequest{SessionId: f.sessionID}
	ctx, info := connect.NewClientContext(ctx)
	info.RequestHeader().Set("Authorization", "Bearer "+f.ownerToken)
	stream, err := f.client.SubscribeAgentSession(ctx, req)
	if err != nil {
		cancel()
		t.Fatalf("authorized owner SubscribeAgentSession: %v", err)
	}

	// The leading ack stamps the session ID but carries no event or transition.
	// Assert it before reading the relayed trace frame.
	ack, receiveErr := receiveAgentFrameOrTimeout(t, stream)
	if receiveErr != nil {
		cancel()
		t.Fatalf("authorized subscribe delivered no leading ack: %v", receiveErr)
	}
	if ack.GetSessionId() != f.sessionID {
		cancel()
		t.Fatalf("ack session_id = %q, want %q (stamped from the routing key)", ack.GetSessionId(), f.sessionID)
	}
	if ack.GetEvent() != nil {
		cancel()
		t.Fatalf("ack carries an event %v, want nil (the ack is a zero-payload registration marker)", ack.GetEvent())
	}
	if st := ack.GetState(); st != compassv1.AgentSessionState_AGENT_SESSION_STATE_UNSPECIFIED {
		cancel()
		t.Fatalf("ack state = %v, want UNSPECIFIED (the ack carries no transition)", st)
	}

	// Second frame is the relayed live trace, faithfully repackaged.
	got, receiveErr := receiveAgentFrameOrTimeout(t, stream)
	if receiveErr != nil {
		cancel()
		t.Fatalf("authorized subscribe delivered no relayed frame: %v", receiveErr)
	}
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

// An authorized member's first frame must be the registration ack: session ID
// set, no event, and unspecified state. It is sent at registration without a
// relayed frame, so opening an idle session cannot deadlock.
func TestSubscribeAgentSessionSendsLeadingRegistrationAck(t *testing.T) {
	f := newAgentSessionFixture(t)

	req := &compassv1.SubscribeAgentSessionRequest{SessionId: f.sessionID}
	ctx := clientContextWithBearer(t.Context(), "Bearer "+f.ownerToken)
	stream, err := f.client.SubscribeAgentSession(ctx, req)
	if err != nil {
		t.Fatalf("authorized owner SubscribeAgentSession: %v", err)
	}
	defer func() { _ = stream.Close() }() // test cleanup; Close error not actionable

	frame, receiveErr := receiveAgentFrameOrTimeout(t, stream)
	if receiveErr != nil {
		t.Fatalf("no leading registration-ack: %v", receiveErr)
	}
	if frame.GetSessionId() != f.sessionID {
		t.Fatalf("ack session_id = %q, want %q (stamped from the routing key)", frame.GetSessionId(), f.sessionID)
	}
	if frame.GetEvent() != nil {
		t.Fatalf("ack carries an event %v, want nil (the ack is a zero-payload registration marker)", frame.GetEvent())
	}
	if st := frame.GetState(); st != compassv1.AgentSessionState_AGENT_SESSION_STATE_UNSPECIFIED {
		t.Fatalf("ack state = %v, want UNSPECIFIED (the ack carries no transition)", st)
	}
}
