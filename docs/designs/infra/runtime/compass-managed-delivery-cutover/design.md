# Design: Compass T3 delivery→EventFabric cutover (RIG-3107)

Status: Active
Ratified: OQ-1..OQ-4 decided by Matt (2026-09-05, see Resolved decisions); frozen on merge
Parent: `docs/designs/infra/runtime/compass-managed-multitenancy/design.md` (frozen), T3
Ledger-impact: appends DL-327..333 for the OQ-1/OQ-2/OQ-3/OQ-4 rulings, the reconnect-seam shape, and the double-publish interpretation (design-ledger-gate)

## Problem / Intent

The frozen multitenancy record's T3 mandates "comms and delivery publish
through" a JetStream-backed `EventFabric` (`compass-managed-multitenancy/design.md:755-758`:
"The fabric seams and their implementations, all on the standalone NATS stack
service. `EventFabric` (comms and delivery publish through it)…"). Today the
delivery work queue is triggered purely in-process: the single
`commsBus := events.NewBus[*compassv1.SubscribeCommsResponse]()`
(`go/server/serve.go:512`) fans to three consumers via
`startCommsBusConsumers` (`go/server/sinks.go:172-175`), and the delivery
consumer tails it in `Run` (`go/internal/delivery/consumer.go:307-415`). An
in-process trigger cannot cross Server instances and loses unprocessed events
on restart. This record designs the cutover of exactly ONE consumer — the
delivery work queue — from the bus to the fabric's durable, at-least-once,
cross-instance JetStream delivery, per Matt's pre-decided Option B scope.

## Approach

**Option B (pre-decided by Matt; not re-litigated here): migrate only the
delivery consumer; the in-process bus survives for the client stream and
presence.**

The three consumers of the one bus and their disposition:

1. `comms.SubscribeComms` (client gRPC fan-out) — **keeps the bus**. It needs
   full payloads, bus seq/epoch, and D9-visibility filtering, which the compact
   `EventRef` ("event kind + row id + tenant… never a payload copy",
   `compass-managed-multitenancy/design.md:774-775`) does not carry. The
   client-edge NATS migration is a later, not-yet-recorded task (not scheduled
   by the parent's T5, which is multi-Server operation + NATS clustering).
2. `delivery.Consumer` — **migrates to the fabric** (this record).
3. `presence.Publisher` (`go/server/sinks.go:159-165`) — **keeps the bus**
   (same payload/seq needs).

### The mechanic

**Publish side.** `comms.publishMessagePosted`
(`go/internal/comms/mapping.go:503-509`) currently does only

```go
func (c *Comms) publishMessagePosted(ctx context.Context, m store.Message) {
    c.bus.PublishCtx(ctx, &compassv1.SubscribeCommsResponse{
        Payload: &compassv1.SubscribeCommsResponse_MessagePosted{...},
    })
}
```

It gains a second, fabric publish of
`EventRef{Tenant: <ctx tenant>, Kind: KindMessagePosted, RowID: string(m.ID)}`
on `CommsSubject(tenant, KindMessagePosted)`, alongside (after) the bus
publish. Both its call sites already sit AFTER the Postgres commit:
`PostMessage` publishes only `if inserted` after `AppendMessage` returns
(`go/internal/comms/comms.go:428-430`), and `RespondToAsk` publishes after
`AnswerAsk`'s one transaction returns (`comms.go:453-472`). The consumer
re-reads Postgres, so commit-before-publish is the load-bearing order and it
already holds. The tenant comes from the RIG-3106 ctx accessor
(`store.TenantFromContext`, `go/internal/store/context.go:19-26`) with the
bootstrap-tenant fallback mirroring `Store.resolveTenant`
(`go/internal/store/tenant.go:54-58`: "if t, ok := TenantFromContext(ctx); ok
&& t != \"\" { return t } return s.bootstrapTenantID") — tenant rides the
subject, never a field on the wire event beyond `EventRef.Tenant` itself.

The other six bus publishers (`publishChannelChanged`,
`publishAgentWorkspaceChanged`, `publishMessageUpdated`,
`publishTopicUpserted`, etc., `mapping.go:463-529`) stay bus-only: the
delivery consumer's `handleEvent` acts only on `MessagePosted` — "A
MessagePosted is the delivery trigger; … every other variant is ignored"
(`go/internal/delivery/consumer.go:417-427`) — so nothing else needs the
durable hop yet.

**Consume side.** `delivery.Consumer.Run` stops subscribing to the bus for its
trigger and instead consumes
`EventFabric.SubscribeKind(ctx, KindMessagePosted, fn)` — the tenant-wildcard
read side landed in PR3 (#903): "SubscribeKind drives fn for every event of one
kind ACROSS EVERY TENANT … one DURABLE queue-group consumer … each matching
event is claimed by exactly one Server instance"
(`go/internal/fabric/event_fabric.go:86-99`, PR3 tree). Since `handleEvent`
ignores every non-MessagePosted variant, the consumer drops its bus
subscription entirely: `NewConsumer` loses the `bus` parameter
(`consumer.go:262`), and the `bus` field (`consumer.go:181`) is removed.

Per event, the callback re-reads the message from Postgres via the EXISTING
`DeliveryReads.MessageByID(ctx, ref.RowID)` (`consumer.go:70`) under a
tenant-scoped ctx (OQ-3), then runs the EXISTING `onMessagePosted`
classification (`go/internal/delivery/dispatch.go:37-82`) — human-authored →
`fanOut` now; agent-authored with live author session → `hold`
(`dispatch.go:89-93`); agent-authored dead → re-read + `fanOut`. The re-read
means the wire-payload path (`handleEvent` handing the bus's
`posted.GetMessage()` into `onMessagePosted`) is replaced by the
`storeMessageToWire` re-read shape that already exists for the dead-author and
fire paths (`consumer.go:586-589`).

**Fabric ack/retry semantics inherited.** `Publish` "return[s] only once the
server has acked it into the stream — so a Publish that returns nil means the
event is stored, and one that returns an error is genuinely unpublished"
(`event_fabric.go:13-16`, PR3 tree). Subscribe-side: "fn returning normally
acks … A failure Naks for immediate redelivery until NumDelivered reaches
MaxDeliver — total ATTEMPTS, not retries — at which point the message is
parked on DLQSubject and Term'd" (`event_fabric.go:66-72`). `DefaultMaxDeliver
= 5`, `DefaultAckWait = 30 * time.Second`
(`go/internal/fabric/stream.go:23-33`, PR3 tree). Publish-side dedup rides
`msgID` — "Deterministic in (tenant, kind, row id) so two Servers publishing
the same logical change … collapse to one stored message inside the stream's
duplicate window" (`go/internal/fabric/eventref.go:61-64`, PR3 tree).

### Publish failure semantics (commit ok, fabric publish fails)

Today's bus publish is INFALLIBLE: `Bus.Publish`/`PublishCtx` return only a
seq, no error (`go/events/events.go:173-182`), so today there is NO
commit-ok/publish-failed window at all. The fabric publish is strictly stronger
in that it CAN report failure — which introduces a delivery-loss mode the bus
never had. This record keeps the post RPC's contract unchanged: a fabric-publish
error after a successful commit is **logged loud (ErrorContext) + counted (a new
`compass.delivery.fabric_publish_failures` counter), never returned to the
caller** — the message is committed, so failing the RPC would report a persisted
write as failed.

But a counter is not a recovery path. The delivery-cursor sweep (`drainStarts` →
`sweepSession`, `go/internal/delivery/settle.go:124-144`) and
`scanMissedMentions` (`go/internal/delivery/scan.go:34`) are the RIGHT backstop
COMPONENTS, but they are NOT "the same lattice that absorbs a dropped bus
event": (1) today's only bus-loss mode is in-process subscriber lag, which fires
an IMMEDIATE in-process trigger (`sub.Lagged()` → `sweepAllLive` +
`scanMissedMentions`, `consumer.go:360-408`) — a trigger this cutover deletes
with the lag branch; and (2) `scanMissedMentions` routes ONLY mentions +
ask-answers (`scan.go:35-70`), never plain delivers, so it recovers a
publish-failed PLAIN message not at all. **What triggers full-set recovery after
a publish-side failure is resolved by OQ-3 part 2 / DL-330: a fabric-reconnect
hook plus a minutes-scale periodic floor tick, each running `sweepAllLive` +
`scanMissedMentions`.**
No outbox table in this PR — Postgres remains "the sole durability source of
truth" and the cursor defines what is owed; an outbox would be a second
delivery-trigger store (see Alternatives, and OQ-3 part 2 for the trigger
question the outbox rejection assumed away).

### Why the double-publish is NOT a Global-Constraint violation

The frozen record's constraint reads: "**One eventing path — NATS only.** No
parallel in-process channel fabric; NATS runs as a standalone stack service in
every deployment" (`compass-managed-multitenancy/design.md:635-637`). That
bans a second SWAPPABLE `EventFabric` implementation (an in-process channel
impl of the seam) — it does not ban the pre-existing `events.Bus` coexisting
during the phased, multi-step migration. During this transitional phase,
`message_posted` is intentionally published twice: once on the bus (for the
two consumers that stay, client stream + presence) and once on the fabric (for
the one consumer that migrates). The two publishes serve disjoint consumer
sets; no consumer reads both, so no double-handling occurs. The transitional
shape ends when the client edge migrates and the bus retires — a later,
not-yet-recorded task, distinct from the parent's T5 (multi-Server clustering).

## Alternatives considered

- **Migrate all three consumers at once.** Rejected upstream (Matt, Option B):
  the client stream and presence need payload/seq/epoch/D9-visibility the
  `EventRef` deliberately does not carry; carrying payloads would breach
  "never a payload copy" (`compass-managed-multitenancy/design.md:774-775`).
- **Transactional outbox for the fabric publish.** Publishes the EventRef from
  a Postgres-committed outbox row, closing the commit-ok/publish-fail window
  exactly. Rejected for this PR: it adds a table + relay loop, and the frozen
  record pins Postgres as durability owner with the fabric as transport only
  (`compass-managed-multitenancy/design.md:627-634`). Note the correction from
  the red-team: the cursor sweep closes the window only if something TRIGGERS
  it, which OQ-3 part 2 resolved — the outbox is one of the candidate answers
  there (it makes the publish itself durable, sidestepping the trigger
  question), weighed against the lighter reconnect-hook/floor-tick triggers.
- **Keep the delivery consumer's bus subscription alongside the fabric one.**
  Double-handling: every MessagePosted would classify (and potentially
  dispatch) twice per Server. The SUBJECTS.md migration note pins this: "a
  migration introducing `SubscribeKind` must retire the concrete subscribes
  rather than double-handle events" (`go/internal/fabric/SUBJECTS.md:45-47`,
  PR3 tree) — the same logic applies to the bus tail.

## Global Constraints

- **Double-publish is the sanctioned transitional shape.** `message_posted`
  goes to BOTH the in-process bus (client stream + presence, until the client
  edge migrates) and the fabric (delivery). This does not violate the frozen
  "One eventing path — NATS only" constraint, which bans a second swappable
  `EventFabric` implementation, not the bus's phased coexistence (see
  Approach). No OTHER event kind gains a fabric publish in this PR.
- **Publish after commit, always.** The fabric publish happens strictly after
  the Postgres commit (both call sites already are:
  `go/internal/comms/comms.go:428-430`, `:453-472`); the consumer re-reads the
  row the ref names, so a pre-commit publish would read a missing row. A
  publish failure after commit is logged + counted, never returned to the
  poster (Approach, "Publish failure semantics").
- **The bus survives for the client stream and presence.** Only
  `delivery.Consumer` leaves the bus. `comms.SubscribeComms` and
  `presence.Publisher` keep tailing `commsBus`; `serve.go`'s bus construction,
  `drainDoors`'s `commsBus.Close()` (`go/server/serve.go:838-839`), and the
  lag/replay semantics of those consumers are untouched.
- **Postgres remains the sole durability source of truth.** The fabric carries
  compact `EventRef`s only; every consumer action re-reads Postgres. No new
  durable state outside Postgres + the JetStream stream's transport window.
- **Shared-consumer config discipline.** The durable queue-group consumer is
  shared across Server instances, so "every instance subscribing to a subject
  must run the same fabric Config" (`event_fabric.go:62-64`, PR3 tree).
- **Ledger delta owed.** This record's ratified OQ rulings append DL rows to
  `docs/designs/DECISIONS.md` at submit (the design-ledger-gate CI check
  enforces the coupling); the driver lands the rows with this record's PR.
- **Depends on PR1–PR3 of the T3 stack** (`package fabric` incl.
  `SubscribeKind`, #903) being merged; this workspace's main (`46e389a4`) does
  not yet contain `go/internal/fabric` — fabric citations above are to the
  PR3 tree.
- **Single-instance deployment is a transitional constraint (OQ-4).** This
  cutover moves the delivery TRIGGER cross-instance, but the dispatch plane it
  feeds — `SessionForAccount`/`LiveAgentSessions`
  (`runnerhub/relay_comms.go:179-196`), the held registry, settle edges,
  per-session gates (`consumer.go:195-232`) — is instance-local hub RAM. Until
  the parent record's durable session bindings land (parent T4,
  `compass-managed-multitenancy/design.md:795`, sequenced AFTER this cutover), a
  second Server instance breaks hold/settle semantics for cross-instance authors
  (OQ-4). Deploy a single Server until then; the T4 integration proof exercises
  transport claim semantics only (not multi-instance delivery correctness).
- **A nil/unconstructed fabric is a fail-closed startup error at assembly.**
  `comms.NewComms`'s nil-fabric ⇒ bus-only affordance (T1) exists for UNIT
  TESTS; a post-cutover delivery `Consumer` with a nil fabric has NO trigger at
  all (a zero-delivery Server that commits posts and never delivers). Server
  assembly (`serve.go`, T3) MUST construct a real fabric or fail to boot — never
  a silent degraded mode.
- **Scan-vs-hold is closed under one critical section.** `scanMissedMentions`
  does `messageHeld(id)` check → route → `MarkMentionsRouted` (`scan.go:44-60`);
  under OQ-2's callback-direct concurrency a fabric callback can `hold(m)`
  between the check and the mark, routing mentions off partial blocks and then
  excluding a later-block mention from recovery (`mentions_routed_at` stamped).
  T2 re-checks `messageHeld` under the same critical section as the mark (or
  skips the mark when held-at-mark-time). Today's single goroutine gave this for
  free; Option A must restore it explicitly.
- **Dispatch callbacks are bounded-work; the ack must not block behind a long
  sweep.** OQ-1's ack-on-receive means the fabric acks when the callback
  returns. If `onEventRef`'s dispatch queues behind an in-flight `sweepSession`
  holding the recipient's gate for a large owed-set (`settle.go:333-349`,
  `dispatch.go:358-395`), the ack can exceed `AckWait = 30s` ⇒ redelivery of a
  healthy in-flight message ⇒ `NumDelivered` marching toward `MaxDeliver = 5`
  ⇒ a healthy message DLQ-parks under sustained gate contention. Keep
  per-callback work bounded; T4 includes a slow-callback (gate-held) redelivery
  case.

## Plan

### T1 — Fabric publish of message_posted (publish side)

Add the fabric publish to `publishMessagePosted` after the bus publish;
resolve the tenant from ctx with the bootstrap fallback.

- **Interfaces:** consumes
  `EventFabric.Publish(ctx context.Context, subject string, ref EventRef) error`
  and `CommsSubject(tenant string, kind EventKind) (string, error)` +
  `fabric.KindMessagePosted` (PR3);
  `store.TenantFromContext(ctx context.Context) (store.TenantID, bool)`
  (`go/internal/store/context.go:23`). Produces: a new exported
  `func (s *Store) ResolveTenant(ctx context.Context) TenantID` (promoting the
  unexported `resolveTenant`, `go/internal/store/tenant.go:54-58`, so comms
  gets the same set-or-bootstrap-fallback semantics the store's writes use);
  `comms.NewComms` gains a `fabric fabric.EventFabric` parameter (nil-safe:
  nil ⇒ bus-only, so unit tests and any not-yet-wired assembly keep working);
  `publishMessagePosted(ctx, m)` extended with the fabric publish + the
  `compass.delivery.fabric_publish_failures` counter.
- **Test cycle:** a comms unit test with a fake `EventFabric` proving (a) a
  genuine insert publishes exactly one `EventRef{Tenant, KindMessagePosted,
  RowID: m.ID}` on the tenant's subject AFTER `AppendMessage` returns, (b) an
  idempotent-retry (`inserted=false`) publishes nothing, (c) a fabric publish
  error is swallowed (RPC still succeeds) and the counter increments, (d)
  `RespondToAsk` publishes for the answer message. Existing comms suite stays
  green with a nil fabric.

### T2 — Consumer trigger cutover (consume side)

Replace the consumer's bus tail with `SubscribeKind`; delete the bus-specific
machinery per the OQ rulings.

- **Interfaces:** consumes
  `EventFabric.SubscribeKind(ctx context.Context, kind EventKind, fn func(EventRef)) (Unsubscribe, error)`
  (PR3, `event_fabric.go:105-117`); `DeliveryReads.MessageByID(ctx, messageID string) (store.Message, error)`
  (`consumer.go:70`); `store.WithTenant(ctx, t TenantID) context.Context`
  (`context.go:15-17`); `store.WithSystemRole(ctx) context.Context`
  (`tenant_tx.go:48-50`). Produces: `NewConsumer(st DeliveryReads, dispatch
  ControlDispatcher, resolver SessionResolver, fab fabric.EventFabric, log
  *slog.Logger) *Consumer` (bus parameter removed); a new
  `func (c *Consumer) onEventRef(ctx context.Context, ref fabric.EventRef)`
  that re-reads the message (tenant-scoped per OQ-3) and calls the existing
  `onMessagePosted`; `Run(ctx)` restructured per OQ-2's ruling: the
  settle/start drain loop (`notify` → `drainSettles`/`drainStarts`,
  `consumer.go:355-357`) survives unchanged; the `sub.Live` arm, the replay
  drain, and the `sub.Lagged()` overrun branch (`consumer.go:342-349,
  358-411`) are deleted (their replacement is the OQ-3 part 2 recovery trigger;
  see the recovery note in this task's test cycle). The consumer's `onEventRef`
  plus its constructor are the T2 surface; the `serve.go` fabric construction and
  `startDeliveryConsumer` rewiring are T3 (assembly). Also produces the
  fabric-level per-consumer serial-callback contract OQ-2 rests on: a doc
  comment on `Subscribe`/`SubscribeKind` (`event_fabric.go`, PR3 tree — reached
  because PR4 stacks on #903) promising the callback is invoked serially per
  consumer, plus a fabric test asserting no two callbacks overlap, so the
  consumer's concurrency argument rests on a fabric contract rather than a
  nats.go internal.
- **Test cycle:** the same commit that removes the `bus` field/parameter and the
  `afterResubscribe` seam edits EVERY test file in `package delivery` that
  references `c.bus.*`, `events.Bus`, or `c.afterResubscribe` — eleven at time
  of writing, because `package delivery` is ONE compilation unit that breaks
  simultaneously: `helpers_test.go`'s shared `newTestConsumer` constructor
  (which most bus-driving tests route through) and `trace_test.go`'s
  `publishCtxResponse` helper signature both name the `events.Bus` type
  directly, alongside `consumer_test.go`, `mention_test.go`,
  `offline_mention_test.go`, `pre_settle_closure_pgtest_test.go`,
  `scan_wiring_test.go`, `ask_answer_target_test.go`, `sweep_test.go`,
  `pin_sweep_test.go`, and `ask_answer_recovery_pgtest_test.go` — so the
  delivery package COMPILES and the existing unit suite stays green at this
  commit (the overrun-branch tests that assert `afterResubscribe` are deleted
  WITH the branch, here, not deferred). The suite is re-driven with the fake
  trigger swapped from bus-publish to a fake-fabric `onEventRef` injection
  (hold/fire split, mention routing, steer-only precedence, gates — all
  unchanged behavior); a new test proving a ref whose row is missing
  (`store.ErrNotFound`) is handled per OQ-1's ack ruling; a red-green test that
  a `hold` landing between `scanMissedMentions`'s held-check and its
  `MarkMentionsRouted` does NOT strand the message's later-block mentions (the
  scan-vs-hold critical-section invariant, Global Constraints); the fabric
  no-overlap contract test above; and a race-detector run
  (`go test -race ./go/internal/delivery/...`) covering
  concurrent `onEventRef` + settle drain per OQ-2's ruling. **Recovery-trigger
  ordering:** deleting the `sub.Lagged()` overrun branch removes the only
  mid-run recovery trigger, so the OQ-3 part 2 replacement (T5) MUST land in the
  same PR — the branch is never deleted in a merged tree without its
  replacement present.

### T3 — Fabric construction + lifecycle in server assembly

Construct the `EventFabric` in server assembly and wire it into comms +
delivery; fail closed when it is absent.

- **Interfaces:** consumes the fabric constructor + its NATS endpoint config
  (`go/internal/fabric`, PR3 tree); `serve.go`'s existing `commsBus`
  construction (`go/server/serve.go:512`) and `drainDoors` close ordering
  (`serve.go:838-839`). Produces: fabric construction in `serve.go` threaded
  into BOTH `comms.NewComms` (T1) and `startDeliveryConsumer`
  (`go/server/sinks.go:132-148`, replacing `commsBus` for the delivery trigger);
  a fail-closed startup error when NATS/the fabric cannot be constructed (never
  a nil-fabric degraded boot); fabric `Close`/Drain ordered relative to
  `drainDoors` and the serve group so in-flight callbacks finish before teardown
  (`event_fabric.go:152-171`, PR3 tree).
- **Test cycle:** a server-assembly test (or the existing boot/wiring test
  extended) proving a nil/unconstructable fabric fails startup rather than
  booting a zero-delivery Server; existing comms+delivery suites green under the
  real wiring.

### T4 — Two-instance + redelivery + DLQ integration proof

The frozen T3 test-cycle obligations
(`compass-managed-multitenancy/design.md:789-793`), scoped to delivery. **Scope
note:** this proves fabric TRANSPORT claim semantics only (single-claim across a
queue-group, redelivery, DLQ-park). Multi-instance DELIVERY correctness
(author-session resolution against a live session on the OTHER instance) is
explicitly OUT of scope and blocked by the OQ-4 single-instance constraint until
parent T4's durable session bindings land — the two-instance harness here must
NOT assert hold/settle correctness for a cross-instance author.

- **Interfaces:** consumes the PR3 fabric test harness (test-scoped NATS,
  `newFabric(t, Config{})` shape, `event_fabric_test.go`); two `Server`
  assemblies against one NATS + one Postgres. Produces: an integration test
  proving (a) a message posted on instance A is claimed by exactly ONE of two
  delivery consumers (the shared durable queue-group,
  `event_fabric.go:93-96`), (b) a callback failure Naks and redelivers up to
  `MaxDeliver`, (c) attempt exhaustion parks on `DLQSubject` with the
  `Compass-Original-Subject` header naming the concrete tenant subject
  (`SUBJECTS.md:158-161`), (d) a slow (gate-held) callback that exceeds
  `AckWait` redelivers a HEALTHY message and — absent bounded callback work —
  can march to a DLQ-park (the bounded-callback invariant, Global Constraints).
- **Test cycle:** the test IS the deliverable; plus the full existing
  comms+delivery suites green under the new wiring.

### T5 — Recovery-path rewire per OQ-3 part 2 ruling

Install the Matt-ruled publish-failure recovery trigger (no bus-lag signal
exists anymore) and re-derive the no-loss argument from JetStream durability.

- **Interfaces:** consumes `sweepAllLive(ctx)` (`settle.go:315-319`),
  `scanMissedMentions(ctx)` (`scan.go:34`),
  `SessionResolver.LiveAgentSessions()` (`consumer.go:59`), and the ruled
  reconnect seam. **That seam does not exist yet** — the
  `EventFabric` interface (`fabric.go:23-31`, PR3 tree) exposes only
  `Publish`/`Subscribe`/`SubscribeKind`, and the fabric's own
  `ReconnectHandler` (`fabric.go:262-264`) is log-only and set once at `New()`;
  `Config.Options` REPLACES it (a later `nats.ReconnectHandler` wins), so wiring
  a consumer callback through `Config.Options` discards the fabric's outage
  diagnostics unless the caller re-logs (`fabric.go:68-70`). T5 therefore
  PRODUCES the seam per the seam-shape ruling (see Resolved decisions, OQ-3
  part 2 → seam): a new `EventFabric` method
  `OnReconnect(fn func()) (Unsubscribe, error)` on the interface and `*Fabric`,
  chained onto the fabric's existing `ReconnectHandler` so its outage log
  survives, reached by the delivery consumer through the interface value it
  already holds.
  Produces: a fabric-reconnect hook (via `OnReconnect`) AND a minutes-scale
  periodic floor tick, each running `sweepAllLive` + `scanMissedMentions` —
  restoring the mid-run recovery the deleted overrun branch provided and covering
  plain delivers `scanMissedMentions` cannot — with doc-comment updates that
  re-derive the no-loss argument from JetStream durability instead of the ring
  overrun.
- **Test cycle:** a red-green unit test that a publish-failed PLAIN
  (non-mention) message to a live, never-restarting recipient IS recovered by
  the ruled trigger — this is the DL-330 silent-stall hole the red-team
  promoted to CRITICAL, so the record's headline recovery ruling ships with a
  test proving it closes; a test that the start-time scan still runs before the
  first event; and a test that `OnReconnect`'s chained callback fires the
  `sweepAllLive` + `scanMissedMentions` pair WITHOUT displacing the fabric's own
  reconnect log (the chained-not-replaced invariant DL-333 rests on). The
  overrun-branch `afterResubscribe` tests are deleted in T2 with the branch, not
  here.

### T6 — Changelog + record cross-references

- **Interfaces:** consumes `docs/designs/DECISIONS.md`; this record. Produces:
  the changelog entry and this record's cross-references. The DL rows for the
  ratified OQ rulings and the reconnect-seam shape (DL-327..333, incl. the
  double-publish-is-not-a-Global-Constraint-violation interpretation) landed
  WITH this record's own freeze PR per the "Ledger delta owed" Global
  Constraint — they are NOT re-produced here (the append-only unique-ID rule
  forbids duplicating them). The parent multitenancy record's
  T3 task is FROZEN prose — it is NOT rewritten; this record's own DL rows +
  `Parent:` header carry the "delivery cutover landed" linkage (the freeze rule
  adds a record/ledger row, never rewrites frozen prose).
- **Test cycle:** design-ledger-gate CI green.

## Tasks

- [ ] T1: fabric publish in `publishMessagePosted` + `Store.ResolveTenant` +
      failure counter (tests a–d)
- [ ] T2: consumer trigger cutover — `SubscribeKind` in, bus tail out, per
      OQ-1/OQ-2/OQ-3 rulings; fabric serial-callback contract doc + no-overlap
      test; every `package delivery` test file referencing `c.bus.*`/
      `events.Bus`/`c.afterResubscribe` edited in this commit (eleven, incl. the
      shared `newTestConsumer` in `helpers_test.go`); scan-vs-hold
      critical-section test; suites green + race run
- [ ] T3: fabric construction + lifecycle in `serve.go` assembly; fail-closed
      nil-fabric startup
- [ ] T4: two-instance single-claim, redelivery, DLQ-park integration proof
      (transport-only scope note; slow-callback redelivery case)
- [ ] T5: recovery-path rewire — Matt-ruled publish-failure trigger
      (`sweepAllLive` / `scanMissedMentions`) per OQ-3 part 2; PRODUCES the
      reconnect seam (does not exist yet); lands in T2's PR; plain-deliver
      recovery test
- [ ] T6: changelog + record cross-references (DL-327..333 already landed with
      this record's freeze PR)

## Resolved decisions

All five load-bearing questions were ratified by Matt (2026-09-05); the fork
analysis is retained below for the executor, each stamped with its decided
outcome. The `skill://review` pass surfaced one follow-on API-shape sub-fork
(the reconnect seam's shape, under OQ-3 part 2), which Matt ruled the same day
(Option A — add `EventFabric.OnReconnect`, stamped in that section). No live
question survives to the freeze.

### OQ-1 (LOAD-BEARING): JetStream ack timing for HELD delivers

**DECISION (Matt, 2026-09-05): Option A — ack-on-receive.** The
design-critic red-team verified end-to-end that the cursor sweep closes the
hold-to-fire window exactly as today.

**The fork.** The fabric acks a message when the subscriber callback returns
(`event_fabric.go:66-72`: "fn returning normally acks"). But an agent-authored
message whose author still streams is not delivered on receipt —
`onMessagePosted` HOLDs it keyed by the author's session
(`dispatch.go:65-81`: "If the author has a live session, HOLD until it
settles"), and it fires only at the author's settle edge (`fireHeld`,
`settle.go:269-290`), potentially a whole agent turn later. When does the
JetStream ack happen?

**Option A — ack-on-receive (recommended).** The callback returns (⇒ acks) as
soon as the message is classified and either dispatched or registered in the
held map. The held registry stays in-memory; a Server crash between hold and
fire loses the registry — exactly as it does TODAY with the in-process bus —
and recovery is the same existing lattice: on restart every session re-promotes
through `OnSessionStarted` → `drainStarts` → `sweepSession` (the cursor-driven
resweep, `settle.go:116-144`), and "the recipient still receives the message
via the reconnect cursor sweep, independent of this registry"
(`consumer.go:212-215`). Ack means "the durable trigger did its job: the owed
state is in Postgres (cursor) and the in-RAM timing registry".

- Pro: preserves today's semantics exactly; no ack-window pressure; a turn of
  any length holds nothing in-flight.
- Con: the JetStream redelivery guarantee does not cover the hold-to-fire
  window (but the cursor sweep already does, and always has).

**Option B — ack-on-fire.** The callback blocks (or the ack is deferred) until
`fireHeld` dispatches the message, keeping it in-flight/unacked for the whole
author turn.

- Pro: JetStream's redelivery covers the hold window; a crash mid-hold
  redelivers rather than waiting for the recipient's next start edge.
- Con: `DefaultAckWait = 30 * time.Second` (`stream.go:30-33`) is far shorter
  than an agent turn, so a healthy held message would redeliver mid-turn,
  re-classify, re-hold (duplicate held entries), and after `DefaultMaxDeliver
  = 5` attempts a perfectly healthy message DLQ-parks
  (`stream.go:23-28`). Avoiding that means per-message `InProgress()`
  heartbeats threaded from the hold registry into the fabric — new API
  surface, a liveness loop, and an in-flight slot pinned per concurrently
  streaming author. High complexity for a window an existing durable
  mechanism (the cursor sweep) already closes.

**Recommendation: Option A (ack-on-receive).** Restart durability of held
delivers falls to the Postgres delivery-cursor sweep, which is the designed
no-loss backstop today; JetStream buys cross-instance claim + durability up to
classification, which is the actual gap this PR closes.

### OQ-2 (LOAD-BEARING): concurrency model — callback-direct vs loop-enqueue

**DECISION (Matt, 2026-09-05): Option A — callback-direct under `c.mu` +
gates.** The red-team confirmed the relaxed ordering is one today's code never
had; the two Option-A obligations (scan-vs-hold critical section,
bounded-callback/AckWait) are folded as Global Constraints + T2/T4 tests, not
open forks.

**The fork.** Today `Run` is deliberately a single goroutine: one `select`
over the bus Live channel and the settle/start drains (`consumer.go:351-414`),
so post-time `onMessagePosted`→`hold` and settle-time
`drainSettles`→`fireHeld` never overlap. The fabric delivers on ITS goroutine
via the `Consume` callback (`event_fabric.go:140-142`). Where does
`onMessagePosted` run?

**Option A — run directly on the fabric callback goroutine (recommended).**
`onEventRef` re-reads + classifies + holds/dispatches on the fabric's
goroutine, concurrent with the consumer loop's settle/start drains.

- Shared state is already lock-protected: `hold` takes `c.mu`
  (`dispatch.go:89-93`), `fireHeld` pops the registry under `c.mu`
  (`settle.go:270-273`), and per-recipient dispatch ordering is owned by the
  per-session gates — "a live deliver takes the gate per message, so it
  queues BEHIND an in-flight sweep for the same session"
  (`dispatch.go:327-332`) — which were designed for exactly this
  live-vs-sweep interleaving.
- Two MessagePosted callbacks never overlap: the fabric's `Consume` dispatches
  serially per consumer through a single per-subscription goroutine
  (nats.go@v1.53.1 `jetstream/pull.go:287` invokes the handler inline;
  `nats.go:5080` runs one `waitForMsgs` goroutine per async subscription). This
  is a transitive library property; T2 gives it a fabric-level contract (a doc
  comment on `Subscribe`/`SubscribeKind` + a no-overlap fabric test).
- The one ordering hazard — a settle drain firing BEFORE a
  logically-earlier post is held, delaying that deliver to the author's NEXT
  settle edge — is **not new**: today's single loop `select`s between
  `c.notify` and `sub.Live` non-deterministically (`consumer.go:352-358`),
  so a settle already queued can drain before an earlier-posted message
  still sitting in the Live buffer. The single goroutine prevents overlap,
  not cross-channel order; the cursor sweep is, and remains, the no-loss
  floor. This must be stated in the code as the re-proved invariant.
- Con: the invariant argument lives in `c.mu` + gates rather than "one
  goroutine, QED"; a `-race` suite run over concurrent onEventRef+drain is
  mandatory (T2's test cycle).

**Option B — enqueue onto the existing single loop, callback waits.** The
callback appends the ref to a queue (mirroring `settleQueue`) and blocks on a
per-message completion handshake until the loop processes it, so the ack still
means "processed".

- Pro: single-goroutine ordering preserved verbatim; zero new concurrency to
  prove.
- Con: needs a per-message done-channel handshake; the fabric goroutine
  blocks on loop scheduling, so a long start-edge sweep (a full
  `sweepSession` + `sweepPins` + `sweepOwedMentions` per queued start,
  `settle.go:124-144`) stalls the ack past `AckWait` ⇒ spurious redelivery
  of healthy messages; and a non-blocking variant (ack on enqueue) makes the
  ack mean "buffered in RAM", strictly weaker than Option A's "classified
  and held/dispatched".

**Recommendation: Option A (callback-direct under `c.mu` + gates).** It is the
smaller mechanism, its ack meaning composes with OQ-1's ack-on-receive, and the
ordering property it relaxes is one today's code never actually had. The
red-team surfaced two Option-A-specific obligations that are NOT forks (folded,
not open): the scan-vs-hold critical-section invariant and the
bounded-callback/AckWait invariant — both in Global Constraints, tested in
T2/T4.

### OQ-3 (LOAD-BEARING): RLS scope split + lag-recovery replacement

**DECISION (Matt, 2026-09-05): part 1 → Option A (split scope, incl.
tenant-stamped `heldEntry`); part 2 → Option A (reconnect-hook + periodic floor
tick).** Part 2 was the CRITICAL red-team fork: the recovery trigger, not just
the sweep functions, is load-bearing. `sweepAllLive` runs on a fabric-reconnect
hook AND a minutes-scale floor tick (T5), so a publish-failed plain deliver to
an always-live recipient is recovered without a session restart. Option B
(publisher-side bounded retry) MAY be added as belt-and-suspenders but is not
sufficient alone.

**Seam shape (follow-on fork surfaced in review). DECISION (Matt, 2026-09-05):
Option A — add an `EventFabric` reconnect method.** The ratified
reconnect-hook ruling named a trigger, not a seam: the frozen `EventFabric`
interface (`fabric.go:23-31`) exposes no reconnect notification, and the
fabric's own `ReconnectHandler` (`fabric.go:262-264`) is log-only, set once at
`New()`, and REPLACED (not chained) through `Config.Options`
(`fabric.go:68-70`). So the trigger needs a NEW seam, and its shape is an
API-shape call on the frozen 3-method interface — put to Matt rather than
decided by the author, and ruled Option A.

- **Option A — add an `EventFabric` reconnect method (ruled).**
  `OnReconnect(fn func()) (Unsubscribe, error)`, chained onto the fabric's
  existing log handler so its outage diagnostics survive, so the delivery
  consumer reaches the trigger through the interface value it already holds.
  - Pro: the trigger lives in one package; the fabric keeps ownership of its own
    reconnect diagnostics; consistent with the PR3 `SubscribeKind` precedent (the
    interface already grew a read-side method for the delivery singleton's needs).
    Con: grows the frozen 3-method seam to four methods (accepted — the seam is
    still pre-GA and grew for `SubscribeKind` on the same reasoning).
- **Option B — assembly-side handler wired through `Config.Options` in T3**
  (rejected): re-emitting the fabric's log line and calling into the consumer.
  - Pro: leaves the `EventFabric` interface untouched. Con: splits the trigger
    across `serve.go` + delivery, and re-implements the outage diagnostics the
    fabric already owns (a caller replacing `ReconnectHandler` loses them,
    `fabric.go:68-70`).

T5 produces the `OnReconnect` method on `EventFabric` + `*Fabric`; it does not
exist on the frozen interface today.

**The fork, part 1 — RLS scope.** Today `Run` marks the whole loop
`ctx = store.WithSystemRole(ctx)` (`consumer.go:316`; BYPASSRLS), justified
because "the fan-out consumer is a cross-tenant background loop"
(`consumer.go:308-315`). The cutover hands each event an explicit
`ref.Tenant`. How is the split drawn?

**Option A — split: system-role for the background sweeps, tenant-scoped
per-event processing (recommended).** `Run` captures the pre-system-role base
ctx; the sweeps and drains (`drainStarts`/`sweepSession`/`sweepPins`/
`sweepOwedMentions`/`scanMissedMentions`, all inherently cross-tenant — they
enumerate every agent/tenant) keep `WithSystemRole`; `onEventRef` builds
`tctx := store.WithTenant(baseCtx, ref.Tenant)` (NOT derived from the
system-role ctx) for the `MessageByID` re-read and the whole
`onMessagePosted` classification+dispatch chain.

- Pro: fail-closed defense-in-depth — a forged/corrupted ref whose RowID
  belongs to another tenant reads zero rows under RLS instead of
  cross-tenant-delivering under BYPASSRLS; the ref's tenant is load-bearing,
  which is the point of stamping it.
- Con: one message's processing path runs under a different DB role than the
  sweep that may re-deliver the same message; `drainSettles`→`fireHeld`'s
  re-read (`settle.go:276`) still runs system-role (the held entry carries
  no tenant today — adding `tenant` to `heldEntry` and tenant-scoping the
  fire re-read is the consistent extension, and Option A includes it).

**Option B — keep whole-loop system-role; `ref.Tenant` is routing-only.**

- Pro: zero RLS churn; smallest diff.
- Con: forfeits the isolation dividend of the tenant-stamped ref; every
  delivery re-read stays BYPASSRLS forever, and the later client-edge migration
  would have to retrofit the split anyway.

**The fork, part 2 — what replaces the bus-lag recovery?** The
`sub.Lagged()` overrun branch (`consumer.go:360-408`) — re-subscribe, then
`sweepAllLive` (`settle.go:315-319`), then `scanMissedMentions` — is
bus-ring-specific: JetStream's durable stream cannot "overrun" a consumer (an
unconsumed backlog is retained and redelivered; a failed callback Naks). With
no lag signal, what still triggers a full live-set sweep?

**This is a GENUINE, LOAD-BEARING FORK (promoted from the design-critic
red-team, CRITICAL).** The draft's original mapping — "`scanMissedMentions` at
start covers the publish-failed window; `sweepAllLive` retriggers on fabric
re-subscribe" — does NOT hold, on two counts:

1. `scanMissedMentions` routes ONLY mentions + ask-answers (`scan.go:35-70`:
   `routeMentionsFor` + `routeAskAnswerFor` + `MarkMentionsRouted`), never
   `fanOut`/`dispatchTo`. A publish-failed PLAIN (mentionless) message appears
   in the scan set (`UnroutedMentionMessages` is `mentions_routed_at IS NULL`
   over ALL messages, `queries/delivery_cursors.sql:56-62`) but gets ZERO
   delivery from it.
2. The "re-subscribe" trigger never fires in the exact scenario that CAUSES
   publish failures — a NATS outage. `nats.MaxReconnects(-1)` with a log-only
   handler (`fabric.go:249-264`) auto-reconnects WITHOUT tearing down the
   `ConsumeContext` (`event_fabric.go:152-171` tears down only on
   Unsubscribe/ctx-done/Close), so the consumer resumes with no re-subscribe
   event. Net: outage → committed messages with failed publishes → for a plain
   deliver to a LIVE, never-restarting recipient, NO trigger ever fires. The RPC
   succeeded; the counter ticked in a log nobody watches; the message is never
   delivered. Silent at-least-once → deliver-at-next-recipient-restart, i.e. for
   an always-on agent, effectively never.

**The fork as put to Matt: what triggers full-set recovery after a publish-side
failure?**

- **Option A — reconnect-hook + periodic floor tick (recommended).** Expose a
  fabric reconnect-notification seam (`fabric.go:66-70` documents callers may add
  NATS handlers safely) and, on reconnect, run `sweepAllLive` +
  `scanMissedMentions` — the direct analog of today's deleted lag branch, mapping
  "transport was degraded" → "run the recovery pass." Add a minutes-scale
  periodic tick running the same pair as a floor, so a single transient publish
  failure while NATS is UP (no reconnect) is still bounded-latency recovered, and
  the mid-run recovery the deleted overrun branch provided is restored.
  `sweepAllLive` (not just the scan) is load-bearing here — it is the only path
  that recovers plain delivers to live recipients.
  - Pro: closes both the outage window and the up-but-transient window; bounds
    the residual to the tick interval; small, no new durable store. Con: a
    periodic sweep is coarse; picks a tick interval (a latency/cost knob).
- **Option B — publisher-side bounded retry of the fabric publish.** The
  `EventRef` `msgID` dedup (`eventref.go:61-64`, within `DefaultDuplicateWindow`)
  makes retries safe by construction; retry a few times before giving up.
  - Pro: closes most of the window at the source, no consumer-side trigger. Con:
    an outage longer than the retry budget still drops; doesn't restore mid-run
    scan recovery; must still pair with SOME trigger for the residual.
- **Option C — transactional outbox.** Make the publish itself durable (see
  Alternatives). Pro: closes the window exactly. Con: a second delivery-trigger
  store, which the frozen record's Postgres-durability-owner constraint pushes
  against.
- **Option D — accept the stall explicitly.** Document that publish-failed plain
  delivers to always-live recipients wait for the recipient's next session
  restart, and rely on the counter/alert. Pro: zero new machinery. Con: a
  silent-until-restart delivery hole in agent messaging — the property today's
  design never had.

**Recommendation: Option A (reconnect-hook + periodic floor tick) for part 2,
plus Option A of part 1 (split scope, incl. tenant-stamped `heldEntry`).** Option
A is the faithful restoration of the trigger this cutover deletes, and composes
with the durable-consumer position semantics; Option B is a cheap
belt-and-suspenders worth adding but not sufficient alone.

### OQ-4 (LOAD-BEARING): cross-instance session locality (single-instance transitional constraint)

**DECISION (Matt, 2026-09-05): Option A — single-instance transitional
constraint.** Matt's note: the whole T3 stack lands together, so the exposure
window is small. A single Server is assumed until the parent record's durable
session bindings land (parent T4); the T4 integration proof here is scoped to
transport claim semantics only. Durable author-session resolution is parent-T4's
job, not this cutover's.

**The fork (promoted from the design-critic red-team, HIGH).** This cutover's
headline motivation is that "an in-process trigger cannot cross Server
instances." It moves the TRIGGER cross-instance (each `MessagePosted` is claimed
by exactly one instance via the durable queue-group), but the dispatch plane the
trigger feeds is instance-local hub RAM: `SessionForAccount`/`LiveAgentSessions`
(`runnerhub/relay_comms.go:179-196`), the held registry, settle edges, and
per-session gates (`consumer.go:195-232`). In a two-instance deployment, an
agent-authored message can be claimed by the instance NOT hosting the author's
session: `onMessagePosted` resolves the author against THAT instance's hub →
`live=false` → the dead-author arm fires immediately from the CURRENT (partial,
still-streaming) blocks (`dispatch.go:70-81`). Hold-until-settle silently
vanishes; worse, `fanOut` stamps `MarkMentionsRouted` over partial text, so a
mention arriving in a later block is structurally excluded from the recovery
scan (mention LOSS, not delay); and if the recipient is live on that instance,
the settled suffix is never redelivered (content loss). The record's own
two-instance proof (T4) would pass GREEN over this hole (it asserts single-claim,
not delivery correctness).

**The fork as put to Matt:**

- **Option A — single-instance transitional constraint (recommended).** State in
  Global Constraints (done) that a single Server is assumed
  until the parent record's durable session bindings land (parent T4,
  `compass-managed-multitenancy/design.md:795`, sequenced AFTER this cutover);
  scope the T4 integration proof to transport claim semantics only; point at what
  parent-T4 must fix (author-session resolution against durable bindings).
  - Pro: honest about what this PR delivers; doesn't front-run the parent's
    sequencing; smallest scope. Con: the cross-instance benefit is only partially
    realized until parent-T4 (the trigger is cross-instance; delivery correctness
    is not).
- **Option B — pull durable author-session resolution into this cutover.**
  Resolve author liveness from durable session state (Postgres session rows)
  instead of the hub map for the hold decision.
  - Pro: real multi-instance delivery correctness now. Con: front-runs the
    parent's T4 sequencing and materially enlarges this PR's scope onto the
    session-binding substrate the parent owns.

**Recommendation: Option A (single-instance transitional constraint),** with the
Global Constraint + the T4 scope note already folded in above; parent-T4 carries
the durable author-session resolution.
