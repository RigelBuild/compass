//go:build unix

package runner

// ResolveEgress: the per-backend egress policy that lives beside the uid
// resolution. A backend that cannot enforce egress carries no policy at all,
// because provision refuses one that reaches an unenforceable tier — but an
// explicit allowlist is operator intent this tier cannot honor, so it fails
// startup rather than being discarded.

import (
	"strings"
	"testing"

	"github.com/RigelBuild/compass/go/internal/runtime"
)

// unenforceableBackend is a WorkloadRuntime implementing the egressUnenforcer
// capability, standing in for the host backend without spawning host children.
type unenforceableBackend struct {
	*pipeRuntime
	unenforced bool
}

func (b unenforceableBackend) EgressUnenforced() bool { return b.unenforced }

// TestResolveEgressDropsTheDefaultPolicyOnAnUnenforceableBackend: the parsed
// default is a real configured policy even with an empty allowlist, and
// provision refuses any configured policy on this tier. So the resolver must
// hand back an unconfigured one, or every host launch would fail.
func TestResolveEgressDropsTheDefaultPolicyOnAnUnenforceableBackend(t *testing.T) {
	parsed, err := runtime.AllowEgress()
	if err != nil {
		t.Fatalf("AllowEgress() error = %v", err)
	}
	if !parsed.Configured() {
		t.Fatal("precondition: the parsed default must be a configured policy")
	}

	resolved, err := ResolveEgress(unenforceableBackend{pipeRuntime: newPipeRuntime(), unenforced: true}, parsed)
	if err != nil {
		t.Fatalf("ResolveEgress with no allowlist = %v, want success", err)
	}
	if resolved.Configured() {
		t.Error("resolved policy: Configured() = true, want false (an unenforceable tier must carry no policy)")
	}
}

// TestResolveEgressRefusesAnExplicitAllowlistOnAnUnenforceableBackend: a
// non-empty allowlist is explicit intent to confine egress. Zeroing it would
// deliver the opposite of what the operator asked for, silently, so startup
// fails and names both the count and the remedy.
func TestResolveEgressRefusesAnExplicitAllowlistOnAnUnenforceableBackend(t *testing.T) {
	parsed := runtime.MustAllowEgress("github.com", "api.anthropic.com")

	_, err := ResolveEgress(unenforceableBackend{pipeRuntime: newPipeRuntime(), unenforced: true}, parsed)
	if err == nil {
		t.Fatal("ResolveEgress with an allowlist on an unenforceable backend: err = nil, want refusal")
	}
	if !strings.Contains(err.Error(), "cannot enforce an egress allowlist") {
		t.Errorf("error %q does not contain \"cannot enforce an egress allowlist\"", err.Error())
	}
	if !strings.Contains(err.Error(), "select a container backend") {
		t.Errorf("error %q does not contain \"select a container backend\" — the remedy must be actionable", err.Error())
	}
}

// TestResolveEgressLeavesAContainerBackendUntouched pins the byte-identical
// claim at this seam: a backend without the capability keeps the operator's
// policy exactly, allowlist and configured flag alike.
func TestResolveEgressLeavesAContainerBackendUntouched(t *testing.T) {
	parsed := runtime.MustAllowEgress("github.com")

	resolved, err := ResolveEgress(newPipeRuntime(), parsed)
	if err != nil {
		t.Fatalf("ResolveEgress on a container backend = %v, want success", err)
	}
	if !resolved.Configured() {
		t.Error("container backend: Configured() = false, want true")
	}
	if got := resolved.Hosts(); len(got) != 1 || got[0] != "github.com" {
		t.Errorf("container backend: Hosts() = %q, want [github.com]", got)
	}
}

// TestResolveEgressHonorsAnUnenforcerReportingFalse: the capability is a
// question, not a type tag. A backend implementing it but answering false is an
// enforcing backend, so its policy must survive.
func TestResolveEgressHonorsAnUnenforcerReportingFalse(t *testing.T) {
	parsed := runtime.MustAllowEgress("github.com")

	resolved, err := ResolveEgress(unenforceableBackend{pipeRuntime: newPipeRuntime(), unenforced: false}, parsed)
	if err != nil {
		t.Fatalf("ResolveEgress = %v, want success", err)
	}
	if !resolved.Configured() || len(resolved.Hosts()) != 1 {
		t.Errorf("a backend reporting EgressUnenforced()=false must keep its policy; Configured()=%v Hosts()=%q",
			resolved.Configured(), resolved.Hosts())
	}
}
