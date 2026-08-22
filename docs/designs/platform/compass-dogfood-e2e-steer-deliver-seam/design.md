# Compass dogfood e2e — steer/deliver split observation seam

Status: Active

Extends the frozen dogfood-e2e harness contract
([`../compass-dogfood-e2e/design.md`](../compass-dogfood-e2e/design.md), §A5
legs 3-4, which frames the deliverable as "a reusable e2e harness"). This record
covers only the remaining leg-4 assertion RIG-1788 owes: proving, **over the real
wire**, that an `@`-mention delivers a **steer** to the mentioned live agent
session while a subscribed-but-unmentioned peer gets a **deliver**.

> **Premise correction (load-bearing).** An earlier decision picked "an
> in-process decorator wrapping the `ControlDispatcher` inside the fixture's
> stack." That rests on a false belief that the e2e stack is in-process. It is
> **not**: `stack.Up` spawns compass-server as an `os/exec` **child process**
> (`go/internal/stack/stack.go:219` → `go/internal/stack/adapters/process.go:63`),
> and the fixture reaches it only over HTTPS (`go/e2e/fixture.go:288`). The
> delivery consumer, hub, and `ControlDispatcher` all live in the **child's**
> address space, so a Go decorator constructed in the test process cannot observe
> them. This record re-derives the seam against the real multi-process
> architecture; Matt ratified the corrected seam (option (2), agent-side
> session-trace event) on 2026-08-21 — see Decision.

## Problem / Intent

`go/e2e/legthreefour_test.go` passes today (spawn + second container + the
deliver-side bus fan), but the steer-vs-deliver **split** on the recipient side
is a deferred `TODO(SEA-1788)` (legthreefour_test.go:239-255). The op-kind
(`steer` | `deliver`) travels Server→Runner→agent over the Runner's per-session
`Control` stream (`proto/compass/v1/agent_gateway.proto:77`) — an agent-facing
internal surface no client RPC observes — so the over-the-wire e2e has no way to
assert the split.

The routing *decision* is already proven **in-process** against the real
`Consumer`: `go/internal/delivery/mention_test.go` drives the real consumer
through the real events bus (`c.bus.Publish`) and asserts a mentioned member gets
`opSteer` while an unmentioned subscriber gets `opDeliver` (cases 1-2,
mention_test.go:33-87) — with a **fake** dispatcher recording the op-kind. What
that in-process test cannot prove is that the chosen op survives the real wire.
**RIG-1788/#476 is direct evidence this gap matters**: the gateway's
`representable()` rejected every populated steer with `CodeInvalidArgument`, so
every over-the-wire steer to a live session was dead — yet `mention_test.go`
stayed green, because its fake dispatcher never crosses `Hub.DispatchControl` →
Runner → agent. The e2e's unique job is exactly that wire crossing.

Intent: add a **deterministic, reusable, cross-process** observation of which
control op-kind each recipient session actually received over the wire for a
given message, then use it to close the leg-4 split assertion with a second real
peer.

## Global Constraints

- **Go**, behind the repo devenv shell (`direnv exec … go …`). The e2e package is
  `//go:build podman` and stands up a real **multi-process** stack via `stack.Up`
  inside `NewFixture` (`go/e2e/fixture.go:169`): a spawned postgres child, a
  spawned compass-server child, and a spawned compass-runner child
  (`go/internal/stack/stack.go:193,219,247`). The test process is a **client**
  of that server over TLS, not a co-resident of it.
- **No sleeps, no polling, no retries** (`rule://no-retries`). Every wait is
  event-gated + `ctx`-bounded, matching the existing fixture primitives
  (`AwaitDelivery` at `go/e2e/comms_ops.go:62`, `AwaitTurnSettled` at
  `go/e2e/agent_ops.go:131` — a goroutine-pumped `stream.Receive()` raced against
  a derived deadline).
- **Determinism over behavior.** Matt's ruling: "spend the upfront cost of
  setting up the best test harnesses so that adding more tests later is easy as
  possible, and they are deterministic and not flaky." The observation reads the
  **actual dispatched/received op-kind**, never infers it from turn-timing.
- **Cross-process by construction.** The observation must cross the test↔server
  process boundary; the ratified seam (option (2)) crosses it as an agent-side
  session-trace event on the existing `SubscribeAgentSession` stream.
- **Reusable.** Any future leg asserting "what op-kind did session X receive for
  message Y" uses the same primitive.

## Why the earlier in-process-decorator plan cannot work

The delivery `Consumer` does dispatch every op through one injected interface,
and server assembly wires it in one place:

```go
// go/internal/delivery/consumer.go:42-44 (the interface)
type ControlDispatcher interface {
    DispatchControl(ctx context.Context, sessionID string, op *compassv1internal.AgentControl) error
}
// go/server/sinks.go:135-136 (the sole wiring; hub is the dispatcher)
c := delivery.NewConsumer(commsBus, st, hub, hub, log)
```

But `startDeliveryConsumer` runs inside the **compass-server binary**, which the
fixture launches as a child via `deps.Supervisor` (`stack.go:219`, an `os/exec`
child — `adapters/process.go:63`). A decorator constructed in the *test* process
has no reference into the *child's* consumer. `stack.Deps` inverts only external
*process* effects (`ProcessSupervisor`, `CertEnsurer`, …; `deps.go:14-47`) — it
carries no in-memory Go collaborator that reaches server internals, and it
cannot, across a process boundary. So the observation must cross the wire.

## Alternatives considered

The two the earlier pass already reasoned about stay rejected on their merits:

- **(a) DeliveryAck-frame observation — rejected, INSUFFICIENT.** The recipient's
  `DeliveryAck` carries **only** `messageId`, no op-kind, and both steer and
  deliver emit an identical ack at injection
  (`packages/compass-agent/src/transport/control-source.ts:400` — `markApplied(seq)`
  is the single apply reached after both the steer and deliver branches). A
  DeliveryAck read cannot tell the two recipients apart.
- **(c) Behavioral observation via turn structure — rejected, FLAKY.** A steer is
  a mid-turn interrupt and a deliver is turn-end coalesced
  (`control-source.ts:361-387`, `immediate.steer` vs `immediate.deliver`). The
  difference is observable only by forcing a deterministic mid-turn window with
  the canned script — timing-sensitive, the flakiness Matt's ruling rejects. The
  leg-4 peer is idle (drives no turn), so there is no turn to observe anyway.

Two dead ends this pass ruled out with fresh grounding:

- **(d) In-process `ControlDispatcher` decorator — IMPOSSIBLE.** See above: the
  consumer lives in a child process.
- **(e) Store delivery-cursor read — IMPOSSIBLE.** `agent_delivery_cursors` is a
  single contiguous `acked_seq` low-water mark (`go/internal/store/migrations/0001_init.sql:447-450`);
  steer and deliver both advance the same cursor. No op-kind is persisted.

The surviving candidates — the OQ1 fork — are:

- **(1) A test-only `ProcessSupervisor` + a structured dispatch log line.** Add
  one `slog` line at the server's single dispatch chokepoint
  (`Hub.DispatchControl`, `go/internal/runnerhub/dispatch_control.go:38-54`, which
  emits none today) carrying `session_id`, `op_kind`, `message_id`; the fixture
  supplies a `stack.Deps.Supervisor` decorator that tees the compass-server
  child's stderr (already forwarded — `adapters/process.go:65`) and an
  `AwaitControlDispatch` waiter tails the teed lines. **Pro:** minimal production
  change (one log line), no proto/API surface, cross-process, reusable. **Con:**
  the test couples to a log-line format (a structured `slog` line with a pinned
  message keeps it stable, but it is still string-parsing) and it observes the
  *server's send*, one hop short of the agent actually receiving it.
- **(2) A first-class session-trace event (proto append + agent-side emit).** Add
  a `SessionInjection { op_kind, message_id, from_handle }` case to the
  `SessionEvent` oneof (`proto/compass/v1/compass.proto:453-460`; additions are
  non-breaking appends per the file's own note, line 447), emitted **agent-side**
  when `control-source.ts` dispatches a steer vs a deliver (`:383-386`), relayed
  up the existing telemetry spine and observed over `SubscribeAgentSession`
  (compass.proto:111) with the existing event-gated `AwaitTurnSettled`-class
  machinery. **Pro:** deterministic, event-gated on the existing typed stream,
  reusable, crosses the **full** wire (server→runner→agent→back — the exact path
  #476's bug lived on, so this test would have caught it), and doubles as a real
  product signal (a session-tail pane can show "steered by @X" vs "delivered from
  @Y"). **Con:** the largest scope — a product-API addition (proto + agent emit +
  the frame plumbed through the runner relay).
- **(3) Accept the in-process coverage; narrow leg-4's e2e to the wire-observable
  effects.** Keep the deliver-side bus-fan + spawn + container assertions already
  green; document that the steer/deliver *decision* is proven in-process
  (mention_test.go) and close the TODO without a new seam. **Pro:** cheapest, no
  new surface. **Con:** leaves exactly the wire gap #476 fell through — an
  in-process green with a dead wire — so it is the weakest against the class of
  bug we just shipped a fix for.

## Decision

**Option (2), the agent-side session-trace event — ratified by Matt
(2026-08-21).** It is the only candidate that crosses the full
server→runner→agent wire (so it would have caught #476), it is deterministic and
event-gated on the existing typed session stream rather than on log-string
parsing, it is reusable for any future op-kind assertion, and it is a genuine
product signal rather than test-only scaffolding — directly Matt's "invest in the
best harness" principle (the emit is a one-line sibling of the existing
`deliveryAck`, on the FrameSink path that ack already rides). The one real cost is a
**public** proto surface (`SessionEvent.SessionInjection`): op-kind becomes
client-visible. This reverses **no** frozen decision. The frozen parent frames the
split only as its observable outcome — "steer reaches the mentioned peer's real
session, deliver reaches the unmentioned one" (`../compass-dogfood-e2e/design.md:664`)
— and takes **no** position on whether the op-kind is client-visible; the only
"recipient-side / internal" characterization is a code-comment gloss
(`go/internal/delivery/consumer.go:309`), not a design ruling. So the public signal
decides a question the parent left open rather than overriding one it closed, and it
doubles as a genuine product signal (a session tail showing "steered by @X" vs
"delivered from @Y"). Options (1) and (3) are not taken; they remain recorded
above as the rejected alternatives.

> **Supersedes the earlier `compass-e2e-mention-split-observation` record (PR
> #441).** That record settled the same seam with an env-gated dispatch tap
> **inside compass-server** (a test-only seam threaded through the production
> binary) plus a kind-blind turn-settle receipt arm. On the corrected
> multi-process grounding both approaches catch the #476 drop class equally; the
> deciding factors are that this record's signal (a) adds **no** test-only code
> to the production server, (b) observes the op-kind at the agent's actual
> post-wire receipt point in one signal rather than composing a pre-gateway tap
> with a kind-blind receipt, and (c) doubles as a product signal. #441's steer
> un-park (its T1) shipped independently as PR #476 and stands under either
> record. #441's seam-independent grounding (spawner-reuse for the 2nd peer,
> `UpdateChannelMembers` subscribe, canned body-marker determinism,
> window-scoped exclusion) is folded into the Plan/Tasks below.

## Plan

In dependency order — T2 depends on T1, T3 on T1+T2.

- **T1 — the cross-process op-kind signal.** A `SessionInjection` case on the
  `SessionEvent` oneof, emitted **agent-side** and relayed up the existing
  telemetry spine to an `AgentSessionFrame`, so the op-kind rides the same typed
  session stream a client already tails. **Emit site (idle-safe, load-bearing):**
  the emit fires in `CompassAgent.steer()` / `deliver()`
  (`packages/compass-agent/src/agent.ts:294`, `:216`) right beside the existing
  `this.#sink.emit({kind:"deliveryAck", …})` (agent.ts:254/324/362/409/514), so
  it uses the **exact** FrameSink path the ack already rides and fires at the same
  point — "emitted" ≡ "injected". This matters because leg-4's peers are **idle**
  (drive no model turn): the op-kind dispatch and injection happen on the event
  loop at control decode (`control-source.ts:383-386` → `agent.ts` steer/deliver),
  independent of any running turn, so the emit fires for an idle recipient — a
  turn-scoped emit would silently never observe the idle peer's deliver
  (false-green). **Publish lane (F3):** the injection frame must ride a
  non-dropped path — the FrameSink trace/session queue is bounded drop-oldest
  (`transport/publish-spine.ts:21-26`), so either pin the injection frame off the
  drop-oldest lane or bound the leg's frame count so it cannot overflow.
- **T2 — the Fixture `AwaitControlDispatch` primitive.** An event-gated waiter
  over `SubscribeAgentSession` frames selecting the `SessionInjection` case,
  `ctx`-bounded via a derived deadline exactly as `AwaitTurnSettled`, exposing
  `func (f *Fixture) AwaitControlDispatch(ctx, sessionID string, match func(opKind, messageID string) bool) (opKind string, err error)`.
- **T3 — the leg-4 split assertion.** Extend the one ordered run in
  `TestLegThreeFourSpawnAndMessaging` (replace the `TODO(SEA-1788)` block,
  legthreefour_test.go:239-255) — not a new podman test, which would re-pay the
  multi-minute stack+container cost. **Second recipient reuses the leg-3
  spawner** (subscribed-but-unmentioned) — no third container: subscribe it to
  the mentioned peer's home channel via `CommsService.UpdateChannelMembers`
  (`proto/compass/v1/comms.proto:65`) so it joins the deliver set
  (`go/internal/store/delivery_reads.go` `SubscribedAgents`, author excluded;
  the mention post's author is the fixture human admin, so neither agent is
  excluded). Open **both** peers' `SubscribeAgentSession` observation **before**
  the `@`-mention post (the tail is live-fan with no replay —
  legthreefour_test.go:124-128 — so opening after the post races the turn
  edges). Assert peer-1 → `steer`, spawner → `deliver` for the same message id
  (positive), then read the retained injection frames and assert peer-1 saw no
  `deliver` and the spawner no `steer` for that id within the window
  (exclusion). **Exclusion is window-scoped**, not an absolute negative: the
  frozen design permits a steered-but-unacked message to be sweep-redelivered as
  a plain deliver later (`go/internal/delivery/settle.go:241` builds a
  `deliverOp` for every owed message), so an absolute "never a deliver" would
  assert a guarantee the system does not make. The existing leg-3 (spawn +
  container) and leg-4 bus-fan assertions stay.
- **Canned-script determinism (load-bearing).** The steer- and deliver-driven
  turns dial the shared canned backend and would race the positional script —
  the same problem the root-supervisor Setup turn already solves by body-marker
  routing (`go/e2e/cannedmodel.go:137-157`). The mention text is a body marker of
  the same kind: a `CannedMarkerReply(marker, reply)` fixture option serves a
  fixed off-script turn to any request whose body carries the marker, so each
  leg's ordered script stays drawn only by its scripted turns.

## Tasks

- [ ] **T1 — cross-process op-kind signal**
  - Interfaces: a public `SessionInjection` case on `SessionEvent`
    (`compass.proto`) `{ SessionInjectionKind op_kind; string message_id; string from_handle; }`
    plus a new public `SessionInjectionKind` enum (`UNSPECIFIED=0, STEER, DELIVER`) —
    see OQ2 (forced public, the internal `AgentControl` discriminant cannot cross
    the gen-fence); the agent-side emit added in `CompassAgent.steer()`/`deliver()`
    (`agent.ts:294`/`:216`) as a sibling `this.#sink.emit` beside the existing
    `deliveryAck` emit (reuse `EventMapper`'s `#sessionEvent` id/clock stamping,
    mapping.ts:237); the runner relay + server hop carrying the new `SessionEvent`
    case to `AgentSessionFrame` (the same path today's `assistant_text`/`notice`
    frames ride — no new relay, one new oneof arm).
  - Red-green: a compass-agent unit test asserting `steer(msg)`/`deliver(msg)`
    each emit exactly one `SessionInjection` frame with the right `op_kind` +
    `message_id`, beside the existing `deliveryAck` (mirrors agent.test.ts's
    deliver/steer emit assertions). Fails before the emit arm exists.
- [ ] **T2 — Fixture AwaitControlDispatch primitive**
  - Interfaces: `func (f *Fixture) AwaitControlDispatch(ctx context.Context, sessionID string, match func(opKind, messageID string) bool) (opKind string, err error)`,
    event-gated + `ctx`-bounded, mirroring `AwaitTurnSettled` (agent_ops.go:131).
- [ ] **T3 — leg-4 steer/deliver split assertion**
  - Interfaces: extends the one ordered run in `TestLegThreeFourSpawnAndMessaging`
    (`go/e2e/legthreefour_test.go`, replacing the `TODO(SEA-1788)` block); consumes
    T2's `AwaitControlDispatch`. Second recipient is the **reused leg-3 spawner**
    (no third container), subscribed to the mentioned peer's home channel via a
    fixture `SubscribeMember(ctx, channelID, accountID)` wrapping
    `CommsService.UpdateChannelMembers` (`comms.proto:65`). Resolve the peer's
    session id (open its tail before the mention post) via the store read the leg
    already uses (`store.Open(ctx, f.DSN())`; precedent exported reads
    `AgentByHandle`/`AgentOwner`). Canned determinism via a
    `CannedMarkerReply(marker, reply)` fixture option routing the mention-marked
    turns off the positional script (mirror `cannedmodel.go:137-157`). Positive:
    peer-1 → `steer`, spawner → `deliver` for the message id; exclusion
    (window-scoped to each recipient's turn settle): peer-1 no `deliver`, spawner
    no `steer` for that id.

Note on ledgers: this record lives in the sealed platform design corpus
(`docs/designs/platform/`), which the design-ledger-gate governs only for the
**product** corpus (`docs/designs/product/DECISIONS.md`). A platform record adds
no DECISIONS row and declares no ledger delta, mirroring its frozen parent
([`../compass-dogfood-e2e/design.md`](../compass-dogfood-e2e/design.md) note on
ledgers). Ledger-impact: none.

## Resolved decisions

- **OQ1 — the cross-process observation mechanism. RESOLVED (Matt, 2026-08-21):
  option (2), the agent-side session-trace event.** See Decision. The two
  fallbacks (1) and (3) are recorded under Alternatives; they are not taken.
- **OQ2 — where the op-kind enum lives. RESOLVED (forced): a new public
  `SessionInjectionKind` enum on `compass.proto`.** Not a real fork: `AgentControl`
  / `SteerControl` live in `agent.proto`, marked INTERNAL-ONLY
  (`proto/compass/v1/agent.proto:3-6`) and fenced off the public client gen by
  `buf.gen.yaml:28-32` (a leak trips the SEA-1267 gen-fence check in
  `proto/moon.yml`). `SessionEvent` is on the **public** `compass.proto`
  (`SubscribeAgentSession`'s payload), so its `SessionInjection` case cannot carry
  the internal discriminant — the public enum is the only option that keeps the
  gen-fence intact.
