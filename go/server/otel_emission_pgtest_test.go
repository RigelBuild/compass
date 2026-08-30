//go:build pgtest && unix

package server

// T4b server-emission integration: the OTel wiring T2 adds to the shipped
// socket door and the network-door CORS policy, exercised end-to-end.
//
// The emission path is only observable through a REAL provider: otelconnect and
// NewTraceResponseInterceptor both read the GLOBAL tracer provider (otelconnect
// interceptor.go:56 otel.GetTracerProvider), so a test that would prove a span
// carries compass.message.id and the response carries a matching traceresponse
// header must install a global SDK provider with an in-memory exporter, then
// drive PostMessage over the production socket door (Serve). resetGlobals-style
// save/restore keeps the installed globals from leaking into sibling tests.
//
// Store-gated (Serve opens the store of record and PostMessage's D9 membership
// gate is store-enforced), so behind `//go:build pgtest && unix` via the shared
// pgtest harness (pgtest.RequireDSN → an isolated-schema DSN, or t.Skip when no
// runtime). Hermetic: t.TempDir() socket + state paths, no fixed ports.
//
// context.Background() is the test root (rule://go-thread-context _test.go
// exemption): threaded into store.Open, the pre-seed, and every RPC below.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"connectrpc.com/connect"
	"connectrpc.com/otelconnect"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/RigelBuild/compass/go/events"
	compassv1 "github.com/RigelBuild/compass/go/gen/compass/v1"
	"github.com/RigelBuild/compass/go/gen/compass/v1/compassv1connect"
	"github.com/RigelBuild/compass/go/internal/comms"
	"github.com/RigelBuild/compass/go/internal/pgtest"
	"github.com/RigelBuild/compass/go/internal/store"
)

// messageIDAttr is the span attribute PostMessage/RespondToAsk stamp the
// appended message's id onto (comms.go), and traceResponseHdr is the W3C
// trace-response header the interceptor sets (otel.NewTraceResponseInterceptor).
const (
	messageIDAttr     = "compass.message.id"
	traceResponseHdr  = "traceresponse"
	exposeHeadersHdr  = "Access-Control-Expose-Headers"
	corsOriginForTest = "https://app.example.com"
)

// installGlobalSpanExporter installs a global SDK TracerProvider feeding an
// in-memory exporter and a W3C propagator (what otelconnect and the trace-
// response interceptor read), restoring the prior globals on cleanup so an
// enabled-path test never leaks its provider into a sibling. It mirrors the
// enabled path SetupTracerProvider installs, but with a SyncSpanProcessor onto
// an InMemoryExporter so the test reads spans without a collector or a flush
// race. Returns the exporter the assertions read.
func installGlobalSpanExporter(t *testing.T) *tracetest.InMemoryExporter {
	t.Helper()
	prevTP := otel.GetTracerProvider()
	prevProp := otel.GetTextMapPropagator()
	exp := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exp))
	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(propagation.TraceContext{})
	t.Cleanup(func() {
		_ = tp.Shutdown(context.Background()) // best-effort flush; sync exporter already holds the spans, error not actionable in test
		otel.SetTracerProvider(prevTP)
		otel.SetTextMapPropagator(prevProp)
	})
	return exp
}

// seedAdminChannel opens the store, finds-or-creates the bootstrap admin (the
// account the socket door attributes every RPC to), and creates one OPEN channel
// the admin is a member of — so PostMessage's D9 membership gate admits a post
// over the socket. It returns the DSN (so Serve opens the SAME isolated schema)
// and the channel id. Serve's own BootstrapAdmin later finds this same admin.
func seedAdminChannel(t *testing.T, ctx context.Context) (dsn string, channelID string) {
	t.Helper()
	dsn = pgtest.RequireDSN(t)
	st, err := store.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store Open: %v", err)
	}
	t.Cleanup(st.Close)
	admin, err := st.BootstrapAdmin(ctx, store.NewUser{Handle: bootstrapAdminHandle, DisplayName: bootstrapAdminDisplayName})
	if err != nil {
		t.Fatalf("BootstrapAdmin: %v", err)
	}
	ch, err := st.CreateChannel(ctx, admin.ID, store.NewChannel{
		Name: "otel-emission", Kind: store.ChannelKindChannel,
		MemberAccountIDs: []store.AccountID{admin.ID},
	})
	if err != nil {
		t.Fatalf("CreateChannel: %v", err)
	}
	return dsn, string(ch.ID)
}

// postOverSocket posts one text message to channelID over the shipped socket
// door and returns the connect response (whose header carries traceresponse if
// the interceptor set one) and the created message id.
func postOverSocket(t *testing.T, ctx context.Context, socketPath, channelID string) (*connect.Response[compassv1.PostMessageResponse], string) {
	t.Helper()
	client := newUDSCommsClient(t, socketPath)
	resp, err := client.PostMessage(ctx, connect.NewRequest(&compassv1.PostMessageRequest{Container: &compassv1.PostMessageRequest_ChannelId{ChannelId: channelID}, Topic: &compassv1.PostMessageRequest_TopicName{TopicName: "general"}, CreateTopic: true, Blocks: []*compassv1.MessageBlock{{Block: &compassv1.MessageBlock_Text{Text: "hello otel"}}}}))
	if err != nil {
		t.Fatalf("PostMessage over socket: %v", err)
	}
	return resp, resp.Msg.GetMessage().GetId()
}

// TestServerEmissionSocketDoorTracesPostMessage is the enabled-path emission
// contract on the SHIPPED socket door: a PostMessage RPC produces a server span
// carrying the compass.message.id of the appended message, and the response
// carries a traceresponse header whose trace id equals that span's trace id.
// Drives Serve (the production socket door with the otelconnect + trace-response
// interceptors T2 mounts) against a real store, with a global in-memory span
// exporter installed so the span is observable.
func TestServerEmissionSocketDoorTracesPostMessage(t *testing.T) {
	ctx := context.Background() // test root (rule://go-thread-context _test.go exemption)
	exp := installGlobalSpanExporter(t)
	dsn, channelID := seedAdminChannel(t, ctx)

	socketPath := serveOTelSocket(t, "otel-emission-test", dsn)

	resp, msgID := postOverSocket(t, ctx, socketPath, channelID)
	if msgID == "" {
		t.Fatal("PostMessage returned an empty message id")
	}

	// (a) exactly one server span carries the appended message's id.
	span := spanWithMessageID(t, exp.GetSpans(), msgID)

	// (b) the traceresponse header's trace id equals that span's trace id.
	hdr := resp.Header().Get(traceResponseHdr)
	if hdr == "" {
		t.Fatal("response carried no traceresponse header on the enabled path")
	}
	wantTraceID := span.SpanContext.TraceID().String()
	// grammar: 00-<32hex traceid>-<16hex spanid>-<2hex flags>
	if !w3cTraceResponse.MatchString(hdr) {
		t.Fatalf("traceresponse %q is not W3C 00-… grammar", hdr)
	}
	if gotTraceID := hdr[3:35]; gotTraceID != wantTraceID {
		t.Fatalf("traceresponse trace id = %q, want the span's trace id %q (header %q)", gotTraceID, wantTraceID, hdr)
	}
}

// TestServerEmissionDisabledSetsNoTraceResponseHeader is the disabled-path
// contract: with NO global provider installed (the shipped default when
// OTEL_EXPORTER_OTLP_ENDPOINT is unset), a PostMessage over the socket door sets
// NO traceresponse header — otelconnect falls back to the global no-op provider,
// so no span is ever recorded and the interceptor finds no valid span context to
// stamp. (Span absence is proven transitively: no header can be stamped without a
// recording span; a direct in-memory-exporter check would be vacuous, since no
// code path routes spans into a non-global provider.)
func TestServerEmissionDisabledSetsNoTraceResponseHeader(t *testing.T) {
	ctx := context.Background() // test root (rule://go-thread-context _test.go exemption)
	// Save/restore globals but install NO SDK provider: the shipped disabled
	// state. otelconnect and the trace-response interceptor both read the global
	// tracer provider, so with only the default no-op global in place the door can
	// record no span and the interceptor finds no valid span context to stamp.
	prevTP := otel.GetTracerProvider()
	prevProp := otel.GetTextMapPropagator()
	t.Cleanup(func() {
		otel.SetTracerProvider(prevTP)
		otel.SetTextMapPropagator(prevProp)
	})

	dsn, channelID := seedAdminChannel(t, ctx)
	socketPath := serveOTelSocket(t, "otel-disabled-test", dsn)

	resp, _ := postOverSocket(t, ctx, socketPath, channelID)

	// The real disabled-path proof: no valid span context ⇒ no traceresponse
	// header. (An in-memory exporter cannot prove absence here — no code path
	// routes spans into a non-global provider, so its emptiness is vacuous.)
	if hdr := resp.Header().Get(traceResponseHdr); hdr != "" {
		t.Fatalf("disabled path set traceresponse = %q, want none", hdr)
	}
}

// TestNetworkDoorExposesTraceResponseHeader pins the CORS half of T2: the
// network door's Access-Control-Expose-Headers must include traceresponse so a
// cross-origin browser can READ the header the trace-response interceptor sets.
// Drives the production buildNetworkServer (via buildDoorHandler's sibling shape)
// and inspects the actual-request CORS response, where rs/cors emits
// Access-Control-Expose-Headers (not on the preflight).
func TestNetworkDoorExposesTraceResponseHeader(t *testing.T) {
	ctx := context.Background() // test root (rule://go-thread-context _test.go exemption)
	st, admin, _ := newNetworkStore(t)

	bus := events.NewBus[busPayload]()
	t.Cleanup(bus.Close)
	svc := newService("otel-cors-test", bus, st, nil, nil, nil, nil)
	commsBus := events.NewBus[*compassv1.SubscribeCommsResponse]()
	t.Cleanup(commsBus.Close)
	commsSvc := comms.NewComms(st, commsBus, admin)
	secretsSvc := newSecretsService(st, nil, nil)
	otelIC, err := otelconnect.NewInterceptor()
	if err != nil {
		t.Fatalf("otelconnect.NewInterceptor: %v", err)
	}
	srv, err := buildNetworkServer(ctx, ServeConfig{
		StateDir:          t.TempDir(),
		CORSAllowedOrigin: corsOriginForTest,
	}, svc, commsSvc, secretsSvc, nil, st, admin, nil, nil, otelIC, nil, nil, nil)
	if err != nil {
		t.Fatalf("buildNetworkServer: %v", err)
	}

	// An actual (non-preflight) cross-origin GET carries the CORS
	// Access-Control-Expose-Headers response header; rs/cors joins the exposed
	// set into one comma-separated value.
	req := httptest.NewRequest(http.MethodGet, compassv1connect.CompassServiceGetServerInfoProcedure, nil)
	req.Header.Set("Origin", corsOriginForTest)
	rec := httptest.NewRecorder()
	srv.Handler.ServeHTTP(rec, req)

	exposed := rec.Result().Header.Get(exposeHeadersHdr)
	if exposed == "" {
		t.Fatalf("%s absent on a cross-origin actual request; the network door exposes no headers", exposeHeadersHdr)
	}
	if !headerListContains(exposed, traceResponseHdr) {
		t.Fatalf("%s = %q, want it to include %q so a browser can read the trace-response header", exposeHeadersHdr, exposed, traceResponseHdr)
	}
}

// serveOTelSocket starts a production Serve on a fresh t.TempDir() socket with
// the given version + DSN, waits for the socket to bind, and returns its path.
// Serve is driven on a background goroutine bounded by a cancel deferred via
// t.Cleanup so the server drains at test end. It gates on a served RPC
// (waitListening + the caller's first PostMessage) rather than a sleep.
func serveOTelSocket(t *testing.T, version, dsn string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "compass.sock")
	ctx, cancel := context.WithCancel(context.Background()) // test root (rule://go-thread-context _test.go exemption)
	errCh := make(chan error, 1)
	go func() {
		errCh <- Serve(ctx, ServeConfig{SocketPath: path, Version: version, DatabaseDSN: dsn})
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-errCh: // Serve returns nil on a clean ctx-cancel drain; a fault is covered by the serve_pgtest tests
		case <-timeAfter():
			t.Fatal("Serve did not return after ctx cancel")
		}
	})
	waitListening(t, path)
	return path
}

// spanWithMessageID returns the single exported span carrying the given
// compass.message.id attribute, failing if zero or more than one match — the
// PostMessage origin span is unique per post, so a duplicate means the id
// leaked onto an unrelated span.
func spanWithMessageID(t *testing.T, spans tracetest.SpanStubs, msgID string) tracetest.SpanStub {
	t.Helper()
	var match []tracetest.SpanStub
	for _, s := range spans {
		for _, a := range s.Attributes {
			if string(a.Key) == messageIDAttr && a.Value.AsString() == msgID {
				match = append(match, s)
			}
		}
	}
	if len(match) != 1 {
		t.Fatalf("found %d spans carrying %s=%q, want exactly 1 (spans exported: %d)", len(match), messageIDAttr, msgID, len(spans))
	}
	return match[0]
}

// headerListContains reports whether a comma-separated header value (as rs/cors
// joins Access-Control-Expose-Headers) contains target, case-insensitively.
func headerListContains(list, target string) bool {
	for _, part := range strings.Split(list, ",") {
		if strings.EqualFold(strings.TrimSpace(part), target) {
			return true
		}
	}
	return false
}

// w3cTraceResponse matches the traceresponse header grammar the interceptor
// emits: 00-<32hex traceid>-<16hex spanid>-<2hex flags>.
var w3cTraceResponse = regexp.MustCompile(`^00-[0-9a-f]{32}-[0-9a-f]{16}-[0-9a-f]{2}$`)
