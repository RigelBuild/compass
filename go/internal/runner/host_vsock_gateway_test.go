//go:build unix

package runner

// agentHost's microVM (vsock-gateway) Provision leg: when the engine satisfies
// the unexported vsockGatewayEngine probe, Provision inverts its order (Launch
// first, then serve), refuses BOTH refused mounts (the agent socket and the
// config tree), and serves the SAME generated AgentGateway handler over the
// per-session suffixed AF_UNIX path the backend reports — recording the listener
// in the same h.sockets map so teardown closes it. A Serve failure after Launch
// tears the launched session down. And the podman path (no probe) stays
// byte-identical: socket served pre-Launch, both mounts appended. RefreshConfig
// skips a probed session so a ConfigVersion signal never churns a microVM agent
// it cannot deliver config to. Every case names a contract a plausible bug
// would break.

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"testing"

	"connectrpc.com/connect"

	compassv1 "github.com/RigelBuild/compass/go/gen/compass/v1"
	compassv1internal "github.com/RigelBuild/compass/go/internal/gen/compass/v1"
	"github.com/RigelBuild/compass/go/internal/gen/compass/v1/compassv1internalconnect"
	"github.com/RigelBuild/compass/go/internal/runnertest"
	"github.com/RigelBuild/compass/go/internal/runtime"
)

// vsockGatewayFakeRuntime is a ContainerRuntime that ALSO implements the
// vsockGatewayEngine probe (AgentGatewayEndpoint), so agentHost drives its
// microVM Provision leg. Create returns the container name as its engine id (so
// each container is distinguishable), and AgentGatewayEndpoint hands back a real
// short-lived AF_UNIX path under the test's own dir — the plain suffixed listener
// gateway.Serve binds, dialable hermetically with no vsock. It embeds the stub
// so ExecStreaming drives a real terminatable child for Start/Stop.
type vsockGatewayFakeRuntime struct {
	*stubStreamingRuntime
	endpointDir string
	// endpoints records the resolved path per container name, so a test can dial
	// exactly what Provision served.
	endpoints map[string]string
	// missing, when true, makes AgentGatewayEndpoint report ok=false for every
	// name — the "backend reports no session" resolve-miss path.
	missing bool
}

func newVsockGatewayFakeRuntime(t *testing.T) *vsockGatewayFakeRuntime {
	t.Helper()
	return &vsockGatewayFakeRuntime{
		stubStreamingRuntime: newStubStreamingRuntime(t),
		endpointDir:          t.TempDir(),
		endpoints:            map[string]string{},
	}
}

func (r *vsockGatewayFakeRuntime) Create(_ context.Context, spec runtime.ContainerSpec) (runtime.ContainerID, error) {
	r.mu.Lock()
	r.calls = append(r.calls, "create")
	r.created = append(r.created, spec)
	r.mu.Unlock()
	return runtime.ContainerID(spec.Name), nil
}

func (r *vsockGatewayFakeRuntime) AgentGatewayEndpoint(name string) (string, bool) {
	if r.missing {
		return "", false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	path, ok := r.endpoints[name]
	if !ok {
		// A short path (the endpoint dir is a t.TempDir leaf) so the AF_UNIX
		// bind fits the sun_path budget; the leaf mirrors the suffixed shape.
		path = filepath.Join(r.endpointDir, name+"_1025")
		r.endpoints[name] = path
	}
	return path, true
}

// newVsockGatewayFixture builds the concrete *agentHost over the vsock-gateway
// fake runtime whose ServerLink forwards to relay, returning the host and the
// engine. The transport wiring mirrors newTransportFixture.
func newVsockGatewayFixture(t *testing.T, relay compassv1internalconnect.RunnerServiceHandler) (*agentHost, *vsockGatewayFakeRuntime) {
	t.Helper()
	engine := newVsockGatewayFakeRuntime(t)
	registry := runtime.NewAgentRegistry()
	rt := runtime.NewAgentRuntimeWithRegistry(engine, registry)
	link := newLink(newRunnerServiceServer(t, relay))
	specs := &fakeSpecBuilder{spec: liveSpec()}
	var n int
	newID := func() string { n++; return "sess-" + string(rune('0'+n)) }
	host := NewSessionHost(link, rt, registry, engine, specs, AgentHostConfig{RuntimeDir: t.TempDir()}, discardLoggerRunner(), newID)
	return host.(*agentHost), engine
}

// TestVsockProvisionServesAtSuffixedPathWithNoRefusedMounts pins the microVM
// leg: the spec reaching the engine carries ONLY the workspace mount (no agent
// socket, no config tree), no socket is served pre-Launch, and the listener the
// host records post-Launch serves the REAL generated handler at the fake's
// suffixed path — dialable over plain AF_UNIX with no vsock.
func TestVsockProvisionServesAtSuffixedPathWithNoRefusedMounts(t *testing.T) {
	fake := &recordingRelay{}
	h, engine := newVsockGatewayFixture(t, fake)
	ctx := context.Background()

	name, err := h.Provision(ctx, &compassv1.ProvisionAgentWorkspaceRequest{AgentHandle: "0123456789abcdef0123456789abcdef"})
	if err != nil {
		t.Fatalf("Provision = %v, want success", err)
	}

	// The spec that reached the engine carries only the workspace mount: no
	// agent-socket mount, no config mount — the microVM backend refuses both.
	created := engine.createdSpecs()
	if len(created) != 1 {
		t.Fatalf("engine created %d containers, want 1", len(created))
	}
	for _, m := range created[0].Mounts {
		if m.ContainerPath == agentSocketMountPath {
			t.Fatalf("vsock provision appended the agent-socket mount %q; it must be replaced by vsock", m.ContainerPath)
		}
		if m.ContainerPath == agentConfigMountPath {
			t.Fatalf("vsock provision appended the config mount %q; it must be deferred", m.ContainerPath)
		}
	}

	// The listener was recorded at the fake's suffixed endpoint path.
	wantPath, ok := engine.AgentGatewayEndpoint(name)
	if !ok {
		t.Fatal("fake engine has no endpoint for the provisioned container")
	}
	gotPath := listenerPath(t, h, name)
	if gotPath != wantPath {
		t.Fatalf("recorded listener path = %q, want the suffixed endpoint %q", gotPath, wantPath)
	}

	// No config version was seeded (the materialize block is skipped).
	h.mu.Lock()
	_, seeded := h.configVersions[name]
	h.mu.Unlock()
	if seeded {
		t.Fatal("vsock provision seeded a config version; the materialize block must be skipped (§(f))")
	}

	// The recorded listener serves the real generated handler: a bound session
	// round-trips, an unbound one fails closed.
	sessionID, err := h.Start(ctx, &compassv1.StartAgentSessionRequest{ContainerName: name}, "")
	if err != nil {
		t.Fatalf("Start = %v", err)
	}
	t.Cleanup(func() { _ = h.Stop(context.Background(), sessionID) })

	client := runnertest.DialAgentSocket(t, gotPath)
	callCtx, cancel := context.WithTimeout(ctx, testTimeout)
	defer cancel()
	resp, err := client.Comms(callCtx, connect.NewRequest(&compassv1internal.CommsCallRequest{
		CallId: "vc-1",
		Call: &compassv1internal.CommsCallRequest_Post{
			Post: &compassv1.PostMessageRequest{Container: &compassv1.PostMessageRequest_ChannelId{ChannelId: "chan-1"}},
		},
	}))
	if err != nil {
		t.Fatalf("Comms over the vsock-suffixed socket = %v, want the round-trip result", err)
	}
	got := fake.snapshot()
	if len(got) != 1 || got[0].GetSessionId() != sessionID {
		t.Fatalf("relayed calls = %+v, want exactly one carrying session id %q", got, sessionID)
	}
	if resp.Msg.GetCallId() != "vc-1" {
		t.Fatalf("result call id = %q, want vc-1", resp.Msg.GetCallId())
	}
}

// TestVsockProvisionTeardownClosesListener pins teardown symmetry on the microVM
// leg: Remove tears the container down and closes the suffixed listener.
func TestVsockProvisionTeardownClosesListener(t *testing.T) {
	fake := &recordingRelay{}
	h, engine := newVsockGatewayFixture(t, fake)
	ctx := context.Background()

	name, err := h.Provision(ctx, &compassv1.ProvisionAgentWorkspaceRequest{AgentHandle: "0123456789abcdef0123456789abcdef"})
	if err != nil {
		t.Fatalf("Provision = %v", err)
	}
	path := listenerPath(t, h, name)

	if err := h.Remove(ctx, name); err != nil {
		t.Fatalf("Remove = %v, want success", err)
	}
	if socketServed(t, h, name) {
		t.Fatal("suffixed listener still served after Remove; the socket must be closed")
	}
	assertRecorded(t, engine.calls, "stop")
	assertRecorded(t, engine.calls, "remove")
	// The socket file is gone — a fresh bind at the path succeeds.
	l, err := net.Listen("unix", path)
	if err != nil {
		t.Fatalf("re-binding the suffixed path after Remove = %v; the socket file must be removed", err)
	}
	_ = l.Close() // cleanup: the bind was only to prove the file was removed.
}

// TestVsockProvisionServeFailureTearsDownSession pins the failure symmetry: a
// Serve failure after a successful Launch (a pre-occupied suffixed path) tears
// the launched session down through the runtime Teardown and Provision errs, so
// no VM outlives a session whose gateway never came up.
func TestVsockProvisionServeFailureTearsDownSession(t *testing.T) {
	fake := &recordingRelay{}
	h, engine := newVsockGatewayFixture(t, fake)
	ctx := context.Background()

	// Pre-occupy the suffixed path with a NON-socket file so listenAgentSocket's
	// reclaim refuses it and Serve fails.
	name := "cont-1"
	occupied := filepath.Join(engine.endpointDir, name+"_1025")
	engine.endpoints[name] = occupied
	if err := os.WriteFile(occupied, []byte("x"), 0o600); err != nil {
		t.Fatalf("pre-occupying suffixed path: %v", err)
	}

	_, err := h.Provision(ctx, &compassv1.ProvisionAgentWorkspaceRequest{AgentHandle: "0123456789abcdef0123456789abcdef"})
	if err == nil {
		t.Fatal("Provision with an unservable suffixed path = nil, want a Serve error")
	}
	// The launched session was torn down (stop + remove through Teardown).
	assertRecorded(t, engine.calls, "stop")
	assertRecorded(t, engine.calls, "remove")
	// No listener leaked for the name.
	if socketServed(t, h, name) {
		t.Fatal("a failed Serve left a recorded listener; nothing must be recorded on the failure path")
	}
}

// TestVsockProvisionResolveMissTearsDownSession pins the resolve-miss leg: if the
// backend reports no session for the launched name, the launched container is
// still torn down and Provision errs.
func TestVsockProvisionResolveMissTearsDownSession(t *testing.T) {
	fake := &recordingRelay{}
	h, engine := newVsockGatewayFixture(t, fake)
	engine.missing = true
	ctx := context.Background()

	_, err := h.Provision(ctx, &compassv1.ProvisionAgentWorkspaceRequest{AgentHandle: "0123456789abcdef0123456789abcdef"})
	if err == nil {
		t.Fatal("Provision with an unresolvable endpoint = nil, want an error")
	}
	assertRecorded(t, engine.calls, "stop")
	assertRecorded(t, engine.calls, "remove")
}

// TestVsockRefreshConfigSkipsProbedSession pins the refresh gate (§(f)): a
// RefreshConfig pass over a live vsock-gateway session issues NO re-materialize
// and NO Reload — the session is skipped, so a fleet ConfigVersion signal never
// Stop+StartAgent-churns a microVM agent it cannot deliver config to.
//
// The pass is engineered to fire a reload if the gate were removed: the relay
// serves a NON-EMPTY, MOVING bundle version (v-2) and the vsock path never seeds
// configVersions, so an un-gated refreshOneContainer would see "" != "v-2" and
// reload; relabel is stubbed so the Materialize the un-gated path runs would
// SUCCEED (the real chcon would fail on a non-SELinux host and mask the reload
// behind a swallowed error, hiding the regression). The signal is the agent
// relaunch count (exec_streaming): exactly 1 (the Start) with the gate present,
// which would become 2 (Start + churn-reload) if the gate were deleted. A reload
// never calls the engine's Stop (that is Teardown), so a stop-count assertion
// cannot see this churn — the launch count can.
func TestVsockRefreshConfigSkipsProbedSession(t *testing.T) {
	pub := newCapturePublish()
	pub.setConfigBundle(configBundleAt(t, "v-2"))
	h, engine := newVsockGatewayFixture(t, pub)
	ctx := context.Background()
	// Stub relabel so the Materialize an UN-gated pass would run succeeds; with
	// the gate present it is never reached (the probe short-circuits first).
	_ = stubRelabelAnyRoot(t)

	name, err := h.Provision(ctx, &compassv1.ProvisionAgentWorkspaceRequest{AgentHandle: "0123456789abcdef0123456789abcdef"})
	if err != nil {
		t.Fatalf("Provision = %v", err)
	}
	sessionID, err := h.Start(ctx, &compassv1.StartAgentSessionRequest{ContainerName: name}, "")
	if err != nil {
		t.Fatalf("Start = %v", err)
	}
	t.Cleanup(func() { _ = h.Stop(context.Background(), sessionID) })

	if got := engine.countCall("exec_streaming"); got != 1 {
		t.Fatalf("agent launched %d times after Start, want 1", got)
	}
	if err := h.RefreshConfig(ctx); err != nil {
		t.Fatalf("RefreshConfig = %v, want success", err)
	}
	// The probe gate skipped the session: no re-materialize, no reload, so the
	// agent was launched exactly once (its Start). A dropped gate would reload
	// the session and push this to 2.
	if got := engine.countCall("exec_streaming"); got != 1 {
		t.Fatalf("RefreshConfig relaunched the probed session (%d launches, want 1); the gate must skip it (§(f))", got)
	}
}
