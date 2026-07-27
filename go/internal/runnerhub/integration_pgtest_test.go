//go:build pgtest && unix

package runnerhub_test

// End-to-end T4 seam integration, gated behind the `pgtest` tag and SKIPPING
// cleanly when no Postgres/podman runtime is available (via internal/pgtest, the
// same gate serve_pgtest_test.go uses). It drives the WHOLE public path against
// a real store + real event bus:
//
//	hub.Provision → Sessions relay → real Runner (runner.RunSessions) →
//	AgentRuntime.Launch (pipe-backed fake container) → hub.Start → the agent
//	relay (StartAgent) → PublishEvents → hub.Deliver → the real store + bus.
//
// The container is a pipe-backed fake ContainerRuntime (no real compass-agent
// image exists in CI): its ExecStreaming returns a StreamingExec whose IO.Stdout
// is an io.PipeReader the test writes framed protojson agent frames into. A
// conversation frame is written THROUGH Deliver to a store-backed
// ConversationSink (observed by reading the message back), and a session
// lifecycle frame fans onto SubscribeEvents (observed on a real bus
// subscription). This is the seam terminating a real wire and committing to a
// real store — an external-package (black-box) test, driving only exported APIs.

import (
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	stdruntime "runtime"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/encoding/protojson"

	"github.com/sealedsecurity/compass/go/events"
	compassv1 "github.com/sealedsecurity/compass/go/gen/compass/v1"
	compassv1internal "github.com/sealedsecurity/compass/go/internal/gen/compass/v1"
	"github.com/sealedsecurity/compass/go/internal/pgtest"
	"github.com/sealedsecurity/compass/go/internal/runner"
	"github.com/sealedsecurity/compass/go/internal/runnerhub"
	"github.com/sealedsecurity/compass/go/internal/runtime"
	"github.com/sealedsecurity/compass/go/internal/store"
)

const integrationTimeout = 30 * time.Second

func TestIntegrationProvisionStartRelayToStoreAndBus(t *testing.T) {
	dsn := pgtest.RequireDSN(t) // SKIPs when no podman/DSN — never fails hard.
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	st, err := store.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(st.Close)

	// A real agent account + its home channel: the write-through target for a
	// relayed conversation frame. The agent is a member of its home channel, so
	// AppendMessage authorizes.
	admin, err := st.BootstrapAdmin(ctx, store.NewUser{Handle: "admin", DisplayName: "Administrator"})
	if err != nil {
		t.Fatalf("BootstrapAdmin: %v", err)
	}
	agent, err := st.CreateAgent(ctx, admin.ID, store.NewAgent{Handle: "atlas", DisplayName: "Atlas"})
	if err != nil {
		t.Fatalf("CreateAgent: %v", err)
	}
	homeChannel := agent.Agent.HomeChannelID

	// The real event bus (the SubscribeEvents surface) + a live subscription to
	// observe the lifecycle status the seam fans out.
	bus := events.NewBus[*compassv1.SubscribeEventsResponse]()
	t.Cleanup(bus.Close)
	sub, err := bus.Subscribe(0, 0)
	if err != nil {
		t.Fatalf("bus.Subscribe: %v", err)
	}

	// A store-backed ConversationSink: a relayed conversation frame is committed
	// to the agent's home channel and the commit is signalled so the test
	// event-gates on the observed write, not a sleep.
	convSink := &storeConversationSink{
		store:     st,
		channelID: homeChannel,
		author:    agent.ID,
		committed: make(chan store.MessageID, 4),
	}
	hub := runnerhub.NewHub(convSink, busLifecycle{bus: bus}, noopTail{}, nil, discardLog())

	// Mount the RunnerService door on an h2c server, accepting one Runner token.
	resolver := &integResolver{token: "runner-tok", subj: store.Subject{Kind: store.SubjectRunner, ID: "runner-1"}}
	url := mountRunnerServer(t, hub, resolver.resolve)

	// A real Runner dials in over the wire with a pipe-backed container engine.
	engine := newIntegPipeRuntime(t)
	link, err := runner.Dial(ctx, runner.RunnerConfig{
		RunnerID:   "runner-1",
		ServerAddr: url,
		Token:      "runner-tok",
		Engine:     engine,
		HTTPClient: h2cClient(t),
	})
	if err != nil {
		t.Fatalf("runner.Dial: %v", err)
	}
	if link.Reattached() {
		t.Fatal("first enroll reattached = true, want false")
	}

	// Drive the Runner's Sessions dispatch loop with the production SessionHost
	// over the pipe-backed engine + spec builder.
	specs, err := runner.NewConfigSpecBuilder(runner.SpecDefaults{
		Image:       "compass-agent:latest",
		Egress:      runtime.MustAllowEgress("github.com"),
		CheckoutDir: "/work/repo",
		HomeDir:     "/home/agent",
		UID:         1000,
		NamePrefix:  "compass-agent-",
	})
	if err != nil {
		t.Fatalf("NewConfigSpecBuilder: %v", err)
	}
	registry := runtime.NewAgentRegistry()
	rt := runtime.NewAgentRuntimeWithRegistry(engine, registry)
	host := runner.NewSessionHost(link, rt, registry, engine, specs, t.TempDir(), discardLog(), nil)
	loopDone := make(chan error, 1)
	go func() { loopDone <- link.RunSessions(ctx, host) }()

	// Provision the workspace through the public hub path → the Runner launches
	// the (fake) container and returns its name. The Sessions stream is
	// server-speaks-first: RunSessions' bootstrap Send flushes the headers that
	// run the server handler's router.attach, and that round-trip is async to the
	// goroutine spawned above. A command dispatched into the pre-attach window
	// gets a retriable Unavailable ("no live runner sessions stream") — the same
	// transient a production client rides out. Gate on the seam being live by
	// retrying Provision (idempotent on its stable request id "prov-1", so no
	// double-provision), yielding to the handler goroutine between probes; a
	// deadline bounds it so a genuinely wedged seam fails fast, never a sleep.
	var provResp *compassv1.ProvisionAgentWorkspaceResponse
	provDeadline := time.After(integrationTimeout)
	for {
		provResp, err = hub.Provision(ctx, "prov-1", &compassv1.ProvisionAgentWorkspaceRequest{
			AgentAccountId: string(agent.ID),
			Repo:           &compassv1.ProvisionAgentWorkspaceRequest_LocalPath{LocalPath: "/mirror/repo.git"},
		})
		if err == nil {
			break
		}
		if connect.CodeOf(err) != connect.CodeUnavailable {
			t.Fatalf("hub.Provision over the seam = %v", err)
		}
		select {
		case <-provDeadline:
			t.Fatalf("hub.Provision never reached a live Sessions stream: %v", err)
		default:
		}
		stdruntime.Gosched()
	}
	containerName := provResp.GetContainerName()
	if containerName == "" {
		t.Fatal("Provision returned an empty container name")
	}

	// Start the session → the Runner spawns the agent relay over the pipe engine.
	startResp, err := hub.Start(ctx, "start-1", &compassv1.StartAgentSessionRequest{ContainerName: containerName})
	if err != nil {
		t.Fatalf("hub.Start over the seam = %v", err)
	}
	sessionID := startResp.GetSessionId()
	if sessionID == "" {
		t.Fatal("Start returned an empty session id")
	}

	// The relay goroutine is now reading the agent's stdout pipe. Write one
	// conversation frame (→ committed to the store) and one lifecycle frame (→
	// fanned onto the bus).
	writeAgentFrame(t, engine.stdoutW, &compassv1internal.AgentFrame{
		Frame: &compassv1internal.AgentFrame_ConversationPosted{
			ConversationPosted: &compassv1.MessagePosted{
				Message: &compassv1.Message{
					Blocks: []*compassv1.MessageBlock{{Block: &compassv1.MessageBlock_Text{Text: "e2e relayed reply"}}},
				},
			},
		},
	})
	writeAgentFrame(t, engine.stdoutW, &compassv1internal.AgentFrame{
		Frame: &compassv1internal.AgentFrame_Session{
			Session: &compassv1internal.SessionFrame{State: compassv1.AgentSessionState_AGENT_SESSION_STATE_WORKING},
		},
	})

	// The conversation frame was written THROUGH Deliver to the real store: gate
	// on the commit signal, then read the message back.
	select {
	case <-convSink.committed:
	case <-time.After(integrationTimeout):
		t.Fatal("conversation frame never committed to the store through the seam")
	}
	msgs, err := st.ListMessages(ctx, agent.ID, store.ContainerRef{ChannelID: homeChannel}, store.Page{Limit: 10})
	if err != nil {
		t.Fatalf("ListMessages: %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("store has %d messages, want 1 (the relayed frame written through)", len(msgs))
	}
	if got := textOf(msgs[0]); got != "e2e relayed reply" {
		t.Fatalf("stored message text = %q, want the relayed frame body", got)
	}

	// The lifecycle frame fanned onto the real bus: gate on the live event.
	status := waitLifecycle(t, sub)
	if status.GetSessionId() != sessionID {
		t.Fatalf("lifecycle status session id = %q, want %q", status.GetSessionId(), sessionID)
	}
	if status.GetState() != compassv1.AgentSessionState_AGENT_SESSION_STATE_WORKING {
		t.Fatalf("lifecycle status state = %v, want WORKING (the relayed transition)", status.GetState())
	}

	// Clean teardown mirrors production shutdown (run.go): stop the session, then
	// cancel the loop. closeStdout models the container's stdout closing on death
	// (the fake's pipe is not ctx-bound the way a real exec's is), so the relay's
	// scan ends; cancel then unwinds RunSessions' blocking Receive.
	if err := host.Stop(ctx, sessionID); err != nil {
		t.Fatalf("host.Stop = %v", err)
	}
	engine.closeStdout()
	cancel()
	select {
	case <-loopDone:
	case <-time.After(integrationTimeout):
		t.Fatal("RunSessions loop did not end after ctx cancel")
	}
}

// storeConversationSink commits a relayed conversation frame to a fixed channel
// as a fixed author (the seam's session→channel mapping T5 will own; fixed here
// so the test drives a real store write-through). It signals each commit so the
// test event-gates on the observed row.
type storeConversationSink struct {
	store     *store.Store
	channelID store.ChannelID
	author    store.AccountID
	committed chan store.MessageID
}

func (s *storeConversationSink) PostAgentMessage(ctx context.Context, _ string, posted *compassv1.MessagePosted, updated *compassv1.MessageUpdated) error {
	var msg *compassv1.Message
	switch {
	case posted != nil:
		msg = posted.GetMessage()
	case updated != nil:
		msg = updated.GetMessage()
	}
	text := firstText(msg)
	stored, _, err := s.store.AppendMessage(ctx, store.Message{
		Container:       store.ContainerRef{ChannelID: s.channelID},
		AuthorAccountID: s.author,
		Blocks:          []store.MessageBlock{{Text: &text}},
	}, "")
	if err != nil {
		return err
	}
	s.committed <- stored.ID
	return nil
}

// busLifecycle fans an AgentSessionStatus onto the SubscribeEvents bus, matching
// board.Projection's production bus fan-out (minus the projection's own recording).
type busLifecycle struct {
	bus *events.Bus[*compassv1.SubscribeEventsResponse]
}

func (s busLifecycle) PublishSessionStatus(status *compassv1.AgentSessionStatus) {
	s.bus.Publish(&compassv1.SubscribeEventsResponse{
		Payload: &compassv1.SubscribeEventsResponse_AgentSessionStatus{AgentSessionStatus: status},
	})
}

// noopTail is the observation-pane tail sink; the integration path asserts the
// conversation + lifecycle surfaces, so the tail is a no-op here.
type noopTail struct{}

func (noopTail) RelaySessionFrame(string, *compassv1internal.SessionFrame) {}

// integResolver accepts exactly one Runner token.
type integResolver struct {
	token string
	subj  store.Subject
}

func (r *integResolver) resolve(_ context.Context, presented string, want store.SubjectKind) (store.Subject, error) {
	if presented != r.token || r.subj.Kind != want {
		return store.Subject{}, store.ErrNotFound
	}
	return r.subj, nil
}

// integPipeRuntime is the pipe-backed fake ContainerRuntime: ExecStreaming
// returns a StreamingExec whose IO.Stdout is an io.PipeReader the test writes
// framed agent frames into, plus a real terminatable Process (via a shell-stub
// PodmanCLI) so host.Stop's Terminate works. Lifecycle methods are no-op
// successes so Launch registers a handle.
type integPipeRuntime struct {
	stdoutR *io.PipeReader
	stdoutW *io.PipeWriter
	stderrR *io.PipeReader
	stderrW *io.PipeWriter
	cli     *runtime.PodmanCLI // shell-stub podman → a real terminatable Process
}

func newIntegPipeRuntime(t *testing.T) *integPipeRuntime {
	t.Helper()
	outR, outW := io.Pipe()
	errR, errW := io.Pipe()
	dir := t.TempDir()
	stub := "#!/bin/sh\nexec sleep 120\n"
	prog := filepath.Join(dir, "podman-stub.sh")
	if err := os.WriteFile(prog, []byte(stub), 0o755); err != nil {
		t.Fatalf("writing streaming stub: %v", err)
	}
	return &integPipeRuntime{
		stdoutR: outR, stdoutW: outW, stderrR: errR, stderrW: errW,
		cli: runtime.NewPodmanCLI().WithProgram(prog),
	}
}

func (f *integPipeRuntime) Create(context.Context, runtime.ContainerSpec) (runtime.ContainerID, error) {
	return runtime.ContainerID("fake-id"), nil
}
func (f *integPipeRuntime) Start(context.Context, runtime.ContainerID) error { return nil }
func (f *integPipeRuntime) Exec(context.Context, runtime.ContainerID, runtime.ExecSpec) (runtime.ExecOutput, error) {
	return runtime.ExecOutput{}, nil
}
func (f *integPipeRuntime) ExecStreaming(ctx context.Context, id runtime.ContainerID, spec runtime.StreamingExecSpec) (*runtime.StreamingExec, error) {
	// A real streaming exec against the shell stub gives a live, terminatable
	// Process (host.Stop calls Process.Terminate). Its own stdio goes to the
	// harmless sleep; the relay reads OUR stdout pipe (the frames the test
	// writes) instead, so we swap IO.Stdout/Stderr onto the returned handle.
	xs, err := f.cli.ExecStreaming(ctx, id, spec)
	if err != nil {
		return nil, err
	}
	xs.IO.Stdout = f.stdoutR
	xs.IO.Stderr = f.stderrR
	return xs, nil
}
func (f *integPipeRuntime) Stop(context.Context, runtime.ContainerID, time.Duration) error {
	return nil
}
func (f *integPipeRuntime) Remove(context.Context, runtime.ContainerID) error { return nil }
func (f *integPipeRuntime) Exists(context.Context, string) (bool, error)      { return false, nil }
func (f *integPipeRuntime) closeStdout()                                      { _ = f.stdoutW.Close() }

// --- helpers -----------------------------------------------------------------

func writeAgentFrame(t *testing.T, w io.Writer, frame *compassv1internal.AgentFrame) {
	t.Helper()
	b, err := protojson.Marshal(frame)
	if err != nil {
		t.Fatalf("marshal frame: %v", err)
	}
	if _, err := w.Write(append(b, '\n')); err != nil {
		t.Fatalf("write frame: %v", err)
	}
}

func waitLifecycle(t *testing.T, sub events.Subscription[*compassv1.SubscribeEventsResponse]) *compassv1.AgentSessionStatus {
	t.Helper()
	deadline := time.After(integrationTimeout)
	// Replay first (in case the publish landed before the loop reads Live).
	for _, e := range sub.Replay {
		if s := e.Payload.GetAgentSessionStatus(); s != nil {
			return s
		}
	}
	for {
		select {
		case ev, ok := <-sub.Live:
			if !ok {
				t.Fatal("bus Live closed before a lifecycle status arrived")
			}
			if s := ev.Payload.GetAgentSessionStatus(); s != nil {
				return s
			}
		case <-deadline:
			t.Fatal("no AgentSessionStatus fanned onto the bus through the seam")
		}
	}
}

func firstText(m *compassv1.Message) string {
	for _, b := range m.GetBlocks() {
		if t := b.GetText(); t != "" {
			return t
		}
	}
	return ""
}

func textOf(m store.Message) string {
	for _, b := range m.Blocks {
		if b.Text != nil {
			return *b.Text
		}
	}
	return ""
}

func discardLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func cleartextH2() *http.Protocols {
	p := new(http.Protocols)
	p.SetHTTP1(true)
	p.SetUnencryptedHTTP2(true)
	return p
}

func h2cClient(t *testing.T) *http.Client {
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

func mountRunnerServer(t *testing.T, hub *runnerhub.Hub, resolve runnerhub.TokenResolver) string {
	t.Helper()
	path, handler := runnerhub.NewMountedHandler(hub, resolve)
	mux := http.NewServeMux()
	mux.Handle(path, handler)
	srv := httptest.NewUnstartedServer(mux)
	srv.Config.Protocols = cleartextH2()
	srv.Start()
	t.Cleanup(srv.Close)
	return srv.URL
}

var _ = runtime.ContainerID("")
