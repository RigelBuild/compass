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

	"github.com/sealedsecurity/compass/go/internal/bridge"
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
	svc := newBridgeService(bridge.NewPump(bridge.NewUnixTarget(socket)), emitter)
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
