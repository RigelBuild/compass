//go:build unix

package runtime

// The hermetic MicroVMRuntime.Start suite: it drives Start behind the launchFunc
// + newGuestClient seams so no real cloud-hypervisor boots and no real vsock is
// dialed. A fake guestVM answers Health (with the minted boot nonce) and records
// Shutdown; a fake GuestControlClient records the ProvisionRequest. Together they
// prove the script-delivery contract (§(a)/(e)) and the fail-closed teardown
// (§(b)/(d)) with no KVM. It is //go:build unix because the seams and the
// guestVM interface it fakes are unix-only.
//
// This is deliberately NOT modelled on serveFakeGuest, which returns a
// *GuestExec over a plain unix listener that cannot speak the vsock CONNECT
// preamble and cannot answer Health.

import (
	"context"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"

	compassv1 "github.com/RigelBuild/compass/go/internal/gen/compass/v1"
	"github.com/RigelBuild/compass/go/internal/gen/compass/v1/compassv1internalconnect"
	"github.com/RigelBuild/compass/go/internal/runtime/microvm"
)

// fakeGuestVM is a guestVM handle that answers Health ready (echoing the minted
// nonce so awaitHealthy's identity binding passes) and records whether Shutdown
// was called (the fail-closed teardown assertion). WaitVMMExit/PSS satisfy the
// interface's remaining method set but are unexercised here.
type fakeGuestVM struct {
	nonce       []byte
	mu          sync.Mutex
	shutdown    bool
	healthCalls int
}

func (f *fakeGuestVM) Health(context.Context) (*compassv1.HealthResponse, error) {
	f.mu.Lock()
	f.healthCalls++
	f.mu.Unlock()
	return &compassv1.HealthResponse{
		NetProvisioned:   true,
		WorkspaceMounted: true,
		BootNonce:        f.nonce,
	}, nil
}

func (f *fakeGuestVM) Shutdown(context.Context) error {
	f.mu.Lock()
	f.shutdown = true
	f.mu.Unlock()
	return nil
}

func (f *fakeGuestVM) WaitVMMExit(_ time.Duration) bool { return true }

// PSS returns an empty map: it satisfies the guestVM interface's method set (the
// //go:build microvm contract test calls session.vm.PSS()) but is never invoked
// on the hermetic Start path this fake serves.
func (f *fakeGuestVM) PSS() (map[string]int64, error) { return map[string]int64{}, nil }

func (f *fakeGuestVM) wasShutdown() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.shutdown
}

// fakeGuestClient is a GuestControlClient that records the ProvisionRequest and
// optionally fails it. Only Provision is exercised by Start; the other verbs
// satisfy the interface but are never called on this hermetic path.
type fakeGuestClient struct {
	mu          sync.Mutex
	provisioned *compassv1.ProvisionRequest
	provErr     error
}

func (c *fakeGuestClient) Provision(_ context.Context, req *connect.Request[compassv1.ProvisionRequest]) (*connect.Response[compassv1.ProvisionResponse], error) {
	c.mu.Lock()
	c.provisioned = req.Msg
	c.mu.Unlock()
	if c.provErr != nil {
		return nil, c.provErr
	}
	return connect.NewResponse(&compassv1.ProvisionResponse{}), nil
}

func (c *fakeGuestClient) Health(context.Context, *connect.Request[compassv1.HealthRequest]) (*connect.Response[compassv1.HealthResponse], error) {
	return nil, errors.New("fakeGuestClient: Health not used on the hermetic Start path")
}

func (c *fakeGuestClient) Exec(context.Context, *connect.Request[compassv1.ExecRequest]) (*connect.Response[compassv1.ExecResponse], error) {
	return nil, errors.New("fakeGuestClient: Exec not used on the hermetic Start path")
}

func (c *fakeGuestClient) ExecStream(context.Context) *connect.BidiStreamForClient[compassv1.ExecStreamRequest, compassv1.ExecStreamResponse] {
	return nil
}

func (c *fakeGuestClient) Signal(context.Context, *connect.Request[compassv1.SignalRequest]) (*connect.Response[compassv1.SignalResponse], error) {
	return nil, errors.New("fakeGuestClient: Signal not used on the hermetic Start path")
}

func (c *fakeGuestClient) recorded() *compassv1.ProvisionRequest {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.provisioned
}

var _ guestVM = (*fakeGuestVM)(nil)
var _ compassv1internalconnect.GuestControlClient = (*fakeGuestClient)(nil)

// seamStart wires a MicroVMRuntime's launch + client seams to the supplied fakes
// and creates one session, returning the runtime, the created id, and the fakes.
// The Create records the zero-value default-deny script (unless spec overrides),
// which Start must then deliver verbatim.
func seamStart(t *testing.T, spec ContainerSpec, provErr error) (*MicroVMRuntime, ContainerID, *fakeGuestVM, *fakeGuestClient) {
	t.Helper()
	m := NewMicroVMRuntime(MicroVMConfig{RunRoot: shortRunRoot(t)})
	id, err := m.Create(t.Context(), spec)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	session, err := m.session(id)
	if err != nil {
		t.Fatalf("session after Create: %v", err)
	}
	vm := &fakeGuestVM{nonce: session.nonce}
	client := &fakeGuestClient{provErr: provErr}
	m.launchFunc = func(context.Context, microvm.BootConfig) (guestVM, error) { return vm, nil }
	m.newGuestClient = func(string, uint32) compassv1internalconnect.GuestControlClient { return client }
	return m, id, vm, client
}

// shortRunRoot returns a fresh, SHORT runroot outside the test-name-embedding
// t.TempDir() tree, so the per-session suffixed sockets (widest:
// <root>/microvm/<32-hex>/vsock.sock_1025) stay under the AF_UNIX sun_path
// budget Create now guards pre-boot (§(e)). A t.TempDir() root embeds the long
// test-function name and overflows the 107-byte cap — the very failure the
// guard reports. Mirrors e2eConfig's short-root rationale.
func shortRunRoot(t *testing.T) string {
	t.Helper()
	//nolint:usetesting // t.TempDir embeds the long test name, overflowing the AF_UNIX sun_path budget the pre-boot gateway-path guard enforces — the very failure a short fixed root prevents.
	root, err := os.MkdirTemp("", "cvm")
	if err != nil {
		t.Fatalf("creating short microvm runroot: %v", err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(root); err != nil {
			t.Errorf("removing microvm runroot %s: %v", root, err)
		}
	})
	return root
}

// TestCreateRecordsDefaultDenyScript pins §(e): Create records the zero-value
// EgressPolicy's full default-deny base ruleset on the session (never empty), so
// every ContainerSpec-created session boots armed even with no allowlist set.
func TestCreateRecordsDefaultDenyScript(t *testing.T) {
	m := NewMicroVMRuntime(MicroVMConfig{RunRoot: shortRunRoot(t)})
	id, err := m.Create(t.Context(), ContainerSpec{Name: "agent-1", UID: 1000})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	session, err := m.session(id)
	if err != nil {
		t.Fatalf("session after Create: %v", err)
	}
	if session.nftScript == "" {
		t.Fatal("Create recorded an empty nftScript; the zero-value policy must emit the default-deny base ruleset (§(e))")
	}
	if session.nftScript != (EgressPolicy{}).NftScript() {
		t.Errorf("recorded nftScript does not equal the zero-value default-deny script")
	}
}

// TestStartDeliversScriptVerbatim: via the launchFunc + newGuestClient seams,
// Start's Provision RPC carries the exact script Create recorded — the host→guest
// egress delivery contract (§(a)), hermetic with no real VMM or vsock dial.
func TestStartDeliversScriptVerbatim(t *testing.T) {
	spec := ContainerSpec{Name: "agent-1", UID: 1000, Egress: MustAllowEgress("github.com")}
	m, id, vm, client := seamStart(t, spec, nil)

	want := spec.Egress.NftScript()
	if err := m.Start(t.Context(), id); err != nil {
		t.Fatalf("Start: %v", err)
	}
	req := client.recorded()
	if req == nil {
		t.Fatal("Start did not issue a Provision RPC")
	}
	if req.GetNftScript() != want {
		t.Errorf("ProvisionRequest.NftScript = %q, want the recorded script %q", req.GetNftScript(), want)
	}
	if req.GetDefaultExecUid() != 1000 {
		t.Errorf("ProvisionRequest.DefaultExecUid = %d, want 1000", req.GetDefaultExecUid())
	}
	// The success path transfers handle ownership to the session and must NOT
	// run the deferred teardown (mirror of the error-path teardown assertion).
	session, err := m.session(id)
	if err != nil {
		t.Fatalf("session after successful Start: %v", err)
	}
	if session.vm == nil {
		t.Error("a successful Start must store the booted VM handle on the session")
	}
	if vm.wasShutdown() {
		t.Error("a successful Start must not tear the booted VM down")
	}
}

// TestStartProvisionErrorFailsAndTearsDown: a Provision error fails Start and the
// fake handle's Shutdown is called (the fail-closed teardown, §(b)/(d)) — the VM
// booted by launchFunc must not be left running when the arm/provision fails.
func TestStartProvisionErrorFailsAndTearsDown(t *testing.T) {
	provErr := connect.NewError(connect.CodeInternal, errors.New("arm failed"))
	spec := ContainerSpec{Name: "agent-1", UID: 1000}
	m, id, vm, _ := seamStart(t, spec, provErr)

	err := m.Start(t.Context(), id)
	if err == nil {
		t.Fatal("Start must fail when Provision errors")
	}
	if !vm.wasShutdown() {
		t.Error("a failed Provision must tear the booted VM down (Shutdown), fail-closed")
	}
	// The session must not retain a VM handle after a failed Start.
	session, sessErr := m.session(id)
	if sessErr != nil {
		t.Fatalf("session after failed Start: %v", sessErr)
	}
	if session.vm != nil {
		t.Error("a failed Start must not store the VM handle on the session")
	}
}

// TestStartNilGuestHandleFailsClosed pins the fail-closed guard: a launchFunc
// that returns (nil, nil) — a mis-written seam — makes Start return a clear
// error instead of nil-derefing the guestVM handle in the teardown defer or
// awaitHealthy. The production microvm.Launch adapter never returns (nil, nil),
// so this guards only against a broken test seam, but it must fail loud.
func TestStartNilGuestHandleFailsClosed(t *testing.T) {
	m := NewMicroVMRuntime(MicroVMConfig{RunRoot: shortRunRoot(t)})
	id, err := m.Create(t.Context(), ContainerSpec{Name: "agent-1", UID: 1000})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	// The (nil, nil) return is the exact mis-written-seam input the guard defends
	// against; nilnil is right in general but this test needs the pathological pair.
	m.launchFunc = func(context.Context, microvm.BootConfig) (guestVM, error) { return nil, nil } //nolint:nilnil // intentional: exercises the fail-closed nil-handle guard

	err = m.Start(t.Context(), id)
	if err == nil {
		t.Fatal("Start must fail when launchFunc returns a nil guest handle with no error")
	}
	if !strings.Contains(err.Error(), "nil guest") {
		t.Errorf("Start error = %q, want it to name the nil guest handle condition", err.Error())
	}
	// A nil handle must not be stored on the session.
	session, sessErr := m.session(id)
	if sessErr != nil {
		t.Fatalf("session after failed Start: %v", sessErr)
	}
	if session.vm != nil {
		t.Error("a failed Start must not store a VM handle on the session")
	}
}
