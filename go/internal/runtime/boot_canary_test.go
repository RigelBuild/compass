//go:build unix

package runtime

// The hermetic BootCanary suite: it drives BootCanary behind the launchFunc +
// newGuestClient seams so no real cloud-hypervisor boots and no real vsock is
// dialed, proving the canary's Create→Start→Exec→Remove sequencing, report
// assembly, deadline derivation, teardown-on-failure, and reserved naming with
// no KVM (record §(e)/(f)/(g)). It is //go:build unix because the seams and the
// guestVM interface it fakes are unix-only.
//
// KEY SEAM SUBTLETY: BootCanary calls Create INTERNALLY, minting a fresh boot
// nonce the test cannot pre-read. So the fake launchFunc parses the hex nonce out
// of cfg.Cmdline ("compass.boot_nonce="+hex) and hands it to a fake guestVM whose
// Health echoes THAT nonce — otherwise awaitHealthy's identity binding fails. The
// fake guestClient's Exec echoes the command's argument back on stdout with exit
// 0, so BootCanary's echo-nonce round-trip check passes without a real guest.

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"

	compassv1 "github.com/RigelBuild/compass/go/internal/gen/compass/v1"
	"github.com/RigelBuild/compass/go/internal/gen/compass/v1/compassv1internalconnect"
	"github.com/RigelBuild/compass/go/internal/runtime/microvm"
)

// canaryFakeVM is a guestVM handle for the canary path: Health answers ready and
// echoes the boot nonce the fake launchFunc decoded from cfg.Cmdline (so
// awaitHealthy's identity binding passes), PSS returns a configurable non-empty
// map (so GuestRSSBytes is assertable), Shutdown is recorded (the teardown
// assertion) and can be forced to fail via shutdownErr (the teardown-error-join
// assertion).
type canaryFakeVM struct {
	nonce       []byte
	pss         map[string]int64
	pssErr      error
	shutdownErr error
	mu          sync.Mutex
	shutdown    bool
}

func (f *canaryFakeVM) Health(context.Context) (*compassv1.HealthResponse, error) {
	return &compassv1.HealthResponse{
		NetProvisioned:   true,
		WorkspaceMounted: true,
		BootNonce:        f.nonce,
	}, nil
}

func (f *canaryFakeVM) Shutdown(context.Context) error {
	f.mu.Lock()
	f.shutdown = true
	f.mu.Unlock()
	return f.shutdownErr
}

func (f *canaryFakeVM) WaitVMMExit(_ time.Duration) bool { return true }

func (f *canaryFakeVM) PSS() (map[string]int64, error) { return f.pss, f.pssErr }

func (f *canaryFakeVM) wasShutdown() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.shutdown
}

var _ guestVM = (*canaryFakeVM)(nil)

// canaryLaunchRecorder is the fake launchFunc: it decodes the boot nonce from
// cfg.Cmdline, builds a canaryFakeVM echoing it, and records the launched VMs +
// the deadline the launch ctx carried (for the ctx-derivation assertions). A
// non-nil launchErr makes launch fail (the Start-failure case).
type canaryLaunchRecorder struct {
	mu            sync.Mutex
	pss           map[string]int64
	pssErr        error
	shutdownErr   error
	launchErr     error
	vms           []*canaryFakeVM
	calls         int
	lastDeadline  time.Time
	lastHasDeadln bool
}

func (r *canaryLaunchRecorder) launch(ctx context.Context, cfg microvm.BootConfig) (guestVM, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls++
	deadline, ok := ctx.Deadline()
	r.lastDeadline = deadline
	r.lastHasDeadln = ok
	if r.launchErr != nil {
		return nil, r.launchErr
	}
	nonce, err := parseBootNonce(cfg.Cmdline)
	if err != nil {
		return nil, err
	}
	vm := &canaryFakeVM{nonce: nonce, pss: r.pss, pssErr: r.pssErr, shutdownErr: r.shutdownErr}
	r.vms = append(r.vms, vm)
	return vm, nil
}

func (r *canaryLaunchRecorder) snapshot() (calls int, deadline time.Time, hasDeadline bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls, r.lastDeadline, r.lastHasDeadln
}

// parseBootNonce decodes the raw boot nonce out of the cmdline bootConfig set
// ("compass.boot_nonce="+hex), the seam-level value the fake launchFunc sees
// before Launch would append its own console/vsock params.
func parseBootNonce(cmdline string) ([]byte, error) {
	const prefix = "compass.boot_nonce="
	if !strings.HasPrefix(cmdline, prefix) {
		return nil, fmt.Errorf("canary fake: cmdline %q missing %q prefix", cmdline, prefix)
	}
	return hex.DecodeString(strings.TrimPrefix(cmdline, prefix))
}

// canaryFakeClient is a GuestControlClient for the canary path: Provision
// succeeds, and Exec echoes the command's argument back on stdout with a
// configurable exit code (default 0) unless execErr forces a transport failure.
// stdout, when non-nil, overrides the echoed output verbatim so a test can drive
// the exit-0-but-wrong-stdout case (the nonce-mismatch branch).
type canaryFakeClient struct {
	execErr  error
	stdout   *string
	exitCode int32
}

func (c *canaryFakeClient) Provision(context.Context, *connect.Request[compassv1.ProvisionRequest]) (*connect.Response[compassv1.ProvisionResponse], error) {
	return connect.NewResponse(&compassv1.ProvisionResponse{}), nil
}

func (c *canaryFakeClient) Exec(_ context.Context, req *connect.Request[compassv1.ExecRequest]) (*connect.Response[compassv1.ExecResponse], error) {
	if c.execErr != nil {
		return nil, c.execErr
	}
	// Echo the argument(s) after the command name, mirroring `echo <nonce>`, so
	// BootCanary's nonce-round-trip check passes — unless stdout overrides it.
	cmd := req.Msg.GetCommand()
	var out string
	if len(cmd) > 1 {
		out = strings.Join(cmd[1:], " ")
	}
	if c.stdout != nil {
		out = *c.stdout
	}
	return connect.NewResponse(&compassv1.ExecResponse{
		Stdout:   []byte(out + "\n"),
		ExitCode: c.exitCode,
	}), nil
}

func (c *canaryFakeClient) Health(context.Context, *connect.Request[compassv1.HealthRequest]) (*connect.Response[compassv1.HealthResponse], error) {
	return nil, errors.New("canaryFakeClient: Health not used on the canary path")
}

func (c *canaryFakeClient) ExecStream(context.Context) *connect.BidiStreamForClient[compassv1.ExecStreamRequest, compassv1.ExecStreamResponse] {
	return nil
}

func (c *canaryFakeClient) Signal(context.Context, *connect.Request[compassv1.SignalRequest]) (*connect.Response[compassv1.SignalResponse], error) {
	return nil, errors.New("canaryFakeClient: Signal not used on the canary path")
}

var _ compassv1internalconnect.GuestControlClient = (*canaryFakeClient)(nil)

// seamCanary wires a MicroVMRuntime's launch + client seams to the canary fakes
// over a short runroot, returning the runtime, the launch recorder, and the
// client so a test can tune failures.
func seamCanary(t *testing.T, pss map[string]int64) (*MicroVMRuntime, *canaryLaunchRecorder, *canaryFakeClient) {
	t.Helper()
	m := NewMicroVMRuntime(MicroVMConfig{RunRoot: shortRunRoot(t)})
	rec := &canaryLaunchRecorder{pss: pss}
	client := &canaryFakeClient{}
	m.launchFunc = rec.launch
	m.newGuestClient = func(string, uint32) compassv1internalconnect.GuestControlClient { return client }
	return m, rec, client
}

// sessionCount reads the live session-table size under the lock.
func sessionCount(m *MicroVMRuntime) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.sessions)
}

// canaryTempDirs is the set of leftover throwaway-workspace dirs BootCanary
// creates under os.TempDir(), so a test can assert it removed its own.
func canaryTempDirs(t *testing.T) map[string]bool {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(os.TempDir(), canaryNamePrefix+"*"))
	if err != nil {
		t.Fatalf("globbing canary temp dirs: %v", err)
	}
	set := make(map[string]bool, len(matches))
	for _, p := range matches {
		set[p] = true
	}
	return set
}

// assertNoTempLeak fails if any canary workspace dir exists now that did not
// before the call — proving BootCanary deleted the throwaway dir it minted.
func assertNoTempLeak(t *testing.T, before map[string]bool) {
	t.Helper()
	for p := range canaryTempDirs(t) {
		if !before[p] {
			t.Errorf("canary leaked a throwaway workspace dir: %s", p)
		}
	}
}

// TestBootCanarySequencing pins the happy path: Create→Start→Exec→Remove run in
// order, the report is populated (BootLatency > 0 from the Start wall time,
// GuestRSSBytes summed from PSS kB→bytes), the launched VM is torn down, the
// session table is empty after, and no throwaway workspace leaks (record §(e)).
func TestBootCanarySequencing(t *testing.T) {
	before := canaryTempDirs(t)
	m, rec, _ := seamCanary(t, map[string]int64{"cloud-hypervisor": 100, "virtiofsd": 50})

	report, err := m.BootCanary(t.Context())
	if err != nil {
		t.Fatalf("BootCanary = %v, want nil", err)
	}
	if report.BootLatency <= 0 {
		t.Errorf("BootLatency = %v, want > 0", report.BootLatency)
	}
	// (100 + 50) kB * 1024 = 153600 bytes.
	if want := int64((100 + 50) * 1024); report.GuestRSSBytes != want {
		t.Errorf("GuestRSSBytes = %d, want %d (PSS kB summed and converted to bytes)", report.GuestRSSBytes, want)
	}
	if calls, _, _ := rec.snapshot(); calls != 1 {
		t.Errorf("launch called %d times, want 1", calls)
	}
	if len(rec.vms) != 1 || !rec.vms[0].wasShutdown() {
		t.Error("canary VM was not shut down on teardown")
	}
	if n := sessionCount(m); n != 0 {
		t.Errorf("session table has %d entries after BootCanary, want 0", n)
	}
	assertNoTempLeak(t, before)
}

// TestBootCanaryStartFailureLeaksNothing: a launchFunc error fails BootCanary and
// leaves no session in the table and no throwaway workspace on disk — Create's
// entry is torn down by the always-run Remove (record §(e)/(f)).
func TestBootCanaryStartFailureLeaksNothing(t *testing.T) {
	before := canaryTempDirs(t)
	m, rec, _ := seamCanary(t, nil)
	rec.launchErr = errors.New("boom: launch refused")

	_, err := m.BootCanary(t.Context())
	if err == nil {
		t.Fatal("BootCanary = nil, want the launch error")
	}
	if !strings.Contains(err.Error(), "boom") {
		t.Errorf("error %q does not carry the launch failure", err)
	}
	if n := sessionCount(m); n != 0 {
		t.Errorf("session table has %d entries after a failed BootCanary, want 0", n)
	}
	assertNoTempLeak(t, before)
}

// TestBootCanaryEchoFailureStillTearsDown: an echo-exec transport failure fails
// BootCanary, but the always-run Remove still shuts the VM down, empties the
// session table, and deletes the throwaway workspace (record §(e)).
func TestBootCanaryEchoFailureStillTearsDown(t *testing.T) {
	before := canaryTempDirs(t)
	m, rec, client := seamCanary(t, nil)
	client.execErr = errors.New("boom: exec refused")

	_, err := m.BootCanary(t.Context())
	if err == nil {
		t.Fatal("BootCanary = nil, want the echo-exec error")
	}
	if len(rec.vms) != 1 || !rec.vms[0].wasShutdown() {
		t.Error("canary VM was not shut down after the echo failure")
	}
	if n := sessionCount(m); n != 0 {
		t.Errorf("session table has %d entries after a failed BootCanary, want 0", n)
	}
	assertNoTempLeak(t, before)
}

// TestBootCanaryNonZeroExitFails: an echo exec that returns a non-zero exit code
// (a successful call, not a transport error) is still a canary failure — the
// canary is a health gate, so a bad exit fails it.
func TestBootCanaryNonZeroExitFails(t *testing.T) {
	before := canaryTempDirs(t)
	m, rec, client := seamCanary(t, nil)
	client.exitCode = 3

	_, err := m.BootCanary(t.Context())
	if err == nil {
		t.Fatal("BootCanary = nil, want a non-zero-exit failure")
	}
	if !strings.Contains(err.Error(), "exited 3") {
		t.Errorf("error %q does not name the non-zero exit", err)
	}
	if len(rec.vms) != 1 || !rec.vms[0].wasShutdown() {
		t.Error("canary VM was not shut down after the non-zero-exit failure")
	}
	if n := sessionCount(m); n != 0 {
		t.Errorf("session table has %d entries after a failed BootCanary, want 0", n)
	}
	assertNoTempLeak(t, before)
}

// TestBootCanaryNonceMismatchFails: an echo exec that returns exit 0 but stdout
// NOT containing the boot nonce is a canary failure. Exit 0 alone only proves a
// call returned; the nonce round-trip is the sole assertion that the guest really
// ran OUR command and returned OUR data — the "exec gate" leg of the whole-chain
// claim (record §(e)). A regression dropping this check (or a guestd returning
// exit 0 with stubbed/empty stdout) would let a broken exec path pass the startup
// canary as a healthy boot, the exact fail-open the gate prevents; this test
// breaks on that. The always-run teardown must still fire.
func TestBootCanaryNonceMismatchFails(t *testing.T) {
	before := canaryTempDirs(t)
	m, rec, client := seamCanary(t, nil)
	wrong := "not-the-nonce"
	client.stdout = &wrong // exit 0, but stdout never carries the minted nonce

	_, err := m.BootCanary(t.Context())
	if err == nil {
		t.Fatal("BootCanary = nil, want a nonce-mismatch failure")
	}
	if !strings.Contains(err.Error(), "does not contain the nonce") {
		t.Errorf("error %q does not name the nonce mismatch", err)
	}
	if len(rec.vms) != 1 || !rec.vms[0].wasShutdown() {
		t.Error("canary VM was not shut down after the nonce-mismatch failure")
	}
	if n := sessionCount(m); n != 0 {
		t.Errorf("session table has %d entries after a failed BootCanary, want 0", n)
	}
	assertNoTempLeak(t, before)
}

// TestBootCanaryPSSErrorNonFatal pins the one fail-open seam on an otherwise
// fail-closed startup gate (record §(e)/OQ-10): a PSS read error is telemetry,
// never fatal — the canary still succeeds with GuestRSSBytes == 0 and the
// session still tears down. A regression flipping this branch to fatal would
// make the canary spuriously refuse Runner startup on a host with an unreadable
// smaps_rollup, and this test breaks on that flip.
func TestBootCanaryPSSErrorNonFatal(t *testing.T) {
	before := canaryTempDirs(t)
	m, rec, _ := seamCanary(t, nil)
	rec.pssErr = errors.New("boom: smaps_rollup unreadable")

	report, err := m.BootCanary(t.Context())
	if err != nil {
		t.Fatalf("BootCanary = %v, want nil (a PSS read error is best-effort telemetry, never fatal)", err)
	}
	if report.GuestRSSBytes != 0 {
		t.Errorf("GuestRSSBytes = %d, want 0 on a PSS read error", report.GuestRSSBytes)
	}
	if len(rec.vms) != 1 || !rec.vms[0].wasShutdown() {
		t.Error("canary VM was not shut down after a PSS read error")
	}
	if n := sessionCount(m); n != 0 {
		t.Errorf("session table has %d entries after BootCanary, want 0", n)
	}
	assertNoTempLeak(t, before)
}

// TestBootCanaryPartialPSSStillReported pins the OTHER half of the PSS contract:
// the real VM.PSS() (microvm/launch.go) returns a PARTIAL map ALONGSIDE a non-nil
// joined error when some children read and some do not — that is its normal shape,
// not an edge case. BootCanary must sum what it could read rather than discarding
// the partial map on any error. A regression zeroing the map inside the error
// branch (a plausible "tidy the error path" refactor) silently drops real
// telemetry, and this test breaks on that (record §(e)/OQ-10).
func TestBootCanaryPartialPSSStillReported(t *testing.T) {
	before := canaryTempDirs(t)
	m, rec, _ := seamCanary(t, map[string]int64{"cloud-hypervisor": 100, "virtiofsd": 50})
	rec.pssErr = errors.New("boom: one child's smaps_rollup unreadable")

	report, err := m.BootCanary(t.Context())
	if err != nil {
		t.Fatalf("BootCanary = %v, want nil (a partial PSS read is best-effort telemetry, never fatal)", err)
	}
	// (100 + 50) kB * 1024 = 153600 bytes — the partial map is still summed
	// despite the accompanying error.
	if want := int64((100 + 50) * 1024); report.GuestRSSBytes != want {
		t.Errorf("GuestRSSBytes = %d, want %d (a partial PSS map must still be reported)", report.GuestRSSBytes, want)
	}
	if len(rec.vms) != 1 || !rec.vms[0].wasShutdown() {
		t.Error("canary VM was not shut down after a partial PSS read")
	}
	if n := sessionCount(m); n != 0 {
		t.Errorf("session table has %d entries after BootCanary, want 0", n)
	}
	assertNoTempLeak(t, before)
}

// TestBootCanaryTeardownErrorJoined pins the documented teardown-error-join
// contract (BootCanary doc, record §(e)/(f)): the always-run Remove teardown's
// error is joined into BootCanary's return, never discarded. On an OTHERWISE
// successful canary (boot + echo + nonce all pass), a failing guest Shutdown must
// still surface as a non-nil BootCanary error carrying the teardown failure. The
// existing failure-path tests only assert teardown RAN (wasShutdown); none proves
// a FAILING teardown reaches the caller. Because BootCanary gates Runner startup,
// a silently-swallowed Remove failure would report a canary that leaked a live
// VMM+virtiofsd as a clean boot — the fail-closed posture inverted on the exact
// path the gate protects. The named-return + errors.Join idiom is the kind a
// "tidy the error path" refactor flattens to a plain defer; this test breaks on
// that. (The sibling throwaway-workspace RemoveAll join at BootCanary's other
// defer uses the identical named-return errors.Join mechanism this proves, but
// cannot be reddened deterministically without a production temp-dir seam —
// os.TempDir() is not writable to force RemoveAll to fail regardless of uid — so
// it rides the shared mechanism proof rather than a root-sensitive test.)
func TestBootCanaryTeardownErrorJoined(t *testing.T) {
	before := canaryTempDirs(t)
	m, rec, _ := seamCanary(t, map[string]int64{"cloud-hypervisor": 100})
	rec.shutdownErr = errors.New("boom: shutdown refused")

	report, err := m.BootCanary(t.Context())
	if err == nil {
		t.Fatal("BootCanary = nil, want a non-nil error carrying the teardown failure")
	}
	if !strings.Contains(err.Error(), "boom: shutdown refused") {
		t.Errorf("BootCanary error = %v, want it to carry the teardown shutdown failure", err)
	}
	// The boot chain itself succeeded, so the report is still assembled from what
	// ran before teardown — the error is the teardown's, not the boot's.
	if want := int64(100 * 1024); report.GuestRSSBytes != want {
		t.Errorf("GuestRSSBytes = %d, want %d (the boot chain succeeded; only teardown failed)", report.GuestRSSBytes, want)
	}
	// Teardown still RAN despite erroring: the VM's Shutdown was invoked and the
	// session table drained (Remove deletes the entry before Shutdown).
	if len(rec.vms) != 1 || !rec.vms[0].wasShutdown() {
		t.Error("canary VM Shutdown was not invoked during teardown")
	}
	if n := sessionCount(m); n != 0 {
		t.Errorf("session table has %d entries after BootCanary, want 0", n)
	}
	assertNoTempLeak(t, before)
}

// TestBootCanaryDerivesDeadlineWhenCallerHasNone: with a deadline-less caller
// ctx, BootCanary derives the internal canaryDeadline bound and threads it into
// Start (so a wedged boot cannot hang Runner startup) — the launch ctx carries a
// deadline ~canaryDeadline out (record §(f)).
func TestBootCanaryDerivesDeadlineWhenCallerHasNone(t *testing.T) {
	m, rec, _ := seamCanary(t, nil)

	if _, err := m.BootCanary(t.Context()); err != nil {
		t.Fatalf("BootCanary = %v, want nil", err)
	}
	_, deadline, ok := rec.snapshot()
	if !ok {
		t.Fatal("launch ctx carried no deadline; BootCanary did not derive the canary bound")
	}
	remaining := time.Until(deadline)
	if remaining <= 0 || remaining > canaryDeadline {
		t.Errorf("derived deadline %v out, want within (0, %v]", remaining, canaryDeadline)
	}
}

// TestBootCanaryHonorsCallerDeadline: a caller ctx WITH a deadline is used as-is
// — BootCanary does NOT re-derive a fresh canaryDeadline over it, so the launch
// ctx carries the caller's shorter deadline (record §(f)).
func TestBootCanaryHonorsCallerDeadline(t *testing.T) {
	m, rec, _ := seamCanary(t, nil)
	callerBound := 5 * time.Second
	ctx, cancel := context.WithTimeout(t.Context(), callerBound)
	defer cancel()

	if _, err := m.BootCanary(ctx); err != nil {
		t.Fatalf("BootCanary = %v, want nil", err)
	}
	_, deadline, ok := rec.snapshot()
	if !ok {
		t.Fatal("launch ctx carried no deadline")
	}
	// The caller's 5s deadline must be honored as-is, not widened to the 90s
	// canaryDeadline: remaining must be well under canaryDeadline.
	if remaining := time.Until(deadline); remaining > callerBound {
		t.Errorf("launch deadline %v out exceeds the caller's %v bound; BootCanary re-derived instead of honoring the caller", remaining, callerBound)
	}
}

// TestBootCanaryHonorsLongerCallerDeadline: a caller ctx whose deadline EXCEEDS
// canaryDeadline is used as-is — BootCanary does NOT clamp it down to the 90s
// canary bound. This is the case the `if _, ok := ctx.Deadline(); !ok` guard
// actually protects: a shorter caller deadline is enforced by context.WithTimeout
// regardless of the guard, so only a longer one distinguishes honoring the caller
// from re-deriving. A regression dropping the guard (always re-deriving
// canaryDeadline) would clamp the caller's longer deadline, and this test breaks
// on that (record §(f)).
func TestBootCanaryHonorsLongerCallerDeadline(t *testing.T) {
	m, rec, _ := seamCanary(t, nil)
	callerBound := 10 * time.Minute
	ctx, cancel := context.WithTimeout(t.Context(), callerBound)
	defer cancel()

	if _, err := m.BootCanary(ctx); err != nil {
		t.Fatalf("BootCanary = %v, want nil", err)
	}
	_, deadline, ok := rec.snapshot()
	if !ok {
		t.Fatal("launch ctx carried no deadline")
	}
	// The caller's 10m deadline must pass through, not be clamped to the 90s
	// canaryDeadline: remaining must be materially greater than canaryDeadline.
	if remaining := time.Until(deadline); remaining <= canaryDeadline {
		t.Errorf("launch deadline %v out was clamped to the canary bound; BootCanary re-derived instead of honoring the caller's longer %v deadline", remaining, callerBound)
	}
}

// TestCanaryNameReserved pins the naming contract (record §(e)/(g)): a minted
// canary name carries the reserved compass-canary- prefix, never the agent
// session prefix, so a canary can never collide with a real agent session; two
// mints differ.
func TestCanaryNameReserved(t *testing.T) {
	name, err := canaryName()
	if err != nil {
		t.Fatalf("canaryName = %v, want nil", err)
	}
	if !strings.HasPrefix(name, canaryNamePrefix) {
		t.Errorf("canary name %q lacks the reserved %q prefix", name, canaryNamePrefix)
	}
	// The agent session prefix is "compass-agent-" (runner.AgentContainerNamePrefix,
	// not imported here to avoid the runner→runtime import cycle).
	const agentSessionPrefix = "compass-agent-"
	if strings.HasPrefix(name, agentSessionPrefix) {
		t.Errorf("canary name %q collides with the agent session prefix %q", name, agentSessionPrefix)
	}
	other, err := canaryName()
	if err != nil {
		t.Fatalf("canaryName (second) = %v, want nil", err)
	}
	if name == other {
		t.Errorf("two canary names collided: %q", name)
	}
}
