//go:build unix

package main

import (
	"strings"
	"testing"

	"github.com/sealedsecurity/compass/go/internal/runtime"
)

// The Runner's uid is a load-bearing precondition, not a preference: the agent
// image bakes /nix and $HOME as defaultAgentUID and podman's --userns=keep-id
// maps the host uid through unchanged, so a Runner on any other uid launches an
// agent that cannot write /nix. The guard must reject that at startup, and its
// message must name the invariant — an operator who only sees "permission
// denied" from a nix build three layers down cannot act on it.
func TestVerifyRunnerUID(t *testing.T) {
	if err := verifyRunnerUID(int(defaultAgentUID)); err != nil {
		t.Fatalf("verifyRunnerUID(%d) = %v, want nil", defaultAgentUID, err)
	}

	err := verifyRunnerUID(int(defaultAgentUID) + 1)
	if err == nil {
		t.Fatalf("verifyRunnerUID(%d) = nil, want an error", defaultAgentUID+1)
	}
	for _, want := range []string{"1000", "1001", "/nix", "keep-id"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("verifyRunnerUID error %q does not name %q", err, want)
		}
	}
}

// parseMount is the operator surface for --mount: a malformed value must be
// rejected at flag-parse with a message an operator can act on (it names the bad
// input and the host:container[:ro] shape), and a well-formed value must reach
// SpecDefaults.Mounts intact — the ':ro' suffix is the load-bearing bit that
// makes a mount read-only, so ReadOnly must be exact.
func TestParseMount(t *testing.T) {
	okCases := []struct {
		name string
		in   string
		want runtime.Mount
	}{
		{"read-write", "host:container", runtime.Mount{HostPath: "host", ContainerPath: "container", ReadOnly: false}},
		{"read-only", "/host/mirror:/workspace/mirror:ro", runtime.Mount{HostPath: "/host/mirror", ContainerPath: "/workspace/mirror", ReadOnly: true}},
	}
	for _, tc := range okCases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseMount(tc.in)
			if err != nil {
				t.Fatalf("parseMount(%q) = unexpected error %v, want %+v", tc.in, err, tc.want)
			}
			if got != tc.want {
				t.Errorf("parseMount(%q) = %+v, want %+v", tc.in, got, tc.want)
			}
		})
	}

	errCases := []struct {
		name string
		in   string
	}{
		{"empty", ""},
		{"one field", "host"},
		{"empty container", "host:"},
		{"empty host", ":container"},
		{"bad mode", "host:container:rw"},
		{"four fields", "host:container:ro:extra"},
		{"comma in path", "/ho,st:/container"},
	}
	for _, tc := range errCases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := parseMount(tc.in)
			if err == nil {
				t.Fatalf("parseMount(%q) = nil error, want a rejection", tc.in)
			}
			if !strings.Contains(err.Error(), "host:container[:ro]") {
				t.Errorf("parseMount(%q) error %q does not name the accepted shape host:container[:ro]", tc.in, err)
			}
		})
	}
}
