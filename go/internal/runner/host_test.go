//go:build unix

package runner

// The production SessionHost (agentHost): OQ6 row 5 (Runner-authoritative
// Status, answered from the host's own live set), the Start-twice-same-container
// → errAlreadyRunning guard, Stop idempotency (an unknown session succeeds),
// Provision driving SpecBuilder→AgentRuntime.Launch, and Reload reusing the
// session id. Every test names the contract a plausible bug would break.
//
// Start/Reload spawn a relay whose AgentStream.Stop terminates a real child, so
// these use the stub-streaming runtime (a real terminatable Process) plus a live
// PublishEvents wire for the relay to publish onto.

import (
	"context"
	"errors"
	"slices"
	"testing"

	compassv1 "github.com/sealedsecurity/compass/go/gen/compass/v1"
	"github.com/sealedsecurity/compass/go/internal/runtime"
)

// fakeSpecBuilder is a hand-written SpecBuilder: it records the request it was
// asked to build and returns a scripted spec (or error), so Provision's wiring
// to Launch is asserted without deriving a real image/egress spec.
type fakeSpecBuilder struct {
	spec runtime.AgentSpec
	err  error
	last *compassv1.ProvisionAgentWorkspaceRequest
}

func (b *fakeSpecBuilder) BuildSpec(req *compassv1.ProvisionAgentWorkspaceRequest) (runtime.AgentSpec, error) {
	b.last = req
	return b.spec, b.err
}

// newHostFixture builds an agentHost over the stub-streaming runtime (real,
// terminatable Process) with its registry + runtime wired, a live PublishEvents
// wire for the relay, and a deterministic id minter. Returns the host, the
// engine, the registry, and the spec builder.
func newHostFixture(t *testing.T, specs SpecBuilder) (SessionHost, *stubStreamingRuntime, *runtime.AgentRegistry) {
	t.Helper()
	engine := newStubStreamingRuntime(t)
	registry := runtime.NewAgentRegistry()
	rt := runtime.NewAgentRuntimeWithRegistry(engine, registry)
	// The relay publishes onto a real wire; a capture server drains it so the
	// relay goroutine never blocks and the stream terminates cleanly.
	link := newLink(newRunnerServiceServer(t, newCapturePublish()))
	var n int
	newID := func() string { n++; return "sess-" + string(rune('0'+n)) }
	host := NewSessionHost(link, rt, registry, engine, specs, t.TempDir(), discardLoggerRunner(), newID)
	return host, engine, registry
}

// Provision derives the AgentSpec from the request via the SpecBuilder and
// launches the container through AgentRuntime, returning the launched name. A
// bug that bypassed the SpecBuilder or never called Launch would redden the
// recorded request or the returned name.
func TestProvisionDrivesSpecBuilderThenLaunch(t *testing.T) {
	specs := &fakeSpecBuilder{spec: runtime.AgentSpec{
		Name:  "atlas-agent-1",
		Image: "compass-agent:latest",
		Workspace: runtime.Workspace{
			Source:      runtime.LocalPathSource("/src/demo.git"),
			CheckoutDir: "/work/repo",
			HomeDir:     "/home/agent",
			UID:         1000,
		},
		Egress: runtime.MustAllowEgress("github.com"),
	}}
	host, engine, registry := newHostFixture(t, specs)

	name, err := host.Provision(context.Background(), &compassv1.ProvisionAgentWorkspaceRequest{AgentAccountId: "acct-1"})
	if err != nil {
		t.Fatalf("Provision = %v, want success", err)
	}
	if name != "atlas-agent-1" {
		t.Fatalf("Provision returned name %q, want the spec's container name", name)
	}
	if specs.last == nil || specs.last.GetAgentAccountId() != "acct-1" {
		t.Fatalf("SpecBuilder.BuildSpec was not called with the request; got %+v", specs.last)
	}
	// Launch ran (create+start on the engine) and registered the handle so a
	// later Start resolves it by name.
	if _, ok := registry.Resolve("atlas-agent-1"); !ok {
		t.Fatal("launched container not registered; a later Start could not resolve it")
	}
	assertRecorded(t, engine.calls, "create")
	assertRecorded(t, engine.calls, "start")
}

// A SpecBuilder error aborts Provision before Launch — a misconfigured request
// must not create a container.
func TestProvisionSpecBuilderErrorAborts(t *testing.T) {
	specs := &fakeSpecBuilder{err: errors.New("no repo in request")}
	host, engine, _ := newHostFixture(t, specs)

	_, err := host.Provision(context.Background(), &compassv1.ProvisionAgentWorkspaceRequest{})
	if err == nil {
		t.Fatal("Provision with a failing spec builder = nil, want the build error")
	}
	if len(engine.calls) != 0 {
		t.Fatalf("engine touched (%v) despite a spec-build failure; Launch must not run", engine.calls)
	}
}

// OQ6 row 3 (Runner side): starting a second session on a container already
// hosting a live one returns errAlreadyRunning — a genuine double start. The
// first Start succeeds; the second, same container, fails.
func TestStartTwiceSameContainerIsAlreadyRunning(t *testing.T) {
	specs := &fakeSpecBuilder{spec: liveSpec()}
	host, _, _ := newHostFixture(t, specs)
	ctx := context.Background()

	if _, err := host.Provision(ctx, &compassv1.ProvisionAgentWorkspaceRequest{AgentAccountId: "a"}); err != nil {
		t.Fatalf("Provision = %v", err)
	}
	first, err := host.Start(ctx, &compassv1.StartAgentSessionRequest{ContainerName: "cont-1"})
	if err != nil {
		t.Fatalf("first Start = %v, want success", err)
	}
	if first == "" {
		t.Fatal("first Start returned an empty session id")
	}

	_, err = host.Start(ctx, &compassv1.StartAgentSessionRequest{ContainerName: "cont-1"})
	if !errors.Is(err, errAlreadyRunning) {
		t.Fatalf("second Start on the same container = %v, want errAlreadyRunning", err)
	}
	// Cleanly tear the live session down (terminates the stub child).
	if err := host.Stop(ctx, first); err != nil {
		t.Fatalf("Stop(first) = %v", err)
	}
}

// Starting an unregistered container is errSessionUnknown — Start resolves the
// container by name through the registry, so an unlaunched name is not found.
func TestStartUnknownContainerIsSessionUnknown(t *testing.T) {
	specs := &fakeSpecBuilder{spec: liveSpec()}
	host, _, _ := newHostFixture(t, specs)
	_, err := host.Start(context.Background(), &compassv1.StartAgentSessionRequest{ContainerName: "never-launched"})
	if !errors.Is(err, errSessionUnknown) {
		t.Fatalf("Start of an unlaunched container = %v, want errSessionUnknown", err)
	}
}

// Stop is idempotent: stopping an unknown/already-stopped session succeeds
// (matching the frozen StopAgentSession semantics). A bug that errored on an
// unknown session would make a retry after a successful stop fail.
func TestStopUnknownSessionSucceeds(t *testing.T) {
	specs := &fakeSpecBuilder{spec: liveSpec()}
	host, _, _ := newHostFixture(t, specs)
	if err := host.Stop(context.Background(), "no-such-session"); err != nil {
		t.Fatalf("Stop(unknown) = %v, want nil (idempotent)", err)
	}
}

// OQ6 row 5: Status is answered from the Runner's own live session set — a
// started session appears with its live state, and a stopped one is gone. The
// Runner is authoritative for live truth. A bug that answered from a stale or
// external source would not reflect the live set.
func TestStatusIsAnsweredFromLiveSet(t *testing.T) {
	specs := &fakeSpecBuilder{spec: liveSpec()}
	host, _, _ := newHostFixture(t, specs)
	ctx := context.Background()

	if _, err := host.Provision(ctx, &compassv1.ProvisionAgentWorkspaceRequest{AgentAccountId: "a"}); err != nil {
		t.Fatalf("Provision = %v", err)
	}
	sessionID, err := host.Start(ctx, &compassv1.StartAgentSessionRequest{ContainerName: "cont-1"})
	if err != nil {
		t.Fatalf("Start = %v", err)
	}

	// A targeted Status returns exactly the live session, in READY state.
	one, err := host.Status(ctx, sessionID)
	if err != nil {
		t.Fatalf("Status(one) = %v", err)
	}
	if len(one) != 1 || one[0].GetSessionId() != sessionID {
		t.Fatalf("Status(one) = %+v, want the one live session %q", one, sessionID)
	}
	if one[0].GetState() != compassv1.AgentSessionState_AGENT_SESSION_STATE_READY {
		t.Fatalf("live session state = %v, want READY", one[0].GetState())
	}

	// An empty id returns every live session.
	all, err := host.Status(ctx, "")
	if err != nil {
		t.Fatalf("Status(all) = %v", err)
	}
	if len(all) != 1 {
		t.Fatalf("Status(all) returned %d sessions, want 1 (the live set)", len(all))
	}

	// After Stop the session leaves the live set: a targeted Status is NotFound,
	// and the all-set is empty — the Runner's answer tracks its live truth.
	if err := host.Stop(ctx, sessionID); err != nil {
		t.Fatalf("Stop = %v", err)
	}
	if _, err := host.Status(ctx, sessionID); !errors.Is(err, errSessionUnknown) {
		t.Fatalf("Status(stopped session) = %v, want errSessionUnknown (gone from the live set)", err)
	}
	all, err = host.Status(ctx, "")
	if err != nil {
		t.Fatalf("Status(all after stop) = %v", err)
	}
	if len(all) != 0 {
		t.Fatalf("Status(all) after stop returned %d, want 0 (live set empty)", len(all))
	}
}

// Reload restarts a session's agent in place, reusing the SAME session id (the
// board entry stays continuous). A bug that minted a new id would break board
// continuity.
func TestReloadReusesSessionId(t *testing.T) {
	specs := &fakeSpecBuilder{spec: liveSpec()}
	host, _, _ := newHostFixture(t, specs)
	ctx := context.Background()

	if _, err := host.Provision(ctx, &compassv1.ProvisionAgentWorkspaceRequest{AgentAccountId: "a"}); err != nil {
		t.Fatalf("Provision = %v", err)
	}
	sessionID, err := host.Start(ctx, &compassv1.StartAgentSessionRequest{ContainerName: "cont-1"})
	if err != nil {
		t.Fatalf("Start = %v", err)
	}

	if err := host.Reload(ctx, sessionID); err != nil {
		t.Fatalf("Reload = %v", err)
	}

	// The session still exists under the SAME id after reload.
	statuses, err := host.Status(ctx, sessionID)
	if err != nil {
		t.Fatalf("Status after reload = %v (the session id must be reused)", err)
	}
	if len(statuses) != 1 || statuses[0].GetSessionId() != sessionID {
		t.Fatalf("after reload Status = %+v, want the same session id %q", statuses, sessionID)
	}
	// Reload re-started the agent: the session is live and READY, not left
	// aborted mid-reload.
	if statuses[0].GetState() != compassv1.AgentSessionState_AGENT_SESSION_STATE_READY {
		t.Fatalf("after reload state = %v, want READY (reload must re-start the agent, leaving a live session)", statuses[0].GetState())
	}
	if err := host.Stop(ctx, sessionID); err != nil {
		t.Fatalf("Stop = %v", err)
	}
}

// Reloading an unknown session is errSessionUnknown — there is nothing live to
// restart.
func TestReloadUnknownSessionIsSessionUnknown(t *testing.T) {
	specs := &fakeSpecBuilder{spec: liveSpec()}
	host, _, _ := newHostFixture(t, specs)
	if err := host.Reload(context.Background(), "ghost"); !errors.Is(err, errSessionUnknown) {
		t.Fatalf("Reload(unknown) = %v, want errSessionUnknown", err)
	}
}

// liveSpec is a minimal launchable AgentSpec for the test container "cont-1".
func liveSpec() runtime.AgentSpec {
	return runtime.AgentSpec{
		Name:  "cont-1",
		Image: "compass-agent:latest",
		Workspace: runtime.Workspace{
			Source:      runtime.LocalPathSource("/src/demo.git"),
			CheckoutDir: "/work/repo",
			HomeDir:     "/home/agent",
			UID:         1000,
		},
		Egress: runtime.MustAllowEgress("github.com"),
	}
}

// assertRecorded fails unless calls contains want.
func assertRecorded(t *testing.T, calls []string, want string) {
	t.Helper()
	if slices.Contains(calls, want) {
		return
	}
	t.Fatalf("engine calls %v missing %q", calls, want)
}
