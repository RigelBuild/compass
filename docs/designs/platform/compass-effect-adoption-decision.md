# Decision: adopt Effect on the compass agent-runner

Status: Decided (Matt, 2026-08-20)
Linear: RIG-2501 (decision lineage: RIG-2384)

This is the compass-owned statement of a single decision: **the compass
agent-runner adopts [Effect](https://effect.website) (the effect-system library
for TypeScript).** It records the *why* — the rationale a future reader needs to
understand why this one TypeScript package took on Effect. The *how* (the
primitive-by-primitive migration of the transport layer) is a separate record,
[`compass-agent-effect-adoption/design.md`](compass-agent-effect-adoption/design.md).

Scope is deliberately narrow: the **compass agent-runner package only**. This
decision does not adopt Effect anywhere else in Compass — not the Go daemon, not
any other package — and it is not a fleet-wide recommendation.

## What Effect is

Effect adds an effect system on top of TypeScript: a single
`Effect<Success, Error, Requirements>` type that tracks success values, typed
errors, and dependencies in one signature, plus fibers (structured
concurrency), `Layer`-based dependency injection, `Schedule` (declarative
retry/backoff), a `Schema` module (runtime validation from types), and built-in
tracing. Its real pitch is not five separate features but that one type
*composes* all of them: typed errors, cancellation, DI, and tracing propagate
through the same value. That composition only pays where those concerns
genuinely co-occur in one codebase.

## The decision

**Adopt Effect on the compass agent-runner now.** Three facts, together, make
this the one place in Compass where Effect earns its weight — and make *now* the
right time.

### 1. TypeScript is not a choice here — so "why not Go" has no answer

Everywhere else in Compass, the honest question that defeats Effect is "why
would we reach for Effect over Go?" Go already has natively what Effect bolts
onto TypeScript: goroutines + channels + `context.Context` for structured
concurrency and cancellation, multi-return `(T, error)` for errors-in-the-
signature, interfaces + constructor injection for DI, a few lines of `time` /
`context` for retry/backoff. For a service we would otherwise write in Go,
adopting Effect means paying a large tax — an all-in paradigm shift, a steep
learning curve, ecosystem lock-in — to *simulate in TypeScript* what Go gives
for free.

The agent-runner is the one first-party surface where that question has no
answer, because **Go is not available**: the agent-runner rides on OMP as its
base agent runtime, and OMP is TypeScript, so the agent-runner must stay
TypeScript. The honest comparison here is therefore not "Effect vs. Go" but
"Effect vs. hand-rolling the same machinery in plain TypeScript."

### 2. The transport already hand-rolls the exact machinery Effect ships

The agent-runner's transport layer already hand-rolls the Effect-shaped
subsystem:

- a **bounded priority queue with drop-oldest overload** for the trace lane
  (`transport/publish-spine.ts`),
- **bounded-backoff retry** for priority frames (`transport/publish-spine.ts`),
  and
- a **durable-unary retry with idempotency keys plus a drain budget**
  (`transport/frame-sink.ts`).

That is Queue + Schedule + supervision — the precise primitives Effect ships. So
adopting Effect here does not bolt a paradigm onto code that has no use for it;
it *retires* hand-rolled machinery in favor of the library primitives that do
the same job.

### 3. It is pre-launch and greenfield — so adopting now is strictly cheaper

Compass does not run anywhere yet, not even for dogfood. That removes the
retrofit risk entirely: there is no production behavior to preserve. The
transport's behavior *is* pinned by an exhaustive black-box test suite, which
stays the migration's merge gate — but there is no live traffic to protect.

The agent-runner is pre-launch and *will* grow as the product is built out.
Adopting Effect now, while the transport is greenfield and not load-bearing, is
strictly cheaper than retrofitting it later, once it is running and has accreted
more hand-rolled subsystems. Doing it now means the transport's
Queue/Schedule/supervision needs are met by Effect's primitives from the start,
rather than growing a second and third bespoke version to be replaced under load
later.

## Why not the alternatives

- **Adopt Effect fleet-wide.** No. Most of the TypeScript surface is one-shot
  gate/codemod CLIs and a couple of small networked services — none has an
  Effect-shaped problem. The value proposition does not map onto a one-shot-CLI
  surface, and there is essentially nothing first-party for `Effect.Schema` to
  unify. Fleet-wide adoption would impose a paradigm shift and a heavy
  dependency on every tiny tool for no observable gain.
- **Adopt Effect on the backend service.** No — the backend service with genuine
  service complexity is the compass daemon, and it is already Go, where the
  concurrency/error/DI story is native. Rewriting Go → TS/Effect trades a
  strength for a simulation of it.
- **Lighter point tools instead, even on the agent-runner.** For a *partial*
  need this would win (`neverthrow` for typed errors, `AbortController` for
  cancellation, an incumbent validator for schema). But the agent-runner is the
  case where the concerns co-occur *and* the machinery already exists
  hand-rolled — which is exactly where Effect's composition pays and a pile of
  point tools would re-grow the same bespoke glue Effect retires.

## Scope and boundaries

- **Package:** `packages/compass-agent` (the agent-runner) only. First cut is
  its `src/transport/` layer.
- **Not reopened:** the fleet-wide "watch, do not adopt" posture for the rest of
  the TypeScript surface, and the compass Go daemon staying Go, are both
  unaffected by this decision.
- **The how:** the migration design — the runtime boundary, the per-invariant
  primitive mapping, and the migration order — lives in
  [`compass-agent-effect-adoption/design.md`](compass-agent-effect-adoption/design.md).
