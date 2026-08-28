package runtime

// microvm.go is the microVM ContainerRuntime backend seam: the operator config,
// the MicroVMRuntime type + its per-session state table, and the config-driven
// backend selection the Runner startup uses to choose between the microVM and
// podman backends. The lifecycle method bodies — which boot a VMM, wire the
// virtiofs share, and speak the guest control plane over vsock — live in
// microvm_lifecycle.go behind a //go:build unix tag, because the microvm
// package they call (Launch/GuestExec/VM) is itself unix-only. This file holds
// only what backend selection needs to type-check on any platform: the config
// structs, the type declaration, and SelectBackend.

import (
	"fmt"
	"strings"
	"sync"
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

// MicroVMRuntime is a ContainerRuntime that isolates each agent in its own
// microVM instead of a rootless container. It holds the operator wiring plus a
// per-session state table (keyed by the ContainerID Create mints), guarded by
// mu against concurrent lifecycle calls. Its method bodies live in
// microvm_lifecycle.go (//go:build unix); the microvmSession type they operate
// on is declared there too.
type MicroVMRuntime struct {
	config MicroVMConfig
	mu     sync.Mutex
	// sessions maps each live ContainerID to its session state. Every read and
	// write is guarded by mu. Name lookups (Exists, duplicate-name refusal) scan
	// this map for a matching spec.Name — a scan is cheap at one-VM-per-session
	// scale and keeps a single source of truth.
	sessions map[ContainerID]*microvmSession
}

// NewMicroVMRuntime builds a MicroVMRuntime from the supplied config, mirroring
// NewPodmanCLI's shape, with an empty session table ready for Create.
func NewMicroVMRuntime(cfg MicroVMConfig) *MicroVMRuntime {
	return &MicroVMRuntime{
		config:   cfg,
		sessions: make(map[ContainerID]*microvmSession),
	}
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
