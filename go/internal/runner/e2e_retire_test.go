//go:build unix

package runner

// agentHost.Stop must retire the stopped session's control state on the
// container's socket. The socket outlives the session (Stop/Start reuses the
// container and its listener) and the Runner mints a fresh session id per cycle,
// so nothing but this teardown call would ever release the state — without it
// the served producer accumulates one session's worth per cycle for the life of
// the Runner process.
//
// WHERE THE HALVES LIVE. The leak itself — the producer's session map going
// 1 -> 0 — is asserted in gateway/retire_wiring_test.go, because `control` and
// `sessions` are unexported there and a merely-leaked map entry has no exported
// surface. This file pins the other half: that agentHost.Stop is what DRIVES it,
// through the real Provision -> Serve -> Stop path over a real socket.
//
// What it observes, and why that is not the trivially-true thing. Retiring a
// session with a live subscription advances its generation and closes its wake
// channel, so the drainer unparks, sees it has been displaced, and returns —
// ending the agent's Control server-stream. Stop's other action, s.stream.Stop(),
// terminates the container's agent CHILD; it does not touch the Unix socket, the
// HTTP server, or the producer, so it cannot end a Control stream. Retirement is
// the only thing in Stop that can, which is what makes the assertion load-bearing
// rather than incidental.
//
// Deterministic and sleep-free. A subscription cannot be observed binding from
// outside the gateway package (Control blocks the dialing client until the
// handler first writes), so binding is gated on a TAKEOVER: two subscriptions
// race, whichever binds second displaces the first, and the first's stream ending
// is proof the second is registered on the served producer. Whichever survives is
// then the one Stop must retire. testTimeout bounds every wait as a fail-fast net,
// never as synchronization.

import (
	"context"
	"testing"

	compassv1 "github.com/RigelBuild/compass/go/gen/compass/v1"
	compassv1internal "github.com/RigelBuild/compass/go/internal/gen/compass/v1"
	"github.com/RigelBuild/compass/go/internal/gen/compass/v1/compassv1internalconnect"
	"github.com/RigelBuild/compass/go/internal/runnertest"

	"connectrpc.com/connect"
)

// controlSub is one agent Control subscription dialed over a container's real
// socket. done carries the stream's terminal error exactly once, when the
// server-stream ends — the observable shadow of the subscription being retired
// or displaced. The whole call runs in a goroutine because Control blocks the
// client until the handler first writes, so an unbound subscription would
// otherwise wedge the test rather than fail it.
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
		stream, err := client.Control(ctx, connect.NewRequest(&compassv1internal.ControlSubscribeRequest{}))
		if err != nil {
			sub.done <- err
			return
		}
		defer func() { _ = stream.Close() }()
		// Drain to termination: the ops themselves are the gateway package's
		// contract, so only the stream's END is this test's business.
		for stream.Receive() {
		}
		sub.done <- stream.Err()
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

	name, err := h.Provision(ctx, &compassv1.ProvisionAgentWorkspaceRequest{AgentAccountId: "acct-1"})
	if err != nil {
		t.Fatalf("Provision = %v", err)
	}
	sessionID, err := h.Start(ctx, &compassv1.StartAgentSessionRequest{ContainerName: name}, "")
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
