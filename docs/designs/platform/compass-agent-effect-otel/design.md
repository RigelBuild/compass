# Design: Adopt `@effect/opentelemetry` for compass-agent transport observability

Status: Draft
Linear: RIG-2426 (decision-to-adopt; implementation is a separate later issue).
Parent record: `docs/designs/platform/compass-agent-effect-adoption/design.md`
(RIG-2424, merged as PRs #465/#467/#471/#474; T5 runtime consolidation is
PR #475 — OPEN at drafting time; see Global Constraints for the ordering
dependency).

## Problem / Intent

The RIG-2424 migration put the agent-runner transport
(`packages/compass-agent/src/transport/`) on Effect 3.22.1, but its
observability is still hand-rolled and mostly invisible: loss counters are
plain module-local numbers readable only by polling two methods
(`publish-spine.ts:78,83` — `droppedTraceCount()` / `failedPriorityCount()`),
retry ladders and reconnect storms emit nothing, and there is no
trace/metric/log export at all (verified: zero `otel`/`OTLP` references under
`packages/compass-agent`). `@effect/opentelemetry` turns the fiber seams the
migration just created into OTel spans and the hand-rolled counters into OTel
metrics — inside the existing containment boundary, exported to the house
Grafana stack. This record decides how, preserving the parent record's frozen
containment rule: zero `effect` (and zero `@effect/opentelemetry`) types in
any exported signature.

## Approach

**Adopt `@effect/opentelemetry@0.64.0` as a transport-internal observability
layer: spans wrap the effects the RIG-2424 migration created, `Metric`
counters/gauges replace the hand-rolled loss counters, and the whole OTel SDK
is provided as a `Layer` into the single transport-owned `ManagedRuntime` —
nothing crosses the containment boundary.** `Effect.withSpan` lives in core
`effect` (already a dependency, pinned `^3.22.1`, root `package.json:18`), so
instrumentation costs no new import surface in module code; the OTel package
is touched only at the runtime composition point. With no Tracer/Meter
provider in the runtime's layer, instrumentation is inert without being a
literal no-op: `withSpan` uses the default no-op tracer, and `Metric`
increments still accumulate in Effect's internal in-memory `MetricRegistry`
but are never EXPORTED — so instrumentation is observably invisible in unit
tests and in agents with no export endpoint, and the existing black-box test
suite stays green unmodified.

**Verified API surface (source: the published `@effect/opentelemetry@0.64.0`
tarball, `npm pack`, dated 2026-07-13 — cited as
`@effect/opentelemetry@0.64.0/<path>:<line>`):**

- **Versioning.** The package versions independently on a 0.x line — it is
  NOT semver-matched to `effect`. npm dist-tags at drafting time:
  `latest: 0.64.0`, `rc: 4.0.0-rc.111` (npm registry metadata, 2026-08-22) —
  the repo-HEAD `4.0.0-rc` line is the effect-4 RC; do not reach for it. Pin
  `0.64.0`. Its `effect` peer is `"effect": "^3.22.0"`
  (`@effect/opentelemetry@0.64.0/package.json` `peerDependencies`), satisfied
  by the repo's `^3.22.1`.
- **Modules.** Twelve, per the barrel
  (`@effect/opentelemetry@0.64.0/dist/dts/index.d.ts:4-53`, twelve
  `export * as` lines — e.g. `export * as NodeSdk from "./NodeSdk.js";`):
  `Logger`, `Metrics`, `NodeSdk`, `Otlp`, `OtlpLogger`, `OtlpMetrics`,
  `OtlpResource`, `OtlpSerialization`, `OtlpTracer`, `Resource`, `Tracer`,
  `WebSdk`. (The RIG-2426 recon's "six modules: NodeSdk, WebSdk, OtelTracer,
  OtelLogger, OtelMetrics, Resource" was WRONG — `OtelTracer`/`OtelLogger`/
  `OtelMetrics` are not module names; `OtelTracer` is a Context tag inside
  the `Tracer` module, `dist/dts/Tracer.d.ts:87`: `export declare const
  OtelTracer: Tag<OtelTracer, Otel.Tracer>`. The `Otlp*` modules are a
  lightweight exporter alternative this record does not use; they are the
  only consumers of the `@effect/platform` peer.)
- **`NodeSdk.layer(config)`** takes a `Configuration`
  (`dist/dts/NodeSdk.d.ts:19-30`) with keys `spanProcessor`, `tracerConfig`,
  `metricReader`, `logRecordProcessor`, `loggerProviderConfig`, `resource`
  (`{ serviceName, serviceVersion?, attributes? }`), `shutdownTimeout` —
  quoted: `readonly spanProcessor?: SpanProcessor |
  ReadonlyArray<SpanProcessor> | undefined;` (`NodeSdk.d.ts:20`). It is
  called with a lazy thunk or an Effect: `(evaluate: LazyArg<Configuration>):
  Layer.Layer<Resource.Resource>` (`NodeSdk.d.ts:43-48`).
- **One layer wires all three signals.** `NodeSdk.layer` internally composes
  tracing, metrics, AND logging from its config keys
  (`src/NodeSdk.ts:98-124` in the tarball: `TracerLive` from
  `config.spanProcessor`, `MetricsLive = Metrics.layer(...)` from
  `config.metricReader` at `:108-110`, `LoggerLive =
  Logger.layerLoggerAdd + Logger.layerLoggerProvider(...)` from
  `config.logRecordProcessor` at `:112-120`, merged at `:122-124`:
  `Layer.mergeAll(TracerLive, MetricsLive, LoggerLive).pipe(
  Layer.provideMerge(ResourceLive))`). So this design needs exactly ONE
  layer call — no separate `Metrics.layer()`/logger-layer merge in our code.
  (The recon's "metrics install via `OtelMetrics.layer()`" was wrong twice:
  wrong module name, and unnecessary — the standalone entry point is
  `Metrics.layer(evaluate, options?)`, `dist/dts/Metrics.d.ts:26-28`, which
  `NodeSdk.layer` already calls for us.)
- **Fiber-tree→trace-tree propagation is real.** The Effect tracer the layer
  installs reads the fiber's current span when bridging into the OTel
  context: `src/internal/tracer.ts:184-195` — `context(execution, fiber) {
  const currentSpan = fiber.currentSpan; … return OtelApi.context.with(
  populateContext(OtelApi.context.active(), currentSpan), execution)` — so a
  span opened by `Effect.withSpan` in a forked fiber parents its children
  automatically.
- **Log/trace correlation** rides the same `NodeSdk.layer` call: providing
  `logRecordProcessor` installs `Logger.layerLoggerAdd`
  (`dist/dts/Logger.d.ts:28`; wired at `src/NodeSdk.ts:112-120`), which
  routes `Effect.log` into the OTel Logs SDK where entries pick up the
  active span's `trace_id`/`span_id` — matching the house
  Grafana/golang-observability correlation standard.
- **Declared peers** (`@effect/opentelemetry@0.64.0/package.json`
  `peerDependencies`): `@opentelemetry/api ^1.9`, `resources ^2.0.0`,
  `sdk-logs >=0.203.0 <0.300.0`, `sdk-metrics ^2.0.0`, `sdk-trace-base
  ^2.0.0`, `sdk-trace-node ^2.0.0`, `sdk-trace-web ^2.0.0`,
  `semantic-conventions ^1.33.0`, `@effect/platform ^0.97.0`, `effect
  ^3.22.0`. The OTLP exporter packages are NOT peers — they are the app's
  own choice (O1b).

### Decision 1 — span granularity and naming

**Recommendation: span the bounded, per-operation effects — one span per
attempt — and never span an unbounded pump loop.** A span over a
session-lifetime fiber (the publish pump, the control reconnect pump) would
stay open for hours and export nothing until teardown; the useful trace unit
is the individual send/connect attempt.

Considered and rejected: one pump-loop-lifetime span carrying span EVENTS per
attempt (the standard OTel answer for long-lived loops) — rejected because
per-attempt spans give queryable per-attempt durations and per-attempt status
(a failed send is a red span, not an event buried in an hours-long parent),
at no extra mechanism (the pump already runs each attempt as its own effect).

Span set, named `compass_agent.transport.<module>.<op>`:

| Span | Wraps (file:line) | Attributes |
| --- | --- | --- |
| `…publish.batch` | Each cycled batch send — `publish-spine.ts:201-203`: `const result = yield* Effect.either(Effect.tryPromise(() => publish(oneBatch())))` | `batch_size`, `priority_count`, `retry_index` (the pump-scoped `priorityRetries` at send time) |
| `…frame_sink.durable_send` | The whole forked durable send incl. retries — the `send` pipeline built in `launchDurable` (`frame-sink.ts:160-198`), i.e. around the `Effect.retry(Schedule.fromDelays(...))` pipe at `frame-sink.ts:179-184` | `frame_kind`; span status = give-up error on retry exhaustion |
| `…frame_sink.durable_attempt` | Each individual unary attempt — the inner `Effect.tryPromise` at `frame-sink.ts:169-178` (child of `durable_send`; retries appear as sibling attempt spans under one parent, the fiber-tree propagation doing the parenting). NOTE: `frame-sink.ts:164` is the idempotency-key mint (`const idempotencyKey = ...`), which MUST stay outside the span/retry scope — the key is minted once per logical frame, not per attempt | `attempt` (via a `Ref` ticked in the retried effect) |
| `…control.connection` | One control-stream connection attempt — the `Effect.tryPromise` consuming the server-stream at `control-source.ts:477-496` (`for await (const wire of stream) dispatch(wire)`) | `attempt` (ladder index, known at span open — a `withSpan` attribute); `established` is a span EVENT, not an attribute — see the mechanism note below |
| `…frame_sink.drain` / `…publish.drain` | The teardown flushes — `frame-sink.ts:257-260` (`FiberSet.awaitEmpty` then `spine.drain()`) and `publish-spine.ts:266-279` (final wake + `Fiber.join(pumpFiber)`) | none; duration IS the signal (drain-bounded-by-shutdown-deadline is the contract being watched) |

**`…control.connection` mechanism (why not the obvious attributes).** Two of
the connection's interesting facts are NOT annotatable as attributes from
inside the wrapped effect:

- `noProgress` is computed at `control-source.ts:522` (`noProgress =
  madeProgress ? 0 : noProgress + 1;`) — AFTER the `Effect.tryPromise` the
  span wraps (`:477-496`) has settled, i.e. after the span has closed. A
  closed span cannot take an attribute. It is NOT carried on the span at
  all: it is already the `no_progress_depth` gauge (Decision 2), and
  duplicating a gauge as a post-hoc span attribute buys nothing.
- `established` is set inside the `onHeader` callback
  (`control-source.ts:484-487`: `onHeader: () => { established = true;
  openedAt = now(); }`) — a plain JS closure invoked by the Connect stream
  outside any Effect context, so `Effect.annotateCurrentSpan` cannot reach
  it. Instead it becomes a span EVENT: inside the `withSpan` scope, before
  entering the tryPromise, the effect captures the live span handle
  (`yield* Effect.currentSpan`), and the `onHeader` closure calls
  `span.event("established", ...)` — `Span.event(name, startTime,
  attributes?)` is a plain synchronous method on the span object
  (`effect@3.22.1/dist/dts/Tracer.d.ts:102-103`: `event(name: string,
  startTime: bigint, attributes?: Record<string, unknown>): void;`),
  callable from a closure while the span is open — and header receipt
  always happens while the tryPromise (and thus the span) is still live.
  An event also carries the timestamp for free, which an attribute would
  lose (time-to-established is the interesting number).

Not spanned: the sync `emit()` enqueue paths (`publish-spine.ts:249-252` — a
span per enqueued trace frame is per-frame overhead on a hot sync path for no
question anyone asks; queue depth is a metric, below) and the pump loops
themselves.

### Decision 2 — which counters become `Metric` counters vs gauges

**Recommendation: every monotone loss/attempt count becomes a `Metric.counter`;
the two "current level" readings (queue depth, consecutive-retry depth) become
`Metric.gauge`.** The hand-rolled fields, each cited:

| Source (quoted) | Metric | Kind |
| --- | --- | --- |
| `publish-spine.ts:249` — `if (traceSize() >= TRACE_QUEUE_CAP) dropped++;` | `compass_agent.transport.publish.trace_frames_lost` `{reason="overflow"}` | counter |
| `publish-spine.ts:212` — `failedBatchFrames += batch.length - priorityCount;` | same counter, `{reason="failed_batch"}` — the two reasons sum to today's `droppedTraceCount()` (`publish-spine.ts:260-261`: `return dropped + failedBatchFrames;`) | counter |
| `publish-spine.ts:221-222` — `if (priorityRetries >= PRIORITY_BATCH_RETRY_MS.length) { failedPriorityFrames += priorityCount;` | `compass_agent.transport.publish.priority_frames_lost` — never-drop loss, a contract breach; kept a SEPARATE metric exactly as the source keeps a separate counter (declaration `publish-spine.ts:134`: `let failedPriorityFrames = 0;`, rationale comment `:131-133`) | counter |
| `publish-spine.ts:228-229` — `const delay = PRIORITY_BATCH_RETRY_MS[priorityRetries]; priorityRetries++;` | `compass_agent.transport.publish.priority_batch_retries` (attempts, monotone) AND `…publish.priority_retry_depth` gauge (the pump-scoped consecutive budget: set on `:229`, reset to 0 on success at `:205` — a level, not a count) | counter + gauge |
| trace queue depth — `publish-spine.ts:114-116` `traceQ` (`const traceQ = runtime.runSync(Queue.sliding<PublishFrameRequest>(TRACE_QUEUE_CAP))`), read via `traceSize()` (`:147-149`) | `compass_agent.transport.publish.trace_queue_depth`, sampled at each batch take — the `takeBatch` effect, `publish-spine.ts:155-168` (`const traceFrames = yield* Queue.takeUpTo(traceQ, room);` at `:164`) | gauge |
| `frame-sink.ts:179-184` — `Effect.retry(Schedule.fromDelays(DURABLE_RETRY_BACKOFF_MS[0], ...))` | `compass_agent.transport.frame_sink.durable_attempts` (every attempt) and `…durable_give_ups` (the `onSettle(err)` path, `frame-sink.ts:192-196`) | counters |
| `control-source.ts:549` — `const delay = CONTROL_RECONNECT_BACKOFF_MS[attempt++];` | `compass_agent.transport.control.reconnects` | counter |
| `control-source.ts:522` — `noProgress = madeProgress ? 0 : noProgress + 1;` | `compass_agent.transport.control.no_progress_depth` (level against `CONTROL_RECONNECT_NO_PROGRESS_MAX = 10`, `control-source.ts:151`) | gauge |
| `control-source.ts:543-544` — `if (established && now() - openedAt >= CONTROL_RECONNECT_MIN_UPTIME_MS) attempt = 0;` | `compass_agent.transport.control.flap_resets` | counter |
| `control-source.ts:297-299` — `function count(eventType: string, reason: string): void { onUnmapped(...) }` | `compass_agent.transport.control.unmapped` `{event_type}` — piggybacks the existing single funnel; the `onUnmapped` callback contract is unchanged | counter |

Considered and rejected: histograms for batch send duration and `batch_size`
distribution. **No histograms in this cut** — a deliberate decision
(adoption-record minimalism: the span set already carries per-attempt
durations queryable in Tempo, and every distribution question so far reduces
to the counters/gauges above), not an omission. A histogram cut can ride a
later record once a concrete dashboard needs one.

The existing exported methods `droppedTraceCount()` / `failedPriorityCount()`
(`publish-spine.ts:78,83`) STAY — tests consume them
(`frame-sink.test.ts:315,389`) and they are the frozen `PublishSpine` shape.
Metrics are additive, driven from the same increment sites.

### Decision 3 — exporter wiring against the deployed stack

**There is no existing OTLP/Grafana endpoint config on the agent today**
(verified: no `OTEL`/`otlp` reference under `packages/compass-agent`; the
agent's env surface is the `COMPASS_*` namespace plus a Runner-materialized
env file, `cli.ts:96-105,500-523`). Moreover the agent container is
egress-sealed by design — the transport's whole point is "a local hop, no
network path, so the egress seal is untouched" (`index.ts:4-5`). A direct
OTLP-over-network exporter from inside the container is the agent's first
network egress — a posture decision resolved as (a) (Open Questions Q1). The
substrate stays socket-based (AF_UNIX today, virtio-vsock under the microVM
end-state, never IP —
`docs/designs/platform/compass-elastic-session-runtime/microvm-runner.md`), so
OTLP is genuinely the agent's first *network* path and is drawn against the
destination collector, never the socket/vsock gateway.

**Recommendation: standard env-gated OTLP, off by default.** At transport
construction, read `OTEL_EXPORTER_OTLP_ENDPOINT`: unset → the runtime gets no
OTel layer at all (`Layer.empty` beyond the existing logger removal — zero
overhead, tests unaffected); set → `NodeSdk.layer` with a
`BatchSpanProcessor(OTLPTraceExporter)` + `PeriodicExportingMetricReader
(OTLPMetricExporter)` over HTTP/protobuf. The deployer decides per-container
whether to set the endpoint in the Runner-materialized env file; the TS CLI
sources it into `process.env` (`cli.ts:519-523`) and the layer reads it — the
end-to-end injection path is owned by O5 (Open Questions Q5). The root
`bun.lock` already
carries the `@opentelemetry/*` SDK line (`bun.lock:559-585` —
`@opentelemetry/api@1.9.1`, `sdk-metrics@2.9.0`,
`exporter-trace-otlp-proto@0.220.0` etc. as transitives), so the exporter
packages introduce no unvetted publisher — and those resolved versions
satisfy 0.64.0's declared peer ranges (api `^1.9`, sdk-metrics `^2.0.0`,
sdk-trace-base `^2.0.0`).

### Decision 4 — providing the layer into the single `ManagedRuntime`

**Recommendation: merge the OTel layer at the runtime construction point
inside `createUnixSocketTransport`, leaving its exported signature untouched
(`index.ts:86`: `export function createUnixSocketTransport(socketPath:
string): RunnerTransport`).** RIG-2424 T5 (PR #475, open) consolidates the
three per-module transitional runtimes into one
`ManagedRuntime.make(Logger.remove(Logger.defaultLogger))` owned by the
transport and threaded via a module-private `runtime-channel.ts` WeakMap. This
design composes on top of that exact seam: the one construction site becomes

```ts
ManagedRuntime.make(
  Layer.merge(Logger.remove(Logger.defaultLogger), makeOtelLayer()),
)
```

where `makeOtelLayer(): Layer.Layer<never>` is a new transport-internal,
non-re-exported module (`otel-layer.ts`) returning `Layer.empty` when no
endpoint is configured and a single `NodeSdk.layer(() => config)` call when
one is. The declared `Layer.Layer<never>` return is deliberate, not the
natural type: `NodeSdk.layer` is typed `Layer.Layer<Resource.Resource>`
(`@effect/opentelemetry@0.64.0/dist/dts/NodeSdk.d.ts:43-48`), and `Layer`'s
`ROut` is contravariant (`effect` `Layer.d.ts:54`: `export interface Layer<in
ROut, ...>`), so `Layer<Resource.Resource>` is assignable to `Layer<never>` —
the `Resource.Resource` output is discarded to keep
`ManagedRuntime.make(Layer.merge(...))` a `ManagedRuntime<never>`. The one
call wires tracing, metrics, AND log correlation, because `NodeSdk.layer`
composes all three from its `spanProcessor` / `metricReader` /
`logRecordProcessor` config keys internally
(`@effect/opentelemetry@0.64.0/src/NodeSdk.ts:98-124`; keys
`dist/dts/NodeSdk.d.ts:19-30`).
Because T5 already gives every lane the same runtime through the private
channel, providing the layer once instruments all three modules with no
further plumbing. `shutdownTimeout`
(`dist/dts/NodeSdk.d.ts:30`) bounds the SDK flush inside the existing
`runtime.dispose()` in `close()`, so teardown ordering (drain → close) is
unchanged. No exported signature anywhere in `src/transport/` gains an
`Effect`/`Layer`/`Runtime` type; `emit()` stays sync/void — and O1a makes
that containment mechanical with an export-surface test (see Plan).

Log correlation: the transport modules currently log nothing through
`Effect.log` (the one console path is `control-source.ts`'s
`defaultOnUnmapped` console.error). Setting the `logRecordProcessor` key
initializes the OTel Logs SDK for a path with no current caller — a
deliberate, cheap YAGNI: it pre-wires correlated logging so any `Effect.log`
added at internal seams during implementation lands in the Logs SDK
correlated to the active span, with no live consumer today. Converting the
existing console path is NOT in scope (the `onUnmapped` callback contract
stays).

### Decision 5 — dependency / supply-chain posture

**Recommendation: exact-pin `@effect/opentelemetry@0.64.0` (not a caret) in
the root catalog, plus caret entries for the `@opentelemetry/*` SDK peers and
the two app-chosen OTLP exporter packages (the exporters are the app's own
deps, NOT declared 0.64.0 peers — Approach / O1b), all subject to the
standard 5-day `minimumReleaseAge = 432000` cooldown
(`bunfig.toml:5-6`) with NO exclusion added** — mirroring the parent record's
posture for `effect` itself (adoption record, Global Constraints: "the
cooldown applies to `effect` as designed"). Exact pin rather than caret
because the package versions independently on a fast-moving 0.x line where
minor bumps track `effect` majors (npm dist-tags: `latest: 0.64.0`, `rc:
4.0.0-rc.111` — the current head is the effect-4 RC); a caret float could
silently pull an effect-4-targeted build. Version bumps are deliberate,
reviewed events.

The pin asymmetry (exact for `@effect/opentelemetry`, carets for the
`@opentelemetry/*` peers) is deliberate: `bun.lock` freezes the entire
resolved graph, so the peer carets only matter at an explicit re-install/
update — at which point the exact `@effect/opentelemetry` pin is the anchor
the peers re-resolve around, and the peers' own 2.x/0.22x lines are the
stable upstream OTel SDK, not the fast-moving 0.x line the exact pin guards
against.

Cooldown clearance: `0.64.0` was published `2026-07-13T15:33:46.803Z` (npm
registry `time` metadata) — ~40 days old at drafting (2026-08-22), so it
clears the 5-day `minimumReleaseAge` with no exclusion, the same clearance
the parent record modeled for `effect` 3.22.1.

## Global Constraints

- **Containment (frozen, parent record):** no `effect`,
  `@effect/opentelemetry`, or `@opentelemetry/*` type in any signature
  exported from `src/transport/` (the package exports `RunnerTransport`
  (`index.ts:56`) and `createUnixSocketTransport` (`index.ts:86`) plus the
  module factories); `emit()` stays sync/void; `runPromise`/`runFork`/
  `runSync` only at the existing seams. The OTel layer and every span/metric
  is transport-internal, and O1a pins this with an export-surface test.
- **Effect floor `3.22.1`** (root catalog `package.json:18`,
  `"effect": "^3.22.1"`); pin `@effect/opentelemetry@0.64.0` exactly — never
  the `4.0.0-rc` line. 0.64.0's `effect` peer is `^3.22.0`
  (`@effect/opentelemetry@0.64.0/package.json`), satisfied.
- **Ordering: implementation lands AFTER RIG-2424 T5 (PR #475) merges.** The
  single-runtime composition point is the seam this design provides the layer
  into; against today's three transitional per-module runtimes
  (`frame-sink.ts:104`, `publish-spine.ts:105`, `control-source.ts:279`) the
  layer would have to be provided three times. If #475 is rejected, this
  record's Decision 4 must be re-cut — surface that, don't improvise.
- **Every RIG-2424 invariant survives:** the existing transport `.test.ts`
  suites stay green UNMODIFIED (instrumentation is observably inert without a
  provider layer). `droppedTraceCount()`/`failedPriorityCount()` keep their
  names, semantics, and values (`publish-spine.ts:78,83,260-264`).
- **Off by default.** No endpoint env ⇒ no OTel layer, no exporter, no
  network egress; no overhead on the sync happy path (the `emit()` enqueue is
  not spanned) — the only sync-path metric is the overflow-drop counter
  (`publish-spine.ts:249`), a cheap in-memory registry increment on the drop
  branch, not exported without a provider.
- **Dependency hygiene:** root-catalog entries; 5-day cooldown applies; no
  `minimumReleaseAgeExcludes` additions (`bunfig.toml:20-24` stays as-is).
- Commit identity per repo convention (mintaka author, Matt co-author
  trailer, `Spec-impact:` line); code comments cite this record by path
  (`docs/designs/platform/compass-agent-effect-otel/design.md`), no issue-id
  in code.

## Plan

Implementation is a SEPARATE later issue; this plan is the frozen task
decomposition it executes. Strict order O1a → O1b → O2 → O3 → O4; O5 (the
endpoint-injection reachability task) depends only on O1a's gate and may land
any time after it. OQ1 is ruled (a), so O1b is confirmed (not contingent).
Each task is one PR gated by the full
existing transport suite green (`bun test` from `packages/compass-agent/`)
plus its own additions. O1a does not start until RIG-2424 T5 (PR #475) is
merged.

### O1a — Dep pin + internal layer scaffold + runtime merge (posture-neutral scaffold)

Add `@effect/opentelemetry@0.64.0` (exact) and its declared peers
(`@effect/opentelemetry@0.64.0/package.json` `peerDependencies`:
`@opentelemetry/api`, `resources`, `sdk-logs`, `sdk-metrics`,
`sdk-trace-base`, `sdk-trace-node`, `sdk-trace-web`, `semantic-conventions`;
`@effect/platform` is a declared peer consumed only by the `Otlp*` modules
this design does not import — whether bun requires it installed is settled at
implementation time, not papered over) to the root catalog +
`packages/compass-agent/package.json`. Create `src/transport/otel-layer.ts`
(module-private, NOT re-exported from `index.ts`): reads
`OTEL_EXPORTER_OTLP_ENDPOINT` once at call time; returns `Layer.empty` when
unset; when set, composes the `NodeSdk.layer` configuration — but the
exporter/endpoint wiring itself sits behind the env gate as a SEAM whose
concrete contents are O1b's (an unset gate is the only path exercised until
O1b lands). Only the unset path is built and tested here, so O1a's concrete
deliverable — `Layer.empty`, the export-surface containment test, and the
no-op `withSpan`/`Metric` test — is posture-neutral. Merge `makeOtelLayer()` into the single runtime construction in
`createUnixSocketTransport` at the T5 seam. Add an `otel-layer.test.ts`
pinning: unset env → `Layer.empty` (identity check); a runtime built with the
merged layer runs `Effect.withSpan` and a `Metric.counter` increment without
error and disposes cleanly.

**Containment test (parent record's export-surface rule, made mechanical):**
the same PR adds a test (or a lint fence) asserting the `src/transport/`
package export surface — `index.ts` / the emitted `.d.ts` — contains no type
from `effect`, `@effect/opentelemetry`, or `@opentelemetry/*`. This turns the
parent record's frozen rule ("the public `.d.ts` stays free of the `effect`
package", `docs/designs/platform/compass-agent-effect-adoption/design.md`
Global Constraints) from remembered convention into a red test.

Interfaces:

- Consumes: `NodeSdk.layer(config)`
  (`@effect/opentelemetry@0.64.0/dist/dts/NodeSdk.d.ts:43-54`; config keys
  `:19-30`); the T5 runtime construction site in `index.ts` (post-#475).
- Produces: `makeOtelLayer(): Layer.Layer<never>` — internal, not exported
  from `src/transport/index.ts`; `createUnixSocketTransport(socketPath:
  string): RunnerTransport` UNCHANGED; the export-surface containment test.

### O1b — OTLP exporter wiring (OQ1 ruled (a) — confirmed)

Fill O1a's exporter seam: `BatchSpanProcessor(OTLPTraceExporter)` as
`spanProcessor` + `PeriodicExportingMetricReader(OTLPMetricExporter)` as
`metricReader`, over HTTP/protobuf, reading `OTEL_EXPORTER_OTLP_ENDPOINT`;
add the two exporter packages (`@opentelemetry/exporter-trace-otlp-proto`,
`@opentelemetry/exporter-metrics-otlp-proto`) — these are NOT declared peers
of 0.64.0, they are this task's own deps. `Resource` naming `service.name =
compass-agent` via the config `resource: { serviceName }` key
(`dist/dts/NodeSdk.d.ts:25-29`); bounded `shutdownTimeout`. Test: set env → a
runtime built with the layer exports a span and a metric to an in-process
OTLP stub and disposes within the shutdown timeout.

**OQ1 is ruled (a)**, so this task is confirmed as drafted; the exporter is a
direct OTLP-over-network export against the destination collector (never the
socket/vsock gateway — the substrate is non-IP; see Open Questions Q1). Spans
(O4) and metrics (O2/O3) no-op without a provider and bind to whatever
provider the layer installs.

Interfaces:

- Consumes: O1a's exporter seam; `@opentelemetry/exporter-*-otlp-proto`.
- Produces: the env-gated live export path; no exported-signature change.

### O2 — Metrics on the publish spine + frame sink

Define the Decision-2 metrics for the two outbound modules in a
module-private `otel-metrics.ts` (`Metric.counter`/`Metric.gauge` values are
cheap module-level constants; without a provider they no-op). Increment at
exactly the cited sites: `publish-spine.ts:249` (overflow drop), `:212`
(failed-batch trace loss), `:221-222` (priority loss), `:228-229` +
`:205` (retry counter + depth gauge set/reset), queue-depth gauge sampled in
`takeBatch` (`publish-spine.ts:155-168`); `frame-sink.ts` attempt counter
inside the retried effect (`:169-178`), give-up counter in the `onSettle`
error arm (`:192-196`). `droppedTraceCount()`/`failedPriorityCount()` values
unchanged. Test: with a test `MetricReader` in the layer, a forced overflow /
failed batch / give-up produces the expected metric deltas alongside the
unchanged method values.

Interfaces:

- Consumes: `Metric` from core `effect`; the increment sites above.
- Produces: internal metric constants; `PublishSpine`/`FrameSink` exported
  shapes UNCHANGED (`publish-spine.ts:67-88`, `frame.ts`).

### O3 — Metrics + spans on the control source

Control-lane metrics (`reconnects` at `control-source.ts:549`,
`no_progress_depth` at `:522`, `flap_resets` at `:543-544`, `unmapped` in
`count()` at `:297-299`) and the `…control.connection` span wrapping the
per-connection `Effect.tryPromise` (`:477-496`) using the Decision-1
mechanism: `attempt` as a `withSpan` attribute (known at span open);
`established` as a span EVENT emitted by the `onHeader` closure through the
captured `Effect.currentSpan` handle (`Span.event`,
`effect@3.22.1/dist/dts/Tracer.d.ts:103`); `no_progress` NOT on the span (it
is the gauge). Test: a scripted flapping fake transport yields the expected
reconnect/flap-reset counts and one connection span per attempt, with the
`established` event present on spans whose fake stream delivered a header.

Interfaces:

- Consumes: `Effect.withSpan`, `Effect.currentSpan`, `Metric` (core
  `effect`); the pump effect in `createSocketControlSource`.
- Produces: `createSocketControlSource(transport, immediate, options?):
  ControlSource` UNCHANGED.

### O4 — Spans on the outbound path + drain

`…publish.batch` around `publish-spine.ts:201-203`; `…frame_sink.durable_send`
/ `…durable_attempt` around `frame-sink.ts:160-198`/`:169-178` (attempt index
via a `Ref` ticked inside the retried effect; the idempotency-key mint at
`:164` stays outside both the span and the retry scope); `…frame_sink.drain`
and `…publish.drain` around `frame-sink.ts:257-260` and
`publish-spine.ts:266-279`. Test: with an in-memory span processor in the
layer, one durable send with two forced failures yields one `durable_send`
parent with three `durable_attempt` children (fiber-tree parenting observed,
not assumed); a drain yields the two drain spans closed before `dispose()`
resolves.

Interfaces:

- Consumes: `Effect.withSpan` (core `effect`); the effects cited above.
- Produces: all exported shapes UNCHANGED; span names per Decision 1.

### O5 — end-to-end env-injection reachability for the endpoint

Own the injection half OQ5 identified as unowned: verify and pin that an
`OTEL_EXPORTER_OTLP_ENDPOINT` set in the Runner-materialized env file reaches
the OTel layer. The key is not `COMPASS_*`-prefixed, so `isReservedEnvKey`
(`cli.ts:103-105`) does not drop it, and the generic env-file sourcing
(`cli.ts:519-523`) merges it into `process.env` before the session — and thus
the transport — is constructed, where O1a's gate reads it. Add a test driving
the env-file sourcing (`readEnvFile` / `main()`) with a file that carries the
endpoint key, asserting it lands in `process.env` unfiltered while a
`COMPASS_`-prefixed key does not — closing the env-file→layer chain. Document
the deployer contract: the endpoint is set as a normal key in the
Runner-materialized env file; no new Runner-side mechanism is needed. TS-only,
in this package's lane; compass-runner reviews it against the provision/env
contract. Nothing here touches Go-side `execSpec` env.

Interfaces:

- Consumes: `readEnvFile` / `envFilePath` / `isReservedEnvKey`
  (`cli.ts:91-105`), the env-file sourcing in `main()` (`cli.ts:519-523`);
  O1a's endpoint gate.
- Produces: the reachability test + deployer-contract doc; no exported-
  signature change; no Runner/Go change.

## Tasks

- [ ] O1a — dep pin 0.64.0 + peers + `otel-layer.ts` scaffold (exporter seam
  env-gated, empty until O1b) + runtime merge + layer test + export-surface
  containment test (starts only after RIG-2424 T5 / PR #475 merges)
- [ ] O1b — OTLP exporter wiring behind the env gate (OQ1 ruled (a))
- [ ] O2 — publish-spine + frame-sink metrics at the cited increment sites
- [ ] O3 — control-source metrics + connection span (event mechanism per
  Decision 1)
- [ ] O4 — outbound + drain spans, fiber-tree parenting pinned by test
- [ ] O5 — end-to-end env-injection reachability test + deployer-contract doc
  (depends only on O1a; TS-only, compass-runner reviews the injection half)

## Open Questions

All five are now RESOLVED — Matt ruled each; compass-runner (Runner/substrate
owner) resolved the substrate and injection-ownership sub-forks Matt raised.
The record is drafted against these dispositions; it freezes when this PR
merges.

1. **Egress posture: where does OTLP go from an egress-sealed container?**
   RESOLVED — **(a) env-gated direct OTLP export, off by default.** Matt ruled
   (a) and raised a substrate sub-fork (does the agent↔Runner socket converge
   to a network transport?); compass-runner resolved it: it does NOT. The
   microVM design swaps the gateway AF_UNIX socket to virtio-vsock — a
   transport swap, not a protocol change (hybrid vsock, host end stays
   AF_UNIX, same Connect/h2c;
   `docs/designs/platform/compass-elastic-session-runtime/microvm-runner.md`,
   Approach (b)/(c), D8) — and vsock is host-local by construction (guest↔its
   own VMM, non-IP, unreachable from the guest's network netns). So OTLP is
   genuinely the agent's first *network* egress in BOTH the container and the
   microVM end-state, drawn against the destination collector over the guest's
   own netns + egress allowlist, never the socket/vsock gateway. A
   central/remote Runner over a network hop was a D8 candidate and was
   REJECTED (co-located one-per-box; the fleet control plane is a separate
   design, RIG-2485), so the substrate never becomes network — no convergence
   risk, no hedge. Decision 3 / O1b stand as drafted. (Forward note: under the
   microVM end-state, egress is in-guest default-deny + a resolved allowlist;
   the OTLP collector endpoint becomes one allowlist entry — future config,
   not a blocker.)
2. **Which endpoint?** RESOLVED — **user-provided (self-host) or
   Runner/iac-injected (managed); provisioning is out of this package's
   scope.** Compass ships as a self-hosted deployment, so the operator
   supplies their own OTLP endpoint via `OTEL_EXPORTER_OTLP_ENDPOINT` (env, no
   code change) — the standard self-hosted-observability pattern. Optionally
   bundling a server-side OTel Collector (Grafana's LGTM `docker-otel-lgtm`
   stack is the canonical example) that fans out to the operator's backend is
   a legitimate FUTURE server-side option, explicitly NOT in this
   agent-package record's scope.
3. **Platform DECISIONS ledger.** RESOLVED — **no ledger entry; the record is
   the ruling.** `docs/designs/platform/` is a real compass category (its
   records include this record's own parent, the RIG-2424 adoption record),
   but the `DECISIONS.md` ledger lives under `docs/designs/product/` and the
   design-ledger-gate CI check governs product records ONLY
   (`tools/design-ledger-gate/index.ts`, `PRODUCT_DIR = "docs/designs/product"`);
   its touch-coupling leg does not fire for a `platform/` record, so #485 is
   not ledger-red. No ledger file is created.
4. **Scope of the metric surface: transport-only, or also `CompassAgent`-
   level?** RESOLVED — **transport-only now.** `effect` is imported ONLY
   inside `src/transport/` (verified: no `effect` import elsewhere under
   `packages/compass-agent/src`); the agent loop (`CompassAgent`, `agent.ts`;
   `lifecycle.ts`; `comms.ts`) is plain TypeScript on the OMP SDK, consuming
   the transport through the `FrameSink`/`ControlSource` seams — not
   Effect-based. Instrumenting it "via effect" would first require putting the
   agent loop under Effect (a large separate adoption) or a parallel raw
   `@opentelemetry/api` path — either way a different mechanism outside this
   record's containment-cheap transport scope. Agent-loop observability is its
   own future record.
5. **Who owns the env-injection path for `OTEL_EXPORTER_OTLP_ENDPOINT`?**
   RESOLVED — **RIG-2426 owns both halves end-to-end (agent-side gate +
   injection), as one task in this package's lane (O5); no separate Runner
   record, no cross-package split.** Matt ruled fold-it-in; compass-runner
   confirmed the injection path is the TS CLI env-file sourcing
   (`packages/compass-agent/src/cli.ts:96-105,500-523`) — the compass-agent TS
   package (this lane), sitting ABOVE the Go `ContainerRuntime` seam, NOT the
   Runner's Go lane and NOT in the microVM record's scope. The endpoint key is
   not `COMPASS_*`-prefixed, so `isReservedEnvKey` (`cli.ts:103-105`) does not
   drop it and it flows through the generic env-file sourcing
   (`cli.ts:519-523`) into `process.env`, where the OTel layer reads it at
   transport construction. compass-runner reviews the injection half for
   correctness against the provision/env contract; escalate to Matt only if it
   turns out to need Go-side env assembly (`agent_exec.go` `execSpec` env),
   which it does not today.
