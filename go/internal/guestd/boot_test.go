//go:build linux

package guestd

// Hermetic suite for the boot orchestrator (run) — the fail-closed ordering gate
// (§(d)). It injects fake seams and asserts the invariant that makes a
// successful handshake proof of bringup: the serve step is reached ONLY after
// net AND mount both succeed; a failing net aborts before the mount runs; a
// failing mount aborts before the server starts. No VM, no sockets, no sleeps —
// the fakes record each step synchronously and serve is event-gated on
// t.Context().

import (
	"context"
	"errors"
	"slices"
	"syscall"
	"testing"
)

// recorder captures the order boot steps run in, so a test can assert both that
// a step ran and that it ran after its prerequisites.
type recorder struct {
	steps []string
}

func (r *recorder) mark(step string) { r.steps = append(r.steps, step) }

func (r *recorder) ran(step string) bool {
	return slices.Contains(r.steps, step)
}

// fakeNet is a netProvisioner that records its call and returns a canned error.
type fakeNet struct {
	rec *recorder
	err error
}

func (f *fakeNet) Provision(context.Context) error {
	f.rec.mark("net")
	return f.err
}

// fakeMount is a workspaceMounter that records its call and returns a canned
// error.
type fakeMount struct {
	rec *recorder
	err error
}

func (f *fakeMount) Mount() error {
	f.rec.mark("mount")
	return f.err
}

// newSteps wires a bootSteps whose api/readCmdline/net/mount stages record into
// rec and whose serve step signals reached (so a test can gate on serve being
// entered without polling the recorder across goroutines), captures the Health
// service it received, and then blocks until ctx is cancelled — serve as the
// terminal step, exactly as production does. served holds the service serve was
// handed, or nil if serve was never reached. cmdline is the kernel command line
// the readCmdline step returns; cmdlineErr, when non-nil, makes that step fail.
func newSteps(rec *recorder, apiErr, netErr, mountErr, cmdlineErr error, cmdline string) (bootSteps, **supervisor, chan struct{}) {
	served := new(*supervisor)
	reached := make(chan struct{})
	steps := bootSteps{
		mountAPIFilesystems: func() error {
			rec.mark("api")
			return apiErr
		},
		readCmdline: func() ([]byte, error) {
			rec.mark("cmdline")
			return []byte(cmdline), cmdlineErr
		},
		net:       &fakeNet{rec: rec, err: netErr},
		workspace: &fakeMount{rec: rec, err: mountErr},
		serve: func(ctx context.Context, _ uint32, svc *supervisor) error {
			rec.mark("serve")
			*served = svc
			close(reached)
			<-ctx.Done()
			return ctx.Err()
		},
		powerOff: func() error {
			rec.mark("poweroff")
			return nil
		},
	}
	return steps, served, reached
}

func TestBootServesHealthOnlyAfterNetAndMount(t *testing.T) {
	rec := &recorder{}
	steps, served, reached := newSteps(rec, nil, nil, nil, nil, "compass.vsock_port=1024")

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		done <- run(ctx, config{guestdVersion: "test-ver"}, steps)
	}()

	// Gate on serve being entered (it closes reached before blocking on ctx),
	// then cancel to let run return. The reached signal establishes
	// happens-before over every recorder write, so the assertions below read a
	// stable slice — no sleeps, no polling.
	<-reached
	cancel()

	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("run returned %v, want context.Canceled (clean shutdown after serve)", err)
	}

	// Order: api, cmdline, net, mount, serve — the cmdline read follows the API
	// mount (§(d) step 1: /proc must be mounted first), and serve is last.
	wantOrder := []string{"api", "cmdline", "net", "mount", "serve"}
	if len(rec.steps) != len(wantOrder) {
		t.Fatalf("boot ran steps %v, want %v", rec.steps, wantOrder)
	}
	for i, s := range wantOrder {
		if rec.steps[i] != s {
			t.Fatalf("boot step %d = %q, want %q (full order %v)", i, rec.steps[i], s, rec.steps)
		}
	}

	// The served Health service reflects the completed bringup state.
	svc := *served
	if svc == nil {
		t.Fatal("serve was reached but received no health service")
	}
	if !svc.netProvisioned || !svc.workspaceMounted {
		t.Fatalf("served health = {net:%v mount:%v}, want both true", svc.netProvisioned, svc.workspaceMounted)
	}
	if svc.version != "test-ver" {
		t.Fatalf("served health version = %q, want test-ver", svc.version)
	}
}

func TestBootFailsClosedOnNetError(t *testing.T) {
	rec := &recorder{}
	steps, served, _ := newSteps(rec, nil, errors.New("dhcp timed out"), nil, nil, "compass.vsock_port=1024")

	err := run(t.Context(), config{}, steps)
	if err == nil {
		t.Fatal("run with failing net returned nil, want fail-closed error")
	}
	// The mount and serve steps must NOT have run — a failing provisioner aborts
	// the boot before anything depends on the network.
	if rec.ran("mount") {
		t.Fatalf("mount ran after net failed; boot order was %v", rec.steps)
	}
	if rec.ran("serve") {
		t.Fatalf("serve ran after net failed; boot order was %v", rec.steps)
	}
	if *served != nil {
		t.Fatal("a health service was constructed despite net failure")
	}
}

func TestBootFailsClosedOnMountError(t *testing.T) {
	rec := &recorder{}
	steps, served, _ := newSteps(rec, nil, nil, errors.New("virtiofs mount failed"), nil, "compass.vsock_port=1024")

	err := run(t.Context(), config{}, steps)
	if err == nil {
		t.Fatal("run with failing mount returned nil, want fail-closed error")
	}
	if !rec.ran("net") {
		t.Fatalf("net must run before mount; boot order was %v", rec.steps)
	}
	// serve must NOT have run — a failing mount aborts before the server starts,
	// so Health is never served.
	if rec.ran("serve") {
		t.Fatalf("serve ran after mount failed; boot order was %v", rec.steps)
	}
	if *served != nil {
		t.Fatal("a health service was constructed despite mount failure")
	}
}

func TestBootFailsClosedOnAPIMountError(t *testing.T) {
	rec := &recorder{}
	steps, served, _ := newSteps(rec, errors.New("proc mount failed"), nil, nil, nil, "compass.vsock_port=1024")

	err := run(t.Context(), config{}, steps)
	if err == nil {
		t.Fatal("run with failing API mount returned nil, want fail-closed error")
	}
	// Nothing past the API mount may run.
	if rec.ran("net") || rec.ran("mount") || rec.ran("serve") {
		t.Fatalf("a step ran after API mount failed; boot order was %v", rec.steps)
	}
	if *served != nil {
		t.Fatal("a health service was constructed despite API mount failure")
	}
}

// TestBootReadsCmdlineOnlyAfterAPIMount is the regression for the ordering
// inversion where the kernel cmdline was read before /proc was mounted. The
// initramfs hands over a bare root, so /proc/cmdline is unreadable until
// mountAPIFilesystems runs (§(d) step 1). A readCmdline that hard-fails unless
// the api-mount step ran first proves the read is inside the sequence, after the
// mount — and that its failure fail-closes the boot before net/mount/serve.
func TestBootReadsCmdlineOnlyAfterAPIMount(t *testing.T) {
	rec := &recorder{}
	steps, served, reached := newSteps(rec, nil, nil, nil, nil, "compass.vsock_port=1024")
	// Replace readCmdline with one that hard-fails if /proc is not mounted yet,
	// modelling the bare-root reality: the read must follow the api mount. If the
	// old ordering (read before mount) regressed, this returns an error and the
	// boot fail-closes before serve — caught by the assertions below.
	steps.readCmdline = func() ([]byte, error) {
		rec.mark("cmdline")
		if !rec.ran("api") {
			return nil, errors.New("/proc/cmdline: no such file or directory")
		}
		return []byte("compass.vsock_port=1024"), nil
	}

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- run(ctx, config{}, steps) }()

	// Gate on serve being entered — reaching it proves the cmdline read (and
	// net+mount) all succeeded, which only happens if the read followed the
	// api mount. A read-before-mount regression fail-closes before serve, so
	// also watch done: surface that as a clean assertion instead of hanging on
	// reached until the package test timeout.
	select {
	case <-reached:
	case err := <-done:
		t.Fatalf("boot fail-closed before serve (cmdline read before api mount?); order %v: %v", rec.steps, err)
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("run returned %v, want context.Canceled after clean serve", err)
	}

	// The cmdline read must appear immediately after the api mount, before net.
	if len(rec.steps) < 2 || rec.steps[0] != "api" || rec.steps[1] != "cmdline" {
		t.Fatalf("cmdline was not read right after the api mount; order was %v", rec.steps)
	}
	if *served == nil {
		t.Fatal("serve was reached but received no health service")
	}
}

// TestBootFailsClosedOnCmdlineReadError asserts an unreadable cmdline aborts the
// boot right after the api mount — before net, mount, or serve — and constructs
// no Health service. This is the fail-closed half of the ordering fix: even with
// /proc mounted, a genuinely unreadable cmdline must not let the boot proceed.
func TestBootFailsClosedOnCmdlineReadError(t *testing.T) {
	rec := &recorder{}
	steps, served, _ := newSteps(rec, nil, nil, nil, errors.New("cmdline unreadable"), "")

	err := run(t.Context(), config{}, steps)
	if err == nil {
		t.Fatal("run with unreadable cmdline returned nil, want fail-closed error")
	}
	if !rec.ran("api") {
		t.Fatalf("api mount must run before the cmdline read; order was %v", rec.steps)
	}
	if rec.ran("net") || rec.ran("mount") || rec.ran("serve") {
		t.Fatalf("a step ran after the cmdline read failed; order was %v", rec.steps)
	}
	if *served != nil {
		t.Fatal("a health service was constructed despite a cmdline read failure")
	}
}

// TestBootFailsClosedOnBadCmdline asserts a cmdline missing compass.vsock_port
// aborts the boot after the api mount and before net — parseVsockPort's error is
// surfaced as a fail-closed boot error.
func TestBootFailsClosedOnBadCmdline(t *testing.T) {
	rec := &recorder{}
	steps, served, _ := newSteps(rec, nil, nil, nil, nil, "console=ttyS0")

	err := run(t.Context(), config{}, steps)
	if err == nil {
		t.Fatal("run with a portless cmdline returned nil, want fail-closed error")
	}
	if rec.ran("net") || rec.ran("mount") || rec.ran("serve") {
		t.Fatalf("a step ran after a bad cmdline; order was %v", rec.steps)
	}
	if *served != nil {
		t.Fatal("a health service was constructed despite a bad cmdline")
	}
}

// TestBootPowersOffOnRPCStop drives the full run() poweroff gate: a serve that
// simulates an RPC Stop (flags rpcStop + cancels serving via initiateStop) then
// drains clean must end in reboot(RB_POWER_OFF), since a bare PID-1 exit panics
// the kernel (§(d)). Exercises the integration TestRPCStopCancels... asserts in
// isolation.
func TestBootPowersOffOnRPCStop(t *testing.T) {
	rec := &recorder{}
	steps := bootSteps{
		mountAPIFilesystems: func() error { rec.mark("api"); return nil },
		readCmdline:         func() ([]byte, error) { rec.mark("cmdline"); return []byte("compass.vsock_port=1024"), nil },
		net:                 &fakeNet{rec: rec},
		workspace:           &fakeMount{rec: rec},
		serve: func(_ context.Context, _ uint32, svc *supervisor) error {
			rec.mark("serve")
			svc.initiateStop(syscall.SIGTERM) // RPC Stop: sets rpcStop, cancels serving
			return nil                        // clean drain
		},
		powerOff: func() error { rec.mark("poweroff"); return nil },
	}
	if err := run(t.Context(), config{}, steps); err != nil {
		t.Fatalf("run after a clean RPC stop = %v, want nil", err)
	}
	if !rec.ran("poweroff") {
		t.Fatalf("power-off did not run after an RPC stop; order was %v", rec.steps)
	}
}

// TestBootPowersOffOnRPCStopDespiteDrainError is the reliability contract: an
// RPC Stop whose graceful drain overran (serve returns a non-nil error, e.g. a
// child that ignored SIGTERM held Shutdown past its deadline) must STILL power
// off, so the VMM observes a real guest shutdown within the host's timeout
// instead of burning it to a hard kill. The poweroff is gated on rpcStop alone,
// not on a clean serveErr.
func TestBootPowersOffOnRPCStopDespiteDrainError(t *testing.T) {
	rec := &recorder{}
	drainErr := errors.New("shutdown deadline exceeded")
	steps := bootSteps{
		mountAPIFilesystems: func() error { rec.mark("api"); return nil },
		readCmdline:         func() ([]byte, error) { rec.mark("cmdline"); return []byte("compass.vsock_port=1024"), nil },
		net:                 &fakeNet{rec: rec},
		workspace:           &fakeMount{rec: rec},
		serve: func(_ context.Context, _ uint32, svc *supervisor) error {
			rec.mark("serve")
			svc.initiateStop(syscall.SIGTERM)
			return drainErr // drain overran
		},
		powerOff: func() error { rec.mark("poweroff"); return nil },
	}
	// run returns powerOff()'s result (nil), NOT the drain error — the guest
	// powered off, so main never falls through to a bare PID-1 exit.
	if err := run(t.Context(), config{}, steps); err != nil {
		t.Fatalf("run after an RPC stop with a drain error = %v, want nil (powered off)", err)
	}
	if !rec.ran("poweroff") {
		t.Fatalf("power-off was skipped on a drain error during an RPC stop; order was %v", rec.steps)
	}
}

// TestBootDoesNotPowerOffOnSignalCancel is the negative gate: a Unix-signal
// shutdown (ctx cancelled, no RPC Stop) must NOT power off — rpcStop is false,
// so run returns the serve error and lets main exit (the V2a path), and the
// host observes the dial failure.
func TestBootDoesNotPowerOffOnSignalCancel(t *testing.T) {
	rec := &recorder{}
	steps := bootSteps{
		mountAPIFilesystems: func() error { rec.mark("api"); return nil },
		readCmdline:         func() ([]byte, error) { rec.mark("cmdline"); return []byte("compass.vsock_port=1024"), nil },
		net:                 &fakeNet{rec: rec},
		workspace:           &fakeMount{rec: rec},
		serve: func(ctx context.Context, _ uint32, _ *supervisor) error {
			rec.mark("serve")
			<-ctx.Done()
			return ctx.Err()
		},
		powerOff: func() error { rec.mark("poweroff"); return nil },
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel() // simulate a Unix-signal shutdown: serve returns on ctx, rpcStop false
	err := run(ctx, config{}, steps)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("run after a signal cancel = %v, want context.Canceled", err)
	}
	if rec.ran("poweroff") {
		t.Fatalf("power-off ran on a plain signal cancel (rpcStop false); order was %v", rec.steps)
	}
}
