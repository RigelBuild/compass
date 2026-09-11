package preflight

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// okDeps returns a Deps whose every injected effect passes, for the given host
// GOOS. Tests override individual fields to drive one failure at a time.
func okDeps(goos string) Deps {
	return Deps{
		GOOS:           goos,
		PodmanRootless: func(context.Context) error { return nil },
		PodmanVersion:  func(context.Context) error { return nil },
		ImagePresent:   func(context.Context, string) error { return nil },
	}
}

var testParams = Params{
	AgentImage: "ghcr.io/rigelbuild/compass-agent:latest",
}

// resultByName finds a check result by its Name.
func resultByName(t *testing.T, rs []Result, name string) Result {
	t.Helper()
	for _, r := range rs {
		if r.Name == name {
			return r
		}
	}
	t.Fatalf("no result named %q in %v", name, rs)
	return Result{}
}

func TestRunAllPass(t *testing.T) {
	ctx := context.Background()
	rs := okDeps("linux").Run(ctx, testParams)

	// linux: os, podman, podman-version, image (no machine check on linux).
	if len(rs) != 4 {
		t.Fatalf("want 4 results on linux, got %d: %v", len(rs), rs)
	}
	for _, r := range rs {
		if !r.OK {
			t.Errorf("check %q failed: %s", r.Name, r.Detail)
		}
	}
	if err := rs.Err(); err != nil {
		t.Errorf("want nil error when all pass, got %v", err)
	}
}

func TestRunWrongGOOS(t *testing.T) {
	ctx := context.Background()
	rs := okDeps("windows").Run(ctx, testParams)

	got := resultByName(t, rs, checkOS)
	if got.OK {
		t.Fatal("os check should fail on windows")
	}
	for _, tok := range []string{"linux", "darwin", "windows"} {
		if !strings.Contains(got.Detail, tok) {
			t.Errorf("os detail %q missing token %q", got.Detail, tok)
		}
	}
	assertErrContains(t, rs.Err(), "windows")
}

// TestRunDarwinPasses: darwin is a supported OS. With a ready machine adapter
// wired, all checks pass and the machine check appears in the results.
func TestRunDarwinPasses(t *testing.T) {
	ctx := context.Background()
	d := okDeps("darwin")
	d.MachineReady = func(context.Context) error { return nil }
	rs := d.Run(ctx, testParams)

	if resultByName(t, rs, checkOS).OK != true {
		t.Error("os check should pass on darwin")
	}
	if got := resultByName(t, rs, checkMachine); !got.OK {
		t.Errorf("machine check should pass with a ready adapter: %s", got.Detail)
	}
	if err := rs.Err(); err != nil {
		t.Errorf("want nil error on darwin all-pass, got %v", err)
	}
}

// TestRunMachineCheckAbsentOnLinux: linux has no podman machine, so the machine
// check never appears even if a MachineReady adapter is somehow wired.
func TestRunMachineCheckAbsentOnLinux(t *testing.T) {
	ctx := context.Background()
	d := okDeps("linux")
	d.MachineReady = func(context.Context) error { return errors.New("should not be called on linux") }
	rs := d.Run(ctx, testParams)

	for _, r := range rs {
		if r.Name == checkMachine {
			t.Fatalf("machine check present on linux: %v", rs)
		}
	}
}

// TestRunMachineNotReadyOnDarwin: a darwin machine that is not ready fails the
// machine check and folds into the aggregated error.
func TestRunMachineNotReadyOnDarwin(t *testing.T) {
	ctx := context.Background()
	d := okDeps("darwin")
	d.MachineReady = func(context.Context) error { return errors.New("machine stopped") }
	rs := d.Run(ctx, testParams)

	got := resultByName(t, rs, checkMachine)
	if got.OK {
		t.Fatal("machine check should fail when the machine is not ready")
	}
	if !strings.Contains(got.Detail, "machine stopped") {
		t.Errorf("machine detail %q missing probe error", got.Detail)
	}
	assertErrContains(t, rs.Err(), "machine stopped")
}

// TestRunMachineCheckFailsOnDarwinWithoutAdapter: a nil MachineReady on darwin
// is a wiring defect, and the check FAILS rather than vanishing. Omitting it
// would hand a Mac with no podman machine an all-green preflight followed by an
// undiagnosable downstream failure — the silent skip this behavior removes.
func TestRunMachineCheckFailsOnDarwinWithoutAdapter(t *testing.T) {
	ctx := context.Background()
	rs := okDeps("darwin").Run(ctx, testParams)

	got := resultByName(t, rs, checkMachine)
	if got.OK {
		t.Fatal("machine check passed on darwin with no adapter wired; it must fail, never be skipped")
	}
	if !strings.Contains(got.Detail, "no podman machine adapter is wired") {
		t.Errorf("machine detail %q does not name the missing adapter", got.Detail)
	}
	assertErrContains(t, rs.Err(), "no podman machine adapter is wired")
}

// TestRunMachineCheckAlwaysPresentOnDarwin: the machine check is present in the
// results on darwin for EVERY adapter state — ready, failing, or unwired. The
// regression this pins is the check being absent from a darwin run, which reads
// as a pass to any caller that classifies by result.
func TestRunMachineCheckAlwaysPresentOnDarwin(t *testing.T) {
	ctx := context.Background()
	adapters := map[string]func(context.Context) error{
		"ready":   func(context.Context) error { return nil },
		"failing": func(context.Context) error { return errors.New("machine down") },
		"unwired": nil,
	}
	for name, adapter := range adapters {
		t.Run(name, func(t *testing.T) {
			d := okDeps("darwin")
			d.MachineReady = adapter
			rs := d.Run(ctx, testParams)
			// resultByName t.Fatalf's when the check is missing, which IS the
			// assertion: an absent machine check fails this test.
			resultByName(t, rs, checkMachine)
		})
	}
}

func TestRunPodmanProbeFails(t *testing.T) {
	ctx := context.Background()
	d := okDeps("linux")
	d.PodmanRootless = func(context.Context) error {
		return errors.New("podman socket not found")
	}
	rs := d.Run(ctx, testParams)

	got := resultByName(t, rs, checkPodman)
	if got.OK {
		t.Fatal("podman check should fail when probe errors")
	}
	if !strings.Contains(got.Detail, "podman socket not found") {
		t.Errorf("podman detail %q missing probe error", got.Detail)
	}
	assertErrContains(t, rs.Err(), "rootless podman")
}

// TestRunPodmanVersionGate is the delta-4 gate: a below-floor podman fails FATAL
// carrying the "podman N.N or newer" copy verbatim; at/above the floor passes.
// The version probe is injected, so the test is hermetic (no real podman). The
// probe's error copy is what VerifyUsernsRemapSupport emits, reused verbatim by
// the check.
func TestRunPodmanVersionGate(t *testing.T) {
	// The exact copy runtime.(*PodmanCLI).VerifyUsernsRemapSupport emits below
	// the floor, which the injected probe surfaces and the check carries through.
	const belowFloorCopy = "podman 4.3 or newer is required (the container userns " +
		"remap --userns=keep-id:uid=,gid= is a 4.3+ option), but this host has podman 3.4.4"

	tests := []struct {
		name      string
		probe     func(context.Context) error
		wantOK    bool
		wantToken string
	}{
		{
			name:      "below floor is refused",
			probe:     func(context.Context) error { return errors.New(belowFloorCopy) },
			wantOK:    false,
			wantToken: "podman 4.3 or newer is required",
		},
		{
			name:   "at or above floor passes",
			probe:  func(context.Context) error { return nil },
			wantOK: true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			d := okDeps("linux")
			d.PodmanVersion = tc.probe
			rs := d.Run(ctx, testParams)

			got := resultByName(t, rs, checkPodmanVersion)
			if got.OK != tc.wantOK {
				t.Fatalf("podman-version check OK = %v, want %v (detail %q)", got.OK, tc.wantOK, got.Detail)
			}
			if tc.wantOK {
				if err := rs.Err(); err != nil {
					t.Errorf("want nil error at/above floor, got %v", err)
				}
				return
			}
			if !strings.Contains(got.Detail, tc.wantToken) {
				t.Errorf("podman-version detail %q missing token %q", got.Detail, tc.wantToken)
			}
			assertErrContains(t, rs.Err(), tc.wantToken)
		})
	}
}

func TestRunImageAbsent(t *testing.T) {
	ctx := context.Background()
	d := okDeps("linux")
	d.ImagePresent = func(context.Context, string) error {
		return errors.New("no such image")
	}
	rs := d.Run(ctx, testParams)

	got := resultByName(t, rs, checkImage)
	if got.OK {
		t.Fatal("image check should fail when image absent")
	}
	if !strings.Contains(got.Detail, testParams.AgentImage) {
		t.Errorf("image detail %q missing image ref %q", got.Detail, testParams.AgentImage)
	}
	assertErrContains(t, rs.Err(), testParams.AgentImage)
}

// TestRunMultipleFailures asserts Run does not short-circuit: every failing
// precondition is reported and appears in the aggregated error.
func TestRunMultipleFailures(t *testing.T) {
	ctx := context.Background()
	d := Deps{
		GOOS:           "windows",
		PodmanRootless: func(context.Context) error { return errors.New("no podman") },
		PodmanVersion:  func(context.Context) error { return errors.New("podman too old") },
		ImagePresent:   func(context.Context, string) error { return errors.New("no image") },
	}
	rs := d.Run(ctx, testParams)

	for _, name := range []string{checkOS, checkPodman, checkPodmanVersion, checkImage} {
		if resultByName(t, rs, name).OK {
			t.Errorf("check %q should have failed", name)
		}
	}

	err := rs.Err()
	if err == nil {
		t.Fatal("want aggregated error for multiple failures, got nil")
	}
	// Every failing check's load-bearing token must appear in one error.
	for _, tok := range []string{
		"windows", "no podman", "podman too old", testParams.AgentImage,
	} {
		if !strings.Contains(err.Error(), tok) {
			t.Errorf("aggregated error %q missing token %q", err.Error(), tok)
		}
	}
}

// TestResultsErrMethod exercises the named-slice Err method directly.
func TestResultsErrMethod(t *testing.T) {
	if err := (Results{{Name: checkOS, OK: true}}).Err(); err != nil {
		t.Errorf("want nil for all-OK, got %v", err)
	}
	err := (Results{{Name: checkImage, OK: false, Detail: "boom"}}).Err()
	if err == nil || !strings.Contains(err.Error(), "boom") {
		t.Errorf("want error containing detail, got %v", err)
	}
}

func assertErrContains(t *testing.T, err error, tok string) {
	t.Helper()
	if err == nil {
		t.Fatalf("want error containing %q, got nil", tok)
	}
	if !strings.Contains(err.Error(), tok) {
		t.Errorf("error %q missing token %q", err.Error(), tok)
	}
}

// TestRunProvisionsMachineBeforeProbingPodman pins the ORDER of the darwin
// checks, which is load-bearing rather than cosmetic.
//
// Every podman command that reaches the container ENGINE talks to the Linux
// VM, so with no machine provisioned they fail. Measured on macOS 26.5.1 with
// podman 5.8.6 and no machine, `podman info` and
// `podman version --format {{.Client.Version}}` — the exact argv the two
// probes run — both exit 125. The machine check's adapter is what provisions
// that VM, so it has to run first or those two probes are guaranteed failures
// on the exact host the check exists to serve. Run does not short-circuit and
// both probes are fatal, so the wrong order fails a fresh Mac that
// provisioning would have fixed.
//
// The fakes below reproduce that coupling: the podman probes fail until the
// machine adapter has run.
func TestRunProvisionsMachineBeforeProbingPodman(t *testing.T) {
	ctx := context.Background()

	provisioned := false
	d := okDeps("darwin")
	d.MachineReady = func(context.Context) error {
		provisioned = true
		return nil
	}
	needsMachine := func(context.Context) error {
		if !provisioned {
			// The real copy podman 5.8.6 emits, trimmed.
			return errors.New("Cannot connect to Podman: unable to connect to Podman socket")
		}
		return nil
	}
	d.PodmanRootless = needsMachine
	d.PodmanVersion = needsMachine
	// imagePresent shells `podman image exists`, which also reaches the engine
	// inside the VM, so it carries the same precondition and belongs in the
	// same latch. Without this, a future edit hoisting the image check above
	// the machine block would be caught by nothing.
	d.ImagePresent = func(ctx context.Context, _ string) error { return needsMachine(ctx) }

	rs := d.Run(ctx, testParams)

	for _, name := range []string{checkMachine, checkPodman, checkPodmanVersion, checkImage} {
		res := resultByName(t, rs, name)
		if !res.OK {
			t.Errorf("check %q failed on a host where provisioning succeeds: %s\n"+
				"the machine must be provisioned before the podman probes run",
				name, res.Detail)
		}
	}

	// The machine result must be REPORTED after the podman probes even though
	// it RAN first, so Err leads with a root cause rather than its symptom.
	posOf := func(name string) int {
		for i, r := range rs {
			if r.Name == name {
				return i
			}
		}
		t.Fatalf("check %q missing from results", name)
		return -1
	}
	if posOf(checkMachine) < posOf(checkPodman) {
		t.Errorf("machine result is reported at %d, before podman at %d; "+
			"Err would lead with the symptom instead of the root cause",
			posOf(checkMachine), posOf(checkPodman))
	}
}
