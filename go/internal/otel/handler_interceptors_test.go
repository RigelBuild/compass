package otel

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"connectrpc.com/connect/v2"
	"connectrpc.com/connect/v2/connecthttp"
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
	ic, err := otelconnect.NewServerInterceptor()
	if err != nil {
		t.Fatalf("otelconnect.NewServerInterceptor: %v", err)
	}
	var restSawSpan bool
	var restSawEmptyTraceResponse bool
	rest := connect.ServerInterceptor(func(next connect.ServerFunc) connect.ServerFunc {
		return func(ctx context.Context, spec connect.Spec, stream connect.ServerStream) error {
			restSawSpan = trace.SpanFromContext(ctx).SpanContext().IsValid()
			info, _ := connect.CallInfoForServerContext(ctx)
			restSawEmptyTraceResponse = info.ResponseHeader().Get("traceresponse") == ""
			return next(ctx, spec, stream)
		}
	})
	spec := connect.Spec{StreamType: connect.StreamTypeUnary, Procedure: "/test.Service/Call"}
	server := connect.NewServer(HandlerInterceptors(ic, rest))
	server.Register(connect.Method{Spec: spec, Handler: func(_ context.Context, _ connect.Spec, stream connect.ServerStream) error {
		if err := stream.Receive(&emptypb.Empty{}); err != nil {
			return err
		}
		return stream.Send(&emptypb.Empty{})
	}})
	mux := http.NewServeMux()
	connecthttp.Mount(mux, server)
	httpServer := httptest.NewServer(mux)
	t.Cleanup(httpServer.Close)

	client := connect.NewClient(connecthttp.NewTransport(httpServer.Client(), httpServer.URL))
	ctx, clientInfo := connect.NewClientContext(t.Context())
	if err := client.CallUnary(ctx, spec, &emptypb.Empty{}, &emptypb.Empty{}); err != nil {
		t.Fatalf("CallUnary: %v", err)
	}
	if !restSawSpan {
		t.Fatal("caller interceptor did not run inside the server span")
	}
	if !restSawEmptyTraceResponse {
		t.Fatal("traceresponse was set before the rest interceptor returned")
	}
	if got := clientInfo.ResponseHeader().Get("traceresponse"); got == "" {
		t.Fatal("traceresponse header is empty")
	}
	spans := exporter.GetSpans()
	if len(spans) != 1 || spans[0].SpanKind != trace.SpanKindServer {
		t.Fatalf("recorded spans = %+v, want exactly one server span", spans)
	}
}
