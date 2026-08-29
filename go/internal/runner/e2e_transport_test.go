//go:build unix

package runner

// T5 Half-A end-to-end proof (SEA-1351): the Runner-side agent->Runner call
// transport, exercised through the REAL integrated stack rather than the T2
// gateway/socket unit fakes. Every test dials the actual per-container Unix
// socket that Provision served, over a real h2c Connect AgentGatewayClient, and
// asserts the call flows agent -> socket -> Gateway -> (container->session
// resolve) -> RelayCommsCall -> fake Server -> back. The T2 tests use
// UnimplementedAgentGatewayHandler / blockingGateway and never touch the real
// agentHost-backed Gateway, so this seam is uncovered until here.
//
// Deterministic + event-gated only (channels, short-deadline contexts): no
// sleeps, no retries (rule://no-retries). testTimeout (helpers_test.go) bounds
// every blocking wait so a wedged forward fails fast instead of hanging.

import (
	"context"
	"errors"
	"os"
	"strconv"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"

	compassv1 "github.com/RigelBuild/compass/go/gen/compass/v1"
	compassv1internal "github.com/RigelBuild/compass/go/internal/gen/compass/v1"
	"github.com/RigelBuild/compass/go/internal/gen/compass/v1/compassv1internalconnect"
	"github.com/RigelBuild/compass/go/internal/runnertest"
	"github.com/RigelBuild/compass/go/internal/runtime"
)

// recordingRelay is a fake RunnerService Server standing in for the real
// RelayCommsCall endpoint. It captures every RelayCommsCallRequest it receives
// (under a mutex, so the test goroutine and the h2c handler goroutine race
// safely) and returns a canned result echoing the call id, so the test can
// prove the exact session id + call reached the Server and the result flowed
// back. When started != nil it blocks the forward until release is closed or
// the request context is cancelled — the in-flight state a test event-gates on
// for the force-close scenario, mirroring gateway/socket_test.go's
// blockingGateway. Enroll/PublishEvents/Sessions stay unimplemented (the
// embedded Unimplemented handler) — this seam only exercises RelayCommsCall.
type recordingRelay struct {
	compassv1internalconnect.UnimplementedRunnerServiceHandler

	mu       sync.Mutex
	received []*compassv1internal.RelayCommsCallRequest

	started   chan struct{} // non-nil => block the forward until release/ctx-cancel
	release   chan struct{}
	startOnce sync.Once
}

// FetchSecrets serves the Runner's pre-exec by-container fetch with an empty set
// — these transport tests do not exercise secret delivery, they only need Start
// to get past the materialize step.
func (r *recordingRelay) FetchSecrets(
	context.Context, *connect.Request[compassv1internal.FetchSecretsRequest],
) (*connect.Response[compassv1internal.FetchSecretsResponse], error) {
	return connect.NewResponse(&compassv1internal.FetchSecretsResponse{}), nil
}

// FetchAgentConfig serves the unconfigured-fleet bundle so Provision's config
// materialize succeeds — these transport tests do not exercise config delivery.
func (r *recordingRelay) FetchAgentConfig(
	_ context.Context, _ *connect.Request[compassv1internal.FetchAgentConfigRequest],
	stream *connect.ServerStream[compassv1internal.FetchAgentConfigResponse],
) error {
	return sendEmptyAgentConfig(stream)
}

func (r *recordingRelay) RelayCommsCall(
	ctx context.Context, req *connect.Request[compassv1internal.RelayCommsCallRequest],
) (*connect.Response[compassv1internal.RelayCommsCallResponse], error) {
	r.mu.Lock()
	r.received = append(r.received, req.Msg)
	r.mu.Unlock()

	if r.started != nil {
		r.startOnce.Do(func() { close(r.started) })
		select {
		case <-r.release:
		case <-ctx.Done():
			return nil, connect.NewError(connect.CodeCanceled, ctx.Err())
		}
	}

	call := req.Msg.GetCall()
	return connect.NewResponse(&compassv1internal.RelayCommsCallResponse{
		Result: &compassv1internal.CommsCallResult{
			CallId: call.GetCallId(),
			Result: &compassv1internal.CommsCallResult_Post{
				Post: &compassv1.PostMessageResponse{Message: &compassv1.Message{Id: "msg-" + call.GetCallId()}},
			},
		},
	}), nil
}

// snapshot returns a copy of the requests received so far, taken under the lock.
func (r *recordingRelay) snapshot() []*compassv1internal.RelayCommsCallRequest {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]*compassv1internal.RelayCommsCallRequest(nil), r.received...)
}

// newTransportFixture builds the concrete *agentHost over the stub-streaming
// runtime (a real, terminatable child so Start/Stop drive a live agent) whose
// ServerLink forwards to relay. It returns *agentHost (not the SessionHost
// interface) because the transport tests need .Provision/.Start/.Session/.Close
// plus white-box reads of h.sockets for the served socket path. The per-container
// sockets land under a fresh t.TempDir runtime dir.
func newTransportFixture(t *testing.T, relay compassv1internalconnect.RunnerServiceHandler) *agentHost {
	t.Helper()
	engine := newStubStreamingRuntime(t)
	registry := runtime.NewAgentRegistry()
	rt := runtime.NewAgentRuntimeWithRegistry(engine, registry)
	link := newLink(newRunnerServiceServer(t, relay))
	var n int
	newID := func() string { n++; return "sess-" + strconv.Itoa(n) }
	specs := &fakeSpecBuilder{spec: liveSpec()}
	host := NewSessionHost(link, rt, registry, engine, specs, AgentHostConfig{RuntimeDir: t.TempDir()}, discardLoggerRunner(), newID)
	return host.(*agentHost)
}

// listenerPath reads the host path of the live socket Provision served for a
// container (white-box, under h.mu). This is the exact socket an in-container
// agent would bind-mount and dial.
func listenerPath(t *testing.T, h *agentHost, container string) string {
	t.Helper()
	h.mu.Lock()
	defer h.mu.Unlock()
	l, ok := h.sockets[container]
	if !ok {
		t.Fatalf("no agent socket recorded for container %q", container)
	}
	return l.Path()
}

// TestE2ERoundTripUnderBoundSession — the GREEN happy path. Contract: a call
// arriving on the container's socket while a session is bound reaches the Server
// carrying (a) the exact session id Start minted and (b) the agent's CallId
// verbatim, and the Server's result flows back to the client. Proving the exact
// session id reached RelayCommsCall IS the attribution proof at the Runner seam
// (OQ-2: the Runner forwards the session id it structurally owns and asserts no
// account). Mutation that reddens it: forwarding a wrong/empty session id
// (Gateway reading the wrong container->session mapping), dropping/duplicating
// the forward, or Gateway.Comms not returning the Server's result — each breaks
// one of the four assertions below.
func TestE2ERoundTripUnderBoundSession(t *testing.T) {
	fake := &recordingRelay{}
	h := newTransportFixture(t, fake)
	ctx := context.Background()

	name, err := h.Provision(ctx, &compassv1.ProvisionAgentWorkspaceRequest{AgentHandle: "0123456789abcdef0123456789abcdef"})
	if err != nil {
		t.Fatalf("Provision = %v", err)
	}
	sessionID, err := h.Start(ctx, &compassv1.StartAgentSessionRequest{ContainerName: name}, "")
	if err != nil {
		t.Fatalf("Start = %v", err)
	}
	t.Cleanup(func() { _ = h.Stop(context.Background(), sessionID) })

	client := runnertest.DialAgentSocket(t, listenerPath(t, h, name))
	callCtx, cancel := context.WithTimeout(ctx, testTimeout)
	defer cancel()
	resp, err := client.Comms(callCtx, connect.NewRequest(&compassv1internal.CommsCallRequest{
		CallId: "tc-1",
		Call: &compassv1internal.CommsCallRequest_Post{
			Post: &compassv1.PostMessageRequest{
				Container: &compassv1.PostMessageRequest_ChannelId{ChannelId: "chan-1"},
			},
		},
	}))
	if err != nil {
		t.Fatalf("Comms over the socket = %v, want the round-trip result", err)
	}

	got := fake.snapshot()
	if len(got) != 1 {
		t.Fatalf("Server received %d RelayCommsCall requests, want exactly 1", len(got))
	}
	if got[0].GetSessionId() != sessionID {
		t.Fatalf("relayed session id = %q, want the Start-minted %q", got[0].GetSessionId(), sessionID)
	}
	if id := got[0].GetCall().GetCallId(); id != "tc-1" {
		t.Fatalf("relayed call id = %q, want the agent's %q (verbatim forward)", id, "tc-1")
	}
	if resp.Msg.GetCallId() != "tc-1" {
		t.Fatalf("result call id = %q, want %q (the Server result flowed back)", resp.Msg.GetCallId(), "tc-1")
	}
	if mid := resp.Msg.GetPost().GetMessage().GetId(); mid != "msg-tc-1" {
		t.Fatalf("result payload message id = %q, want the canned %q", mid, "msg-tc-1")
	}
}

// TestE2EFailClosedBeforeStart — the socket is live from Provision, before Start
// binds a session. Contract: a call in that window fails closed
// CodePermissionDenied AND never reaches the Server (no forward with an empty
// session id, no bootstrap-admin-attributed side effect). Mutation that reddens
// it: dropping the ok/empty-session check in Session/Gateway.Comms so it forwards
// with an empty session id — the client would then see a non-PermissionDenied
// outcome and the Server's received slice would be non-empty.
func TestE2EFailClosedBeforeStart(t *testing.T) {
	fake := &recordingRelay{}
	h := newTransportFixture(t, fake)
	ctx := context.Background()

	name, err := h.Provision(ctx, &compassv1.ProvisionAgentWorkspaceRequest{AgentHandle: "0123456789abcdef0123456789abcdef"})
	if err != nil {
		t.Fatalf("Provision = %v", err)
	}
	// Deliberately no Start: the socket is served, but no session is bound.

	client := runnertest.DialAgentSocket(t, listenerPath(t, h, name))
	callCtx, cancel := context.WithTimeout(ctx, testTimeout)
	defer cancel()
	_, err = client.Comms(callCtx, connect.NewRequest(&compassv1internal.CommsCallRequest{CallId: "tc-2"}))
	if err == nil {
		t.Fatal("Comms before Start = nil, want CodePermissionDenied (no session bound)")
	}
	if code := connect.CodeOf(err); code != connect.CodePermissionDenied {
		t.Fatalf("Comms before Start code = %v, want CodePermissionDenied", code)
	}
	if got := fake.snapshot(); len(got) != 0 {
		t.Fatalf("Server received %d relayed calls before Start, want 0 (never forward an empty session id)", len(got))
	}
}

// TestE2EInFlightCallForceClosedAtTeardown — the integrated agentHost.Close path
// drives the socket teardown (T2 proves SocketListener.Close in isolation; here
// the host drives it over a call that is genuinely in-flight through the whole
// stack). Contract: an in-flight forward wedged past the drain grace is
// force-closed so the client sees a Connect error (not a hang, not a success),
// h.Close returns, and the socket file is removed. Deterministic: the in-flight
// state is event-gated on the fake's started channel, and the grace is the
// production drain deadline injected via a short-deadline context — not a
// settle-sleep. Mutation that reddens it: h.Close not calling listener.Close (so
// the call hangs and the socket file survives), or Close not force-closing after
// the drain overruns (so the wedged call never returns).
func TestE2EInFlightCallForceClosedAtTeardown(t *testing.T) {
	fake := &recordingRelay{started: make(chan struct{}), release: make(chan struct{})}
	defer close(fake.release) // release the handler if it survives the force-close
	h := newTransportFixture(t, fake)
	ctx := context.Background()

	name, err := h.Provision(ctx, &compassv1.ProvisionAgentWorkspaceRequest{AgentHandle: "0123456789abcdef0123456789abcdef"})
	if err != nil {
		t.Fatalf("Provision = %v", err)
	}
	sessionID, err := h.Start(ctx, &compassv1.StartAgentSessionRequest{ContainerName: name}, "")
	if err != nil {
		t.Fatalf("Start = %v", err)
	}
	t.Cleanup(func() { _ = h.Stop(context.Background(), sessionID) })

	socketPath := listenerPath(t, h, name)
	client := runnertest.DialAgentSocket(t, socketPath)

	callErr := make(chan error, 1)
	go func() {
		_, err := client.Comms(context.Background(), connect.NewRequest(&compassv1internal.CommsCallRequest{CallId: "tc-3"}))
		callErr <- err
	}()

	// Event-gate: proceed only once the forward is genuinely in-flight at the Server.
	select {
	case <-fake.started:
	case <-timeAfter():
		t.Fatal("relay forward never entered in-flight; cannot exercise the force-close path")
	}

	// The forward is wedged; a normal drain blocks until the grace, so a
	// short-deadline context forces host teardown to overrun and force-close.
	closeCtx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	h.Close(closeCtx)

	select {
	case err := <-callErr:
		if err == nil {
			t.Fatal("host teardown must terminate the in-flight call; client saw a success")
		}
	case <-timeAfter():
		t.Fatal("in-flight call was not terminated by host teardown; it hung")
	}

	if _, err := os.Lstat(socketPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("host teardown must remove the agent socket: Lstat = %v", err)
	}
}

// TestFreshStartSendsReplayCompleteFirst — a fresh (non-resume) Start lifts the
// agent's replay barrier by sending replay_complete as the FIRST control op, so
// the first idle-deliver that starts the agent's turn is dispatched rather than
// refused. Event-gated with no clock: the producer retains ops until acked, so a
// sentinel prompt enqueued via Deliver AFTER Start sits behind the Start-sent
// replay_complete; a subscriber attaching later reads them in order. Contract:
// the first op is replay_complete and ordinary ops follow it. Mutation that
// reddens it: dropping the fresh-start SendControl in host.Start (the first op
// would then be the sentinel, GetReplayComplete() == nil).
func TestFreshStartSendsReplayCompleteFirst(t *testing.T) {
	fake := &recordingRelay{}
	h := newTransportFixture(t, fake)
	ctx := context.Background()

	name, err := h.Provision(ctx, &compassv1.ProvisionAgentWorkspaceRequest{AgentHandle: "0123456789abcdef0123456789abcdef"})
	if err != nil {
		t.Fatalf("Provision = %v", err)
	}
	sessionID, err := h.Start(ctx, &compassv1.StartAgentSessionRequest{ContainerName: name}, "")
	if err != nil {
		t.Fatalf("Start = %v", err)
	}
	t.Cleanup(func() { _ = h.Stop(context.Background(), sessionID) })

	sentinel := &compassv1internal.AgentControl{
		Control: &compassv1internal.AgentControl_Prompt{
			Prompt: &compassv1internal.PromptControl{Input: "after-barrier"},
		},
	}
	if err := h.Deliver(ctx, sessionID, sentinel); err != nil {
		t.Fatalf("Deliver sentinel = %v", err)
	}

	subCtx, cancel := context.WithTimeout(ctx, testTimeout)
	defer cancel()
	stream, err := runnertest.DialAgentSocket(t, listenerPath(t, h, name)).Control(subCtx,
		connect.NewRequest(&compassv1internal.ControlSubscribeRequest{}))
	if err != nil {
		t.Fatalf("Control over the socket = %v, want a bound subscription", err)
	}
	defer func() { _ = stream.Close() }()

	if !stream.Receive() {
		t.Fatalf("no first op reached the agent (stream err %v): fresh Start did not send replay_complete", stream.Err())
	}
	if stream.Msg().GetReplayComplete() == nil {
		t.Fatalf("first op = %v, want replay_complete first on a fresh start", stream.Msg())
	}
	if !stream.Receive() {
		t.Fatalf("no second op reached the agent (stream err %v): sentinel did not follow replay_complete", stream.Err())
	}
	if input := stream.Msg().GetPrompt().GetInput(); input != "after-barrier" {
		t.Fatalf("second op input = %q, want the sentinel %q (ordinary ops follow the barrier lift)", input, "after-barrier")
	}
}

// TestResumeStartSendsReplayCompleteFirst — a resume Start (non-empty
// resume_session_id) lifts the agent's replay barrier by sending replay_complete
// as the FIRST control op, exactly like a fresh start. A file-based resume
// (SEA-1570) loads its transcript synchronously (COMPASS_RESUME_SESSION_FILE)
// before the agent subscribes to the control stream, so replay_complete arriving
// on that stream is the correct "replay done, live ops may flow" signal; without
// it every channel-driven turn on a resumed agent is refused by the closed
// barrier forever. The control-plane restart-replay path (gateway HoldForReplay
// released on ReplayCompleteAck) has no production caller, so the file-based
// resume is the only resume that runs and it needs this lift. Same clockless
// event-gate as the fresh case: a sentinel prompt enqueued via Deliver AFTER
// Start sits behind the Start-sent replay_complete. Contract: the first op is
// replay_complete and the sentinel follows. Mutation that reddens it: restoring
// the resume-session-id guard in host.Start (the first op would then be the
// sentinel, GetReplayComplete() == nil).
func TestResumeStartSendsReplayCompleteFirst(t *testing.T) {
	fake := &recordingRelay{}
	h := newTransportFixture(t, fake)
	ctx := context.Background()

	name, err := h.Provision(ctx, &compassv1.ProvisionAgentWorkspaceRequest{AgentHandle: "0123456789abcdef0123456789abcdef"})
	if err != nil {
		t.Fatalf("Provision = %v", err)
	}
	sessionID, err := h.Start(ctx, &compassv1.StartAgentSessionRequest{ContainerName: name, ResumeSessionId: "resume-1"}, "some transcript body")
	if err != nil {
		t.Fatalf("Start = %v", err)
	}
	t.Cleanup(func() { _ = h.Stop(context.Background(), sessionID) })

	sentinel := &compassv1internal.AgentControl{
		Control: &compassv1internal.AgentControl_Prompt{
			Prompt: &compassv1internal.PromptControl{Input: "after-barrier"},
		},
	}
	if err := h.Deliver(ctx, sessionID, sentinel); err != nil {
		t.Fatalf("Deliver sentinel = %v", err)
	}

	subCtx, cancel := context.WithTimeout(ctx, testTimeout)
	defer cancel()
	stream, err := runnertest.DialAgentSocket(t, listenerPath(t, h, name)).Control(subCtx,
		connect.NewRequest(&compassv1internal.ControlSubscribeRequest{}))
	if err != nil {
		t.Fatalf("Control over the socket = %v, want a bound subscription", err)
	}
	defer func() { _ = stream.Close() }()

	if !stream.Receive() {
		t.Fatalf("no first op reached the agent (stream err %v): resume Start did not send replay_complete", stream.Err())
	}
	if stream.Msg().GetReplayComplete() == nil {
		t.Fatalf("first op = %v, want replay_complete first on a resume start", stream.Msg())
	}
	if !stream.Receive() {
		t.Fatalf("no second op reached the agent (stream err %v): sentinel did not follow replay_complete", stream.Err())
	}
	if input := stream.Msg().GetPrompt().GetInput(); input != "after-barrier" {
		t.Fatalf("second op input = %q, want the sentinel %q (ordinary ops follow the barrier lift)", input, "after-barrier")
	}
}
