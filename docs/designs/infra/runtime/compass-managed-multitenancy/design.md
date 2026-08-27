# Compass managed-service multi-tenancy and NATS substrate

Status: Draft

Tracking: RIG-2861

> **Design record.** This designs the tenancy model, the Server↔Runner
> connection topology, and the NATS/eventing substrate placement for Compass as
> a multi-tenant managed service on shared infrastructure. Code citations
> resolve against the working copy at the RIG-2861 branch point (line numbers
> drift as code evolves). Frozen on merge; executing agents read this as the
> contract for RIG-2861.

## Problem / Intent

Compass will ship as a **multi-tenant managed service on shared
infrastructure** — not Server-per-tenant, not isolated per-tenant Runners:
shared infra with per-tenant isolation enforced above the metal. The parent
end-state record already declares Compass "a hosted multi-tenant agent
platform" built on an open-source core
(`docs/designs/infra/runtime/compass-elastic-session-runtime/design.md:19-24`),
with multi-tenancy explicitly the managed-service layer over a
single-tenant-deployable OSS core (same record, the "OSS core and managed
service" section, lines 71-101). The codebase is genuinely single-tenant
today, so the substrate choices below are cheap to build in now and expensive
to retrofit later:

- **No tenant model.** `CreateUser` is a flat insert — `INSERT INTO accounts
  (id, handle, display_name) VALUES ($1, $2, $3)` then a `user_accounts`
  subtype row (`go/internal/store/accounts.go:28-38`, whole method
  `:13-52`). Nothing in `go/internal/store` mentions a tenant.
- **One shared pool backs everything.** "Store is the Postgres store of
  record. It owns a pgx connection pool" (`go/internal/store/store.go:33-37`);
  every store method runs on `s.pool`. Migrations are embedded in the binary
  and applied at `Open` under a single advisory lock
  (`go/internal/store/store.go:18-31`) — a one-database assumption.
- **Single-Runner MVP is baked into the hub.** "Single-Runner MVP — every
  binding belongs to the one enrolled Runner, so a reconnect clears the whole
  map" (`go/internal/runnerhub/hub.go:869-870`; same posture at `:86-89`).
  Session→account bindings are the hub's in-memory maps
  (`go/internal/runnerhub/relay_comms.go:159-184`); the durable half is
  `agent_placements` (`go/internal/store/agent_placements.go:66` — an UPSERT
  keyed on the agent, doc `:33-39`).
- **Delivery fan-out is an in-process bus — the cross-instance gap.** The
  delivery consumer tails the in-process comms event bus (`bus
  *events.Bus[*compassv1.SubscribeCommsResponse]`,
  `go/internal/delivery/consumer.go:148`, constructed at `:222`) and
  dispatches `deliver` controls to live recipients; the durable per-(agent,
  channel) delivery cursor (`go/internal/store/delivery_cursors.go`) advances
  only on recipient ack. The bus is `go/events` — a generic in-process ring +
  live tail (`go/events/events.go:1-13`), instantiated by both comms
  (`go/internal/comms/comms.go:31-37`; "a second instance of the generic
  events.Bus", `go/internal/comms/doc.go:14-16`) and delivery. The instant a
  second Server instance runs, a message posted on instance A must reach a
  session bound on instance B — nothing handles that hop today.

This record answers three coupled questions — tenant data isolation,
Server↔Runner connection topology, and NATS adoption/placement — each with
options, tradeoffs, and a recommendation, plus a staged adoption path so
operational cost is paid when it is earned.

## Approach

### Q1 — tenant data isolation: shared DB + `tenant_id` + Postgres RLS

**Options.**

1. **Separate Postgres database per tenant.** Strongest blast-radius
   isolation: a bad migration, a runaway query, a backup/restore, or a
   compromise of one tenant's DB touches only that tenant. Per-tenant
   encryption keys and compliance-driven data residency are natural.
   Costs: N× everything operational. Migrations are embedded and applied at
   `Open` under one advisory lock (`go/internal/store/store.go:18-31`) — a
   per-tenant-DB fleet turns every schema change into an N-database rollout
   with partial-failure states the codebase has no machinery for. Every
   Server instance needs a pool per tenant (`go/internal/store/store.go:37`
   is one pool today), so connection count scales tenants × instances and
   exhausts Postgres long before compute is the bottleneck. Cross-tenant
   operational queries (fleet health, abuse detection, billing rollups)
   require fan-out tooling.
2. **One shared database, `tenant_id` column + row-level isolation
   (recommended).** One schema, one migration run, one pool per instance.
   Isolation is enforced *in the database* with Postgres row-level security:
   every tenant-owned table carries `tenant_id`, RLS policies filter on a
   per-transaction `SET LOCAL` tenancy setting, and a query that forgets its
   tenant scope returns zero rows instead of another tenant's data —
   fail-closed by construction, not by review vigilance. Costs: weaker
   blast-radius isolation (one cluster's outage, restore, or noisy neighbor
   is every tenant's), per-tenant encryption is application-level rather
   than physical, and RLS discipline must be total (a `BYPASSRLS` role or a
   policy-less table is a hole).
3. **Schema-per-tenant** (middle ground) — rejected; see Alternatives
   considered.

**Recommendation: option 2** for the managed tier, with a **documented escape
hatch to a dedicated database for a compliance-sensitive tenant**. The escape
hatch is cheap on the data side precisely because the shared-DB schema still
carries `tenant_id` everywhere: a dedicated-DB tenant runs the identical
schema with exactly one tenant row, so the data move is clean. It is not
zero code: `Store` owns exactly one pool (`go/internal/store/store.go:37`),
so serving a promoted tenant needs per-tenant pool routing inside `Store` —
keyed on the resolved tenant, a bounded named addition, not a behavioral
fork — rather than a dedicated Server stack (which would drift toward the
rejected Server-per-tenant shape).

Mechanics:

- A new `tenants` table; `accounts` gains a `tenant_id` FK (the account is
  the root of everything else — agent trees hang off `agent_accounts`
  ownership rows created in `CreateAgent`, `go/internal/store/accounts.go`),
  so tables reachable only through an account inherit tenancy through the FK
  chain but still carry a denormalized `tenant_id` where RLS needs a local
  column.
- Tenant identity rides the request context exactly as caller identity does
  today (the actor is "never a request field (spoofable); it is the account
  authenticated on the connection, read from the request context",
  `go/internal/comms/comms.go:10-15`): the auth interceptor resolves token →
  account → tenant, and the store sets the tenancy GUC per transaction.
- **OSS single-tenant stays degenerate, not configured.** The OSS core runs
  with the one bootstrap tenant row auto-created at `Open` (the
  `BootstrapAdmin` pattern, `go/internal/store/accounts.go:54-63`); RLS
  policies are present but every row belongs to the one tenant, so a
  single-tenant deployment pays one extra indexed column and nothing else.
  No `if multiTenant` forks in store code — the RIG-1717 "one architecture,
  two products" seam
  (`docs/designs/infra/runtime/compass-elastic-session-runtime/design.md:71-101`).
- Noisy-neighbor at the DB is handled with per-tenant statement timeouts and
  admission limits at the Server edge, not by physical separation; a tenant
  that outgrows that is the escape-hatch case.

### Q2 — Server↔Runner connection topology: all-to-all over a message-bus fabric

**Options.**

1. **Single Server owns its Runners (ownership/sharding).** Each Runner
   enrolls with exactly one Server instance, which holds its Connect streams
   and all its session bindings — a straight scale-out of today's shape
   (in-memory bindings in the hub, `go/internal/runnerhub/relay_comms.go:3-15`;
   single-Runner assumptions at `go/internal/runnerhub/hub.go:86-89,869-870`).
   Simple to reason about: a session's Server is its Runner's Server.
   Costs: it contradicts RIG-2394's D8/D9 elastic-runtime contract. A
   suspended session must wake on **any** box with capacity
   (`docs/designs/infra/runtime/compass-elastic-session-runtime/microvm-runner.md:762,801-816`),
   so a wake routinely lands on a Runner owned by a *different* Server than
   the one holding the client's stream — every wake becomes a cross-Server
   handoff. (The box-independent durable-volume backend D9 depends on is
   RIG-2485/RIG-2394 work not yet built, so the wake-lands-elsewhere urgency
   is a design-forward assumption, not current behavior.) Server failure
   orphans its whole Runner set until a re-ownership
   protocol completes (a protocol option 1 forces you to invent). Scheduling
   is constrained to the owner's view of capacity, not the fleet's.
2. **All Servers reach all Runners, direct gRPC mesh.** Any Server can drive
   any Runner, satisfying non-sticky wake — but with N Servers × M Runners
   standing Connect streams, per-pair enrollment/reconnect state, and every
   Server duplicating the hub's binding maps for every Runner. The hub's
   in-memory binding model (`SessionForAccount`,
   `go/internal/runnerhub/relay_comms.go:170-184`) does not survive this
   shape: the binding must become shared state anyway, at which point the N×M
   stream mesh is pure overhead.
3. **All-to-all over a message-bus fabric (recommended).** Each Runner holds
   ONE connection — to the bus — and each Server holds ONE. Session commands
   are published to per-Runner subjects; Runner events fan in on per-Server
   or queue-group subjects. Any Server drives any Runner by publishing;
   Runner failover is a resubscribe, Server failover is invisible to Runners.
   The session→Runner routing truth stays durable in Postgres
   (`agent_placements`, `go/internal/store/agent_placements.go:8-22,33-39`) —
   the bus routes, it never owns placement.

**Recommendation: option 3.** RIG-2394's D9 non-sticky wake contract
(`docs/designs/infra/runtime/compass-elastic-session-runtime/microvm-runner.md:801-816`)
*requires* any-Server-drives-any-Runner, and once that is required, the
choice is only between an N×M direct-stream mesh and a hub-and-spoke fabric —
the fabric wins on connection count, failover semantics, and blast radius.
This is exactly where NATS earns its place (Q3). A Server-to-Server
forwarding variant — a genuine middle ground — is weighed in Alternatives
considered; it loses at managed scale but is the recommended **Phase-1**
Runner topology (see Staged adoption path).

What carries over unchanged:

- **The trust model.** The Runner stays a pure forwarder that asserts no
  account: "the account is resolved Server-side from the hub's own binding,
  never asserted by the Runner (transport design Decision #3)"
  (`go/internal/runnerhub/hub.go:234-236`; the fail-closed `CodeNotFound`
  contract, `go/internal/runnerhub/relay_comms.go:7-15,242-249`). On the
  fabric, the binding moves from one hub's RAM to a durable + cached form any
  Server instance can resolve, but resolution stays Server-side and
  fail-closed.
- **Deliver stays the one seam.** The hub was built for this: "a future
  brokered transport replaces only what feeds it — the write-through, the
  registry, and the router are transport-agnostic"
  (`go/internal/runnerhub/hub.go:10-14`). The fabric replaces the feed, not
  the write-through.
- **The agent↔Runner hop is untouched.** RIG-2394 froze it as vsock through
  the attribution boundary
  (`docs/designs/infra/runtime/compass-elastic-session-runtime/microvm-runner.md`);
  NATS is never exposed to the untrusted in-VM side.

Session-affinity nuance: non-sticky does not mean stateless. While a session
is live its placement row pins it to one Runner
(`go/internal/store/agent_placements.go:33-39` — an UPSERT keyed on the
agent), and commands for it route to that Runner's subject. Affinity is a
*routing lookup*, not a *connection ownership*; failover changes a row and a
subject, never a stream topology.

### Q3 — NATS adoption + placement: transport/routing fabric under the async layer, Postgres stays the source of truth

**What NATS is for here — and what it is not.** NATS is an eventing/messaging
substrate UNDER the async layer. It does not replace Connect at the
synchronous RPC edges (Client↔Server RPC stays Connect/gRPC, DL-013), it does
not touch the agent↔Runner vsock hop (RIG-2394), and it never becomes a second
store — Postgres remains the durability source of truth, preserving the
DL-019/DL-020 spirit ("the ring is never a second store; losing it loses no
committed state",
`docs/designs/product/compass-architecture-lineage/design.md:52-56`).

**Candidate roles, judged individually.**

1. **Cross-instance comms event bus (adopt).** Today's fan-out is
   write-through Postgres then publish on the in-process `events.Bus`
   (`go/internal/comms/comms.go:1-16`, `go/internal/comms/doc.go:6-16`). At
   >1 instance the publish additionally goes to a NATS subject per event
   space; every instance's bus bridges NATS → its local ring, so all existing
   subscribers (SubscribeComms, the delivery consumer at
   `go/internal/delivery/consumer.go:143-152`) are untouched. Core NATS
   (at-most-once) suffices: the bus is already loss-tolerant by design — the
   durable delivery cursor + reconnect sweep recover anything the ring
   drops (`go/internal/delivery/consumer.go:3-9`, cursor advance on ack only).
2. **Cross-instance session→account/placement routing (adopt).** The hub's
   in-memory binding maps become instance-local caches over durable truth
   (`agent_placements` + a session-binding table), with NATS carrying
   invalidation/update events so any instance resolves a session without
   polling. Postgres is the arbiter on any cache miss or conflict.
3. **Delivery-worker queue groups (adopt).** The delivery consumer's dispatch
   work partitions across instances with NATS queue-group semantics — each
   posted-message event is claimed by one worker — replacing the implicit
   "the one process does everything" model. The `ControlDispatcher` /
   `SessionResolver` / `DeliveryReads` interfaces
   (`go/internal/delivery/consumer.go:42-63`) are the injection seams; the
   consumer logic above them does not change.
4. **Server↔Runner command/event fabric (adopt — the Q2 topology).**
   Per-Runner command subjects, Runner event fan-in, enrollment heartbeats.
   Request-reply for the RPC-shaped legs (`RelayCommsCall`,
   `CommitConversationFrame`) with the same fail-closed semantics as today
   (`go/internal/runnerhub/relay_comms.go:242-320`).
5. **JetStream durable streams (adopt narrowly — only where no Postgres
   cursor exists).** For comms delivery, JetStream is *rejected*: the durable
   per-(agent, channel) cursor in Postgres
   (`go/internal/store/delivery_cursors.go`, advanced only on recipient ack —
   `go/internal/runnerhub/hub.go:713-724`) is already the at-least-once
   guarantee, and a JetStream consumer cursor beside it would be a second
   source of delivery truth (exactly the dual-store DL-020 forbids).
   JetStream is the right tool only for streams with **no existing Postgres
   cursor and genuinely bus-shaped durability needs**: Runner telemetry
   fan-in and scheduling events (both consumed by RIG-2485), where replay of
   a bounded window beats inventing a new Postgres cursor table per stream.
   Each JetStream adoption is a per-stream decision with the burden of proof
   on JetStream.

**Rejected placements.** NATS as a Client-facing transport (stays Connect);
NATS on the agent↔Runner hop (frozen vsock, RIG-2394); JetStream as the comms
message store (Postgres write-through is the product's structure — the
audit/search substrate rationale behind DL-021,
`docs/designs/product/compass-architecture-lineage/design.md:46-51` — and
that survives this record intact).

**Recommendation.** Adopt NATS as the **cross-instance transport/routing
fabric**: core NATS pub/sub for the comms bus bridge and routing invalidation,
queue groups for delivery workers, per-Runner subjects for the Q2 topology;
JetStream only for telemetry/scheduling fan-in, never for comms delivery.
Postgres remains the sole durability source of truth. Adoption is staged (see
Staged adoption path) so single-instance deployments — including the OSS core
— never pay for a broker they do not run.

## Scope boundary with RIG-2485

RIG-2485 ("Compass managed control plane: box provisioning, session
scheduling, telemetry fan-in") overlaps this record; the lane decision is to
**scope cleanly, not absorb** — recorded here as settled.

**This record (RIG-2861) owns:**

- the tenancy model — data isolation and tenant identity (Q1);
- the Server↔Runner **connection topology** — who drives whom,
  message-bus-vs-N²-gRPC (Q2);
- the NATS/eventing substrate — placement, subject/contract shapes, and the
  staged adoption path (Q3).

**RIG-2485 owns box lifecycle mechanics:** provisioning/deprovisioning KVM
hosts, bin-packing, drain/cordon, dead-box reaping, and the durable-volume
mechanism (building on the P2 volume contract,
`docs/designs/infra/runtime/compass-elastic-session-runtime/design.md:603-653`).

**The seam:** this record defines the Server↔Runner messaging fabric and the
scheduling-event contract at the transport layer (the subjects, the
envelope, and the delivery/ordering semantics scheduling events ride);
RIG-2485 owns the scheduling-event **payload schemas** (what fields a
scheduling event carries) and **consumes** the fabric to place and
reschedule sessions. Neither lane rebuilds the other's half: RIG-2485 does
not define bus subjects or tenancy, and this record does not define
placement policy, bin-packing, box lifecycle, or event payload fields.
RIG-2394's D8/D9 contract (box-independent durable state, suspend-to-durable,
non-sticky wake on any Runner —
`docs/designs/infra/runtime/compass-elastic-session-runtime/microvm-runner.md:762,801-816`;
RIG-2485 is the control plane those rulings defer to) is a hard input to Q2:
the topology chosen here is what makes any-Runner wake routable.

**Repo boundary (RIG-1717):** the tenancy substrate — schema, RLS, and the
fabric seams and implementations (T1-T6) — lands in the OSS
`RigelBuild/compass` core, because the OSS core must run it
single-tenant-degenerate; the managed-service orchestration (tenant
provisioning, billing, box lifecycle) is the private control-plane layer
(RIG-2485 and the managed control plane), consistent with RIG-1717's
one-architecture-two-products split
(`docs/designs/infra/runtime/compass-elastic-session-runtime/design.md:71-101`).

## Substrate independence

The Server↔Runner topology (Q2) and the fabric (Q3) are **agnostic to the box
provisioning substrate** — the metal a Runner runs on. The fabric routes a
command to a Runner by subject and resolves placement from durable Postgres
state (`agent_placements` + session bindings); it never observes whether that
Runner is a process on an elastic bare-metal host, a nested-virt cloud VM, or a
pod on a nested-virt Kubernetes node pool. So the choice of managed box
substrate does not change any Q1/Q2/Q3 answer, interface, or task in this
record.

That substrate choice is nonetheless a live managed-control-plane fork, because
RIG-2394's D2 froze the *managed* path as elastic **bare-metal** (AWS `*.metal`,
GCP `c3-metal`) specifically to dodge the ~10% nested-virtualization tax, and
called a nested cloud VM "a self-inflicted tradeoff, never the managed path"
(`docs/designs/infra/runtime/compass-elastic-session-runtime/microvm-runner.md:671-692`).
Two facts verified this session stale that premise: AWS enabled nested
virtualization on **non-metal** EC2 (C8i/M8i/R8i, no surcharge, 2026-02), and
Azure offers a credit-funded nested-virt AKS node pool — so a nested-virt
**node pool** (running the Runner + cloud-hypervisor as ordinary host
processes, *not* Kata pod-sandbox runtime-class, whose CRI/containerd control
plane cannot carry the agent↔Runner gateway RPC — RIG-2394 D4) is now a real
managed option beside bare-metal. Because that fork spans this record's managed
topology and RIG-2485/RIG-2394-D2's box lifecycle, it is routed to Matt as a
cross-lane decision (RIG-2878), not decided here; it gates only the box layer
*beneath* this substrate-agnostic topology.

## Alternatives considered

### Schema-per-tenant (Q1 middle ground)

One database, one Postgres schema per tenant, `search_path` switching.
Rejected: it inherits per-tenant-DB's N× migration problem (every schema
change runs once per schema, against the embedded-migrations-at-`Open` model,
`go/internal/store/store.go:18-31`) while gaining almost none of its isolation
(same cluster, same WAL, same restore blast radius), and `search_path`-based
scoping is easier to get silently wrong than an RLS policy that fails closed.

### Server-per-tenant (Q1/Q2 radical)

Dedicated Server (and optionally Runner fleet) per tenant. Out of scope by
the issue's premise — shared infrastructure with isolation above the metal —
and rejected on merit: it turns every fleet upgrade into an N-tenant rollout
and forfeits cross-tenant density, the economic driver of the managed service
(`docs/designs/infra/runtime/compass-elastic-session-runtime/design.md:91-94`).

### Postgres LISTEN/NOTIFY as the terminal fabric (Q3)

LISTEN/NOTIFY handles cross-instance comms fan-out with zero new
infrastructure and is adopted here as the **stepping-stone phase** (see Staged
adoption path). It is rejected as the terminal state: payloads are capped at
8KB, notifications are dropped on disconnected listeners (fine for the
loss-tolerant bus, useless for queue semantics), there are no queue groups or
subject hierarchies, and every notification is a store round-trip on the same
cluster the tenancy model is already asking to carry all durable truth —
at managed-service scale the fabric load belongs off the store of record.

### N×M direct gRPC mesh (Q2 option 2)

Weighed in Q2 and rejected: satisfies non-sticky wake but scales standing
streams and per-pair state as tenants × fleet grows, and still forces the
binding maps out of single-hub RAM into shared state — after which the mesh
adds nothing the fabric does not do with two orders of magnitude fewer
connections.

### Server-to-Server forwarding (Q2 middle ground)

A Runner enrolls with exactly **one** Server; the placement/binding rows in
Postgres name the *serving* Server; any other Server forwards a session
command over one internal gRPC hop — an S×S mesh among Servers, not S×M
across the Runner fleet. It satisfies RIG-2394's D9 non-sticky wake
(`docs/designs/infra/runtime/compass-elastic-session-runtime/microvm-runner.md:801-816`):
a Server death is handled by its Runners re-enrolling elsewhere plus a row
update, with no re-ownership protocol to invent. It keeps DL-013 fully
intact (Runner↔Server stays Connect), needs no broker, and composes with the
Phase-1 LISTEN/NOTIFY comms fabric. Real costs: a forwarding-hop latency tax
on every cross-Server command, a Server-failure window while its Runners
re-enroll, S×S mesh state that still grows with the Server fleet, and the
cross-Server command-ordering question (see Global Constraints) — though
forwarding through the serving Server naturally serializes one Runner's
command stream. The bus still wins **at managed scale** — one connection per
party instead of a growing S×S mesh, failover as a resubscribe instead of a
re-enroll window, and no forwarding-latency tax on every cross-Server
command — but forwarding is the honest Phase-1 shape, and the Staged
adoption path adopts it there.

### Collapsing Phase 1 — in-process straight to NATS (Q3)

The staged path interposes a LISTEN/NOTIFY fabric (T4) that the terminal
NATS state discards, and the record's own Phase-2 gate text ("they tend to
arrive together at managed-service scale") doubts the Phase-1 window is
long — so a deployment could skip Phase 1 entirely: in-process → NATS at the
multi-instance boundary, deleting T4, T4b, and the T6-on-Phase-1 edges. The argument for
collapsing: the 2-3-instance window with no delivery partitioning may be too
short to earn a throwaway fabric with its own reconnect/resync machinery.
The counter: a self-host/OSS deployment wanting multi-instance without a
broker would miss LISTEN/NOTIFY — but that class ships Phase 0 as its
permanent default per this record. Resolution (not an open question):
LISTEN/NOTIFY stays the recommended default for the gradual-growth path, and
**T4 is optional** — T6 depends on T3/T5 only (T4 and the Phase-1 Runner
work T4b are stepping-stones the collapsed path skips) — and a deployment expecting
rapid growth past the Phase-2 gate may skip Phase 1 (see Staged adoption
path).

### JetStream as the delivery-durability layer (Q3)

Rejected (detailed in Q3 role 5): the Postgres delivery cursor advanced on
recipient ack (`go/internal/store/delivery_cursors.go`;
`go/internal/runnerhub/hub.go:713-724`) is already the at-least-once
guarantee, and a parallel JetStream consumer cursor would be a second
delivery-truth store — the DL-020 dual-store failure mode.

## Staged adoption path

Operational cost is paid when earned. Each phase has an explicit advancement
gate; a deployment that never crosses a gate never runs the next phase's
infrastructure. The OSS core ships Phase 0 as its permanent default.

### Phase 0 — single instance, in-process bus (today)

The shipped shape: one Server, the generic in-process `events.Bus`
(`go/events/events.go:1-13`) backing SubscribeEvents, SubscribeComms, and the
delivery consumer. No broker, no cross-instance anything. The Phase-0 work in
this record is **seam work plus one single-instance-safe behavior change**:
introduce the `EventFabric` and `RunnerFabric` interfaces (Plan T3/T5) with
in-process implementations, land the tenancy schema (Plan T1/T2), and make
session bindings durable (T5 — a Server restart resolves a pre-restart
session, which fails today) — so later phases are implementation swaps, not
rewrites.

**Gate to Phase 1:** a second Server instance is actually needed — HA
requirement or vertical-scaling ceiling on the managed deployment — not
before.

### Phase 1 — few instances, Postgres LISTEN/NOTIFY stepping-stone

Cross-instance comms fan-out over LISTEN/NOTIFY behind the `EventFabric`
seam: a mutation still writes Postgres first (write-through preserved,
`go/internal/comms/doc.go:9-12`), then NOTIFYs a compact event reference;
each instance's listener re-publishes onto its local ring. Session-binding
invalidation rides the same channel. No new infrastructure — the cluster the
deployment already operates carries the fabric.

**Phase-1 Runner topology: Server-to-Server forwarding** (weighed in
Alternatives considered; owned by Plan T4b). Each Runner enrolls with
exactly one Server; placement/binding rows in Postgres name the serving
Server (T5 makes bindings durable); any other Server forwards a session
command over one internal gRPC hop. This requires generalizing the hub's
single-Runner binding assumption — "a reconnect clears the whole map"
(`go/internal/runnerhub/hub.go:869-870`) — to multi-Runner/multi-Server
enrollment; T4b owns that generalization and the forwarding hop.

A deployment expecting rapid growth past the Phase-2 gate MAY skip this
phase entirely and adopt NATS at the multi-instance boundary (T4 is an
optional stepping-stone — see the collapsing-Phase-1 alternative);
LISTEN/NOTIFY remains the recommended default for the gradual-growth path.

**Gate to Phase 2:** any of — (a) NOTIFY publish latency or the primary's
LISTEN/NOTIFY connection load crosses a deployment-set threshold, surfaced
through the Server's OTel collector (the bundled OTLP fan-in endpoint,
`go/internal/stack/collector_container.go:22-32`; the fabric emitter that
reports it is added as gate-instrumentation work in T4); (b) the
Server↔Runner forwarding-edge count or the re-enroll churn on Server
failure crosses a deployment-set threshold on the same OTel surface (the
emitter reporting it is gate-instrumentation work in T4b; fleet
elasticity under RIG-2485 makes edges churn); (c) the single delivery
consumer instance's dispatch backlog depth stays above a deployment-set
drain threshold (the consumer is one ctx-rooted `Run` loop today,
`go/internal/delivery/consumer.go:255`; its backlog emitter is likewise
gate-instrumentation work). Any one suffices — they tend to arrive together
at managed-service scale.

### Phase 2 — managed scale, NATS fabric

A NATS cluster lands as the transport/routing fabric (Q3 recommendation):

- `EventFabric` swaps LISTEN/NOTIFY for core NATS subjects (per event space,
  tenant-scoped subject prefixes; extending per-tenant isolation to the
  fabric via NATS accounts/permissions is the recommended example — OQ-3
  finalizes the mechanism);
- the delivery consumer's trigger side moves to a queue group (one claimant
  per posted-message event);
- Server↔Runner moves to the Q2 bus topology: per-Runner command subjects,
  event fan-in, request-reply for the RPC-shaped relay legs.

Postgres remains the durability source of truth throughout; the fabric
carries references and routing, and anything the fabric drops is recovered
by the existing cursor sweeps.

**Gate to Phase 3:** a concrete stream exists with no Postgres cursor and a
demonstrated replay need — Runner telemetry fan-in or scheduling events under
RIG-2485 — evidenced by a measured signal: the gap/loss count on that stream
across consumer restarts (core NATS drops what a disconnected subscriber
misses), surfaced through the Server's OTel collector
(`go/internal/stack/collector_container.go:22-32`; the per-stream gap
counter is added as gate-instrumentation work with the stream), exceeding a
deployment-set tolerance where a new Postgres cursor table for that stream
is judged worse than a bounded JetStream window.

### Phase 3 — JetStream, per-stream and narrow

JetStream enabled only for the streams that passed the Phase-3 gate,
per-stream. Comms delivery never moves (its Postgres cursor is the
guarantee). Each adoption records its retention window and its recovery
story; a JetStream stream is never the sole holder of state the product
cannot regenerate.

## Plan

### Global Constraints

- **Postgres is the sole durability source of truth for fabric-carried
  state.** No task may introduce a second store of committed
  comms/routing/session-binding state; the fabric carries transport/routing
  references only, and every fabric recovery path terminates in a Postgres
  cursor or row (preserves DL-019/DL-020's spirit). Transcript bodies are
  out of scope: the blob seam (DL-019) and the two-tier transcript archive
  (DL-093, `docs/designs/DECISIONS.md:95`) remain their durability owners.
- **Fail-closed identity everywhere.** Tenant and account resolution follow
  the existing posture: identity is never a request field, always resolved
  server-side from the authenticated connection
  (`go/internal/comms/comms.go:10-15`); an unresolvable session or tenant is
  `CodeNotFound`/zero rows, never a fallback
  (`go/internal/runnerhub/relay_comms.go:7-15`).
- **The OSS core stays single-tenant-degenerate.** No `if multiTenant`
  behavioral forks; single-tenant runs the same schema with one bootstrap
  tenant row and the Phase-0 in-process fabric. No broker is required to run
  the OSS core.
- **The agent↔Runner hop is frozen.** vsock through the attribution boundary
  (RIG-2394); no task exposes NATS to the in-VM side.
- **Seams before swaps.** Every fabric change lands behind an interface with
  the in-process implementation first (the hub's own posture,
  `go/internal/runnerhub/hub.go:10-14`); a phase advance swaps an
  implementation, never rewrites a consumer.
- **Migrations stay embedded-and-ordered** (`go/internal/store/store.go:18-31`);
  tenancy DDL rides the same mechanism.
- **Cross-Server command ordering on one Runner subject must be safe.**
  Per-Runner command subjects with multiple Server publishers guarantee
  only per-publisher ordering in NATS, while today's Sessions stream is a
  single ordered stream per Runner. Either commands for one session are
  serialized through its placement-owning path (the forwarding shape does
  this naturally), or the relay legs are idempotent/order-independent — a
  required invariant with a required test (T6).
- **Session-level `SET` of the tenancy GUC is banned.** All tenant scoping
  goes through `beginTenantTx` (`SET LOCAL` inside an explicit
  transaction); a session-level `SET` leaks tenant identity across pgxpool
  checkouts.
- **No transaction-pooling proxy.** The design assumes a direct
  `pgxpool → Postgres` connection; a transaction-pooling proxy (pgbouncer
  transaction mode) breaks Phase-1 LISTEN/NOTIFY (which needs a real
  session) and complicates the GUC discipline. A later record may design
  for one; until then its absence is a stated invariant.

### T1 — tenant schema + identity plumbing

Add the `tenants` table, `tenant_id` on `accounts` (and denormalized onto
tenant-owned tables RLS will police), the bootstrap-tenant seed at `Open`
(mirroring `BootstrapAdmin`, `go/internal/store/accounts.go:54-63`), and the
context seam that carries the resolved tenant.

- **Interfaces:** consumes the existing migration mechanism
  (`go/internal/store/store.go:18-31`). Produces: migration `NNNN_tenants.sql`
  (`tenants(id, slug, display_name, created_at_unix_ms)`; `accounts` gains
  `tenant_id TEXT NOT NULL REFERENCES tenants(id)`);
  `store.TenantID` (string newtype beside `store.AccountID`);
  `func (s *Store) BootstrapTenant(ctx context.Context) (TenantID, error)`;
  `func WithTenant(ctx context.Context, t TenantID) context.Context` +
  `func TenantFromContext(ctx context.Context) (TenantID, bool)` in the store
  package (pattern: comms `actorFromContext`,
  `go/internal/comms/comms.go:10-15`).
- **Test cycle:** migration applies on fresh + existing DBs; `CreateUser`
  under a tenant context stamps the row; single-tenant boot seeds exactly one
  tenant idempotently.

### T2 — RLS enforcement + per-transaction tenant scoping

RLS policies on every tenant-owned table, filtering on a per-transaction GUC
the store sets from the context tenant; the store's application role runs
without `BYPASSRLS`. Four hardening requirements are part of this task's
definition, not follow-ups:

- **`FORCE ROW LEVEL SECURITY` per policied table.** The store role applies
  migrations at `Open` (`go/internal/store/store.go:18-31`), so it *owns*
  every table — and Postgres table owners bypass RLS by default regardless
  of `BYPASSRLS`. Without `FORCE`, the policies are silently inert for the
  exact role they constrain; "runs without `BYPASSRLS`" alone does not
  close the hole.
- **GUC-unset semantics.** `current_setting('compass.tenant_id')` on a
  never-set connection raises error 42704 (not zero rows), and a
  session-level `SET` (the banned form, Global Constraints) would leave the
  value behind across pgxpool checkouts — `SET LOCAL` itself is
  transaction-scoped and resets at COMMIT/ROLLBACK. The policy must use
  `current_setting('compass.tenant_id', true)` (missing_ok) plus an
  explicit non-empty guard. That guard is defense against the never-set
  NULL case under missing_ok and any session `SET`/`set_config` that leaves
  an *empty* value; a session `SET` that installs a *non-empty* tenant id is
  prevented by the `SET LOCAL`-only ban (Global Constraints), not by the
  guard — the ban is the real defense against a mis-scoped non-empty leftover.
- **`SET LOCAL` only.** Session-level `SET` of the GUC is banned (Global
  Constraints); all tenant scoping goes through `beginTenantTx`.
- **The refactor is whole-store, and bounded.** Many store methods run a
  bare `s.pool.Exec`/`Query` with no transaction today (e.g.
  `RecordAgentPlacement`, `go/internal/store/agent_placements.go:66`, and
  broadly across `accounts.go`, `messages.go`, `delivery_cursors.go`);
  `beginTenantTx` converts each into Begin + `SET LOCAL` + query + Commit,
  roughly two extra round-trips per call. Hot-path mitigation: batch the
  `SET LOCAL` (or `set_config(...)`) with the query in one round-trip via
  pgx batching, so tenant scoping does not double every store call's
  latency.

- **Interfaces:** consumes T1's `TenantFromContext`. Produces: migration
  `NNNN_rls.sql` (`ALTER TABLE … ENABLE ROW LEVEL SECURITY` and
  `ALTER TABLE … FORCE ROW LEVEL SECURITY`, plus one policy per table of
  the shape `USING (current_setting('compass.tenant_id', true) <> '' AND
  tenant_id = current_setting('compass.tenant_id', true))`);
  `func (s *Store) beginTenantTx(ctx context.Context) (pgx.Tx, error)` —
  `Begin` + `SET LOCAL compass.tenant_id`, batched with the first query in
  one round-trip — threaded through every existing store method (all run
  on `s.pool` today, `go/internal/store/store.go:33-37`).
- **Test cycle:** a `pgtest` suite proving cross-tenant reads return zero
  rows, cross-tenant writes fail, a never-set connection fails closed (no
  rows, never all rows, never an error-42704 escape), and a pooled
  connection previously scoped to tenant A and reused without scoping does
  not see tenant A's rows; plus an owner-role probe proving
  `FORCE ROW LEVEL SECURITY` is in effect (the migration-applying role
  cannot read cross-tenant).

### T3 — `EventFabric` seam + in-process implementation (Phase 0)

Extract the cross-instance fan-out seam above the in-process bus so comms and
delivery publish through it.

- **Interfaces:** consumes `go/events.Bus`
  (`go/events/events.go:1-13`) and the comms write-through publish sites
  (`go/internal/comms/doc.go:9-12`). Produces:
  `package fabric` with
  `type EventFabric interface { Publish(ctx context.Context, subject string, ref EventRef) error; Subscribe(ctx context.Context, subject string, fn func(EventRef)) (Unsubscribe, error) }`
  where `EventRef` is a compact reference (event kind + row id + tenant),
  never a payload copy — subscribers re-read Postgres; and
  `fabric.InProcess` (a no-op loopback: publish calls local subscribers
  synchronously with the ring as today).
- **Test cycle:** comms + delivery behavior unchanged under `InProcess`
  (existing suites green); a two-fabric-instance test proves the seam carries
  a publish across instances (with a channel-backed test fabric).

### T4 — LISTEN/NOTIFY fabric (Phase 1)

`fabric.PgNotify`: publish = `NOTIFY` a channel with the serialized
`EventRef`; each instance runs one listener connection re-publishing onto its
local ring. Session-binding invalidation for T5 rides the same fabric.

- **Interfaces:** consumes T3's `EventFabric` + the store pool. Produces
  `fabric.NewPgNotify(pool *pgxpool.Pool, log *slog.Logger) *PgNotify`;
  a reconnect loop with a full local-ring resync on listener reconnect
  (dropped NOTIFYs are recovered by the same cursor sweeps that recover ring
  loss, `go/internal/delivery/consumer.go:3-9`).
- **Test cycle:** `pgtest` two-instance test — post on A, deliver to a
  session resolved on B; listener kill/reconnect recovers via sweep, no
  message loss (cursor advances exactly once).

### T4b — Server-to-Server command forwarding (Phase 1)

The Runner half of Phase 1 that T4 (the comms half) does not cover: each
Runner enrolls with exactly one Server, and any other Server forwards a
session command over one internal gRPC hop to the serving Server, which
drives its locally-enrolled Runner (the Server-to-Server forwarding shape,
Alternatives considered). This generalizes the hub's single-Runner binding
assumption — "a reconnect clears the whole map"
(`go/internal/runnerhub/hub.go:869-870`) — to multi-Runner/multi-Server
enrollment.

- **Interfaces:** consumes T5's durable session bindings (the
  `session_bindings` and `agent_placements` tables,
  `go/internal/store/agent_placements.go:33-39`) and
  the hub's enrollment/binding surface (the single-Runner re-enroll posture,
  `go/internal/runnerhub/hub.go:86-89`; the whole-map-clear assumption to
  generalize, `:869-870`; today's command router, `:925-938`). Produces: a
  serving-Server column on the session/placement binding (which Server
  currently holds a Runner's enrollment); a `RunnerFabric`
  direct-Connect+forwarding implementation that, on a command for a session
  whose serving Server is another instance, forwards over one internal gRPC
  hop to that Server; the generalization of the hub binding maps beyond
  the single-Runner assumption; and the OTel emitter reporting
  forwarding-edge count and Server-failure re-enroll churn (the Phase-2
  gate-(b) instrumentation).
- **Test cycle:** cross-Server command routing (a command issued on instance
  A for a session served by instance B forwards and lands); per-Runner
  command serialization through the placement-owning path (the cross-Server
  ordering invariant, Global Constraints — no steer-after-start reorder);
  Server-failure re-enroll window (a dead serving Server's Runners re-enroll
  elsewhere and the rows update, fail-closed until they do).
- Depends: T5 (durable bindings). Like T4, this is a Phase-1 stepping-stone
  a deployment may skip by jumping straight to the Phase-2 NATS bus (the
  collapsing-Phase-1 path).

### T5 — durable session bindings + `RunnerFabric` seam

Move the hub's session→account/Runner binding truth from single-hub RAM to
Postgres (beside `agent_placements`), with the hub's maps demoted to a cache
invalidated over the `EventFabric`; define the `RunnerFabric` interface the
Q2 topology lands behind.

- **Interfaces:** consumes `agent_placements`
  (`go/internal/store/agent_placements.go:33-39`) and the hub binding surface
  (`bindContainer`/`promoteSession`/`unbindSession`,
  `go/internal/runnerhub/relay_comms.go:30-157`). Produces: migration
  `NNNN_session_bindings.sql`
  (`session_bindings(session_id PK, agent_account_id FK, runner_id, bound_at_unix_ms)`);
  store methods `RecordSessionBinding` / `ResolveSessionAccount` /
  `DeleteSessionBinding` (fail-closed `ErrNotFound`); and
  `type RunnerFabric interface { SendCommand(ctx context.Context, runnerID string, cmd *compassv1internal.SessionsResponse) error; Events(ctx context.Context) (<-chan RunnerEvent, error) }`
  with the direct-Connect implementation wrapping today's command router
  (`go/internal/runnerhub/hub.go:925-938`).
- **Test cycle:** a Server restart resolves a pre-restart session's account
  from the durable binding (fails today); the fail-closed contract holds — a
  stopped/unbound session is `CodeNotFound` exactly as
  `go/internal/runnerhub/relay_comms.go:242-249` specifies.

### T6 — NATS fabric implementations (Phase 2)

`fabric.NATS` implementing `EventFabric` (tenant-prefixed subjects; NATS
accounts/permissions per tenant is the recommended, non-normative example —
OQ-3 finalizes the fabric-isolation mechanism) and `RunnerFabric` (per-Runner
command subjects, event fan-in, request-reply for `RelayCommsCall` /
`CommitConversationFrame` legs preserving their status-code contracts,
`go/internal/runnerhub/relay_comms.go:242-320`); delivery trigger moves to a
queue group behind the existing consumer interfaces
(`go/internal/delivery/consumer.go:42-63`).

- **Interfaces:** consumes T3/T5 seams + a NATS cluster (deployment owned by
  infra; connection config via server flags/env). Produces the two fabric
  implementations plus subject-naming doc
  (`compass.<tenant>.comms.<kind>`, `compass.runner.<runner_id>.cmd`,
  `compass.runner.events` queue-grouped) as a supporting file beside this
  record. **Ordering invariant (Global Constraints):** a per-Runner command
  subject with multiple Server publishers gives only per-publisher ordering
  in NATS, where today's Sessions stream is one ordered stream per Runner —
  so commands for one session MUST be serialized through its
  placement-owning path, or the relay legs MUST be
  idempotent/order-independent; T6 picks and implements one.
- **Test cycle:** integration suite against an embedded/test NATS server:
  cross-instance deliver, queue-group single-claim, Runner failover
  (resubscribe re-routes commands), fabric outage degrades to sweep-recovered
  delivery with zero committed-state loss, and the ordering invariant —
  steer-after-start published from two Server instances against one session
  must not reorder.

### T7 — JetStream telemetry/scheduling streams (Phase 3, gated)

Only after the Phase-3 gate: JetStream streams for Runner telemetry fan-in
and scheduling events, consumed by RIG-2485.

- **Interfaces:** consumes T6's NATS cluster + RIG-2485's event payload
  definitions (that lane owns them). Produces stream/retention configs and a
  thin publisher on the Runner-event path; explicitly NOT a delivery-cursor
  replacement.
- **Test cycle:** bounded-window replay after consumer restart; stream loss
  degrades to live-only telemetry, never state loss.

## Tasks

- [ ] **T1** — tenant schema + identity plumbing (`tenants`, `tenant_id`,
      bootstrap seed, context seam)
- [ ] **T2** — RLS enforcement + per-transaction tenant scoping (depends: T1)
- [ ] **T3** — `EventFabric` seam + in-process implementation (Phase 0)
- [ ] **T4** — LISTEN/NOTIFY fabric (Phase 1; depends: T3) — OPTIONAL
      stepping-stone a deployment may skip (see the collapsing-Phase-1
      alternative)
- [ ] **T4b** — Server-to-Server command forwarding + multi-Runner
      enrollment generalization (Phase 1; depends: T5) — OPTIONAL
      stepping-stone a deployment may skip (see the collapsing-Phase-1
      alternative)
- [ ] **T5** — durable session bindings + `RunnerFabric` seam (Phase 0;
      depends: T3)
- [ ] **T6** — NATS `EventFabric` + `RunnerFabric` + delivery queue groups
      (Phase 2; depends: T3, T5, and the Phase-2 gate; T4 and T4b are
      Phase-1 stepping-stones required only on the gradual-growth path —
      the collapsed path skips both)
- [ ] **T7** — JetStream telemetry/scheduling streams (Phase 3; depends: T6,
      the Phase-3 gate, and RIG-2485 payload contracts)

## Ledger impact

The driver flips `docs/designs/DECISIONS.md`; this section is the verbatim
delta. New rows take the next free `DL-<n>` IDs at flip time (append-only);
they are named N1-N4 here only for cross-reference.

### Proposed row adds

- **N1 (Storage):** "Managed-tier tenant isolation is one shared Postgres
  database with a `tenant_id` column and row-level security enforced per
  transaction (fail-closed GUC scoping); a compliance-sensitive tenant may be
  promoted to a dedicated database running the identical schema (escape
  hatch, not a fork); the OSS core runs the same schema single-tenant with
  one bootstrap tenant row" — Record: this record, §Q1.
- **N2 (Topology & tiers):** "Server↔Runner topology at managed scale is
  all-Servers-to-all-Runners over a message-bus fabric (one connection per
  party to the fabric), never single-Server-owns-Runners and never an N×M
  direct-stream mesh; session→Runner routing truth stays durable in Postgres
  (`agent_placements` + session bindings), and the Runner remains a pure
  forwarder with Server-side fail-closed account resolution" — Record: this
  record, §Q2.
- **N3 (Transport):** "NATS is adopted as the cross-instance
  transport/routing fabric under the async layer — comms event fan-out
  bridge, session-binding invalidation, delivery-worker queue groups, and the
  Server↔Runner command/event fabric — via the staged path single-instance
  in-process → Postgres LISTEN/NOTIFY → NATS, each phase behind an
  advancement gate; Connect stays the synchronous RPC edge and the
  agent↔Runner hop stays vsock (RIG-2394)" — Record: this record, §Q3 +
  §Staged adoption path. Supersedes DL-014 and DL-021.
- **N4 (Storage):** "Postgres is the sole durability source of truth for
  fabric-carried state (comms, routing, session bindings): the fabric
  carries transport/routing references only, every fabric recovery path
  terminates in a Postgres cursor or row, and JetStream is admitted
  per-stream only where no Postgres cursor exists (telemetry / scheduling
  fan-in) — never for comms delivery. This does not disturb the existing
  transcript-body blob seam (DL-019) or the two-tier transcript archive
  (DL-093), which remain the durability owners for transcript bodies" —
  Record: this record, §Q3 role 5 + §Phase 3. Supersedes DL-019, re-scoping
  ONLY its "JetStream is comms-only" clause; the store-of-record and
  transcript-blob clauses survive verbatim.

### Proposed supersessions (Active rows, Matt-ruled — see Open Questions OQ-1)

- **DL-014** ("NATS/JetStream is not a Client/Runner-facing transport; it is
  comms-internal only", `docs/designs/DECISIONS.md:66`) → Superseded by N3.
  The Q2/Q3 fabric IS Runner-facing transport (trusted control-plane tier
  only); the Client edge and the in-VM hop stay NATS-free, which N3 restates.
- **DL-019** ("Postgres is the store of record; transcript bodies live in
  object storage behind a blob seam; JetStream is comms-only",
  `docs/designs/DECISIONS.md:76`) → Superseded by N4, which re-scopes ONLY
  the "JetStream is comms-only" clause; the store-of-record and
  transcript-blob-seam clauses survive verbatim in the successor row.
- **DL-021** ("The comms substrate is Postgres write-through fan-out, not a
  swappable NATS-backed seam", `docs/designs/DECISIONS.md:78`) → Superseded
  by N3. The write-through half survives — a mutation still commits to
  Postgres before any fan-out (`go/internal/comms/doc.go:9-12`) and comms
  stays first-party — but the "not a swappable NATS-backed seam" half is
  exactly what the `EventFabric` seam reverses for the cross-instance hop.

### Rows that survive untouched

- **DL-020** (in-memory bus is a cache/fan-out ring, never a second store,
  `docs/designs/DECISIONS.md:77`) — actively preserved: the fabric extends
  the ring's loss-tolerant cache role across instances; N4 restates the
  single-store invariant.
- **DL-093** (two-tier transcript archive: Postgres hot-tail pruned at
  flush + S3-compatible object-store cold archive,
  `docs/designs/DECISIONS.md:95`) — N4 explicitly preserves it: the archive
  remains a durability owner for transcript bodies; the
  fabric-carried-state invariant never touches it.
- **DL-015 / DL-017** (the in-container agent talks to the Runner alone over
  the per-container socket / `AgentGateway` — `Publish` telemetry stream +
  `Control` server-stream, `docs/designs/DECISIONS.md:67,69`) — untouched by
  construction: the `EventFabric` terminates at the Runner and the untrusted
  in-VM agent hop stays off it (RIG-2394 D8 names NATS a bad fit for that
  attribution-boundary hop,
  `docs/designs/infra/runtime/compass-elastic-session-runtime/microvm-runner.md:795-798`).
  No task in this record extends the fabric past the Runner into the agent hop.

DL-013 ("Transport is gRPC everywhere (Client↔Server and Runner↔Server),
authenticated by per-Runner provisioned tokens",
`docs/designs/DECISIONS.md:65`) does NOT sit here: its Runner↔Server clause
needs a first-class supersession decision either way — see OQ-5.

## Open Questions

### OQ-1 (load-bearing) — supersede DL-014/019/021?

These are Matt-ruled Active decisions (2026-07-04/06) that directly forbid
what Q3 recommends: DL-021 says the comms substrate is "not a swappable
NATS-backed seam"; DL-014/019 confine NATS/JetStream to "comms-internal
only". Introducing NATS as the cross-instance comms/routing fabric and the
Server↔Runner control-plane fabric is precisely reopening them — permitted
under the RIG-2675 nothing-frozen posture, but the supersession call is
Matt's, not this record's.

**The fork, framed precisely:** adopt NATS as the *transport/routing fabric
under the async layer* — with Postgres still the sole durability source of
truth (preserving the DL-019/DL-020 spirit: write-through first, every
recovery path a Postgres cursor, the fabric loses nothing committed) — or
keep the DL-014/019/021 posture and scale on Postgres LISTEN/NOTIFY plus a
direct-stream Runner mesh indefinitely.

**Recommendation:** supersede DL-014 and DL-021 via N3, and DL-019 via N4
(which re-scopes only the JetStream clause and restates the surviving
store-of-record and blob-seam clauses); DL-020 and DL-093 survive untouched.
DL-013's Runner↔Server clause is its own first-class fork — OQ-5.

### OQ-2 (load-bearing) — RLS as the enforcement mechanism, or application-level scoping?

Q1's shared-DB recommendation is firm, but the enforcement layer has a real
fork: Postgres RLS with a per-transaction GUC (recommended — fail-closed in
the database, survives a store-code bug) vs application-level `WHERE
tenant_id = $n` discipline (simpler, no GUC/`SET LOCAL` overhead on every
transaction, no RLS planner surprises on hot paths, but one forgotten
predicate is a cross-tenant leak). **Recommendation: RLS**, accepting the
per-transaction `SET LOCAL` cost and planner care on hot queries; the
failure mode of the alternative is a silent data breach, the failure mode of
RLS is a measurable slowdown.

### OQ-3 (non-load-bearing, deferred) — NATS deployment shape at Phase 2

Cluster sizing, placement (co-located with Servers vs dedicated), and
whether per-tenant isolation uses NATS accounts or subject-permission
scoping alone. Deferred with rationale: the fabric carries transport/routing
references only (event kind + row id + tenant — N4), and the durable tenant
boundary is RLS on the Postgres re-read, so a cross-tenant `EventRef`
delivered to the wrong instance re-reads under that instance's *request-path*
RLS scope and returns zero rows (the system-role background paths of OQ-4
read cross-tenant by design, but expose data only onto a recipient's own
tenant-scoped connection — the user-facing exposure boundary stays the
RLS-scoped request read);
not the primary tenant boundary, and no interface in the Plan changes with
the answer (`fabric.NATS` config is deployment input). The Phase-2 gate is
not near; RIG-2485's telemetry volume estimates should inform it. The "NATS
accounts/permissions per tenant" wording in T6 and Phase 2 is a
non-normative recommended example — OQ-3 finalizes that choice.

### OQ-4 (load-bearing) — cross-tenant system paths under RLS

The background/system loops run with no tenant in context and are
cross-tenant by design: the delivery consumer's ctx-rooted `Run` loop tails
the bus and sweeps delivery cursors for all tenants
(`go/internal/delivery/consumer.go:255`), the deliver-ack cursor advance
(`go/internal/runnerhub/hub.go:713-724`), reattach recovery reads over
`agent_placements`, and the lag-resync sweep over every live binding
(`go/internal/delivery/consumer.go:53-55`). Under T2 as specced — RLS on
every tenant-owned table, an app role without `BYPASSRLS`, missing tenant
fails closed to zero rows — every one of these sees zero rows and delivery
halts fleet-wide. This is a load-bearing design decision, not an
implementation detail. Options:

1. **A policy-exempt system role** used only by the background loops —
   reintroduces a standing bypass credential on hot paths.
2. **Per-tenant iteration in every background loop** — no bypass anywhere,
   but it changes the delivery consumer's complexity class (every sweep
   becomes tenants × work).
3. **A system-context GUC / OR-clause in every policy** — keeps one role,
   but weakens every policy with a second acceptance path.

**Recommendation: option 1** — a narrowly-scoped system role granted only
to the background workers, never the request path: the bypass surface is a
named role with a named consumer list, auditable, and every request-path
query stays fail-closed. Its failure surface differs from OQ-2's (a leaked
system credential reads everything; a forgotten predicate leaks one query),
which is why this is Matt's call, not the record's.

### OQ-5 (load-bearing) — DL-013's Runner↔Server clause needs a successor

DL-013 verbatim: "Transport is gRPC everywhere (Client↔Server and
Runner↔Server), authenticated by per-Runner provisioned tokens"
(`docs/designs/DECISIONS.md:65`). N2's own "one connection per party to the
fabric" retires the Runner↔Server Connect edge at Phase 2 — enrollment, the
Sessions command stream, and PublishEvents all move to the fabric — so
DL-013's per-Runner-provisioned-token authn clause needs a successor either
way; "assumes composing" is not tenable. Two variants:

- **Variant A — supersede DL-013's Runner clause.** The Runner holds one
  fabric connection; define fabric-native Runner authn: per-Runner NATS
  credentials, a subject-permission mapping to the enrollment-token model,
  and the anti-spoofing analysis — what stops Runner A publishing on Runner
  B's event subject and spoofing the fail-closed Server-side binding
  resolution.
- **Variant B — keep a reduced Connect enrollment/authn edge.** The Runner
  authenticates via Connect and receives NATS credentials as an enrollment
  artifact; DL-013 survives in reduced form (Client↔Server gRPC + a
  Runner↔Server enrollment/authn edge) and only the bulk command/event
  traffic moves to the fabric.

**Recommendation: Variant B** — it reuses the existing per-Runner token
trust anchor and keeps the authn story continuous, at the cost of a
residual Connect edge. This composes with OQ-1's supersession decision:
under Variant A, DL-013 joins the supersession list; under Variant B it is
amended, not retired.
