package store

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
)

func newRecordingSpanTracer(t *testing.T) (spanTracer, *tracetest.SpanRecorder, trace.Tracer) {
	t.Helper()
	recorder := tracetest.NewSpanRecorder()
	provider := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder))
	t.Cleanup(func() {
		if err := provider.Shutdown(context.WithoutCancel(t.Context())); err != nil {
			t.Errorf("shutdown tracer provider: %v", err)
		}
	})
	return spanTracer{tracer: provider.Tracer(instrumentationScope)}, recorder, provider.Tracer("test")
}

func requireRecordedSpan(t *testing.T, recorder *tracetest.SpanRecorder, name string) sdktrace.ReadOnlySpan {
	t.Helper()
	var found []sdktrace.ReadOnlySpan
	for _, span := range recorder.Ended() {
		if span.Name() == name {
			found = append(found, span)
		}
	}
	if len(found) != 1 {
		t.Fatalf("recorded spans named %q = %d, want 1", name, len(found))
	}
	return found[0]
}

func assertSpan(t *testing.T, got sdktrace.ReadOnlySpan, name, operation string, parent trace.SpanContext, wantCode codes.Code, wantDescription, wantSQLState string) {
	t.Helper()
	if got.Name() != name {
		t.Errorf("span name = %q, want %q", got.Name(), name)
	}
	if got.SpanKind() != trace.SpanKindClient {
		t.Errorf("span kind = %v, want client", got.SpanKind())
	}
	if got.Parent().SpanID() != parent.SpanID() || got.Parent().TraceID() != parent.TraceID() {
		t.Errorf("span parent = %v, want %v", got.Parent(), parent)
	}
	if got.Status().Code != wantCode || got.Status().Description != wantDescription {
		t.Errorf("span status = (%v, %q), want (%v, %q)", got.Status().Code, got.Status().Description, wantCode, wantDescription)
	}
	attrs := make(map[string]string)
	for _, attr := range got.Attributes() {
		attrs[string(attr.Key)] = attr.Value.AsString()
	}
	want := map[string]string{
		"db.system":         "postgresql",
		"db.operation.name": operation,
	}
	if wantSQLState != "" {
		want["db.response.status_code"] = wantSQLState
	}
	if len(attrs) != len(want) {
		t.Errorf("span attributes = %v, want exactly %v", attrs, want)
	}
	for key, value := range want {
		if attrs[key] != value {
			t.Errorf("span attribute %q = %q, want %q", key, attrs[key], value)
		}
	}
}

func TestSpanTracerQuerySpans(t *testing.T) {
	for _, tc := range []struct {
		name      string
		sql       string
		spanName  string
		operation string
	}{
		{
			name:      "named query",
			sql:       "-- name: GetTourState :one\nSELECT 'query-secret'",
			spanName:  "GetTourState",
			operation: "GetTourState",
		},
		{
			name:      "unnamed query",
			sql:       "SELECT 'query-secret'",
			spanName:  "postgres",
			operation: "postgres",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tracer, recorder, parentTracer := newRecordingSpanTracer(t)
			parentCtx, parent := parentTracer.Start(t.Context(), "request")
			queryCtx := tracer.TraceQueryStart(parentCtx, nil, pgx.TraceQueryStartData{SQL: tc.sql})
			tracer.TraceQueryEnd(queryCtx, nil, pgx.TraceQueryEndData{})
			parent.End()

			span := requireRecordedSpan(t, recorder, tc.spanName)
			assertSpan(t, span, tc.spanName, tc.operation, parent.SpanContext(), codes.Unset, "", "")
		})
	}
}

func TestSpanTracerBatchUsesFirstSQLCQueryName(t *testing.T) {
	tracer, recorder, parentTracer := newRecordingSpanTracer(t)
	parentCtx, parent := parentTracer.Start(t.Context(), "request")
	batch := &pgx.Batch{}
	batch.Queue("SET LOCAL ROLE compass_app")
	batch.Queue("SELECT set_config($1, $2, true)", "compass.tenant_id", "tenant")
	batch.Queue("-- name: GetTourState :one\nSELECT 'query-secret'")

	batchCtx := tracer.TraceBatchStart(parentCtx, nil, pgx.TraceBatchStartData{Batch: batch})
	tracer.TraceBatchQuery(batchCtx, nil, pgx.TraceBatchQueryData{SQL: "-- name: GetTourState :one\nSELECT 'query-secret'"})
	tracer.TraceBatchEnd(batchCtx, nil, pgx.TraceBatchEndData{})
	parent.End()

	span := requireRecordedSpan(t, recorder, "GetTourState")
	assertSpan(t, span, "GetTourState", "GetTourState", parent.SpanContext(), codes.Unset, "", "")
}

func TestSpanTracerBatchWithoutSQLCQueryUsesFallbackName(t *testing.T) {
	tracer, recorder, parentTracer := newRecordingSpanTracer(t)
	parentCtx, parent := parentTracer.Start(t.Context(), "request")
	batch := &pgx.Batch{}
	batch.Queue("SET LOCAL ROLE compass_app")

	batchCtx := tracer.TraceBatchStart(parentCtx, nil, pgx.TraceBatchStartData{Batch: batch})
	tracer.TraceBatchEnd(batchCtx, nil, pgx.TraceBatchEndData{})
	parent.End()

	span := requireRecordedSpan(t, recorder, "postgres.batch")
	assertSpan(t, span, "postgres.batch", "postgres.batch", parent.SpanContext(), codes.Unset, "", "")
}

func TestSpanTracerUsesGenericErrorDescription(t *testing.T) {
	tracer, recorder, parentTracer := newRecordingSpanTracer(t)
	parentCtx, parent := parentTracer.Start(t.Context(), "request")
	queryCtx := tracer.TraceQueryStart(parentCtx, nil, pgx.TraceQueryStartData{SQL: "SELECT 'secret'"})
	tracer.TraceQueryEnd(queryCtx, nil, pgx.TraceQueryEndData{Err: errors.New("sensitive error")})
	parent.End()

	span := requireRecordedSpan(t, recorder, "postgres")
	assertSpan(t, span, "postgres", "postgres", parent.SpanContext(), codes.Error, "error", "")
}

func TestSpanTracerRecordsPostgresErrorCode(t *testing.T) {
	tracer, recorder, parentTracer := newRecordingSpanTracer(t)
	parentCtx, parent := parentTracer.Start(t.Context(), "request")
	queryCtx := tracer.TraceQueryStart(parentCtx, nil, pgx.TraceQueryStartData{SQL: "-- name: InsertAccount :exec\nINSERT INTO accounts VALUES ('secret')"})
	err := errors.Join(errors.New("wrapper"), &pgconn.PgError{Code: "23505", Message: "secret value"})
	tracer.TraceQueryEnd(queryCtx, nil, pgx.TraceQueryEndData{Err: err})
	parent.End()

	span := requireRecordedSpan(t, recorder, "InsertAccount")
	assertSpan(t, span, "InsertAccount", "InsertAccount", parent.SpanContext(), codes.Error, "23505", "23505")
}

func TestSpanTracerDoesNotStartWithoutParent(t *testing.T) {
	tracer, recorder, _ := newRecordingSpanTracer(t)
	ctx := tracer.TraceQueryStart(t.Context(), nil, pgx.TraceQueryStartData{SQL: "-- name: GetTourState :one\nSELECT 1"})
	tracer.TraceQueryEnd(ctx, nil, pgx.TraceQueryEndData{})
	batch := &pgx.Batch{}
	batch.Queue("-- name: GetTourState :one\nSELECT 1")
	ctx = tracer.TraceBatchStart(t.Context(), nil, pgx.TraceBatchStartData{Batch: batch})
	tracer.TraceBatchEnd(ctx, nil, pgx.TraceBatchEndData{})
	if spans := recorder.Ended(); len(spans) != 0 {
		t.Fatalf("recorded spans without parent = %d, want 0", len(spans))
	}
}
