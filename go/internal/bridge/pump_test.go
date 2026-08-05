//go:build unix

package bridge

// The T3 bridge gate: the compass_rpc pump exercised against a REAL in-process
// stub daemon served over cleartext-HTTP/2 (h2c) on a Unix domain socket — the
// same door the shipped daemon serves. Deterministic + event-gated only: every
// synchronization point is a channel/observed event, never a sleep, and there
// are no retries. A short-deadline root context bounds each blocking wait so a
// wedged pump fails fast instead of hanging.

import (
	"context"
	"io"
	"net"
	"net/http"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

const testTimeout = 5 * time.Second

// stubServer is a real h2c http.Server on a UDS listener, torn down via
// t.Cleanup. handler serves each request; socketPath is what NewUnixTarget dials.
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

// collect runs the pump synchronously and returns the frames it emitted in order.
func collect(ctx context.Context, target *Target, call Call) []Frame {
	var frames []Frame
	NewPump(target).Do(ctx, call, func(f Frame) {
		frames = append(frames, f)
	})
	return frames
}

func TestPumpUnaryHappyPath(t *testing.T) {
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

	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()

	call := Call{
		Path: "/compass.v1.Service/Method?q=1",
		Headers: [][2]string{
			{"Content-Type", "application/grpc-web+proto"},
			{"X-Grpc-Web", "1"},
		},
		Body: []byte("request-payload"),
	}
	frames := collect(ctx, NewUnixTarget(socket), call)

	// The stub observed the request verbatim.
	if gotPath != "/compass.v1.Service/Method?q=1" {
		t.Errorf("stub path = %q, want the full path+query", gotPath)
	}
	if gotHeader != "1" {
		t.Errorf("stub X-Grpc-Web = %q, want %q", gotHeader, "1")
	}
	if string(gotBody) != "request-payload" {
		t.Errorf("stub body = %q, want %q", gotBody, "request-payload")
	}

	// Frames arrive head -> body(bytes) -> end.
	if len(frames) != 3 {
		t.Fatalf("got %d frames, want 3: %#v", len(frames), frames)
	}
	head, ok := frames[0].(HeadFrame)
	if !ok {
		t.Fatalf("frame[0] = %T, want HeadFrame", frames[0])
	}
	if head.Status != http.StatusOK {
		t.Errorf("head status = %d, want %d", head.Status, http.StatusOK)
	}
	if !hasHeaderPair(head.Headers, "X-Daemon", "ok") {
		t.Errorf("head headers %v missing X-Daemon: ok", head.Headers)
	}
	if !hasHeaderPair(head.Headers, "Content-Type", "application/grpc-web+proto") {
		t.Errorf("head headers %v missing Content-Type", head.Headers)
	}
	body, ok := frames[1].(BodyFrame)
	if !ok {
		t.Fatalf("frame[1] = %T, want BodyFrame", frames[1])
	}
	if string(body.Chunk) != string(respBody) {
		t.Errorf("body chunk = %q, want %q", body.Chunk, respBody)
	}
	if _, ok := frames[2].(EndFrame); !ok {
		t.Fatalf("frame[2] = %T, want EndFrame", frames[2])
	}
}

func TestPumpMultiFrameServerStream(t *testing.T) {
	const nChunks = 4
	// release[i] gates the server writing chunk i; closed by the client after it
	// observes chunk i-1, proving each body frame is delivered before the next is
	// even written (streaming is not buffered/coalesced).
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

	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()

	frames := make(chan Frame, 2*nChunks+2)
	done := make(chan struct{})
	go func() {
		NewPump(NewUnixTarget(socket)).Do(ctx, Call{Path: "/stream"}, func(f Frame) {
			frames <- f
		})
		close(done)
	}()

	// head first.
	if _, ok := recvFrame(t, frames).(HeadFrame); !ok {
		t.Fatalf("first frame is not HeadFrame")
	}

	// Drive each chunk: release it, then require exactly that body frame arrives
	// before releasing the next. If the pump buffered, we'd deadlock waiting for
	// a body frame the server hasn't been unblocked to write.
	for i := range nChunks {
		close(release[i])
		f := recvFrame(t, frames)
		body, ok := f.(BodyFrame)
		if !ok {
			t.Fatalf("chunk %d: frame = %T, want BodyFrame", i, f)
		}
		if len(body.Chunk) != 1 || body.Chunk[0] != byte('a'+i) {
			t.Fatalf("chunk %d = %q, want %q", i, body.Chunk, []byte{byte('a' + i)})
		}
	}

	if _, ok := recvFrame(t, frames).(EndFrame); !ok {
		t.Fatalf("final frame is not EndFrame")
	}
	select {
	case <-done:
	case <-time.After(testTimeout):
		t.Fatal("pump did not return after end")
	}
}

func TestPumpMidStreamCancel(t *testing.T) {
	streaming := make(chan struct{}) // closed once server has flushed first chunk
	serverGone := make(chan struct{})

	socket := stubServer(t, func(w http.ResponseWriter, r *http.Request) {
		defer close(serverGone)
		flusher := w.(http.Flusher)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("first"))
		flusher.Flush()
		close(streaming)
		// Block until the client cancels and tears down the request.
		<-r.Context().Done()
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var mu sync.Mutex
	var frames []Frame
	firstBody := make(chan struct{})
	var once sync.Once
	done := make(chan struct{})
	go func() {
		NewPump(NewUnixTarget(socket)).Do(ctx, Call{Path: "/stream"}, func(f Frame) {
			mu.Lock()
			frames = append(frames, f)
			isBody := false
			if _, ok := f.(BodyFrame); ok {
				isBody = true
			}
			mu.Unlock()
			if isBody {
				once.Do(func() { close(firstBody) })
			}
		})
		close(done)
	}()

	// Gate on observing the first body frame (event, not sleep), then cancel.
	select {
	case <-firstBody:
	case <-time.After(testTimeout):
		t.Fatal("never received first body frame")
	}
	cancel()

	// Pump must return promptly and emit nothing further.
	select {
	case <-done:
	case <-time.After(testTimeout):
		t.Fatal("pump did not stop promptly after cancel")
	}
	<-serverGone

	mu.Lock()
	defer mu.Unlock()
	// Exactly head + one body; no EndFrame or ErrorFrame after cancel.
	for i, f := range frames {
		switch f.(type) {
		case HeadFrame, BodyFrame:
			// expected
		case EndFrame:
			t.Errorf("frame[%d]: spurious EndFrame after cancel", i)
		case ErrorFrame:
			t.Errorf("frame[%d]: spurious ErrorFrame after cancel (canceled subscription is not an error)", i)
		default:
			t.Errorf("frame[%d]: unexpected %T", i, f)
		}
	}
	if len(frames) == 0 {
		t.Fatal("expected at least head + body before cancel")
	}
}

func TestPumpDialErrorBeforeHead(t *testing.T) {
	// A socket path that nothing is listening on: the dial fails before any head.
	socket := filepath.Join(t.TempDir(), "nonexistent.sock")

	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()

	frames := collect(ctx, NewUnixTarget(socket), Call{Path: "/x"})

	if len(frames) != 1 {
		t.Fatalf("got %d frames, want exactly 1 error frame: %#v", len(frames), frames)
	}
	errFrame, ok := frames[0].(ErrorFrame)
	if !ok {
		t.Fatalf("frame[0] = %T, want ErrorFrame", frames[0])
	}
	if errFrame.Message == "" {
		t.Error("error frame message is empty")
	}
	for _, f := range frames {
		switch f.(type) {
		case HeadFrame:
			t.Error("unexpected HeadFrame on dial failure")
		case EndFrame:
			t.Error("unexpected EndFrame on dial failure")
		}
	}
}

// recvFrame receives one frame or fails on timeout (event-gated, no sleeps).
func recvFrame(t *testing.T, ch <-chan Frame) Frame {
	t.Helper()
	select {
	case f := <-ch:
		return f
	case <-time.After(testTimeout):
		t.Fatal("timed out waiting for frame")
		return nil
	}
}

func hasHeaderPair(pairs [][2]string, name, value string) bool {
	for _, kv := range pairs {
		if kv[0] == name && kv[1] == value {
			return true
		}
	}
	return false
}
