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
  requirement). It runs at **`compass.rigel.build`**. Its code lives in a
  **private monorepo**, not this one. **It does not exist yet — it is a
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
  hosted control surface, and multi-tenant scheduling live in the private
  monorepo. When a design touches one, state that it is managed-plane and
  out of scope; do not design it here.
- **The core must not assume it is single-tenant, nor assume it is managed.**
  Prefer a seam the managed service can extend over a choice that only fits one
  product. A store, an endpoint, or a URL is configured at deploy, never
  hardcoded to one product's shape.
- **Do not name the private monorepo.** Refer to "the private monorepo" or "the
  managed multi-tenant service" — never a repo proper name (applies the
  describe-behavior-directly principle from [`AGENTS.md`](../../AGENTS.md)
  Hygiene).

## Deploy-time differences the core already carries

- **Public URL is per-deployment.** Managed is `compass.rigel.build`; a
  self-hosted deploy sets its own. The core reads a public-base-URL config value
  (flag + env), never a hardcoded host — see
  [`compass-linear-agent-responder`](../designs/product/compass-linear-agent-responder/design.md).
- **Bundled dependencies default on, with a clean external opt-out.** The
  self-hosted stack bundles its postgres by default and exposes
  `--database-external`; the managed tier supplies its own. Future bundled deps
  follow the same "bundle-by-default, opt-out for managed" shape — see
  [`compass-distribution`](../designs/platform/compass-distribution/design.md).

## Canonical statement

The load-bearing architectural framing lives in the runtime record,
[`compass-elastic-session-runtime` §"OSS core and managed service"](../designs/infra/runtime/compass-elastic-session-runtime/design.md):
"Compass ships as two products over one shared core." This concept doc lifts
that framing to always-on orientation so every agent holds it before designing,
not only the ones who read that record.
