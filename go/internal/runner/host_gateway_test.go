//go:build unix

package runner

// agentHost's host-process Provision leg: when the engine satisfies the
// unexported hostStateEngine probe (AgentStateDir), Provision runs the host
// leg. The host tier has NO bind mounts, so the leg serves the per-agent gateway
// socket and materializes the config tree INSIDE the handle's own 0700 state dir
// and threads both paths to the agent as env vars on the streaming exec — never
// mounting them at the frozen /run/compass paths. A serve/materialize failure
// after Launch tears both the socket and the container down. Every case names a
// contract a plausible bug would break.

import (
	"context"
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

// hostStateFakeRuntime is a WorkloadRuntime that ALSO implements the
// hostStateEngine probe (AgentStateDir), so agentHost drives its host-process
// Provision leg. Create mints a real 0700 state dir per container (with the
// socket/ and config/ subdirs the host backend's Create makes), keyed by the
// synthetic id it returns — so AgentStateDir(handle.ID()) resolves the same dir
// the leg serves into. It embeds the stub so ExecStreaming drives a real
// terminatable child for Start/Stop.
type hostStateFakeRuntime struct {
	*stubStreamingRuntime
	stateRoot string
	// stateDirs records the minted state dir per synthetic id, so a test can
	// assert the leg served/materialized inside exactly that dir.
	stateDirs map[runtime.WorkloadID]string
	// missing, when true, makes AgentStateDir report ok=false for every id — the
	// "backend reports no handle" resolve-miss path.
	missing bool
	// blockConfig, when true, makes Create leave a regular FILE at the state
	// dir's config path (creating only the socket subdir), so the socket serves
	// but the leg's later Materialize → ensureRoot MkdirAll hits ENOTDIR — the
	// materialize-fails-after-socket-serves failure path.
	blockConfig bool
}

func newHostStateFakeRuntime(t *testing.T) *hostStateFakeRuntime {
	t.Helper()
	return &hostStateFakeRuntime{
		stubStreamingRuntime: newStubStreamingRuntime(t),
		stateRoot:            t.TempDir(),
		stateDirs:            map[runtime.WorkloadID]string{},
	}
}

func (r *hostStateFakeRuntime) Create(_ context.Context, spec runtime.WorkloadSpec) (runtime.WorkloadID, error) {
	id := runtime.WorkloadID(spec.Name)
	dir := filepath.Join(r.stateRoot, spec.Name)
	// Mirror the host backend's Create: private 0700 state dir. blockConfig makes
	// the config path a regular file so a later Materialize fails after the
	// socket is already serving; otherwise create both subdirs the leg uses.
	if err := os.MkdirAll(filepath.Join(dir, "socket"), 0o700); err != nil {
		return "", err
	}
	if r.blockConfig {
		if err := os.WriteFile(filepath.Join(dir, "config"), []byte("x"), 0o600); err != nil {
			return "", err
		}
	}
	r.mu.Lock()
	r.calls = append(r.calls, "create")
	r.created = append(r.created, spec)
	r.mu.Unlock()
	r.stateDirs[id] = dir
	return id, nil
}

func (r *hostStateFakeRuntime) AgentStateDir(id runtime.WorkloadID) (string, bool) {
	if r.missing {
		return "", false
	}
	dir, ok := r.stateDirs[id]
	return dir, ok
}

// newHostGatewayFixture builds the concrete *agentHost over the host-state fake
// runtime whose ServerLink forwards to relay, returning the host and the engine.
// The transport wiring mirrors newVsockGatewayFixture.
func newHostGatewayFixture(t *testing.T, relay compassv1internalconnect.RunnerServiceHandler) (*agentHost, *hostStateFakeRuntime) {
	t.Helper()
	engine := newHostStateFakeRuntime(t)
	registry := runtime.NewAgentRegistry()
	rt := runtime.NewAgentRuntimeWithRegistry(engine, registry)
	link := newLink(newRunnerServiceServer(t, relay))
	specs := &fakeSpecBuilder{spec: liveSpec()}
	var n int
	newID := func() string { n++; return "sess-" + string(rune('0'+n)) }
	host := NewSessionHost(link, rt, registry, engine, specs, AgentHostConfig{RuntimeDir: t.TempDir()}, discardLoggerRunner(), newID)
	return host.(*agentHost), engine
}

// hostAgentHandle is the fixed agent-handle argument the host-leg tests
// provision with — a 32-hex account id the spec builder echoes onto the spec.
const hostAgentHandle = "0123456789abcdef0123456789abcdef"

// TestHostProvisionServesInStateDirWithNoMounts pins the host leg: the spec
// reaching the engine carries NO agent-socket mount and NO config mount (a host
// process has none), the socket is served at a path INSIDE the handle's own
// state dir, and the config tree is materialized under that same state dir. The
// listener the host records serves the REAL generated handler, dialable over
// plain AF_UNIX.
func TestHostProvisionServesInStateDirWithNoMounts(t *testing.T) {
	fake := &recordingRelay{}
	h, engine := newHostGatewayFixture(t, fake)
	ctx := context.Background()

	name, err := h.Provision(ctx, &compassv1.ProvisionAgentWorkspaceRequest{AgentHandle: hostAgentHandle})
	if err != nil {
		t.Fatalf("Provision = %v, want success", err)
	}

	// The spec that reached the engine carries only the workspace mount: the host
	// leg appends neither the agent-socket mount nor the config mount — mirroring
	// the vsock leg's no-refused-mount assertion (host_vsock_gateway_test.go).
	created := engine.createdSpecs()
	if len(created) != 1 {
		t.Fatalf("engine created %d containers, want 1", len(created))
	}
	for _, m := range created[0].Mounts {
		if m.ContainerPath == agentSocketMountPath {
			t.Fatalf("host provision appended the agent-socket mount %q; a host process has no mounts", m.ContainerPath)
		}
		if m.ContainerPath == agentConfigMountPath {
			t.Fatalf("host provision appended the config mount %q; a host process has no mounts", m.ContainerPath)
		}
	}

	// The socket was served inside the handle's state dir, not at a RuntimeDir
	// path — the whole point of the leg.
	stateDir, ok := engine.AgentStateDir(runtime.WorkloadID(name))
	if !ok {
		t.Fatal("fake engine has no state dir for the provisioned container")
	}
	wantSocket := filepath.Join(stateDir, "socket", agentSocketFile)
	gotSocket := listenerPath(t, h, name)
	if gotSocket != wantSocket {
		t.Fatalf("recorded listener path = %q, want the state-dir socket %q", gotSocket, wantSocket)
	}

	// The config tree was materialized under the state dir's config root (the
	// unconfigured-fleet bundle still ensures the root exists).
	wantConfigRoot := filepath.Join(stateDir, "config")
	if info, statErr := os.Stat(wantConfigRoot); statErr != nil || !info.IsDir() {
		t.Fatalf("config root %q not created under the state dir: err=%v", wantConfigRoot, statErr)
	}

	// The recorded listener serves the real generated handler: a bound session
	// round-trips over the state-dir socket.
	sessionID, err := h.Start(ctx, &compassv1.StartAgentSessionRequest{ContainerName: name}, "")
	if err != nil {
		t.Fatalf("Start = %v", err)
	}
	t.Cleanup(func() { _ = h.Stop(context.Background(), sessionID) }) // cleanup: best-effort teardown of the started agent.

	client := runnertest.DialAgentSocket(t, gotSocket)
	callCtx, cancel := context.WithTimeout(ctx, testTimeout)
	defer cancel()
	resp, err := client.Comms(callCtx, connect.NewRequest(&compassv1internal.CommsCallRequest{
		CallId: "hc-1",
		Call: &compassv1internal.CommsCallRequest_Post{
			Post: &compassv1.PostMessageRequest{Container: &compassv1.PostMessageRequest_ChannelId{ChannelId: "chan-1"}},
		},
	}))
	if err != nil {
		t.Fatalf("Comms over the state-dir socket = %v, want the round-trip result", err)
	}
	if resp.Msg.GetCallId() != "hc-1" {
		t.Fatalf("result call id = %q, want hc-1", resp.Msg.GetCallId())
	}
}

// TestHostStartThreadsTransportEnvVars pins that the host leg threads BOTH the
// socket path and the config root onto the agent's streaming exec as env vars —
// pointing at paths inside the handle's own state dir — so the agent (which has
// no mounts) dials and reads where the leg served. The container tiers set
// neither var; here both must be present and state-dir-rooted.
func TestHostStartThreadsTransportEnvVars(t *testing.T) {
	fake := &recordingRelay{}
	h, engine := newHostGatewayFixture(t, fake)
	ctx := context.Background()

	name, err := h.Provision(ctx, &compassv1.ProvisionAgentWorkspaceRequest{AgentHandle: hostAgentHandle})
	if err != nil {
		t.Fatalf("Provision = %v", err)
	}
	sessionID, err := h.Start(ctx, &compassv1.StartAgentSessionRequest{ContainerName: name}, "")
	if err != nil {
		t.Fatalf("Start = %v", err)
	}
	t.Cleanup(func() { _ = h.Stop(context.Background(), sessionID) }) // cleanup: best-effort teardown of the started agent.

	stateDir, ok := engine.AgentStateDir(runtime.WorkloadID(name))
	if !ok {
		t.Fatal("fake engine has no state dir for the provisioned container")
	}
	got := onlyStreamingSpec(t, engine.stubStreamingRuntime)
	wantSocket := filepath.Join(stateDir, "socket", agentSocketFile)
	if got.Env["COMPASS_AGENT_SOCKET_PATH"] != wantSocket {
		t.Fatalf("agent exec COMPASS_AGENT_SOCKET_PATH = %q, want the state-dir socket %q", got.Env["COMPASS_AGENT_SOCKET_PATH"], wantSocket)
	}
	wantConfigRoot := filepath.Join(stateDir, "config")
	if got.Env["COMPASS_AGENT_CONFIG_MOUNT_PATH"] != wantConfigRoot {
		t.Fatalf("agent exec COMPASS_AGENT_CONFIG_MOUNT_PATH = %q, want the state-dir config root %q", got.Env["COMPASS_AGENT_CONFIG_MOUNT_PATH"], wantConfigRoot)
	}
}

// TestHostProvisionResolveMissTearsDownSession pins the resolve-miss leg: if the
// backend reports no state dir for the launched name, the launched container is
// still torn down and Provision errs — no agent runs with no reachable transport.
func TestHostProvisionResolveMissTearsDownSession(t *testing.T) {
	fake := &recordingRelay{}
	h, engine := newHostGatewayFixture(t, fake)
	engine.missing = true
	ctx := context.Background()

	_, err := h.Provision(ctx, &compassv1.ProvisionAgentWorkspaceRequest{AgentHandle: hostAgentHandle})
	if err == nil {
		t.Fatal("Provision with an unresolvable state dir = nil, want an error")
	}
	assertRecorded(t, engine.calls, "stop")
	assertRecorded(t, engine.calls, "remove")
	if socketServed(t, h, liveSpec().Name) {
		t.Fatal("a resolve miss left a recorded listener; nothing must be recorded on the failure path")
	}
}

// TestHostProvisionConfigMaterializeFailureTearsDownSocket pins the failure
// symmetry the design calls for: a config-materialize failure AFTER the socket
// is already serving tears the socket down (no leak) AND tears the launched
// container down. The materialize is forced to fail by pre-occupying the config
// root path with a regular FILE, so ensureRoot's MkdirAll hits ENOTDIR.
func TestHostProvisionConfigMaterializeFailureTearsDownSocket(t *testing.T) {
	// A non-empty, MOVING bundle so Materialize takes the unpack path (which
	// calls ensureRoot) rather than the unconfigured no-op.
	pub := newCapturePublish()
	pub.setConfigBundle(configBundleAt(t, "v-1"))
	h, engine := newHostGatewayFixture(t, pub)
	ctx := context.Background()

	// blockConfig makes the fake's Create leave a regular file at the config path,
	// so the leg's Materialize → ensureRoot MkdirAll fails ENOTDIR AFTER the
	// socket is already serving — the "materialize fails after serve" path.
	engine.blockConfig = true
	name := liveSpec().Name

	_, err := h.Provision(ctx, &compassv1.ProvisionAgentWorkspaceRequest{AgentHandle: hostAgentHandle})
	if err == nil {
		t.Fatal("Provision with an unwritable config root = nil, want the materialize error")
	}
	// The socket served before the materialize must not leak.
	if socketServed(t, h, name) {
		t.Fatal("a failed config materialize left the socket served; it must be torn down")
	}
	// The launched container was torn down (stop + remove through Teardown).
	assertRecorded(t, engine.calls, "stop")
	assertRecorded(t, engine.calls, "remove")
}
