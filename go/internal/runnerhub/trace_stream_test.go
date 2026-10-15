//go:build unix

package runnerhub

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"connectrpc.com/connect"
	"connectrpc.com/otelconnect"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"

	compassv1 "github.com/RigelBuild/compass/go/gen/compass/v1"
	compassv1internal "github.com/RigelBuild/compass/go/internal/gen/compass/v1"
	"github.com/RigelBuild/compass/go/internal/store"
)

type traceRecordingBindingStore struct {
	*fakeBindingStore
	mu     sync.Mutex
	calls  map[string]trace.SpanContext
	called chan string
}

func newTraceRecordingBindingStore() *traceRecordingBindingStore {
	return &traceRecordingBindingStore{
		fakeBindingStore: newFakeBindingStore(),
		calls:            make(map[string]trace.SpanContext),
		called:           make(chan string, 8),
	}
}

func (b *traceRecordingBindingStore) ResolveSessionBinding(ctx context.Context, sessionID string) (store.AccountID, string, string, error) {
	b.mu.Lock()
	b.calls["resolve"] = trace.SpanContextFromContext(ctx)
	b.mu.Unlock()
	b.called <- "resolve"
	return b.fakeBindingStore.ResolveSessionBinding(ctx, sessionID)
}

func (b *traceRecordingBindingStore) SessionBindingTenant(ctx context.Context, sessionID, runnerID string) (store.TenantID, error) {
	b.mu.Lock()
	b.calls["tenant"] = trace.SpanContextFromContext(ctx)
	b.mu.Unlock()
	b.called <- "tenant"
	return b.fakeBindingStore.SessionBindingTenant(ctx, sessionID, runnerID)
}

func TestRunnerHubStreamsStripWorkSpanAndRetainStreamMetrics(t *testing.T) {
	previousProvider := otel.GetTracerProvider()
	previousPropagator := otel.GetTextMapPropagator()
	recorder := tracetest.NewSpanRecorder()
	provider := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder))
	otel.SetTracerProvider(provider)
	otel.SetTextMapPropagator(propagation.TraceContext{})
	t.Cleanup(func() {
		otel.SetTracerProvider(previousProvider)
		otel.SetTextMapPropagator(previousPropagator)
		if err := provider.Shutdown(context.Background()); err != nil {
			t.Errorf("shutdown tracer provider: %v", err)
		}
	})

	bindings := newTraceRecordingBindingStore()
	hub := newHubOnly()
	hub.SetSessionBindingStore(bindings)
	hub.enroll(t.Context(), testRunnerID, runnerSubject(), compassv1.RuntimeTier_RUNTIME_TIER_UNSPECIFIED, compassv1.EgressPosture_EGRESS_POSTURE_UNSPECIFIED)
	resolver := &fakeResolver{tokens: map[string]resolverEntry{
		"runner-tok": {subj: store.Subject{Kind: store.SubjectRunner, ID: testRunnerID}},
	}}
	otelIC, err := otelconnect.NewInterceptor()
	if err != nil {
		t.Fatalf("otelconnect.NewInterceptor: %v", err)
	}
	path, handler := NewMountedHandler(hub, resolver.resolve, nil, nil, otelIC)
	mux := http.NewServeMux()
	mux.Handle(path, handler)
	server := httptest.NewUnstartedServer(mux)
	server.Config.Protocols = cleartextHTTP2()
	server.Start()
	t.Cleanup(server.Close)
	client := newRawRunnerClient(t, server.URL, "runner-tok")

	publish := client.PublishEvents(t.Context())
	if err := publish.Send(&compassv1internal.PublishEventsRequest{
		RunnerSeq: 1,
		SessionId: "unmapped-session",
		Frame:     sessionTraceFrame("trace context probe"),
	}); err != nil {
		t.Fatalf("PublishEvents.Send: %v", err)
	}
	if _, err := publish.CloseAndReceive(); err != nil {
		t.Fatalf("PublishEvents.CloseAndReceive: %v", err)
	}
	if got := waitTraceBindingCall(t, bindings); got != "resolve" {
		t.Fatalf("first binding call = %q, want resolve", got)
	}
	assertNoSpanAtBindingCall(t, bindings, "resolve")

	bindings.seedBinding("lost-session", testAgentAccount, testRunnerID)
	sessions := client.Sessions(t.Context())
	loopErr := make(chan error, 1)
	go runnerSessionsLoopWithNotFound(sessions, loopErr)
	waitRouterAttached(t, hub)
	if err := hub.DispatchControl(t.Context(), "lost-session", &compassv1internal.AgentControl{}); err != nil {
		t.Fatalf("DispatchControl: %v", err)
	}
	if got := waitTraceBindingCall(t, bindings); got != "tenant" {
		t.Fatalf("NotFound binding call = %q, want tenant", got)
	}
	assertNoSpanAtBindingCall(t, bindings, "tenant")
	if err := sessions.CloseRequest(); err != nil {
		t.Fatalf("Sessions.CloseRequest: %v", err)
	}
	if err := recvLoopErr(t, loopErr); err != nil {
		t.Fatalf("Runner Sessions loop: %v", err)
	}

	if ended := recorder.Ended(); len(ended) != 2 || ended[0].SpanKind() != trace.SpanKindServer || ended[1].SpanKind() != trace.SpanKindServer {
		t.Fatalf("ended spans = %+v, want one server span per stream", ended)
	}
}

func waitTraceBindingCall(t *testing.T, bindings *traceRecordingBindingStore) string {
	t.Helper()
	select {
	case got := <-bindings.called:
		return got
	case <-timeAfter():
		t.Fatal("binding store did not record a call")
		return ""
	}
}

func assertNoSpanAtBindingCall(t *testing.T, bindings *traceRecordingBindingStore, call string) {
	t.Helper()
	bindings.mu.Lock()
	sc := bindings.calls[call]
	bindings.mu.Unlock()
	if sc.IsValid() {
		t.Fatalf("%s carried stream span %s, want no active span", call, sc.SpanID())
	}
}

func runnerSessionsLoopWithNotFound(stream *connect.BidiStreamForClient[compassv1internal.SessionsRequest, compassv1internal.SessionsResponse], done chan<- error) {
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
		if cmd.GetCommand() == nil {
			continue
		}
		if err := stream.Send(&compassv1internal.SessionsRequest{
			RequestId: cmd.GetRequestId(),
			Result: &compassv1internal.SessionsRequest_Error{Error: &compassv1internal.RunnerError{
				Code: compassv1internal.RunnerErrorCode_RUNNER_ERROR_CODE_NOT_FOUND,
			}},
		}); err != nil {
			done <- err
			return
		}
	}
}
