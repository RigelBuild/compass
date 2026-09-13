//go:build unix

package runner

// ResolveWorkspaceUID: the per-backend uid policy that lives beside the non-root
// check. A backend that names its own uid (the host tier) wins; every container
// tier falls back to the baked AgentUID. These moved here from the cmd binary
// when the resolution moved off a concrete type assertion onto a capability
// probe on the runtime.

import (
	"errors"
	"strings"
	"testing"

	"github.com/RigelBuild/compass/go/internal/agentuid"
	"github.com/RigelBuild/compass/go/internal/runtime"
)

// fixedUIDBackend is a WorkloadRuntime that implements the workspaceUIDResolver
// capability with a fixed answer, so the resolver's probe hit (and its error and
// root paths) are exercisable without a real host euid, which this box cannot set
// to 0 or -1.
type fixedUIDBackend struct {
	*pipeRuntime
	uid uint32
	err error
}

func (b fixedUIDBackend) WorkspaceUID() (uint32, error) { return b.uid, b.err }

// TestResolveWorkspaceUIDCapabilityWins: a backend implementing the capability
// has its WorkspaceUID honored, so the resolved uid is the backend's own value,
// never the baked fleet constant. The fake reports a uid distinct from AgentUID
// so the probe-over-default behavior discriminates — a resolver that ignored the
// capability and returned AgentUID would fail here. The real host backend proves
// the euid-capture closure end to end in the cmd package.
func TestResolveWorkspaceUIDCapabilityWins(t *testing.T) {
	const backendUID = 4242
	uid, err := ResolveWorkspaceUID(fixedUIDBackend{pipeRuntime: newPipeRuntime(), uid: backendUID})
	if err != nil {
		t.Fatalf("ResolveWorkspaceUID(capability backend) err = %v, want nil", err)
	}
	if uid != backendUID {
		t.Fatalf("ResolveWorkspaceUID(capability backend) = %d, want the backend's uid %d", uid, backendUID)
	}
	if uid == agentuid.AgentUID {
		t.Fatalf("ResolveWorkspaceUID = %d, must not be the baked fleet constant", uid)
	}
}

// TestResolveWorkspaceUIDContainerTiersKeepConstant: the podman/microvm/
// apple-container tiers do not implement the capability, so the resolver falls
// back to AgentUID byte-identically — their userns remap maps the invoking host
// uid onto the baked 1000.
func TestResolveWorkspaceUIDContainerTiersKeepConstant(t *testing.T) {
	engines := map[string]runtime.WorkloadRuntime{
		"podman":          runtime.NewPodmanCLI(),
		"microvm":         runtime.NewMicroVMRuntime(runtime.MicroVMConfig{}),
		"apple-container": runtime.NewAppleContainerCLI(runtime.AppleContainerConfig{}),
	}
	for name, engine := range engines {
		t.Run(name, func(t *testing.T) {
			uid, err := ResolveWorkspaceUID(engine)
			if err != nil {
				t.Fatalf("ResolveWorkspaceUID(%s) err = %v, want nil", name, err)
			}
			if uid != agentuid.AgentUID {
				t.Fatalf("ResolveWorkspaceUID(%s) = %d, want AgentUID %d unchanged", name, uid, agentuid.AgentUID)
			}
		})
	}
}

// TestResolveWorkspaceUIDPropagatesRefusal: a backend whose WorkspaceUID reports
// the no-syscall case (geteuid unavailable) is refused, not wrapped into a huge
// uid — the resolver returns the backend's error verbatim rather than swallowing
// it and defaulting to AgentUID.
func TestResolveWorkspaceUIDPropagatesRefusal(t *testing.T) {
	refusal := errors.New("host backend: geteuid returned -1, no usable effective uid to run agents as")
	_, err := ResolveWorkspaceUID(fixedUIDBackend{pipeRuntime: newPipeRuntime(), err: refusal})
	if !errors.Is(err, refusal) {
		t.Fatalf("ResolveWorkspaceUID(refusing backend) err = %v, want the backend's refusal", err)
	}
}

// TestResolveWorkspaceUIDRootRefusedAtStartup: a backend resolving to uid 0
// reaches the pre-existing non-root refusal in NewConfigSpecBuilder at startup,
// not at the first provision. The resolver itself returns 0 (root reaches the
// same check the container tiers do); the constructor is what refuses it.
func TestResolveWorkspaceUIDRootRefusedAtStartup(t *testing.T) {
	uid, err := ResolveWorkspaceUID(fixedUIDBackend{pipeRuntime: newPipeRuntime(), uid: 0})
	if err != nil {
		t.Fatalf("ResolveWorkspaceUID(uid=0) err = %v, want nil (the non-root check refuses it)", err)
	}
	if uid != 0 {
		t.Fatalf("ResolveWorkspaceUID(uid=0) = %d, want 0", uid)
	}
	d := goodDefaults()
	d.UID = uid
	if _, err := NewConfigSpecBuilder(d); err == nil {
		t.Fatal("NewConfigSpecBuilder with a root uid err = nil, want a startup refusal")
	} else if !strings.Contains(err.Error(), "non-root") {
		t.Fatalf("error %q is not the non-root refusal", err)
	}
}
