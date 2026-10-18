//go:build unix

package server

// withBodyReadDeadline arms every request; authenticated Runner streams release their deadline.

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect/v2"
	"connectrpc.com/connect/v2/connecthttp"
	"connectrpc.com/otelconnect"

	"github.com/RigelBuild/compass/go/events"
	compassv1 "github.com/RigelBuild/compass/go/gen/compass/v1"
	"github.com/RigelBuild/compass/go/gen/compass/v1/compassv1connect"
	"github.com/RigelBuild/compass/go/internal/bodydeadline"
	compassv1internal "github.com/RigelBuild/compass/go/internal/gen/compass/v1"
	"github.com/RigelBuild/compass/go/internal/gen/compass/v1/compassv1internalconnect"
	"github.com/RigelBuild/compass/go/internal/runnerhub"
	"github.com/RigelBuild/compass/go/internal/store"
)

// bodyReadResult is the outcome of the inner handler's full read of the request
// body: the bytes it managed to read and the terminal error (nil on a clean
// EOF, a timeout when the deadline cut the read). Delivered over a buffered
// channel so the handler goroutine never blocks on the send.
type bodyReadResult struct {
	body   []byte
	err    error
	remote string // server-side r.RemoteAddr: equal across streams iff one conn
	proto  int
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
	ne, ok := errors.AsType[net.Error](err)
	return ok && ne.Timeout()
}

// deadlineDoorTimeout is the short body deadline every drip test arms.
const deadlineDoorTimeout = 100 * time.Millisecond

func startDeadlineDoor(t *testing.T, custom func(*http.Request) bodyReadResult) (*httptest.Server, <-chan bodyReadResult) {
	t.Helper()
	results := make(chan bodyReadResult, 1)
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var result bodyReadResult
		if custom == nil {
			result.body, result.err = io.ReadAll(r.Body)
		} else {
			result = custom(r)
		}
		results <- result
		w.WriteHeader(http.StatusOK)
	})
	srv := httptest.NewServer(withBodyReadDeadline(inner, deadlineDoorTimeout))
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
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, srv.URL+path, pr)
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

// TestNetworkDoorBodyReadDeadlineCutsSlowBody verifies a stalled bounded body is cut.
func TestNetworkDoorBodyReadDeadlineCutsSlowBody(t *testing.T) {
	t.Parallel()
	srv, results := startDeadlineDoor(t, nil)
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

// TestNetworkDoorBodyReadDeadlineCutsPreAuthRunnerDrip covers unauthenticated Runner requests.
func TestNetworkDoorBodyReadDeadlineCutsPreAuthRunnerDrip(t *testing.T) {
	t.Parallel()
	srv, results := startDeadlineDoor(t, nil)
	pw := driveDrip(t, srv, compassv1internalconnect.RunnerServiceSessionsProcedure)
	if _, err := pw.Write([]byte("x")); err != nil {
		t.Fatalf("drip first byte: %v", err)
	}
	select {
	case res := <-results:
		if !isBodyReadTimeout(res.err) {
			t.Fatalf("pre-auth Runner body read err = %v, want timeout", res.err)
		}
	case <-timeAfter():
		t.Fatal("pre-auth Runner body read did not return before the safety-net deadline")
	}
}

// TestNetworkDoorBodyReadDeadlineReleasedPathCompletes proves release permits a slow Runner body.
func TestNetworkDoorBodyReadDeadlineReleasedPathCompletes(t *testing.T) {
	t.Parallel()
	const pause = 4 * deadlineDoorTimeout
	const want = "runner-session-frame"
	custom := func(r *http.Request) bodyReadResult {
		bodydeadline.Release(r.Context())
		bodydeadline.Release(r.Context())
		body, err := io.ReadAll(r.Body)
		return bodyReadResult{body: body, err: err}
	}
	srv, results := startDeadlineDoor(t, custom)
	pw := driveDrip(t, srv, compassv1internalconnect.RunnerServiceSessionsProcedure)
	if _, err := pw.Write([]byte(want[:1])); err != nil {
		t.Fatalf("drip first byte: %v", err)
	}
	// Continue only after the original deadline would have elapsed.
	timer := time.NewTimer(pause)
	t.Cleanup(func() { timer.Stop() })
	<-timer.C
	if _, err := pw.Write([]byte(want[1:])); err != nil {
		t.Fatalf("drip remainder: %v", err)
	}
	if err := pw.Close(); err != nil {
		t.Fatalf("close drip body: %v", err)
	}
	select {
	case res := <-results:
		if res.err != nil {
			t.Fatalf("released Runner body read err = %v, want nil", res.err)
		}
		if string(res.body) != want {
			t.Fatalf("released Runner body = %q, want %q", res.body, want)
		}
	case <-timeAfter():
		t.Fatal("released Runner body read did not return before the safety-net deadline")
	}
}

// TestNetworkDoorBodyReadDeadlineNonReleasableReleaseIsNoop proves a non-releasable path ignores release requests.
func TestNetworkDoorBodyReadDeadlineNonReleasableReleaseIsNoop(t *testing.T) {
	t.Parallel()
	custom := func(r *http.Request) bodyReadResult {
		bodydeadline.Release(r.Context())
		_, err := io.ReadAll(r.Body)
		return bodyReadResult{err: err}
	}
	srv, results := startDeadlineDoor(t, custom)
	pw := driveDrip(t, srv, compassv1connect.CompassServiceIssueTokenProcedure)
	if _, err := pw.Write([]byte("x")); err != nil {
		t.Fatalf("drip first byte: %v", err)
	}
	select {
	case res := <-results:
		if !isBodyReadTimeout(res.err) {
			t.Fatalf("non-releasable body read err = %v, want timeout", res.err)
		}
	case <-timeAfter():
		t.Fatal("non-releasable body read did not return before the safety-net deadline")
	}
}

// TestNetworkDoorRunnerPublishEventsBodyDeadlineRelease covers real auth and h2c streaming.
func TestNetworkDoorRunnerPublishEventsBodyDeadlineRelease(t *testing.T) {
	for _, tc := range []struct {
		name      string
		token     string
		wantError bool
	}{
		{name: "valid bearer releases deadline", token: "runner-token"},
		{name: "invalid bearer is unauthenticated", token: "invalid-token", wantError: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client, armed := newNetworkDoorRunnerClient(t, tc.token)
			stream, err := client.PublishEvents(t.Context())
			if err != nil {
				t.Fatalf("open PublishEvents: %v", err)
			}
			firstSendErr := stream.Send(&compassv1internal.PublishEventsRequest{SessionId: "s"})
			// Client Send returns before the server sees the request, so time from the server's arm.
			select {
			case err := <-armed:
				if err != nil {
					t.Fatalf("server could not arm the body-read deadline: %v", err)
				}
			case <-time.After(testTimeout):
				t.Fatal("server never armed the body-read deadline")
			}
			time.Sleep(4 * deadlineDoorTimeout) //nolint:forbidigo // Deliberately waits past the deadline to prove auth release keeps the stream open; sleep is the timing subject (rule://go-no-sleep-in-test)
			secondSendErr := stream.Send(&compassv1internal.PublishEventsRequest{SessionId: "s"})
			_, closeErr := stream.CloseAndReceive()

			if tc.wantError {
				for _, err := range []error{firstSendErr, secondSendErr, closeErr} {
					if connect.CodeOf(err) == connect.CodeUnauthenticated {
						return
					}
				}
				t.Fatalf("PublishEvents errors = send1 %v, send2 %v, close %v; want Unauthenticated", firstSendErr, secondSendErr, closeErr)
			}
			if firstSendErr != nil || secondSendErr != nil || closeErr != nil {
				t.Fatalf("PublishEvents errors = send1 %v, send2 %v, close %v; want nil", firstSendErr, secondSendErr, closeErr)
			}
		})
	}
}

// armSignal reports the result of the door's first non-zero read deadline, so the
// test times from a deadline that was actually armed.
type armSignal struct {
	http.ResponseWriter
	once  *sync.Once
	armed chan error
}

func (a armSignal) SetReadDeadline(deadline time.Time) error {
	err := http.NewResponseController(a.ResponseWriter).SetReadDeadline(deadline)
	if !deadline.IsZero() {
		a.once.Do(func() { a.armed <- err })
	}
	return err
}

func (a armSignal) Unwrap() http.ResponseWriter { return a.ResponseWriter }

func newNetworkDoorRunnerClient(t *testing.T, token string) (compassv1internalconnect.RunnerServiceClient, <-chan error) {
	t.Helper()
	otelIC, err := otelconnect.NewServerInterceptor()
	if err != nil {
		t.Fatalf("otelconnect.NewServerInterceptor: %v", err)
	}
	resolve := func(_ context.Context, presented string, want store.SubjectKind) (store.Subject, error) {
		if presented != "runner-token" || want != store.SubjectRunner {
			return store.Subject{}, errors.New("invalid test Runner credential")
		}
		return store.Subject{Kind: store.SubjectRunner, ID: "runner-1"}, nil
	}
	hub := runnerhub.NewHub(nil, nil, nil, nil)
	mux := http.NewServeMux()
	runnerhub.Mount(mux, hub, resolve, nil, nil, otelIC)
	door := withBodyReadDeadline(mux, deadlineDoorTimeout)
	armed := make(chan error, 1)
	var once sync.Once
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		door.ServeHTTP(armSignal{ResponseWriter: w, once: &once, armed: armed}, r)
	}))
	srv.Config.Protocols = cleartextHTTP2()
	srv.Start()
	t.Cleanup(srv.Close)

	tr := h2cTransport(func(ctx context.Context, network, addr string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, network, addr)
	})
	t.Cleanup(tr.CloseIdleConnections)
	return compassv1internalconnect.NewRunnerServiceClient(connect.NewClient(connecthttp.NewTransport(
		&http.Client{Transport: tr}, srv.URL, connecthttp.WithReadMaxBytes(0),
	), (&serverTestBearer{token: token}).ClientInterceptor)), armed
}

// TestNetworkDoorBodyDeadlineReleasableSet pins the exact Runner stream membership.
func TestNetworkDoorBodyDeadlineReleasableSet(t *testing.T) {
	t.Parallel()
	releasable := []struct{ name, procedure string }{
		{"RunnerService.Sessions", compassv1internalconnect.RunnerServiceSessionsProcedure},
		{"RunnerService.PublishEvents", compassv1internalconnect.RunnerServicePublishEventsProcedure},
	}
	bounded := []struct{ name, procedure string }{
		{"CompassService.SubscribeEvents", compassv1connect.CompassServiceSubscribeEventsProcedure},
		{"CompassService.IssueToken", compassv1connect.CompassServiceIssueTokenProcedure},
		{"CommsService.SubscribeComms", compassv1connect.CommsServiceSubscribeCommsProcedure},
		{"RunnerService.Enroll", compassv1internalconnect.RunnerServiceEnrollProcedure},
	}
	for _, tc := range releasable {
		t.Run("releasable/"+tc.name, func(t *testing.T) {
			t.Parallel()
			if _, ok := bodyDeadlineReleasable[tc.procedure]; !ok {
				t.Errorf("bodyDeadlineReleasable is missing %q", tc.procedure)
			}
		})
	}
	for _, tc := range bounded {
		t.Run("bounded/"+tc.name, func(t *testing.T) {
			t.Parallel()
			if _, ok := bodyDeadlineReleasable[tc.procedure]; ok {
				t.Errorf("bodyDeadlineReleasable wrongly contains %q", tc.procedure)
			}
		})
	}
	if len(bodyDeadlineReleasable) != len(releasable) {
		t.Errorf("len(bodyDeadlineReleasable) = %d, want %d", len(bodyDeadlineReleasable), len(releasable))
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
// Both requests use non-releasable paths, so both arm a deadline; the drip stream's
// body stalls UNBOUNDED (its writer only closes in cleanup), so the socket read
// deadline is the only thing that can end its server-side read — time is the
// subject under test, never a sync device. Every assertion gates on the real
// per-stream server-side read outcome delivered over a buffered channel, bounded
// by the shared safety-net. Hermetic: h2c over loopback, OS-assigned port, no
// Postgres — the default `go test ./server/` lane.
func TestNetworkDoorBodyDeadlineIsolatesHTTP2Streams(t *testing.T) {
	t.Parallel()
	// A non-releasable path: both requests arm the deadline. The ?stream= query
	// keys the two concurrent streams' outcomes apart; r.URL.Path stays
	// non-releasable (queries do not affect the releasable lookup).
	const isolationPath = "/isolation"
	const wantB = "prompt-sibling-body"

	// Per-stream server-side read outcomes, buffered so the handler goroutines never
	// block on the send. startedA fires when the drip stream's handler is live, so A
	// is open and stalled before B launches and the two streams run concurrently.
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
			resA <- bodyReadResult{body: body, err: err, remote: r.RemoteAddr, proto: r.ProtoMajor}
		case "B":
			resB <- bodyReadResult{body: body, err: err, remote: r.RemoteAddr, proto: r.ProtoMajor}
		}
		w.WriteHeader(http.StatusOK)
	})

	// h2c server whose handler is the PRODUCTION middleware — mirrors
	// newH2CTestServer's h2c setup shape (unstarted → cleartext protocols →
	// Start → Cleanup) but wraps a custom inner handler.
	srv := httptest.NewUnstartedServer(withBodyReadDeadline(inner, deadlineDoorTimeout))
	srv.Config.Protocols = cleartextHTTP2()
	srv.Start()
	t.Cleanup(srv.Close)

	// Both streams ride one explicit h2c ClientConn (it never redials), the
	// premise that makes this a per-STREAM proof; asserted server-side below.
	tr := h2cTransport(func(ctx context.Context, network, addr string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, network, addr)
	})
	cc, err := tr.NewClientConn(t.Context(), "http", srv.Listener.Addr().String())
	if err != nil {
		t.Fatalf("open the shared h2c conn: %v", err)
	}
	t.Cleanup(func() {
		if err := cc.Close(); err != nil {
			t.Errorf("close the shared h2c conn: %v", err)
		}
	})
	client := &http.Client{Transport: cc}

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
	// the same non-releasable path (so it also arms a deadline). It must complete
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
	// both rode ONE h2 connection, so assert that from the server's side.
	if outA.remote != outB.remote || outA.proto != 2 || outB.proto != 2 {
		t.Fatalf("streams A (%s, HTTP/%d) and B (%s, HTTP/%d) did not share one h2 connection, so this cannot distinguish per-stream from per-connection isolation", outA.remote, outA.proto, outB.remote, outB.proto)
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
// deadline, so a non-releasable server-stream (SubscribeEvents) arms it — the exact
// seam the survival test drives, minus the production 30s wait. The mux is the
// same handler the door ships, so the middleware sees the real, non-releasable
// procedure path. Torn down in t.Cleanup.
func startServerStreamDeadlineDoor(t *testing.T, svc *service, timeout time.Duration) string {
	t.Helper()
	server := connect.NewServer()
	compassv1connect.RegisterCompassServiceHandler(server, svc)
	mux := http.NewServeMux()
	connecthttp.Mount(mux, server)
	srv := httptest.NewUnstartedServer(withBodyReadDeadline(mux, timeout))
	srv.Config.Protocols = cleartextHTTP2()
	srv.Start()
	t.Cleanup(srv.Close)
	return srv.URL
}

// TestNetworkDoorBodyDeadlineDoesNotCutServerStreamResponse is the invariant
// that makes the body-read deadline safe to apply to the server-streams
// (SubscribeEvents / SubscribeComms) that keep it: a non-releasable server-stream's
// long-lived RESPONSE half must survive PAST the request-body read deadline. The
// deadline is scoped to the request-body read and released at body EOF — NOT
// armed on the whole connection/request — so a fast unary request followed by a
// long response write is not bounded by it.
//
// SubscribeEvents is non-releasable, so the middleware arms the short deadline on
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
	// Prime one event pre-subscribe: connect flushes the response headers on the
	// handler's first Send, and SubscribeEvents RoundTrip blocks until they arrive —
	// with an empty bus the open would deadlock. Its snapshot replay below is also the
	// proof the stream opened and delivered a frame.
	bus.Publish(t.Context(), statusEvent())

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
	time.Sleep(postDeadlineWait) //nolint:forbidigo // Deliberately waits past the deadline; deadline expiry is the timing subject under test (rule://go-no-sleep-in-test)
	bus.Publish(t.Context(), statusEvent())

	live := recvOne(t, stream)
	if live.GetSeq() != 2 {
		t.Fatalf("post-deadline live msg seq = %d, want 2: the server-stream response did not survive past the body-read deadline", live.GetSeq())
	}
	if live.GetServerStatus() == nil {
		t.Fatalf("post-deadline live payload = %T, want ServerStatus", live.GetPayload())
	}
}
