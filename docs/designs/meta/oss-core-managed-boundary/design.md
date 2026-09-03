# The OSS-core / managed-service boundary

Status: Draft

Tracking: RIG-2861

> **Method record.** This formalizes where OSS-core versus managed-only work
> lands and how core capability is framed, so future records route correctly.
> It governs documentation and routing conventions only; it owns no code.

## Problem / Intent

Managed-service features frequently land in the OSS core — multi-tenancy,
the Server↔Runner topology, and the NATS eventing substrate are the live
example (the RIG-2861 record,
`docs/designs/infra/runtime/compass-managed-multitenancy/design.md`). Without
a stated convention, agents do not know what belongs in the public
`RigelBuild/compass` core versus the managed plane (built out of tree), and
public records risk "managed-service" framing for what is really core
capability.

## Approach

### Framing

Features that land in the OSS core to enable the managed product are
described by **what they do for the OSS product** — multi-tenant in one
deployment, horizontally scalable across instances, HA-capable — because
that is literally true and makes the OSS product better. The managed service
is a **consumer and operator** of that core, never the subject of the
record. This is the parent record's "one architecture, two products" seam
(`docs/designs/infra/runtime/compass-elastic-session-runtime/design.md:71-101`),
which already draws the line: this record designs a change to the OSS core,
and nothing in the managed control plane (tenant orchestration, billing, the
hosted control surface) is designed here — those are managed-plane concerns,
built out of tree.

### Boundary test

> Does the public single-tenant product compile and run this?

- **Yes** → OSS core (`RigelBuild/compass`, records under `docs/designs/`
  here).
- **No, because it needs cloud infrastructure, billing, or
  tenant-provisioning orchestration** → managed plane, out of tree (built as a
  separate private product; not designed or mirrored here).

### Examples

| Lands in | Work |
| --- | --- |
| OSS core | Tenancy schema and RLS enforcement |
| OSS core | Server↔Runner connection topology |
| OSS core | NATS eventing substrate |
| OSS core | The single-binary embedded-NATS default |
| Managed plane (out of tree) | Cloud deployment IaC (Pulumi, Kubernetes manifests) |
| Managed plane (out of tree) | Tenant provisioning and billing orchestration |
| Managed plane (out of tree) | Operational runbooks for the hosted service |

The RIG-2861 tenancy record is the worked example of
core-capability-under-managed-motivation: motivated by the managed product,
designed and framed as OSS-core capability, with the managed deployment a
configuration of it.

## Tasks

This record owns doc-convention adoption only — no code tasks. New design
records apply the framing and boundary test above at authoring time.

## Open Questions

None load-bearing. The boundary test is a routing convention; a genuinely
ambiguous case (a feature the public product runs but only the managed
product exercises) is resolved at design time in that record, citing this
one.
