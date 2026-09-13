//go:build unix

// The config-driven SpecBuilder: it assembles a launchable runtime.AgentSpec
// from operator-supplied defaults (the image, the default-deny egress allowlist,
// the workspace layout). It is the production SpecBuilder the Runner binary
// wires; the per-agent-account credential and egress derivation that later tiers
// add plugs into the same SpecBuilder seam without changing Provision.
package runner

import (
	"errors"
	"fmt"
	"strings"

	compassv1 "github.com/RigelBuild/compass/go/gen/compass/v1"
	"github.com/RigelBuild/compass/go/internal/agentuid"
	"github.com/RigelBuild/compass/go/internal/runtime"
)

// SpecDefaults are the operator-provisioned, request-independent parts of an
// AgentSpec: the image every agent container runs, the default-deny egress
// allowlist, the in-container checkout/home layout + agent uid, and any read-only
// host mounts (e.g. a bare-repo mirror cache). Everything here is policy set once
// at Runner startup.
type SpecDefaults struct {
	Image string
	// ImageIrrelevant records that the selected backend never reads Image, so an
	// empty Image is a valid configuration rather than a missing field.
	ImageIrrelevant bool
	Egress          runtime.EgressPolicy
	CheckoutDir     string
	HomeDir         string
	UID             uint32
	Mounts          []runtime.Mount
	// NamePrefix prefixes the derived container name so containers are
	// identifiable per Runner/agent; the agent account id is appended.
	NamePrefix string
}

// configSpecBuilder builds specs from fixed defaults + the request.
type configSpecBuilder struct {
	defaults SpecDefaults
}

// NewConfigSpecBuilder returns a SpecBuilder that assembles each AgentSpec from
// defaults plus the provision request. Returns an error if the defaults are
// incomplete (no image or no checkout dir), so a misconfigured Runner fails at
// startup rather than at the first provision.
func NewConfigSpecBuilder(defaults SpecDefaults) (SpecBuilder, error) {
	if defaults.Image == "" && !defaults.ImageIrrelevant {
		return nil, errors.New("spec defaults require an image")
	}
	// The pair encodes one fact, so a contradiction would carry a stale image
	// into every spec on a backend that declared the field unread.
	if defaults.ImageIrrelevant && defaults.Image != "" {
		return nil, errors.New("spec defaults declare the agent image irrelevant but also set one")
	}
	if defaults.CheckoutDir == "" || defaults.HomeDir == "" {
		return nil, errors.New("spec defaults require checkout and home dirs")
	}
	// The other operand of the container name. validAccountID constrains the
	// request-derived half; this constrains the operator-derived half, so both
	// inputs to a path segment are checked and a separator here cannot escape
	// RuntimeDir through the same filepath.Join clean.
	if strings.Contains(defaults.NamePrefix, "/") {
		return nil, errors.New("spec defaults name prefix must not contain a path separator")
	}
	if defaults.UID == 0 {
		return nil, errors.New("spec defaults require a non-root uid")
	}
	// Length is a separate property from shape, and the budget depends on it.
	// The Runner's startup socket-path budget (validateRuntimeDir) models the
	// container name as AgentContainerNamePrefix + a 32-char account id. A
	// longer prefix would build a path wider than the budget cleared, so the
	// runtime dir would pass at boot and the socket would then fail EINVAL at
	// bind — the exact failure the budget check exists to prevent. Reject the
	// prefix here instead, at the same startup edge, so the model stays true.
	if len(defaults.NamePrefix) > len(AgentContainerNamePrefix) {
		return nil, fmt.Errorf(
			"spec defaults name prefix %q (%d bytes) exceeds the %d bytes the agent socket path budget reserves for it",
			defaults.NamePrefix, len(defaults.NamePrefix), len(AgentContainerNamePrefix))
	}
	return &configSpecBuilder{defaults: defaults}, nil
}

// workspaceUIDResolver is the backend capability of naming the uid its agent
// workspaces run as. The host backend implements it (it runs agents as direct
// children under the Runner's own euid, so the uid is that euid); the container
// tiers do not, because their userns remap maps the invoking host uid onto the
// baked fleet constant, so they need no per-Runner uid.
type workspaceUIDResolver interface {
	WorkspaceUID() (uint32, error)
}

// Compile-time regression guard, mirroring the preflight probes' assertions:
// the binding is structural and cross-package, so a signature drift on either
// side would otherwise fall through to the AgentUID default below — silently,
// late (at first provision), and invisibly on a euid-1000 box.
var _ workspaceUIDResolver = (*runtime.HostRuntime)(nil)

// ResolveWorkspaceUID resolves the uid every agent workspace runs as, keyed off
// the resolved engine. A backend that names its own uid (the host tier) wins;
// every other backend falls back to agentuid.AgentUID.
//
// Unlike verifyBackendPreflight's fail-closed default, an unrecognized backend
// here is NOT an error: the container tiers legitimately do not implement this
// capability, so AgentUID is the correct, deliberate default for them — not an
// oversight. The result feeds NewConfigSpecBuilder's non-root check below, which
// refuses a root uid at startup whichever branch produced it.
func ResolveWorkspaceUID(engine runtime.WorkloadRuntime) (uint32, error) {
	if r, ok := engine.(workspaceUIDResolver); ok {
		return r.WorkspaceUID()
	}
	return agentuid.AgentUID, nil
}

// egressUnenforcer is the backend capability of declaring that it cannot
// constrain egress. The host backend implements it (a host child shares the
// host's network namespace, so there is no boundary to firewall); the container
// tiers do not, because each has a netns of its own to arm.
type egressUnenforcer interface {
	EgressUnenforced() bool
}

// Compile-time regression guard, mirroring workspaceUIDResolver above: a
// signature drift would otherwise silently restore the configured policy below
// and fail every host launch at first provision instead of at startup.
var _ egressUnenforcer = (*runtime.HostRuntime)(nil)

// ResolveEgress decides the egress policy the Runner's specs carry. A backend
// that cannot enforce egress gets the zero-value policy — deliberately
// unconfigured, because AgentRuntime.provision refuses any policy that reaches
// an unenforceable tier, and the operator's parsed default is a real policy even
// when the allowlist is empty.
//
// A non-empty allowlist is different: it is explicit operator intent to confine
// egress, and this tier cannot. Zeroing it would silently deliver the opposite
// of what was asked, so startup fails instead. Every other backend keeps the
// parsed policy untouched.
func ResolveEgress(engine runtime.WorkloadRuntime, parsed runtime.EgressPolicy) (runtime.EgressPolicy, error) {
	u, ok := engine.(egressUnenforcer)
	if !ok || !u.EgressUnenforced() {
		return parsed, nil
	}
	if hosts := parsed.Hosts(); len(hosts) > 0 {
		return runtime.EgressPolicy{}, fmt.Errorf(
			"this backend cannot enforce an egress allowlist, but %d host(s) were allowlisted: it runs agents as host processes sharing the host network namespace, so drop the allowlist to run here, or select a container backend to keep it",
			len(hosts))
	}
	return runtime.EgressPolicy{}, nil
}

// imageIrrelevant is the backend capability of declaring that it never consults
// the agent OCI image. The microVM backend implements it: the agent toolchain is
// packed into the guest root filesystem at build time, so the boot path reads
// the rootfs/kernel/initrd triple and never spec.Image.
type imageIrrelevant interface {
	AgentImageIrrelevant() bool
}

// Compile-time regression guard, mirroring egressUnenforcer above: a signature
// drift would otherwise silently restore the required-image rule below and fail
// every microVM Runner at startup.
var _ imageIrrelevant = (*runtime.MicroVMRuntime)(nil)

// ResolveAgentImage decides the agent image the Runner's specs carry, and
// reports whether the backend reads it at all. A backend that never reads the
// image yields ("", true), so startup does not demand an operator value the
// boot path cannot use.
//
// A configured image is different: it is explicit operator intent to pin the
// agent, and on this backend the pin lives in the guest rootfs the Runner was
// given, not in an OCI ref. Ignoring it would leave the operator believing a
// pin is in force that nothing reads, so startup fails instead. Every other
// backend requires the image exactly as before.
func ResolveAgentImage(engine runtime.WorkloadRuntime, configured string) (string, bool, error) {
	i, ok := engine.(imageIrrelevant)
	if !ok || !i.AgentImageIrrelevant() {
		if configured == "" {
			return "", false, errors.New("an agent image is required: pass --image or set $COMPASS_AGENT_IMAGE")
		}
		return configured, false, nil
	}
	if configured != "" {
		return "", false, errors.New(
			"this backend runs the agent from the guest root filesystem and cannot apply an agent image: the agent version is pinned by the --microvm-rootfs image, so drop --image/$COMPASS_AGENT_IMAGE to run here, or select a container backend to pin the agent by OCI reference")
	}
	return "", true, nil
}

// BuildSpec maps the request's agent account onto a full AgentSpec, filling
// image/egress/workspace-layout from the defaults.
func (b *configSpecBuilder) BuildSpec(req *compassv1.ProvisionAgentWorkspaceRequest) (runtime.AgentSpec, error) {
	d := b.defaults
	accountID := req.GetAgentHandle()
	if err := validAccountID(accountID); err != nil {
		return runtime.AgentSpec{}, err
	}
	name := d.NamePrefix + accountID
	return runtime.AgentSpec{
		Name:  name,
		Image: d.Image,
		Workspace: runtime.Workspace{
			CheckoutDir: d.CheckoutDir,
			HomeDir:     d.HomeDir,
			UID:         d.UID,
		},
		Egress:         d.Egress,
		Mounts:         d.Mounts,
		Persona:        req.GetPersona(),
		Role:           req.GetRole(),
		AgentAccountID: accountID,
	}, nil
}

// validAccountID refuses an agent account id that is not a fixed-width lowercase
// hex string — exactly agentAccountIDWidth (32) characters, each in [0-9a-f].
// This is the exact shape the server mints (16 random bytes hex-encoded,
// store/ids.go newID), so a well-formed request always passes.
//
// The id is not merely a label: it is concatenated into the container name
// (spec.go BuildSpec) and that name becomes a path segment of the agent socket,
// RuntimeDir/containers/<container>/agent.sock (host.go). A fixed-width hex
// string cannot carry a "/", a "..", a ".", a control or format character, or
// invalid UTF-8, so it cannot escape RuntimeDir nor forge a container name an
// operator reads back from `podman ps` or the Runner's logs. Constraining the
// id to its minted shape here — the hop before hub.Provision creates the 0700
// directory and binds the socket — is what keeps that path segment safe.
//
// The width is asserted here on purpose: run.go's validateRuntimeDir derives the
// startup socket-path budget from agentAccountIDWidth, so an id of any other
// width would break the model the budget check is built on. Nothing upstream
// makes this check redundant — the foreign key that ties the id to a real
// account is enforced later by RecordAgentContainer, and the admin-only RPC
// narrows who calls Provision, not what they may pass.
func validAccountID(id string) error {
	if id == "" {
		return errors.New("provision request requires an agent account id")
	}
	if len(id) != agentAccountIDWidth {
		return fmt.Errorf(
			"agent account id %q is %d characters, not the required %d",
			id, len(id), agentAccountIDWidth)
	}
	if strings.ContainsFunc(id, func(r rune) bool {
		return (r < '0' || r > '9') && (r < 'a' || r > 'f')
	}) {
		return fmt.Errorf("agent account id %q is not lowercase hex ([0-9a-f])", id)
	}
	return nil
}
