//go:build unix

package runnerhub

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
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
	otelx "github.com/RigelBuild/compass/go/internal/otel"
	"github.com/RigelBuild/compass/go/internal/store"
)

const h6Traceparent = "00-11111111111111111111111111111111-2222222222222222-01"

func TestPresenceEdgesRestoreTheirSourceTrace(t *testing.T) {
	previousProvider := otel.GetTracerProvider()
	previousPropagator := otel.GetTextMapPropagator()
	provider := sdktrace.NewTracerProvider()
	otel.SetTracerProvider(provider)
	otel.SetTextMapPropagator(propagation.TraceContext{})
	t.Cleanup(func() {
		otel.SetTracerProvider(previousProvider)
		otel.SetTextMapPropagator(previousPropagator)
		if err := provider.Shutdown(context.WithoutCancel(t.Context())); err != nil {
			t.Errorf("shutdown tracer provider: %v", err)
		}
	})

	hub := newHubOnly()
	pres := &fakePresenceSink{lifecycleCh: make(chan lifecycleRecord, 8)}
	hub.SetPresenceSink(pres)
	bindings := newFakeBindingStore()
	bindings.seedBinding("lost-session", testAgentAccount, testRunnerID)
	hub.SetSessionBindingStore(bindings)

	ctx, span := otel.Tracer("runnerhub.h6").Start(t.Context(), "enroll")
	if _, err := hub.enroll(ctx, testRunnerID, runnerSubject(), 0, 0); err != nil {
		t.Fatalf("enroll: %v", err)
	}
	enrolled := <-pres.lifecycleCh
	if enrolled.account != testAgentAccount || enrolled.traceparent == "" ||
		enrolled.traceparent != otelx.Traceparent(ctx) {
		t.Fatalf("enroll presence edge = %+v, want enroll traceparent %q",
			enrolled, otelx.Traceparent(ctx))
	}
	span.End()
	promotionCtx, promotionSpan := otel.Tracer("runnerhub.h6").Start(t.Context(), "promote")
	hub.bindContainer("container-traced", testAgentAccount, testRunnerID)
	hub.promoteSession(promotionCtx, "container-traced", "traced-session")
	if got := pres.promotedSnapshot(); len(got) != 1 || got[0].traceparent != otelx.Traceparent(promotionCtx) {
		t.Fatalf("promoted edge = %+v, want traceparent %q", got, otelx.Traceparent(promotionCtx))
	}
	promotionSpan.End()

	releaseCtx, releaseSpan := otel.Tracer("runnerhub.h6").Start(t.Context(), "release")
	hub.unbindSession(releaseCtx, "traced-session")
	releaseSpan.End()
	var released lifecycleRecord
	select {
	case released = <-pres.lifecycleCh:
	case <-timeAfter():
		t.Fatal("release did not produce a presence edge")
	}
	if released.state != compassv1.AgentSessionState_AGENT_SESSION_STATE_DISCONNECTED ||
		released.traceparent != otelx.Traceparent(releaseCtx) {
		t.Fatalf("release edge = %+v, want DISCONNECTED with release traceparent %q",
			released, otelx.Traceparent(releaseCtx))
	}

	hub.bindContainer("container-lost", testAgentAccount, testRunnerID)
	hub.promoteSession(t.Context(), "container-lost", "lost-session")
	traceCtx, traceSpan := otel.Tracer("runnerhub.h6").Start(t.Context(), "publish-client")
	interceptor, err := otelconnect.NewInterceptor()
	if err != nil {
		t.Fatalf("otelconnect.NewInterceptor: %v", err)
	}
	path, handler := NewMountedHandler(hub, runnerResolverForFetch().resolve, nil, nil, interceptor)
	mux := http.NewServeMux()
	mux.Handle(path, handler)
	server := httptest.NewUnstartedServer(mux)
	server.Config.Protocols = cleartextHTTP2()
	server.Start()
	t.Cleanup(server.Close)
	client := newRawRunnerClient(t, server.URL, "runner-tok")
	publish := client.PublishEvents(traceCtx)
	if err := publish.Send(&compassv1internal.PublishEventsRequest{
		RunnerSeq: 1,
		SessionId: "lost-session",
		Frame:     sessionStateFrame(compassv1.AgentSessionState_AGENT_SESSION_STATE_WORKING),
	}); err != nil {
		t.Fatalf("PublishEvents.Send: %v", err)
	}
	if _, err := publish.CloseAndReceive(); err != nil {
		t.Fatalf("PublishEvents.CloseAndReceive: %v", err)
	}
	traceSpan.End()
	if got := <-pres.lifecycleCh; got.traceparent != "" {
		t.Fatalf("PublishEvents edge traceparent = %q, want empty", got.traceparent)
	}

	streamCtx, streamSpan := otel.Tracer("runnerhub.h6").Start(t.Context(), "sessions-client")
	streamCtx = otelx.ContextWithTraceparent(streamCtx, h6Traceparent)
	sessions := client.Sessions(streamCtx)
	loopErr := make(chan error, 1)
	go runnerSessionsLoopWithNotFound(sessions, loopErr)
	waitRouterAttached(t, hub)
	if err := hub.DispatchControl(streamCtx, "lost-session", &compassv1internal.AgentControl{}); err != nil {
		t.Fatalf("DispatchControl: %v", err)
	}
	var got lifecycleRecord
	select {
	case got = <-pres.lifecycleCh:
	case <-timeAfter():
		t.Fatal("send-only refusal did not produce a presence edge")
	}
	if err := sessions.CloseRequest(); err != nil {
		t.Fatalf("Sessions.CloseRequest: %v", err)
	}
	if err := recvLoopErr(t, loopErr); err != nil {
		t.Fatalf("Runner Sessions loop: %v", err)
	}
	streamSpan.End()
	if got.state != compassv1.AgentSessionState_AGENT_SESSION_STATE_DISCONNECTED || got.traceparent != h6Traceparent {
		t.Fatalf("send-only refusal edge = %+v, want DISCONNECTED with deliver traceparent", got)
	}
}

func TestRelayCommsCallLogCarriesServerSpan(t *testing.T) {
	previousProvider := otel.GetTracerProvider()
	previousPropagator := otel.GetTextMapPropagator()
	recorder := tracetest.NewSpanRecorder()
	provider := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder))
	otel.SetTracerProvider(provider)
	otel.SetTextMapPropagator(propagation.TraceContext{})
	t.Cleanup(func() {
		otel.SetTracerProvider(previousProvider)
		otel.SetTextMapPropagator(previousPropagator)
		if err := provider.Shutdown(context.WithoutCancel(t.Context())); err != nil {
			t.Errorf("shutdown tracer provider: %v", err)
		}
	})

	var logs bytes.Buffer
	logger := slog.New(otelx.NewTraceHandler(slog.NewTextHandler(&logs, nil)))
	bindings := newTraceRecordingBindingStore()
	bindings.tenantErr = store.ErrConflict
	hub := NewHub(&fakeLifecycleSink{}, &fakeTailSink{}, &fakeCommsCaller{}, logger)
	hub.SetSessionBindingStore(bindings)
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
	_, err = client.RelayCommsCall(t.Context(), connect.NewRequest(&compassv1internal.RelayCommsCallRequest{SessionId: "sess-1"}))
	if connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Fatalf("RelayCommsCall error = %v, want FailedPrecondition", err)
	}

	ended := recorder.Ended()
	if len(ended) != 1 || ended[0].SpanKind() != trace.SpanKindServer {
		t.Fatalf("ended spans = %+v, want one server span", ended)
	}
	line := logs.String()
	if !strings.Contains(line, "trace_id="+ended[0].SpanContext().TraceID().String()) || !strings.Contains(line, "span_id="+ended[0].SpanContext().SpanID().String()) {
		t.Fatalf("RelayCommsCall log = %q, want server span trace_id=%s span_id=%s", line, ended[0].SpanContext().TraceID(), ended[0].SpanContext().SpanID())
	}
}

func TestPublishEventsHubDeliverLogHasNoStreamTrace(t *testing.T) {
	previousProvider := otel.GetTracerProvider()
	previousPropagator := otel.GetTextMapPropagator()
	recorder := tracetest.NewSpanRecorder()
	provider := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder))
	otel.SetTracerProvider(provider)
	otel.SetTextMapPropagator(propagation.TraceContext{})
	t.Cleanup(func() {
		otel.SetTracerProvider(previousProvider)
		otel.SetTextMapPropagator(previousPropagator)
		if err := provider.Shutdown(context.WithoutCancel(t.Context())); err != nil {
			t.Errorf("shutdown tracer provider: %v", err)
		}
	})

	var logs bytes.Buffer
	logger := slog.New(otelx.NewTraceHandler(slog.NewTextHandler(&logs, nil)))
	hub := NewHub(&fakeLifecycleSink{}, &fakeTailSink{}, nil, logger)
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
	ctx, span := otel.Tracer("runnerhub.trace-test").Start(t.Context(), "publish-events-test")
	defer span.End()
	publish := client.PublishEvents(ctx)
	if err := publish.Send(&compassv1internal.PublishEventsRequest{
		RunnerSeq: 1,
		SessionId: "sess-1",
		Frame:     &compassv1internal.AgentFrame{},
	}); err != nil {
		t.Fatalf("PublishEvents.Send: %v", err)
	}
	if _, err := publish.CloseAndReceive(); err != nil {
		t.Fatalf("PublishEvents.CloseAndReceive: %v", err)
	}

	ended := recorder.Ended()
	if len(ended) != 1 || ended[0].SpanKind() != trace.SpanKindServer {
		t.Fatalf("ended spans = %+v, want one server stream span", ended)
	}
	if line := logs.String(); !strings.Contains(line, "msg=\"unknown runner frame") || strings.Contains(line, "trace_id=") {
		t.Fatalf("Hub.Deliver log = %q, want unknown frame without trace_id", line)
	}
}

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
