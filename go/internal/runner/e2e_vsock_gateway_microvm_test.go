//go:build microvm && unix

package runner

// The V4 W3 KVM-gated end-to-end gateway-over-vsock acceptance suite (RIG-3092):
// the milestone gate for AgentGateway-over-host-side-vsock, driving a real
// agentHost over a real MicroVMRuntime engine on live hardware so the whole path
// W1 (guestd's in-guest unix->vsock forwarder) + W2 (host serves the same
// generated handler over the per-session suffixed AF_UNIX path, probe-gated in
// Provision) is proven where it actually runs. Every test opens with
// microvmtest.Require(t): on a KVM-less box it SKIPS (unless
// COMPASS_REQUIRE_MICROVM=1 forces a hard fail), so the suite is only real where
// /dev/kvm is openable and the guest images are exported into the env.
//
// It consumes W1+W2 through the public surfaces (agentHost.Provision/Start/
// Remove/Close over a MicroVMRuntime engine, the white-box h.sockets read via
// listenerPath) and complements — never duplicates — the hermetic W2 suite
// (host_vsock_gateway_test.go, a fake probe engine, no VM) and the hermetic W1
// suite (guestd/gateway_proxy_test.go, an injected dialGateway, no vsock): those
// prove the wiring in isolation; this proves the two halves meet over a real
// cloud-hypervisor hybrid-vsock channel.
//
// TWO TRANSPORT LEGS, both proven here:
//   - HOST leg: the host serves gateway.Serve over <runtimeDir>/vsock.sock_1025
//     (the CH suffixed path, record §(a)/(b)); a host-side Connect client dials
//     that plain AF_UNIX path directly (runnertest.DialAgentSocket), so the
//     serve path, identity binding, fail-closed window, and teardown are proven
//     without needing a guest at all (TestVsockGateway_HostServesSuffixedSocket,
//     _FailClosedBeforeStart, _TeardownRemovesSuffixedSocket).
//   - GUEST leg: an agent-uid exec inside the booted guest runs a bun probe that
//     dials /run/compass/agent.sock (W1's forwarder), which bridges to the host
//     Gateway over AF_VSOCK, driving a real Comms round-trip end to end
//     (TestVsockGateway_InGuestRoundTripOverVsock). The vsock-is-not-IP non-goal
//     (record §(g)) rides the same booted session.
//
// DETERMINISM. The suite never launches the real compass-agent: agentCommand is
// overridden to an inert in-guest keep-alive for the run, so the ONLY traffic
// the fake Server relay sees is what a test's own probe sends — a call count is
// then an exact assertion, not a race against a live agent's startup chatter.
// The KVM ctx-lifetime footgun is honored throughout: Start/Provision take
// t.Context() (the VM lifetime), NEVER a WithTimeout+defer-cancel ctx, which
// would kill the guest mid-session; per-exec/per-call deadlines are separate
// short-lived contexts that never bound a boot.

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"

	compassv1 "github.com/RigelBuild/compass/go/gen/compass/v1"
	compassv1internal "github.com/RigelBuild/compass/go/internal/gen/compass/v1"
	"github.com/RigelBuild/compass/go/internal/microvmtest"
	"github.com/RigelBuild/compass/go/internal/runnertest"
	"github.com/RigelBuild/compass/go/internal/runtime"
)

const (
	// w3AgentHandle is the 32-hex agent handle every W3 Provision uses (the
	// BuildSpec input shape the real spec builder validates); its exact value is
	// irrelevant, only that it is the minted fixed-width lowercase-hex shape.
	w3AgentHandle = "0123456789abcdef0123456789abcdef"
	// w3ProbeCallID is the Comms CallId the round-trip probes send; asserting it
	// flows to the relay verbatim and back proves the call, not merely a connect.
	w3ProbeCallID = "w3-vsock-1"
	// inGuestProbeTimeout bounds one in-guest bun-probe exec (the host-side ctx).
	// A real boot is ~2s and the probe is a single unary call, so this is
	// generous headroom, not a tuned value; it must stay well under the KVM
	// -timeout so a wedge fails as itself, not as a suite timeout.
	inGuestProbeTimeout = 60 * time.Second
)

// w3Relay is the fake Server for the W3 suite: recordingRelay (capturing every
// RelayCommsCall so a round-trip is an exact assertion) with FetchSecrets
// overridden to the "no secrets surface" posture — CodeFailedPrecondition. That
// is the legitimate secrets-less deployment Start tolerates by SKIPPING the
// secret materialize (host.go:384-388), which is load-bearing here: the guest
// rootfs has NO writable filesystem for the agent uid (erofs root read-only, no
// /home or /tmp, and the /workspace virtiofs mount rejects mkdir with EINVAL),
// so the materialize's `mkdir $HOME/.compass` cannot succeed and is not part of
// the gateway-transport contract W3 proves. Skipping it keeps Start on the
// bind-session-then-launch path the round-trip needs, without a real secrets
// surface a hermetic fake cannot host.
type w3Relay struct {
	*recordingRelay
}

func newW3Relay() *w3Relay { return &w3Relay{recordingRelay: &recordingRelay{}} }

// FetchSecrets returns CodeFailedPrecondition — the "no secrets surface" signal
// (host.go:367-370) — so Start skips the in-guest secret materialize the
// read-only guest rootfs cannot host.
func (r *w3Relay) FetchSecrets(
	context.Context, *connect.Request[compassv1internal.FetchSecretsRequest],
) (*connect.Response[compassv1internal.FetchSecretsResponse], error) {
	return nil, connect.NewError(connect.CodeFailedPrecondition, errors.New("w3: no secrets surface"))
}

// newMicroVMGatewayFixture builds a concrete *agentHost whose engine is a REAL
// MicroVMRuntime booting real guests, wired to relay as the fake Server. It
// mirrors newVsockGatewayFixture (the hermetic W2 fixture) with the fake engine
// swapped for NewMicroVMRuntime(e2eConfig(...)) — so agentHost drives its
// microVM (vsock-gateway) Provision leg against real hardware. Returns the host
// and the runtime; the caller drives Provision/Start and (for the guest leg)
// execs through the runtime by the container's resolved id.
func newMicroVMGatewayFixture(t *testing.T, relay *w3Relay) (*agentHost, *runtime.MicroVMRuntime) {
	t.Helper()
	env := microvmtest.Require(t)
	engine := runtime.NewMicroVMRuntime(w3MicroVMConfig(t, env))
	registry := runtime.NewAgentRegistry()
	rt := runtime.NewAgentRuntimeWithRegistry(engine, registry)
	link := newLink(newRunnerServiceServer(t, relay))
	specs := &fakeSpecBuilder{spec: w3LiveSpec(t)}
	var n int
	newID := func() string { n++; return "w3-sess-" + string(rune('0'+n)) }
	host := NewSessionHost(link, rt, registry, engine, specs, AgentHostConfig{RuntimeDir: t.TempDir()}, discardLoggerRunner(), newID)
	return host.(*agentHost), engine
}

// w3MicroVMConfig builds a MicroVMConfig from the resolved test env and a fresh,
// SHORT runroot. The short root is load-bearing, not cosmetic: the widest
// per-session socket leaf is <RunRoot>/microvm/<32-hex id>/vsock.sock_1025 (a
// 57-byte tail), and a t.TempDir() root — which embeds the long test-function
// name — overflows the 107-byte AF_UNIX sun_path cap so the bind fails EINVAL
// and the boot times out. This mirrors runtime.e2eConfig (the runtime package's
// own KVM harness helper), duplicated here because that helper is an unexported
// test symbol in a different package.
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

// w3LiveSpec is a launchable AgentSpec for a real microVM session: a single
// /workspace mount (the only mount the microVM backend accepts) backed by a
// throwaway dir, agent uid 1000, and the zero-value-safe default-deny egress.
// The image name is ignored by the microVM backend (the rootfs is the image),
// and Command is ignored too (guestd PID-1 is the keep-alive), so this is the
// minimal spec that boots.
func w3LiveSpec(t *testing.T) runtime.AgentSpec {
	t.Helper()
	return runtime.AgentSpec{
		Name:  "w3-vsock-agent",
		Image: "compass-agent:latest",
		Workspace: runtime.Workspace{
			CheckoutDir: "/workspace",
			HomeDir:     "/workspace",
			UID:         1000,
		},
		Mounts: []runtime.Mount{{HostPath: t.TempDir(), ContainerPath: "/workspace"}},
		Egress: runtime.EgressPolicy{},
	}
}

// inertAgentCommand replaces the real compass-agent argv for the duration of a
// test with an in-guest keep-alive: a sleep that outlives the test. StartAgent
// execs this instead of the agent, so no real agent traffic reaches the fake
// relay and a call count is an exact assertion. Restores the original on
// cleanup. `sleep` is a coreutils binary present in the guest rootfs (proven by
// the toolchain-closure probe).
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

	name, err := h.Provision(t.Context(), &compassv1.ProvisionAgentWorkspaceRequest{AgentHandle: w3AgentHandle})
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
	sessionID, err := h.Start(t.Context(), &compassv1.StartAgentSessionRequest{ContainerName: name}, "")
	if err != nil {
		t.Fatalf("Start (bind session + launch inert in-guest keep-alive) = %v", err)
	}

	client := runnertest.DialAgentSocket(t, gotPath)
	callCtx, cancel := context.WithTimeout(t.Context(), testTimeout)
	defer cancel()
	resp, err := client.Comms(callCtx, connect.NewRequest(&compassv1internal.CommsCallRequest{
		CallId: w3ProbeCallID,
		Call: &compassv1internal.CommsCallRequest_Post{
			Post: &compassv1.PostMessageRequest{Container: &compassv1.PostMessageRequest_ChannelId{ChannelId: "chan-1"}},
		},
	}))
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
	if resp.Msg.GetCallId() != w3ProbeCallID {
		t.Fatalf("result call id = %q, want %q (the Server result flowed back)", resp.Msg.GetCallId(), w3ProbeCallID)
	}
}

// TestVsockGateway_FailClosedBeforeStart is check (3), host side: the suffixed
// socket is live from Provision (served after Launch, before Start binds a
// session), so a Comms call in that window fails closed CodePermissionDenied and
// never reaches the Server — the pre-Start posture proven over the real vsock
// serve path, not a fake. Mutation that reddens it: dropping the no-session
// fail-closed check so the call forwards with an empty session id (the client
// would see a non-PermissionDenied outcome and the relay would be non-empty).
func TestVsockGateway_FailClosedBeforeStart(t *testing.T) {
	inertAgentCommand(t)
	fake := newW3Relay()
	h, _ := newMicroVMGatewayFixture(t, fake)

	name, err := h.Provision(t.Context(), &compassv1.ProvisionAgentWorkspaceRequest{AgentHandle: w3AgentHandle})
	if err != nil {
		t.Fatalf("Provision = %v", err)
	}
	t.Cleanup(func() { _ = h.Remove(context.WithoutCancel(t.Context()), name) })
	// Deliberately no Start: the gateway is served, but no session is bound.

	client := runnertest.DialAgentSocket(t, listenerPath(t, h, name))
	callCtx, cancel := context.WithTimeout(t.Context(), testTimeout)
	defer cancel()
	_, err = client.Comms(callCtx, connect.NewRequest(&compassv1internal.CommsCallRequest{CallId: "w3-early"}))
	if err == nil {
		t.Fatal("Comms before Start = nil, want CodePermissionDenied (no session bound over the vsock serve path)")
	}
	if code := connect.CodeOf(err); code != connect.CodePermissionDenied {
		t.Fatalf("Comms-before-Start code = %v, want CodePermissionDenied", code)
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

	name, err := h.Provision(t.Context(), &compassv1.ProvisionAgentWorkspaceRequest{AgentHandle: w3AgentHandle})
	if err != nil {
		t.Fatalf("Provision = %v", err)
	}
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

	name, err := h.Provision(t.Context(), &compassv1.ProvisionAgentWorkspaceRequest{AgentHandle: w3AgentHandle})
	if err != nil {
		t.Fatalf("Provision = %v", err)
	}
	t.Cleanup(func() { _ = h.Remove(context.WithoutCancel(t.Context()), name) })
	sessionID, err := h.Start(t.Context(), &compassv1.StartAgentSessionRequest{ContainerName: name}, "")
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

	name, err := h.Provision(t.Context(), &compassv1.ProvisionAgentWorkspaceRequest{AgentHandle: w3AgentHandle})
	if err != nil {
		t.Fatalf("Provision = %v", err)
	}
	t.Cleanup(func() { _ = h.Remove(context.WithoutCancel(t.Context()), name) })
	if _, err := h.Start(t.Context(), &compassv1.StartAgentSessionRequest{ContainerName: name}, ""); err != nil {
		t.Fatalf("Start = %v", err)
	}
	id := resolveContainerID(t, h, name)

	// The vsock gateway path works (positive control: without it, "both fail"
	// below would pass vacuously).
	out := runInGuestProbe(t, engine, id, w3ProbeCallID)
	if out.callID != w3ProbeCallID {
		t.Fatalf("in-guest vsock round-trip = %q, want %q; the deny leg is only meaningful if the vsock path works", out.callID, w3ProbeCallID)
	}

	// A raw-IP egress attempt from inside the guest is blocked by the always-armed
	// default-deny firewall — the gateway is not reachable by any IP route. Probe
	// a globally-routable raw IP (never a name — DNS stalls the harness resolver),
	// bounded by a guest-side timeout so a dropped SYN reports exit 124.
	if inGuestCanReachIP(t, engine, id, "8.8.8.8") {
		t.Fatal("an IP destination reached the guest egress path; the vsock gateway must be orthogonal to (never a hole in) the default-deny IP seal")
	}
}

// probeResult is the one fact the in-guest bun probe reports back: the CallId
// the host Gateway's relay echoed into the Comms result. An empty callID means
// the probe ran but the round-trip did not complete (the failure the caller
// asserts against).
type probeResult struct {
	callID string
}

// runInGuestProbe execs a bun script inside the guest (agent uid 1000) that
// dials /run/compass/agent.sock and issues one AgentGateway.Comms unary over
// HTTP/1.1 Connect (a JSON POST — the gateway serves SetHTTP1, and Connect-unary
// over HTTP/1.1 is a plain POST of the request message). The script prints a
// single line `RESULT <json>` the host parses for the echoed call id. A
// transport/exec error is a harness fault (t.Fatalf); a completed exec whose
// body carries the result is the round-trip proof.
func runInGuestProbe(t *testing.T, m *runtime.MicroVMRuntime, id runtime.ContainerID, callID string) probeResult {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), inGuestProbeTimeout)
	defer cancel()

	// The Connect-unary request body for AgentGateway.Comms is the JSON encoding
	// of CommsCallRequest. A minimal call_id-only body is enough: the fake relay
	// echoes call_id into the result, so a matching echo proves the full loop.
	reqBody, err := json.Marshal(map[string]any{"call_id": callID})
	if err != nil {
		t.Fatalf("marshaling probe request: %v", err)
	}
	script := inGuestProbeScript(string(reqBody))
	out, err := m.Exec(ctx, id, runtime.NewExecSpec("bun", "-e", script).AsUser("1000"))
	if err != nil {
		t.Fatalf("in-guest bun probe exec errored (harness fault, not a round-trip verdict): %v", err)
	}
	t.Logf("in-guest probe: exit=%d\nstdout:\n%s\nstderr:\n%s", out.ExitCode, out.Stdout, out.Stderr)
	if out.ExitCode != 0 {
		t.Fatalf("in-guest bun probe exited %d (want 0); stderr: %s", out.ExitCode, out.Stderr)
	}
	return parseProbeResult(t, out.Stdout)
}

// inGuestProbeScript is the bun program the probe execs. It POSTs the Connect
// unary to the agent socket over a Unix-domain fetch (bun's `unix:` option) and
// prints `RESULT <json>` with the response body, or `RESULT-ERR <msg>` on a
// failure — a single parseable line either way so the host never guesses.
func inGuestProbeScript(reqJSON string) string {
	// The URL host is a placeholder; `unix` routes every byte to the socket. The
	// Connect-unary content type for JSON is application/json; the RPC path is
	// the fully-qualified procedure. Bounded by an AbortSignal so a wedged dial
	// fails as a printed error, not a hang inside the guest.
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

// parseProbeResult extracts the echoed call id from the probe's `RESULT <json>`
// line. The Comms result JSON is a CommsCallResult encoded by connect-go's JSON
// codec (protobuf JSON, camelCase field names), so the echoed field is `callId`.
// A missing/!RESULT line means the probe did not complete the round-trip.
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

// inGuestCanReachIP execs an agent-uid bash /dev/tcp connect to ip:443 inside
// the guest, bounded by a guest-side `timeout`, and reports whether the
// handshake completed. Mirrors egress_inguest_microvm_test.go's canReachIPv4: a
// dropped SYN hangs until the guest timeout fires (exit 124, unreachable); an
// allowed host completes (exit 0, "connected"). Any exec/transport error is a
// harness fault.
func inGuestCanReachIP(t *testing.T, m *runtime.MicroVMRuntime, id runtime.ContainerID, ip string) bool {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), inGuestProbeTimeout)
	defer cancel()
	script := "timeout 10 bash -c 'exec 3<>/dev/tcp/" + ip + "/443 && echo connected'"
	out, err := m.Exec(ctx, id, runtime.NewExecSpec("sh", "-c", script).AsUser("1000"))
	if err != nil {
		t.Fatalf("in-guest IP connect probe to %s errored (harness fault, not a firewall verdict): %v", ip, err)
	}
	reached := out.ExitCode == 0 && strings.Contains(out.Stdout, "connected")
	t.Logf("in-guest connect %s:443 -> reached=%v (exit=%d)", ip, reached, out.ExitCode)
	return reached
}

// resolveContainerID reads the engine ContainerID the registry bound for the
// provisioned name — the id in-guest execs address. White-box: the registry is
// the runner's own, and the id is otherwise unexported from Provision's return.
func resolveContainerID(t *testing.T, h *agentHost, name string) runtime.ContainerID {
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
	_ = filepath.Dir(path) // dir mode is asserted by the gateway's own listen path; leaf mode is the W3 anchor
}
