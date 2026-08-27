# Design: End-to-end trace continuity — inbound message → turn → tool calls

Status: Draft (freezes on merge).
Linear: RIG-2508 (follow-on; the sibling record deferred this).
Sibling record: `docs/designs/platform/compass-agent-loop-otel/design.md`
(FROZEN; merged in PR #561 at `2740d2d3`). This record does NOT reopen its OQ1
ruling (b): two independent providers to one collector.

## Ruling (Matt, 2026-08-27)

All four Open Questions ruled; the record freezes on merge as (b)-shaped
throughout:

- **OQ1 (load-bearing) = (b)** — server-side `traceparent` stamping on the
  inbound control. The turn joins the SERVER's trace for its message
  (creation → routing → delivery → turn → tool calls, one connected trace);
  the cross-lane wire contract (compass-server stamps, mirroring the
  from_handle denorm) is authorized. The (a) agent-side-mint / T3′ contingency
  is CLOSED.
- **OQ2 = hybrid** — true parent when one message starts an idle turn, span
  links for the coalesced (N→1) and mid-turn-steer cases.
- **OQ3 = yes** — `SessionInjection` carries the same `traceparent` string,
  folded into T4.
- **OQ4 = out of scope** (confirmed) — outbound turn → published frames gets
  its own record if/when asked.

So the full T1→T4 set is live: T1/T2 land agent-side (topology core + the now
FINAL `traceparent: string` signature), then T4 (compass-server proto + stamp)
and T3 (agent-side decode) ship as a stacked pair. This ruling also fixes the
scope of compass-server's RIG-2685 as a CONTINUATION of the stamped context
(server = trace origin), not a fresh root.

## Problem / Intent

Once the sibling record's T1 activation lands, the loop emits a connected
`invoke_agent → chat / execute_tool` span tree per turn — but the tree is
rootless: nothing connects an inbound channel MESSAGE to the turn it starts.
Matt's directive — "trace messages to turns they start on agents, into the
tool calls" — is half-delivered by activation (turn → tool calls come free);
this record designs the other half, which the sibling explicitly deferred as
"a span-LINKS problem … requires threading trace context through the frame
sink (a real transport change, out of scope here)"
(`../compass-agent-loop-otel/design.md:295-299`). Scope: INBOUND continuity
only (message → turn). Outbound (turn → published frames) stays deferred
(Open Question 4).

## Approach

**Thread a W3C `traceparent` STRING from the server's inbound steer/deliver
control to the turn's `invoke_agent` span — as a true PARENT when exactly one
message starts an idle turn, and as SPAN LINKS for the coalesced (N→1) and
mid-turn-steer cases — via a small first-party bridge module that composes
with the sibling record's activation through the loop's shipped `onSpanStart`
hook. Trace context crosses every boundary as a plain string, never an OTel
type. Server-side origination (the load-bearing fork) is RECOMMENDED but
cross-lane; the agent-side TOPOLOGY machinery is identical under either ruling
and lands first, inert, while the `traceparent`-string signature threaded
through `steer`/`deliver` is (b)-shaped and finalizes on the OQ1 ruling.**

### The verified mechanism (what makes each case possible)

All claims verified this session at the installed `@oh-my-pi/*@16.5.2`.

**Parentage (the clean case) rides `context.active()`.** The loop's
`invoke_agent` span takes no explicit parent — `startInvokeAgentSpan` calls
`startSpan` with only kind+model (`pi-agent-core@16.5.2/src/telemetry.ts:673-677`),
and `startSpan` resolves its context as
`const ctx = options.parent ? trace.setSpan(context.active(), options.parent) : context.active();`
(`telemetry.ts:495`). The call chain from `session.agent.prompt()` to that
read is SYNCHRONOUS end to end:

- `prompt()` runs straight to `await this.#runLoop(msgs, promptOptions)` with
  no earlier await (`pi-agent-core@16.5.2/src/agent.ts:980-1022`);
- `#runLoop` has no top-level await before invoking
  `agentLoop(messages, context, config, …)` (`agent.ts:1061-1211` — the only
  `await`s in that range sit inside callbacks: `:1093` cursorOnToolResult,
  `:1152` syncContextBeforeModelCall, `:1202` getAsideMessages);
- `agentLoop`'s launch IIFE runs synchronously to `await runLoop(…)`
  (`pi-agent-core@16.5.2/src/agent-loop.ts:342-357`), and `runLoop` starts the
  span before its first await:
  `const telemetry = resolveTelemetry(config.telemetry, config.sessionId);` /
  `const invokeAgentSpan = startInvokeAgentSpan(telemetry, config.model);`
  (`agent-loop.ts:691-692`).

So wrapping the `prompt()` call in
`context.with(trace.setSpan(context.active(), trace.wrapSpanContext(remoteCtx)), …)`
makes the remote context `invoke_agent`'s true parent — no loop change, no
OMP change. `@opentelemetry/api` is already a DIRECT dependency
(`packages/compass-agent/package.json:22`:
`"@opentelemetry/api": "catalog:"`), so this costs zero new deps.

Two honest caveats on this mechanism, stated so they are designed for rather
than tripped over:

- **The synchronicity is a property of the installed 16.5.2 SDK, not a
  documented SDK contract.** A future SDK release inserting an await anywhere
  on the `prompt() → startInvokeAgentSpan` path would break parentage
  SILENTLY (the turn span would simply come out rootless). T2's "idle steer
  ⇒ remote parent" test is the CANARY: it exercises the real chain against
  the installed SDK, so such a regression reddens CI, not production.
- **Parentage additionally requires a registered OTel context manager.**
  Without one, `context.with` / `context.active()` are no-ops and parentage
  silently fails. The sibling record's T1 registration provides exactly this:
  `tracerProvider.register({ contextManager: new
  AsyncLocalStorageContextManager().enable() })`
  (`pi-coding-agent@16.5.2/src/telemetry-export.ts:110`). This is an explicit
  invariant coupling the bridge to the sibling T1 registration path — the
  bridge is only ever built on the same enabled path that runs
  `initTelemetryExport()` (Decision 4), so the coupling holds by
  construction.

**Links (the other two cases) ride the shipped `onSpanStart` hook plus
`Span.addLink`.** The telemetry config the sibling record activates as `{}`
accepts `readonly onSpanStart?: (ctx: TelemetryHookContext) => void;`
(`telemetry.ts:377`), where `TelemetryHookContext` carries the live span and
its kind: `export interface TelemetryHookContext extends
TelemetryAttributeContext { readonly span: Span; }` (`telemetry.ts:306-309`),
`kind: TelemetrySpanKind` with
`type TelemetrySpanKind = "invoke_agent" | "chat" | "execute_tool" | "handoff"`
(`telemetry.ts:295,177`). Capturing the current `invoke_agent` span there
gives the agent a handle to attach links after creation:
`addLink(link: Link): this;`
(`@opentelemetry/api@1.9.1/build/src/trace/span.d.ts:65`; the installed SDK
line is `sdk-trace-base@2.10.0`, which implements it). Per the `addLink` API
contract, a link added AFTER creation does not affect the `invoke_agent` span's
sampling decision — fine here: that decision is made at turn start and the
off-by-default `BatchSpanProcessor` path runs no head sampler that could drop a
linked-late span. Hook failures are
non-fatal by contract (`telemetry.ts:284` `"on_span_start_failed"` warning
code). The sibling record anticipated exactly this extension: richer config
"is additive one-field-at-a-time later"
(`../compass-agent-loop-otel/design.md:584-588`).

**The capture must filter out subagent turns.** The loop spreads the parent's
telemetry config into every task-subagent loop —
`{ ...options.parentTelemetry, agent: subagentAgentIdentity, conversationId:
undefined }` (`pi-coding-agent@16.5.2/src/task/executor.ts:2409-2417`) — so
`onSpanStart` fires for the MAIN turn's `invoke_agent` AND for every
subagent's `invoke_agent`, all sharing the one hook closure, and compass runs
subagents (`cli.ts` discoverAgents). An unguarded single `capturedInvokeAgent`
slot would be clobbered by a subagent's span, mis-targeting mid-turn-steer
links and leaving later steers no-oping after the subagent's `onSpanEnd`
clears the slot. The guard: capture only when `ctx.agent === undefined`. The
main compass turn activates with `telemetry: {}` (no agent identity ⇒
`ctx.agent === undefined`), while every subagent config sets
`agent: subagentAgentIdentity` (non-undefined, `executor.ts:2402-2413`) — an
EXACT filter selecting only the main loop's turn spans. The single slot is
then correct: `Agent.prompt` throws `AgentBusyError` when already streaming
(`pi-agent-core@16.5.2/src/agent.ts:985-986`), so at most ONE un-identified
`invoke_agent` is live at a time.

### The three inbound shapes and their honest topology (Decision 1)

The turn boundary lives in `packages/compass-agent/src/agent.ts`, with three
distinct shapes:

1. **Idle steer — 1:1, PARENT.** `steer()` computes idleness and starts a new
   turn via `this.#session.agent.prompt(content)` (`agent.ts:417,474`),
   synchronously from the idle gate. Exactly one message, one new turn: the
   message's remote context becomes `invoke_agent`'s parent.
2. **Idle deliver flush — N:1, PARENT iff N equals 1, LINKS otherwise.**
   `#flushDelivers()` coalesces the whole `#deliverQueue` into ONE prompt
   (`agent.ts:540-550` — `this.#session.agent.prompt(input)`). A span has one
   parent and many links: a single-message batch parents; a multi-message
   batch links every message's context onto the turn (each link's attributes
   carry its `compass.message.id`).
3. **Mid-turn steer — no new turn, LINK on the LIVE span.**
   `this.#session.agent.steer(agentMsg)` injects into the already-running
   loop (`agent.ts:418-433`); the `invoke_agent` span already exists and is
   already parented. The bridge adds a link to the captured live span.

**Deliberate scope boundary — the raw `#applyControl` paths.** Two further
wire-driven injection paths exist and are scoped OUT: `case "prompt"` →
`await this.#session.agent.prompt(control.input)` (`agent.ts:618`) and
`case "steer"` → `this.#session.agent.steer(control.message)`
(`agent.ts:630`), which bypass `CompassAgent.steer()`/`deliver()` entirely.
The boundary is principled, not an omission: `from_handle` today threads
ONLY through the channel steer/deliver path (`control-source.ts:414` —
`immediate.steer(msg, wire.control.value.fromHandle)`); the raw
`control:prompt`/`control:steer` cases carry no from_handle denorm either.
Under OQ1 = (b), traceparent rides the exact same denormalized-control
fields as from_handle, so its scope is identical by construction: the
channel-message injection path (idle steer / idle deliver-flush / mid-turn
steer). The raw control paths are a distinct lower-level injection surface;
they gain continuity if and when they gain a from_handle-style denorm, under
their own record.

**Hybrid (parent-when-1:1, link-otherwise) over links-uniform — weighed:**
links-uniform buys one code path and uniform query semantics, but forfeits
the single connected end-to-end trace (server → turn → tool calls) in the
MAJORITY case — most trace viewers render parentage as a tree and links as a
footnote, so uniform links demote the headline deliverable to a click-through
everywhere, to spare two extra branches the agent already distinguishes
today (`#flushDelivers` vs the two `steer()` arms are already separate code
paths with separate acks). Hybrid states the true relationship in each case:
parentage where causality is singular, links where it genuinely is many-to-one
or after-the-fact. Uniformity for queries is restored by ATTRIBUTES, not
topology: every turn span gets `compass.message.ids` stamped regardless of
case (Decision 3), so "which messages fed this turn" never depends on
topology. Reversible cheaply (agent-local; no wire shape depends on it) —
recommended, not load-bearing (OQ2).

### Where trace context originates (Decision 2 — THE fork, OQ1)

**(a) Agent-side minting:** at injection the agent starts a local
`compass.message.injection` span (or synthesizes a context) and parents/links
the turn to it. No wire change, ships entirely in this lane — but the trace
ROOT is agent-local: the server's own span for that message's creation,
routing, and dispatch is a DIFFERENT trace, so "end-to-end" stops at the
container boundary. The `SessionInjection` observation frame
(`agent.ts:225-233`, `mapping.ts:256-268`) already records
(opKind, messageId, fromHandle) at injection time; an agent-local span adds a
span-shaped duplicate of that record and little else. The minted
`compass.message.injection` span is itself a local trace ROOT (or, if a
`SessionInjection` observation span exists, parented to it) — making explicit
that under (a) the trace begins at the agent, the same limitation this option
already concedes.

**(b) Server-side stamping — RECOMMENDED:** the server serializes its current
span context as a W3C `traceparent` string onto the inbound control, exactly
as it already denormalizes the author handle — `SteerControl.from_handle`,
"resolved … server-side (RIG-2486 T1)" is the shipped precedent
(`packages/compass-agent/src/gen/compass/v1/agent_pb.ts:379-388`:
`fromHandle: string` field 2; threading at
`src/transport/control-source.ts:410-414` —
`immediate.steer(msg, wire.control.value.fromHandle)`). The turn then joins
the SERVER's trace for that message: creation → routing → delivery → turn →
tool calls, one connected trace. Cost: a cross-lane wire contract
(compass-server stamps; the `AgentControl` wire is its lane), gated on Matt's
ruling.

**(c) Attribute-only correlation — REJECTED, noted for completeness:** the
turn already gets `compass.message.ids` stamped (Decision 1), and the
server's own spans carry the same message id, so "which server trace produced
this turn" is answerable today as a backend attribute JOIN with ZERO wire
change. (c) dominates (a) — the same trace-disconnection for less machinery —
but not (b): correlation stays a query, not a parent/link edge, so no trace
viewer renders a single connected message→turn→tool-calls trace. Named so the
fork shows its full option space.

The agent-side TOPOLOGY machinery — the `onSpanStart` capture hook and its
subagent filter, the single `capturedInvokeAgent` slot, `linkActiveTurn`'s
`addLink` target, the `context.with` parent-wrapping, and the three-shapes
threading through the injection sites — is IDENTICAL under either ruling; only
the SOURCE of the remote context differs. Under (b) that source is a
wire-carried `traceparent` STRING arriving at `steer`/`deliver`, so those
methods gain a `traceparent: string` arg (T2). Under (a) there is NO string at
the boundary: the context is minted INSIDE the bridge at injection (T3′), so
`steer`/`deliver` would take an internal mint seam, not a string. So the
topology core lands first inert under EITHER ruling, but the `traceparent:
string` signature on `steer`/`deliver`/`parseTraceparent` is (b)-shaped and
FINALIZES only once Matt rules OQ1 — under (a) it is replaced by the T3′ mint
seam. The ruling gates T3/T4 and this one signature detail, not the topology
core. This record recommends (b): (a) delivers a trace that begins at the agent
for a directive that asks for message-to-turn continuity, and the precedent
(from_handle) shows the wire change is routine.

### Carriage (Decision 3)

- **Wire field:** `string traceparent` appended to BOTH `SteerControl` (next
  free field, 3) and `DeliverControl`, W3C `traceparent` header format
  (`00-<trace-id>-<span-id>-<flags>`). Additive proto3 field; empty string
  when the server has no active span (mirrors the from_handle store-miss
  posture: "Empty when the Server could not resolve …",
  `control-source.ts:168-176`). Proto3 field presence makes absent and empty
  indistinguishable — intended; empty means "no context, stamp nothing".
- **Fence-safe by construction — with the hole the split closes.**
  `CompassAgentOptions` is a PACKAGE export
  (`packages/compass-agent/src/index.ts:12` — `export { CompassAgent, type
  CompassAgentOptions } from "./agent";`), and the fence test does NOT watch
  it: `export-surface.test.ts` roots only under `src/transport/` and excludes
  CompassAgent — so an OTel-typed option would be a SILENT containment hole,
  not a caught one. Hence the T1 split: the agent receives only the narrow
  `TurnTracer` (two string-only methods, zero OTel types); the hook members
  (`Span`/`TelemetryHookContext`-typed) stay on the full bridge object held
  by `cli.ts` alone. On the wire side the context crosses as a plain string
  through the existing `ImmediateControl` callback shape
  (`control-source.ts:168-176`), widened from
  `steer(msg: Message, fromHandle: string)` to carry `traceparent: string`.
  Parsing string → `SpanContext` happens inside the bridge module, outside
  the transport, and the fence holds by construction even where the fence
  test does not look.
- **Parsing:** a ~15-line first-party W3C parser (fixed grammar, hex fields)
  in the bridge module. NOT `propagation.extract()` — the global propagator
  is a no-op unless something registers one, and registering one is exactly
  the kind of global side effect the off-by-default constraint bans.
- **Observation symmetry:** the `SessionInjection` frame optionally gains the
  same `traceparent` string so the server can join its observation record to
  the trace — additive, folded into T4, not load-bearing (OQ3).

### Off-by-default parity (Decision 4)

The bridge exists ONLY on the sibling record's enabled path: `cli.ts` builds
it beside the gated `telemetry: {}` (sibling T1) and passes the agent its
narrow `TurnTracer` facet; when telemetry is off the agent receives
`undefined` and every call site no-ops. No span context is parsed, no
`context.with` wrapper runs, no hook is installed — black-box behavior with
no endpoint stays bit-identical, the same guarantee the sibling record pins.
`cli.ts` composes the hooks into the telemetry option
(`{} → { onSpanStart: bridge.onSpanStart, onSpanEnd: bridge.onSpanEnd }`) on
the enabled path only; `Agent.setTelemetry` exists as a post-construction
seam if wiring order ever needs it (`pi-agent-core@16.5.2/src/agent.ts:560-562`).
This wiring PRESUPPOSES the sibling record's T1 gated block, which does not
exist in `cli.ts` today (sibling T1 is a separate in-flight PR): no gated
block ⇒ nowhere to install the hooks and no registered context manager for
parentage — sibling-T1-first is a hard ordering dependency (restated in the
Plan).

## Global Constraints

- **No OTel type on any EXPORTED surface — transport fence AND package
  barrel.** Trace context crosses the transport and the wire as a W3C
  `traceparent` STRING only; `export-surface.test.ts` stays green unmodified.
  The exported `CompassAgentOptions` (`index.ts:12`) carries only the
  OTel-type-free `TurnTracer`; the `Span`-typed hook members live on the full
  bridge held by `cli.ts`, which the fence test's blind spot (roots under
  `src/transport/` only) makes load-bearing rather than redundant.
- **Off by default (inherited, hard).** No OTLP endpoint ⇒ no bridge, no
  hook, no context parsing, zero behavior delta; existing suites stay green
  unmodified. Same predicate as the sibling record's activation gate.
- **No new dependencies; no `bun.lock` change; no FOD bump.**
  `@opentelemetry/api` is already direct
  (`packages/compass-agent/package.json:22`); the W3C parser is first-party;
  test spans use the house `InMemorySpanExporter` pattern
  (`src/transport/outbound-spans.test.ts`).
- **Sibling record stays frozen; OQ1(b) not reopened.** Loop spans stay on
  the global provider; the transport keeps its scoped provider. The wire
  string is provider-independent, so this composes with (b) untouched.
- **Lane ownership is explicit per task.** Agent-side (bridge, CompassAgent
  threading, control-source decode) = compass-agent lane. The proto field +
  server-side stamping = compass-server lane, a cross-lane contract like
  from_handle (RIG-2486 T1).
- **Scope boundary:** continuity covers the channel-message injection path
  only (idle steer / idle deliver-flush / mid-turn steer). The raw
  `#applyControl` `control:prompt` (`agent.ts:618`) / `control:steer`
  (`agent.ts:630`) paths are deliberately out of scope — they carry no
  from_handle denorm today and would gain continuity via their own
  from_handle-style denorm under a separate record (Decision 1).
- **Wire additions are additive proto3 fields** (buf-breaking-safe), empty
  string = no context.
- Commit identity: mintaka author + Matt co-author trailer; code comments
  cite this record by path
  (`docs/designs/platform/compass-agent-message-trace-continuity/design.md`),
  never an issue id.
- **Ledger note (driver flips it, never this record):** target is
  `docs/designs/platform/DECISIONS.md`, which does NOT exist at drafting time
  (verified: no such file under `docs/designs/platform/`); the sibling
  record's OQ3 precedent ruled "no ledger entry for platform records; the
  record is the ruling". If the platform ledger has since been created, the
  rows are: (1) message→turn continuity topology (hybrid parent/links);
  (2) the OQ1 origination ruling; (3) traceparent-as-plain-string across the
  fence. Otherwise no ledger delta.

## Plan

T1 (the bridge module) and T2's TOPOLOGY core — the `onSpanStart` capture +
subagent filter, the `capturedInvokeAgent` slot, `linkActiveTurn`/`addLink`,
the `context.with` wrapping, the three-shapes threading, and the
`compass.message.ids` stamping — are agent-lane and ruling-independent (the
machinery is identical under either OQ1 answer). They land first, inert without
a context source when telemetry is off and self-contained when on. The one part
of T2 that carried the OQ1 dependency is the `traceparent: string` argument on
`steer`/`deliver`/`parseTraceparent`: that shape assumes (b), a wire-carried
string. With OQ1 ruled (b), the signature is FINAL — no T3′ mint-seam
contingency remains — so T2 lands its topology core and its settled string
signature together. Both T1 and T2 DEPEND on the sibling record's T1 (the gated
`telemetry: {}` block in `cli.ts`, a separate in-flight PR) merging first: the
bridge installs its hooks INTO that gated option and needs its registered
context manager for parentage — no gated block, nowhere to install. T3 (wire
decode, agent lane) and T4 (proto + server stamping, SERVER lane) are UNBLOCKED
by the OQ1 = (b) ruling and sequence as a stacked pair — T4's proto lands first,
T3 regenerates against it. Each task is its own PR gated by the package
suite (`bun test` from `packages/compass-agent/`) plus its own additions.

### T1 — Trace-continuity bridge module (agent lane)

New `packages/compass-agent/src/trace-bridge.ts` (compass-agent–owned, NOT
under `src/transport/`): parse + topology + live-span capture in one place.
The bridge is ONE object internally, but its type is SPLIT by consumer
(Decision 3's fence hole): the agent sees only the narrow, OTel-type-free
`TurnTracer`; `cli.ts` alone holds the full bridge with the `Span`-typed hook
members.

- `parseTraceparent(header: string): SpanContext | undefined` — first-party
  W3C parser (version `00`, 32-hex trace-id, 16-hex span-id, 2-hex flags;
  reject all-zero ids). Returns `undefined` on malformed/empty input — a bad
  header NEVER fails an injection.
- `createTraceBridge(): TraceBridge` — one internal object with all four
  members:
  - `runWithParent<T>(traceparent: string, fn: () => T): T` — parses; wraps
    `fn` in `context.with(trace.setSpan(context.active(),
    trace.wrapSpanContext(spanContext)), fn)`; runs `fn` bare on parse
    failure. Used around the two synchronous `prompt()` call sites.
  - `linkActiveTurn(traceparent: string, messageId: string): void` — parses;
    `capturedInvokeAgent?.addLink({ context: spanContext, attributes:
    { "compass.message.id": messageId } })`; no-op if no live turn span or
    parse failure.
  - `stampActiveTurn(messageIds: string): void` —
    `capturedInvokeAgent?.setAttribute("compass.message.ids", messageIds)`;
    stamps the query key DIRECTLY on the captured span (the hook's
    `TelemetryHookContext` carries no message ids, `telemetry.ts:306-309`);
    no-op if no live turn span.
  - `onSpanStart(ctx: TelemetryHookContext): void` — captures `ctx.span` only
    when `ctx.kind === "invoke_agent"` AND `ctx.agent === undefined` (the
    subagent filter: every task-subagent loop runs with a non-undefined
    `agent` identity, `executor.ts:2409-2417`, while the main turn's
    `telemetry: {}` leaves it undefined — and `AgentBusyError` on concurrent
    `prompt`, `agent.ts:985-986`, guarantees at most one un-identified
    invoke_agent live at a time, so the single slot is correct). Cleared on
    the matching `onSpanEnd` under the same filter. Installed by `cli.ts`,
    never reachable through an agent-facing type.

Tests (in-process, house pattern `src/transport/outbound-spans.test.ts` —
`InMemorySpanExporter` + `SimpleSpanProcessor` via
`trace.setGlobalTracerProvider`, `trace.disable()` in teardown):

- parser: valid header round-trips trace/span id; malformed, empty, all-zero,
  wrong-version inputs ⇒ `undefined`.
- `runWithParent`: a span started inside `fn` has the remote context as
  parent; parse-failure path still runs `fn` and starts a root span.
- `linkActiveTurn`: with a captured invoke_agent span, the exported span
  carries the link + `compass.message.id` attribute; without one, no throw.
- subagent filter: an `onSpanStart` with `ctx.agent` set does NOT overwrite
  the captured main-turn span; a subsequent `linkActiveTurn` still links the
  MAIN turn.

Interfaces:

- Consumes: `@opentelemetry/api` (`context`, `trace`, `Span`, `SpanContext`,
  `Link`) — already a direct dep (`package.json:22`);
  `TelemetryHookContext { span: Span; kind: TelemetrySpanKind }`
  (`pi-agent-core@16.5.2/src/telemetry.ts:306-309,295,177`).
- Produces: `parseTraceparent(header: string): SpanContext | undefined`;
  `interface TurnTracer { runWithParent<T>(traceparent: string,
  fn: () => T): T; linkActiveTurn(traceparent: string, messageId: string):
  void; stampActiveTurn(messageIds: string): void; }` — the agent-facing type,
  ZERO OTel types;
  `createTraceBridge(): TraceBridge` where `interface TraceBridge extends
  TurnTracer { onSpanStart(ctx: TelemetryHookContext): void;
  onSpanEnd(ctx: TelemetryHookContext): void; }` — cli.ts-facing only. Only
  `TurnTracer` ever appears on an exported signature (via
  `CompassAgentOptions`, `index.ts:12`); the hook members stay off every
  CompassAgent/transport export, so the fence holds by construction.

### T2 — Thread the tracer through CompassAgent's three injection shapes (agent lane)

`packages/compass-agent/src/agent.ts` + the `cli.ts` composition:

- `CompassAgent` accepts an optional `tracer?: TurnTracer` (constructor
  option beside the existing sink/mapper deps) — the NARROW type only, so the
  exported `CompassAgentOptions` (`index.ts:12`) stays OTel-type-free;
  `undefined` (telemetry off) ⇒ every call below no-ops via optional
  chaining.
- `steer(msg, fromHandle, traceparent = "")` / `deliver(msg, fromHandle,
  traceparent = "")` gain a third string arg, defaulted empty —
  source-compatible with existing callers/tests.
- Idle steer (`agent.ts:474`): wrap the `prompt(content)` call in
  `tracer.runWithParent(traceparent, …)` — case-1 parentage.
- Mid-turn steer (`agent.ts:433`): `tracer.linkActiveTurn(traceparent,
  msg.id)` beside `session.agent.steer(agentMsg)` — case-3 link.
- Deliver: stash traceparent per message id beside `#deliverFromHandles`
  (`agent.ts:115` pattern); in `#flushDelivers` (`agent.ts:540-550`), if the
  batch is a single message wrap `prompt(input)` in `runWithParent`,
  else run bare and `linkActiveTurn` each queued context onto the new turn
  span once it starts (capture order: the hook fires synchronously inside
  `prompt()`, so links attach on the next microtask alongside the acks).
- Every case stamps `compass.message.ids` (comma-joined) DIRECTLY on the
  captured `invoke_agent` span (`stampActiveTurn`, a `setAttribute` on the slot,
  NOT through `onSpanStart` — the hook context carries no message ids) — the
  topology-independent query key.
- `cli.ts` (sibling T1's gated block — sibling-T1-first is a hard ordering
  dependency, see the Plan preamble): the enabled path builds the full
  bridge, installs `telemetry: { onSpanStart: bridge.onSpanStart, onSpanEnd:
  bridge.onSpanEnd }`, and passes the bridge AS `TurnTracer` to
  `CompassAgent`; the disabled path passes neither (bit-identical off). The
  hooks live entirely in the cli.ts composition + the bridge module.

Tests: extend `agent.test.ts` harness — idle steer with traceparent ⇒
exported invoke_agent has remote parent; N=1 flush ⇒ parent; N=2 flush ⇒ two
links with per-message ids; mid-turn steer ⇒ link on the LIVE span; empty
traceparent ⇒ no parent/link and no throw; tracer absent ⇒ identical frames
to today (bit-identical off). The idle-steer-parent test doubles as the
CANARY for the SDK synchronicity property (Approach caveats): an SDK bump
that inserts an await before `startInvokeAgentSpan` reddens it.

Interfaces:

- Consumes: `TurnTracer` + `TraceBridge` (T1); the injection sites
  (`agent.ts:433,474,540-550`); the sibling record's gated activation block
  in `cli.ts` `main()` (MUST be merged first).
- Produces: `CompassAgentOptions.tracer?: TurnTracer` (string-only, no OTel
  types on the export); `CompassAgent.steer(msg: Message, fromHandle?:
  string, traceparent?: string): void` and `deliver(…)` likewise —
  string-only widening, fence-clean. The `traceparent: string` arg is FINAL
  under the OQ1 = (b) ruling (a wire-carried string arriving at
  `steer`/`deliver`); no T3′ agent-local mint-seam contingency remains.

### T3 — Decode `traceparent` off the wire control (agent lane; OQ1 ruled (b))

`packages/compass-agent/src/transport/control-source.ts` +
`src/gen` regeneration once T4's proto field exists:

- Widen `ImmediateControl` (`control-source.ts:168-176`) to
  `steer(msg: Message, fromHandle: string, traceparent: string): void` (and
  deliver) — plain strings, fence intact.
- Dispatch site (`control-source.ts:410-414`): pass
  `wire.control.value.traceparent` through, mirroring the from_handle
  threading verbatim.
- `cli.ts` immediate handlers (`cli.ts:865-868`) forward the third arg.

Tests: mirror the RIG-2486 from_handle threading tests
(`control-source.test.ts:122-148,409-412`) — a populated steer/deliver op
carrying `traceparent` reaches the immediate handler with the value; absent
field ⇒ empty string.

Interfaces:

- Consumes: regenerated `SteerControl`/`DeliverControl` with
  `traceparent: string` (T4).
- Produces: `ImmediateControl` with the widened string signatures; no other
  export change.

### T4 — Wire field + server-side stamping (compass-server lane; CROSS-LANE, OQ1 ruled (b))

Owned by compass-server, exactly like the from_handle denorm (RIG-2486 T1):

- Proto: `string traceparent` added to `SteerControl` (field 3) and
  `DeliverControl` (next free field) in `proto/compass/v1/agent.proto`;
  additive, buf-breaking-safe; regenerate both language stacks.
- Server: when wrapping the `AgentControl` for dispatch, serialize the
  current span context (Go OTel `propagation.TraceContext` inject or manual
  format) into the field; empty when no span is active — never block or fail
  a delivery on trace machinery.
- Optional observation symmetry (OQ3): `SessionInjection` gains the same
  string so server-side consumers can join the observation to the trace.
- **Downstream Go instrumentation:** the server-side spans this leg parents
  off are the subject of compass-server's RIG-2685 (instrument
  `CommsService.PostMessage` as trace root + delivery consumer + gateway
  control + gRPC context propagation). Under this (b) ruling RIG-2685's
  gRPC-propagation leg EXTRACTS+CONTINUES the traceparent this task stamps
  rather than minting a fresh root at `PostMessage` — the server becomes the
  trace origin and RIG-2685 is a continuation. Coordinated with compass-server
  (owns RIG-2685); no dependency in the other direction (T4 stamps whatever
  context is active, root or continuation).

Interfaces:

- Consumes: the server's existing control-wrapping path (where from_handle
  is resolved today).
- Produces: the wire contract T3 decodes. Coordinated as a stacked pair:
  T4's proto lands first, T3 regenerates against it.

## Tasks

- [x] OQ1 RULED by Matt (2026-08-27) — origination = (b) server-side
  `traceparent` stamping. T3/T4 unblocked and (b)-shaped; the (a)/T3′
  contingency is closed.
- [ ] T1 — `trace-bridge.ts`: W3C parser, `runWithParent`, `linkActiveTurn`,
  invoke_agent capture hook with the `ctx.agent === undefined` subagent
  filter; `TurnTracer` (agent-facing, OTel-type-free) split from the full
  `TraceBridge` (cli.ts-facing); in-memory span tests incl. the
  subagent-no-clobber case (agent lane)
- [ ] T2 — thread `TurnTracer` through steer/deliver/flush; hybrid parent/link
  topology; `compass.message.ids` attribute; gated wiring in `cli.ts` (hooks
  installed by cli.ts into the telemetry option; DEPENDS on sibling-record T1
  merging first); bit-identical-off tests + the synchronicity canary. The
  `traceparent: string` arg on steer/deliver/parseTraceparent is (b)-shaped and
  FINALIZES on the OQ1 ruling — provisional until then (agent lane)
- [ ] T3 — decode `traceparent` off `SteerControl`/`DeliverControl` through
  `ImmediateControl` to the agent (agent lane; after T4's proto)
- [ ] T4 — proto field + server-side stamping at control-wrap time
  (compass-server lane; cross-lane dependency, stacked under T3)

## Open Questions

1. **LOAD-BEARING — where does trace context originate?** (a) agent-side
   minting: no wire change, single-lane, but the trace roots at the agent —
   the server's message-creation/routing spans stay a disconnected trace, so
   "message → turn" continuity is only as deep as the container. (b)
   server-side `traceparent` stamping on the inbound control: true
   end-to-end continuity (server message handling → delivery → turn → tool
   calls in ONE trace), at the cost of a cross-lane wire contract with
   compass-server. A third option, (c) attribute-only correlation (backend
   JOIN on `compass.message.ids`, zero wire change), dominates (a) but not
   (b) — correlation stays a query, not a trace edge (Decision 2).
   **Recommendation: (b).** The directive is explicitly about tracing
   MESSAGES to turns; (a) cannot show the message's server half by
   construction, (c) never yields a single connected trace, and the
   from_handle precedent (RIG-2486 T1) shows this exact
   denorm-onto-the-control move is routine and additive. The agent machinery
   (T1/T2) is identical either way, so the ruling gates only T3/T4. **The
   ruling also sets the scope of compass-server's RIG-2685** (Go OTel message
   tracing: `PostMessage` root + delivery consumer + gateway control + gRPC
   propagation): under (b) RIG-2685's propagation leg EXTRACTS+CONTINUES this
   record's stamped traceparent (server = origin, RIG-2685 = continuation);
   under (a) RIG-2685 mints server-side and the agent continues; under (c)
   RIG-2685 is unaffected. **RULED by Matt (2026-08-27): (b).** T3/T4 are
   unblocked; compass-server's RIG-2685 continues this record's stamped
   traceparent (server = origin).
2. **Topology: hybrid (parent when 1:1-idle, links otherwise) vs
   links-uniform?** Weighed in Decision 1. **Recommendation: hybrid** — the
   majority case earns a real connected trace; attributes
   (`compass.message.ids`) carry query uniformity; the split costs two
   branches on code paths that are already distinct. Not load-bearing:
   agent-local, reversible without touching any wire shape. **RULED: hybrid.**
3. **Should `SessionInjection` also carry the traceparent string?** Additive
   observation symmetry so the server can join its injection record to the
   trace. **Recommendation: yes, folded into T4** (it is the same proto
   surface and lane). Not load-bearing. **RULED: yes.**
4. **Outbound symmetry — linking a turn to its PUBLISHED frames.** Explicitly
   OUT OF SCOPE, restating the sibling record's deferral: the transport's
   `publish.batch` aggregates frames from many turns, so it is a
   links-through-the-frame-sink problem on the OUTBOUND side
   (`../compass-agent-loop-otel/design.md:295-299`). This record covers
   inbound only; outbound gets its own record if/when Matt asks for it. Noted
   so the deferral is a decision, not an omission. **CONFIRMED out of scope.**
