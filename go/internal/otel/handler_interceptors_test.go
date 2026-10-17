package otel

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"connectrpc.com/connect"
	"connectrpc.com/otelconnect"
	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
	"google.golang.org/protobuf/types/known/emptypb"
)

type contextKey struct{}

func TestWithoutSpanClearsOnlyTheSpan(t *testing.T) {
	provider := sdktrace.NewTracerProvider()
	t.Cleanup(func() {
		if err := provider.Shutdown(context.Background()); err != nil {
			t.Errorf("shutdown tracer provider: %v", err)
		}
	})
	ctx, span := provider.Tracer("without-span-test").Start(t.Context(), "stream")
	defer span.End()
	ctx = context.WithValue(ctx, contextKey{}, "kept")
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	got := WithoutSpan(ctx)
	if sc := trace.SpanContextFromContext(got); sc.IsValid() {
		t.Fatalf("span context = %v, want invalid", sc)
	}
	if value := got.Value(contextKey{}); value != "kept" {
		t.Errorf("context value = %v, want kept", value)
	}
	cancel()
	if got.Err() != context.Canceled {
		t.Errorf("cancellation = %v, want %v", got.Err(), context.Canceled)
	}
}

func TestHandlerInterceptorsMountsSpanAndTraceResponseBeforeRest(t *testing.T) {
	previous := otel.GetTracerProvider()
	exporter := tracetest.NewInMemoryExporter()
	provider := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exporter))
	otel.SetTracerProvider(provider)
	t.Cleanup(func() {
		otel.SetTracerProvider(previous)
		if err := provider.Shutdown(context.Background()); err != nil {
			t.Errorf("shutdown tracer provider: %v", err)
		}
	})
	ic, err := otelconnect.NewInterceptor()
	if err != nil {
		t.Fatalf("otelconnect.NewInterceptor: %v", err)
	}
	var restSawSpan bool
	var restSawEmptyTraceResponse bool
	rest := connect.UnaryInterceptorFunc(func(next connect.UnaryFunc) connect.UnaryFunc {
		return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
			restSawSpan = trace.SpanFromContext(ctx).SpanContext().IsValid()
			resp, err := next(ctx, req)
			if resp != nil {
				restSawEmptyTraceResponse = resp.Header().Get("traceresponse") == ""
			}
			return resp, err
		}
	})
	handler := connect.NewUnaryHandler("/test.Service/Call", func(context.Context, *emptypb.Empty) (*emptypb.Empty, error) {
		return &emptypb.Empty{}, nil
	}, HandlerInterceptors(ic, rest))
	mux := http.NewServeMux()
	mux.Handle("/test.Service/Call", handler)
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)

	client := connect.NewClient[emptypb.Empty, emptypb.Empty](server.Client(), server.URL+"/test.Service/Call")
	response, err := client.CallUnary(t.Context(), &emptypb.Empty{})
	if err != nil {
		t.Fatalf("CallUnary: %v", err)
	}
	if !restSawSpan {
		t.Fatal("caller interceptor did not run inside the server span")
	}
	if !restSawEmptyTraceResponse {
		t.Fatal("traceresponse set before the caller interceptor returned")
	}
	// The outer trace-response interceptor adds this header after rest returns.
	if got := response.Header().Get("traceresponse"); got == "" {
		t.Fatal("traceresponse header is empty")
	}
	spans := exporter.GetSpans()
	if len(spans) != 1 || spans[0].SpanKind != trace.SpanKindServer {
		t.Fatalf("recorded spans = %+v, want exactly one server span", spans)
	}
}
