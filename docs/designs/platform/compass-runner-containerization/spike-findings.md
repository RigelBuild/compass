# Privilege-shape spike findings

Findings for R7 of
[runner containerization](design.md). **Not yet run** — this file is the
recorded shape of the verification so the questions and their negative controls
are fixed before anyone runs them, rather than being chosen after seeing
results.

The design record's §Privilege shape is a contract *contingent on this spike*.
Two of its grants rest on `[INFERENCE]` claims about upstream
Kubernetes/CRI/cgroup-v2 and node-image behaviour, grounded in no artifact in
this repo. This spike is what converts them to evidence or reopens the option
set.

## Why each item carries a negative control

A positive result alone cannot distinguish "the grant is necessary" from "the
grant is inert and something else made it work". Every item below therefore
states the control that must FAIL, and a control that unexpectedly passes is
the finding — it shrinks the contract.

## Items

### S1 — does a zero-privilege pod boot the composition end to end?

Run the §Privilege shape pod spec on a real node and boot a session microVM:
cloud-hypervisor + virtiofsd + passt, through to an agent session that
executes.

- **Expected:** boots.
- **Negative control:** none needed; this is the load-bearing positive.
- **If it fails:** OQ-1 resolves against containerization and the
  container-vs-host tradeoff reopens. Do **not** grant a capability to make it
  pass — that is the decision this record forbids.
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

- **Expected:** fails `EPERM` (device injection grants a cgroup allowance, not
  filesystem permission).
- **Negative control is the point of the item.** If it *opens* without the gid,
  the `supplementalGroups` grant is unnecessary and drops from the contract,
  along with the gid-value question.
- **Also record:** the node's actual `/dev/kvm` mode and owner, since the
  inference rests on it.

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
