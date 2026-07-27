//go:build unix

package main

import (
	"strings"
	"testing"
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
