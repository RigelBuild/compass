package runtime

// The microVM WorkloadRuntime backend seam: the operator config, the
// MicroVMRuntime type + per-session state table, and config-driven backend
// selection. Lifecycle bodies live in microvm_lifecycle.go behind //go:build
// unix; this file holds only what backend selection needs on any platform.

import (
	"fmt"
	"strings"
	"sync"

	"connectrpc.com/connect/v2"
)

// MicroVMConfig is the operator-supplied wiring for the microVM backend: the
// paths to the VMM and virtiofs daemon binaries, the guest boot images, the
// per-session runtime-dir root, and the default guest sizing. Empty fields are
// tolerated at construction — the values are consumed when a session boots, and
// the V5 preflight names any missing one at startup rather than deep in a
// launch.
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
	// InitrdImage is the path to the guest initramfs image. Load-bearing, not
	// optional: the pinned generic kernel ships its virtio/erofs/overlay drivers
	// as modules, so the initrd is what loads them and mounts the root before
	// switch_root (microvm-v2a §(a)).
	InitrdImage string
	// ImageManifest optionally verifies guest images against a sha256sum-format manifest.
	// Without it, preflight checks presence/readability only; '*' basenames are ambiguous.
	ImageManifest string
	// RunRoot is the root under which each session's runtime dir is created
	// (<RunRoot>/microvm/<session>/), holding that session's AF_UNIX sockets —
	// the layout V7 formalizes with pidfiles.
	RunRoot string
	// DefaultCPUs is the vCPU count each session guest boots with (hotplug-grown
	// later per D5). Zero leaves it to the VMM's own default.
	DefaultCPUs int
	// DefaultMemoryMB is the RAM each session guest boots with, in MiB
	// (hotplug-grown later per D5). Zero leaves it to the VMM's own default.
	DefaultMemoryMB int
	// QuotaRequired makes preflight verify an operator-assigned quota, never assign one.
	// It applies to the session-volume filesystem; unset logs utilization only.
	QuotaRequired bool
	// VolumeRoot identifies the workspace-volume filesystem quota preflight must inspect.
	// RunRoot may be elsewhere to keep AF_UNIX socket paths short; required quota needs this path.
	VolumeRoot string
	// GuestClientInterceptors are passed to guest control clients except Health.
	GuestClientInterceptors []connect.ClientInterceptor
}

// BackendConfig selects and configures the workload runtime backend. Backend
// is the chosen backend name ("podman", "microvm" or "apple-container");
// MicroVM and AppleContainer carry the backend-specific wiring, each consulted
// only when Backend selects it.
type BackendConfig struct {
	// Backend names the runtime backend: "podman" (or empty, the default),
	// "microvm", or "apple-container".
	Backend string
	// MicroVM configures the microVM backend; ignored for the others.
	MicroVM MicroVMConfig
	// AppleContainer configures the apple-container backend; ignored for the
	// others. Appended, never reordered.
	AppleContainer AppleContainerConfig
}

// MicroVMRuntime is a WorkloadRuntime that isolates each agent in its own
// microVM instead of a rootless container. It holds the operator wiring plus a
// per-session state table (keyed by the WorkloadID Create mints), guarded by
// mu against concurrent lifecycle calls. Its method bodies live in
// microvm_lifecycle.go (//go:build unix); the microvmSession type they operate
// on is declared there too.
type MicroVMRuntime struct {
	config MicroVMConfig
	mu     sync.Mutex
	// sessions maps each live WorkloadID to its session state. Every read and
	// write is guarded by mu. Name lookups (Exists, duplicate-name refusal) scan
	// this map for a matching spec.Name — a scan is cheap at one-VM-per-session
	// scale and keeps a single source of truth.
	sessions map[WorkloadID]*microvmSession
	// Tests replace these guest launch and dial seams to avoid starting a VMM or dialing vsock.
	launchFunc     guestLaunchFunc
	newGuestClient guestClientFunc
}

// NewMicroVMRuntime builds a MicroVMRuntime from the supplied config, mirroring
// NewPodmanCLI's shape, with an empty session table ready for Create.
func NewMicroVMRuntime(cfg MicroVMConfig) *MicroVMRuntime {
	m := &MicroVMRuntime{
		config:   cfg,
		sessions: make(map[WorkloadID]*microvmSession),
	}
	m.installSeamDefaults()
	return m
}

// AgentImageIrrelevant marks this backend as never consulting the agent OCI
// image. The agent toolchain is packed into the guest root filesystem at build
// time, so Create/Start boot the rootfs/kernel/initrd triple and no code path
// here reads spec.Image. This reports that the field is unread, never that the
// agent is unpinned — the pin is the rootfs image the Runner was given.
func (m *MicroVMRuntime) AgentImageIrrelevant() bool {
	return true
}

// ValidBackend trims and validates a runtime backend name. Empty selects the
// podman default; the other accepted names select explicit runtime backends.
func ValidBackend(name string) (string, error) {
	backend := strings.TrimSpace(name)
	switch WorkloadTier(backend) {
	case "", WorkloadTierPodman, WorkloadTierMicroVM, WorkloadTierAppleContainer, WorkloadTierHost:
		return backend, nil
	default:
		return "", fmt.Errorf("runtime: unknown backend %q: accepted values are \"podman\" (default), \"microvm\", \"apple-container\" and \"host\"", name)
	}
}

// SelectBackend chooses the workload runtime backend from cfg. An empty or
// "podman" backend returns the podman CLI runtime; "microvm" returns the
// microVM runtime; "apple-container" returns the Apple `container` CLI runtime
// (the macOS arm); "host" returns the direct-host-process runtime (the
// single-trust-domain tier). An unknown backend returns an error naming the
// backend and the accepted values.
//
// An unset backend is podman, the permanent self-host entry tier, so it never
// silently switches an operator onto microVM; microVM is the opt-in that
// untrusted multi-tenant operation requires.
func SelectBackend(cfg BackendConfig) (WorkloadRuntime, error) {
	backend, err := ValidBackend(cfg.Backend)
	if err != nil {
		return nil, err
	}
	switch WorkloadTier(backend) {
	case "", WorkloadTierPodman:
		return NewPodmanCLI(), nil
	case WorkloadTierMicroVM:
		return NewMicroVMRuntime(cfg.MicroVM), nil
	case WorkloadTierAppleContainer:
		return NewAppleContainerCLI(cfg.AppleContainer), nil
	case WorkloadTierHost:
		return NewHostRuntime(defaultHostStateRoot()), nil
	default: // fail closed: an accepted name with no selector must not land on podman
		return nil, fmt.Errorf("runtime: backend %q is accepted but has no selector", backend)
	}
}
