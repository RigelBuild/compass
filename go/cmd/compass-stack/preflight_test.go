//go:build unix

package main

import (
	"errors"
	"strings"
	"testing"
)

// TestDecidePodman covers the podman verdict across its three failure modes and
// the pass: not on PATH, present-but-info-failed, present-but-not-rootless, and
// present+rootless. rootless must be true AND info must succeed AND it must be
// found for a pass. It is the one check that stays stack-specific after the
// hostcheck extraction (rootless-capability is a compass-stack concern).
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
			if got.OK != tt.wantOK {
				t.Errorf("decidePodman(look=%v, info=%v, rootless=%v).OK = %v, want %v (detail: %q)",
					tt.lookErr, tt.infoErr, tt.rootless, got.OK, tt.wantOK, got.Detail)
			}
			if got.Name != "podman" {
				t.Errorf("decidePodman name = %q, want %q", got.Name, "podman")
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

// TestPreflightReportsSecretSpec pins the WIRING (separately from the verdict
// logic, which hostcheck.DecideVersion owns): runPreflight must actually run a
// secretspec check, and must run it AFTER the microVM trio. PATH is pointed at
// an empty dir so every binary check fails deterministically on any host, which
// makes runPreflight's error enumerate the failed check names in check order —
// so it witnesses both presence and position.
func TestPreflightReportsSecretSpec(t *testing.T) {
	t.Setenv("PATH", t.TempDir())

	err := runPreflight(nil)
	if err == nil {
		t.Fatal("expected preflight to fail with an empty PATH, got nil")
	}
	got := err.Error()
	if !strings.Contains(got, "secretspec") {
		t.Fatalf("preflight failure %q does not name the secretspec check; the install-time secrets-write-path dependency is unreported", got)
	}
	// The trio is printed as a group; secretspec is appended after it.
	if last := strings.LastIndex(got, "passt"); last > strings.Index(got, "secretspec") {
		t.Errorf("preflight failure %q reports secretspec before the microVM trio; the trio must stay grouped first", got)
	}
}
