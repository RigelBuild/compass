//go:build unix

package runnerhub

// Exercise Enroll, Start, and PublishEvents through the mounted RunnerService handler over h2c.
// The real generated client and fake Runner loop verify command round trips and frame delivery.

import (
	"context"
	"errors"
	"io"
	"runtime"
	"testing"
	"time"

	compassv1 "github.com/RigelBuild/compass/go/gen/compass/v1"
	compassv1internal "github.com/RigelBuild/compass/go/internal/gen/compass/v1"
	"github.com/RigelBuild/compass/go/internal/gen/compass/v1/compassv1internalconnect"
	"github.com/RigelBuild/compass/go/internal/store"
)

func TestSeamEnrollSessionsRoundTripAndPublishEvents(t *testing.T) {
	hub, _, tail := newHub()
	resolver := &fakeResolver{tokens: map[string]resolverEntry{
		"runner-tok": {subj: store.Subject{Kind: store.SubjectRunner, ID: "runner-1"}},
	}}
	url := newMountedH2CServer(t, hub, resolver.resolve)
	client := newRawRunnerClient(t, url, "runner-tok")

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	// --- Enroll: accepted into the registry, first time reattached=false. ------
	enrollResp, err := client.Enroll(ctx, &compassv1internal.EnrollRequest{RunnerId: "runner-1"})
	if err != nil {
		t.Fatalf("Enroll = %v, want success", err)
	}
	if enrollResp.GetReattached() {
		t.Fatal("first Enroll reattached = true, want false")
	}

	// Open the bidi stream and answer a Start command from the Runner-side loop.
	stream, err := client.Sessions(ctx)
	if err != nil {
		t.Fatal(err)
	}
	loopErr := make(chan error, 1)
	go runnerSessionsLoop(stream, loopErr)

	// Wait for the server handler to attach the router; opening the client stream alone is insufficient.
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
	if err := stream.CloseSend(); err != nil {
		t.Fatalf("CloseRequest = %v", err)
	}
	if err := recvLoopErr(t, loopErr); err != nil {
		t.Fatalf("runner sessions loop ended with %v, want clean EOF", err)
	}

	// Bind the relayed session to an agent account through the same
	// Provision->Start promotion path the command handlers drive. Production
	// binds this at Provision->Start; this seam test drives PublishEvents
	// directly, so it binds here.
	bindSession(hub, "sess-wire")

	// --- PublishEvents: one relayed session trace frame must reach Hub.Deliver
	// and terminate at the tail sink (the surviving Deliver arm). This is the
	// event path terminating the wire. ------------------------------------------
	pub, err := client.PublishEvents(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := pub.Send(&compassv1internal.PublishEventsRequest{
		RunnerSeq: 1,
		SessionId: "sess-wire",
		Frame:     sessionTraceFrame("relayed over the wire"),
	}); err != nil {
		t.Fatalf("PublishEvents.Send = %v", err)
	}
	if _, err := pub.CloseAndReceive(); err != nil {
		t.Fatalf("PublishEvents.CloseAndReceive = %v", err)
	}

	// The session trace frame reached the tail sink through Deliver, verbatim.
	calls := tail.snapshot()
	if len(calls) != 1 {
		t.Fatalf("tail sink saw %d frames after PublishEvents, want 1 (the frame must terminate the wire at Deliver)", len(calls))
	}
	if got := calls[0].frame.GetTypedEvent().GetAssistantText().GetText(); got != "relayed over the wire" {
		t.Fatalf("relayed trace text = %q, want the frame body", got)
	}
}

func TestSeamPublishEventsOpenedBeforeEnrollIsFenced(t *testing.T) {
	hub, _, tail := newHub()
	resolver := &fakeResolver{tokens: map[string]resolverEntry{
		"runner-tok": {subj: store.Subject{Kind: store.SubjectRunner, ID: "runner-1"}},
	}}
	url := newMountedH2CServer(t, hub, resolver.resolve)
	client := newRawRunnerClient(t, url, "runner-tok")
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	pub, err := client.PublishEvents(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := pub.Send(&compassv1internal.PublishEventsRequest{RunnerSeq: 1, SessionId: "sess-wire", Frame: sessionTraceFrame("before enroll")}); err != nil {
		t.Fatalf("PublishEvents.Send = %v", err)
	}
	// Gate on the server handler having taken the frame, so the stream opened pre-enroll.
	for deadline := time.Now().Add(10 * time.Second); ; runtime.Gosched() {
		hub.mu.Lock()
		seen := hub.lastSeq == 1
		hub.mu.Unlock()
		if seen {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("pre-enroll frame never reached Deliver")
		}
	}
	if _, err := client.Enroll(ctx, &compassv1internal.EnrollRequest{RunnerId: "runner-1"}); err != nil {
		t.Fatalf("Enroll = %v, want success", err)
	}
	bindSession(hub, "sess-wire")
	if err := pub.Send(&compassv1internal.PublishEventsRequest{RunnerSeq: 2, SessionId: "sess-wire", Frame: sessionTraceFrame("after enroll")}); err != nil {
		t.Fatalf("PublishEvents.Send = %v", err)
	}
	if _, err := pub.CloseAndReceive(); err != nil {
		t.Fatalf("PublishEvents.CloseAndReceive = %v", err)
	}
	if calls := tail.snapshot(); len(calls) != 0 {
		t.Fatalf("tail sink saw %d frames, want none from a stream opened before enroll", len(calls))
	}
}

// startRoundTrip is the outcome of Hub.Start driven over the wire.
type startRoundTrip struct {
	resp *compassv1.StartAgentSessionResponse
	err  error
}

// runnerSessionsLoop bootstraps the bidi stream, answers Start commands, and returns on close.
// The first Send is required to initiate Connect's HTTP request and mount the server handler.
func runnerSessionsLoop(stream compassv1internalconnect.RunnerServiceSessionsClientStream, done chan<- error) {
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
		case *compassv1internal.SessionsResponse_SecretsVersion:
			// Signal-only: the initial-materialize trigger commands.go fires after
			// promoteSession. Like the production dispatcher, it has no result
			// variant — consume it and send nothing back.
			continue
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
