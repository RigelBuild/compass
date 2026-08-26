# Design: Activate OMP's native loop OpenTelemetry in compass-agent

Status: Draft (freezes on merge). OQ1 RULED by Matt 2026-08-23 → (b) two
independent providers to one collector; the (b) plan below is the one to build.
Linear: RIG-2508 (Refs RIG-2384; spun out of RIG-2426 OQ4).
Sibling record: `docs/designs/repo/compass-agent-effect-otel/design.md`
(RIG-2426/RIG-2518 — the transport OTel, MERGED; its Decision 3 egress posture
and Decision 4 runtime containment are inherited constraints here).

## Problem / Intent

The compass-agent loop emits zero traces today: the sibling record put OTel on
the TRANSPORT only, and its OQ4 deferred loop observability on the premise that
the loop is plain TypeScript needing a from-scratch adoption
(`../../repo/compass-agent-effect-otel/design.md:560-571`). That premise is FALSE: the OMP
SDK the loop is built on ships FULL native OpenTelemetry GenAI instrumentation
— `invoke_agent > chat` / `execute_tool` / `handoff` spans per the GenAI
semantic conventions (`pi-agent-core@16.4.8/src/telemetry.ts:9-17`) — that is
merely opt-in and never switched on: `AgentLoopConfig.telemetry` undefined ⇒
"the loop performs zero tracer lookups" (`pi-agent-core@16.4.8/src/types.ts:434-443`),
and compass-agent's sole `createAgentSession` call (`packages/compass-agent/src/cli.ts:758`)
passes no `telemetry` and sources no `OTEL_*` (verified: zero `OTEL`/`otel`
matches in `cli.ts`). So RIG-2508 is an ACTIVATION + COMPOSITION task, not an
adoption: register a tracer provider in the entrypoint, pass `telemetry` to the
session, and decide how the loop's spans compose with the transport's
already-merged Effect-scoped OTel.

## Approach

**Mirror OMP's own CLI activation pattern in compass-agent's first-party
entrypoint (`cli.ts` `main()`): env-gated global tracer-provider registration +
`telemetry: {}` on the session — reusing `@oh-my-pi/pi-coding-agent`'s shipped
`telemetry-export` module rather than writing our own — and run the loop and
transport as TWO independent providers exporting to the same collector
(recommendation; the fork is Open Question 1).**

### The verified native surface (what we are activating)

- The loop emits GenAI-semconv spans automatically once telemetry is
  configured: span hierarchy `invoke_agent {agent.name}` → `chat {model}` /
  `execute_tool {tool.name}` per run
  (`pi-agent-core@16.4.8/src/telemetry.ts:9-17`); activation is opt-in via
  `AgentLoopConfig.telemetry` — "When unset, every helper short-circuits and
  the loop performs zero tracer lookups. When set but no OTEL SDK is
  registered, `@opentelemetry/api` returns a no-op tracer"
  (`telemetry.ts:19-23`). The loop wires it internally
  (`agent-loop.ts:45-60` imports the whole telemetry helper set).
- The full config surface is `AgentTelemetryConfig`
  (`telemetry.ts:318-396`): `tracer`/`tracerName`, `captureMessageContent`,
  `attributes`/`resolveAttributes`, `agent` identity, `conversationId`
  (falls back to `AgentLoopConfig.sessionId`, `:346-349`), cost/usage hooks
  (`costEstimator`/`onCostDelta`/`onChatUsage`), span hooks
  (`onSpanStart`/`onSpanEnd`/`onRunEnd`/`onTelemetryWarning`). `{}` is a
  complete, valid activation (`types.ts:435-438`: "Passing `{}` enables the
  loop's GenAI-semantic-convention spans … using the global tracer provider").
- `createAgentSession` forwards it verbatim: option declared at
  `pi-coding-agent@16.5.2/src/sdk.ts:561-570`, passed to the loop at
  `sdk.ts:2822` (`telemetry: options.telemetry`). Subagents inherit it with
  their own agent identity and nest under the parent's `execute_tool` span via
  OTEL context propagation (`task/executor.ts:2389-2410`); advisor loops
  likewise (`session/agent-session.ts:2546-2560`). One activation at the
  session covers the whole tree.
- Content capture is env-governed and off by default:
  `OTEL_INSTRUMENTATION_GENAI_CAPTURE_MESSAGE_CONTENT` (`telemetry.ts:58`,
  default wiring `:326-333`).
- Upstream parity: OMP source HEAD (v18.0.0,
  `~/agents/workspaces/upstream-omp/oh-my-pi/packages/agent/src/telemetry.ts:11-22,322`)
  has the identical opt-in shape and `AgentTelemetryConfig` surface — no
  observability delta to wait for; the pinned `^16.4.8` line already has
  everything (`packages/compass-agent/package.json:19-21`).

### Decision 1 — where activation lives, and reuse over rewrite

**Recommendation: activate in `main()` in `packages/compass-agent/src/cli.ts`,
after the Runner env-file sourcing and before `createAgentSession`, by calling
`initTelemetryExport()` / `isTelemetryExportEnabled()` deep-imported from
`@oh-my-pi/pi-coding-agent/telemetry-export`.**

The reference pattern is OMP's own CLI (`pi-coding-agent@16.5.2/src/main.ts:1357-1360`):
`await initTelemetryExport()` registers a global OTLP `NodeTracerProvider`
from `OTEL_EXPORTER_OTLP_*` env, then `if (isTelemetryExportEnabled())
sessionOptions.telemetry = {}`. compass-agent does NOT run pi-coding-agent's
`main()` — `cli.ts` is a first-party entrypoint — so it must make these two
calls itself. Placement inside `main()`:

1. AFTER the env-file merge into `process.env` (`cli.ts:547-551`), so an
   `OTEL_EXPORTER_OTLP_ENDPOINT` set in the Runner-materialized env file is
   honored. The key is not `COMPASS_*`-prefixed, so `isReservedEnvKey`
   (`cli.ts:104-106`) does not drop it — the exact reachability chain the
   sibling record's O5 pinned with a test
   (`../../repo/compass-agent-effect-otel/design.md:478-500`).
2. BEFORE the `createAgentSession` call (`cli.ts:758`), where the gated
   `telemetry: {}` option is added.

**Reuse is available and preferred.** `initTelemetryExport` /
`isTelemetryExportEnabled` / `flushTelemetryExport` are exported from
`pi-coding-agent@16.4.8/src/telemetry-export.ts:43-45,53,142-144`. They are NOT
re-exported from the package barrel (grep of `src/index.ts`: no `telemetry`
match), but the package's exports map declares a wildcard subpath —
`"./*": { "import": "./src/*.ts" }` (`pi-coding-agent@16.4.8/package.json:115-118`)
— and ships `src/` (`package.json:100-102`), so
`import { initTelemetryExport, isTelemetryExportEnabled } from
"@oh-my-pi/pi-coding-agent/telemetry-export"` resolves through the DECLARED
export surface of a package compass-agent already depends on directly
(`packages/compass-agent/package.json:21`). What reuse buys, none of which a
rewrite gets for free:

- The Bun-validated exporter line: "the 1.x line deadlocks under Bun …
  `exporter-trace-otlp-proto@0.218` paired with `sdk-trace-base@2.7` exports
  cleanly" (`telemetry-export.ts:20-23`).
- Correct global registration for Bun: `tracerProvider.register({
  contextManager: new AsyncLocalStorageContextManager().enable() })` — "the
  explicit AsyncLocalStorage context manager keeps parent/child span linkage
  working under Bun" (`telemetry-export.ts:107-110`).
- The full OTEL env contract: `OTEL_SDK_DISABLED`, `OTEL_TRACES_EXPORTER=none`
  (`:59-60`), `OTEL_EXPORTER_OTLP_TRACES_ENDPOINT ?? OTEL_EXPORTER_OTLP_ENDPOINT`
  (`:62-63`), the http/protobuf-only protocol guard (`:69-77`), idempotency
  (`:54-55`), a 30s periodic flush (`:33,113-116`), and postmortem-registered
  shutdown flush (`:118-125`).
- ZERO new dependencies (Decision 4).

One wrinkle: the registered resource defaults `service.name` to `"oh-my-pi"`
unless `OTEL_SERVICE_NAME` is set (`telemetry-export.ts:101-104`). Decision 3
handles this.

### Decision 2 — provider composition with the transport (THE fork; OQ1)

The transport's OTel is Effect-scoped and never global:
`@effect/opentelemetry`'s `NodeSdk.layer` constructs a `NodeTracerProvider`
and wraps it in the scoped `Tracer.OtelTracerProvider` context tag — it never
calls `.register()` / `trace.setGlobalTracerProvider()` (verified in the
installed dep,
`packages/compass-agent/node_modules/@effect/opentelemetry/dist/esm/NodeSdk.js:14-38`
— `layerTracerProvider` news up the provider inside `Layer.scoped`, `layer`
merges Tracer/Metrics/Logger layers; no global API touch). The loop reads the
GLOBAL `@opentelemetry/api` provider (`trace.getTracer`,
`pi-agent-core@16.4.8/src/telemetry.ts:320-321`; `types.ts:437`). So activated
naively, loop spans and transport spans ride two separate providers.

**Recommendation: (b) two independent providers exporting to the same
collector.** Weighed honestly in Alternatives considered; the short form: the
parent/child link (a) would buy is semantically ill-defined here — the
transport batches frames from MANY loop spans into one publish
(`otel-layer.ts` instruments the pump/batch seams, sibling record Decision 1),
and its pump fibers run detached from the loop's async-local context, so a
"unified" tree would mostly show broken or wrong parentage anyway — while (a)
requires reopening the MERGED sibling record's Decision 4 layer wiring.
Correlation under (b) rides identical `service.name`/resource attributes
(Decision 3) plus timing. This is load-bearing and goes to Matt (OQ1).

### Decision 3 — `service.name` alignment (and its honest limit)

The transport tags `service.name = compass-agent` in code
(`src/transport/otel-layer.ts:61`) and that code value WINS over any env: its
`NodeSdk.layer` routes the resource through
`Resource.layerFromEnv(configToAttributes(config.resource))`, which applies the
config attributes AFTER the env read (`Object.assign(attributes,
additionalAttributes)`, installed `@effect/opentelemetry/dist/esm/Resource.js:38-58`).
The reused loop registration is the opposite — env-first:
`OTEL_SERVICE_NAME ?? "oh-my-pi"` (`telemetry-export.ts:103`).

**The activation sets `process.env.OTEL_SERVICE_NAME ??= "compass-agent"` —
but only on the enabled path** (Decision 5's off-by-default constraint: the
`??=` must NOT run when telemetry is off, or it leaks a mutated env to every
tool subprocess the agent spawns and breaks the "bit-identical when off"
guarantee). On the enabled path with no deployer override, both signals land
under `service.name = compass-agent` and correlate.

The honest limit, stated rather than buried: the two sides read the name
asymmetrically, so **a deployer `OTEL_SERVICE_NAME` override renames the LOOP
signal only** — the transport stays `compass-agent` (code wins) — splitting the
two under exactly the override the `??=` was meant to honor. Making them
symmetric would mean the transport reading `OTEL_SERVICE_NAME`, which touches
the FROZEN sibling record — not worth it. Instead: the loop defaults to
`compass-agent` to match the transport, the T2 deployer contract documents that
an override splits the signals (and that the real join key is the shared
resource attributes of Decision 3a below, not the service name), and a
code-level loop resource is rejected as it would fork the registration module.

### Decision 3a — a shared join key so correlation is real, not aspirational

Under (b) the two signals are separate trace trees; "correlated by
`service.name` + time" is weak — the transport spans carry NO session
identifier (`otel-layer.ts:61` sets only `serviceName`), while the loop stamps
`gen_ai.conversation.id` from `AgentLoopConfig.sessionId`
(`pi-agent-core/telemetry.ts:346-349`). Two agents on one collector would be
separable only by time. The fix costs ZERO transport code and no fence breach:
the transport's `NodeSdk.layer` resource already reads `OTEL_RESOURCE_ATTRIBUTES`
natively through `Resource.layerFromEnv` (installed `Resource.js:40-52`), so the
activation sets `process.env.OTEL_RESOURCE_ATTRIBUTES` to carry the session
identity (e.g. `compass.session.id=<id>`) on the SAME enabled path as
`OTEL_SERVICE_NAME`, and both providers stamp it. This makes the join key a real
resource attribute both signals share, and it composes with either OQ1 ruling.
See Open Question 4.

### Decision 4 — dependencies: none new; no FOD bump

Reuse means the exporter/SDK code paths run inside `pi-coding-agent`'s own
dependency closure (`pi-coding-agent@16.4.8/package.json:66-71` —
`@opentelemetry/api`, `context-async-hooks`, `exporter-trace-otlp-proto`,
`resources`, `sdk-trace-base`, `sdk-trace-node`; all dynamic-imported at
`telemetry-export.ts:84-96`). Every one of those is already resolved in the
workspace lockfile (e.g.
`node_modules/.bun/@opentelemetry+context-async-hooks@2.10.0…`,
`…exporter-trace-otlp-proto@0.220.0…` — present in the bun store), and
compass-agent itself already carries the full `@opentelemetry/*` catalog set
from RIG-2518 O1b (`packages/compass-agent/package.json:22-31`; root catalog
`package.json:19-27`). **No `package.json` dependency change, no `bun.lock`
change, no agent-image FOD `outputHash` bump.** (The write-our-own alternative
WOULD need `@opentelemetry/context-async-hooks` promoted to a direct dep —
today it is only a transitive of `sdk-trace-node`,
`node_modules/@opentelemetry/sdk-trace-node/package.json:61-64` — which is
part of why reuse is preferred.)

### Decision 5 — content capture and posture defaults

- **Content capture default OFF.** Prompt/response content is sensitive; the
  activation sets nothing — capture turns on only when a deployer sets
  `OTEL_INSTRUMENTATION_GENAI_CAPTURE_MESSAGE_CONTENT`
  (`pi-agent-core@16.4.8/src/telemetry.ts:58,326-333`; `true`⇒full,
  `summary`⇒bounded summaries).
- **`telemetry: {}` is the whole session option** — mirroring OMP's CLI
  (`main.ts:1354-1356`: "An empty config is enough"). Agent identity, cost
  estimation, and usage hooks are real surface (`telemetry.ts:343-366`) but
  each is a product decision with no consumer today; adding them later is a
  one-field change and needs no re-design.
- **Off by default, inherited hard constraint** (sibling Decision 3): no
  endpoint env ⇒ `initTelemetryExport` returns before registering anything
  (`telemetry-export.ts:62-63`) ⇒ `isTelemetryExportEnabled()` false
  (`:43-45`) ⇒ no `telemetry` option ⇒ the loop does zero tracer lookups
  (`types.ts:437-438`). No provider, no exporter, no network egress,
  black-box behavior unchanged.
- **Product content policy lives at the gateway, not this record (RIG-2711,
  ruled).** The user-facing content-capture toggle — managed: default-ON
  sharing plus a free per-user Privacy Mode mirroring Cursor; self-hosted: a
  single deployer-level collect on/off — is enforced at the gateway chokepoint
  (joined to loop spans on `compass.session.id`), not on the agent. This
  record's agent-side `OTEL_INSTRUMENTATION_GENAI_CAPTURE_MESSAGE_CONTENT`
  hatch stays a debug-only opt-in: the loop's trace *structure* (spans, tool
  calls, timings, errors, token usage) is already captured agent-side and is the
  actual per-agent debug surface, so keeping message *content* off the agent by
  default loses no debuggability. Gateway placement and the settings-UI toggle
  are a compass-server + UI surface tracked in RIG-2711.

## Alternatives considered

### The load-bearing fork: provider composition (Decision 2 / OQ1)

**(a) UNIFY — one global provider both consume; loop and transport spans in
one trace tree.**

The entrypoint registers the global provider (via `initTelemetryExport()`),
and the transport's `otel-layer.ts` stops constructing its own: instead of
`NodeSdk.layer(...)` it wires effect's tracer to the GLOBAL provider via
`Tracer.layerGlobal` — which exists precisely for this:
`layerGlobal = Layer.unwrapEffect(Effect.map(make, Layer.setTracer)).pipe(
Layer.provideMerge(layerGlobalTracer))` where `layerGlobalProvider =
Layer.sync(TracerProvider, () => OtelApi.trace.getTracerProvider())`
(`node_modules/@effect/opentelemetry/dist/esm/internal/tracer.js:273-280`;
re-exported `dist/esm/Tracer.js:39`). The effect tracer picks up an active
OTel span as external parent ONLY when a fiber's span STARTS while an OTel span
is in the AsyncLocalStorage context (`getOtelParent` reads `contextApi.active()`,
`internal/tracer.js:21-30`; `OtelSpan` ctor falls back to it at `:53-54`).

- For: one trace tree in principle; a single exporter/batcher (one egress
  pipeline) instead of two.
- **(a) is cheap to build and buys almost nothing** — the honest framing, which
  matters because Matt will probe both halves:
  1. **Cheap, not a redesign.** `NodeSdk.layer` degrades gracefully with the
     span processor omitted — `isNonEmpty(config.spanProcessor)` gates the
     tracer layer to `Layer.empty` and the `metricReader` path is independent
     (installed `NodeSdk.js:26-31`). So (a) is roughly
     `Layer.merge(Tracer.layerGlobal, NodeSdk.layer(() => ({ metricReader,
     resource })))` in `otel-layer.ts` — a few lines plus test churn plus the
     ordering invariant below, NOT the "reopens merged Decision 4" redesign an
     earlier draft claimed. The transport's METRICS keep riding their own
     `NodeSdk.layer` call unchanged; only the tracer moves to the global.
  2. **But the parentage it buys is fictional — this is why (a) loses.** The
     transport spans worth having run on detached pump/batch/reconnect fibers
     forked at TRANSPORT CONSTRUCTION (`cli.ts:588`), before any session exists
     (`cli.ts:758`), on Effect's scheduler OUTSIDE the loop's ALS context — so
     when their spans start there is no active loop span for `getOtelParent` to
     pick up. `…publish.batch` also aggregates frames from MANY loop turns
     (sibling Decision 1), so a single parent is not even well-defined; and
     `…control.connection` has no loop parent at all. The only spans that would
     genuinely nest are the sync `emit()` enqueues, which the sibling record
     deliberately does NOT span (`../../repo/compass-agent-effect-otel/design.md:159-162`).
     (a) therefore delivers a shared exporter and NO real tree — the same
     correlation (b) gets from a shared collector, at the cost of the ordering
     invariant below and a coupling to the frozen transport.
  3. **Ordering coupling.** The global provider must be registered BEFORE
     transport construction (`cli.ts:588`) or the transport's
     `Tracer.layerGlobal` captures the no-op global provider — a startup-order
     invariant that today does not exist.
- Honest residual for (a): a genuinely unified VIEW of "this loop turn and the
  frames it produced" is a span-LINKS problem, not a parentage one — the
  transport's `…publish.batch` (one batch ← many turns) would LINK to the
  originating turns' trace contexts, which requires threading trace context
  through the frame sink (a real transport change, out of scope here). Naming
  the true shape — links, because the parentage genuinely is fictional — is the
  accurate picture of what a unified view costs when someone wants it. Nothing
  in (b) forecloses it.

**(b) TWO INDEPENDENT PROVIDERS exporting to the same collector —
RECOMMENDED.**

The entrypoint registers the global provider for the loop; the transport
keeps its scoped provider untouched. Both export OTLP/http-protobuf to the
same `OTEL_EXPORTER_OTLP_ENDPOINT`, both under `service.name = compass-agent`
AND both stamped with a shared `compass.session.id` resource attribute
(Decision 3 / 3a).

- For: zero transport changes — the merged sibling record stays frozen; zero
  cross-boundary coupling (the transport containment fence,
  `export-surface.test.ts`, is untouched); the loop side is exactly OMP's
  own battle-tested pattern (`main.ts:1357-1360`); each side keeps its own
  flush/shutdown semantics (postmortem flush `telemetry-export.ts:118-125`
  vs `runtime.dispose()` bounded by `shutdownTimeout`, `otel-layer.ts:53-62`).
- Against: no shared root span — loop traces and transport traces are sibling
  trees, joined by `service.name` + the shared `compass.session.id` resource
  attribute (Decision 3a — without it, only time, which does not separate two
  agents on one collector) + time, not by parent/child. As (a) shows, that
  parent/child link would be fictional anyway. Two BatchSpanProcessors → two
  OTLP client stacks in-process (small, bounded overhead).

### Rejected: write a first-party registration module instead of reusing `telemetry-export`

A compass-agent-owned `loop-otel.ts` doing `NodeTracerProvider` +
`register()` directly. Rejected primarily on TRACK RECORD: it re-implements,
without the battle-testing OMP's own CLI leans on, the Bun deadlock avoidance
(the validated exporter/SDK version pairing), AsyncLocalStorage context-manager
registration, the OTEL kill-switch/protocol env contract, idempotency, and the
flush lifecycle that `telemetry-export.ts` already encodes
(`:20-23,59-77,107-125`) — reproducing that correctly is the real cost, and a
drift there is a silent-no-spans failure. Secondary cost: it would add
`@opentelemetry/context-async-hooks` as a DIRECT dependency (today only a
transitive of `sdk-trace-node`,
`node_modules/@opentelemetry/sdk-trace-node/package.json:61-64`) — a
`package.json` manifest delta, and possibly a `bun.lock`/FOD delta (it would
resolve to the same `@2.10.0` already in the store, so the lock delta may be
nil; not certain enough to lean on). The deep-import subpath is a DECLARED
export (`pi-coding-agent/package.json:115-118`), not a reach into private guts.
Fallback if the subpath were ever withdrawn: this alternative, at the cost
above.

### Rejected: activate via `tracer` override instead of the global provider

`AgentTelemetryConfig.tracer` (`telemetry.ts:319-323`) could carry a tracer
from a non-global provider (e.g. the transport's). Rejected: the transport's
provider lives inside its `ManagedRuntime` behind the containment fence — no
`@opentelemetry/*`/`@effect/*` type may cross the transport's export surface
(sibling Global Constraints; `export-surface.test.ts`), so extracting a
`Tracer` for the loop would breach exactly the boundary the sibling record
froze. The global-provider path needs no new surface anywhere.

## Global Constraints

- **Off by default (hard; inherited from sibling Decision 3).** No
  `OTEL_EXPORTER_OTLP_ENDPOINT` (or `..._TRACES_ENDPOINT`) ⇒ no provider
  registration, no `telemetry` session option, zero tracer lookups in the
  loop (`pi-agent-core@16.4.8/src/types.ts:437-438`), no network egress.
  Black-box behavior with no endpoint is bit-identical to today; the existing
  test suites stay green unmodified.
- **No new dependencies.** The activation reuses
  `@oh-my-pi/pi-coding-agent/telemetry-export` (declared wildcard subpath
  export, `pi-coding-agent@16.4.8/package.json:115-118`) whose OTel deps are
  its own (`package.json:66-71`) and already in the lockfile. `bun.lock`
  MUST NOT change; therefore no agent-image FOD `outputHash` bump
  (`agent-image/entrypoint.nix`). If implementation discovers a dep is
  needed after all, STOP and surface it — that flips the FOD hash and is a
  reviewable event, not a drive-by.
- **Transport containment fence untouched (frozen, sibling record).** No
  change to `src/transport/` under this record's recommended (b); no
  `effect`/`@effect/opentelemetry`/`@opentelemetry/*` type in any exported
  signature; `export-surface.test.ts` stays green unmodified. (If OQ1 is
  ruled (a), the transport re-wiring is a NEW task added to this plan before
  freeze — see OQ1 — never an improvised edit.)
- **`service.name` alignment, with a documented limit (Decision 3/3a):** on the
  enabled path both signals default to `service.name = compass-agent` — the
  transport tags it in code (`src/transport/otel-layer.ts:61`, code wins over
  env), the loop side sets `process.env.OTEL_SERVICE_NAME ??= "compass-agent"`
  ONLY when telemetry is enabled. The two read the name asymmetrically, so a
  deployer `OTEL_SERVICE_NAME` override renames the LOOP signal only; the real
  cross-signal join key is the shared `compass.session.id` resource attribute
  (Decision 3a), set via `OTEL_RESOURCE_ATTRIBUTES` on the same enabled path and
  read natively by both providers. T2's deployer contract documents this.
- **Content capture stays env-opt-in** via
  `OTEL_INSTRUMENTATION_GENAI_CAPTURE_MESSAGE_CONTENT`; the activation never
  sets it (`pi-agent-core/src/telemetry.ts:58,326-333`).
- **Version base: the workspace installs `@oh-my-pi/*@16.5.2`** (`bun.lock`;
  `node_modules/.bun/@oh-my-pi+pi-coding-agent@16.5.2…`), not the `@16.4.8` an
  earlier draft cited — the `^16.4.8` floor (`packages/compass-agent/
  package.json:19-21`) already drifted 16.4.8→16.5.2 unnoticed. Every
  load-bearing surface holds across that drift (verified at 16.5.2:
  `initTelemetryExport`/`telemetry-export.ts:53`, endpoint gate `:62-63`,
  service default `:103`, the `"./*"` export map `package.json:115-118`, the
  CLI activation pattern `main.ts:1354-1360`, the `telemetry` option decl +
  forward `sdk.ts:561-570`/`:2822`), and
  the exporter pairing that actually ships is
  `exporter-trace-otlp-proto@0.220.0` with `sdk-trace-base@2.10.0`. NOTHING pins the exports map or signatures across a
  future minor bump; the guard is cheap and already present — T1's tests import
  the module, so a withdrawn subpath or changed signature fails typecheck/test
  at CI, not in production. That is the honest answer to OQ2.
- **Placement invariant:** provider registration + `telemetry` gating happen
  in `main()` AFTER the env-file merge (`cli.ts:547-551`) and BEFORE
  `createAgentSession` (`cli.ts:758`). NOTE the transport is constructed at
  `cli.ts:588`, between the two — harmless under (b); **if OQ1 is ruled (a),
  registration must additionally precede transport construction (`cli.ts:588`)**
  or the transport's `Tracer.layerGlobal` captures the no-op global provider.
- Commit identity per repo convention (mintaka author, Matt co-author
  trailer); code comments cite this record by path
  (`docs/designs/platform/compass-agent-loop-otel/design.md`), no issue-id in
  code.
- **Ledger note (main agent flips it, not this record's author):** target
  surface is `docs/designs/platform/DECISIONS.md`. NOTE: that file does not
  exist at drafting time — the only ledger is
  `docs/designs/product/DECISIONS.md`, and the sibling record's OQ3 ruled
  "no ledger entry; the record is the ruling" for platform records
  (`../../repo/compass-agent-effect-otel/design.md:552-559`). If the platform ledger has
  since been created, the rows to add are: (1) loop OTel activation via
  reused `telemetry-export` in the first-party entrypoint; (2) the OQ1
  composition ruling ((a) or (b) as Matt decides); (3) no-new-deps / no FOD
  bump. Otherwise, per the sibling precedent, no ledger delta.

## Plan

Both tasks are small; they are separate PRs because they carry different
review lenses (T1 = activation semantics; T2 = deployment/reachability
contract). Order T1 → T2. Each PR is gated by the full package suite green
(`bun test` from `packages/compass-agent/`) plus its own additions. T1 starts
only after Matt rules OQ1 (if (a), a T3 transport re-wiring task is added
before freeze; the plan below is the (b) shape).

### T1 — Activate: registration + gated `telemetry: {}` in `cli.ts` `main()`

In `main()` (`packages/compass-agent/src/cli.ts:528`), after the env-file
merge (`:547-551`):

The order is load-bearing — the loop provider reads `OTEL_SERVICE_NAME` /
`OTEL_RESOURCE_ATTRIBUTES` at `initTelemetryExport()` time, so the env defaults
must be in place BEFORE registration, and both must be gated on telemetry being
on (F2 — an unconditional `process.env` mutation leaks to every tool subprocess
and breaks off-is-bit-identical):

1. Compute the enabled flag from the endpoint env directly (the same predicate
   `isTelemetryExportEnabled` reports — `OTEL_EXPORTER_OTLP_TRACES_ENDPOINT ??
   OTEL_EXPORTER_OTLP_ENDPOINT` present, kill-switches clear;
   `telemetry-export.ts:56-63`). If not enabled, do nothing — no env write, no
   `telemetry` option — and skip to `createAgentSession` unchanged.
2. Enabled path only, BEFORE registering:
   - `process.env.OTEL_SERVICE_NAME ??= "compass-agent";`
   - append `compass.session.id=<id>` to `process.env.OTEL_RESOURCE_ATTRIBUTES`
     (Decision 3a — append, never clobber a deployer-set value), so both
     providers stamp the shared join key.
3. `await initTelemetryExport();` — deep import from
   `@oh-my-pi/pi-coding-agent/telemetry-export`; idempotent
   (`telemetry-export.ts:54-55`), now registers with the env defaults in place.
4. In the `createAgentSession` options (`:758`): spread
   `...(isTelemetryExportEnabled() ? { telemetry: {} } : {})` — key omitted
   entirely when export is off, so the loop keeps its literal-undefined
   zero-lookup path (`types.ts:437-438`).

Tests (the seam, not OMP's loop — OMP's own suite owns span correctness):

- Endpoint unset ⇒ `createAgentSession` receives NO `telemetry` key AND
  `process.env` is unmutated (no `OTEL_SERVICE_NAME`/`OTEL_RESOURCE_ATTRIBUTES`
  written) — true bit-identical inertness (F2). Drive `main()` with the
  existing `MainDeps.createSession` injection seam (`cli.ts:484-522` — tests
  already compose `main` over recorded deps) and assert on the captured options
  and on `process.env` deltas.
- Endpoint set ⇒ the captured options carry `telemetry: {}` and the enabled-path
  env defaults are present. Assert `isTelemetryExportEnabled()` truthiness and
  the captured option shape rather than exporting real spans.
- **Test-isolation hazard (F3), named so it is designed for, not tripped over:**
  `initTelemetryExport` keeps a module-level singleton with no teardown
  (`telemetry-export.ts:36,54-55`) and calls `tracerProvider.register()` on the
  GLOBAL `@opentelemetry/api` provider (`:107-110`); a second global
  registration is rejected-and-logged, not overwritten, and it leaves a live
  `BatchSpanProcessor` + real `OTLPTraceExporter` that attempts an HTTP export
  on the postmortem path (`:118-125`). So the REAL-`initTelemetryExport`
  (endpoint-set) case MUST run in a spawned subprocess (`Bun.spawn` a tiny
  script asserting on its output), never in-process alongside other tests — or
  it poisons the global provider for the rest of the suite and fires network in
  CI. The in-process activation tests mock the module at the `MainDeps` seam and
  assert only `cli.ts`'s gating/env logic. The optional global-provider smoke
  test (`InMemorySpanExporter` + `SimpleSpanProcessor` via
  `trace.setGlobalTracerProvider`, house pattern
  `src/transport/outbound-spans.test.ts:27-31,49-63`) calls `trace.disable()`
  first to guarantee a clean global, and never shares a process with the
  real-register subprocess case.

Interfaces:

- Consumes: `initTelemetryExport(): Promise<void>`,
  `isTelemetryExportEnabled(): boolean`
  (`pi-coding-agent@16.5.2/src/telemetry-export.ts:43-45,53-81`);
  `CreateAgentSessionOptions.telemetry?: AgentTelemetryConfig`
  (`sdk.ts:561-570`); the `MainDeps` test seam (`cli.ts:484-522`).
- Produces: no exported-signature change anywhere; `main(env?, deps?)`
  unchanged (`cli.ts:528-531`).

### T2 — Deployer contract + env reachability pin

Extend the sibling O5 reachability guarantee to the loop's env set: a test
asserting `OTEL_SERVICE_NAME`, `OTEL_EXPORTER_OTLP_TRACES_ENDPOINT`,
`OTEL_RESOURCE_ATTRIBUTES`, and `OTEL_INSTRUMENTATION_GENAI_CAPTURE_MESSAGE_CONTENT`
set in the Runner-materialized env file survive `readEnvFile` filtering
(`isReservedEnvKey`, `cli.ts:104-106` — none are `COMPASS_*`/`HOME`) and land
in `process.env` before the registration point. Document the deployer
contract in the record's directory (`docs/designs/platform/
compass-agent-loop-otel/deployer-contract.md`): endpoint key(s), service-name
override, content-capture opt-in and its sensitivity warning, the
off-by-default guarantee, and TWO asymmetries a deployer must know (both
documented, not code-fixed under (b)):

- **Endpoint-gate asymmetry (F5):** the loop honors
  `OTEL_EXPORTER_OTLP_TRACES_ENDPOINT ?? OTEL_EXPORTER_OTLP_ENDPOINT`
  (`telemetry-export.ts:62-63`); the transport gates ONLY on
  `OTEL_EXPORTER_OTLP_ENDPOINT` (`otel-layer.ts:51-54`, never reads the
  `TRACES_` variant). So `OTEL_EXPORTER_OTLP_ENDPOINT` turns on BOTH signals;
  the `TRACES_` variant alone turns on the loop and leaves the transport dark.
  Contract line: set `OTEL_EXPORTER_OTLP_ENDPOINT` for whole-agent telemetry.
- **Service-name split (F1):** a deployer `OTEL_SERVICE_NAME` override renames
  the loop signal only (the transport's code value wins); the durable
  cross-signal join is `compass.session.id` in `OTEL_RESOURCE_ATTRIBUTES`
  (Decision 3a), not the service name.

No Runner/Go change (same ruling as sibling OQ5).

Interfaces:

- Consumes: `envFilePath`/`readEnvFile` (`cli.ts:92-94,278-285`), the merge
  loop (`cli.ts:547-551`).
- Produces: the reachability test + `deployer-contract.md`; no code-path
  change beyond T1's.

## Tasks

- [x] OQ1 ruled by Matt → (b) two independent providers. No T3; the transport
  stays frozen and untouched.
- [ ] T1 — endpoint-gated registration + enabled-path env defaults
  (`OTEL_SERVICE_NAME`, `compass.session.id` into `OTEL_RESOURCE_ATTRIBUTES`),
  plus `telemetry: {}` in `main()`; seam tests (bit-identical inert-when-unset,
  gated-when-set) in-process over the `MainDeps` mock, real-register smoke in a
  spawned subprocess (F3 isolation)
- [ ] T2 — env reachability test (incl. `OTEL_RESOURCE_ATTRIBUTES`) +
  `deployer-contract.md` documenting both asymmetries (F1 service-name split,
  F5 endpoint-gate)

## Open Questions

1. **LOAD-BEARING — provider composition: (a) unify into one trace tree, or
   (b) two independent providers to one collector? — RULED (Matt, 2026-08-23):
   (b).** Full weighing in Alternatives considered. **Recommended (b); ruled
   (b).** The key point is not that (a) is expensive — it is cheap (a few
   lines: `Tracer.layerGlobal` merged
   into the transport's layer, its metrics untouched, `NodeSdk.layer` degrades
   gracefully with the span processor omitted, `NodeSdk.js:26-31`) — it is that
   (a) buys almost NOTHING: the parent/child links it would create are
   fictional at every seam that matters (the transport's pump/batch/reconnect
   fibers are forked at construction, `cli.ts:588`, outside any loop span's ALS
   context, so `getOtelParent` picks up no parent; `publish.batch` aggregates
   many turns into one span with no well-defined single parent). So (a) trades
   a registration-before-construction ordering invariant and a coupling to the
   frozen transport record for the same collector-level correlation (b) already
   has. (b) is zero transport delta, exactly OMP's own shipped pattern, and
   does not foreclose the genuinely-unified view later — which is a span-LINKS
   change (threading trace context through the frame sink), not the parentage
   (a) offers. Correlation under (b) rides `service.name = compass-agent` + the
   shared `compass.session.id` resource attribute (Decision 3a) + time.
2. **Is the `@oh-my-pi/pi-coding-agent/telemetry-export` deep import an
   acceptable contract across the `^16.4.8` floor's drift?** It is a DECLARED
   wildcard subpath export (`package.json:115-118`) of a direct dependency, the
   Bun-validated path OMP's own CLI uses, but not in the curated barrel
   (`src/index.ts` has no telemetry export). **Recommendation: yes.** The best
   evidence is that the floor ALREADY drifted 16.4.8→16.5.2 in this workspace
   and every load-bearing surface (the exports map, `initTelemetryExport`'s
   signature, the endpoint gate, the service default) held across it unnoticed
   — and nothing PINS them for the next bump, so the guard is a cheap
   import-and-typecheck: T1's tests already import the module, so a withdrawn
   subpath or changed signature reddens CI, not production. Fallback if ever
   withdrawn: the first-party registration module in Alternatives, at a
   manifest delta + possible FOD bump. Low stakes; not a blocker.
3. **`telemetry: {}` now vs a richer config (agent identity, `onChatUsage`
   for token metrics)?** **Recommendation: `{}` now** (OMP-CLI parity); the
   richer surface (`AgentTelemetryConfig`, `telemetry.ts:318-396`) is
   additive one-field-at-a-time later and each field is a product decision
   (e.g. what identity to stamp) with no consumer today. Not load-bearing.
4. **Shared join key — set `compass.session.id` on both signals (Decision
   3a)?** Under (b) the two trees would otherwise be joinable only by
   `service.name` + time (weak: two agents on one collector are separable only
   by time, and an `OTEL_SERVICE_NAME` override splits even the name). Setting
   `compass.session.id` via `OTEL_RESOURCE_ATTRIBUTES` costs one env write on
   the loop side and ZERO transport code (its `Resource.layerFromEnv` reads the
   var natively, `Resource.js:40-52`), and composes with either OQ1 ruling.
   **Recommendation: yes** — it makes (b)'s correlation real rather than
   aspirational. Folded into T1; flagged as an OQ only because it presumes the
   session id is in scope at the `cli.ts` activation point (it is — the
   entrypoint owns session identity). Not load-bearing on its own.
