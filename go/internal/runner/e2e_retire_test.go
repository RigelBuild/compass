//go:build unix

package runner

// Stop retires a live Control stream. This test exercises Provision -> Serve ->
// Stop with a takeover, proving both the displaced and current subscriptions end.

import (
	"context"
	"errors"
	"io"
	"testing"

	compassv1 "github.com/RigelBuild/compass/go/gen/compass/v1"
	compassv1internal "github.com/RigelBuild/compass/go/internal/gen/compass/v1"
	"github.com/RigelBuild/compass/go/internal/gen/compass/v1/compassv1internalconnect"
	"github.com/RigelBuild/compass/go/internal/runnertest"
)

// controlSub tracks a real Control stream over the container socket. It drains
// in a goroutine because Control blocks until the handler first writes.
type controlSub struct {
	name string
	done chan error
}

// subscribeControl opens a Control stream over the socket and reports its
// terminal error on done. Draining to completion (rather than returning the
// stream) keeps the goroutine's only externally-visible act the single send on
// done, so the test never races a partially-consumed stream.
func subscribeControl(ctx context.Context, client compassv1internalconnect.AgentGatewayClient, name string) *controlSub {
	sub := &controlSub{name: name, done: make(chan error, 1)}
	go func() {
		stream, err := client.Control(ctx, &compassv1internal.ControlSubscribeRequest{})
		if err != nil {
			sub.done <- err
			return
		}
		defer func() { _ = stream.Close() }()
		// Only stream termination matters here; gateway tests cover operation details.
		var streamErr error
		for {
			_, err := stream.Receive()
			if err != nil {
				streamErr = err
				break
			}
		}
		if errors.Is(streamErr, io.EOF) {
			streamErr = nil
		}
		sub.done <- streamErr
	}()
	return sub
}

// TestStopRetiresTheSessionsControlState is the wiring assertion: deleting the
// `listener.RetireSession(sessionID)` call from agentHost.Stop reddens it,
// because the surviving subscription's drainer then stays parked on a wake
// channel nothing will ever close and its stream never ends.
func TestStopRetiresTheSessionsControlState(t *testing.T) {
	h := newTransportFixture(t, &recordingRelay{})
	ctx := context.Background()

	name, err := h.Provision(ctx, &compassv1.ProvisionAgentWorkspaceRequest{}, "acct-1")
	if err != nil {
		t.Fatalf("Provision = %v", err)
	}
	sessionID, err := h.Start(ctx, &compassv1.StartAgentSessionRequest{ContainerName: name}, "", "test-session")
	if err != nil {
		t.Fatalf("Start = %v", err)
	}

	// The subscriptions must outlive the client's own scope only until Stop; a
	// cancellable context guarantees a wedged goroutine is reaped when the test
	// ends rather than leaking into the rest of the package's run.
	subCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	client := runnertest.DialAgentSocket(t, listenerPath(t, h, name))
	first := subscribeControl(subCtx, client, "first")
	second := subscribeControl(subCtx, client, "second")

	// Binding gate. Exactly one of the two is displaced by the other's takeover;
	// its stream ending proves the OTHER is registered on the producer the socket
	// serves. Which one wins is a scheduling detail, so the survivor is whichever
	// is still running — never assumed.
	var survivor *controlSub
	select {
	case err := <-first.done:
		survivor = second
		if err != nil {
			t.Fatalf("displaced subscription %q ended with %v, want a clean takeover", first.name, err)
		}
	case err := <-second.done:
		survivor = first
		if err != nil {
			t.Fatalf("displaced subscription %q ended with %v, want a clean takeover", second.name, err)
		}
	case <-timeAfter():
		t.Fatal("neither Control subscription was displaced by the other: no subscription is provably registered on the served producer, so this test could not observe a retirement either way")
	}

	// The survivor is parked on its wake channel. Only a retirement can close it.
	if err := h.Stop(ctx, sessionID); err != nil {
		t.Fatalf("Stop = %v, want success", err)
	}

	select {
	case err := <-survivor.done:
		// A retirement ends the stream cleanly (the session is gone, the transport
		// is fine); a transport error here would mean the stream died of something
		// other than the retirement and would not prove Stop drove it.
		if err != nil {
			t.Fatalf("surviving subscription %q ended with %v after Stop, want a clean end from the retirement", survivor.name, err)
		}
	case <-timeAfter():
		t.Fatalf("subscription %q is still parked after Stop(%s): Stop did not retire the session's control state, so the socket's producer still pins it — one leaked session per Stop/Start cycle for the life of the Runner", survivor.name, sessionID)
	}
}
