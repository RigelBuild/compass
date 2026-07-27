//go:build unix

package runnerhub

// THE anti-orphan live-seam test (compass ruling, load-bearing). It stands up
// the RunnerService handler on an httptest h2c server, dials it with the REAL
// generated RunnerServiceClient over a fake resolver that accepts a SubjectRunner
// token, and exercises the whole seam over the wire:
//   - Enroll → accepted into the registry (Reattached=false first time).
//   - Sessions → the Server pushes one command (Hub.Start from a goroutine), a
//     minimal Runner-side loop returns the correlated result, and Start returns
//     it — proving the command round-trips the bidi stream, dial-out inversion
//     and all.
//   - PublishEvents → one relayed frame reaches Hub.Deliver (the fake sinks see
//     it) — proving the event path is mounted-and-terminating-the-wire, not
//     merely a struct call.
//
// "Mounted-and-terminating-the-wire": every operation goes through the real
// connect handler + interceptors, so a regression that broke the mount, the
// stream inversion, or the Deliver wiring reddens this and only this test.

import (
	"context"
	"errors"
	"io"
	"testing"

	"connectrpc.com/connect"

	compassv1 "github.com/sealedsecurity/compass/go/gen/compass/v1"
	compassv1internal "github.com/sealedsecurity/compass/go/internal/gen/compass/v1"
	"github.com/sealedsecurity/compass/go/internal/store"
)

func TestSeamEnrollSessionsRoundTripAndPublishEvents(t *testing.T) {
	hub, conv, _, tail := newHub()
	resolver := &fakeResolver{tokens: map[string]resolverEntry{
		"runner-tok": {subj: store.Subject{Kind: store.SubjectRunner, ID: "runner-1"}},
	}}
	url := newMountedH2CServer(t, hub, resolver.resolve)
	client := newRawRunnerClient(t, url, "runner-tok")

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	// --- Enroll: accepted into the registry, first time reattached=false. ------
	enrollResp, err := client.Enroll(ctx, connect.NewRequest(&compassv1internal.EnrollRequest{RunnerId: "runner-1"}))
	if err != nil {
		t.Fatalf("Enroll = %v, want success", err)
	}
	if enrollResp.Msg.GetReattached() {
		t.Fatal("first Enroll reattached = true, want false")
	}

	// --- Sessions: open the bidi stream and run a minimal Runner-side dispatch
	// loop that answers one Start command with a fixed session id. ---------------
	stream := client.Sessions(ctx)
	loopErr := make(chan error, 1)
	go runnerSessionsLoop(stream, loopErr)

	// Event-gate on the server-side Sessions handler having bound the command
	// router to this live stream. Opening the stream client-side is not enough:
	// the handler's router.attach runs only once the stream reaches the server,
	// so dispatching before it is bound would (correctly) find no live stream.
	// This gates on that observed attach, deterministically.
	waitRouterAttached(t, hub)

	// The Server pushes a Start command via Hub.Start from a goroutine; it blocks
	// until the Runner returns the correlated result on the request half.
	startOut := make(chan startRoundTrip, 1)
	go func() {
		resp, err := hub.Start(ctx, "req-seam", &compassv1.StartAgentSessionRequest{ContainerName: "c1"})
		startOut <- startRoundTrip{resp: resp, err: err}
	}()

	rt := recvStartRoundTrip(t, startOut)
	if rt.err != nil {
		t.Fatalf("Hub.Start over the wire = %v, want the round-tripped result", rt.err)
	}
	if got := rt.resp.GetSessionId(); got != "sess-wire" {
		t.Fatalf("round-tripped session id = %q, want sess-wire (the Runner's answer)", got)
	}

	// Close the Sessions stream and confirm the loop ended cleanly (the request
	// half saw EOF), so the detach/disconnect path is well-formed.
	if err := stream.CloseRequest(); err != nil {
		t.Fatalf("CloseRequest = %v", err)
	}
	if err := recvLoopErr(t, loopErr); err != nil {
		t.Fatalf("runner sessions loop ended with %v, want clean EOF", err)
	}

	// --- PublishEvents: one relayed frame must reach Hub.Deliver (a fake sink
	// observes it). This is the event path terminating the wire. -----------------
	pub := client.PublishEvents(ctx)
	if err := pub.Send(&compassv1internal.PublishEventsRequest{
		RunnerSeq: 1,
		SessionId: "sess-wire",
		Frame:     convPostedFrame("relayed over the wire"),
	}); err != nil {
		t.Fatalf("PublishEvents.Send = %v", err)
	}
	if err := pub.Send(&compassv1internal.PublishEventsRequest{
		RunnerSeq: 2,
		SessionId: "sess-wire",
		Frame:     sessionStateFrame(compassv1.AgentSessionState_AGENT_SESSION_STATE_READY),
	}); err != nil {
		t.Fatalf("PublishEvents.Send(session) = %v", err)
	}
	if _, err := pub.CloseAndReceive(); err != nil {
		t.Fatalf("PublishEvents.CloseAndReceive = %v", err)
	}

	// The conversation frame reached the comms sink through Deliver, verbatim.
	calls := conv.snapshot()
	if len(calls) != 1 {
		t.Fatalf("conversation sink saw %d frames after PublishEvents, want 1 (the frame must terminate the wire at Deliver)", len(calls))
	}
	if got := firstTextBlock(calls[0].posted.GetMessage()); got != "relayed over the wire" {
		t.Fatalf("relayed message text = %q, want the frame body", got)
	}
	// The session frame reached the tail sink through Deliver.
	if got := len(tail.snapshot()); got != 1 {
		t.Fatalf("tail sink saw %d frames, want 1 (the session frame must reach Deliver)", got)
	}
}

// startRoundTrip is the outcome of Hub.Start driven over the wire.
type startRoundTrip struct {
	resp *compassv1.StartAgentSessionResponse
	err  error
}

// runnerSessionsLoop is a minimal Runner-side dispatch loop: it reads each
// command the Server pushes on the response half and answers a Start with a
// fixed session-id result on the request half. It ends when the stream closes
// (clean EOF → nil).
//
// It opens the stream with one bootstrap Send BEFORE the Receive loop, because
// connect-go's CallBidiStream does not initiate the HTTP request (and so the
// server's Sessions handler + router.attach never run) until the client's first
// Send (connect client.go: "request headers are not sent automatically ...
// require an explicit call to Send"). The empty result carries no request id, so
// the server's router.complete treats it as an unknown-id no-op — it exists only
// to open the stream. runner.RunSessions opens its real Sessions stream with the
// same bootstrap Send (dispatch.go), so this loop mirrors the production client's
// stream-open, not a test shortcut.
func runnerSessionsLoop(stream *connect.BidiStreamForClient[compassv1internal.SessionsRequest, compassv1internal.SessionsResponse], done chan<- error) {
	if err := stream.Send(&compassv1internal.SessionsRequest{}); err != nil {
		done <- err
		return
	}
	for {
		cmd, err := stream.Receive()
		if err != nil {
			if errors.Is(err, io.EOF) {
				done <- nil
				return
			}
			done <- err
			return
		}
		var result *compassv1internal.SessionsRequest
		switch cmd.GetCommand().(type) {
		case *compassv1internal.SessionsResponse_Start:
			result = &compassv1internal.SessionsRequest{
				RequestId: cmd.GetRequestId(),
				Result:    &compassv1internal.SessionsRequest_Start{Start: &compassv1.StartAgentSessionResponse{SessionId: "sess-wire"}},
			}
		default:
			result = &compassv1internal.SessionsRequest{
				RequestId: cmd.GetRequestId(),
				Result: &compassv1internal.SessionsRequest_Error{Error: &compassv1internal.RunnerError{
					Code:    compassv1internal.RunnerErrorCode_RUNNER_ERROR_CODE_INTERNAL,
					Message: "unexpected command",
				}},
			}
		}
		if err := stream.Send(result); err != nil {
			done <- err
			return
		}
	}
}

func recvStartRoundTrip(t *testing.T, ch <-chan startRoundTrip) startRoundTrip {
	t.Helper()
	select {
	case rt := <-ch:
		return rt
	case <-timeAfter():
		t.Fatal("timed out waiting for Hub.Start to round-trip the Sessions stream")
		return startRoundTrip{}
	}
}

func recvLoopErr(t *testing.T, ch <-chan error) error {
	t.Helper()
	select {
	case err := <-ch:
		return err
	case <-timeAfter():
		t.Fatal("timed out waiting for the runner sessions loop to end")
		return nil
	}
}
