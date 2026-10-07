# Privilege-shape spike findings

Findings for R7 of
[runner containerization](design.md). Run 2026-10-05; results are under
[Results](#results). The item plan below was fixed before the run and is kept
unchanged so the controls stay visibly chosen in advance.

**This spike narrows a pod spec; it does not ask whether containerization
works.** The composition already boots on Linux with `/dev/kvm` — the
[microVM CI/dev enablement](../../infra/runtime/compass-elastic-session-runtime/microvm-ci-dev-enablement.md)
record runs KVM-backed boot tests as a required leg on GitHub Actions'
`ubuntu-latest`. What a pod adds is confinement: a cgroup device controller, a
seccomp filter, and a memory cgroup. So every item below asks **which grant the
confinement makes necessary**, and each answer costs at most a wider pod spec
the design record already specifies.

Two grants rest on `[INFERENCE]` claims about upstream Kubernetes/CRI/cgroup-v2
and node-image behaviour, grounded in no artifact in this repo. Converting them
to evidence is the point: the valuable outcome is a *smaller* contract, since a
grant that proves inert drops out.

## Why each item carries a negative control

A positive result alone cannot distinguish "the grant is necessary" from "the
grant is inert and something else made it work". Every item below therefore
states the control that must FAIL, and a control that unexpectedly passes is
the finding — it shrinks the contract.

## Items

### S1 — does the confinement hold, given the composition already boots?

The KVM baseline is established (see above), so this item is scoped to what
confinement changes. Run the §Privilege shape pod spec on a real node and boot
a session microVM: cloud-hypervisor + virtiofsd + passt, through to an agent
session that executes.

- **Expected:** boots. S2-S4 attribute any shortfall to a specific layer, so
  this item's job is to say whether the assembled spec is sufficient, not to
  establish feasibility.
- **Negative control:** none needed; this is the load-bearing positive.
- **If it fails:** expect the cause to be one of the named grants, and widen
  that grant — a pod-spec edit, not a design change. Only a requirement for a
  Linux **capability** or `privileged: true` reopens the container-vs-host
  ruling, and no known mechanism needs one.
- **Record:** the failing syscall and the component that needed it.

### S2 — does the hostPath char-device route actually fail?

Mount `/dev/kvm` as a hostPath char device, with no device plugin, in an
unprivileged pod, and attempt `open()`.

- **Expected:** `open()` fails (the cgroup device controller denies it without
  CRI `Devices` injection).
- **This IS the negative control** for the design record's claim that hostPath
  is non-functional rather than merely worse posture.
- **If it succeeds:** the `[INFERENCE]` is wrong, hostPath returns to the
  option set, and §Privilege shape's device-plugin requirement must be
  re-argued on posture grounds alone.

### S3 — is the `kvm` gid grant necessary?

With the device plugin injecting the device, run as the non-root runner uid
**without** `supplementalGroups`, and attempt `open()`.

- **Expected:** fails `EACCES` (device injection grants a cgroup allowance, not
  filesystem permission, and a DAC denial is `EACCES`). **The errno
  discriminates the layer**: seeing `EPERM` instead would mean the cgroup
  device controller denied the open, not the filesystem — which confounds this
  item with S2 and means the gid question is still unanswered. Record the errno
  verbatim, not just pass/fail.
- **Negative control is the point of the item.** If it *opens* without the gid,
  the `supplementalGroups` grant is unnecessary and drops from the contract,
  along with the gid-value question.
- **Also record:** the node's actual `/dev/kvm` mode and owner, since the
  inference rests on it. The compass dev box measures `crw-rw---- root:kvm`
  (2026-09-12), under which the grant is required; a world-readable mode would
  make it inert, so the node's own mode is the thing that decides it.

### S4 — is the custom `Localhost` seccomp profile required?

Run the same pod under `RuntimeDefault` instead of the custom profile.

- **Expected:** fails on `unshare`, `mount`, or `pivot_root`.
- **Record which syscall and which component** — that is what justifies each
  permit in the profile, and an over-broad profile is a real cost.
- **If `RuntimeDefault` suffices:** OQ-2 resolves, the custom profile drops,
  and with it the on-node staging requirement that couples the DaemonSet to
  operator node provisioning. This is the most valuable possible outcome of the
  spike and must not be assumed away.

### S5 — does guest RAM appear in the pod's memory cgroup?

Boot a session with a known guest memory size and read the pod's
`memory.current`.

- **Expected:** pod memory rises by approximately the guest size, confirming
  §Pod resources' "guest RAM is pod RAM".
- **Negative control:** a pod with no session running shows only the Runner's
  own footprint.
- **If guest RAM is NOT charged to the pod:** the requests/limits sizing model
  is wrong and R3's assertion changes; the eviction risk argument weakens.

### S6 — does a container restart strand any process?

Kill the Runner (pid 1) mid-session and enumerate node processes.

- **Expected:** zero surviving cloud-hypervisor, virtiofsd, or passt
  processes — the pid-namespace teardown reaps them.
- **Negative control:** the same enumeration *before* the kill must find them,
  or the check proves nothing.
- **Also confirm:** the restarted Runner finds only stale pidfiles in the
  hostPath runtime dir and reaps them by its existing pidfile plus
  process-liveness path.

## Reporting

Each item records: what ran, the observed result, the control's result, and
whether the contract changed. An item whose control was not run is reported as
**not verified**, never as passing — a positive without its control is the
failure mode this file exists to prevent.

## Results

**Status: R7 is not complete.** These are observations on a nested-KVM VM, not
the real-hardware run R7 requires, so no item below closes an R3 or R4 gate.
They do show that the opening hypothesis, that every answer costs at most a
grant the record already names, **did not hold**: S1 needs grants and a node
setting outside §Privilege shape, and one of them conflicts with the S3 grant.
The design record now adopts that wider shape. Every item must still be re-run
on the target node before R3 encodes the pod spec.

### Environment

- **Node:** an isolated single-node k3s in an Ubuntu 24.04 VM with nested KVM.
  The S1, S2, and S3 controls show it enforces the device cgroup, seccomp, and
  AppArmor. Version strings were not captured with the evidence.
- **Image:** the runner image built from compass `9a39795`, imported into the
  node as `localhost/compass-runner:r7`.
- **Boot harness:** the repo's `TestNetOnlyBootSmoke` (passt only) and
  `TestFullBoot` (passt + virtiofsd + cloud-hypervisor), built from the same
  commit and run as pid 1 under the pod spec under test.
- **Device plugin:** `ghcr.io/squat/generic-device-plugin` advertising
  `devic.es/kvm` for the existing `/dev/kvm`; it does not change the mode.
- **Node `/dev/kvm`:** `crw-rw---- 0:993` unless a run states otherwise; each
  evidence block records the mode and sysctl it ran under.
- **Rootless kind was tried first and discarded.** hostPath control devices
  opened there, so it did not enforce the device cgroup. That run's output was
  not kept with this evidence: **not verified**.

### Summary

| Item | Observed on the VM | Implication for the record |
| --- | --- | --- |
| S1 | Boots only with AppArmor unconfined, a user-namespaced pod with unmasked `/proc`, a node sysctl, and node `/dev/kvm` at `0666` | §Privilege shape widened to this shape |
| S2 | hostPath `open()` fails `EPERM`; privileged control opens | Supports the inference |
| S3 | Without the kvm gid, `open()` fails `EACCES`; with it, opens | Supports the grant, but it is inert under S1's user namespace |
| S4 | `RuntimeDefault` fails; the custom `Localhost` profile boots | Points to OQ-2 closing: ship the profile |
| S5 | Guest RAM is charged to the pod as `shmem` | Supports the model |
| S6 | Zero processes stranded after a pid 1 kill | Supports the model (pidfile reap not verified) |

### S1 — the assembled spec

**Result: the §Privilege shape as written does not boot.** Every launch fails
in the sandbox setup of passt or virtiofsd, before cloud-hypervisor starts.
Both tests PASS only with the full set below. Each row removes one grant from
that set, with the node sysctl at `0`:

| Removed | Observed |
| --- | --- |
| `Localhost` seccomp (to `RuntimeDefault`) | passt: `Couldn't create user namespace: Operation not permitted` |
| `appArmorProfile: Unconfined` (to `RuntimeDefault`) | passt: `Failed to remount /: Permission denied` |
| `procMount: Unmasked` (to `Default`) | NetOnly PASS; FullBoot fails, virtiofsd: `Error entering sandbox: MountProc(... Operation not permitted)` |
| `hostUsers: false` | Not removable alone: the API rejects `Unmasked` without it. Removing both gives the `MountProc` failure above |
| node sysctl to `1` | passt: `Failed to detach isolating namespaces: Operation not permitted`, even with every pod grant |

- **AppArmor** is a confinement layer the record does not name. The
  container runtime's default profile denies the `mount` passt's sandbox
  performs inside its own user namespace.
- **Masked `/proc`:** virtiofsd's `--sandbox=namespace` mounts a fresh `/proc`
  in its namespace. The kernel refuses that while the container's `/proc` has
  masked or read-only submounts, and only `procMount: Unmasked` removes them.
- **Node sysctl:** with `kernel.apparmor_restrict_unprivileged_userns=1` passt
  fails even under every pod grant; the working set needs `0`. The harness set
  the value before each run, so the Ubuntu 24.04 image default is **not
  verified**. If the target node image enables it, operators must set `0`.
- **No Linux capability and no `privileged: true` was needed.** The pod stays
  `drop: ["ALL"]`, `runAsNonRoot`, `allowPrivilegeEscalation: false`. The
  container-vs-host ruling stands.

**Open interaction (S1 × S3):** under `hostUsers: false`, the injected
`/dev/kvm` shows as `65534:65534` in the pod (`crw-rw----`). The host kvm gid
is outside the pod's id mapping, so with `supplementalGroups: [993]` `open()`
still fails `EACCES`, and the full set fails `/dev/kvm is not openable` with
the node device at `0660`. The passing runs used the node device at `0666`.
Measured: a user-namespaced pod booted only with a world-rw node device. A
device plugin that sets the mode per pod is untested. The design record adopts
node mode `0666` (§Privilege shape).

### S2 — hostPath char device

`/dev/kvm` and the control `/dev/fuse` were mounted as hostPath `CharDevice`
into an unprivileged pod (uid 10001, groups include 993, `drop: ["ALL"]`, no
device plugin). In the pod `/dev/kvm` was `crw-rw---- 0:993` and `/dev/fuse`
`crw-rw-rw-`. Both `open()` calls failed with **`EPERM`** (`Operation not
permitted`). A world-rw mode failing rules out DAC, so this is the cgroup
device controller. **Control:** the same hostPath in a `privileged: true` pod
opened. The record's `[INFERENCE]` holds: hostPath is non-functional, not
merely a worse posture.

### S3 — the kvm gid

With device-plugin injection (`devic.es/kvm: 1`), non-root uid 10001, and the
device `crw-rw---- 0:993` in the pod:

- With `supplementalGroups: [993]`: opens.
- **Control**, without it: fails **`EACCES`** (`Permission denied`), so a DAC
  denial, not the cgroup.

The grant is necessary under the node's `0660` mode, but inert under S1's
`hostUsers: false` (see above).

### S4 — seccomp (OQ-2)

`RuntimeDefault` fails at either sysctl value: passt reports `Couldn't create
user namespace: Operation not permitted`. No syscall trace was captured, so
the exact denied syscall is **not verified**. The working profile is the
container runtime's default profile plus an unconditional allow of `unshare`,
`mount`, `umount2`, `pivot_root`, `setns`, `clone`, and `clone3`, staged at
`/var/lib/kubelet/seccomp/profiles/compass-runner.json`. On this node the
`Localhost` profile and its on-node staging are required; OQ-2 closes once the
real-node run agrees. This spike did not minimize the allow list; R3 should
narrow it with the same remove-one test.

### S5 — memory cgroup

Pods ran the full S1 set with a 2048 MiB guest. The harness wrote 900 MiB to
guest `/tmp` over the guest-control `Exec` RPC (`dd` reported
`943718400 bytes ... copied`). Pod cgroup values:

| Pod | `memory.current` | `shmem` |
| --- | --- | --- |
| Control: no session (runner exits; a `sleep` sidecar holds the pod) | <1 MiB | 0 MiB |
| Guest booted, RAM not written | 229 MiB | 206 MiB |
| Guest booted, 900 MiB written | 1119 MiB | 1089 MiB |

The control is a pod-level baseline only: the Runner binary was not running
in it. The rise of 890 MiB (883 MiB shmem) tracks the guest write. The guest's
`memfd:ch_ram` mapping (`--memory shared=on`) is charged to the pod as
`shmem`, and only as pages are touched. §Pod resources holds. Size requests
and limits to configured guest totals; a fresh guest under-reports until it
uses its RAM.

### S6 — container restart

**Control:** before the kill, the node showed cloud-hypervisor, passt, and two
virtiofsd processes from the runner image. pid 1 was killed via
`crictl stop -t 0`; the container exited with code 137. **After the kill: zero**
cloud-hypervisor, virtiofsd, or passt processes on the node. The pid namespace
teardown reaps them. The stale-pidfile reap on restart was **not verified**:
the harness used an `emptyDir` runtime dir and is not the Runner binary.
