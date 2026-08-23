// Package microvm is the host-side microVM boot harness (design
// docs/designs/platform/compass-elastic-session-runtime/
// microvm-v2a-guest-image-boot-spike.md, RIG-2588). It boots one session guest
// under cloud-hypervisor and dials the in-guest GuestControl plane over the
// hybrid vsock. It is reached only by its own tests today and (later) by V2b's
// MicroVMRuntime; it depends on nothing in go/internal/runtime, so importing it
// there introduces no cycle.
package microvm

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
	VsockCID    uint32 // guest CID (>= 3)
	VsockPort   uint32 // guest port guestd listens on
	VsockSocket string // host AF_UNIX path for --vsock socket=… (the hybrid endpoint the host dials)
	FSTag       string // virtio-fs tag ("workspace")
	FSSocket    string // virtiofsd --socket-path the VMM attaches via --fs
	FSSharedDir string // virtiofsd --shared-dir: the host tree exported as FSTag→/workspace (per-session checkout dir in V2b; a throwaway temp dir in the V2a spike)
	CPUs        int
	MemoryMB    int // always launched with shared=on ((c))
	Net         NetConfig
}
