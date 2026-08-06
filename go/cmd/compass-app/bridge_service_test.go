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
	"context"
	"encoding/base64"
	"io"
	"net"
	"net/http"
	"path/filepath"
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
	svc.run(svc.register(context.Background(), requestID), req)
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
	// torn-down call, not an EndFrame or ErrorFrame. Non-blocking check (the
	// pump provably emits nothing after ctx cancel, so this is deterministic).
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
	svc.run(svc.register(context.Background(), requestID), rpcRequest{RequestID: requestID, Path: "/x"})

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

// hasHeader reports whether pairs contains a {name, value} header.
func hasHeader(pairs []headerPair, name, value string) bool {
	for _, p := range pairs {
		if p.Name == name && p.Value == value {
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
