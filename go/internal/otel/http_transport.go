package otel

import (
	"net/http"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel/propagation"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
	"go.opentelemetry.io/otel/trace"
)

// HTTPTransport adds a client span, redacts query values, and injects no headers.
func HTTPTransport(base http.RoundTripper) http.RoundTripper {
	if base == nil {
		base = http.DefaultTransport
	}
	return otelhttp.NewTransport(redactURL{base: base},
		otelhttp.WithPropagators(propagation.NewCompositeTextMapPropagator()),
	)
}

// redactURL re-sets url.full after otelhttp so query values never reach a span.
type redactURL struct {
	base http.RoundTripper
}

func (rt redactURL) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := rt.base.RoundTrip(req)
	redacted := *req.URL
	redacted.User = nil // otelhttp strips userinfo too; keep that when overwriting url.full
	query := redacted.Query()
	for key, values := range query {
		for i := range values {
			values[i] = "REDACTED"
		}
		query[key] = values
	}
	redacted.RawQuery = query.Encode()
	trace.SpanFromContext(req.Context()).SetAttributes(semconv.URLFull(redacted.String()))
	return resp, err
}
