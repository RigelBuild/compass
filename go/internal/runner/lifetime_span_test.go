//go:build unix

package runner

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"connectrpc.com/connect"
	"go.opentelemetry.io/otel"

	compassv1 "github.com/RigelBuild/compass/go/gen/compass/v1"
	compassv1internal "github.com/RigelBuild/compass/go/internal/gen/compass/v1"
	otelx "github.com/RigelBuild/compass/go/internal/otel"
	"github.com/RigelBuild/compass/go/internal/runtime"
)

// lineSink hands each JSON log line to the test; the handler writes one line
// per Write, so a Write is a record.
type lineSink struct{ lines chan string }

func (s lineSink) Write(p []byte) (int, error) {
	select {
	case s.lines <- string(bytes.TrimSpace(p)):
	default: // the test reads only the line it waits for; never block a drain
	}
	return len(p), nil
}

// lineWith waits for the first log line containing msg.
func (s lineSink) lineWith(t *testing.T, msg string) string {
	t.Helper()
	deadline := timeAfter()
	for {
		select {
		case line := <-s.lines:
			if strings.Contains(line, msg) {
				return line
			}
		case <-deadline:
			t.Fatalf("no log line with %q", msg)
		}
	}
}

// traceLogger logs JSON through TraceHandler, which stamps trace_id from ctx.
func traceLogger() (*slog.Logger, lineSink) {
	sink := lineSink{lines: make(chan string, 64)}
	return slog.New(otelx.NewTraceHandler(slog.NewJSONHandler(sink, nil))), sink
}

func TestStartAgentDrainLogCarriesNoCallerTrace(t *testing.T) {
	installInMemoryTracer(t)
	engine := newPipeRuntime()
	link := newLink(newRunnerServiceServer(t, newCapturePublish()))
	log, buf := traceLogger()

	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	ctx, span := otel.Tracer("test").Start(ctx, "start-command")
	defer span.End()

	if _, err := link.StartAgent(ctx, "sess-1", runtime.WorkloadID("c1"), engine, testAgentEnv(), log); err != nil {
		t.Fatalf("StartAgent = %v", err)
	}
	if _, err := engine.stderrW.Write([]byte("agent says hi\n")); err != nil {
		t.Fatalf("write stderr: %v", err)
	}
	line := buf.lineWith(t, "agent says hi")
	if strings.Contains(line, "trace_id") {
		t.Fatalf("drain line %s carries a trace_id, want none (the Start span %s ended)", line, span.SpanContext().TraceID())
	}

	if err := engine.stderrW.Close(); err != nil {
		t.Fatalf("closing stderr: %v", err)
	}
	engine.closeStdout()
}

// failingPublish refuses every PublishEvents stream, so markErrored's state
// report fails and logs.
type failingPublish struct{ *capturePublish }

func (failingPublish) PublishEvents(context.Context, *connect.ClientStream[compassv1internal.PublishEventsRequest]) (*connect.Response[compassv1internal.PublishEventsResponse], error) {
	return nil, connect.NewError(connect.CodeUnavailable, errors.New("relay down"))
}

func TestRetireOnExitLogCarriesNoCallerTrace(t *testing.T) {
	installInMemoryTracer(t)
	engine := newStubStreamingRuntimeWithScript(t, "#!/bin/sh\nexit 7\n")
	h := newTransportFixtureWithEngine(t, failingPublish{newCapturePublish()}, engine)
	log, buf := traceLogger()
	h.log = log

	ctx, span := otel.Tracer("test").Start(t.Context(), "start-command")
	defer span.End()
	name, err := h.Provision(ctx, &compassv1.ProvisionAgentWorkspaceRequest{}, "acct-1")
	if err != nil {
		t.Fatalf("Provision = %v", err)
	}
	if _, err := h.Start(ctx, &compassv1.StartAgentSessionRequest{ContainerName: name}, "", "sess-dies"); err != nil {
		t.Fatalf("Start = %v", err)
	}
	line := buf.lineWith(t, "reporting agent exit to server")
	if strings.Contains(line, "trace_id") {
		t.Fatalf("markErrored line %s carries a trace_id, want none (the Start span %s ended)", line, span.SpanContext().TraceID())
	}
}
