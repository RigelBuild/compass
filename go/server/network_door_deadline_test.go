//go:build unix

package server

// RIG-1298 — the network door's slow-body (slowloris) read deadline.
//
// withBodyReadDeadline is the outermost network-door middleware: for every
// request whose path is NOT in bodyDeadlineExempt it arms a per-request
// SetReadDeadline via http.ResponseController, so a client that sends headers
// promptly then drips the request body forever is cut instead of tying up a
// connection. The two long-lived-REQUEST-body Runner streams (Sessions,
// PublishEvents) are exempt, because their request half is legitimately open
// for the whole life of the connection.
//
// These tests exercise that contract through a real loopback httptest server
// (HTTP/1.1 over TCP; ResponseController.SetReadDeadline arms the underlying
// conn), driving the middleware itself — not a mock. Time is the subject under
// test, never a synchronization device: the enforced-path drip stalls
// UNBOUNDED (far past any fixed margin) so the socket deadline is guaranteed to
// fire, and the exempt-path pause is a time.Sleep used only as a guaranteed
// LOWER bound (sleeps run long, never short) that sits 4x past the deadline, so
// a wrongly-applied deadline would already have errored before the body
// completes. Every assertion gates on the real server-side read outcome
// delivered over a buffered channel, bounded by the shared testTimeout
// safety-net, never on elapsed wall-clock.
//
// DB-free and hermetic (httptest loopback, OS-assigned port, no Postgres), so
// it runs in the default `go test ./...` lane rather than behind the pgtest tag
// that network_door_test.go carries. White-box (package server) to reference
// the unexported withBodyReadDeadline and bodyDeadlineExempt.

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/RigelBuild/compass/go/events"
	compassv1 "github.com/RigelBuild/compass/go/gen/compass/v1"
	"github.com/RigelBuild/compass/go/gen/compass/v1/compassv1connect"
	"github.com/RigelBuild/compass/go/internal/gen/compass/v1/compassv1internalconnect"
)

// bodyReadResult is the outcome of the inner handler's full read of the request
// body: the bytes it managed to read and the terminal error (nil on a clean
// EOF, a timeout when the deadline cut the read). Delivered over a buffered
// channel so the handler goroutine never blocks on the send.
type bodyReadResult struct {
	body []byte
	err  error
}

// isBodyReadTimeout reports whether err is the socket read-deadline failure the
// middleware produces. net wraps a lapsed read deadline as os.ErrDeadlineExceeded
// inside a *net.OpError that satisfies net.Error with Timeout()==true; the http
// body reader may wrap it further, so both an errors.Is on the sentinel and a
// net.Error Timeout() check are accepted (either alone proves a timeout).
func isBodyReadTimeout(err error) bool {
	if errors.Is(err, os.ErrDeadlineExceeded) {
		return true
	}
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}

// startDeadlineDoor stands up a loopback httptest server whose handler is the
// production withBodyReadDeadline middleware wrapping an inner handler that
// fully reads r.Body (io.ReadAll) and reports the (bytes, error) outcome on the
// returned buffered channel. The middleware is the real one, so removing it or
// failing to arm the deadline changes the reported outcome — that is what makes
// the enforced-path test non-vacuous. The server is torn down in t.Cleanup.
func startDeadlineDoor(t *testing.T, timeout time.Duration) (*httptest.Server, <-chan bodyReadResult) {
	t.Helper()
	results := make(chan bodyReadResult, 1)
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		results <- bodyReadResult{body: body, err: err}
		// A response is written so the client's Do returns promptly; the test
		// asserts on the server-side read outcome above, not on this response.
		w.WriteHeader(http.StatusOK)
	})
	srv := httptest.NewServer(withBodyReadDeadline(inner, timeout))
	t.Cleanup(srv.Close)
	return srv, results
}

// driveDrip issues a chunked POST to path with a pipe-backed body and returns
// the writer the test drips into. The request runs in a goroutine (Do blocks
// until the server responds); cleanup closes the writer (unblocking any pending
// drip) and waits for that goroutine under the shared safety-net, so no client
// goroutine outlives the test — a requirement for a clean -race run. The
// pipe-backed body has unknown length, so the transport frames it as chunked
// and the server keeps reading until the terminating chunk, which the drip
// controls: exactly the "headers arrive, body stalls" shape under test.
func driveDrip(t *testing.T, srv *httptest.Server, path string) *io.PipeWriter {
	t.Helper()
	pr, pw := io.Pipe()
	req, err := http.NewRequest(http.MethodPost, srv.URL+path, pr)
	if err != nil {
		t.Fatalf("build drip request for %q: %v", path, err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		resp, derr := srv.Client().Do(req)
		if derr == nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
		}
	}()
	t.Cleanup(func() {
		_ = pw.Close()
		select {
		case <-done:
		case <-timeAfter():
			t.Error("client drip goroutine did not return before the cleanup safety-net deadline")
		}
	})
	return pw
}

// TestNetworkDoorBodyReadDeadlineCutsSlowBody is the core slowloris regression
// guard: a request to a NON-exempt path (IssueToken) sends one body byte
// promptly, then stalls the body indefinitely. With the middleware arming a
// short read deadline, the server-side io.ReadAll MUST fail with a timeout
// error once the deadline lapses. This reddens the instant the middleware is
// removed or the deadline is never set (the red-proof: swapping the body for
// `return next` lets the read block until cleanup, so the safety-net fires and
// the read either never returns a timeout or reports nil — either way this
// fails). The stall is unbounded (the writer only closes in cleanup), so the
// 100ms deadline is guaranteed to fire long before the testTimeout safety-net,
// with no racing sleep.
func TestNetworkDoorBodyReadDeadlineCutsSlowBody(t *testing.T) {
	t.Parallel()
	const deadline = 100 * time.Millisecond
	srv, results := startDeadlineDoor(t, deadline)
	// IssueToken is a bounded-body unary RPC — not in the exempt set, so the
	// deadline applies. Any non-exempt path (including "/") would do.
	pw := driveDrip(t, srv, compassv1connect.CompassServiceIssueTokenProcedure)

	if _, err := pw.Write([]byte("x")); err != nil {
		t.Fatalf("drip first byte: %v", err)
	}
	// Deliberately never write more: the body stalls until cleanup, so the read
	// deadline is the only thing that can end the server read.

	var res bodyReadResult
	select {
	case res = <-results:
	case <-timeAfter():
		t.Fatal("server body read did not return before the safety-net deadline: the read never observed the injected deadline (middleware absent or deadline unset)")
	}
	if res.err == nil {
		t.Fatalf("server body read err = nil after reading %d bytes, want a timeout error: the slow-body deadline did not cut the drip", len(res.body))
	}
	if !isBodyReadTimeout(res.err) {
		t.Fatalf("server body read err = %v (type %T), want os.ErrDeadlineExceeded or a net.Error with Timeout()==true", res.err, res.err)
	}
}

// TestNetworkDoorBodyReadDeadlineExemptPathCompletes proves the exemption: the
// SAME short-deadline server, driven on an EXEMPT path
// (RunnerService.Sessions), with a body that pauses well past the deadline and
// then completes, MUST read the full body with no error. If the deadline were
// wrongly applied to this path, the read would already have failed like the
// enforced case; that it completes shows withBodyReadDeadline genuinely skips
// exempt procedures. The pause is a time.Sleep used as a guaranteed lower bound
// (sleeps run long, never short) at 4x the deadline, so a regressed exemption
// fails deterministically — the deadline (if set) fires at ~100ms, long before
// the 400ms completion — while the passing path has ample margin.
func TestNetworkDoorBodyReadDeadlineExemptPathCompletes(t *testing.T) {
	t.Parallel()
	const deadline = 100 * time.Millisecond
	const pause = 400 * time.Millisecond // 4x the deadline: a wrongly-set deadline fires long before completion
	const want = "runner-session-frame"
	srv, results := startDeadlineDoor(t, deadline)
	pw := driveDrip(t, srv, compassv1internalconnect.RunnerServiceSessionsProcedure)

	if _, err := pw.Write([]byte(want[:1])); err != nil {
		t.Fatalf("drip first byte: %v", err)
	}
	// A slow client: pause longer than the deadline, then finish. On the exempt
	// path no deadline is armed, so the read must survive the pause.
	time.Sleep(pause) //nolint:forbidigo // the pause IS the timing subject under test: a slow client that outlasts the deadline; on the exempt path no deadline is armed, so the read must survive it (rule://go-no-sleep-in-test timing-under-test exemption)
	if _, err := pw.Write([]byte(want[1:])); err != nil {
		t.Fatalf("drip remainder: %v", err)
	}
	if err := pw.Close(); err != nil { // terminating chunk → io.ReadAll sees EOF
		t.Fatalf("close drip body: %v", err)
	}

	var res bodyReadResult
	select {
	case res = <-results:
	case <-timeAfter():
		t.Fatal("server body read did not return before the safety-net deadline on the exempt path")
	}
	if res.err != nil {
		t.Fatalf("exempt-path body read err = %v, want nil: the deadline was wrongly applied to an exempt Runner stream", res.err)
	}
	if string(res.body) != want {
		t.Fatalf("exempt-path body = %q, want %q: the slow body did not complete", res.body, want)
	}
}

// TestNetworkDoorBodyDeadlineExemptSet pins the exempt set's membership with no
// server and no timing: it must hold EXACTLY the two long-lived-REQUEST-body
// Runner streams (Sessions, PublishEvents) and nothing else. Each bounded-body
// probe documents a distinct intent — the server-streams SubscribeEvents /
// SubscribeComms (whose long-lived half is the RESPONSE, not the request) and
// unary IssueToken stay enforced, and RunnerService.Enroll is unary so it is
// deliberately NOT exempt despite its RunnerService streaming siblings. The len
// guard makes it exact: adding a bounded RPC to the exempt set (reopening the
// slowloris window) or dropping a Runner stream from it (cutting a legitimate
// long-lived request) reddens the matching row or the count.
func TestNetworkDoorBodyDeadlineExemptSet(t *testing.T) {
	t.Parallel()
	exempt := []struct{ name, procedure string }{
		{"RunnerService.Sessions (bidi; request stream carries command results upward for the connection's life)", compassv1internalconnect.RunnerServiceSessionsProcedure},
		{"RunnerService.PublishEvents (client-stream; the Runner drips agent event frames upward continuously)", compassv1internalconnect.RunnerServicePublishEventsProcedure},
	}
	bounded := []struct{ name, procedure string }{
		{"CompassService.SubscribeEvents (server-stream; long-lived half is the RESPONSE, request is bounded)", compassv1connect.CompassServiceSubscribeEventsProcedure},
		{"CompassService.IssueToken (unary; bounded request)", compassv1connect.CompassServiceIssueTokenProcedure},
		{"CommsService.SubscribeComms (server-stream; long-lived half is the RESPONSE, request is bounded)", compassv1connect.CommsServiceSubscribeCommsProcedure},
		{"RunnerService.Enroll (unary; deliberately NOT exempt despite its RunnerService streaming siblings)", compassv1internalconnect.RunnerServiceEnrollProcedure},
	}

	for _, tc := range exempt {
		t.Run("exempt/"+tc.name, func(t *testing.T) {
			t.Parallel()
			if _, ok := bodyDeadlineExempt[tc.procedure]; !ok {
				t.Errorf("bodyDeadlineExempt is missing %q (%s): a long-lived Runner request stream would be wrongly subject to the body-read deadline", tc.procedure, tc.name)
			}
		})
	}
	for _, tc := range bounded {
		t.Run("bounded/"+tc.name, func(t *testing.T) {
			t.Parallel()
			if _, ok := bodyDeadlineExempt[tc.procedure]; ok {
				t.Errorf("bodyDeadlineExempt wrongly contains %q (%s): a bounded-body RPC must stay subject to the slow-body deadline", tc.procedure, tc.name)
			}
		})
	}
	if len(bodyDeadlineExempt) != len(exempt) {
		t.Errorf("len(bodyDeadlineExempt) = %d, want %d (exactly the two long-lived Runner request streams): an unexpected entry is exempt", len(bodyDeadlineExempt), len(exempt))
	}
}

// TestNetworkDoorBodyDeadlineIsolatesHTTP2Streams is the per-STREAM-isolation
// proof for the middleware's central design claim: the read deadline is armed
// per HTTP/2 stream (Go's SetReadDeadline semantics on an h2 request body), so
// one slow request cannot disturb a sibling multiplexed on the SAME connection.
// The existing drip tests run over HTTP/1.1 (one request per conn), which cannot
// distinguish a per-stream deadline from a per-connection one; this test drives
// two concurrent POSTs down a single shared h2c connection and asserts the slow
// one is cut WHILE the prompt sibling completes cleanly. A per-connection
// deadline — the regression this guards — would cut the sibling too.
//
// Both requests use a NON-exempt path, so both arm a deadline; the drip stream's
// body stalls UNBOUNDED (its writer only closes in cleanup), so the socket read
// deadline is the only thing that can end its server-side read — time is the
// subject under test, never a sync device. Every assertion gates on the real
// per-stream server-side read outcome delivered over a buffered channel, bounded
// by the shared safety-net. Hermetic: h2c over loopback, OS-assigned port, no
// Postgres — the default `go test ./server/` lane.
func TestNetworkDoorBodyDeadlineIsolatesHTTP2Streams(t *testing.T) {
	t.Parallel()
	const deadline = 100 * time.Millisecond
	// A non-exempt path: both requests arm the deadline. The ?stream= query
	// keys the two concurrent streams' outcomes apart; r.URL.Path stays
	// non-exempt (queries do not affect the exempt lookup).
	const isolationPath = "/isolation"
	const wantB = "prompt-sibling-body"

	// Per-stream server-side read outcomes, buffered so the handler goroutines
	// never block on the send. startedA fires when the drip stream's handler is
	// live — i.e. its h2 stream (and thus the shared conn) is established — so
	// the sibling can be launched knowing it multiplexes onto the SAME conn
	// rather than racing a fresh dial.
	resA := make(chan bodyReadResult, 1)
	resB := make(chan bodyReadResult, 1)
	startedA := make(chan struct{}, 1)
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		stream := r.URL.Query().Get("stream")
		if stream == "A" {
			startedA <- struct{}{}
		}
		body, err := io.ReadAll(r.Body)
		switch stream {
		case "A":
			resA <- bodyReadResult{body: body, err: err}
		case "B":
			resB <- bodyReadResult{body: body, err: err}
		}
		w.WriteHeader(http.StatusOK)
	})

	// h2c server whose handler is the PRODUCTION middleware — mirrors
	// newH2CTestServer's h2c setup shape (unstarted → cleartext protocols →
	// Start → Cleanup) but wraps a custom inner handler.
	srv := httptest.NewUnstartedServer(withBodyReadDeadline(inner, deadline))
	srv.Config.Protocols = cleartextHTTP2()
	srv.Start()
	t.Cleanup(srv.Close)

	// One shared h2c connection: the dialer counts dials so the test can prove
	// both streams rode a SINGLE connection (dials == 1) — the premise that
	// makes this a per-STREAM (not per-connection) proof. h2 multiplexes
	// concurrent requests onto one conn; launching B only after A's handler is
	// live keeps that single-conn behaviour deterministic instead of racing a
	// second dial.
	var dials atomic.Int32
	tr := h2cTransport(func(ctx context.Context, network, addr string) (net.Conn, error) {
		dials.Add(1)
		var d net.Dialer
		return d.DialContext(ctx, network, addr)
	})
	t.Cleanup(tr.CloseIdleConnections)
	client := &http.Client{Transport: tr}

	// Request A: writes a few bytes then blocks forever (the pipe writer only
	// closes in cleanup), so the socket read deadline is the ONLY thing that can
	// end the server-side read.
	prA, pwA := io.Pipe()
	doneA := launchPost(t, client, srv.URL+isolationPath+"?stream=A", prA, resA)
	t.Cleanup(func() {
		_ = pwA.Close()
		select {
		case <-doneA:
		case <-timeAfter():
			t.Error("stream A client goroutine did not return before the cleanup safety-net deadline")
		}
	})

	if _, err := pwA.Write([]byte("drip")); err != nil {
		t.Fatalf("drip stream A first bytes: %v", err)
	}
	// Deliberately never write more: the body stalls until cleanup, so the read
	// deadline is the only thing that can end stream A's server read.

	select {
	case <-startedA:
	case <-timeAfter():
		t.Fatal("stream A handler never started: the drip request never reached the server")
	}

	// Request B: full body sent promptly and closed, on the SAME client/conn and
	// the SAME non-exempt path (so it also arms a deadline). It must complete
	// cleanly — a per-connection deadline would cut it alongside the slow A.
	doneB := launchPost(t, client, srv.URL+isolationPath+"?stream=B", strings.NewReader(wantB), resB)
	t.Cleanup(func() {
		select {
		case <-doneB:
		case <-timeAfter():
			t.Error("stream B client goroutine did not return before the cleanup safety-net deadline")
		}
	})

	// The sibling completes as soon as its short body is read, well before A's
	// deadline fires. If A's deadline leaked onto B (a per-connection deadline),
	// this read errors instead.
	var outB bodyReadResult
	select {
	case outB = <-resB:
	case <-timeAfter():
		t.Fatal("sibling stream B read never returned before the safety-net: a per-connection deadline may have cut it")
	}
	if outB.err != nil {
		t.Fatalf("sibling stream B read err = %v, want nil: the slow stream A's deadline leaked onto its sibling (deadline is per-connection, not per-stream)", outB.err)
	}
	if string(outB.body) != wantB {
		t.Fatalf("sibling stream B body = %q, want %q: the prompt sibling did not complete", outB.body, wantB)
	}

	// The slow stream MUST be cut by its own per-stream deadline.
	var outA bodyReadResult
	select {
	case outA = <-resA:
	case <-timeAfter():
		t.Fatal("slow stream A read never returned before the safety-net: the per-stream deadline did not fire on the h2 code path")
	}
	if outA.err == nil {
		t.Fatalf("slow stream A read err = nil after %d bytes, want a timeout: the deadline did not cut the drip", len(outA.body))
	}
	if !isBodyReadTimeout(outA.err) {
		t.Fatalf("slow stream A read err = %v (type %T), want os.ErrDeadlineExceeded or a net.Error with Timeout()==true", outA.err, outA.err)
	}

	// The co-occurrence above (A cut, B clean) is only a per-STREAM proof if
	// both rode ONE connection; otherwise a per-connection deadline could still
	// pass. Assert the shared conn.
	if got := dials.Load(); got != 1 {
		t.Fatalf("dial count = %d, want 1: the two requests did not share a single h2 connection, so this cannot distinguish per-stream from per-connection isolation", got)
	}
}

// launchPost fires a POST with the given body on its own goroutine and returns
// a channel closed when the client call returns. A request-build failure is
// reported on failResult (the per-stream outcome channel) so the test still
// observes it; the response body is drained and closed. Shared by the two
// isolation streams so the test body stays within funlen.
func launchPost(t *testing.T, client *http.Client, url string, body io.Reader, failResult chan<- bodyReadResult) <-chan struct{} {
	t.Helper()
	done := make(chan struct{})
	go func() {
		defer close(done)
		req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, url, body)
		if err != nil {
			failResult <- bodyReadResult{err: err}
			return
		}
		resp, derr := client.Do(req)
		if derr == nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
		}
	}()
	return done
}

// startServerStreamDeadlineDoor mounts the REAL compass.v1 CompassService
// handler wrapped in the PRODUCTION withBodyReadDeadline middleware on an h2c
// loopback server and returns its base URL. It mirrors newH2CTestServer's h2c
// setup (unstarted → cleartext protocols → Start → Cleanup) but injects a short
// deadline, so a non-exempt server-stream (SubscribeEvents) arms it — the exact
// seam the survival test drives, minus the production 30s wait. The mux is the
// same handler the door ships, so the middleware sees the real, non-exempt
// procedure path. Torn down in t.Cleanup.
func startServerStreamDeadlineDoor(t *testing.T, svc *service, timeout time.Duration) string {
	t.Helper()
	path, handler := compassv1connect.NewCompassServiceHandler(svc)
	mux := http.NewServeMux()
	mux.Handle(path, handler)
	srv := httptest.NewUnstartedServer(withBodyReadDeadline(mux, timeout))
	srv.Config.Protocols = cleartextHTTP2()
	srv.Start()
	t.Cleanup(srv.Close)
	return srv.URL
}

// TestNetworkDoorBodyDeadlineDoesNotCutServerStreamResponse is the invariant
// that makes the body-read deadline safe to apply to the server-streams
// (SubscribeEvents / SubscribeComms) that keep it: a NON-exempt server-stream's
// long-lived RESPONSE half must survive PAST the request-body read deadline. The
// deadline is scoped to the request-body read and released at body EOF — NOT
// armed on the whole connection/request — so a fast unary request followed by a
// long response write is not bounded by it.
//
// SubscribeEvents is non-exempt, so the middleware arms the short deadline on
// it. Its request is one bounded message; connect half-closes the request stream
// immediately, so net/http releases the deadline at request-body EOF. The test
// opens the stream, then waits 5x the deadline — a guaranteed LOWER bound (a
// time.Sleep runs long, never short), so the deadline would already have fired
// if it bounded the response — and only THEN publishes a live event. The
// subscriber must still receive it.
//
// The rejected design — a deadline that bounds the whole request/connection
// instead of releasing at body EOF — cuts this post-deadline frame: the
// verified teeth-proof re-arms the read deadline after net/http clears it and
// runs THIS exact assertion path (recvOne on the seq-2 frame) over HTTP/1.1,
// where recvOne fails with a false receive. Over the production h2 transport the
// response half is additionally immune (Go's h2 onReadTimeout closes only the
// request body, never the response stream), so this positive assertion pins the
// invariant that keeps SubscribeEvents/SubscribeComms safe on the deadline path.
//
// Time is the subject under test, never a sync device: the post-deadline wait is
// a lower bound and every stream advance gates on a real Receive bounded by the
// shared testTimeout safety-net. Hermetic: h2c over loopback, OS-assigned port,
// no Postgres — the default `go test ./server/` lane.
func TestNetworkDoorBodyDeadlineDoesNotCutServerStreamResponse(t *testing.T) {
	t.Parallel()
	const deadline = 200 * time.Millisecond
	const postDeadlineWait = 5 * deadline

	bus := events.NewBus[busPayload]()
	t.Cleanup(bus.Close)
	// Prime one event pre-subscribe: connect flushes the server-stream's
	// response headers on the handler's first Send, and the client's
	// SubscribeEvents RoundTrip blocks until those headers arrive — with an empty
	// bus the handler tails silently and the open would deadlock. Its snapshot
	// replay below is also the proof the stream opened and delivered a frame.
	bus.Publish(statusEvent())

	url := startServerStreamDeadlineDoor(t, newService("test", bus, nil, nil, nil, nil, nil), deadline)
	client := newH2CClient(t, url)

	stream := subscribe(t, client, &compassv1.SubscribeEventsRequest{SinceSeq: 0})

	// A since_seq==0 subscribe leads with the snapshot-boundary frame; the
	// following snapshot frame proves the stream opened over the deadline door.
	if b := recvOne(t, stream); b.GetSeq() != 0 || b.GetPayload() != nil {
		t.Fatalf("boundary frame = seq %d payload %T, want seq 0 / nil", b.GetSeq(), b.GetPayload())
	}
	if snap := recvOne(t, stream); snap.GetSeq() != 1 {
		t.Fatalf("snapshot seq = %d, want 1 (the primed pre-subscribe event)", snap.GetSeq())
	}

	// Wait well past the deadline, THEN publish. If the deadline had bounded the
	// response half, the stream would already have errored and this Receive
	// would fail instead of delivering the frame.
	time.Sleep(postDeadlineWait) //nolint:forbidigo // deliberately waits past the deadline to prove the response half is NOT deadline-bounded; the sleep IS the timing subject (rule://go-no-sleep-in-test timing-under-test exemption)
	bus.Publish(statusEvent())

	live := recvOne(t, stream)
	if live.GetSeq() != 2 {
		t.Fatalf("post-deadline live msg seq = %d, want 2: the server-stream response did not survive past the body-read deadline", live.GetSeq())
	}
	if live.GetServerStatus() == nil {
		t.Fatalf("post-deadline live payload = %T, want ServerStatus", live.GetPayload())
	}
}
