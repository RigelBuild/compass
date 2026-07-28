//go:build unix

package gateway

// Hermetic suite for the telemetry-ingest handlers: Publish (client-stream
// trace/session frames), PostConversationFrame (durable unary), the shared
// ordered sessionPublisher, and the ack-routing seam to the ControlRouter.
// White-box (package gateway) so it drives the handlers and their seams
// directly, sleep-free: the capture server's PublishEvents handler pushes
// every forwarded frame onto a buffered channel, and recvFrame blocks on that
// channel with a fail-fast deadline — every assertion reads a fact the in-memory
// wire already produced, never an elapsed-time guess (rule://no-retries).
//
// The upstream (EventRelay) is a real in-memory RunnerService PublishEvents
// server (capturePublish) whose generated client is handed to NewGateway as
// events, so each forward terminates a real Connect client-stream. The inbound
// agent-side (Publish client-stream + the WithReadMaxBytes bound) is driven
// through a mounted AgentGateway h2c httptest server, mirroring Serve.

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"

	compassv1 "github.com/sealedsecurity/compass/go/gen/compass/v1"
	compassv1internal "github.com/sealedsecurity/compass/go/internal/gen/compass/v1"
	"github.com/sealedsecurity/compass/go/internal/gen/compass/v1/compassv1internalconnect"
)

// --- capture upstream (EventRelay) -------------------------------------------

// capturePublish is a real RunnerService whose PublishEvents handler captures
// every Runner-forwarded frame on a buffered channel, so a test asserts the
// exact frames + RunnerSeq the Runner emitted over a real wire. Replicated from
// internal/runner/helpers_test.go (a different package), extended with `ended`
// so a test can observe the upstream stream close at clean end.
type capturePublish struct {
	compassv1internalconnect.UnimplementedRunnerServiceHandler
	frames chan *compassv1internal.PublishEventsRequest
	// ended receives once per PublishEvents handler return (buffered, best-effort
	// non-blocking send) so the clean-close test can observe the upstream stream
	// closing without racing on a single-close channel across multiple streams.
	ended chan struct{}
}

func newCapturePublish() *capturePublish {
	return &capturePublish{
		frames: make(chan *compassv1internal.PublishEventsRequest, 64),
		ended:  make(chan struct{}, 8),
	}
}

func (c *capturePublish) PublishEvents(_ context.Context, stream *connect.ClientStream[compassv1internal.PublishEventsRequest]) (*connect.Response[compassv1internal.PublishEventsResponse], error) {
	for stream.Receive() {
		c.frames <- stream.Msg()
	}
	err := stream.Err()
	select {
	case c.ended <- struct{}{}:
	default:
	}
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&compassv1internal.PublishEventsResponse{}), nil
}

// recvFrame reads one captured frame with a fail-fast deadline (testTimeout is a
// safety net, never a synchronization device — the assertion event-gates on the
// frame actually forwarded).
func (c *capturePublish) recvFrame(t *testing.T) *compassv1internal.PublishEventsRequest {
	t.Helper()
	select {
	case f := <-c.frames:
		return f
	case <-time.After(testTimeout):
		t.Fatal("timed out waiting for a forwarded PublishEvents frame")
		return nil
	}
}

// awaitEnded blocks until the capture server's PublishEvents handler has returned
// (its Receive loop drained and the upstream stream closed). The handler pushes
// every frame onto c.frames BEFORE it signals ended, so once ended fires the
// c.frames channel holds exactly the frames that were forwarded — no async wire
// race. Used to make a "how many forwards?" count deterministic.
func (c *capturePublish) awaitEnded(t *testing.T) {
	t.Helper()
	select {
	case <-c.ended:
	case <-time.After(testTimeout):
		t.Fatal("timed out waiting for the upstream PublishEvents stream to close")
	}
}

// drainCount returns how many frames the capture server holds right now. Call
// only after awaitEnded, when the count is settled.
func (c *capturePublish) drainCount() int { return len(c.frames) }

// --- h2c transport helpers ---------------------------------------------------
// cleartextHTTP2 lives in socket.go (production); testTimeout in socket_test.go.
// Both are reused, never redefined.

func h2cHTTPClient(t *testing.T) *http.Client {
	t.Helper()
	p := new(http.Protocols)
	p.SetUnencryptedHTTP2(true)
	tr := &http.Transport{
		Protocols: p,
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, network, addr)
		},
	}
	t.Cleanup(tr.CloseIdleConnections)
	return &http.Client{Transport: tr}
}

// newRunnerServiceServer mounts a RunnerService handler on an h2c httptest
// server and returns a live generated client — the EventRelay handed to
// NewGateway. Torn down via t.Cleanup.
func newRunnerServiceServer(t *testing.T, svc compassv1internalconnect.RunnerServiceHandler) compassv1internalconnect.RunnerServiceClient {
	t.Helper()
	path, handler := compassv1internalconnect.NewRunnerServiceHandler(svc)
	mux := http.NewServeMux()
	mux.Handle(path, handler)
	srv := httptest.NewUnstartedServer(mux)
	srv.Config.Protocols = cleartextHTTP2()
	srv.Start()
	t.Cleanup(srv.Close)
	return compassv1internalconnect.NewRunnerServiceClient(h2cHTTPClient(t), srv.URL)
}

// newClosedRunnerServiceServer mounts a RunnerService handler on an h2c httptest
// server, then immediately closes the server so its transport is dead: every
// PublishEvents Send against the returned client fails. It models a genuine
// upstream forward failure (the Server unreachable), the deterministic lever the
// delivered-or-erred contract must surface — no request-ctx coincidence.
func newClosedRunnerServiceServer(t *testing.T) compassv1internalconnect.RunnerServiceClient {
	t.Helper()
	path, handler := compassv1internalconnect.NewRunnerServiceHandler(newCapturePublish())
	mux := http.NewServeMux()
	mux.Handle(path, handler)
	srv := httptest.NewUnstartedServer(mux)
	srv.Config.Protocols = cleartextHTTP2()
	srv.Start()
	url := srv.URL
	srv.Close() // transport dead: the upstream stream cannot be established.
	return compassv1internalconnect.NewRunnerServiceClient(h2cHTTPClient(t), url)
}

// newAgentGatewayServer mounts g's AgentGateway on an h2c httptest server with
// the production WithReadMaxBytes bound and returns a live generated client — the
// inbound agent-side door, mirroring Serve. Required for the client-stream input
// to Publish and for the real over-limit bound.
func newAgentGatewayServer(t *testing.T, g *Gateway) compassv1internalconnect.AgentGatewayClient {
	t.Helper()
	path, handler := compassv1internalconnect.NewAgentGatewayHandler(g, connect.WithReadMaxBytes(maxAgentMessageBytes))
	mux := http.NewServeMux()
	mux.Handle(path, handler)
	srv := httptest.NewUnstartedServer(mux)
	srv.Config.Protocols = cleartextHTTP2()
	srv.Start()
	t.Cleanup(srv.Close)
	return compassv1internalconnect.NewAgentGatewayClient(h2cHTTPClient(t), srv.URL)
}

// --- frame builders ----------------------------------------------------------

// traceFrame builds a session/trace AgentFrame carrying assistant text (mirrors
// relay_test.go's sessionFrame) — the loss-tolerable Publish-stream variant.
func traceFrame(text string) *compassv1internal.AgentFrame {
	return &compassv1internal.AgentFrame{
		Frame: &compassv1internal.AgentFrame_Session{
			Session: &compassv1internal.SessionFrame{
				TypedEvent: &compassv1.SessionEvent{
					Event: &compassv1.SessionEvent_AssistantText{
						AssistantText: &compassv1.SessionAssistantText{Text: text},
					},
				},
			},
		},
	}
}

// conversationFrame builds a durable conversation_posted AgentFrame — the only
// variant PostConversationFrame accepts.
func conversationFrame(text string) *compassv1internal.AgentFrame {
	return &compassv1internal.AgentFrame{
		Frame: &compassv1internal.AgentFrame_ConversationPosted{
			ConversationPosted: &compassv1.MessagePosted{
				Message: &compassv1.Message{
					Blocks: []*compassv1.MessageBlock{{Block: &compassv1.MessageBlock_Text{Text: text}}},
				},
			},
		},
	}
}

// controlAckFrame builds a ControlAck AgentFrame — a control-plane ack routed to
// the control lane, never relayed upstream.
func controlAckFrame(ackedSeq uint64, appliedAbove []uint64) *compassv1internal.AgentFrame {
	return &compassv1internal.AgentFrame{
		Frame: &compassv1internal.AgentFrame_ControlAck{
			ControlAck: &compassv1internal.ControlAck{AckedSeq: ackedSeq, AppliedAbove: appliedAbove},
		},
	}
}

// replayCompleteAckFrame builds a ReplayCompleteAck AgentFrame — a control-plane
// ack routed to the control lane, never relayed upstream.
func replayCompleteAckFrame() *compassv1internal.AgentFrame {
	return &compassv1internal.AgentFrame{
		Frame: &compassv1internal.AgentFrame_ReplayCompleteAck{
			ReplayCompleteAck: &compassv1internal.ReplayCompleteAck{},
		},
	}
}

// --- fake ControlRouter ------------------------------------------------------

// controlAckCall records one AckControl invocation for the ack-routing test.
type controlAckCall struct {
	sessionID    string
	ackedSeq     uint64
	appliedAbove []uint64
}

// fakeControlRouter records the acks the ingest path routes to the control lane,
// so a test asserts an ack was consumed off Publish and handed to the router
// (not relayed upstream). Mutex-guarded: the Publish handler routes from the
// server goroutine.
type fakeControlRouter struct {
	mu           sync.Mutex
	ackCalls     []controlAckCall
	releaseCalls []string
}

func (f *fakeControlRouter) AckControl(sessionID string, ackedSeq uint64, appliedAbove []uint64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ackCalls = append(f.ackCalls, controlAckCall{sessionID: sessionID, ackedSeq: ackedSeq, appliedAbove: appliedAbove})
}

func (f *fakeControlRouter) ReleaseReplayBarrier(sessionID string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.releaseCalls = append(f.releaseCalls, sessionID)
}

// boundSessions builds a resolver that binds one session id, ok=true.
func boundSessions() *fakeSessions { return &fakeSessions{sessionID: "sess-1", ok: true} }

// staticSessions is a stateless, concurrency-safe SessionForContainer: it records
// nothing, so many handler goroutines can resolve it under -race (the
// fakeSessions double mutates unguarded call-tracking fields, unsafe for the
// concurrency test which resolves from several goroutines at once).
type staticSessions struct {
	sessionID string
	ok        bool
}

func (s staticSessions) Session(string) (string, bool) { return s.sessionID, s.ok }

// toggleSessions is a stateful SessionForContainer whose resolved session id can
// be flipped between two values — modelling a container rebound to a new session
// across Stop→Start while the Gateway is retained. set() swaps the id under a
// mutex so a resolve from any goroutine is race-safe; the existing fakes bind one
// fixed id and cannot switch. ok is always true (a bound session).
type toggleSessions struct {
	mu        sync.Mutex
	sessionID string
}

func (s *toggleSessions) Session(string) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sessionID, true
}

func (s *toggleSessions) set(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sessionID = id
}

// --- Case 1 ------------------------------------------------------------------

// Publish forwards three trace frames Runner-sequenced 1,2,3, in order, under the
// bound session id, bodies verbatim. The single upstream stream preserves order,
// so the capture channel yields them in emission order.
// RED: drop `p.seq++` in sessionPublisher.forward -> RunnerSeq stays 0 -> the
// per-frame seq assertion goes RED.
func TestPublishOrdersThreeTraceFrames(t *testing.T) {
	capture := newCapturePublish()
	events := newRunnerServiceServer(t, capture)
	g := NewGateway(context.Background(), "cont-1", boundSessions(), nil, events)
	client := newAgentGatewayServer(t, g)

	texts := []string{"alpha", "bravo", "charlie"}
	stream := client.Publish(context.Background())
	for _, txt := range texts {
		if err := stream.Send(&compassv1internal.PublishFrameRequest{Frame: traceFrame(txt)}); err != nil {
			t.Fatalf("send %q: %v", txt, err)
		}
	}
	if _, err := stream.CloseAndReceive(); err != nil {
		t.Fatalf("close publish stream: %v", err)
	}

	for i, txt := range texts {
		f := capture.recvFrame(t)
		if got := f.GetRunnerSeq(); got != uint64(i+1) {
			t.Fatalf("frame %d: RunnerSeq = %d, want %d", i, got, i+1)
		}
		if got := f.GetSessionId(); got != "sess-1" {
			t.Fatalf("frame %d: SessionId = %q, want sess-1", i, got)
		}
		if got := f.GetFrame().GetSession().GetTypedEvent().GetAssistantText().GetText(); got != txt {
			t.Fatalf("frame %d: text = %q, want %q (body must ride verbatim)", i, got, txt)
		}
	}
}

// --- Case 2 ------------------------------------------------------------------

// PostConversationFrame forwards ONE durable frame Runner-sequenced, the
// idempotency key rides the PublishEventsRequest envelope, the body is verbatim,
// and the unary returns success.
// RED: drop the pub.forward call (or return success without forwarding) -> the
// capture server sees nothing -> recvFrame times out RED.
func TestPostConversationFrameForwardsSequencedWithKey(t *testing.T) {
	capture := newCapturePublish()
	events := newRunnerServiceServer(t, capture)
	g := NewGateway(context.Background(), "cont-1", boundSessions(), nil, events)

	resp, err := g.PostConversationFrame(context.Background(), connect.NewRequest(&compassv1internal.PostConversationFrameRequest{
		Frame:          conversationFrame("durable body"),
		IdempotencyKey: "key-2",
	}))
	if err != nil {
		t.Fatalf("PostConversationFrame: %v", err)
	}
	if resp == nil {
		t.Fatal("nil response on success")
	}

	// Close the shared upstream, then await the capture handler's return: every
	// forwarded frame is on the channel before `ended` fires, so the count is
	// settled — no async wire race.
	if err := g.releasePublisher(); err != nil {
		t.Fatalf("releasePublisher: %v", err)
	}
	capture.awaitEnded(t)
	if n := capture.drainCount(); n != 1 {
		t.Fatalf("forwards = %d, want exactly 1 for one post", n)
	}

	f := capture.recvFrame(t)
	if got := f.GetRunnerSeq(); got != 1 {
		t.Fatalf("RunnerSeq = %d, want 1", got)
	}
	if got := f.GetIdempotencyKey(); got != "key-2" {
		t.Fatalf("IdempotencyKey = %q, want key-2 (must ride the envelope)", got)
	}
	if got := f.GetFrame().GetConversationPosted().GetMessage().GetBlocks()[0].GetText(); got != "durable body" {
		t.Fatalf("body = %q, want %q (verbatim)", got, "durable body")
	}
}

// --- Case 3 ------------------------------------------------------------------

// PostConversationFrame is delivered-or-erred: a genuine upstream forward failure
// surfaces as a Connect error (CodeUnavailable), never a silent success. F1
// decoupled the shared upstream PublishEvents stream from any one request ctx (it
// now rides g.baseCtx, not the calling unary's ctx), so the old lever — a
// cancelled REQUEST ctx passed to PostConversationFrame — no longer fails the
// Send: it was testing a coincidence (the stream happened to share the request
// ctx), not the contract. Re-levered on a genuine failing upstream: a RunnerService
// server whose transport is dead (pre-closed), so every upstream Send fails
// against it. That is the real forward failure the delivered-or-erred contract
// must surface.
// RED: swallow the pub.forward error (return success without surfacing it) -> the
// unary wrongly returns success against the dead upstream.
func TestPostConversationFrameDeliveredOrErred(t *testing.T) {
	events := newClosedRunnerServiceServer(t)
	g := NewGateway(context.Background(), "cont-1", boundSessions(), nil, events)
	t.Cleanup(func() { _ = g.releasePublisher() })

	resp, err := g.PostConversationFrame(context.Background(), connect.NewRequest(&compassv1internal.PostConversationFrameRequest{
		Frame:          conversationFrame("boom"),
		IdempotencyKey: "key-3",
	}))
	if err == nil {
		t.Fatal("expected an error when the upstream forward fails, got success")
	}
	if resp != nil {
		t.Fatalf("expected nil response on failure, got %+v", resp)
	}
	if got := connect.CodeOf(err); got != connect.CodeUnavailable {
		t.Fatalf("error code = %v, want CodeUnavailable", got)
	}
}

// --- Case 4 ------------------------------------------------------------------

// THE load-bearing test for the shared-publisher critical section: a Publish
// client-stream and several PostConversationFrame unaries forward CONCURRENTLY
// through the one shared ordered publisher. Because forward allocates the seq and
// sends under a SINGLE critical section, allocation order == emission order, so
// the single ordered upstream delivers seqs 1..N with no gap, dup, or reorder —
// asserted in ARRIVAL order (a reorder is exactly the false hub gap this guards).
// RED: split allocate-seq from Send (atomic incr then Send outside the lock) ->
// send order diverges from allocation order -> arrival order is not 1..N.
// Run under -race.
func TestConcurrencyNoFalseGap(t *testing.T) {
	capture := newCapturePublish()
	events := newRunnerServiceServer(t, capture)
	g := NewGateway(context.Background(), "cont-1", staticSessions{sessionID: "sess-1", ok: true}, nil, events)
	client := newAgentGatewayServer(t, g)

	const nStream = 10
	const nUnary = 10
	const total = nStream + nUnary

	stream := client.Publish(context.Background())

	var wg sync.WaitGroup
	// Concurrent Publish sender: nStream trace frames on the client-stream.
	wg.Go(func() {
		for i := range nStream {
			if err := stream.Send(&compassv1internal.PublishFrameRequest{Frame: traceFrame(fmt.Sprintf("t%d", i))}); err != nil {
				t.Errorf("stream send %d: %v", i, err)
				return
			}
		}
	})
	// Concurrent durable unaries against the SAME Gateway (shared publisher).
	for i := range nUnary {
		wg.Go(func() {
			if _, err := g.PostConversationFrame(context.Background(), connect.NewRequest(&compassv1internal.PostConversationFrameRequest{
				Frame:          conversationFrame(fmt.Sprintf("c%d", i)),
				IdempotencyKey: fmt.Sprintf("ck-%d", i),
			})); err != nil {
				t.Errorf("post %d: %v", i, err)
			}
		})
	}
	wg.Wait()
	// Close the Publish stream only after every concurrent forward has returned,
	// so releasePublisher does not close the shared upstream mid-forward.
	if _, err := stream.CloseAndReceive(); err != nil {
		t.Fatalf("close publish stream: %v", err)
	}

	seqs := make([]uint64, 0, total)
	for range total {
		seqs = append(seqs, capture.recvFrame(t).GetRunnerSeq())
	}
	for i, s := range seqs {
		if s != uint64(i+1) {
			t.Fatalf("arrival %d: RunnerSeq = %d, want %d (allocation order must equal emission order); full arrival sequence = %v", i, s, i+1, seqs)
		}
	}
}

// --- Case 5 ------------------------------------------------------------------

// Idempotency dedup: a key already committed in this process short-circuits a
// retry (the committed-but-response-lost case) — exactly ONE forward, and BOTH
// unaries return success (advisory fast-path).
// RED: ignore committedKeys (skip the keyCommitted fast-path) -> the retry
// forwards again -> a second frame is captured.
func TestPostConversationFrameDedupOnKey(t *testing.T) {
	capture := newCapturePublish()
	events := newRunnerServiceServer(t, capture)
	g := NewGateway(context.Background(), "cont-1", boundSessions(), nil, events)

	post := func(key string) {
		t.Helper()
		if _, err := g.PostConversationFrame(context.Background(), connect.NewRequest(&compassv1internal.PostConversationFrameRequest{
			Frame:          conversationFrame("dur"),
			IdempotencyKey: key,
		})); err != nil {
			t.Fatalf("post key=%q: %v", key, err)
		}
	}

	post("k1")
	post("k1") // committed-but-response-lost retry: must not re-forward.

	// Settle the count deterministically: close upstream, await the handler's
	// return, then assert exactly one frame was forwarded.
	if err := g.releasePublisher(); err != nil {
		t.Fatalf("releasePublisher: %v", err)
	}
	capture.awaitEnded(t)
	if n := capture.drainCount(); n != 1 {
		t.Fatalf("forwards = %d, want exactly 1 (dedup on repeated key)", n)
	}
	if got := capture.recvFrame(t).GetIdempotencyKey(); got != "k1" {
		t.Fatalf("forward key = %q, want k1", got)
	}
}

// An EMPTY idempotency key never short-circuits: two empty-key posts both forward
// (there is nothing to dedup on).
// RED: drop the `key != ""` guard on the fast-path -> the second empty-key post
// short-circuits -> only one forward, and the second recvFrame times out RED.
func TestPostConversationFrameEmptyKeyNeverDedups(t *testing.T) {
	capture := newCapturePublish()
	events := newRunnerServiceServer(t, capture)
	g := NewGateway(context.Background(), "cont-1", boundSessions(), nil, events)
	t.Cleanup(func() { _ = g.releasePublisher() })

	for range 2 {
		if _, err := g.PostConversationFrame(context.Background(), connect.NewRequest(&compassv1internal.PostConversationFrameRequest{
			Frame:          conversationFrame("dur"),
			IdempotencyKey: "",
		})); err != nil {
			t.Fatalf("empty-key post: %v", err)
		}
	}

	f1 := capture.recvFrame(t)
	f2 := capture.recvFrame(t)
	if f1.GetRunnerSeq() == f2.GetRunnerSeq() {
		t.Fatalf("empty-key posts collapsed to one forward (both seq %d)", f1.GetRunnerSeq())
	}
}

// --- Case 6 ------------------------------------------------------------------

// Crash-atomicity / advisory-set-loss safety: the in-process committedKeys set is
// an advisory fast-path, NOT the durability boundary. Simulate advisory-set loss
// (a Runner crash) with a FRESH Gateway (empty committedKeys) sharing the same
// upstream: re-posting the same key forwards AGAIN, which is SAFE because the true
// at-most-once boundary is the store's atomic unique-constraint commit on the
// same idempotency_key (store AppendMessage clientRequestID) — exercised in the
// pgtest integration (integration_pgtest_test.go), not here.
// RED: rely on the in-process set for durability (e.g. a package-global set that
// outlives the Gateway) -> the fresh Gateway wrongly dedups and does NOT
// re-forward -> the second recvFrame times out RED.
func TestPostConversationFrameAdvisorySetLossReforwards(t *testing.T) {
	capture := newCapturePublish()
	events := newRunnerServiceServer(t, capture)

	g1 := NewGateway(context.Background(), "cont-1", boundSessions(), nil, events)
	t.Cleanup(func() { _ = g1.releasePublisher() })
	if _, err := g1.PostConversationFrame(context.Background(), connect.NewRequest(&compassv1internal.PostConversationFrameRequest{
		Frame:          conversationFrame("dur"),
		IdempotencyKey: "same-key",
	})); err != nil {
		t.Fatalf("g1 post: %v", err)
	}
	if got := capture.recvFrame(t).GetIdempotencyKey(); got != "same-key" {
		t.Fatalf("g1 forward key = %q, want same-key", got)
	}

	// Advisory-set loss: a fresh Gateway with an empty committedKeys map.
	g2 := NewGateway(context.Background(), "cont-1", boundSessions(), nil, events)
	t.Cleanup(func() { _ = g2.releasePublisher() })
	resp, err := g2.PostConversationFrame(context.Background(), connect.NewRequest(&compassv1internal.PostConversationFrameRequest{
		Frame:          conversationFrame("dur"),
		IdempotencyKey: "same-key",
	}))
	if err != nil {
		t.Fatalf("g2 re-post: %v", err)
	}
	if resp == nil {
		t.Fatal("g2 re-post returned nil response")
	}
	if got := capture.recvFrame(t).GetIdempotencyKey(); got != "same-key" {
		t.Fatalf("g2 forward key = %q, want same-key (a fresh Gateway must re-forward; the advisory set is gone)", got)
	}
}

// --- Case 7 ------------------------------------------------------------------

// Ack routing: ControlAck and ReplayCompleteAck frames on the Publish stream are
// consumed by the ingest path and handed to the ControlRouter, NEVER relayed
// upstream. A trailing trace frame proves the stream kept flowing and is the only
// frame the capture server ever sees, at RunnerSeq 1 (acks consume no sequence).
// RED: remove the ControlAck/ReplayCompleteAck cases (relay them) -> the capture
// server receives an ack frame and the router is never called.
func TestPublishRoutesAcksToControlRouterNotUpstream(t *testing.T) {
	capture := newCapturePublish()
	events := newRunnerServiceServer(t, capture)
	g := NewGateway(context.Background(), "cont-1", boundSessions(), nil, events)
	router := &fakeControlRouter{}
	g.SetControlRouter(router) // set before serving: not concurrency-safe with a live stream.
	client := newAgentGatewayServer(t, g)

	stream := client.Publish(context.Background())
	if err := stream.Send(&compassv1internal.PublishFrameRequest{Frame: controlAckFrame(7, []uint64{9})}); err != nil {
		t.Fatalf("send control ack: %v", err)
	}
	if err := stream.Send(&compassv1internal.PublishFrameRequest{Frame: replayCompleteAckFrame()}); err != nil {
		t.Fatalf("send replay-complete ack: %v", err)
	}
	if err := stream.Send(&compassv1internal.PublishFrameRequest{Frame: traceFrame("real")}); err != nil {
		t.Fatalf("send trace: %v", err)
	}
	if _, err := stream.CloseAndReceive(); err != nil {
		t.Fatalf("close publish stream: %v", err)
	}

	// The only forwarded frame is the trace frame at seq 1 — acks were consumed.
	f := capture.recvFrame(t)
	if f.GetFrame().GetSession() == nil {
		t.Fatalf("forwarded frame variant = %T, want a session trace frame (acks must not be relayed)", f.GetFrame().GetFrame())
	}
	if got := f.GetRunnerSeq(); got != 1 {
		t.Fatalf("trace RunnerSeq = %d, want 1 (acks consume no sequence)", got)
	}
	select {
	case extra := <-capture.frames:
		t.Fatalf("an ack frame was relayed upstream: %+v", extra)
	default:
	}

	router.mu.Lock()
	defer router.mu.Unlock()
	if len(router.ackCalls) != 1 {
		t.Fatalf("AckControl calls = %d, want 1", len(router.ackCalls))
	}
	c := router.ackCalls[0]
	if c.sessionID != "sess-1" || c.ackedSeq != 7 || len(c.appliedAbove) != 1 || c.appliedAbove[0] != 9 {
		t.Fatalf("AckControl(%q, %d, %v), want (sess-1, 7, [9])", c.sessionID, c.ackedSeq, c.appliedAbove)
	}
	if len(router.releaseCalls) != 1 || router.releaseCalls[0] != "sess-1" {
		t.Fatalf("ReleaseReplayBarrier calls = %v, want [sess-1]", router.releaseCalls)
	}
}

// --- Case 8 ------------------------------------------------------------------

// No session bound to the container fails closed CodePermissionDenied on BOTH
// telemetry handlers, and nothing is forwarded upstream — a frame in the pre-Start
// window must never forward under an empty session id.
// RED: flip the `!ok || sessionID == ""` guard -> a frame forwards under an empty
// session id -> the "nothing forwarded" assertion goes RED.
func TestNoSessionFailsClosedBothHandlers(t *testing.T) {
	capture := newCapturePublish()
	events := newRunnerServiceServer(t, capture)
	g := NewGateway(context.Background(), "cont-1", &fakeSessions{ok: false}, nil, events)
	client := newAgentGatewayServer(t, g)

	_, err := g.PostConversationFrame(context.Background(), connect.NewRequest(&compassv1internal.PostConversationFrameRequest{
		Frame:          conversationFrame("nope"),
		IdempotencyKey: "k",
	}))
	if got := connect.CodeOf(err); got != connect.CodePermissionDenied {
		t.Fatalf("PostConversationFrame code = %v, want CodePermissionDenied", got)
	}

	stream := client.Publish(context.Background())
	// Send may not surface the rejection (flush timing); CloseAndReceive is
	// authoritative.
	_ = stream.Send(&compassv1internal.PublishFrameRequest{Frame: traceFrame("nope")})
	if _, perr := stream.CloseAndReceive(); connect.CodeOf(perr) != connect.CodePermissionDenied {
		t.Fatalf("Publish code = %v, want CodePermissionDenied", connect.CodeOf(perr))
	}

	select {
	case f := <-capture.frames:
		t.Fatalf("a frame forwarded under an unbound session: %+v", f)
	default:
	}
}

// --- Case 9 ------------------------------------------------------------------

// A clean Publish stream end closes the upstream PublishEvents stream (awaiting
// its ack) and returns the unary success. The capture handler's `ended` signal
// fires only when its Receive loop ends — i.e. the upstream stream was closed.
// RED: never call releasePublisher/close at clean stream end -> the upstream
// stream stays open, the capture handler never returns -> `ended` never fires.
func TestPublishCleanEndClosesUpstreamAndAwaitsAck(t *testing.T) {
	capture := newCapturePublish()
	events := newRunnerServiceServer(t, capture)
	g := NewGateway(context.Background(), "cont-1", boundSessions(), nil, events)
	client := newAgentGatewayServer(t, g)

	stream := client.Publish(context.Background())
	if err := stream.Send(&compassv1internal.PublishFrameRequest{Frame: traceFrame("x")}); err != nil {
		t.Fatalf("send: %v", err)
	}
	resp, err := stream.CloseAndReceive()
	if err != nil {
		t.Fatalf("Publish returned error, want success on clean end: %v", err)
	}
	if resp == nil {
		t.Fatal("nil Publish response on clean end")
	}
	_ = capture.recvFrame(t) // drain the one forwarded frame.

	select {
	case <-capture.ended:
	case <-time.After(testTimeout):
		t.Fatal("upstream PublishEvents stream was never closed at clean stream end")
	}
}

// --- Case 10 -----------------------------------------------------------------

// A message past the WithReadMaxBytes bound fails the Publish stream with a
// Connect error, not a silent accept — the retired stdout scanner's size cap is
// now this mount bound (a compromised agent cannot stream an unbounded message
// the Runner buffers).
// RED: drop connect.WithReadMaxBytes on the mount -> the oversized message is
// accepted and the stream closes successfully.
func TestPublishOverLimitFailsStream(t *testing.T) {
	capture := newCapturePublish()
	events := newRunnerServiceServer(t, capture)
	g := NewGateway(context.Background(), "cont-1", boundSessions(), nil, events)
	client := newAgentGatewayServer(t, g)

	huge := strings.Repeat("A", maxAgentMessageBytes+1024)
	stream := client.Publish(context.Background())
	// Send may not surface the read rejection (flush timing); CloseAndReceive is
	// authoritative.
	_ = stream.Send(&compassv1internal.PublishFrameRequest{Frame: traceFrame(huge)})
	if _, err := stream.CloseAndReceive(); err == nil {
		t.Fatal("oversized message was accepted; expected a Connect stream error from the WithReadMaxBytes bound")
	}
}

// --- Case 11 (F1) ------------------------------------------------------------

// A PostConversationFrame-created publisher survives the unary's return. The
// durable unary is the first forward, so it lazily opens the shared upstream
// stream; F1 binds that stream to g.baseCtx (the socket's lifetime), NOT the
// unary's request ctx, so cancelling the first unary's ctx AFTER it returns must
// not tear the shared stream down — a SECOND forward through the same Gateway
// still succeeds and the capture server sees BOTH frames. Run under -race.
// RED (pre-fix): give acquirePublisher a ctx param again and open with the
// caller's request ctx (newSessionPublisher(ctx, ...)) — cancelling the first
// unary's ctx then kills the shared stream, so the second forward fails.
func TestPostConversationFramePublisherSurvivesUnaryReturn(t *testing.T) {
	capture := newCapturePublish()
	events := newRunnerServiceServer(t, capture)
	g := NewGateway(context.Background(), "cont-1", boundSessions(), nil, events)
	t.Cleanup(func() { _ = g.releasePublisher() })

	// First durable post: opens the shared upstream against a request ctx we can
	// cancel once the unary has returned.
	ctx, cancel := context.WithCancel(context.Background())
	if _, err := g.PostConversationFrame(ctx, connect.NewRequest(&compassv1internal.PostConversationFrameRequest{
		Frame:          conversationFrame("first"),
		IdempotencyKey: "k1",
	})); err != nil {
		t.Fatalf("first post: %v", err)
	}
	cancel() // the first unary has returned; its request ctx dies here.

	// Second forward through the SAME Gateway: it rides the shared stream the
	// first post opened, which must have outlived the first request ctx.
	if _, err := g.PostConversationFrame(context.Background(), connect.NewRequest(&compassv1internal.PostConversationFrameRequest{
		Frame:          conversationFrame("second"),
		IdempotencyKey: "k2",
	})); err != nil {
		t.Fatalf("second post after first request ctx cancelled: %v", err)
	}

	// Settle: close the shared upstream, await the handler's return, then assert
	// BOTH frames were forwarded (count is settled once ended fires).
	if err := g.releasePublisher(); err != nil {
		t.Fatalf("releasePublisher: %v", err)
	}
	capture.awaitEnded(t)
	if n := capture.drainCount(); n != 2 {
		t.Fatalf("forwards = %d, want 2 (both frames survive the first unary's ctx cancel)", n)
	}
	got := map[string]bool{}
	for range 2 {
		got[capture.recvFrame(t).GetIdempotencyKey()] = true
	}
	if !got["k1"] || !got["k2"] {
		t.Fatalf("captured keys = %v, want both k1 and k2", got)
	}
}

// --- Case 12 (F3) ------------------------------------------------------------

// The publisher resets when the resolved session changes. A Gateway is retained
// for the container socket across Stop→Start, so a publisher opened for a prior
// session can linger; when the container rebinds to a new session, the next
// forward must open a FRESH publisher stamped with the new session id, never the
// stopped session's. The first forward stamps sess-1; after the resolver flips to
// sess-2 the second forward stamps sess-2.
// RED (pre-fix): drop the session-change reset in acquirePublisher (the
// `g.pub != nil && g.pub.sessionID != sessionID` replace-and-close block) — the
// existing publisher is reused and the second frame is stamped sess-1.
func TestPublisherResetsOnSessionChange(t *testing.T) {
	capture := newCapturePublish()
	events := newRunnerServiceServer(t, capture)
	sessions := &toggleSessions{sessionID: "sess-1"}
	g := NewGateway(context.Background(), "cont-1", sessions, nil, events)
	t.Cleanup(func() { _ = g.releasePublisher() })

	if _, err := g.PostConversationFrame(context.Background(), connect.NewRequest(&compassv1internal.PostConversationFrameRequest{
		Frame:          conversationFrame("a"),
		IdempotencyKey: "k1",
	})); err != nil {
		t.Fatalf("first post: %v", err)
	}
	if got := capture.recvFrame(t).GetSessionId(); got != "sess-1" {
		t.Fatalf("first frame SessionId = %q, want sess-1", got)
	}

	sessions.set("sess-2") // container rebound to a new session across Stop→Start.

	if _, err := g.PostConversationFrame(context.Background(), connect.NewRequest(&compassv1internal.PostConversationFrameRequest{
		Frame:          conversationFrame("b"),
		IdempotencyKey: "k2",
	})); err != nil {
		t.Fatalf("second post after session flip: %v", err)
	}
	if got := capture.recvFrame(t).GetSessionId(); got != "sess-2" {
		t.Fatalf("second frame SessionId = %q, want sess-2 (a fresh publisher must stamp the new session)", got)
	}
}

// --- Case 13 (F4) ------------------------------------------------------------

// committedKeys is a bounded LRU, not an unbounded set: an in-container agent
// cannot grow it without limit by emitting many distinct-key durable frames.
// Commit committedKeysMax+N distinct keys, then assert the earliest are evicted
// (keyCommitted==false) while the most-recent stay resident (keyCommitted==true),
// and that eviction is SAFE — a re-post of an evicted key re-forwards (the
// fast-path missed; the store dedups on its own at-most-once boundary). Cheap: a
// few-thousand-over-the-bound Adds, no sleeps.
// RED (pre-fix): size the LRU unbounded — temporarily replace committedKeysMax in
// NewGateway's expirable.NewLRU(...) with a huge capacity (e.g. 1<<30, models the
// pre-fix unbounded map: never evicts) — the earliest key stays committed, so the
// eviction assertion goes RED (and the re-post short-circuits instead of
// re-forwarding, timing out recvFrame).
func TestCommittedKeysBounded(t *testing.T) {
	capture := newCapturePublish()
	events := newRunnerServiceServer(t, capture)
	g := NewGateway(context.Background(), "cont-1", boundSessions(), nil, events)
	t.Cleanup(func() { _ = g.releasePublisher() })

	const overflow = 100
	total := committedKeysMax + overflow
	key := func(i int) string { return fmt.Sprintf("key-%d", i) }
	for i := range total {
		g.markKeyCommitted(key(i))
	}

	// The earliest keys (added first, never touched since) are the LRU victims
	// evicted past committedKeysMax.
	if g.keyCommitted(key(0)) {
		t.Fatalf("earliest key %q still committed; the bounded LRU must have evicted it", key(0))
	}
	// The most-recent key stays resident (the fast-path stays effective for the
	// recent retry window).
	if !g.keyCommitted(key(total - 1)) {
		t.Fatalf("newest key %q evicted; it must stay resident under the bound", key(total-1))
	}

	// Eviction is SAFE: a re-post of an evicted key misses the fast-path and
	// re-forwards. The capture server sees the re-forward under that key.
	if _, err := g.PostConversationFrame(context.Background(), connect.NewRequest(&compassv1internal.PostConversationFrameRequest{
		Frame:          conversationFrame("re"),
		IdempotencyKey: key(0),
	})); err != nil {
		t.Fatalf("re-post of evicted key: %v", err)
	}
	if got := capture.recvFrame(t).GetIdempotencyKey(); got != key(0) {
		t.Fatalf("re-forward key = %q, want %q (an evicted key must re-forward, not short-circuit)", got, key(0))
	}
}
