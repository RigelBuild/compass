//go:build pgtest && unix

package server

// T4b server-emission integration: the OTel wiring T2 adds to the shipped socket
// door and the network-door CORS, end-to-end. The emission path is observable
// only through a REAL global provider (otelconnect reads otel.GetTracerProvider),
// so each test installs one + an in-memory exporter and drives PostMessage.

import (
	"connectrpc.com/connect/v2"
	"connectrpc.com/otelconnect"
	"context"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/RigelBuild/compass/go/events"
	compassv1 "github.com/RigelBuild/compass/go/gen/compass/v1"
	"github.com/RigelBuild/compass/go/gen/compass/v1/compassv1connect"
	"github.com/RigelBuild/compass/go/internal/comms"
	compassotel "github.com/RigelBuild/compass/go/internal/otel"
	"github.com/RigelBuild/compass/go/internal/pgtest"
	"github.com/RigelBuild/compass/go/internal/store"
	"github.com/RigelBuild/compass/go/internal/usage"
)

// messageIDAttr is the span attribute PostMessage/RespondToAsk stamp the
// appended message's id onto (comms.go), and traceResponseHdr is the W3C
// trace-response header the interceptor sets (otel.NewTraceResponseInterceptor).
const (
	messageIDAttr     = "compass.message.id"
	traceResponseHdr  = "Traceresponse"
	exposeHeadersHdr  = "Access-Control-Expose-Headers"
	allowHeadersHdr   = "Access-Control-Allow-Headers"
	corsOriginForTest = "https://app.example.com"
)

// Installs a synchronous in-memory exporter and W3C propagator, restoring
// global state on cleanup.
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

// Creates an open channel for the bootstrap admin, enabling attributed socket
// RPCs and member posts.
func seedAdminChannel(t *testing.T, ctx context.Context) (dsn string, channelID string) {
	t.Helper()
	dsn = pgtest.RequireDSN(t) //nolint:contextcheck // DSN acquisition roots its own setup context; it takes no ctx
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
// door and returns the response, captured response headers, and created message id.
func postOverSocket(t *testing.T, ctx context.Context, socketPath, channelID string) (*connect.Header, string) {
	t.Helper()
	client := newUDSCommsClient(t, socketPath)
	ctx, info := connect.NewClientContext(ctx)
	resp, err := client.PostMessage(ctx, &compassv1.PostMessageRequest{Container: &compassv1.PostMessageRequest_ChannelId{ChannelId: channelID}, Topic: &compassv1.PostMessageRequest_TopicName{TopicName: "general"}, CreateTopic: true, Blocks: []*compassv1.MessageBlock{{Block: &compassv1.MessageBlock_Text{Text: "hello otel"}}}})
	if err != nil {
		t.Fatalf("PostMessage over socket: %v", err)
	}
	return info.ResponseHeader(), resp.GetMessage().GetId()
}

// A PostMessage over Serve's socket door emits a span with the appended message
// ID and a traceresponse header carrying the same trace ID. The global in-memory
// exporter makes the production interceptors' span observable.
func TestServerEmissionSocketDoorTracesPostMessage(t *testing.T) {
	ctx := context.Background() // test root (rule://go-thread-context _test.go exemption)
	exp := installGlobalSpanExporter(t)
	dsn, channelID := seedAdminChannel(t, ctx)

	socketPath := serveOTelSocket(t, "otel-emission-test", dsn)

	responseHeader, msgID := postOverSocket(t, ctx, socketPath, channelID)
	if msgID == "" {
		t.Fatal("PostMessage returned an empty message id")
	}

	// (a) exactly one server span carries the appended message's id.
	span := spanWithMessageID(t, exp.GetSpans(), msgID)

	// (b) the traceresponse header's trace id equals that span's trace id.
	hdr := responseHeader.Get(traceResponseHdr)
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

// With the default no-op provider, PostMessage must set no traceresponse header.
// Without a recording span the interceptor has no valid context to stamp; a
// separate empty exporter would not prove span absence.
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

	responseHeader, _ := postOverSocket(t, ctx, socketPath, channelID)

	// The real disabled-path proof: no valid span context ⇒ no traceresponse
	// header. (An in-memory exporter cannot prove absence here — no code path
	// routes spans into a non-global provider, so its emptiness is vacuous.)
	if hdr := responseHeader.Get(traceResponseHdr); hdr != "" {
		t.Fatalf("disabled path set traceresponse = %q, want none", hdr)
	}
}

// The network door must expose traceresponse to cross-origin browsers. Inspect
// the actual CORS request because Access-Control-Expose-Headers is not on preflight.
func TestNetworkDoorExposesTraceResponseHeader(t *testing.T) {
	ctx := context.Background() // test root (rule://go-thread-context _test.go exemption)
	st, admin, _ := newNetworkStore(t)

	bus := events.NewBus[busPayload]()
	t.Cleanup(bus.Close)
	svc := newService("otel-cors-test", bus, st, nil, nil, nil, nil)
	commsBus := events.NewBus[*compassv1.SubscribeCommsResponse]()
	t.Cleanup(commsBus.Close)
	commsSvc := comms.NewComms(ctx, st, commsBus, nil, admin)
	secretsSvc := newSecretsService(st, nil, nil, nil)
	otelIC, err := otelconnect.NewServerInterceptor()
	if err != nil {
		t.Fatalf("otelconnect.NewServerInterceptor: %v", err)
	}
	srv, err := buildNetworkServer(
		ctx,
		ServeConfig{
			StateDir:          t.TempDir(),
			CORSAllowedOrigin: corsOriginForTest,
		},
		svc,
		commsSvc,
		secretsSvc,
		newUsageService(usage.NewPostgres(st), st),
		compassv1connect.UnimplementedAgentRepositoryServiceHandler{},
		nil,
		st,
		admin,
		nil,
		nil,
		otelIC,
		nil,
		nil,
		nil,
		nil,
		nil,
		gatewayServices{},
		newServerHealth(nil),
		newStreamDrain(),
	)
	if err != nil {
		t.Fatalf("buildNetworkServer: %v", err)
	}

	// An actual (non-preflight) cross-origin GET carries the CORS
	// Access-Control-Expose-Headers response header; rs/cors joins the exposed
	// set into one comma-separated value.
	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, compassv1connect.CompassServiceGetServerInfoProcedure, nil)
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

// A real browser needs the session header in Access-Control-Allow-Headers or its
// preflight blocks the request. The span test calls the handler directly, so this
// checks the separate CORS gate.
func TestNetworkDoorAllowsPostHogSessionRequestHeader(t *testing.T) {
	ctx := context.Background() // test root (rule://go-thread-context _test.go exemption)
	st, admin, _ := newNetworkStore(t)

	bus := events.NewBus[busPayload]()
	t.Cleanup(bus.Close)
	svc := newService("otel-cors-session-test", bus, st, nil, nil, nil, nil)
	commsBus := events.NewBus[*compassv1.SubscribeCommsResponse]()
	t.Cleanup(commsBus.Close)
	commsSvc := comms.NewComms(ctx, st, commsBus, nil, admin)
	secretsSvc := newSecretsService(st, nil, nil, nil)
	otelIC, err := otelconnect.NewServerInterceptor()
	if err != nil {
		t.Fatalf("otelconnect.NewServerInterceptor: %v", err)
	}
	srv, err := buildNetworkServer(
		ctx,
		ServeConfig{
			StateDir:          t.TempDir(),
			CORSAllowedOrigin: corsOriginForTest,
		},
		svc,
		commsSvc,
		secretsSvc,
		newUsageService(usage.NewPostgres(st), st),
		compassv1connect.UnimplementedAgentRepositoryServiceHandler{},
		nil,
		st,
		admin,
		nil,
		nil,
		otelIC,
		nil,
		nil,
		nil,
		nil,
		nil,
		gatewayServices{},
		newServerHealth(nil),
		newStreamDrain(),
	)
	if err != nil {
		t.Fatalf("buildNetworkServer: %v", err)
	}

	// preflight sends an OPTIONS with the given requested headers and returns
	// the Allow-Headers response value. rs/cors matches its allow-list against a
	// sorted, lowercased set, so the request list is built that way or the
	// comparison silently fails to discriminate.
	preflight := func(t *testing.T, requested string) (string, string) {
		t.Helper()
		req := httptest.NewRequestWithContext(t.Context(), http.MethodOptions, compassv1connect.CompassServiceGetServerInfoProcedure, nil)
		req.Header.Set("Origin", corsOriginForTest)
		req.Header.Set("Access-Control-Request-Method", http.MethodPost)
		req.Header.Set("Access-Control-Request-Headers", requested)
		rec := httptest.NewRecorder()
		srv.Handler.ServeHTTP(rec, req)
		res := rec.Result()
		return res.Header.Get(allowHeadersHdr), res.Header.Get("Access-Control-Allow-Origin")
	}

	// Baseline: the headers the UI already sends must pass, or a failure below
	// would prove nothing about the session header specifically.
	const baseline = "authorization,connect-protocol-version,content-type"
	if _, origin := preflight(t, baseline); origin == "" {
		t.Fatal("baseline preflight was rejected; this test cannot discriminate the session header")
	}

	requested := baseline + "," + strings.ToLower(compassotel.PostHogSessionHeader)
	allowed, origin := preflight(t, requested)
	if origin == "" {
		t.Fatalf("preflight REJECTED with the session header requested (%s = %q): a browser would block the whole request, so the UI can never send the J1 key", allowHeadersHdr, allowed)
	}
	if !headerListContains(allowed, compassotel.PostHogSessionHeader) {
		t.Fatalf("%s = %q, want it to include %q so a browser may send the J1 session-id key", allowHeadersHdr, allowed, compassotel.PostHogSessionHeader)
	}
}

// A rejected RPC through the production chain must still stamp session.id on its
// exported span, proving the session interceptor is mounted before auth. Direct
// unit tests cannot detect an unmounted interceptor.
func TestNetworkDoorStampsPostHogSessionIDOnTheSpan(t *testing.T) {
	ctx := context.Background() // test root (rule://go-thread-context _test.go exemption)
	st, admin, _ := newNetworkStore(t)
	exp := installGlobalSpanExporter(t)

	bus := events.NewBus[busPayload]()
	t.Cleanup(bus.Close)
	svc := newService("otel-session-test", bus, st, nil, nil, nil, nil)
	commsBus := events.NewBus[*compassv1.SubscribeCommsResponse]()
	t.Cleanup(commsBus.Close)
	commsSvc := comms.NewComms(t.Context(), st, commsBus, nil, admin)
	secretsSvc := newSecretsService(st, nil, nil, nil)
	otelIC, err := otelconnect.NewServerInterceptor()
	if err != nil {
		t.Fatalf("otelconnect.NewServerInterceptor: %v", err)
	}
	srv, err := buildNetworkServer(
		ctx,
		ServeConfig{StateDir: t.TempDir()},
		svc,
		commsSvc,
		secretsSvc,
		newUsageService(usage.NewPostgres(st), st),
		compassv1connect.UnimplementedAgentRepositoryServiceHandler{},
		nil,
		st,
		admin,
		nil,
		nil,
		otelIC,
		nil,
		nil,
		nil,
		nil,
		nil,
		gatewayServices{},
		newServerHealth(nil),
		newStreamDrain(),
	)
	if err != nil {
		t.Fatalf("buildNetworkServer: %v", err)
	}

	// This unauthenticated request still must stamp the session ID on its span.
	const wantSession = "0198f2c1-7b3a-7000-8b1e-2f9d4c5a6e70"
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, compassv1connect.CompassServiceGetServerInfoProcedure, strings.NewReader("{}"))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Posthog-Session-Id", wantSession)
	rec := httptest.NewRecorder()
	srv.Handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("GetServerInfo = %d %q, want 401: this test asserts the key survives an auth rejection, so a different code means it is measuring something else", rec.Code, rec.Body.String())
	}

	var got []string
	for _, span := range exp.GetSpans() {
		for _, attr := range span.Attributes {
			if attr.Key == "session.id" {
				got = append(got, attr.Value.AsString())
			}
		}
	}
	if len(got) == 0 {
		t.Fatal("no span carries session.id: the session-id interceptor is not mounted on the network door, so a PostHog funnel cannot pivot to the backend trace")
	}
	for _, id := range got {
		if id != wantSession {
			t.Fatalf("session.id = %q, want %q", id, wantSession)
		}
	}
}

// Starts Serve on a temporary socket and cancels it during test cleanup.
func serveOTelSocket(t *testing.T, version, dsn string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "compass.sock")
	ctx, cancel := context.WithCancel(context.Background()) // test root (rule://go-thread-context _test.go exemption)
	errCh := make(chan error, 1)
	cfg := ServeConfig{SocketPath: path, Version: version, DatabaseDSN: dsn}
	provisionMasterKeyProvider(t, &cfg)
	provisionNats(t, &cfg)
	go func() {
		errCh <- Serve(ctx, cfg)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-errCh: // Serve returns nil on clean cancel; fault paths are covered separately
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
	for part := range strings.SplitSeq(list, ",") {
		if strings.EqualFold(strings.TrimSpace(part), target) {
			return true
		}
	}
	return false
}

// w3cTraceResponse matches the traceresponse header grammar the interceptor
// emits: 00-<32hex traceid>-<16hex spanid>-<2hex flags>.
var w3cTraceResponse = regexp.MustCompile(`^00-[0-9a-f]{32}-[0-9a-f]{16}-[0-9a-f]{2}$`)
