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

// newSteps wires a bootSteps whose api/net/mount stages record into rec and
// whose serve step signals reached (so a test can gate on serve being entered
// without polling the recorder across goroutines), captures the Health service
// it received, and then blocks until ctx is cancelled — serve as the terminal
// step, exactly as production does. served holds the service serve was handed,
// or nil if serve was never reached.
func newSteps(rec *recorder, apiErr, netErr, mountErr error) (bootSteps, **healthService, chan struct{}) {
	served := new(*healthService)
	reached := make(chan struct{})
	steps := bootSteps{
		mountAPIFilesystems: func() error {
			rec.mark("api")
			return apiErr
		},
		net:       &fakeNet{rec: rec, err: netErr},
		workspace: &fakeMount{rec: rec, err: mountErr},
		serve: func(ctx context.Context, _ uint32, svc *healthService) error {
			rec.mark("serve")
			*served = svc
			close(reached)
			<-ctx.Done()
			return ctx.Err()
		},
	}
	return steps, served, reached
}

func TestBootServesHealthOnlyAfterNetAndMount(t *testing.T) {
	rec := &recorder{}
	steps, served, reached := newSteps(rec, nil, nil, nil)

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		done <- run(ctx, config{vsockPort: 1024, guestdVersion: "test-ver"}, steps)
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

	// Order: api, net, mount, serve — serve strictly last.
	wantOrder := []string{"api", "net", "mount", "serve"}
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
	steps, served, _ := newSteps(rec, nil, errors.New("dhcp timed out"), nil)

	err := run(t.Context(), config{vsockPort: 1024}, steps)
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
	steps, served, _ := newSteps(rec, nil, nil, errors.New("virtiofs mount failed"))

	err := run(t.Context(), config{vsockPort: 1024}, steps)
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
	steps, served, _ := newSteps(rec, errors.New("proc mount failed"), nil, nil)

	err := run(t.Context(), config{vsockPort: 1024}, steps)
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
