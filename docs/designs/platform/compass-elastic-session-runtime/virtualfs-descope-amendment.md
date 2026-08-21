# Elastic session runtime — amendment: `VirtualFS` descoped from S1 to P2

> **Design amendment.** Amends the frozen elastic session runtime record
> (`docs/designs/platform/compass-elastic-session-runtime/design.md`, RIG-1717,
> merged in PR #446). The merged record is frozen and is **not** rewritten in
> place (a later change adds a record, never rewrites the merged one); this
> amendment records a scope change ruled by Matt during S1 execution and is the
> authority where it and the frozen record disagree. Every `go/internal/*`
> citation is a path in the **`RigelBuild/compass`** monorepo.

Status: Active — ruled by Matt (2026-08-19)
Tracking: RIG-2393 (S1), RIG-2395 (P2)
Amends: RIG-1717 elastic session runtime record (PR #446)

## Problem / Intent

The frozen record's task **S1** lists the `vfs.VirtualFS` source-of-tree seam
(interface + git-checkout backend + provision wiring) as a deliverable
alongside the `compute.ComputeRuntime` seam and the `ContainerRuntime.Resize`
freeze. During S1 execution, building `VirtualFS` surfaced that the seam has
**no production caller at S1** and quietly bakes in an unsettled architectural
decision. This amendment descopes `VirtualFS` from S1 to **P2**, where it
first has a real caller and where its dependent decision is made explicitly.

## Approach

**`VirtualFS` moves from S1 to P2. S1 ships without it.**

Three findings drive the descope:

1. **No S1 caller.** Today the agent self-clones its repos *inside* its own
   container — the Runner never materializes a tree host-side (the
   `AgentRuntime.Launch` path arms egress, installs scoped credentials, and
   creates an **empty** checkout dir as the agent user via `ensureCheckoutDir`,
   `go/internal/runtime/agent.go`; `workspace.go` documents "the agent
   self-clones post-launch"). `VirtualFS`'s destination — the persistent
   volume — does not exist until P2. So at S1 the seam + its git-checkout
   backend would have no production path invoking it: a seam with only a test
   caller.

2. **The elastic/burst path does not depend on it.** `VirtualFS` is only the
   *initial source of tree*. It plays no part in how a heavy op gets compute:
   burst spawns a transient environment **on the same box** (P2 stickiness) and
   shares the session's volume by a bind / virtio-fs **mount** — it never
   transfers a tree (the frozen record's §C3, "The end-state topology", and the
   rejected content-addressed-VFS alternative all say the same: same-volume
   mount, no copy). Cross-box burst — the "move to a larger box" case — is
   explicitly deferred (frozen record OQ 2) and, when built, is a **P2 volume
   attach** concern (network block storage behind the volume lifecycle API),
   not a `VirtualFS` concern. So neither `ComputeRuntime` (S1) nor the burst
   backends (C3) need `VirtualFS`.

3. **It bakes in an unsettled decision.** Taken to its end-state, `VirtualFS`
   moves cloning **Runner-side** (the Runner materializes the tree, mounts it
   into the container), replacing the agent's in-container self-clone. That
   shifts the forge-credential posture: the self-clone uses a scoped
   machine-user token in the agent's own `$HOME`, while a Runner-side clone
   needs forge read-credentials host-side — touching the Server-holds-the-sole-
   forge-credential posture (DL-052). The frozen record does not reconcile this
   with the existing self-clone code; it assumes a "clone-dir workspace today"
   that is not a host-side artifact. That reconciliation is a real design
   decision (who clones, and the credential model), not an S1 implementation
   detail — and it belongs where the persistent volume makes it concrete.

**What S1 ships instead (unchanged by this amendment):** the
`compute.ComputeRuntime` seam + its in-environment passthrough backend + the
fail-closed routing-policy shell (`go/internal/compute`, PR #457), and the
additively-reserved `ContainerRuntime.Resize` verb + `ResourceLimits`
(`go/internal/runtime`, PR #454). These are the two seams with teeth now and
carry no clone/credential entanglement. The agent-self-clone-in-container model
is left untouched (Global Constraint 8: the existing session path stays green;
this amendment strengthens that — S1 now makes **no** change toward Runner-side
cloning).

## Alternatives considered

- **Keep `VirtualFS` in S1 as the frozen record specifies.** Rejected: ships a
  seam + backend + tests with no production caller until P2, and pre-commits
  the self-clone → Runner-clone shift via a seam nothing calls. The record's
  stated rationale for freezing it early ("interop-with-customer-VFS later can
  swap without over-building") does not require the seam to exist *before* it
  has any caller — freezing the shape at P2, when the first backend (volume
  materialization) lands, achieves the same forward-compatibility without the
  dead code.
- **Keep a narrower `VirtualFS` that only manages the FS root (no cloning).**
  Rejected as over-design: with the agent still self-cloning, an FS-root
  manager at S1 wraps `mkdir`/`RemoveAll` — not worth a seam until the volume
  gives it a real job at P2.

## Plan

The descope is a set of concrete edits to the two affected tasks. It adds no
new code task — S1 shrinks, P2 grows.

### S1 (RIG-2393) — remove the `VirtualFS` deliverable

- S1's deliverables are **`ContainerRuntime.Resize` freeze** (PR #454) and
  **`compute.ComputeRuntime`** seam + in-place backend + fail-closed routing
  (PR #457). The `vfs.VirtualFS` seam, its git-checkout backend, the
  `WorkspaceSource` variant, and the provision-materialize wiring are **removed
  from S1** and moved to P2.
- Global Constraint 2 ("every working-tree materialization goes through
  `VirtualFS`") is a **P2-onward** constraint, not an S1 one: at S1 there is no
  working-tree materialization seam, and the agent self-clone remains the
  materialization path until P2 introduces the volume + the seam.

### P2 (RIG-2395) — gains the source-of-tree seam + the clone-model decision

- P2 already owns the persistent session volume + its lifecycle API
  (`CreateVolume`/`Attach`/`Snapshot`/`Archive`/`Restore`/`Expire`). It now also
  owns the **`VirtualFS` source-of-tree seam** (interface + backends) and the
  **provision wiring** that materializes a tree through it onto the volume — the
  seam finally has a real destination and caller here.
- P2 must resolve, as an explicit load-bearing decision, **who clones and the
  credential model**: keep the agent self-clone (materializing onto the volume
  the agent then clones into) vs move cloning Runner-side (host-side clone with
  a Runner-side forge credential, reconciled with DL-052). This is the
  reconciliation the frozen record left implicit; it is now P2's to make.
- The `WorkspaceSource` variant on `runtime.AgentSpec`/`Workspace` lands with P2
  (it only has meaning once a volume-backed source exists).

### Downstream tasks

- **C3 (RIG-2396), D4 (RIG-2397), E5 (RIG-2398)** are unaffected in substance:
  none consumes `VirtualFS` directly. E5's "tree materialized through
  `VirtualFS` onto the persistent volume" wording is satisfied by P2 (which now
  owns that seam) exactly as before — only the seam's *owning task* moved from
  S1 to P2, and P2 was already an E5 dependency.

## Tasks

- [x] Descope decision ruled (Matt, 2026-08-19) and recorded here.
- [x] Close the S1 `VirtualFS` implementation PR (#456) with rationale.
- [ ] Reflect the S1/P2 scope change in the RIG-2393 and RIG-2395 issue bodies.
- [ ] (P2 execution) Build `VirtualFS` + provision wiring + the clone-model
      decision as part of RIG-2395.

Spec-impact: none. Ledger-impact: none. Refs RIG-2393 RIG-1717
