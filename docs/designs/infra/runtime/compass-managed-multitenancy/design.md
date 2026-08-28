# Compass multi-tenancy and NATS eventing substrate

Status: Draft

Tracking: RIG-2861

> **Design record.** This designs a core capability of the Compass OSS
> product: the tenancy model, the Server↔Runner connection topology, and the
> NATS eventing substrate that make one Compass deployment multi-tenant,
> horizontally scalable across Server instances, and HA-capable. Code
> citations resolve against the working copy at the RIG-2861 branch point
> (line numbers drift as code evolves). Frozen on merge; executing agents
> read this as the contract for RIG-2861.

## Problem / Intent

Compass's core must support **multiple tenants in one deployment** and
**scale horizontally across Server instances** — shared infrastructure with
per-tenant isolation enforced above the metal, and any Server instance able
to serve any request. These are OSS-core capabilities: they make the public
product multi-tenant, scalable, and HA-capable in its own right. The managed
service — the hosted multi-tenant platform the parent end-state record
declares (`docs/designs/infra/runtime/compass-elastic-session-runtime/design.md:19-24`),
with multi-tenancy the managed layer over a single-tenant-deployable OSS core
(same record, the "OSS core and managed service" section, lines 71-101) — is
a **consumer and operator** of this core, not its subject. The codebase is
genuinely single-tenant and single-instance today, so the substrate choices
below are cheap to build in now and expensive to retrofit later:

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
- **Delivery fan-out is an in-process ring — the cross-instance gap.** The
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
Server↔Runner connection topology, and the eventing substrate — each with
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

**Recommendation: option 2** for the multi-tenant tier, with a **documented
escape hatch to a dedicated database for a compliance-sensitive tenant**. The
escape hatch is cheap on the data side precisely because the shared-DB schema
still carries `tenant_id` everywhere: a dedicated-DB tenant runs the identical
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

#### Neon as the product database

The managed product database is Neon, which is standard Postgres — RLS,
`CREATE ROLE`, `FORCE ROW LEVEL SECURITY`, and `current_setting()` are all
core Postgres features present there. Nothing in the Q1 design changes for
Neon; the design was Neon-correct by construction, and the alignment is worth
stating precisely because it is load-bearing:

- **The pooled endpoint is PgBouncer in transaction mode.** In transaction
  mode a physical backend connection is shared across tenants between
  transactions, so a session-level `SET` of the tenancy GUC **leaks across
  tenants** — while `SET LOCAL` / `set_config(..., true)` is
  transaction-scoped and safe. That is exactly the discipline this record
  already mandates and exactly the alternative it already bans (Global
  Constraints): our correctness choice is Neon's hard requirement. The same
  code runs unchanged against Neon's pooled and direct endpoints.
- **Roles.** Neon grants `neon_superuser`, not true superuser — which is
  fine, because the design requires a dedicated **non-superuser application
  role** that cannot bypass RLS. `FORCE ROW LEVEL SECURITY` per table (T2)
  still applies: the app role owns the tables it migrates.
- **Per-statement GUC evaluation.** RLS policies must wrap the GUC read in a
  scalar subquery — `(SELECT current_setting('compass.tenant_id', true))` —
  so the planner evaluates it once per statement instead of once per row.
  T2's policy shape carries this.
- **Branching is a dev/test tool, not a tenancy mechanism.** Neon's
  copy-on-write branches are valuable for per-PR preview databases and for
  running the T2 RLS spill suite against a branch of production shape — a
  materially stronger test than a synthetic fixture. But a branch is **not**
  tenant isolation: production tenancy stays RLS-in-one-database, and
  "a branch per tenant" must never be conflated with the shared-pool model.
- **Against Neon Authorize.** Neon's RLS-from-JWT feature
  (`pg_session_jwt`) targets edge clients talking directly to Postgres. Compass
  has an application-server tier that resolves and sets tenant context
  server-side (`go/internal/comms/comms.go:10-15` is the identity posture);
  moving tenant-context authority into a JWT consumed by the database would
  bypass that tier. Recommendation: do not adopt it — tenant-context
  authority stays at the Server tier.
- **The serverless-driver caveat does not apply.** Neon's HTTP-mode
  serverless driver cannot run `SET LOCAL` (no interactive transactions);
  the Compass Server is Go on pgx over the Postgres wire protocol
  (`go/internal/store/store.go:33-37`), so native transactions and
  `SET LOCAL` work normally. If an edge/JS component ever talks to the
  database directly, that is a design change to be raised — not a supported
  path.

### Q2 — Server↔Runner connection topology: load-balanced Servers, subject-addressable Runners

The topology has two distinct shapes for two distinct parties. **Servers are
stateless, standard L7-load-balanced instances**: any Server handles any
client request behind an ALB, and nothing about a Server instance is special
to a tenant or a session — except the live client sockets it happens to hold.
**Runners are individually subject-addressable over the fabric**: a command
for a session must reach the one Runner hosting it, so Runners are addressed,
never balanced.

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
2. **Every Server reaches every Runner over a direct gRPC mesh.** Any Server
   can drive any Runner, satisfying non-sticky wake — but with N Servers × M
   Runners standing Connect streams, per-pair enrollment/reconnect state, and
   every Server duplicating the hub's binding maps for every Runner. The
   hub's in-memory binding model (`SessionForAccount`,
   `go/internal/runnerhub/relay_comms.go:170-184`) does not survive this
   shape: the binding must become shared state anyway, at which point the N×M
   stream mesh is pure overhead.
3. **A message-bus fabric (recommended).** Each Runner holds ONE connection —
   to the fabric — and each Server holds ONE. Session commands are published
   to per-Runner subjects; Runner events fan in on queue-group subjects. Any
   Server drives any Runner by publishing; Runner failover is a resubscribe,
   Server failover is invisible to Runners. The session→Runner routing truth
   stays durable in Postgres
   (`agent_placements`, `go/internal/store/agent_placements.go:8-22,33-39`) —
   the fabric routes, it never owns placement.

**Recommendation: option 3.** RIG-2394's D9 non-sticky wake contract
(`docs/designs/infra/runtime/compass-elastic-session-runtime/microvm-runner.md:801-816`)
*requires* any-Server-drives-any-Runner, and once that is required, the
choice is only between an N×M direct-stream mesh and a hub-and-spoke fabric —
the fabric wins on connection count, failover semantics, and blast radius.
This is exactly where NATS earns its place (Q3).

**The three-hop model.** How a client request becomes a delivered event
across N Servers, and which hop each balancing mechanism owns:

1. **Client → ALB → any Server** (synchronous RPC). Standard L7 load
   balancing over HTTP; Servers are stateless, so any instance handles any
   request. NATS is not in this path.
2. **Internal event processing → NATS queue group** (competing consumers).
   When a message is posted or a Runner publishes an event, every Server
   instance subscribes to the subject under one queue-group name, and NATS
   delivers each message to **exactly one** instance. NATS does this
   balancing itself — the load balancer is not in this path. This is the
   "process each posted message once across N Servers" work: dedup, delivery
   cursor persistence, recipient resolution.
3. **Delivery to a live client → per-connection subject** (addressed, not
   balanced). A client's live subscription stream is stateful and pinned to
   the one Server holding that socket; that Server subscribes to a
   per-connection subject (`client.<sessionID>` shape — a plain subject,
   never a queue group). When the delivery worker on hop 2 needs to reach a
   live recipient, it publishes to that subject, and only the Server holding
   the socket receives it and pushes down the wire.

The classic gotcha — "the client is connected to Server A via the load
balancer, but the event landed on Server B" — is solved by hop 3: B does the
*processing*, then routes the *delivery* to A over A's per-connection
subject. The load balancer only ever sees hop 1. The same addressed-vs-
balanced split applies to Runners: per-Runner subjects to address a specific
Runner, queue groups to share Runner-event processing across Servers.

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

### Q3 — the eventing substrate: one NATS `EventFabric`, embedded or clustered; JetStream as the delivery transport

**One implementation.** There is exactly one eventing substrate: NATS, behind
one `EventFabric`. The NATS server is a Go **library**
(`github.com/nats-io/nats-server/v2/server`), not only an external daemon, so
the single-binary / dev / OSS-default case does not need a bespoke in-process
fabric — it embeds the real NATS server in-process:

```go
opts := &server.Options{DontListen: true, JetStream: true}
ns, _ := server.NewServer(opts)
go ns.Start()
ns.ReadyForConnections(10 * time.Second)
nc, _ := nats.Connect(ns.ClientURL(), nats.InProcessServer(ns))
```

`DontListen: true` plus `nats.InProcessServer(ns)` yields a fully in-process
NATS — zero network sockets, zero sidecar process, zero extra dependency to
distribute, compiled into the Server binary — **including in-process
JetStream**. The multi-instance / HA case connects the same client to a
clustered external NATS. **Application code is identical across both modes**:
same client, same subjects, same queue groups; the only difference is one
configuration value (embed vs connect-to-cluster). One implementation, one
code path, one set of semantics to test — and the embedded mode is the
"runs with no external services" OSS default, backed by real NATS instead of
a bespoke channel fabric.

**What NATS is for here — and what it is not.** NATS is the eventing
substrate UNDER the async layer. It does not replace Connect at the
synchronous RPC edges (Client↔Server RPC stays Connect/gRPC, DL-013), it does
not touch the agent↔Runner vsock hop (RIG-2394), and it never becomes a second
store — Postgres remains the durability source of truth, preserving the
DL-019/DL-020 spirit ("the ring is never a second store; losing it loses no
committed state",
`docs/designs/product/compass-architecture-lineage/design.md:52-56`).

**What rides the substrate.**

- **Comms delivery (JetStream).** Today's fan-out is write-through Postgres
  then publish on the in-process ring (`go/internal/comms/comms.go:1-16`,
  `go/internal/comms/doc.go:6-16`); the delivery consumer tails it
  (`go/internal/delivery/consumer.go:143-152`). Under the fabric, a posted
  message commits to Postgres first (write-through preserved,
  `go/internal/comms/doc.go:9-12`), then rides a JetStream stream for
  fan-out: JetStream provides durable at-least-once delivery, bounded
  replay, and queue-group consumption. **JetStream is a transport, never a
  second truth store**: the message row and the per-(agent, channel)
  delivery cursor in Postgres (`go/internal/store/delivery_cursors.go`,
  advanced only on recipient ack — `go/internal/runnerhub/hub.go:713-724`)
  remain the recovery truth, so anything JetStream drops or double-delivers
  is reconciled against the cursor (preserving DL-020). This reverses the
  earlier draft's rejection of JetStream for comms delivery — see
  Alternatives considered for the reversal record.
- **Routing/binding invalidation (core NATS).** The hub's in-memory binding
  maps become instance-local caches over durable truth (`agent_placements` +
  a session-binding table), with core NATS carrying invalidation/update
  events so any instance resolves a session without polling. Postgres is the
  arbiter on any cache miss or conflict; core NATS at-most-once suffices
  because a dropped invalidation degrades to a cache-miss re-read.
- **Delivery queue groups.** The delivery consumer's dispatch work partitions
  across instances with queue-group semantics — each posted-message event is
  claimed by exactly one worker (the three-hop model's hop 2). The
  `ControlDispatcher` / `SessionResolver` / `DeliveryReads` interfaces
  (`go/internal/delivery/consumer.go:42-63`) are the injection seams; the
  consumer logic above them does not change.
- **Server↔Runner command/event fabric (the Q2 topology).** Per-Runner
  command subjects, Runner event fan-in on queue groups, enrollment
  heartbeats. Request-reply for the RPC-shaped legs (`RelayCommsCall`,
  `CommitConversationFrame`) with the same fail-closed semantics as today
  (`go/internal/runnerhub/relay_comms.go:242-320`). This rides NATS only
  once the server is clustered (Phase 1): the Runner is an external box, and
  the embedded single-binary server takes no network sockets (`DontListen`),
  so at Phase 0 the Runner↔Server edge stays on today's Connect transport —
  `RunnerFabric`'s Phase-0 implementation wraps the existing command router
  (OQ-5 owns the Connect-edge successor).

**JetStream configuration is part of the design, not an ops detail.** The
defaults trade durability for throughput and can lose acknowledged writes
under power failure (the December 2025 Jepsen analysis of NATS documented
~14% acknowledged-write loss under default settings). The delivery stream
runs with: `sync_interval: 100ms` (bounded fsync window), file storage with
R3 replication in the clustered mode (the embedded single-node mode is R1 by
construction — Postgres is the recovery truth either way), explicit
per-message acks, and `max_deliver` with a dead-letter subject so a
poison message parks instead of redelivering forever.

**Rejected placements.** NATS as a Client-facing transport (stays Connect);
NATS on the agent↔Runner hop (frozen vsock, RIG-2394); JetStream as the comms
**message store** (Postgres write-through is the product's structure — the
audit/search substrate rationale behind DL-021,
`docs/designs/product/compass-architecture-lineage/design.md:46-51` — and
that survives this record intact: JetStream carries delivery, Postgres keeps
the message).

**Recommendation.** Adopt NATS as the **single eventing substrate**: embedded
in-process (with in-process JetStream) for the single-binary/OSS default,
clustered for multi-instance; JetStream as the durable comms-delivery
transport; core NATS pub/sub for routing/binding invalidation; queue groups
for delivery workers; per-Runner subjects for the Q2 topology. Postgres
remains the sole durability source of truth. Adoption is staged (see Staged
adoption path) so a single-instance deployment — including the OSS core —
never operates an external broker.

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
The where-does-work-land convention this split implies is formalized in
`docs/designs/meta/oss-core-managed-boundary/design.md`.

## Substrate independence

The Server↔Runner topology (Q2) and the fabric (Q3) are **agnostic to the box
provisioning substrate** — the metal a Runner runs on. The fabric routes a
command to a Runner by subject and resolves placement from durable Postgres
state (`agent_placements` + session bindings); it never observes whether that
Runner is a process on an elastic bare-metal host, a nested-virt cloud VM, or a
pod on a nested-virt Kubernetes node pool. So the choice of managed box
substrate does not change any Q1/Q2/Q3 answer, interface, or task in this
record.

That substrate choice is nonetheless a live managed-control-plane fork.
RIG-2394's D2 froze the *managed* path as elastic **bare-metal** (AWS
`*.metal`, GCP `c3-metal`) specifically to dodge the nested-virtualization
tax, and called a nested cloud VM "a self-inflicted tradeoff, never the
managed path"
(`docs/designs/infra/runtime/compass-elastic-session-runtime/microvm-runner.md:671-692`).
Two later facts stale that premise: AWS enabled nested
virtualization on **non-metal** EC2 (C8i/M8i/R8i, no surcharge, 2026-02), and
Azure offers a credit-funded nested-virt AKS node pool — so a nested-virt
**node pool** (running the Runner + cloud-hypervisor as ordinary host
processes, *not* Kata pod-sandbox runtime-class, whose CRI/containerd control
plane cannot carry the agent↔Runner gateway RPC — RIG-2394 D4) is now a real
managed option beside bare-metal. On the tax itself: the widely-cited ~10% is
nested-virtualization **CPU performance overhead** — a community
rule-of-thumb, not an AWS-published billing surcharge. On a non-metal
instance the agent microVM runs two virtualization levels down (Nitro → EC2
guest → microVM), and the nesting cost (extra VM-exits, nested page-table
walks, virtualized I/O) shows up as roughly 10% more host CPU for the same
work — paid as **~10% fewer concurrent sessions per box** at the same latency
target, i.e. proportionally more or bigger instances for the same load.
`*.metal` pays zero (the microVM runs directly on host VT-x, one
virtualization level). Our agent workload is largely I/O-bound waiting on LLM
calls, so the real overhead is likely under the rule-of-thumb and is worth
measuring on the actual workload before treating 10% as a planning fact.
Because that fork spans this record's topology and RIG-2485/RIG-2394-D2's box
lifecycle, its home is the private managed monorepo's AWS-EKS substrate
design (RIG-2878), not this record; it gates only the box layer *beneath*
this substrate-agnostic topology.

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

### N×M direct gRPC mesh (Q2 option 2)

Weighed in Q2 and rejected: satisfies non-sticky wake but scales standing
streams and per-pair state as tenants × fleet grows, and still forces the
binding maps out of single-hub RAM into shared state — after which the mesh
adds nothing the fabric does not do with two orders of magnitude fewer
connections.

### Two `EventFabric` implementations — in-process channels beside NATS (Q3)

An earlier draft of this record kept the in-process `events.Bus` ring
(`go/events/events.go:1-13`) as a first `EventFabric` implementation for the
single-binary case, with NATS as a second implementation swapped in at
multi-instance scale. Rejected: the NATS server embeds in-process
(`server.Options{DontListen: true}` + `nats.InProcessServer`), which gives
the zero-external-dependency property with **one** implementation — so a
second bespoke channel fabric is pure cost: two code paths to test, two
semantics to keep aligned, and an implementation swap at exactly the moment
a deployment is under scaling pressure. One implementation, one config value.

### Postgres LISTEN/NOTIFY as the fabric (Q3)

Rejected: payloads are capped at 8KB, notifications are dropped for
disconnected listeners, there are no queue groups or subject hierarchies, and
every notification is a round-trip on the same cluster the tenancy model
already asks to carry all durable truth. Embedded NATS removes its one
argument — zero new infrastructure — because in-process NATS is also zero
new infrastructure, with real subjects and queue groups.

### JetStream as the comms-delivery layer — reversal record (Q3)

An earlier draft **rejected** JetStream for comms delivery, on the argument
that the Postgres delivery cursor advanced on recipient ack
(`go/internal/store/delivery_cursors.go`;
`go/internal/runnerhub/hub.go:713-724`) is already the at-least-once
guarantee, and a JetStream consumer cursor beside it would be a second
delivery-truth store (the DL-020 dual-store failure mode). That rejection is
**reversed** — JetStream is now the comms-delivery transport (Q3) — because
two things changed: embedded NATS makes JetStream a zero-external-dependency
option even in the OSS single-binary default, and the roles are cleanly
separable — JetStream is the delivery *transport* (durable at-least-once
fan-out with bounded replay), while the Postgres message row + delivery
cursor remain the *recovery truth* every reconciliation terminates in. The
dual-truth-store objection dissolves when JetStream's consumer state is
treated as disposable transport state, never consulted for recovery — which
the Global Constraints pin.

## Staged adoption path

Operational cost is paid when earned. NATS is the fabric in every phase; the
phase boundary changes only where the NATS server runs. The OSS core ships
Phase 0 as its permanent default.

### Phase 0 — single binary, embedded NATS (the OSS permanent default)

One Server binary embeds the NATS server in-process
(`server.Options{DontListen: true, JetStream: true}` +
`nats.Connect(ns.ClientURL(), nats.InProcessServer(ns))`): zero network
sockets for the fabric, zero sidecar, zero external services — and
in-process JetStream backing durable comms delivery. The Phase-0 work in
this record is the tenancy schema (T1/T2), the `EventFabric` / `RunnerFabric`
seams (T3) — `EventFabric` on the embedded-NATS implementation, `RunnerFabric`
keeping the Runner on today's Connect transport (the Runner is external and
the embedded server takes no network sockets, so the Runner↔Server hop stays
Connect until clustered NATS lands at Phase 1, per OQ-5) — and durable
session bindings (T4 — a Server restart resolves a pre-restart session, which
fails today). Queue groups and JetStream-durable comms delivery are
Server-internal and present in the code from Phase 0, exercised by the one
instance; per-Runner NATS command subjects come online at Phase 1 with the
clustered server.

**Gate to Phase 1:** a second Server instance is actually needed — an HA
requirement or a vertical-scaling ceiling — not before. The gate signal is
surfaced through the Server's OTel collector (the bundled OTLP fan-in
endpoint, `go/internal/stack/collector_container.go:22-32`): the delivery
consumer's dispatch backlog depth (the consumer is one ctx-rooted `Run` loop
today, `go/internal/delivery/consumer.go:255`) and Server saturation metrics,
with the emitter added as gate-instrumentation work in T3.

### Phase 1 — multi-instance / HA, clustered NATS

Flip the NATS connection from embedded to a clustered external NATS — one
configuration value; no code change, no implementation swap. Queue groups now
balance delivery work across instances, per-Runner subjects route commands
from any Server, JetStream delivery is cluster-replicated (R3 file storage),
and binding invalidation fans out across instances — all semantics the code
already carries from Phase 0, now backed by the cluster.

Postgres remains the durability source of truth throughout; the fabric
carries delivery and routing, and anything the fabric drops is recovered by
the existing cursor sweeps (`go/internal/delivery/consumer.go:3-9`).

### JetStream telemetry/scheduling streams (gated, with RIG-2485)

Streams with no Postgres cursor and a genuine bounded-replay need — Runner
telemetry fan-in and scheduling events, both consumed by RIG-2485 — get
their own JetStream streams (T6) only when that lane's consumers exist and a
measured gap/loss signal on the stream, surfaced through the same OTel
collector, exceeds a deployment-set tolerance. Each such stream records its
retention window and recovery story; a JetStream stream is never the sole
holder of state the product cannot regenerate.

## Plan

### Global Constraints

- **Postgres is the sole durability source of truth for fabric-carried
  state.** No task may introduce a second store of committed
  comms/routing/session-binding state; JetStream is a delivery transport
  whose consumer state is disposable, and every fabric recovery path
  terminates in a Postgres cursor or row (preserves DL-019/DL-020's spirit).
  Transcript bodies are out of scope: the blob seam (DL-019) and the
  two-tier transcript archive (DL-093, `docs/designs/DECISIONS.md:95`)
  remain their durability owners.
- **One eventing path — NATS only.** No parallel in-process channel fabric;
  the single-binary default embeds the NATS server in-process
  (`DontListen: true` + in-process JetStream). Application code is identical
  across embedded and clustered modes.
- **Fail-closed identity everywhere.** Tenant and account resolution follow
  the existing posture: identity is never a request field, always resolved
  server-side from the authenticated connection
  (`go/internal/comms/comms.go:10-15`); an unresolvable session or tenant is
  `CodeNotFound`/zero rows, never a fallback
  (`go/internal/runnerhub/relay_comms.go:7-15`).
- **The OSS core stays single-tenant-degenerate.** No `if multiTenant`
  behavioral forks; single-tenant runs the same schema with one bootstrap
  tenant row and the embedded-NATS fabric. No external services are required
  to run the OSS core.
- **The agent↔Runner hop is frozen.** vsock through the attribution boundary
  (RIG-2394); no task exposes NATS to the in-VM side.
- **Seams before swaps.** Every fabric consumer lands behind an interface
  (the hub's own posture, `go/internal/runnerhub/hub.go:10-14`). The
  embedded↔clustered switch is a configuration value behind the same NATS
  implementation — a deployment change, never an implementation swap.
- **Migrations stay embedded-and-ordered** (`go/internal/store/store.go:18-31`);
  tenancy DDL rides the same mechanism.
- **Cross-Server command ordering on one Runner subject must be safe.**
  Per-Runner command subjects with multiple Server publishers guarantee
  only per-publisher ordering in NATS, while today's Sessions stream is a
  single ordered stream per Runner. Either commands for one session are
  serialized through its placement-owning path, or the relay legs are
  idempotent/order-independent — a required invariant with a required test
  (T5).
- **Session-level `SET` of the tenancy GUC is banned.** All tenant scoping
  goes through `beginTenantTx` (`SET LOCAL` inside an explicit
  transaction); a session-level `SET` leaks tenant identity across pooled
  connection checkouts.
- **Transaction-mode connection pooling is supported and expected.** Neon's
  pooled endpoint is PgBouncer in transaction mode, where a physical backend
  is shared across tenants between transactions. Correctness rests on the
  transaction-scoped GUC discipline this record mandates — `SET LOCAL` /
  `set_config(..., true)` only — and session `SET` stays banned precisely
  because it leaks across pooled checkouts. The design must hold unchanged
  on pooled and direct endpoints; T2's test cycle verifies against a
  transaction-mode pooler.

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

### T2 — RLS enforcement + per-transaction tenant scoping (Neon-aware)

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
  value behind across pooled checkouts — `SET LOCAL` itself is
  transaction-scoped and resets at COMMIT/ROLLBACK. The policy must use
  `current_setting('compass.tenant_id', true)` (missing_ok) plus an
  explicit non-empty guard. That guard is defense against the never-set
  NULL case under missing_ok and any session `SET`/`set_config` that leaves
  an *empty* value; a session `SET` that installs a *non-empty* tenant id is
  prevented by the `SET LOCAL`-only ban (Global Constraints), not by the
  guard — the ban is the real defense against a mis-scoped non-empty leftover.
- **Scalar-subquery GUC reads.** Every policy wraps the GUC read as
  `(SELECT current_setting('compass.tenant_id', true))` so the planner
  evaluates it once per statement, not once per row — the per-row form is a
  quiet O(rows) tax on every policied query.
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
  the shape `USING ((SELECT current_setting('compass.tenant_id', true)) <> ''
  AND tenant_id = (SELECT current_setting('compass.tenant_id', true)))`);
  `func (s *Store) beginTenantTx(ctx context.Context) (pgx.Tx, error)` —
  `Begin` + `SET LOCAL compass.tenant_id`, batched with the first query in
  one round-trip — threaded through every existing store method (all run
  on `s.pool` today, `go/internal/store/store.go:33-37`).
- **Test cycle:** a `pgtest` suite proving cross-tenant reads return zero
  rows, cross-tenant writes fail, a never-set connection fails closed (no
  rows, never all rows, never an error-42704 escape), and a pooled
  connection previously scoped to tenant A and reused without scoping does
  not see tenant A's rows; the pooled-reuse case runs against a
  **transaction-mode pooler** (PgBouncer transaction mode — Neon's pooled
  endpoint shape), not only a direct connection; plus an owner-role probe
  proving `FORCE ROW LEVEL SECURITY` is in effect (the migration-applying
  role cannot read cross-tenant). Where a Neon branch of production shape is
  available, the suite additionally runs there (branching as a test tool,
  Q1 Neon subsection).

### T3 — fabric seams: embedded-NATS `EventFabric`, Connect `RunnerFabric` (Phase 0 default)

The fabric seams and their Phase-0 implementations. `EventFabric` (comms and
delivery publish through it) runs on embedded in-process NATS by default.
`RunnerFabric` (the hub's command router lands behind it) keeps the Runner on
today's Connect transport at Phase 0 — the Runner is an external box and the
embedded server takes no network sockets — so its Phase-0 implementation wraps
the existing command router (`go/internal/runnerhub/hub.go:925-938`);
per-Runner NATS command subjects land with clustered NATS at T5.

### T4 — durable session bindings

Move the hub's session→account/Runner binding truth from single-hub RAM to
Postgres (beside `agent_placements`), with the hub's maps demoted to a cache
invalidated over the `EventFabric`.

- **Interfaces:** consumes `agent_placements`
  (`go/internal/store/agent_placements.go:33-39`), the hub binding surface
  (`bindContainer`/`promoteSession`/`unbindSession`,
  `go/internal/runnerhub/relay_comms.go:30-157`), and T3's `EventFabric`
  (invalidation events). Produces: migration
  `NNNN_session_bindings.sql`
  (`session_bindings(session_id PK, agent_account_id FK, runner_id, bound_at_unix_ms)`);
  store methods `RecordSessionBinding` / `ResolveSessionAccount` /
  `DeleteSessionBinding` (fail-closed `ErrNotFound`).
- **Test cycle:** a Server restart resolves a pre-restart session's account
  from the durable binding (fails today); the fail-closed contract holds — a
  stopped/unbound session is `CodeNotFound` exactly as
  `go/internal/runnerhub/relay_comms.go:242-249` specifies; a binding change
  invalidates the cache on a second fabric instance.

### T5 — clustered-NATS deployment mode (Phase 1)

The config-flip to a clustered external NATS, and everything that must be
proven correct only under multiple instances: queue-group delivery
partitioning, per-Runner command subjects driven from any Server,
request-reply for the RPC-shaped relay legs preserving their status-code
contracts (`go/internal/runnerhub/relay_comms.go:242-320`), JetStream R3
file-storage replication, and the cross-Server ordering invariant. This is a
deployment mode plus its multi-instance test surface — not an implementation
swap; T3's fabric code is unchanged.

- **Interfaces:** consumes T3's fabric (`fabric.Config` clustered mode) and
  T4's durable bindings; a NATS cluster (deployment owned by infra;
  connection config via server flags/env). Produces: the clustered `Config`
  wiring + JetStream R3 stream configuration; tenant-scoped subject prefixes
  (per-tenant NATS accounts/permissions is the recommended, non-normative
  example — OQ-3 finalizes the fabric-isolation mechanism); the
  multi-instance integration suite. **Ordering invariant (Global
  Constraints):** a per-Runner command subject with multiple Server
  publishers gives only per-publisher ordering in NATS, where today's
  Sessions stream is one ordered stream per Runner — so commands for one
  session MUST be serialized through its placement-owning path, or the relay
  legs MUST be idempotent/order-independent; T5 picks and implements one.
- **Test cycle:** integration suite against a clustered test NATS:
  cross-instance deliver (post on A, deliver to a session held on B via the
  per-connection subject), queue-group single-claim, Runner failover
  (resubscribe re-routes commands), fabric outage degrades to sweep-recovered
  delivery with zero committed-state loss, and the ordering invariant —
  steer-after-start published from two Server instances against one session
  must not reorder.

### T6 — JetStream telemetry/scheduling streams (gated)

Only after the telemetry/scheduling gate (Staged adoption path): JetStream
streams for Runner telemetry fan-in and scheduling events, consumed by
RIG-2485.

- **Interfaces:** consumes T3's fabric + RIG-2485's event payload
  definitions (that lane owns them). Produces stream/retention configs and a
  thin publisher on the Runner-event path; explicitly NOT a delivery-cursor
  replacement.
- **Test cycle:** bounded-window replay after consumer restart; stream loss
  degrades to live-only telemetry, never state loss.

## Tasks

- [ ] **T1** — tenant schema + identity plumbing (`tenants`, `tenant_id`,
      bootstrap seed, context seam)
- [ ] **T2** — RLS enforcement + per-transaction tenant scoping, Neon-aware
      (scalar-subquery policies, transaction-mode-pooler verification;
      depends: T1)
- [ ] **T3** — fabric seams: embedded-NATS `EventFabric` (`DontListen` +
      in-process JetStream, JetStream-durable comms delivery) + Connect
      `RunnerFabric` (Runner stays on Connect until Phase 1); Phase 0 default
- [ ] **T4** — durable session bindings (depends: T3)
- [ ] **T5** — clustered-NATS deployment mode: config flip, queue groups,
      per-Runner subjects, JetStream R3, cross-Server ordering invariant
      test (Phase 1; depends: T3, T4, and the Phase-1 gate)
- [ ] **T6** — JetStream telemetry/scheduling streams (gated; depends: T3,
      the telemetry/scheduling gate, and RIG-2485 payload contracts)

## Ledger impact

The driver flips `docs/designs/DECISIONS.md`; this section is the verbatim
delta. New rows take the next free `DL-<n>` IDs at flip time (append-only);
they are named N1-N4 here only for cross-reference.

### Proposed row adds

- **N1 (Storage):** "Multi-tenant isolation is one shared Postgres
  database with a `tenant_id` column and row-level security enforced per
  transaction (fail-closed GUC scoping); a compliance-sensitive tenant may be
  promoted to a dedicated database running the identical schema (escape
  hatch, not a fork); the OSS core runs the same schema single-tenant with
  one bootstrap tenant row" — Record: this record, §Q1.
- **N2 (Topology & tiers):** "Servers are stateless L7-load-balanced
  instances (any Server handles any client request); Runners are
  individually subject-addressable over a message-bus fabric (one connection
  per party), never single-Server-owns-Runners and never an N×M
  direct-stream mesh; delivery to a live client routes over a per-connection
  subject to the one Server holding the socket; session→Runner routing truth
  stays durable in Postgres (`agent_placements` + session bindings), and the
  Runner remains a pure forwarder with Server-side fail-closed account
  resolution" — Record: this record, §Q2.
- **N3 (Transport):** "NATS is the single eventing substrate — embedded
  in-process (`DontListen` + in-process JetStream) for the
  single-binary/OSS default, clustered for multi-instance, identical
  application code across both. JetStream is the durable comms-delivery
  transport (at-least-once fan-out with bounded replay); Postgres keeps the
  message row and the per-(agent, channel) delivery cursor as the recovery
  truth. Core NATS carries routing/binding invalidation; queue groups
  partition delivery work; per-Runner subjects carry the Server↔Runner
  command/event fabric. Connect stays the synchronous RPC edge and the
  agent↔Runner hop stays vsock (RIG-2394). No LISTEN/NOTIFY phase exists" —
  Record: this record, §Q3 + §Staged adoption path. Supersedes DL-014 and
  DL-021.
- **N4 (Storage):** "Postgres remains the sole durability source of truth
  for committed comms/routing/session-binding state: JetStream is an
  at-least-once delivery TRANSPORT whose consumer state is disposable, never
  a second truth store; every fabric recovery path terminates in a Postgres
  cursor or row. Supersedes DL-019's 'JetStream is comms-only' clause —
  JetStream now carries comms delivery as transport, with Postgres still the
  truth — while DL-019's store-of-record and transcript-blob clauses survive
  verbatim (the blob seam and the two-tier transcript archive, DL-093,
  remain the durability owners for transcript bodies)" — Record: this
  record, §Q3. Supersedes DL-019, re-scoping ONLY its "JetStream is
  comms-only" clause.

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
  exactly what the `EventFabric` reverses for the cross-instance hop.

### Rows that survive untouched

- **DL-020** (in-memory bus is a cache/fan-out ring, never a second store,
  `docs/designs/DECISIONS.md:77`) — actively preserved: JetStream inherits
  the loss-tolerant transport role across instances with Postgres the
  recovery truth; N4 restates the single-store invariant.
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
only". Introducing NATS as the single eventing substrate — with JetStream
now **actively carrying comms delivery** as the durable transport, a broader
DL-019 reversal than earlier drafts contemplated — is precisely reopening
them: permitted under the RIG-2675 nothing-frozen posture, but the
supersession call is Matt's, not this record's.

**The fork, framed precisely:** adopt NATS as the single eventing substrate
with JetStream as the comms-delivery transport — Postgres still the sole
durability source of truth (preserving the DL-019/DL-020 spirit:
write-through first, every recovery path a Postgres cursor, the fabric loses
nothing committed) — or keep the DL-014/019/021 posture and confine NATS to
comms-internal use, leaving cross-instance scale-out without a fabric.

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

### OQ-3 (non-load-bearing, deferred) — NATS deployment shape

The primary axis — embedded in-process vs clustered external — is decided
(Q3): embedded is the single-binary/OSS default, clustered is the
multi-instance mode, and the switch is one configuration value. What remains
deferred: cluster sizing, placement (co-located with Servers vs dedicated),
and whether per-tenant fabric isolation uses NATS accounts or
subject-permission scoping alone. Deferred with rationale: the `EventRef`
carries references only (event kind + row id + tenant — N4), and the durable
tenant boundary is RLS on the Postgres re-read, so a cross-tenant `EventRef`
delivered to the wrong instance re-reads under that instance's *request-path*
RLS scope and returns zero rows (the system-role background paths of OQ-4
read cross-tenant by design, but expose data only onto a recipient's own
tenant-scoped connection — the user-facing exposure boundary stays the
RLS-scoped request read); the fabric is not the primary tenant boundary, and
no interface in the Plan changes with the answer (`fabric.Config` is
deployment input). RIG-2485's telemetry volume estimates should inform the
clustered shape. The "NATS accounts/permissions per tenant" wording in T5 is
a non-normative recommended example — OQ-3 finalizes that choice.

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
fabric" retires the Runner↔Server Connect edge at Phase 1 — enrollment, the
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
