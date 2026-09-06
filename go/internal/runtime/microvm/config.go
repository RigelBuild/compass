// Package microvm is the host-side microVM boot harness (design
// docs/designs/infra/runtime/compass-elastic-session-runtime/
// microvm-v2a-guest-image-boot-spike.md, RIG-2588). It boots one session guest
// under cloud-hypervisor and dials the in-guest GuestControl plane over the
// hybrid vsock. It is reached only by its own tests today and (later) by V2b's
// MicroVMRuntime; it depends on nothing in go/internal/runtime, so importing it
// there introduces no cycle.
package microvm

import "strconv"

// NetConfig wires the userspace net backend (D6: passt) to the VMM.
type NetConfig struct {
	// VhostUserSocket is the AF_UNIX path passt serves with --vhost-user
	// and CH consumes via --net vhost_user=true,socket=….
	VhostUserSocket string
	// MAC is the guest interface MAC handed to --net mac=….
	MAC string
}

// BootConfig is everything needed to boot one session guest. Produced by the
// backend (V2b) or the test harness (V2a); consumed by Launch.
type BootConfig struct {
	Kernel      string // bzImage path (compass-guest-kernel/bzImage)
	Initrd      string // initramfs path (compass-guest-initrd) — new vs the sketch: required because the pinned kernel's virtio drivers are modules ((a))
	Rootfs      string // erofs image path (compass-guest-rootfs)
	Cmdline     string // kernel cmdline; Launch appends the vsock-port + console parameters
	GatewayPort uint32 // fixed guest port the host serves the AgentGateway on over the suffixed vsock socket; zero ⇒ Launch omits the compass.gateway_port cmdline parameter (harness compatibility, record §(b)/§(d))
	VsockCID    uint32 // guest CID (>= 3)
	VsockPort   uint32 // guest port guestd listens on
	VsockSocket string // host AF_UNIX path for --vsock socket=… (the hybrid endpoint the host dials)
	FSTag       string // virtio-fs tag ("workspace")
	FSSocket    string // virtiofsd --socket-path the VMM attaches via --fs
	FSSharedDir string // virtiofsd --shared-dir: the host tree exported as FSTag→/workspace (per-session checkout dir in V2b; a throwaway temp dir in the V2a spike)
	// AgentUID is the in-guest uid the workload runs as (uid==gid, guestd's
	// linuxCredential). virtiofsd maps it back to the INVOKING host user's
	// (uid,gid) inside its user namespace, so a file the guest agent writes on
	// the share lands host-side owned by the invoking user — the same host
	// ownership podman's `--userns=keep-id:uid=N,gid=N` produces (record §(d),
	// host-ownership parity). Zero disables the mapping (the V2a spike harness,
	// which shares a throwaway dir and asserts nothing about ownership).
	AgentUID uint32
	CPUs     int
	MemoryMB int // always launched with shared=on ((c))
	Net      NetConfig
}

// GatewaySocketPath is the host-side AF_UNIX listener path a guest reaches by
// dialing AF_VSOCK (CID 2, port): cloud-hypervisor's hybrid vsock connects the
// guest's dial to the launch-time --vsock socket path with an appended "_" and
// the guest-side port (record §(a), CH docs/vsock.md "Connecting from Guest to
// Host"). It is the one V4-specific derivation on the host serving path; the
// listener itself is a plain AF_UNIX socket gateway.Serve binds unchanged.
func GatewaySocketPath(vsockSocket string, port uint32) string {
	return vsockSocket + "_" + strconv.FormatUint(uint64(port), 10)
}
