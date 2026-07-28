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
	"strings"
	"syscall"
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
	// First cleanup registered, so LIFO removes the tree LAST — after
	// runSessionsLoop's drain has confirmed the loop left dispatch (see there).
	runtimeDir := shortRuntimeDir(t)
	ctx, cancel := context.WithCancel(context.Background())
	// Registered adjacent to WithCancel so the ~60 lines of fixture setup below
	// (openStoreFixture, runner.Dial, NewConfigSpecBuilder) cannot t.Fatalf out
	// with the context never cancelled. runSessionsLoop registers cancel again,
	// later, so LIFO still runs its copy FIRST and the drain ordering the loop
	// documents is unchanged; context.CancelFunc is idempotent, so the second
	// call here is a no-op.
	t.Cleanup(cancel)

	st, agent, bus, sub := openStoreFixture(t, ctx, dsn)
	homeChannel := agent.Agent.HomeChannelID
	// shortRuntimeDir budgeted the path against a MODEL of the account id
	// (accountIDHexLen "f"s), because it runs before an account exists. Tie the
	// model to the real minted value now that it does: widen store ids and this
	// reddens here, rather than silently invalidating that budget and letting
	// the real socket path overrun.
	if got := len(agent.ID); got != accountIDHexLen {
		t.Fatalf("minted account id is %d chars, but shortRuntimeDir budgeted for %d; update accountIDHexLen", got, accountIDHexLen)
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
		NamePrefix:  agentNamePrefix,
	})
	if err != nil {
		t.Fatalf("NewConfigSpecBuilder: %v", err)
	}
	registry := runtime.NewAgentRegistry()
	rt := runtime.NewAgentRuntimeWithRegistry(engine, registry)
	host := runner.NewSessionHost(link, rt, registry, engine, specs, runtimeDir, discardLog(), nil)
	loopDone := runSessionsLoop(t, ctx, cancel, link, host)

	containerName := provisionWhenSeamLive(t, ctx, hub, agent.ID)

	// Start the session → the Runner spawns the agent relay over the pipe engine.
	startResp, err := hub.Start(ctx, "start-1", &compassv1.StartAgentSessionRequest{ContainerName: containerName})
	if err != nil {
		t.Fatalf("hub.Start over the seam = %v", err)
	}
	sessionID := startResp.GetSessionId()
	if sessionID == "" {
		t.Fatal("Start returned an empty session id")
	}

	writeRelayFrames(t, engine)

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

	assertCleanShutdown(t, ctx, cancel, host, engine, sessionID, loopDone)
}

// assertCleanShutdown mirrors production shutdown (run.go): stop the session,
// then cancel the loop. closeStdout models the container's stdout closing on
// death (the fake's pipe is not ctx-bound the way a real exec's is), so the
// relay's scan ends; cancel then unwinds RunSessions' blocking Receive.
//
// This is the CLEAN path, asserted as its own property. The cleanup registered
// by runSessionsLoop covers the failing paths, where a t.Fatalf skips this.
func assertCleanShutdown(t *testing.T, ctx context.Context, cancel context.CancelFunc, host runner.SessionHost, engine *integPipeRuntime, sessionID string, loopDone <-chan error) {
	t.Helper()
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

// provisionWhenSeamLive provisions the workspace through the public hub path →
// the Runner launches the (fake) container and returns its name.
//
// The Sessions stream is server-speaks-first: RunSessions' bootstrap Send
// flushes the headers that run the server handler's router.attach, and that
// round-trip is async to the loop goroutine. A command dispatched into the
// pre-attach window gets a retriable Unavailable ("no live runner sessions
// stream") — the same transient a production client rides out. So gate on the
// seam being live by retrying, idempotent on the stable request id "prov-1" so
// there is no double-provision, yielding to the handler goroutine between
// probes. A deadline bounds it, so a genuinely wedged seam fails fast rather
// than spinning, and nothing here is a sleep.
func provisionWhenSeamLive(t *testing.T, ctx context.Context, hub *runnerhub.Hub, agentID store.AccountID) string {
	t.Helper()
	deadline := time.After(integrationTimeout)
	for {
		resp, err := hub.Provision(ctx, "prov-1", &compassv1.ProvisionAgentWorkspaceRequest{
			AgentAccountId: string(agentID),
			Repo:           &compassv1.ProvisionAgentWorkspaceRequest_LocalPath{LocalPath: "/mirror/repo.git"},
		})
		if err == nil {
			name := resp.GetContainerName()
			if name == "" {
				t.Fatal("Provision returned an empty container name")
			}
			return name
		}
		if connect.CodeOf(err) != connect.CodeUnavailable {
			t.Fatalf("hub.Provision over the seam = %v", err)
		}
		select {
		case <-deadline:
			t.Fatalf("hub.Provision never reached a live Sessions stream: %v", err)
		default:
		}
		stdruntime.Gosched()
	}
}

// writeRelayFrames drives the agent side of the seam: the relay goroutine is
// reading the agent's stdout pipe, so write one conversation frame (committed
// through to the store) and one lifecycle frame (fanned onto the bus). Together
// they exercise both write-through paths the integration exists to cover.
func writeRelayFrames(t *testing.T, engine *integPipeRuntime) {
	t.Helper()
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
}

// openStoreFixture opens the store and builds the account + bus fixture the seam
// writes through: a real agent account and its home channel (the write-through
// target for a relayed conversation frame — the agent is a member of its home
// channel, so AppendMessage authorizes), plus the real event bus and a live
// subscription observing the lifecycle status the seam fans out.
//
// Its cleanups register here, so under LIFO they run after everything the caller
// registers later: the store and bus outlive the Runner loop that talks to them.
func openStoreFixture(t *testing.T, ctx context.Context, dsn string) (*store.Store, store.Account, *events.Bus[*compassv1.SubscribeEventsResponse], events.Subscription[*compassv1.SubscribeEventsResponse]) {
	t.Helper()
	st, err := store.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(st.Close)

	admin, err := st.BootstrapAdmin(ctx, store.NewUser{Handle: "admin", DisplayName: "Administrator"})
	if err != nil {
		t.Fatalf("BootstrapAdmin: %v", err)
	}
	agent, err := st.CreateAgent(ctx, admin.ID, store.NewAgent{Handle: "atlas", DisplayName: "Atlas"})
	if err != nil {
		t.Fatalf("CreateAgent: %v", err)
	}

	bus := events.NewBus[*compassv1.SubscribeEventsResponse]()
	t.Cleanup(bus.Close)
	sub, err := bus.Subscribe(0, 0)
	if err != nil {
		t.Fatalf("bus.Subscribe: %v", err)
	}
	return st, agent, bus, sub
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

// agentNamePrefix is the container-name prefix this test wires into its
// SpecDefaults, hoisted so shortRuntimeDir models the same name the Runner
// actually builds (BuildSpec in spec.go joins it with the account id). Editing
// the prefix in one place would otherwise silently shrink the modelled path and
// turn the budget assertion into a false negative.
const agentNamePrefix = "compass-agent-"

// accountIDHexLen is the width of a store account id: 16 random bytes
// hex-encoded (internal/store/ids.go:21-28). Fixed at that minting site — the
// only one — rather than validated where the path is built, so it is the right
// width to model the tail with and the wrong thing to call a guarantee.
const accountIDHexLen = 32

// runSessionsLoop starts the Runner's dispatch loop and registers the teardown
// that must bracket it. The ordering is load-bearing in both directions, which
// is why it lives beside the goroutine rather than beside context.WithCancel.
//
// It must run BEFORE httptest's srv.Close (registered inside mountRunnerServer,
// therefore earlier, therefore later under LIFO): Close waits on its handlers,
// and the live Sessions handler returns only once ctx is cancelled, so
// cancelling after Close deadlocks the entire cleanup stack — every later
// cleanup, the runtime dir removal included, never runs.
//
// It must run AFTER the runtime dir removal registered at the top of the test,
// so LIFO reclaims that tree only once the loop has left dispatch. Cancel alone
// would not do it: cancel signals and returns, so the WAIT is what makes the
// ordering mean anything.
//
// The wait is what orders the two; it is not a gate on the removal. If the
// drain times out, the later cleanups still run — a cleanup cannot cancel the
// ones registered before it. Neither `return` (it exits only its own closure)
// nor t.Fatalf (it marks the test and runs the rest of the stack anyway) skips
// them, so the timeout arm reports the collision rather than averting it. That
// is the right trade at this point: the test has already failed, and the
// alternative — a flag threaded into shortRuntimeDir to skip RemoveAll — leaks
// the tree and couples two independent helpers to buy nothing a red test needs.
func runSessionsLoop(t *testing.T, ctx context.Context, cancel context.CancelFunc, link *runner.ServerLink, host runner.SessionHost) <-chan error {
	t.Helper()
	loopDone := make(chan error, 1)
	go func() {
		loopDone <- link.RunSessions(ctx, host)
		// Closed as well as sent: the clean-teardown drain at the end of the test
		// takes the value, and the cleanup below must still observe the exit.
		close(loopDone)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-loopDone:
		case <-time.After(integrationTimeout):
			t.Errorf("RunSessions still running %s after cancel; the runtime dir removal below runs anyway and will race an in-flight Provision", integrationTimeout)
		}
	})
	return loopDone
}

// shortRuntimeDir is a Runner RuntimeDir bounded to fit the AF_UNIX sun_path
// limit, replacing t.TempDir() for the one test here that builds a real agent
// socket. The Runner appends a 69-byte tail to its RuntimeDir
// (/containers/compass-agent-<32hex>/agent.sock, host.go:291) at the
// store-minted 32-hex id, leaving 38 bytes of a 107-byte cap on Linux.
// t.TempDir() derives its path from the TEST NAME, and every one of this
// package's tests exceeds that budget: the ceiling is a 19-character name and
// the shortest here is 26. Only this test fails today because only this one
// opens a socket, so the name-length dependency is a trap for the next test
// that wires a SessionHost.
//
// A fixed short root removes the TEST-NAME dependency. It does not make the
// budget unconditional: the root still comes from TMPDIR, and a deep one (a CI
// work dir, or macOS's ~49-byte /var/folders/<2>/<hash>/T) re-inflates it. So
// the resulting path is asserted rather than assumed.
//
// This site FAILS rather than skips on an over-budget root: a skip would
// silently drop the only end-to-end coverage of the socket path, reporting `ok`
// for a test that asserted nothing. The gateway's padTo fails closed for the
// same reason. The cap is derived the way the production guard derives it
// (gateway.sunPathMax) rather than written down — sun_path is not one size
// across the platforms //go:build unix admits (108 on linux/solaris/illumos,
// 104 on darwin and the BSDs, 1023 on aix), and this file is //go:build unix.
// The derivation is duplicated below because that constant is unexported; the
// NUMBER is never restated.
func shortRuntimeDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "cr") //nolint:usetesting // t.TempDir embeds the test name, which is what put this path over the sun_path cap — the bug this helper exists to prevent
	if err != nil {
		t.Fatalf("MkdirTemp for runner runtime dir: %v", err)
	}
	// Longest path the Runner builds under dir. sun_path holds the path plus a
	// NUL, so the usable cap is one less than the platform's array.
	const sunPathMax = len(syscall.RawSockaddrUnix{}.Path) - 1
	longest := filepath.Join(dir, "containers", agentNamePrefix+strings.Repeat("f", accountIDHexLen), "agent.sock")
	if len(longest) > sunPathMax {
		if rmErr := os.RemoveAll(dir); rmErr != nil {
			t.Errorf("removing over-budget runner runtime dir %q: %v", dir, rmErr)
		}
		t.Fatalf("runner runtime dir %q yields a %d-byte agent socket path, over the %d-byte sun_path cap (TMPDIR too deep)", dir, len(longest), sunPathMax)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(dir); err != nil {
			t.Errorf("removing runner runtime dir %q: %v", dir, err)
		}
	})
	return dir
}

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
