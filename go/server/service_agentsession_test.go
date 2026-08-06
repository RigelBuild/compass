//go:build unix

package server

// Handler-level contracts for SubscribeAgentSession, the
// net-new server-stream RPC (service.go:279). Two of its five branches need no
// store — they fail before the authz gate — so they run in the default lane
// here, driven end to end through a real connect client over in-process h2c (the
// shipped door's protocol). The store-gated branches (authorized delivery,
// not-found/forbidden parity, and the client-hangup slot release, which all
// require passing RequireAgentSessionSubscriber against a real Postgres) live in
// service_agentsession_pgtest_test.go behind the pgtest tag.

import (
	"context"
	"testing"

	"connectrpc.com/connect"

	"github.com/sealedsecurity/compass/go/events"
	compassv1 "github.com/sealedsecurity/compass/go/gen/compass/v1"
	"github.com/sealedsecurity/compass/go/gen/compass/v1/compassv1connect"
)

// recvAgentFrameOrTimeout runs one Receive on a SubscribeAgentSession client
// stream with a deadline safety net; returns the ok value. Mirrors
// recvStreamOrTimeout (service_test.go) for the AgentSessionFrame element type —
// connect's ServerStreamForClient is generic over the message, so the two client
// streams need separately-typed receive helpers. It never sleeps; the timeout
// only guards against a wedged handler.
func recvAgentFrameOrTimeout(t *testing.T, stream *connect.ServerStreamForClient[compassv1.AgentSessionFrame]) bool {
	t.Helper()
	ch := make(chan bool, 1)
	go func() { ch <- stream.Receive() }()
	select {
	case ok := <-ch:
		return ok
	case <-timeAfter():
		t.Fatal("timed out waiting on SubscribeAgentSession stream.Receive()")
		return false
	}
}

// subscribeAgentSessionCode opens a SubscribeAgentSession stream carrying no
// bearer and returns the connect code the door answers with on the first
// Receive. A handler that rejects before streaming surfaces its terminal error
// there (no frame ever arrives); recvAgentFrameOrTimeout is the deadline safety
// net, not a sleep.
func subscribeAgentSessionCode(t *testing.T, client compassv1connect.CompassServiceClient, sessionID string) connect.Code {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()
	stream, err := client.SubscribeAgentSession(ctx, connect.NewRequest(&compassv1.SubscribeAgentSessionRequest{SessionId: sessionID}))
	if err != nil {
		return connect.CodeOf(err)
	}
	defer func() { _ = stream.Close() }()
	if recvAgentFrameOrTimeout(t, stream) {
		t.Fatalf("a rejected SubscribeAgentSession stream delivered a frame: %+v", stream.Msg())
	}
	return connect.CodeOf(stream.Err())
}

// TestSubscribeAgentSessionWithoutTailIsUnavailable pins branch 1
// (service.go:284): a server built with no Runner door has a nil session tail,
// so SubscribeAgentSession is CodeUnavailable — the same retryable-unavailable
// contract as the lifecycle mutators (TestAgentSessionRPCsWithoutRunnerHubAreUnavailable),
// never CodeUnimplemented and never a panic. The tail is checked before the
// caller, so this holds even with no bearer on the request. A regression that
// dropped the nil-tail guard would dereference nil and crash, reddening this.
func TestSubscribeAgentSessionWithoutTailIsUnavailable(t *testing.T) {
	bus := events.NewBus[busPayload]()
	t.Cleanup(bus.Close)
	// nil tail (6th arg): the socket-only path with no Runner-backed fan-out.
	svc := newService("test", bus, nil, nil, nil, nil, nil)
	client := newH2CClient(t, newH2CTestServer(t, svc))

	if code := subscribeAgentSessionCode(t, client, "sess-1"); code != connect.CodeUnavailable {
		t.Fatalf("SubscribeAgentSession with nil tail = %v, want CodeUnavailable", code)
	}
}

// TestSubscribeAgentSessionWithoutCallerIsUnauthenticated pins branch 2
// (service.go:287-292): a tail is present, but the default h2c door mounts no
// bearer interceptor, so no caller is attached and CallerFrom returns !ok. The
// handler fails closed with CodeUnauthenticated rather than streaming an
// unauthorized session — the door-wiring-bug guard. The store is never reached
// (nil store is safe here) because the caller check precedes the authz call. A
// regression that treated a missing caller as authorized would fall through to
// the nil store and panic, or stream unauthorized — either reddens this.
func TestSubscribeAgentSessionWithoutCallerIsUnauthenticated(t *testing.T) {
	bus := events.NewBus[busPayload]()
	t.Cleanup(bus.Close)
	// Tail present so branch 1 passes; nil store is never dereferenced because
	// the missing-caller check returns first.
	svc := newService("test", bus, nil, nil, nil, nil, newSessionTail())
	client := newH2CClient(t, newH2CTestServer(t, svc))

	if code := subscribeAgentSessionCode(t, client, "sess-1"); code != connect.CodeUnauthenticated {
		t.Fatalf("SubscribeAgentSession with no caller = %v, want CodeUnauthenticated", code)
	}
}
