//go:build unix

package runner

// Shared scaffolding for the Runner-side seam tests: a pipe-backed fake
// ContainerRuntime (the existing runtime.fakeRuntime.ExecStreaming is a nil-pipe
// stub, unusable for relay — this one returns a StreamingExec whose IO.Stdout is
// an io.PipeReader the test writes framed protojson into), a recording fake
// runtime for the Provision→Launch path, a capturing PublishEvents server so the
// relay's client-stream terminates a real wire, and the h2c transport helpers.

import (
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"

	compassv1internal "github.com/sealedsecurity/compass/go/internal/gen/compass/v1"
	"github.com/sealedsecurity/compass/go/internal/gen/compass/v1/compassv1internalconnect"
	"github.com/sealedsecurity/compass/go/internal/runtime"
)

// testAgentEnv is the agent exec configuration the relay tests start agents
// with. Values are arbitrary but non-empty, so a test asserting on the exec
// argv sees each var actually carried rather than an omitted empty.
func testAgentEnv() AgentEnv {
	return AgentEnv{UID: 1000, HomeDir: "/home/agent", Workdir: "/work/repo", Model: "test-model"}
}

// testTimeout bounds every blocking wait so a wedged relay fails fast instead of
// hanging the suite. A deadline safety net, never a synchronization device:
// tests event-gate on the frames actually observed, not elapsed time.
const testTimeout = 15 * time.Second

func timeAfter() <-chan time.Time { return time.After(testTimeout) }

// discardLoggerRunner builds a slog.Logger that drops output, so the relay's
// warn/debug diagnostics do not spam the test log.
func discardLoggerRunner() *slog.Logger {
	return slog.New(slog.DiscardHandler)
}

// nopWriteCloser adapts io.Discard to io.WriteCloser for a streaming exec's
// unused stdin.
type nopWriteCloser struct{}

func (nopWriteCloser) Write(p []byte) (int, error) { return len(p), nil }
func (nopWriteCloser) Close() error                { return nil }

// pipeRuntime is a ContainerRuntime whose ExecStreaming returns a StreamingExec
// backed by real in-memory pipes: the test writes newline-framed protojson into
// stdoutW / stderrW and the relay reads them off IO.Stdout / IO.Stderr. Its
// lifecycle methods (Create/Start/Exec/…) are recording no-ops so it also serves
// the Provision→Launch path. Process is left nil: the relay reads only IO, so a
// test that never calls AgentStream.Stop needs no real child handle.
type pipeRuntime struct {
	mu      sync.Mutex
	calls   []string
	stdoutW *io.PipeWriter
	stderrW *io.PipeWriter
	stdoutR *io.PipeReader
	stderrR *io.PipeReader
	execErr error // when set, ExecStreaming fails with it
}

func newPipeRuntime() *pipeRuntime {
	outR, outW := io.Pipe()
	errR, errW := io.Pipe()
	return &pipeRuntime{stdoutW: outW, stderrW: errW, stdoutR: outR, stderrR: errR}
}

func (f *pipeRuntime) Create(context.Context, runtime.ContainerSpec) (runtime.ContainerID, error) {
	f.record("create")
	return runtime.ContainerID("fake-id"), nil
}
func (f *pipeRuntime) Start(context.Context, runtime.ContainerID) error {
	f.record("start")
	return nil
}
func (f *pipeRuntime) Exec(context.Context, runtime.ContainerID, runtime.ExecSpec) (runtime.ExecOutput, error) {
	f.record("exec")
	return runtime.ExecOutput{}, nil
}
func (f *pipeRuntime) ExecStreaming(_ context.Context, _ runtime.ContainerID, _ runtime.StreamingExecSpec) (*runtime.StreamingExec, error) {
	f.record("exec_streaming")
	if f.execErr != nil {
		return nil, f.execErr
	}
	return &runtime.StreamingExec{
		IO: runtime.StreamingIO{Stdin: nopWriteCloser{}, Stdout: f.stdoutR, Stderr: f.stderrR},
		// Process intentionally nil — relay reads only IO; a test using this
		// fake must not call AgentStream.Stop.
	}, nil
}
func (f *pipeRuntime) Stop(context.Context, runtime.ContainerID, time.Duration) error {
	f.record("stop")
	return nil
}
func (f *pipeRuntime) Remove(context.Context, runtime.ContainerID) error {
	f.record("remove")
	return nil
}
func (f *pipeRuntime) Exists(context.Context, string) (bool, error) { return false, nil }

func (f *pipeRuntime) record(call string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, call)
}

// closeStdout closes the stdout pipe writer, ending the relay's scanner (agent
// exit / EOF).
func (f *pipeRuntime) closeStdout() { _ = f.stdoutW.Close() }

// stubStreamingRuntime is a ContainerRuntime whose lifecycle methods are
// recording no-ops and whose ExecStreaming delegates to a real PodmanCLI driving
// a shell stub — so it returns a StreamingExec with a REAL, terminatable
// Process (the host's Reload / live-session Stop call Process.Terminate, which a
// nil-Process fake would panic on). The stub ignores the podman argv and sleeps
// past the test, so it is a live child that SIGKILL reaps. Every streaming exec
// spec it is handed is recorded, so a test can assert what identity and
// configuration the host actually started an agent with.
type stubStreamingRuntime struct {
	mu        sync.Mutex
	calls     []string
	execSpecs []runtime.StreamingExecSpec
	cli       *runtime.PodmanCLI
}

func newStubStreamingRuntime(t *testing.T) *stubStreamingRuntime {
	t.Helper()
	dir := t.TempDir()
	stub := "#!/bin/sh\nexec sleep 120\n"
	prog := filepath.Join(dir, "podman-stub.sh")
	if err := os.WriteFile(prog, []byte(stub), 0o755); err != nil {
		t.Fatalf("writing streaming stub: %v", err)
	}
	return &stubStreamingRuntime{cli: runtime.NewPodmanCLI().WithProgram(prog)}
}

func (f *stubStreamingRuntime) Create(context.Context, runtime.ContainerSpec) (runtime.ContainerID, error) {
	f.record("create")
	return runtime.ContainerID("fake-id"), nil
}
func (f *stubStreamingRuntime) Start(context.Context, runtime.ContainerID) error {
	f.record("start")
	return nil
}
func (f *stubStreamingRuntime) Exec(context.Context, runtime.ContainerID, runtime.ExecSpec) (runtime.ExecOutput, error) {
	f.record("exec")
	return runtime.ExecOutput{}, nil
}
func (f *stubStreamingRuntime) ExecStreaming(ctx context.Context, id runtime.ContainerID, spec runtime.StreamingExecSpec) (*runtime.StreamingExec, error) {
	f.mu.Lock()
	f.calls = append(f.calls, "exec_streaming")
	f.execSpecs = append(f.execSpecs, spec)
	f.mu.Unlock()
	return f.cli.ExecStreaming(ctx, id, spec)
}
func (f *stubStreamingRuntime) Stop(context.Context, runtime.ContainerID, time.Duration) error {
	f.record("stop")
	return nil
}
func (f *stubStreamingRuntime) Remove(context.Context, runtime.ContainerID) error {
	f.record("remove")
	return nil
}
func (f *stubStreamingRuntime) Exists(context.Context, string) (bool, error) { return false, nil }

func (f *stubStreamingRuntime) record(call string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, call)
}

// streamingSpecs returns a copy of the exec specs the host has started agents
// with so far, taken under the lock.
func (f *stubStreamingRuntime) streamingSpecs() []runtime.StreamingExecSpec {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]runtime.StreamingExecSpec(nil), f.execSpecs...)
}

// --- capturing PublishEvents server ------------------------------------------

// capturePublish is a RunnerService handler that captures every PublishEvents
// frame the relay sends, so the relay's Runner-sequencing is asserted over a
// real wire. Enroll/Sessions are unimplemented — the relay tests only exercise
// PublishEvents.
type capturePublish struct {
	compassv1internalconnect.UnimplementedRunnerServiceHandler
	frames chan *compassv1internal.PublishEventsRequest
}

func newCapturePublish() *capturePublish {
	return &capturePublish{frames: make(chan *compassv1internal.PublishEventsRequest, 64)}
}

func (c *capturePublish) PublishEvents(_ context.Context, stream *connect.ClientStream[compassv1internal.PublishEventsRequest]) (*connect.Response[compassv1internal.PublishEventsResponse], error) {
	for stream.Receive() {
		c.frames <- stream.Msg()
	}
	if err := stream.Err(); err != nil {
		return nil, err
	}
	return connect.NewResponse(&compassv1internal.PublishEventsResponse{}), nil
}

// recvFrame reads one captured frame with a fail-fast deadline.
func (c *capturePublish) recvFrame(t *testing.T) *compassv1internal.PublishEventsRequest {
	t.Helper()
	select {
	case f := <-c.frames:
		return f
	case <-timeAfter():
		t.Fatal("timed out waiting for a relayed PublishEvents frame")
		return nil
	}
}

// --- h2c transport -----------------------------------------------------------

func cleartextHTTP2() *http.Protocols {
	p := new(http.Protocols)
	p.SetHTTP1(true)
	p.SetUnencryptedHTTP2(true)
	return p
}

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
// server and returns a live generated client to it. Torn down via t.Cleanup.
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

// newLink builds a ServerLink over the given RunnerService client (white-box:
// the client field is what StartAgent's relay publisher needs).
func newLink(client compassv1internalconnect.RunnerServiceClient) *ServerLink {
	return &ServerLink{client: client}
}
