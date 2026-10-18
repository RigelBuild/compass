//go:build pgtest && unix

package server

// T6 of RigelBuild/docs:docs/designs/compass/observability/compass-server-runner-otel/design.md: the
// end-to-end ONE TURN, ONE TRACE proof over the shipped comms door. Load-bearing
// ordering: install the span exporter before any door/wire (otelconnect captures
// the global tracer provider once at NewInterceptor); see the helper docs below.

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"runtime"
	"testing"

	"connectrpc.com/connect/v2"
	"connectrpc.com/connect/v2/connecthttp"
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
	compassv1internal "github.com/RigelBuild/compass/go/internal/gen/compass/v1"
	"github.com/RigelBuild/compass/go/internal/gen/compass/v1/compassv1internalconnect"
	otelx "github.com/RigelBuild/compass/go/internal/otel"
	"github.com/RigelBuild/compass/go/internal/runnerhub"
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

// serveTracedCommsDoor mounts CommsService on the socket-door h2c test server
// with otel tracing and ambient identity. Call it only after installing the
// global span exporter because the interceptor captures the provider at creation.
func serveTracedCommsDoor(t *testing.T, commsSvc *comms.Comms, actor store.AccountID) string {
	t.Helper()
	otelIC, err := otelconnect.NewServerInterceptor()
	if err != nil {
		t.Fatalf("otelconnect.NewServerInterceptor: %v", err)
	}
	server := connect.NewServer(otelx.HandlerInterceptors(otelIC, auth.AmbientIdentity(actor)))
	compassv1connect.RegisterCommsServiceHandler(server, commsSvc)
	mux := http.NewServeMux()
	connecthttp.Mount(mux, server)
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
	return compassv1connect.NewCommsServiceClient(connect.NewClient(connecthttp.NewTransport(&http.Client{Transport: tr}, baseURL, connecthttp.WithReadMaxBytes(0))))
}

// Mounts a RunnerService door with production interceptors so the client
// traceparent links to a root span. Build after the exporter is installed.
func newRelayRunnerClient(t *testing.T, hub *runnerhub.Hub, st *store.Store) compassv1internalconnect.RunnerServiceClient {
	t.Helper()
	otelIC, err := otelconnect.NewServerInterceptor()
	if err != nil {
		t.Fatalf("otelconnect.NewServerInterceptor: %v", err)
	}
	mux := http.NewServeMux()
	runnerhub.Mount(mux, hub,
		func(ctx context.Context, presented string, want store.SubjectKind) (store.Subject, error) {
			return auth.ResolveToken(ctx, st, presented, want)
		}, nil, nil, otelIC)
	srv := httptest.NewUnstartedServer(mux)
	srv.Config.Protocols = cleartextHTTP2()
	srv.Start()
	t.Cleanup(srv.Close)

	tr := h2cTransport(func(ctx context.Context, network, addr string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, network, addr)
	})
	t.Cleanup(tr.CloseIdleConnections)
	clientIC, err := otelconnect.NewClientInterceptor()
	if err != nil {
		t.Fatalf("otelconnect.NewClientInterceptor: %v", err)
	}
	return compassv1internalconnect.NewRunnerServiceClient(connect.NewClient(connecthttp.NewTransport(
		&http.Client{Transport: tr}, srv.URL,
		connecthttp.WithReadMaxBytes(0),
	), clientIC, runnerBearer(fakeRunnerToken).ClientInterceptor,
	))
}

// Relays an agent Post and returns its message ID; errors include transport and
// in-band tool failures.
func relayAgentPost(t *testing.T, ctx context.Context, client compassv1internalconnect.RunnerServiceClient, sessionID, channelName, body, triggerTP string) string {
	t.Helper()
	resp, err := client.RelayCommsCall(ctx, &compassv1internal.RelayCommsCallRequest{
		SessionId: sessionID,
		Call: &compassv1internal.CommsCallRequest{
			CallId:             "tc-relay-" + body,
			TriggerTraceparent: triggerTP,
			Call: &compassv1internal.CommsCallRequest_Post{Post: &compassv1.PostMessageRequest{
				Container:   &compassv1.PostMessageRequest_ChannelId{ChannelId: channelName},
				Topic:       &compassv1.PostMessageRequest_TopicName{TopicName: "general"},
				CreateTopic: true,
				Blocks:      textBlock(body),
			}},
		},
	})
	if err != nil {
		t.Fatalf("RelayCommsCall(post %q): %v", body, err)
	}
	result := resp.GetResult()
	if e := result.GetError(); e != nil {
		t.Fatalf("RelayCommsCall(post %q) rendered an in-band tool error {code=%q message=%q}; the Post arm did not complete", body, e.GetCode(), e.GetMessage())
	}
	msgID := result.GetPost().GetMessage().GetId()
	if msgID == "" {
		t.Fatalf("RelayCommsCall(post %q) returned no message id; the Post arm did not persist a message", body)
	}
	return msgID
}

// Posts a message through the traced door, returning response headers and the
// created message ID. Unlike postOverSocket, this accepts per-case body text.
func postOverTracedDoor(t *testing.T, ctx context.Context, client compassv1connect.CommsServiceClient, channel store.ChannelID, body string) (*connect.Header, string) {
	t.Helper()
	ctx, info := connect.NewClientContext(ctx)
	resp, err := client.PostMessage(ctx, &compassv1.PostMessageRequest{
		Container:   &compassv1.PostMessageRequest_ChannelId{ChannelId: string(channel)},
		Topic:       &compassv1.PostMessageRequest_TopicName{TopicName: "general"},
		CreateTopic: true,
		Blocks:      []*compassv1.MessageBlock{{Block: &compassv1.MessageBlock_Text{Text: body}}},
	})
	if err != nil {
		t.Fatalf("PostMessage(%q) over the traced door: %v", body, err)
	}
	msgID := resp.GetMessage().GetId()
	if msgID == "" {
		t.Fatalf("PostMessage(%q) returned an empty message id", body)
	}
	return info.ResponseHeader(), msgID
}

// Posts an ask without tracing and returns its message ID and server-minted ask
// ID; caller-supplied ask IDs are stripped.
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

// Starts the session and waits for its edge sweep, preventing asynchronous
// redelivery from shifting later span assertions. The fake Runner does not ack.
func bringSessionLive(t *testing.T, w *mentionE2EWire, exp *tracetest.InMemoryExporter, account store.AccountID, container, session string) {
	t.Helper()
	// Baseline BEFORE Start: the edge it enqueues is the one we wait on. exp is
	// required — every fixture here brings its session live while tracing is on,
	// including (d), which pins the no-op provider only afterwards.
	sweepsBefore := countSpansNamed(exp, startSweepSpanName)
	w.runner.setContainerNames(container)
	w.runner.setStartIDs(session)
	presp, _, err := w.hub.Provision(w.ctx, "", account, &compassv1.ProvisionAgentWorkspaceRequest{})
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
	if got, ok := w.hub.SessionForAccount(context.Background(), account); !ok || got != session {
		t.Fatalf("SessionForAccount(%s) = (%q, %v), want (%q, true) — the hold/fan-out arms resolve through this binding", account, got, ok, session)
	}
	waitForStartSweep(t, exp, sweepsBefore+1)
}

// The final start-edge sweep marks completion because delivery drains sweeps
// serially.
const startSweepSpanName = "delivery.sweep.owedMentions"

func countSpansNamed(exp *tracetest.InMemoryExporter, name string) int {
	n := 0
	for _, s := range exp.GetSpans() {
		if s.Name == name {
			n++
		}
	}
	return n
}

// Waits for owed-mention start-edge sweeps. Count only this sweep because
// reconnect and floor-tick recovery also emit session sweeps.
func waitForStartSweep(t *testing.T, exp *tracetest.InMemoryExporter, n int) {
	t.Helper()
	deadline := timeAfter()
	for {
		got := countSpansNamed(exp, startSweepSpanName)
		if got == n {
			return
		}
		if got > n {
			t.Fatalf("saw %d %q spans, want exactly %d — an unaccounted session start interleaved, so this gate no longer proves MY start edge drained",
				got, startSweepSpanName, n)
			return
		}
		select {
		case <-deadline:
			// Two very different causes, and the second is the likelier one to
			// miss: either the sweep really never drained, or it drained fine and
			// nothing recorded a span because the global provider is not the SDK
			// one feeding exp. A total of zero exported spans points at the latter.
			t.Fatalf("saw %d %q spans (%d exported spans total), want %d — either the session-start sweep never drained, or the global TracerProvider was not the SDK one feeding this exporter while bringSessionLive ran (the sweep resolves otel.Tracer PER CALL at delivery/settle.go:219, so a no-op provider pinned before the gate — as (d) deliberately pins AFTER it — records nothing)",
				got, startSweepSpanName, len(exp.GetSpans()), n)
			return
		default:
		}
		runtime.Gosched()
	}
}

// Installs the global meter provider before wire construction creates counters.
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

// Records send-only control pushes with their traceparent for these assertions.
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

// Selects one operation by session and message; duplicates make traceparent
// attribution ambiguous.
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

// Waits for the exact session/message deliver, not a count or index, with a
// deadline instead of sleeping.
func waitForDeliverOfMessage(t *testing.T, r *recordingRunner, sessionID, messageID string) {
	t.Helper()
	deadline := timeAfter()
	for {
		for _, op := range tracedOps(r) {
			if op.sessionID == sessionID && op.messageID == messageID {
				return
			}
		}
		select {
		case <-deadline:
			t.Fatalf("session %q never saw a deliver carrying message %q (ops: %+v)", sessionID, messageID, tracedOps(r))
			return
		default:
		}
		runtime.Gosched()
	}
}

// Parses W3C traceparent with the shipped helper, verifying its grammar.
func traceIDOfTraceparent(t *testing.T, ctx context.Context, what, tp string) trace.TraceID {
	t.Helper()
	return spanContextOfTraceparent(t, ctx, what, tp).TraceID()
}

// Parses the full span context so callers can assert trace and span IDs.
func spanContextOfTraceparent(t *testing.T, ctx context.Context, what, tp string) trace.SpanContext {
	t.Helper()
	if tp == "" {
		t.Fatalf("%s carried an EMPTY traceparent, want a W3C 00-… value (the turn's trace never reached the agent seam)", what)
	}
	sc := trace.SpanContextFromContext(otelx.ContextWithTraceparent(ctx, tp))
	if !sc.IsValid() {
		t.Fatalf("%s traceparent %q did not parse to a valid span context", what, tp)
	}
	return sc
}

// Selects the RelayCommsCall server span by procedure and message ID; delivery
// spans also carry the message ID.
func relayOriginSpanForMessage(t *testing.T, spans tracetest.SpanStubs, msgID string) tracetest.SpanStub {
	t.Helper()
	want := compassv1internalconnect.RunnerServiceRelayCommsCallProcedure[1:] // trim the leading "/"
	var match []tracetest.SpanStub
	for _, s := range spans {
		if s.Name != want || s.SpanKind != trace.SpanKindServer {
			continue
		}
		if got, ok := spanAttr(s, messageIDAttr); ok && got == msgID {
			match = append(match, s)
		}
	}
	if len(match) != 1 {
		t.Fatalf("found %d SERVER spans named %q carrying %s=%q, want exactly 1 (spans exported: %d)", len(match), want, messageIDAttr, msgID, len(spans))
	}
	return match[0]
}

// Selects the cross-turn link by kind, not position; link attribute limits must
// retain the selector.
func triggerLinkOf(t *testing.T, span tracetest.SpanStub) trace.SpanContext {
	t.Helper()
	var match []trace.SpanContext
	for _, l := range span.Links {
		if linkIsTrigger(l) {
			match = append(match, l.SpanContext)
		}
	}
	if len(match) != 1 {
		t.Fatalf("found %d links carrying %s=%q, want exactly 1 (links on the span: %+v)",
			len(match), triggerLinkKindKey, triggerLinkKindValue, span.Links)
	}
	return match[0]
}

// countTriggerLinks counts the cross-turn trigger links on span. Used for the
// zero case, where triggerLinkOf's t.Fatalf would be the wrong shape.
func countTriggerLinks(span tracetest.SpanStub) int {
	n := 0
	for _, l := range span.Links {
		if linkIsTrigger(l) {
			n++
		}
	}
	return n
}

func linkIsTrigger(l sdktrace.Link) bool {
	for _, a := range l.Attributes {
		if a.Key == triggerLinkKindKey && a.Value.AsString() == triggerLinkKindValue {
			return true
		}
	}
	return false
}

const (
	// Duplicated here so tests check the observable link-kind contract, not the
	// same constant used by its producer.
	triggerLinkKindKey   = attribute.Key("compass.link.kind")
	triggerLinkKindValue = "cross_turn_trigger"
)

// Selects the otelconnect server span by procedure and kind; message ID alone is
// ambiguous with delivery spans.
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
func spanAttr(s tracetest.SpanStub, key string) (string, bool) { //nolint:unparam // read-clarity signature: key names which span attribute is read; currently constant, not dead code.
	for _, a := range s.Attributes {
		if string(a.Key) == key {
			return a.Value.AsString(), true
		}
	}
	return "", false
}

// Returns the origin and delivery spans carrying a message ID: the recorded
// server hops of one turn.
func spansForMessage(spans tracetest.SpanStubs, msgID string) tracetest.SpanStubs {
	var out tracetest.SpanStubs
	for _, s := range spans {
		if got, ok := spanAttr(s, messageIDAttr); ok && got == msgID {
			out = append(out, s)
		}
	}
	return out
}

// Extracts dispatched counts by operation kind and asserts each point has only
// that attribute, preserving metric cardinality.
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
	t.Run("a: the agent-seam op carries the PostMessage handler span's trace id", traceContinuityA)

	// (b) Every recorded server hop span of the turn shares that one trace id —
	// ONE CONNECTED TRACE, not a set of correlated fragments.
	t.Run("b: every recorded server hop span shares the one trace id", traceContinuityB)

	// (c) The traceresponse header on the PostMessage response carries the same
	// trace id — the UI/PostHog handle on the turn.
	t.Run("c: the traceresponse header carries the turn's trace id", traceContinuityC)

	// (d) THE OFF-BY-DEFAULT INVARIANT: with the endpoint unset — no global SDK
	// provider, the shipped default — the identical flow still DISPATCHES, with
	// an empty traceparent and zero recorded spans. Absence of tracing must never
	// break delivery.
	t.Run("d: disabled dispatches successfully with an empty traceparent and no spans", traceContinuityD)

	// (e) CONTINUITY ACROSS THE HOLD EDGE: an agent post is HELD while its author
	// streams and fired at the author's settle edge on the ctx-free drain loop, so
	// only the traceparent captured at hold() and restamped at fireHeld keeps the
	// fired deliver inside the reply turn's trace.
	t.Run("e: a held-then-settled deliver stays in the agent post's own trace", traceContinuityE)

	// (f) THE CONTINUITY BOUNDARY: a SWEEP-delivered message is fresh-rooted BY
	// DESIGN — a sweep is its own operation, not a continuation of the post that
	// happened to be missed. So its op's trace id must DIFFER from the post's,
	// and both must be real (a fresh root is a NEW trace, never the empty one).
	t.Run("f: a sweep-delivered op is fresh-rooted, not the post's trace", traceContinuityF)

	// (g) An ask-answer turn: the answer message rides the normal message rail,
	// so its deliver must sit in the RespondToAsk handler's trace — the answer's
	// delivery origin.
	t.Run("g: the ask answer's deliver carries the RespondToAsk handler span's trace id", traceContinuityG)

	// (h) An agent-authored post over the RunnerService door: its origin span is
	// the otelconnect RelayCommsCall handler span. Proves it EXISTS and is a FRESH
	// ROOT — the door REFUSING an offered parent (the Runner propagates one when
	// tracing is on), because otelconnect's trustRemote defaults false.
	t.Run("h: an agent-authored post's RelayCommsCall origin span is a fresh root", traceContinuityH)

	// (i) TERMINATION. A reply whose call carries a trigger_traceparent starts a
	// NEW trace and merely LINKS back — never nests, which stops a conversation
	// from growing one unbounded tree. Both halves asserted, plus the empty-
	// trigger negative control that makes the positive non-vacuous.
	t.Run("i: a reply carrying a trigger_traceparent starts a NEW trace linked to the trigger", traceContinuityI)

	// (j) The op-kind delivery counter increments — and, the load-bearing half,
	// carries NO per-session/per-channel label. A session or channel id here is
	// an unbounded-cardinality metric, the §Global Constraints hard rule.
	t.Run("j: the dispatch counter increments with the op kind as its only label", traceContinuityJ)
}

func traceContinuityA(t *testing.T) {
	ctx := context.Background() // test root (rule://go-thread-context _test.go exemption)
	exp := installGlobalSpanExporter(t)
	w := newMentionE2EWire(t)

	recip := w.seedAgentMember(t, "seamrecip", true)
	const recipSess = "sess-seamrecip-1"
	bringSessionLive(t, w, exp, recip.ID, containerFor("seamrecip"), recipSess)

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
	if resp.Get(traceResponseHdr) == "" {
		t.Fatal("enabled path set no traceresponse header on the post")
	}
}

func traceContinuityB(t *testing.T) {
	ctx := context.Background() // test root (rule://go-thread-context _test.go exemption)
	exp := installGlobalSpanExporter(t)
	w := newMentionE2EWire(t)

	recip := w.seedAgentMember(t, "hoprecip", true)
	const recipSess = "sess-hoprecip-1"
	bringSessionLive(t, w, exp, recip.ID, containerFor("hoprecip"), recipSess)

	client := newTracedCommsClient(t, serveTracedCommsDoor(t, w.comms, w.adminID))
	_, msgID := postOverTracedDoor(t, ctx, client, w.channel, "every hop, one trace")

	waitForDeliverOfMessage(t, w.runner, recipSess, msgID)

	// FIFO BARRIER, not a sleep: gatedDispatch completes inside the consumer's
	// serial fabric callback before the next event is handled, so a LATER post's
	// deliver proves the first dispatch returned. Waited BY IDENTITY (a re-
	// delivered first message would shift any fixed index; position never held).
	barrierMsgID := w.post(t, "barrier: a plain post completes the earlier dispatch")
	waitForDeliverOfMessage(t, w.runner, recipSess, barrierMsgID)

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
}

func traceContinuityC(t *testing.T) {
	ctx := context.Background() // test root (rule://go-thread-context _test.go exemption)
	exp := installGlobalSpanExporter(t)
	w := newMentionE2EWire(t)

	recip := w.seedAgentMember(t, "hdrrecip", true)
	const recipSess = "sess-hdrrecip-1"
	bringSessionLive(t, w, exp, recip.ID, containerFor("hdrrecip"), recipSess)

	client := newTracedCommsClient(t, serveTracedCommsDoor(t, w.comms, w.adminID))
	resp, msgID := postOverTracedDoor(t, ctx, client, w.channel, "header carries the trace")

	waitForControlDelivers(t, w.runner, recipSess, 1)

	want := originServerSpan(t, exp.GetSpans(), compassv1connect.CommsServicePostMessageProcedure).SpanContext.TraceID().String()

	hdr := resp.Get(traceResponseHdr)
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
}

func traceContinuityD(t *testing.T) {
	ctx := context.Background() // test root (rule://go-thread-context _test.go exemption)
	// (d) brings the session live while tracing is still ON so it can use the
	// start-edge sweep gate, and only THEN pins the provider to no-op. Ordering
	// is load-bearing: the pin must precede the door build (otelconnect captures
	// the global provider at construction) and the post below it.
	exp := installGlobalSpanExporter(t)
	w := newMentionE2EWire(t)
	recip := w.seedAgentMember(t, "offrecip", true)
	const recipSess = "sess-offrecip-1"
	bringSessionLive(t, w, exp, recip.ID, containerFor("offrecip"), recipSess)

	// NOW pin the disabled state: install NO SDK provider (the shipped disabled
	// state), PINNED to an explicit no-op rather than left alone so a leaked
	// provider cannot weaken the assertion. Cleanup is LIFO: this restore runs
	// before installGlobalSpanExporter's own.
	prevTP := otel.GetTracerProvider()
	prevProp := otel.GetTextMapPropagator()
	otel.SetTracerProvider(noop.NewTracerProvider())
	t.Cleanup(func() {
		otel.SetTracerProvider(prevTP)
		otel.SetTextMapPropagator(prevProp)
	})
	// A recorder deliberately NOT made global (house idiom at
	// internal/runner/otel_test.go:98-103): nothing routes spans into a
	// non-global provider, so its emptiness is a catch-net, not the proof. The
	// real disabled-path proof is the empty traceparent + absent header below.
	offExp := tracetest.NewInMemoryExporter()
	offTP := sdktrace.NewTracerProvider(sdktrace.WithSyncer(offExp))
	t.Cleanup(func() {
		_ = offTP.Shutdown(context.Background()) // deferred test cleanup: the sync exporter already holds any spans, so this error is not actionable
	})
	// Non-vacuous counterpart to offExp: exp IS fed by a global SDK provider,
	// right up to the pin above. Freezing its count here means the post below
	// can be asserted to add NOTHING, which proves the pin actually took
	// effect — the one absence claim in (d) that does not rely on offExp.
	spansAtPin := len(exp.GetSpans())

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
	if hdr := resp.Get(traceResponseHdr); hdr != "" {
		t.Fatalf("disabled path set traceresponse = %q, want none", hdr)
	}
	if got := offExp.GetSpans(); len(got) != 0 {
		t.Fatalf("disabled path recorded %d spans, want 0", len(got))
	}
	if got := len(exp.GetSpans()); got != spansAtPin {
		t.Fatalf("disabled path added %d spans to the global-fed exporter (%d -> %d), want none — the no-op pin did not take effect", got-spansAtPin, spansAtPin, got)
	}
}

func traceContinuityE(t *testing.T) {
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
	bringSessionLive(t, w, exp, author.ID, containerFor("heldauthor"), authorSess)
	bringSessionLive(t, w, exp, recip.ID, containerFor("heldrecip"), recipSess)

	// The author posts over a door attributed to the AGENT, so the post has a
	// real handler span AND an agent author (the hold arm's precondition).
	authorClient := newTracedCommsClient(t, serveTracedCommsDoor(t, w.comms, author.ID))
	_, heldMsgID := postOverTracedDoor(t, ctx, authorClient, w.channel, "held until I settle")

	// FIFO BARRIER, not a sleep: the fabric runs delivery callbacks serially and
	// in order, so a later HUMAN post (settled at post, dispatched at once) whose
	// deliver is observed proves the earlier agent post was already held. Same
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
	w.consumer.OnSessionSettled(authorSess, compassv1.AgentSessionState_AGENT_SESSION_STATE_READY, 0)
	waitForControlDelivers(t, w.runner, recipSess, 2)

	want := originServerSpan(t, exp.GetSpans(), compassv1connect.CommsServicePostMessageProcedure).SpanContext.TraceID()
	op := tracedOpFor(t, w.runner, recipSess, heldMsgID)
	if got := traceIDOfTraceparent(t, ctx, "the settled deliver", op.traceparent); got != want {
		t.Fatalf("settled deliver trace id = %s, want the agent post's origin %s — continuity was lost across the settle edge", got, want)
	}
}

func traceContinuityF(t *testing.T) {
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
}

func traceContinuityG(t *testing.T) {
	ctx := context.Background() // test root (rule://go-thread-context _test.go exemption)
	exp := installGlobalSpanExporter(t)
	w := newMentionE2EWire(t)

	recip := w.seedAgentMember(t, "askrecip", true)
	const recipSess = "sess-askrecip-1"
	bringSessionLive(t, w, exp, recip.ID, containerFor("askrecip"), recipSess)

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
	if _, err := client.RespondToAsk(ctx, &compassv1.RespondToAskRequest{
		AskId:   askID,
		Answers: []*compassv1.AskQuestionAnswer{{QuestionId: "q1", ChosenOptionIds: []string{"opt-a"}}},
	}); err != nil {
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
}

func traceContinuityH(t *testing.T) {
	ctx := context.Background() // test root (rule://go-thread-context _test.go exemption)
	exp := installGlobalSpanExporter(t)
	w := newMentionE2EWire(t)

	author := w.seedAgentMember(t, "relayauthor", true)
	const authorSess = "sess-relayauthor-1"
	bringSessionLive(t, w, exp, author.ID, containerFor("relayauthor"), authorSess)

	// The door is built HERE, after the exporter: otelconnect captures the
	// global tracer provider once at NewServerInterceptor().
	relay := newRelayRunnerClient(t, w.hub, w.store)
	msgID := relayAgentPost(t, ctx, relay, authorSess, w.channelName, "agent speaks first", "")

	origin := relayOriginSpanForMessage(t, exp.GetSpans(), msgID)
	// A fresh root is a statement about the recorded PARENT, not about trace
	// ids: a trace-id comparison can pass by accident (two independent roots
	// differ trivially), while a nested span would carry a valid parent.
	if origin.Parent.IsValid() {
		t.Fatalf("RelayCommsCall origin span parent = %s (trace %s), want NO valid parent — the agent-authored post's trace must start at this door, not continue an inbound one",
			origin.Parent.SpanID(), origin.Parent.TraceID())
	}
	// The fixture must be the shipped topology: the client interceptor
	// propagates a traceparent that otelconnect turns into one transport link.
	// Drop it and the parent check above passes vacuously, so this keeps it
	// honest. No trigger_traceparent, so the count isolates the transport link.
	if len(origin.Links) != 1 {
		t.Fatalf("origin span carries %d links, want 1 (otelconnect's transport link) — no traceparent reached the door, so this is not the shipped topology and the fresh-root assertion above is vacuous", len(origin.Links))
	}
	if !origin.SpanContext.TraceID().IsValid() {
		t.Fatal("RelayCommsCall origin span has an invalid trace id; it recorded nothing, so 'fresh root' would be vacuous")
	}
}

func traceContinuityI(t *testing.T) {
	ctx := context.Background() // test root (rule://go-thread-context _test.go exemption)
	exp := installGlobalSpanExporter(t)
	w := newMentionE2EWire(t)

	author := w.seedAgentMember(t, "linkauthor", true)
	const authorSess = "sess-linkauthor-1"
	bringSessionLive(t, w, exp, author.ID, containerFor("linkauthor"), authorSess)

	// A synthetic-but-VALID remote trigger context, parsed through the one
	// shipped W3C helper (never string-sliced) so trigger names exactly what
	// production's linkTrigger will parse out of the same bytes.
	const triggerTP = "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01"
	trigger := spanContextOfTraceparent(t, ctx, "the synthetic trigger", triggerTP)

	relay := newRelayRunnerClient(t, w.hub, w.store)
	linkedMsgID := relayAgentPost(t, ctx, relay, authorSess, w.channelName, "reply to the trigger", triggerTP)
	// The negative control, same fixture and same door: no trigger at all.
	plainMsgID := relayAgentPost(t, ctx, relay, authorSess, w.channelName, "unprompted post", "")

	spans := exp.GetSpans()
	linked := relayOriginSpanForMessage(t, spans, linkedMsgID)

	// Half 1 — TERMINATION: the reply is a new trace, not a continuation.
	if got := linked.SpanContext.TraceID(); got == trigger.TraceID() {
		t.Fatalf("reply span trace id = %s, want a trace DIFFERENT from the trigger's %s — the reply NESTED into the trigger's trace instead of terminating it", got, trigger.TraceID())
	}
	// And not a child by any other route: a link is not a parent.
	if linked.Parent.IsValid() {
		t.Fatalf("reply span parent = %s, want none — a trigger_traceparent must produce a LINK, never a parent", linked.Parent.SpanID())
	}

	// Half 2 — the causal edge is recorded, pointing at the trigger. Selected
	// BY ATTRIBUTE, never by position: otelconnect's server branch mints its own
	// link from the inbound transport context, so this span legitimately carries
	// more than one link.
	link := triggerLinkOf(t, linked)
	if link.TraceID() != trigger.TraceID() || link.SpanID() != trigger.SpanID() {
		t.Fatalf("reply span trigger link = (trace %s, span %s), want the trigger's (trace %s, span %s)",
			link.TraceID(), link.SpanID(), trigger.TraceID(), trigger.SpanID())
	}

	// The negative: with no trigger_traceparent there is no CROSS-TURN link.
	// Without this, half 2 would also pass against an implementation that
	// links unconditionally to something. Counted by attribute, so
	// otelconnect's transport link does not mask the assertion.
	plain := relayOriginSpanForMessage(t, spans, plainMsgID)
	if n := countTriggerLinks(plain); n != 0 {
		t.Fatalf("a post with an EMPTY trigger_traceparent carries %d cross-turn trigger links, want 0 (links=%+v)", n, plain.Links)
	}
}

func traceContinuityJ(t *testing.T) {
	ctx := context.Background() // test root (rule://go-thread-context _test.go exemption)
	// The meter provider MUST precede the wire: the counter is created once at
	// delivery.NewConsumer from the then-global meter.
	reader := installGlobalMetricReader(t)
	exp := installGlobalSpanExporter(t)
	w := newMentionE2EWire(t)

	// One recipient of each op kind: a MENTIONED member is steered, a plain
	// subscriber is delivered to — so both label values are exercised.
	steered := w.seedAgentMember(t, "metricsteer", true)
	delivered := w.seedAgentMember(t, "metricdeliver", true)
	const steerSess = "sess-metricsteer-1"
	const deliverSess = "sess-metricdeliver-1"
	bringSessionLive(t, w, exp, steered.ID, containerFor("metricsteer"), steerSess)
	bringSessionLive(t, w, exp, delivered.ID, containerFor("metricdeliver"), deliverSess)

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
}
