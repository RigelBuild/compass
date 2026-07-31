//go:build unix

package main

import (
	"slices"
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

// egressWithS3 merges the S3 endpoint's host into the default-deny allowlist, so
// the agent can reach the object store it persists its session log to — without
// hand-widening the CSV allowlist at deploy time. The endpoint host must appear
// in the policy alongside the CSV hosts; a bug that dropped it leaves the
// firewall silently blocking every persistence write.
func TestEgressWithS3AllowsEndpointHostAlongsideCSV(t *testing.T) {
	policy, err := egressWithS3("api.example.com, cache.example.com", "https://s3.example:9000")
	if err != nil {
		t.Fatalf("egressWithS3 = %v", err)
	}
	for _, want := range []string{"api.example.com", "cache.example.com", "s3.example"} {
		if !slices.Contains(policy.Hosts(), want) {
			t.Fatalf("egress hosts %v missing %q", policy.Hosts(), want)
		}
	}
}

// An empty S3 endpoint adds no host — the dev path, persistence off — so the
// policy is exactly the CSV allowlist and nothing extra sneaks in.
func TestEgressWithS3EmptyEndpointAddsNothing(t *testing.T) {
	got, err := egressWithS3("api.example.com", "")
	if err != nil {
		t.Fatalf("egressWithS3 = %v", err)
	}
	want := runtime.MustAllowEgress("api.example.com")
	if !slices.Equal(got.Hosts(), want.Hosts()) {
		t.Fatalf("egress hosts = %v, want exactly the CSV hosts %v", got.Hosts(), want.Hosts())
	}
}

// A set endpoint that yields no host is a misconfiguration: egressWithS3 must
// error rather than build a firewall that silently blocks every persistence
// write. A bare scheme-relative or empty-host URL is the failure mode.
func TestEgressWithS3RejectsHostlessEndpoint(t *testing.T) {
	if _, err := egressWithS3("", "http://%zz"); err == nil {
		t.Fatal("egressWithS3 with an unparseable endpoint = nil error, want a failure")
	}
	if _, err := egressWithS3("", "/just/a/path"); err == nil {
		t.Fatal("egressWithS3 with a hostless endpoint = nil error, want a failure")
	}
}
