//go:build unix

package main

import (
	"flag"
	"fmt"
	"os"
	"os/exec" //nolint:depguard // podman/microVM preflight probe: LookPath + fixed-arg version/info subprocesses
	"regexp"
	"strconv"
	"strings"
)

// preflight is the T9 host bring-up gate: it surfaces the KVM/podman/microVM
// prerequisites at install-time rather than at first `up`, printing a legible
// pass/fail line per check. It is deliberately MINIMAL and SELF-CONTAINED.
//
// DEPENDENCY HONESTY (design RIG-1746, task T9): there is NO
// VerifyMicroVMSupport function in the runtime lane today — the name occurs only
// in a forward-looking comment (go/internal/runtime/microvm.go:126-128: "the
// default collapses to microVM guarded by a VerifyMicroVMSupport hard gate at
// startup"). These checks are T9's OWN, and are TO BE REPLACED by the runtime
// lane's eventual preflight gate once it lands. Keeping them here — small,
// honest, and off the runtime lane's critical path — is what lets T9 ship
// without blocking on that lane. Do not grow this into a capability framework.

// checkResult is the outcome of one preflight check: a stable name, whether it
// passed, and a human-legible detail line explaining the verdict (the version
// found, the reason it failed). It is what printCheck renders and what the pure
// decision helpers return, so the decisions are unit-testable without touching a
// real /dev/kvm or podman.
type checkResult struct {
	name   string
	ok     bool
	detail string
}

// podmanBinary is the podman executable name, resolved on PATH. It is the check
// name and the LookPath target, so it is named once here (goconst).
const podmanBinary = "podman"

// versionFloor pins the minimum acceptable version of one microVM userspace
// binary. The floor tracks the devenv.lock pin the dev shell provides (design
// S3: "the shell provides one pinned version from devenv.lock; the runtime
// preflight will enforce the floor"). Fields is the version parsed into ordered
// integer groups (major, minor, patch — or year, month, day for passt's
// date-based scheme), compared field by field by atLeast.
type versionFloor struct {
	binary string
	fields []int
	// display is the floor rendered for the pass/fail detail line.
	display string
}

// microVMFloors is the userspace trio the microVM backend drives, each at the
// devenv.lock pin as its floor (devenv.nix:209-211). passt versions by date
// (YYYY_MM_DD), so its floor is a year/month/day triple; cloud-hypervisor and
// virtiofsd are semver.
var microVMFloors = []versionFloor{
	{binary: "cloud-hypervisor", fields: []int{53, 0, 0}, display: "53.0.0"},
	{binary: "virtiofsd", fields: []int{1, 14, 0}, display: "1.14.0"},
	{binary: "passt", fields: []int{2025, 9, 19}, display: "2025_09_19"},
}

// versionGroups splits a version string into ordered integer groups on any
// non-digit run, so "v53.0.0" -> [53 0 0], "1.14.0" -> [1 14 0], and passt's
// "2025_09_19.623dbf6" -> [2025 9 19 623 6] (the hash "623dbf6" yields its two
// digit runs). Comparison only reads as many leading groups as the floor
// defines, so a trailing build-hash group never affects the verdict.
var digitRun = regexp.MustCompile(`[0-9]+`)

func versionGroups(s string) []int {
	matches := digitRun.FindAllString(s, -1)
	groups := make([]int, 0, len(matches))
	for _, m := range matches {
		n, err := strconv.Atoi(m)
		if err != nil {
			// A digit run that overflows int is not a version we can compare;
			// stop at the last comparable group rather than guess.
			break
		}
		groups = append(groups, n)
	}
	return groups
}

// atLeast reports whether the parsed version groups are at or above the floor,
// comparing field by field. A got that is shorter than the floor at an equal
// prefix is treated as below (a missing patch field reads as 0 only when got has
// fewer fields AND the compared prefix was equal — handled by treating absent
// fields as 0). Returns false when got has no numeric groups at all.
func atLeast(got, floor []int) bool {
	if len(got) == 0 {
		return false
	}
	for i, f := range floor {
		var g int
		if i < len(got) {
			g = got[i]
		}
		if g > f {
			return true
		}
		if g < f {
			return false
		}
	}
	return true
}

// firstLine returns the first line of s, trimmed. Version commands often print
// extra lines (cloud-hypervisor prints a migration-protocol line, passt prints a
// license) so the version token is parsed from the first line only.
func firstLine(s string) string {
	if line, _, found := strings.Cut(s, "\n"); found {
		return strings.TrimSpace(line)
	}
	return strings.TrimSpace(s)
}

// decideKVM is the pure verdict for the /dev/kvm check: openErr is the result of
// attempting to open the device R/W. Presence on the filesystem is not enough —
// the uid needs read/write access a VMM opens with — so the caller opens (and
// closes) it and passes the error here. nil means openable.
func decideKVM(openErr error) checkResult {
	if openErr != nil {
		return checkResult{
			name:   "kvm",
			ok:     false,
			detail: fmt.Sprintf("/dev/kvm not openable: %v (host must expose KVM; add the invoking user to the kvm group)", openErr),
		}
	}
	return checkResult{name: "kvm", ok: true, detail: "/dev/kvm present and openable"}
}

// decidePodman is the pure verdict for the podman check. lookErr is the
// exec.LookPath result (nil = podman on PATH); infoErr and rootless are the
// result of `podman info` reporting Host.Security.Rootless. podman must be both
// present and rootless-capable (the stack runs postgres as a rootless container,
// S4).
func decidePodman(lookErr, infoErr error, rootless bool) checkResult {
	if lookErr != nil {
		return checkResult{name: podmanBinary, ok: false, detail: fmt.Sprintf("podman not found on PATH: %v", lookErr)}
	}
	if infoErr != nil {
		return checkResult{name: podmanBinary, ok: false, detail: fmt.Sprintf("podman present but `podman info` failed: %v", infoErr)}
	}
	if !rootless {
		return checkResult{name: podmanBinary, ok: false, detail: "podman present but not rootless-capable (Host.Security.Rootless=false)"}
	}
	return checkResult{name: podmanBinary, ok: true, detail: "podman present and rootless-capable"}
}

// decideVersion is the pure verdict for one microVM userspace binary. lookErr is
// the exec.LookPath result; runErr is the `--version` invocation error; output
// is that command's stdout. The version is parsed from the first line and
// compared against the floor.
func decideVersion(f versionFloor, lookErr, runErr error, output string) checkResult {
	if lookErr != nil {
		return checkResult{name: f.binary, ok: false, detail: fmt.Sprintf("%s not found on PATH: %v", f.binary, lookErr)}
	}
	if runErr != nil {
		return checkResult{name: f.binary, ok: false, detail: fmt.Sprintf("%s present but `%s --version` failed: %v", f.binary, f.binary, runErr)}
	}
	line := firstLine(output)
	got := versionGroups(line)
	if len(got) == 0 {
		return checkResult{name: f.binary, ok: false, detail: fmt.Sprintf("%s present but no version parsed from %q", f.binary, line)}
	}
	if !atLeast(got, f.fields) {
		return checkResult{name: f.binary, ok: false, detail: fmt.Sprintf("reported %q is below the floor %s", line, f.display)}
	}
	return checkResult{name: f.binary, ok: true, detail: fmt.Sprintf("reported %q at/above floor %s", line, f.display)}
}

// probeKVM opens /dev/kvm R/W and immediately closes it, returning the open
// error. It is the effectful half of the KVM check; decideKVM is the pure half.
func probeKVM() error {
	fd, err := os.OpenFile("/dev/kvm", os.O_RDWR, 0)
	if err != nil {
		return err
	}
	// Probe only; a real VMM opens its own handle. A close error on a device we
	// only opened to test access is not actionable.
	_ = fd.Close()
	return nil
}

// checkKVM runs the /dev/kvm probe and returns its verdict.
func checkKVM() checkResult { return decideKVM(probeKVM()) }

// checkPodman resolves podman on PATH and, if found, asks `podman info` whether
// the host is rootless-capable, then returns the verdict.
func checkPodman() checkResult {
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
func checkMicroVMBinary(f versionFloor) checkResult {
	path, lookErr := exec.LookPath(f.binary)
	if lookErr != nil {
		return decideVersion(f, lookErr, nil, "")
	}
	// G204: path is a LookPath-resolved fixed binary name and "--version" is a
	// literal — neither is user-controlled (mirrors adapters/process.go:63).
	out, runErr := exec.Command(path, "--version").Output() //nolint:gosec // G204: LookPath-resolved fixed binary, literal arg
	return decideVersion(f, nil, runErr, string(out))
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

	checks := []checkResult{checkKVM(), checkPodman()}
	for _, f := range microVMFloors {
		checks = append(checks, checkMicroVMBinary(f))
	}

	var failed []string
	for _, c := range checks {
		printCheck(c)
		if !c.ok {
			failed = append(failed, c.name)
		}
	}
	if len(failed) > 0 {
		return fmt.Errorf("preflight failed: %s (see the per-check lines above)", strings.Join(failed, ", "))
	}
	return nil
}

// printCheck writes one pass/fail line to stdout. Output is the command's own
// result, so it goes to stdout (logs go to stderr).
func printCheck(c checkResult) {
	mark := "FAIL"
	if c.ok {
		mark = "PASS"
	}
	// Best-effort CLI output; a write failure to the terminal is not actionable.
	_, _ = fmt.Fprintf(os.Stdout, "[%s] %-16s %s\n", mark, c.name, c.detail)
}
