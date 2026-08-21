//go:build unix

package main

// The T3 bridge-service gate: compass_rpc / compass_rpc_cancel exercised against
// a REAL in-process stub compass.v1 daemon served over cleartext-HTTP/2 (h2c) on
// a Unix domain socket — the same door the shipped daemon serves, mirroring
// go/internal/bridge/pump_test.go. The service is driven through its Go
// entrypoint with a FAKE event emitter that captures every emitted frame, so the
// whole stream/cancel path is verified WITHOUT a live webview. Deterministic +
// event-gated only: every synchronization point is a channel or observed event,
// never a sleep, and there are no retries. A short-deadline root context bounds
// each blocking wait so a wedged service fails fast instead of hanging.

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/RigelBuild/compass/go/internal/bridge"
)

const testTimeout = 5 * time.Second

// emitted is one captured Emit call: the event name and the decoded frame.
type emitted struct {
	name  string
	frame responseFrame
}

// fakeEmitter is the eventEmitter the service emits through in tests. Each Emit
// is pushed onto a buffered channel in call order (Emit is invoked from the
// pump's single goroutine, so channel order is frame order). It stands in for
// the real Wails app.Event with no webview. The buffer is generous so Emit never
// blocks the pump goroutine — the test drains it as an event stream.
type fakeEmitter struct {
	ch chan emitted
}

func newFakeEmitter() *fakeEmitter {
	return &fakeEmitter{ch: make(chan emitted, 64)}
}

func (e *fakeEmitter) Emit(name string, data ...any) bool {
	var frame responseFrame
	if len(data) == 1 {
		frame, _ = data[0].(responseFrame)
	}
	e.ch <- emitted{name: name, frame: frame}
	return false
}

// stubServer is a real h2c http.Server on a UDS listener, torn down via
// t.Cleanup. handler serves each request; socketPath is what the pump dials.
func stubServer(t *testing.T, handler http.HandlerFunc) (socketPath string) {
	t.Helper()
	socketPath = filepath.Join(t.TempDir(), "daemon.sock")
	ln, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatalf("listen unix: %v", err)
	}
	p := new(http.Protocols)
	p.SetUnencryptedHTTP2(true)
	srv := &http.Server{Handler: handler, Protocols: p}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
		defer cancel()
		_ = srv.Shutdown(ctx)
	})
	return socketPath
}

// newService builds a bridge service dialing socket, wired to a fresh fake
// emitter the test drains.
func newService(socket string) (*bridgeService, *fakeEmitter) {
	emitter := newFakeEmitter()
	svc := newBridgeService(bridge.NewPump(bridge.NewUnixTarget(socket)), emitter, nil, nil)
	return svc, emitter
}

// recv receives one emitted frame or fails on timeout (event-gated, no sleeps).
func recv(t *testing.T, e *fakeEmitter) emitted {
	t.Helper()
	select {
	case ev := <-e.ch:
		return ev
	case <-time.After(testTimeout):
		t.Fatal("timed out waiting for an emitted frame")
		return emitted{}
	}
}

func TestCompassRPCUnaryRoundTrip(t *testing.T) {
	var (
		gotPath   string
		gotHeader string
		gotBody   []byte
	)
	respBody := []byte("grpc-web-response-bytes")

	socket := stubServer(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path + func() string {
			if r.URL.RawQuery == "" {
				return ""
			}
			return "?" + r.URL.RawQuery
		}()
		gotHeader = r.Header.Get("X-Grpc-Web")
		b, _ := io.ReadAll(r.Body)
		gotBody = b
		w.Header().Set("Content-Type", "application/grpc-web+proto")
		w.Header().Set("X-Daemon", "ok")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(respBody)
	})

	svc, emitter := newService(socket)
	const requestID = "req-unary"
	req := rpcRequest{
		RequestID: requestID,
		Path:      "/compass.v1.Service/Method?q=1",
		Headers: []headerPair{
			{Name: "Content-Type", Value: "application/grpc-web+proto"},
			{Name: "X-Grpc-Web", Value: "1"},
		},
		Body: []byte("request-payload"),
	}
	// Drive the forwarding synchronously: when run returns, every frame has been
	// emitted (buffered on the fake) and the in-flight entry is cleared, so the
	// frame sequence and the "call finished" assertions are fully deterministic
	// with no spin or sleep. CompassRPC is the same call on a goroutine.
	callCtx, call := svc.register(context.Background(), requestID)
	svc.run(callCtx, call, req)
	// head -> body(base64) -> end, all on the per-requestId event.
	head := recv(t, emitter)
	if head.name != "compass_rpc:"+requestID {
		t.Errorf("event name = %q, want per-requestId key", head.name)
	}
	if head.frame.Kind != "head" {
		t.Fatalf("frame[0].kind = %q, want head", head.frame.Kind)
	}
	if head.frame.Status != http.StatusOK {
		t.Errorf("head status = %d, want %d", head.frame.Status, http.StatusOK)
	}
	if !hasHeader(head.frame.Headers, "X-Daemon", "ok") {
		t.Errorf("head headers %v missing X-Daemon: ok", head.frame.Headers)
	}
	if !hasHeader(head.frame.Headers, "Content-Type", "application/grpc-web+proto") {
		t.Errorf("head headers %v missing Content-Type", head.frame.Headers)
	}

	body := recv(t, emitter)
	if body.frame.Kind != "body" {
		t.Fatalf("frame[1].kind = %q, want body", body.frame.Kind)
	}
	// Chunk is standard base64 of the raw response bytes (JS decodeChunk does atob).
	decoded, err := base64.StdEncoding.DecodeString(body.frame.Chunk)
	if err != nil {
		t.Fatalf("body chunk is not standard base64: %v", err)
	}
	if string(decoded) != string(respBody) {
		t.Errorf("decoded body = %q, want %q", decoded, respBody)
	}

	end := recv(t, emitter)
	if end.frame.Kind != "end" {
		t.Fatalf("frame[2].kind = %q, want end", end.frame.Kind)
	}

	// The stub observed the request verbatim (path+query, headers, body forwarded).
	if gotPath != "/compass.v1.Service/Method?q=1" {
		t.Errorf("stub path = %q, want the full path+query", gotPath)
	}
	if gotHeader != "1" {
		t.Errorf("stub X-Grpc-Web = %q, want %q", gotHeader, "1")
	}
	if string(gotBody) != "request-payload" {
		t.Errorf("stub body = %q, want %q", gotBody, "request-payload")
	}

	// The call is finished: its in-flight entry is gone (no leak, cancel is a no-op).
	assertNotInflight(t, svc, requestID)
}

func TestCompassRPCMultiFrameServerStream(t *testing.T) {
	const nChunks = 4
	// release[i] gates the server writing chunk i; closed by the client after it
	// observes chunk i-1, proving each body event is delivered before the next is
	// even written (the service does not buffer/coalesce the stream).
	release := make([]chan struct{}, nChunks)
	for i := range release {
		release[i] = make(chan struct{})
	}

	socket := stubServer(t, func(w http.ResponseWriter, r *http.Request) {
		flusher, ok := w.(http.Flusher)
		if !ok {
			t.Errorf("ResponseWriter is not a Flusher")
			return
		}
		w.WriteHeader(http.StatusOK)
		flusher.Flush()
		for i := range nChunks {
			select {
			case <-release[i]:
			case <-r.Context().Done():
				return
			}
			_, _ = w.Write([]byte{byte('a' + i)})
			flusher.Flush()
		}
	})

	svc, emitter := newService(socket)
	svc.CompassRPC(context.Background(), rpcRequest{RequestID: "req-stream", Path: "/stream"})

	// head first.
	if head := recv(t, emitter); head.frame.Kind != "head" {
		t.Fatalf("first frame kind = %q, want head", head.frame.Kind)
	}

	// Drive each chunk: release it, then require exactly that body event arrives
	// before releasing the next. If the service buffered, we'd deadlock waiting on
	// a body event the server hasn't been unblocked to write.
	for i := range nChunks {
		close(release[i])
		ev := recv(t, emitter)
		if ev.frame.Kind != "body" {
			t.Fatalf("chunk %d: frame kind = %q, want body", i, ev.frame.Kind)
		}
		decoded, err := base64.StdEncoding.DecodeString(ev.frame.Chunk)
		if err != nil {
			t.Fatalf("chunk %d: not base64: %v", i, err)
		}
		if len(decoded) != 1 || decoded[0] != byte('a'+i) {
			t.Fatalf("chunk %d = %q, want %q", i, decoded, []byte{byte('a' + i)})
		}
	}

	if end := recv(t, emitter); end.frame.Kind != "end" {
		t.Fatalf("final frame kind = %q, want end", end.frame.Kind)
	}
}

func TestCompassRPCMidStreamCancel(t *testing.T) {
	serverGone := make(chan struct{})

	socket := stubServer(t, func(w http.ResponseWriter, r *http.Request) {
		defer close(serverGone)
		flusher := w.(http.Flusher)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("first"))
		flusher.Flush()
		// Block until the client cancels and tears down the request.
		<-r.Context().Done()
	})

	svc, emitter := newService(socket)
	const requestID = "req-cancel"
	svc.CompassRPC(context.Background(), rpcRequest{RequestID: requestID, Path: "/stream"})

	// Gate on observing head + the first body event (events, not sleeps).
	if head := recv(t, emitter); head.frame.Kind != "head" {
		t.Fatalf("first frame kind = %q, want head", head.frame.Kind)
	}
	if body := recv(t, emitter); body.frame.Kind != "body" {
		t.Fatalf("second frame kind = %q, want body", body.frame.Kind)
	}

	// Cancel mid-stream. The pump's Do observes the ctx cancel, tears down the
	// HTTP request, and stops silently — emitting no further frames (pump
	// contract). Observing serverGone proves the request was torn down.
	svc.CompassRPCCancel(context.Background(), cancelRequest{RequestID: requestID})
	select {
	case <-serverGone:
	case <-time.After(testTimeout):
		t.Fatal("server request was not torn down after cancel")
	}

	// No further frame is emitted after cancel. The only frames ever emitted were
	// the head + one body already drained above; a canceled subscription is a
	// torn-down call, not an EndFrame or ErrorFrame. Non-blocking check: the pump
	// emits nothing after it OBSERVES the ctx cancel, and this test's stub server
	// writes no further bytes after "first" (it blocks on ctx.Done()), so no
	// in-flight body races the cancel here. End-to-end late-frame safety is
	// provided by the JS `canceled` guard in daemon-transport.ts.
	select {
	case ev := <-emitter.ch:
		t.Fatalf("spurious frame after cancel: kind=%q (a canceled call must emit nothing further)", ev.frame.Kind)
	default:
	}

	// Cancel dropped the in-flight entry; a second cancel is a harmless no-op.
	assertNotInflight(t, svc, requestID)
	svc.CompassRPCCancel(context.Background(), cancelRequest{RequestID: requestID})
}

func TestCompassRPCDialErrorBeforeHead(t *testing.T) {
	// A socket path nothing is listening on: the dial fails before any head, so
	// the service emits exactly one error frame and nothing else.
	socket := filepath.Join(t.TempDir(), "nonexistent.sock")

	svc, emitter := newService(socket)
	const requestID = "req-dialerr"
	// Synchronous drive: when run returns, the terminal error frame is the only
	// frame emitted and the in-flight entry is cleared — so "exactly one error
	// frame, nothing after" is deterministic without spinning on the map.
	callCtx, call := svc.register(context.Background(), requestID)
	svc.run(callCtx, call, rpcRequest{RequestID: requestID, Path: "/x"})

	ev := recv(t, emitter)
	if ev.name != "compass_rpc:"+requestID {
		t.Errorf("event name = %q, want per-requestId key", ev.name)
	}
	if ev.frame.Kind != "error" {
		t.Fatalf("frame kind = %q, want error", ev.frame.Kind)
	}
	if ev.frame.Message == "" {
		t.Error("error frame message is empty")
	}

	// Nothing follows the error frame.
	select {
	case extra := <-emitter.ch:
		t.Fatalf("frame after the terminal error: kind=%q", extra.frame.Kind)
	default:
	}
	assertNotInflight(t, svc, requestID)
}

// hasHeader reports whether pairs contains a [name, value] header tuple.
func hasHeader(pairs [][2]string, name, value string) bool {
	for _, p := range pairs {
		if p[0] == name && p[1] == value {
			return true
		}
	}
	return false
}

// assertNotInflight fails if requestID still has an in-flight entry.
func assertNotInflight(t *testing.T, svc *bridgeService, requestID string) {
	t.Helper()
	svc.mu.Lock()
	_, ok := svc.inflight[requestID]
	svc.mu.Unlock()
	if ok {
		t.Errorf("requestId %q still in-flight, want cleared", requestID)
	}
}

// assertInflight fails if requestID does NOT have an in-flight entry — the
// positive counterpart to assertNotInflight, used to prove a call survives a
// close-cancel that targeted a different window.
func assertInflight(t *testing.T, svc *bridgeService, requestID string) {
	t.Helper()
	svc.mu.Lock()
	_, ok := svc.inflight[requestID]
	svc.mu.Unlock()
	if !ok {
		t.Errorf("requestId %q not in-flight, want live entry", requestID)
	}
}

// TestResponseFrameWireContract locks the JSON wire shape of every ResponseFrame
// kind against the JS contract (apps/ui/src/daemon-transport.ts:19-23). The
// load-bearing assertion is that head headers marshal as [name,value] TUPLE
// arrays ([["x","y"]]) — the JS consumer does new Headers(frame.headers), which
// requires tuples and throws on {name,value} objects. This test fails against the
// pre-fix object shape.
func TestResponseFrameWireContract(t *testing.T) {
	t.Run("head", func(t *testing.T) {
		b, err := json.Marshal(frameToResponse(bridge.HeadFrame{Status: 200, Headers: [][2]string{{"x", "y"}}}))
		if err != nil {
			t.Fatalf("marshal head: %v", err)
		}
		s := string(b)
		if !strings.Contains(s, `"kind":"head"`) {
			t.Errorf("head JSON %s missing kind:head", s)
		}
		if !strings.Contains(s, `"status":200`) {
			t.Errorf("head JSON %s missing status:200", s)
		}
		if !strings.Contains(s, `"headers":[["x","y"]]`) {
			t.Errorf("head JSON %s: headers not rendered as tuple array [[\"x\",\"y\"]]", s)
		}
		if strings.Contains(s, `{"name"`) {
			t.Errorf("head JSON %s: headers rendered as {name,value} objects, must be tuples", s)
		}
	})
	t.Run("body", func(t *testing.T) {
		b, err := json.Marshal(frameToResponse(bridge.BodyFrame{Chunk: []byte("hi")}))
		if err != nil {
			t.Fatalf("marshal body: %v", err)
		}
		s := string(b)
		if !strings.Contains(s, `"kind":"body"`) {
			t.Errorf("body JSON %s missing kind:body", s)
		}
		if !strings.Contains(s, `"chunk":"aGk="`) {
			t.Errorf("body JSON %s: chunk not standard base64 of \"hi\"", s)
		}
	})
	t.Run("end", func(t *testing.T) {
		b, err := json.Marshal(frameToResponse(bridge.EndFrame{}))
		if err != nil {
			t.Fatalf("marshal end: %v", err)
		}
		if s := string(b); s != `{"kind":"end"}` {
			t.Errorf("end JSON = %s, want {\"kind\":\"end\"}", s)
		}
	})
	t.Run("error", func(t *testing.T) {
		b, err := json.Marshal(frameToResponse(bridge.ErrorFrame{Message: "boom"}))
		if err != nil {
			t.Fatalf("marshal error: %v", err)
		}
		s := string(b)
		if !strings.Contains(s, `"kind":"error"`) {
			t.Errorf("error JSON %s missing kind:error", s)
		}
		if !strings.Contains(s, `"message":"boom"`) {
			t.Errorf("error JSON %s missing message:boom", s)
		}
	})
}

// TestRPCRequestBodyDecodesNumberArray locks the load-bearing contract that a JS
// number[] body decodes element-wise into a Go []byte (json.Unmarshal maps a JSON
// number array into []byte directly).
func TestRPCRequestBodyDecodesNumberArray(t *testing.T) {
	var req rpcRequest
	if err := json.Unmarshal([]byte(`{"requestId":"r","path":"/x","body":[1,2,3,250]}`), &req); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !bytes.Equal(req.Body, []byte{1, 2, 3, 250}) {
		t.Errorf("req.Body = %v, want [1 2 3 250]", req.Body)
	}
}

// TestCompassRPCConcurrentDistinctIDs proves per-requestId keying and call
// independence: two concurrent calls with DISTINCT requestIds run against their
// own isolated service+emitter+socket, each frame arrives on its OWN
// compass_rpc:<id> event name (never cross-delivered), and cancelling one leaves
// the other still streaming. Event-gated via release channels, no sleeps.
func TestCompassRPCConcurrentDistinctIDs(t *testing.T) {
	// releaseA gates server A writing its body chunk; serverBGone confirms B tore
	// down on cancel. A keeps streaming after B is cancelled.
	releaseA := make(chan struct{})
	serverBGone := make(chan struct{})

	socketA := stubServer(t, func(w http.ResponseWriter, r *http.Request) {
		flusher := w.(http.Flusher)
		w.WriteHeader(http.StatusOK)
		flusher.Flush()
		select {
		case <-releaseA:
		case <-r.Context().Done():
			return
		}
		_, _ = w.Write([]byte("A"))
		flusher.Flush()
	})
	socketB := stubServer(t, func(w http.ResponseWriter, r *http.Request) {
		defer close(serverBGone)
		flusher := w.(http.Flusher)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("first"))
		flusher.Flush()
		<-r.Context().Done()
	})

	svcA, emitterA := newService(socketA)
	svcB, emitterB := newService(socketB)
	const idA = "req-A"
	const idB = "req-B"

	svcA.CompassRPC(context.Background(), rpcRequest{RequestID: idA, Path: "/a"})
	svcB.CompassRPC(context.Background(), rpcRequest{RequestID: idB, Path: "/b"})

	// Each call's head arrives on its OWN event name — proves per-id keying.
	headA := recv(t, emitterA)
	if headA.name != "compass_rpc:"+idA {
		t.Errorf("A head event name = %q, want compass_rpc:%s", headA.name, idA)
	}
	if headA.frame.Kind != "head" {
		t.Fatalf("A first frame kind = %q, want head", headA.frame.Kind)
	}
	headB := recv(t, emitterB)
	if headB.name != "compass_rpc:"+idB {
		t.Errorf("B head event name = %q, want compass_rpc:%s", headB.name, idB)
	}
	if headB.frame.Kind != "head" {
		t.Fatalf("B first frame kind = %q, want head", headB.frame.Kind)
	}

	// Drain B's first body so the cancel targets a live stream.
	if body := recv(t, emitterB); body.frame.Kind != "body" {
		t.Fatalf("B second frame kind = %q, want body", body.frame.Kind)
	}

	// Cancel B; it tears down and stops silently. A is untouched.
	svcB.CompassRPCCancel(context.Background(), cancelRequest{RequestID: idB})
	select {
	case <-serverBGone:
	case <-time.After(testTimeout):
		t.Fatal("server B was not torn down after cancel")
	}

	// A is still streaming: release its chunk and observe it arrive on A's event.
	close(releaseA)
	bodyA := recv(t, emitterA)
	if bodyA.name != "compass_rpc:"+idA {
		t.Errorf("A body event name = %q, want compass_rpc:%s", bodyA.name, idA)
	}
	if bodyA.frame.Kind != "body" {
		t.Fatalf("A body frame kind = %q, want body", bodyA.frame.Kind)
	}
	decoded, err := base64.StdEncoding.DecodeString(bodyA.frame.Chunk)
	if err != nil {
		t.Fatalf("A body chunk not base64: %v", err)
	}
	if string(decoded) != "A" {
		t.Errorf("A body = %q, want %q", decoded, "A")
	}
	if end := recv(t, emitterA); end.frame.Kind != "end" {
		t.Fatalf("A final frame kind = %q, want end", end.frame.Kind)
	}

	assertNotInflight(t, svcB, idB)
}

// TestAccountIDBoundGetter: the bound AccountID method returns the account id
// the embedded launch set on the service (the value the JS/UI reads over IPC to
// build the native ConnectionProvider), and the empty string when none was
// resolved. This is the T4.1 hand-off surface for the caller identity.
func TestAccountIDBoundGetter(t *testing.T) {
	svc, _ := newService("/unused.sock")
	if got := svc.AccountID(context.Background()); got != "" {
		t.Errorf("AccountID with no identity = %q, want empty", got)
	}
	svc.accountID = "acc-resolved"
	if got := svc.AccountID(context.Background()); got != "acc-resolved" {
		t.Errorf("AccountID = %q, want acc-resolved", got)
	}
}

// fakeWindow is a windowDispatcher test double: it records each per-window
// delivery (event name + decoded frame) onto a buffered channel, the per-window
// analogue of fakeEmitter. It stands in for a real *application.WebviewWindow so
// the routed frame path is verified without the GTK webview stack (which does
// not compile under the unix test tag). The service consults call.window through
// the windowDispatcher seam, so a test injects one by setting inflightCall.window
// directly — windowFromContext returns nil in the non-gtk3 test build.
type fakeWindow struct {
	ch chan emitted
}

func newFakeWindow() *fakeWindow {
	return &fakeWindow{ch: make(chan emitted, 64)}
}

func (w *fakeWindow) dispatch(name string, resp responseFrame) {
	w.ch <- emitted{name: name, frame: resp}
}

// recvWindow receives one delivery on a fake window's channel or fails on
// timeout (event-gated, no sleeps), mirroring recv for the emitter path.
func recvWindow(t *testing.T, w *fakeWindow) emitted {
	t.Helper()
	select {
	case ev := <-w.ch:
		return ev
	case <-time.After(testTimeout):
		t.Fatal("timed out waiting for a per-window delivery")
		return emitted{}
	}
}

// assertNoEmit fails if the app-wide emitter received any frame within a short
// window — proving a windowed call did NOT fall back to the broadcast path. It
// is the negative half of per-window routing: frames go to the window's channel
// ONLY. A short deadline bounds the check; the frames it guards against are
// emitted synchronously on the same pump goroutine that already delivered to the
// window, so by the time the window's terminal frame is observed, any stray Emit
// would already be buffered.
func assertNoEmit(t *testing.T, e *fakeEmitter) {
	t.Helper()
	select {
	case ev := <-e.ch:
		t.Fatalf("app-wide Emit received %q frame for a windowed call; want per-window only", ev.frame.Kind)
	case <-time.After(50 * time.Millisecond):
	}
}

// runWindowed drives one call synchronously with its window handle injected on
// the inflightCall, returning after every frame has been routed and the entry
// cleared (the deterministic single-shot pattern of TestCompassRPCUnaryRoundTrip).
// The window is set on the registered call directly because windowFromContext
// returns nil in the non-gtk3 test build; the seam field is exactly what the
// routed sink (emitFrame) consults.
func runWindowed(svc *bridgeService, win windowDispatcher, req rpcRequest) {
	callCtx, call := svc.register(context.Background(), req.RequestID)
	call.window = win
	svc.run(callCtx, call, req)
}

// TestCompassRPCRoutesToOriginatingWindow proves M3's core behavior: a call with
// a captured window routes ALL its frames to THAT window's dispatcher (per-window
// delivery), and NOT to the app-wide emitter — so with more than one window open
// a frame never broadcasts to a non-owning window.
func TestCompassRPCRoutesToOriginatingWindow(t *testing.T) {
	respBody := []byte("windowed-response")
	socket := stubServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/grpc-web+proto")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(respBody)
	})

	svc, emitter := newService(socket)
	win := newFakeWindow()
	const requestID = "req-windowed"
	runWindowed(svc, win, rpcRequest{RequestID: requestID, Path: "/compass.v1.Service/Method"})

	// head -> body -> end, all on the WINDOW's channel under the per-id event.
	head := recvWindow(t, win)
	if head.name != "compass_rpc:"+requestID {
		t.Errorf("window delivery name = %q, want per-requestId key", head.name)
	}
	if head.frame.Kind != frameKindHead {
		t.Fatalf("frame[0].kind = %q, want head", head.frame.Kind)
	}
	if body := recvWindow(t, win); body.frame.Kind != frameKindBody {
		t.Fatalf("frame[1].kind = %q, want body", body.frame.Kind)
	}
	if end := recvWindow(t, win); end.frame.Kind != frameKindEnd {
		t.Fatalf("frame[2].kind = %q, want end", end.frame.Kind)
	}

	// The app-wide emitter saw nothing: a windowed call never broadcasts.
	assertNoEmit(t, emitter)
	assertNotInflight(t, svc, requestID)
}

// TestCompassRPCConcurrentTwoWindowIsolation is the §M3 two-window arm on the
// concurrent-distinct-ids shape: ONE bridgeService (the production shape — a
// single service is created once in launch()) drives two calls CONCURRENTLY,
// each carrying a DIFFERENT originating window, sharing the one mutex-guarded
// inflight map. Each call's frames must land ONLY on its own window's channel,
// never the other's and never the app-wide emitter. This is the arm that would
// actually catch a routing regression: a service-scoped (rather than per-call)
// window handle, or a closure that captured the wrong call, would cross-deliver
// here — where two independent services could not. Event-gated, no sleeps.
func TestCompassRPCConcurrentTwoWindowIsolation(t *testing.T) {
	handler := func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/grpc-web+proto")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	}
	socket := stubServer(t, handler)

	svc, emitter := newService(socket)
	winA := newFakeWindow()
	winB := newFakeWindow()
	const idA = "req-winA"
	const idB = "req-winB"

	// Drive both calls on the ONE service concurrently, mirroring CompassRPC's
	// own register-then-go-run, with each call's originating window injected on
	// the registered inflightCall (windowFromContext returns nil in this
	// non-gtk3 test build, so the seam field is set directly). The two run
	// goroutines share svc.inflight — the concurrency the arm exists to stress.
	// Each run signals done on return (after its deferred finish clears the
	// inflight entry), so the not-inflight assertion is gated on completion
	// rather than racing the goroutine — event-gated, no sleeps.
	launch := func(win windowDispatcher, req rpcRequest) chan struct{} {
		callCtx, call := svc.register(context.Background(), req.RequestID)
		call.window = win
		done := make(chan struct{})
		go func() {
			defer close(done)
			svc.run(callCtx, call, req)
		}()
		return done
	}
	doneA := launch(winA, rpcRequest{RequestID: idA, Path: "/a"})
	doneB := launch(winB, rpcRequest{RequestID: idB, Path: "/b"})

	// Each window receives its own stream through the terminal frame; every
	// frame must carry that window's own per-id event name (no cross-delivery).
	drainWindow(t, winA, idA)
	drainWindow(t, winB, idB)

	// Both runs returned (deferred finish cleared each entry); assert teardown.
	waitDone(t, doneA)
	waitDone(t, doneB)

	// Neither concurrent call fell back to the app-wide broadcast.
	assertNoEmit(t, emitter)
	assertNotInflight(t, svc, idA)
	assertNotInflight(t, svc, idB)
}

// drainWindow reads frames off a window's channel through the terminal end frame,
// asserting every one carries the expected per-id event name (no cross-delivery).
func drainWindow(t *testing.T, w *fakeWindow, requestID string) {
	t.Helper()
	want := "compass_rpc:" + requestID
	for {
		ev := recvWindow(t, w)
		if ev.name != want {
			t.Errorf("cross-window delivery: got %q, want %q", ev.name, want)
		}
		if ev.frame.Kind == frameKindEnd || ev.frame.Kind == frameKindError {
			return
		}
	}
}

// waitDone blocks until a run goroutine signals completion, or fails on timeout
// (event-gated, no sleeps) — the completion gate for a concurrently-driven call.
func waitDone(t *testing.T, done <-chan struct{}) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(testTimeout):
		t.Fatal("timed out waiting for a run goroutine to finish")
	}
}

// TestCompassRPCNoWindowFallsBackToEmit pins the preserved fallback: a call with
// no captured window emits through the app-wide eventEmitter seam (the fakeEmitter
// path), exactly as before M3. This is the invariant the existing no-window tests
// (which drive CompassRPC with a plain context) depend on.
func TestCompassRPCNoWindowFallsBackToEmit(t *testing.T) {
	socket := stubServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/grpc-web+proto")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	svc, emitter := newService(socket)
	const requestID = "req-nowindow"
	// No window injected (call.window stays nil), matching windowFromContext's
	// nil result for a windowless caller.
	callCtx, call := svc.register(context.Background(), requestID)
	if call.window != nil {
		t.Fatalf("no-window register captured a window %v, want nil", call.window)
	}
	svc.run(callCtx, call, rpcRequest{RequestID: requestID, Path: "/x"})

	if head := recv(t, emitter); head.name != "compass_rpc:"+requestID || head.frame.Kind != frameKindHead {
		t.Fatalf("head = (%q,%q), want (compass_rpc:%s, head)", head.name, head.frame.Kind, requestID)
	}
	assertNotInflight(t, svc, requestID)
}

// TestCompassRPCDestroyedWindowDropsFrames pins A4: a frame for a call whose
// window has closed is DROPPED, not fallback-broadcast. The real
// *application.WebviewWindow.DispatchWailsEvent no-ops once isDestroyed()
// (webview_window.go:1373); through the windowDispatcher seam a "destroyed"
// window is one whose dispatch is a no-op. The call must still complete and tear
// down cleanly, with NO frame delivered anywhere (not the window, not the
// app-wide emitter) and no panic.
func TestCompassRPCDestroyedWindowDropsFrames(t *testing.T) {
	socket := stubServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/grpc-web+proto")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("dropped"))
	})
	svc, emitter := newService(socket)
	const requestID = "req-destroyed"
	// A destroyed window: dispatch is a no-op, modeling DispatchWailsEvent's
	// isDestroyed() guard. Routing still targets it (not the fallback), so the
	// frames are dropped rather than broadcast app-wide.
	runWindowed(svc, destroyedWindow{}, rpcRequest{RequestID: requestID, Path: "/gone"})

	// Nothing reached the app-wide emitter: the drop is NOT a fallback broadcast.
	assertNoEmit(t, emitter)
	// The call finished and cleaned up despite every frame being dropped.
	assertNotInflight(t, svc, requestID)
}

// destroyedWindow is a windowDispatcher whose dispatch is a no-op, modeling a
// window whose DispatchWailsEvent has begun no-opping because the window
// isDestroyed() (webview_window.go:1373). Frames routed to it are silently
// dropped — the A4 destroyed-window behavior.
type destroyedWindow struct{}

func (destroyedWindow) dispatch(string, responseFrame) {}

// TestCancelWindowSweepsOnlyClosingWindow proves §M3b close-time cancel: ONE
// service holds four long-lived calls — TWO on winA (A1, A2), one on winB, and
// one with no window (the fallback/windowless path) — all kept in-flight by a
// stub handler that blocks until the test releases it. cancelWindow(winA) drops
// BOTH of winA's calls and nothing else: their entries are gone and their run
// goroutines return (each canceled pump stops → finish), while winB's call and
// the windowless call stay live and in-flight. Two calls on winA is the point —
// it pins the collect-and-cancel-ALL loop against a first-match-only regression
// (a stray break/return would leak A2 and this test would redden).
// cancelWindow(nil) then sweeps nothing. Finally the server is released so the
// survivors drain and the test exits clean. Event-gated on channels/waitDone,
// no sleeps.
func TestCancelWindowSweepsOnlyClosingWindow(t *testing.T) {
	release := make(chan struct{})
	handler := func(w http.ResponseWriter, _ *http.Request) {
		<-release // block so each call's pump stays in-flight until released
		w.Header().Set("Content-Type", "application/grpc-web+proto")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	}
	socket := stubServer(t, handler)

	svc, _ := newService(socket)
	winA := newFakeWindow()
	winB := newFakeWindow()
	const idA1 = "req-winA-1"
	const idA2 = "req-winA-2"
	const idB = "req-winB"
	const idC = "req-nowin"

	// Launch the calls on the ONE service, each on its own run goroutine
	// (CompassRPC's own register-then-go-run shape). The window is injected on
	// the registered inflightCall directly because windowFromContext returns nil
	// in this non-gtk3 test build. Each run signals done on return so teardown is
	// event-gated, not raced.
	launch := func(win windowDispatcher, req rpcRequest) chan struct{} {
		callCtx, call := svc.register(context.Background(), req.RequestID)
		call.window = win
		done := make(chan struct{})
		go func() {
			defer close(done)
			svc.run(callCtx, call, req)
		}()
		return done
	}
	doneA1 := launch(winA, rpcRequest{RequestID: idA1, Path: "/a1"})
	doneA2 := launch(winA, rpcRequest{RequestID: idA2, Path: "/a2"})
	doneB := launch(winB, rpcRequest{RequestID: idB, Path: "/b"})
	doneC := launch(nil, rpcRequest{RequestID: idC, Path: "/c"})

	// All four are registered and blocked in their pump (the handler is stuck on
	// release), so all four entries are live before any cancel.
	assertInflight(t, svc, idA1)
	assertInflight(t, svc, idA2)
	assertInflight(t, svc, idB)
	assertInflight(t, svc, idC)

	// Close winA: BOTH of winA's calls are swept, not just the first. Each entry
	// is dropped and its canceled pump stops → run returns → done closes.
	svc.cancelWindow(winA)
	waitDone(t, doneA1)
	waitDone(t, doneA2)
	assertNotInflight(t, svc, idA1)
	assertNotInflight(t, svc, idA2)

	// B (other window) and C (no window) are untouched by winA's close.
	assertInflight(t, svc, idB)
	assertInflight(t, svc, idC)

	// A nil window matches nothing: the windowless/fallback call is never swept
	// by a close, and the surviving windowed call stays live.
	svc.cancelWindow(nil)
	assertInflight(t, svc, idB)
	assertInflight(t, svc, idC)

	// Release the server so B and C complete and tear down; the test exits clean.
	close(release)
	waitDone(t, doneB)
	waitDone(t, doneC)
	assertNotInflight(t, svc, idB)
	assertNotInflight(t, svc, idC)
}
