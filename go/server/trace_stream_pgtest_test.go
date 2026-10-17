//go:build pgtest && unix

package server

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect/v2"
	"connectrpc.com/connect/v2/connecthttp"
	"connectrpc.com/otelconnect"
	"github.com/jackc/pgx/v5"
	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"

	"github.com/RigelBuild/compass/go/events"
	compassv1 "github.com/RigelBuild/compass/go/gen/compass/v1"
	"github.com/RigelBuild/compass/go/gen/compass/v1/compassv1connect"
	"github.com/RigelBuild/compass/go/internal/auth"
	"github.com/RigelBuild/compass/go/internal/comms"
	otelx "github.com/RigelBuild/compass/go/internal/otel"
	"github.com/RigelBuild/compass/go/internal/pgtest"
	"github.com/RigelBuild/compass/go/internal/store"
)

type streamQueryTracer struct {
	mu       sync.Mutex
	contexts map[string][]trace.SpanContext
}

func (t *streamQueryTracer) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	if name, ok := streamQueryName(data.SQL); ok {
		t.record(name, ctx)
	}
	return ctx
}

func (t *streamQueryTracer) TraceBatchStart(ctx context.Context, _ *pgx.Conn, _ pgx.TraceBatchStartData) context.Context {
	return ctx
}

func (t *streamQueryTracer) TraceBatchQuery(ctx context.Context, _ *pgx.Conn, data pgx.TraceBatchQueryData) {
	if name, ok := streamQueryName(data.SQL); ok {
		t.record(name, ctx)
	}
}

func (t *streamQueryTracer) TraceBatchEnd(context.Context, *pgx.Conn, pgx.TraceBatchEndData) {}

func streamQueryName(sql string) (string, bool) {
	rest, ok := strings.CutPrefix(sql, "-- name: ")
	if !ok {
		return "", false
	}
	name, _, _ := strings.Cut(rest, " ")
	return name, name == "RequireAgentSessionSubscriber" || name == "MessagesHeadSeq"
}

func (*streamQueryTracer) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

func (t *streamQueryTracer) record(name string, ctx context.Context) {
	t.mu.Lock()
	t.contexts[name] = append(t.contexts[name], trace.SpanContextFromContext(ctx))
	t.mu.Unlock()
}

func (t *streamQueryTracer) reset() {
	t.mu.Lock()
	t.contexts = make(map[string][]trace.SpanContext)
	t.mu.Unlock()
}

func (t *streamQueryTracer) spanContexts(name string) []trace.SpanContext {
	t.mu.Lock()
	defer t.mu.Unlock()
	return append([]trace.SpanContext(nil), t.contexts[name]...)
}

func installStreamSpanRecorder(t *testing.T) (*tracetest.SpanRecorder, connect.ServerInterceptor) {
	t.Helper()
	previous := otel.GetTracerProvider()
	recorder := tracetest.NewSpanRecorder()
	provider := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder))
	otel.SetTracerProvider(provider)
	t.Cleanup(func() {
		otel.SetTracerProvider(previous)
		if err := provider.Shutdown(context.Background()); err != nil {
			t.Errorf("shutdown tracer provider: %v", err)
		}
	})
	ic, err := otelconnect.NewServerInterceptor()
	if err != nil {
		t.Fatalf("otelconnect.NewServerInterceptor: %v", err)
	}
	return recorder, ic
}

func tracedStreamServer(t *testing.T, handler http.Handler) *httptest.Server {
	t.Helper()
	srv := httptest.NewUnstartedServer(handler)
	srv.Config.Protocols = cleartextHTTP2()
	srv.Start()
	t.Cleanup(srv.Close)
	return srv
}

func openTraceStreamStore(t *testing.T, tracer *streamQueryTracer) (*store.Store, store.Account, store.Account) {
	t.Helper()
	st, err := store.OpenTraced(t.Context(), pgtest.RequireDSN(t), tracer)
	if err != nil {
		t.Fatalf("store OpenTraced: %v", err)
	}
	t.Cleanup(st.Close)
	admin, err := st.BootstrapAdmin(t.Context(), store.NewUser{Handle: "admin", DisplayName: "admin"})
	if err != nil {
		t.Fatalf("BootstrapAdmin: %v", err)
	}
	owner, err := st.CreateUser(t.Context(), store.NewUser{Handle: "owner", DisplayName: "owner"})
	if err != nil {
		t.Fatalf("CreateUser(owner): %v", err)
	}
	return st, admin, owner
}

func TestSubscribeAgentSessionStoreQueryHasNoSpan(t *testing.T) {
	queries := &streamQueryTracer{contexts: make(map[string][]trace.SpanContext)}
	st, admin, owner := openTraceStreamStore(t, queries)
	agent, err := st.CreateAgent(t.Context(), owner.ID, store.NewAgent{Handle: "agent", DisplayName: "agent"})
	if err != nil {
		t.Fatalf("CreateAgent: %v", err)
	}
	const sessionID = "trace-stream-session"
	if err := st.RecordAgentSession(t.Context(), sessionID, agent.ID); err != nil {
		t.Fatalf("RecordAgentSession: %v", err)
	}
	ownerToken, err := auth.IssueAccountToken(t.Context(), st, owner.ID)
	if err != nil {
		t.Fatalf("IssueAccountToken(owner): %v", err)
	}

	recorder, ic := installStreamSpanRecorder(t)
	bus := events.NewBus[busPayload]()
	t.Cleanup(bus.Close)
	tail := newSessionTail()
	svc := newService("trace-stream-test", bus, st, nil, nil, nil, tail)
	serverService := connect.NewServer(otelx.HandlerInterceptors(ic,
		auth.BearerInterceptor(st), auth.NewAdminGate(admin.ID)))
	compassv1connect.RegisterCompassServiceHandler(serverService, svc)
	mux := http.NewServeMux()
	connecthttp.Mount(mux, serverService)
	server := tracedStreamServer(t, mux)
	client := compassv1connect.NewCompassServiceClient(connect.NewClient(connecthttp.NewTransport(newH2CStreamClient(t), server.URL, connecthttp.WithReadMaxBytes(0))))
	ctx, cancel := context.WithCancel(t.Context())
	req := &compassv1.SubscribeAgentSessionRequest{SessionId: sessionID}
	ctx, info := connect.NewClientContext(ctx)
	info.RequestHeader().Set("Authorization", "Bearer "+ownerToken)
	stream, err := client.SubscribeAgentSession(ctx, req)
	if err != nil {
		cancel()
		t.Fatalf("SubscribeAgentSession: %v", err)
	}
	frame, receiveErr := receiveAgentFrameOrTimeout(t, stream)
	if receiveErr != nil || frame.GetSessionId() != sessionID {
		cancel()
		t.Fatalf("registration acknowledgement = %+v, err=%v", frame, receiveErr)
	}
	cancel()
	if extra, receiveErr := receiveAgentFrameOrTimeout(t, stream); receiveErr == nil {
		t.Fatalf("SubscribeAgentSession delivered unexpected post-cancel frame: %+v", extra)
	}
	_ = stream.Close()
	waitTailSubscribers(t, tail, sessionID, 0)

	assertStreamQueryHasNoSpan(t, queries, "RequireAgentSessionSubscriber")
	waitForStreamSpan(t, recorder)
	assertStreamSpanRecorded(t, recorder, "RequireAgentSessionSubscriber")
}

func assertStreamSpanRecorded(t *testing.T, recorder *tracetest.SpanRecorder, queryName string) {
	t.Helper()
	spans := recorder.Ended()
	var serverSpans []sdktrace.ReadOnlySpan
	for _, span := range spans {
		if span.SpanKind() == trace.SpanKindServer {
			serverSpans = append(serverSpans, span)
		}
	}
	if len(serverSpans) != 1 {
		t.Fatalf("ended server spans = %+v, want one", serverSpans)
	}
	serverContext := serverSpans[0].SpanContext()
	for _, span := range spans {
		parent := span.Parent()
		if span.SpanKind() == trace.SpanKindClient && span.Name() == queryName &&
			parent.TraceID() == serverContext.TraceID() && parent.SpanID() == serverContext.SpanID() {
			t.Errorf("ended client span %q is a child of the stream server span, want no span for that query", queryName)
		}
	}
}

func waitForStreamSpan(t *testing.T, recorder *tracetest.SpanRecorder) {
	t.Helper()
	deadline := timeAfter()
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	for {
		for _, span := range recorder.Ended() {
			if span.SpanKind() == trace.SpanKindServer {
				return
			}
		}
		select {
		case <-deadline:
			t.Fatal("server span did not end after client canceled stream")
		case <-ticker.C:
		}
	}
}

func TestSubscribeCommsHeadQueryHasNoSpan(t *testing.T) {
	queries := &streamQueryTracer{contexts: make(map[string][]trace.SpanContext)}
	st, admin, _ := openTraceStreamStore(t, queries)
	recorder, ic := installStreamSpanRecorder(t)
	bus := events.NewBus[*compassv1.SubscribeCommsResponse]()
	t.Cleanup(bus.Close)
	service := comms.NewComms(t.Context(), st, bus, nil, admin.ID)
	serverService := connect.NewServer(otelx.HandlerInterceptors(ic, auth.AmbientIdentity(admin.ID)))
	compassv1connect.RegisterCommsServiceHandler(serverService, service)
	mux := http.NewServeMux()
	connecthttp.Mount(mux, serverService)
	queries.reset()
	server := tracedStreamServer(t, mux)
	client := compassv1connect.NewCommsServiceClient(connect.NewClient(connecthttp.NewTransport(newH2CStreamClient(t), server.URL, connecthttp.WithReadMaxBytes(0))))

	ctx, cancel := context.WithCancel(t.Context())
	stream, err := client.SubscribeComms(ctx, &compassv1.SubscribeCommsRequest{SinceSeq: 0})
	if err != nil {
		cancel()
		t.Fatalf("SubscribeComms: %v", err)
	}
	boundary, receiveErr := stream.Receive()
	if receiveErr != nil || boundary.GetSnapshotSeq() != 0 {
		cancel()
		_ = stream.Close()
		t.Fatalf("snapshot boundary = %+v, err=%v", boundary, receiveErr)
	}
	cancel()
	if extra, receiveErr := stream.Receive(); receiveErr == nil {
		t.Fatalf("SubscribeComms delivered unexpected post-cancel frame: %+v", extra)
	}
	_ = stream.Close()

	assertStreamQueryHasNoSpan(t, queries, "MessagesHeadSeq")
	waitForStreamSpan(t, recorder)
	assertStreamSpanRecorded(t, recorder, "MessagesHeadSeq")
}

func assertStreamQueryHasNoSpan(t *testing.T, queries *streamQueryTracer, name string) {
	t.Helper()
	contexts := queries.spanContexts(name)
	if len(contexts) != 1 {
		t.Fatalf("%s query contexts = %d, want exactly one", name, len(contexts))
	}
	if contexts[0].IsValid() {
		t.Fatalf("%s query carried stream span %s, want no active span", name, contexts[0].SpanID())
	}
}

func newH2CStreamClient(t *testing.T) *http.Client {
	t.Helper()
	transport := h2cTransport(func(ctx context.Context, network, addr string) (net.Conn, error) {
		var dialer net.Dialer
		return dialer.DialContext(ctx, network, addr)
	})
	t.Cleanup(transport.CloseIdleConnections)
	return &http.Client{Transport: transport}
}
