# Self-hosted and managed: two products, one core

Compass ships as **two products over one shared core**. Every design and every
agent building Compass must hold this split — it decides where a change lives,
what is in scope for a design record in this repo, and which assumptions a
feature may make about its environment.

## The two products

- **Self-hosted Compass — the open-source core, this repository.** The entire
  self-hosted product lives in `RigelBuild/compass` (AGPL). A deployer runs the
  headless stack (`compass-stack up` — server + runner + postgres) on their own
  KVM-capable host and reaches it at **whatever URL they choose**. This is the
  only product whose code is in this repo, so it is the product every design
  record here designs.
- **Managed Compass — the private, commercially-licensed multi-tenant service.**
  A hosted, multi-tenant service that **reuses the self-hosted core rather than
  forking it**, and adds the control plane the core does not have (tenant
  orchestration, billing, cross-tenant fleet health, and inter-tenant isolation
  as a product
  requirement). It runs at **`compass.rigel.build`**. It is built **out of
  tree**, as a separate private product. **It does not exist yet — it is a
  near-future buildout** — but designs land now with it in view so the core
  stays a clean base for it.

One codebase in this repo is the core; the managed service is a second product
built on top of it, out of tree.

## What this means for a design in this repo

- **The core is the product you design here.** A design record under
  `docs/designs/` designs a change to the self-hosted core. Its code citations
  resolve against this repo; its tasks land here.
- **Managed control-plane concerns are out of scope for this repo — name them,
  then defer.** Tenant orchestration, billing, cross-tenant analytics, the
  hosted control surface, and multi-tenant scheduling are managed-plane
  concerns. When a design touches one, state that it is managed-plane and out
  of scope; do not design it here, and do not point at where it is designed.
- **The managed product's end-state and rollout stay out of this repo.**
  Managed *capability* implemented in the core belongs here, framed as core
  capability (what it does for the self-hosted product). But the managed
  *product's* end-state, deployment shape, and rollout sequencing — "the
  managed deployment drops X at the first-external-tenant milestone," "the
  managed service's sole runtime is Y" — are private product roadmap, not core
  capability. Build the seam the managed product can extend and describe the
  core default; do not describe or sequence the out-of-tree end-state.
- **The core must not assume it is single-tenant, nor assume it is managed.**
  Prefer a seam the managed service can extend over a choice that only fits one
  product. A store, an endpoint, or a URL is configured at deploy, never
  hardcoded to one product's shape.
- **Never name or point at the private repo.** Do not name it, and do not
  point at it as a place — no "the private monorepo," no "designed there," no
  "built in the private monorepo." When the other product must be referred to
  at all, say **"the managed service"** (the product, as a consumer/operator
  of the core) — generically, and only when unavoidable; prefer describing the
  core capability directly so it need not be named. (Applies the
  describe-behavior-directly principle from [`AGENTS.md`](../../AGENTS.md)
  Hygiene.)
- **Never cite it as an authority.** This is the leak that survives the rule
  above, because it can read as ordinary technical writing: attributing a
  decision to the other repo's spec — "its frozen spec splits X this way,"
  "its words: …," "rejected there on cost." Naming no repo does not fix it;
  "an internal spec says" leaks the same fact, that the reasoning lives
  somewhere a reader cannot go. **A record in this repo argues its own
  position from its own reasons.** If the argument is sound, state it here and
  own it; if it is only true because another document said so, it does not
  belong in a public record. A reader must never be told that the real
  justification is elsewhere.
- **What is not enforceable.** `moon run orion-ref-gate:check` fails closed on
  the literal repo name, and that is the whole of the automation. The
  place-pointer and authority-citation bans are **deliberately not gated**:
  the phrasings are open-ended, and a phrase-list gate over this corpus is
  mostly false positives — the convention and the gate's own source discuss
  the ban constantly. So these two rules rest entirely on the author and the
  reviewer. Treat them as load-bearing, not aspirational: nothing downstream
  will catch a miss.

## Deploy-time differences the core already carries

- **Public URL is per-deployment.** Managed is `compass.rigel.build`; a
  self-hosted deploy sets its own. The core reads a public-base-URL config value
  (flag + env), never a hardcoded host — see
  [`compass-linear-agent-responder`](../designs/server/compass-linear-agent-responder/design.md).
- **Bundled dependencies default on, with a clean external opt-out.** The
  self-hosted stack bundles its postgres by default and exposes
  `--database-external`; the managed tier supplies its own. Future bundled deps
  follow the same "bundle-by-default, opt-out for managed" shape — see
  [`compass-distribution`](../designs/infra/release/compass-distribution/design.md).

## Canonical statement

The load-bearing architectural framing lives in the runtime record,
[`compass-elastic-session-runtime` §"OSS core and managed service"](../designs/infra/runtime/compass-elastic-session-runtime/design.md):
"Compass ships as two products over one shared core." This concept doc lifts
that framing to always-on orientation so every agent holds it before designing,
not only the ones who read that record.
