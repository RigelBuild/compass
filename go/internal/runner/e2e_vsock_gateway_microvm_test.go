//go:build microvm && unix

package runner

// KVM-gated E2E tests prove host and guest gateway legs meet over real vsock.
// They skip without KVM unless COMPASS_REQUIRE_MICROVM=1. Hermetic suites cover
// each half; this suite boots a real guest but uses an inert agent so probe call
// counts remain exact. Provision and Start use t.Context() to preserve VM life.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect/v2"

	compassv1 "github.com/RigelBuild/compass/go/gen/compass/v1"
	"github.com/RigelBuild/compass/go/internal/agentuid"
	compassv1internal "github.com/RigelBuild/compass/go/internal/gen/compass/v1"
	"github.com/RigelBuild/compass/go/internal/microvmtest"
	"github.com/RigelBuild/compass/go/internal/runnertest"
	"github.com/RigelBuild/compass/go/internal/runtime"
)

const (
	// w3AccountID is the valid fixed-width lowercase-hex BuildSpec account shape.
	w3AccountID = "0123456789abcdef0123456789abcdef"
	// w3ProbeCallID is checked at the fake relay to prove the full round-trip.
	w3ProbeCallID = "w3-vsock-1"
	// inGuestProbeTimeout bounds one probe without bounding VM lifetime.
	inGuestProbeTimeout = 60 * time.Second
	// Keep the guest firewall probe shorter than its host exec deadline so a
	// dropped SYN is reported as unreachable, not as a harness timeout.
	guestConnectTimeoutSecs = 10
)

// w3Relay returns the no-secrets signal because the guest rootfs cannot host
// secret materialization. Start then reaches the bind-and-launch path under test.
type w3Relay struct {
	*recordingRelay
}

func newW3Relay() *w3Relay { return &w3Relay{recordingRelay: &recordingRelay{}} }

// FetchSecrets signals that the read-only guest rootfs cannot host secrets.
func (r *w3Relay) FetchSecrets(
	context.Context, *compassv1internal.FetchSecretsRequest,
) (*compassv1internal.FetchSecretsResponse, error) {
	return nil, connect.NewError(connect.CodeFailedPrecondition, "w3: no secrets surface")
}

// newMicroVMGatewayFixture mirrors the hermetic fixture with a real guest. It
// returns the host and engine for probe execution.
func newMicroVMGatewayFixture(t *testing.T, relay *w3Relay) (*agentHost, *runtime.MicroVMRuntime) {
	t.Helper()
	env := microvmtest.Require(t)
	engine := runtime.NewMicroVMRuntime(w3MicroVMConfig(t, env))
	registry := runtime.NewAgentRegistry()
	rt := runtime.NewAgentRuntimeWithRegistry(engine, registry)
	link := newLink(newRunnerServiceServer(t, relay))
	specs := &fakeSpecBuilder{spec: w3LiveSpec(t)}
	host := NewSessionHost(link, rt, registry, engine, specs, AgentHostConfig{RuntimeDir: t.TempDir()}, discardLoggerRunner()).(*agentHost)
	closeHostAtCleanup(t, host)
	return host, engine
}

// w3MicroVMConfig keeps the per-session AF_UNIX path within sun_path's 107-byte
// limit by using a short runroot instead of t.TempDir's long test-name path.
func w3MicroVMConfig(t *testing.T, env microvmtest.Env) runtime.MicroVMConfig {
	t.Helper()
	//nolint:usetesting // t.TempDir embeds the long test-function name, overflowing the 107-byte AF_UNIX sun_path budget for the per-session sockets — the very failure this short root prevents.
	runRoot, err := os.MkdirTemp("", "w3vm")
	if err != nil {
		t.Fatalf("creating short microvm runroot: %v", err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(runRoot); err != nil {
			t.Errorf("removing microvm runroot %s: %v", runRoot, err)
		}
	})
	return runtime.MicroVMConfig{
		VMMPath:         env.VMMPath,
		VirtiofsdPath:   env.VirtiofsdPath,
		KernelImage:     env.KernelImage,
		RootfsImage:     env.RootfsImage,
		InitrdImage:     env.InitrdImage,
		RunRoot:         runRoot,
		DefaultCPUs:     2,
		DefaultMemoryMB: 1024,
	}
}

// w3LiveSpec is the minimal launchable microVM spec: one /workspace mount,
// agent uid, and default-deny egress. The backend ignores Image and Command.
func w3LiveSpec(t *testing.T) runtime.AgentSpec {
	t.Helper()
	return runtime.AgentSpec{
		Name:  "w3-vsock-agent",
		Image: "compass-agent:latest",
		Workspace: runtime.Workspace{
			CheckoutDir: "/workspace",
			HomeDir:     "/workspace",
			UID:         agentuid.AgentUID,
		},
		Mounts: []runtime.Mount{{HostPath: t.TempDir(), ContainerPath: "/workspace"}},
		Egress: runtime.EgressPolicy{},
	}
}

// inertAgentCommand prevents real agent traffic from reaching the fake relay.
// The guest's sleep process keeps the session alive until cleanup restores argv.
func inertAgentCommand(t *testing.T) {
	t.Helper()
	orig := agentCommand
	agentCommand = []string{"sleep", "infinity"}
	t.Cleanup(func() { agentCommand = orig })
}

// TestVsockGateway_HostServesSuffixedSocket is check (1): a session booted
// through the full backend has the host serving the AgentGateway over the real
// per-session suffixed AF_UNIX path (<runtimeDir>/vsock.sock_1025, 0600), and a
// host-side Connect client dialing that path round-trips a Comms call under the
// bound session to the fake Server and back. This proves W2's post-Launch serve
// on real hardware: the suffixed path is derived from a real booted session's
// runtime dir, and gateway.Serve reused verbatim answers there.
//
// Mutation that reddens it: a wrong suffixed path (the listener recorded
// elsewhere), no serve on the microVM Provision leg, or a broken session bind
// (the Comms call would then fail closed or carry the wrong session id).
func TestVsockGateway_HostServesSuffixedSocket(t *testing.T) {
	inertAgentCommand(t)
	fake := newW3Relay()
	h, engine := newMicroVMGatewayFixture(t, fake)

	name, err := h.Provision(t.Context(), &compassv1.ProvisionAgentWorkspaceRequest{}, w3AccountID)
	if err != nil {
		t.Fatalf("Provision (boot + serve over vsock suffixed path) = %v", err)
	}
	t.Cleanup(func() { _ = h.Remove(context.WithoutCancel(t.Context()), name) })

	// The recorded host listener is the CH suffixed path derived from the real
	// booted session's runtime dir, and it is exactly what the backend reports.
	gotPath := listenerPath(t, h, name)
	wantPath, ok := engine.AgentGatewayEndpoint(name)
	if !ok {
		t.Fatal("backend reports no gateway endpoint for the booted session")
	}
	if gotPath != wantPath {
		t.Fatalf("recorded listener path = %q, want the backend's suffixed endpoint %q", gotPath, wantPath)
	}
	if !strings.HasSuffix(gotPath, "/vsock.sock_1025") {
		t.Fatalf("suffixed path = %q, want a .../vsock.sock_1025 tail (the CH guest->host contract, port 1025)", gotPath)
	}
	assertSocket0600(t, gotPath)

	// Bind the session, then a host-side Connect client dialing the suffixed
	// path directly (a plain AF_UNIX socket, no vsock needed host-side) round-
	// trips a Comms call through the real Gateway to the fake Server and back.
	sessionID, err := h.Start(t.Context(), &compassv1.StartAgentSessionRequest{ContainerName: name}, "", "test-session")
	if err != nil {
		t.Fatalf("Start (bind session + launch inert in-guest keep-alive) = %v", err)
	}

	client := runnertest.DialAgentSocket(t, gotPath)
	callCtx, cancel := context.WithTimeout(t.Context(), testTimeout)
	defer cancel()
	resp, err := client.Comms(callCtx, &compassv1internal.CommsCallRequest{
		CallId: w3ProbeCallID,
		Call: &compassv1internal.CommsCallRequest_Post{
			Post: &compassv1.PostMessageRequest{Container: &compassv1.PostMessageRequest_ChannelId{ChannelId: "chan-1"}},
		},
	})
	if err != nil {
		t.Fatalf("Comms over the real suffixed socket = %v, want the round-trip result", err)
	}
	got := fake.snapshot()
	if len(got) != 1 {
		t.Fatalf("Server received %d relayed calls, want exactly 1 (the inert keep-alive sends none)", len(got))
	}
	if got[0].GetSessionId() != sessionID {
		t.Fatalf("relayed session id = %q, want the Start-minted %q", got[0].GetSessionId(), sessionID)
	}
	if resp.GetCallId() != w3ProbeCallID {
		t.Fatalf("result call id = %q, want %q (the Server result flowed back)", resp.GetCallId(), w3ProbeCallID)
	}
}

// TestVsockGateway_FailClosedBeforeStart is check (3), host side: the suffixed
// socket is live from Provision before Start binds a session. An early Comms
// call fails with FailedPrecondition and never reaches the Server.
func TestVsockGateway_FailClosedBeforeStart(t *testing.T) {
	inertAgentCommand(t)
	fake := newW3Relay()
	h, _ := newMicroVMGatewayFixture(t, fake)

	name, err := h.Provision(t.Context(), &compassv1.ProvisionAgentWorkspaceRequest{}, w3AccountID)
	if err != nil {
		t.Fatalf("Provision = %v", err)
	}
	t.Cleanup(func() { _ = h.Remove(context.WithoutCancel(t.Context()), name) })
	// Deliberately no Start: the gateway is served, but no session is bound.

	client := runnertest.DialAgentSocket(t, listenerPath(t, h, name))
	callCtx, cancel := context.WithTimeout(t.Context(), testTimeout)
	defer cancel()
	_, err = client.Comms(callCtx, &compassv1internal.CommsCallRequest{CallId: "w3-early"})
	if err == nil {
		t.Fatal("Comms before Start = nil, want CodeFailedPrecondition")
	}
	if code := connect.CodeOf(err); code != connect.CodeFailedPrecondition {
		t.Fatalf("Comms-before-Start code = %v, want CodeFailedPrecondition", code)
	}
	if got := fake.snapshot(); len(got) != 0 {
		t.Fatalf("Server received %d relayed calls before Start, want 0 (never forward an empty session id)", len(got))
	}
}

// TestVsockGateway_TeardownRemovesSuffixedSocket is check (5): Remove tears the
// booted session down and removes the suffixed socket file + recorded listener,
// leaving nothing behind. Mutation that reddens it: Remove not closing the vsock
// listener (the socket file would survive) or not deregistering it (socketServed
// would still report it). The session runtime dir removal is the MicroVMRuntime
// Remove's own contract, proven by the lifecycle suite; here we assert the
// gateway socket specifically.
func TestVsockGateway_TeardownRemovesSuffixedSocket(t *testing.T) {
	inertAgentCommand(t)
	fake := newW3Relay()
	h, _ := newMicroVMGatewayFixture(t, fake)

	name, err := h.Provision(t.Context(), &compassv1.ProvisionAgentWorkspaceRequest{}, w3AccountID)
	if err != nil {
		t.Fatalf("Provision = %v", err)
	}
	t.Cleanup(func() { _ = h.Remove(context.WithoutCancel(t.Context()), name) })
	path := listenerPath(t, h, name)
	if _, err := os.Lstat(path); err != nil {
		t.Fatalf("suffixed socket must exist after Provision: Lstat = %v", err)
	}

	if err := h.Remove(t.Context(), name); err != nil {
		t.Fatalf("Remove = %v, want success", err)
	}
	if socketServed(t, h, name) {
		t.Fatal("suffixed listener still recorded after Remove; the vsock socket must be closed + deregistered")
	}
	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Remove must remove the suffixed socket file: Lstat = %v, want not-exist", err)
	}
}

// TestVsockGateway_InGuestRoundTripOverVsock is check (2) — the load-bearing
// end-to-end: an agent-uid exec INSIDE the booted guest drives a Comms call over
// W1's forwarder (/run/compass/agent.sock -> AF_VSOCK CID 2 port 1025 -> the
// host's real Gateway -> the fake Server) and back. This is the only leg that
// exercises the actual vsock hop; the host-side tests dial the suffixed path
// directly. The probe is a bun one-liner (OQ-5: the guest rootfs ships bun as
// its only Connect-capable runtime — no node/curl/socat), issuing the Connect
// unary over HTTP/1.1 (the gateway serves SetHTTP1) as a JSON POST.
//
// Mutation that reddens it: W1's forwarder absent/misdialed (the guest connect
// fails), the host not serving over vsock (no listener behind the muxer), or a
// broken session bind (the call fails closed instead of round-tripping).
func TestVsockGateway_InGuestRoundTripOverVsock(t *testing.T) {
	inertAgentCommand(t)
	fake := newW3Relay()
	h, engine := newMicroVMGatewayFixture(t, fake)

	name, err := h.Provision(t.Context(), &compassv1.ProvisionAgentWorkspaceRequest{}, w3AccountID)
	if err != nil {
		t.Fatalf("Provision = %v", err)
	}
	t.Cleanup(func() { _ = h.Remove(context.WithoutCancel(t.Context()), name) })
	sessionID, err := h.Start(t.Context(), &compassv1.StartAgentSessionRequest{ContainerName: name}, "", "test-session")
	if err != nil {
		t.Fatalf("Start = %v", err)
	}

	id := resolveContainerID(t, h, name)

	// The in-guest probe: a bun script dialing the agent's fixed socket over
	// HTTP/1.1 Connect-unary, printing a single result line the host parses.
	out := runInGuestProbe(t, engine, id, w3ProbeCallID)
	if out.callID != w3ProbeCallID {
		t.Fatalf("in-guest round-trip result call id = %q, want %q (the call flowed guest->vsock->host Gateway->relay and back)", out.callID, w3ProbeCallID)
	}

	// The host saw exactly the probe's one call, carrying the bound session id —
	// proving the guest's byte stream reached the real Gateway over vsock and was
	// attributed to the session Start bound.
	got := fake.snapshot()
	if len(got) != 1 {
		t.Fatalf("Server received %d relayed calls, want exactly 1 (only the in-guest probe)", len(got))
	}
	if got[0].GetSessionId() != sessionID {
		t.Fatalf("relayed session id = %q, want the Start-minted %q (the vsock leg carried the bound session)", got[0].GetSessionId(), sessionID)
	}
	if got[0].GetCall().GetCallId() != w3ProbeCallID {
		t.Fatalf("relayed call id = %q, want %q (verbatim forward from the guest)", got[0].GetCall().GetCallId(), w3ProbeCallID)
	}
}

// TestVsockGateway_InGuestVsockIsNotIP is check (4), the vsock-is-not-IP
// non-goal (record §(g)): from inside the armed guest netns (default-deny per
// V3), no IP destination reaches the host — while the vsock gateway path still
// works. This proves the vsock channel is orthogonal to the egress seal: the
// gateway is reachable over AF_VSOCK precisely because it is NOT an IP route the
// firewall governs. A regression that tunneled the gateway over IP would either
// be blocked by the firewall (breaking the round-trip) or open an egress hole
// (reddening the deny leg).
func TestVsockGateway_InGuestVsockIsNotIP(t *testing.T) {
	inertAgentCommand(t)
	fake := newW3Relay()
	h, engine := newMicroVMGatewayFixture(t, fake)

	name, err := h.Provision(t.Context(), &compassv1.ProvisionAgentWorkspaceRequest{}, w3AccountID)
	if err != nil {
		t.Fatalf("Provision = %v", err)
	}
	t.Cleanup(func() { _ = h.Remove(context.WithoutCancel(t.Context()), name) })
	if _, err := h.Start(t.Context(), &compassv1.StartAgentSessionRequest{ContainerName: name}, "", "test-session"); err != nil {
		t.Fatalf("Start = %v", err)
	}
	id := resolveContainerID(t, h, name)

	// Positive control: without a working vsock round-trip, both-fail would pass.
	out := runInGuestProbe(t, engine, id, w3ProbeCallID)
	if out.callID != w3ProbeCallID {
		t.Fatalf("in-guest vsock round-trip = %q, want %q; the deny leg is only meaningful if the vsock path works", out.callID, w3ProbeCallID)
	}

	// A raw-IP connection must fail under default-deny even though vsock works.
	// Use an IP to avoid DNS stalls; the guest timeout distinguishes a dropped
	// SYN from a harness failure.
	if inGuestCanReachIP(t, engine, id, "8.8.8.8") {
		t.Fatal("an IP destination reached the guest egress path; the vsock gateway must be orthogonal to (never a hole in) the default-deny IP seal")
	}
}

// probeResult carries the CallId echoed by the host relay; empty means the
// round-trip did not complete.
type probeResult struct {
	callID string
}

// runInGuestProbe sends one HTTP/1.1 Connect JSON POST to the guest socket and
// parses the echoed CallId. Transport errors are harness failures.
func runInGuestProbe(t *testing.T, m *runtime.MicroVMRuntime, id runtime.WorkloadID, callID string) probeResult {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), inGuestProbeTimeout)
	defer cancel()

	// A call_id-only body is enough because the fake relay echoes it in the result.
	reqBody, err := json.Marshal(map[string]any{"call_id": callID})
	if err != nil {
		t.Fatalf("marshaling probe request: %v", err)
	}
	script := inGuestProbeScript(string(reqBody))
	out, err := m.Exec(ctx, id, runtime.NewExecSpec("bun", "-e", script).AsUser(strconv.Itoa(int(agentuid.AgentUID))))
	if err != nil {
		t.Fatalf("in-guest bun probe exec errored (harness fault, not a round-trip verdict): %v", err)
	}
	t.Logf("in-guest probe: exit=%d\nstdout:\n%s\nstderr:\n%s", out.ExitCode, out.Stdout, out.Stderr)
	if out.ExitCode != 0 {
		t.Fatalf("in-guest bun probe exited %d (want 0); stderr: %s", out.ExitCode, out.Stderr)
	}
	return parseProbeResult(t, out.Stdout)
}

// inGuestProbeScript sends a Connect unary over Bun's Unix-socket fetch and emits
// one parseable result line.
func inGuestProbeScript(reqJSON string) string {
	// The URL host is a placeholder; the unix option routes bytes to the socket.
	// The RPC is bounded by AbortSignal so a wedged dial reports an error.
	// reqJSON is embedded in a JS template literal; it must contain no backtick or
	// ${...}. Dynamic payloads must be encoded before insertion.
	return `
const body = ` + "`" + reqJSON + "`" + `;
try {
  const res = await fetch("http://gateway/compass.v1.AgentGateway/Comms", {
    unix: "/run/compass/agent.sock",
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body,
    signal: AbortSignal.timeout(15000),
  });
  const text = await res.text();
  if (res.status !== 200) { console.log("RESULT-ERR status=" + res.status + " body=" + text); process.exit(1); }
  console.log("RESULT " + text);
} catch (e) { console.log("RESULT-ERR " + (e && e.message ? e.message : String(e))); process.exit(1); }
`
}

// parseProbeResult extracts the echoed protobuf-JSON callId. Missing RESULT means
// the probe did not complete.
func parseProbeResult(t *testing.T, stdout string) probeResult {
	t.Helper()
	for line := range strings.SplitSeq(stdout, "\n") {
		rest, ok := strings.CutPrefix(strings.TrimSpace(line), "RESULT ")
		if !ok {
			continue
		}
		var msg struct {
			CallID string `json:"callId"`
		}
		if err := json.Unmarshal([]byte(rest), &msg); err != nil {
			t.Fatalf("parsing probe RESULT json %q: %v", rest, err)
		}
		return probeResult{callID: msg.CallID}
	}
	t.Fatalf("no RESULT line in probe stdout (round-trip did not complete):\n%s", stdout)
	return probeResult{}
}

// inGuestCanReachIP probes TCP egress with a guest-side timeout. A dropped SYN
// returns unreachable; a completed handshake returns connected. Other errors
// indicate a harness failure.
func inGuestCanReachIP(t *testing.T, m *runtime.MicroVMRuntime, id runtime.WorkloadID, ip string) bool {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), inGuestProbeTimeout)
	defer cancel()
	script := fmt.Sprintf("timeout %d bash -c 'exec 3<>/dev/tcp/%s/443 && echo connected'", guestConnectTimeoutSecs, ip)
	out, err := m.Exec(ctx, id, runtime.NewExecSpec("sh", "-c", script).AsUser(strconv.Itoa(int(agentuid.AgentUID))))
	if err != nil {
		t.Fatalf("in-guest IP connect probe to %s errored (harness fault, not a firewall verdict): %v", ip, err)
	}
	reached := out.ExitCode == 0 && strings.Contains(out.Stdout, "connected")
	t.Logf("in-guest connect %s:443 -> reached=%v (exit=%d)", ip, reached, out.ExitCode)
	return reached
}

// resolveContainerID reads the engine WorkloadID the registry bound for the
// provisioned name — the id in-guest execs address. White-box: the registry is
// the runner's own, and the id is otherwise unexported from Provision's return.
func resolveContainerID(t *testing.T, h *agentHost, name string) runtime.WorkloadID {
	t.Helper()
	handle, ok := h.registry.Resolve(name)
	if !ok {
		t.Fatalf("no registry handle for provisioned container %q", name)
	}
	return handle.ID()
}

// assertSocket0600 fails unless path is a socket with owner-only (0600) mode —
// the host-side gateway socket posture (gateway/socket.go socketFileMode).
func assertSocket0600(t *testing.T, path string) {
	t.Helper()
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatalf("stat suffixed socket %q: %v", path, err)
	}
	if info.Mode().Type() != os.ModeSocket {
		t.Fatalf("suffixed path %q is not a socket (type %s)", path, info.Mode().Type())
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("suffixed socket %q mode = %o, want 0600 (owner-only, Runner-owned)", path, perm)
	}
}
