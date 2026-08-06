//go:build unix

package server

// Tests for the compass.v1 service handlers, exercised end to end through a real
// connect-go client over in-process cleartext HTTP/2 — the same door the server
// ships, minus the Unix socket. Each test wires a real event bus so the
// SubscribeEvents snapshot/tail/resync paths run against the actual bus, not a
// stub.

import (
	"context"
	"errors"
	"io"
	"math/rand"
	"testing"

	"connectrpc.com/connect"

	"github.com/sealedsecurity/compass/go/events"
	compassv1 "github.com/sealedsecurity/compass/go/gen/compass/v1"
	"github.com/sealedsecurity/compass/go/gen/compass/v1/compassv1connect"
)

// statusEvent is a positioned ServerStatus{Ready} response the bus carries — the
// shape the server publishes on startup and on every liveness change.
func statusEvent() busPayload {
	return &compassv1.SubscribeEventsResponse{
		Payload: &compassv1.SubscribeEventsResponse_ServerStatus{
			ServerStatus: &compassv1.ServerStatus{State: compassv1.ServerState_SERVER_STATE_READY},
		},
	}
}

// chunkEvent is a positioned AgentMessageChunk carrying `size` incompressible
// bytes of text. Incompressibility matters: connect gzips responses by default,
// so a run of zero bytes would collapse on the wire and never fill the client's
// flow-control window — the lag test needs real bytes to create real
// backpressure. The bytes are a slice of one fixed, high-entropy buffer, so
// every event resists gzip yet the payload is fully deterministic across runs
// (no clock, no entropy source). Reuse is safe: connect gzips each streamed
// message independently, so identical bytes still fill the wire.
func chunkEvent(size int) busPayload {
	if size > len(incompressibleBytes) {
		size = len(incompressibleBytes)
	}
	return &compassv1.SubscribeEventsResponse{
		Payload: &compassv1.SubscribeEventsResponse_AgentMessageChunk{
			AgentMessageChunk: &compassv1.AgentMessageChunk{Text: string(incompressibleBytes[:size])},
		},
	}
}

// incompressibleBytes is a fixed 64 KiB block of random printable ASCII, built
// once from a fixed-seed PRNG: valid UTF-8 (proto string fields are UTF-8
// validated on marshal), high-entropy enough to defeat gzip, and deterministic.
var incompressibleBytes = func() []byte {
	const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/"
	r := rand.New(rand.NewSource(1))
	buf := make([]byte, 64*1024)
	for i := range buf {
		buf[i] = alphabet[r.Intn(len(alphabet))]
	}
	return buf
}()

// subscribe opens a SubscribeEvents stream and registers cleanup. The returned
// context is cancelled on cleanup so the server-side handler unblocks and the
// bus subscriber is released even if the test leaves the stream open.
func subscribe(t *testing.T, client compassv1connect.CompassServiceClient, req *compassv1.SubscribeEventsRequest) *connect.ServerStreamForClient[compassv1.SubscribeEventsResponse] {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	stream, err := client.SubscribeEvents(ctx, connect.NewRequest(req))
	if err != nil {
		t.Fatalf("SubscribeEvents: %v", err)
	}
	t.Cleanup(func() { _ = stream.Close() })
	return stream
}

// recvOne advances the stream by one message with a deadline safety net, so a
// handler that never sends fails the test instead of hanging.
func recvOne(t *testing.T, stream *connect.ServerStreamForClient[compassv1.SubscribeEventsResponse]) *compassv1.SubscribeEventsResponse {
	t.Helper()
	if !recvStreamOrTimeout(t, stream) {
		t.Fatalf("stream.Receive() = false, want a message; err = %v", stream.Err())
	}
	return stream.Msg()
}

func TestGetServerInfoReturnsConfiguredVersionAndApiVersion(t *testing.T) {
	bus := events.NewBus[busPayload]()
	t.Cleanup(bus.Close)
	url := newH2CTestServer(t, newService("9.9.9-test", bus, nil, nil, nil, nil, nil))
	client := newH2CClient(t, url)

	resp, err := client.GetServerInfo(context.Background(), connect.NewRequest(&compassv1.GetServerInfoRequest{}))
	if err != nil {
		t.Fatalf("GetServerInfo: %v", err)
	}
	if got := resp.Msg.GetVersion(); got != "9.9.9-test" {
		t.Fatalf("Version = %q, want %q (the configured build version)", got, "9.9.9-test")
	}
	if got := resp.Msg.GetApiVersion(); got != apiVersion {
		t.Fatalf("ApiVersion = %q, want %q", got, apiVersion)
	}
	if apiVersion != "compass.v1" {
		t.Fatalf("apiVersion const = %q, want compass.v1 (the contract version)", apiVersion)
	}
}

func TestSubscribeEventsSnapshotThenTail(t *testing.T) {
	bus := events.NewBus[busPayload]()
	t.Cleanup(bus.Close)
	// Two events pre-subscribe: they must arrive as the snapshot replay.
	bus.Publish(statusEvent())
	bus.Publish(statusEvent())

	url := newH2CTestServer(t, newService("test", bus, nil, nil, nil, nil, nil))
	client := newH2CClient(t, url)

	stream := subscribe(t, client, &compassv1.SubscribeEventsRequest{SinceSeq: 0})

	// A since_seq==0 subscribe leads with the snapshot-boundary frame (Seq=0, no
	// payload, SnapshotSeq=0) before the replay — consume it first.
	boundary := recvOne(t, stream)
	if boundary.GetSeq() != 0 {
		t.Fatalf("boundary seq = %d, want 0 (a control marker, not a cursor)", boundary.GetSeq())
	}
	if boundary.GetPayload() != nil {
		t.Fatalf("boundary payload = %T, want nil (positional marker)", boundary.GetPayload())
	}

	// Snapshot: seqs 1 and 2, oldest first, each stamped with the bus epoch.
	for wantSeq := uint64(1); wantSeq <= 2; wantSeq++ {
		msg := recvOne(t, stream)
		if msg.GetSeq() != wantSeq {
			t.Fatalf("snapshot msg seq = %d, want %d", msg.GetSeq(), wantSeq)
		}
		if msg.GetInstanceEpoch() != bus.InstanceEpoch() {
			t.Fatalf("snapshot seq %d epoch = %d, want %d", wantSeq, msg.GetInstanceEpoch(), bus.InstanceEpoch())
		}
		if msg.GetServerStatus() == nil {
			t.Fatalf("snapshot seq %d payload = %T, want ServerStatus", wantSeq, msg.GetPayload())
		}
	}

	// Live tail: a post-subscribe publish arrives with the next seq. Publishing
	// only after both snapshot events were received is the event gate — no
	// sleep, no race with the replay drain.
	bus.Publish(statusEvent())
	live := recvOne(t, stream)
	if live.GetSeq() != 3 {
		t.Fatalf("live msg seq = %d, want 3 (the seq after the snapshot's)", live.GetSeq())
	}
	if live.GetServerStatus() == nil {
		t.Fatalf("live payload = %T, want ServerStatus", live.GetPayload())
	}
}

func TestSubscribeEventsUnderflowCursorYieldsSingleTerminalResync(t *testing.T) {
	bus := events.NewBus[busPayload]()
	t.Cleanup(bus.Close)
	bus.Publish(statusEvent())
	bus.Publish(statusEvent())
	bus.Publish(statusEvent())

	url := newH2CTestServer(t, newService("test", bus, nil, nil, nil, nil, nil))
	client := newH2CClient(t, url)

	// A positioned cursor with epoch 0 (an old/other-instance client) can't be
	// served gap-free, so the handler answers with exactly one ResyncRequired
	// and closes the stream.
	stream := subscribe(t, client, &compassv1.SubscribeEventsRequest{SinceSeq: 2, InstanceEpoch: 0})

	msg := recvOne(t, stream)
	if msg.GetResyncRequired() == nil {
		t.Fatalf("first msg payload = %T, want ResyncRequired", msg.GetPayload())
	}
	if msg.GetSeq() != 0 {
		t.Fatalf("resync seq = %d, want 0 (a control signal, not a cursor)", msg.GetSeq())
	}
	if msg.GetInstanceEpoch() != bus.InstanceEpoch() {
		t.Fatalf("resync epoch = %d, want %d (the live instance epoch to echo back)", msg.GetInstanceEpoch(), bus.InstanceEpoch())
	}

	// It is terminal and singular: the stream ends cleanly (EOF, no error) with
	// no further messages.
	assertStreamEndsClean(t, stream)
}

func TestSubscribeEventsLaggingSubscriberGetsTerminalResync(t *testing.T) {
	bus := events.NewBus[busPayload]()
	t.Cleanup(bus.Close)

	url := newH2CTestServer(t, newService("test", bus, nil, nil, nil, nil, nil))
	client := newH2CClient(t, url)

	// Prime one small event before subscribing. connect flushes the stream's
	// response headers on the handler's first Send, and SubscribeEvents (the
	// client RoundTrip) blocks until those headers arrive — with an empty bus the
	// handler would sit in the live loop and never send, deadlocking the test. A
	// single replay event makes the handler Send once, so the client stream
	// opens; the server-side subscriber is then registered and ready to be
	// flooded on its live tail.
	bus.Publish(statusEvent())

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	stream, err := client.SubscribeEvents(ctx, connect.NewRequest(&compassv1.SubscribeEventsRequest{SinceSeq: 0}))
	if err != nil {
		t.Fatalf("SubscribeEvents: %v", err)
	}
	t.Cleanup(func() { _ = stream.Close() })

	// Now flood the live tail while the client reads nothing. The server's Send
	// blocks once the client's HTTP/2 stream window (a fixed ~4 MiB) plus socket
	// buffers fill; the forward loop then stops draining the bus's live buffer,
	// which fills (liveBufferCapacity = 1024) and overruns. The bus latches the
	// subscriber lagged and closes its Live channel, so once the client resumes
	// reading and the buffered events drain, the handler emits a terminal
	// ResyncRequired.
	//
	// Deterministic overrun, no timing: 16 KiB incompressible events cap what the
	// transport can absorb before Send blocks at ~4 MiB / 16 KiB ≈ 256 events
	// plus bounded socket-buffer slack — far under the flood, which is many
	// multiples of that ceiling plus the whole live buffer. The client reads
	// nothing until every publish has returned, so the pile-up is guaranteed.
	const eventBytes = 16 * 1024
	const flood = 8192 // >> transport ceiling (~256) + liveBufferCapacity (1024)
	for range flood {
		bus.Publish(chunkEvent(eventBytes))
	}

	// Drain the stream. Whatever the server managed to buffer comes through;
	// the guaranteed terminal message is a ResyncRequired (Seq=0), after which
	// the stream ends cleanly.
	sawResync := false
	for {
		if !recvStreamOrTimeout(t, stream) {
			break
		}
		msg := stream.Msg()
		if msg.GetResyncRequired() != nil {
			if msg.GetSeq() != 0 {
				t.Fatalf("resync seq = %d, want 0", msg.GetSeq())
			}
			sawResync = true
			// Resync is terminal: nothing follows it.
			assertStreamEndsClean(t, stream)
			break
		}
	}
	if !sawResync {
		t.Fatalf("stream ended without a terminal ResyncRequired; err = %v", stream.Err())
	}
}

// assertStreamEndsClean asserts the next Receive returns false with a clean EOF
// (nil Err) — the stream ended, not errored.
func assertStreamEndsClean(t *testing.T, stream *connect.ServerStreamForClient[compassv1.SubscribeEventsResponse]) {
	t.Helper()
	if recvStreamOrTimeout(t, stream) {
		t.Fatalf("stream delivered another message after the terminal one: %+v", stream.Msg())
	}
	if err := stream.Err(); err != nil && !errors.Is(err, io.EOF) {
		t.Fatalf("stream ended with error %v, want clean EOF", err)
	}
}

// recvStreamOrTimeout runs one Receive with a deadline safety net; returns the
// ok value. It never sleeps — the timeout only guards against a wedged handler.
func recvStreamOrTimeout(t *testing.T, stream *connect.ServerStreamForClient[compassv1.SubscribeEventsResponse]) bool {
	t.Helper()
	ch := make(chan bool, 1)
	go func() { ch <- stream.Receive() }()
	select {
	case ok := <-ch:
		return ok
	case <-timeAfter():
		t.Fatal("timed out waiting on stream.Receive()")
		return false
	}
}

// TestAgentSessionRPCsWithoutRunnerHubAreUnavailable pins the net-new contract
// for the three agent-session lifecycle mutators: on a server built with no
// Runner door (hub nil, the socket-only path), each returns connect
// CodeUnavailable — never CodeUnimplemented (the pre-handler default) and never
// a panic/success. GetAgentStatus is not in this set: it is served from the
// Bridge board projection (not a Runner relay), so it does not depend on the
// hub — its serving path is covered in service_agentstatus_test.go. Driven
// through the real connect client so the actual handler dispatch runs.
func TestAgentSessionRPCsWithoutRunnerHubAreUnavailable(t *testing.T) {
	bus := events.NewBus[busPayload]()
	t.Cleanup(bus.Close)
	svc := newService("test", bus, nil, nil, nil, nil, nil)
	url := newH2CTestServer(t, svc)
	client := newH2CClient(t, url)

	cases := []struct {
		name string
		call func(ctx context.Context, client compassv1connect.CompassServiceClient) error
	}{
		{
			name: "StartAgentSession",
			call: func(ctx context.Context, client compassv1connect.CompassServiceClient) error {
				_, err := client.StartAgentSession(ctx, connect.NewRequest(&compassv1.StartAgentSessionRequest{ContainerName: "c1"}))
				return err
			},
		},
		{
			name: "StopAgentSession",
			call: func(ctx context.Context, client compassv1connect.CompassServiceClient) error {
				_, err := client.StopAgentSession(ctx, connect.NewRequest(&compassv1.StopAgentSessionRequest{SessionId: "s1"}))
				return err
			},
		},
		{
			name: "ReloadAgentSession",
			call: func(ctx context.Context, client compassv1connect.CompassServiceClient) error {
				_, err := client.ReloadAgentSession(ctx, connect.NewRequest(&compassv1.ReloadAgentSessionRequest{SessionId: "s1"}))
				return err
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.call(context.Background(), client)
			if err == nil {
				t.Fatalf("expected error, got nil")
			}
			// CodeUnavailable (not CodeUnimplemented) is the contract: a server
			// with no Runner door pins the RPC as retryable-unavailable, never
			// the old unimplemented-stub code. Asserting Unavailable pins that
			// regression — CodeUnimplemented != CodeUnavailable, so this fails
			// if the handler ever reverts to the embedded Unimplemented default.
			if got := connect.CodeOf(err); got != connect.CodeUnavailable {
				t.Fatalf("want CodeUnavailable (not CodeUnimplemented), got %v (err=%v)", got, err)
			}
		})
	}
}
