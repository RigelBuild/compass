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
//     installs a global SDK TracerProvider via sdktrace.WithSyncer onto an
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
// the CommsService handler over THIS wire's comms service, behind CommsService's
// real socket-door interceptor chain
// (serve.go:698-699: otelconnect first, then the trace-response interceptor),
// plus the ambient-identity pair that CompassService mounts on the same door
// (serve.go:708-710). This splice is the one deliberate departure from
// production: the shipped CommsService socket door mounts no actor interceptor
// and comms instead uses its bootstrap-admin actorFromContext fallback
// (serve.go:703-705; comms.go:760-765), so it cannot attribute an AGENT author,
// which assertion (e) requires. The interceptor ORDER still matches production.
// Parameterizing the ambient pair fabricates no privilege: auth.AmbientIdentity
// -> withCaller sets exactly the callerKey + comms.WithActor pair that the
// network door's BearerInterceptor sets after resolving a real token
// (auth/interceptor.go:35-38 vs :64-68). That yields a real otelconnect handler
// span, a real traceresponse header, and the wire's real bus -> consumer ->
// hub -> recordingRunner spine.
//
// WithSyncer's simple span processor removes the export-after-End race (it
// exports from OnEnd on the ending goroutine): a span is readable
// from exp.GetSpans() the moment it ends. It does NOT order End against a wire
// observation: a delivery.dispatch hop ends after DispatchControl returns
// (dispatch.go:373,390), while the frame reaches the fake Runner through a
// non-blocking enqueue and separate sender goroutine (router.go:243-248), so
// assertions reading hop spans need a FIFO barrier (as (b) now has). The
// origin handler span is not exposed to this problem because otelconnect ends
// it inside the interceptor before the client receives the response. Every wait
// remains event-gated on an observed wire fact via waitFor* helpers — never a
// sleep, never a retry loop, never an invented deadline.
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
// (h) and (i) drive the RunnerService door directly (newRelayRunnerClient): the
// agent-authored leg's origin span is otelconnect's RelayCommsCall handler span,
// and the cross-turn causal edge is the LINK executeCall's Post arm adds from
// the call's trigger_traceparent (linkTrigger, called from executeCall's Post
// arm in runnerhub/relay_comms.go).

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"runtime"
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

// serveTracedCommsDoor mounts the CommsService over commsSvc behind the
// PRODUCTION socket-door interceptor chain (serve.go:698-699,708-710) on an h2c
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

// newRelayRunnerClient mounts a SECOND RunnerService door over the wire's OWN
// hub — the production mount (runnerhub.NewMountedHandler: otelconnect
// outermost, then the bearer pair) — and returns a RunnerService client that
// mounts otelconnect outermost of the fake Runner's bearer, mirroring the real
// ServerLink client (internal/runner/runner.go:112-115), so a RelayCommsCall
// traverses a real door and runs under a real otelconnect handler span.
//
// The CLIENT interceptor is load-bearing, not decoration. otelconnect's client
// branch injects a traceparent request header, and its server branch then
// extracts it and — because trustRemote defaults false — mints the span with
// WithNewRoot + a link to that transport context (otelconnect interceptor.go:
// 110-117). Omit it and the door sees no inbound traceparent at all: the span
// is a root because nobody offered a parent, so (h)'s fresh-root assertion
// passes for a trivial reason, and the origin span's link set holds only what
// linkTrigger put there — a topology that never ships.
//
// A second door rather than a change to attachFakeRunner: that helper's door
// carries the recordingRunner's live Sessions stream, and RelayCommsCall needs
// none of it — the hub resolves session_id -> account from its own binding
// (runnerhub/relay_comms.go, Hub.RelayCommsCall), which bringSessionLive has
// already promoted. Mounting here also keeps this door's interceptor
// construction inside the subtest,
// AFTER installGlobalSpanExporter, which is the load-bearing half:
// otelconnect captures the global tracer provider once, at NewInterceptor()
// (otelconnect interceptor.go:56-60).
func newRelayRunnerClient(t *testing.T, hub *runnerhub.Hub, st *store.Store) compassv1internalconnect.RunnerServiceClient {
	t.Helper()
	otelIC, err := otelconnect.NewInterceptor()
	if err != nil {
		t.Fatalf("otelconnect.NewInterceptor: %v", err)
	}
	path, handler := runnerhub.NewMountedHandler(hub,
		func(ctx context.Context, presented string, want store.SubjectKind) (store.Subject, error) {
			return auth.ResolveToken(ctx, st, presented, want)
		}, nil, nil, otelIC)
	mux := http.NewServeMux()
	mux.Handle(path, handler)
	srv := httptest.NewUnstartedServer(mux)
	srv.Config.Protocols = cleartextHTTP2()
	srv.Start()
	t.Cleanup(srv.Close)

	tr := h2cTransport(func(ctx context.Context, network, addr string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, network, addr)
	})
	t.Cleanup(tr.CloseIdleConnections)
	clientIC, err := otelconnect.NewInterceptor()
	if err != nil {
		t.Fatalf("otelconnect.NewInterceptor (client): %v", err)
	}
	return compassv1internalconnect.NewRunnerServiceClient(
		&http.Client{Transport: tr}, srv.URL,
		connect.WithInterceptors(clientIC, runnerBearer(fakeRunnerToken)),
	)
}

// relayAgentPost issues ONE agent-initiated RelayCommsCall Post through client,
// under sessionID, carrying triggerTP as the request's trigger_traceparent (""
// for the no-trigger case), and returns the id of the message the post created.
// It fails on a transport error AND on the in-band CommsCallError variant — the
// arm the hub renders a tool failure into — so a call that never reached the
// Post arm can never pass for one that did.
//
// The post addresses the channel by NAME: PostAsAccountByName is the agent-tool
// entry executeCall's Post arm dispatches to, and it resolves the name within
// the AGENT's visible set (comms/agent_caller.go:201-227).
func relayAgentPost(t *testing.T, ctx context.Context, client compassv1internalconnect.RunnerServiceClient, sessionID, channelName, body, triggerTP string) string {
	t.Helper()
	resp, err := client.RelayCommsCall(ctx, connect.NewRequest(&compassv1internal.RelayCommsCallRequest{
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
	}))
	if err != nil {
		t.Fatalf("RelayCommsCall(post %q): %v", body, err)
	}
	result := resp.Msg.GetResult()
	if e := result.GetError(); e != nil {
		t.Fatalf("RelayCommsCall(post %q) rendered an in-band tool error {code=%q message=%q}; the Post arm did not complete", body, e.GetCode(), e.GetMessage())
	}
	msgID := result.GetPost().GetMessage().GetId()
	if msgID == "" {
		t.Fatalf("RelayCommsCall(post %q) returned no message id; the Post arm did not persist a message", body)
	}
	return msgID
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
//
// Returns only once this session's START-EDGE SWEEP has finished, which is what
// keeps the wire quiet for the assertions. hub.Start fires OnSessionStarted
// synchronously (runnerhub/relay_comms.go:75-76), but that only ENQUEUES the
// edge (delivery/settle.go:51-65); the sweep itself runs later, on the Run
// loop's OTHER select arm (delivery/consumer.go:355-357). Left ungated it can
// land AFTER the first post commits and deliver that message a SECOND time —
// the recordingRunner never acks, so the cursor never advances and sweepSession
// still sees it owed — and because sweepSession dispatches directly
// (settle.go:343) rather than through gatedDispatch, the duplicate carries no
// delivery.dispatch hop span and a different, fresh-rooted traceparent. That
// breaks tracedOpFor's exactly-one contract in (a)/(c)/(d)/(e)/(g) — this
// helper's callers that read by identity — and shifts EVERY position-based wire
// read, including (e):753 and (g):834, and (j), whose reads are positional only.
// (f) calls tracedOpFor but never this helper: it drives the start edge itself,
// so its swept deliver is the subject under test, not a contaminant.
// Gating here removes the race at its source instead of loosening the readers,
// which would let a swept op with the WRONG trace satisfy an assertion.
func bringSessionLive(t *testing.T, w *mentionE2EWire, exp *tracetest.InMemoryExporter, account store.AccountID, container, session string) {
	t.Helper()
	// Baseline BEFORE Start: the edge it enqueues is the one we wait on. exp is
	// required — every fixture here brings its session live while tracing is on,
	// including (d), which pins the no-op provider only afterwards.
	sweepsBefore := countSpansNamed(exp, startSweepSpanName)
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
	waitForStartSweep(t, exp, sweepsBefore+1)
}

// startSweepSpanName is the LAST of the three sweeps drainStarts runs per start
// edge (delivery/settle.go:134-142: sweepSession, sweepPins, then this one), so
// observing it end means the whole start edge for one session is drained.
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

// waitForStartSweep event-gates until at least n start-edge sweeps have ENDED.
// Counting is sound even with several live sessions because drainStarts pops
// the queue one edge at a time on a single goroutine (settle.go:124-134), so
// the nth completed sweep means n edges are fully drained. WithSyncer's exporter
// makes a span readable the moment it ends, and GetSpans locks internally.
// Same deadline + Gosched idiom as the other waiters: no sleep, no retry loop.
//
// The count is a proxy for "MY edge drained", and that proxy holds only because
// start edges in these fixtures are strictly sequential on the test goroutine:
// bringSessionLive is the only producer, and it is called before any post. The
// wake path cannot slip an extra edge in between, because promoteSession deletes
// the container binding as it promotes (runnerhub/relay_comms.go:65) and returns
// early when the lookup misses (:55-57), so a wake's re-Start on the same
// placement container promotes nothing. A future fixture that starts a session
// concurrently would break that precondition, which is why the wait below
// demands an EXACT count: an unaccounted interleaved edge then fails loudly
// instead of satisfying the wait and returning with my own edge still pending.
//
// delivery.sweep.owedMentions is also the only sweep name safe to count. The
// obvious alternative, delivery.sweep.session, is emitted by the bus-lag overrun
// path too (sweepAllLive), so its count is inflatable by something that is not a
// start edge.
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

// waitForDeliverOfMessage event-gates until sessionID has been pushed a deliver
// carrying messageID — an IDENTITY predicate, never a count or an index, so a
// duplicate dispatch of some OTHER message cannot satisfy it and cannot shift
// the awaited one off a fixed position. The start-edge sweep the wire enqueues
// is exactly such a duplicate source (consumer.go:355-357 drains it on the Run
// loop's other select arm, and the recordingRunner never acks, so
// sweepSession's owed set still holds the message). Same deadline primitive and
// Gosched yield as the spine's waitFor* helpers: no sleep, no retry loop.
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

// traceIDOfTraceparent parses a W3C traceparent through the shipped
// otel.ContextWithTraceparent helper and returns its trace id, failing when the
// string is empty or does not parse to a VALID span context. Parsing (never
// string-slicing) is what makes the assertion a statement about the real W3C
// grammar the agent seam carries.
func traceIDOfTraceparent(t *testing.T, ctx context.Context, what, tp string) trace.TraceID {
	t.Helper()
	return spanContextOfTraceparent(t, ctx, what, tp).TraceID()
}

// spanContextOfTraceparent is traceIDOfTraceparent's whole-span-context form:
// (i) asserts a link's SPAN id as well as its trace id, so it needs both halves
// of the parsed remote context, and it parses through the same one shipped
// helper.
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

// relayOriginSpanForMessage returns the single SERVER-kind RelayCommsCall
// handler span that carries compass.message.id == msgID — the origin span of ONE
// agent-authored post relayed through the runner door.
//
// originServerSpan cannot serve here: it demands exactly one span of the
// procedure in the whole export, and (i) deliberately drives TWO relayed posts
// (the linked one and the empty-trigger control) into one fixture. The message id
// is the discriminator because comms stamps it on the CURRENT span at append
// (comms/comms.go:424) — which on this path IS the otelconnect RelayCommsCall
// handler span. Name + kind still gate the match, so a delivery hop span
// carrying the same id (delivery/dispatch.go:375) is never a candidate.
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

// triggerLinkOf returns the span context of the ONE cross-turn trigger link on
// span, selected by the compass.link.kind attribute linkTrigger stamps
// (runnerhub/relay_comms.go). Selecting by attribute rather than by index is
// required: otelconnect's server branch mints its own link from the inbound
// transport context, so the trigger link is neither the only nor reliably the
// first element of Links.
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
	// The attribute linkTrigger stamps on the cross-turn causal link
	// (runnerhub/relay_comms.go). Duplicated here rather than exported from
	// runnerhub: it is an OBSERVABLE contract a trace consumer selects on, so a
	// test that reads it through the same constant it is written from could not
	// catch a rename that breaks every existing consumer.
	triggerLinkKindKey   = attribute.Key("compass.link.kind")
	triggerLinkKindValue = "cross_turn_trigger"
)

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
		bringSessionLive(t, w, exp, recip.ID, containerFor("hoprecip"), recipSess)

		client := newTracedCommsClient(t, serveTracedCommsDoor(t, w.comms, w.adminID))
		_, msgID := postOverTracedDoor(t, ctx, client, w.channel, "every hop, one trace")

		waitForDeliverOfMessage(t, w.runner, recipSess, msgID)

		// FIFO BARRIER, not a sleep: `gatedDispatch` runs to completion on the
		// consumer's Run goroutine, ending its hop span as it returns
		// (dispatch.go:373, after the DispatchControl at :390) — and only then does
		// the loop take the next bus event. So observing a LATER post's deliver
		// proves the first dispatch already returned, which the first message's own
		// wire fact does not: the frame reaches the Runner through a non-blocking
		// enqueue plus a separate sender goroutine (router.go:243-248).
		//
		// Waited for BY IDENTITY, never by index. The start-edge sweep the wire's
		// bringSessionLive enqueues runs on the Run loop's other select arm
		// (consumer.go:355-357) and can deliver the first message a second time —
		// the recordingRunner never acks, so the cursor never advances and
		// sweepSession (settle.go:326-348) still sees it owed. That duplicate would
		// shift the barrier off any fixed index; its POSITION was never what
		// carried the happens-before, only its presence.
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
	})

	// (c) The traceresponse header on the PostMessage response carries the same
	// trace id — the UI/PostHog handle on the turn.
	t.Run("c: the traceresponse header carries the turn's trace id", func(t *testing.T) {
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
		// (d) brings the session live while tracing is still ON, so it can use the
		// same start-edge sweep gate as every other subtest, and only THEN pins the
		// global provider to no-op. Ordering matters and is load-bearing in both
		// directions: gating needs a recorded sweep span, and the disabled-path
		// claim needs the pin to precede everything it is a claim about. It does —
		// the door is built after the pin (otelconnect captures the global provider
		// at construction), and the post that carries the assertions happens after
		// that. Bringing the session live earlier changes nothing the assertions
		// read: they are about the POST's traceparent, its response header, and
		// that delivery still lands.
		exp := installGlobalSpanExporter(t)
		w := newMentionE2EWire(t)
		recip := w.seedAgentMember(t, "offrecip", true)
		const recipSess = "sess-offrecip-1"
		bringSessionLive(t, w, exp, recip.ID, containerFor("offrecip"), recipSess)

		// NOW pin the disabled state. Save/restore the globals but install NO SDK
		// provider — the shipped disabled state (mirroring
		// TestServerEmissionDisabledSetsNoTraceResponseHeader). The global is
		// PINNED to an explicit no-op provider rather than merely left alone, so
		// the assertion cannot be weakened by another test's leaked provider; that
		// pin is the established idiom at internal/runner/otel_test.go:98-100.
		// Cleanup composes under LIFO: this restore runs first and re-installs the
		// SDK provider, then installGlobalSpanExporter's own cleanup shuts it down
		// and restores the provider that was live before the subtest.
		prevTP := otel.GetTracerProvider()
		prevProp := otel.GetTextMapPropagator()
		otel.SetTracerProvider(noop.NewTracerProvider())
		t.Cleanup(func() {
			otel.SetTracerProvider(prevTP)
			otel.SetTextMapPropagator(prevProp)
		})
		// A recorder deliberately NOT made global, mirroring the house idiom at
		// internal/runner/otel_test.go:98-103: it is a catch-net, not a proof.
		// Nothing routes spans into a non-global provider, so its emptiness is
		// vacuous on its own (go/server/otel_emission_pgtest_test.go:184-186).
		// The real disabled-path proof is the empty traceparent, absent
		// traceresponse header, and delivery that still lands.
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
		if hdr := resp.Header().Get(traceResponseHdr); hdr != "" {
			t.Fatalf("disabled path set traceresponse = %q, want none", hdr)
		}
		if got := offExp.GetSpans(); len(got) != 0 {
			t.Fatalf("disabled path recorded %d spans, want 0", len(got))
		}
		if got := len(exp.GetSpans()); got != spansAtPin {
			t.Fatalf("disabled path added %d spans to the global-fed exporter (%d -> %d), want none — the no-op pin did not take effect", got-spansAtPin, spansAtPin, got)
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
		bringSessionLive(t, w, exp, author.ID, containerFor("heldauthor"), authorSess)
		bringSessionLive(t, w, exp, recip.ID, containerFor("heldrecip"), recipSess)

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

	// (h) An agent-authored post arrives over the RunnerService door, so its
	// origin span is the otelconnect RelayCommsCall handler span. This proves it
	// EXISTS (the door is traced at all) and that it is a FRESH ROOT — and the
	// root-ness is the door REFUSING an offered parent, not nobody offering one.
	// The Runner DOES propagate a traceparent (runner.go mounts otelconnect on
	// the ServerLink client, which this fixture mirrors); the span is a root
	// because otelconnect's trustRemote defaults false, so it mints the span
	// WithNewRoot plus a link to that transport context.
	t.Run("h: an agent-authored post's RelayCommsCall origin span is a fresh root", func(t *testing.T) {
		ctx := context.Background() // test root (rule://go-thread-context _test.go exemption)
		exp := installGlobalSpanExporter(t)
		w := newMentionE2EWire(t)

		author := w.seedAgentMember(t, "relayauthor", true)
		const authorSess = "sess-relayauthor-1"
		bringSessionLive(t, w, exp, author.ID, containerFor("relayauthor"), authorSess)

		// The door is built HERE, after the exporter: otelconnect captures the
		// global tracer provider once, at NewInterceptor().
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
		// The fixture must actually be the shipped topology: the client
		// interceptor propagates a traceparent, which otelconnect's server
		// branch turns into exactly one transport link. Drop the client
		// interceptor and the parent check above still passes — vacuously,
		// because nothing offered a parent — so this is what keeps that
		// assertion honest. The post carried no trigger_traceparent, so
		// linkTrigger added nothing and the count isolates the transport link.
		if len(origin.Links) != 1 {
			t.Fatalf("origin span carries %d links, want 1 (otelconnect's transport link) — no traceparent reached the door, so this is not the shipped topology and the fresh-root assertion above is vacuous", len(origin.Links))
		}
		if !origin.SpanContext.TraceID().IsValid() {
			t.Fatal("RelayCommsCall origin span has an invalid trace id; it recorded nothing, so 'fresh root' would be vacuous")
		}
	})

	// (i) TERMINATION. A reply whose call carries a trigger_traceparent starts a
	// NEW trace and merely LINKS back to the trigger — never nests under it,
	// which is what stops a conversation from growing one unbounded tree. Both
	// halves are asserted, plus the negative control (an EMPTY trigger adds no
	// link at all) that makes the positive non-vacuous.
	t.Run("i: a reply carrying a trigger_traceparent starts a NEW trace linked to the trigger", func(t *testing.T) {
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

		// Half 2 — the causal edge is recorded, and points at the trigger. The
		// trigger link is selected BY ATTRIBUTE, never by position or count:
		// otelconnect's server branch mints its own link from the inbound
		// transport context (the client interceptor above propagates one), so
		// this span legitimately carries more than one link.
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
	})

	// (j) The op-kind delivery counter increments — and, the load-bearing half,
	// carries NO per-session/per-channel label. A session or channel id here is
	// an unbounded-cardinality metric, the §Global Constraints hard rule.
	t.Run("j: the dispatch counter increments with the op kind as its only label", func(t *testing.T) {
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
	})
}
