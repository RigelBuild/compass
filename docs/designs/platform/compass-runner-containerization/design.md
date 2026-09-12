# Containerizing the Compass Runner

Status: Draft — freezes on merge. §Privilege shape names the grants R7 must confirm are *necessary*; R7 narrows the grant set, it does not decide whether containerization works.

Ledger-impact: mints DL-358

## Problem / Intent

Compass ships a Runner that launches each agent session as a microVM
(cloud-hypervisor + virtiofsd + passt) rootless, as an ordinary host process.
Operators who run Compass on Kubernetes have no supported way to deploy it:
there is no Runner container image, no lane that builds one, and no recorded
answer to what such a container would have to be granted in order to boot a
microVM at all.

That last question is the hard one, and it is why this is a design record
rather than a packaging task. A rootless *host* user has affordances a
container does not — setuid `newuidmap`, unrestricted `unshare(2)`, no seccomp
filter — so "the Runner already runs rootless" does not establish that a
locked-down pod can run it. This record decides whether the Runner is
containerized, states exactly what the container is granted and what it is
denied, says where the KVM userland and guest assets live, and defines the
Kubernetes object contract for a fleet whose real workload — the session
microVMs — is invisible to Kubernetes.

Scope is the **core capability**: the image, the privilege shape, and the
generic Kubernetes object contract any conformant cluster can run. Choosing a
cloud, a node provisioner, or a GitOps delivery path for a particular
deployment is an operator concern and out of scope here.

## Approach

**Containerize the Runner as the Kubernetes delivery and lifecycle unit,
running rootless inside an unprivileged pod with a scoped `/dev/kvm` device —
never `privileged: true`.**

The apparent conflict with the runtime record's "launched rootless as an
ordinary host process per session"
(`docs/designs/infra/runtime/compass-elastic-session-runtime/microvm-runner.md`,
§Approach (a) “VMM shape”) dissolves once the two layers are separated. *Rootless* is a property of
the uid the Runner and its VMM children run as. *Ordinary host process*
distinguishes the VMM from a CRI-owned pod-sandbox runtime. A container is a
namespaced process tree: cloud-hypervisor, virtiofsd, and passt run as
ordinary children of the containerized Runner at a non-zero uid, exactly as
they would under systemd.

What the runtime record forbids is *privilege* — its D7 discussion pushes
anything needing a capability the rootless Runner lacks out to operator
provisioning plus preflight verification. That sentence rules out capabilities
as a *requirement*; it does not establish that a `drop: ["ALL"]` +
`RuntimeDefault` pod grants *enough*, because a pod denies affordances the
host-rootless model silently assumed.

**This is a grant-tuning question, not a feasibility one.** The composition is
already verified to boot on Linux with `/dev/kvm`: the frozen
[microVM CI/dev enablement](../../infra/runtime/compass-elastic-session-runtime/microvm-ci-dev-enablement.md)
record runs KVM-backed boot tests as a required leg on GitHub Actions'
`ubuntu-latest`. What a pod adds over that environment is confinement — a
cgroup device controller, a seccomp filter, and a memory cgroup — so R7 asks
**which grants the confinement makes necessary**, not whether microVMs run in
containers. Each plausible R7 outcome costs a wider pod spec (a seccomp
profile, a device plugin, a supplemental gid), all of which this record already
specifies; none of them reopens the container-vs-host ruling. The one result
that *would* reopen it is a requirement for a Linux **capability** or
`privileged: true`, which no identified mechanism in the composition needs.

### Container vs host process: why the container wins

A host systemd service and a container both satisfy the runtime record. The
container wins on four properties an operator needs and systemd does not
provide:

1. **Atomic, rollback-able version delivery.** The image digest names the
   entire userland — Runner binary, cloud-hypervisor, virtiofsd, passt, guest
   kernel and rootfs — as one unit. A host service versions each of those
   independently, and a partial upgrade is a supported state.
2. **A declarative fleet object.** One DaemonSet describes the Runner on every
   eligible node, with a rollout strategy and a health surface, rather than N
   nodes' worth of drifted unit files.
3. **Isolation of the userland from the host.** The VMM binaries and guest
   assets live in the image, not in the node's filesystem, so a node never
   accumulates Compass-specific packages.
4. **No node mutation on upgrade.** Changing the Runner version is a pod
   template change, not a node reprovision.

The cost is the privilege question, which is the subject of §Privilege shape
and R7.

### Net backend: passt, implemented, not an open pick

Networking needs no capability. A rootless VMM cannot create a host tap, so
networking is a userspace concern the backend provides — and the implemented
D6 backend is passt, an unprivileged userspace forwarder. Guest networking is
in-guest; the Runner dials **out** to the Server over gRPC with its per-Runner
token (`go/cmd/compass-runner/main.go`), so the pod needs no host port and no
inbound service.

### Privilege shape (the pod spec — the grant set R7 narrows)

Every grant is justified; anything not listed is denied. R7 confirms which
grants are *necessary* rather than inert, on real hardware, before R3 encodes
them — a grant that proves inert drops out, which is the outcome to hope for. The rulings pick what R7 verifies first; they do not remove the
verification.

- **`/dev/kvm` via a device plugin — NOT `privileged: true`, NOT a raw
  hostPath char-device mount.** `/dev/kvm` is a world-irrelevant char device
  the KVM API gates by fd; the runtime record's D3 requires only that it
  "exists and is openable by the Runner uid". A generic device plugin
  advertises a `devices.kubelet.io/kvm`-style resource and the container
  runtime injects the device node with the correct cgroup device-controller
  allowance; the pod requests it via `resources.limits`.

  The hostPath char-device route is rejected as **non-functional**, not merely
  as a worse posture. **[INFERENCE]** A hostPath mount exposes the node's
  device into the mount namespace, but the cgroup device controller (eBPF-backed
  on cgroup v2) still denies `open()` unless the runtime injects the device via
  CRI's `Devices` field — which is exactly what a device plugin's
  `ContainerAllocateResponse` does — so a hostPath char device functions only
  under privileged mode, which is banned here. Marked inference because this is
  upstream Kubernetes/CRI/cgroup-v2 behaviour, grounded in no artifact in this
  repo, and because it is what demotes hostPath from *worse-posture* to *not a
  candidate*. If it is wrong, hostPath returns to the option set and the
  device-plugin requirement must be re-argued on posture grounds — a change to
  *which* mechanism delivers the device, not to whether containerization works.
  **R7 verifies it**:
  attempt the hostPath route on a real node and confirm `open()` actually fails
  without device-plugin injection.

  The device-plugin implementation is an operator pick, not frozen here; any
  community plugin image an operator pins is thinly maintained and should be
  pinned by digest. The contract this record carries is *scoped device node,
  zero capabilities, no privileged mode*; R7 confirms the device-plugin
  mechanism is the one that delivers it.

- **`securityContext`:** `runAsNonRoot: true`, `runAsUser`/`runAsGroup` fixed
  to a dedicated runner uid, `allowPrivilegeEscalation: false`,
  `capabilities.drop: ["ALL"]`, and seccomp `type: Localhost` with a custom
  profile permitting `unshare`/`mount`/`pivot_root`. The custom profile is a
  new deliverable: authored with R3, staged on-node under the kubelet's seccomp
  root by operator node provisioning, and asserted by R3's test.

  `RuntimeDefault` is not sufficient, which is the whole reason the profile
  exists — see §Alternatives considered.

  No capability is added. Project-quota *assignment*
  (`FS_IOC_FSSETXATTR` + `quotactl`) needs `CAP_SYS_ADMIN` the rootless Runner
  lacks, so it stays an operator-provisioning concern verified by Runner
  preflight (the runtime record's D7). Networking needs nothing (passt).

  One filesystem grant IS required. **[INFERENCE]** `/dev/kvm` is typically
  `root:kvm 0660` — **measured `crw-rw---- root:kvm` on the compass dev box
  (2026-09-12)**, which supersedes the `crw-rw-rw-` reading in the frozen
  microVM CI/dev enablement record; the grant is inert under a world-readable
  mode and required under `0660`, so R7 must record the node's actual mode —
  and device injection grants a *cgroup allowance*, not
  filesystem permission — so the non-root runner uid needs the `kvm` gid via
  `securityContext.supplementalGroups` (or a node-provisioning chmod), or the
  first real `open()` fails `EACCES`. The errno is the observable that separates
  the two layers: a DAC permission denial is `EACCES`, while the cgroup
  device-controller denial in the `/dev/kvm` bullet above is `EPERM`. Marked inference because the device node's
  mode and ownership are properties of the node image's udev rules, not of
  anything in this repo, and because it is the sole justification for the
  `supplementalGroups` grant. **R7 verifies it directly**, and the negative
  control is the one that matters: confirm a non-root uid *without* the kvm gid
  actually fails to open the device. If it opens without the gid, the grant is
  unnecessary and drops out of the contract.

- **`hostNetwork: false`, `hostPID: false`.** Guest networking is in-guest and
  the Runner dials out, so no host ports and no inbound service. hostPID is
  unnecessary because the VMM/virtiofsd children are the Runner's own
  descendants inside the container pid namespace.

  The restart semantics follow deterministically rather than being chosen:
  without `shareProcessNamespace` the container's pid 1 **is** the Runner, so
  Runner death tears down the pid namespace and the kernel kills every
  descendant (VMM, virtiofsd, passt); a kubelet-driven restart kills the
  container cgroup regardless. "Container restart implies full session
  teardown" is therefore forced, not elected — and it guarantees no stranded
  VMM processes. The hostPath runtime dir then correctly serves the reap path:
  after a restart the backend finds only stale pidfiles, which it already
  handles by pidfile plus process-liveness check.

- **hostPath mounts, exactly two:**
  1. the session-volume tree (the project-quota filesystem operator
     provisioning supplies) — read-write, `type: Directory`;
  2. the Runner runtime dir (`--runtime-dir`, default `/run/compass`) plus the
     microVM runroot (`--microvm-runroot`) — a single host tree, read-write.

  Host-visible rather than `emptyDir` because the tree must outlive the **pod**,
  not just the container. An `emptyDir` is pod-scoped and does survive a
  container restart, so that axis does not distinguish them; what it does not
  survive is pod recreation or a node reboot, which is exactly when a stale
  pidfile from a previous pod must still be visible for orphan-reaping. Nothing else: the VMM/virtiofsd binaries and the guest
  kernel/rootfs/initrd ship **in the image**, so no hostPath reaches them.

- **Explicitly absent:** `privileged`, every capability, `hostNetwork`,
  `hostPID`, `hostIPC`, any `/dev` directory mount, any container-runtime
  socket.

### Pod resources, QoS, and eviction: guest RAM is pod RAM

The VMM is an ordinary child of the Runner inside the pod's cgroup, so guest
memory is the VMM process's own memory and is charged to the pod's memory
cgroup: **every session microVM's RAM counts against the Runner pod.** This
holds regardless of the virtio-fs memory mode.

`--memory shared=on` is nonetheless set unconditionally (the argv construction
in `go/internal/runtime/microvm/launch.go`; `BootConfig.MemoryMB` in
`go/internal/runtime/microvm/config.go` is documented as always launched with
`shared=on`), because virtio-fs requires it. Its consequence here is an
*accounting* one, not a charging one: guest memory is memfd/shared mappings
visible in more than one process, so naively summing RSS across
cloud-hypervisor, virtiofsd, and the Runner double-counts it. Size against PSS
or against the configured guest total, never a sum of RSS.

With absent or low memory requests the pod is BestEffort (requests and limits
fully absent) or Burstable with a large usage-over-requests overage — precisely
the pod the kubelet's node-pressure eviction ranks first — and evicting it
kills every session on the node. A
priorityClass helps preemption and eviction ranking only if requests are
honest. The DaemonSet's requests and limits MUST account for the aggregate
guest RAM of the node's session capacity, not just the Runner process itself.
R3's test cycle asserts requests and limits are present and sized to the
session-capacity model.

### Where the KVM userland and guest assets live: in the image

The image carries the Runner binary, cloud-hypervisor, virtiofsd, passt, and
the guest kernel/rootfs/initrd. This is deliberately image-heavy: it is what
makes the digest name the whole userland (§Container vs host process,
property 1) and what keeps hostPath down to two mounts. The guest assets dominate the
image size; that cost is accepted in exchange for atomic version delivery.

### Kubernetes object contract

Stated as generic Kubernetes so any conformant cluster can run it. An operator
supplies the *values* (node labels, taints, the device-plugin resource name,
capacity sizing); this record fixes the *shape*.

- **DaemonSet**, so the Runner lands on every eligible node.
  - `nodeSelector` on an operator-chosen label key, and a matching
    `toleration` for the taint that keeps non-Runner workloads off those nodes.
    Keys and values are the operator's; the contract is that both exist.
  - `updateStrategy` with a bounded `maxUnavailable`: a Runner pod replacement
    terminates that node's sessions (§Privilege shape, restart semantics), so a
    rollout is session-affecting and must be rate-limited.
  - `securityContext`, device resource request, and the two hostPath mounts per
    §Privilege shape.
  - `spec.nodeName` via `fieldRef` into the environment, so the Runner can
    identify its node.
  - A scrape annotation for the metrics endpoint.
  - **Liveness probe: conservative, or absent.** A probe-driven container
    restart is the same full-session teardown as a rollout (§Privilege shape,
    restart semantics) — pid 1 dies and every session on the node dies with
    it — but nothing rate-limits it the way `maxUnavailable` bounds a rollout,
    so a transient health blip costs the whole node's sessions. Prefer a
    readiness probe for traffic gating and either omit liveness or give it a
    failure threshold well past any transient stall. A startup probe is
    unobjectionable.
  - **`terminationGracePeriodSeconds` sized to the reap budget.** On
    termination the kubelet SIGTERMs pid 1 and SIGKILLs after the grace
    period; that window is what lets the Runner shut its VMM children down in
    order and notify the Server. Too short and teardown is hard — still no
    stranded processes (the pid namespace guarantees that), but no graceful
    session drain. Size it to the per-session reap budget times the node's
    session capacity.
- **RBAC**: a dedicated ServiceAccount with the narrowest role the Runner
  actually needs. The Runner dials out to the Server and does not drive the
  Kubernetes API for session work, so this is minimal by construction.
- **priorityClass**: high enough that a Runner pod is not preempted by ordinary
  workload, consistent with the eviction reasoning above.

### Relationship to the elastic-session-runtime record

This record does not amend the runtime record's D3/D6/D7. It *consumes* them
and adds the container/pod layer beneath: D3's "openable by the Runner uid"
becomes a device-plugin grant plus a gid; D6's passt backend is unchanged; D7's
quota assignment stays outside the Runner, satisfied by operator provisioning
and checked by preflight.

## Alternatives considered

### Host-level systemd service — rejected

Satisfies the runtime record and avoids the privilege question entirely, but
loses all four properties in §Container vs host process: no atomic digest, no
declarative fleet object, node-resident userland, and node mutation on every
upgrade. Rejected for operators who are already running Kubernetes; it remains
the right shape for a bare-metal single-node deployment, which this record does
not address.

### `privileged: true` DaemonSet — rejected

Trivially works and is what most VM-on-Kubernetes stacks ship. Rejected: a
privileged pod holds every capability and effectively owns the node, which
defeats the isolation the microVM boundary exists to provide. The whole point
of the microVM is that a session cannot reach the host; a privileged Runner
re-opens that path from the other side.

### `RuntimeDefault` seccomp with no custom profile — rejected

Preferred if it worked, since it needs no on-node artifact. Rejected because
the composition needs `unshare`/`mount`/`pivot_root`, which `RuntimeDefault`
restricts — this is precisely the host-rootless affordance a pod removes, and
the reason §Privilege shape carries a custom `Localhost` profile as a new
deliverable. The cost is real: `Localhost` requires the profile be staged on
the node before the pod starts, which couples the DaemonSet to operator node
provisioning.

### Static pod / runner-in-node-image hybrid — rejected

Bakes the Runner into the node image and manages it as a static pod. Gets
atomic delivery of a sort, but the unit of rollback becomes the node image, so
a Runner version bump is a node reprovision — the exact cost containerizing is
meant to remove.

### Kata / pod-sandbox runtime class — rejected upstream

Already rejected by the runtime record: the Runner runs the VMM as its own
children, not as a CRI-owned pod sandbox. Restated here only because it is the
first thing a Kubernetes reader reaches for.

## Global Constraints

- **No capability, no `privileged`.** The grant set may widen along the axes
  §Privilege shape already names (seccomp profile, device plugin, supplemental
  gid) — that is R7 narrowing or confirming a spec, and is expected. What is
  banned is patching a shortfall with a Linux capability or `privileged: true`:
  if the composition genuinely needed one, the container-vs-host ruling is
  reopened instead. No mechanism in the composition is known to need one.
- **The image is the unit of version.** Anything in the KVM userland or guest
  asset set ships in the image; nothing is expected on the node except the two
  hostPath trees and the seccomp profile.
- **A rollout is session-affecting — and so is anything else that replaces or
  restarts the pod.** Any change to the pod template terminates sessions on
  each replaced node, so every delivery mechanism must rate-limit it. The same
  blast radius applies to a probe-driven restart and to a **node drain**
  (autoscaler, node upgrade, manual drain), neither of which is a delivery
  mechanism and so neither is covered by that rate limit. A fleet
  PodDisruptionBudget can bound *concurrent* drains — it applies to
  eviction-API disruption, though not to the DaemonSet controller's own
  rollout — but it cannot keep a pod alive while its own node drains, so
  draining a Runner node is inherently session-terminating and must be
  scheduled as such.
- **Operator-supplied values stay operator-supplied.** This record fixes object
  shape, not node labels, taint values, capacity numbers, or a GitOps path.
- **Deployment-plane concerns are out of scope.** Which cluster, which node
  provisioner, and how the manifests reach a cluster are operator concerns;
  they are named and deferred here, never described.

## Plan

### R1 — `runner-image/` project

A `Dockerfile` built on a **minimal hardened base** (distroless, or Alpine
where a shell is genuinely needed), carrying the Runner binary,
cloud-hypervisor, virtiofsd, passt, and the guest kernel/rootfs/initrd.

**Not a nix image.** The two mechanisms split by what the image's *runtime* is,
not by what built the artifact: an image that **is** a Nix environment (a CI
step image, a dev/agent shell) earns `nix2container`; a **prebuilt
application** on a minimal base is a Dockerfile built by rootless BuildKit. A
first-party image does not earn the nix path merely because Nix built it or it
ships a compiled binary — build tool and runtime base are independent choices.
The prescribed shape for a nix-built artifact is to `nix build` it and `COPY`
the result onto the base. `nix2container` for app images is rejected on cost: a
per-app regeneration tax plus a maintained fork.

The Runner is squarely the prebuilt-application row: a static `CGO_ENABLED=0`
Go binary that exec's three userland binaries and runs no package manager,
toolchain, or `nix` at runtime. The agent-image lane sits on the *other* row by
name, because that container's job is to be a toolchain. Sharing the
`*-image/` directory shape is not sharing the mechanism.

Distroless is the stronger posture for a managed multi-tenant cluster and fits
the Runner's shape — but the KVM userland binaries come from nixpkgs and carry
store-path interpreter and rpath references, so R1 must settle how they are
made runnable on a minimal base (a static or patchelf'd copy, or an Alpine base
with the loader present). That is R1's one real engineering question.

Test cycle: the image builds reproducibly; the resulting image contains each
expected binary and guest asset; every carried binary actually executes on the
chosen base (the store-path-reference check above, which a layer-contents
assertion alone would miss).

### R2 — publish lane

Extend the release workflow to build and publish the runner image by digest,
built with rootless BuildKit (`buildkitd`/`buildctl`) per the spec cited in R1
— never `docker build`, never a host docker socket. The deployed contract is
the resolved immutable digest (`repo@sha256:…`), not a tag: GHCR has no
server-side tag immutability, so R3's DaemonSet pins the digest.

Test cycle: a tagged run publishes a manifest whose digest is recorded in the
run output; a second build of the same input yields the same digest.

### R3 — DaemonSet + RBAC manifests as config-as-data

Author the object contract from §Kubernetes object contract as plain
manifests, parameterized where §Global Constraints says the operator supplies
values. Test cycle: the rendered objects assert the full §Privilege shape
(no `privileged`, `drop: ["ALL"]`, `runAsNonRoot`, the device resource, the
`Localhost` profile reference, exactly two hostPath mounts); requests and
limits are present and sized per the capacity model; `maxUnavailable` is
bounded.

### R4 — `/dev/kvm` device-plugin delivery

Document and encode the device-plugin requirement, including the resource-name
parameterization and the `supplementalGroups` gid — both confirmed necessary
(or dropped as inert) by R7.
Test cycle: rendered pod spec requests the device resource and carries the gid;
a spec that omits either fails the assertion.

### R5 — entrypoint fix for `--backend microvm`

The image entrypoint must pass agent-image semantics correctly under
`--backend microvm`. Test cycle: the container started with `--backend microvm`
resolves its agent image from the documented flag/env precedence, asserted
without launching a VM.

### R6 — ledger row

Mint DL-358 recording the containerize ruling and the zero-privilege
constraint.

### R7 — privilege-shape spike on real hardware (gates the §Privilege shape freeze)

Confirms which grants the confinement makes necessary. Findings land in
`spike-findings.md` beside this record. It must answer, each with a negative
control:

1. Does an unprivileged pod with the §Privilege shape grants boot
   cloud-hypervisor + virtiofsd + passt end to end?
2. Does the hostPath char-device route actually fail `open()` without
   device-plugin injection? (If it succeeds, the option set reopens.)
3. Does a non-root uid without the kvm gid fail to open `/dev/kvm`? (If it
   opens, the `supplementalGroups` grant drops.)
4. Is the custom `Localhost` profile actually required — does
   `RuntimeDefault` fail, and on which syscall?
5. Does guest RAM appear in the pod's memory cgroup as §Pod resources claims?
6. Does a container restart leave zero stranded VMM/virtiofsd/passt processes?

### Task ordering

R7 gates R3 and R4 (it freezes the contract they encode). R1 gates R2. R5 is
independent. R6 lands with the freeze.

## Tasks

| Task | Deliverable | Depends on |
| ------ | ----------- | ---------- |
| R1 | `runner-image/` Dockerfile on a minimal hardened base | — |
| R2 | publish lane by digest | R1 |
| R3 | DaemonSet + RBAC manifests + render tests | R7 |
| R4 | device-plugin resource + gid wiring | R7 |
| R5 | entrypoint agent-image fix under `--backend microvm` | — |
| R6 | DL-358 ledger row | R7 |
| R7 | real-hardware privilege spike + `spike-findings.md` | R1 |

## Open Questions

### OQ-1 [non-load-bearing] — which grants does confinement make necessary?

Not a feasibility question. The composition boots on KVM today (see §Approach);
R7 determines which of the specified grants — seccomp profile, device plugin,
`supplementalGroups` — are load-bearing rather than inert, so the pod spec can
be narrowed to the minimum that works. Every outcome is a pod-spec edit this
record already anticipates.

The ruling *would* reopen only if some component required a Linux **capability**
or `privileged: true`. No mechanism in the composition is known to: cloud-
hypervisor, virtiofsd and passt are ordinary user binaries by the frozen
runtime record's Global Constraint. Treat that as the low-probability tail, not
the expected case.

### OQ-2 [load-bearing] — is the custom seccomp profile avoidable?

The genuinely open question, and the only one with a real cost attached. If
`RuntimeDefault` suffices, the `Localhost` profile and its on-node staging
requirement both drop, which materially simplifies the operator's job. If it
does not, we ship the profile — a known, bounded cost this record already
specifies, not a setback. R7 item 4 settles it.

### OQ-3 [non-load-bearing] — device-plugin implementation pick

The contract names a resource, not a plugin. Which plugin an operator runs is
an implementation choice; the community options are thinly maintained and want
a digest pin.

### OQ-4 [non-load-bearing] — runner uid and kvm gid values

Fixed values chosen at implementation and single-sourced between the image, the
manifests, and operator node provisioning.

### OQ-5 [non-load-bearing] — session-volume host path and filesystem

The path is operator-supplied; the filesystem must support project quotas for
D7. Default path chosen at implementation.

## Resolved decisions

- **Containerize the Runner** as the Kubernetes delivery unit, rather than a
  host systemd service — only if R7 surfaced a capability requirement, which
  no known mechanism in the composition needs (§Container vs host process).
- **Never `privileged: true`** — the microVM isolation boundary is the reason
  the Runner exists, and a privileged Runner re-opens the host path
  (§Alternatives considered).
- **`/dev/kvm` by device plugin**, not hostPath char device, not privileged
  mode (§Privilege shape).
- **Guest RAM is pod RAM**, so DaemonSet requests must be sized to node
  session capacity, not to the Runner process (§Pod resources).
- **Container restart implies full session teardown** — forced by the pid
  namespace, not chosen (§Privilege shape).
- **The image carries the whole KVM userland and guest assets**, keeping
  hostPath to exactly two mounts (§Where the KVM userland and guest assets
  live).
