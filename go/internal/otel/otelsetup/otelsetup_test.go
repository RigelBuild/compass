package otelsetup

import (
	"context"
	"testing"

	"go.opentelemetry.io/otel"
	noopmetric "go.opentelemetry.io/otel/metric/noop"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace/noop"
)

// resetGlobals restores the global tracer/meter providers and propagator so an
// enabled-path test never leaks its installed globals into another test.
func resetGlobals(t *testing.T) {
	t.Helper()
	prevTP := otel.GetTracerProvider()
	prevMP := otel.GetMeterProvider()
	prevProp := otel.GetTextMapPropagator()
	t.Cleanup(func() {
		otel.SetTracerProvider(prevTP)
		otel.SetMeterProvider(prevMP)
		otel.SetTextMapPropagator(prevProp)
	})
}

// TestSetupTracerProviderDisabled asserts the empty-endpoint path returns a
// non-nil no-op shutdown and installs NO global tracer provider.
func TestSetupTracerProviderDisabled(t *testing.T) {
	resetGlobals(t)
	// A sentinel noop provider proves Setup* did not overwrite the global.
	sentinel := noop.NewTracerProvider()
	otel.SetTracerProvider(sentinel)

	shutdown, err := SetupTracerProvider(context.Background(), Config{ServiceName: "compass-server"})
	if err != nil {
		t.Fatalf("disabled SetupTracerProvider err = %v, want nil", err)
	}
	if shutdown == nil {
		t.Fatal("disabled SetupTracerProvider returned nil shutdown, want non-nil no-op")
	}
	if err := shutdown(context.Background()); err != nil {
		t.Fatalf("no-op shutdown err = %v, want nil", err)
	}
	if _, ok := otel.GetTracerProvider().(*sdktrace.TracerProvider); ok {
		t.Fatal("disabled path installed an SDK TracerProvider, want none")
	}
}

// TestSetupMeterProviderDisabled asserts the empty-endpoint path returns a
// non-nil no-op shutdown and installs NO global meter provider.
func TestSetupMeterProviderDisabled(t *testing.T) {
	resetGlobals(t)
	// A sentinel noop provider proves Setup* did not overwrite the global.
	sentinel := noopmetric.NewMeterProvider()
	otel.SetMeterProvider(sentinel)

	shutdown, err := SetupMeterProvider(context.Background(), Config{ServiceName: "compass-runner"})
	if err != nil {
		t.Fatalf("disabled SetupMeterProvider err = %v, want nil", err)
	}
	if shutdown == nil {
		t.Fatal("disabled SetupMeterProvider returned nil shutdown, want non-nil no-op")
	}
	if err := shutdown(context.Background()); err != nil {
		t.Fatalf("no-op shutdown err = %v, want nil", err)
	}
	if _, ok := otel.GetMeterProvider().(*sdkmetric.MeterProvider); ok {
		t.Fatal("disabled path installed an SDK MeterProvider, want none")
	}
}

// TestSetupTracerProviderEnabledResource asserts the enabled path installs a
// global SDK TracerProvider, a W3C propagator, and a resource carrying
// service.name/service.version.
func TestSetupTracerProviderEnabledResource(t *testing.T) {
	resetGlobals(t)
	cfg := Config{ServiceName: "compass-server", ServiceVersion: "1.2.3", Endpoint: "http://localhost:4318"}

	shutdown, err := SetupTracerProvider(context.Background(), cfg)
	if err != nil {
		t.Fatalf("enabled SetupTracerProvider err = %v", err)
	}
	t.Cleanup(func() { _ = shutdown(context.Background()) }) // best-effort flush; no collector, error not actionable in test

	if _, ok := otel.GetTracerProvider().(*sdktrace.TracerProvider); !ok {
		t.Fatalf("enabled path did not install an SDK TracerProvider, got %T", otel.GetTracerProvider())
	}

	// The enabled path installs the global W3C TraceContext propagator.
	if fields := otel.GetTextMapPropagator().Fields(); len(fields) == 0 || fields[0] != "traceparent" {
		t.Errorf("global propagator fields = %v, want traceparent", fields)
	}
}

// TestNewResourceAttributes asserts the resource the enabled providers carry
// tags service.name/service.version from Config. newResource is the single
// resource builder both SetupTracerProvider and SetupMeterProvider use, so this
// locks the attribute contract at its source.
func TestNewResourceAttributes(t *testing.T) {
	res, err := newResource(context.Background(), Config{ServiceName: "compass-server", ServiceVersion: "1.2.3"})
	if err != nil {
		t.Fatalf("newResource err = %v", err)
	}
	attrs := map[string]string{}
	for _, kv := range res.Attributes() {
		attrs[string(kv.Key)] = kv.Value.AsString()
	}
	if attrs["service.name"] != "compass-server" {
		t.Errorf("resource service.name = %q, want compass-server", attrs["service.name"])
	}
	if attrs["service.version"] != "1.2.3" {
		t.Errorf("resource service.version = %q, want 1.2.3", attrs["service.version"])
	}
}

// TestSetupMeterProviderEnabled asserts the enabled path installs a global SDK
// MeterProvider (mirrors the tracer enabled-path assertion).
func TestSetupMeterProviderEnabled(t *testing.T) {
	resetGlobals(t)
	cfg := Config{ServiceName: "compass-server", ServiceVersion: "1.2.3", Endpoint: "http://localhost:4318"}
	shutdown, err := SetupMeterProvider(context.Background(), cfg)
	if err != nil {
		t.Fatalf("enabled SetupMeterProvider err = %v, want nil", err)
	}
	t.Cleanup(func() { _ = shutdown(context.Background()) }) // best-effort flush; no collector, error not actionable in test
	if _, ok := otel.GetMeterProvider().(*sdkmetric.MeterProvider); !ok {
		t.Fatalf("enabled path did not install an SDK MeterProvider, got %T", otel.GetMeterProvider())
	}
}
