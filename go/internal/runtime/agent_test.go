package runtime

// THE T6 HERMETIC GATE. FakeRuntime records the calls AgentRuntime.Launch /
// Teardown make against the container engine, so the lifecycle's composition —
// stage ordering, which user each step runs as, the scoped $HOME, the
// failure-cleanup path — is asserted without a real container. Every test here
// pins a contract a plausible reorder / wrong-user / missing-cleanup bug would
// break.

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeRuntime records the calls the lifecycle makes so composition (order, user,
// env) is asserted without a real container. The recorded slices are
// mutex-guarded so the fake stays honest under -race even though a single Launch
// drives it from one goroutine.
type fakeRuntime struct {
	t                  *testing.T
	mu                 sync.Mutex
	calls              []string
	execs              []ExecSpec
	failExecContaining string
}

func newFakeRuntime(t *testing.T) *fakeRuntime {
	t.Helper()
	return &fakeRuntime{t: t}
}

func (f *fakeRuntime) Create(_ context.Context, spec ContainerSpec) (ContainerID, error) {
	f.record("create:" + spec.Name)
	// The container must carry NET_ADMIN so the entrypoint can arm nft.
	if !slices.Contains(spec.CapAdd, "NET_ADMIN") {
		f.t.Errorf("Create spec.CapAdd = %v, must contain NET_ADMIN so the entrypoint can arm the firewall", spec.CapAdd)
	}
	return ContainerID("fake-id"), nil
}

func (f *fakeRuntime) Start(_ context.Context, _ ContainerID) error {
	f.record("start")
	return nil
}

func (f *fakeRuntime) Exec(_ context.Context, _ ContainerID, spec ExecSpec) (ExecOutput, error) {
	joined := strings.Join(spec.Command, " ")
	f.mu.Lock()
	f.calls = append(f.calls, "exec:"+joined)
	f.execs = append(f.execs, spec)
	f.mu.Unlock()

	if f.failExecContaining != "" && strings.Contains(joined, f.failExecContaining) {
		return ExecOutput{Stderr: "boom", ExitCode: 1}, nil
	}
	return ExecOutput{}, nil
}

func (f *fakeRuntime) ExecStreaming(_ context.Context, _ ContainerID, spec StreamingExecSpec) (*StreamingExec, error) {
	f.record("exec_streaming:" + strings.Join(spec.Command, " "))
	// No T6 lifecycle test drives a streaming exec. Rather than synthesize pipe
	// handles it can't honestly back (or spawn a real `sh -c cat` the way the
	// Rust fake does), the fake refuses — keeping the ContainerRuntime interface
	// satisfied without leaking a real process.
	return nil, errors.New("fakeRuntime does not support streaming exec")
}

func (f *fakeRuntime) Stop(_ context.Context, _ ContainerID, _ time.Duration) error {
	f.record("stop")
	return nil
}

func (f *fakeRuntime) Remove(_ context.Context, _ ContainerID) error {
	f.record("remove")
	return nil
}

func (f *fakeRuntime) Exists(_ context.Context, _ string) (bool, error) {
	return false, nil
}

func (f *fakeRuntime) record(call string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, call)
}

func (f *fakeRuntime) callsSnapshot() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.calls)
}

func (f *fakeRuntime) execsSnapshot() []ExecSpec {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.execs)
}

// specWithCreds mirrors the Rust `spec_with_creds` helper: a fixed agent spec
// with credentials attached only when withCreds is true.
func specWithCreds(withCreds bool) AgentSpec {
	var creds *Credentials
	if withCreds {
		creds = &Credentials{Host: "github.com", Username: "seal-agent", Token: "ghp_x"}
	}
	return AgentSpec{
		Name:  "atlas-1",
		Image: "compass-agent:latest",
		Workspace: Workspace{
			Source:      LocalPathSource("/src/demo.git"),
			Branch:      "main",
			CheckoutDir: "/work/repo",
			HomeDir:     "/home/agent",
			UID:         1000,
			Credentials: creds,
		},
		Egress: MustAllowEgress("github.com"),
		Mounts: nil,
	}
}

// lastIndexOf returns the index of the last call equal to want, or -1 — the
// idiomatic Go stand-in for Rust's rposition (the stdlib slices package has no
// LastIndex).
func lastIndexOf(calls []string, want string) int {
	for i, call := range slices.Backward(calls) {
		if call == want {
			return i
		}
	}
	return -1
}

func TestLaunchOrdersStagesEgressBeforeClone(t *testing.T) {
	fake := newFakeRuntime(t)
	rt := NewAgentRuntime(fake)

	if _, err := rt.Launch(t.Context(), specWithCreds(true)); err != nil {
		t.Fatalf("Launch error = %v", err)
	}

	calls := fake.callsSnapshot()
	create := slices.IndexFunc(calls, func(c string) bool { return strings.HasPrefix(c, "create:") })
	start := slices.Index(calls, "start")
	egress := slices.IndexFunc(calls, func(c string) bool { return strings.Contains(c, "compass_egress") })
	clone := slices.IndexFunc(calls, func(c string) bool { return strings.Contains(c, "git clone") })
	if create < 0 || start < 0 || egress < 0 || clone < 0 {
		t.Fatalf("missing a stage call: create=%d start=%d egress=%d clone=%d; calls=%v", create, start, egress, clone, calls)
	}

	// create -> start -> arm egress -> ... -> clone. Egress must be armed before
	// the clone reaches the network.
	if create >= start {
		t.Errorf("create (%d) must precede start (%d)", create, start)
	}
	if start >= egress {
		t.Errorf("egress (%d) must be armed after start (%d)", egress, start)
	}
	if egress >= clone {
		t.Errorf("egress (%d) must be armed before the clone (%d)", egress, clone)
	}
}

// A source-less workspace (empty Workspace.Source) provisions without ever
// emitting a `git clone` exec — the Runner clones no repo (SEA-1527). An agent
// is not constrained to one repo; it clones what it has credentials for. The
// firewall is still armed, and CheckoutDir is still created (the clone would
// otherwise have materialized it) so the session's --workdir has a cwd.
func TestLaunchWithoutSourceSkipsClone(t *testing.T) {
	fake := newFakeRuntime(t)
	rt := NewAgentRuntime(fake)

	spec := specWithCreds(true)
	spec.Workspace.Source = RepoSource{}

	if _, err := rt.Launch(t.Context(), spec); err != nil {
		t.Fatalf("Launch error = %v", err)
	}

	calls := fake.callsSnapshot()
	for _, call := range calls {
		if strings.Contains(call, "git clone") {
			t.Fatalf("source-less workspace emitted a clone call %q, want none", call)
		}
	}
	// Egress is still armed on the source-less path (the clone is skipped, the
	// firewall is not) — a source-less container must not launch unfirewalled.
	if !slices.ContainsFunc(calls, func(c string) bool { return strings.Contains(c, "compass_egress") }) {
		t.Fatal("source-less workspace armed no egress firewall, want egress still armed")
	}
	// CheckoutDir is still created as the agent user. The clone used to be what
	// materialized it; without this the session's --workdir CheckoutDir would
	// chdir into a missing directory and the agent would never start.
	var mkdir *ExecSpec
	execs := fake.execsSnapshot()
	for i := range execs {
		if len(execs[i].Command) > 0 && execs[i].Command[0] == "mkdir" {
			mkdir = &execs[i]
			break
		}
	}
	if mkdir == nil {
		t.Fatal("source-less workspace created no checkout dir, want a mkdir of CheckoutDir")
	}
	if !slices.Contains(mkdir.Command, spec.Workspace.CheckoutDir) {
		t.Errorf("checkout-dir mkdir = %v, want it to target CheckoutDir %q", mkdir.Command, spec.Workspace.CheckoutDir)
	}
	if mkdir.User == nil || *mkdir.User != "1000" {
		t.Errorf("checkout-dir mkdir User = %v, want \"1000\" (created as the agent, owned by the agent)", mkdir.User)
	}
}

func TestCloneRunsAsAgentUserWithScopedHome(t *testing.T) {
	fake := newFakeRuntime(t)
	rt := NewAgentRuntime(fake)

	if _, err := rt.Launch(t.Context(), specWithCreds(true)); err != nil {
		t.Fatalf("Launch error = %v", err)
	}

	var clone *ExecSpec
	execs := fake.execsSnapshot()
	for i := range execs {
		if slices.Contains(execs[i].Command, "clone") {
			clone = &execs[i]
			break
		}
	}
	if clone == nil {
		t.Fatal("no clone exec recorded")
	}
	if clone.User == nil || *clone.User != "1000" {
		t.Errorf("clone exec User = %v, want \"1000\" (clone runs unprivileged)", clone.User)
	}
	if got := clone.Env["HOME"]; got != "/home/agent" {
		t.Errorf("clone exec HOME = %q, want %q (scoped $HOME)", got, "/home/agent")
	}
}

func TestEgressIsArmedAsRootNotTheAgentUser(t *testing.T) {
	fake := newFakeRuntime(t)
	rt := NewAgentRuntime(fake)

	if _, err := rt.Launch(t.Context(), specWithCreds(false)); err != nil {
		t.Fatalf("Launch error = %v", err)
	}

	var egress *ExecSpec
	execs := fake.execsSnapshot()
	for i := range execs {
		if slices.ContainsFunc(execs[i].Command, func(tok string) bool { return strings.Contains(tok, "compass_egress") }) {
			egress = &execs[i]
			break
		}
	}
	if egress == nil {
		t.Fatal("no egress exec recorded")
	}
	// Armed as root (no --user): the agent user could not create nft rules.
	if egress.User != nil {
		t.Errorf("egress exec User = %q, want nil (armed as container root)", *egress.User)
	}
}

func TestNoCredentialsSkipsCredentialExec(t *testing.T) {
	fake := newFakeRuntime(t)
	rt := NewAgentRuntime(fake)

	if _, err := rt.Launch(t.Context(), specWithCreds(false)); err != nil {
		t.Fatalf("Launch error = %v", err)
	}

	// The credential script (which carries the "git-credentials" needle) rides
	// in ExecSpec.Stdin, not Command — so the no-install contract is checked
	// where the script actually lives. With no credentials, no exec carries it.
	for _, e := range fake.execsSnapshot() {
		if e.Stdin != nil && strings.Contains(*e.Stdin, "git-credentials") {
			t.Errorf("a credential-install exec ran with no credentials configured; stdin: %q", *e.Stdin)
		}
	}
}

func TestFailedProvisionRemovesThePartialContainer(t *testing.T) {
	fake := newFakeRuntime(t)
	fake.failExecContaining = "compass_egress"
	rt := NewAgentRuntime(fake)

	_, err := rt.Launch(t.Context(), specWithCreds(true))

	var inContainer *InContainerError
	if !errors.As(err, &inContainer) {
		t.Fatalf("Launch error = %v, want *InContainerError", err)
	}
	if inContainer.Stage != "arm egress" {
		t.Fatalf("InContainerError.Stage = %q, want %q", inContainer.Stage, "arm egress")
	}
	if !slices.Contains(fake.callsSnapshot(), "remove") {
		t.Errorf("a failed launch must remove the partial container; calls = %v", fake.callsSnapshot())
	}
}

func TestTeardownStopsThenRemoves(t *testing.T) {
	fake := newFakeRuntime(t)
	rt := NewAgentRuntime(fake)

	handle, err := rt.Launch(t.Context(), specWithCreds(false))
	if err != nil {
		t.Fatalf("Launch error = %v", err)
	}
	if err := rt.Teardown(t.Context(), handle); err != nil {
		t.Fatalf("Teardown error = %v", err)
	}

	calls := fake.callsSnapshot()
	stop := lastIndexOf(calls, "stop")
	remove := lastIndexOf(calls, "remove")
	if stop < 0 || remove < 0 {
		t.Fatalf("teardown missing stop/remove: stop=%d remove=%d; calls=%v", stop, remove, calls)
	}
	if stop >= remove {
		t.Errorf("stop (%d) must precede remove (%d)", stop, remove)
	}
}
