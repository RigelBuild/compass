//go:build unix

package server

// Exercises CompassService handlers over cleartext HTTP/2 with a real event bus,
// including SubscribeEvents snapshot, tail, and resync paths.

import (
	"context"
	"errors"
	"io"
	"math/rand"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"

	"connectrpc.com/connect/v2"
	"connectrpc.com/connect/v2/connecthttp"
	"connectrpc.com/otelconnect"

	"github.com/RigelBuild/compass/go/events"
	compassv1 "github.com/RigelBuild/compass/go/gen/compass/v1"
	"github.com/RigelBuild/compass/go/gen/compass/v1/compassv1connect"
	compassv1internal "github.com/RigelBuild/compass/go/internal/gen/compass/v1"
	"github.com/RigelBuild/compass/go/internal/gen/compass/v1/compassv1internalconnect"
	"github.com/RigelBuild/compass/go/internal/runnerhub"
	"github.com/RigelBuild/compass/go/internal/store"
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

// Builds an incompressible chunk event; compression would not fill the HTTP/2
// flow-control window.
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
func subscribe(t *testing.T, client compassv1connect.CompassServiceClient, req *compassv1.SubscribeEventsRequest) compassv1connect.CompassServiceSubscribeEventsClientStream {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	stream, err := client.SubscribeEvents(ctx, req)
	if err != nil {
		t.Fatalf("SubscribeEvents: %v", err)
	}
	t.Cleanup(func() { _ = stream.Close() })
	return stream
}

// recvOne advances the stream by one message with a deadline safety net, so a
// handler that never sends fails the test instead of hanging.
func recvOne(t *testing.T, stream compassv1connect.CompassServiceSubscribeEventsClientStream) *compassv1.SubscribeEventsResponse {
	t.Helper()
	msg, err := recvStreamOrTimeout(t, stream)
	if err != nil {
		t.Fatalf("stream.Receive() = %v, want a message", err)
	}
	return msg
}

func mountInfoRunner(t *testing.T, hub *runnerhub.Hub) string {
	t.Helper()
	otelIC, err := otelconnect.NewServerInterceptor()
	if err != nil {
		t.Fatalf("otelconnect.NewServerInterceptor: %v", err)
	}
	mux := http.NewServeMux()
	runnerhub.Mount(mux, hub, func(_ context.Context, token string, kind store.SubjectKind) (store.Subject, error) {
		if token != "runner-token" || kind != store.SubjectRunner {
			return store.Subject{}, errors.New("invalid test Runner credential")
		}
		return store.Subject{Kind: store.SubjectRunner, ID: "runner-1"}, nil
	}, nil, nil, otelIC)
	srv := httptest.NewUnstartedServer(mux)
	srv.Config.Protocols = cleartextHTTP2()
	srv.Start()
	t.Cleanup(srv.Close)
	return srv.URL
}

type serverTestBearer struct{ token string }

func (b *serverTestBearer) ClientInterceptor(next connect.ClientFunc) connect.ClientFunc {
	return func(ctx context.Context, spec connect.Spec) (connect.ClientStream, error) {
		info, ok := connect.CallInfoForClientContext(ctx)
		if !ok {
			return nil, errors.New("missing Connect client call info")
		}
		info.RequestHeader().Set("Authorization", "Bearer "+b.token)
		return next(ctx, spec)
	}
}

func TestGetServerInfoReturnsConfiguredVersionAndApiVersion(t *testing.T) {
	bus := events.NewBus[busPayload]()
	t.Cleanup(bus.Close)
	hub := runnerhub.NewHub(nil, nil, nil, nil)
	url := newH2CTestServer(t, newService("9.9.9-test", bus, nil, hub, nil, nil, nil))
	client := newH2CClient(t, url)

	resp, err := client.GetServerInfo(context.Background(), &compassv1.GetServerInfoRequest{})
	if err != nil {
		t.Fatalf("GetServerInfo before enroll: %v", err)
	}
	if got := resp.GetEnrolledRunners(); len(got) != 0 {
		t.Fatalf("EnrolledRunners before enroll = %v, want empty", got)
	}
	runnerURL := mountInfoRunner(t, hub)
	tr := h2cTransport(func(ctx context.Context, network, addr string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, network, addr)
	})
	t.Cleanup(tr.CloseIdleConnections)
	runnerClient := compassv1internalconnect.NewRunnerServiceClient(connect.NewClient(connecthttp.NewTransport(
		&http.Client{Transport: tr},
		runnerURL,
		connecthttp.WithReadMaxBytes(0),
	), (&serverTestBearer{token: "runner-token"}).ClientInterceptor))
	if _, err := runnerClient.Enroll(context.Background(), &compassv1internal.EnrollRequest{RunnerId: "runner-1"}); err != nil {
		t.Fatalf("RunnerService.Enroll: %v", err)
	}
	resp, err = client.GetServerInfo(context.Background(), &compassv1.GetServerInfoRequest{})
	if err != nil {
		t.Fatalf("GetServerInfo after enroll: %v", err)
	}
	if got := resp.GetEnrolledRunners(); len(got) != 1 || got[0].GetId() != "runner-1" || got[0].GetEnrollment() != 1 {
		t.Fatalf("EnrolledRunners after enroll = %v, want runner-1 at enrollment 1", got)
	}
	if got := resp.GetVersion(); got != "9.9.9-test" {
		t.Fatalf("Version = %q, want %q (the configured build version)", got, "9.9.9-test")
	}
	if got := resp.GetApiVersion(); got != apiVersion {
		t.Fatalf("ApiVersion = %q, want %q", got, apiVersion)
	}
}

func TestSubscribeEventsSnapshotThenTail(t *testing.T) {
	bus := events.NewBus[busPayload]()
	t.Cleanup(bus.Close)
	// Two events pre-subscribe: they must arrive as the snapshot replay.
	bus.Publish(t.Context(), statusEvent())
	bus.Publish(t.Context(), statusEvent())

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
	bus.Publish(t.Context(), statusEvent())
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
	bus.Publish(t.Context(), statusEvent())
	bus.Publish(t.Context(), statusEvent())
	bus.Publish(t.Context(), statusEvent())

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

	// Prime replay so headers flush; an empty bus blocks RoundTrip before registration.
	bus.Publish(t.Context(), statusEvent())

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	stream, err := client.SubscribeEvents(ctx, &compassv1.SubscribeEventsRequest{SinceSeq: 0})
	if err != nil {
		t.Fatalf("SubscribeEvents: %v", err)
	}
	t.Cleanup(func() { _ = stream.Close() })

	// Flood without reading until the live buffer overruns, then drain to observe
	// the terminal ResyncRequired.
	const eventBytes = 16 * 1024
	const flood = 8192 // >> transport ceiling (~256) + liveBufferCapacity (1024)
	for range flood {
		bus.Publish(t.Context(), chunkEvent(eventBytes))
	}

	// Drain the stream. Whatever the server managed to buffer comes through;
	// the guaranteed terminal message is a ResyncRequired (Seq=0), after which
	// the stream ends cleanly.
	sawResync := false
	for {
		msg, err := recvStreamOrTimeout(t, stream)
		if err != nil {
			break
		}
		if msg.GetResyncRequired() != nil {
			if msg.GetSeq() != 0 {
				t.Fatalf("resync seq = %d, want 0", msg.GetSeq())
			}
			sawResync = true
			assertStreamEndsClean(t, stream)
			break
		}
	}
	if !sawResync {
		t.Fatal("stream ended without a terminal ResyncRequired")
	}
}

// assertStreamEndsClean asserts the next Receive returns false with a clean EOF
// (nil Err) — the stream ended, not errored.
func assertStreamEndsClean(t *testing.T, stream compassv1connect.CompassServiceSubscribeEventsClientStream) {
	t.Helper()
	msg, err := recvStreamOrTimeout(t, stream)
	if err == nil {
		t.Fatalf("stream delivered another message after the terminal one: %+v", msg)
	}
	if !errors.Is(err, io.EOF) {
		t.Fatalf("stream ended with error %v, want clean EOF", err)
	}
}

func recvStreamOrTimeout(t *testing.T, stream compassv1connect.CompassServiceSubscribeEventsClientStream) (*compassv1.SubscribeEventsResponse, error) {
	t.Helper()
	type receiveResult struct {
		msg *compassv1.SubscribeEventsResponse
		err error
	}
	ch := make(chan receiveResult, 1)
	go func() {
		msg, err := stream.Receive()
		ch <- receiveResult{msg: msg, err: err}
	}()
	select {
	case result := <-ch:
		return result.msg, result.err
	case <-timeAfter():
		t.Fatal("timed out waiting on stream.Receive()")
		return nil, context.DeadlineExceeded
	}
}

// Without a Runner hub, agent-session mutators return CodeUnavailable through
// Connect; GetAgentStatus remains a local projection RPC.
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
				_, err := client.StartAgentSession(ctx, &compassv1.StartAgentSessionRequest{ContainerName: "c1"})
				return err
			},
		},
		{
			name: "StopAgentSession",
			call: func(ctx context.Context, client compassv1connect.CompassServiceClient) error {
				_, err := client.StopAgentSession(ctx, &compassv1.StopAgentSessionRequest{SessionId: "s1"})
				return err
			},
		},
		{
			name: "ReloadAgentSession",
			call: func(ctx context.Context, client compassv1connect.CompassServiceClient) error {
				_, err := client.ReloadAgentSession(ctx, &compassv1.ReloadAgentSessionRequest{SessionId: "s1"})
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
			// CodeUnavailable is the contract for a missing Runner door; it must
			// not fall back to the embedded Unimplemented handler.
			if got := connect.CodeOf(err); got != connect.CodeUnavailable {
				t.Fatalf("want CodeUnavailable (not CodeUnimplemented), got %v (err=%v)", got, err)
			}
		})
	}
}
