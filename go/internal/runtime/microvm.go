package runtime

// microvm.go is the microVM ContainerRuntime backend seam: a MicroVMRuntime
// that satisfies the same ContainerRuntime interface as PodmanCLI, plus the
// config-driven backend selection the Runner startup uses to choose between
// them. Every runtime method is a typed-error stub here — the in-guest control
// plane that boots a VMM, wires the virtiofs share, and speaks the agent
// protocol over vsock lands later, behind these frozen signatures. Selecting
// the microVM backend today therefore fails loudly at first use rather than
// silently faking container behavior.

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

// MicroVMConfig is the operator-supplied wiring for the microVM backend: the
// paths to the VMM and virtiofs daemon binaries and to the guest kernel and
// rootfs images. Empty fields are tolerated at construction — the values are
// consumed when the in-guest control plane lands, and the V5 preflight names
// any missing one at startup rather than deep in a launch.
type MicroVMConfig struct {
	// VMMPath is the path to the virtual machine monitor binary.
	VMMPath string
	// VirtiofsdPath is the path to the virtiofs daemon binary that serves the
	// guest's shared filesystem.
	VirtiofsdPath string
	// KernelImage is the path to the guest kernel image booted in each microVM.
	KernelImage string
	// RootfsImage is the path to the guest root filesystem image.
	RootfsImage string
}

// BackendConfig selects and configures the container runtime backend. Backend
// is the chosen backend name ("podman" or "microvm"); MicroVM carries the
// microVM-specific wiring, consulted only when Backend selects it.
type BackendConfig struct {
	// Backend names the runtime backend: "podman" (or empty, the transitional
	// default) or "microvm".
	Backend string
	// MicroVM configures the microVM backend; ignored for podman.
	MicroVM MicroVMConfig
}

// ErrMicroVMNotImplemented is returned by every MicroVMRuntime method until the
// in-guest control plane lands. The full ContainerRuntime surface is frozen on
// the type now (so backend selection can choose it and no interface change
// lands later); the VMM boot, virtiofs share, and vsock agent transport behind
// each verb are still to come, so invoking one today is a programming error the
// sentinel names explicitly rather than a silent no-op that would fake a
// container operation that never happened.
var ErrMicroVMNotImplemented = errors.New("runtime: MicroVMRuntime is not implemented until the in-guest control plane lands")

// MicroVMRuntime is a ContainerRuntime that isolates each agent in its own
// microVM instead of a rootless container. It holds the microVM wiring the
// in-guest control plane will consume; its methods are typed-error stubs until
// that lands.
type MicroVMRuntime struct {
	config MicroVMConfig
}

// NewMicroVMRuntime builds a MicroVMRuntime from the supplied config, mirroring
// NewPodmanCLI's shape.
func NewMicroVMRuntime(cfg MicroVMConfig) *MicroVMRuntime {
	return &MicroVMRuntime{config: cfg}
}

var _ ContainerRuntime = (*MicroVMRuntime)(nil)

// Create is unimplemented until the in-guest control plane lands.
func (m *MicroVMRuntime) Create(_ context.Context, _ ContainerSpec) (ContainerID, error) {
	return "", ErrMicroVMNotImplemented
}

// Start is unimplemented until the in-guest control plane lands.
func (m *MicroVMRuntime) Start(_ context.Context, _ ContainerID) error {
	return ErrMicroVMNotImplemented
}

// Exec is unimplemented until the in-guest control plane lands.
func (m *MicroVMRuntime) Exec(_ context.Context, _ ContainerID, _ ExecSpec) (ExecOutput, error) {
	return ExecOutput{}, ErrMicroVMNotImplemented
}

// ExecStreaming is unimplemented until the in-guest control plane lands.
func (m *MicroVMRuntime) ExecStreaming(_ context.Context, _ ContainerID, _ StreamingExecSpec) (*StreamingExec, error) {
	return nil, ErrMicroVMNotImplemented
}

// Stop is unimplemented until the in-guest control plane lands.
func (m *MicroVMRuntime) Stop(_ context.Context, _ ContainerID, _ time.Duration) error {
	return ErrMicroVMNotImplemented
}

// Remove is unimplemented until the in-guest control plane lands.
func (m *MicroVMRuntime) Remove(_ context.Context, _ ContainerID) error {
	return ErrMicroVMNotImplemented
}

// Exists is unimplemented until the in-guest control plane lands.
func (m *MicroVMRuntime) Exists(_ context.Context, _ string) (bool, error) {
	return false, ErrMicroVMNotImplemented
}

// MountLabel is unimplemented until the in-guest control plane lands.
func (m *MicroVMRuntime) MountLabel(_ context.Context, _ ContainerID) (string, error) {
	return "", ErrMicroVMNotImplemented
}

// Resize is unimplemented until the in-guest control plane lands.
func (m *MicroVMRuntime) Resize(_ context.Context, _ ContainerID, _ ResourceLimits) error {
	return ErrMicroVMNotImplemented
}

// SelectBackend chooses the container runtime backend from cfg. An empty or
// "podman" backend returns the podman CLI runtime; "microvm" returns the
// microVM runtime; any other value is an error naming the unknown backend and
// the accepted values.
//
// During the transitional period both backends ship and the default is podman:
// the proven container path stays the floor while the microVM backend is
// brought up, so an unset backend never silently switches an operator onto the
// unfinished path. Once the microVM backend is the sole runtime, the default
// collapses to microVM guarded by a VerifyMicroVMSupport hard gate at startup —
// a legible refusal when the host cannot run microVMs, with no fallback to the
// container path.
func SelectBackend(cfg BackendConfig) (ContainerRuntime, error) {
	switch strings.TrimSpace(cfg.Backend) {
	case "", "podman":
		return NewPodmanCLI(), nil
	case "microvm":
		return NewMicroVMRuntime(cfg.MicroVM), nil
	default:
		return nil, fmt.Errorf("runtime: unknown backend %q: accepted values are \"podman\" (default) and \"microvm\"", cfg.Backend)
	}
}
