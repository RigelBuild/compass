# microVM Runner Backend

Status: PROPOSED — RIG-2394 (reframed from a detailing record to a full design pass, 2026-08-21). The six load-bearing forks are resolved (see Decisions D1-D7); this record freezes on merge.

Parent: [compass-elastic-session-runtime/design.md](./design.md) — this record details under the parent's frozen decisions but replaces its falsified I1 implementation premise.

## Problem / Intent

The parent record's task I1 assumed the microVM inter-tenant boundary is an
engine/runtime configuration behind the existing interface: "slot a microVM OCI
runtime (krun/libkrun or kata) via podman's `--runtime` selection …
No new caller-facing seam — the boundary is an engine/runtime configuration
behind the existing interface" (design.md:576-593). **That premise is
falsified.** A microVM boundary breaks *both* host↔guest control channels the
Runner depends on:

1. **`podman exec` does not enter a microVM guest.** Upstream crun issue
   [#2090](https://github.com/containers/crun/issues/2090) (open, updated
   2026-08): "The krun backend does not support exec … there is no existing
   mechanism for launching executables into the microVM." Only experimental,
   non-upstream patched-crun + libkrun forks (a vsock exec server in
   `init.krun`) make exec work. And Compass is **exec-everything**: the
   container's main process is a sleep loop — "Keep the container alive so the
   Runner can exec into it; the agent is driven via exec, not as the
   container's main process" with `Command: []string{"sleep", "infinity"}`
   (`go/internal/runtime/agent.go:269-271`); the agent itself is launched via
   `engine.ExecStreaming(ctx, id, env.execSpec())`
   (`go/internal/runner/agent_exec.go:140`); and every provisioning step —
   "provision runs the post-start steps, all inside the running container:
   firewall (root), credentials (agent user), checkout dir (agent user)"
   (`go/internal/runtime/agent.go:288-298`), including `armEgress`'s
   `r.runtime.Exec(ctx, id, NewExecSpec("sh", "-c", egress.NftScript()))`
   (`agent.go:304`) — is a post-start `Exec`. With no exec into the guest,
   none of this runs.
2. **The agent's AF_UNIX gateway socket does not cross a VM boundary.** The
   `ContainerSpec.Mounts` doc: "the per-container agent gateway socket is
   mounted read-write (the agent must connect() to it)"
   (`go/internal/runtime/podman.go:100-102`). Package
   `go/internal/runner/gateway` is "the Runner side of the agent->Runner call
   transport: a per-container Unix-socket Connect server the in-container
   first-party agent dials to reach its Runner"
   (`go/internal/runner/gateway/socket.go:3-6`), carrying Comms, Lifecycle,
   Forge, Publish, PostConversationFrame, and the Control lane
   (`gateway.go`, `control.go`). A bind-mounted unix socket is just an inode;
   a guest `connect()` never reaches the host-side listener across a VM
   boundary.

The shape that actually works — how Kata's `kata-agent` and
firecracker-containerd's guest agent both do it — is an **in-guest agent
speaking a protocol over virtio-vsock**, with the host Runner driving
create/exec/stdio/signal *and* the gateway control plane through that vsock
channel. That is real Runner control-plane work regardless of VMM choice, and
it is the honest scope of this record: a **dedicated microVM Runner backend**
— a second implementation behind `runtime.ContainerRuntime` — rather than a
config swap.

This record details *under* the parent's frozen decisions (Decision 5: the
inter-tenant boundary IS a hardware-virtualized microVM, design.md:881-894;
the virtio-fs no-copy invariant, design.md:586-587) and replaces only the
falsified implementation mechanism. Per design.md:892-894, "Through Dogfood +
trusted-tenant Beta the rootless container remains the running boundary" — so
this work runs in parallel with M0/S1/P2/C3 and blocks nothing before the
first external multi-tenant tenant. Beyond that point the container path is
**removed** and microVM becomes the sole runtime (D2); the container backend
is a transitional bootstrap, not a permanent second runtime.

## Approach

A microVM backend as a **sibling `ContainerRuntime` implementation** beside
`PodmanCLI`, selected by Runner config. `podman.go`'s own layering note
anticipated exactly this seam use: "Everything above depends on the interface,
so a libpod-REST backend can replace it without touching a caller"
(`go/internal/runtime/podman.go:11-13`). The `ContainerRuntime` interface
(`podman.go:303-352`: Create/Start/Exec/ExecStreaming/Stop/Remove/Exists/
MountLabel/Resize) is the contract; `AgentRuntime`, the gateway, and the
session lifecycle above it stay untouched.

### (a) VMM shape

Per D1, the VMM is **cloud-hypervisor**. The design relies only on the
**virtio-fs-preserving shape** — a KVM-backed VMM offering virtio-fs
(shared-memory file sharing), virtio-vsock (host↔guest stream channel), and
virtio-net, launched rootless as an ordinary host process per session.
Firecracker is excluded by the frozen no-copy invariant (no virtio-fs — see
Alternatives). cloud-hypervisor's virtio-fs
([docs/fs.md](https://github.com/cloud-hypervisor/cloud-hypervisor/blob/main/docs/fs.md):
`--fs` with `--memory shared=on`) and memory/CPU hotplug
([docs/hotplug.md](https://github.com/cloud-hypervisor/cloud-hypervisor/blob/main/docs/hotplug.md))
directly serve both the invariant and the S1-reserved `Resize` seam (D5).

Per session, the backend owns three host processes: the VMM, a dedicated
`virtiofsd` instance, and (transitively) the guest. All three are supervised
(see (f)).

**Guest networking (D6).** A rootless VMM cannot create a host tap, so guest
networking is a userspace concern the backend must provide. Per D6 the design
uses **virtio-net + a passt/gvproxy-class userspace net backend**: the guest
gets a real network namespace and its own IP, so the in-guest nft egress
ruleset (c) and its allowlist DNS resolution work exactly as they do in the
container today. `compass-guestd` provisions the guest IP and
`/etc/resolv.conf` at boot, before the arm step. libkrun's TSI (no guest
interface, no netns) is rejected: it moves the egress boundary back to a host
proxy and forfeits (c)'s integrity gain.

### (b) In-guest agent + vsock control plane

Two channels ride one virtio-vsock device, on distinct ports:

- **Guest supervisor channel (new work).** A small in-guest supervisor
  (`compass-guestd`, a new process for privilege separation — D4; it also
  supervises the compass-agent) is the guest's PID-1-adjacent init. It serves
  a Connect/h2c service over vsock implementing the exec surface the Runner
  needs:
  `Exec(spec) → output`, `ExecStreaming(spec) → (stdio streams, kill/wait
  handle)`, `Signal`, plus guest boot/health. The host-side
  `MicroVMRuntime.Exec`/`ExecStreaming` translate `runtime.ExecSpec` /
  `StreamingExecSpec` onto this service, preserving the interface contract
  ("A non-zero exit is a successful runtime call returning a failed command …
  not an error", `podman.go:310-314`). Stdin/stdout/stderr for `ExecStreaming`
  are carried as streams over the same connection so `AgentStream`'s drain
  model (`agent_exec.go:150-161`) keeps working unchanged above the seam.
- **Gateway channel (transport swap, not a protocol change).** The
  per-container `AgentGateway` (Comms/Lifecycle/Forge/Publish/
  PostConversationFrame/Control) is already Connect-over-h2c on a stream
  socket (`gateway/socket.go`). The host Runner serves the *same generated
  handler* over the host-side vsock transport instead of the AF_UNIX path;
  the in-guest agent dials vsock instead of the unix socket. The concrete
  host-transport shape follows D1's VMM: under
  cloud-hypervisor it is **hybrid vsock** — the host end stays an AF_UNIX
  socket, selected by a `CONNECT <port>` preamble
  ([CH docs/vsock.md](https://github.com/cloud-hypervisor/cloud-hypervisor/blob/main/docs/vsock.md)),
  so the existing `net.Listener`/Connect serving path largely transfers; a VMM
  exposing a kernel AF_VSOCK listener would bind that instead. No message,
  handler, or relay changes — `Gateway`, `ControlSender`, and the Server
  relays are transport-agnostic. The socket→container identity binding
  (socket path = container identity today, `gateway.go:132-134`) becomes
  per-session-port→VM identity, assigned and recorded by the backend at boot.

### (c) Egress default-deny inside the guest

The fail-closed integrity model is preserved by moving the arm step from
`podman exec` to the guest supervisor's boot sequence:

1. `compass-guestd` (guest root) receives the `EgressPolicy.NftScript()`
   output over the supervisor channel as part of the boot/provision request
   and runs it inside the guest netns — same script, same fail-closed
   semantics ("`set -eu` makes the base ruleset fail closed — if any … rule
   fails to install, the script aborts non-zero and the caller tears the
   container down rather than running it unfirewalled",
   `go/internal/runtime/egress.go:76-79`). A non-zero arm result fails the
   boot; the backend tears the VM down.
2. Only after a successful arm does the supervisor accept exec requests, and
   every agent exec runs as the non-root agent uid with an empty capability
   set — the same posture as today ("the agent then runs as a non-root user
   whose capability set is empty, so it cannot flush or edit the ruleset",
   `egress.go:7-9`). The guest supervisor enforces this: an exec spec may not
   request uid 0 or added capabilities.
3. The supervisor **authenticates its vsock peer**: it accepts control
   requests only from the host (CID 2) and refuses the in-guest loopback
   (CID 1). Linux ≥5.6 vsock loopback lets any in-guest process — the agent
   needs no capability to `connect()` a vsock — dial the supervisor's port
   locally; without peer-CID checking that path would let the workload request
   a uid-0 `Exec` or re-arm `Provision` and defeat the non-root posture
   (`egress.go:6-10`). Asserted by V8's in-guest escalation probe.

On the inter-tenant axis this is *stronger* than the container model: the
ruleset lives in the guest kernel, `CAP_NET_ADMIN` never has to be granted to
the workload boundary at all (no `CapAdd: []string{capNetAdmin}` equivalent,
cf. `agent.go:266`), and vsock is not IP — the gateway channel is unreachable
from the firewalled guest netns by construction, preserving "no new port, no
outbound route" (`socket.go:16`). The tradeoff (D4): the supervisor is a
session-lifetime guest-root RPC server, a new persistent surface the container
model lacks (there the arm is a one-shot root exec that exits,
`agent.go:300-307`) — contained by the peer-CID authentication above and the
uid/capability gate on every exec.

### (d) virtio-fs session volume: stable path, per-tenant isolation, quota

- **Stable path (frozen invariant).** One dedicated `virtiofsd` per session,
  rooted at that session's volume directory; the guest mounts the virtio-fs
  tag at the same stable absolute path the container path uses, preserving
  the no-copy invariant P2/C3 depend on (design.md:586-587).
- **Per-tenant host-side mount-point isolation (named mechanism).** Each
  `virtiofsd` runs with its **sandbox enabled** — virtiofsd's
  `--sandbox=namespace` mode `unshare(2)`s into a private **mount namespace**
  and pivot-roots to the shared directory, so the daemon process itself
  cannot name any host path outside the session volume even if compromised
  by a hostile guest
  ([virtiofsd README, sandbox modes](https://gitlab.com/virtio-fs/virtiofsd#examples)).
  Rootless operation composes a **user namespace** with it
  (`--sandbox=namespace` under an unprivileged userns): virtiofsd does its own
  uid/gid translation via that userns (subuid/subgid + `newuidmap`), not the
  podman `--userns=keep-id` argv (`podman.go:412-417`) — but the target is the
  same host-side ownership on the session volume, so files stay identical
  between backends. Host-ownership parity is asserted by V6's test cycle. A
  guest can therefore reach exactly one directory subtree and nothing else;
  another tenant's volume is not merely unreadable but *unnameable*.
- **Resource-exhaustion control (quota) — verify, never assign (D7).**
  virtio-fs itself imposes no space or inode bound, so a hostile guest can
  exhaust the shared filesystem. But the obvious mechanisms collide with
  "rootless is hard": project-quota *assignment* (`FS_IOC_FSSETXATTR` +
  `quotactl`) and the loopback-image fallback (`mount(2)`) both need
  `CAP_SYS_ADMIN` the Runner does not have (`podman.go:22-24`), and the frozen
  no-copy invariant forbids swapping the virtio-fs volume for a quota-bounded
  block device. Per D7 the Runner therefore never *assigns* quota: the
  multi-tenant deployment provisions the session-volume filesystem with
  per-directory project quota via operator IaC at deploy, and preflight (V5)
  *verifies is active* (a read-only, rootless-safe check), failing startup if
  absent. Dogfood's single trusted tenant ships no host-enforced quota.

### (e) Preflight + boot canary + KVM-absent hard-fail

Mirrors `VerifyUsernsRemapSupport` (`podman.go:443-467`), which "errors below
the floor, naming both the required floor and the found version so an operator
… learns the cause at startup rather than deep inside the first container
create":

- **`VerifyMicroVMSupport` (static preflight, Runner startup):** `/dev/kvm`
  exists and is openable by the Runner uid; the vsock prerequisite is present
  (a host `/dev/vhost-vsock` for a kernel-vhost transport, *not* required for
  cloud-hypervisor's userspace hybrid-vsock socket (D1)); the VMM and
  `virtiofsd` binaries
  are found and at/above pinned version floors; the guest kernel + rootfs
  image assets are present and hash-verified.
- **Boot canary (dynamic preflight):** at startup (and on demand), boot a
  minimal canary VM: VMM start → guest supervisor handshake over vsock →
  echo exec → teardown, under a deadline. This proves the whole chain — KVM,
  vsock, image, supervisor — not just binary presence, and produces the
  boot-latency measurement for (h).
- **KVM-absent ⇒ hard-fail (D3):** with no container fallback, `/dev/kvm`
  absence (or any preflight failure) aborts Runner startup with an error
  naming the missing capability and the fix ("needs KVM — use the managed
  service or a KVM host"). This supersedes the parent's frozen
  degrade-to-container behavior (design.md:600-601), which only held while the
  container path existed; a silent isolation downgrade on a multi-tenant box
  after a KVM regression is a security incident, not a degraded mode.

### (f) Teardown and mid-session death

The backend supervises its per-session process trio (VMM, virtiofsd, guest —
the guest via the supervisor channel's liveness). Failure handling:

- **VMM death mid-session:** the vsock connections and exec streams break;
  the backend marks the container handle dead, fails in-flight `Exec`s with a
  distinguishable error (the `CommandError`/`TimeoutError` discipline,
  `podman.go:277-296`), tears down the peer virtiofsd, and releases the vsock
  port and volume mount state. `Remove` is idempotent on an already-dead VM.
- **virtiofsd death mid-session:** the guest's virtio-fs mount goes stale;
  treated as fatal to the session (no remount-and-hope): kill the VMM, same
  teardown path. The session volume itself is durable on the host and
  unaffected — resume/replay is the session lifecycle's existing job.
- **Runner crash:** on startup the backend reaps orphaned VMM/virtiofsd
  processes by their per-session runtime dir (pidfiles + process-liveness
  check). This is **new** behavior, not a mirror of the podman path — the
  podman transactional remove-on-start-failure (`agent.go:278-283`) cleans a
  *failed create*, not orphans left by a Runner crash. Healthy VMs found at
  restart are **killed and rebooted on next request, not adopted**: the
  supervisor handshake state is in-process and not reconstructable across a
  Runner restart (same "no remount-and-hope" posture as virtiofsd death).

### (g) Observability + kill switch

- Every existing session metric gains a `backend` label (`podman`/`microvm`).
  New metrics: VM boot latency (VMM start → supervisor handshake), per-VM
  RSS, vsock RPC latency, virtiofsd restarts, quota utilization, canary
  result. Boot/teardown transitions log at INFO with session id and timings.
- **Transitional kill switch (retires with the container path, D2).** While
  both backends coexist through Dogfood + trusted Beta, a single config flag
  (`runtime.backend: podman`) reverts the fleet to the container path without
  a code deploy — the safety valve that makes shipping microVM incrementally
  safe. The no-regression Global Constraint guarantees the reverted path is
  byte-identical to today's. Once the container path is removed (D2) there is
  nothing to revert to and the flag is gone; from then on a broken microVM
  path is a roll-back-the-deploy operation, not a runtime toggle.

### (h) Boot-latency / RSS overhead budget

A microVM start costs more than `podman create+start`; the budget question is
*how much is acceptable* before it degrades session-start UX and box density.
The record does not invent numbers: the boot canary (e) and a benchmark in the
test suite (task V8) measure boot-to-first-exec latency and steady-state
per-VM RSS against the container baseline on real hardware; the budget is set
from those measurements (Q-budget, deferred to data). The S1-reserved `Resize`
seam bounds the *sizing* side: the VM boots at inner-loop size and grows on
demand via cloud-hypervisor hotplug rather than reserving peak RAM (D5).

## Alternatives considered

- **podman `--runtime` krun/crun-vm (the parent's I1 premise) — rejected.**
  The config-swap the parent expected. Falsified: crun's krun backend has no
  exec ("there is no existing mechanism for launching executables into the
  microVM", [crun #2090](https://github.com/containers/crun/issues/2090)),
  which breaks the exec-everything Runner (`agent.go:269-271`,
  `agent.go:288-298`, `agent_exec.go:140`); and the bind-mounted AF_UNIX
  gateway socket (`podman.go:100-102`) does not cross a VM boundary. Making
  it work would mean maintaining non-upstream crun/libkrun forks *and* still
  building the vsock gateway transport — all the cost of a dedicated backend
  with none of the control.
- **Firecracker — rejected.** Minimal device model by design: block and net
  devices only, **no virtio-fs**
  ([firecracker docs — supported devices](https://github.com/firecracker-microvm/firecracker/blob/main/docs/api_requests/README.md);
  virtio-fs has been explicitly declined upstream,
  [firecracker #1180](https://github.com/firecracker-microvm/firecracker/issues/1180)).
  Forfeits the frozen no-copy virtio-fs invariant (design.md:586-587) — a
  hard backend filter, so Firecracker is out regardless of its other merits
  (exec via its guest agent over vsock is the same shape we adopt).
- **Kata Containers — rejected (D1).** The most mature guest control plane in
  existence (kata-agent over ttRPC/vsock, exactly the channel shape this
  record builds) and virtio-fs by default. But Kata is shimv2/containerd-only
  — podman support is unresolved upstream
  ([kata-containers #722](https://github.com/kata-containers/kata-containers/issues/722))
  — so adopting it means running a containerd stack beside the rootless-podman
  engine: a second container engine with its own rootless story, image path,
  and operational surface, a far larger blast radius than a plain rootless VMM
  process. The gateway vsock transport work remains ours either way, so the
  mature agent saves little of the real cost.
- **libkrun-direct (embed libkrun, not via podman) — rejected (D1).**
  virtio-fs and vsock supported, KVM-backed, and the one candidate that also
  runs on macOS (HVF), designed for embedding
  ([libkrun](https://github.com/containers/libkrun)). Its remaining draw was
  the native-macOS-embedded path — dropped by D2 — so the reason to carry a
  second VMM is gone. Costs regardless: the guest kernel payload (libkrunfw),
  a heavy nix packaging item, and its device model carries no memory-hotplug
  device for the S1 `Resize` seam (virtio list:
  console/block/fs/gpu/net/vsock/balloon(free-page-reporting only)/rng,
  [libkrun README](https://github.com/containers/libkrun#virtio-device-support)),
  foreclosing in-place resize (D5).
- **cloud-hypervisor — chosen (D1).** virtio-fs
  ([docs/fs.md](https://github.com/cloud-hypervisor/cloud-hypervisor/blob/main/docs/fs.md)),
  virtio-vsock
  ([docs/device_model.md](https://github.com/cloud-hypervisor/cloud-hypervisor/blob/main/docs/device_model.md)),
  CPU + memory hotplug/resize
  ([docs/hotplug.md](https://github.com/cloud-hypervisor/cloud-hypervisor/blob/main/docs/hotplug.md))
  plus machine-to-machine live migration
  ([CH README](https://github.com/cloud-hypervisor/cloud-hypervisor#objectives)),
  Rust, security-focused, runs rootless as an ordinary process, proven as a
  Kata VMM. Hotplug directly serves the S1-reserved
  `ContainerRuntime.Resize` (`podman.go:342-351`, D5). Runs on KVM/MSHV, not
  macOS HVF — acceptable because native-macOS-embedded is dropped (D2). Cost:
  we build the guest supervisor ourselves (would have been shared with the
  libkrun option).
- **gVisor — rejected.** Userspace syscall interception (a Sentry kernel in
  userspace, [gvisor.dev architecture](https://gvisor.dev/docs/)), not a
  hardware-virtualization boundary. Fails the frozen Decision 5 ("the
  inter-tenant boundary IS a hardware-virtualized microVM",
  design.md:881-888) on its face.

## Global Constraints

- **virtio-fs no-copy invariant (hard backend filter).** The session volume
  reaches the guest by virtio-fs at the same stable absolute path
  (design.md:586-587). Any VMM without virtio-fs is disqualified.
- **Egress fail-closed integrity model preserved.** The `NftScript()`
  fail-closed semantics (`egress.go:76-79`) arm inside the guest before any
  agent exec is accepted; a failed arm fails the boot. The agent runs as a
  non-root uid with an empty capability set and cannot alter the ruleset
  (`egress.go:6-10`); the guest supervisor refuses exec specs requesting
  uid 0 or capabilities.
- **Guest networking is userspace (D6).** A rootless VMM cannot create a host
  tap; the guest reaches the network via virtio-net + a passt/gvproxy-class
  userspace backend, giving it a real netns so the in-guest egress ruleset
  holds (Approach (a)/(c)).
- **Co-located Runner (D8).** The Runner runs one-per-box, managing only that
  box's local microVMs over host-local vsock; fleet provisioning/scheduling and
  telemetry fan-in are a separate control plane, out of scope here. The agent
  never talks directly to the Server — the local Runner remains the sole,
  frozen attribution boundary.
- **Rootless is hard.** No daemon, no root, no rootful fallback
  (`podman.go:22-24`). The VMM, virtiofsd, and every backend process run as
  the invoking user; host-side file ownership on the session volume matches
  the podman path (`--userns=keep-id`, `podman.go:412-417`) via virtiofsd's
  own userns uid/gid translation (Approach (d)). No backend step requires a
  capability the rootless Runner lacks; anything that would (quota assignment)
  is pushed to operator provisioning + preflight verification (D7).
- **KVM-absent ⇒ hard-fail (D3).** With no container fallback, a box without
  microVM support cannot run Compass: `VerifyMicroVMSupport` (V5) fails Runner
  startup with an error naming the missing capability and the fix. This
  supersedes the parent's degrade-to-container default (design.md:600-601),
  which only held while the container path existed.
- **Per-tenant mount isolation + resource quota.** Every per-session
  virtiofsd is mount-namespace-sandboxed to its volume subtree (Approach (d));
  virtio-fs alone provides no space or inode bound. Per D7 the Runner
  *verifies* an operator-provisioned quota (rootless-safe) and never assigns
  one.
- **No regression to the container path during transition.** While both
  backends coexist (D2), selecting the container backend yields behavior
  byte-identical to today's podman path: same argv (`createArgs`,
  `podman.go:404-431`), same code paths, no new preflight failures. Enforced
  by the existing suite running unchanged against the container backend. This
  constraint retires when the container path is removed.
- **Transitional container path, then microVM-only (D2).** The rootless
  container remains the running boundary through Dogfood + trusted-tenant Beta
  (design.md:892-894) and is then **removed**: microVM is the sole runtime.
  This work runs in parallel with M0/S1/P2/C3 and gates nothing before the
  first external multi-tenant tenant.

## Plan

Tasks are ordered by dependency; V2 (guest image + supervisor + vsock
transport) is the schedule-critical and largest item and is deliberately split
into a packaging/boot spike (V2a) and the control plane proper (V2b) so the
risky unknowns (nix-packaged guest kernel/rootfs, rootless VMM boot, vsock
availability) surface first with minimal code.

### V1 — backend seam + selection + startup gate

A `MicroVMRuntime` skeleton implementing `runtime.ContainerRuntime`, plus the
config-driven backend selection in Runner startup. Through the transitional
period (D2) both backends exist and selection resolves to the configured one,
defaulting to the container path while microVM is proven; once microVM is the
sole runtime the selection collapses to microVM with `VerifyMicroVMSupport`
(V5) as a hard startup gate (D3 — no container fallback to select).

- **Interfaces:** produces `runtime.MicroVMRuntime` satisfying
  `runtime.ContainerRuntime` (`Create(ctx, ContainerSpec) (ContainerID,
  error)`, `Start`, `Exec(ctx, ContainerID, ExecSpec) (ExecOutput, error)`,
  `ExecStreaming(ctx, ContainerID, StreamingExecSpec) (*StreamingExec,
  error)`, `Stop`, `Remove`, `Exists`, `MountLabel`, `Resize` —
  `podman.go:303-352`), every method returning a typed
  `ErrMicroVMNotImplemented` until V2b/V3 fill them in; produces
  `runtime.SelectBackend(cfg RunnerConfig) (ContainerRuntime, error)`.
  Consumes `RunnerConfig` (new fields `Backend string`,
  `MicroVM struct{ VMMPath, VirtiofsdPath, KernelImage, RootfsImage string }`).
- **Test cycle:** selection unit tests (transitional: configured backend
  resolves; microVM-only: absent-KVM → startup error naming the missing
  capability, D3); the existing session suite green under the container
  backend, proving the transitional path is unregressed.

### V2a — guest image + boot spike (packaging)

The nix-packaged guest artifacts and a proven rootless boot: guest kernel,
rootfs carrying the devenv toolchain (reusing the compass-agent image build's
contents) **plus the egress prerequisites the arm step needs in-guest —
`nft`, `getent`, `awk`, and a writable `/etc/resolv.conf`**
(`egress.go:76-78`), and a `compass-guestd` stub that brings up guest
networking (D6: virtio-net + the userspace net backend, guest IP +
resolv.conf), mounts the virtio-fs tag at the stable path, and answers one
vsock handshake. Deliverable is a script/test that boots cloud-hypervisor
(D1) rootless, brings up the net backend, gets the handshake, and
exits — the de-risking milestone.

- **Interfaces:** produces nix derivations `compass-guest-kernel`,
  `compass-guest-rootfs` (carrying the egress toolset above), and the
  `compass-guestd` binary entrypoint; a
  `BootConfig{Kernel, Rootfs, VsockCID, VsockPort, FSTag, FSSocket string;
  CPUs int; MemoryMB int; Net NetConfig}` struct consumed by V2b's boot code.
  No Runner wiring yet.
- **Test cycle:** an integration test (KVM-gated, skipped where absent) that
  boots, brings up guest networking, handshakes over vsock under a deadline,
  and tears down; records boot-latency and RSS numbers feeding (h)/Q-budget.

### V2b — in-guest supervisor + vsock exec control plane

`compass-guestd` grown into the real supervisor (guest boot: mount virtio-fs,
arm egress per V3, then serve exec), and `MicroVMRuntime`'s
Create/Start/Exec/ExecStreaming/Stop/Remove implemented against it: Create
allocates the per-session runtime dir, vsock port, and virtiofsd; Start boots
the VMM and completes the supervisor handshake; Exec/ExecStreaming translate
`ExecSpec`/`StreamingExecSpec` onto the vsock service preserving the
non-zero-exit-is-not-an-error contract (`podman.go:310-314`) and the
`StreamingExec` pipe + kill/wait shape `AgentStream` consumes
(`agent_exec.go:140-161`).

- **Interfaces:** produces the `GuestControl` Connect service (proto:
  `Exec(ExecRequest) returns (ExecResponse)`,
  `ExecStream(stream ExecStreamFrame) returns (stream ExecStreamFrame)`,
  `Signal(SignalRequest)`, `Provision(ProvisionRequest) returns
  (ProvisionResponse)`, `Health(HealthRequest)`) served by `compass-guestd`
  over vsock; produces the filled `MicroVMRuntime` methods. Consumes V2a's
  artifacts and `BootConfig`.
- **Test cycle:** contract tests asserting `MicroVMRuntime` and `PodmanCLI`
  behave identically through the `ContainerRuntime` surface (shared
  table-driven contract suite, KVM-gated for the microVM rows): exec exit
  codes, stdin feeding (`WriteAgentFile`'s stdin-not-argv invariant,
  `agent.go:241-248`), streaming stdio, kill/wait, uid enforcement (uid-0
  exec refused).

### V3 — egress-in-guest

The `Provision` step delivering `EgressPolicy.NftScript()` to `compass-guestd`
for in-guest arming before exec acceptance, and `AgentRuntime.provision`'s arm
path routed through it on the microVM backend (the podman path unchanged).

- **Interfaces:** produces the `ProvisionRequest{NftScript string}` handling
  in `compass-guestd` (run as guest root, `set -eu` fail-closed, non-zero →
  boot failure) and the supervisor's armed-gate (exec refused until armed).
  Consumes `runtime.EgressPolicy.NftScript() string` (`egress.go:71-107`)
  unchanged.
- **Test cycle:** in-guest egress integration tests: allowlisted host
  reachable, non-allowlisted host blocked, both address families; arm-failure
  → VM torn down, session start fails; post-arm agent-uid exec cannot alter
  the ruleset (`nft flush` as agent uid fails).

### V4 — gateway over vsock

The `AgentGateway` served over the host-side vsock transport per session
beside the AF_UNIX path. Per D1 (cloud-hypervisor) the transport is the
**hybrid vsock** model — the host end is an AF_UNIX socket and a
`CONNECT <port>` line selects the guest-side port
([CH docs/vsock.md](https://github.com/cloud-hypervisor/cloud-hypervisor/blob/main/docs/vsock.md)),
so the existing `net.Listener`/Connect serving path largely transfers (the
host stays AF_UNIX). Either way the generated handler
(`gateway/gateway.go:288-316`) is bound with the same `WithReadMaxBytes`
bound, and the port→container identity binding lives in the backend. The
in-guest agent's dial target (the guest-side vsock port, replacing today's
fixed socket **mount path** — there is no socket env var today,
`agent_exec.go:80-94`, `gateway/socket.go:299-301`) is a **new** value
injected via the exec environment.

- **Interfaces:** produces a vsock-transport constructor (sibling of `Serve`,
  `socket.go`) binding the generated
  `compassv1internalconnect.NewAgentGatewayHandler` unchanged over the
  hybrid AF_UNIX vsock transport (D1). No proto or handler changes.
- **Test cycle:** the gateway hermetic suites re-run over the host-side vsock
  transport (or a vsock-shaped `net.Listener` fake where CI lacks the VMM);
  end-to-end: in-guest agent dials, Comms/Publish/Control round-trip
  (mirroring `e2e_transport_test.go`); the firewalled guest netns cannot
  reach the vsock gateway via IP (non-goal check: vsock is not IP).

### V5 — preflight, boot canary, startup gate

`VerifyMicroVMSupport` (static checks: /dev/kvm openable by the Runner uid;
the vsock prerequisite — a host `/dev/vhost-vsock` for a kernel-vhost
transport, *not* required for cloud-hypervisor's userspace hybrid-vsock socket
(D1), confirmed against the V2a spike; binary versions; image hashes)
mirroring `VerifyUsernsRemapSupport`'s name-the-floor-and-the-found error
posture (`podman.go:443-450`); the boot canary; and the hard-fail startup gate
wiring into V1's selection (D3 — no container fallback, so a failed preflight
aborts startup rather than degrading).

- **Interfaces:** produces `(*MicroVMRuntime) VerifyMicroVMSupport(ctx) error`
  and `(*MicroVMRuntime) BootCanary(ctx) (CanaryReport, error)` with
  `CanaryReport{BootLatency time.Duration; GuestRSSBytes int64}`; a failed
  `VerifyMicroVMSupport` returns a startup error naming the missing capability
  and the fix (D3), never a degrade signal.
- **Test cycle:** preflight unit tests per failure axis (fake probes);
  absent-KVM → Runner startup fails with the capability-naming error (D3);
  canary integration test (KVM-gated).

### V6 — virtio-fs isolation + quota verification

Per-session virtiofsd supervision with `--sandbox=namespace` (mount-ns +
userns confinement to the volume subtree) and the volume quota **verification**
path: the Runner confirms an operator-provisioned project quota is active on
the session-volume filesystem (a read-only, rootless-safe check — no
`CAP_SYS_ADMIN` assignment step), surfacing quota utilization to
observability. Per D7 the Runner verifies, never assigns — verify-only, with
no host-enforced quota for Dogfood's single trusted tenant and an
operator-provisioned quota verified for the multi-tenant profile.

- **Interfaces:** produces `runtime.VolumeQuota{Bytes int64; Inodes int64}`
  (the expected bound, for the preflight comparison and metrics) and
  `verifyVolumeQuota(path string, want VolumeQuota) error` (reads the
  filesystem's active project quota, errors when absent under the
  quota-required multi-tenant profile — a `MicroVM.QuotaRequired bool` config,
  distinct from the retired runtime-selection knob); produces the
  `virtiofsdProc` supervisor (spawn args
  incl. `--sandbox=namespace`, liveness, kill-on-teardown). Consumes the
  session volume directory the parent's P2 volume lifecycle owns.
- **Test cycle:** isolation probe — a guest exec attempts path traversal
  (`..`, absolute paths, symlink escape) out of the volume and is confined;
  a second session's volume is unreachable; host-ownership parity between the
  podman and microVM backends on the shared volume; quota tests (on a
  test-provisioned quota'd filesystem) — writes past the byte bound and
  creates past the inode bound fail inside the guest with ENOSPC/EDQUOT while
  the host filesystem stays healthy; missing-quota + `required` → preflight
  errors.

### V7 — teardown, crash recovery, observability

The failure-mode matrix from Approach (f): VMM death, virtiofsd death, Runner
restart orphan-reaping; the `backend`-labeled metrics and new microVM metrics
from (g).

- **Interfaces:** produces the per-session runtime-dir layout
  (`<runroot>/microvm/<session>/{vmm.pid,virtiofsd.pid,vsock.port}`),
  `(*MicroVMRuntime) ReapOrphans(ctx) error` called at startup, and the
  metric set (`compass_microvm_boot_seconds`, `compass_microvm_rss_bytes`,
  `compass_microvm_vsock_rpc_seconds`, `compass_microvm_quota_used_ratio`,
  `compass_microvm_canary_ok`). Consumes V2b's process handles.
- **Test cycle:** kill the VMM mid-exec → in-flight exec fails with the
  typed error, teardown completes, no orphan processes or stale vsock ports;
  kill virtiofsd → session torn down, volume intact on host; restart the
  backend with planted orphan pidfiles → reaped.

### V8 — isolation / contract / failure-mode acceptance suite

The proving suite the parent's I1 test cycle demanded (design.md:596-601),
run against the real backend on KVM hardware. This is the task that PROVES
inter-tenant isolation rather than exercising the happy path.

- **Interfaces:** consumes everything above; produces the CI job
  (KVM-labeled runner) and the benchmark report feeding the boot-budget
  measurement (Q-budget).
- **Test cycle:** (1) inter-tenant probe: from guest A, attempt to reach
  guest B's volume path, B's vsock port, the host filesystem, and the host
  metadata/network — all fail; (2) egress fail-closed asserted inside the
  guest netns (V3's tests run under the full backend); (3) S1 contract tests
  pass unchanged on the microVM backend; (4) boot-timeout: a wedged boot is
  killed at the deadline and cleaned; (5) mid-session VMM death (V7's tests
  under the full session lifecycle incl. gateway streams breaking cleanly);
  (6) KVM-absent hard-fail: Runner startup aborts with the capability-naming
  error (D3); (7) boot-latency + RSS benchmark vs the
  container baseline; (8) **in-guest escalation probe:** an agent-uid
  process inside the guest dials the supervisor vsock port over loopback
  (CID 1) and is refused (peer-CID authentication, (c)) — the exec/re-arm
  surface is unreachable from the workload.

## Tasks

- [ ] V1 — backend seam + selection + startup gate
- [ ] V2a — guest image + boot spike (packaging; de-risks V2b)
- [ ] V2b — in-guest supervisor + vsock exec control plane
      (schedule-critical, largest task)
- [ ] V3 — egress-in-guest (fail-closed arm before exec acceptance)
- [ ] V4 — gateway over vsock (AgentGateway over host vsock transport, no proto changes)
- [ ] V5 — preflight + boot canary + hard-fail startup gate
- [ ] V6 — virtio-fs mount-ns isolation + volume quota verification
- [ ] V7 — teardown, crash recovery, observability + metrics
- [ ] V8 — isolation/contract/failure-mode acceptance suite + benchmarks

## Decisions

The six load-bearing forks below were resolved by the human before freeze; the
record is written against these decisions. The reasoning that fixed each is
kept so the executor sees *why*, not just *what*.

1. **D1 — VMM: cloud-hypervisor, and only cloud-hypervisor.** Weighed on four
   axes: (a) virtio-fs — all non-Firecracker candidates qualify; (b) guest
   control-plane maturity — Kata ships kata-agent complete, but we build our
   own supervisor (V2b) for the gateway channel regardless, so the mature
   agent saves little; (c) rootless-podman coexistence — Kata is
   containerd-only ([kata #722](https://github.com/kata-containers/kata-containers/issues/722)),
   i.e. a *second container engine* beside the rootless-podman substrate with
   its own rootless/image/ops surface, a far larger blast radius than a plain
   rootless VMM process; (d) the S1-reserved `Resize`/`ResourceLimits` seam
   (`podman.go:342-351`) — cloud-hypervisor has CPU + memory hotplug
   ([docs/hotplug.md](https://github.com/cloud-hypervisor/cloud-hypervisor/blob/main/docs/hotplug.md))
   *and* machine-to-machine live migration
   ([CH README](https://github.com/cloud-hypervisor/cloud-hypervisor#objectives)),
   which serve D5's in-place-then-relocate resize directly; libkrun's device
   model exposes no memory-hotplug device (virtio list:
   console/block/fs/gpu/net/vsock/balloon(free-page-reporting only)/rng,
   [libkrun README](https://github.com/containers/libkrun#virtio-device-support))
   and its C API carries no resize call; Kata's resize rides containerd.
   cloud-hypervisor is the only option meeting all four, runs rootless as an
   ordinary process, and is a proven Kata VMM. **libkrun (macOS via HVF) is
   *not* adopted** — see D2: native-macOS-embedded is dropped, so the one
   reason to carry a second VMM is gone. cloud-hypervisor runs on KVM (Linux)
   or MSHV, not macOS HVF ([CH README](https://github.com/cloud-hypervisor/cloud-hypervisor#1-what-is-cloud-hypervisor)) —
   acceptable because the only supported hosts are KVM-capable (D2). D1 also
   fixes the **host-side vsock transport shape**: cloud-hypervisor uses hybrid
   vsock (host end is an AF_UNIX socket selected by a `CONNECT <port>`
   preamble, not an AF_VSOCK listener,
   [CH docs/vsock.md](https://github.com/cloud-hypervisor/cloud-hypervisor/blob/main/docs/vsock.md)),
   so the existing Connect/`net.Listener` serving path largely transfers.
2. **D2 — microVM is the *only* runtime; the container path is a transitional
   bootstrap, deleted after trusted Beta.** The parent kept the rootless
   container as the running boundary through Dogfood + trusted Beta
   (design.md:892-894); this record commits to **removing it** once microVM is
   proven, rather than maintaining two runtimes indefinitely. Rationale: a
   second permanent runtime roughly *doubles* the production support and
   bugfix surface, which for a solo maintainer dominates the one-time build
   cost — and microVM must be built regardless (it is the reason the parent
   design exists). Reach is preserved without a second runtime: the managed
   service runs cloud-hypervisor on **elastic, hourly, autoscaling bare-metal**
   instances (AWS `*.metal` in standard ASGs; GCP `c3-*-metal` "consumed and
   managed in the same way as VM instances") — no nesting, so **no ~10%
   nested-virtualization performance tax**; self-hosters provision a
   KVM-capable box *for* Compass (an owned/homelab box, Hetzner-class
   bare-metal, or a nested-virt-enabled hyperscaler instance — GCP any Linux
   VM, Azure Dv3/Ev3+, AWS C8i/M8i/R8i or `.metal`). **Native-macOS-embedded
   is dropped**: macOS has no KVM, Compass is an always-on/overnight workload
   ill-suited to a personal Mac anyway, and macOS users use the managed
   service or point the app at a remote KVM Runner. The ~10% tax therefore
   only ever applies to a self-hoster who *chooses* a nested cloud VM over
   bare-metal — a self-inflicted, clearly-documented tradeoff, never the
   managed path.
3. **D3 — KVM absent ⇒ hard-fail, loudly.** With no container fallback (D2),
   a box without microVM support cannot run Compass. `VerifyMicroVMSupport`
   (V5) fails Runner startup with an error naming the missing capability and
   pointing at the fix ("needs KVM — use the managed service or a KVM-capable
   host"), mirroring `VerifyUsernsRemapSupport`'s name-the-floor posture
   (`podman.go:443-467`). This *supersedes* the parent's frozen
   degrade-to-container default (design.md:600-601), which only made sense
   while the container path existed; a silent isolation downgrade after a KVM
   regression on a multi-tenant box is a security incident, not a degraded
   mode. The `microvm.required` knob collapses to always-on: there is nothing
   to degrade *to*.
4. **D4 — in-guest control: a new `compass-guestd` supervisor over Connect/h2c
   on vsock.** Privilege separation: the supervisor runs as guest root
   (mount, nft arm, spawn-as-uid) and refuses agent-privilege escalation;
   folding it into compass-agent would put root work in the very workload
   process the boundary exists to contain. `compass-guestd` is *also* the
   in-guest Go process that supervises the compass-agent (restart/health/
   lifecycle) — the planned agent-supervisor and the runtime control plane are
   one process. Protocol is Connect over h2c on a vsock stream, reusing the
   AgentGateway's existing stack (`gateway/socket.go`) — one protocol stack,
   one generated-code path, streaming built in. **ttRPC (Kata's choice) is
   rejected**: it is a wire-incompatible second RPC stack whose only benefit
   (lower RSS, [ttRPC README](https://github.com/containerd/ttrpc)) is dwarfed
   by the VM's own RSS. Tradeoff accepted: a session-lifetime guest-root RPC
   server is a new persistent surface the container model lacks (there the arm
   is a one-shot root exec that exits, `agent.go:300-307`), contained by (c)'s
   peer-CID authentication and the uid/capability gate on every exec.
5. **D5 — sizing: hotplug, and one `Resize` API that escalates to relocation.**
   Design for hotplug (D1's cloud-hypervisor enables CPU + memory hotplug), so
   the VM boots at inner-loop size and grows for `ClassResized` ops
   (`compute/compute.go:59-62`) rather than reserving peak RAM. `Resize`'s
   *contract* expresses relocation as an outcome now: attempt **in-place
   hotplug** first; when the host has no headroom, **escalate to relocating
   the session to a larger instance**. The relocation path reuses the parent's
   suspend-idle session-transfer machinery (VMM-agnostic:
   suspend/serialize → reboot on the new box → resume) in preference to CH
   live-migration (which needs migratable/shared storage). This record ships
   `MicroVMRuntime.Resize` returning `ErrResizeNotImplemented` (the same
   posture as `PodmanCLI.Resize`, `podman.go:621-623`); the hotplug + relocate
   *behavior* lands in C3's resize backend, not here.
6. **D6 — guest networking: virtio-net + a passt/gvproxy-class userspace
   backend.** A rootless VMM cannot create a host tap, so networking is a
   userspace concern the backend provides. virtio-net + a passt/gvproxy-class
   backend gives the guest a real netns and its own IP, so the in-guest nft
   ruleset (c) and allowlist resolution against a provisioned `/etc/resolv.conf`
   (`getent`, `egress.go:71-113`) work exactly as in the container today.
   Security posture: passt is an unprivileged host userspace forwarder that
   does no filtering itself; egress is enforced by the in-guest nft ruleset
   guestd arms as root *before* the non-root agent runs, and the agent has no
   `CAP_NET_ADMIN` so it cannot flush/edit nft — the guest *workload* cannot
   modify egress (only a guest-*kernel* compromise could, and that is contained
   by the VM boundary). **TSI is rejected** ([libkrun README](https://github.com/containers/libkrun#networking)):
   no guest netns means egress can no longer be an in-guest nft ruleset and
   must relocate to a host-side proxy, dissolving (c)'s integrity model — and
   it is libkrun-only, which D1 does not adopt. `compass-guestd` provisions the
   guest IP and `/etc/resolv.conf` at boot, before the arm step. An optional
   host-side egress firewall on the VMM's uid MAY be added later as defense
   against a guest-kernel escape; not in this record's scope.
7. **D7 — volume quota: verify, never assign.** The resource-exhaustion bound
   (d) collides with "rootless is hard": project-quota *assignment*
   (`FS_IOC_FSSETXATTR` + `quotactl`) and the loopback-image fallback
   (`mount(2)`) both need `CAP_SYS_ADMIN` the rootless Runner lacks
   (`podman.go:22-24`), and the frozen no-copy invariant forbids swapping the
   virtio-fs volume for a quota-bounded block device. Decision: the Runner
   never *assigns* quota. The multi-tenant deployment provisions the
   session-volume filesystem with per-directory project quota via operator IaC
   at deploy; preflight (V5) *verifies* it is active (a read-only,
   rootless-safe check) and fails startup if absent. V6 ships quota
   *verification*, not assignment.
8. **D8 — deployment topology: the Runner is co-located one-per-box; fleet
   orchestration is a separate control plane. This is settled — do not
   reopen.** The Runner runs *on the same physical box* as the microVMs it
   manages (a node-agent, kubelet-style), managing only that box's local VMs.
   A separate managed **control plane** provisions boxes on the hyperscaler
   and schedules sessions onto per-box Runners. This is decided for three
   reasons, each of which would otherwise resurface as the same question:
   - **vsock is host-local by construction.** The agent↔Runner transport
     (Approach (b)) is virtio-vsock, which only connects a guest to the VMM on
     *its own* host. A central/remote Runner managing VMs across boxes cannot
     use vsock — it would force transport (b) onto a network socket and
     re-harden attribution over a network hop. Co-location keeps the local,
     non-IP hop and the transport this record designs.
   - **It removes the bottleneck, not adds one.** A single central Runner
     relaying every comms/publish/forge frame for the whole fleet is the
     bottleneck. One Runner per box scales horizontally and makes each relay a
     cheap local vsock call — so "run multiple Runners so we don't get
     bottlenecked" is *satisfied by* co-location, not a reason against it.
   - **It preserves the frozen attribution boundary.** The agent stays
     untrusted with no outbound route except through its local Runner, which
     owns the session→account binding structurally (`gateway.go`: "the Runner
     resolves session_id → account … fail-closed") and the ordered `RunnerSeq`
     the Server's gap-detection depends on (`publisher.go`). The agent does
     **not** talk directly to the Server — that would hand an untrusted process
     a Server-facing credential and break loss-detection, to save a hop
     co-location already makes local. The frozen transport-consolidation
     record stands.

   **Out of scope for this record (own design when the managed service is
   built):** the control plane's box provisioning + session scheduling, the
   Runner→Server telemetry fan-in, and any message-bus adoption (NATS/JetStream
   is a candidate for that *trusted* control-plane tier — Runner→Server fan-in,
   durable session-log, inter-Runner scheduling — but is a **bad** fit for the
   untrusted agent↔Runner hop and is not adopted here). This record is scoped
   to the per-box microVM backend only. The control plane is tracked as
   **RIG-2485** (Backlog), to be designed when the managed service is built.

### Deferred (non-load-bearing, resolved in implementation)

- **Q-budget — boot-latency / per-VM-RSS budget values.** Set from V2a/V8
  measurements on real hardware, not invented here. The mechanism (canary +
  benchmark + `backend`-labeled metrics) is in-scope and designed; the
  thresholds follow the data.
- **Q-mountlabel — `MountLabel` on the microVM backend.** SELinux MCS
  relabeling (`podman.go:336-339`) is a shared-kernel concern; inside a guest
  the config-update path likely needs no relabel. Resolved during V2b by
  making `MountLabel` return the empty label and routing the config
  materializer accordingly; escalate only if the relabel path proves
  load-bearing on the microVM backend.
