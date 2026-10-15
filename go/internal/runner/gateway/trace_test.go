//go:build unix

package gateway

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	"connectrpc.com/otelconnect"

	compassv1internal "github.com/RigelBuild/compass/go/internal/gen/compass/v1"
	"github.com/RigelBuild/compass/go/internal/gen/compass/v1/compassv1internalconnect"
	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
)

func installGatewaySpanRecorder(t *testing.T) *tracetest.SpanRecorder {
	t.Helper()
	recorder := tracetest.NewSpanRecorder()
	provider := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder))
	previous := otel.GetTracerProvider()
	otel.SetTracerProvider(provider)
	t.Cleanup(func() {
		otel.SetTracerProvider(previous)
		if err := provider.Shutdown(context.Background()); err != nil {
			t.Errorf("shutdown tracer provider: %v", err)
		}
	})
	return recorder
}

type spanRelayServer struct {
	compassv1internalconnect.UnimplementedRunnerServiceHandler
}

func (spanRelayServer) RelayCommsCall(
	_ context.Context,
	req *connect.Request[compassv1internal.RelayCommsCallRequest],
) (*connect.Response[compassv1internal.RelayCommsCallResponse], error) {
	return connect.NewResponse(&compassv1internal.RelayCommsCallResponse{
		Result: &compassv1internal.CommsCallResult{CallId: req.Msg.GetCall().GetCallId()},
	}), nil
}

func newSpanRelayClient(t *testing.T) compassv1internalconnect.RunnerServiceClient {
	t.Helper()
	clientInterceptor, err := otelconnect.NewInterceptor()
	if err != nil {
		t.Fatalf("otelconnect.NewInterceptor: %v", err)
	}
	path, handler := compassv1internalconnect.NewRunnerServiceHandler(spanRelayServer{})
	mux := http.NewServeMux()
	mux.Handle(path, handler)
	server := httptest.NewUnstartedServer(mux)
	server.Config.Protocols = cleartextHTTP2()
	server.Start()
	t.Cleanup(server.Close)
	return compassv1internalconnect.NewRunnerServiceClient(
		h2cHTTPClient(t), server.URL, connect.WithInterceptors(clientInterceptor),
	)
}

func closeGatewaySocket(t *testing.T, listener *SocketListener) func() {
	t.Helper()
	closed := false
	closeListener := func() {
		if closed {
			return
		}
		closed = true
		if err := listener.Close(context.Background()); err != nil {
			t.Errorf("close gateway socket: %v", err)
		}
	}
	t.Cleanup(closeListener)
	return closeListener
}

func TestServeCommsEmitsServerAndChildRelaySpans(t *testing.T) {
	recorder := installGatewaySpanRecorder(t)
	listener, err := Serve(t.Context(), socketPath(t), "cont-1", Deps{
		Sessions: staticSessions{sessionID: "sess-1", ok: true},
		Relay:    newSpanRelayClient(t),
	})
	if err != nil {
		t.Fatalf("Serve: %v", err)
	}
	closeListener := closeGatewaySocket(t, listener)

	client := agentClient(t, listener.Path())
	resp, err := client.Comms(t.Context(), connect.NewRequest(&compassv1internal.CommsCallRequest{CallId: "span-call"}))
	if err != nil {
		t.Fatalf("Comms: %v", err)
	}
	if got := resp.Msg.GetCallId(); got != "span-call" {
		t.Fatalf("Comms call id = %q, want span-call", got)
	}
	closeListener()

	var serverSpan, clientSpan sdktrace.ReadOnlySpan
	for _, span := range recorder.Ended() {
		switch span.SpanKind() {
		case trace.SpanKindServer:
			if serverSpan != nil {
				t.Fatalf("recorded multiple server spans: %d", len(recorder.Ended()))
			}
			serverSpan = span
		case trace.SpanKindClient:
			if clientSpan != nil {
				t.Fatalf("recorded multiple client spans: %d", len(recorder.Ended()))
			}
			clientSpan = span
		default:
			continue
		}
	}
	if serverSpan == nil {
		t.Fatalf("recorded spans = %d, want one AgentGateway server span and its RelayCommsCall client child", len(recorder.Ended()))
	}
	if clientSpan == nil {
		t.Fatalf("recorded spans = %d, want RelayCommsCall client span", len(recorder.Ended()))
	}
	if got, want := clientSpan.Parent().SpanID(), serverSpan.SpanContext().SpanID(); got != want {
		t.Errorf("RelayCommsCall client parent span id = %s, want AgentGateway server span id %s", got, want)
	}
	if got, want := clientSpan.SpanContext().TraceID(), serverSpan.SpanContext().TraceID(); got != want {
		t.Errorf("RelayCommsCall client trace id = %s, want AgentGateway server trace id %s", got, want)
	}
}

func TestServeControlEmitsServerSpanAfterStreamCloses(t *testing.T) {
	recorder := installGatewaySpanRecorder(t)
	listener, err := Serve(t.Context(), socketPath(t), "cont-1", Deps{
		Sessions: staticSessions{sessionID: "sess-1", ok: true},
	})
	if err != nil {
		t.Fatalf("Serve: %v", err)
	}
	closeListener := closeGatewaySocket(t, listener)
	listener.BindSession("sess-1")

	ctx, cancel := context.WithCancel(t.Context())
	client := agentClient(t, listener.Path())
	completed := make(chan error, 1)
	go func() {
		stream, err := client.Control(ctx, connect.NewRequest(&compassv1internal.ControlSubscribeRequest{}))
		if err == nil {
			for stream.Receive() {
			}
			err = stream.Err()
		}
		completed <- err
	}()
	eventually(t, "Control subscription to bind", func() bool {
		return listener.control.subscriptionGeneration("sess-1") == 1
	})
	cancel()
	select {
	case err := <-completed:
		if err == nil {
			t.Fatal("Control client completed after cancellation with no error")
		}
	case <-time.After(testTimeout):
		t.Fatal("Control client did not finish after cancellation")
	}
	closeListener()

	var serverSpans int
	for _, span := range recorder.Ended() {
		if span.SpanKind() == trace.SpanKindServer {
			serverSpans++
		}
	}
	if serverSpans != 1 {
		t.Fatalf("recorded server spans = %d, want exactly one after Control closes (total spans %d)", serverSpans, len(recorder.Ended()))
	}
}

// Publish streams live for the agent's life, so per-message span events would grow without bound.
func TestServePublishSpanHasNoMessageEvents(t *testing.T) {
	recorder := installGatewaySpanRecorder(t)
	capture := newCapturePublish()
	events := newRunnerServiceServer(t, capture)
	g := NewGateway(context.Background(), "cont-1", Deps{Sessions: boundSessions(), Events: events})
	client := newAgentGatewayServer(t, g)

	stream := client.Publish(context.Background())
	for _, text := range []string{"alpha", "bravo", "charlie"} {
		if err := stream.Send(&compassv1internal.PublishFrameRequest{Frame: traceFrame(text)}); err != nil {
			t.Fatalf("send %q: %v", text, err)
		}
	}
	if _, err := stream.CloseAndReceive(); err != nil {
		t.Fatalf("close publish stream: %v", err)
	}

	var publishSpan sdktrace.ReadOnlySpan
	for _, span := range recorder.Ended() {
		if span.SpanKind() == trace.SpanKindServer && strings.HasSuffix(span.Name(), "AgentGateway/Publish") {
			if publishSpan != nil {
				t.Fatalf("recorded multiple AgentGateway Publish server spans")
			}
			publishSpan = span
		}
	}
	if publishSpan == nil {
		t.Fatalf("no ended AgentGateway Publish server span recorded (total spans %d)", len(recorder.Ended()))
	}
	if got := len(publishSpan.Events()); got != 0 {
		t.Errorf("AgentGateway Publish server span events = %d, want 0", got)
	}
}
