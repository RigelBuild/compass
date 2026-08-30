//go:build pgtest && unix

package runnerhub_test

// End-to-end T5 seam integration, gated behind the `pgtest` tag and SKIPPING
// cleanly when no Postgres/podman runtime is available (via internal/pgtest, the
// same gate serve_pgtest_test.go uses). It drives the WHOLE public path — an
// agent-initiated comms call over the real per-container AgentGateway socket —
// against a real store + real comms bus:
//
//	hub.Provision → Sessions relay → real Runner (runner.RunSessions) →
//	AgentRuntime.Launch (stub container) + gateway.Serve (the per-container Unix
//	socket) → hub.Start (binds session→account) → the agent dials the socket →
//	Gateway.Comms → RelayCommsCall → hub → comms.PostAsAccount → the real store
//	+ the real comms bus.
//
// Post-#16 the retired stdout→PublishEvents relay is gone: the agent's protocol
// no longer rides stdout (that pipe is diagnostics-only), so a conversation is
// no longer a stdout frame written through Deliver. It is now a CommsCallRequest
// Post the in-container agent makes over its socket. The Server resolves the
// relayed session_id to the bound agent account (fail-closed) and executes the
// Post under it through the SAME PostMessage handler a human takes — so it
// commits a Message row to Postgres (observed by reading it back under the agent
// account) AND fans MessagePosted onto the comms bus (observed on a live
// subscription). The container is a stub ContainerRuntime (no real compass-agent
// image exists in CI) whose ExecStreaming spawns a live, terminatable child so
// host.Stop reaps a real process. This is the seam terminating a real
// agent-initiated wire and committing to a real store — an external-package
// (black-box) test, driving only exported APIs.

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	stdruntime "runtime"
	"testing"
	"time"

	"connectrpc.com/connect"

	"github.com/RigelBuild/compass/go/events"
	compassv1 "github.com/RigelBuild/compass/go/gen/compass/v1"
	"github.com/RigelBuild/compass/go/internal/comms"
	compassv1internal "github.com/RigelBuild/compass/go/internal/gen/compass/v1"
	"github.com/RigelBuild/compass/go/internal/pgtest"
	"github.com/RigelBuild/compass/go/internal/runner"
	"github.com/RigelBuild/compass/go/internal/runnerhub"
	"github.com/RigelBuild/compass/go/internal/runnertest"
	"github.com/RigelBuild/compass/go/internal/runnertest/runnerloop"
	"github.com/RigelBuild/compass/go/internal/runtime"
	"github.com/RigelBuild/compass/go/internal/store"
)

const integrationTimeout = 30 * time.Second

// relayText is the body the agent posts over the socket, asserted back out of
// the store (the committed row) and the comms bus (the fanned MessagePosted).
const relayText = "e2e reply over the AgentGateway socket"

func TestIntegrationSocketPostCommitsToStoreAndFansOnBus(t *testing.T) {
	dsn := pgtest.RequireDSN(t) // SKIPs when no podman/DSN — never fails hard.
	// First cleanup registered, so LIFO removes the tree LAST — after
	// runnerloop.RunSessionsLoop's drain has confirmed the loop left dispatch (see there).
	runtimeDir := runnertest.ShortRuntimeDir(t, agentNamePrefix, accountIDHexLen)
	ctx, cancel := context.WithCancel(context.Background())
	// Registered adjacent to WithCancel so the ~60 lines of fixture setup below
	// (openStoreFixture, runner.Dial, NewConfigSpecBuilder) cannot t.Fatalf out
	// with the context never cancelled. runnerloop.RunSessionsLoop registers cancel again,
	// later, so LIFO still runs its copy FIRST and the drain ordering the loop
	// documents is unchanged; context.CancelFunc is idempotent, so the second
	// call here is a no-op.
	t.Cleanup(cancel)

	st, agent, commsSvc, commsSub := openStoreFixture(t, ctx, dsn)
	homeChannel := agent.Agent.HomeChannelID
	// runnertest.ShortRuntimeDir budgeted the path against a MODEL of the account id
	// (accountIDHexLen "f"s), because it runs before an account exists. Tie the
	// model to the real minted value now that it does: widen store ids and this
	// reddens here, rather than silently invalidating that budget and letting
	// the real socket path overrun.
	if got := len(agent.ID); got != accountIDHexLen {
		t.Fatalf("minted account id is %d chars, but ShortRuntimeDir budgeted for %d; update accountIDHexLen", got, accountIDHexLen)
	}

	// comms is the hub's CommsCaller — the real agent-comms execution leg over
	// the real store + bus. Deliver never runs on this path (the stdout relay
	// #16 retired is gone, so no relayed frame reaches the write-through sinks),
	// so the two write-through sinks are no-ops; only the RelayCommsCall leg is
	// exercised.
	hub := runnerhub.NewHub(noopLifecycleSink{}, noopTail{}, commsSvc, discardLog())

	// Mount the RunnerService door on an h2c server, accepting one Runner token.
	resolver := &integResolver{token: "runner-tok", subj: store.Subject{Kind: store.SubjectRunner, ID: "runner-1"}}
	url := mountRunnerServer(t, hub, resolver.resolve)

	// A real Runner dials in over the wire with a stub container engine.
	engine := newIntegStubRuntime(t)
	link, err := runner.Dial(ctx, runner.RunnerConfig{
		RunnerID:   "runner-1",
		ServerAddr: url,
		Token:      "runner-tok",
		Engine:     engine,
		HTTPClient: runnertest.H2CClient(t),
	})
	if err != nil {
		t.Fatalf("runner.Dial: %v", err)
	}
	if link.Reattached() {
		t.Fatal("first enroll reattached = true, want false")
	}

	// Drive the Runner's Sessions dispatch loop with the production SessionHost
	// over the stub engine + spec builder.
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
	host := runner.NewSessionHost(link, rt, registry, engine, specs, runner.AgentHostConfig{RuntimeDir: runtimeDir}, discardLog(), nil)
	loopDone := runnerloop.RunSessionsLoop(t, ctx, cancel, link, host, integrationTimeout)

	// Provision serves the per-container AgentGateway socket (before container
	// launch), so it is live when the agent dials.
	containerName := provisionWhenSeamLive(t, ctx, hub, agent.ID)

	// Start binds the session to the account BOTH in the Runner (so the Gateway
	// resolves container→session) AND in the hub (so RelayCommsCall resolves
	// session→account). A comms call before this fails closed CodePermissionDenied.
	startResp, err := hub.Start(ctx, "start-1", &compassv1.StartAgentSessionRequest{ContainerName: containerName})
	if err != nil {
		t.Fatalf("hub.Start over the seam = %v", err)
	}
	sessionID := startResp.GetSessionId()
	if sessionID == "" {
		t.Fatal("Start returned an empty session id")
	}

	// Dial the real per-container socket and make an agent-initiated Comms Post —
	// the exact wire an in-container agent rides. client.Comms blocks until the
	// full round-trip (socket → Gateway → RelayCommsCall → hub → comms → store +
	// bus → back) completes, so both the commit and the fan are done on return.
	client := runnertest.DialAgentSocket(t, agentSocketPath(runtimeDir, containerName))
	callCtx, cancelCall := context.WithTimeout(ctx, integrationTimeout)
	defer cancelCall()
	resp, err := client.Comms(callCtx, connect.NewRequest(&compassv1internal.CommsCallRequest{
		CallId: "call-1",
		Call: &compassv1internal.CommsCallRequest_Post{
			Post: &compassv1.PostMessageRequest{Container: &compassv1.PostMessageRequest_ChannelId{ChannelId: "atlas"}, Topic: &compassv1.PostMessageRequest_TopicName{TopicName: "general"}, CreateTopic: true, Blocks: []*compassv1.MessageBlock{{Block: &compassv1.MessageBlock_Text{Text: relayText}}}},
		},
	}))
	if err != nil {
		t.Fatalf("Comms over the socket = %v, want the round-trip result", err)
	}

	// The Server resolved session→account from its own binding and attributed
	// the post to the AGENT account (never the bootstrap admin): the round-trip
	// result carries the call id verbatim and the agent as author.
	result := resp.Msg
	if got := result.GetCallId(); got != "call-1" {
		t.Fatalf("result call id = %q, want the agent's %q (verbatim forward)", got, "call-1")
	}
	posted := result.GetPost().GetMessage()
	if posted == nil {
		t.Fatal("Comms post result carried no message")
	}
	if got := posted.GetAuthorAccountId(); got != string(agent.ID) {
		t.Fatalf("posted message author = %q, want the bound agent account %q (not admin)", got, agent.ID)
	}

	// Committed to the REAL store: read it back under the agent account.
	msgs, err := st.ListMessages(ctx, store.ListMessagesQuery{Actor: agent.ID, ChannelID: homeChannel, Page: store.Page{Limit: 10}})
	if err != nil {
		t.Fatalf("ListMessages: %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("store has %d messages, want 1 (the socket post committed through)", len(msgs))
	}
	if got := textOf(msgs[0]); got != relayText {
		t.Fatalf("stored message text = %q, want the posted body", got)
	}
	if msgs[0].AuthorAccountID != agent.ID {
		t.Fatalf("stored message author = %q, want the agent account %q", msgs[0].AuthorAccountID, agent.ID)
	}

	// Fanned onto the REAL comms bus as MessagePosted — the new model's event fan
	// for an agent post (the retired relay's SubscribeEvents lifecycle frame has
	// no replacement Runner trigger, so the post's own bus event IS the seam's
	// live-fan coverage). Gate on the live event.
	fanned := waitMessagePosted(t, commsSub)
	// A message carries only its topic now; that it landed in the home channel is
	// proven by the home-scoped store read-back above (the channel is resolved
	// through the topic server-side, not echoed on the wire message).
	if got := fanned.GetMessage().GetTopicId(); got == "" {
		t.Fatal("fanned MessagePosted TopicId = \"\", want the home channel's home topic id")
	}
	if got := firstText(fanned.GetMessage()); got != relayText {
		t.Fatalf("fanned MessagePosted text = %q, want the posted body", got)
	}

	assertCleanShutdown(t, ctx, cancel, host, sessionID, loopDone)
}

// assertCleanShutdown mirrors production shutdown (run.go): stop the session,
// cancel the loop, then close the host (draining the per-container socket). The
// child the stub exec spawned dies on host.Stop's Terminate, closing its pipes
// so StartAgent's drains end on EOF; cancel then unwinds RunSessions' blocking
// Receive.
//
// This is the CLEAN path, asserted as its own property. The cleanup registered
// by runnerloop.RunSessionsLoop covers the failing paths, where a t.Fatalf skips this.
func assertCleanShutdown(t *testing.T, ctx context.Context, cancel context.CancelFunc, host runner.SessionHost, sessionID string, loopDone <-chan error) {
	t.Helper()
	if err := host.Stop(ctx, sessionID); err != nil {
		t.Fatalf("host.Stop = %v", err)
	}
	cancel()
	select {
	case err := <-loopDone:
		// A cancelled ctx is the expected end; anything else is a real stream
		// failure, and discarding it here would let the loop die of a genuine
		// error while this assertion still reads as a clean shutdown.
		if err != nil && !errors.Is(err, context.Canceled) {
			t.Fatalf("RunSessions = %v, want a clean end after ctx cancel", err)
		}
	case <-time.After(integrationTimeout):
		t.Fatal("RunSessions loop did not end after ctx cancel")
	}
	// Mirror run.go's deferred host.Close after RunSessions returns: drain the
	// AgentGateway socket Provision served so its listener goroutine is torn down
	// deterministically rather than left serving until the runtime dir is
	// removed. ctx is cancelled by now, so the bounded drain rides a fresh
	// short-deadline context rooted at the test root (Background is the
	// sanctioned test root; ctx here is already done).
	if closer, ok := host.(interface{ Close(context.Context) }); ok {
		closeCtx, cancelClose := context.WithTimeout(context.Background(), integrationTimeout)
		defer cancelClose()
		closer.Close(closeCtx)
	}
}

// provisionWhenSeamLive provisions the workspace through the public hub path →
// the Runner launches the (stub) container and returns its name.
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
		resp, _, err := hub.Provision(ctx, "prov-1", &compassv1.ProvisionAgentWorkspaceRequest{AgentHandle: string(agentID)})
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

// openStoreFixture opens the store and builds the account + comms fixture the
// seam writes through: a real agent account and its home channel (the agent is a
// member of its home channel, so PostAsAccount authorizes), plus the real comms
// bus, a live subscription observing the MessagePosted the seam fans, and the
// real comms.Comms that is the hub's CommsCaller — it executes the
// agent-initiated Post under the resolved account over the same PostMessage
// handler a human takes (store commit + MessagePosted fan-out).
//
// Its cleanups register here, so under LIFO they run after everything the caller
// registers later: the store and bus outlive the Runner loop that talks to them.
func openStoreFixture(t *testing.T, ctx context.Context, dsn string) (*store.Store, store.Account, *comms.Comms, events.Subscription[*compassv1.SubscribeCommsResponse]) {
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

	bus := events.NewBus[*compassv1.SubscribeCommsResponse]()
	t.Cleanup(bus.Close)
	// adminID is the comms handler's ambient fallback; the agent-initiated leg
	// (PostAsAccount) overrides it per-call with the resolved agent account, so
	// the post attributes to the agent, never the admin.
	commsSvc := comms.NewComms(st, bus, admin.ID)
	sub, err := bus.Subscribe(0, 0)
	if err != nil {
		t.Fatalf("bus.Subscribe: %v", err)
	}
	return st, agent, commsSvc, sub
}

// noopLifecycleSink is the hub's LifecycleSink stub; no relayed lifecycle frame
// reaches Deliver on this path.
type noopLifecycleSink struct{}

func (noopLifecycleSink) PublishSessionStatus(*compassv1.AgentSessionStatus) {}

// noopTail is the observation-pane tail sink; nothing relays session frames on
// this path, so the tail is a no-op here.
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

// integStubRuntime is the fake ContainerRuntime backing the Runner: its
// ExecStreaming spawns a real, terminatable child (a shell-stub `podman`
// exec-ing `sleep`) so host.Stop's Terminate reaps a live process and
// StartAgent's pipe drains end on that reap. Post-#16 nothing rides
// stdout/stderr — the agent's protocol travels the AgentGateway socket — so the
// child's own (empty) pipes are what StartAgent drains and no frame is injected.
// Lifecycle methods are no-op successes so Launch registers a handle.
type integStubRuntime struct {
	cli *runtime.PodmanCLI // shell-stub podman → a real terminatable Process
}

func newIntegStubRuntime(t *testing.T) *integStubRuntime {
	t.Helper()
	dir := t.TempDir()
	stub := "#!/bin/sh\nexec sleep 120\n"
	prog := filepath.Join(dir, "podman-stub.sh")
	if err := os.WriteFile(prog, []byte(stub), 0o755); err != nil {
		t.Fatalf("writing streaming stub: %v", err)
	}
	return &integStubRuntime{cli: runtime.NewPodmanCLI().WithProgram(prog)}
}

func (f *integStubRuntime) Create(context.Context, runtime.ContainerSpec) (runtime.ContainerID, error) {
	return runtime.ContainerID("fake-id"), nil
}
func (f *integStubRuntime) Start(context.Context, runtime.ContainerID) error { return nil }
func (f *integStubRuntime) Exec(context.Context, runtime.ContainerID, runtime.ExecSpec) (runtime.ExecOutput, error) {
	return runtime.ExecOutput{}, nil
}
func (f *integStubRuntime) ExecStreaming(ctx context.Context, id runtime.ContainerID, spec runtime.StreamingExecSpec) (*runtime.StreamingExec, error) {
	// A real streaming exec against the shell stub: a live, terminatable Process
	// (host.Stop → Terminate) whose stdout/stderr pipes StartAgent drains. The
	// stub just sleeps, so the pipes stay empty until Terminate closes them.
	return f.cli.ExecStreaming(ctx, id, spec)
}
func (f *integStubRuntime) Stop(context.Context, runtime.ContainerID, time.Duration) error {
	return nil
}
func (f *integStubRuntime) Remove(context.Context, runtime.ContainerID) error { return nil }
func (f *integStubRuntime) Exists(context.Context, string) (bool, error)      { return false, nil }
func (f *integStubRuntime) MountLabel(context.Context, runtime.ContainerID) (string, error) {
	return "", nil
}
func (f *integStubRuntime) Resize(context.Context, runtime.ContainerID, runtime.ResourceLimits) error {
	return nil
}

// --- helpers -----------------------------------------------------------------

// agentSocketPath is the host path the Runner serves a container's AgentGateway
// socket at: RuntimeDir/containers/<container>/agent.sock (host.go's
// agentSocketDir/agentSocketFile layout). Reconstructed here rather than read
// off the host — h.sockets is unexported and this is an external test package —
// so it is the exact socket an in-container agent bind-mounts and dials.
func agentSocketPath(runtimeDir, containerName string) string {
	return filepath.Join(runtimeDir, "containers", containerName, "agent.sock")
}

// waitMessagePosted returns the first MessagePosted the comms bus fans out,
// checking Replay first (the publish is synchronous within the socket Post's
// round-trip, so it has already landed on the ring by the time client.Comms
// returns) then Live. The deadline bounds a wedged fan so it fails fast; nothing
// here is a sleep.
func waitMessagePosted(t *testing.T, sub events.Subscription[*compassv1.SubscribeCommsResponse]) *compassv1.MessagePosted {
	t.Helper()
	deadline := time.After(integrationTimeout)
	for _, e := range sub.Replay {
		if m := e.Payload.GetMessagePosted(); m != nil {
			return m
		}
	}
	for {
		select {
		case ev, ok := <-sub.Live:
			if !ok {
				t.Fatal("comms bus Live closed before a MessagePosted arrived")
			}
			if m := ev.Payload.GetMessagePosted(); m != nil {
				return m
			}
		case <-deadline:
			t.Fatal("no MessagePosted fanned onto the comms bus through the seam")
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
// SpecDefaults, hoisted so runnertest.ShortRuntimeDir models the same name the Runner
// actually builds (BuildSpec in spec.go joins it with the account id). Editing
// the prefix in one place would otherwise silently shrink the modelled path and
// turn the budget assertion into a false negative.
const agentNamePrefix = "compass-agent-"

// accountIDHexLen is the width of a store account id: 16 random bytes
// hex-encoded (internal/store/ids.go:21-28). Fixed at that minting site — the
// only one — rather than validated where the path is built, so it is the right
// width to model the tail with and the wrong thing to call a guarantee.
const accountIDHexLen = 32

func mountRunnerServer(t *testing.T, hub *runnerhub.Hub, resolve runnerhub.TokenResolver) string {
	t.Helper()
	path, handler := runnerhub.NewMountedHandler(hub, resolve, nil, nil)
	mux := http.NewServeMux()
	mux.Handle(path, handler)
	srv := httptest.NewUnstartedServer(mux)
	srv.Config.Protocols = runnertest.CleartextH2()
	srv.Start()
	t.Cleanup(srv.Close)
	return srv.URL
}
