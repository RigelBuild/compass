# Compass observability & in-product data architecture

Status: Draft
Directory-form record under `docs/designs/observability/`.
Products framing: [`docs/concepts/self-host-and-managed.md`](../../../concepts/self-host-and-managed.md)
(two products, one core) — referenced throughout, not restated.

## Problem / Intent

Compass needs its full observability and in-product data architecture captured
durably: the in-product usage/metrics/graphs surface, the OTLP ops-export path,
and both crossed with the two products — the self-hosted OSS core in this repo
and the private, commercially-licensed managed multi-tenant service in the
private monorepo — including per-user/org paths on the managed side and Rigel's
observability over
the whole managed service. Today only fragments exist (agent-side OTLP emission
is shipped; the UI usage strip renders stub data; there is no fan-in, no
in-product data store, and no recorded resolution of the "bigger store for
managed scale?" question). This record lays out the whole architecture and
resolves the store question as a Decision.

## Approach

### The two planes × two products — the load-bearing separation

Everything in this record hangs off ONE distinction. There are two planes, and
they must never be collapsed:

- **Plane A — the in-product data surface.** Graphs and data rendered INSIDE
  the Compass UI (SolidJS) that the END USER sees: LLM account usage, token
  spend, and other product metrics. This is a **product feature** with its own
  store, its own read API, and native Solid charts — never embedded Grafana.
- **Plane B — OTLP ops-observability export.** Telemetry egress (traces,
  metrics, logs) to an EXTERNAL Grafana-class backend — the operator's own for
  self-host, Rigel's own for the managed service — for ops observability. This
  is an **ops integration**: Compass emits and routes, it does not store or
  render this data.

Crossed with the two products
([`self-host-and-managed.md`](../../../concepts/self-host-and-managed.md); the
canonical framing is `docs/designs/infra/runtime/compass-elastic-session-runtime/design.md:71-101`,
"Compass ships as two products over one shared core"):

| | Plane A — in-product data surface | Plane B — OTLP ops export |
| --- | --- | --- |
| **Self-hosted (OSS core, this repo)** | In-app charts from the deployment's own bundled Postgres. Single-team/single-org scope. | Emission off by default; operator points `OTEL_EXPORTER_OTLP_ENDPOINT` at THEIR backend; bundled fan-in collector with `--otel-external` opt-out; dashboard JSON shipped in-repo. |
| **Managed (private, commercially-licensed; private monorepo, compass.rigel.build) — per user/org** | The SAME in-app data surface, **tenant-scoped**: identical read API, scoped to the org. Each org's data is separated. Plane A is authoritative for usage/spend (see D5). | Per-tenant telemetry export (an operator/enterprise ask) is a named managed-plane deferral, not designed here; whole-service export is row 3. |
| **Managed — Rigel over the whole service** | **Cross-tenant fleet analytics + billing** — built in the private monorepo on the core's seams: analytics over the tenant-scoped rollup reads, billing over the tenant-scoped raw-event export (D1/D5). Named here, designed there. | The fan-in collector exports whole-service telemetry to Rigel's own Grafana — Rigel consumes Plane B "just like a user", plus managed control-plane obs designed in the private monorepo. |

Cross-tenant analytics, billing, and control-plane observability are
managed-plane concerns: they live in **the private monorepo** and are designed
there when the managed service is built. This record's job is to make sure the
core's seams support them — tenant-scopable rollup reads (D2), a raw-event
export contract for billing (D5), and an export path Rigel can point at itself
(D3) — not to design them.

### Plane A — in-product data surface

**Source.** Two sources feed Plane A (D5): the OMP gateway **to be bundled into
the Server** records per-account/org **token usage + spend** (the display
quantity), and the runtime (server/runner) records **Manager/agent compute
usage** (the billing-grade quantity managed caps and charges overage on).
**Neither source exists in `go/` today**, and they unblock differently: bundling
the gateway has NO design record yet — an explicit **upstream prerequisite** of
the token source (flagged in the Plan, not a task here) — whereas the
compute-usage accounting needs no separate record, being new runtime
instrumentation **T1 owns** on the write path (distinct from T4b's OTel
telemetry: billing compute cannot be derived from traces any more than from
token counts).

**Store and read path.** Usage/spend events land in Postgres rollup tables in
the compass-server store (the store today is a single squashed migration,
`go/internal/store/migrations/0001_init.sql:1-8`), behind an append-only write
contract; the UI reads fixed JSON time-series through a tenant-scoped gRPC
read API on the `compass.v1` contract — the sole UI↔server door
(`AGENTS.md:49-58`: schema under `proto/compass/v1`, regen via
`moon run compass-proto:gen`, never a raw stub). See Decisions D1/D2.

**Rendering.** Native charts in the Solid UI — **not embedded Grafana** (AGPL
plus insufficient customizability; see Alternatives). The shell already
exists: `apps/ui/src/components/UsageBar.tsx` renders per-account
provider/plan/tokensUsed/tokensLimit/resetIn meters ("mirrors Orca's
account/usage strip", `UsageBar.tsx:16-17`) from `STUB_USAGE`
(`apps/ui/src/stub-data.ts:1342`, with a `costToday` field at
`stub-data.ts:446`) — wired to nothing. Cut 1 of the UI work is wiring this
shell to real gateway data.

**Peer convention (survey-verified).** Peers near-universally ship an in-app
data surface and NONE embeds Grafana — every one renders its own frontend
charts from its own store (backend query → JSON time-series → frontend
chart): PostHog Insights (`posthog/queries/insight.py` executing against
ClickHouse via `sync_execute`), Sentry Custom Dashboards (ClickHouse via
Snuba), GitLab Value Stream Analytics (ClickHouse CDC'd from Postgres), Coder
Insights (`coderd/insights.go:38-74`, Postgres rollup queries →
`httpapi.Write` JSON), Supabase Logs & Analytics (Postgres `_analytics` schema
by default on self-host), Immich Server Statistics (on-demand Postgres
aggregation). Compass follows the Coder/Supabase shape — Postgres rollups —
for the reasons resolved in Decision D1.

### Plane B — OTLP ops export + fan-in

**Already shipped (emission).** The agent side emits OTLP today: RIG-2508 loop
OTel (`docs/designs/observability/compass-agent-loop-otel/design.md`) and
RIG-2426/RIG-2518 transport OTel
(`docs/designs/repo/compass-agent-effect-otel/design.md`). OTLP/http-protobuf,
endpoint operator-supplied via `OTEL_EXPORTER_OTLP_ENDPOINT`, OFF by default.
The Runner materializes `${home}/.compass/env`; the endpoint key survives
`isReservedEnvKey` filtering (`packages/compass-agent/src/cli.ts:104-106` —
only `HOME` and `COMPASS_*` are reserved), the reachability chain pinned in
`docs/designs/repo/compass-agent-effect-otel/otel-endpoint-deployer-contract.md:24-28`.

**New: the fan-in collector.** Emission is per-surface; ops needs one place to
point at. A **bundled OTel Collector** becomes the single fan-in point: every
Compass surface that emits sends to one OTLP endpoint, and the collector exports
out — to the operator's backend (self-host) or Rigel's (managed). This realizes
prior parked art: the effect-otel record's OQ2 already named a bundled
server-side collector (Grafana's `docker-otel-lgtm` fan-out shape) as "a
legitimate FUTURE server-side option"
(`docs/designs/repo/compass-agent-effect-otel/design.md:543-551`).

**What actually emits today — and the gap.** Only the **agent** emits OTLP
(RIG-2508/RIG-2518, TypeScript). A repo grep of `go/` finds **zero** OTel
instrumentation: the **server, runner, and stack have no emission**, and the
web UI has none. So a fan-in collector shipped today would fan in a single
producer, and "Rigel's observability over the whole managed service" would
degrade to whole-**agent** observability. Matt ratified (2026-08-26) that this
gap is in scope: server + runner get Go-side OTel SDK emission (T4b) and the
system gets full end-to-end tracing across process boundaries; see D6. UI product
analytics is a separate plane, resolved as OQ-B' (PostHog embedded in the UI, not
OTel); browser OTel/RUM is a named follow-up. Until T4b lands, Plane-B prose must
not claim four emitters in the present tense.

**Bundle-with-opt-out, on the S4 pattern.** The collector ships as a second
supervised stack component, following the distribution record's
postgres-as-container pattern
(`docs/designs/infra/release/compass-distribution/design.md:258-273`, S4;
DL-260/DL-262 at `design.md:752,754`). It reuses the generic
container-teardown seam (`go/internal/stack/deps.go:142-154`, name-keyed) as-is,
but the spawn chain (`go/internal/stack/stack.go:193-263`) is a hard-coded
ordered sequence with per-component readiness waiters (`waitReady`/`waitPostgres`
are component-specific, `stack.go:288-323`) — so adding the collector is a real
code change with a new bespoke readiness probe (collector health), not a
drop-in. The `--database-external` opt-out is the template
(`compass-distribution/design.md:407-411`) — here `--otel-external <endpoint>`.
S4's own corollary sanctions the bundle-by-default posture: "The corollary for
future external deps …: bundle each by default with its own opt-out — the
compounding-standup-pain risk comes from making deps BYO, not from having them"
(`design.md:271-273`). The peer survey's dominant pattern is
expose-metrics-and-BYO-collector (Temporal, Coder, Supabase, Immich, Sentry);
Compass deliberately takes the batteries-included fork because S4 already ruled
bundle-by-default for this stack, and the opt-out preserves the BYO path. Default
posture (D3): the bundled collector is **present and receiving** by default, and
**exports nowhere until an export endpoint is configured** — it drops on the
floor rather than buffering to disk, so the zero-config self-hoster gets a live
local endpoint with no sink-fill risk.

**Dashboards.** Ship Grafana dashboard JSON in-repo so an operator's Grafana —
and Rigel's managed Grafana, "just like a user" — gets prebuilt dashboards.
Peer precedent: GitLab ships dashboards out-of-repo
(`gitlab-org/grafana-dashboards`, with current examples in its runbooks repo),
Temporal auto-provisions `temporalio/dashboards` via Helm, Supabase ships
`supabase-grafana` dashboard JSON, Coder ships the `coder/observability`
chart. Shipping dashboard JSON is the near-universal peer practice.

### The three data classes — why "one big store" is the wrong frame

The "bigger dep for scale?" worry assumes one monolithic observability store.
Across the full breadth there are THREE distinct data classes, each with a
different natural home — and the scale worry attaches to the two classes the
OSS core never stores:

- **Class 1 — OTLP ops telemetry (Plane B): the firehose.** High-cardinality
  traces + metrics from agents/runners/server/UI. The only genuinely
  massive-scale-store class — and its scale-appropriate home already exists
  and is NOT built by Compass: the operator's own Grafana/LGTM backend
  (self-host) and Rigel's Grafana Cloud (managed). The bundled fan-in
  collector ROUTES to it; Compass never builds or bundles a heavy TSDB/OLAP
  for this. The "bigger dep that supports massive scale" is a backend we
  POINT AT, not one we own.
- **Class 2 — in-product usage/spend (Plane A): the ONLY store the OSS core
  builds.** Per-account/per-org usage events, rendered in the Compass UI:
  Manager/agent **compute-usage** events (the billing-grade quantity, from the
  runtime) and **token-usage/spend** events (the display quantity, from the
  bundled OMP gateway) — see D5 for why they are distinct. A BOUNDED per-tenant
  rollup, not an event firehose: bounded by a single org's own activity even at
  managed scale, because each org's data is separated. It never sees cross-tenant
  firehose volume.
- **Class 3 — cross-tenant managed analytics (whole managed service, for
  Rigel).** PostHog-scale product analytics across ALL tenants is where a
  ClickHouse-class OLAP genuinely belongs — and it lives in the PRIVATE
  MONOREPO, built when the managed service is. Any big-scale analytics dep,
  if ever, is adopted THERE, on Rigel's own infra. Not this repo, not now.

With the classes separated, the store question resolves cleanly — Decision D1.

### Physical placement — where each class lives, and how it graduates

The classes above are logical; Matt asked the physical question directly — does
the analytics data live in the product DB (Neon) or somewhere separate, and how
much goes to Postgres vs a big-data engine. The answer is that the product DB is
**never** an analytics home, and the placement is fixed per class so the
foundation scales without a re-instrumentation later:

| Data class | Self-hosted core home | Managed home | Scale graduation |
| --- | --- | --- | --- |
| Product OLTP (accounts, orgs, sessions, settings) | Bundled Postgres | Neon (the product DB) | Route heavy reads to a Neon read replica/branch — never run analytical scans on the primary product branch. |
| Class 2 — billing-grade compute-usage log + derived rollups (D5/D1) | Bundled Postgres, its own store behind the `UsageStore` seam | Its own Neon project/schema, deliberately separate from the product DB | Swap the Class-2 backend behind the D1 seam if per-org volume ever outgrows Postgres (headroom, not a foregone migration). |
| Class 1 — ops firehose: metrics, logs, traces (Plane B) | Operator's own Grafana/LGTM backend | Rigel's Grafana Cloud (Mimir/Loki/Tempo) | Govern cardinality at the emission point (Adaptive Metrics/Logs/Traces) — never a SQL DB. |
| Class 3 — cross-tenant analytics (whole managed service) | Not built in the core | PostHog's HogQL warehouse (managed plane; D7) now | Graduate to a purpose-built OLAP (ClickHouse/BigQuery) fed from Postgres by CDC (e.g. PeerDB, which supports a Postgres/Neon source directly) — a replication config, not a re-instrumentation. |

Two placement rules fall out of the table. **The product Neon branch never
carries analytical scan load** — OLAP-scale scans contending with product OLTP
on one instance is the classic failure mode, and Neon branching makes the
read-replica path cheap. And **"collect the data now so we are ready at scale"
is already the design, not a new store**: capture the append-only compute-usage
stream today with a stable schema (D5), and the graduation to a big-data engine
is a CDC pipeline off that same stream — no re-instrumentation, no cost balloon
(each class sits on the backend whose pricing model fits it), and no scale wall.
The D1 store-swap seam is exactly the "optimize placement early so we can swap
the backend later" lever.

## Decisions

### D1 — The store (RATIFIED): Class-2 = Postgres in the core; a store-swap seam lets managed back it with a bigger dep

Matt asked whether Compass — heading toward a PostHog-model managed service —
should adopt "a bigger dep that can support massive scale early on." **Ratified
by Matt (2026-08-26): "use postgres for the in product metrics, build a seam so
we can swap out on the managed service for a bigger dep."** So:

- **Class 2 store = Postgres in the core, day-1, both products. The core never
  bundles a bigger dep.** The firehose that would justify ClickHouse is Class 1
  (lives on the LGTM backend we point at) and Class 3 (lives in the private
  plane) — NEVER Class 2. So the honest answer to "bigger dep early?" is NO for
  everything the OSS core builds: Postgres is the sole Class-2 implementation the
  core ships, and a bigger dep, if managed ever needs one, is swapped in behind
  the seam on the managed plane (next bullet) — never bundled into the core.
  Reasons the core stays Postgres: the S4 anti-standup-pain posture (never bundle
  a heavy OLAP into self-host —
  `docs/designs/infra/release/compass-distribution/design.md:258-273`); the
  vendor-neutrality hard rule ("Every primitive must be self-hostable:
  rootless podman, git, nftables, Postgres, an S3-compatible object store,
  our own agent loop" —
  `docs/designs/infra/runtime/compass-elastic-session-runtime/design.md:418-422`);
  and Class 2 is bounded per-org, so Postgres fits it even at managed per-org
  scale — the managed swap is headroom, not a foregone migration.
- **Build the SEAM as a genuine store-swap boundary (the durable part, per
  Matt's ruling):** (a) an append-only usage/spend-event WRITE contract (events
  in) and (b) a store-agnostic, tenant-scoped gRPC READ API returning fixed JSON
  time-series the Solid UI consumes (self-host = single tenant; managed = per-org
  scope). The Class-2 STORE sits behind an interface (write events + read
  rollups/series), with Postgres as the sole implementation the core ships. The
  seam does two jobs: it lets the MANAGED plane **swap the Class-2 backend for a
  bigger dep at its scale** without touching the read API or the UI, and it lets
  managed build Class-3 cross-tenant aggregates ON TOP of the same tenant-scoped
  reads. The core never takes an OLAP dep; the swap is a managed-plane choice the
  seam keeps cheap (a backend swap behind a stable interface, not a contract
  migration).
- **Fits the existing store posture:** compass-server's relational store is
  Postgres (single squashed migration,
  `go/internal/store/migrations/0001_init.sql:1-8`), and the DL-174
  differential-oracle test pyramid already gates Postgres as the one live
  dependency — "a deterministic in-memory/fake reference proves each contract
  cheaply, and a live `pgtest` suite proves the real Postgres backend obeys
  the same contract"
  (`docs/designs/meta/compass-test-strategy/design.md:123-128,412-416`).
  Class-2 rollup tables are a natural addition, no new dependency.

The rejected bigger-dep paths are in Alternatives considered.

### D2 — The read API is a `compass.v1` contract, tenant-scoped from day 1

Plane-A reads go through the sole UI↔server door: a proto contract under
`proto/compass/v1`, regenerated clients, never a raw stub (`AGENTS.md:49-58`).
Every read carries tenant scope. **"Tenant" concretely, day-1:** the core
schema has no org/tenant concept today (the squashed `0001_init.sql` carries
`accounts`/`user_accounts`/`agent_accounts`, no org). So the read API and the
`UsageStore` carry a **`tenant_id` column populated from a fixed single-tenant
sentinel** in the OSS core, resolved server-side from the token subject — never
client-supplied. Self-host is that one sentinel tenant forever; the managed
plane populates real org ids against the same column, so an orgs table is a
managed-plane addition that needs no core schema change. **Granularity is a
closed proto enum** (e.g. `HOUR`/`DAY`/`WEEK`), fixed in the contract — the read
API only serves granularities the rollups pre-aggregate, so the enum, not the
store, bounds what a caller can ask for (arbitrary-granularity queries would
require raw events and are out of the read API). This is the seam Class 3's
analytics build on; Class 3 billing uses the raw-event export (D5), not this
rollup read.

### D3 — Plane B fan-in is a bundled collector with `--otel-external` opt-out

The stack bundles an OTel Collector as a supervised component (S4 pattern — a
new component on the spawn chain `go/internal/stack/stack.go:193-263` with a
bespoke collector-health readiness probe, reusing the generic teardown seam
`deps.go:142-154`; not a drop-in). `--otel-external <endpoint>` opts out (the
`--database-external` template, `compass-distribution/design.md:407-411`); the
managed plane supplies its own. **Default posture:** the bundled collector is
present and receiving by default, and **exports nowhere until an export endpoint
is configured**, dropping rather than buffering to disk — so a zero-config
self-hoster gets a live local endpoint with no sink-fill risk, and configuring
an export backend is the single step that turns egress on. Compass surfaces emit
**to** this collector's OTLP endpoint (that endpoint is what
`OTEL_EXPORTER_OTLP_ENDPOINT` points at); agent-side emission stays exactly as
shipped (RIG-2508/RIG-2518). Which surfaces actually emit is D6 — today only the
agent does.

### D4 — No embedded Grafana; native Solid charts

Plane A renders native charts in the Solid UI. Matt, verbatim: "agreed on not
bundling grafana itself into the app - there's agpl issues with that and it's
not customizable enough. We'd just build charts etc into the solid web ui."
The peer survey found zero products embedding Grafana in-app (see Approach).

### D5 — The compute-usage event log is the durable, billing-grade contract; rollups are derived (DECIDED — OQ-A)

Class 3 includes **billing**, and billing cannot be built on the D2 rollup read
API — it needs exact, auditable, idempotent event-level data with late-event and
correction semantics. That is an append-only event stream T1 writes, not a view
over it. So the D1 seam is **two contracts, not one**: (1) the tenant-scoped
rollup READ API (D2) for Plane A + cross-tenant analytics, and (2) a
tenant-scoped **raw-event export contract** the managed plane consumes for
billing. **What is billed is fixed by the tokens-and-billing model**
([`tokens-and-billing.md`](../../../concepts/tokens-and-billing.md)): Rigel does
not sell tokens (the user brings them, BYOK / BYO cloud subscription), so the
managed plane bills the **compute it brings** — Manager/agent activity, with caps
and overage. That makes the billing-grade record the **compute-usage event log**,
not the token/spend log. LLM token usage/spend is recorded off the gateway for
the in-product charts but is **not billed day-1** (it becomes billable only under
a future fully-managed-tokens offering); connected cloud-subscription
usage/resets are a monitored quota signal. So T1 writes **two event kinds** and
only the compute-usage one is the billing contract.
Recommendation, now DECIDED: the **append-only compute-usage event log is the
durable billing-grade record**, and the rollup tables are **derived and
rebuildable** from the event logs. The core commits day-1 to the compute-usage
event schema's shape (the durable contract) but does **not** build the exporter
(CDC / batch export / paginated event-read RPC) — that is a managed-plane build on
the committed shape. This severs the hidden coupling the three-class split would
otherwise hide (without it, managed billing either dual-writes usage events — the
exact failure the split avoids — or reaches around the read API into core tables).
This matches the usage-based-billing norm: OpenMeter, Lago, Metronome, Orb, and
Stripe all treat the immutable raw-event log as the source of truth and derive
billable quantities from it (Stripe's own guidance: "your internal metering layer
should produce its own rollups; Stripe should not be your source of truth — your
database is"). The interim gateway lineage the OMP gateway descends from persisted
a per-request spend row (a stopgap being removed; the bundled OMP gateway is the
durable **token**-metering point, feeding the display quantity — not the billing
contract). **Matt delegated this call (2026-08-26, "make a decision around
that"); DECIDED: the day-1 seam commits the compute-usage raw-event export
contract — billing is not severed. See OQ-A.**

### D6 — Server/runner OTel emission and full end-to-end tracing are in scope (RATIFIED Matt 2026-08-26)

The D3 fan-in collector is only worth its cost if more than the agent emits into
it. Today **only the agent emits** (RIG-2508/RIG-2518); a `go/` grep finds no
server/runner/stack OTel. **Matt ratified (2026-08-26): "we need otel thru all …
Also want full end to end tracing thru the system."** So:

- **Server and runner get Go-side OTel SDK emission** — RATIFIED in scope as
  T4b. This may land in this record or a sibling doc under the same design
  (Matt: "can be in this PR in a separate doc if needed"); it is kept as T4b
  here so the fan-in collector (T4) has more than one producer.
- **Full end-to-end distributed tracing through the system** is RATIFIED as a
  system goal: trace context propagates across every process/service boundary
  (server → runner → agent → bundled gateway) so one user request/turn is a
  single connected trace, not a per-surface island. This extends the agent-side
  work scoped in the in-flight message-trace-continuity design (RIG-2508,
  PR #649 — `docs/designs/observability/compass-agent-message-trace-continuity/design.md`
  once merged) outward to the Go surfaces. That record introduces the agent-side
  W3C `traceparent` seam but leaves its own **OQ1** open (server-side origination):
  only under its fork (b), server-side `traceparent` stamping, does a single
  trace span the server → agent boundary; under fork (a) end-to-end stops at the
  agent. **This end-to-end ratification presumes OQ1 resolves to (b)** — the two
  records must be reconciled on that point, and T4b's server/runner emission
  joins that seam.
- **UI product analytics is PostHog, a separate plane from this OTel work
  (RESOLVED, OQ-B') — but the two planes are joinable, and the core builds that
  join seam (D7).** Matt ruled product analytics is PostHog embedded in the Solid
  UI (managed → Rigel's PostHog; self-hosted → off-by-default or the deployer's
  own PostHog). Product analytics is a different signal from OTel — PostHog's
  frontend↔backend correlation is its own `X-POSTHOG-SESSION-ID` header, not the
  W3C `traceparent` this decision propagates — so the two are **parallel signals,
  joined by a shared correlation key, not merged data planes**. The backend OTel
  firehose is NOT fanned into PostHog by default (a deliberate billing-safety
  choice, D7); it stays on Plane B (Grafana/LGTM). Browser OTel/RUM (still
  experimental) is a named follow-up. See OQ-B' and D7.

Plane-B prose still describes only the agent as emitting **today**; T4b closes
the server/runner gap.

### D7 — The two planes join by a shared correlation key, not by fanning OTel into PostHog (billing-safe; core builds the seam)

Matt's direction: "primarily need it for managed, but we need to build out the
OSS core so that we can join them," plus "cautious about ingesting OTel directly
into PostHog for billing reasons." Both resolve to one decision: the core builds
a **correlation-key join seam**, and the actual joining/analysis happens in the
managed plane. Concretely there are three ways to join PostHog product analytics
with backend OTel, and the choice is a billing-safety call:

- **J1 — correlation-key join (CHOSEN for the core seam; storage-agnostic).**
  Full traces stay in Grafana/Tempo (Plane B); PostHog holds product events plus
  a shared key. Backend spans carry the PostHog session id (the UI's
  `X-POSTHOG-SESSION-ID` / semconv `session.id`), and product events carry the
  OTel `trace_id` (PostHog exposes native `$ai_trace_id` / `$ai_session_id`
  properties for exactly this). You pivot from a product funnel to the backend
  trace by the shared key without either system holding the other's data. **Zero
  trace-volume billing into PostHog** — the billing-safe answer. The core's job is
  to make the key present on both sides: propagate the session id onto backend
  spans and stamp the trace id onto the product events. That seam is what "build
  the core so we can join them" means.
- **J2 — collector fan-out into PostHog (managed option, NOT the core default).**
  The bundled OTel Collector (D3) *can* add PostHog's OTLP/HTTP endpoint as a
  second exporter (PostHog does ingest OTLP — Distributed Tracing, beta July 2026,
  free while in beta, on both Cloud and self-hosted). But the backend trace
  firehose is the highest-volume signal (an LLM call emits ~8-15 spans), and
  PostHog has **not announced post-beta trace pricing** — so firehosing OTel into
  PostHog is the billing risk Matt named. If ever enabled, it must be behind
  collector **tail-sampling** (keep errors + slow traces + a small healthy
  sample), and it is a **managed-plane choice**, off in the core.
- **J3 — unified HogQL warehouse / reverse export.** Querying traces + product
  events together in PostHog's warehouse (or exporting PostHog events into
  Grafana) is a managed-plane analytics build, not a core concern.

So the core commits **J1 only**: the correlation-key seam, billing-safe by
construction. J2/J3 are managed-plane options the seam leaves open. Self-hosted
support for the join is best-effort, not a priority (Matt: self-hosted "we aren't
getting the data anyway").

## Global Constraints

- **No embedded Grafana in Plane A** (AGPL + customizability) — native Solid
  charts only.
- **Never bundle ClickHouse (or any heavy OLAP) into self-host** — S4
  anti-standup-pain (`compass-distribution/design.md:258-273`) + the
  vendor-neutrality hard rule (`compass-elastic-session-runtime/design.md:418-422`).
- **OSS-core vs managed seam governs scope**: this record designs the OSS
  core; managed control-plane obs (cross-tenant aggregate, billing, OLAP
  adoption) is private-monorepo, named + deferred — per
  [`self-host-and-managed.md`](../../../concepts/self-host-and-managed.md).
- **The store abstraction is the day-1 commitment, not the store choice** —
  the append-only write contract + tenant-scoped read API (D1) precede and
  outlive any storage detail, and are what let the managed plane swap the
  Class-2 backend for a bigger dep behind a stable interface (Matt-ratified).
- **Plane A (gateway → Postgres) is authoritative for usage/spend; Plane B token
  metrics are a best-effort ops signal.** Token usage/cost appears on BOTH planes
  — the shipped agent OTel emits it (Plane B) and the gateway records it (Plane A)
  — so the same quantity flows through two pipelines with different loss/sampling
  characteristics. In-product charts + billing read Plane A; Plane B's token
  metrics are for ops dashboards only. When they disagree, Plane A is right by
  definition.
- **`compass.v1` is the sole UI↔server door**: any read API is a proto
  contract change under `proto/compass/v1` with regenerated clients
  (`AGENTS.md:49-58`), never a raw stub.
- **New store code fits the DL-174 test pyramid**: an in-memory reference in
  the default gate plus a `pgtest` suite proving the real Postgres backend
  obeys the same contract
  (`compass-test-strategy/design.md:123-128`).
- **Plane-B emission stays off by default** on the agent path, endpoint via
  `OTEL_EXPORTER_OTLP_ENDPOINT`, per the shipped RIG-2508/RIG-2518 posture.
- Markdownlint clean; prose and examples use `===`/`!==` semantics — never
  loose equality (`== null` exempt) — per the repo's `ts-no-loose-equality`
  rule.

## Alternatives considered

### Embedded Grafana in the product UI — rejected

AGPL license issues with bundling Grafana into the app, and it is not
customizable enough for a product surface (Matt, verbatim in D4). The peer
survey confirms nobody does this: PostHog, Sentry, GitLab, Coder, Supabase,
and Immich all render their own frontend charts from their own store; none
embeds Grafana panels in-app.

### ClickHouse / OLAP in the core, day-1 ("true PostHog model early") — rejected

Drags a heavy OLAP dep (Keeper/ZooKeeper etc.) into every self-host bundle for
volume the core's Class-2 data never reaches; violates the S4
anti-standup-pain posture and the vendor-neutrality hard rule. The genuine
PostHog-scale need is Class 3, which is private-plane — so day-1 OLAP in the
core buys nothing the D1 seam doesn't already enable later, at real cost now.
PostHog/Sentry/GitLab do run ClickHouse — for exactly the cross-tenant,
all-events analytics that is Class 3 here, and GitLab notably keeps it a
non-default secondary store even then.

### TimescaleDB (Postgres extension) as one store for both — rejected

Unnecessary: it solves Class-1 firehose time-series, but Class 1 lives on the
LGTM backend we point at, not a Compass-owned store; for the bounded Class-2
rollups plain Postgres suffices. It would add a TSL-licensed extension (a
license-posture question against the AGPL core and the vendor-neutrality
rule) for no Class-2 benefit. Revisit ONLY if a concrete Class-2 workload
ever proves plain Postgres insufficient — a reversible change behind the same
D1 seam.

### Metrics-only — "expose OTLP and call it a day" — rejected

Rejected per Matt: "so we can't just expose metrics and call it a day i
think." Class 2 — the in-product data surface — is a primary product feature;
Plane B does not cover it. The peer survey backs this: an in-app data surface
is the norm for mature products (PostHog, Sentry, GitLab, Coder, Supabase),
and the one clear exception (Temporal, which defers everything to external
Grafana) is the posture Matt explicitly declined.

## Plan

**Upstream prerequisite (flagged, not a task here — it has no design record
yet): bundling the OMP gateway into the Server.** The gateway is the Class-2
event source; T1-T3 depend on it. Its design record must land first.

**Out of scope (private monorepo — named, deferred):** the managed control
plane — cross-tenant analytics and aggregate observability, billing, any
OLAP-backend adoption (Class 3), tenant scheduling, per-tenant telemetry export.
UI product analytics is RESOLVED as its own plane (OQ-B': PostHog embedded in the
UI, off-by-default self-hosted / the deployer's own PostHog, managed → Rigel's
PostHog), added when the UI work lands, not an OTel task here; browser OTel/RUM is
a named follow-up. Managed-plane items are designed in the private monorepo when
the managed service is built, on top of this record's seams. (The Tasks
Out-of-scope list carries the same set.)

### T1 — Usage/event store + write contract

Owner: compass-server.

Append-only event write path behind the D1 write contract; the **compute-usage
event log is the durable billing-grade record** (D5), with the token-usage event
log as the Plane-A display quantity, and **Postgres rollup tables derived and
rebuildable from both** (new migration folded into the store per the existing
squash convention, `go/internal/store/migrations/0001_init.sql:10-15`);
store-agnostic contract so the write side never leaks Postgres shapes.
**Event-log lifecycle:** the rollups are bounded by cardinality
(tenant × account × window) and stay small, but the raw event logs grow with
activity — T1 sets a retention/compaction policy for the raw logs (a bounded
self-host retention window; the managed plane sets its own, since under D5 the
compute-usage event log is billing's source and its retention is a managed-plane
input). Rollups, being rebuildable, are the long-lived read source for Plane A.

Interfaces:

- Consumes: **compute-usage** events from the runtime (server/runner accounting
  of Manager/agent activity — new instrumentation T1 adds, absent from `go/`
  today) and **token-usage** events from the bundled OMP gateway (the upstream
  prerequisite); the existing store open/migration machinery behind
  `0001_init.sql`.
- Produces: a **`UsageStore` interface** over an append-only event write plus
  rollup/series reads (keyed by tenant × account × window), with a Postgres
  implementation as the sole backend the core ships, plus an in-memory reference.
  This interface IS the Matt-ratified store-swap seam: the managed plane provides
  an alternative backend for a bigger dep without changing the interface, the T2
  read RPCs, or the UI. Both backends are proven against the one contract via the
  DL-174 pyramid (`pgtest` + in-memory ref, `compass-test-strategy/design.md:123-128`).
- The write side carries **two distinct event kinds** (per the tokens-and-billing
  model — they are different quantities with different roles, so the contract must
  not conflate them):
  - a **compute-usage event** — Manager/agent activity (the unit managed caps and
    charges overage on: e.g. run/session/agent-active accounting keyed by
    account/org id, with a timestamp). This is the **billing-grade** record: the
    quantity the managed plane bills, so it is exact, auditable, and the durable
    source of truth. Its source is the runtime (server/runner accounting of
    Manager/agent activity), **not** the gateway — compute cannot be derived from
    token counts. This is what the D5 raw-event export contract is over.
  - a **token-usage event** — per-model-call usage/spend recorded off the bundled
    OMP gateway (account/org id, provider, model, tokens in/out, cost, timestamp).
    This powers the Plane-A in-product usage/spend charts and is **recorded for
    display, not billed day-1** (BYOK / BYO cloud subscription — Rigel does not
    sell tokens); it becomes billable only if a fully-managed-tokens offering ever
    ships. It is **not** the billing-grade contract.
- Produces (contract only, per D5; OQ-A DECIDED — in scope): the **compute-usage
  event schema as a committed, billing-grade shape** the managed plane's exporter
  reads. The core commits the event shape day-1; it does NOT build the exporter
  (CDC / batch / paginated event-read) — that is a managed-plane build. Matt
  delegated OQ-A and it is decided toward this in-record contract, so T1 commits
  the compute-usage schema as the billing-grade contract (the token-usage event is
  the display quantity, explicitly not the billing contract).

### T2 — Tenant-scoped read gRPC

Owner: compass-server.

Store-agnostic read API returning fixed JSON time-series/aggregates with
per-tenant scoping (self-host = single tenant, managed = per-org). This is
the `compass.v1` contract door.

Interfaces:

- Consumes: T1's rollup tables; the `compass.v1` contract discipline
  (`AGENTS.md:49-58`).
- Produces: new RPCs in the schema under `proto/compass/v1` (e.g.
  `GetUsageSeries(tenant, account?, window, granularity) → series of
  {bucket, tokensIn, tokensOut, cost}`), regenerated Go + TS clients via
  `moon run compass-proto:gen`; tenant scope enforced server-side, never
  client-supplied trust.

### T3 — In-app charts (Plane A UI)

Owner: compass-ui.

Wire the existing `UsageBar` shell (`apps/ui/src/components/UsageBar.tsx:18-47`,
today reading `STUB_USAGE` from `apps/ui/src/stub-data.ts:1342`) to real
gateway data through the generated client — the smallest visible win — then
richer usage/spend views. Native Solid charts: hand-rolled SVG for simple
meters (as `UsageBar` already does), a Solid charting lib only when real
time-series charts are needed.

Interfaces:

- Consumes: T2's generated `@compass/client` RPCs.
- Produces: `UsageBar` reading live per-account usage (replacing the
  `STUB_USAGE` import); a usage/spend view rendering T2's time-series JSON.

### T4 — Plane-B fan-in collector

Owner: compass-server/distribution.

Bundled OTel Collector as a supervised stack component: one OTLP endpoint the
emitting Compass surfaces send to, exporting out to the operator's (or Rigel's)
backend. Today only the agent emits (RIG-2508/RIG-2518); server/runner emission
is T4b.

Interfaces:

- Consumes: the S4 supervised-component pattern — spawn chain
  (`go/internal/stack/stack.go:193-263`, a new component + bespoke collector
  readiness probe), container teardown seam
  (`go/internal/stack/deps.go:142-154`), pgid record v2 container entries
  (DL-262); the `--database-external` opt-out template
  (`compass-distribution/design.md:407-411`); the shipped deployer contract
  for `OTEL_EXPORTER_OTLP_ENDPOINT`
  (`compass-agent-effect-otel/otel-endpoint-deployer-contract.md:24-28`).
- Produces: a digest-pinned collector image + stack component with readiness
  probe; `--otel-external <endpoint>` opt-out; default posture per D3 (receives
  by default, exports nowhere until configured); the Runner-materialized env
  pointing agent emission at the bundled collector.

### T4b — Server + runner OTel emission + end-to-end tracing (per D6; RATIFIED)

Owner: compass-server.

Go-side OTel SDK emission for the server and runner, so the fan-in collector
(T4) fans in more than one producer and "Rigel's observability over the whole
managed service" is real. RATIFIED in scope by Matt (2026-08-26, "otel thru
all"); may be authored in a sibling doc under this design if it grows large
(Matt: "can be in this PR in a separate doc if needed"). Includes **end-to-end
trace propagation**: server, runner, and agent share one trace context across
process boundaries (W3C `traceparent`), so a single user request/turn is one
connected trace. This joins the agent-side propagation scoped in the in-flight
Go surfaces. UI product analytics (PostHog) is NOT this task — it is a separate
plane resolved in OQ-B' (PostHog embedded in the UI, off-by-default on
self-hosted). It joins this OTel work by a correlation key, not by merging planes;
the backend trace firehose is not fanned into PostHog by default (billing-safe,
D7). Browser OTel/RUM stays a named follow-up per OQ-B'.

Interfaces:

- Consumes: the Go OTel SDK; the collector OTLP endpoint (T4); the W3C
  `traceparent` seam the agent-side trace-continuity record introduces (spanning
  server → agent only once that record's OQ1 resolves to server-side stamping).
- Produces: server + runner traces/metrics emitted to the bundled collector,
  trace context propagated across the server → runner → agent boundaries;
  the metric/span names T5's dashboards render.

### T5 — Ship dashboard JSON

Owner: compass-server/distribution.

Prebuilt Grafana dashboards in-repo for the operator's Grafana and Rigel's
managed Grafana ("just like a user"). Peer precedent: GitLab, Temporal,
Supabase, Coder all ship dashboard JSON (see Approach).

Interfaces:

- Consumes: the metric/span names emitted by the shipped agent OTel
  (RIG-2508/RIG-2518) and T4's collector pipeline.
- Produces: `dashboards/*.json` in-repo + a docs pointer from the self-host
  guide; no provisioning automation (import is the operator's one step).

## Tasks

Two tracks. **Track B (Plane B) is the only independently executable work now** —
it depends only on shipped agent emission (RIG-2508/RIG-2518) and existing stack
code. **Track A (Plane A) is blocked** on the undesigned OMP-gateway-into-Server
prerequisite. Execute Track B first; Track A unblocks when the gateway record
lands.

Track B — unblocked (do first):

- [ ] T4 — Plane-B fan-in collector (Owner: compass-server/distribution) —
      bundled supervised collector + `--otel-external` opt-out + D3 default
      posture.
- [ ] T4b — Server + runner OTel emission + end-to-end tracing (Owner:
      compass-server) — Go-side OTel SDK for server + runner + cross-boundary
      trace propagation (RATIFIED; may be a sibling doc).
- [ ] T5 — Dashboard JSON (Owner: compass-server/distribution) — in-repo
      Grafana dashboards for operators + Rigel.

Track A — blocked on the OMP-gateway prerequisite:

- [ ] PREREQUISITE (upstream, not a task here — write its design record FIRST):
      OMP-gateway-into-Server — gates T1-T3.
- [ ] T1 — Usage/event store + write contract (Owner: compass-server) — the
      runtime compute-usage accounting instrumentation (new in `go/`) + two
      append-only event kinds (compute-usage = billing-grade from the runtime;
      token-usage = display from the gateway) + derived Postgres rollups +
      retention policy + in-memory ref + pgtest suite.
- [ ] T2 — Tenant-scoped read gRPC (Owner: compass-server) — `compass.v1`
      schema change (fixed granularity enum) + regenerated clients + server-side
      tenant scoping.
- [ ] T3 — In-app charts (Owner: compass-ui) — UsageBar wired to live data,
      then time-series usage/spend views, native Solid rendering.
- [ ] T6 — PostHog embed + correlation-key join seam (Owner: compass-ui +
      compass-server; per OQ-B'/D7) — embed `posthog-js` behind an off-by-default
      enable flag + configurable host; stamp the OTel `trace_id` onto product
      events and propagate the PostHog session id onto backend spans (J1). Does
      NOT fan the OTel firehose into PostHog (that is a managed-plane, tail-sampled
      option). **`posthog-js` is a measurement/data SDK only — no PostHog-rendered
      UI ships in the product** (Matt: no PostHog UI elements in our own app; they
      would look off and are not Solid). Any in-app engagement surface (first-run
      tour, changelog/announcement banner) is built natively in Solid; PostHog
      contributes only headless data — event capture, and flag/early-access-feature
      JSON payloads (`getFeatureFlagPayload` / `getEarlyAccessFeatures`) our own
      component renders — never a PostHog widget. The native first-run product tour
      itself is a separate compass-ui/ux product concern, tracked outside this
      record. Its own plane; sequences after the core emission/store work.
- Out of scope (private monorepo, deferred): cross-tenant analytics /
  aggregate obs, billing exporter, Class-3 OLAP adoption, tenant scheduling,
  per-tenant telemetry export. UI product analytics is RESOLVED (OQ-B'): PostHog
  embedded in the UI, off-by-default on self-hosted / the deployer's own PostHog,
  managed → Rigel's PostHog — its own plane, added when the UI work lands, not an
  OTel task here. Browser OTel/RUM is a named follow-up, not day-1.

## Open Questions

The store question (bigger dep for scale?) is RESOLVED as Decision D1
(Matt-ratified). OQ-B (server/runner emission) is RESOLVED — Matt ratified
"otel thru all" + full end-to-end tracing (2026-08-26); see D6/T4b. **OQ-A and
OQ-B' are now RESOLVED too** (2026-08-26): OQ-A by Matt's delegation (billing
records committed as a raw-event contract; the tokens-and-billing model is
captured in [`tokens-and-billing.md`](../../../concepts/tokens-and-billing.md)),
OQ-B' by Matt's PostHog ruling. Both are kept below with their resolutions and
the evidence behind them; the only items still awaiting Matt are the two
confirmation asks flagged inline (the OQ-B' core-seam shape, and the
cross-record OQ1 reconciliation D6 depends on).

- **OQ-A [RESOLVED — Matt delegated 2026-08-26 ("make a decision around that");
  decided: commit the raw-event contract] — billing seam shape.** Matt delegated
  the call and fixed what is being recorded (see the tokens-and-billing concept
  doc): Rigel does not sell tokens — the user brings them (BYOK or their own cloud
  subscription) and all tokens flow through the bundled OMP gateway — so the
  managed service bills for the **compute it brings** (Manager/agent usage caps +
  overage), not for model calls. That makes three distinct recorded quantities,
  and separating them is the decision:
  - **Manager/agent compute usage** — the billing-grade quantity managed caps and
    charges overage on. Because it backs billing it must be exact, auditable, and
    reconstructable, so it is an append-only event, not a lossy counter.
  - **LLM token usage + spend** — recorded off the gateway for the in-product
    usage/spend charts (Plane A) so a user watches their own spend against their
    own keys/subscription; not billed day-1 (a fully-managed-tokens offering,
    which would make it billable, is a named future, not day-1).
  - **Connected cloud-subscription usage/resets** — a monitored quota signal
    (mirrors Rigel's existing OMP-gateway + Grafana quota monitoring), not a
    charge.
  **Decision: the day-1 seam commits the tenant-scoped append-only
  compute-usage raw-event contract** (per D5) — the compute-usage event log (the
  quantity managed bills) is the durable, billing-grade source of truth, rollups
  are derived and rebuildable, and the managed plane builds the billing exporter
  on the committed event shape. The token-usage event (provider/model/tokens/cost
  off the gateway) is the Plane-A display quantity, explicitly NOT the billing
  contract day-1. Billing is NOT severed. This is the usage-based-billing norm:
  OpenMeter ("raw events are what you need for backfills, disputes, migrations";
  store both), Orb ("the source of truth is immutable event history, not lossy
  counters"), Metronome (rollups continuously reconciled against raw-event
  recomputation), Lago (`raw_events` is what aggregation taps), Stripe ("your
  internal metering layer should produce its own rollups; Stripe should not be
  your source of truth — your database is"). The interim gateway lineage the OMP
  gateway descends from persisted a per-request spend row; it is a stopgap being
  removed, and the bundled OMP gateway is the durable **token**-metering point
  (feeding the display quantity). So T1 commits both event schemas, with the
  compute-usage one as the billing-grade contract.
- **OQ-B' [RESOLVED — Matt 2026-08-26; core-seam shape recommended] — UI product
  analytics + browser telemetry.** Matt ruled: **product analytics is PostHog,
  embedded directly in the Solid UI.** Managed Compass is fully instrumented with
  PostHog (Rigel's own PostHog backend); a self-hosted deploy either **disables it
  (default)** or **points it at the deployer's own PostHog**. He weighed and
  declined a vendor-agnostic collection layer ("I don't really think there is a
  standard for that and we'd likely lose some PostHog features"). The research
  backs both the ruling and its cost:
  - **A vendor-neutral frontend-analytics seam exists but forfeits PostHog's
    value.** The de-facto neutral shape is Segment's `analytics.js` event API
    (`track`/`identify`/`page`), which RudderStack implements and can fan out to a
    PostHog destination, so app code *can* stay vendor-agnostic for basic event
    capture. But PostHog's actual value — autocapture, session replay, feature
    flags, surveys, heatmaps — is all proprietary to `posthog-js` and absent from
    the `analytics.js` standard (PostHog's own Segment-destination docs state this
    explicitly). A neutral SDK buys portability for the part that is commodity and
    loses the part that justifies PostHog. **The neutrality the core actually
    needs is different**: never force a self-hoster to run PostHog — met by the
    off-by-default enable plus configurable host below, not by a vendor-agnostic
    SDK.
  - **PostHog and backend OTel are two planes, joined by a correlation key — the
    core builds that seam but does not fan the OTel firehose into PostHog (D7).**
    They are different signals: PostHog's frontend↔backend correlation is its own
    `X-POSTHOG-SESSION-ID`/`X-POSTHOG-DISTINCT-ID` headers, not W3C `traceparent`,
    so they don't merge into one trace — they join by a shared key (trace_id on
    PostHog events via native `$ai_trace_id`; PostHog session id onto backend
    spans). Full backend traces stay on Plane B (Grafana/LGTM); PostHog holds
    product events plus the key. This is the **billing-safe** path: PostHog does
    ingest OTLP (Distributed Tracing — beta July 2026, free while in beta, on both
    Cloud AND self-hosted), but the backend trace firehose is the highest-volume
    signal (~8-15 spans per LLM call) and PostHog has **not announced post-beta
    trace pricing**, so firehosing OTel into PostHog is a real cost risk. The
    collector fan-out to PostHog (J2) is therefore a **managed-plane option behind
    tail-sampling**, off in the core; the core commits the correlation-key join
    (J1) only. See D7 for the full seam.
  - **Browser OTel/RUM: still not in the core now.** Independent of product
    analytics, OTel's own docs call browser client instrumentation "experimental
    and mostly unspecified, subject to breaking change"; the one mature OSS path,
    Grafana Faro, is a heavier commitment. Backend traces correlated by
    request/trace id (T4b) cover the need; browser RUM is a named follow-up, not
    day-1.
  - **Recommended core seam (for Matt to confirm):** the Solid UI embeds
    `posthog-js`, gated by an **off-by-default enable flag plus a configurable
    PostHog host** — unconfigured means no analytics leave the deployment, and a
    self-hoster who wants it points the host at their own PostHog. Managed sets
    both to Rigel's PostHog. PostHog-the-product is a managed-plane and
    opt-in-self-host choice, never a hard core dependency; the core ships only the
    embed and the enable/host seam.
