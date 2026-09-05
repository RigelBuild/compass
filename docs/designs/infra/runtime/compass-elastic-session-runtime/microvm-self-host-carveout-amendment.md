# Elastic session runtime — amendment: microVM KVM-only carved out for self-host single-tenant

> **Design amendment.** Amends the frozen microVM KVM-only amendment
> (`docs/designs/infra/runtime/compass-elastic-session-runtime/microvm-kvm-only-amendment.md`,
> RIG-1717/RIG-2394, ruled by Matt 2026-08-23). That amendment is frozen and is
> **not** rewritten in place (a later change adds a record, never rewrites the
> frozen one); this amendment records the self-host carve-out ruled by Matt
> (2026-08-31, DL-325) and is the authority where it and the KVM-only amendment
> disagree. Every citation is a path in the **`RigelBuild/compass`** monorepo.

Status: Active — ruled by Matt (2026-08-31)
Tracking: RIG-3070 (runner adoption strategy)
Amends: RIG-1717/RIG-2394 microVM KVM-only amendment (`microvm-kvm-only-amendment.md`)
Refs: DL-325; runner-adoption-strategy record (`../compass-runner-adoption-strategy/design.md`, §The ruled topology)

## Problem / Intent

The KVM-only amendment reads as an absolute end-state: "the runtime is
KVM-only; it does not degrade to the container runtime"
(`microvm-kvm-only-amendment.md:34-35`), and "A KVM-absent host does not
get a lesser boundary; it does not run" (`:96-97`). That posture was ruled
for the boundary whose purpose is isolating **untrusted tenants** from each
other.
The runner-adoption-strategy record (DL-325, RIG-3070) splits the runner end
state by trust model, which carves the podman entry tier back in for
**self-host single-tenant** deployments — where the operator is the only
tenant and there is no untrusted code to isolate. A reader grounding in the
KVM-only amendment sees the absolute with no pointer to that carve-out; this
amendment supplies the pointer so the frozen amendment is not silently wrong
— the reconciliation mechanism its own Alternatives named (the
`virtualfs-descope-amendment.md` precedent in this directory).

## Approach

**The KVM-only posture is RATIFIED for untrusted multi-tenant operation and
AMENDED for self-host single-tenant.** The authoritative reconciled reading
lives in DL-325 and the runner-adoption-strategy record's §The ruled
topology; this amendment does not restate it, it points at it:

- **Untrusted multi-tenant** — unchanged. The microVM/KVM hardware boundary
  is required; a KVM-absent host does not run. The KVM-only amendment stands
  verbatim.
- **Self-host single-tenant** — amended. Podman is a permanent, supported
  entry tier requiring no `/dev/kvm`; microVM is the recommended (not
  required) upgrade for defense-in-depth or an operator running untrusted
  code. Podman here is a first-class runtime choice, not a fallback and not a
  lesser boundary imposed on an unwitting tenant.

The carve-out is scoped precisely: it does not reopen the KVM-only
amendment's retirement of the *permanent KVM-absent degrade-to-container
fallback* for the multi-tenant boundary (that stays retired); it rules that
self-host single-tenant is a distinct deployment where podman is a chosen
tier, not a degrade.

## Alternatives considered

- **Leave the KVM-only amendment as-is and rely on DL-325's override.**
  Rejected for the same reason that amendment rejected the identical move for
  its own parent: a frozen record read as an absolute, with no pointer to the
  override, falsifies the end-state for a reader grounding in it. This
  amendment is that pointer.

## Tasks

- [x] Self-host carve-out ruled (Matt, 2026-08-31) and recorded as DL-325.
- [x] Pointer from the KVM-only amendment's directory to DL-325 recorded here.

Spec-impact: none (the authoritative ruling is DL-325; this is the
record-level pointer). Ledger-impact: none (DL-325 already carries the
ruling; this amendment mints no row). Refs RIG-3070 RIG-1717 RIG-2394
