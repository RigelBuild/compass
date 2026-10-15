//go:build unix

package gateway

import (
	"context"
	"testing"

	"connectrpc.com/connect"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/trace"

	compassv1internal "github.com/RigelBuild/compass/go/internal/gen/compass/v1"
)

// ctxRecordingRelay records the ctx each shared stream opens on, then delegates.
type ctxRecordingRelay struct {
	inner  EventRelay
	opened chan context.Context
}

func (r ctxRecordingRelay) PublishEvents(ctx context.Context) *connect.ClientStreamForClient[compassv1internal.PublishEventsRequest, compassv1internal.PublishEventsResponse] {
	r.opened <- ctx
	return r.inner.PublishEvents(ctx)
}

func TestServeSharedPublisherCarriesNoCallerSpan(t *testing.T) {
	installGatewaySpanRecorder(t) // a recording provider makes the Provision span real
	relay := ctxRecordingRelay{inner: newSpanRelayClient(t), opened: make(chan context.Context, 1)}

	provisionCtx, provision := otel.Tracer("test").Start(t.Context(), "provision")
	listener, err := Serve(provisionCtx, socketPath(t), "cont-1", Deps{
		Sessions: staticSessions{sessionID: "sess-1", ok: true},
		Events:   relay,
	})
	provision.End()
	if err != nil {
		t.Fatalf("Serve: %v", err)
	}
	closeGatewaySocket(t, listener)

	pub := listener.gateway.acquirePublisher("sess-1")
	t.Cleanup(func() {
		if err := pub.close(); err != nil {
			t.Logf("closing publisher: %v", err) // the relay leaves PublishEvents unimplemented
		}
	})
	opened := <-relay.opened
	if sc := trace.SpanContextFromContext(opened); sc.IsValid() {
		t.Fatalf("shared publisher ctx holds span %s, want none (the Provision span %s must not outlive its call)",
			sc.SpanID(), provision.SpanContext().SpanID())
	}
}
