package store

import (
	"context"
	"errors"

	"github.com/RigelBuild/compass/go/internal/sqlc"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
	"go.opentelemetry.io/otel/trace"
)

const (
	unnamedQuerySpanName = "postgres"
	batchSpanName        = "postgres.batch"
)

// spanTracer records client spans for SQLC queries and batches under a valid parent.
// It does not retain a context beyond pgx's tracer callbacks.
type spanTracer struct {
	tracer trace.Tracer
}

type spanContextKey struct{}

func (t spanTracer) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	if !trace.SpanContextFromContext(ctx).IsValid() {
		return ctx
	}
	name := querySpanName(data.SQL, unnamedQuerySpanName)
	ctx, span := t.tracer.Start(ctx, name, trace.WithSpanKind(trace.SpanKindClient)) //nolint:spancheck // ended in TraceQueryEnd
	span.SetAttributes(semconv.DBSystemPostgreSQL, semconv.DBOperationName(name))
	return context.WithValue(ctx, spanContextKey{}, span) //nolint:spancheck // ended in TraceQueryEnd
}

func (t spanTracer) TraceQueryEnd(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryEndData) {
	span, ok := ctx.Value(spanContextKey{}).(trace.Span)
	if !ok {
		return
	}
	finishSpan(span, data.Err)
}

func (t spanTracer) TraceBatchStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceBatchStartData) context.Context {
	if !trace.SpanContextFromContext(ctx).IsValid() {
		return ctx
	}
	name := batchSpanName
	if data.Batch != nil {
		for _, query := range data.Batch.QueuedQueries {
			if query == nil {
				continue
			}
			if queryName, ok := sqlc.QueryName(query.SQL); ok {
				name = queryName
				break
			}
		}
	}
	ctx, span := t.tracer.Start(ctx, name, trace.WithSpanKind(trace.SpanKindClient)) //nolint:spancheck // ended in TraceBatchEnd
	span.SetAttributes(semconv.DBSystemPostgreSQL, semconv.DBOperationName(name))
	return context.WithValue(ctx, spanContextKey{}, span) //nolint:spancheck // ended in TraceBatchEnd
}

func (spanTracer) TraceBatchQuery(context.Context, *pgx.Conn, pgx.TraceBatchQueryData) {}

func (t spanTracer) TraceBatchEnd(ctx context.Context, _ *pgx.Conn, data pgx.TraceBatchEndData) {
	span, ok := ctx.Value(spanContextKey{}).(trace.Span)
	if !ok {
		return
	}
	finishSpan(span, data.Err)
}

func querySpanName(query, fallback string) string {
	if name, ok := sqlc.QueryName(query); ok {
		return name
	}
	return fallback
}

func finishSpan(span trace.Span, err error) {
	if err != nil {
		description := "error"
		if pgErr, ok := errors.AsType[*pgconn.PgError](err); ok {
			description = pgErr.Code
			span.SetAttributes(attribute.String("db.response.status_code", pgErr.Code))
		}
		span.SetStatus(codes.Error, description)
	}
	span.End()
}
