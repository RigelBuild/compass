//go:build unix

package main

import (
	"flag"
	"fmt"
	"os"
	"os/exec" //nolint:depguard // podman/microVM preflight probe: LookPath + fixed-arg version/info subprocesses
	"strings"

	"github.com/RigelBuild/compass/go/internal/hostcheck"
	runtimelib "github.com/RigelBuild/compass/go/internal/runtime"
)

// preflight is the T9 host bring-up gate: it surfaces the KVM/podman/microVM and
// secretspec-CLI prerequisites at install-time rather than at first `up`. A thin
// consumer of the runtime lane's internal/hostcheck core, keeping only the
// stack-specific podman-rootless and secretspec-CLI checks. The server's secret
// read path spawns that CLI, and operators rotate with it. Do not grow it.

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

// checkBinaryVersion resolves one external binary on PATH and runs its
// --version, then returns the version verdict against the floor. It is generic
// over any VersionFloor — the microVM trio and the secretspec CLI alike.
func checkBinaryVersion(f hostcheck.VersionFloor) hostcheck.Result {
	path, lookErr := exec.LookPath(f.Binary)
	if lookErr != nil {
		return hostcheck.DecideVersion(f, lookErr, nil, "")
	}
	// G204: path is a LookPath-resolved fixed binary name and "--version" is a
	// literal — neither is user-controlled (mirrors adapters/process.go:63).
	out, runErr := exec.Command(path, "--version").Output() //nolint:gosec // G204: LookPath-resolved fixed binary, literal arg
	return hostcheck.DecideVersion(f, nil, runErr, string(out))
}

// runPreflight runs every check, prints a legible result line per check to
// stdout, and returns an error naming the hard failures. A failed check is a
// legible non-zero exit, never a crash: every check catches a missing dependency.
func runPreflight(args []string) error {
	fs := flag.NewFlagSet("preflight", flag.ContinueOnError)
	runtimeBackend := fs.String("runtime-backend", "",
		"Session backend the runner runs agents on ('microvm'). Empty leaves the "+
			"runner on its own resolution — there is no implicit microVM. "+
			"Honors $COMPASS_RUNTIME_BACKEND; the flag wins.")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *runtimeBackend == "" {
		*runtimeBackend = os.Getenv("COMPASS_RUNTIME_BACKEND")
	}
	backend, err := runtimelib.ValidBackend(*runtimeBackend)
	if err != nil {
		return err
	}

	checks := []preflightCheck{
		{result: checkKVM(), microVM: true},
		{result: checkPodman()},
	}
	for _, f := range hostcheck.MicroVMFloors {
		checks = append(checks, preflightCheck{result: checkBinaryVersion(f), microVM: true})
	}
	// Keep secretspec after the contiguous microVM group; it is a server
	// dependency rather than a microVM userspace binary.
	checks = append(checks, preflightCheck{result: checkBinaryVersion(hostcheck.SecretSpecFloor)})

	lines, failed := preflightLines(backend, checks)
	for _, line := range lines {
		if err := printCheckLine(line); err != nil {
			return err
		}
	}
	if len(failed) > 0 {
		return fmt.Errorf("preflight failed: %s (see the per-check lines above)", strings.Join(failed, ", "))
	}
	return nil
}

type preflightCheck struct {
	result  hostcheck.Result
	microVM bool
}

// preflightLines applies backend policy and formats results in check order.
// Only checks marked as microVM prerequisites become advisory off that backend.
func preflightLines(backend string, checks []preflightCheck) ([]string, []string) {
	lines := make([]string, 0, len(checks))
	var failed []string
	for _, check := range checks {
		c := check.result
		mark := "PASS"
		if !c.OK {
			mark = "FAIL"
			if backend != string(runtimelib.WorkloadTierMicroVM) && check.microVM {
				mark = "WARN"
			} else {
				failed = append(failed, c.Name)
			}
		}
		lines = append(lines, fmt.Sprintf("[%s] %-16s %s", mark, c.Name, c.Detail))
	}
	return lines, failed
}

// printCheckLine writes one preformatted result to stdout.
func printCheckLine(line string) error {
	if _, err := fmt.Fprintln(os.Stdout, line); err != nil {
		return fmt.Errorf("writing preflight output: %w", err)
	}
	return nil
}
