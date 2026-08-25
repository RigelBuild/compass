# Elastic session runtime — amendment: microVM is KVM-only, no degrade-to-container

> **Design amendment.** Amends the frozen elastic session runtime record
> (`docs/designs/infra/runtime/compass-elastic-session-runtime/design.md`, RIG-1717,
> merged in PR #446). The merged record is frozen and is **not** rewritten in
> place (a later change adds a record, never rewrites the merged one); this
> amendment records a direction ruled by Matt (2026-08-23) and is the authority
> where it and the frozen record disagree. Every citation is a path in the
> **`RigelBuild/compass`** monorepo. (The parent record's own `Status:` line
> still reads `Draft` — it freezes on merge regardless of that text, per the
> freeze-on-merge convention; a housekeeping flip of that line, being a change
> to the merged record, is out of scope for this amendment.)

Status: Active — ruled by Matt (2026-08-23)
Tracking: RIG-1717 (elastic session runtime)
Amends: RIG-1717 elastic session runtime record (PR #446)
Refs: RIG-2394 microVM Runner backend record (`microvm-runner.md`, merged PR #488)

## Problem / Intent

The frozen record specifies a **permanent KVM-absent fallback**: on a box
without KVM the runtime **degrades to the rootless container runtime** with an
explicit capability log, "never silently". It says this three times:

- Global Constraint 9 (`design.md:466-467`): "The microVM boundary (I1) adds a
  KVM/nested-virt floor and a microVM-runtime floor (krun/libkrun or kata); a
  box without KVM **degrades to the container runtime** with an explicit
  capability log (I1), never silently."
- I1 test cycle (`design.md:600-601`): "the **KVM-absent path degrades to the
  container runtime** with an explicit capability log, never silently."
- Task I1 (`design.md:828`): "**KVM-absent degrade-to-container path** (parallel
  with M0/S1; consumed by C3)."

That is no longer the direction. Matt ruled (2026-08-23): **the runtime is
KVM-only; it does not degrade to the container runtime.** The newer microVM
Runner record already reflects this and supersedes the parent's degrade
language on both points:

- **KVM-only end state.** `microvm-runner.md:63-65` (D2): "the container path is
  **removed** and microVM becomes the **sole runtime** (D2); the container
  backend is a transitional bootstrap, not a permanent second runtime."
- **KVM-absent is a hard-fail, not a degrade.** `microvm-runner.md:211` (§(e),
  titled "Preflight + boot canary + KVM-absent hard-fail") + `:218`
  (`VerifyMicroVMSupport`, static preflight at Runner startup: "`/dev/kvm`
  exists and is openable by the Runner uid").

The parent record still literally reads "degrades to the container runtime",
with no pointer to the override, so a reader of the frozen record gets the wrong
end-state. This amendment reconciles it.

## Approach

**The permanent KVM-absent degrade-to-container path is removed. The microVM
runtime requires KVM; a KVM-absent host is a hard-fail at preflight
(`VerifyMicroVMSupport`, `microvm-runner.md:218`), not a runtime that quietly
falls back to shared-kernel containers.**

Reconciled reading of the three superseded clauses:

- **Global Constraint 9** (`design.md:466-467`) — the "a box without KVM
  degrades to the container runtime … never silently" clause is superseded. The
  KVM/nested-virt floor and the microVM-runtime floor stand; below the KVM
  floor the Runner **hard-fails** (the capability log becomes a startup error,
  not a fallback to a lesser boundary).
- **I1 test cycle** (`design.md:600-601`) — the "KVM-absent path degrades …"
  assertion is superseded by the hard-fail assertion: the KVM-absent path is a
  legible preflight failure (`VerifyMicroVMSupport` errors below the floor,
  naming the required capability), never a silent or a degraded run.
- **Task I1** (`design.md:828`) — the "KVM-absent degrade-to-container path"
  deliverable is removed from I1's scope; I1 delivers the microVM backend and
  its KVM-absent hard-fail preflight, not a degrade path.

### What this amendment does NOT change

The **transitional** role of the rootless container is untouched — this
amendment removes only the *permanent end-state fallback*, not the bootstrap
timeline:

- `design.md:892-894` (OQ-5, the resolved inter-tenant-boundary decision):
  "Through Dogfood + trusted-tenant Beta the rootless container remains the
  running boundary; I1 lands the microVM before the first external multi-tenant
  tenant." This stays exactly as frozen, and `microvm-runner.md:60-65` restates
  it: the container is the running boundary **through Beta**, then removed. The
  container is a transitional bootstrap, not a KVM-absent fallback — the two are
  different roles, and only the fallback role is retired here.
- The microVM boundary itself (OQ-5 / Task I1's core), the seams, the volume
  lifecycle, and every other frozen decision are unchanged.

## Alternatives considered

- **Keep the degrade-to-container path as a self-host / KVM-absent
  convenience.** Rejected by Matt (2026-08-23): the runtime is KVM-only. A
  permanent shared-kernel fallback for a boundary whose entire purpose is
  hardware isolation against untrusted code is a standing hole in the security
  posture the microVM exists to provide, and it splits every downstream path
  (C3 burst, D4 density) into two runtime shapes forever. A KVM-absent host does
  not get a lesser boundary; it does not run.
- **Leave the parent record as-is and rely on the newer record's override.**
  Rejected: the frozen parent still reads "degrades to the container runtime" in
  three places with no pointer to `microvm-runner.md`'s hard-fail, so a reader
  grounding in the parent gets a falsified end-state. The corpus reconciles
  drift by adding an authoritative amendment (the `virtualfs-descope-amendment.md`
  precedent in this directory), not by leaving a frozen record silently wrong.

## Tasks

- [x] Direction ruled (Matt, 2026-08-23) and recorded here.
- [ ] (RIG-2394 execution) I1's `VerifyMicroVMSupport` preflight hard-fails on a
      KVM-absent host per `microvm-runner.md:211-228`; no degrade-to-container
      code path is built.

Spec-impact: supersedes the KVM-absent degrade-to-container clauses of the
elastic-runtime record (Global Constraint 9, I1 test cycle, Task I1); the
transitional-container timeline (OQ-5, `design.md:892-894`) is unchanged.
Ledger-impact: none (the parent's degrade clause and RIG-2394's KVM-only/hard-fail
decisions are record-level, not ledgered).
Refs RIG-1717 RIG-2394
