//go:build pgtest && unix

package store

import (
	"context"
	"slices"
	"testing"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/RigelBuild/compass/go/internal/pgtest"
)

func installStoreSpanRecorder(t *testing.T) (*tracetest.SpanRecorder, *sdktrace.TracerProvider) {
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
	return recorder, provider
}

func TestStoreQueriesTraceOnlyUnderRequestParent(t *testing.T) {
	recorder, provider := installStoreSpanRecorder(t)
	s := newTestStore(t)
	account := mustUser(t, s, "trace-query-user")

	t.Run("request parent", func(t *testing.T) {
		ctx, parent := provider.Tracer(instrumentationScope).Start(t.Context(), "request")
		if _, err := s.GetAccount(WithTenant(ctx, s.bootstrapTenantID), account.ID); err != nil {
			t.Fatalf("GetAccount(tenant): %v", err)
		}
		if _, err := s.GetAccount(ctx, account.ID); err != nil {
			t.Fatalf("GetAccount(no tenant): %v", err)
		}
		parent.End()

		spans := recorder.Ended()
		if len(spans) != 3 {
			t.Fatalf("recorded spans = %d, want request and two query spans", len(spans))
		}
		var queries []sdktrace.ReadOnlySpan
		for _, span := range spans {
			if span.Name() == "GetAccount" {
				queries = append(queries, span)
			}
		}
		if len(queries) != 2 {
			t.Fatalf("GetAccount spans = %d, want 2", len(queries))
		}
		for _, span := range queries {
			assertSpan(t, span, spanWant{name: "GetAccount", operation: "GetAccount", parent: parent.SpanContext(), code: codes.Unset})
		}
	})
	t.Run("no parent", func(t *testing.T) {
		recorder.Reset()
		if _, err := s.GetAccount(WithTenant(t.Context(), s.bootstrapTenantID), account.ID); err != nil {
			t.Fatalf("GetAccount: %v", err)
		}
		if _, err := s.ListEnabledForgeRepos(WithSystemRole(t.Context())); err != nil {
			t.Fatalf("ListEnabledForgeRepos: %v", err)
		}
		if spans := recorder.Ended(); len(spans) != 0 {
			t.Fatalf("recorded spans without parent = %d, want 0", len(spans))
		}
	})
}

func TestOpenTracedComposesQueryCounterAndSpanTracer(t *testing.T) {
	recorder, provider := installStoreSpanRecorder(t)
	counter := &pgtest.SQLCQueryCounter{}
	s := newTracedTestStore(t, counter)
	account := mustUser(t, s, "trace-counter-user")
	counter.Reset()

	ctx, parent := provider.Tracer(instrumentationScope).Start(t.Context(), "request")
	if _, err := s.GetAccount(WithTenant(ctx, s.bootstrapTenantID), account.ID); err != nil {
		t.Fatalf("GetAccount: %v", err)
	}
	parent.End()

	if got, want := counter.Names(), []string{"GetAccount"}; !slices.Equal(got, want) {
		t.Errorf("SQLCQueryCounter names = %v, want %v", got, want)
	}
	if counter.Count() != 1 {
		t.Errorf("SQLCQueryCounter count = %d, want 1", counter.Count())
	}
	if spans := recorder.Ended(); len(spans) != 2 {
		t.Fatalf("recorded spans = %d, want request and query", len(spans))
	}
	assertSpan(t, requireRecordedSpan(t, recorder, "GetAccount"), spanWant{name: "GetAccount", operation: "GetAccount", parent: parent.SpanContext(), code: codes.Unset})
}
