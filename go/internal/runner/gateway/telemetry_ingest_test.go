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
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
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

// durableFrame builds an admitted durable frame for the generic durable-path
// tests (delivered-or-erred, retryability, dedup, advisory-set loss, eviction)
// that only need SOME frame PostConversationFrame accepts. Post-T7 the only
// admitted variant is transcript_entry, so this builds that; the tests assert
// forwarding/dedup behavior, not the frame's inner shape.
func durableFrame(text string) *compassv1internal.AgentFrame {
	return transcriptEntryFrame(text, false, 0)
}

// transcriptEntryFrame builds a durable transcript_entry AgentFrame — the
// SEA-1570 tee variant PostConversationFrame carries beside the conversation
// frames.
func transcriptEntryFrame(entryJSON string, checkpoint bool, seq uint64) *compassv1internal.AgentFrame {
	return &compassv1internal.AgentFrame{
		Frame: &compassv1internal.AgentFrame_TranscriptEntry{
			TranscriptEntry: &compassv1internal.TranscriptEntry{
				EntryJson:  entryJSON,
				Checkpoint: checkpoint,
				EntrySeq:   seq,
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

// --- fake ConversationCommitter ----------------------------------------------

// capturedCommit records one CommitConversationFrame call: the session id the
// Runner resolved, the frame it forwarded verbatim, and the idempotency key.
type capturedCommit struct {
	sessionID      string
	frame          *compassv1internal.AgentFrame
	idempotencyKey string
}

// fakeCommitter is the durable path's ConversationCommitter under test. It
// records every commit call and returns a canned (resp, err) — the Runner is a
// pure forwarder, so a test drives the Server's retryability-split Connect status
// by setting err and asserts the Runner passes it straight through. Mutex-guarded
// because the concurrency test drives it from several goroutines at once (mirrors
// fakeRelay's guarded recorder).
type fakeCommitter struct {
	mu    sync.Mutex
	calls []capturedCommit
	// err, when non-nil, is returned from every CommitConversationFrame call
	// (models the Server erring). resp is returned on success; a nil resp is
	// filled with an empty response so a caller never dereferences nil.
	err  error
	resp *compassv1internal.CommitConversationFrameResponse
}

func (f *fakeCommitter) CommitConversationFrame(_ context.Context, req *connect.Request[compassv1internal.CommitConversationFrameRequest]) (*connect.Response[compassv1internal.CommitConversationFrameResponse], error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, capturedCommit{
		sessionID:      req.Msg.GetSessionId(),
		frame:          req.Msg.GetFrame(),
		idempotencyKey: req.Msg.GetIdempotencyKey(),
	})
	if f.err != nil {
		return nil, f.err
	}
	resp := f.resp
	if resp == nil {
		resp = &compassv1internal.CommitConversationFrameResponse{}
	}
	return connect.NewResponse(resp), nil
}

// snapshot returns a copy of the recorded commit calls under the lock.
func (f *fakeCommitter) snapshot() []capturedCommit {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]capturedCommit(nil), f.calls...)
}

// count returns how many commit calls have been recorded.
func (f *fakeCommitter) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
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
	g := NewGateway(context.Background(), "cont-1", boundSessions(), nil, nil, events, nil)
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

// A transcript_entry frame (the SEA-1570 tee variant) rides the same durable
// unary as the conversation frames: it must reach the committer byte-identical
// under the request's session and idempotency key. Guards T7's guard widening.
// RED before the isConversationFrame widening: transcript_entry is rejected
// CodeInvalidArgument at :54 and never reaches the committer -> the count and
// verbatim-payload assertions go RED.
func TestPostConversationFrameForwardsTranscriptEntry(t *testing.T) {
	committer := &fakeCommitter{}
	g := NewGateway(context.Background(), "cont-1", boundSessions(), nil, nil, nil, committer)

	resp, err := g.PostConversationFrame(context.Background(), connect.NewRequest(&compassv1internal.PostConversationFrameRequest{
		Frame:          transcriptEntryFrame(`{"role":"assistant"}`, true, 7),
		IdempotencyKey: "key-te",
	}))
	if err != nil {
		t.Fatalf("PostConversationFrame: %v", err)
	}
	if resp == nil {
		t.Fatal("nil response on success")
	}

	calls := committer.snapshot()
	if len(calls) != 1 {
		t.Fatalf("commit calls = %d, want exactly 1 for one post", len(calls))
	}
	c := calls[0]
	if c.sessionID != "sess-1" {
		t.Fatalf("commit SessionId = %q, want sess-1 (the Runner sends the session it structurally owns)", c.sessionID)
	}
	if c.idempotencyKey != "key-te" {
		t.Fatalf("commit IdempotencyKey = %q, want key-te (must ride the request)", c.idempotencyKey)
	}
	te := c.frame.GetTranscriptEntry()
	if te == nil {
		t.Fatal("committed frame is not a transcript_entry — the tee variant was not forwarded verbatim")
	}
	if got := te.GetEntryJson(); got != `{"role":"assistant"}` {
		t.Fatalf("committed entry_json = %q, want verbatim", got)
	}
	if !te.GetCheckpoint() || te.GetEntrySeq() != 7 {
		t.Fatalf("committed checkpoint/seq = %v/%d, want true/7 (verbatim)", te.GetCheckpoint(), te.GetEntrySeq())
	}
}

// --- Case 3 ------------------------------------------------------------------

// PostConversationFrame is delivered-or-erred: a commit failure surfaces as the
// Server's Connect error, never a silent success. The Runner is a pure forwarder,
// so it passes the Server's retryability-split status straight through (mirror
// Comms). Levered on a committer returning CodeUnavailable — the real commit
// failure the delivered-or-erred contract must surface.
// RED: swallow the commit error (return success without surfacing it) -> the
// unary wrongly returns success against a failing commit.
func TestPostConversationFrameDeliveredOrErred(t *testing.T) {
	committer := &fakeCommitter{err: connect.NewError(connect.CodeUnavailable, errors.New("server unreachable"))}
	g := NewGateway(context.Background(), "cont-1", boundSessions(), nil, nil, nil, committer)

	resp, err := g.PostConversationFrame(context.Background(), connect.NewRequest(&compassv1internal.PostConversationFrameRequest{
		Frame:          durableFrame("boom"),
		IdempotencyKey: "key-3",
	}))
	if err == nil {
		t.Fatal("expected an error when the commit fails, got success")
	}
	if resp != nil {
		t.Fatalf("expected nil response on failure, got %+v", resp)
	}
	if got := connect.CodeOf(err); got != connect.CodeUnavailable {
		t.Fatalf("error code = %v, want CodeUnavailable (the Server's status, passed straight through)", got)
	}
}

// THE durable-contract test (OQ-3 item 5): the retryability split, and that a
// transient commit failure leaves the idempotency key UN-committed so the sink
// retries the SAME key, while a success marks it committed so a retry
// short-circuits without a second commit. The old wedge test defended publisher
// teardown on the durable path, which no longer exists — the commit IS the
// durable boundary, so there is no buffer-accept-then-lose to express.
//
// RED: swallow the commit error -> a transient failure wrongly returns success.
// RED: mark the key committed on a transient failure -> the retry short-circuits
// (no second commit call) and a genuinely-uncommitted frame is silently dropped.
func TestPostConversationFrameAtLeastOnceRetryability(t *testing.T) {
	// A permanent failure surfaces terminal (the agent drops): CodeNotFound /
	// CodeInvalidArgument. A transient failure surfaces retryable
	// (CodeUnavailable). The Runner passes each straight through.
	for _, tc := range []struct {
		name string
		code connect.Code
	}{
		{"permanent-not-found", connect.CodeNotFound},
		{"permanent-invalid-argument", connect.CodeInvalidArgument},
		{"transient-unavailable", connect.CodeUnavailable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			committer := &fakeCommitter{err: connect.NewError(tc.code, errors.New("commit failed"))}
			g := NewGateway(context.Background(), "cont-1", boundSessions(), nil, nil, nil, committer)
			_, err := g.PostConversationFrame(context.Background(), connect.NewRequest(&compassv1internal.PostConversationFrameRequest{
				Frame:          durableFrame("f"),
				IdempotencyKey: "retry-key",
			}))
			if got := connect.CodeOf(err); got != tc.code {
				t.Fatalf("error code = %v, want %v (the Server's retryability-split status passed straight through)", got, tc.code)
			}
			// A failed commit must NOT mark the key committed: the sink retries
			// the SAME key, and the advisory fast-path must let it re-commit.
			if g.keyCommitted("retry-key") {
				t.Fatal("key marked committed after a commit FAILURE: a retry would wrongly short-circuit and the frame would be silently dropped")
			}
		})
	}

	// The retry after a transient failure re-commits under the SAME key (the
	// fast-path did not short-circuit), and once it succeeds the key IS marked so
	// a further retry short-circuits without a second commit.
	committer := &fakeCommitter{err: connect.NewError(connect.CodeUnavailable, errors.New("transient"))}
	g := NewGateway(context.Background(), "cont-1", boundSessions(), nil, nil, nil, committer)
	post := func() error {
		_, err := g.PostConversationFrame(context.Background(), connect.NewRequest(&compassv1internal.PostConversationFrameRequest{
			Frame:          durableFrame("f"),
			IdempotencyKey: "k",
		}))
		return err
	}

	if err := post(); connect.CodeOf(err) != connect.CodeUnavailable {
		t.Fatalf("first (transient) commit = %v, want CodeUnavailable", err)
	}
	// The retry must re-commit (a second call), because the transient failure
	// left the key un-committed.
	committer.mu.Lock()
	committer.err = nil // the Server recovers; the retry now succeeds.
	committer.mu.Unlock()
	if err := post(); err != nil {
		t.Fatalf("retry after transient failure = %v, want success", err)
	}
	if n := committer.count(); n != 2 {
		t.Fatalf("commit calls = %d, want 2 (the transient failure must NOT short-circuit the retry)", n)
	}
	if !g.keyCommitted("k") {
		t.Fatal("key not marked committed after a SUCCESSFUL commit: a legitimate retry would re-commit needlessly")
	}
	// A further retry short-circuits: the successful commit marked the key, so no
	// third commit call is made.
	if err := post(); err != nil {
		t.Fatalf("post-success retry = %v, want success (advisory short-circuit)", err)
	}
	if n := committer.count(); n != 2 {
		t.Fatalf("commit calls = %d, want still 2 (a committed key must short-circuit the retry)", n)
	}
}

// --- Case 4 ------------------------------------------------------------------

// THE load-bearing test for the publisher's critical section: several trace
// frames stream through the ordered publisher and arrive Runner-sequenced 1..N
// with no gap, dup, or reorder. Since the swap, durable conversation frames
// commit off this spine (CommitConversationFrame), so ONLY Publish drives the
// publisher — the concurrent-durable-unary arm is gone. The Publish client-stream
// is itself serial, but forward still allocates the seq and sends under a SINGLE
// critical section, so a slow Send can never let allocation order diverge from
// emission order (the false hub gap this guards).
// RED: split allocate-seq from Send (atomic incr then Send outside the lock) ->
// send order diverges from allocation order -> arrival order is not 1..N.
// Run under -race.
func TestConcurrencyNoFalseGap(t *testing.T) {
	capture := newCapturePublish()
	events := newRunnerServiceServer(t, capture)
	g := NewGateway(context.Background(), "cont-1", staticSessions{sessionID: "sess-1", ok: true}, nil, nil, events, nil)
	client := newAgentGatewayServer(t, g)

	const total = 20

	stream := client.Publish(context.Background())
	for i := range total {
		if err := stream.Send(&compassv1internal.PublishFrameRequest{Frame: traceFrame(fmt.Sprintf("t%d", i))}); err != nil {
			t.Fatalf("stream send %d: %v", i, err)
		}
	}
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

// seqSink is a RunnerService whose PublishEvents records only the RunnerSeq of
// every frame it receives, into an unbounded slice under a mutex. Deliberately
// NOT capturePublish: that fixture sends each frame to a bounded channel, so a
// handler blocks once the buffer fills, and a test opening several streams can
// park a handler forever — httptest.Server.Close then hangs the whole package in
// cleanup. An append can never block, so any number of concurrent streams drain
// to completion with no drainer goroutine at all.
type seqSink struct {
	compassv1internalconnect.UnimplementedRunnerServiceHandler

	mu   sync.Mutex
	seen []uint64
}

func (s *seqSink) PublishEvents(_ context.Context, stream *connect.ClientStream[compassv1internal.PublishEventsRequest]) (*connect.Response[compassv1internal.PublishEventsResponse], error) {
	for stream.Receive() {
		s.mu.Lock()
		s.seen = append(s.seen, stream.Msg().GetRunnerSeq())
		s.mu.Unlock()
	}
	if err := stream.Err(); err != nil {
		return nil, err
	}
	return connect.NewResponse(&compassv1internal.PublishEventsResponse{}), nil
}

// seqs returns a copy of what has been recorded so far.
func (s *seqSink) seqs() []uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]uint64(nil), s.seen...)
}

// THE guard for the counter's ownership, and it needs no race at all: the defect
// is purely sequential. Post, release, post — a counter owned by the publisher
// restarts, so the second frame is stamped 1 again.
//
// Asserting the exact sequence rather than merely the absence of a duplicate:
// a variant defect that restarts at a value which happens not to collide would
// pass a duplicates-only check. [1 2] pins the contract.
//
// RED: give newSessionPublisher its own &seqCounter{} instead of the Gateway's
// -> seqs = [1 1], and this fails every run.
func TestSequenceSurvivesPublisherReplacement(t *testing.T) {
	sink := &seqSink{}
	events := newRunnerServiceServer(t, sink)
	g := NewGateway(context.Background(), "cont-1", boundSessions(), nil, nil, events, nil)

	// Drive the publisher directly: since the swap, only Publish rides it, so the
	// counter-ownership guarantee is exercised through the publisher API itself
	// rather than a durable unary (which no longer touches the publisher).
	forward := func(txt string) {
		t.Helper()
		if err := g.acquirePublisher("sess-1").forward(traceFrame(txt)); err != nil {
			t.Fatalf("forward %q = %v, want success", txt, err)
		}
	}

	// Each release drops the publisher; the next forward builds a fresh one. The
	// sequence must continue across both swaps.
	forward("p1")
	if err := g.releasePublisher(); err != nil {
		t.Fatalf("release after p1 = %v", err)
	}
	forward("p2")
	if err := g.releasePublisher(); err != nil {
		t.Fatalf("release after p2 = %v", err)
	}

	got := sink.seqs()
	want := []uint64{1, 2}
	if !slices.Equal(got, want) {
		t.Fatalf("RunnerSeq sequence = %v, want %v: the counter must survive a publisher replacement", got, want)
	}
}

// A failed forward must not burn a sequence number. The counter is
// socket-lifetime, so an allocated-but-unsent number is a permanent hole, and
// the hub reads a skipped number as in-transit loss (runnerhub/hub.go:230) — so
// a durable frame correctly erring back to the agent for retry would make the
// Server report a loss that never happened.
//
// RED: drop the p.seq.rollback(seq) call in sessionPublisher.forward -> the
// delivered sequence is [1 3] and the failed frame's number is gone forever.
func TestFailedForwardDoesNotBurnASequenceNumber(t *testing.T) {
	sink := &seqSink{}
	live := newRunnerServiceServer(t, sink)
	g := NewGateway(context.Background(), "cont-1", boundSessions(), nil, nil, live, nil)

	forward := func(txt string) error {
		return g.acquirePublisher("sess-1").forward(traceFrame(txt))
	}

	if err := forward("ok-1"); err != nil {
		t.Fatalf("first forward = %v, want success", err)
	}

	// Point at a dead upstream so the next forward fails, then back at the live
	// one. The failure must leave no hole behind it.
	//
	// Release before the dead swap: repointing g.events does not touch the
	// publisher already holding an open stream to the previous upstream, and an
	// abandoned stream leaves its server-side handler receiving forever —
	// httptest's Server.Close then blocks on it at cleanup and the package times
	// out.
	if err := g.releasePublisher(); err != nil {
		t.Fatalf("release before the dead swap = %v", err)
	}
	g.events = newClosedRunnerServiceServer(t)
	if err := forward("dead-2"); err == nil {
		t.Fatal("forward against a dead upstream = success, want an error")
	}
	// Driving forward directly (unlike the Publish handler) does not auto-release
	// on failure, so drop the dead publisher explicitly before the live swap; its
	// close errors on the dead transport, which is expected and not actionable.
	_ = g.releasePublisher()
	g.events = live
	if err := forward("ok-3"); err != nil {
		t.Fatalf("forward after a failure = %v, want success", err)
	}
	if err := g.releasePublisher(); err != nil {
		t.Fatalf("final release = %v", err)
	}

	got := sink.seqs()
	want := []uint64{1, 2}
	if !slices.Equal(got, want) {
		t.Fatalf("delivered RunnerSeq = %v, want %v: the failed forward burned a number, which the hub reads as in-transit loss", got, want)
	}
}

// A SUPPLEMENTARY race check, not the guard. TestSequenceSurvivesPublisherReplacement
// above is the deterministic guard for the counter's ownership; this one adds the
// concurrent case the other sibling explicitly avoids — TestConcurrencyNoFalseGap
// closes its Publish stream only after every forward has returned, so
// releasePublisher can never run mid-forward there. This test lets it.
//
// Deliberately weaker as an assertion, because under a genuine race both the
// arrival count and the interleaving are nondeterministic: it can only claim that
// no seq arrived TWICE, which is possible only if two publishers stamped the same
// session independently. Its detection rate is correspondingly partial — measured
// 26 of 40 runs under -race with a per-publisher counter — so it must never be
// the only thing standing between the codebase and this defect. The sequential
// test detects the same defect 100% of the time with no race at all.
func TestReleaseDoesNotRestartTheSequence(t *testing.T) {
	sink := &seqSink{}
	events := newRunnerServiceServer(t, sink)
	g := NewGateway(context.Background(), "cont-1", boundSessions(), nil, nil, events, nil)

	// This test opens MORE than one upstream stream (the release closes the
	// first, a later forward opens a fresh one), which the shared capture
	// fixture is not built for: its send is blocking and its `ended` channel
	// signals once per handler return, consumed by a single receiver.
	// So this test owns its own sink — an unbounded recorder that can never
	// block a handler, which is the only property the assertion needs.
	//
	// What is asserted: a DUPLICATE RunnerSeq. Under a genuine race the arrival
	// count and interleaving are both nondeterministic, but a repeated seq is
	// possible only if two publishers stamped the same session independently.
	var (
		sinkMu sync.Mutex
		seen   = map[uint64]bool{}
		dup    uint64
	)
	record := func(seq uint64) {
		sinkMu.Lock()
		defer sinkMu.Unlock()
		if seen[seq] && dup == 0 {
			dup = seq
		}
		seen[seq] = true
	}

	const nForward = 24
	var wg sync.WaitGroup
	for i := range nForward {
		wg.Go(func() {
			// An error is legitimate here: a forward racing the release fails
			// cleanly rather than being silently dropped. What must never happen
			// is a SUCCESS carrying a restarted seq — so the error is not
			// actionable and is deliberately discarded.
			_ = g.acquirePublisher("sess-1").forward(traceFrame(fmt.Sprintf("r%d", i)))
		})
	}
	// Release WHILE the forwards are in flight — the case the sibling avoids. A
	// close error racing an in-flight forward is expected and not actionable.
	wg.Go(func() { _ = g.releasePublisher() })
	wg.Wait()
	// Drop the last publisher so every handler's Receive loop ends and its
	// httptest server can close in cleanup without parking.
	_ = g.releasePublisher()

	for _, seq := range sink.seqs() {
		record(seq)
	}
	sinkMu.Lock()
	defer sinkMu.Unlock()
	if dup != 0 {
		t.Fatalf("RunnerSeq %d arrived twice: the sequence restarted, so two publishers stamped the same session independently", dup)
	}
	if len(seen) == 0 {
		t.Fatal("no frames captured; the test cannot discriminate")
	}
}

// --- Case 5 ------------------------------------------------------------------

// Idempotency dedup: a key already committed in this process short-circuits a
// retry (the committed-but-response-lost case) — exactly ONE commit call, and
// BOTH unaries return success (advisory fast-path).
// RED: ignore committedKeys (skip the keyCommitted fast-path) -> the retry
// commits again -> a second commit call is recorded.
func TestPostConversationFrameDedupOnKey(t *testing.T) {
	committer := &fakeCommitter{}
	g := NewGateway(context.Background(), "cont-1", boundSessions(), nil, nil, nil, committer)

	post := func(key string) {
		t.Helper()
		if _, err := g.PostConversationFrame(context.Background(), connect.NewRequest(&compassv1internal.PostConversationFrameRequest{
			Frame:          durableFrame("dur"),
			IdempotencyKey: key,
		})); err != nil {
			t.Fatalf("post key=%q: %v", key, err)
		}
	}

	post("k1")
	post("k1") // committed-but-response-lost retry: must not re-commit.

	calls := committer.snapshot()
	if len(calls) != 1 {
		t.Fatalf("commit calls = %d, want exactly 1 (dedup on repeated key)", len(calls))
	}
	if calls[0].idempotencyKey != "k1" {
		t.Fatalf("commit key = %q, want k1", calls[0].idempotencyKey)
	}
}

// An EMPTY idempotency key never short-circuits: two empty-key posts both commit
// (there is nothing to dedup on).
// RED: drop the `key != ""` guard on the fast-path -> the second empty-key post
// short-circuits -> only one commit call is recorded.
func TestPostConversationFrameEmptyKeyNeverDedups(t *testing.T) {
	committer := &fakeCommitter{}
	g := NewGateway(context.Background(), "cont-1", boundSessions(), nil, nil, nil, committer)

	for range 2 {
		if _, err := g.PostConversationFrame(context.Background(), connect.NewRequest(&compassv1internal.PostConversationFrameRequest{
			Frame:          durableFrame("dur"),
			IdempotencyKey: "",
		})); err != nil {
			t.Fatalf("empty-key post: %v", err)
		}
	}

	if n := committer.count(); n != 2 {
		t.Fatalf("commit calls = %d, want 2 (an empty key must never dedup)", n)
	}
}

// A non-transcript frame fails closed CodeInvalidArgument BEFORE the commit RPC
// — post-T7 the durable unary carries only the SEA-1570 transcript_entry variant,
// so a trace/session frame, an empty AgentFrame with an unset oneof, and a nil
// frame must each be rejected without ever reaching the committer.
// RED: drop the isConversationFrame guard -> a non-conversation frame reaches the
// commit RPC -> the commit-count assertion goes RED.
func TestPostConversationFrameRejectsNonConversationFrame(t *testing.T) {
	cases := []struct {
		name  string
		frame *compassv1internal.AgentFrame
	}{
		{"trace/session frame", traceFrame("not durable")},
		{"empty frame, unset oneof", &compassv1internal.AgentFrame{}},
		{"nil frame", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			committer := &fakeCommitter{}
			g := NewGateway(context.Background(), "cont-1", boundSessions(), nil, nil, nil, committer)

			_, err := g.PostConversationFrame(context.Background(), connect.NewRequest(&compassv1internal.PostConversationFrameRequest{
				Frame:          tc.frame,
				IdempotencyKey: "k",
			}))
			if got := connect.CodeOf(err); got != connect.CodeInvalidArgument {
				t.Fatalf("PostConversationFrame code = %v, want CodeInvalidArgument", got)
			}
			if n := committer.count(); n != 0 {
				t.Fatalf("commit calls = %d, want 0: a rejected frame must never reach the commit RPC", n)
			}
		})
	}
}

// --- Case 6 ------------------------------------------------------------------

// Crash-atomicity / advisory-set-loss safety: the in-process committedKeys set is
// an advisory fast-path, NOT the durability boundary. Simulate advisory-set loss
// (a Runner crash) with a FRESH Gateway (empty committedKeys) sharing the same
// committer: re-posting the same key commits AGAIN, which is SAFE because the true
// at-most-once boundary is the store's atomic unique-constraint commit on the
// same idempotency_key (store AppendMessage clientRequestID) — exercised in the
// pgtest integration (integration_pgtest_test.go), not here.
// RED: rely on the in-process set for durability (e.g. a package-global set that
// outlives the Gateway) -> the fresh Gateway wrongly dedups and does NOT
// re-commit -> the second commit call is never recorded.
func TestPostConversationFrameAdvisorySetLossReforwards(t *testing.T) {
	committer := &fakeCommitter{}

	g1 := NewGateway(context.Background(), "cont-1", boundSessions(), nil, nil, nil, committer)
	if _, err := g1.PostConversationFrame(context.Background(), connect.NewRequest(&compassv1internal.PostConversationFrameRequest{
		Frame:          durableFrame("dur"),
		IdempotencyKey: "same-key",
	})); err != nil {
		t.Fatalf("g1 post: %v", err)
	}

	// Advisory-set loss: a fresh Gateway with an empty committedKeys map.
	g2 := NewGateway(context.Background(), "cont-1", boundSessions(), nil, nil, nil, committer)
	resp, err := g2.PostConversationFrame(context.Background(), connect.NewRequest(&compassv1internal.PostConversationFrameRequest{
		Frame:          durableFrame("dur"),
		IdempotencyKey: "same-key",
	}))
	if err != nil {
		t.Fatalf("g2 re-post: %v", err)
	}
	if resp == nil {
		t.Fatal("g2 re-post returned nil response")
	}

	calls := committer.snapshot()
	if len(calls) != 2 {
		t.Fatalf("commit calls = %d, want 2 (a fresh Gateway must re-commit; the advisory set is gone)", len(calls))
	}
	for i, c := range calls {
		if c.idempotencyKey != "same-key" {
			t.Fatalf("commit %d key = %q, want same-key", i, c.idempotencyKey)
		}
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
	g := NewGateway(context.Background(), "cont-1", boundSessions(), nil, nil, events, nil)
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
	committer := &fakeCommitter{}
	g := NewGateway(context.Background(), "cont-1", &fakeSessions{ok: false}, nil, nil, events, committer)
	client := newAgentGatewayServer(t, g)

	_, err := g.PostConversationFrame(context.Background(), connect.NewRequest(&compassv1internal.PostConversationFrameRequest{
		Frame:          durableFrame("nope"),
		IdempotencyKey: "k",
	}))
	if got := connect.CodeOf(err); got != connect.CodePermissionDenied {
		t.Fatalf("PostConversationFrame code = %v, want CodePermissionDenied", got)
	}
	if n := committer.count(); n != 0 {
		t.Fatalf("commit calls = %d, want 0: a frame must never commit under an unbound session", n)
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
	g := NewGateway(context.Background(), "cont-1", boundSessions(), nil, nil, events, nil)
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
	g := NewGateway(context.Background(), "cont-1", boundSessions(), nil, nil, events, nil)
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

// --- Case 12 (F3) ------------------------------------------------------------

// The publisher resets when the resolved session changes. A Gateway is retained
// for the container socket across Stop→Start, so a publisher opened for a prior
// session can linger; when the container rebinds to a new session, the next
// forward must open a FRESH publisher stamped with the new session id, never the
// stopped session's. Driven directly through the publisher API (only Publish
// rides it since the swap): the first forward stamps sess-1; after the resolver
// flips to sess-2 the second forward stamps sess-2.
// RED (pre-fix): drop the session-change reset in acquirePublisher (the
// `g.pub != nil && g.pub.sessionID != sessionID` replace-and-close block) — the
// existing publisher is reused and the second frame is stamped sess-1.
func TestPublisherResetsOnSessionChange(t *testing.T) {
	capture := newCapturePublish()
	events := newRunnerServiceServer(t, capture)
	sessions := &toggleSessions{sessionID: "sess-1"}
	g := NewGateway(context.Background(), "cont-1", sessions, nil, nil, events, nil)
	t.Cleanup(func() { _ = g.releasePublisher() })

	// acquirePublisher resolves the session the same way the Publish handler
	// does, so drive it with the resolver's current id.
	forward := func() {
		t.Helper()
		sessionID, _ := sessions.Session("cont-1")
		if err := g.acquirePublisher(sessionID).forward(traceFrame("f")); err != nil {
			t.Fatalf("forward under %q = %v, want success", sessionID, err)
		}
	}

	forward()
	if got := capture.recvFrame(t).GetSessionId(); got != "sess-1" {
		t.Fatalf("first frame SessionId = %q, want sess-1", got)
	}

	sessions.set("sess-2") // container rebound to a new session across Stop→Start.

	forward()
	if got := capture.recvFrame(t).GetSessionId(); got != "sess-2" {
		t.Fatalf("second frame SessionId = %q, want sess-2 (a fresh publisher must stamp the new session)", got)
	}
}

// --- Case 13 (F4) ------------------------------------------------------------

// committedKeys is a bounded LRU, not an unbounded set: an in-container agent
// cannot grow it without limit by emitting many distinct-key durable frames.
// Commit committedKeysMax+N distinct keys, then assert the earliest are evicted
// (keyCommitted==false) while the most-recent stay resident (keyCommitted==true),
// and that eviction is SAFE — a re-post of an evicted key re-commits (the
// fast-path missed; the store dedups on its own at-most-once boundary). Cheap: a
// few-thousand-over-the-bound Adds, no sleeps.
// RED (pre-fix): size the LRU unbounded — temporarily replace committedKeysMax in
// NewGateway's expirable.NewLRU(...) with a huge capacity (e.g. 1<<30, models the
// pre-fix unbounded map: never evicts) — the earliest key stays committed, so the
// eviction assertion goes RED (and the re-post short-circuits instead of
// re-committing, so no second commit call is recorded).
func TestCommittedKeysBounded(t *testing.T) {
	committer := &fakeCommitter{}
	g := NewGateway(context.Background(), "cont-1", boundSessions(), nil, nil, nil, committer)

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
	// re-commits. The committer records the re-commit under that key.
	if _, err := g.PostConversationFrame(context.Background(), connect.NewRequest(&compassv1internal.PostConversationFrameRequest{
		Frame:          durableFrame("re"),
		IdempotencyKey: key(0),
	})); err != nil {
		t.Fatalf("re-post of evicted key: %v", err)
	}
	calls := committer.snapshot()
	if len(calls) != 1 {
		t.Fatalf("commit calls = %d, want 1 (an evicted key must re-commit, not short-circuit)", len(calls))
	}
	if calls[0].idempotencyKey != key(0) {
		t.Fatalf("re-commit key = %q, want %q", calls[0].idempotencyKey, key(0))
	}
}
