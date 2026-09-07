//go:build pgtest && unix

package server

// T6 of the frozen record docs/designs/observability/compass-server-runner-otel/
// design.md: the end-to-end proof of the ratified ONE TURN, ONE TRACE goal, over
// the SHIPPED comms door and the REAL delivery spine.
//
// The composition. Two halves already exist in this package and this file is
// where they meet:
//
//   - The emission half (otel_emission_pgtest_test.go): installGlobalSpanExporter
//     installs a global SDK TracerProvider with a SyncSpanProcessor onto an
//     InMemoryExporter plus a W3C propagator. This is the ONLY way the emission
//     path is observable — otelconnect and NewTraceResponseInterceptor both read
//     the GLOBAL tracer provider, and otelconnect captures it ONCE at
//     NewInterceptor(). So the exporter MUST be installed BEFORE any door or wire
//     is built; every fixture below does that first, and the ordering is
//     load-bearing, not stylistic.
//   - The spine half (offline_mention_e2e_pgtest_test.go): newMentionE2EWire
//     stands up a real store, a real runnerhub.Hub with a recordingRunner door,
//     a real comms service on a fresh comms bus, and the delivery consumer wired
//     exactly as production's startDeliveryConsumer does (real resume waker).
//
// Why the door here is mounted rather than driven through Serve. serveOTelSocket
// starts a Serve with SocketPath only — no Listen — and Serve mounts the
// RunnerService door ONLY on the network door (serve.go:493-495,
// network_door.go:344). So that Serve's hub has no enrolled Runner, and its
// comms bus / consumer / hub are its OWN instances: a post over that socket
// never reaches this wire's recordingRunner, because the two share only the
// database, never the in-process bus. Pointing serveOTelSocket at the wire's DSN
// therefore cannot compose the two halves. What DOES compose them is mounting
// the CommsService handler over THIS wire's comms service behind the production
// socket-door interceptor chain (serve.go:698-699,709-710: otelconnect first,
// then the trace-response interceptor, then the ambient-identity pair) — the
// same shape newH2CTestServerWithInterceptors gives CompassService. That yields
// a real otelconnect handler span, a real traceresponse header, and the wire's
// real bus -> consumer -> hub -> recordingRunner spine.
//
// The ambient-identity parameter is what lets an assertion post AS a given
// account: the shipped socket door parameterizes exactly this interceptor with
// the bootstrap admin, so a door parameterized with an agent's account id is the
// production chain, not a bespoke one — and it is the only way to get an
// agent-authored post that sits under a real handler span (the hold-edge case).
//
// No flush race by construction: the exporter runs a SyncSpanProcessor, so a
// span is in exp.GetSpans() the moment it ends. Every wait is event-gated on an
// observed wire fact via the spine file's waitFor* helpers — never a sleep,
// never a retry loop, never an invented deadline.
//
// Spans are selected by NAME + SERVER kind, never by message id alone: the
// delivery hop span stamps the SAME compass.message.id as the origin handler
// span (delivery/dispatch.go:375), so the sibling file's spanWithMessageID
// ("exactly one match") is correct only where nothing dispatches — which is true
// for its own Serve-with-no-Runner fixture and false for every fixture here.
//
// context.Background() is the test root (rule://go-thread-context _test.go
// exemption): threaded into the wire, the doors, and every RPC below.
//
// (h) and (i) are BLOCKED on production code that is not on main; each is a
// t.Skip naming the exact missing symbol. See their subtests.

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"

	"connectrpc.com/connect"
	"connectrpc.com/otelconnect"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/noop"

	compassv1 "github.com/RigelBuild/compass/go/gen/compass/v1"
	"github.com/RigelBuild/compass/go/gen/compass/v1/compassv1connect"
	"github.com/RigelBuild/compass/go/internal/auth"
	"github.com/RigelBuild/compass/go/internal/comms"
	otelx "github.com/RigelBuild/compass/go/internal/otel"
	"github.com/RigelBuild/compass/go/internal/store"
)

// dispatchedMetricName is the op-kind delivery counter T5 increments at
// gatedDispatch, and opKindAttr is the ONLY attribute it is ever labelled with
// (the §Global Constraints cardinality rule).
const (
	dispatchedMetricName = "compass.delivery.dispatched"
	opKindAttr           = "compass.op.kind"
)

// --- fixtures ----------------------------------------------------------------

// serveTracedCommsDoor mounts the CommsService over commsSvc behind the
// PRODUCTION socket-door interceptor chain (serve.go:698-699,709-710) on an h2c
// httptest server, attributing every RPC to actor via the ambient-identity pair
// — so a post over this door is authored by actor and runs under a real
// otelconnect handler span whose trace id the response's traceresponse header
// carries. Returns the base URL; torn down via t.Cleanup.
//
// MUST be called only AFTER installGlobalSpanExporter: otelconnect captures the
// global tracer provider and propagator once, at NewInterceptor().
func serveTracedCommsDoor(t *testing.T, commsSvc *comms.Comms, actor store.AccountID) string {
	t.Helper()
	otelIC, err := otelconnect.NewInterceptor()
	if err != nil {
		t.Fatalf("otelconnect.NewInterceptor: %v", err)
	}
	path, handler := compassv1connect.NewCommsServiceHandler(commsSvc,
		connect.WithInterceptors(otelIC, otelx.NewTraceResponseInterceptor(),
			auth.AmbientIdentity(actor), auth.AmbientStreamInterceptor(actor)))
	mux := http.NewServeMux()
	mux.Handle(path, handler)
	srv := httptest.NewUnstartedServer(mux)
	srv.Config.Protocols = cleartextHTTP2()
	srv.Start()
	t.Cleanup(srv.Close)
	return srv.URL
}

// newTracedCommsClient builds a CommsService connect client speaking h2c
// prior-knowledge to baseURL, mirroring newSecretsH2CClient. Idle conns are
// closed via t.Cleanup.
func newTracedCommsClient(t *testing.T, baseURL string) compassv1connect.CommsServiceClient {
	t.Helper()
	tr := h2cTransport(func(ctx context.Context, network, addr string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, network, addr)
	})
	t.Cleanup(tr.CloseIdleConnections)
	return compassv1connect.NewCommsServiceClient(&http.Client{Transport: tr}, baseURL)
}

// postOverTracedDoor posts one text message into the wire's shared channel over
// the traced door and returns the connect response (whose header carries
// traceresponse) plus the created message id. It is postOverSocket's sibling for
// this file's mounted door and per-case body text (that helper hardcodes its
// body, so a mention case needs this one rather than a change over there).
func postOverTracedDoor(t *testing.T, ctx context.Context, client compassv1connect.CommsServiceClient, channel store.ChannelID, body string) (*connect.Response[compassv1.PostMessageResponse], string) {
	t.Helper()
	resp, err := client.PostMessage(ctx, connect.NewRequest(&compassv1.PostMessageRequest{
		Container:   &compassv1.PostMessageRequest_ChannelId{ChannelId: string(channel)},
		Topic:       &compassv1.PostMessageRequest_TopicName{TopicName: "general"},
		CreateTopic: true,
		Blocks:      []*compassv1.MessageBlock{{Block: &compassv1.MessageBlock_Text{Text: body}}},
	}))
	if err != nil {
		t.Fatalf("PostMessage(%q) over the traced door: %v", body, err)
	}
	msgID := resp.Msg.GetMessage().GetId()
	if msgID == "" {
		t.Fatalf("PostMessage(%q) returned an empty message id", body)
	}
	return resp, msgID
}

// postAsk authors an ask message as the admin in-process (no span needed — only
// the ANSWER's handler span is under test) and returns the ask message id plus
// the SERVER-MINTED ask id RespondToAsk is keyed on (a caller-supplied ask_id is
// stripped, comms_test.go:498-503).
func postAsk(t *testing.T, w *mentionE2EWire, question string) (msgID, askID string) {
	t.Helper()
	resp, err := w.comms.PostAsAccount(w.ctx, w.adminID, &compassv1.PostMessageRequest{
		Container:   &compassv1.PostMessageRequest_ChannelId{ChannelId: string(w.channel)},
		Topic:       &compassv1.PostMessageRequest_TopicName{TopicName: "general"},
		CreateTopic: true,
		Blocks: []*compassv1.MessageBlock{{Block: &compassv1.MessageBlock_Ask{Ask: &compassv1.Ask{
			Questions: []*compassv1.AskQuestion{{
				QuestionId: "q1",
				Question:   question,
				Options: []*compassv1.AskOption{
					{Id: "opt-a", Label: "staging"},
					{Id: "opt-b", Label: "prod"},
				},
			}},
		}}}},
	})
	if err != nil {
		t.Fatalf("PostAsAccount(ask): %v", err)
	}
	msgID = resp.GetMessage().GetId()
	for _, b := range resp.GetMessage().GetBlocks() {
		if a := b.GetAsk(); a != nil {
			askID = a.GetAskId()
		}
	}
	if msgID == "" || askID == "" {
		t.Fatalf("ask post returned msgID=%q askID=%q, want both non-empty (the server mints the ask id)", msgID, askID)
	}
	return msgID, askID
}

// bringSessionLive drives the REAL hub Provision->Start pair so account's
// container->session binding is promoted (promoteSession) and SessionForAccount
// resolves — the precondition for the author-hold arm to hold and for a live
// fan-out to reach a recipient. Mirrors the forge e2e's goLive, using the
// runner's FIFO overrides so distinct accounts get distinct containers/sessions.
//
// Called BEFORE any post in a fixture: doing it after would contend with the
// wake path's own Start for the same FIFO entry.
func bringSessionLive(t *testing.T, w *mentionE2EWire, account store.AccountID, container, session string) {
	t.Helper()
	w.runner.setContainerNames(container)
	w.runner.setStartIDs(session)
	presp, _, err := w.hub.Provision(w.ctx, "", &compassv1.ProvisionAgentWorkspaceRequest{AgentHandle: string(account)})
	if err != nil {
		t.Fatalf("Provision(%s): %v", account, err)
	}
	sresp, err := w.hub.Start(w.ctx, "", &compassv1.StartAgentSessionRequest{ContainerName: presp.GetContainerName()})
	if err != nil {
		t.Fatalf("Start(%s): %v", account, err)
	}
	if got := sresp.GetSessionId(); got != session {
		t.Fatalf("Start session id = %q, want %q", got, session)
	}
	if got, ok := w.hub.SessionForAccount(account); !ok || got != session {
		t.Fatalf("SessionForAccount(%s) = (%q, %v), want (%q, true) — the hold/fan-out arms resolve through this binding", account, got, ok, session)
	}
}

// installGlobalMetricReader installs a global SDK MeterProvider over a manual
// reader, restoring the prior global on cleanup. It MUST run BEFORE the wire is
// built: T5 creates the compass.delivery.dispatched counter ONCE at
// delivery.NewConsumer from the then-global meter (consumer.go:269), so a
// provider installed afterwards records nothing. Mirrors the T5 unit test's
// setup (delivery/trace_test.go:210-221).
func installGlobalMetricReader(t *testing.T) *sdkmetric.ManualReader {
	t.Helper()
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	prev := otel.GetMeterProvider()
	otel.SetMeterProvider(mp)
	t.Cleanup(func() {
		otel.SetMeterProvider(prev)
		_ = mp.Shutdown(context.Background()) // deferred test cleanup: the manual reader already holds the collected points, so this error is not actionable
	})
	return reader
}

// --- wire-fact and span readers ----------------------------------------------

// tracedOp is one observed send-only control push on the wire, carrying the W3C
// traceparent the op was stamped with — the field the spine file's controlRecord
// deliberately omits and every assertion here reads.
type tracedOp struct {
	sessionID   string
	messageID   string
	kind        controlKind
	traceparent string
}

// tracedOps snapshots every DeliverControl the Server pushed to the recording
// Runner, with its op kind, message id, and traceparent. Same wire source and
// locking as allControlDelivers; it reads the traceparent field too.
func tracedOps(r *recordingRunner) []tracedOp {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []tracedOp
	for _, c := range r.seen {
		dc := c.GetDeliverControl()
		if dc == nil {
			continue
		}
		rec := tracedOp{sessionID: dc.GetSessionId(), kind: controlOther}
		switch op := dc.GetOp(); {
		case op.GetSteer() != nil:
			s := op.GetSteer()
			rec.kind, rec.messageID, rec.traceparent = controlSteer, s.GetMessage().GetId(), s.GetTraceparent()
		case op.GetDeliver() != nil:
			d := op.GetDeliver()
			rec.kind, rec.messageID, rec.traceparent = controlDeliver, d.GetMessage().GetId(), d.GetTraceparent()
		}
		out = append(out, rec)
	}
	return out
}

// tracedOpFor returns the single op pushed to sessionID carrying messageID,
// failing on zero or more than one match — so a duplicate dispatch (which would
// make "the" traceparent ambiguous) is a loud failure, never a silent pick.
func tracedOpFor(t *testing.T, r *recordingRunner, sessionID, messageID string) tracedOp {
	t.Helper()
	var match []tracedOp
	for _, op := range tracedOps(r) {
		if op.sessionID == sessionID && op.messageID == messageID {
			match = append(match, op)
		}
	}
	if len(match) != 1 {
		t.Fatalf("session %q saw %d ops carrying message %q, want exactly 1 (ops: %+v)", sessionID, len(match), messageID, tracedOps(r))
	}
	return match[0]
}

// traceIDOfTraceparent parses a W3C traceparent through the shipped
// otel.ContextWithTraceparent helper and returns its trace id, failing when the
// string is empty or does not parse to a VALID span context. Parsing (never
// string-slicing) is what makes the assertion a statement about the real W3C
// grammar the agent seam carries.
func traceIDOfTraceparent(t *testing.T, ctx context.Context, what, tp string) trace.TraceID {
	t.Helper()
	if tp == "" {
		t.Fatalf("%s carried an EMPTY traceparent, want a W3C 00-… value (the turn's trace never reached the agent seam)", what)
	}
	sc := trace.SpanContextFromContext(otelx.ContextWithTraceparent(ctx, tp))
	if !sc.IsValid() {
		t.Fatalf("%s traceparent %q did not parse to a valid span context", what, tp)
	}
	return sc.TraceID()
}

// originServerSpan returns the single SERVER-kind span named procedure — the
// otelconnect handler span, whose name is the procedure path with the leading
// slash trimmed (otelconnect interceptor.go:94). Selecting on name + kind rather
// than on compass.message.id is required here: the delivery hop span stamps the
// same message id (delivery/dispatch.go:375), so an id-only match is ambiguous
// the moment anything dispatches.
func originServerSpan(t *testing.T, spans tracetest.SpanStubs, procedure string) tracetest.SpanStub {
	t.Helper()
	want := procedure[1:] // trim the leading "/" the generated constant carries
	var match []tracetest.SpanStub
	for _, s := range spans {
		if s.Name == want && s.SpanKind == trace.SpanKindServer {
			match = append(match, s)
		}
	}
	if len(match) != 1 {
		t.Fatalf("found %d SERVER spans named %q, want exactly 1 (spans exported: %d)", len(match), want, len(spans))
	}
	return match[0]
}

// spanAttr returns the string value of key on s, and whether it was present.
func spanAttr(s tracetest.SpanStub, key string) (string, bool) {
	for _, a := range s.Attributes {
		if string(a.Key) == key {
			return a.Value.AsString(), true
		}
	}
	return "", false
}

// spansForMessage returns every exported span carrying compass.message.id ==
// msgID — the origin handler span plus each delivery hop span for that message,
// i.e. the recorded server hops of one turn.
func spansForMessage(spans tracetest.SpanStubs, msgID string) tracetest.SpanStubs {
	var out tracetest.SpanStubs
	for _, s := range spans {
		if got, ok := spanAttr(s, messageIDAttr); ok && got == msgID {
			out = append(out, s)
		}
	}
	return out
}

// dispatchedOpKindCounts extracts the compass.delivery.dispatched sum per op
// kind and asserts the attribute SET on every data point is EXACTLY the op-kind
// key — the load-bearing cardinality half (never a session, channel, or message
// label). The extraction idiom is the T5 unit test's dispatchedCounts
// (delivery/trace_test.go:256), which lives in package delivery and so cannot be
// called from here.
func dispatchedOpKindCounts(t *testing.T, rm *metricdata.ResourceMetrics) map[string]int64 {
	t.Helper()
	out := map[string]int64{}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != dispatchedMetricName {
				continue
			}
			sum, ok := m.Data.(metricdata.Sum[int64])
			if !ok {
				t.Fatalf("%s data = %T, want Sum[int64]", dispatchedMetricName, m.Data)
			}
			for _, dp := range sum.DataPoints {
				attrs := dp.Attributes.ToSlice()
				if len(attrs) != 1 || attrs[0].Key != attribute.Key(opKindAttr) {
					t.Fatalf("%s data point attrs = %v, want EXACTLY {%s} — a per-session/per-channel label is a cardinality hazard", dispatchedMetricName, attrs, opKindAttr)
				}
				out[attrs[0].Value.AsString()] += dp.Value
			}
		}
	}
	return out
}

// --- the T6 assertions -------------------------------------------------------

// TestTraceContinuityOneTurnOneTraceEndToEnd is T6: the record's assertions
// (a)-(j), each as its own subtest over its own fixture.
func TestTraceContinuityOneTurnOneTraceEndToEnd(t *testing.T) {
	// (a) The op captured at the gateway/agent seam parses to the SAME trace id
	// as the PostMessage handler span — the one-turn-one-trace claim itself,
	// measured on the WIRE (what the agent actually receives), not on a span.
	t.Run("a: the agent-seam op carries the PostMessage handler span's trace id", func(t *testing.T) {
		ctx := context.Background() // test root (rule://go-thread-context _test.go exemption)
		exp := installGlobalSpanExporter(t)
		w := newMentionE2EWire(t)

		recip := w.seedAgentMember(t, "seamrecip", true)
		const recipSess = "sess-seamrecip-1"
		bringSessionLive(t, w, recip.ID, containerFor("seamrecip"), recipSess)

		client := newTracedCommsClient(t, serveTracedCommsDoor(t, w.comms, w.adminID))
		resp, msgID := postOverTracedDoor(t, ctx, client, w.channel, "one turn, one trace")

		waitForControlDelivers(t, w.runner, recipSess, 1)

		origin := originServerSpan(t, exp.GetSpans(), compassv1connect.CommsServicePostMessageProcedure)
		if got, _ := spanAttr(origin, messageIDAttr); got != msgID {
			t.Fatalf("origin span %s = %q, want the appended message %q", messageIDAttr, got, msgID)
		}
		want := origin.SpanContext.TraceID()

		op := tracedOpFor(t, w.runner, recipSess, msgID)
		if op.kind != controlDeliver {
			t.Fatalf("seam op kind = %v, want a plain deliver (a subscribed, unmentioned recipient)", op.kind)
		}
		if got := traceIDOfTraceparent(t, ctx, "the agent-seam deliver op", op.traceparent); got != want {
			t.Fatalf("seam op trace id = %s, want the PostMessage handler span's %s — the turn's trace did not reach the agent", got, want)
		}
		// The response header is (c)'s subject; asserting it is set here keeps
		// this fixture's own precondition honest.
		if resp.Header().Get(traceResponseHdr) == "" {
			t.Fatal("enabled path set no traceresponse header on the post")
		}
	})

	// (b) Every recorded server hop span of the turn shares that one trace id —
	// ONE CONNECTED TRACE, not a set of correlated fragments.
	t.Run("b: every recorded server hop span shares the one trace id", func(t *testing.T) {
		ctx := context.Background() // test root (rule://go-thread-context _test.go exemption)
		exp := installGlobalSpanExporter(t)
		w := newMentionE2EWire(t)

		recip := w.seedAgentMember(t, "hoprecip", true)
		const recipSess = "sess-hoprecip-1"
		bringSessionLive(t, w, recip.ID, containerFor("hoprecip"), recipSess)

		client := newTracedCommsClient(t, serveTracedCommsDoor(t, w.comms, w.adminID))
		_, msgID := postOverTracedDoor(t, ctx, client, w.channel, "every hop, one trace")

		waitForControlDelivers(t, w.runner, recipSess, 1)

		spans := exp.GetSpans()
		want := originServerSpan(t, spans, compassv1connect.CommsServicePostMessageProcedure).SpanContext.TraceID()

		hops := spansForMessage(spans, msgID)
		// Non-vacuous by construction: the origin handler span AND at least one
		// delivery.dispatch hop span must both be present, or "they all agree"
		// would be a statement about a one-element set.
		if len(hops) < 2 {
			t.Fatalf("spans carrying %s=%q = %d, want >= 2 (the origin handler span plus at least one delivery hop)", messageIDAttr, msgID, len(hops))
		}
		for _, s := range hops {
			if got := s.SpanContext.TraceID(); got != want {
				t.Fatalf("hop span %q trace id = %s, want the turn's %s — the trace is fragmented, not connected", s.Name, got, want)
			}
		}
	})

	// (c) The traceresponse header on the PostMessage response carries the same
	// trace id — the UI/PostHog handle on the turn.
	t.Run("c: the traceresponse header carries the turn's trace id", func(t *testing.T) {
		ctx := context.Background() // test root (rule://go-thread-context _test.go exemption)
		exp := installGlobalSpanExporter(t)
		w := newMentionE2EWire(t)

		recip := w.seedAgentMember(t, "hdrrecip", true)
		const recipSess = "sess-hdrrecip-1"
		bringSessionLive(t, w, recip.ID, containerFor("hdrrecip"), recipSess)

		client := newTracedCommsClient(t, serveTracedCommsDoor(t, w.comms, w.adminID))
		resp, msgID := postOverTracedDoor(t, ctx, client, w.channel, "header carries the trace")

		waitForControlDelivers(t, w.runner, recipSess, 1)

		want := originServerSpan(t, exp.GetSpans(), compassv1connect.CommsServicePostMessageProcedure).SpanContext.TraceID().String()

		hdr := resp.Header().Get(traceResponseHdr)
		if hdr == "" {
			t.Fatal("response carried no traceresponse header on the enabled path")
		}
		if !w3cTraceResponse.MatchString(hdr) {
			t.Fatalf("traceresponse %q is not the W3C 00-… grammar", hdr)
		}
		if got := hdr[3:35]; got != want {
			t.Fatalf("traceresponse trace id = %q, want the handler span's %q (header %q)", got, want, hdr)
		}
		// The same turn, same trace: the header the caller reads and the op the
		// agent receives name ONE trace id.
		op := tracedOpFor(t, w.runner, recipSess, msgID)
		if got := traceIDOfTraceparent(t, ctx, "the agent-seam deliver op", op.traceparent).String(); got != want {
			t.Fatalf("seam op trace id = %q, want the traceresponse header's %q", got, want)
		}
	})

	// (d) THE OFF-BY-DEFAULT INVARIANT: with the endpoint unset — no global SDK
	// provider, the shipped default — the identical flow still DISPATCHES, with
	// an empty traceparent and zero recorded spans. Absence of tracing must never
	// break delivery.
	t.Run("d: disabled dispatches successfully with an empty traceparent and no spans", func(t *testing.T) {
		ctx := context.Background() // test root (rule://go-thread-context _test.go exemption)
		// Save/restore the globals but install NO SDK provider — the shipped
		// disabled state (mirroring TestServerEmissionDisabledSetsNoTraceResponse
		// Header). The global is PINNED to an explicit no-op provider rather than
		// merely left alone, so the assertion cannot be weakened by another
		// test's leaked provider; that pin is the established idiom at
		// internal/runner/otel_test.go:98-100.
		prevTP := otel.GetTracerProvider()
		prevProp := otel.GetTextMapPropagator()
		otel.SetTracerProvider(noop.NewTracerProvider())
		t.Cleanup(func() {
			otel.SetTracerProvider(prevTP)
			otel.SetTextMapPropagator(prevProp)
		})
		// A recorder deliberately NOT made global: any span the disabled path
		// somehow emitted through an SDK provider would be caught here, yet none
		// is, because every emission site reads the (no-op) global.
		offExp := tracetest.NewInMemoryExporter()
		offTP := sdktrace.NewTracerProvider(sdktrace.WithSyncer(offExp))
		t.Cleanup(func() {
			_ = offTP.Shutdown(context.Background()) // deferred test cleanup: the sync exporter already holds any spans, so this error is not actionable
		})

		w := newMentionE2EWire(t)
		recip := w.seedAgentMember(t, "offrecip", true)
		const recipSess = "sess-offrecip-1"
		bringSessionLive(t, w, recip.ID, containerFor("offrecip"), recipSess)

		client := newTracedCommsClient(t, serveTracedCommsDoor(t, w.comms, w.adminID))
		resp, msgID := postOverTracedDoor(t, ctx, client, w.channel, "delivery survives tracing being off")

		// The load-bearing half: delivery still happens.
		waitForControlDelivers(t, w.runner, recipSess, 1)
		op := tracedOpFor(t, w.runner, recipSess, msgID)
		if op.kind != controlDeliver {
			t.Fatalf("disabled-path op kind = %v, want a plain deliver", op.kind)
		}
		if op.traceparent != "" {
			t.Fatalf("disabled-path op traceparent = %q, want EMPTY (no provider ⇒ no span ⇒ nothing to stamp)", op.traceparent)
		}
		if hdr := resp.Header().Get(traceResponseHdr); hdr != "" {
			t.Fatalf("disabled path set traceresponse = %q, want none", hdr)
		}
		if got := offExp.GetSpans(); len(got) != 0 {
			t.Fatalf("disabled path recorded %d spans, want 0", len(got))
		}
	})

	// (e) CONTINUITY ACROSS THE HOLD EDGE: an agent post is HELD while its author
	// streams and fired at the author's settle edge on the ctx-free drain loop, so
	// only the traceparent captured at hold() and restamped at fireHeld keeps the
	// fired deliver inside the reply turn's trace.
	t.Run("e: a held-then-settled deliver stays in the agent post's own trace", func(t *testing.T) {
		ctx := context.Background() // test root (rule://go-thread-context _test.go exemption)
		exp := installGlobalSpanExporter(t)
		w := newMentionE2EWire(t)

		author := w.seedAgentMember(t, "heldauthor", true)
		recip := w.seedAgentMember(t, "heldrecip", true)
		const authorSess = "sess-heldauthor-1"
		const recipSess = "sess-heldrecip-1"
		// BOTH live before any post: the author's live session is what makes the
		// post take the hold arm, and the recipient's is what lets the fired
		// deliver reach the wire.
		bringSessionLive(t, w, author.ID, containerFor("heldauthor"), authorSess)
		bringSessionLive(t, w, recip.ID, containerFor("heldrecip"), recipSess)

		// The author posts over a door attributed to the AGENT, so the post has a
		// real handler span AND an agent author (the hold arm's precondition).
		authorClient := newTracedCommsClient(t, serveTracedCommsDoor(t, w.comms, author.ID))
		_, heldMsgID := postOverTracedDoor(t, ctx, authorClient, w.channel, "held until I settle")

		// FIFO BARRIER, not a sleep: the consumer's Run loop drains bus events on
		// ONE goroutine in order, so a later HUMAN post (settled at post, hence
		// dispatched immediately) whose deliver is observed on the wire proves the
		// earlier agent post was already processed — i.e. already held. Without
		// this, settling could race ahead of the hold and fire nothing. Same
		// barrier idiom as the offline-mention e2e's cycle A.
		barrierMsgID := w.post(t, "barrier: a human post settles at once")
		barrier := waitForControlDelivers(t, w.runner, recipSess, 1)
		if barrier[0].messageID != barrierMsgID {
			t.Fatalf("first deliver = %q, want the barrier message %q", barrier[0].messageID, barrierMsgID)
		}
		// Held, therefore NOT yet dispatched: the agent post is absent from the wire.
		for _, op := range tracedOps(w.runner) {
			if op.messageID == heldMsgID {
				t.Fatalf("the agent post %q dispatched before its settle edge (op %+v), want it HELD", heldMsgID, op)
			}
		}

		// Settle fires the held set on the bare drain ctx (no active span).
		w.consumer.OnSessionSettled(authorSess, compassv1.AgentSessionState_AGENT_SESSION_STATE_READY)
		waitForControlDelivers(t, w.runner, recipSess, 2)

		want := originServerSpan(t, exp.GetSpans(), compassv1connect.CommsServicePostMessageProcedure).SpanContext.TraceID()
		op := tracedOpFor(t, w.runner, recipSess, heldMsgID)
		if got := traceIDOfTraceparent(t, ctx, "the settled deliver", op.traceparent); got != want {
			t.Fatalf("settled deliver trace id = %s, want the agent post's origin %s — continuity was lost across the settle edge", got, want)
		}
	})

	// (f) THE CONTINUITY BOUNDARY: a SWEEP-delivered message is fresh-rooted BY
	// DESIGN — a sweep is its own operation, not a continuation of the post that
	// happened to be missed. So its op's trace id must DIFFER from the post's,
	// and both must be real (a fresh root is a NEW trace, never the empty one).
	t.Run("f: a sweep-delivered op is fresh-rooted, not the post's trace", func(t *testing.T) {
		ctx := context.Background() // test root (rule://go-thread-context _test.go exemption)
		exp := installGlobalSpanExporter(t)
		w := newMentionE2EWire(t)

		// Subscribed (in the sweep set) but NOT hub-live: the post finds no live
		// session, so nothing dispatches and the cursor sweep is the delivery path.
		agent := w.seedAgentMember(t, "sweeprecip", true)

		client := newTracedCommsClient(t, serveTracedCommsDoor(t, w.comms, w.adminID))
		_, msgID := postOverTracedDoor(t, ctx, client, w.channel, "swept on the next start")

		// Event-gate on the wake's wire fact, so the post is fully processed
		// before the start edge is driven.
		waitForStartCount(t, w.runner, 1)
		if got := allControlDelivers(w.runner); len(got) != 0 {
			t.Fatalf("dispatches while offline = %d, want 0 (nothing live to deliver to)", len(got))
		}

		// The agent comes live: the start-edge cursor sweep delivers, rooting its
		// own delivery.sweep.session span.
		const sess = "sess-sweeprecip-1"
		w.consumer.OnSessionStarted(sess, agent.ID)
		waitForControlDelivers(t, w.runner, sess, 1)

		postTraceID := originServerSpan(t, exp.GetSpans(), compassv1connect.CommsServicePostMessageProcedure).SpanContext.TraceID()
		if !postTraceID.IsValid() {
			t.Fatal("the post's own trace id is invalid; the boundary assertion would be vacuous")
		}
		op := tracedOpFor(t, w.runner, sess, msgID)
		// Valid and non-empty: traceIDOfTraceparent fails on either, so a fresh
		// root that silently stamped nothing cannot pass as "different".
		sweepTraceID := traceIDOfTraceparent(t, ctx, "the swept deliver op", op.traceparent)
		if sweepTraceID == postTraceID {
			t.Fatalf("swept op trace id = %s, want a trace DIFFERENT from the post's %s (a sweep is fresh-rooted by design, never a continuation)", sweepTraceID, postTraceID)
		}
	})

	// (g) An ask-answer turn: the answer message rides the normal message rail,
	// so its deliver must sit in the RespondToAsk handler's trace — the answer's
	// delivery origin.
	t.Run("g: the ask answer's deliver carries the RespondToAsk handler span's trace id", func(t *testing.T) {
		ctx := context.Background() // test root (rule://go-thread-context _test.go exemption)
		exp := installGlobalSpanExporter(t)
		w := newMentionE2EWire(t)

		recip := w.seedAgentMember(t, "askrecip", true)
		const recipSess = "sess-askrecip-1"
		bringSessionLive(t, w, recip.ID, containerFor("askrecip"), recipSess)

		// The ask itself needs no span — only the ANSWER's handler span is under
		// test — so it is posted in-process.
		askMsgID, askID := postAsk(t, w, "Which environment?")
		// The ask post is itself a message on the rail: gate on its deliver so the
		// answer's deliver is unambiguously the second one.
		askDeliver := waitForControlDelivers(t, w.runner, recipSess, 1)
		if askDeliver[0].messageID != askMsgID {
			t.Fatalf("first deliver = %q, want the ask message %q", askDeliver[0].messageID, askMsgID)
		}

		client := newTracedCommsClient(t, serveTracedCommsDoor(t, w.comms, w.adminID))
		if _, err := client.RespondToAsk(ctx, connect.NewRequest(&compassv1.RespondToAskRequest{
			AskId:   askID,
			Answers: []*compassv1.AskQuestionAnswer{{QuestionId: "q1", ChosenOptionIds: []string{"opt-a"}}},
		})); err != nil {
			t.Fatalf("RespondToAsk over the traced door: %v", err)
		}
		waitForControlDelivers(t, w.runner, recipSess, 2)

		// RespondToAskResponse carries no fields, so the answer message id is read
		// off the handler span's own compass.message.id — the id comms stamps there
		// (comms.go:465) — which is also what makes this the answer's origin span.
		answerSpan := originServerSpan(t, exp.GetSpans(), compassv1connect.CommsServiceRespondToAskProcedure)
		answerMsgID, ok := spanAttr(answerSpan, messageIDAttr)
		if !ok || answerMsgID == "" {
			t.Fatalf("RespondToAsk span carries no %s attribute; the answer's delivery origin is unidentifiable", messageIDAttr)
		}
		if answerMsgID == askMsgID {
			t.Fatalf("RespondToAsk span %s = %q, want the NEW answer message, not the ask %q", messageIDAttr, answerMsgID, askMsgID)
		}

		op := tracedOpFor(t, w.runner, recipSess, answerMsgID)
		want := answerSpan.SpanContext.TraceID()
		if got := traceIDOfTraceparent(t, ctx, "the answer message's deliver op", op.traceparent); got != want {
			t.Fatalf("answer deliver trace id = %s, want the RespondToAsk handler span's %s", got, want)
		}
	})

	// (h) BLOCKED — not implemented on main.
	t.Run("h: an agent-authored post's RelayCommsCall origin span is a fresh root", func(t *testing.T) {
		// The assertion needs a RelayCommsCall origin span to exist. It does not:
		// the RunnerService door is mounted by runnerhub.NewMountedHandler
		// (handler.go:433-439), whose ONLY interceptors are the bearer pair —
		// there is no otelconnect on that door, and package internal/runnerhub
		// contains no otelconnect import and no tracer call at all. So no span is
		// ever created for RelayCommsCall, and "the origin span exists" cannot be
		// driven without adding that production wiring.
		//
		// Blocker: T5 move 6 (design.md:705-712) is absent from main. Un-skip
		// once RelayCommsCall creates its origin span.
		t.Skip("blocked: no RelayCommsCall origin span exists — runnerhub.NewMountedHandler (internal/runnerhub/handler.go:433-439) mounts no otelconnect interceptor and internal/runnerhub creates no spans (T5 move 6, design.md:705-712, not on main)")
	})

	// (i) BLOCKED — not implemented on main.
	t.Run("i: a reply carrying a trigger_traceparent starts a NEW trace linked to the trigger", func(t *testing.T) {
		// The termination mechanism is the span LINK T5 move 6 builds from
		// CommsCallRequest.trigger_traceparent at the RelayCommsCall Post arm. The
		// proto field is generated and reachable
		// (internal/gen/compass/v1/agent_gateway.pb.go:139,269 GetTriggerTraceparent),
		// but NOTHING outside generated code reads it, and the repository contains
		// no span-link construction whatsoever: no AddLink, no trace.WithLinks, no
		// trace.LinkFromContext in any non-generated, non-vendored Go file. So
		// neither half of the assertion — the new trace id NOR the link target —
		// has a production mechanism to observe.
		//
		// Blocker: T5 move 6 (design.md:705-712) is absent from main. Un-skip once
		// executeCall's Post arm adds the link.
		t.Skip("blocked: trigger_traceparent has no reader and no span link exists — GetTriggerTraceparent (internal/gen/compass/v1/agent_gateway.pb.go:269) is unread outside generated code, and no AddLink/trace.WithLinks/trace.LinkFromContext appears in non-generated code (T5 move 6, design.md:705-712, not on main)")
	})

	// (j) The op-kind delivery counter increments — and, the load-bearing half,
	// carries NO per-session/per-channel label. A session or channel id here is
	// an unbounded-cardinality metric, the §Global Constraints hard rule.
	t.Run("j: the dispatch counter increments with the op kind as its only label", func(t *testing.T) {
		ctx := context.Background() // test root (rule://go-thread-context _test.go exemption)
		// The meter provider MUST precede the wire: the counter is created once at
		// delivery.NewConsumer from the then-global meter.
		reader := installGlobalMetricReader(t)
		installGlobalSpanExporter(t)
		w := newMentionE2EWire(t)

		// One recipient of each op kind: a MENTIONED member is steered, a plain
		// subscriber is delivered to — so both label values are exercised.
		steered := w.seedAgentMember(t, "metricsteer", true)
		delivered := w.seedAgentMember(t, "metricdeliver", true)
		const steerSess = "sess-metricsteer-1"
		const deliverSess = "sess-metricdeliver-1"
		bringSessionLive(t, w, steered.ID, containerFor("metricsteer"), steerSess)
		bringSessionLive(t, w, delivered.ID, containerFor("metricdeliver"), deliverSess)

		client := newTracedCommsClient(t, serveTracedCommsDoor(t, w.comms, w.adminID))
		_, msgID := postOverTracedDoor(t, ctx, client, w.channel, "@metricsteer counted once, by kind")

		steer := waitForControlDelivers(t, w.runner, steerSess, 1)
		if steer[0].kind != controlSteer || steer[0].messageID != msgID {
			t.Fatalf("mentioned member's dispatch = %+v, want {steer, %s}", steer[0], msgID)
		}
		deliver := waitForControlDelivers(t, w.runner, deliverSess, 1)
		if deliver[0].kind != controlDeliver || deliver[0].messageID != msgID {
			t.Fatalf("plain subscriber's dispatch = %+v, want {deliver, %s}", deliver[0], msgID)
		}

		var rm metricdata.ResourceMetrics
		if err := reader.Collect(ctx, &rm); err != nil {
			t.Fatalf("collect metrics: %v", err)
		}
		// The cardinality rule is enforced inside dispatchedOpKindCounts: it fails
		// if any data point carries an attribute other than the op kind.
		counts := dispatchedOpKindCounts(t, &rm)
		if counts["steer"] != 1 {
			t.Fatalf("%s{steer} = %d, want 1 (counts=%v)", dispatchedMetricName, counts["steer"], counts)
		}
		if counts["deliver"] != 1 {
			t.Fatalf("%s{deliver} = %d, want 1 (counts=%v)", dispatchedMetricName, counts["deliver"], counts)
		}
	})
}
