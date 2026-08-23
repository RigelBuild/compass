# microVM Runner V2a — Guest Image + Boot Spike

Status: PROPOSED — details the V2a milestone under the frozen parent
[microvm-runner.md](./microvm-runner.md) (its Plan § V2a, microvm-runner.md:443-464).

## Problem / Intent

The microVM backend's schedule-critical risk is not the Go control plane — it
is whether the nix-packaged guest artifacts actually boot: a devenv.lock-pinned
kernel direct-booting under cloud-hypervisor, a rootfs carrying the agent
toolchain closure with a writable `/nix`, rootless userspace networking, and a
working hybrid-vsock channel. V2a de-risks exactly that with minimal code
(microvm-runner.md:411-415): pack the E3 contents closure into a bootable
image, grow the `compass-guestd` stub into a real init that brings up
networking, mounts the virtio-fs workspace, and answers one vsock handshake,
and prove the whole chain with a KVM-gated integration test that boots,
handshakes under a deadline, records boot-latency + RSS (feeding the parent's
(h)/Q-budget, microvm-runner.md:279-287,839-842), and tears down. No Runner
wiring — `MicroVMRuntime`'s methods stay `ErrMicroVMNotImplemented` stubs
(`go/internal/runtime/microvm.go:55`) until V2b.

## Approach

Each subsection resolves one fork the parent deferred to V2a
(`guest-image/default.nix:24-27`). Every decision is also listed in
`## Open Questions` for the pre-freeze batch; the body designs against the
recommended option so the record is coherent.

### (a) Rootfs delivery: read-only erofs image on virtio-blk + whole-root tmpfs overlay

The E3 contents closure (`guest-image/default.nix:124-160`) is packed with
`mkfs.erofs` into a **read-only erofs image**, attached as the boot disk
(cloud-hypervisor `--disk path=…`, virtio-blk,
[docs/device_model.md](https://github.com/cloud-hypervisor/cloud-hypervisor/blob/main/docs/device_model.md)).
The guest's writable view is a **whole-root overlay**: erofs lower + tmpfs
upper/work, assembled in a small initramfs before `switch_root`. This gives
every path — `/etc`, `/var`, `/tmp`, and critically `/nix/store` — copy-on-write
semantics over an immutable, content-addressed image.

Why erofs, and why an initramfs is not optional:

- **The pinned generic kernel ships virtio as modules, not built-ins.** The
  nixpkgs kernel config builds `CONFIG_VIRTIO_BLK=m`, `CONFIG_VIRTIO_PCI=m`,
  `CONFIG_VIRTIO_NET=m`, `CONFIG_VIRTIO_FS=m`, `CONFIG_VIRTIO_VSOCKETS=m`,
  `CONFIG_EROFS_FS=m`, `CONFIG_OVERLAY_FS=m` as loadable modules (nixpkgs sets
  no `=y` override for these in
  `pkgs/os-specific/linux/kernel/common-config.nix`). This is not asserted from
  a running host — T1 makes it self-verifying: a derivation-time check greps the
  packed kernel derivation's `.config` for the exact `=m` set the initramfs
  assumes, so a nixpkgs-pin move that flips a module to `=y` or drops it fails
  the moon gate instead of silently producing a non-booting initrd. A kernel
  that cannot mount its root device without modules **requires an initramfs to
  load them** — with any image format. Building a custom `=y` kernel instead
  would forfeit the substituted-free-from-cache property E3 chose deliberately
  (`guest-image/default.nix:10-17`), so the initramfs is the cheap, pin-honoring
  path. It stays tiny: it carries only the boot-critical set needed to mount the
  root overlay and `switch_root` (`virtio_pci`, `virtio_blk`, `erofs`,
  `overlay`) plus a module-loading + mount init — no busybox userland needed
  beyond what nixpkgs' initrd tooling provides. Every *post*-`switch_root`
  module need (guestd's `virtio_net`/`virtiofs`/vsock transport, and V3's
  in-guest netfilter arm — `nf_tables` alone is not sufficient: `egress.go`'s
  ruleset also pulls `nf_conntrack`/`nft_ct` and the interval/pipapo set
  backends, `go/internal/runtime/egress.go:76-77`) autoloads from the full
  `/lib/modules/<version>` tree the rootfs ships (T1), so the module surface is
  correct for V3 without re-enumeration.
- **Reproducible and verifiable.** `mkfs.erofs` supports fixed timestamps and
  deterministic output (`-T0`/`--all-root`-class flags,
  [erofs-utils](https://git.kernel.org/pub/scm/linux/kernel/git/xiang/erofs-utils.git/about/)),
  so the image derivation is bit-reproducible from the closure — which is what
  lets V5's preflight hash-verify image assets (microvm-runner.md:221-224).
- **Shared, immutable, per-session-free.** One store-path image serves every
  session read-only; per-session writable state costs only tmpfs pages actually
  written. A writable format would force a per-session image copy (see
  Alternatives).

The packed image **is** the `compass-guest-rootfs` attr: the naming-locked attr
stays, its value changes from the format-agnostic tree to the boot-consumable
erofs image, and the E3 tree becomes an internal `let`-binding it packs. This
matches the existing consumer contract — `microvmtest.RootfsEnvVar` is
documented as "the guest rootfs image to boot"
(`go/internal/microvmtest/microvmtest.go:51-54`) and the CI leg exports the
`nix build` result of that attr — so no env-var or moon change is needed for
the rename-free repurposing (`guest-image/moon.yml:46`). A new
`compass-guest-initrd` attr carries the initramfs.

### (b) Writable `/nix`: the whole-root overlay, not a per-mount split

The toolchain's in-guest `nix` needs a writable store (the agent rebuilds its
own devenv in-guest, `agent-image/toolchain.nix:11-14`, single-user nix owned
by the agent uid, `toolchain.nix:47-77`). The whole-root overlay from (a)
covers it: `/nix/store` is writable through the tmpfs upper while the packed
closure stays immutable in the lower. No second mechanism (no separate ext4
`/nix` volume, no bind), so there is exactly one writability story to reason
about, and E3's writable-regular-file `/etc/resolv.conf` requirement
(`guest-image/default.nix:75-88`) is satisfied for free — the upper makes every
file rewritable in-guest.

Tradeoff, stated: upper-layer writes cost guest RAM (tmpfs), so a large
in-guest `nix build` can pressure memory. That is acceptable for the spike and
for inner-loop sessions (the VM grows via D5 hotplug when sized up); a
durable/disk-backed upper is a deliberate deferral to V2b+ (OQ-D, non-blocking
here).

### (c) Net backend: passt, wired as vhost-user-net

D6 fixed the class (virtio-net + passt/gvproxy-class userspace backend,
microvm-runner.md:733-750); V2a picks **passt**, concretely:

- **Already pinned and shipped.** E1's devenv Linux append packages
  `pkgs.passt` beside cloud-hypervisor and virtiofsd, chosen explicitly over
  gvproxy ("C, no in-guest Go runtime, packaged in nixpkgs, and free of
  gvproxy's podman-machine coupling", `devenv.nix:165-186`). Picking gvproxy
  now would contradict a shipped, reviewed decision.
- **The rootless wiring to cloud-hypervisor is vhost-user.** CH's plain
  `--net` creates a host TAP, which needs `CAP_NET_ADMIN`
  ([docs/vhost-user-net-testing.md](https://github.com/cloud-hypervisor/cloud-hypervisor/blob/main/docs/vhost-user-net-testing.md))
  — unavailable rootless. passt ships a vhost-user mode (`--vhost-user`, the
  command socket via `--socket`,
  [passt(1)](https://passt.top/builds/latest/web/passt.1.html)), and CH
  enables vhost-user-net with `--net vhost_user=true,socket=<path>`
  ([docs/device_model.md](https://github.com/cloud-hypervisor/cloud-hypervisor/blob/main/docs/device_model.md)).
  vhost-user requires shared guest memory — `--memory shared=on` — which
  virtio-fs already forces
  ([docs/fs.md](https://github.com/cloud-hypervisor/cloud-hypervisor/blob/main/docs/fs.md),
  [docs/memory.md](https://github.com/cloud-hypervisor/cloud-hypervisor/blob/main/docs/memory.md)),
  so the two devices share one memory config with no extra cost.
- **Addressing is deterministic and host-controlled.** passt's `-a`/`-g`/`-n`/
  `-D` flags fix the address, gateway, netmask, and DNS it offers
  ([passt(1)](https://passt.top/builds/latest/web/passt.1.html)); the harness
  passes those same values to guestd via the kernel cmdline (OQ-C static path),
  which is the input `compass-guestd` needs to write `/etc/resolv.conf` before
  the arm step reads it (`egress.go:112-131`, D6). (passt also ships a built-in
  DHCP/DHCPv6/NDP server, the OQ-C fallback.)

The pinned versions satisfy the wiring: passt `2025_09_19` (vhost-user shipped
upstream well before), cloud-hypervisor `53.0`, virtiofsd `1.14.0` (all
resolved from the root devenv.lock nixpkgs pin).

**But the pairing is the spike's primary unknown, not settled wiring.** Each
leg above is individually documented; the *composition* — passt's
`--vhost-user` backend negotiating vhost-user with cloud-hypervisor's
vhost-user-net frontend — has no published upstream validation. passt's
vhost-user integration work targets QEMU/libvirt
([libvirt passt-as-vhost-user-net series](https://patchew.org/Libvirt/20250213181953.922499-1-laine@redhat.com/)),
and cloud-hypervisor's own vhost-user-net testing targets DPDK backends
([docs/vhost-user-net-testing.md](https://github.com/cloud-hypervisor/cloud-hypervisor/blob/main/docs/vhost-user-net-testing.md));
no CH doc or release note mentions passt. vhost-user is a negotiated protocol
(feature bits, client/server socket roles, memory-region shape), so this is a
real integration risk — exactly what the spike exists to surface, but the
record treats it as an explicit unknown (OQ-G): T4 isolates it with a
**net-only boot smoke** before the full stack, and gvproxy's vhost-user mode is
**pre-declared as the fallback** (with the E1-pin amendment it would require) so
a negotiation failure resolves against a ratified contingency instead of
stalling the milestone on an emergency fork.

### (d) `compass-guestd` real init: boot sequence and fail-closed posture

The E3 `writeCBin` stub (`guest-image/default.nix:60-73`) is replaced by a real
Go binary at `go/cmd/compass-guestd`, still installed at `/sbin/init` and on
PATH (`default.nix:150-152`), per D4 (the in-guest Go supervisor,
microvm-runner.md:704-719). It runs as guest PID 1 (post-`switch_root`) and
executes, in order:

1. **Mount the API filesystems** it needs (`/proc`, `/sys`, `/dev` devtmpfs) —
   the initramfs hands over a bare root.
2. **Bring up networking (D6):** link-up `lo` and the virtio-net interface,
   assign the address, install the default route, and **write
   `/etc/resolv.conf`** — before anything else depends on it
   (microvm-runner.md:95-103,747-748). The address/gateway/netmask/DNS values
   come from the kernel cmdline the harness owns (set to match passt's
   deterministic `-a`/`-g`/`-n`/`-D` launch flags), applied in-process with Go
   `netlink` alone — no in-guest DHCP client and no DHCP-over-vhost-user
   exchange on the critical path (OQ-C; the Go-DHCP-library path is the
   documented fallback if DHCP must be proven in V2a).
3. **Mount the virtio-fs workspace:** `mount(2)` the `workspace` tag at
   `/workspace`, type `virtiofs` (the tag/path contract E3 fixed,
   `guest-image/default.nix:90-113`; the stable-path invariant,
   microvm-runner.md:179-182). Because the guest has **no systemd** — guestd
   *is* init — E3's declarative `workspace.mount` unit is dead weight and is
   **removed** from the rootfs in the same change; the contract it documented
   (tag `workspace` → `/workspace`) moves into guestd.
4. **Serve the handshake:** listen on AF_VSOCK (guest CID, port from the
   kernel cmdline, see (e)) and serve the `GuestControl` Connect/h2c service
   with the one spike RPC, `Health`.
5. **Idle.** The spike guest has no agent to supervise; guestd serves Health
   until the host tears the VMM down.

**Fail-closed:** any step failing logs the cause to the console
(`console=ttyS0`; `CONFIG_SERIAL_8250_CONSOLE=y` is built-in, so early output
needs no module) and exits non-zero — PID 1 death panics the guest, the
handshake never answers, and the host side fails at its deadline and tears
down. `Health` is only served *after* net + mount succeed, so a successful
handshake **is** the proof that D6 bringup and the virtio-fs mount happened —
the same posture as the parent's arm-before-exec gate (microvm-runner.md:139-157),
one milestone earlier.

### (e) The vsock handshake: `GuestControl.Health` over hybrid vsock

The spike proves the exact channel V2b's control plane rides (D1 hybrid vsock;
D4 Connect/h2c):

- **Proto seed.** A new `proto/compass/v1/guest_control.proto` defines
  `service GuestControl` with the single spike RPC:
  `rpc Health(HealthRequest) returns (HealthResponse)` — deliberately the
  subset of V2b's planned surface
  (`Exec`/`ExecStream`/`Signal`/`Provision`/`Health`, and the parent's own
  `Health(HealthRequest)` sketch, microvm-runner.md:478-483,487), so V2b grows
  the service additively with no wire break. Message names are the plain
  `HealthRequest`/`HealthResponse` (no collision in `compass.v1`, and matching
  the parent sketch keeps the buf `RPC_REQUEST/RESPONSE_STANDARD_NAME` rules
  satisfied); the suffix-less `service GuestControl` reuses the existing
  `AgentGateway` precedent — a documented `ignore_only` `SERVICE_SUFFIX`
  carve-out in `buf.yaml` (`buf.yaml:16-36`), not a second naming convention.
  `HealthResponse` carries what the spike must assert: `guestd_version string`,
  `net_provisioned bool`, `workspace_mounted bool`.
- **Guest side.** guestd serves the generated Connect handler over h2c on an
  AF_VSOCK listener (port from cmdline), mirroring the gateway's existing
  h2c-on-a-stream-socket stack (`go/internal/runner/gateway/socket.go:303-306`).
- **Host side.** cloud-hypervisor's vsock is hybrid: the host end is the
  AF_UNIX socket given to `--vsock cid=<cid>,socket=<path>`, and a connection
  is steered by writing a `CONNECT <port>` line before application data
  ([docs/vsock.md](https://github.com/cloud-hypervisor/cloud-hypervisor/blob/main/docs/vsock.md));
  the muxer acks with an `OK <assigned_port>` line before the byte stream goes
  transparent (CH's implementation is a copy of Firecracker's,
  [docs/device_model.md](https://github.com/cloud-hypervisor/cloud-hypervisor/blob/main/docs/device_model.md);
  protocol detail per
  [Firecracker docs/vsock.md](https://github.com/firecracker-microvm/firecracker/blob/main/docs/vsock.md)).
  The host dials with a custom `DialContext` that opens the unix socket,
  writes the preamble, consumes the `OK` line, and hands the connection to an
  h2c Connect client — the seed of V4's gateway transport
  (microvm-runner.md:508-527).

### (f) `BootConfig`: the V2b-consumed contract, finalized

The parent sketched all-string fields (microvm-runner.md:456-460); V2a
finalizes with honest types and the two fields the packaging decisions above
make load-bearing (`Initrd`, `Cmdline`):

```go
// Package go/internal/runtime/microvm.

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
    CPUs        int
    MemoryMB    int    // always launched with shared=on ((c))
    Net         NetConfig
}
```

Type deviations from the sketch (`VsockCID`/`VsockPort` numeric, `Initrd` and
`Cmdline` added, `VsockSocket` split from the CID) are flagged as OQ-E for the
freeze batch.

### (g) The boot-spike harness

A host-side package `go/internal/runtime/microvm` (imported by V2b's
`MicroVMRuntime` later; no import cycle — it depends on nothing in
`go/internal/runtime`) owns `Launch(ctx, BootConfig) (*VM, error)`: start
virtiofsd and passt, build the cloud-hypervisor argv, start it, and expose
`DialGuest` (the (e) dialer), `Shutdown`, and process handles. The KVM-gated
integration test lives beside it (`boot_microvm_test.go`,
`//go:build microvm && unix`), calls `microvmtest.Require(t)` first
(`go/internal/microvmtest/microvmtest.go:96-117`; tag + env pattern per
`canary_microvm_test.go:1,55-56`), boots, asserts a `Health` response with
`net_provisioned && workspace_mounted` under a deadline, records boot latency
(Launch→Health-OK) and, per process (VMM, virtiofsd, passt), **PSS** from
`/proc/<pid>/smaps_rollup` — not summed `VmHWM`: under `--memory shared=on` the
guest RAM is a shared mapping all three map, so `VmHWM` counts those pages in
each and summing double/triple-counts the number feeding the parent's deferred
Q-budget (microvm-runner.md:839-842); PSS divides shared pages among mappers.
Launch also captures the guest serial console (`--serial file=<log>`) into the
test log — a fail-closed PID-1 exit is otherwise an undebuggable deadline
timeout, including the corrupt-rootfs negative case. Each child is spawned with
`SysProcAttr.Pdeathsig = SIGTERM` so a killed test process cannot orphan the
VMM/virtiofsd/passt; teardown in `t.Cleanup` asserts no orphan processes.
`microvmtest` grows two resolutions: a
`COMPASS_TEST_GUEST_INITRD` env var and a PATH lookup for `passt`, the same
shape as the existing kernel/rootfs/VMM resolution
(`microvmtest.go:43-62,133-141`). The numbers are recorded, not budgeted —
Q-budget stays data-deferred (microvm-runner.md:839-842).

## Alternatives considered

### Rootfs format (vs (a) erofs + overlay)

- **Writable ext4 image — rejected.** Boots without overlay machinery, but a
  writable root means a **per-session image copy** (sessions cannot share a
  mutable disk), costing full-closure disk + copy time per boot and breaking
  the hash-verified-immutable-asset property V5's preflight wants
  (microvm-runner.md:221-224). Still needs the initramfs anyway — `virtio_blk`
  and `ext4` are modules in the pinned kernel ((a)) — so it saves nothing on
  boot plumbing either.
- **Initramfs-as-root (whole closure in the initrd) — rejected.** No block
  device at all, and rootfs-over-tmpfs is trivially writable. But the whole
  toolchain closure (nix + bun + git + coreutils…,
  `agent-image/toolchain.nix:127-157`) would be decompressed into guest RAM at
  every boot — hundreds of MB of tmpfs charged against `MemoryMB` before the
  workload runs — and boot time pays the full decompress. Directly worsens
  both numbers the spike exists to measure ((h)/Q-budget). A *minimal*
  initramfs (modules + switch_root only) is what (a) adopts.
- **virtio-fs as root (share the store-path tree directly) — rejected.**
  Skips image packing entirely, but: cloud-hypervisor's `--fs` boot examples
  are disk-rooted, and rootfs-on-virtiofs requires the initramfs to bring up
  the vhost-user-fs transport before mount — strictly more fragile than
  erofs-on-blk; every root I/O then crosses a userspace daemon (boot-latency
  tax on the hot path the spike measures); and it burns a second per-session
  virtiofsd *plus* muddies the isolation story (the session-volume virtiofsd
  is sandboxed to the volume subtree, microvm-runner.md:183-197 — a root
  virtiofsd would need its own posture). The store is immutable content; a
  block image serves it better.
- **squashfs instead of erofs — near-tie, erofs chosen.** Both are read-only,
  compressed, module-available (`CONFIG_SQUASHFS=m`, `CONFIG_EROFS_FS=m` in
  the pinned config). erofs is the direction the container/VM ecosystem is
  consolidating on (designed for immutable-image use; random-access without
  whole-block decompress,
  [erofs kernel docs](https://docs.kernel.org/filesystems/erofs.html)), and
  `erofs-utils` 1.9.2 resolves from the same root pin. No reason to prefer
  squashfs beyond age.

### Writable-/nix mechanism (vs (b) whole-root overlay)

- **Targeted overlays (only `/nix/store`, `/etc`, `/var`) — rejected.** More
  mounts, more enumeration risk (each writable path the toolchain assumes must
  be discovered one boot failure at a time — resolv.conf, nix profiles, bun
  cache, `$HOME`); saves nothing, since an untouched whole-root upper costs ~0.
- **Separate writable ext4 `/nix` volume — rejected.** Reintroduces the
  per-session-copy problem for the store and a second block device, to solve a
  problem the overlay already solves.

### Net backend (vs (c) passt)

- **gvproxy — rejected.** Would work (gvisor-tap-vsock serves QEMU/HyperKit
  and exposes vhost-user-net via its own forks/modes), but E1 already weighed
  and rejected it for the devenv append ("C, no in-guest Go runtime, packaged
  in nixpkgs, and free of gvproxy's podman-machine coupling",
  `devenv.nix:165-186`); it is not in the pinned toolset, and its DHCP/DNS
  story duplicates what passt ships
  ([passt — Services](https://passt.top/passt/about/)).
- **CH native `--net` (TAP) — rejected.** Requires `CAP_NET_ADMIN` on the VMM
  binary to create the TAP
  ([docs/vhost-user-net-testing.md](https://github.com/cloud-hypervisor/cloud-hypervisor/blob/main/docs/vhost-user-net-testing.md)),
  violating "Rootless is hard" (microvm-runner.md:379-385).

### Handshake shape (vs (e) Connect/h2c `Health`)

- **Raw line-protocol ping (echo over vsock) — rejected.** Fewer moving parts
  for the spike, but proves the wrong channel: V2b's control plane is
  Connect/h2c (D4, microvm-runner.md:704-719), and h2c-over-hybrid-vsock (the
  preamble interplay, HTTP/2 prior-knowledge over a unix socket) is precisely
  the risky integration the spike should surface. A throwaway protocol would
  leave that risk for V2b.

## Global Constraints

Every task below inherits these; they restate the parent's binding decisions
in V2a-concrete form.

- **cloud-hypervisor ONLY (D1).** The VMM, virtiofsd, and passt binaries come
  from the devenv.nix Linux append (`devenv.nix:182-186`, shipped E1) — this
  record consumes them and never re-packages or re-pins them. Hybrid vsock:
  host AF_UNIX + `CONNECT <port>` preamble
  ([docs/vsock.md](https://github.com/cloud-hypervisor/cloud-hypervisor/blob/main/docs/vsock.md)).
- **Naming is LOCKED.** `guest-image/`, `compass-guest-kernel`,
  `compass-guest-rootfs`, `compass-guestd` keep their names (ecosystem
  precedent; the parent and E3 already ship them). The one new attr this
  record adds is `compass-guest-initrd`, following the same scheme.
- **Pin discipline (E3's divergence note).** `guest-image/default.nix` feeds
  ROOT's devenv.lock-pinned pkgs to the agent-image toolchain imports —
  deliberate, so the moon gate's inputs track the root pin
  (`guest-image/default.nix:29-36`, `guest-image/moon.yml:60-70`). New
  packing/initrd derivations MUST resolve from the same `pkgs` and any new
  file they import MUST be added to the moon `inputs` globs.
- **The kernel stays the substituted generic nixpkgs kernel.** No custom
  config in V2a (`guest-image/default.nix:14-17`); the module-loading
  initramfs is the compensation. A size/boot-time-optimized kernel remains
  the E3-noted deferral.
- **Rootless is hard (parent GC).** Every spike process — cloud-hypervisor,
  virtiofsd, passt, the Go test — runs as the invoking user; the only
  privilege is being in the `kvm` group to open `/dev/kvm` (E6's udev rule:
  root:kvm 0660). No TAP, no `CAP_NET_ADMIN`, no rootful helper.
- **KVM-gate pattern (D3-adjacent).** Every test that boots carries
  `//go:build microvm && unix` and calls `microvmtest.Require(t)` first
  (`go/internal/microvmtest/microvmtest.go:96-117`); skip-on-absent-KVM,
  hard-fail under `COMPASS_REQUIRE_MICROVM=1` (`microvmtest.go:35-41`). The
  test process needs the `kvm` supplementary group.
- **External-reference gate.** This record and every artifact it plans are
  Compass tracked files: no references to the private monorepo's name, no
  private hostnames, no private-tracker issue slugs (RIG-NNN is fine).
- **Egress prerequisites ride along.** The rootfs keeps `nft`/`getent`/`awk`
  and the writable regular-file `/etc/resolv.conf`
  (`guest-image/default.nix:141-156`; `egress.go:76-77`) — V3 depends on
  them; the packing step must not lose them.
- **No Runner wiring.** `MicroVMRuntime` methods stay
  `ErrMicroVMNotImplemented` (`go/internal/runtime/microvm.go:48-55`); V2a's
  Go code lives in the new `go/internal/runtime/microvm` package +
  `go/cmd/compass-guestd` and is reached only by the KVM-gated test.

## Plan

Tasks are ordered by dependency; each is one reviewer's gate. T2 and T3 are
independent of each other and can run in parallel lanes; T1's packing depends
on T2's binary (its nix build replaces the E3 stub with the real guestd, so
T1's non-guestd parts can start alongside but it completes after T2); T4
integrates everything.

### T1 — image packing: erofs rootfs + module initramfs

Extend `guest-image/default.nix`: fold the E3 contents tree into a
`let`-bound `rootfsTree`, add the erofs packing step, and add the initramfs
derivation. Remove the now-dead `workspace.mount` systemd unit ((d) — the
guest has no systemd; guestd mounts the tag itself). Update the moon `build`
task to also realize `compass-guest-initrd`.

- **Interfaces:** produces nix attrs `compass-guest-kernel` (unchanged:
  `pkgs.linuxPackages.kernel`, bzImage at `${out}/bzImage`,
  `guest-image/default.nix:115-118`), `compass-guest-rootfs` (now: a
  reproducible erofs image file packed from the E3 tree with
  `pkgs.erofs-utils` `mkfs.erofs`, deterministic flags, all-root ownership),
  and NEW `compass-guest-initrd` (a zstd-compressed cpio built with nixpkgs'
  `makeInitrd`-class tooling carrying only the **boot-critical** module set —
  `virtio_pci`, `virtio_blk`, `erofs`, `overlay` — from the SAME kernel
  derivation's modules, plus an init that loads them, mounts erofs root + tmpfs
  overlay, and `switch_root`s to `/sbin/init`; post-`switch_root` module needs
  come from the rootfs, not here). The `compass-guest-rootfs` image also ships
  the kernel derivation's full `/lib/modules/<version>` tree (already in the
  closure, zero extra build) so guestd's `virtio_net`/`virtiofs`/vsock and V3's
  netfilter stack autoload on demand ((a)). Consumes the E3 tree
  (`guest-image/default.nix:124-160`) and the T2 guestd binary as the tree's
  `/sbin/init` (a nix `buildGoModule` of `go/cmd/compass-guestd`, replacing
  the `writeCBin` stub).
- **Test cycle:** `nix build -f default.nix compass-guest-kernel
  compass-guest-rootfs compass-guest-initrd` green under the moon gate
  (`guest-image/moon.yml:46` extended); a derivation-time check that the kernel
  `.config` still carries the exact `=m` module set the initramfs assumes
  ((a)), so a pin move fails the gate instead of producing a non-booting initrd;
  a determinism check (build twice, compare hashes — cheap since the tree is
  fixed); the packed image mounts via T4's boot (the real proof). Moon `inputs`
  extended for any new imports.

### T2 — `compass-guestd` real init

The Go binary per (d): API mounts, static net bringup + resolv.conf, virtio-fs
mount, vsock Connect/h2c `Health` server, fail-closed exits. Lives at
`go/cmd/compass-guestd` with its logic in `go/internal/guestd`.

- **Interfaces:** produces the `compass-guestd` binary (static, Linux-only;
  built by T1's nix packaging and by the module for tests). Consumes the T3
  proto's generated `compassv1connect.GuestControlHandler`; kernel cmdline
  parameters read from `/proc/cmdline` — `compass.vsock_port=<n>`, the net
  4-tuple (`compass.ip`/`compass.gw`/`compass.netmask`/`compass.dns`, set by the
  harness to match passt's `-a`/`-g`/`-n`/`-D` flags, OQ-C), and `console=ttyS0`
  for diagnostics; the virtio-fs tag contract `workspace` → `/workspace`
  (`guest-image/default.nix:90-113` note; the mount is
  `mount("workspace", "/workspace", "virtiofs", …)` per
  [CH docs/fs.md](https://github.com/cloud-hypervisor/cloud-hypervisor/blob/main/docs/fs.md)).
  One new Go dep on the recommended static path — `github.com/vishvananda/netlink`
  for link/addr/route; the vsock listener uses `golang.org/x/sys/unix`
  `AF_VSOCK` (already an indirect dep) or the thin `github.com/mdlayher/vsock`
  convenience. The DHCP fallback (OQ-C) would add `github.com/insomniacslk/dhcp`.
- **Test cycle:** unit tests for the pieces that run hermetically: cmdline
  parsing (net 4-tuple + vsock port), resolv.conf rendering from the parsed DNS
  value, boot-sequence ordering (Health served only after net+mount success —
  fake the two provisioners and assert the gate), fail-closed exit on any step
  error. The in-VM behavior is asserted by T4, not mocked here.

### T3 — `GuestControl` proto seed + hybrid-vsock dialer

The proto and the host-side dial path, both V2b/V4 seeds.

- **Interfaces:** produces `proto/compass/v1/guest_control.proto`
  (`service GuestControl { rpc Health(HealthRequest) returns
  (HealthResponse); }`, response fields per (e), with a `SERVICE_SUFFIX`
  `ignore_only` carve-out per the `AgentGateway` precedent, `buf.yaml:16-36`)
  and its buf-generated
  Go stubs (the existing `buf.gen.yaml` pipeline,
  `go/gen/compass/v1/compassv1connect/`); produces
  `go/internal/runtime/microvm.DialGuest(ctx context.Context, vsockSocket
  string, port uint32) (net.Conn, error)` implementing the `CONNECT
  <port>`/`OK` preamble
  ([CH docs/vsock.md](https://github.com/cloud-hypervisor/cloud-hypervisor/blob/main/docs/vsock.md)),
  plus `GuestClient(cfg BootConfig) compassv1connect.GuestControlClient`
  wrapping it in an h2c HTTP client.
- **Test cycle:** dialer unit test against a fake muxer (a unix-socket
  listener speaking the `CONNECT`/`OK` preamble, then echoing) — asserts
  preamble bytes, OK parsing, error on refusal; a hermetic round-trip of the
  generated `Health` client/handler over the fake transport (no KVM needed).

### T4 — `Launch` harness + the KVM-gated boot spike test

The deliverable (microvm-runner.md:452-454): boot rootless, handshake,
measure, tear down.

- **Interfaces:** produces `go/internal/runtime/microvm.BootConfig` +
  `NetConfig` exactly per (f); `Launch(ctx context.Context, cfg BootConfig)
  (*VM, error)` — starts virtiofsd (`--socket-path=cfg.FSSocket
  --shared-dir=<dir> --sandbox=namespace`), passt (`--vhost-user
  --socket cfg.Net.VhostUserSocket --pid <file>`), then cloud-hypervisor
  (`--kernel cfg.Kernel --initramfs cfg.Initrd --disk path=cfg.Rootfs,readonly=on
  --cmdline … --cpus boot=cfg.CPUs --memory size=<MB>M,shared=on
  --serial file=<console-log> --console off
  --fs tag=cfg.FSTag,socket=cfg.FSSocket
  --net vhost_user=true,socket=cfg.Net.VhostUserSocket,mac=cfg.Net.MAC
  --vsock cid=cfg.VsockCID,socket=cfg.VsockSocket`; flags per
  [docs/fs.md](https://github.com/cloud-hypervisor/cloud-hypervisor/blob/main/docs/fs.md),
  [docs/device_model.md](https://github.com/cloud-hypervisor/cloud-hypervisor/blob/main/docs/device_model.md),
  [docs/vsock.md](https://github.com/cloud-hypervisor/cloud-hypervisor/blob/main/docs/vsock.md));
  `(*VM) Health(ctx) (*HealthResponse, error)` via T3's client;
  `(*VM) Shutdown(ctx) error` (kill VMM, then reap virtiofsd/passt, remove
  sockets); `(*VM) PSS() (map[string]int64, error)` reading `Pss` from
  `/proc/<pid>/smaps_rollup` per process (not summed `VmHWM` — (g)); each child
  spawned with `SysProcAttr.Pdeathsig = SIGTERM`. Consumes T1's images, T2's
  guestd (inside the image),
  T3's dialer, and `microvmtest.Require`'s Env extended with `InitrdImage`
  (`COMPASS_TEST_GUEST_INITRD`) and `PasstPath` (PATH lookup, same shape as
  `microvmtest.go:56-62,133-141`).
- **Test cycle:** `go/internal/runtime/microvm/boot_microvm_test.go`
  (`//go:build microvm && unix`; `microvmtest.Require(t)` first). A **net-only
  boot smoke runs first** (OQ-G): boot with the vhost-user-net device but
  without virtio-fs/vsock, asserting only that the passt×CH vhost-user
  negotiation succeeds and the guest gets its address — so the spike's primary
  unknown fails in isolation, with a cause from the captured serial console,
  instead of as an ambiguous timeout inside the full stack. The full boot then
  runs with 2 CPUs / 1024 MB, `Health` OK with `net_provisioned &&
  workspace_mounted` within a 60 s deadline (the spike asserts mount + net state
  via the response flags — host↔`/workspace` content round-trip is V2b's); logs
  boot-latency + per-process PSS; `t.Cleanup` verifies all three PIDs are gone
  and the unix sockets removed (no orphans); a boot with a corrupt rootfs path
  fails inside the deadline with a named error from the serial log (fail-closed
  teardown path exercised). Runs on the dev box now (E6: `/dev/kvm` root:kvm
  0660, invoking user in `kvm` group) and on the CI KVM leg under
  `COMPASS_REQUIRE_MICROVM=1`.

## Tasks

- [ ] T1 — image packing: erofs `compass-guest-rootfs` + `compass-guest-initrd`
      module initramfs; drop workspace.mount; moon gate extended
- [ ] T2 — `compass-guestd` real init (net bringup, resolv.conf, virtio-fs
      mount, vsock `Health`, fail-closed)
- [ ] T3 — `GuestControl` proto seed + hybrid-vsock `DialGuest`/`GuestClient`
- [ ] T4 — `Launch` harness + KVM-gated boot spike test (boot, handshake,
      latency + RSS, teardown)

## Open Questions

Batched for the driver's single `ask`; the body designs against each
recommendation.

- **OQ-A (load-bearing) — rootfs format: erofs+overlay vs ext4 vs
  initramfs-root vs virtiofs-root.** Options and tradeoffs in
  `## Alternatives considered`. **Recommend (a): read-only erofs on
  virtio-blk + whole-root tmpfs overlay via a minimal module initramfs** —
  shared immutable image, hash-verifiable, per-session cost ≈ tmpfs writes;
  the initramfs is mandatory under the pinned modular kernel regardless of
  format.
- **OQ-B (load-bearing) — net backend: passt vs gvproxy.** **Recommend
  passt**: already the E1-pinned, reviewed choice (`devenv.nix:165-186`);
  vhost-user wiring to CH is rootless; built-in DHCP/DNS feeds guestd's
  resolv.conf provisioning. gvproxy would add an unpinned dependency for no
  capability gain.
- **OQ-C (load-bearing) — guestd's net-bringup implementation.** Three options:
  (1) **static config from the kernel cmdline** — the harness owns both passt's
  deterministic addressing flags (`-a`/`-g`/`-n`/`-D`,
  [passt(1)](https://passt.top/builds/latest/web/passt.1.html)) and the cmdline,
  so guestd applies a fixed address/gateway/DNS with `netlink` alone (ONE new
  go.mod dep) and no DHCP exchange; (2) in-process Go DHCP client
  (`vishvananda/netlink` + `insomniacslk/dhcp`, ~three deps); (3) shell out to a
  DHCP binary in the image. **Recommend (1) static.** DHCP is not part of any
  V2b+ contract (the parent requires only that guestd provisions the IP +
  resolv.conf, microvm-runner.md:95-103,733-750 — never *how*), and static
  removes both a dep and the DHCP-broadcast-over-vhost-user exchange, keeping it
  off the same critical path as the untested vhost-user pairing (OQ-G) — the
  right posture for a minimal-code spike. Option (2) is the documented fallback
  if Matt wants DHCP proven in V2a; (3) is rejected (an extra in-image daemon
  for no gain).
- **OQ-D (non-load-bearing, deferred) — durable/disk-backed overlay upper for
  heavy in-guest `nix build` workloads.** tmpfs upper is correct for the
  spike and inner-loop; revisit in V2b+ when session sizing data exists
  (D5 hotplug covers the interim). Defer with rationale — no V2a ambiguity.
- **OQ-E (load-bearing, small) — `BootConfig` deviations from the parent's
  sketch:** `VsockCID`/`VsockPort` as `uint32` (vsock addressing is numeric,
  [vsock(7)](https://man7.org/linux/man-pages/man7/vsock.7.html)), added
  `Initrd` + `Cmdline` fields (consequence of OQ-A), and `VsockSocket`
  (hybrid host endpoint) split out. The parent sketched all-string
  (microvm-runner.md:459-460); this is a detailing refinement, not a
  contradiction — but V2b consumes it, so it should be ratified.
  **Recommend the (f) struct as written.**
- **OQ-F (load-bearing, small) — removing E3's `workspace.mount` systemd
  unit.** The unit assumed a systemd guest; guestd-as-PID-1 has no systemd,
  so the unit is unreachable dead weight and the tag→path contract moves into
  guestd ((d)). **Recommend removal in T1.** Alternative: keep it as inert
  documentation — rejected, a dead artifact that looks load-bearing is worse
  than a comment.
- **OQ-G (load-bearing) — the passt×cloud-hypervisor vhost-user-net pairing has
  no published upstream validation.** Every individual leg of §(c) is
  documented, but the *composition* is not: passt's vhost-user integration
  targets QEMU/libvirt
  ([libvirt series](https://patchew.org/Libvirt/20250213181953.922499-1-laine@redhat.com/)),
  CH's vhost-user-net testing targets DPDK backends
  ([docs/vhost-user-net-testing.md](https://github.com/cloud-hypervisor/cloud-hypervisor/blob/main/docs/vhost-user-net-testing.md)),
  and no CH doc mentions passt — so the negotiation (feature bits, socket roles,
  memory-region shape) is the spike's single riskiest unknown. **Recommend: keep
  passt (the E1-pinned, correct backend) and treat the pairing as an explicit
  unknown** — T4 isolates it with a net-only boot smoke before the full stack
  ((g)/T4), and **pre-declare the fallback**: if negotiation fails, switch to
  gvproxy's vhost-user mode, which requires a documented amendment to E1's passt
  pin (`devenv.nix:165-186`). The ask for Matt is to **pre-authorize that
  gvproxy fallback** so a T4 negotiation failure resolves against a ratified
  contingency instead of stalling on an emergency design fork. Alternative:
  leave the fallback unspecified and treat a negotiation failure as a fresh
  design decision if it happens — rejected, it stalls the milestone at exactly
  its riskiest step.
