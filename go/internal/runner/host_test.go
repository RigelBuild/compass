//go:build unix

package runner

// The production SessionHost (agentHost): OQ6 row 5 (Runner-authoritative
// Status, answered from the host's own live set), the Start-twice-same-container
// → errAlreadyRunning guard, Stop idempotency (an unknown session succeeds),
// Provision driving SpecBuilder→AgentRuntime.Launch, Reload reusing the session
// id, and the AgentEnv the host derives from a launched container's handle (the
// exec's uid, $HOME, checkout and model) staying identical across Start and
// Reload. Every test names the contract a plausible bug would break.
//
// Start/Reload spawn an agent whose AgentStream.Stop terminates a real child, so
// these use the stub-streaming runtime (a real terminatable Process) plus a
// RunnerService wire for the link's client.

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"

	compassv1 "github.com/RigelBuild/compass/go/gen/compass/v1"
	compassv1internal "github.com/RigelBuild/compass/go/internal/gen/compass/v1"
	"github.com/RigelBuild/compass/go/internal/runtime"
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
	if b.err != nil {
		return runtime.AgentSpec{}, b.err
	}
	// Echo the request's account onto the returned spec, as the real BuildSpec
	// does (spec.go:98) — so the account threads Provision→spec→handle→session
	// and the Status stamp (host.go:384/533/537) is exercised end-to-end.
	spec := b.spec
	spec.AgentAccountID = req.GetAgentAccountId()
	return spec, nil
}

// newHostFixture builds an agentHost over the stub-streaming runtime (real,
// terminatable Process) with its registry + runtime wired, a live PublishEvents
// wire for the link's client, and a deterministic id minter. Returns the host, the
// engine, the registry, and the spec builder.
func newHostFixture(t *testing.T, specs SpecBuilder) (SessionHost, *stubStreamingRuntime, *runtime.AgentRegistry) {
	t.Helper()
	return newHostFixtureWithModel(t, specs, "")
}

// newHostFixtureWithModel is newHostFixture with the host's Runner-wide model
// selector set, for the tests that assert what configuration a started agent
// actually receives.
func newHostFixtureWithModel(t *testing.T, specs SpecBuilder, model string) (SessionHost, *stubStreamingRuntime, *runtime.AgentRegistry) {
	t.Helper()
	engine := newStubStreamingRuntime(t)
	registry := runtime.NewAgentRegistry()
	rt := runtime.NewAgentRuntimeWithRegistry(engine, registry)
	// newLink needs a RunnerService client; the capture server terminates a real
	// wire so nothing in the host path blocks on a missing handler.
	link := newLink(newRunnerServiceServer(t, newCapturePublish()))
	var n int
	newID := func() string { n++; return "sess-" + string(rune('0'+n)) }
	cfg := AgentHostConfig{RuntimeDir: t.TempDir(), AgentModel: model}
	host := NewSessionHost(link, rt, registry, engine, specs, cfg, discardLoggerRunner(), newID)
	return host, engine, registry
}

// newHostFixtureWithPublish is newHostFixture that also returns the
// capturePublish backing the link's client, so a test can seed the resolved set
// FetchSecrets returns and assert which selector the Runner used.
func newHostFixtureWithPublish(t *testing.T, specs SpecBuilder) (SessionHost, *stubStreamingRuntime, *capturePublish) {
	t.Helper()
	engine := newStubStreamingRuntime(t)
	registry := runtime.NewAgentRegistry()
	rt := runtime.NewAgentRuntimeWithRegistry(engine, registry)
	pub := newCapturePublish()
	link := newLink(newRunnerServiceServer(t, pub))
	var n int
	newID := func() string { n++; return "sess-" + string(rune('0'+n)) }
	cfg := AgentHostConfig{RuntimeDir: t.TempDir()}
	host := NewSessionHost(link, rt, registry, engine, specs, cfg, discardLoggerRunner(), newID)
	return host, engine, pub
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
			CheckoutDir: "/work/repo",
			HomeDir:     "/home/agent",
			UID:         1000,
		},
		Egress: runtime.MustAllowEgress("github.com"),
	}}
	host, engine, registry := newHostFixture(t, specs)

	name, err := host.Provision(context.Background(), &compassv1.ProvisionAgentWorkspaceRequest{AgentAccountId: "0123456789abcdef0123456789abcdef"})
	if err != nil {
		t.Fatalf("Provision = %v, want success", err)
	}
	if name != "atlas-agent-1" {
		t.Fatalf("Provision returned name %q, want the spec's container name", name)
	}
	if specs.last == nil || specs.last.GetAgentAccountId() != "0123456789abcdef0123456789abcdef" {
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

// Provision materializes the fleet config and bind-mounts it read-only into the
// launched container BEFORE launch, at agentConfigMountPath, with the config
// root as its host path. On an unconfigured fleet (the fixture's empty bundle)
// Materialize still creates the root and returns it, so the mount is always
// present. A bug that skipped the config mount, mounted a version dir instead of
// the parent root, or mounted it writable would redden here.
func TestProvisionMaterializesConfigAndMountsBeforeLaunch(t *testing.T) {
	specs := &fakeSpecBuilder{spec: liveSpec()}
	host, engine, _ := newHostFixture(t, specs)

	name, err := host.Provision(context.Background(), &compassv1.ProvisionAgentWorkspaceRequest{})
	if err != nil {
		t.Fatalf("Provision = %v, want success", err)
	}

	created := engine.createdSpecs()
	if len(created) != 1 {
		t.Fatalf("engine created %d containers, want 1", len(created))
	}
	agentHost, ok := host.(*agentHost)
	if !ok {
		t.Fatalf("host is %T, want *agentHost", host)
	}
	wantRoot := filepath.Join(agentHost.runtimeDir, "containers", liveSpec().Name, "config")
	var found *runtime.Mount
	for i := range created[0].Mounts {
		if created[0].Mounts[i].ContainerPath == agentConfigMountPath {
			found = &created[0].Mounts[i]
			break
		}
	}
	if found == nil {
		t.Fatalf("launched spec for %q has no config mount at %q; mounts=%+v", name, agentConfigMountPath, created[0].Mounts)
	}
	if found.HostPath != wantRoot {
		t.Fatalf("config mount host path = %q, want the config root %q", found.HostPath, wantRoot)
	}
	if !found.ReadOnly {
		t.Fatal("config mount is writable, want read-only")
	}
	if info, statErr := os.Stat(wantRoot); statErr != nil || !info.IsDir() {
		t.Fatalf("config root %q not created on disk: err=%v", wantRoot, statErr)
	}
}

// A GENUINE config-fetch fault (CodeUnavailable — a transient transport failure)
// aborts Provision before Launch: a container must never come up when config
// delivery is actually broken. The socket served for the container is torn down
// (mirroring the Launch-failure cleanup) so it does not leak. This is the
// counterpart to TestProvisionToleratesNoConfigSurface: that a no-config-SURFACE
// server (CodeFailedPrecondition) provisions anyway, but a genuine fault does
// not. A bug that launched anyway, or left the socket behind, would redden here.
func TestProvisionConfigMaterializeErrorAborts(t *testing.T) {
	engine := newStubStreamingRuntime(t)
	registry := runtime.NewAgentRegistry()
	rt := runtime.NewAgentRuntimeWithRegistry(engine, registry)
	pub := newCapturePublish()
	pub.setConfigErr(connect.NewError(connect.CodeUnavailable, errors.New("config fetch down")))
	link := newLink(newRunnerServiceServer(t, pub))
	cfg := AgentHostConfig{RuntimeDir: t.TempDir()}
	host := NewSessionHost(link, rt, registry, engine, &fakeSpecBuilder{spec: liveSpec()}, cfg, discardLoggerRunner(), nil)

	_, err := host.Provision(context.Background(), &compassv1.ProvisionAgentWorkspaceRequest{})
	if err == nil {
		t.Fatal("Provision with a failing config materialize = nil, want the materialize error")
	}
	if slices.Contains(engine.callsSnapshot(), "create") || slices.Contains(engine.callsSnapshot(), "start") {
		t.Fatalf("engine touched (%v) despite a materialize failure; Launch must not run", engine.callsSnapshot())
	}
	if socketServed(t, host, liveSpec().Name) {
		t.Fatal("agent socket left served after an aborted provision; it must be torn down")
	}
}

// TestProvisionToleratesNoConfigSurface pins the no-config-SURFACE contract: a
// Server with no config store wired returns CodeFailedPrecondition from
// FetchAgentConfig, and the Runner reads that as "no config to inject" and
// provisions anyway — the mirror of TestStartToleratesNoSecretsSurface, and the
// posture today's production Server ships (its handler is built with a nil
// config store). The container still comes up and still gets the config mount,
// pointing at the ensured (empty) config root. A bug that aborted on
// CodeFailedPrecondition — conflating a no-surface server with a genuine fetch
// fault — would break provisioning fleet-wide and redden here.
func TestProvisionToleratesNoConfigSurface(t *testing.T) {
	engine := newStubStreamingRuntime(t)
	registry := runtime.NewAgentRegistry()
	rt := runtime.NewAgentRuntimeWithRegistry(engine, registry)
	pub := newCapturePublish()
	pub.setConfigErr(connect.NewError(connect.CodeFailedPrecondition, errors.New("no config store wired")))
	link := newLink(newRunnerServiceServer(t, pub))
	cfg := AgentHostConfig{RuntimeDir: t.TempDir()}
	host := NewSessionHost(link, rt, registry, engine, &fakeSpecBuilder{spec: liveSpec()}, cfg, discardLoggerRunner(), nil)

	name, err := host.Provision(context.Background(), &compassv1.ProvisionAgentWorkspaceRequest{})
	if err != nil {
		t.Fatalf("Provision with no config surface = %v, want success (provision anyway)", err)
	}
	created := engine.createdSpecs()
	if len(created) != 1 {
		t.Fatalf("engine created %d containers, want 1 (provision must proceed)", len(created))
	}
	agentHost, ok := host.(*agentHost)
	if !ok {
		t.Fatalf("host is %T, want *agentHost", host)
	}
	wantRoot := filepath.Join(agentHost.runtimeDir, "containers", liveSpec().Name, "config")
	var found *runtime.Mount
	for i := range created[0].Mounts {
		if created[0].Mounts[i].ContainerPath == agentConfigMountPath {
			found = &created[0].Mounts[i]
			break
		}
	}
	if found == nil {
		t.Fatalf("launched spec for %q has no config mount at %q; mounts=%+v", name, agentConfigMountPath, created[0].Mounts)
	}
	if found.HostPath != wantRoot {
		t.Fatalf("config mount host path = %q, want the ensured config root %q", found.HostPath, wantRoot)
	}
	if !found.ReadOnly {
		t.Fatal("config mount is writable, want read-only")
	}
	if info, statErr := os.Stat(wantRoot); statErr != nil || !info.IsDir() {
		t.Fatalf("config root %q not created on disk: err=%v", wantRoot, statErr)
	}
}

// seqSpecBuilder yields a distinct-named AgentSpec per BuildSpec call, cycling
// through the scripted specs in order — so two provisions on one host launch two
// differently-named containers, exercising per-container config-root isolation.
type seqSpecBuilder struct {
	specs []runtime.AgentSpec
	n     int
}

func (b *seqSpecBuilder) BuildSpec(*compassv1.ProvisionAgentWorkspaceRequest) (runtime.AgentSpec, error) {
	spec := b.specs[b.n%len(b.specs)]
	b.n++
	return spec, nil
}

// Two containers provisioned on ONE host must each get their OWN config root at
// <RuntimeDir>/containers/<name>/config — never a shared tree. A shared root is
// the SEA-1659 bug: every bind mount is :Z-relabeled into the container's
// private SELinux MCS category, so a second container provisioning against a
// shared root re-steals it from the first. This test reddens against the
// pre-reshape shared-root code (both mounts resolve to the same
// <RuntimeDir>/config) and passes once each container roots its own subtree.
func TestProvisionPerContainerConfigRootsAreDistinct(t *testing.T) {
	specA := liveSpec()
	specA.Name = "cont-a"
	specB := liveSpec()
	specB.Name = "cont-b"
	specs := &seqSpecBuilder{specs: []runtime.AgentSpec{specA, specB}}
	host, engine, _ := newHostFixture(t, specs)

	if _, err := host.Provision(context.Background(), &compassv1.ProvisionAgentWorkspaceRequest{}); err != nil {
		t.Fatalf("Provision A = %v, want success", err)
	}
	if _, err := host.Provision(context.Background(), &compassv1.ProvisionAgentWorkspaceRequest{}); err != nil {
		t.Fatalf("Provision B = %v, want success", err)
	}

	agentHost, ok := host.(*agentHost)
	if !ok {
		t.Fatalf("host is %T, want *agentHost", host)
	}
	created := engine.createdSpecs()
	if len(created) != 2 {
		t.Fatalf("engine created %d containers, want 2", len(created))
	}

	configMount := func(spec runtime.ContainerSpec) *runtime.Mount {
		for i := range spec.Mounts {
			if spec.Mounts[i].ContainerPath == agentConfigMountPath {
				return &spec.Mounts[i]
			}
		}
		return nil
	}

	hostPaths := make([]string, 0, len(created))
	for i := range created {
		mount := configMount(created[i])
		if mount == nil {
			t.Fatalf("launched spec %q has no config mount at %q; mounts=%+v", created[i].Name, agentConfigMountPath, created[i].Mounts)
		}
		wantRoot := filepath.Join(agentHost.runtimeDir, "containers", created[i].Name, "config")
		if mount.HostPath != wantRoot {
			t.Fatalf("container %q config mount host path = %q, want per-container root %q", created[i].Name, mount.HostPath, wantRoot)
		}
		if !mount.ReadOnly {
			t.Fatalf("container %q config mount is writable, want read-only", created[i].Name)
		}
		if info, statErr := os.Stat(mount.HostPath); statErr != nil || !info.IsDir() {
			t.Fatalf("config root %q not created on disk: err=%v", mount.HostPath, statErr)
		}
		hostPaths = append(hostPaths, mount.HostPath)
	}
	if hostPaths[0] == hostPaths[1] {
		t.Fatalf("both containers share config root %q; per-container roots must be distinct", hostPaths[0])
	}
}

// OQ6 row 3 (Runner side): starting a second session on a container already
// hosting a live one returns errAlreadyRunning — a genuine double start. The
// first Start succeeds; the second, same container, fails.
func TestStartTwiceSameContainerIsAlreadyRunning(t *testing.T) {
	specs := &fakeSpecBuilder{spec: liveSpec()}
	host, _, _ := newHostFixture(t, specs)
	ctx := context.Background()

	if _, err := host.Provision(ctx, &compassv1.ProvisionAgentWorkspaceRequest{AgentAccountId: "0123456789abcdef0123456789abcdef"}); err != nil {
		t.Fatalf("Provision = %v", err)
	}
	first, err := host.Start(ctx, &compassv1.StartAgentSessionRequest{ContainerName: "cont-1"}, "")
	if err != nil {
		t.Fatalf("first Start = %v, want success", err)
	}
	if first == "" {
		t.Fatal("first Start returned an empty session id")
	}

	_, err = host.Start(ctx, &compassv1.StartAgentSessionRequest{ContainerName: "cont-1"}, "")
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
	_, err := host.Start(context.Background(), &compassv1.StartAgentSessionRequest{ContainerName: "never-launched"}, "")
	if !errors.Is(err, errSessionUnknown) {
		t.Fatalf("Start of an unlaunched container = %v, want errSessionUnknown", err)
	}
}

// Stop is idempotent: stopping an unknown/already-stopped session succeeds
// (matching the established StopAgentSession semantics). A bug that errored on an
// unknown session would make a retry after a successful stop fail.
func TestStopUnknownSessionSucceeds(t *testing.T) {
	specs := &fakeSpecBuilder{spec: liveSpec()}
	host, _, _ := newHostFixture(t, specs)
	if err := host.Stop(context.Background(), "no-such-session"); err != nil {
		t.Fatalf("Stop(unknown) = %v, want nil (idempotent)", err)
	}
}

// socketServed reports whether the host still holds an open agent socket for the
// container — white-box, under h.mu, mirroring e2e_transport_test.go's
// listenerPath. Remove must leave none behind.
func socketServed(t *testing.T, host SessionHost, container string) bool {
	t.Helper()
	h, ok := host.(*agentHost)
	if !ok {
		t.Fatalf("fixture host is %T, want *agentHost for white-box socket assertion", host)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	_, served := h.sockets[container]
	return served
}

// Remove tears a container down and everything bound to it: the live session is
// retired (gone from the Runner's live set), the container is torn down through
// the AgentRuntime (engine stop + remove; deregistered so it no longer
// resolves), and the agent socket is closed. A bug that skipped any of these
// would leak a session, a container, or a socket past the teardown.
func TestRemoveTearsDownContainerAndRetiresSession(t *testing.T) {
	specs := &fakeSpecBuilder{spec: liveSpec()}
	host, engine, registry := newHostFixture(t, specs)
	ctx := context.Background()

	name, err := host.Provision(ctx, &compassv1.ProvisionAgentWorkspaceRequest{AgentAccountId: "0123456789abcdef0123456789abcdef"})
	if err != nil {
		t.Fatalf("Provision = %v", err)
	}
	sessionID, err := host.Start(ctx, &compassv1.StartAgentSessionRequest{ContainerName: name}, "")
	if err != nil {
		t.Fatalf("Start = %v", err)
	}

	if err := host.Remove(ctx, name); err != nil {
		t.Fatalf("Remove = %v, want success", err)
	}

	// The session is gone from the Runner's live set.
	if _, err := host.Status(ctx, sessionID); !errors.Is(err, errSessionUnknown) {
		t.Fatalf("Status(session after Remove) = %v, want errSessionUnknown (session retired)", err)
	}
	// The container was torn down (engine stop + remove) and deregistered.
	assertRecorded(t, engine.calls, "stop")
	assertRecorded(t, engine.calls, "remove")
	if _, ok := registry.Resolve(name); ok {
		t.Fatal("container still registered after Remove; Teardown must deregister it")
	}
	// The agent socket is closed — no listener outlives the container.
	if socketServed(t, host, name) {
		t.Fatal("agent socket still served after Remove; the socket must be closed")
	}
}

// Remove is idempotent, like Stop: a second Remove of an already-removed
// container is a no-op success and does not tear a container down again, and a
// Remove of a never-provisioned container simply succeeds. A bug that errored on
// an unknown container would break a teardown retry after a successful remove.
func TestRemoveIsIdempotent(t *testing.T) {
	specs := &fakeSpecBuilder{spec: liveSpec()}
	host, engine, _ := newHostFixture(t, specs)
	ctx := context.Background()

	name, err := host.Provision(ctx, &compassv1.ProvisionAgentWorkspaceRequest{AgentAccountId: "0123456789abcdef0123456789abcdef"})
	if err != nil {
		t.Fatalf("Provision = %v", err)
	}
	if _, err := host.Start(ctx, &compassv1.StartAgentSessionRequest{ContainerName: name}, ""); err != nil {
		t.Fatalf("Start = %v", err)
	}
	if err := host.Remove(ctx, name); err != nil {
		t.Fatalf("first Remove = %v, want success", err)
	}
	callsAfterFirst := len(engine.calls)

	// A second Remove of the same container is a no-op success: nothing left to
	// resolve, so no further engine teardown runs.
	if err := host.Remove(ctx, name); err != nil {
		t.Fatalf("second Remove = %v, want nil (idempotent)", err)
	}
	if got := len(engine.calls); got != callsAfterFirst {
		t.Fatalf("second Remove drove %d more engine calls, want 0 (no double teardown): %v", got-callsAfterFirst, engine.calls)
	}

	// A Remove of a never-provisioned container succeeds too.
	if err := host.Remove(ctx, "never-provisioned"); err != nil {
		t.Fatalf("Remove(unknown) = %v, want nil (idempotent)", err)
	}
}

// A container whose registry handle is already gone (crash-reclaimed, or removed
// underneath) but whose agent socket still lingers still has its socket closed
// by Remove — so a Remove always leaves no socket behind, even when there is no
// container left to tear down. A bug that only closed the socket after a
// successful Teardown would leak the socket in this path.
func TestRemoveClosesSocketWhenHandleAlreadyGone(t *testing.T) {
	specs := &fakeSpecBuilder{spec: liveSpec()}
	host, engine, registry := newHostFixture(t, specs)
	ctx := context.Background()

	name, err := host.Provision(ctx, &compassv1.ProvisionAgentWorkspaceRequest{AgentAccountId: "0123456789abcdef0123456789abcdef"})
	if err != nil {
		t.Fatalf("Provision = %v", err)
	}
	if !socketServed(t, host, name) {
		t.Fatal("Provision did not serve an agent socket")
	}
	// Model the container handle gone while the socket lingers.
	registry.Deregister(name)
	callsBefore := len(engine.calls)

	if err := host.Remove(ctx, name); err != nil {
		t.Fatalf("Remove = %v, want success", err)
	}
	// No Teardown ran (no handle to resolve), so the engine is untouched.
	if got := len(engine.calls); got != callsBefore {
		t.Fatalf("Remove drove %d engine calls with no handle to resolve, want 0: %v", got-callsBefore, engine.calls)
	}
	// The lingering socket is closed regardless.
	if socketServed(t, host, name) {
		t.Fatal("agent socket still served after Remove; a lingering socket must be closed even when the handle is gone")
	}
}

// A Teardown that fails partway (the engine Stop errors, so the container is
// deregistered but not removed) still closes the agent socket and surfaces the
// error: Remove leaves no socket behind on ANY path, and never answers a lying
// success while the teardown failed. A bug that closed the socket only on the
// success path (e.g. after the Teardown call rather than in a defer) would leak
// the socket here and hand the Server a false success.
func TestRemoveClosesSocketWhenTeardownFails(t *testing.T) {
	specs := &fakeSpecBuilder{spec: liveSpec()}
	host, engine, _ := newHostFixture(t, specs)
	ctx := context.Background()

	name, err := host.Provision(ctx, &compassv1.ProvisionAgentWorkspaceRequest{AgentAccountId: "0123456789abcdef0123456789abcdef"})
	if err != nil {
		t.Fatalf("Provision = %v", err)
	}
	if _, err := host.Start(ctx, &compassv1.StartAgentSessionRequest{ContainerName: name}, ""); err != nil {
		t.Fatalf("Start = %v", err)
	}
	engine.stopErr = errors.New("engine stop failed")

	if err := host.Remove(ctx, name); err == nil {
		t.Fatal("Remove = nil, want the Teardown error surfaced (never a lying success on a failed teardown)")
	}
	// The socket is closed regardless of the teardown failure — no listener leaks.
	if socketServed(t, host, name) {
		t.Fatal("agent socket still served after a failed-teardown Remove; the socket must be closed on every path")
	}
}

// SEA-1635: a Teardown that fails partway (engine Stop errors) must leave the
// container's registry handle RESOLVABLE, so a Remove retry re-runs Teardown
// rather than answering a lying success over a leaked container. This pins the
// deregister-LAST ordering: AgentRuntime.Teardown deregisters only after Stop
// and Remove both succeed. Under the old deregister-first ordering the first
// failed Remove would already have dropped the handle, so the retry's
// registry.Resolve would miss, Teardown would be skipped, and Remove would
// return nil while the engine container leaked forever — the exact bug this
// guards. The retry (with Stop healed) both re-resolves and succeeds.
func TestFailedTeardownLeavesContainerResolvableForRetry(t *testing.T) {
	specs := &fakeSpecBuilder{spec: liveSpec()}
	host, engine, registry := newHostFixture(t, specs)
	ctx := context.Background()

	name, err := host.Provision(ctx, &compassv1.ProvisionAgentWorkspaceRequest{AgentAccountId: "0123456789abcdef0123456789abcdef"})
	if err != nil {
		t.Fatalf("Provision = %v", err)
	}
	if _, err := host.Start(ctx, &compassv1.StartAgentSessionRequest{ContainerName: name}, ""); err != nil {
		t.Fatalf("Start = %v", err)
	}

	// First Remove: engine Stop fails, so Teardown returns partway.
	engine.stopErr = errors.New("engine stop failed")
	if err := host.Remove(ctx, name); err == nil {
		t.Fatal("Remove = nil, want the Teardown error surfaced on a failed stop")
	}
	// The handle must still resolve — deregister-last means a failed Stop never
	// dropped it. Deregister-first would have orphaned the container here.
	if _, ok := registry.Resolve(name); !ok {
		t.Fatal("container handle not resolvable after a failed Teardown; a Remove retry would skip teardown and leak the container")
	}

	// Retry with Stop healed: Teardown re-runs (the handle still resolved),
	// tears the container down, and only now deregisters.
	engine.stopErr = nil
	if err := host.Remove(ctx, name); err != nil {
		t.Fatalf("Remove retry = %v, want success once the engine recovers", err)
	}
	if _, ok := registry.Resolve(name); ok {
		t.Fatal("container handle still resolvable after a successful Remove; teardown must deregister once Stop+Remove succeed")
	}
	// The successful retry actually drove the engine teardown (stop then remove),
	// proving the retry re-ran Teardown rather than short-circuiting.
	assertRecorded(t, engine.calls, "remove")
}

// Close stops AND removes every container the runner is hosting, not just its
// sockets. The defect this guards: the old Close closed only the per-container
// socket listeners, so after a shutdown-driven Close the podman containers kept
// running unsupervised (conmon double-forks them out of the runner's process
// group, so the stack's group-signal cannot reach them either). Close must drain
// them through the same teardown path Remove uses. Two containers are provisioned
// and started; Close must drive an engine stop+remove for BOTH and leave no
// socket served.
func TestCloseStopsEveryProvisionedContainer(t *testing.T) {
	host, engine, _ := newConfigRefreshFixture(t)
	ctx := context.Background()

	nameA := provisionAndStart(t, host, "a")
	nameB := provisionAndStart(t, host, "b")

	host.Close(ctx)

	// Both containers were stopped and removed — not merely un-socketed.
	assertRecorded(t, engine.calls, "stop")
	assertRecorded(t, engine.calls, "remove")
	if got := engine.countCall("stop"); got != 2 {
		t.Fatalf("engine stop calls = %d, want 2 (both provisioned containers stopped): %v", got, engine.calls)
	}
	if got := engine.countCall("remove"); got != 2 {
		t.Fatalf("engine remove calls = %d, want 2 (both provisioned containers removed): %v", got, engine.calls)
	}
	if socketServed(t, host, nameA) || socketServed(t, host, nameB) {
		t.Fatal("agent socket still served after Close; every socket must be closed")
	}
}

// Close keys off the provisioned-container set (the sockets index), not the live
// session set: a container provisioned but never Started still has a running
// podman container behind it, so Close must stop+remove it too. A bug that
// enumerated only started sessions would leak a provisioned-but-idle container.
func TestCloseTearsDownProvisionedButNotStartedContainer(t *testing.T) {
	host, engine, _ := newConfigRefreshFixture(t)
	ctx := context.Background()

	name, err := host.Provision(ctx, &compassv1.ProvisionAgentWorkspaceRequest{AgentAccountId: "a"})
	if err != nil {
		t.Fatalf("Provision = %v", err)
	}
	// No Start: the container is provisioned and running, but binds no session.

	host.Close(ctx)

	if got := engine.countCall("stop"); got != 1 {
		t.Fatalf("engine stop calls = %d, want 1 (the provisioned-but-unstarted container): %v", got, engine.calls)
	}
	if got := engine.countCall("remove"); got != 1 {
		t.Fatalf("engine remove calls = %d, want 1 (the provisioned-but-unstarted container): %v", got, engine.calls)
	}
	if socketServed(t, host, name) {
		t.Fatal("agent socket still served after Close; a provisioned container's socket must be closed")
	}
}

// Close is best-effort with per-container isolation: one container's Stop failing
// must not abort a sibling's teardown. The failing container aborts at stop (its
// AgentRuntime.Teardown returns at the stop stage, before remove and deregister),
// while the healthy sibling still runs stop→remove→deregister to completion — and
// every socket is closed regardless. This proves the concurrent fan-out isolates
// a failure to its own container rather than letting it short-circuit the drain.
func TestCloseIsBestEffortOnStopError(t *testing.T) {
	host, engine, _ := newConfigRefreshFixture(t)
	ctx := context.Background()

	nameA := provisionAndStart(t, host, "a")
	nameB := provisionAndStart(t, host, "b")
	// The engine keys container ids by name (Create returns ContainerID(spec.Name)),
	// so fail exactly container A's Stop and leave B healthy.
	idA := runtime.ContainerID(nameA)
	idB := runtime.ContainerID(nameB)
	engine.stopErrByID = map[runtime.ContainerID]error{idA: errors.New("engine stop failed")}

	host.Close(ctx)

	// The failing container aborted at stop: it recorded a stop but never a remove.
	if got := engine.countCallForID(idA, "stop"); got != 1 {
		t.Fatalf("failing container stop calls = %d, want 1 (attempted): %v", got, engine.callsByID[idA])
	}
	if got := engine.countCallForID(idA, "remove"); got != 0 {
		t.Fatalf("failing container remove calls = %d, want 0 (Teardown aborts at stop): %v", got, engine.callsByID[idA])
	}
	// The healthy sibling fully tore down: stop, then remove, then deregister.
	if got := engine.countCallForID(idB, "stop"); got != 1 {
		t.Fatalf("healthy container stop calls = %d, want 1: %v", got, engine.callsByID[idB])
	}
	if got := engine.countCallForID(idB, "remove"); got != 1 {
		t.Fatalf("healthy container remove calls = %d, want 1 (a failing sibling must not abort it): %v", got, engine.callsByID[idB])
	}
	if _, ok := host.registry.Resolve(nameB); ok {
		t.Fatal("healthy container handle still resolvable after Close; a full teardown must deregister it")
	}
	// Both sockets are closed regardless of the teardown failure.
	if socketServed(t, host, nameA) || socketServed(t, host, nameB) {
		t.Fatal("agent socket still served after a best-effort Close; every socket must be closed on every path")
	}
}

// Close joins its concurrent teardowns before returning — no goroutine outlives
// it. A test-controlled gate parks both teardowns mid-Stop: while it is held,
// Close must NOT have returned and no container may have reached remove (the
// teardowns are stalled at stop). Releasing the gate lets every teardown finish;
// only then may Close return, with all stops and removes recorded. Deleting
// wg.Wait() from Close breaks this deterministically — Close would return while
// the teardowns are still parked, tripping the "not returned yet"/"no removes"
// assertion. Run under -race, this also guards the concurrent map/slice access.
func TestCloseJoinsConcurrentTeardowns(t *testing.T) {
	host, engine, _ := newConfigRefreshFixture(t)
	ctx := context.Background()

	provisionAndStart(t, host, "a")
	provisionAndStart(t, host, "b")

	// Park every teardown mid-Stop until the test releases the gate, and have each
	// signal stopEntered when it reaches Stop (a real event the test gates on, not
	// a poll). stopEntered is buffered for both containers so a teardown can signal
	// and then park without waiting for the test to read. The gate is released on
	// every exit path (including a failing assertion) so the suite can't hang.
	gate := make(chan struct{})
	entered := make(chan runtime.ContainerID, 2)
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(gate) }) }
	t.Cleanup(release)
	engine.mu.Lock()
	engine.stopGate = gate
	engine.stopEntered = entered
	engine.mu.Unlock()

	done := make(chan struct{})
	go func() {
		host.Close(ctx)
		close(done)
	}()

	// Wait until both teardowns have entered Stop (each sends on stopEntered, now
	// parked on the gate) — a real event with a bounded failsafe, no poll, no sleeps.
	for range 2 {
		select {
		case <-entered:
		case <-time.After(30 * time.Second):
			t.Fatal("teardowns did not both reach Stop within 30s")
		case <-done:
			t.Fatal("Close returned before its teardowns reached Stop")
		}
	}

	// While the gate is held: Close has not returned and no remove has happened.
	// Without wg.Wait(), Close would have returned here → this fails.
	select {
	case <-done:
		t.Fatal("Close returned while teardowns were still parked mid-Stop; it must join before returning")
	default:
	}
	if got := engine.countCall("remove"); got != 0 {
		t.Fatalf("engine remove calls = %d while teardowns parked at stop, want 0: %v", got, engine.calls)
	}

	// Release the gate; every teardown now runs to completion and Close returns.
	release()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("Close did not return within 30s after the gate released; teardowns must join, not hang")
	}

	// Every teardown finished before Close returned: both stops and both removes
	// are recorded, with no goroutine still running behind Close.
	if got := engine.countCall("stop"); got != 2 {
		t.Fatalf("engine stop calls = %d after Close returned, want 2 (all teardowns joined): %v", got, engine.calls)
	}
	if got := engine.countCall("remove"); got != 2 {
		t.Fatalf("engine remove calls = %d after Close returned, want 2 (all teardowns joined): %v", got, engine.calls)
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

	if _, err := host.Provision(ctx, &compassv1.ProvisionAgentWorkspaceRequest{AgentAccountId: "0123456789abcdef0123456789abcdef"}); err != nil {
		t.Fatalf("Provision = %v", err)
	}
	sessionID, err := host.Start(ctx, &compassv1.StartAgentSessionRequest{ContainerName: "cont-1"}, "")
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
	// The account provisioned for this container is stamped onto its status —
	// the DL-167 attribution the reject-on-live scan matches on. This is the sole
	// PRODUCTION source of that field (dropping the stamp at host.go:533 would
	// silently disable reject-on-live).
	if got := one[0].GetAgentAccountId(); got != "0123456789abcdef0123456789abcdef" {
		t.Fatalf("Status(one) account = %q, want the provisioned account", got)
	}

	// An empty id returns every live session.
	all, err := host.Status(ctx, "")
	if err != nil {
		t.Fatalf("Status(all) = %v", err)
	}
	if len(all) != 1 {
		t.Fatalf("Status(all) returned %d sessions, want 1 (the live set)", len(all))
	}
	if got := all[0].GetAgentAccountId(); got != "0123456789abcdef0123456789abcdef" {
		t.Fatalf("Status(all) account = %q, want the provisioned account (the all-arm at host.go:537 must stamp it too)", got)
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

	if _, err := host.Provision(ctx, &compassv1.ProvisionAgentWorkspaceRequest{AgentAccountId: "0123456789abcdef0123456789abcdef"}); err != nil {
		t.Fatalf("Provision = %v", err)
	}
	sessionID, err := host.Start(ctx, &compassv1.StartAgentSessionRequest{ContainerName: "cont-1"}, "")
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

// Start runs the agent with the identity and configuration derived from the
// launched container's OWN handle — uid, $HOME and checkout from the container
// it execs into, model from Runner-wide host config. This is what keeps a
// session's exec consistent with the container hosting it: a bug that read the
// uid or the checkout from anywhere but the handle (a hardcoded default, another
// container's spec, the Runner's own environment) would put the agent in the
// wrong directory or run it under the wrong user. The spec values here are
// deliberately unlike the other fixtures' so a stale-source bug cannot pass by
// coincidence.
func TestStartExecsAgentWithTheContainersOwnIdentity(t *testing.T) {
	spec := liveSpec()
	spec.Workspace.UID = 4242
	spec.Workspace.HomeDir = "/home/scoped"
	spec.Workspace.CheckoutDir = "/srv/checkout"
	spec.Persona = "You are Ada."
	specs := &fakeSpecBuilder{spec: spec}
	host, engine, _ := newHostFixtureWithModel(t, specs, "claude-opus-4")
	ctx := context.Background()

	if _, err := host.Provision(ctx, &compassv1.ProvisionAgentWorkspaceRequest{AgentAccountId: "0123456789abcdef0123456789abcdef"}); err != nil {
		t.Fatalf("Provision = %v", err)
	}
	sessionID, err := host.Start(ctx, &compassv1.StartAgentSessionRequest{ContainerName: "cont-1"}, "")
	if err != nil {
		t.Fatalf("Start = %v", err)
	}
	t.Cleanup(func() { _ = host.Stop(context.Background(), sessionID) })

	got := onlyStreamingSpec(t, engine)
	if got.User == nil || *got.User != "4242" {
		t.Fatalf("exec --user = %v, want the handle's workspace uid 4242 (an exec with no --user inherits the container's NET_ADMIN)", derefOr(got.User))
	}
	if got.Workdir == nil || *got.Workdir != "/srv/checkout" {
		t.Fatalf("exec --workdir = %v, want the handle's checkout /srv/checkout", derefOr(got.Workdir))
	}
	for _, want := range []struct{ key, value string }{
		{"HOME", "/home/scoped"},
		{"COMPASS_WORKDIR", "/srv/checkout"},
		{"COMPASS_MODEL", "claude-opus-4"},
		{"COMPASS_PERSONA", "You are Ada."},
	} {
		if got := got.Env[want.key]; got != want.value {
			t.Fatalf("exec env %s = %q, want %q", want.key, got, want.value)
		}
	}
}

// A Runner with no configured model starts its agents with COMPASS_MODEL
// ABSENT, so the agent falls back to its own SDK default rather than receiving
// a blank selector it must special-case. A bug that always exported the host's
// model field would export an empty value here.
func TestStartOmitsModelWhenRunnerHasNoneConfigured(t *testing.T) {
	specs := &fakeSpecBuilder{spec: liveSpec()}
	host, engine, _ := newHostFixtureWithModel(t, specs, "")
	ctx := context.Background()

	if _, err := host.Provision(ctx, &compassv1.ProvisionAgentWorkspaceRequest{AgentAccountId: "0123456789abcdef0123456789abcdef"}); err != nil {
		t.Fatalf("Provision = %v", err)
	}
	sessionID, err := host.Start(ctx, &compassv1.StartAgentSessionRequest{ContainerName: "cont-1"}, "")
	if err != nil {
		t.Fatalf("Start = %v", err)
	}
	t.Cleanup(func() { _ = host.Stop(context.Background(), sessionID) })

	if got, ok := onlyStreamingSpec(t, engine).Env["COMPASS_MODEL"]; ok {
		t.Fatalf("COMPASS_MODEL exported as %q with no model configured; want the key absent so the agent uses its SDK default", got)
	}
}

// Reload relaunches with the SAME exec configuration Start used. Reload has no
// AgentEnv of its own: it re-resolves the container's handle from the registry
// so the restarted agent cannot drift from the one it replaces — a Reload that
// relaunched bare would silently move the agent's cwd back to $HOME and drop
// its model selector mid-session, with the board still showing a live session.
func TestReloadRelaunchesWithTheSameAgentEnv(t *testing.T) {
	spec := liveSpec()
	spec.Workspace.UID = 4242
	spec.Workspace.HomeDir = "/home/scoped"
	spec.Workspace.CheckoutDir = "/srv/checkout"
	specs := &fakeSpecBuilder{spec: spec}
	host, engine, _ := newHostFixtureWithModel(t, specs, "claude-opus-4")
	ctx := context.Background()

	if _, err := host.Provision(ctx, &compassv1.ProvisionAgentWorkspaceRequest{AgentAccountId: "0123456789abcdef0123456789abcdef"}); err != nil {
		t.Fatalf("Provision = %v", err)
	}
	sessionID, err := host.Start(ctx, &compassv1.StartAgentSessionRequest{ContainerName: "cont-1"}, "")
	if err != nil {
		t.Fatalf("Start = %v", err)
	}
	if err := host.Reload(ctx, sessionID); err != nil {
		t.Fatalf("Reload = %v", err)
	}
	t.Cleanup(func() { _ = host.Stop(context.Background(), sessionID) })

	execs := engine.streamingSpecs()
	if len(execs) != 2 {
		t.Fatalf("ExecStreaming called %d times, want 2 (Start then Reload's relaunch)", len(execs))
	}
	// Guard against the degenerate pass where BOTH execs are bare: the relaunch
	// must match a Start that itself carried the container's identity.
	if execs[0].User == nil || execs[0].Workdir == nil {
		t.Fatalf("the initial Start exec carried no user/workdir (%+v); the equality below would be vacuous", execs[0])
	}
	if !reflect.DeepEqual(execs[0], execs[1]) {
		t.Fatalf("Reload relaunched with %+v (user %v, workdir %v), want the same exec spec Start used: %+v (user %v, workdir %v)",
			execs[1], derefOr(execs[1].User), derefOr(execs[1].Workdir),
			execs[0], derefOr(execs[0].User), derefOr(execs[0].Workdir))
	}
}

// A session whose container has since left the registry cannot be reloaded:
// Reload returns errSessionUnknown rather than relaunching without the handle's
// identity. Deregistering models the container being torn down underneath a
// live session. Without the re-resolve, Reload would have started a bare,
// root-privileged agent in a container it can no longer describe.
//
// The rejection must also be a true no-op: the handle is re-resolved BEFORE the
// stream is stopped, so the failed Reload leaves the original agent running
// behind a session the live set still truthfully reports READY. Resolving after
// the stop instead killed the agent and then bailed, leaving a wedged session —
// READY with nothing behind it, and no longer stoppable.
func TestReloadWithDeregisteredContainerIsSessionUnknown(t *testing.T) {
	specs := &fakeSpecBuilder{spec: liveSpec()}
	host, engine, registry := newHostFixture(t, specs)
	ctx := context.Background()

	if _, err := host.Provision(ctx, &compassv1.ProvisionAgentWorkspaceRequest{AgentAccountId: "0123456789abcdef0123456789abcdef"}); err != nil {
		t.Fatalf("Provision = %v", err)
	}
	sessionID, err := host.Start(ctx, &compassv1.StartAgentSessionRequest{ContainerName: "cont-1"}, "")
	if err != nil {
		t.Fatalf("Start = %v", err)
	}
	registry.Deregister("cont-1")

	if err := host.Reload(ctx, sessionID); !errors.Is(err, errSessionUnknown) {
		t.Fatalf("Reload of a session whose container is gone = %v, want errSessionUnknown", err)
	}
	if n := len(engine.streamingSpecs()); n != 1 {
		t.Fatalf("ExecStreaming called %d times, want 1 (the original Start only); Reload must not relaunch without a handle", n)
	}
	live, err := host.Status(ctx, sessionID)
	if err != nil {
		t.Fatalf("Status after a rejected Reload = %v, want the session still live", err)
	}
	if len(live) != 1 || live[0].GetState() != compassv1.AgentSessionState_AGENT_SESSION_STATE_READY {
		t.Fatalf("Status after a rejected Reload = %+v, want one READY session", live)
	}
	// The READY above is only honest if the agent is still running. Stop is the
	// probe: it terminates and reaps the live child, which succeeds exactly once.
	// Against an agent the rejected Reload had already stopped, this second
	// teardown cannot reap again and errors.
	if err := host.Stop(ctx, sessionID); err != nil {
		t.Fatalf("Stop after a rejected Reload = %v, want success; the rejected Reload must leave the agent running, not wedged", err)
	}
}

// onlyStreamingSpec returns the single exec spec the host started an agent
// with, failing if the count is anything but one.
func onlyStreamingSpec(t *testing.T, engine *stubStreamingRuntime) runtime.StreamingExecSpec {
	t.Helper()
	specs := engine.streamingSpecs()
	if len(specs) != 1 {
		t.Fatalf("ExecStreaming called %d times, want exactly 1", len(specs))
	}
	return specs[0]
}

// derefOr renders an optional exec-spec field for a failure message: its value,
// or "<unset>" when the pointer is nil.
func derefOr(p *string) string {
	if p == nil {
		return "<unset>"
	}
	return *p
}

// liveSpec is a minimal launchable AgentSpec for the test container "cont-1".
func liveSpec() runtime.AgentSpec {
	return runtime.AgentSpec{
		Name:  "cont-1",
		Image: "compass-agent:latest",
		Workspace: runtime.Workspace{
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

// TestStartMaterializesSecretsBeforeExec pins the load-bearing conformance
// invariant: the secret materialize (a plain `exec` of `sh -s`) runs BEFORE the
// agent launch (`exec_streaming`), so the agent never comes up before its
// secrets are in the container. The stub records the ordered call log; the
// materialize exec must precede the streaming exec.
func TestStartMaterializesSecretsBeforeExec(t *testing.T) {
	specs := &fakeSpecBuilder{spec: liveSpec()}
	host, engine, pub := newHostFixtureWithPublish(t, specs)
	pub.setSecrets(&compassv1internal.ResolvedSecret{
		Name:     "OPENAI",
		Value:    "sk-live",
		Kind:     compassv1.SecretKind_SECRET_KIND_PROVIDER,
		Provider: "openai",
		Delivery: compassv1.SecretDelivery_SECRET_DELIVERY_FILE,
	})
	ctx := context.Background()

	if _, err := host.Provision(ctx, &compassv1.ProvisionAgentWorkspaceRequest{AgentAccountId: "0123456789abcdef0123456789abcdef"}); err != nil {
		t.Fatalf("Provision = %v", err)
	}
	sessionID, err := host.Start(ctx, &compassv1.StartAgentSessionRequest{ContainerName: "cont-1"}, "")
	if err != nil {
		t.Fatalf("Start = %v", err)
	}
	t.Cleanup(func() { _ = host.Stop(context.Background(), sessionID) })

	calls := engine.callsSnapshot()
	firstExec := slices.Index(calls, "exec")
	launch := slices.Index(calls, "exec_streaming")
	if firstExec == -1 {
		t.Fatalf("no materialize exec ran before the agent launch; calls = %v", calls)
	}
	if launch == -1 {
		t.Fatalf("agent was never launched; calls = %v", calls)
	}
	if firstExec > launch {
		t.Fatalf("materialize exec (idx %d) ran AFTER the agent launch (idx %d); the agent can race an empty seed. calls = %v", firstExec, launch, calls)
	}
}

// TestStartFetchesSecretsByContainer pins that the pre-exec fetch selects the
// container binding (not a session, which does not exist yet), so the Server can
// authorize it against the Provision-time container→account binding.
func TestStartFetchesSecretsByContainer(t *testing.T) {
	specs := &fakeSpecBuilder{spec: liveSpec()}
	host, _, pub := newHostFixtureWithPublish(t, specs)
	ctx := context.Background()

	if _, err := host.Provision(ctx, &compassv1.ProvisionAgentWorkspaceRequest{AgentAccountId: "0123456789abcdef0123456789abcdef"}); err != nil {
		t.Fatalf("Provision = %v", err)
	}
	sessionID, err := host.Start(ctx, &compassv1.StartAgentSessionRequest{ContainerName: "cont-1"}, "")
	if err != nil {
		t.Fatalf("Start = %v", err)
	}
	t.Cleanup(func() { _ = host.Stop(context.Background(), sessionID) })

	reqs := pub.fetchRequests()
	if len(reqs) != 1 {
		t.Fatalf("FetchSecrets called %d times during Start, want exactly 1", len(reqs))
	}
	if got := reqs[0].GetContainerName(); got != "cont-1" {
		t.Fatalf("pre-exec fetch container_name = %q, want cont-1", got)
	}
	if got := reqs[0].GetSessionId(); got != "" {
		t.Fatalf("pre-exec fetch set session_id = %q, want empty (by-container selector only)", got)
	}
}

// TestStartAgentExecCarriesNoEnvFile pins that the agent's own exec does NOT
// carry an --env-file: env-delivery secrets are materialized to the in-container
// $HOME/.compass/env and sourced by the agent, never passed on the exec (podman
// resolves --env-file host-side, where the container file does not exist). The
// streaming spec still carries HOME so the agent can locate that file.
func TestStartAgentExecCarriesNoEnvFile(t *testing.T) {
	spec := liveSpec()
	spec.Workspace.HomeDir = "/home/scoped"
	specs := &fakeSpecBuilder{spec: spec}
	host, engine, _ := newHostFixtureWithPublish(t, specs)
	ctx := context.Background()

	if _, err := host.Provision(ctx, &compassv1.ProvisionAgentWorkspaceRequest{AgentAccountId: "0123456789abcdef0123456789abcdef"}); err != nil {
		t.Fatalf("Provision = %v", err)
	}
	sessionID, err := host.Start(ctx, &compassv1.StartAgentSessionRequest{ContainerName: "cont-1"}, "")
	if err != nil {
		t.Fatalf("Start = %v", err)
	}
	t.Cleanup(func() { _ = host.Stop(context.Background(), sessionID) })

	got := onlyStreamingSpec(t, engine)
	// The StreamingExecSpec no longer has an EnvFile field at all — the compiler
	// enforces that env-delivery cannot ride --env-file. What remains to check is
	// that HOME still rides the exec so the agent can locate $HOME/.compass/env
	// to source it.
	if got.Env["HOME"] != "/home/scoped" {
		t.Fatalf("agent exec HOME = %q, want /home/scoped", got.Env["HOME"])
	}
}

// TestStartToleratesNoSecretsSurface pins that a Server with no secrets surface
// (FetchSecrets → CodeFailedPrecondition) does not block agent start: the agent
// must still come up when there is simply nothing to materialize.
func TestStartToleratesNoSecretsSurface(t *testing.T) {
	specs := &fakeSpecBuilder{spec: liveSpec()}
	host, engine, pub := newHostFixtureWithPublish(t, specs)
	pub.setFetchErr(connect.NewError(connect.CodeFailedPrecondition, errors.New("no secret resolver wired")))
	ctx := context.Background()

	if _, err := host.Provision(ctx, &compassv1.ProvisionAgentWorkspaceRequest{AgentAccountId: "0123456789abcdef0123456789abcdef"}); err != nil {
		t.Fatalf("Provision = %v", err)
	}
	sessionID, err := host.Start(ctx, &compassv1.StartAgentSessionRequest{ContainerName: "cont-1"}, "")
	if err != nil {
		t.Fatalf("Start with no secrets surface = %v, want the agent to start anyway", err)
	}
	t.Cleanup(func() { _ = host.Stop(context.Background(), sessionID) })

	// The agent exec still ran despite the unavailable secrets surface.
	onlyStreamingSpec(t, engine)
}

// TestStartFailsClosedOnFetchError pins the security-critical other arm of the
// fetch switch: any fetch error EXCEPT the no-secrets-surface
// CodeFailedPrecondition fails the Start, so the agent never comes up silently
// missing secrets that exist. The transient-transport case (CodeUnavailable) is
// the one that matters most — it is deliberately NOT tolerated, because a blip
// against a Server that DOES have secrets must not be read as "no secrets".
func TestStartFailsClosedOnFetchError(t *testing.T) {
	for _, tc := range []struct {
		name string
		code connect.Code
	}{
		{"transient transport blip", connect.CodeUnavailable},
		{"authz denial", connect.CodePermissionDenied},
		{"resolver fault", connect.CodeInternal},
	} {
		t.Run(tc.name, func(t *testing.T) {
			specs := &fakeSpecBuilder{spec: liveSpec()}
			host, engine, pub := newHostFixtureWithPublish(t, specs)
			pub.setFetchErr(connect.NewError(tc.code, errors.New(tc.name)))
			ctx := context.Background()

			if _, err := host.Provision(ctx, &compassv1.ProvisionAgentWorkspaceRequest{AgentAccountId: "0123456789abcdef0123456789abcdef"}); err != nil {
				t.Fatalf("Provision = %v", err)
			}
			if _, err := host.Start(ctx, &compassv1.StartAgentSessionRequest{ContainerName: "cont-1"}, ""); err == nil {
				t.Fatalf("Start with a %v fetch error = nil, want the Start to fail closed", tc.code)
			}

			// The agent exec must NOT have run: only the no-secrets-surface
			// CodeFailedPrecondition tolerates a missing set; every other code
			// fails before StartAgent.
			if execs := engine.streamingSpecs(); len(execs) != 0 {
				t.Fatalf("ExecStreaming ran %d times on a fail-closed Start, want 0", len(execs))
			}
		})
	}
}

// newHostFixtureWithRecordingExec mirrors newHostFixtureWithModel but wires a
// recordingExecRuntime as the engine, so BOTH the agent-launch ExecStreaming and
// the one-shot Exec (the resume-write) are captured — the default
// stubStreamingRuntime.Exec is a recording no-op that keeps no spec, so a resume
// write would be invisible to an assertion.
func newHostFixtureWithRecordingExec(t *testing.T, specs SpecBuilder) (SessionHost, *recordingExecRuntime) {
	t.Helper()
	engine := newRecordingExecRuntime(t)
	registry := runtime.NewAgentRegistry()
	rt := runtime.NewAgentRuntimeWithRegistry(engine, registry)
	link := newLink(newRunnerServiceServer(t, newCapturePublish()))
	var n int
	newID := func() string { n++; return "sess-" + string(rune('0'+n)) }
	cfg := AgentHostConfig{RuntimeDir: t.TempDir()}
	host := NewSessionHost(link, rt, registry, engine, specs, cfg, discardLoggerRunner(), newID)
	return host, engine
}

// TestStartWithResumeBodyMaterializesSessionFile: a Start carrying a non-empty
// resume_session_id + body writes that body into the container at
// $HOME/.compass/resume/<id>.jsonl over a one-shot Exec running as the agent uid,
// with the body on stdin (never argv), and exports COMPASS_RESUME_SESSION_FILE
// on the agent launch pointing at that same absolute path. The agent's first
// read of a resumed session must find the reconstructed file.
func TestStartWithResumeBodyMaterializesSessionFile(t *testing.T) {
	specs := &fakeSpecBuilder{spec: liveSpec()}
	host, engine := newHostFixtureWithRecordingExec(t, specs)
	ctx := context.Background()

	if _, err := host.Provision(ctx, &compassv1.ProvisionAgentWorkspaceRequest{AgentAccountId: "0123456789abcdef0123456789abcdef"}); err != nil {
		t.Fatalf("Provision = %v", err)
	}
	const resumeID = "sess-abc123"
	const body = "{\"type\":\"user\"}\n{\"type\":\"assistant\"}\n"
	wantPath := "/home/agent/.compass/resume/" + resumeID + ".jsonl"

	if _, err := host.Start(ctx, &compassv1.StartAgentSessionRequest{ContainerName: "cont-1", ResumeSessionId: resumeID}, body); err != nil {
		t.Fatalf("Start = %v", err)
	}

	// The resume-write one-shot Exec: body on stdin, path as the sh positional
	// arg, as the agent uid.
	var write *runtime.ExecSpec
	for _, spec := range engine.execSnapshot() {
		if spec.Stdin != nil && *spec.Stdin == body {
			s := spec
			write = &s
			break
		}
	}
	if write == nil {
		t.Fatal("no resume-write Exec whose Stdin == the resume body ran")
	}
	if !slices.Contains(write.Command, wantPath) {
		t.Fatalf("resume-write Command = %v, want it to carry the path %q as a positional arg", write.Command, wantPath)
	}
	if write.Command[0] != "sh" {
		t.Fatalf("resume-write Command[0] = %q, want the sh script shell", write.Command[0])
	}
	// The write must land 0600 under umask 077, matching every sibling $HOME
	// materializer (a session transcript is as sensitive as the env file).
	if script := strings.Join(write.Command, " "); !strings.Contains(script, "umask 077") || !strings.Contains(script, "chmod 600") {
		t.Fatalf("resume-write script = %q, want it to set umask 077 + chmod 600", script)
	}
	if write.User == nil || *write.User != "1000" {
		t.Fatalf("resume-write User = %v, want the agent uid 1000", derefOr(write.User))
	}

	// The agent launch carries COMPASS_RESUME_SESSION_FILE == the same absolute
	// path the file was written to.
	launch := onlyStreamingSpec(t, engine.stubStreamingRuntime)
	if got := launch.Env["COMPASS_RESUME_SESSION_FILE"]; got != wantPath {
		t.Fatalf("launch COMPASS_RESUME_SESSION_FILE = %q, want %q", got, wantPath)
	}
}

// TestStartWithoutResumeDoesNotMaterializeOrSetEnv: a fresh Start (empty
// resume_session_id) writes no resume file and exports no
// COMPASS_RESUME_SESSION_FILE, so a non-resume start never materializes stale
// state nor points the agent at a file that does not exist.
func TestStartWithoutResumeDoesNotMaterializeOrSetEnv(t *testing.T) {
	specs := &fakeSpecBuilder{spec: liveSpec()}
	host, engine := newHostFixtureWithRecordingExec(t, specs)
	ctx := context.Background()

	if _, err := host.Provision(ctx, &compassv1.ProvisionAgentWorkspaceRequest{AgentAccountId: "0123456789abcdef0123456789abcdef"}); err != nil {
		t.Fatalf("Provision = %v", err)
	}
	if _, err := host.Start(ctx, &compassv1.StartAgentSessionRequest{ContainerName: "cont-1"}, ""); err != nil {
		t.Fatalf("Start = %v", err)
	}

	for _, spec := range engine.execSnapshot() {
		if spec.Stdin == nil {
			continue
		}
		for _, arg := range spec.Command {
			if strings.Contains(arg, "/.compass/resume/") {
				t.Fatalf("a resume-write Exec ran on a fresh start; Command %v", spec.Command)
			}
		}
	}
	launch := onlyStreamingSpec(t, engine.stubStreamingRuntime)
	if _, ok := launch.Env["COMPASS_RESUME_SESSION_FILE"]; ok {
		t.Fatalf("launch env carries COMPASS_RESUME_SESSION_FILE on a fresh start, want it unset")
	}
}

// TestStartResumeBodyWithoutIDStartsFresh: a resume body with an EMPTY
// resume_session_id is a Server-side skew — the id is the sole discriminator, so
// the body is dropped and the Start proceeds as a fresh (non-resume) launch: no
// resume-write Exec, no COMPASS_RESUME_SESSION_FILE, and the agent still comes
// up. This pins the symmetric drop-and-warn path (host.go else-if) so a future
// change can't silently start materializing off a body without an authorized id.
func TestStartResumeBodyWithoutIDStartsFresh(t *testing.T) {
	specs := &fakeSpecBuilder{spec: liveSpec()}
	host, engine := newHostFixtureWithRecordingExec(t, specs)
	ctx := context.Background()

	if _, err := host.Provision(ctx, &compassv1.ProvisionAgentWorkspaceRequest{AgentAccountId: "0123456789abcdef0123456789abcdef"}); err != nil {
		t.Fatalf("Provision = %v", err)
	}
	// Body set, id empty: the body must be dropped, not materialized.
	if _, err := host.Start(ctx, &compassv1.StartAgentSessionRequest{ContainerName: "cont-1"}, "body"); err != nil {
		t.Fatalf("Start = %v", err)
	}

	for _, spec := range engine.execSnapshot() {
		if spec.Stdin != nil && *spec.Stdin == "body" {
			t.Fatalf("a resume-write Exec ran for a body without an id; Command %v", spec.Command)
		}
	}
	launch := onlyStreamingSpec(t, engine.stubStreamingRuntime)
	if _, ok := launch.Env["COMPASS_RESUME_SESSION_FILE"]; ok {
		t.Fatalf("launch env carries COMPASS_RESUME_SESSION_FILE for a body without an id, want it unset")
	}
}

// TestStartResumeWriteFailureFailsStart: when the resume-write Exec fails, Start
// fails wrapping the error and never launches the agent — the write runs strictly
// before StartAgent, so the agent must not come up over a missing/partial resume
// file.
func TestStartResumeWriteFailureFailsStart(t *testing.T) {
	specs := &fakeSpecBuilder{spec: liveSpec()}
	host, engine := newHostFixtureWithRecordingExec(t, specs)
	ctx := context.Background()

	if _, err := host.Provision(ctx, &compassv1.ProvisionAgentWorkspaceRequest{AgentAccountId: "0123456789abcdef0123456789abcdef"}); err != nil {
		t.Fatalf("Provision = %v", err)
	}
	// Fail ONLY the resume-write Exec. The secrets env-file materialize also
	// runs an Exec in Start (always, even with no secrets) strictly before the
	// resume write, so a global execErr would trip THAT one first and this test
	// would pass without ever exercising the resume-write path. Select the
	// resume write by its stdin — it is the one Exec fed the raw resume body.
	engine.mu.Lock()
	engine.execErr = errors.New("write failed inside container")
	engine.execErrStdin = "body"
	engine.mu.Unlock()
	_, err := host.Start(ctx, &compassv1.StartAgentSessionRequest{ContainerName: "cont-1", ResumeSessionId: "sess-x"}, "body")
	if err == nil {
		t.Fatal("Start with a failing resume-write = nil, want the Start to fail closed")
	}
	// Pin the failure to the resume stage, not a coincidental earlier Exec: the
	// error must be the resume-materialize wrap AND carry the injected cause.
	if !strings.Contains(err.Error(), "materializing resume session file") {
		t.Fatalf("Start error = %v, want the resume-materialize stage wrap", err)
	}
	if !strings.Contains(err.Error(), "write failed inside container") {
		t.Fatalf("Start error = %v, want it to wrap the write failure", err)
	}
	// The resume-write Exec actually ran (stdin == the resume body).
	sawResumeWrite := false
	for _, spec := range engine.execSnapshot() {
		if spec.Stdin != nil && *spec.Stdin == "body" {
			sawResumeWrite = true
		}
	}
	if !sawResumeWrite {
		t.Fatal("no resume-write Exec ran; the failure was injected on the wrong stage")
	}
	if execs := engine.streamingSpecs(); len(execs) != 0 {
		t.Fatalf("ExecStreaming ran %d times after a failed resume-write, want 0 (write precedes StartAgent)", len(execs))
	}
}

// TestStartRejectsResumeIDTraversal: a resume_session_id that is not a bare path
// element (a separator, a parent ref) fails the Start closed and writes nothing.
// The id becomes a filename component in the container, and the Runner is a
// distinct trust boundary from the authz-gating Server: a crafted id must never
// redirect the materialize write outside .compass/resume/.
func TestStartRejectsResumeIDTraversal(t *testing.T) {
	cases := map[string]string{
		"parent refs":   "../../etc/cron.d/x",
		"nested subdir": "sub/dir",
		"absolute path": "/abs/path",
		"trailing sep":  "sess/",
	}
	for name, id := range cases {
		t.Run(name, func(t *testing.T) {
			specs := &fakeSpecBuilder{spec: liveSpec()}
			host, engine := newHostFixtureWithRecordingExec(t, specs)
			ctx := context.Background()

			if _, err := host.Provision(ctx, &compassv1.ProvisionAgentWorkspaceRequest{AgentAccountId: "0123456789abcdef0123456789abcdef"}); err != nil {
				t.Fatalf("Provision = %v", err)
			}
			_, err := host.Start(ctx, &compassv1.StartAgentSessionRequest{ContainerName: "cont-1", ResumeSessionId: id}, "body")
			if err == nil {
				t.Fatalf("Start with resume_session_id %q = nil, want the Start to fail closed", id)
			}
			if !strings.Contains(err.Error(), "invalid resume_session_id") {
				t.Fatalf("Start error = %v, want it to reject the invalid resume_session_id", err)
			}
			// Nothing materialized: no resume-write Exec, no agent launch.
			for _, spec := range engine.execSnapshot() {
				if spec.Stdin != nil && *spec.Stdin == "body" {
					t.Fatalf("a resume-write Exec ran for a rejected id %q; Command %v", id, spec.Command)
				}
			}
			if execs := engine.streamingSpecs(); len(execs) != 0 {
				t.Fatalf("ExecStreaming ran %d times for a rejected id %q, want 0", len(execs), id)
			}
		})
	}
}

// TestStartResumeIDDotStaysInResumeDir: a bare "." or ".." passes the
// element-shape guard (each equals its own filepath.Base and has no separator)
// and is safe ONLY because the ".jsonl" suffix turns it into an inert in-dir
// filename. This pins that the materialized path still resolves directly inside
// .compass/resume/ — a future change to the suffix or the guard that reintroduced
// a "." / ".." traversal would redden here.
func TestStartResumeIDDotStaysInResumeDir(t *testing.T) {
	for _, id := range []string{".", ".."} {
		t.Run(id, func(t *testing.T) {
			specs := &fakeSpecBuilder{spec: liveSpec()}
			host, engine := newHostFixtureWithRecordingExec(t, specs)
			ctx := context.Background()

			if _, err := host.Provision(ctx, &compassv1.ProvisionAgentWorkspaceRequest{AgentAccountId: "0123456789abcdef0123456789abcdef"}); err != nil {
				t.Fatalf("Provision = %v", err)
			}
			if _, err := host.Start(ctx, &compassv1.StartAgentSessionRequest{ContainerName: "cont-1", ResumeSessionId: id}, "body"); err != nil {
				t.Fatalf("Start with resume_session_id %q = %v, want it to succeed (suffix neutralizes the dot)", id, err)
			}
			var write *runtime.ExecSpec
			for _, spec := range engine.execSnapshot() {
				if spec.Stdin != nil && *spec.Stdin == "body" {
					s := spec
					write = &s
					break
				}
			}
			if write == nil {
				t.Fatalf("no resume-write Exec ran for id %q", id)
			}
			wantPath := "/home/agent/.compass/resume/" + id + ".jsonl"
			if !slices.Contains(write.Command, wantPath) {
				t.Fatalf("resume-write Command = %v, want the path %q inside .compass/resume/", write.Command, wantPath)
			}
		})
	}
}
