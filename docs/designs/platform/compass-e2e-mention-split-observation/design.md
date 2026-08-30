# Design: E2E observation seam for the mention steer/deliver split (RIG-1788 / H4)

Status: Approved — the three load-bearing forks below were ruled by Matt
(2026-08-19); this record freezes on merge as the contract T1–T6 execute
against. No implementation in this record's PR.
Parent record: [compass-dogfood-e2e](../../infra/ci/compass-dogfood-e2e/design.md) (FROZEN —
this record does not reopen its tier or its assertion; it settles only the
observation MECHANISM that record explicitly deferred).
Ledger-impact: none — this is a `docs/designs/platform/` record; the
design-ledger gate governs product records only (`tools/design-ledger-gate/
index.ts:44-45`: ``export const PRODUCT_DIR = "docs/designs/product"``, and
`isRecord` at `:205`: ``if (!file.startsWith(`${PRODUCT_DIR}/`)) return
false``), so a platform record neither touches `DECISIONS.md` nor needs a
`Ledger-impact:` PR-body line. The line above is belt-and-suspenders
documentation, not a gate requirement.

## Problem / Intent

The frozen dogfood-e2e design requires H4's leg 4 to prove, over the real
stack, that "steer reaches the mentioned peer's real session, deliver reaches
the unmentioned one" (`docs/designs/platform/compass-dogfood-e2e/design.md:663-665`).
The e2e test asserts everything else in H4 already; the split itself is an
explicit deferral (`go/e2e/legthreefour_test.go:239-251`):

> `TODO(RIG-1788): assert the steer-vs-deliver SPLIT on the recipient side —
> [...] This is unconfirmable from the e2e fixture as it stands: the
> steer/deliver op-kind is an AgentControl the delivery consumer dispatches
> over the Runner's per-session Control stream (agent-facing internal
> surface), NOT a comms-bus event a client SubscribeComms observes`

This record decides the observation seam that makes the split assertable from
the harness, and how the test stands up the second (subscribed-but-unmentioned)
recipient. WHAT to assert and the e2e TIER are frozen; only the mechanism is
open here.

## Approach

### The split is server→runner control-lane only — recon ground truth

The op-kind is decided in the delivery consumer and exists nowhere a client
can see it:

- `go/internal/delivery/dispatch.go:203-213` — the two dispatch arms differ
  only in the op constructor:

  ```go
  func (c *Consumer) dispatchTo(ctx context.Context, sessionID string, msg *compassv1.Message) {
      c.gatedDispatch(ctx, sessionID, deliverOp(msg), msg.GetId())
  }
  ...
  func (c *Consumer) dispatchSteerTo(ctx context.Context, sessionID string, msg *compassv1.Message) {
      c.gatedDispatch(ctx, sessionID, steerOp(msg), msg.GetId())
  }
  ```

- `go/internal/delivery/consumer.go:288-307` — `deliverOp`/`steerOp` wrap the
  message in `AgentControl_Deliver` / `AgentControl_Steer`; the comment at
  `:298-300` pins the frozen intent: "the only deliver-vs-steer difference is
  recipient-side (design.md:558-562)".
- The relay envelope is internal: `proto/compass/v1/runner.proto:299-309` —
  "DispatchControl — the Server->Runner relay envelope for a control op";
  `SteerControl`/`DeliverControl` live in the internal `agent.proto:195-228`.

The in-process suite proves the split through a fake
(`go/internal/delivery/mention_test.go:33-55` `TestMentionedMemberGetsSteerNotDeliver`,
`:59-87` `TestUnmentionedSubscriberGetsDeliver`), classifying via
`go/internal/delivery/helpers_test.go:58-67`:

```go
func classifyOp(op *compassv1internal.AgentControl) (opKind, string) {
    switch {
    case op.GetSteer() != nil:
        return opSteer, op.GetSteer().GetMessage().GetId()
    case op.GetDeliver() != nil:
        return opDeliver, op.GetDeliver().GetMessage().GetId()
    ...
```

The e2e tier exists to supersede that fake over the real stack.

### Feasibility finding 1 — the stack is multi-process; a pure-`go/e2e` tap is impossible

The brief's hypothesis ("a test-only injection point at stack construction is
plausible") does NOT hold as an in-process wrap. `stack.Up` spawns the server
as a child **binary**, not an in-process assembly
(`go/internal/stack/stack.go:218-222`):

```go
// 3. compass-server (socket / listen / tls / database).
server, err := s.deps.Supervisor.Start(ctx, serverSpec(cfg, cert))
```

and the fixture compiles those binaries onto PATH (`go/e2e/fixture.go:176-180`
`buildBinariesFromModuleRoot`). The `ControlDispatcher` the consumer uses is
wired inside the compass-server process at serve assembly
(`go/server/sinks.go:126-127`):

```go
func startDeliveryConsumer(gctx context.Context, g *errgroup.Group, commsBus *events.Bus[...], st *store.Store, hub *runnerhub.Hub, log *slog.Logger) {
    c := delivery.NewConsumer(commsBus, st, hub, hub, log)
```

So a dispatcher tap requires a (tiny) **production seam in compass-server**:
an env-gated wrapper around the hub at exactly this call site, streaming
records across the process boundary to the harness. Precedent for test-only
seams living in production delivery code already exists:
`go/internal/delivery/consumer.go:150-154` — "beforeGate ... a TEST-ONLY seam
(nil in production)".

### Feasibility finding 2 — the runner gateway still drops steer ops (production gap)

Discovered during recon and load-bearing for every option: the Runner's
control producer rejects `AgentControl_Steer` as an empty shell
(`go/internal/runner/gateway/control.go:195-200`):

```go
func representable(op *compassv1internal.AgentControl) bool {
    switch op.GetControl().(type) {
    case *compassv1internal.AgentControl_Steer,
        *compassv1internal.AgentControl_Replay,
        *compassv1internal.AgentControl_Config:
        return false
```

and `send` fails such ops (`control.go:516-517`: `if !representable(op) {
return connect.NewError(connect.CodeInvalidArgument, errEmptyControlVariant) }`).
That guard is STALE: `SteerControl` has carried a populated
`compass.v1.Message` since the mention work (`proto/compass/v1/agent.proto:202-205`:
"message SteerControl { ... Message message = 1; }"), and the agent side
already consumes it
(`packages/compass-agent/src/transport/control-source.ts:22-27`: "BOTH carry
the comms Message on the wire (`SteerControl.message` / `DeliverControl.message` ...)
— decoded here and dispatched through `immediate.steer` / `immediate.deliver`").
Only the gateway's must-not-send list was never updated when steer was
populated (its own comment at `control.go:69-70` un-parked Deliver but not
Steer).

Consequence: over the real stack today a steer is dispatched by the server,
relayed to the Runner (`go/internal/runner/dispatch.go:469`
`d.host.Deliver(ctx, c.DeliverControl.GetSessionId(), c.DeliverControl.GetOp())`),
and then **dropped at the gateway** — it never reaches the mentioned agent.
This is exactly the class of composed-path bug the e2e tier exists to catch,
and it caught it at design time. Un-parking steer (mirror of the Deliver
un-park pinned by `go/internal/runner/gateway/control_test.go:284-288`
`TestControlSendsDeliver`) is a prerequisite for any assertion that claims
"steer reaches the mentioned peer's real session".

### Option A — store-side delivery-cursor read: FORECLOSED

The cursor records receipt, never kind. `go/internal/store/delivery_cursors.go:18-22`
seeds only `(agent_account_id, channel_id, acked_seq)`:

```sql
INSERT INTO agent_delivery_cursors (agent_account_id, channel_id, acked_seq)
```

and `AckDelivery` advances it identically for both kinds
(`delivery_cursors.go:79-83`: "resolves messageID → messages.seq ... marks the
seq acked (retained in above_seqs), then advances the contiguous cursor") —
the agent's `DeliveryAck` carries a message id, not an op kind
(`proto/compass/v1/agent.proto:230-233`: "DeliveryAck — the agent's
per-message delivery receipt ... Correlates to the delivered message by id").
No stored column distinguishes steer from deliver; a cursor read CANNOT make
the split observable. Ruled out.

### Option B — client session-frame read: CONSTRAINED, cannot carry kind today

The public per-session stream carries a typed trace + lifecycle only.
`proto/compass/v1/compass.proto:524-528`:

```proto
message AgentSessionFrame {
  string session_id = 1;
  SessionEvent event = 2;
  AgentSessionState state = 3;
}
```

with `SessionEvent`'s oneof limited to
`assistant_text/thinking/tool_call/tool_call_update/plan/notice`
(`compass.proto:453-460`). No control op-kind is representable on any client
surface. So a pure-client observer can only see downstream EFFECTS (turn
edges), which cannot distinguish the kinds for idle recipients: the agent
contract makes an idle deliver start a turn just as an idle steer does
(`packages/compass-agent/src/agent.ts:252-254`: "a frame arriving while the
agent is idle starts a new turn; only the @-mention steer interrupts a turn in
progress"). A behavioral-only proof therefore needs a scripted LONG turn held
mid-flight on the mentioned peer so the interrupt is visible — new canned-stub
machinery to hold an SSE stream open until steered — and even then it proves
the effect, not the dispatch decision, and it is blocked on Finding 2 anyway
(the steer never arrives). Kept as documented fallback, not recommended.

### Option C — public trace event carrying op-kind: REJECTED

Adding a `SessionEvent`/`AgentSessionFrame` variant that surfaces the control
op-kind to clients contradicts the frozen intent that the difference is
"recipient-side" and internal (`consumer.go:298-300` quoting frozen
design.md:558-562), widens a public proto (`SessionEvent` is "the render
contract for the UI's session pane", `compass.proto:440-441`) for a test-only
need, and creates a permanent client-visible surface the product design never
sanctioned. Rejected; recorded here so it is not re-litigated.

### RECOMMENDED — Option 1: env-gated dispatch tap in compass-server + receipt via existing session tails

Two independent signals compose into the frozen assertion:

1. **Kind, per recipient (the tap).** compass-server, when a test-only env
   var (`COMPASS_TEST_DISPATCH_TAP`, carrying a unix-socket path) is set at
   serve assembly, wraps the hub in a recording `delivery.ControlDispatcher`
   before `delivery.NewConsumer` (`go/server/sinks.go:127`). The wrapper dials
   the harness's listener once and writes one NDJSON record per
   `DispatchControl` call: `{"session_id":..., "op":"steer"|"deliver"|"other",
   "message_id":...}` — the over-the-wire analogue of the in-process
   `dispatchRecorder`, classifying exactly as `helpers_test.go:58-67` does.
   Unset env (production) = nil wrapper, zero behavior change. The harness
   listens before `stack.Up`, then event-gates on blocking, ctx-bounded socket
   reads (no polling, no sleeps — `rule://no-retries`, matching the legs'
   discipline stated at `legthreefour_test.go:38-40`). The env var reaches the
   child because the process adapter inherits the parent environment
   (`go/internal/stack/adapters/process.go:64`:
   `cmd.Env = append(os.Environ(), spec.Env...)`), threaded explicitly via a
   `stack.Config` field → `serverSpec` Env entry (mirroring how the A4 knobs
   prove their forward path, `go/e2e/fixture.go:225-229`).

2. **Receipt, per recipient (existing primitives).** The delivered/steered
   message drives a turn on each idle recipient (agent contract above), and
   the harness already event-gates turns: `OpenSessionTail`/`AwaitTurnSettled`
   over `SubscribeAgentSession` (`go/e2e/agent_ops.go:87-157`). A settled turn
   on the mentioned peer after the mention post proves the steer genuinely
   reached its real session (this arm is RED until Finding 2's un-park lands —
   the intended present-but-red discipline the leg already uses,
   `legthreefour_test.go:33-40`); a settled turn on the unmentioned subscriber
   proves the deliver reached its session. Tap gives KIND, tail gives RECEIPT;
   together they are "steer reaches the mentioned peer's real session, deliver
   reaches the unmentioned one" with no public surface change.

Why this over B: it asserts the ground-truth dispatch decision (the thing
`mention_test.go` proves in-process) at the same seam, deterministically,
per-recipient, and it composes with — rather than depends on — timing of turn
content. Why the receipt arm at all: a tap-only assertion sits BEFORE the
runner gateway (`hub.DispatchControl` is send-only,
`go/internal/runnerhub/dispatch_control.go:22-27`: "The error return is a
SYNCHRONOUS refusal only"), so tap-only would have gone green while Finding 2
silently dropped every steer — the receipt arm is what makes the assertion
honest end-to-end.

The receipt arm proves the steer ARRIVED at the mentioned peer's real
session as the dispatched op-kind (tap) and drove a real turn there (tail);
it does not separately assert the mid-turn-INTERRUPT semantics of a steer,
because both recipients are idle when the mention lands and an idle deliver
starts a turn just as an idle steer does
(`packages/compass-agent/src/agent.ts:252-254`). Proving
interrupt-vs-coalesce is a distinct, heavier scenario (a peer scripted
mid-turn) the frozen H4 green condition does not require — H4 green is
"steer reaches the mentioned peer's real session" (frozen `design.md:663`),
not "interrupts a live turn" — so it is out of scope here, noted so the
residual is not mistaken for a gap.

**The second real peer.** No third container: the SPAWNER is reused as the
subscribed-but-unmentioned recipient. It is an agent with a live session
already stood up by leg 3; the test subscribes it to the peer's home channel
via `CommsService.UpdateChannelMembers` ("One RPC covers join,
subscribe-toggle...", `proto/compass/v1/comms.proto:62-65`), which makes it a
member of the deliver set (`go/internal/store/delivery_reads.go:15-29`
`SubscribedAgents` — subscribed-or-home members, author excluded; the author
of the mention post is the fixture's human admin, so neither agent is
excluded). The mentioned peer gets ONLY the steer, the spawner ONLY the
deliver — the exact two-recipient shape of
`TestUnmentionedSubscriberGetsDeliver` (`mention_test.go:75-86`) over the real
stack.

The spawned peer reaches the SAME canned backend the leg drives, not one of
its own: `AgentModel`, `Mounts`, and `EgressAllow` are Runner-host-level
(`go/internal/runner/host.go:117-142`, `go/internal/stack/spec.go:94-95`), so
every container the host launches shares the fixture's canned model endpoint,
and the receipt arm's green on BOTH recipients depends on this (the leg-3
"peer has no canned backend of its own" comment describes ownership, not
reachability).

**Extend, not a new test.** The split assertions replace the
`TODO(RIG-1788)` block inside `TestLegThreeFourSpawnAndMessaging`
(`legthreefour_test.go:239-255`) rather than standing up a new test: the
frozen H4 green condition is "one ordered run" (frozen design.md:663), the
spawner/peer/containers/tails the split needs are exactly the ones leg 3 built,
and a second podman test would pay the full multi-minute stack+container cost
again in the CI job that runs this tier
(`.github/workflows/ci.yml:694-717`, rolled up as required via `:774-796`).

**Canned-script determinism.** The steer- and deliver-driven turns dial the
shared canned backend and would race the positional script — the same problem
the root-supervisor Setup turn already has, solved by body-marker routing
(`go/e2e/cannedmodel.go:137-152`: "The handler classifies a request as the
supervisor's Setup turn by this marker and serves it setupReply off a SEPARATE
counter"). The mention text is a body marker of the same kind: requests whose
body carries it are served a fixed text turn off-script, so each leg's ordered
script stays drawn only by the test's scripted turns.

## Global Constraints

- Design layer only in this record's PR; implementation follows as its own
  task-sliced PRs.
- Public repo: no internal-infrastructure names anywhere; no `RIG-`/`SEA-`
  issue ids in source or test code (commit subjects / PR bodies only).
- Every wait in the e2e leg is event-gated and ctx-bounded: no sleeps, no
  polling, no retries (the discipline pinned at `legthreefour_test.go:38-40`).
- The new assertion runs inside the required `dogfood-e2e` CI job
  (`.github/workflows/ci.yml:694-717` — `go test -tags podman -race`, with the
  fail-on-skip guard at `:719-753` and the `CI` rollup at `:774-796`); it must
  stay `-race` clean.
- The tap is test-only: env unset ⇒ production wiring is byte-for-byte
  unchanged (`startDeliveryConsumer` passes the bare hub).
- The frozen parent record is not edited; present-but-red is the accepted
  intermediate state for the receipt arm until T1 lands (matching how the leg
  itself rode RED awaiting H3, `legthreefour_test.go:33-40`).
- Go code follows the package's existing seams and naming; no new proto
  messages, fields, or RPCs anywhere in this design.

## Plan

Ordering: T1 (production un-park) and T2 (tap seam) are independent; T3-T5
(harness) depend on T2's contract; T6 (the assertion) depends on all prior.
Each task carries its own red→green cycle and is one reviewable PR-sized unit.

### T1 — Un-park `AgentControl_Steer` at the runner gateway (production fix)

The stale must-not-send entry drops every real steer (Finding 2). Remove
`*compassv1internal.AgentControl_Steer` from the `representable` reject list
(`go/internal/runner/gateway/control.go:197`) and update the stale comments
(`control.go:65-70`, `:191-194`) — the exact mirror of the Deliver un-park.

Interfaces:

- Consumes: `func representable(op *compassv1internal.AgentControl) bool`
  (`control.go:195`) — reject list shrinks to `AgentControl_Replay`,
  `AgentControl_Config`, `nil`.
- Produces: a steer op accepted by
  `func (p *controlProducer) Send(sessionID string, op *compassv1internal.AgentControl) error`
  (`control.go:230`) and drained to the agent socket; downstream
  `agent.ts` `steer(msg: Message): void` (`agent.ts:261`) already consumes it.

Test cycle: red — move the `"steer"` case out of
`TestControlRejectsEmptyVariants` (`control_test.go:232-238`) and add
`TestControlSendsSteer` mirroring `TestControlSendsDeliver`
(`control_test.go:284-307`: Send a populated steer, assert
`got.GetSteer().GetMessage().GetId()` off the stream); it fails with
`errEmptyControlVariant` on today's code. Green — the one-line reject-list
change.

`TestControlSendsSteer` also asserts the admitted steer is retained until
acked and retired on ack (the kind-agnostic retention path `control_test.go`
already exercises for deliver), so the un-park does not regress the
durable-until-acked contract for the newly-admitted variant.

### T2 — Dispatch tap seam in compass-server

Env-gated wrapper at the one consumer wiring site.

Interfaces:

- Consumes: `delivery.ControlDispatcher`
  (`go/internal/delivery/consumer.go:42-44`:
  `DispatchControl(ctx context.Context, sessionID string, op *compassv1internal.AgentControl) error`);
  the wiring site `startDeliveryConsumer` (`go/server/sinks.go:126-127`); env
  var `COMPASS_TEST_DISPATCH_TAP` (a unix-socket path).
- Produces, in `go/server` (new file `dispatch_tap.go`):
  - `type tapDispatcher struct { next delivery.ControlDispatcher; mu sync.Mutex; enc *json.Encoder }`
  - `func newTapDispatcher(next delivery.ControlDispatcher, socketPath string) (*tapDispatcher, error)`
    — dials the harness listener once (`net.Dial("unix", socketPath)`); a
    dial failure is a hard serve-assembly error (env set means a test asked
    for the tap; failing loud beats silently asserting nothing).
  - `func (t *tapDispatcher) DispatchControl(ctx context.Context, sessionID string, op *compassv1internal.AgentControl) error`
    — classifies the op (`op.GetSteer()` / `op.GetDeliver()`, the
    `classifyOp` shape from `delivery/helpers_test.go:58-67`), writes one
    NDJSON record `{"session_id":string,"op":"steer"|"deliver"|"other","message_id":string}`
    under `mu`, then delegates to `next` unconditionally (record-then-forward;
    a tap write error is logged, never fails the dispatch).
  - `startDeliveryConsumer` change: `var dispatch delivery.ControlDispatcher = hub`,
    wrapped iff `os.Getenv("COMPASS_TEST_DISPATCH_TAP") != ""`.

Test cycle: red — a `go/server` unit test wiring `tapDispatcher` over a fake
next dispatcher and an in-test unix listener asserts the record content, kind
classification, forward-always semantics, and concurrency safety under `-race`
(two goroutines dispatching); fails to compile before the seam exists. Green —
the wrapper. (Consumer sweep dispatches — `settle.go:182,242` — also flow
through `c.dispatch` and thus the tap; harness matching is by
`message_id`+`session_id`, so extra records are benign noise by design.)

### T3 — Thread the tap path through the stack spec

Explicit contract, mirroring the A4 knobs' forward-path proof.

Interfaces:

- Consumes: `stack.Config` (`go/internal/stack/config.go`),
  `func serverSpec(cfg Config, cert CertResult) ProcessSpec`
  (`go/internal/stack/spec.go:25`), `ProcessSpec.Env`
  (`go/internal/stack/deps.go:55-59`), and the adapter's env append
  (`adapters/process.go:64`).
- Produces: `Config.DispatchTapSocket string` (doc-commented TEST-ONLY, empty
  in production); `serverSpec` appends
  `Env: []string{"COMPASS_TEST_DISPATCH_TAP=" + cfg.DispatchTapSocket}` iff
  non-empty (today `serverSpec` sets no Env — `spec.go:26-38` — so this is
  additive).

Test cycle: red — extend the spec unit test
(`go/internal/stack/spec_test.go`, the Env discipline at `:110-116`) to assert
the entry appears iff the field is set; fails before the field exists. Green —
the field + append.

### T4 — Fixture tap listener + `AwaitDispatch` primitive

Interfaces:

- Consumes: the fixture root (`go/e2e/fixture.go:198` `shortRoot` — sun_path
  budget applies, the socket lives under it), `stack.Config` build
  (`fixture.go:218-237`), the timeouts convention (`go/e2e/timeouts.go`).
- Produces, in `go/e2e` (new file `dispatch_tap_ops.go`, podman-tagged):
  - a `DispatchRecord` type with `SessionID`, `Op`, and `MessageID` string
    fields (JSON tags `session_id` / `op` / `message_id`), one record per
    tapped op.
  - fixture option `func WithDispatchTap() fixtureOption` — before `stack.Up`:
    `net.Listen("unix", filepath.Join(root, "tap.sock"))`, set
    `cfg.DispatchTapSocket`, register listener close on `t.Cleanup`; a
    background accept feeds a buffered record channel the primitive drains.
  - `func (f *Fixture) AwaitDispatch(ctx context.Context, match func(DispatchRecord) bool) (DispatchRecord, error)`
    — blocking, ctx-bounded receive from the record channel until `match`
    (the `AwaitDelivery` shape, `go/e2e/comms_ops.go:62-66`). Draining is
    MATCH-based, not positional, so the steer-vs-deliver arrival order is
    immaterial (each await matches its own `{op, session, message_id}`).
  - `func (f *Fixture) DispatchRecords() []DispatchRecord` — a mutex-guarded
    snapshot of every record drained so far. Records are RETAINED in an
    in-fixture slice (never discarded on a failed `match`): the exclusion
    assertion (T6 step 6) reads this AFTER the receipt arm settles, so a
    record arriving between the positive match and the settle is still
    visible to the negative check. Retaining is load-bearing — a
    discard-on-miss primitive cannot support "peer received no deliver for
    this id", and a sweep can redeliver a dropped steer AS a deliver
    (`go/internal/delivery/settle.go:241` builds `deliverOp` for every owed
    message), the exact record the exclusion must be able to see.

Test cycle: red — with T2+T3 landed, a minimal fixture-level exercise (the
leg-2 test extended or a focused harness test): post to a live agent's home
channel with the tap armed and `AwaitDispatch` a `deliver` record for the
posted message id; fails before the option/primitive exist. Green — the
listener + primitive.

### T5 — Second-recipient plumbing: subscribe primitive + session resolution

Interfaces:

- Consumes: `CommsService.UpdateChannelMembers` (`comms.proto:65`); the store
  read surface the e2e already uses (`store.Open(ctx, f.DSN())`,
  `legthreefour_test.go:118-122`; precedent for e2e-motivated exported reads:
  `AgentByHandle`, `AgentOwner` at `:142,171`); `agent_sessions` schema
  (`go/internal/store/migrations/0001_init.sql:340-348` — `session_id` PK,
  `agent_account_id` FK, `agent_sessions_agent_idx`).
- Produces:
  - `func (f *Fixture) SubscribeMember(ctx context.Context, channelID, accountID string) error`
    in `go/e2e/comms_ops.go` — one `UpdateChannelMembers` call adding the
    account as a subscribed member.
  - `func (s *Store) AgentSessionIDs(ctx context.Context, agent AccountID) ([]string, error)`
    in `go/internal/store/agent_sessions.go` — the sessions bound to an agent
    account (index-backed), so the harness can resolve the spawned peer's
    session id and open its tail BEFORE the mention post (the tail stream is
    live-fan with no replay — `legthreefour_test.go:124-128` — so opening
    after the post races the turn's edges).

Test cycle: red — a store pgtest asserting `AgentSessionIDs` returns exactly
the recorded session (mirrors `agent_sessions_test.go` conventions) fails
before the method exists; `SubscribeMember`'s proof is T6's deliver arm.
Green — the two additions.

### T6 — The split assertion in `TestLegThreeFourSpawnAndMessaging`

Replace the `TODO(RIG-1788)` block (`legthreefour_test.go:239-255`) with the
ordered split assertions; arm the fixture with `WithDispatchTap()` and extend
the canned script/routing.

Interfaces:

- Consumes: T4's `AwaitDispatch`, T5's `SubscribeMember`/`AgentSessionIDs`,
  the existing `OpenSessionTail`/`AwaitTurnSettled` (`agent_ops.go:87-157`),
  and a canned-backend marker route for the mention-driven turns — extend
  `go/e2e/cannedmodel.go` with a configurable body-marker route mirroring the
  Setup-turn route (`cannedmodel.go:137-157`, request-body routing at
  `:309-321`): `func CannedMarkerReply(marker, reply string) fixtureOption`
  (served off-counter, so the positional script is untouched).
- Produces, appended to the one ordered run after the existing bus-fan assert
  (`:229-237`):
  1. `SubscribeMember(peer.Agent.HomeChannelID, spawnerID)` — the spawner
     becomes the subscribed-but-unmentioned recipient (before the mention
     post).
  2. Resolve the peer's session via `AgentSessionIDs`; open its tail; the
     spawner's tail (`:129-133`) is already open and is reused.
  3. Post the `@mention` (existing, `:217-221`).
  4. KIND (positive): `AwaitDispatch` twice — a `{Op: "steer", SessionID:
     peerSession, MessageID: messageID}` record and a `{Op: "deliver",
     SessionID: spawnerSession, MessageID: messageID}` record (match-based
     drain, the two-recipient shape of `mention_test.go:75-86`).
  5. RECEIPT: `AwaitTurnSettled` on both tails — the steer-injected turn on
     the peer and the deliver-coalesced turn on the spawner, each settling on
     the `CannedMarkerReply` route (marker = the mention text, which the
     coalesced/steered prompt bodies carry).
  6. KIND (exclusion, AFTER both settles): read `DispatchRecords()` and
     assert that within the window up to each recipient's settle the peer
     session has NO `deliver` record and the spawner NO `steer` record for
     the message id. The exclusion is WINDOW-SCOPED to the settle, not an
     absolute negative: the frozen design permits a steered-but-unacked
     message to be sweep-redelivered as a deliver later
     (`go/internal/delivery/dispatch.go:122-123`, frozen `design.md:546-548`),
     so an absolute "never a deliver" would assert a guarantee the system
     does not make (OQ4).
- No `RIG-`/`SEA-` id in the added code; the replaced TODO comment goes away
  with the deferral it marked.

Test cycle: red — with T1 unmerged the receipt arm's peer settle never comes
(steer dropped at the gateway) and with T2-T5 unmerged the leg does not
compile; the assertion's own red is exercised by temporarily reverting T1
locally (steer record present at the tap, peer turn never settles — proving
the receipt arm is the end-to-end guard, not decoration). Green — the full
ordered run passes under the CI `dogfood-e2e` job's `go test -tags podman
-race` (`ci.yml:694-717`) with the fail-on-skip guard (`:719-753`) still
derived-green.

## Tasks

- [ ] T1 — un-park `AgentControl_Steer` in `representable`
      (`gateway/control.go:197`) + `TestControlSendsSteer`; comments updated.
- [ ] T2 — `tapDispatcher` in `go/server/dispatch_tap.go`, env-gated wrap in
      `startDeliveryConsumer`; unit test incl. `-race` concurrency.
- [ ] T3 — `stack.Config.DispatchTapSocket` → `serverSpec` Env entry; spec
      unit test.
- [ ] T4 — `WithDispatchTap()` fixture option + `AwaitDispatch` /
      `DispatchRecords` primitives in `go/e2e` (records retained, match-based
      drain); fixture-level deliver-record proof.
- [ ] T5 — `Fixture.SubscribeMember` + `store.AgentSessionIDs`; store pgtest.
- [ ] T6 — replace the split TODO in `TestLegThreeFourSpawnAndMessaging` with
      the kind(positive) + receipt + kind(exclusion-after-settle) assertions;
      `CannedMarkerReply` route; green in the `dogfood-e2e` CI job.

## Resolved Decisions

The three load-bearing forks were put to Matt and ruled (2026-08-19); the
rulings below are the frozen contract. The rejected alternatives are retained
as the rationale trail, not open questions.

1. **[RESOLVED — seam choice] Option 1: env-gated dispatch tap + receipt via
   session tails.** Ruled in. The tap asserts the ground-truth op-kind per
   recipient, deterministically, with no public-surface change, at the cost of
   a small env-gated production seam in compass-server (in-package precedent
   for test-only seams, `consumer.go:150-154`). The receipt arm (T6 step 5) is
   kept: a tap-only assertion sits before the runner gateway and would have
   stayed green across the very steer-drop bug this recon found. Rejected:
   (2) behavioral-indirect via `SubscribeAgentSession` only — cannot
   distinguish kinds for idle recipients (`agent.ts:252-254`), needs new
   hold-a-turn-open stub machinery, proves effect not decision, and is blocked
   on the same gateway un-park anyway; (3) a public trace-event variant
   carrying op-kind — permanent client-visible surface creep against the
   frozen "recipient-side/internal" intent.

   Weighed-and-rejected sub-alternatives to the tap's gating/placement:
   (i) build-tag gating (`-tags` in `buildBinariesFromModuleRoot`,
   `go/e2e/fixture.go:176-180`) instead of an env var — removes the tap from
   production binaries entirely, but the e2e then no longer exercises the
   byte-identical production binary; rejected on binary-fidelity grounds.
   (ii) tapping at the runner gateway post-`representable`
   (`go/internal/runner/gateway/control.go`) — one signal that is both
   op-kind AND past-the-gateway (would have caught Finding 2 tap-only), at a
   symmetric env-gate cost in compass-runner; rejected because it observes a
   point further from the dispatch DECISION the assertion is about and still
   needs the receipt arm for the peer's session-arrival, so it buys nothing
   over tap+receipt.

2. **[RESOLVED — steer un-park scope] T1 rides this design as its first,
   independent PR.** Ruled: fold the production fix here, not a separate
   issue. The runner gateway drops every steer today (`gateway/control.go:197`
   — Finding 2), which no in-process test catches because the split suite
   observes dispatch, not the gateway; the un-park is a one-line reject-list
   change with a mirrored `TestControlSendsSteer`, prerequisite to any
   truthful "steer reaches the real session" green, and lands next to the
   recon that found it. It is independent of the harness tasks (T2 onward), so
   it reviews and merges on its own even though it ships in this stack.

3. **[RESOLVED — exclusion semantics] Window-scoped to the peer's turn
   settle.** Ruled: window-scoped, NOT an absolute negative. T6's exclusion
   ("the mentioned peer received no *deliver* for this message id") is scoped
   to the observation window ending at the peer's turn settle, because the
   frozen design permits a steered-but-unacked message to be sweep-redelivered
   as a plain deliver on a later reconnect/resync
   (`go/internal/delivery/dispatch.go:122-123`;
   `go/internal/delivery/settle.go:241` builds `deliverOp` for every owed
   message; frozen `design.md:546-548`). An absolute negative would assert a
   guarantee the system does not make. This also hardens T6's
   red-by-reverting-T1 check: with the exclusion re-read after settle, a sweep
   that greens the receipt arm under reverted T1 is caught by the deliver
   record it leaves on the peer session.

### Settled implementation detail (not a fork)

**Tap plumbing shape:** the explicit `stack.Config.DispatchTapSocket` →
`serverSpec` Env entry (T3) — a testable, visible contract mirroring the A4
knobs, preferred over implicit parent-env inheritance (`t.Setenv`). Minor,
recommendation adopted; T3 assumes the explicit form.
