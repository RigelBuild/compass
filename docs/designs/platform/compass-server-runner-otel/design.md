# Design: compass-server + compass-runner OTel emission and end-to-end trace continuity (T4b, RIG-2685)

Sibling record of the frozen observability-architecture design
([`../compass-observability-architecture/design.md`](../compass-observability-architecture/design.md),
§T4b at `design.md:635-660`), which permits T4b "in a sibling doc under this
design if it grows large" (`design.md:641-643`). Matt ratified the scope
2026-08-26: "otel thru all" + "full end to end tracing thru the system"
(`design.md:641-646`).

Ledger-impact: none — this is a `docs/designs/platform/` record; the
design-ledger gate governs the `GOVERNED_ROOTS` buckets only
(`tools/design-ledger-gate/index.ts:52-60`: `ui, agent, server, meta, infra,
repo, product`), and `platform` is not among them.

## Problem / Intent

Only the agent emits OTel today: `go/` has zero OTel code and `go/go.mod` has
zero `go.opentelemetry.io` dependencies (grep-verified this session), so the T4
bundled fan-in collector (PR #672) has a single producer and a user turn is
observable only inside the agent process. T4b gives both Go binaries
(compass-server, compass-runner) OTel SDK trace AND metric emission to that
collector, and propagates W3C `traceparent` context across the
server → runner → agent process boundaries against the frozen #649 contract —
so one user turn is ONE connected trace that TERMINATES at the turn boundary
(replies branch into linked new traces, never one unbounded trace).

## Approach

Two legs, one record. **Emission** establishes the (currently nonexistent) Go
OTel convention as a single new bootstrap package; **propagation** stamps the
server's active span context onto the steer/deliver control ops as a W3C
`traceparent` string, relayed verbatim by the runner and continued by the
agent (compass-agent #649, frozen at f468431e).

### Emission: one bootstrap package, `go/internal/otel`

A new `go/internal/otel` package — a flat one-concern-per-dir sibling of
`auth`/`comms`/`delivery`/`appconfig`, cleared with compass-server (no
file-zone collision). It is the ONLY place a `TracerProvider` is constructed;
both binaries wire it identically, so the repo keeps exactly one
tracer/exporter convention.

- **Exporter:** OTLP/http-protobuf
  (`go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp`) —
  matches the T4 bundled collector and the agent's posture
  (`docs/designs/repo/compass-agent-effect-otel/otel-endpoint-deployer-contract.md:34-38`:
  endpoint set ⇒ "OTLP-over-HTTP/protobuf export to that collector").
- **Off by default, endpoint-gated:** when `OTEL_EXPORTER_OTLP_ENDPOINT` is
  unset, `SetupTracerProvider` installs NO provider and returns a no-op
  shutdown — zero overhead, no egress, mirroring the agent's
  `otel-layer.ts` (`otel-endpoint-deployer-contract.md:36-38`). This is also
  the codebase config idiom (empty flag/env = feature off).
- **Config style:** flag + env fallback via the existing helpers —
  `firstNonEmpty` (server, `go/cmd/compass-server/main.go:451`) / `orEnv`
  (runner, `go/cmd/compass-runner/main.go:211-216`) — NOT the client-app TOML
  (`appconfig` is the client app only). The endpoint knob is the standard
  `OTEL_EXPORTER_OTLP_ENDPOINT` env var (optionally mirrored by an
  `--otel-endpoint` flag with the env as fallback, matching every other knob).
- **Resource attributes:** `service.name` = `compass-server` /
  `compass-runner` (binary names, distinct from the agent's `compass-agent`);
  `service.version` from each main's ldflags `var version`
  (`compass-server/main.go:28`, `compass-runner/main.go:31`), passed through
  the config struct because the otel package cannot import `main`.
  `compass.session.id` stays the cross-surface join key, carried as a SPAN
  attribute on the message-path spans (server/runner spans are not
  per-session processes, so it cannot be a process resource attribute here).
  `deployment.environment` is OMITTED — there is no environment concept in
  the codebase today and this record does not introduce one.
- **Wire point:** in each `run()`, immediately after
  `slog.SetDefault(...)` (server `main.go:75`, runner `main.go:88`) and
  BEFORE `server.Serve` (`main.go:94`) / `runner.Run` (`main.go:161`),
  passing the `signal.NotifyContext` ctx each `run()` already creates
  (server `main.go:85`, runner `main.go:158`) — the process-root
  `context.Background()` inside those two `run()`s is `main`'s exemption
  under the go-thread-context rule; the otel package itself NEVER re-roots.
  The shutdown func is deferred right there, before the drain, and flushes
  under a bounded timeout derived from the caller's ctx (mirroring the
  agent's 2s shutdown bound).
- **RPC spans via otelconnect — on the handlers that carry a delivery
  origin.** `connectrpc.com/otelconnect`'s interceptor produces the server
  RPC span, but it must be mounted on the CommsService handler, which serves
  `PostMessage`/`RespondToAsk` — the origin RPCs. That handler is built TODAY
  with NO interceptor options (`go/server/serve.go:458`
  `NewCommsServiceHandler(commsSvc)`), and the same handler value is reused on
  both the shipped socket door (`serve.go:634`) and the dev door
  (`serve.go:671`); on the network door it is `netCommsHandler`
  (`network_door.go:269`), built off the shared bearer/admin chain
  (`network_door.go:263-267`, applied to both `netHandler` and
  `netCommsHandler`). So T2 adds otelconnect (+ the traceresponse interceptor,
  below) to the CommsService construction at `serve.go:458` for the socket/dev
  doors and prepends it to the `network_door.go:263-267` chain for the network
  door. It ALSO goes on the CompassService chains (`serve.go:626` socket,
  `:662` dev; the network CompassService rides the same `:263-267` chain) so
  the RPCs the UI calls (`SubscribeEvents`, etc.) carry the traceresponse
  header. The Secrets chains (`serve.go:631`/`:668`) carry no delivery origin
  and need no origin span. The runner's outbound connect client gets the same
  interceptor for the enroll/Sessions dials. Interceptor ORDER: otelconnect
  goes FIRST (outermost) in each chain so the security-critical
  AdminGate→Ambient ordering (`serve.go:642-658`) is unchanged relative to
  itself.
- **Metrics (Matt ruled OQ1 = traces + metrics).** The same otelconnect
  interceptor emits RPC duration/count metrics once a `MeterProvider` exists, so
  the metrics leg is the same wiring plus a second provider — no
  re-instrumentation. `go/internal/otel` gains `SetupMeterProvider` (OTLP/http
  metrics exporter, same endpoint gate as the tracer). Beyond the free RPC
  metrics, T5 adds ONE hand-instrumented delivery counter keyed by op-kind
  (steer/deliver) only. Cardinality is bounded by hard rule: op-kind labels
  ONLY — NEVER per-session or per-channel labels (see §Global Constraints).
- **Build tags:** both mains are `//go:build unix`; `go/internal/otel` builds
  cross-platform (the OTel SDK pulls nothing unix-only), so it carries no tag.

**Dependency / image impact (stated per the brief):** this adds
`go.opentelemetry.io/otel`, `.../sdk`, `.../sdk/metric`,
`.../exporters/otlp/otlptrace/otlptracehttp`,
`.../exporters/otlp/otlpmetric/otlpmetrichttp`,
and `connectrpc.com/otelconnect` to `go/go.mod` (today: zero OTel deps), at an
OTel Go SDK floor **≥ v1.23.0** (the version that adds `Span.AddLink`, required
by the T5 move-6 cross-turn Link). New Go
module deps change the server/runner container images' fixed-output-derivation
(FOD) hash — the same class of impact the agent loop-otel record flagged for
the agent image. **compass-native (the stack/image lane) rebuilds; coordinate
at impl PR time.**

### Propagation: server-origin stamping, runner relay, agent continuation

The #649 contract is FROZEN (merged f468431e): the server is the trace ORIGIN
(OQ1=(b)); it serializes its CURRENT active span context into an additive
proto3 `string traceparent` on the INTERNAL control ops at control-wrap time;
the runner RELAYS the string; the agent CONTINUES off it. Never mint a fresh
root ON THE DELIVERY PATH — a delivered message never resets its trace (a NEW
post, by contrast, IS a new turn and a fresh linked root; see §Trace lifetime
and termination). W3C grammar exactly
(`00-<32hex traceid>-<16hex spanid>-<2hex flags>`); EMPTY string when no
active span — trace machinery never blocks or fails a delivery (mirroring the
`from_handle` store-miss posture, `go/internal/delivery/consumer.go:399-402`).

**Wire seam — four additive proto fields** (verified field numbers, current
source). Matt ruled at the freeze gate (2026-08-27) that the trace crosses onto
the public observation surface AND that a proto change to carry cross-turn
causality is in scope:

- `SteerControl` (`proto/compass/v1/agent.proto:196-206`): `message = 1`,
  `from_handle = 2` → `traceparent` = **field 3** (internal control op).
- `DeliverControl` (`agent.proto:219-236`): `message = 1`, `topic_name = 2`,
  `from_handle = 3` → `traceparent` = **field 4** (internal control op).
- `SessionInjection` (`proto/compass/v1/compass.proto:524-533`; next-free after
  `op_kind = 1`, `message_id = 2`, `from_handle = 3`) → `traceparent` =
  **field 4** (PUBLIC observation surface; Matt ruled OQ2 = yes). A plain
  `string` add does NOT touch the gen-fenced enum (`SEA-1267`,
  `compass.proto:535-539` fences `SessionInjectionKind`, not scalar fields), so
  it is a clean additive public-API field. The agent EMITS it on the injection
  observation from the `traceparent` it decoded (#649 T3), giving public
  session-stream consumers a direct observation→trace join.
- `CommsCallRequest` (`proto/compass/v1/agent_gateway.proto:106-115`;
  `call_id = 1` + the `call` oneof `2-6` on current main) →
  `trigger_traceparent` = **field 10** (INTERNAL agent-gateway leg). NOT
  field 7: the in-flight #628 (RIG-2673, held for the RIG-2751 handle-first
  reshape) already extends this SAME oneof with `create_channel = 7`,
  `update_members = 8`, `create_channel_group = 9`, and proto field numbers are
  message-wide (oneof members and scalar fields share one number space), so 7-9
  are claimed. Field 10 lands clean regardless of #628/RIG-2751 merge order.
  compass-server — the `agent_gateway.proto` file-zone authority (they own #628,
  RIG-2751, and this T4 edit) — RATIFIED field 10 (independently source-verified;
  they sequence #628's conflict-clear and this scalar together in their zone), so
  it is authoritative, not a placeholder. The delivered
  message's traceparent the agent re-attaches on its outbound post, so the
  server can LINK the reply's new trace to the message that triggered it — the
  cross-turn causal edge that keeps traces terminating (see §Trace lifetime and
  termination).

All four are buf-breaking-safe pure adds; `moon run compass-proto:gen`
regenerates both stacks.

**The bus gap — the one genuinely new seam.** The frozen contract requires an
ACTIVE span at control-wrap time, but the wrap site is the delivery consumer
(`deliverOp`/`steerOp`, `consumer.go:374-395`), whose goroutine is rooted on
the serve ctx (`consumer.go:255 Run(ctx)`), not on the PostMessage request
ctx: `PostMessage` publishes onto the in-process events bus
(`comms.go:355-357` → `publishMessagePosted`, `mapping.go:463-468`), and
`Bus.Publish` (`go/events/events.go:165-176`) takes no ctx — the trace
context dies at the bus boundary. Without bridging it, the "origin" span and
the dispatch spans are two disconnected traces.

Bridge it in-process, in the vocabulary already there: thread the caller's
ctx to the bus and stamp the serialized traceparent onto the existing
`Stamped[P]` envelope (`events.go:171-176` already stamps `Seq`/`AtUnixMS`/
`InstanceEpoch`; this adds one string field). Rather than a full signature
cutover of `Publish` — which would ripple ctx into deliberately ctx-free
structural sinks (`LifecycleSink.PublishSessionStatus`, the presence
publisher, `board.Projection`, `publishReady`), see §Alternatives — add a
ctx-carrying `PublishCtx(ctx, payload)` used ONLY by the message-origin
publisher (`publishMessagePosted`, `mapping.go:463-468`). The consumer extracts
the traceparent back into its dispatch ctx in `handleEvent`
(`consumer.go:360-366`) before the wrap; a publisher with no active span
stamps the empty string, read downstream as "no active span" — the contract's
empty-string posture. This is not a new abstraction: it extends the bus's
existing stamped-envelope pattern.

Parenting is by SpanContext (trace-id/span-id/flags), NOT by a live span
object — so a dispatch span is a valid child of the origin's REMOTE span
context even though the PostMessage handler span has already ENDED by the time
the async consumer dispatches (the standard async/queue propagation pattern).
Two consequences to expect: the origin span's duration does not envelope its
async children (some UIs render the parent "shorter than" its subtree —
cosmetic), and the sampled flag rides the traceparent, so a downstream
ParentBased sampler honors the origin's decision (a not-sampled origin
correctly suppresses the whole dispatch subtree).

**Stamp and relay sites (all verified current source):**

- **Origin (human post):** `CommsService.PostMessage` (`comms.go:334-359`) —
  the otelconnect span on the CommsService handler (added by T2 at
  `serve.go:458` / `network_door.go:263-269`, above) IS the origin span; the
  handler adds `message.id` as a span attribute after the append. No
  hand-minted span. `RespondToAsk` (`comms.go:368-397`) rides the SAME
  handler, so its answer-message post gets an origin span from the same
  interceptor.
  *(Agent-authored posts do NOT ride the CommsService RPC — they arrive via
  the unary `RelayCommsCall` and call `PostMessage` in-process; their origin
  is Open Question 5.)*
- **Control-wrap (stamp):** `deliverOp` / `steerOp`
  (`consumer.go:374-395`) gain a `traceparent string` parameter, populated
  from the consumer's (bus-extracted) ctx — the same site where
  `from_handle` is resolved today (`authorHandle`, `consumer.go:403-415`).
- **Classification-hop span:** the consumer's `gatedDispatch`
  (`go/internal/delivery/dispatch.go:343-354`) wraps
  `DispatchControl(ctx, sessionID, op)` (`:350`) in a span with attributes
  `compass.message.id`, `compass.op.kind` (steer/deliver), and
  `compass.session.id` — so a trace filters to one message.
- **Server→runner hop:** `runnerhub.Hub.DispatchControl`
  (`go/internal/runnerhub/dispatch_control.go:38-54`) pushes send-only down
  the long-lived Sessions stream, where per-message header propagation is
  impossible — the traceparent RIDES the op itself, which the runner relays
  verbatim (`go/internal/runner/dispatch.go:457-472` hands
  `c.DeliverControl.GetOp()` to `host.Deliver` at `:469` unmodified;
  `gateway.Send` clones, never rewrites, the op —
  `go/internal/runner/gateway/control.go:100-104`). No runner-side stamping
  code is needed for correctness; the runner leg is spans only.
- **Runner→agent hop span:** the runner's DeliverControl dispatch arm
  (`runner/dispatch.go:457-472`) starts a span parented off the op's
  `traceparent` (extracted, not ctx-inherited — the Sessions stream ctx is
  not the message's trace), same attributes as the server hop. The
  `controlProducer.Send` path (`gateway/control.go:230-232`) is queue-entry,
  not I/O, and `Send(sessionID, op)` carries no ctx; the dispatch-arm span
  (which has the caller's ctx per go-thread-context) is the runner hop of
  record, ending when `host.Deliver` returns "durably queued".
- **Sweep dispatches** (`go/internal/delivery/settle.go:193`, `:244`, and the
  cursor sweep's `DispatchControl` in the same file) have no originating
  request: each sweep pass roots its OWN span (a sweep-rooted trace), and the
  re-wrapped ops carry that sweep span's traceparent — never a stale one from
  the original post. Empty when the consumer has no provider installed.
- **Held agent-authored deliver** (`dispatch.go:81 hold` →
  `settle.go:257 fireHeld`): an agent post whose author session is live is
  HELD until that session settles, then fired on the consumer LOOP ctx (which
  carries no span). The held registry therefore carries the ORIGIN traceparent
  beside the message id (T5 move 5) and restamps it at fire — otherwise the
  agent-reply-fans-out leg, the most interesting half of a turn, silently
  stamps empty. Its non-emptiness depends on Open Question 5 (agent origin).
- **Agent continuation:** compass-agent's T3 (RIG-2871) decodes the field;
  their parser rejects all-zero ids and no-ops on malformed/empty. Their work
  is stacked on the proto field task below.

**Continuity boundary (stated explicitly).** "One user turn is ONE connected
trace" is a LIVE-dispatch guarantee — the live deliver plus the held
agent-reply. Sweep, offline-recipient, pin, owed-mention, and overrun-window
deliveries are fresh roots BY DESIGN: they re-read from the store, which
carries no traceparent, and inventing durable trace storage to reconnect them
to a possibly-dead prior-process trace is scope explosion for negative value.
The held path is the one deferred delivery that stays connected — its
traceparent is in-memory, not re-read from the store.

### Trace lifetime and termination

The ratified goal is "one turn, one trace" — and the corollary Matt named at the
freeze gate: **a trace must TERMINATE.** Agents relay to each other, so naively
continuing the same trace onto every downstream reply (Alternative (c)) yields a
single trace that never ends — agents ping-ponging for days accrue into one
unbounded span tree, un-exportable and un-queryable. Termination here is
structural, not a timeout:

- **A trace spans exactly one turn.** Origin (a human `PostMessage` RPC span, or
  an agent `RelayCommsCall` RPC span under (a′)) → dispatch → runner/agent hop →
  the recipient's turn, which the agent-side continuation (#649 T3) joins. The
  trace ends when that turn settles. There is no cross-turn PARENT edge.
- **A reply is a NEW trace, LINKED — never a child.** When the recipient agent
  posts during its turn, that post is a fresh origin (a′ = fresh server-minted
  root), starting trace N+1. Trace N+1 carries an OTel **span Link** to the
  message that triggered it, expressing "caused by" WITHOUT lifetime nesting.
  Links do not compound: trace N+1 is O(one turn) however deep the causal chain.
- **The causal chain is data, not a span tree.** "Around and around for days" is
  a long chain of linked one-turn traces — each terminating, each independently
  sampled and exported, navigable as a causal graph (query: "what did this
  message transitively trigger?") but never one eternal trace. No artificial hop
  cap is needed for correctness, because nothing accumulates in a single trace; a
  cap would be a query-side product choice, not a plumbing requirement.
- **Consistent with frozen #649.** The fresh root for an agent post is minted
  SERVER-side at its `RelayCommsCall` execution — "the server is origin" reads
  naturally as "the server-side execution of the post is the origin." #649's
  "never mint a fresh root at PostMessage or the agent" governs the DELIVERY
  path (a delivered message never resets its trace); a NEW post is a new turn,
  which is exactly where a new trace begins.

**The link seam.** The trigger's traceparent reaches the origin via
`CommsCallRequest.trigger_traceparent` (field 10, above): the agent re-attaches
the `traceparent` it decoded (#649 T3) onto its outbound post, which rides
`RelayCommsCall` to the server. The server's `RelayCommsCall` origin span (a′)
adds a span **Link** from `trigger_traceparent` — a LINK, never a parent. The
Link is attached to the ALREADY-STARTED otelconnect span via `Span.AddLink`
(OTel Go SDK floor **≥ v1.23.0**, which the fresh `go.mod` deps pull) — NOT at
span creation, because otelconnect owns the span factory and exposes no
link-at-creation hook. Empty `trigger_traceparent` (a human-seeded first turn,
or no active trigger) adds no link, per the never-block posture.

**Fresh-root invariant (load-bearing for termination).** The `RelayCommsCall`
origin span MUST be a fresh root with respect to its trigger. This holds because
causality crosses ONLY via the `trigger_traceparent` proto field + the explicit
`Span.AddLink` — NEVER via transport propagation. The corollary constraint: the
runner's outbound `RelayCommsCall` dial must not run under an active span whose
context W3C-propagates as a `traceparent` REQUEST header, or otelconnect on the
server would PARENT the origin span on it and silently extend the trigger's
trace instead of linking. The runner satisfies this today (it relays the op
verbatim and does not execute under the delivered message's OTel ctx); T3's
outbound client interceptor covers the enroll/Sessions dials, and the
`RelayCommsCall` dial must not inject a delivered-trace header (stated so an
implementer preserves it).

Agent-side attachment is compass-agent's lane (a task beside their #649 T3
decode); the field and the server-side link are this record's.

**Response-boundary trace id (cross-plane, serves #656 T6/RIG-2874).** The UI
has no client-side OTel tracer (browser RUM is a #656 named follow-up), so
the PostHog↔OTel correlation seam needs the server to be the trace-id source
at the compass.v1 RESPONSE boundary. A small unary interceptor in
`go/internal/otel` (appended after otelconnect on the same handlers,
`serve.go:458`/`:626`/`:662` and the `network_door.go:263-269` chain) sets a
`traceresponse` response header
(W3C trace-context response draft grammar, same `00-…` shape) carrying the
handler span's trace id. The UI reads the header off any RPC response and
hands the trace id to PostHog as the correlation key — the id is
server-minted, never client-minted. This is a produced surface of T2 below.

### Alternatives considered

- **Bus bridge via span links instead of ctx threading (INTRA-turn)** — the
  consumer starts an unparented span and LINKS to the origin by re-deriving it
  from the message id. Rejected FOR THE IN-PROCESS BUS HOP: links do not satisfy
  "one connected trace" WITHIN a turn (the ratified goal), and there is no
  stored span context to link to anyway. (Links ARE the right tool for the
  CROSS-turn causal edge, where a parent edge would never terminate — see
  §Trace lifetime and termination. Intra-turn: ctx threading; cross-turn: link.)
- **Traceparent on the public `MessagePosted` wire event** — rejected: the
  bus payload is the public `SubscribeCommsResponse` proto; trace plumbing on
  a public client-facing wire message to solve an in-process hop is scope
  leakage. The `Stamped` envelope is in-process and already exists for
  exactly this kind of stamping.
- **Hand-written RPC middleware instead of otelconnect** — rejected: reuse
  before inventing; otelconnect is the connect-go ecosystem's maintained
  instrumentation and covers client+server, unary+stream.
- **Full `Publish(ctx, payload)` signature cutover across all callers** —
  rejected in favor of the narrower `PublishCtx`. A full cutover ripples a ctx
  parameter into structural sink interfaces
  (`LifecycleSink.PublishSessionStatus` et al., `hub.go:44-51`) whose
  contracts deliberately keep them ctx-free ("must NOT store the caller's
  ctx"), to carry a trace those lifecycle / presence / board events never
  participate in — polluting multiple interfaces for one concern, or forcing
  `context.Background()` at those sites (go-thread-context-forbidden outside
  main/tests). `PublishCtx` confines ctx to the one message-origin publisher
  that actually originates a delivery trace; the two-method bus surface is the
  standard incremental-ctx idiom (cf. `http.NewRequestWithContext`), not a
  competing convention.

## Global Constraints

- **All-Go.** This is Go server/runner code; no new runtime, no sidecars.
- **Off by default, endpoint-gated.** Unset `OTEL_EXPORTER_OTLP_ENDPOINT` ⇒
  no provider installed, no-op API, zero egress (both binaries).
- **Never fail or block a delivery on trace machinery.** Empty `traceparent`
  when no active span; malformed input no-ops agent-side; a span error is
  never a dispatch error.
- **Frozen #649 wire contract.** Field numbers exactly as verified above; W3C
  grammar exactly `00-<32hex>-<16hex>-<2hex>`; server is origin, runner
  relays, agent continues; never mint a fresh root ON THE DELIVERY PATH (a
  delivered message never resets its trace). A NEW post IS a new turn: its
  server-side `RelayCommsCall`/`PostMessage` origin is a fresh root, linked to
  its trigger — never parented on it (§Trace lifetime and termination).
- **ctx threading (go-thread-context).** Every new API accepts the caller's
  `ctx context.Context` first; no `context.Background()`/`TODO()` outside the
  two mains and `_test.go` files. Shutdown timeouts derive from the caller's
  ctx.
- **One convention.** `go/internal/otel` is the only provider-construction
  site; no second tracer/exporter path may appear beside it.
- **Money/`micro-USD` N/A.** No spend events here — that is the T1/T4 usage
  plane, a different record.
- **Proto changes are additive only**, buf-breaking-gate clean; regenerate
  both stacks with `moon run compass-proto:gen`. Scope (Matt ruled at the
  freeze gate): the internal control ops (`agent.proto`), the public
  `SessionInjection` observation (`compass.proto`, OQ2 = yes), and the internal
  `CommsCallRequest.trigger_traceparent` (`agent_gateway.proto`). The public
  `SessionInjection` field is a public-API change — its impl PR carries the
  appropriate ledger row at impl time (this platform record stays
  `Ledger-impact: none`).
- **Metrics cardinality (hard rule).** Delivery/dispatch metric labels are
  op-kind (steer/deliver) ONLY — NEVER per-session, per-channel, or per-account
  labels. Unbounded label sets are a metrics-backend DoS; op-kind is a
  two-value domain.
- **Trace termination.** One trace = one turn. A reply is a fresh server-minted
  root LINKED to its trigger, never a child — so no trace grows without bound
  however long agents relay. A missing/empty `trigger_traceparent` adds no link
  and never blocks the post.
- **Interceptor order.** otelconnect prepends; the AdminGate→AmbientIdentity
  relative order (`serve.go:642-668`) is untouched. otelconnect mints a span
  for a request AdminGate then rejects on the dev/network door — bounded, and
  only when the endpoint is set; acceptable.
- **Agent-post origin ctx.** The agent-authored post path is the unary
  `RelayCommsCall` RPC (`relay_comms.go:250`) → `executeCall`
  (`relay_comms.go:409`) → `PostAsAccount` (`agent_caller.go:131-148`, which
  calls `PostMessage` in-process under `WithActor`) → `publishMessagePosted`.
  Because `RelayCommsCall` is a UNARY RunnerService RPC (not multiplexed on
  the Sessions stream), its handler ctx is per-call — so an otelconnect span
  on the RunnerService handler is a per-post origin, with no stream-lifetime
  span to leak. The constraint is therefore simply that the publish ctx is
  this per-call RPC ctx, never a process/stream root. See Open Question 5.
- **Record hygiene.** Markdownlint-clean; ships as its own PR with
  `Co-authored-by: Matt Wilkinson <matt@rigel.build>` (driver-owned);
  `Ledger-impact: none` (platform bucket is ungoverned,
  `tools/design-ledger-gate/index.ts:52-60`).
- **Image/FOD.** New `go.opentelemetry.io/*` + `connectrpc.com/otelconnect`
  deps change both container images' FOD hash; compass-native rebuilds.

## Cross-lane ownership

- **compass-obs (RIG-2685, this lane)** owns the emission leg (traces + metrics),
  the server-side stamping / runner-relay / message-path-span / cross-turn-link
  LOGIC, and the e2e test (T1, T2, T3, T5, T6 below).
- **compass-server** owns the proto seam (T4 below): `traceparent` on the
  internal control ops (`agent.proto`) AND on the public `SessionInjection`
  observation (`compass.proto`, OQ2 = yes) AND `trigger_traceparent` on the
  internal `CommsCallRequest` (`agent_gateway.proto`) — a stacked-PR seam that
  unblocks compass-agent's decode/emit. The touched Go file zones (`comms.go`,
  `consumer.go`, serve assembly, `go/events`, and — for the (a′) origin span
  and the T5 move-6 causal Link — `go/internal/runnerhub` (`relay_comms.go`,
  `handler.go`)) are compass-server's; emission convention and wiring, INCLUDING
  the runnerhub interceptor + link edits, were cleared with them 2026-08-27 and
  are coordinated at impl time like the `go/events` surface add.
- **compass-agent** owns the agent side: continuation off `traceparent` (#649
  T3), PLUS two additions this record introduces — emitting `traceparent` on the
  public `SessionInjection` observation, and re-attaching the decoded
  `traceparent` as `trigger_traceparent` on outbound posts (the cross-turn link
  source). They regenerate once T4 lands.
- **compass-native** owns the image rebuild the new deps force (trace + metric
  exporters).

## Plan

### T1 — `go/internal/otel` bootstrap package

Owner: compass-obs.

The single Go OTel convention: config resolution, tracer AND meter provider
construction, traceparent serialization helpers, and the trace-response
interceptor. No provider (tracer or meter) when the endpoint is empty. Unit
tests cover: disabled path returns a no-op shutdown and installs no global
tracer/meter provider; enabled path installs providers whose resource carries
`service.name`/`service.version`; `Traceparent(ctx)` returns `""` with no span
and a W3C-grammar string with one; `ContextWithTraceparent` round-trips;
malformed input yields an unchanged ctx.

Interfaces:

- Consumes: `go.opentelemetry.io/otel`, `.../sdk/trace`, `.../sdk/metric`,
  `.../sdk/resource`, `.../exporters/otlp/otlptrace/otlptracehttp`,
  `.../exporters/otlp/otlpmetric/otlpmetrichttp`,
  `go.opentelemetry.io/otel/propagation`; `connectrpc.com/otelconnect`
  (re-exported wiring only).
- Produces (package `otel`, import path
  `github.com/RigelBuild/compass/go/internal/otel`):

  ```go
  // Config carries the per-binary identity and the endpoint gate.
  type Config struct {
      ServiceName    string // "compass-server" | "compass-runner"
      ServiceVersion string // each main's ldflags var version
      Endpoint       string // OTEL_EXPORTER_OTLP_ENDPOINT; empty = disabled
  }

  // SetupTracerProvider installs the global TracerProvider + W3C propagator
  // when cfg.Endpoint is non-empty; otherwise it is a no-op. The returned
  // shutdown flushes and stops the provider, bounded by the ctx it is given
  // (callers derive a timeout from their own ctx — never Background()).
  func SetupTracerProvider(ctx context.Context, cfg Config) (shutdown func(context.Context) error, err error)

  // SetupMeterProvider installs the global MeterProvider (OTLP/http metrics
  // exporter) under the same endpoint gate; a no-op when cfg.Endpoint is empty.
  // With it installed, otelconnect emits RPC duration/count metrics from the
  // same interceptor. Returns a shutdown that flushes and stops the provider.
  func SetupMeterProvider(ctx context.Context, cfg Config) (shutdown func(context.Context) error, err error)

  // Traceparent serializes ctx's active span context to the W3C string
  // ("00-<32hex>-<16hex>-<2hex>"), or "" when there is no valid active span.
  func Traceparent(ctx context.Context) string

  // ContextWithTraceparent returns ctx carrying the remote span context
  // parsed from tp; malformed or empty tp returns ctx unchanged.
  func ContextWithTraceparent(ctx context.Context, tp string) context.Context

  // NewTraceResponseInterceptor sets the "traceresponse" response header
  // from the handler span's context on every unary response (the UI/PostHog
  // trace_id source; see §Response-boundary trace id).
  func NewTraceResponseInterceptor() connect.UnaryInterceptorFunc
  ```

### T2 — Server emission wiring + PostMessage origin span + response trace id

Owner: compass-obs.

Wire the provider into the server binary and put RPC spans on the handlers
that carry a delivery origin. `--otel-endpoint` flag with
`OTEL_EXPORTER_OTLP_ENDPOINT` fallback via `firstNonEmpty`
(`compass-server/main.go:451`), resolved in `buildServeConfig` into
`ServeConfig`; `SetupTracerProvider` AND `SetupMeterProvider` called in `run()`
after `slog.SetDefault` (`main.go:81`) with the `signal.NotifyContext` ctx
(`main.go:91`), both shutdowns deferred before `server.Serve` returns
(`main.go:100`).

The origin span is the linchpin, so the otelconnect interceptor +
`NewTraceResponseInterceptor()` mount on the CommsService handler that serves
`PostMessage`/`RespondToAsk`: it is built with NO options today
(`serve.go:458` `NewCommsServiceHandler(commsSvc)`, reused on socket `:634`
and dev `:671`), so T2 adds a `connect.WithInterceptors(...)` there; on the
network door the CommsService handler rides the shared chain
(`network_door.go:263-269`), so otelconnect prepends to THAT chain. Add the
same interceptors to the CompassService chains (`serve.go:626` socket, `:662`
dev; network CompassService also rides `:263-269`) so the UI's CompassService
RPCs carry the `traceresponse` header. `PostMessage` (`comms.go:334-359`) and
`RespondToAsk` (`comms.go:368-397`) add `compass.message.id` to the handler
span after the append. Test cycle: a serve-level test with an in-memory span
exporter asserts a PostMessage RPC ON THE SHIPPED SOCKET DOOR (not merely a
CompassService procedure) produces a span carrying the message-id attribute,
and that the response carries a `traceresponse` header whose trace id equals
that span's; a disabled-endpoint test asserts zero spans and no header.

The `traceresponse` header must ALSO be added to both CORS builders'
`ExposedHeaders`: `devCORS` (`serve.go:758`, ExposedHeaders at `:763`) and
`networkCORS` (`network_door.go:143`) today expose only
`connectcors.ExposedHeaders()`, so a browser on the cross-origin network door
(exactly where PostHog correlation matters) cannot read a header absent from
`Access-Control-Expose-Headers`; the test asserts the header is READABLE
through the CORS'd door, not merely on the wire.

Interfaces:

- Consumes: T1's `otel.Config`/`SetupTracerProvider`/
  `NewTraceResponseInterceptor`; `otelconnect.NewInterceptor()`;
  `firstNonEmpty` (`main.go:451`).
- Produces:

  ```go
  // ServeConfig gains the endpoint knob (go/server/serve.go).
  type ServeConfig struct {
      // ...existing fields...
      OtelEndpoint string // empty = tracing off
  }
  ```

  plus server RPC spans on all three doors and the `traceresponse` response
  header (the #656 T6/RIG-2874 correlation seam's trace_id source).

### T3 — Runner emission wiring

Owner: compass-obs.

Same shape in the runner binary: `--otel-endpoint` flag with env fallback via
`orEnv` (`compass-runner/main.go:211-216`), `SetupTracerProvider` +
`SetupMeterProvider` in `run()` after `slog.SetDefault` (`main.go:88`) with the
`signal.NotifyContext` ctx (`main.go:158`), both shutdowns deferred before
`runner.Run` (`main.go:161`);
otelconnect interceptor on the runner's outbound connect client(s) so the
enroll/Sessions dials emit client spans. Test cycle: unit test on the flag
resolution; an in-memory-exporter test asserting a client span on an
outbound RPC when enabled and none when disabled.

Interfaces:

- Consumes: T1; `orEnv` (`main.go:211-216`); the runner's connect client
  construction sites in `go/internal/runner`.
- Produces:

  ```go
  // RunnerConfig gains the endpoint knob (go/internal/runner/runner.go).
  type RunnerConfig struct {
      // ...existing fields...
      OtelEndpoint string // empty = tracing off
  }
  ```

### T4 — trace proto fields: control ops, public observation, causal link (stacked seam)

Owner: **compass-server** (proto file zones; coordinated 2026-08-27). Its own
task deliberately: it is the stacked-PR seam compass-agent's decode/emit (T3,
RIG-2871) is blocked on — landing it first, alone, unblocks that lane before any
Go stamping code exists.

Additive proto3 fields, verified against current source. Per Matt's freeze-gate
rulings this spans the internal control ops, the public observation surface
(OQ2 = yes), and the internal agent-gateway leg (the causal-link source):

```proto
// proto/compass/v1/agent.proto — internal control ops
message SteerControl {
  Message message = 1;
  string from_handle = 2;
  // W3C traceparent ("00-<32hex>-<16hex>-<2hex>") of the server span this
  // steer was wrapped under; empty when the server had no active span.
  string traceparent = 3;
}
message DeliverControl {
  Message message = 1;
  string topic_name = 2;
  string from_handle = 3;
  string traceparent = 4;  // same contract as SteerControl.traceparent
}

// proto/compass/v1/compass.proto — PUBLIC observation (OQ2 = yes)
message SessionInjection {
  SessionInjectionKind op_kind = 1;
  string message_id = 2;
  string from_handle = 3;
  // Trace the agent continued for this injection; lets a public
  // session-stream consumer join the observation to its trace. Scalar add —
  // does not touch the SEA-1267 gen-fenced enum.
  string traceparent = 4;
}

// proto/compass/v1/agent_gateway.proto — INTERNAL agent-gateway leg
message CommsCallRequest {
  string call_id = 1;
  // oneof call { ... } occupies 2-6; #628 (held) claims 7-9 in the same oneof,
  // so 10 is the next collision-free scalar slot (ratified by compass-server,
  // the agent_gateway.proto file-zone authority).
  // The delivered message's traceparent the agent re-attaches on an outbound
  // post, so the server links the reply's new trace to its trigger. Empty on a
  // human-seeded first turn.
  string trigger_traceparent = 10;
}
```

Test cycle: `moon run compass-proto:gen` regenerates both stacks; buf-breaking
gate green (pure adds); generated Go accessors (`GetTraceparent()`,
`GetTriggerTraceparent()`) compile.

Interfaces:

- Consumes: `agent.proto:196-206`, `:219-236`; `compass.proto:524-533`;
  `agent_gateway.proto:106-115`.
- Produces: `SteerControl.Traceparent` (3), `DeliverControl.Traceparent` (4),
  `SessionInjection.Traceparent` (4), `CommsCallRequest.TriggerTraceparent` (10)
  in both stacks — the wire seams T5 stamps/links and compass-agent
  decodes/emits.

### T5 — Bus bridge + control-wrap stamping + message-path spans

Owner: compass-obs. Depends on T1, T2, T4.

The propagation leg proper, in seven moves:

1. **Bus bridge (`PublishCtx`, message-origin only).** Add
   `PublishCtx(ctx, payload)` (`go/events/events.go`, beside the ctx-free
   `Publish` at `:165`) that stamps the serialized traceparent onto
   `Stamped[P]` (beside `Seq`/`AtUnixMS`/`InstanceEpoch`, `events.go:171-176`).
   Only `publishMessagePosted` (`mapping.go:463-468`) switches to `PublishCtx`,
   threading ctx from BOTH its live-dispatch callers — `PostMessage`
   (`comms.go:356`) and `RespondToAsk` (`comms.go:395`, the answer message);
   every other publisher (board
   `issue_projection.go:90`/`:119`, `projection.go:100`; presence
   `presence.go:216`, `activity.go:48`; `publishReady` `serve.go:746`; the
   `mapping.go` metadata publishers) keeps the ctx-free `Publish` and leaves
   `Traceparent` empty. §Alternatives rejects the full signature cutover (it
   would ripple ctx into ctx-free structural sinks).
2. **Consumer extraction.** `handleEvent` (`consumer.go:360-366`) derives its
   dispatch ctx via `otel.ContextWithTraceparent(ctx, event.Traceparent)`
   before `onMessagePosted`.
3. **Stamping at control-wrap.** `deliverOp`/`steerOp`
   (`consumer.go:374-395`) gain a `traceparent string` parameter (populated
   with `otel.Traceparent(ctx)` at the call sites, beside the existing
   `authorHandle` resolution); sweeps (`settle.go:193`, `:244`, and the
   cursor sweep) root their own per-pass span and stamp from it.
4. **Hop spans.** `gatedDispatch` (`dispatch.go:343-354`) wraps the
   `DispatchControl` call (`:350`) in a span; the runner's DeliverControl
   dispatch arm (`runner/dispatch.go:457-472`) starts a span parented off
   the op's traceparent around `host.Deliver` (`:469`). Both carry
   `compass.message.id`, `compass.op.kind`, `compass.session.id` attributes.
   The op itself is relayed VERBATIM (clone-only, `gateway/control.go:100-104`)
   — the runner never rewrites the traceparent.
5. **Held-deliver continuity.** The held registry (`dispatch.go:81-85 hold` →
   `settle.go:257-273 fireHeld`) carries the origin traceparent beside the
   message id — `held` becomes `[]heldEntry{messageID, traceparent}`, captured
   at `hold()` from the bus-extracted ctx and restamped via
   `otel.ContextWithTraceparent` at `fireHeld` before `fanOut`. This keeps the
   agent-reply-fans-out leg connected across the settle edge (`fireHeld` runs
   on the ctx-free consumer loop, so without this the whole held set stamps
   empty); empty-in stays empty-out when the agent post had no origin span
   (Open Question 5).
6. **Cross-turn causal link (a′ + termination).** In `executeCall`'s Post arm
   (`relay_comms.go:416`), after otelconnect has created the `RelayCommsCall`
   origin span on the handler ctx, add a span **Link** built from
   `CommsCallRequest.trigger_traceparent` via the OTel link API
   (`trace.SpanFromContext(ctx)` + a link from the parsed remote context) — a
   LINK, never a parent, so the reply's fresh root references its trigger without
   nesting. Empty `trigger_traceparent` adds no link. This is the mechanism that
   keeps traces terminating (§Trace lifetime and termination).
7. **Op-kind delivery metric.** A single Int64Counter
   (`compass.delivery.dispatched`) CREATED ONCE at meter setup / consumer
   construction and held as a field (never re-created inside the hot path —
   per-call instrument creation is an OTel anti-pattern: duplicate-instrument
   churn/warnings), incremented at `gatedDispatch` (`dispatch.go:343-354`) with
   the op-kind (steer/deliver) attribute ONLY — never per-session/per-channel
   (§Global Constraints cardinality rule). Op-kind is read from the
   `*AgentControl` oneof, the SAME source as the move-4 `compass.op.kind` span
   attribute. RPC duration/count come free from otelconnect once the meter
   provider exists (T2/T3).

Test cycle: bus round-trip test (a `PublishCtx` under an active span yields a
Stamped event whose traceparent matches; no span ⇒ empty); consumer test
extending `TestDeliverAndSteerCarryAuthorFromHandle`'s harness
(`delivery/mention_test.go:261`) asserting the dispatched steer AND deliver
ops carry the publisher's traceparent, and empty when no span; a held-then-
settle test asserting the FIRED held ops carry the origin traceparent across
the settle edge (empty-in ⇒ empty-out); a no-provider test asserting dispatch
is unaffected (never blocked); a runner dispatch-arm test asserting the
relayed op reaches `host.Deliver` with the traceparent unmodified; a
causal-link test asserting a `RelayCommsCall` with a non-empty
`trigger_traceparent` produces an origin span that is a fresh root (trace id ≠
the trigger's) carrying a Link to the trigger's context, and that an empty
`trigger_traceparent` produces a root with no link; a metric test asserting
`compass.delivery.dispatched` increments with the op-kind attribute and no
session/channel labels.

Interfaces:

- Consumes: T1 helpers; T4 generated fields; the sites cited above.
- Produces:

  ```go
  // go/events/events.go
  func (b *Bus[P]) PublishCtx(ctx context.Context, payload P) uint64 // origin path
  // Publish(payload P) stays ctx-free for every non-message publisher.
  type Stamped[P any] struct {
      Seq           uint64
      AtUnixMS      int64
      InstanceEpoch uint64
      Traceparent   string // W3C; "" when the publisher had no active span
      Payload       P
  }

  // go/internal/delivery/consumer.go
  func deliverOp(msg *compassv1.Message, fromHandle, traceparent string) *compassv1internal.AgentControl
  func steerOp(msg *compassv1.Message, fromHandle, traceparent string) *compassv1internal.AgentControl

  // go/internal/delivery/dispatch.go — held registry carries the origin traceparent
  type heldEntry struct{ messageID, traceparent string }
  ```

### T6 — E2E trace-continuity test: one turn, one trace

Owner: compass-obs. Depends on T2, T3, T5.

The proof of the ratified goal. An in-process end-to-end test (on the
pattern of the existing cross-process delivery tests, e.g.
`go/server/offline_mention_e2e_pgtest_test.go`) with an in-memory span
exporter (`sdk/trace/tracetest.NewInMemoryExporter`) installed on both the
server and (where the harness hosts it) the runner side: post a message via
`PostMessage`, drive it through the consumer to a bound session, capture the
op at the gateway/agent seam, and assert (a) the op's `traceparent` parses
to the SAME trace id as the PostMessage handler span, (b) every recorded
server/runner hop span shares that trace id (one connected trace), (c) the
`traceresponse` header on the PostMessage response carries the same trace
id, and (d) with the endpoint unset the identical flow dispatches
successfully with empty traceparent and zero recorded spans. Agent-side
continuation is compass-agent's test surface (RIG-2871), not re-tested here.

Additional cases pin the paths T5 adds and the continuity BOUNDARY: (e) a
held-then-settle agent post asserts the fired deliver carries the same trace
id as the agent post's own origin (continuity across the hold edge, within the
reply turn's trace); (f) a sweep-delivered message asserts its op's traceparent
parses to a trace id DIFFERENT from the post's, pinning fresh-root-by-design
(§Continuity boundary); (g) an ask-answer turn (`RespondToAsk`) asserts the
answer message's deliver op carries the same trace id as the RespondToAsk
handler span; (h) an agent-authored post (a′) asserts its `RelayCommsCall`
origin span exists and is a fresh root; (i) TERMINATION — a reply carrying a
`trigger_traceparent` asserts a NEW trace id (≠ the trigger's) with a Link back
to the trigger, proving replies branch rather than extend one unbounded trace;
(j) the `compass.delivery.dispatched` op-kind counter increments and carries no
per-session/per-channel label.

Interfaces:

- Consumes: T1/T2/T3/T5 outputs;
  `go.opentelemetry.io/otel/sdk/trace/tracetest`;
  the delivery test harness (`go/internal/delivery/helpers_test.go`).
- Produces: the e2e continuity test; the span/attribute names it locks in
  become the contract T5-dashboards (#656 T5) renders.

## Tasks

- [ ] T1 — `go/internal/otel` bootstrap package + config + off-by-default
      gating (tracer AND meter provider) + traceparent helpers + trace-response
      interceptor + unit tests (Owner: compass-obs)
- [ ] T2 — Server emission wiring (flag/env → `ServeConfig`, `run()` tracer +
      meter setup, otelconnect on the delivery-origin handlers) + PostMessage
      origin-span attrs + `traceresponse` response header + both CORS builders'
      `ExposedHeaders` (Owner: compass-obs)
- [ ] T3 — Runner emission wiring (flag/env → `RunnerConfig`, `run()` tracer +
      meter setup, client interceptor on outbound dials) (Owner: compass-obs)
- [ ] T4 — Additive proto fields: `traceparent` on SteerControl (3),
      DeliverControl (4), public SessionInjection (4), and `trigger_traceparent`
      on CommsCallRequest (10; #628 holds 7-9) + regen both stacks (Owner: **compass-server**;
      stacked seam — lands first, unblocks compass-agent decode/emit RIG-2871)
- [ ] T5 — Bus ctx/traceparent bridge (`PublishCtx`, message-origin only) +
      control-wrap stamping + sweep-rooted spans + held-deliver continuity +
      server/runner hop spans + cross-turn causal link + op-kind delivery
      metric + tests (Owner: compass-obs)
- [ ] T6 — E2E one-turn-one-trace + termination (linked new trace) +
      metric-recorded test (Owner: compass-obs)
- [ ] compass-agent — emit `traceparent` on the public SessionInjection
      observation + re-attach decoded `traceparent` as `trigger_traceparent` on
      outbound posts (the cross-turn link source), beside #649 T3 decode
      (Owner: compass-agent; after T4)
- [ ] Driver — coordinate compass-native image rebuild (FOD hash change) and
      compass-agent regen at T4/impl PR time

## Open Questions

1. **RESOLVED (Matt, freeze gate 2026-08-27): traces + metrics in T4b.**
   #656 D6/T4b says "traces/metrics" (parent record
   `compass-observability-architecture/design.md:658`). Matt ruled the metrics
   leg ships now, not as a follow-up. Scope: otelconnect RPC duration/count (free
   from the same interceptor once a MeterProvider exists) + ONE delivery counter
   (`compass.delivery.dispatched`) keyed by op-kind ONLY — never per-session or
   per-channel labels (§Global Constraints cardinality rule). Metrics wiring is
   the same providers/exporters plus `SetupMeterProvider`, no re-instrumentation
   (folded into T1/T2/T3/T5).
2. **RESOLVED (Matt, freeze gate 2026-08-27): YES — expose `traceparent` on the
   public `SessionInjection` observation (field 4).** `SessionInjection`
   (`proto/compass/v1/compass.proto:524-533`; next-free field 4 after
   `op_kind=1, message_id=2, from_handle=3`) is the public
   `SessionEvent`/`SubscribeAgentSession` payload. Adding a scalar `string
   traceparent` does NOT touch the gen-fenced enum (`SEA-1267`,
   `compass.proto:535-539`, fences `SessionInjectionKind`), so it is a clean
   additive public-API field. The agent emits it from the `traceparent` it
   decoded (#649 T3), giving public session-stream consumers a direct
   observation→trace join (#649 OQ3's observation symmetry). Folded into T4
   (proto) + compass-agent emission (cross-lane ownership). The impl PR touching
   the public surface carries its own ledger row at impl time.
3. **Bus trace-context bridge shape.** T5 bridges the trace context across the
   in-process bus via a NEW `PublishCtx(ctx, payload)` used only by the
   message-origin publisher, leaving the ctx-free `Publish(payload)` for every
   other publisher (§Alternatives rejects a full-signature cutover — it would
   ripple ctx into deliberately ctx-free structural sinks like `LifecycleSink`).
   This is compass-server's file zone (`go/events`, `go/internal/comms`).
   **DECIDED (compass-obs; coordinate at impl): adopt the `PublishCtx` split as
   designed; coordinate the surface add with compass-server at impl** (flagged
   here so the impl PR review isn't the first they hear of it).
4. **`traceresponse` header name.** The W3C trace-context response header is
   still a draft. **DECIDED (compass-obs): use `traceresponse` with the standard
   `00-…` grammar anyway** — it is the emerging standard, costs nothing, and
   a rename is a one-line change in T1's interceptor + the UI reader; a
   bespoke `x-compass-trace-id` buys nothing.
5. **RESOLVED (Matt, freeze gate 2026-08-27): (a′) + cross-turn link.** Agent
   posts do not ride the `CommsService` RPC — they ride the UNARY `RelayCommsCall`
   (`runner.proto:95`; `Hub.RelayCommsCall` `relay_comms.go:250`) →
   `Hub.executeCall` (`relay_comms.go:409`, Post arm `:416`) →
   `Comms.PostAsAccount` (`agent_caller.go:131-148`) → `PostMessage` in-process.
   Because `RelayCommsCall` is unary (not on the bidi Sessions stream,
   `runner.proto:70`), its handler ctx is per-call — no stream-lifetime span to
   leak. **(a′):** add the otelconnect interceptor to the RunnerService handler
   (`NewMountedHandler`, `handler.go:433-437`), so every `RelayCommsCall` gets a
   per-call origin span for free — symmetric to the `CommsService` handler,
   in-lane, no hand-minted span. This makes an agent post a fresh SERVER-minted
   root (the turn boundary), and T5's `PublishCtx` threading already carries it
   to the stamp. Matt also greenlit the proto change that turns "(a′) fresh root"
   into a terminating causal chain: `trigger_traceparent` on `CommsCallRequest`
   (T4) + a server-side span **Link** (T5 move 6), so the reply's new trace
   references its trigger without a parent edge. This SUPERSEDES option (c)
   (continue the delivered context onto the reply) precisely because a parent
   edge would never terminate — the record's answer to Matt's "the trace needs
   to terminate somewhere" (§Trace lifetime and termination).
