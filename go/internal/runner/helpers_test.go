//go:build unix

package runner

// Shared scaffolding for the Runner-side seam tests: a pipe-backed fake
// ContainerRuntime (the existing runtime.fakeRuntime.ExecStreaming is a nil-pipe
// stub — this one returns a StreamingExec whose IO.Stdout/IO.Stderr are
// io.PipeReaders the test writes into), a recording fake runtime for the
// Provision→Launch path, a capturing slog handler for the drain's log lines, a
// PublishEvents server that backs newLink's client, and the h2c transport
// helpers.

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
	"sync/atomic"
	"testing"
	"time"

	"connectrpc.com/connect"

	compassv1internal "github.com/sealedsecurity/compass/go/internal/gen/compass/v1"
	"github.com/sealedsecurity/compass/go/internal/gen/compass/v1/compassv1internalconnect"
	"github.com/sealedsecurity/compass/go/internal/runtime"
)

// testAgentEnv is the agent exec configuration the tests start agents with.
// Values are arbitrary but non-empty, so a test asserting on the exec argv sees
// each var actually carried rather than an omitted empty.
func testAgentEnv() AgentEnv {
	return AgentEnv{UID: 1000, HomeDir: "/home/agent", Workdir: "/work/repo", Model: "test-model"}
}

// testTimeout bounds every blocking wait so a wedged drain fails fast instead of
// hanging the suite. A deadline safety net, never a synchronization device:
// tests event-gate on the frames actually observed, not elapsed time.
const testTimeout = 15 * time.Second

func timeAfter() <-chan time.Time { return time.After(testTimeout) }

// discardLoggerRunner builds a slog.Logger that drops output, so the drain's
// per-line diagnostics do not spam the test log.
func discardLoggerRunner() *slog.Logger {
	return slog.New(slog.DiscardHandler)
}

// nopWriteCloser adapts io.Discard to io.WriteCloser for a streaming exec's
// unused stdin.
type nopWriteCloser struct{}

func (nopWriteCloser) Write(p []byte) (int, error) { return len(p), nil }
func (nopWriteCloser) Close() error                { return nil }

// pipeRuntime is a ContainerRuntime whose ExecStreaming returns a StreamingExec
// backed by real in-memory pipes: the test writes lines into stdoutW / stderrW
// and the drains read them off IO.Stdout / IO.Stderr. Its
// lifecycle methods (Create/Start/Exec/…) are recording no-ops so it also serves
// the Provision→Launch path. Process is left nil: the drains read only IO, so a
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
		// Process intentionally nil — the drains read only IO; a test using this
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
func (f *pipeRuntime) MountLabel(context.Context, runtime.ContainerID) (string, error) {
	return "", nil
}

func (f *pipeRuntime) record(call string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, call)
}

// closeStdout closes the stdout pipe writer, ending the stdout drain (agent
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
	mu          sync.Mutex
	calls       []string
	execSpecs   []runtime.StreamingExecSpec
	cli         *runtime.PodmanCLI
	stopErr     error                            // when set, engine Stop fails — models a Teardown partial failure
	stopErrByID map[runtime.ContainerID]error    // per-container Stop error; overrides stopErr for the keyed id
	stopGate    chan struct{}                    // when non-nil, Stop blocks on it (after recording) — test-controlled teardown parking
	callsByID   map[runtime.ContainerID][]string // per-container lifecycle calls (stop/remove), for fan-out isolation assertions
	created     []runtime.ContainerSpec
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

func (f *stubStreamingRuntime) Create(_ context.Context, spec runtime.ContainerSpec) (runtime.ContainerID, error) {
	f.mu.Lock()
	f.calls = append(f.calls, "create")
	f.created = append(f.created, spec)
	f.mu.Unlock()
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
func (f *stubStreamingRuntime) Stop(_ context.Context, id runtime.ContainerID, _ time.Duration) error {
	f.record("stop")
	f.recordForID(id, "stop")
	// Park mid-Stop when the test holds the gate, so a teardown can be observed
	// in progress (stop recorded, remove not yet reached) before it is released.
	f.mu.Lock()
	gate := f.stopGate
	f.mu.Unlock()
	if gate != nil {
		<-gate
	}
	f.mu.Lock()
	err := f.stopErr
	if e, ok := f.stopErrByID[id]; ok {
		err = e
	}
	f.mu.Unlock()
	return err
}
func (f *stubStreamingRuntime) Remove(_ context.Context, id runtime.ContainerID) error {
	f.record("remove")
	f.recordForID(id, "remove")
	return nil
}
func (f *stubStreamingRuntime) Exists(context.Context, string) (bool, error) { return false, nil }
func (f *stubStreamingRuntime) MountLabel(context.Context, runtime.ContainerID) (string, error) {
	return "", nil
}

func (f *stubStreamingRuntime) record(call string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, call)
}

// recordForID records a lifecycle call against a specific container id, so a
// fan-out isolation assertion can prove one container reached remove while
// another aborted at stop.
func (f *stubStreamingRuntime) recordForID(id runtime.ContainerID, call string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.callsByID == nil {
		f.callsByID = map[runtime.ContainerID][]string{}
	}
	f.callsByID[id] = append(f.callsByID[id], call)
}

// countCallForID reports how many times the named lifecycle call was recorded
// for a specific container id, taken under the lock — mirrors countCall for the
// per-container fan-out assertions.
func (f *stubStreamingRuntime) countCallForID(id runtime.ContainerID, call string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.callsByID[id] {
		if c == call {
			n++
		}
	}
	return n
}

// streamingSpecs returns a copy of the exec specs the host has started agents
// with so far, taken under the lock.
func (f *stubStreamingRuntime) streamingSpecs() []runtime.StreamingExecSpec {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]runtime.StreamingExecSpec(nil), f.execSpecs...)
}

// callsSnapshot returns the ordered lifecycle calls the stub has seen — used to
// assert the materialize exec ("exec") ran before the agent launch
// ("exec_streaming").
func (f *stubStreamingRuntime) callsSnapshot() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

// countCall reports how many times the named lifecycle call was recorded, taken
// under the lock — so a Close-drain assertion can count per-container stops and
// removes.
func (f *stubStreamingRuntime) countCall(call string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.calls {
		if c == call {
			n++
		}
	}
	return n
}

// createdSpecs returns a copy of the ContainerSpecs the host has created
// containers with so far, taken under the lock.
func (f *stubStreamingRuntime) createdSpecs() []runtime.ContainerSpec {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]runtime.ContainerSpec(nil), f.created...)
}

// --- capturing PublishEvents server ------------------------------------------

// capturePublish is a RunnerService handler backing newLink's client. Post-C5
// nothing publishes agent frames, so its role is inverted: it exists to prove
// PublishEvents is never used. `opened` counts stream opens — a live publisher
// opens the stream before it sends anything, so a non-zero count catches a
// resurrected relay even when no frame has crossed the wire yet.
type capturePublish struct {
	compassv1internalconnect.UnimplementedRunnerServiceHandler
	frames chan *compassv1internal.PublishEventsRequest
	opened atomic.Uint64
	// secrets is the set FetchSecrets returns (default empty); fetchErr, when
	// set, is returned instead (e.g. a CodeFailedPrecondition no-secrets-surface
	// server); fetchReqs records each request so a test can assert the pre-exec
	// by-container fetch.
	mu        sync.Mutex
	secrets   []*compassv1internal.ResolvedSecret
	fetchErr  error
	fetchReqs []*compassv1internal.FetchSecretsRequest
	// configBundle is the bundle FetchAgentConfig returns (default the empty,
	// unconfigured-fleet bundle); configErr, when set, is returned instead so a
	// test can force the config-materialize path to fail.
	configBundle AgentConfigBundle
	configErr    error
}

func newCapturePublish() *capturePublish {
	return &capturePublish{frames: make(chan *compassv1internal.PublishEventsRequest, 64)}
}

func (c *capturePublish) PublishEvents(_ context.Context, stream *connect.ClientStream[compassv1internal.PublishEventsRequest]) (*connect.Response[compassv1internal.PublishEventsResponse], error) {
	c.opened.Add(1)
	for stream.Receive() {
		c.frames <- stream.Msg()
	}
	if err := stream.Err(); err != nil {
		return nil, err
	}
	return connect.NewResponse(&compassv1internal.PublishEventsResponse{}), nil
}

// FetchSecrets serves the Runner's pre-exec (by-container) and rotation
// (by-session) fetch, returning the configured set and recording the request
// so a test can assert which selector the Runner used. Default set is empty.
func (c *capturePublish) FetchSecrets(_ context.Context, req *connect.Request[compassv1internal.FetchSecretsRequest]) (*connect.Response[compassv1internal.FetchSecretsResponse], error) {
	c.mu.Lock()
	c.fetchReqs = append(c.fetchReqs, req.Msg)
	secrets, fetchErr := c.secrets, c.fetchErr
	c.mu.Unlock()
	if fetchErr != nil {
		return nil, fetchErr
	}
	return connect.NewResponse(&compassv1internal.FetchSecretsResponse{Secrets: secrets}), nil
}

// FetchAgentConfig serves the Runner's config fetch as a server stream: it sends
// the configured bundle's version frame followed by its tarball as one chunk, or
// returns configErr before any frame so a test can force the materialize path to
// fail. The default (zero) bundle streams an empty version frame — the
// unconfigured-fleet path Materialize accepts with no error.
func (c *capturePublish) FetchAgentConfig(_ context.Context, _ *connect.Request[compassv1internal.FetchAgentConfigRequest], stream *connect.ServerStream[compassv1internal.FetchAgentConfigResponse]) error {
	c.mu.Lock()
	bundle, err := c.configBundle, c.configErr
	c.mu.Unlock()
	if err != nil {
		return err
	}
	if err := stream.Send(versionFrame(bundle.Version)); err != nil {
		return err
	}
	if len(bundle.Tarball) > 0 {
		if err := stream.Send(chunkFrame(bundle.Tarball)); err != nil {
			return err
		}
	}
	return nil
}

// sendEmptyAgentConfig streams the unconfigured-fleet bundle (a lone empty
// version frame) — the FetchAgentConfig response the provision-path fixtures
// that don't exercise config need so Materialize succeeds with an empty mount.
func sendEmptyAgentConfig(stream *connect.ServerStream[compassv1internal.FetchAgentConfigResponse]) error {
	return stream.Send(versionFrame(""))
}

// setConfigBundle sets the bundle FetchAgentConfig streams, so a test can move
// the fleet config version between materialize passes.
func (c *capturePublish) setConfigBundle(bundle AgentConfigBundle) {
	c.mu.Lock()
	c.configBundle = bundle
	c.mu.Unlock()
}

// setConfigErr makes FetchAgentConfig return err instead of a bundle, so a test
// can force the config-materialize path to fail.
func (c *capturePublish) setConfigErr(err error) {
	c.mu.Lock()
	c.configErr = err
	c.mu.Unlock()
}

// streamOpens reports how many PublishEvents streams were opened against this
// handler.
func (c *capturePublish) streamOpens() uint64 { return c.opened.Load() }

// setSecrets sets the resolved set FetchSecrets returns.
func (c *capturePublish) setSecrets(secrets ...*compassv1internal.ResolvedSecret) {
	c.mu.Lock()
	c.secrets = secrets
	c.mu.Unlock()
}

// setFetchErr makes FetchSecrets return err (e.g. a CodeFailedPrecondition
// no-secrets-surface server) instead of a resolved set.
func (c *capturePublish) setFetchErr(err error) {
	c.mu.Lock()
	c.fetchErr = err
	c.mu.Unlock()
}

// fetchRequests returns a copy of the FetchSecrets requests seen so far.
func (c *capturePublish) fetchRequests() []*compassv1internal.FetchSecretsRequest {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]*compassv1internal.FetchSecretsRequest(nil), c.fetchReqs...)
}

// --- capturing diagnostic log ------------------------------------------------

// captureLog is a slog.Handler that publishes every record's message and its
// string attributes to a channel, so a test can event-gate on a log line the
// way the PublishEvents tests gate on a frame — no sleeps, no polling. It backs
// the stdout-drain assertions: after the relay is retired, "the bytes reached
// the diagnostic log" is the only observable the drain leaves behind.
type captureLog struct {
	lines   chan logLine
	dropped atomic.Uint64
}

// logLine is one captured record: its message plus the attributes the drain
// stamps (session id and the drained text).
type logLine struct {
	msg   string
	attrs map[string]string
}

func newCaptureLog() *captureLog {
	return &captureLog{lines: make(chan logLine, 64)}
}

func (c *captureLog) Enabled(context.Context, slog.Level) bool { return true }

func (c *captureLog) Handle(_ context.Context, r slog.Record) error {
	line := logLine{msg: r.Message, attrs: make(map[string]string, r.NumAttrs())}
	r.Attrs(func(a slog.Attr) bool {
		line.attrs[a.Key] = a.Value.String()
		return true
	})
	select {
	case c.lines <- line:
	default:
		// A full buffer must never block the unit under test, so the record is
		// dropped — counted, so a downstream timeout can say why.
		c.dropped.Add(1)
	}
	return nil
}

// WithAttrs / WithGroup intentionally drop attrs and groups: the drain stamps
// every attribute inline on its Debug call, so nothing is lost today. A future
// refactor to `log.With(...)` must implement these first, or the session_id
// assertions will silently see an empty map.
func (c *captureLog) WithAttrs([]slog.Attr) slog.Handler { return c }
func (c *captureLog) WithGroup(string) slog.Handler      { return c }

// logger builds the slog.Logger the unit under test writes into.
func (c *captureLog) logger() *slog.Logger { return slog.New(c) }

// recvLine reads one captured log record with a fail-fast deadline. A timeout
// reports any dropped records, so an undersized buffer diagnoses itself instead
// of looking like a wedged drain.
func (c *captureLog) recvLine(t *testing.T) logLine {
	t.Helper()
	select {
	case l := <-c.lines:
		return l
	case <-timeAfter():
		if n := c.dropped.Load(); n > 0 {
			t.Fatalf("timed out waiting for a diagnostic log line (%d records dropped — buffer too small)", n)
		}
		t.Fatal("timed out waiting for a diagnostic log line")
		return logLine{}
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
// the client field backs Sessions and the per-container gateway).
func newLink(client compassv1internalconnect.RunnerServiceClient) *ServerLink {
	return &ServerLink{client: client}
}
