package preflight

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// Deps is the set of external effects the preflight core is inverted over. Each
// func field is one genuine external effect; the real adapters (which shell out
// to podman and inspect the local image store) are supplied at the wiring
// boundary, and unit tests supply stubs. The core imports none of those
// subsystems itself.
type Deps struct {
	// GOOS is the host operating system (runtime.GOOS at the wiring boundary).
	// Injected rather than read directly so the OS check is unit-testable.
	GOOS string
	// PodmanRootless probes that rootless podman is present and usable. A nil
	// error means it is available; a non-nil error explains why not.
	PodmanRootless func(ctx context.Context) error
	// PodmanVersion checks the runner's userns floor; its startup refusal is
	// swallowed by the fire-and-return stack, so surface the same error here.
	PodmanVersion func(ctx context.Context) error
	// MachineReady is consulted only on darwin. It is required there; a nil
	// adapter reports a failed check rather than letting a broken host pass.
	MachineReady func(ctx context.Context) error
	// ImagePresent probes that the given agent image ref is present in the local
	// container store. A nil error means present; a non-nil error means it is not
	// available locally (it is pulled from GHCR at first run).
	ImagePresent func(ctx context.Context, image string) error
}

// Params is what the checks need from the caller: the resolved agent image ref.
type Params struct {
	// AgentImage is the container image ref the embedded runner runs; its
	// presence in the local store is checked.
	AgentImage string
}

// Result is the outcome of one preflight check. Name identifies the check; OK
// reports whether it passed; Detail carries the actionable failure copy when
// !OK (and may be empty when OK).
type Result struct {
	Name   string
	OK     bool
	Detail string
}

// Check names, stable for logs and error copy.
const (
	checkOS            = "os"
	checkPodman        = "podman"
	checkPodmanVersion = "podman-version"
	checkMachine       = "machine"
	checkImage         = "image"
)

// Exported aliases of the check names, so callers can classify results by check
// (e.g. the wiring boundary hard-gates host-capability checks and treats the
// image check as advisory). These are additive: the unexported names above stay
// the values written into Result.Name, and these consts alias them so a caller's
// classification cannot drift from the Run implementation.
const (
	CheckOS            = checkOS
	CheckPodman        = checkPodman
	CheckPodmanVersion = checkPodmanVersion
	CheckMachine       = checkMachine
	CheckImage         = checkImage
)

// Run executes every host precondition and returns one Result per check. It
// does NOT short-circuit: an operator should see every failing precondition at
// once, so all checks run even after an earlier failure. Call the returned
// Results' Err method to fold the failures into one legible error.
//
// Execution order is load-bearing on darwin: the machine check provisions the
// Linux VM that the podman probes talk to, so it runs first. The order results
// are APPENDED in is separate, and is the reporting order Err formats — the
// machine result is appended after the podman probes so a root cause leads.
func (d Deps) Run(ctx context.Context, p Params) Results {
	results := make(Results, 0, 5)

	// (1) OS is supported: linux or darwin (Windows/WSL is out of scope, OQ-4).
	osRes := Result{Name: checkOS, OK: d.GOOS == "linux" || d.GOOS == "darwin"}
	if !osRes.OK {
		osRes.Detail = "embedded mode runs on linux or darwin, this host is " + d.GOOS
	}
	results = append(results, osRes)

	// Provision the darwin VM before podman probes: without it, both fail. Then
	// append the machine result after podman failures so the missing podman root
	// cause appears before its machine symptom.
	darwin := d.GOOS == "darwin"
	var machineRes Result
	if darwin {
		machineRes = Result{Name: checkMachine, OK: true}
		if d.MachineReady == nil {
			machineRes.OK = false
			machineRes.Detail = "no podman machine adapter is wired on darwin; " +
				"embedded mode cannot verify the Linux VM podman runs inside " +
				"(this is a build/wiring defect, not a host condition)"
		} else if err := d.MachineReady(ctx); err != nil {
			machineRes.OK = false
			machineRes.Detail = fmt.Sprintf("the podman machine is not ready: %v", err)
		}
	}

	// (3) Rootless podman present.
	podmanRes := Result{Name: checkPodman, OK: true}
	if err := d.PodmanRootless(ctx); err != nil {
		podmanRes.OK = false
		podmanRes.Detail = fmt.Sprintf("rootless podman is required: %v", err)
	}
	results = append(results, podmanRes)

	// Surface the runner's userns floor here because the embedded stack hides
	// the runner's startup refusal; reuse the probe's error copy.
	pvRes := Result{Name: checkPodmanVersion, OK: true}
	if err := d.PodmanVersion(ctx); err != nil {
		pvRes.OK = false
		pvRes.Detail = err.Error()
	}
	results = append(results, pvRes)

	// The darwin machine result RAN above, before both podman probes; it is
	// reported here so a genuine "podman is not installed" leads the message.
	if darwin {
		results = append(results, machineRes)
	}

	// (5) Agent image present in the local store. Reporting "not available
	// locally" is the correct behavior, not a stub: the image is pulled from
	// GHCR at first run.
	imageRes := Result{Name: checkImage, OK: true}
	if err := d.ImagePresent(ctx, p.AgentImage); err != nil {
		imageRes.OK = false
		imageRes.Detail = fmt.Sprintf(
			"agent image %s is not available locally; it is pulled from GHCR at "+
				"first run: %v", p.AgentImage, err)
	}
	results = append(results, imageRes)

	return results
}

// Results is a preflight run's set of check outcomes.
type Results []Result

// Err aggregates the failed checks into one legible multi-line error, or returns
// nil when every check passed. Returning all failures at once lets the operator
// fix every unmet precondition in a single pass.
func (rs Results) Err() error {
	var failed []Result
	for _, r := range rs {
		if !r.OK {
			failed = append(failed, r)
		}
	}
	if len(failed) == 0 {
		return nil
	}
	var b strings.Builder
	b.WriteString("embedded-mode preflight failed:")
	for _, r := range failed {
		b.WriteString("\n  - ")
		b.WriteString(r.Detail)
	}
	return errors.New(b.String())
}
