//go:build unix

package runner

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"

	otelx "github.com/RigelBuild/compass/go/internal/otel"
	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/noop"
)

const (
	commandEnvelopeTraceparent = "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01"
	commandOpTraceparent       = "00-0af7651916cd43dd8448eb211c80319c-b7ad6b7169203331-01"
)

func installCommandTracer(t *testing.T) *tracetest.SpanRecorder {
	t.Helper()
	recorder := tracetest.NewSpanRecorder()
	provider := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder))
	previous := otel.GetTracerProvider()
	otel.SetTracerProvider(provider)
	t.Cleanup(func() {
		otel.SetTracerProvider(previous)
		if err := provider.Shutdown(context.WithoutCancel(t.Context())); err != nil {
			t.Errorf("shutdown tracer provider: %v", err)
		}
	})
	return recorder
}

func commandLog(t *testing.T, stream *scriptStream, host SessionHost, ctx context.Context) string {
	t.Helper()
	stream.recv <- recvItem{err: io.EOF}
	var output bytes.Buffer
	logger := slog.New(otelx.NewTraceHandler(slog.NewTextHandler(&output, nil)))
	if err := runSessions(ctx, stream, host, logger); err != nil {
		t.Fatalf("runSessions: %v", err)
	}
	return output.String()
}

func TestRunSessionsCommandSpanUsesEnvelopeTraceparent(t *testing.T) {
	recorder := installCommandTracer(t)
	remote := trace.SpanContextFromContext(otelx.ContextWithTraceparent(t.Context(), commandEnvelopeTraceparent))
	stream := newScriptStream()
	cmd := startCommand("trace-test")
	cmd.Traceparent = commandEnvelopeTraceparent
	stream.pushCmd(cmd)

	var output bytes.Buffer
	logger := slog.New(otelx.NewTraceHandler(slog.NewTextHandler(&output, nil)))
	stream.recv <- recvItem{err: io.EOF}
	if err := runSessions(t.Context(), stream, &fakeSessionHost{startErr: errors.New("start failed")}, logger); err != nil {
		t.Fatalf("runSessions: %v", err)
	}
	var command sdktrace.ReadOnlySpan
	for _, span := range recorder.Ended() {
		if span.Name() == "runner.sessions.command" {
			command = span
		}
	}
	if command == nil {
		t.Fatal("no runner.sessions.command span recorded")
	}
	if got := command.Parent(); got.TraceID() != remote.TraceID() || got.SpanID() != remote.SpanID() {
		t.Fatalf("command parent = %s/%s, want envelope parent %s/%s", got.TraceID(), got.SpanID(), remote.TraceID(), remote.SpanID())
	}
	line := output.String()
	if !strings.Contains(line, "trace_id="+remote.TraceID().String()) || !strings.Contains(line, "span_id="+command.SpanContext().SpanID().String()) {
		t.Fatalf("session command failure log = %q, want envelope trace id and command span id", line)
	}
}

func TestRunSessionsDeliverControlPrefersOpTraceparent(t *testing.T) {
	recorder := installCommandTracer(t)
	opParent := trace.SpanContextFromContext(otelx.ContextWithTraceparent(t.Context(), commandOpTraceparent))
	stream := newScriptStream()
	cmd := deliverControlCommand("trace-test", "session-op", "message-op")
	cmd.Traceparent = commandEnvelopeTraceparent
	cmd.GetDeliverControl().GetOp().GetDeliver().Traceparent = commandOpTraceparent
	stream.pushCmd(cmd)

	stream.recv <- recvItem{err: io.EOF}
	if err := runSessions(t.Context(), stream, &fakeSessionHost{}, discardLoggerRunner()); err != nil {
		t.Fatalf("runSessions: %v", err)
	}
	var command sdktrace.ReadOnlySpan
	for _, span := range recorder.Ended() {
		if span.Name() == "runner.sessions.command" {
			command = span
		}
	}
	if command == nil {
		t.Fatal("no runner.sessions.command span recorded")
	}
	if got := command.Parent(); got.TraceID() != opParent.TraceID() || got.SpanID() != opParent.SpanID() {
		t.Fatalf("command parent = %s/%s, want op parent %s/%s, not envelope", got.TraceID(), got.SpanID(), opParent.TraceID(), opParent.SpanID())
	}
	attributes := make(map[string]string)
	for _, attr := range command.Attributes() {
		attributes[string(attr.Key)] = attr.Value.AsString()
	}
	for key, want := range map[string]string{
		"compass.runner.command": "deliver_control",
		"compass.op.kind":        "deliver",
		"compass.session.id":     "session-op",
	} {
		if got := attributes[key]; got != want {
			t.Errorf("%s = %q, want %q", key, got, want)
		}
	}
}

func TestRunSessionsProviderOffKeepsEnvelopeTraceIDInLog(t *testing.T) {
	previous := otel.GetTracerProvider()
	otel.SetTracerProvider(noop.NewTracerProvider())
	t.Cleanup(func() { otel.SetTracerProvider(previous) })

	ctx := otelx.ContextWithTraceparent(t.Context(), commandEnvelopeTraceparent)
	stream := newScriptStream()
	cmd := startCommand("trace-test")
	cmd.Traceparent = commandEnvelopeTraceparent
	stream.pushCmd(cmd)
	output := commandLog(t, stream, &fakeSessionHost{startErr: errors.New("start failed")}, ctx)
	remote := trace.SpanContextFromContext(ctx)
	if !strings.Contains(output, "trace_id="+remote.TraceID().String()) {
		t.Fatalf("session command failure log = %q, want envelope trace id %s with provider disabled", output, remote.TraceID())
	}
}

func TestRunSessionsEmptyEnvelopeStartsRoot(t *testing.T) {
	recorder := installCommandTracer(t)
	stream := newScriptStream()
	stream.pushCmd(startCommand("root-test"))
	stream.recv <- recvItem{err: io.EOF}
	if err := runSessions(t.Context(), stream, &fakeSessionHost{startErr: errors.New("start failed")}, discardLoggerRunner()); err != nil {
		t.Fatalf("runSessions: %v", err)
	}
	for _, span := range recorder.Ended() {
		if span.Name() == "runner.sessions.command" {
			if span.Parent().IsValid() || !span.SpanContext().IsValid() {
				t.Fatalf("command span context = %+v, parent = %+v, want valid root span", span.SpanContext(), span.Parent())
			}
			return
		}
	}
	t.Fatal("no runner.sessions.command span recorded")
}
