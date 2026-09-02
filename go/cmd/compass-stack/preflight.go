//go:build unix

package main

import (
	"flag"
	"fmt"
	"os"
	"os/exec" //nolint:depguard // podman/microVM preflight probe: LookPath + fixed-arg version/info subprocesses
	"strings"

	"github.com/RigelBuild/compass/go/internal/hostcheck"
)

// preflight is the T9 host bring-up gate: it surfaces the KVM/podman/microVM
// prerequisites at install-time rather than at first `up`, printing a legible
// pass/fail line per check. It is deliberately MINIMAL and a thin consumer.
//
// DEPENDENCY HONESTY: the host-capability check logic now lives in the runtime
// lane's internal/hostcheck (the /dev/kvm probe, the version-floor comparator,
// the per-check verdicts). The runtime lane also defines VerifyMicroVMSupport
// (go/internal/runtime/microvm_preflight.go) to enforce the same floors as a
// hard-fail Runner-startup gate; its wiring into Runner selection lands in a
// later V5 wave. This command is a thin consumer of that shared core — no
// longer a placeholder to be replaced — and keeps only what is stack-specific:
// the podman rootless-capability check (postgres runs as a rootless container)
// and the print/exit surface. Do
// not grow this into a capability framework.

// podmanBinary is the podman executable name, resolved on PATH. It is the check
// name and the LookPath target, so it is named once here (goconst).
const podmanBinary = "podman"

// decidePodman is the pure verdict for the podman check. lookErr is the
// exec.LookPath result (nil = podman on PATH); infoErr and rootless are the
// result of `podman info` reporting Host.Security.Rootless. podman must be both
// present and rootless-capable (the stack runs postgres as a rootless container,
// S4).
func decidePodman(lookErr, infoErr error, rootless bool) hostcheck.Result {
	if lookErr != nil {
		return hostcheck.Result{Name: podmanBinary, OK: false, Detail: fmt.Sprintf("podman not found on PATH: %v", lookErr)}
	}
	if infoErr != nil {
		return hostcheck.Result{Name: podmanBinary, OK: false, Detail: fmt.Sprintf("podman present but `podman info` failed: %v", infoErr)}
	}
	if !rootless {
		return hostcheck.Result{Name: podmanBinary, OK: false, Detail: "podman present but not rootless-capable (Host.Security.Rootless=false)"}
	}
	return hostcheck.Result{Name: podmanBinary, OK: true, Detail: "podman present and rootless-capable"}
}

// checkKVM runs the /dev/kvm probe and returns its verdict.
func checkKVM() hostcheck.Result { return hostcheck.DecideKVM(hostcheck.ProbeKVM()) }

// checkPodman resolves podman on PATH and, if found, asks `podman info` whether
// the host is rootless-capable, then returns the verdict.
func checkPodman() hostcheck.Result {
	path, lookErr := exec.LookPath(podmanBinary)
	if lookErr != nil {
		return decidePodman(lookErr, nil, false)
	}
	// G204: path is a LookPath-resolved fixed binary name and the args are
	// literals — neither is user-controlled (mirrors adapters/process.go:63).
	out, infoErr := exec.Command(path, "info", "--format", "{{.Host.Security.Rootless}}").Output() //nolint:gosec // G204: LookPath-resolved fixed binary, literal args
	rootless := strings.TrimSpace(string(out)) == "true"
	return decidePodman(nil, infoErr, rootless)
}

// checkMicroVMBinary resolves one trio binary on PATH and runs its --version,
// then returns the version verdict against the floor.
func checkMicroVMBinary(f hostcheck.VersionFloor) hostcheck.Result {
	path, lookErr := exec.LookPath(f.Binary)
	if lookErr != nil {
		return hostcheck.DecideVersion(f, lookErr, nil, "")
	}
	// G204: path is a LookPath-resolved fixed binary name and "--version" is a
	// literal — neither is user-controlled (mirrors adapters/process.go:63).
	out, runErr := exec.Command(path, "--version").Output() //nolint:gosec // G204: LookPath-resolved fixed binary, literal arg
	return hostcheck.DecideVersion(f, nil, runErr, string(out))
}

// runPreflight runs every check, prints a legible pass/fail line per check to
// stdout, and returns an error naming the failed checks if any failed. A failed
// check is a legible non-zero exit, never a crash: every check catches the
// absence of its dependency and reports it.
func runPreflight(args []string) error {
	// Reject unknown flags legibly, mirroring up/down/status; preflight takes
	// none today, so any arg is a usage error rather than a silent no-op.
	fs := flag.NewFlagSet("preflight", flag.ContinueOnError)
	if err := fs.Parse(args); err != nil {
		return err
	}

	checks := []hostcheck.Result{checkKVM(), checkPodman()}
	for _, f := range hostcheck.MicroVMFloors {
		checks = append(checks, checkMicroVMBinary(f))
	}

	var failed []string
	for _, c := range checks {
		printCheck(c)
		if !c.OK {
			failed = append(failed, c.Name)
		}
	}
	if len(failed) > 0 {
		return fmt.Errorf("preflight failed: %s (see the per-check lines above)", strings.Join(failed, ", "))
	}
	return nil
}

// printCheck writes one pass/fail line to stdout. Output is the command's own
// result, so it goes to stdout (logs go to stderr).
func printCheck(c hostcheck.Result) {
	mark := "FAIL"
	if c.OK {
		mark = "PASS"
	}
	// Best-effort CLI output; a write failure to the terminal is not actionable.
	_, _ = fmt.Fprintf(os.Stdout, "[%s] %-16s %s\n", mark, c.Name, c.Detail)
}
