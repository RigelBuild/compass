// Package hostcheck is the shared host-capability check core: the /dev/kvm open
// probe, the version-floor comparator, and the per-check verdicts consumed by
// both the compass-stack install-time gate (cmd/compass-stack) and the runtime
// lane's VerifyMicroVMSupport (internal/runtime). It is an untagged leaf package
// so both a package-main consumer and the unix-tagged runtime lane can import it,
// and it holds the ONE copy of the devenv.lock version floors the two gates
// share.
package hostcheck

import (
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"
)

// Result is the outcome of one host-capability check: a stable name, whether it
// passed, and a human-legible detail line explaining the verdict (the version
// found, the reason it failed). It is what the pure decision helpers return, so
// the decisions are unit-testable without touching a real /dev/kvm.
type Result struct {
	Name   string
	OK     bool
	Detail string
}

// VersionFloor pins the minimum acceptable version of one microVM userspace
// binary. The floor tracks the devenv.lock pin the dev shell provides. Fields is
// the version parsed into ordered integer groups (major, minor, patch — or year,
// month, day for passt's date-based scheme), compared field by field by AtLeast.
type VersionFloor struct {
	Binary string
	Fields []int
	// Display is the floor rendered for the verdict detail line.
	Display string
}

// MicroVMFloors is the userspace trio the microVM backend drives, each at the
// devenv.lock pin as its floor. passt versions by date (YYYY_MM_DD), so its
// floor is a year/month/day triple; cloud-hypervisor and virtiofsd are semver.
var MicroVMFloors = []VersionFloor{
	{Binary: "cloud-hypervisor", Fields: []int{53, 0, 0}, Display: "53.0.0"},
	{Binary: "virtiofsd", Fields: []int{1, 14, 0}, Display: "1.14.0"},
	{Binary: "passt", Fields: []int{2025, 9, 19}, Display: "2025_09_19"},
}

// digitRun matches one run of decimal digits; VersionGroups splits on it.
var digitRun = regexp.MustCompile(`[0-9]+`)

// VersionGroups splits a version string into ordered integer groups on any
// non-digit run, so "v53.0.0" -> [53 0 0], "1.14.0" -> [1 14 0], and passt's
// "2025_09_19.623dbf6" -> [2025 9 19 623 6] (the hash "623dbf6" yields its two
// digit runs). Comparison only reads as many leading groups as the floor
// defines, so a trailing build-hash group never affects the verdict.
func VersionGroups(s string) []int {
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

// AtLeast reports whether the parsed version groups are at or above the floor,
// comparing field by field. A got that is shorter than the floor at an equal
// prefix is treated as below (a missing patch field reads as 0 only when got has
// fewer fields AND the compared prefix was equal — handled by treating absent
// fields as 0). Returns false when got has no numeric groups at all.
func AtLeast(got, floor []int) bool {
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

// FirstLine returns the first line of s, trimmed. Version commands often print
// extra lines (cloud-hypervisor prints a migration-protocol line, passt prints a
// license) so the version token is parsed from the first line only.
func FirstLine(s string) string {
	if line, _, found := strings.Cut(s, "\n"); found {
		return strings.TrimSpace(line)
	}
	return strings.TrimSpace(s)
}

// DecideKVM is the pure verdict for the /dev/kvm check: openErr is the result of
// attempting to open the device R/W. Presence on the filesystem is not enough —
// the uid needs read/write access a VMM opens with — so the caller opens (and
// closes) it and passes the error here. nil means openable.
func DecideKVM(openErr error) Result {
	if openErr != nil {
		return Result{
			Name:   "kvm",
			OK:     false,
			Detail: fmt.Sprintf("/dev/kvm not openable: %v (host must expose KVM; add the invoking user to the kvm group)", openErr),
		}
	}
	return Result{Name: "kvm", OK: true, Detail: "/dev/kvm present and openable"}
}

// DecideVersion is the pure verdict for one microVM userspace binary. lookErr is
// the exec.LookPath result; runErr is the `--version` invocation error; output
// is that command's stdout. The version is parsed from the first line and
// compared against the floor.
func DecideVersion(f VersionFloor, lookErr, runErr error, output string) Result {
	if lookErr != nil {
		return Result{Name: f.Binary, OK: false, Detail: fmt.Sprintf("%s not found on PATH: %v", f.Binary, lookErr)}
	}
	if runErr != nil {
		return Result{Name: f.Binary, OK: false, Detail: fmt.Sprintf("%s present but `%s --version` failed: %v", f.Binary, f.Binary, runErr)}
	}
	line := FirstLine(output)
	got := VersionGroups(line)
	if len(got) == 0 {
		return Result{Name: f.Binary, OK: false, Detail: fmt.Sprintf("%s present but no version parsed from %q", f.Binary, line)}
	}
	if !AtLeast(got, f.Fields) {
		return Result{Name: f.Binary, OK: false, Detail: fmt.Sprintf("reported %q is below the floor %s", line, f.Display)}
	}
	return Result{Name: f.Binary, OK: true, Detail: fmt.Sprintf("reported %q at/above floor %s", line, f.Display)}
}

// ProbeKVM opens /dev/kvm R/W and immediately closes it, returning the open
// error. It is the effectful half of the KVM check; DecideKVM is the pure half.
func ProbeKVM() error {
	fd, err := os.OpenFile("/dev/kvm", os.O_RDWR, 0)
	if err != nil {
		return err
	}
	// Probe only; a real VMM opens its own handle. A close error on a device we
	// only opened to test access is not actionable.
	_ = fd.Close()
	return nil
}
