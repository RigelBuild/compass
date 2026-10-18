//go:build pgtest && unix

package runnerhub_test

// Exercise the agent-initiated comms path over a real AgentGateway socket, Runner, store, and bus.
// The container runtime is stubbed, but its child process is real and terminatable.

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	stdruntime "runtime"
	"testing"
	"time"

	"connectrpc.com/connect/v2"
	"connectrpc.com/otelconnect"

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
	// Register cleanup first so LIFO removes the runtime tree after the Runner loop drains.
	runtimeDir := runnertest.ShortRuntimeDir(t, agentNamePrefix, accountIDHexLen)
	ctx, cancel := context.WithCancel(context.Background())
	// Cancel on every exit; runnerloop registers an idempotent copy for drain ordering.
	t.Cleanup(cancel)

	st, agent, commsSvc, commsSub := openStoreFixture(t, ctx, dsn)
	homeChannel := agent.Agent.HomeChannelID
	// Verify the minted account id still matches the width used to budget the socket path.
	if got := len(agent.ID); got != accountIDHexLen {
		t.Fatalf("minted account id is %d chars, but ShortRuntimeDir budgeted for %d; update accountIDHexLen", got, accountIDHexLen)
	}

	// The real Comms service exercises the agent write path; Deliver is not used here.
	hub := runnerhub.NewHub(noopLifecycleSink{}, noopTail{}, commsSvc, discardLog())

	// Mount the RunnerService door on an h2c server, accepting one Runner token.
	resolver := &integResolver{token: "runner-tok", subj: store.Subject{Kind: store.SubjectRunner, ID: "runner-1"}}
	url := mountRunnerServer(t, hub, resolver.resolve)

	// A real Runner dials in over the wire with a stub container engine.
	engine := newIntegStubRuntime(t)
	link, err := runner.Dial(ctx, runner.RunnerConfig{
		RunnerID:   "runner-1",
		ServerAddr: url,
		Token:      runner.StaticToken("runner-tok"),
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
	host := runner.NewSessionHost(link, rt, registry, engine, specs, runner.AgentHostConfig{RuntimeDir: runtimeDir}, discardLog())
	loopDone := runnerloop.RunSessionsLoop(t, ctx, cancel, link, host, integrationTimeout)

	// Provision serves the per-container AgentGateway socket (before container
	// launch), so it is live when the agent dials.
	containerName := provisionWhenSeamLive(t, ctx, hub, agent.ID)

	// Start binds the session to the account in the Runner and hub. Before that,
	// a comms call fails closed with CodeFailedPrecondition.
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
	resp, err := client.Comms(callCtx, &compassv1internal.CommsCallRequest{
		CallId: "call-1",
		Call: &compassv1internal.CommsCallRequest_Post{
			Post: &compassv1.PostMessageRequest{Container: &compassv1.PostMessageRequest_ChannelId{ChannelId: "atlas"}, Topic: &compassv1.PostMessageRequest_TopicName{TopicName: "general"}, CreateTopic: true, Blocks: []*compassv1.MessageBlock{{Block: &compassv1.MessageBlock_Text{Text: relayText}}}},
		},
	})
	if err != nil {
		t.Fatalf("Comms over the socket = %v, want the round-trip result", err)
	}

	// The Server resolved session→account from its own binding and attributed
	// the post to the AGENT account (never the bootstrap admin): the round-trip
	// result carries the call id verbatim and the agent as author.
	result := resp
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

// assertCleanShutdown stops the session, cancels the loop, and closes the host.
// Its explicit clean path complements the registered cleanup for failing paths.
func assertCleanShutdown(t *testing.T, ctx context.Context, cancel context.CancelFunc, host runner.SessionHost, sessionID string, loopDone <-chan error) {
	t.Helper()
	if err := host.Stop(ctx, sessionID); err != nil {
		t.Fatalf("host.Stop = %v", err)
	}
	cancel()
	select {
	case err := <-loopDone:
		// Cancellation is expected; another error would indicate a genuine stream failure.
		if err != nil && !errors.Is(err, context.Canceled) {
			t.Fatalf("RunSessions = %v, want a clean end after ctx cancel", err)
		}
	case <-time.After(integrationTimeout):
		t.Fatal("RunSessions loop did not end after ctx cancel")
	}
	// Close the socket after RunSessions exits, with a bounded context detached from cancellation.
	if closer, ok := host.(interface{ Close(ctx context.Context) }); ok {
		closeCtx, cancelClose := context.WithTimeout(context.WithoutCancel(ctx), integrationTimeout)
		defer cancelClose()
		closer.Close(closeCtx)
	}
}

// provisionWhenSeamLive retries the stable request id until the Sessions handler attaches.
// The deadline bounds failure; yielding lets the handler run without sleeping.
func provisionWhenSeamLive(t *testing.T, ctx context.Context, hub *runnerhub.Hub, agentID store.AccountID) string {
	t.Helper()
	deadline := time.After(integrationTimeout)
	for {
		resp, _, err := hub.Provision(ctx, "prov-1", agentID, &compassv1.ProvisionAgentWorkspaceRequest{})
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

// openStoreFixture creates an agent, home channel, live bus subscription, and real Comms service.
// Register cleanup here so the store and bus outlive the Runner loop.
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
	// The handler's admin fallback is overridden per call with the resolved agent account.
	commsSvc := comms.NewComms(ctx, st, bus, nil, admin.ID)
	sub, err := bus.Subscribe(0, 0)
	if err != nil {
		t.Fatalf("bus.Subscribe: %v", err)
	}
	return st, agent, commsSvc, sub
}

// noopLifecycleSink is the hub's LifecycleSink stub; no relayed lifecycle frame
// reaches Deliver on this path.
type noopLifecycleSink struct{}

func (noopLifecycleSink) PublishSessionStatus(context.Context, *compassv1.AgentSessionStatus) {}

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

// integStubRuntime launches a real child process for termination tests.
// Agent protocol traffic uses the socket; the child's pipes stay empty.
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

func (f *integStubRuntime) Create(context.Context, runtime.WorkloadSpec) (runtime.WorkloadID, error) {
	return runtime.WorkloadID("fake-id"), nil
}
func (f *integStubRuntime) Start(context.Context, runtime.WorkloadID) error { return nil }
func (f *integStubRuntime) Exec(context.Context, runtime.WorkloadID, runtime.ExecSpec) (runtime.ExecOutput, error) {
	return runtime.ExecOutput{}, nil
}
func (f *integStubRuntime) ExecStreaming(ctx context.Context, id runtime.WorkloadID, spec runtime.StreamingExecSpec) (*runtime.StreamingExec, error) {
	// ExecStreaming launches a terminatable child whose pipes remain empty until Stop.
	return f.cli.ExecStreaming(ctx, id, spec)
}
func (f *integStubRuntime) Stop(context.Context, runtime.WorkloadID, time.Duration) error {
	return nil
}
func (f *integStubRuntime) Remove(context.Context, runtime.WorkloadID) error { return nil }
func (f *integStubRuntime) Exists(context.Context, string) (bool, error)     { return true, nil }
func (f *integStubRuntime) Running(context.Context, string) (bool, error)    { return false, nil }
func (f *integStubRuntime) MountLabel(context.Context, runtime.WorkloadID) (string, error) {
	return "", nil
}
func (f *integStubRuntime) Resize(context.Context, runtime.WorkloadID, runtime.ResourceLimits) error {
	return nil
}

// --- helpers -----------------------------------------------------------------

// agentSocketPath reconstructs the externally visible socket path from the host layout.
// The test cannot inspect the unexported host socket map.
func agentSocketPath(runtimeDir, containerName string) string {
	return filepath.Join(runtimeDir, "containers", containerName, "agent.sock")
}

// waitMessagePosted checks replay before live because the synchronous publish is already in the ring.
// Its deadline only bounds failure.
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

func discardLog() *slog.Logger { return slog.New(slog.DiscardHandler) }

// agentNamePrefix is shared with SpecDefaults so the runtime-path budget uses the Runner's actual prefix.
const agentNamePrefix = "compass-agent-"

// accountIDHexLen matches the 16-byte, hex-encoded store account ID used for path budgeting.
const accountIDHexLen = 32

func mountRunnerServer(t *testing.T, hub *runnerhub.Hub, resolve runnerhub.TokenResolver) string {
	t.Helper()
	otelIC, err := otelconnect.NewServerInterceptor()
	if err != nil {
		t.Fatalf("otelconnect.NewServerInterceptor: %v", err)
	}
	mux := http.NewServeMux()
	runnerhub.Mount(mux, hub, resolve, nil, nil, otelIC)
	srv := httptest.NewUnstartedServer(mux)
	srv.Config.Protocols = runnertest.CleartextH2()
	srv.Start()
	t.Cleanup(srv.Close)
	return srv.URL
}
