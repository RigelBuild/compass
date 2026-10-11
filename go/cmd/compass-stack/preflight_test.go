//go:build unix

package main

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/RigelBuild/compass/go/internal/hostcheck"
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

// TestRunPreflightRuntimeBackendPrecedence verifies that an explicit backend
// overrides the environment and that the environment is used when the flag is
// absent. An empty PATH makes the binary probes deterministic.
func TestRunPreflightRuntimeBackendPrecedence(t *testing.T) {
	tests := []struct {
		name                string
		envBackend          string
		args                []string
		wantMicroVMFailures bool
	}{
		{
			name:                "flag overrides environment",
			envBackend:          "microvm",
			args:                []string{"--runtime-backend", "podman"},
			wantMicroVMFailures: false,
		},
		{
			name:                "environment applies without flag",
			envBackend:          "microvm",
			wantMicroVMFailures: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("PATH", t.TempDir())
			t.Setenv("COMPASS_RUNTIME_BACKEND", tt.envBackend)

			err := runPreflight(t.Context(), tt.args)
			if err == nil {
				t.Fatal("expected required podman and secretspec checks to fail with an empty PATH")
			}
			got := err.Error()
			for _, name := range []string{"cloud-hypervisor", "virtiofsd", "passt"} {
				contains := strings.Contains(got, name)
				if contains != tt.wantMicroVMFailures {
					t.Errorf("preflight failure %q includes microVM check %q = %v, want %v", got, name, contains, tt.wantMicroVMFailures)
				}
			}
			for _, name := range []string{"podman", "secretspec"} {
				if !strings.Contains(got, name) {
					t.Errorf("preflight failure %q does not include required check %q", got, name)
				}
			}
		})
	}
}

// TestRunPreflightBackendValidation checks backend normalization and rejects
// invalid names before running checks. Empty PATH keeps binary outcomes stable.
func TestRunPreflightBackendValidation(t *testing.T) {
	tests := []struct {
		name            string
		backend         string
		wantErrContains []string
		wantErrAbsent   []string
	}{
		{
			name:            "unknown backend is rejected before checks",
			backend:         "micorvm",
			wantErrContains: []string{"micorvm", "accepted values are", "podman", "microvm", "apple-container", "host"},
			wantErrAbsent:   []string{"preflight failed:"},
		},
		{
			name:    "whitespace around microvm is trimmed",
			backend: " microvm ",
			// kvm may pass or fail by host; the trio fails hard either way with an empty PATH.
			wantErrContains: []string{"preflight failed:", "podman", "cloud-hypervisor", "virtiofsd", "passt", "secretspec"},
		},
		{
			name:            "apple-container has advisory microvm prerequisites",
			backend:         "apple-container",
			wantErrContains: []string{"preflight failed: podman, secretspec"},
			wantErrAbsent:   []string{"kvm", "cloud-hypervisor", "virtiofsd", "passt"},
		},
		{
			name:            "host has advisory microvm prerequisites",
			backend:         "host",
			wantErrContains: []string{"preflight failed: podman, secretspec"},
			wantErrAbsent:   []string{"kvm", "cloud-hypervisor", "virtiofsd", "passt"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("PATH", t.TempDir())
			err := runPreflight(t.Context(), []string{"--runtime-backend", tt.backend})
			if err == nil {
				t.Fatal("expected preflight error with an empty PATH or unknown backend")
			}
			for _, want := range tt.wantErrContains {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("preflight error %q does not contain %q", err, want)
				}
			}
			for _, absent := range tt.wantErrAbsent {
				if strings.Contains(err.Error(), absent) {
					t.Errorf("preflight error %q unexpectedly contains %q", err, absent)
				}
			}
		})
	}
}

// TestPreflightLines applies backend policy to synthetic results so both
// advisory and hard-failure paths are deterministic on every host.
func TestPreflightLines(t *testing.T) {
	const defaultWarnings = `[WARN] kvm              detail
[PASS] podman           detail
[WARN] cloud-hypervisor detail
[WARN] virtiofsd        detail
[WARN] passt            detail
[PASS] secretspec       detail`
	const microVMFailures = `[PASS] kvm              detail
[PASS] podman           detail
[FAIL] cloud-hypervisor detail
[FAIL] virtiofsd        detail
[FAIL] passt            detail
[PASS] secretspec       detail`
	const microVMAllPass = `[PASS] kvm              detail
[PASS] podman           detail
[PASS] cloud-hypervisor detail
[PASS] virtiofsd        detail
[PASS] passt            detail
[PASS] secretspec       detail`
	const podmanFailure = `[PASS] kvm              detail
[FAIL] podman           detail
[PASS] cloud-hypervisor detail
[PASS] virtiofsd        detail
[PASS] passt            detail
[PASS] secretspec       detail`
	const secretspecFailure = `[PASS] kvm              detail
[PASS] podman           detail
[PASS] cloud-hypervisor detail
[PASS] virtiofsd        detail
[PASS] passt            detail
[FAIL] secretspec       detail`
	tests := []struct {
		name       string
		backend    string
		failures   []string
		wantOutput string
		wantFailed []string
	}{
		{name: "empty backend warns for KVM and trio", failures: []string{"kvm", "cloud-hypervisor", "virtiofsd", "passt"}, wantOutput: defaultWarnings},
		{name: "microvm KVM failure is hard", backend: "microvm", failures: []string{"kvm"}, wantOutput: strings.ReplaceAll(microVMAllPass, "[PASS] kvm", "[FAIL] kvm"), wantFailed: []string{"kvm"}},
		{name: "microvm tool failures are hard", backend: "microvm", failures: []string{"cloud-hypervisor", "virtiofsd", "passt"}, wantOutput: microVMFailures, wantFailed: []string{"cloud-hypervisor", "virtiofsd", "passt"}},
		{name: "microvm all checks pass", backend: "microvm", wantOutput: microVMAllPass},
		{name: "podman failure is hard by default", failures: []string{"podman"}, wantOutput: podmanFailure, wantFailed: []string{"podman"}},
		{name: "secretspec failure is hard by default", failures: []string{"secretspec"}, wantOutput: secretspecFailure, wantFailed: []string{"secretspec"}},
		{name: "podman backend matches empty backend", backend: "podman", failures: []string{"kvm", "cloud-hypervisor", "virtiofsd", "passt"}, wantOutput: defaultWarnings},
		{name: "podman and secretspec failures stay hard for microvm", backend: "microvm", failures: []string{"podman", "secretspec"}, wantOutput: strings.ReplaceAll(strings.ReplaceAll(secretspecFailure, "[PASS] podman", "[FAIL] podman"), "[PASS] secretspec", "[FAIL] secretspec"), wantFailed: []string{"podman", "secretspec"}},
		{name: "apple-container prerequisites are advisory", backend: "apple-container", failures: []string{"kvm", "cloud-hypervisor", "virtiofsd", "passt"}, wantOutput: defaultWarnings},
		{name: "host prerequisites are advisory", backend: "host", failures: []string{"kvm", "cloud-hypervisor", "virtiofsd", "passt"}, wantOutput: defaultWarnings},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			lines, failed := preflightLines(tt.backend, preflightTestResults(tt.failures...))
			if got := strings.Join(lines, "\n"); got != tt.wantOutput {
				t.Errorf("preflightLines(%q) output =\n%s\nwant:\n%s", tt.backend, got, tt.wantOutput)
			}
			if !slices.Equal(failed, tt.wantFailed) {
				t.Errorf("preflightLines(%q) failed = %#v, want %#v", tt.backend, failed, tt.wantFailed)
			}
		})
	}
}

func preflightTestResults(failures ...string) []preflightCheck {
	checks := make([]preflightCheck, 0, 6)
	for _, name := range []string{"kvm", "podman", "cloud-hypervisor", "virtiofsd", "passt", "secretspec"} {
		checks = append(checks, preflightCheck{
			result:  hostcheck.Result{Name: name, OK: !slices.Contains(failures, name), Detail: "detail"},
			microVM: name == "kvm" || name == "cloud-hypervisor" || name == "virtiofsd" || name == "passt",
		})
	}
	return checks
}

// TestPreflightReportsSecretSpec pins the wiring and check order. PATH is
// empty and microVM is explicit so every check fails and the error includes
// secretspec after the microVM trio.
func TestPreflightReportsSecretSpec(t *testing.T) {
	t.Setenv("PATH", t.TempDir())

	err := runPreflight(t.Context(), []string{"--runtime-backend", "microvm"})
	if err == nil {
		t.Fatal("expected preflight to fail with an empty PATH, got nil")
	}
	got := err.Error()
	if !strings.Contains(got, "secretspec") {
		t.Fatalf("preflight failure %q does not name the secretspec check; the install-time server-secrets dependency is unreported", got)
	}
	// The trio is printed as a group; secretspec is appended after it.
	if last := strings.LastIndex(got, "passt"); last > strings.Index(got, "secretspec") {
		t.Errorf("preflight failure %q reports secretspec before the microVM trio; the trio must stay grouped first", got)
	}
}
