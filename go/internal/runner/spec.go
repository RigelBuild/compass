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

	compassv1 "github.com/sealedsecurity/compass/go/gen/compass/v1"
	"github.com/sealedsecurity/compass/go/internal/runtime"
)

// SpecDefaults are the operator-provisioned, request-independent parts of an
// AgentSpec: the image every agent container runs, the default-deny egress
// allowlist, the in-container checkout/home layout + agent uid, and any read-only
// host mounts (e.g. a bare-repo mirror cache). Everything here is policy set once
// at Runner startup.
type SpecDefaults struct {
	Image       string
	Egress      runtime.EgressPolicy
	CheckoutDir string
	HomeDir     string
	UID         uint32
	Mounts      []runtime.Mount
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
	if defaults.Image == "" {
		return nil, errors.New("spec defaults require an image")
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

// BuildSpec maps the request's agent account onto a full AgentSpec, filling
// image/egress/workspace-layout from the defaults.
func (b *configSpecBuilder) BuildSpec(req *compassv1.ProvisionAgentWorkspaceRequest) (runtime.AgentSpec, error) {
	d := b.defaults
	accountID := req.GetAgentAccountId()
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
		Egress:  d.Egress,
		Mounts:  d.Mounts,
		Persona: req.GetPersona(),
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
