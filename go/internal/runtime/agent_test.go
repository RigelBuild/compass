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

func TestLaunchOrdersStagesEgressBeforeCheckoutDir(t *testing.T) {
	fake := newFakeRuntime(t)
	rt := NewAgentRuntime(fake)

	if _, err := rt.Launch(t.Context(), specWithCreds(true)); err != nil {
		t.Fatalf("Launch error = %v", err)
	}

	calls := fake.callsSnapshot()
	create := slices.IndexFunc(calls, func(c string) bool { return strings.HasPrefix(c, "create:") })
	start := slices.Index(calls, "start")
	egress := slices.IndexFunc(calls, func(c string) bool { return strings.Contains(c, "compass_egress") })
	mkdir := slices.IndexFunc(calls, func(c string) bool { return strings.Contains(c, "mkdir") })
	if create < 0 || start < 0 || egress < 0 || mkdir < 0 {
		t.Fatalf("missing a stage call: create=%d start=%d egress=%d mkdir=%d; calls=%v", create, start, egress, mkdir, calls)
	}
	// The runner no longer clones; it only creates the checkout dir the agent
	// self-clones into post-launch.
	if slices.ContainsFunc(calls, func(c string) bool { return strings.Contains(c, "git clone") }) {
		t.Fatalf("launch must not run a git clone; calls=%v", calls)
	}

	// create -> start -> arm egress -> ... -> create checkout dir.
	if create >= start {
		t.Errorf("create (%d) must precede start (%d)", create, start)
	}
	if start >= egress {
		t.Errorf("egress (%d) must be armed after start (%d)", egress, start)
	}
	if egress >= mkdir {
		t.Errorf("egress (%d) must be armed before the checkout dir is created (%d)", egress, mkdir)
	}
}

func TestCheckoutDirCreatedAsAgentUser(t *testing.T) {
	fake := newFakeRuntime(t)
	rt := NewAgentRuntime(fake)

	if _, err := rt.Launch(t.Context(), specWithCreds(true)); err != nil {
		t.Fatalf("Launch error = %v", err)
	}

	var mkdir *ExecSpec
	execs := fake.execsSnapshot()
	for i := range execs {
		if slices.Contains(execs[i].Command, "mkdir") {
			mkdir = &execs[i]
			break
		}
		if slices.Contains(execs[i].Command, "clone") {
			t.Fatalf("launch must not run a git clone; command=%v", execs[i].Command)
		}
	}
	if mkdir == nil {
		t.Fatal("no mkdir exec recorded")
	}
	if mkdir.User == nil || *mkdir.User != "1000" {
		t.Errorf("mkdir exec User = %v, want \"1000\" (checkout dir owned by the agent)", mkdir.User)
	}
	if !slices.Contains(mkdir.Command, "/work/repo") {
		t.Errorf("mkdir exec Command = %v, want it to target the checkout dir", mkdir.Command)
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
