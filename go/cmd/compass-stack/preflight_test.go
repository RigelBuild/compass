//go:build unix

package main

import (
	"errors"
	"strings"
	"testing"
)

// TestDecideKVM covers the /dev/kvm verdict: an open error fails, a nil open
// passes. The open is injected (as microvmtest.decideKVMSource injects its
// probe), so the decision is tested without a real /dev/kvm.
func TestDecideKVM(t *testing.T) {
	tests := []struct {
		name    string
		openErr error
		wantOK  bool
	}{
		{name: "openable", openErr: nil, wantOK: true},
		{name: "permission denied", openErr: errors.New("permission denied"), wantOK: false},
		{name: "absent", openErr: errors.New("no such file or directory"), wantOK: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := decideKVM(tt.openErr)
			if got.ok != tt.wantOK {
				t.Errorf("decideKVM(%v).ok = %v, want %v (detail: %q)", tt.openErr, got.ok, tt.wantOK, got.detail)
			}
			if got.name != "kvm" {
				t.Errorf("decideKVM name = %q, want %q", got.name, "kvm")
			}
			if got.detail == "" {
				t.Error("decideKVM detail is empty; a verdict must be legible")
			}
		})
	}
}

// TestDecidePodman covers the podman verdict across its three failure modes and
// the pass: not on PATH, present-but-info-failed, present-but-not-rootless, and
// present+rootless. rootless must be true AND info must succeed AND it must be
// found for a pass.
func TestDecidePodman(t *testing.T) {
	lookErr := errors.New("exec: \"podman\": executable file not found in $PATH")
	infoErr := errors.New("cannot connect to podman")
	tests := []struct {
		name     string
		lookErr  error
		infoErr  error
		rootless bool
		wantOK   bool
	}{
		{name: "not on PATH", lookErr: lookErr, wantOK: false},
		{name: "info failed", infoErr: infoErr, wantOK: false},
		{name: "not rootless", rootless: false, wantOK: false},
		{name: "present and rootless", rootless: true, wantOK: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := decidePodman(tt.lookErr, tt.infoErr, tt.rootless)
			if got.ok != tt.wantOK {
				t.Errorf("decidePodman(look=%v, info=%v, rootless=%v).ok = %v, want %v (detail: %q)",
					tt.lookErr, tt.infoErr, tt.rootless, got.ok, tt.wantOK, got.detail)
			}
			if got.name != "podman" {
				t.Errorf("decidePodman name = %q, want %q", got.name, "podman")
			}
		})
	}
}

// TestVersionGroups covers the version parser: semver, a v-prefix, passt's
// date+hash scheme (the trailing hash yields a group the floor never reads), and
// a string with no digits.
func TestVersionGroups(t *testing.T) {
	tests := []struct {
		in   string
		want []int
	}{
		{in: "cloud-hypervisor v53.0.0", want: []int{53, 0, 0}},
		{in: "1.14.0", want: []int{1, 14, 0}},
		{in: "passt 2025_09_19.623dbf6", want: []int{2025, 9, 19, 623, 6}},
		{in: "no digits here", want: nil},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			got := versionGroups(tt.in)
			if len(got) != len(tt.want) {
				t.Fatalf("versionGroups(%q) = %v, want %v", tt.in, got, tt.want)
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Fatalf("versionGroups(%q) = %v, want %v", tt.in, got, tt.want)
				}
			}
		})
	}
}

// TestAtLeast covers the field-by-field floor comparison: equal, above on a
// leading field, below on a trailing field, a longer got above (trailing build
// group ignored), an empty got, and the tricky absent-trailing-field cases where
// a shorter non-empty got's missing field reads as zero to decide the verdict.
func TestAtLeast(t *testing.T) {
	tests := []struct {
		name  string
		got   []int
		floor []int
		want  bool
	}{
		{name: "equal", got: []int{53, 0, 0}, floor: []int{53, 0, 0}, want: true},
		{name: "above on major", got: []int{54, 0, 0}, floor: []int{53, 0, 0}, want: true},
		{name: "below on major", got: []int{52, 9, 9}, floor: []int{53, 0, 0}, want: false},
		{name: "below on patch", got: []int{1, 13, 9}, floor: []int{1, 14, 0}, want: false},
		{name: "above on minor", got: []int{1, 15, 0}, floor: []int{1, 14, 0}, want: true},
		{name: "trailing build group ignored", got: []int{2025, 9, 19, 623}, floor: []int{2025, 9, 19}, want: true},
		{name: "date below on day", got: []int{2025, 9, 18}, floor: []int{2025, 9, 19}, want: false},
		{name: "empty got", got: nil, floor: []int{1, 0, 0}, want: false},
		{name: "shorter got below on absent patch", got: []int{53, 0}, floor: []int{53, 0, 1}, want: false},
		{name: "shorter got equal via absent-as-zero", got: []int{1, 14}, floor: []int{1, 14, 0}, want: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := atLeast(tt.got, tt.floor); got != tt.want {
				t.Errorf("atLeast(%v, %v) = %v, want %v", tt.got, tt.floor, got, tt.want)
			}
		})
	}
}

// TestDecideVersion covers the per-binary verdict end to end: absent, version
// command failing, unparseable output, below the floor, at the floor, and above.
func TestDecideVersion(t *testing.T) {
	floor := versionFloor{binary: "cloud-hypervisor", fields: []int{53, 0, 0}, display: "53.0.0"}
	lookErr := errors.New("not found in $PATH")
	runErr := errors.New("exit status 1")
	tests := []struct {
		name    string
		lookErr error
		runErr  error
		output  string
		wantOK  bool
	}{
		{name: "absent", lookErr: lookErr, wantOK: false},
		{name: "version command failed", runErr: runErr, wantOK: false},
		{name: "unparseable output", output: "no version token", wantOK: false},
		{name: "below floor", output: "cloud-hypervisor v52.0.0", wantOK: false},
		{name: "at floor", output: "cloud-hypervisor v53.0.0\nMigration Protocol Versions: 0", wantOK: true},
		{name: "above floor", output: "cloud-hypervisor v54.1.0", wantOK: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := decideVersion(floor, tt.lookErr, tt.runErr, tt.output)
			if got.ok != tt.wantOK {
				t.Errorf("decideVersion(%q).ok = %v, want %v (detail: %q)", tt.output, got.ok, tt.wantOK, got.detail)
			}
			if got.name != floor.binary {
				t.Errorf("decideVersion name = %q, want %q", got.name, floor.binary)
			}
		})
	}
}

// TestPreflightDispatch confirms preflight is a recognized subcommand: an
// unknown subcommand error names it, so an operator who typos it is told the
// valid set.
func TestPreflightDispatch(t *testing.T) {
	err := run([]string{"bogus"})
	if err == nil {
		t.Fatal("expected error for unknown subcommand, got nil")
	}
	if got := err.Error(); !strings.Contains(got, "preflight") {
		t.Errorf("unknown-subcommand error %q does not name preflight", got)
	}
}
