# Design: Adopt Effect on the compass agent-runner transport

**AMENDMENT (2026-08-21, platform — RIG-2424 T1 smoke test):** OQ-3's assertion that the sync `emit()` trace-lane path uses `Queue.unsafeOffer` on a `Queue.sliding` and is drop-oldest / "always returns true (never signals eviction)" / needs **no runtime** is **factually wrong** for `effect` 3.22.1, verified at source (`effect/internal/queue.js`: `unsafeOffer` L114-137 bypasses the strategy and offers to the backing bounded queue -> on a full sliding queue it **rejects** the newest element and returns `false` = drop-**newest**; the effectful `offer` L138-163 is the only path reaching `SlidingStrategy.handleSurplus` L449-455, which is `core.sync` and drops-**oldest**) and empirically (`unsafeOffer` [1,2,3,4,5]->sliding(3) survivors [1,2,3] returns [T,T,T,F,F]; `runSync(Queue.offer)` survivors [3,4,5]).

**Forced correction (unique mechanism):** the trace-lane sync `emit()` path is `runtime.runSync(Queue.offer(q, frame))` on the `Queue.sliding` — sliding `offer` completes synchronously (never suspends, so `runSync` cannot throw on a live queue), drops-**oldest**, and the eviction stays synchronously countable by reading `q.unsafeSize()` (returns `Option<number>`) **before** the offer (`size == capacity` => imminent eviction). Spelling: `q.unsafeSize()`, not `Queue.sizeUnsafe`.

**Contract preserved — this revises one internal mechanism, not the frozen shape:** `emit()` stays sync/void; no `Effect<>` in any exported signature (`runSync` is transport-internal); the trace lane -> `Queue.sliding` mapping is unchanged; drop-oldest and the sync drop-count are preserved (`frame-sink.test.ts:435` drop-oldest ordinals stay green). OQ-3's "sync path needs no runtime" becomes "sync path calls `runtime.runSync`." T5's single-transport-owned-`ManagedRuntime` containment is **unaffected**. Scope: contained to T3 (trace lane); T1, T2 untouched.

Status: Draft
Linear: RIG-2384. The adopt/don't-adopt call is **frozen** — ruled by Matt
(2026-08-20), frozen in the internal monorepo's effect-ts evaluation record.
This record
designs only the *how*: the migration of the compass agent-runner's transport
layer onto Effect.

## Problem / Intent

The frozen ruling adopts Effect **now, on `packages/compass-agent` only**,
naming the transport layer as the concrete target because it already
hand-rolls the exact primitives Effect ships — "a bounded priority queue with
drop-oldest overload (`transport/publish-spine.ts:43`), bounded-backoff retry
for priority frames (`publish-spine.ts:50-55`), and a durable-unary retry with
idempotency keys + a drain budget (`transport/frame-sink.ts`) — Queue +
Schedule + supervision" (the frozen evaluation record). Compass is
pre-launch ("does not run anywhere yet, not even for dogfood", per the same
record), so the migration carries no production
retrofit risk — but the transport's behavior IS already contractually pinned
by exhaustive tests (`frame-sink.test.ts`, `control-source.test.ts`,
`index.test.ts` — 30+ invariant tests, e.g. `frame-sink.test.ts:294`
bounded-drop-oldest, `:322` STOPPED-ahead-of-backlog, `:355`
failed-batch-priority-never-drop; `control-source.test.ts:828-1514` the
flap-detector/no-progress-budget family). This record designs a migration that
replaces the hand-rolled machinery with Effect primitives while keeping every
one of those tests green and `CompassAgent`'s public shape unchanged.

## Approach

**Incremental, primitive-by-primitive migration behind the existing module
interfaces, scoped to `src/transport/` as the first cut, with the Effect
`Runtime` confined entirely inside the transport modules.** No exported
signature changes: `PublishSpine` (`publish-spine.ts:57-78`), `FrameSink`
(`frame.ts:59-69`), `ControlSource`, and `RunnerTransport` (`index.ts:56-69`)
keep their promise/sync/AsyncIterable shapes, and the exhaustive black-box
tests stay green as the merge gate for every task.

### The runtime boundary (fork 3)

`createUnixSocketTransport` (`index.ts:86-114`) becomes the composition point
that owns one `ManagedRuntime` per transport instance. Everything Effect lives
behind it:

- **Sync `emit()` path.** `emit()` must stay synchronous/void
  (`frame.ts:59-60`; `frame-sink.ts:24-27`). Both enqueue sub-paths are
  synchronous. The trace path enqueues onto a bounded *sliding* `Queue`, which
  never suspends (sliding drops rather than blocks), via `Queue.unsafeOffer` —
  the escape hatch Effect ships for sync producers. The priority path is a
  synchronous array push onto the ruled-hybrid plain array (`enqueuePriority`),
  equally sync and `void`. Neither suspends, so both satisfy the frozen
  sync/void contract without a runtime, and no `Effect<...>` type appears in any
  exported signature.
- **Promise-returning seams.** `drain()`, `emitDurable()` are
  `runtime.runPromise(effect)` at the boundary — same external contract as
  today (`frame.ts:69`, `publish-spine.ts:77`).
- **The OMP SDK never sees Effect.** `CompassAgent` and the composition root
  (`cli.ts:853-884`, the load-bearing `storage.drain → sink.drain →
  transport.close` chain) are untouched; `transport.close()` additionally
  disposes the runtime after aborting the HTTP/2 session manager
  (`index.ts:112`).
- **The Connect client stays promise-land.** Each cycled batch send wraps
  `client.publish(stream)` in `Effect.tryPromise`; the durable unary wraps
  `client.postConversationFrame` likewise. Effect orchestrates *around* the
  I/O; the I/O layer is unchanged.

### Module mapping (fork 4)

**Lane representation (ruled — Matt, 2026-08-21).** Effect is adopted where it
*retires* hand-rolled machinery, and only there. The loss-tolerable trace lane
maps 1:1 onto `Queue.sliding` (drop-oldest is exactly its overflow strategy),
so it migrates. The never-drop priority lane does not: Effect 3.22.1 ships no
primitive for its needs — `TPriorityQueue` is STM-only and orders *elements*
not lanes, and there is no offer-to-front and no non-lossy two-lane select
(verified against effect source). Forcing `Queue.unbounded` there would add an
external drop-never guard, a front-insert shim, and a custom two-lane wake
*around* a FIFO queue — growing bespoke code rather than retiring it. So the
priority lane stays a plain array orchestrated by the Effect pump, and the two
missing primitives are tracked for upstreaming (RIG-2420) so the asymmetry can
close in a future Effect version. Everything else — retry, timeout, fiber
supervision, interruptible sleep — maps cleanly and migrates.

| Invariant (grounded) | Effect primitive / representation | Composition detail |
| --- | --- | --- |
| Bounded trace queue, cap 1024, drop-oldest + counted (`publish-spine.ts:43,196-199`) | `Queue.sliding(TRACE_QUEUE_CAP)` — sliding IS drop-oldest, retiring the manual cap-shift | `unsafeOffer` on a sliding queue always returns `true` (never signals eviction, `effect` `Queue.ts:710-729`), so the drop counter reads `Queue.sizeUnsafe(q)` (sync, `Queue.ts:1735`) immediately before each `unsafeOffer` and increments when it is `>= cap`. Race-free: the producer is synchronous, so the size-read and offer occupy one tick with no await between — the pump fiber cannot interleave a take |
| Never-drop priority lane, drained ahead of trace (`publish-spine.ts:87,114-123`) | **Plain array retained** (ruled) — orchestrated by the Effect pump, drained priority-first | No Effect primitive fits (see Lane representation above); the priority-first `takeBatch` (`publish-spine.ts:109-124`) stays composed logic over the array |
| Failed-batch re-enqueue priority at the front + bounded priority retry `[50,200,800]` with a pump-scoped consecutive budget (`publish-spine.ts:55,142-164`) | The pump loop retains the failed batch's priority slice and re-tries it; the budget is pump-run-scoped state (a plain counter reset only on a successful send), delay-by-index + `Effect.sleep` | NOT `Schedule.fromDelays` per batch: a per-batch schedule gives every queued priority batch a fresh 3-step ladder, so draining N batches against a dead socket costs O(N) × ~1.05 s and silently weakens drain-bounded-by-shutdown-deadline from O(1). The budget's scope is the pump run, not the send, so `Schedule` is the wrong primitive here |
| Demand-driven pump wake (`kick()` + running-flag + do/while re-check, `publish-spine.ts`) | A `Queue.sliding(1)<void>` wake latch: each `enqueueTrace`/`enqueuePriority` `unsafeOffer`s a unit; the pump `take`s it (blocks while idle), drains both lanes, loops while more work or open, and exits when `ended && both lanes empty` | Coalescing (`sliding(1)`) collapses a burst of enqueues to one wake; an idle agent blocked on the latch take opens no stream (the tested "never emits → no stream" property). Replaces the running-flag + `finally`-clear + do/while re-check with fiber-await-signal. The loop's terminal exit (`ended` set by `drain()` + both lanes drained) is what lets the drain-time `runPromise(join)` resolve — see the Drain row |
| Stream-cycling batches, `PUBLISH_BATCH_MAX = 256` (`publish-spine.ts:10-19,48`) | The pump is a forked `Fiber` running the batch loop; each batch is one `Effect.tryPromise` over `client.publish(oneBatch())` | No primitive for "open/end/reopen a client-stream" — the async-generator-per-batch driver (`publish-spine.ts:136-140`) is kept verbatim. The one-microtask defer before the first batch (`publish-spine.ts:174-181`, same-tick STOPPED must lead) is preserved explicitly (`Effect.yieldNow()`) |
| Durable retry `[50,200,800,2000]` + per-attempt 5 s deadline turning a hang into a retryable error (`frame-sink.ts:49,60,112-130`) | `Effect.retry(Schedule.fromDelays(...DURABLE_RETRY_BACKOFF_MS))` over the send; the per-attempt deadline stays the Connect `CallOptions.timeoutMs` | Keep `timeoutMs`, not a layered `Effect.timeout`: only `timeoutMs` cancels the RPC on the wire (a promise-wrapped call keeps running when the fiber is interrupted), and the hang→DeadlineExceeded conversion is the tested contract (OQ-4) |
| Idempotency key stable across retries, distinct across instances (`frame-sink.ts:85-97,107`) | Unchanged — minted once per logical frame *outside* the retried effect | Key minting must stay outside `Effect.retry`'s scope |
| In-flight durable set awaited by `drain()` (`frame-sink.ts:85,146-151,197-208`) | `FiberSet`: each durable send is a forked fiber joined at drain via `FiberSet.awaitEmpty` | Give-up disposition split (emit swallows / emitDurable rejects, `frame-sink.ts:133-151`) is a per-call `catchAll` vs propagate; causes unwrapped at the reject seam (see Global Constraints) |
| Drain bounded by shutdown deadline; teardown terminal (`publish-spine.ts:74-77,214-222`) | `drain()` sets the terminal (`ended`) flag, offers a final wake to the latch so the blocked pump observes `ended`, lets it flush the remaining queued frames priority-first (a same-tick STOPPED still leads) and RETURN, then awaits the pump fiber via `runPromise(join)` — the fiber returning is what resolves the join | Terminal-rejection of post-drain enqueues (`publish-spine.ts:195,204`) stays an explicit flag — `Queue.shutdown` would make late offers *throw*, not silently no-op. The pump is never interrupted: interrupt could abandon queued never-drop priority frames |
| Control-stream reconnect: backoff `[50,200,800,2000]`, min-uptime flap reset, no-progress budget (`control-source.ts:86-88,100,150`) | Pump as an interruptible `Fiber`; backoff is `Effect.sleep(delay)` raced against a single abort listener on the source-lifetime `AbortController` (kept first-class) | The ladder is an explicit attempt-indexed loop (`delay = CONTROL_RECONNECT_BACKOFF_MS[attempt]`), NOT `Schedule.fromDelays`: the flap reset zeroes the attempt index mid-ladder (`control-source.ts:100`) and a `Schedule` under `Effect.retry` advances internally with no external reset handle. The flap-reset and progress-not-rate budget stay bespoke `Ref` state; uptime stamping stays on the injected `now()` sampled in `onHeader`, never Effect `Clock` (the F2 tests inject and advance `now`). `return()` → `abort.abort()` remains the cancellation root; the wait's one abort listener is always detached when the race settles, preserving the "exactly one live listener mid-wait, zero after `return()`" observable that `control-source.test.ts` F5 pins |

### Migration order (fork 2, why incremental)

A full rewrite risks silently losing one of the ~30 pinned invariants; a
thin edge-wrapper adopts the dependency without retiring any hand-rolled
machinery (the ruling's entire point).
Incremental-behind-interfaces gets the primitives replaced module by module,
each PR gated by the untouched black-box tests. Order: dependency + smoke
(T1) → frame-sink durable lane (T2, the cleanest mapping) → publish-spine
(T3, the composition-heavy one) → control-source pump (T4, the largest) →
runtime lifecycle consolidation in `index.ts` (T5).

## Alternatives considered

- **(a) Full transport rewrite onto Effect in one change.** Rejected: the
  test files encode ~30 subtle invariants (e.g. the microtask defer that
  makes a same-tick STOPPED lead the batch, `publish-spine.ts:174-181`; the
  in-flight-is-progress arm of the reconnect budget,
  `control-source.ts:108-117`) and a single big-bang diff maximizes the odds
  one is lost and the reviewer misses it. Incremental keeps each diff small
  enough that a red test localizes to one module.
- **(b) Thin Effect wrapper at the edges only.** Rejected: it adds the
  dependency without replacing any hand-rolled Queue/Schedule/supervision —
  the opposite of the frozen rationale ("the transport layer's
  Queue/Schedule/supervision needs are met by Effect's primitives from the
  start, rather than growing a second and third bespoke version").
  This is distinct from the ruled hybrid
  (OQ-7): the hybrid retires every mapping that has a fitting primitive
  (trace `Queue.sliding`, retry, timeout, fiber supervision, interruptible
  sleep) and keeps hand-rolled state *only* where Effect ships no primitive
  (the priority lane); the edge-wrapper retires nothing.
- **(c) Whole-package blast radius now (CompassAgent, comms, lifecycle,
  cli).** Rejected for the first cut: the ruling names the transport as the
  concrete target; the agent loop and cli
  composition root are OMP-SDK-facing promise/callback surfaces where Effect
  buys least and churns most. The package-wide scope stays available — new
  agent-runner subsystems SHOULD be Effect-first — but retrofitting the
  non-transport modules is not designed here (Global Constraints).
- **(d) Effect 4.0 RC.** Rejected: 4.0 is a release candidate (rc npm tag),
  not stable; pin stable 3.x (see Global Constraints) and treat a later 3→4
  major as its own change.

## Global Constraints

- **Scope: `packages/compass-agent` only.** Not the Go daemon, not the internal
  monorepo's tools, not any other compass package
  (the frozen ruling's scope).
  Within the package, first cut is `src/transport/` (OQ-1).
- **Every transport invariant survives, no behavior regression.** The pinned
  set: bounded trace queue drop-oldest + counted (`publish-spine.ts:43,196-199`);
  never-drop priority lane drained ahead of trace (`publish-spine.ts:24-35`);
  stream-cycling bounded batches (`publish-spine.ts:10-19,48`); failed-batch
  front-re-enqueue + bounded priority retry with the SEPARATE failure counter
  (`publish-spine.ts:50-55,142-164`); durable retry + stable idempotency key +
  per-attempt deadline (`frame-sink.ts:49,60,85-131`); drain bounded by the
  shutdown deadline (`frame-sink.ts:197-208`, `cli.ts:853-884`); control-stream
  bounded reconnect with min-uptime flap reset + no-progress budget
  (`control-source.ts:86-150`); apply-then-ack + seq dedup (`control-source.ts`
  header contract). The existing `.test.ts` files are the executable form of
  this constraint and MUST stay green unmodified through T2–T4 (test additions
  allowed; assertion changes are not — OQ-5).
- **`emit()` stays synchronous/void; `CompassAgent`'s public shape is
  unchanged** (`frame.ts:59-60`). No `effect` type — neither an `Effect<...>`
  computation nor a `ManagedRuntime`/`Runtime` — appears in any signature
  exported from `src/transport/` (the package-level exports
  `createSocketFrameSink`, `createSocketControlSource`,
  `createUnixSocketTransport`, `RunnerTransport`, `index.ts:22-27`). The
  runtime is threaded from the transport into the sink/source through a
  module-private channel (a non-exported accessor on the internal transport
  object), never an exported factory parameter — so the public `.d.ts` stays
  free of the `effect` package. The Runtime begins and ends inside the
  transport modules.
- **Error identity is preserved at the promise boundary.** Every `runPromise`
  seam that can reject unwraps the Effect cause so callers receive the original
  error object (the raw `ConnectError` / stream error), not an Effect
  `FiberFailure` wrapper; and the transport runtime silences the default
  fiber-failure logger so a failure already routed through the `FiberSet`/
  `Deferred` bridge does not also print to the console. No current assertion
  pins error *type* (`control-source`/`frame-sink` decline to), but the tee
  backend and the agent control-loop consume these errors, so the shape is a
  contract even where a test does not pin it.
- **Bun runtime; TypeScript strict; biome.** Effect is pure TypeScript
  (no native deps) and supports Bun; T1 includes an explicit Bun smoke test
  (fibers, timers, `runPromise`) as the verification, not an assumption. The
  biome `noRestrictedImports` fence (`biome.json:11-27`) fences connect
  imports, not `effect` — no lint-config change needed.
- **Dependency hygiene.** `effect` enters via the root workspace catalog
  (`package.json:12-22` — it is not there today; `packages/compass-agent/
  package.json:13-21` has no effect dep) and must clear the 5-day
  `minimumReleaseAge = 432000` cooldown (`bunfig.toml:5-6`). Pin latest
  stable 3.x ≥ 5 days old at install time (3.22.1, published 2026-07-30, is
  21 days old on 2026-08-20 and clears it; it is the latest stable — 4.0 is
  RC-only); NOT the 4.0 RC. No `minimumReleaseAgeExcludes` entry is added for
  `effect` (the existing `@tanstack/virtual-core` exclusion, `bunfig.toml:13`,
  is unrelated) — the cooldown applies to `effect` as designed.
- **Constants keep their names and values.** `TRACE_QUEUE_CAP`,
  `PUBLISH_BATCH_MAX`, `PRIORITY_BATCH_RETRY_MS`, `DURABLE_RETRY_BACKOFF_MS`,
  `DURABLE_CALL_TIMEOUT_MS`, `CONTROL_RECONNECT_*` remain exported/local as
  today — tests import several (`frame-sink.test.ts:45`).
- Commit identity per repo convention (seal + Matt co-author trailer);
  squash-merge; code comments cite this record by path, no issue-id metadata.

## Plan

Strict dependency order T1 → T2 → T3 → T4 → T5. Each task is one PR, gated by
the full existing transport test suite green (`bun test
packages/compass-agent`) plus its own additions.

### T1 — Add the `effect` dependency + Bun runtime smoke

Add `effect` (latest stable 3.x clearing the 5-day cooldown; 3.22.1 as of
drafting) to the root workspace catalog (`package.json:12-22`) and to
`packages/compass-agent/package.json` as `"effect": "catalog:"`. Add a
`src/transport/effect-smoke.test.ts` that exercises, under Bun, the exact
primitives the later tasks rely on: `ManagedRuntime.make` + `runPromise`,
`Queue.sliding` overflow semantics (offer past cap drops OLDEST — assert
which elements survive), `Queue.unsafeOffer` from a sync frame,
`Schedule.fromDelays` retry timing, `Effect.timeout` interrupting a
never-resolving promise, `FiberSet` join-on-drain, and a fork-a-consumer-fiber
check (fork a pump fiber, synchronously offer N frames plus one marker in one
tick, assert the fiber observes nothing until the burst's tick completes — the
same-tick ordering T3's STOPPED-leads tests stand on, in miniature). This test
doubles as
the pinned record of which Effect semantics the design assumes — if an Effect
upgrade ever changes one, this reddens first.

Interfaces:

- Consumes: `effect` public API (`Effect`, `Queue`, `Schedule`, `Fiber`,
  `FiberSet`, `Deferred`, `ManagedRuntime`).
- Produces: no source change beyond the test; catalog entry
  `"effect": "^3.22.1"` (exact minor pinned at implementation time to the
  newest release ≥ 5 days old).

### T2 — Migrate the frame-sink durable lane onto Effect

Rewrite the internals of `sendDurable` / `launchDurable` / the in-flight set
(`frame-sink.ts:83-151,197-208`) as Effect:

- `sendDurable` becomes `Effect.tryPromise(() =>
  transport.postConversationFrame(request, { timeoutMs:
  DURABLE_CALL_TIMEOUT_MS }))` piped through
  `Effect.retry(Schedule.fromDelays(...DURABLE_RETRY_BACKOFF_MS))`. The
  per-attempt Connect `timeoutMs` is KEPT (it cancels the underlying RPC,
  which a bare `Effect.timeout` around a promise cannot — promise
  interruption does not abort the wire call); `Effect.timeout` is NOT layered
  on top (OQ-4 resolution proposed inline). The idempotency key is minted
  once, outside the retried effect (`frame-sink.ts:107` semantics preserved).
  The durable lane's existing backoff ladder is preserved as-is; at its
  terminal give-up boundary the Effect cause is unwrapped at the
  `runPromise`/reject seam so the caller receives the original `ConnectError`,
  not an Effect `FiberFailure` wrapper (`frame-sink.ts:133-151` give-up
  disposition and the no-`unhandledRejection` assertion both preserved). The
  transport runtime silences the default fiber-failure logger so a handled
  forked-send failure does not double-report to the console.
- The `inflight` Set of promises becomes a `FiberSet` owned by the sink;
  `emit()` forks the send with the swallow disposition, `emitDurable()` forks
  with propagate (`Deferred` bridged to the returned promise), `drain()`
  becomes `runPromise(FiberSet.awaitEmpty(...))` followed by `spine.drain()`
  — same snapshot-and-await semantics as `frame-sink.ts:202-204`. The
  `Deferred` reject bridge is wired at fork time, not at drain/join, so a
  forked durable failure is always observed by `emitDurable`'s returned promise
  before `FiberSet.awaitEmpty` resolves — preserving the no-`unhandledRejection`
  contract (`frame-sink.test.ts:392`).
- The sink owns a `ManagedRuntime`. Until T5 moves ownership into the
  transport, the sink makes its own and disposes it at the end of `drain()`
  (terminal for the sink — post-drain enqueues are no-ops by contract), so no
  undisposed runtime leaks across the T2–T4 transitional state (see T5).

Gate: `frame-sink.test.ts` green UNMODIFIED — all 10 tests, notably retry
key-reuse (`:231`), drain-awaits-commit (`:259`), give-up rejects emitDurable
(`:392`).

Interfaces:

- Consumes: `RunnerTransport.postConversationFrame(req, options?):
  Promise<PostConversationFrameResponse>` (`index.ts:60-63`), `PublishSpine`
  (`publish-spine.ts:57-78`), `effect` from T1.
- Produces: `createSocketFrameSink(transport: RunnerTransport): FrameSink`
  — signature UNCHANGED (`frame-sink.ts:81`); `FrameSink` shape unchanged
  (`frame.ts:59-69`: `emit(frame: OutboundFrame): void`,
  `emitDurable(frame: OutboundFrame): Promise<void>`,
  `drain(): Promise<void>`).

### T3 — Migrate the publish spine onto Effect

Rewrite `createPublishSpine` internals (`publish-spine.ts:84-224`):

- Trace lane: `Queue.sliding(TRACE_QUEUE_CAP)`; `enqueueTrace` uses
  `unsafeOffer` (sync, never suspends). The drop counter reads
  `Queue.sizeUnsafe(q)` immediately before the offer and increments when it is
  `>= cap` — `unsafeOffer` on a sliding queue always returns `true` and never
  signals the eviction, so the pre-offer size read is the observable-drop
  contract (`publish-spine.ts:58-61,68`). Sync producer, so the size read and
  offer share one tick and no take interleaves.
- Priority lane: stays a **plain array** (ruled — see Lane representation),
  drained priority-first by the pump. Failure retries the retained failed-batch
  slice the pump loop already holds (`publish-spine.ts:160`), re-tried at the
  front, no `unshift` shim.
- The pump: one forked fiber running the batch loop, woken by a
  `Queue.sliding(1)<void>` latch (`enqueueTrace`/`enqueuePriority` offer a unit;
  the pump blocks on `take` while idle, so an agent that never emits opens no
  stream; `sliding(1)` coalesces an enqueue burst to one wake). Each iteration:
  takeBatch (priority-first, cap `PUBLISH_BATCH_MAX`), `Effect.tryPromise` over
  `publish(oneBatch())` (stream-cycling generator kept verbatim,
  `publish-spine.ts:136-140`), failure split — trace counted; priority re-tried
  on the bounded ladder `PRIORITY_BATCH_RETRY_MS` `[50,200,800]` with a
  **pump-run-scoped consecutive budget** (a plain counter, reset only on a
  successful send, delay-by-index + `Effect.sleep`), then counted into the
  SEPARATE `failedPriorityCount`. NOT `Schedule.fromDelays` per batch: the
  budget bounds `drain()` on a dead socket at O(1) across all queued priority
  batches (`publish-spine.ts:142-164`), and a per-batch schedule would make it
  O(N). The one-microtask defer before the first batch
  (`publish-spine.ts:174-181` — same-tick STOPPED must lead) is preserved
  explicitly (`Effect.yieldNow()`).
- `drain()`: set the terminal (`ended`) flag (late enqueues silently no-op,
  NOT `Queue.shutdown` — that throws on late offers, `publish-spine.ts:195,204`
  contract), offer a final wake to the latch so the blocked pump observes
  `ended`, let it flush the remaining queued frames priority-first (a same-tick
  STOPPED still leads) and RETURN, then await the pump fiber via
  `runPromise(join)`. The pump loop's exit condition is `ended && both lanes
  empty`, and the fiber returning is what resolves the join — it is never
  interrupted, since interrupt could abandon queued never-drop priority frames.

Gate: every spine-covering test green unmodified — bounded overflow
(`frame-sink.test.ts:294`), drop-OLDEST ordinals (`:435`), STOPPED-leads
same-tick (`:322`) and cross-batch (`:474`), failed-batch split (`:355`),
deliveryAck priority sub-lane (`:590`). The spine has no standalone test file;
its invariants are pinned indirectly through `frame-sink.test.ts` (the sink
composes the real spine, `cli.test.ts:387-389`), so the implementer should not
hunt for a `publish-spine.test.ts` that does not exist.

Interfaces:

- Consumes: `publish: (stream: AsyncIterable<PublishFrameRequest>) =>
  Promise<unknown>` (`publish-spine.ts:84-85`), `effect`.
- Produces: `createPublishSpine(publish): PublishSpine` — signature and
  `PublishSpine` interface UNCHANGED (`publish-spine.ts:57-78`:
  `enqueueTrace(frame): void`, `enqueuePriority(frame): void`,
  `droppedTraceCount(): number`, `failedPriorityCount(): number`,
  `drain(): Promise<void>`); exported constants unchanged.

### T4 — Migrate the control-source pump onto Effect

Rewrite the reconnect pump inside `createSocketControlSource`
(`control-source.ts:256-582`): the background pump becomes an interruptible
fiber, and iterator `return()` interrupts it (cancelling the in-flight Control
RPC via the `{ signal }` it threads). The source-lifetime `AbortController`
stays first-class — it is the cancellation root (`return()` → `abort.abort()`)
and the signal threaded into the Connect call. The backoff wait is
`Effect.sleep(delay)` raced against a single listener registered on that one
abort signal, always detached when the race settles either way; this keeps the
"exactly one live abort listener mid-wait, zero after `return()`" observable
that `control-source.test.ts` F5 (`:1514-1575`) pins, which a bare
`Effect.sleep` under fiber interruption (registering nothing on the signal)
would redden. The ladder is an explicit attempt-indexed loop
(`delay = CONTROL_RECONNECT_BACKOFF_MS[attempt]`), NOT `Schedule.fromDelays`:
the min-uptime flap reset (`control-source.ts:100`) zeroes the attempt index
mid-ladder, and a `Schedule` consumed by `Effect.retry` advances internally
with no external reset handle. The flap reset and the progress-not-rate
no-progress budget (`control-source.ts:150`) stay bespoke `Ref` state, and
uptime stamping stays on the injected `now()` sampled in `onHeader` (never
Effect `Clock` — the F2 tests inject and advance `now`); the comments
documenting WHY (`control-source.ts:102-149`) move with the code.
The module-local `ManagedRuntime` (until T5) is disposed in the iterator's
`return()`, alongside `abort.abort()`/`buffer.close()`: `ControlSource` is an
`AsyncIterable` with no `drain()`, so `return()` is its only teardown seam, and
a T4 `return()` test asserts no runtime is left live rather than deferring all
fiber-leak coverage to T5.
`AsyncBuffer` (uncapped, `control/buffer.ts:18-21`) and `AckCursor` are NOT
migrated in this task — they are self-contained data structures with their
own tests; converting them buys no primitive replacement (OQ-1 boundary).

Gate: `control-source.test.ts` green unmodified — all ~20 tests including the
F2 flap family (`:828,895,943,1010`), in-flight-is-progress (`:1087`), and
the F3/F4/F5 abandonment/interruption family (`:1198,1459,1514`). F5 pins the
abort-listener count of the backoff wait, which is why the wait keeps the
AbortController first-class (see T4 prose) rather than replacing it with bare
fiber interruption.

Interfaces:

- Consumes: `RunnerTransport.control(req, options?):
  AsyncIterable<AgentControl>` (`index.ts:64-67`), `ImmediateControl`
  (`control-source.ts:160-163`), `AsyncBuffer`/`AckCursor` (unchanged),
  `effect`.
- Produces: `createSocketControlSource(transport, immediate, options?):
  ControlSource` — signature UNCHANGED (`control-source.ts:256-260`);
  exported reconnect constants unchanged.

### T5 — Consolidate runtime ownership in the transport

Move `ManagedRuntime` ownership to `createUnixSocketTransport`
(`index.ts:86-114`): the transport constructs one runtime and threads it into
the memoized spine and into sink/source construction through a module-private
channel (a non-exported accessor on the internal transport object, NOT an
exported factory parameter — the public factory signatures stay effect-free
per Global Constraints). `close()` becomes `sessionManager.abort()` followed
by `runtime.dispose()` — ordered after the drain barrier exactly as today's
close is (`cli.ts:874-879`; `index.ts:50-54` documents why close follows
drain). Until T5, each migrated module makes a module-local runtime disposed at
its own teardown seam: T2 (sink) and T3 (spine) dispose it at the end of
`drain()`, while T4 (control-source) disposes it in the iterator's `return()`
(alongside `abort.abort()`/`buffer.close()`) because `ControlSource` is an
`AsyncIterable` with no `drain()`. T5 retires all of those in favor of the
single transport-owned runtime. The single-scheduler property therefore holds
on the
production wiring path (`createUnixSocketTransport`); test paths that construct
a bare sink/source over a fake transport still use the per-factory default
runtime, which is expected and disposed at drain. `cli.ts` is untouched except
that `transport.close()` now also disposes the runtime — same call site, same
ordering.

Gate: `index.test.ts` green unmodified; full package suite green; a new test
asserting `close()` after `drain()` leaves no live fibers (no hanging
process — the exact leak class `close()` exists for, `index.ts:50-54`).

Interfaces:

- Consumes: T2–T4 factories; `ManagedRuntime` from `effect`.
- Produces: `createUnixSocketTransport(socketPath: string): RunnerTransport`
  — signature UNCHANGED (`index.ts:86`); `RunnerTransport` interface
  UNCHANGED (`index.ts:56-69`), `close(): void` now also disposing the
  runtime (dispose is fire-and-forget from close's sync signature; the
  drain barrier has already quiesced all fibers).

## Tasks

- [ ] T1 — `effect` dependency via workspace catalog (stable 3.x, clears the
      5-day cooldown) + Bun runtime smoke test pinning the assumed semantics.
- [ ] T2 — Frame-sink durable lane onto Effect (`Effect.retry` +
      `Schedule.fromDelays`, `FiberSet` in-flight tracking); `FrameSink`
      shape unchanged; `frame-sink.test.ts` green unmodified.
- [ ] T3 — Publish spine onto Effect (`Queue.sliding` trace lane with a
      pre-offer `sizeUnsafe` drop counter; priority lane kept a plain array
      under the pump per the ruling; `sliding(1)` wake latch; fiber pump,
      stream-cycling driver kept; pump-scoped consecutive priority-retry
      budget); `PublishSpine` shape + constants unchanged; all spine tests
      green unmodified.
- [ ] T4 — Control-source reconnect pump onto Effect (interruptible fiber;
      backoff = `Effect.sleep` raced against one source-lifetime abort
      listener, AbortController kept first-class; attempt-indexed ladder;
      flap-reset + no-progress budget kept as composed `Ref` policy);
      `control-source.test.ts` green unmodified.
- [ ] T5 — Single `ManagedRuntime` owned by `createUnixSocketTransport`;
      `close()` disposes it after the drain barrier; no-live-fibers test.

## Open Questions

Each fork below carries a recommendation; the record's Approach and Plan are
written against the recommendations. **Load-bearing** questions need Matt's
ruling before implementation starts (they change the shape of the work);
**deferrable** ones can be resolved in-PR by the implementer + reviewer.

- **OQ-1 (load-bearing) — Blast radius within the package.** The frozen
  ruling scopes adoption to the agent-runner package and names the transport
  as the concrete target.
  **Recommendation: first cut = `src/transport/` only** (this Plan), with
  `control/buffer.ts` + `control/ack-cursor.ts` explicitly excluded (self-
  contained data structures, no primitive replacement to gain), and a
  standing convention that NEW agent-runner subsystems are Effect-first.
  Retrofitting CompassAgent/comms/lifecycle/cli is deliberately not designed
  here — it would churn OMP-SDK-facing promise surfaces for no primitive
  gain. Load-bearing because a "whole package now" ruling would restructure
  the Plan entirely.
- **OQ-2 (load-bearing) — Migration strategy.** Full rewrite vs incremental
  vs edge wrapper. **Recommendation: incremental, primitive-by-primitive
  behind the frozen module interfaces** (Approach; Alternatives a/b) — one
  module per PR, existing tests green unmodified as each PR's merge gate.
  Load-bearing because it fixes the PR structure and the review contract.
- **OQ-3 (load-bearing) — The OMP-SDK / Effect Runtime boundary.**
  **Recommendation: the Runtime lives entirely inside `src/transport/`,
  owned by `createUnixSocketTransport` (T5); sync `emit()` is served by
  `Queue.unsafeOffer`; promise seams (`drain`, `emitDurable`,
  `RunnerTransport` methods) are `runPromise` at the module boundary; no
  `Effect<...>` in any exported type.** Load-bearing because it is the
  containment guarantee — if Effect types were allowed to leak into
  `CompassAgent`, the frozen "public shape unchanged" constraint breaks and
  the adoption becomes package-wide de facto.
- **OQ-4 (deferrable) — Per-attempt deadline mechanism.** Keep Connect's
  `CallOptions.timeoutMs` (cancels the RPC on the wire,
  `frame-sink.ts:114-116`) vs replace with `Effect.timeout` (interrupts the
  fiber but a promise-wrapped RPC keeps running wire-side).
  **Recommendation: keep `timeoutMs`** — it is the only variant that
  actually aborts the hung call, and the hang→DeadlineExceeded conversion is
  the tested contract (`frame-sink.ts:51-60`). Deferrable: either choice
  preserves the observable retry behavior; the implementer verifies via the
  existing hang test.
- **OQ-5 (deferrable) — Test strategy: TestClock vs black-box.**
  **Recommendation: keep the existing tests black-box and UNMODIFIED as the
  merge gate for T2–T4** — they are the behavioral contract, and rewriting
  assertions onto `TestClock` risks silently weakening one (the brief's
  stated risk). NEW tests added by the migration (T1 smoke, T5 no-live-
  fibers) MAY use `TestClock`/virtual time where it removes real-timer
  flakiness. A wholesale TestClock rewrite of the invariant suites is a
  possible follow-up AFTER the migration lands, as its own reviewed change —
  never in the same PR as the code it gates. Deferrable because the
  recommendation is the no-change default; only a desire to speed up the
  slowest timer tests would reopen it.
- **OQ-6 (deferrable) — Effect version pin.** Latest stable 3.x that clears
  the 5-day `minimumReleaseAge` at install time (3.22.1, published
  2026-07-30 — 21 days old on 2026-08-20 — clears it; latest stable, 4.0
  is RC-only and excluded).
  Deferrable: the implementer pins whatever the newest qualifying 3.x is at
  T1 time; a later 3→4 major is its own change with its own record if the
  API surface moves.
- **OQ-7 (load-bearing) — RULED (Matt, 2026-08-21). Lane representation.**
  Represent the two queue lanes as Effect `Queue`s with composition shims, or
  keep them as plain arrays under Effect orchestration? **Ruling: hybrid.** The
  loss-tolerable trace lane migrates to `Queue.sliding` (drop-oldest is exactly
  its overflow strategy — a clean retirement of hand-rolled machinery); the
  never-drop priority lane stays a plain array, because Effect 3.22.1 ships no
  primitive that fits it (no offer-to-front, no non-lossy two-lane select;
  `TPriorityQueue` is STM-only and orders elements not lanes — verified against
  effect source) and forcing `Queue` there would add shims *around* a FIFO
  queue rather than retire code. The two missing primitives are tracked for
  upstreaming in **RIG-2420** so the asymmetry can close in a future Effect
  version, at which point the priority-lane shim becomes a one-line swap. This
  ruling is why the mapping table's priority-lane and pump-budget rows keep
  bespoke state (findings the design-critic raised as silent-invariant risks
  under a full-`Queue` representation); it is settled, not open.
