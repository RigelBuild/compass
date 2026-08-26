# Tokens and billing: who brings the tokens, what Rigel charges for

Compass runs LLM agents, so someone pays for the tokens and someone pays for the
compute they run on. This doc fixes **who brings which** and **what Rigel
actually bills for**, because it is load-bearing for the observability and
in-product-data design and easy to get backwards. It composes with
[`self-host-and-managed.md`](./self-host-and-managed.md) — the split there
(open-source core vs private managed service) is what makes the billing model
asymmetric.

## Tokens: the user brings them (BYOK / BYO cloud subscription)

**Rigel does not provide LLM tokens.** In both products, the user supplies their
own model access — either their own API keys, or their own cloud provider
subscription — and Compass runs against those. Rigel does not resell tokens and
does not sit in the token supply chain.

- **All tokens flow through the OMP gateway bundled into the Server** (the
  durable target design — the bundling is an upstream prerequisite still to be
  designed, tracked in the observability record). The gateway is the single
  egress for model calls: the user's keys/subscriptions go in, every model call
  goes through it, and it is the one place per-request token usage and spend are
  recorded. There is no second token path.
- **The gateway is the token metering point, not an external billing service.**
  Any earlier or interim routing (a stopgap gateway used during buildout) is
  transitional and is being removed; the bundled OMP gateway is the durable
  design. New code and designs assume the OMP gateway, never the stopgap.
- **Fully-managed tokens (Rigel provides the tokens too) is a possible future,
  not day-1.** It may be added later as a managed-plane offering; nothing in the
  core should assume it, and no day-1 design depends on it.

## Billing: Rigel charges for compute, not tokens

The two products bill differently because they bring different things.

- **Self-hosted core — Rigel bills nothing.** The deployer brings their own
  host, their own compute, and their own tokens. There is no metering-for-charge
  here; usage is recorded only so the deployer can *see* it in their own product
  surface (the Plane A in-product data surface). This is consistent with the
  open-source core owing the deployer no phone-home.
- **Managed service — Rigel bills for the compute it brings.** The managed
  multi-tenant service supplies the compute the Managers and agents run on, so
  that compute is the billable resource: per-tenant **usage caps and
  extra-usage/overage charges** on Manager and agent activity. Tokens stay the
  user's (BYOK / BYO cloud subscription); Rigel meters and charges for compute,
  not for the model calls themselves. This is a managed-plane concern and lives
  in the private monorepo, but the core must expose the usage it needs.

## What gets recorded (and why), in both products

Three distinct things are recorded off the OMP gateway and the runtime. Keeping
them separate matters: one is billing-grade, one is display, one is a
health/quota signal.

- **Manager/agent compute usage — billing-grade on managed.** The metered
  activity the managed service caps and charges overage on. Because it backs
  billing, it must be exact, auditable, and reconstructable — an append-only
  event record, not a lossy counter (the durable-event-log rationale is the obs
  record's Decision D5).
- **LLM token usage and spend — recorded for display, not billed day-1.** Every
  model call's tokens-in/out and cost, captured at the gateway. It powers the
  in-product usage/spend charts the user sees, and it is *recorded* even though
  Rigel does not bill it, so the user can watch their own token spend against
  their own keys/subscription. If fully-managed tokens ever ship, this same
  record becomes billable.
- **Connected cloud-subscription usage and resets — a monitored quota signal.**
  When a user connects a cloud provider subscription, Compass monitors that
  subscription's usage and reset windows (mirroring Rigel's existing OMP
  gateway plus Grafana quota-monitoring setup), so the user knows how much of
  their own subscription is left before a reset. A health/quota signal, not a
  charge.

## Where this model is designed

This doc is the *model*. The store, contracts, and planes that implement it are
the observability and in-product-data design record,
[`compass-observability-architecture`](../designs/platform/compass-observability-architecture/design.md):
the gateway-recorded usage lands in the core's own store behind a store-swap
seam (Decision D1), the append-only event log is the durable billing-grade
contract with derived rollups (Decision D5), and the managed plane builds the
billing exporter on the committed event shape in the private monorepo. Product
analytics on the UI side (how Rigel instruments the managed UI, and how a
self-hosted deploy keeps its data local) is tracked there too.
