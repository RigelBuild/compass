package otel

import (
	"context"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
)

// noopShutdown is the shutdown returned by the disabled (empty-endpoint) path:
// non-nil, and a no-op flush that always succeeds.
func noopShutdown(context.Context) error { return nil }

// newResource builds the OTel resource tagging every span/metric with the
// binary's service.name and service.version.
func newResource(ctx context.Context, cfg Config) (*resource.Resource, error) {
	return resource.New(ctx,
		resource.WithAttributes(
			semconv.ServiceName(cfg.ServiceName),
			semconv.ServiceVersion(cfg.ServiceVersion),
		),
	)
}

// SetupTracerProvider installs the global TracerProvider + W3C propagator when
// cfg.Endpoint is non-empty; otherwise it is a no-op. The returned shutdown
// flushes and stops the provider, bounded by the ctx it is given (callers
// derive a timeout from their own ctx — never Background()).
func SetupTracerProvider(ctx context.Context, cfg Config) (shutdown func(context.Context) error, err error) {
	if !cfg.enabled() {
		return noopShutdown, nil
	}

	res, err := newResource(ctx, cfg)
	if err != nil {
		return nil, err
	}

	// The exporter reads OTEL_EXPORTER_OTLP_ENDPOINT itself (standard OTLP
	// base-endpoint per-signal path suffixing); passing an explicit URL would
	// defeat the /v1/traces suffixing, so no endpoint option is set here.
	exporter, err := otlptracehttp.New(ctx)
	if err != nil {
		return nil, err
	}

	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exporter),
		sdktrace.WithResource(res),
	)
	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(propagation.TraceContext{})

	return tp.Shutdown, nil
}

// SetupMeterProvider installs the global MeterProvider (OTLP/http metrics
// exporter) under the same endpoint gate; a no-op when cfg.Endpoint is empty.
// With it installed, otelconnect emits RPC duration/count metrics from the same
// interceptor. Returns a shutdown that flushes and stops the provider.
func SetupMeterProvider(ctx context.Context, cfg Config) (shutdown func(context.Context) error, err error) {
	if !cfg.enabled() {
		return noopShutdown, nil
	}

	res, err := newResource(ctx, cfg)
	if err != nil {
		return nil, err
	}

	exporter, err := otlpmetrichttp.New(ctx)
	if err != nil {
		return nil, err
	}

	mp := metric.NewMeterProvider(
		metric.WithReader(metric.NewPeriodicReader(exporter)),
		metric.WithResource(res),
	)
	otel.SetMeterProvider(mp)

	return mp.Shutdown, nil
}
