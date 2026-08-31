# Compass first-turn delivery — remove `initial_prompt`

Status: Draft

## Problem / Intent

Compass carries an `initial_prompt` field through its whole start contract
(proto → server → runner → agent SDK → UI → e2e harness), but the Runner never
sends it into the container — `(*agentHost).Start` reads only the container
name (`go/internal/runner/host.go:284-285`: `func (h *agentHost) Start(ctx
context.Context, req *compassv1.StartAgentSessionRequest, resumeBody string)`
… `name := req.GetContainerName()`; no `GetInitialPrompt` call exists in the
file), so every prompt riding the field is silently dropped and the agent
idles forever. Matt has ruled the field out of existence: **remove
`initial_prompt` from everything; an agent session always starts idle; its
first turn arrives through its channel.** This record designs the removal and
the channel-first-turn model, and is what unblocks RIG-1792 H8 / PR #256
(dogfood e2e `TestLegTwoRealTurn` is red today precisely because its first
turn rides the dropped field).

## Approach

### The frozen ruling (design HOW, not WHETHER)

Matt, verbatim:

> "Managers get started in two ways: 1. the initial root manager that starts
> on boot, and 2. a manager that was provisioned by another manager. For 2,
> the initial prompt would just be a DM from the starting manager to the new
> one — it doesn't need an initial_prompt field. For 1, this is part of the
> whole Setup flow — the manager needs to ask the user what repos/projects,
> set up the tree/devenv shell(s), etc. The best way to do this might be an
> initial Setup thread in their home channel, created by Compass itself. But
> then we need a `@compass` reserved alias for Compass itself to send messages
> into threads. So ultimately I think we get rid of the initial_prompt, and
> drop it from everything: the provisioning Manager sends a message to the new
> one in 99% of cases, and then in the setup one it's a custom flow based
> around an initial Setup thread."

No `initial_prompt` fallback survives anywhere. The cutover is clean: field
removed and reserved, every consumer flipped in one atomic change.

### The seam the first turn rides (case 2 — the server + agent halves exist; the Runner middle leg is UNBUILT)

**Honest state (driver source-verified).** The server-side fan-out/dispatch and
the agent-side idle-deliver arm both exist, but a posted channel message does
NOT reach a production agent session today: the Runner middle leg is unbuilt at
three points. Case 2 therefore needs a **Runner deliver-lane build** (gaps a+b)
plus a **fresh-start barrier-lift** (gap c) — see PR-A (T-R3, the OQ-A ruling
folded below). The three gaps:

- **Gap (a) — runner dispatch has no `DeliverControl` arm.**
  `go/internal/runner/dispatch.go:359` switches on
  Start/Provision/Stop/Remove/Reload/Status/SecretsVersion/ConfigVersion only;
  a server-pushed `SessionsResponse_DeliverControl` falls to the default and
  returns `errorResult(id, errors.New("unrecognized session command variant"))`
  (`dispatch.go:446-449`). The server DOES send this variant —
  `go/internal/runnerhub/dispatch_control.go:44-53` wraps the op in
  `SessionsResponse_DeliverControl` and `router.send1`s it — and the wire
  variant exists (`go/internal/gen/compass/v1/runner.pb.go:618,655`). The send
  path is live; the receive arm is missing. **T-R1 builds it.**
- **Gap (b) — gateway control lane rejects deliver/steer.**
  `go/internal/runner/gateway/control.go:192-203` `representable()` returns
  `false` for `AgentControl_Deliver` and `AgentControl_Steer` (and
  Replay/Config) — "the four parked variants are empty shells." Even with arm
  (a) built, the send is dropped as non-representable. **T-R2 fixes it** (both
  now carry a comms `Message`).
- **Gap (c) — no fresh-start `replay_complete` sender + the agent barrier
  defaults CLOSED, and a refused deliver is acked away (stranded).** Runner-side
  `HoldForReplay` has "No production caller exists yet"
  (`go/internal/runner/gateway/control.go:421-429`). Agent-side barrier:
  `packages/compass-agent/src/transport/control-source.ts:278`
  `let replayComplete = false`; a `deliver`/`steer` arriving before
  `replayComplete` is decoded is refused-and-counted ("live immediate op before
  ReplayComplete — refused by replay barrier", `control-source.ts:373-377`),
  then `acks.markApplied(seq)` (`:390`) acks it — pruning Runner retention, so
  it is NEVER redelivered. On a FRESH (non-resume) start nothing sends
  `replay_complete`, so the barrier never lifts and the first deliver is
  refused-and-stranded. **T-R3 lifts it**: on a FRESH start the Runner sends
  `AgentControl{replay_complete}` as the first control op after Bind (Matt's
  OQ-A ruling, folded below).

The pieces that DO exist, and that PR-A/PR-B compose with:

1. **Fan-out** — the delivery consumer wraps a posted message in the deliver
   op: `go/internal/delivery/consumer.go:288-291` —
   `func deliverOp(msg *compassv1.Message) *compassv1internal.AgentControl`
   builds `AgentControl_Deliver{Deliver: &DeliverControl{Message: msg}}`.
2. **Dispatch** — `consumer.go:42-43`: `ControlDispatcher` is
   `DispatchControl(ctx context.Context, sessionID string, op
   *compassv1internal.AgentControl) error`; `:55` `LiveAgentSessions() map
   [store.AccountID]string` resolves live recipients.
3. **Relay** — `go/internal/runnerhub/dispatch_control.go:38-53`
   `DispatchControl` wraps the op and `router.send1`s it down the server→runner
   `SessionsResponse` stream — the send that gaps (a)+(b) currently strand.
4. **Turn start** — `packages/compass-agent/src/agent.ts:183` `deliver(msg:
   Message)` with the RT-3 contract at `:83-85`: "A delivered channel message
   is coalesced to a turn-end prompt: mid-turn delivers queue and flush as ONE
   prompt when the turn settles; **an idle deliver starts a turn at once.**"
   This is downstream of the barrier (gap c) — a pre-`replayComplete` deliver is
   refused at `control-source.ts:373-390` before it ever reaches `deliver()`.
5. **Owed-message sweep** — a message posted before the session is live is not
   lost once the lane is built: `go/internal/delivery/settle.go:47-53`
   `OnSessionStarted(sessionID, account)` (the hub's `SessionStartSink`,
   `go/internal/runnerhub/hub.go:95-98`, fired at `promoteSession` right after
   `StartAgentSession` binds account→session) ENQUEUES the start edge onto the
   consumer's queue (`settle.go:51-53`); the reconnect sweep runs later in the
   consumer loop and redelivers every owed message to the freshly-live session.

So the intended first-turn model is: **start the session idle, then post the
brief into a channel the new agent receives on; the idle-deliver arm starts its
first turn** — once the Runner deliver-lane (PR-A) carries the deliver and the
barrier lifts. Both orderings (post-then-start via the owed sweep,
start-then-post via live dispatch) converge on the same turn.

**Which channel carries the case-2 "DM":** a **per-pair manager↔peer DM
channel** — a `ChannelKindDM` channel (`go/internal/store/types.go:74-75`:
`ChannelKindDM ChannelKind = 1`, "a direct conversation between two accounts")
whose two members are the spawning manager and the new peer, auto-provisioned on
the spawn edge (Matt's OQ-B ruling). This is a point-to-point carrier, NOT the
manager's coordination channel — that channel is a BROADCAST: its members are
the manager plus ALL of its direct reports (`go/internal/store/coordination.go`
`CoordinationReports` seeds `members := []AccountID{managerAgentID}` then appends
every `agent_accounts` row with `parent_agent_id = manager`, `coordination.go:314-334`)
under an OWNER_ONLY + mandatory_subscription policy (`CoordinationChannelSpec`,
`coordination.go:54-70`), so a brief posted there starts/queues a turn on EVERY
sibling report, not just the addressee. Matt's intent is token-minimization:
most messages should flow through these pairwise DM channels so siblings do not
spend tokens on briefs they don't need to see.

The DM channel reuses primitives that already exist:
`store.CreateChannel` + `expandOwnerMembership` (`go/internal/store/channels.go:72-140`,
`expandOwnerMembership` at `:198`) create a 2-member channel and carry BOTH
members' owning users into membership ("an agent↔agent DM carries both owners",
`channels.go:76-79`), so the operator (the manager's and the peer's owning user)
retains read/respond visibility into every DM channel. What does NOT exist today
is any manager↔peer DM AUTO-PROVISION on the spawn edge — nothing get-or-creates
a pairwise DM when a peer is spawned — so this record ADDS that machinery (see
the Plan task T-R0 below), modeled on the existing coordination reconcile hook
(`CoordinationHook`, `coordination.go:10-23`, fired inside the CreateAgent tx)
but keyed on the `{manager, peer}` pair, 2-member, and idempotent on re-spawn.

`SpawnAsAccount` creates the peer with `ParentAgentID: caller`
(`go/server/lifecycle.go:139-148`). Once the DM exists, the manager posts the
brief into it with its existing `comms_post_message` tool
(`packages/compass-agent/src/comms.ts:263-270`); the peer receives it via
ordinary DM membership — both parties are members, so no mandatory-subscription
broadcast disjunct is needed. For a **human-started** agent, the equivalent is
the owner posting into the agent's home channel — the owner and the agent are
both members from creation and the agent is always-subscribed
(`go/internal/store/accounts.go:124-131`, RT-2); this is unchanged (a home
channel is already a 2-party owner+agent channel). No new tool, RPC, or proto
SHAPE is needed for case 2 — but the Runner deliver-lane AND the DM auto-provision
(PR-A) ARE new machinery. The UI channel-proliferation UX (many DM channels
cluttering the operator's view) is a KNOWN later problem Matt has deferred
("work on how to manage the UX of this better later") — out of scope here.

### Per-site removal (the blast radius, each site re-verified)

| Site | Today (file:line, `ws-first-turn`) | Removal |
| --- | --- | --- |
| `StartAgentSessionRequest.initial_prompt = 2` | `proto/compass/v1/compass.proto:594` (`string initial_prompt = 2;`, doc `:592-593` "Optional initial prompt … Empty = start idle") | Delete field + doc; add `reserved 2; reserved "initial_prompt";` |
| `SpawnAgentRequest.initial_prompt = 2` | `compass.proto:618` (`string initial_prompt = 2;` on the DL-166 composite) | Delete; `reserved 2; reserved "initial_prompt";` |
| `SpawnPeerRequest.initial_prompt = 3` | `proto/compass/v1/agent_gateway.proto:170` (`string initial_prompt = 3; // threaded to StartAgentSessionRequest.initial_prompt`) | Delete; `reserved 3; reserved "initial_prompt";` |
| Generated code | `go/gen/compass/v1/compass.pb.go:2767,2885`, `go/internal/gen/compass/v1/agent_gateway.pb.go:595`, `packages/compass-agent/src/gen/compass/v1/{compass_pb.ts:1143,1206, agent_gateway_pb.ts:299}`, `packages/compass-client/src/gen/compass/v1/compass_pb.ts:1143,1206` | Regenerate via the repo's buf lanes (`buf.gen.yaml`, `buf.gen.internal-go.yaml`, `buf.gen.agent-ts.yaml`) — never hand-edit |
| Server: agent-spawn leg | `go/server/lifecycle.go:325` — `InitialPrompt: req.GetInitialPrompt(),` inside `hub.Start(ctx, "", &compassv1.StartAgentSessionRequest{...})` | Drop the field from the literal |
| Server: composite SpawnAgent | `go/server/spawn.go:135` — `InitialPrompt: msg.GetInitialPrompt(),` in `runSpawn` | Drop the field from the literal |
| Runner | `go/internal/runner/host.go:284-285` — `Start` reads only `req.GetContainerName()`, never the field (the standing bug) | Removing the field makes the drop correct-by-construction (no `host.go` change). NOT "already enrolled for delivery": the Runner deliver-lane is UNBUILT (dispatch has no `SessionsResponse_DeliverControl` arm, `dispatch.go:359/446-449`; `representable()` rejects Deliver/Steer, `gateway/control.go:192-203`) — built additively in PR-A (T-R1/T-R2), NOT in this removal PR |
| Agent SDK spawn tool | `packages/compass-agent/src/lifecycle.ts:90-92` — `"initial_prompt?": type("string").describe("Initial prompt to seed the new peer's first turn")`; `:159` `initialPrompt: params.initial_prompt ?? ""`; tool description "optionally a display name and an initial prompt…" (`:146-148`) | Remove the schema key, the request field, and the description clause; describe the replacement ("post the peer's brief to your DM channel with it after spawn") |
| UI spawn state | `apps/ui/src/spawn.ts:38` `SessionBinding.initialPrompt`, `:55` `SpawnSpec.initialPrompt`, `:82` capture in `beginSpawn`, `:168` `return b.initialPrompt ? "working" : "idle"` | Remove both fields; `bindingDotState` `running` arm returns `"idle"` unconditionally (every spawn is a start-idle spawn) |
| UI start dialog | `apps/ui/src/components/StartAgentDialog.tsx:10-24` — the dialog's only input is the prompt textarea (`spec: Omit<SpawnSpec, "initialPrompt">`, submit re-attaches `initialPrompt`) | Delete `StartAgentDialog` (+ its test); the board's start affordance calls the spawn action directly with `{agentAccountId, workstreamId}`. The user prompts the agent by posting in its home channel — the exact model this record ratifies |
| Go tests | `go/server/lifecycle_pgtest_test.go:79,130`; `go/server/lifecycle_e2e_pgtest_test.go:120,337`; `go/server/service_spawn_pgtest_test.go:39`; `go/internal/runner/gateway/lifecycle_test.go:65` (all `InitialPrompt: "go"` fixtures) | Drop the field from the fixtures (no assertion depends on its value) |
| TS tests | `packages/compass-agent/src/lifecycle.test.ts:179,190,207`; `apps/ui/src/spawn.test.ts:24,38,60,65-67,177,183`; `apps/ui/src/components/StartAgentDialog.test.tsx` (whole file); `apps/ui/src/components/NewWorkstreamDialog.test.tsx:134` (asserts the spec carries NO `initialPrompt` — keep, now trivially true; reword comment) | Update/remove per the code changes |
| e2e harness | `go/e2e/agent_ops.go:50-56` `StartSession(ctx, containerName, initialPrompt)`; `:70-77` `Resume(ctx, container, resumeSessionID, initialPrompt)`; `go/e2e/legtwo_test.go:108` `StartSession(ctx, containerName, "say hello and stop")`; `go/e2e/legthreefour_test.go:58-61` spawn args JSON `"initial_prompt":%q` | See the harness re-model below — the prompt parameter is removed and the first turn moves to `PostMessage` |

Proto discipline: a removed field number/name MUST be `reserved` so the wire
number is never reused — this is a hard Global Constraint below. Because the
regenerated types delete the `InitialPrompt`/`initialPrompt` members, every
consumer breaks at compile the moment the proto lands: **the removal is ONE
atomic PR (PR-B)** across the monorepo (proto + regen + server + runner tests +
SDK + UI + harness) — it cannot be staged internally, since a partial removal
leaves the tree non-compiling.

**Two-PR staging (finding 4).** PR-B does NOT stand alone: the harness re-model
in it (leg-2's first turn moving to a channel `PostMessage`) only greens once a
posted message actually reaches the agent session — which today it does not (the
Runner deliver-lane is unbuilt, §"the seam", gaps a–c). So the Runner
deliver-lane build lands FIRST as its own PR:

- **PR-A — Runner deliver-lane + spawn-edge DM auto-provision (additive,
  independently landable, greens nothing yet).** T-R0 (per-pair manager↔peer DM
  auto-provision on the spawn edge), T-R1 (`dispatch.go`
  `SessionsResponse_DeliverControl` arm), T-R2 (`representable()` admits
  Deliver/Steer), T-R3 (fresh-start barrier-lift, the OQ-A ruling). Purely
  additive — no proto removal, no consumer break — so it is revertable on its
  own and reviewable in isolation.
- **PR-B — atomic `initial_prompt` removal + harness re-model (greens leg-2).**
  The single atomic PR above (T1–T7), landing AFTER PR-A so its re-modeled
  leg-2 turn can actually settle.

This deliberately does NOT fold a new Runner subsystem into the same atomic PR
as the proto removal: coupling a revertable feature (the deliver-lane) to an
unrevertable removal (the reserved field) would be past reviewable size and
would make the feature un-revertable independently of the removal.

### The e2e harness re-model (the H8 unblock)

`TestLegTwoRealTurn` (`go/e2e/legtwo_test.go:65-66`: "CreateAgent -> Provision
-> StartSession(initial_prompt) -> AwaitSessionSettled -> assert … transcript
is non-empty") drives its turn through the removed seam, and the frozen
dogfood-e2e record documents that seam as a primitive
(`docs/designs/platform/compass-dogfood-e2e/design.md:220`:
`StartSession(container, prompt, resumeID)` →
`StartAgentSession{initial_prompt}`). Re-model:

- `StartSession(ctx, container)` — no prompt; `Resume(ctx, container,
  resumeSessionID)` — no prompt. Both still return the minted session id.
- The first turn becomes a **post into the agent's home channel**: the harness
  user is the agent's owner and therefore a home-channel member from creation
  (`accounts.go:124-131`), and `CreateAgentResponse`'s account carries
  `home_channel_id` (`proto/compass/v1/comms.proto:173`) — the fixture's
  `CreateAgent` (`go/e2e/agent_ops.go:18`) grows a home-channel return. The
  existing `PostMessage` primitive (`go/e2e/comms_ops.go:22`) posts the
  scripted prompt; the deliver path carries it; the idle-deliver arm starts
  the turn against the canned model (`go/e2e/fixture.go:80`
  `WithCannedModel`).
- **Settle-wait re-spec — split to fix a subscribe-after-post race.**
  `AwaitSessionSettled` today opens its `SubscribeAgentSession` stream WHEN
  CALLED and returns on the FIRST `AGENT_SESSION_STATE_READY` frame
  (`go/e2e/agent_ops.go:91-101`). The re-modeled order is
  `StartSession` → `PostMessage` → settle-wait, but if the wait opens its
  stream AFTER the post, a canned turn can start AND settle in the
  post→subscribe window, fanning WORKING/READY to zero subscribers → the wait
  hangs. `SubscribeAgentSession` is live-fan (no replay ring), so a missed edge
  is gone. **Fix is ORDERING, not the state machine:** split into
  - `OpenSessionTail(ctx, sessionID) (stream, error)` — opens the
    `SubscribeAgentSession` stream BEFORE `PostMessage`, so it is already
    subscribed when the turn fans. This mirrors the in-repo precedent
    `legthreefour_test.go:176-180` ("Open one subscription before the post so it
    sees the live fan of the deliver-side MessagePosted event").
  - `AwaitTurnSettled(ctx, stream) error` — reads the already-open stream,
    skips frames until a WORKING state is observed, then returns on the next
    READY (WORKING→READY = one settled turn). Same event-gated, no-poll,
    ctx-bounded contract as today.

  Guard note: the "boot-idle READY" premise was an over-claim. On the STREAM the
  agent emits STARTING at boot (`packages/compass-agent/src/agent.ts:147-149`)
  and READY only at `agent_end` (`packages/compass-agent/src/mapping.ts:115-116`);
  the READY at `go/internal/runner/host.go:405,785` is the Runner's Status-answer
  state, NOT a stream frame. So the guard is stated as "ignore everything until
  the first WORKING, then return on the next READY" — not a claim about a
  boot-READY frame appearing on the stream. This also closes the dogfood-e2e
  record's own flag that `AwaitSessionSettled` is "the ONE primitive with no
  grounded wire contract today" (`compass-dogfood-e2e/design.md:223`) — the
  contract is now WORKING→READY on `SubscribeAgentSession`.

  > **Amendment (RIG-3044, 2026-08-31) — the open-before-post ordering is now a
  > server-guaranteed happens-before, not a wall-clock assumption.** The re-spec
  > above fixed the settle-wait race by ORDERING (open the tail before the post),
  > but the ordering it relied on was still a timing assumption: `OpenSessionTail`
  > returned only on the FIRST fanned frame, so on a stone-idle session the client's
  > `SubscribeAgentSession` RoundTrip blocked until the driving post produced a
  > frame — the open could not actually complete before the post it was meant to
  > precede, and a driven injection could fan to zero subscribers before `subscribe()`
  > registered (the RIG-3044 e2e flake, `TestLegThreeFourSpawnAndMessaging` → 157s
  > `settleTimeout`). The fix: `SubscribeAgentSession` now sends a leading
  > zero-payload **registration-ack** frame (the existing `AgentSessionFrame`,
  > `session_id` only, nil event, `AGENT_SESSION_STATE_UNSPECIFIED`) the instant
  > `subscribe()` registers the subscriber — mirroring `SubscribeEvents`'
  > `snapshotBoundary` and `SubscribeComms`' `commsSnapshotBoundary`. `OpenSessionTail`
  > therefore returns on REGISTRATION, so a synchronous open-before-post is now a true
  > happens-before: once the open returns, the subscription is provably live and the
  > post's injections cannot be raced away. This is still a live tail, NOT a replay
  > ring / resync / reattach — the ack carries no history (see DL-310).
- `legthreefour_test.go:59-61` drops `"initial_prompt"` from the canned spawn
  tool-call JSON. This is HYGIENE, not a wire guard: ArkType KEEPS/ignores
  undeclared keys by default (`spawnParameters` sets no `"+": "reject"`), so a
  leftover `initial_prompt` key would be silently IGNORED, not rejected — the
  turn would not fail on it. See the arktype note below.

Post-then-await ordering: open the tail (`OpenSessionTail`) BEFORE `PostMessage`,
which is posted AFTER `StartSession` returns. On the freshly-promoted session the
start edge is ENQUEUED before `StartAgentSession` responds — the
`SessionStartSink` (`go/internal/runnerhub/hub.go:95-98`) only appends the start
edge (`go/internal/delivery/settle.go:51-53`); the reconnect sweep runs later in
the consumer loop — so the post lands as either a live dispatch or an async owed
sweep, both reaching the agent (convergence via live dispatch OR async sweep).
Once the Runner deliver-lane (PR-A) carries the deliver and the barrier is
lifted (OQ-A), the agent's replay barrier no longer strands it: a
pre-`replayComplete` deliver is otherwise refused-and-acked-away at
`packages/compass-agent/src/transport/control-source.ts:373-390`, so the
barrier-lift is a precondition, not a "held until ReplayComplete" guarantee.

### Case 1 — root-manager boot: scoped OUT to a follow-up record

Recommendation: **(b) scope case 1 to its own follow-up record**, with this
record freezing only its shape: the root manager's first turn is a
Compass-authored **initial Setup thread in its home channel**, which requires
a **reserved `@compass` system-sender alias**. Rationale for the split:

- The `@compass` sender breaks a store invariant this record otherwise never
  touches: every `messages.author_account_id` today resolves to a real
  `accounts` row, and `CreateAgent`/handle validation has **no reserved-name
  guard** (`accounts.go:131-137` checks only non-empty — nothing stops a user
  registering the handle `compass` today). A system sender needs its own
  design: reserved-handle enforcement at account creation, whether `@compass`
  is a real reserved account row or a sentinel author, how the UI renders it,
  and how delivery treats its posts (it must never be a deliver *recipient*).
- The Setup flow itself (what the thread says, the repos/projects
  interrogation, tree/devenv bring-up) is product-design work with no
  dependency on — and no blocker for — the `initial_prompt` removal: nothing
  boots a root manager through `initial_prompt` today, so removing the field
  strands no existing case-1 path.
- Case 2 + the harness re-model unblock RIG-1792 H8 now; coupling them to the
  system-sender design would serialize an unrelated, larger decision in front
  of a red CI gate.

The follow-up record owes: the reserved-alias representation, reserved-handle
validation, Setup-thread creation trigger (root-manager first
`StartAgentSession`), thread content/versioning, and its ledger rows. This
record's DL-187 row (below) freezes the shape so the interim cannot regress
into a prompt-field revival. Matt has ruled (OQ-C): the `@compass` reserved
alias is the frozen system-sender mechanism, used for ANY system-level message
sender (not just the root-manager Setup thread) — "the @compass alias will be
used for any system-level messages, so we can freeze it now." The follow-up
record inherits the mechanism and details only its representation + the Setup
flow.

### Alternatives considered

- **Keep `initial_prompt` as a deprecated no-op field** — rejected by Matt
  verbatim ("drop it from everything"); also perpetuates the silent-drop bug
  as API surface.
- **Case-2 brief via the manager's coordination channel** — rejected (OQ-B,
  Matt): the coordination channel is a BROADCAST to the manager plus ALL direct
  reports (`coordination.go:314-334` / `CoordinationChannelSpec`), so every
  sibling would receive a brief meant for one peer, spending tokens on messages
  they don't need. The chosen carrier is a per-pair `ChannelKindDM` channel
  (§"Which channel carries the case-2 'DM'").
- **Bake an @-mention-the-addressee convention onto the coordination broadcast**
  — rejected (OQ-B): it leaves every sibling receiving the deliver and only
  upgrades the named peer to a steer, still burning sibling tokens; the per-pair
  DM avoids the broadcast entirely.
- **A dedicated `PromptControl` first-turn op** — redundant: `DeliverControl`
  with the idle-deliver arm IS the first-turn primitive, already ratified
  (DL-071/073). Its agent-side arm is wired (`agent.ts:183`); its Runner middle
  leg is not (PR-A builds it, §"the seam") — a new `PromptControl` would need
  the SAME Runner deliver-lane, so it adds a redundant op for no saved work.

## Global Constraints

1. **Reserved on removal.** Every removed proto field reserves BOTH its number
   and its name: `StartAgentSessionRequest` → `reserved 2; reserved
   "initial_prompt";`; `SpawnAgentRequest` → `reserved 2; reserved
   "initial_prompt";`; `SpawnPeerRequest` → `reserved 3; reserved
   "initial_prompt";`. Wire numbers are never reused.
2. **No fallback, no shim.** No deprecated field, no server-side tolerance
   read, no "empty means idle" remnant text. A session starts idle,
   unconditionally; the only turn-start inputs are the existing control lane
   ops (deliver/steer/prompt-from-resume-replay).
3. **Gen code is generated.** All four gen trees (`go/gen`, `go/internal/gen`,
   `packages/compass-agent/src/gen`, `packages/compass-client/src/gen`)
   regenerate via the repo buf lanes; hand-edits are review failures.
4. **Two-PR staging (PR-A then PR-B).** PR-B (the removal) cannot be staged
   internally (the regenerated types break every consumer at compile); its
   tasks T1–T7 land as slices of that one atomic PR. But it lands AFTER PR-A,
   the additive Runner deliver-lane + DM auto-provision (T-R0/T-R1/T-R2/T-R3), because PR-B's re-modeled
   leg-2 turn only greens once a posted message reaches the session (§"the
   seam"; §"Two-PR staging").
5. **Frozen records stay frozen.** `compass-agent-spawn-despawn`,
   `compass-spawn-control`, and `compass-dogfood-e2e` contain now-superseded
   `initial_prompt` content; they are NOT edited. Supersession is recorded in
   the ledger (DL-186) and in this record.
6. **Event-gated tests only.** The re-modeled harness wait stays no-sleep,
   no-poll (the same bar as today's `AwaitSessionSettled`,
   `agent_ops.go:91-101`), now via `OpenSessionTail` + `AwaitTurnSettled`.

## Plan

### PR-A — Runner deliver-lane + spawn-edge DM auto-provision (additive; lands first)

These tasks build the Runner middle leg §"the seam" shows is unbuilt, plus the
per-pair manager↔peer DM auto-provision the case-2 brief posts into.
Additive only (no proto removal), independently landable, greens nothing yet.

#### T-R1 — Runner dispatch: `DeliverControl` arm (lane: implement-hard, Go runner)

Add a `case *compassv1internal.SessionsResponse_DeliverControl:` arm to the
`dispatcher.execute` switch (`go/internal/runner/dispatch.go:359`), mirroring
the existing arms' shape. It unwraps `c.DeliverControl.GetOp()` (the
`AgentControl`) and `c.DeliverControl.GetSessionId()` and routes the op to that
session's control producer, so a server-pushed deliver is no longer met by the
`:446-449` default's `"unrecognized session command variant"`. The send side is
already live (`go/internal/runnerhub/dispatch_control.go:44-53`;
`go/internal/gen/compass/v1/runner.pb.go:618,655`).

#### T-R2 — gateway control: admit payload-carrying Deliver/Steer (lane: implement, Go runner)

`go/internal/runner/gateway/control.go:192-203` `representable()` currently
returns `false` for `AgentControl_Deliver` and `AgentControl_Steer`. Both now
carry a comms `Message` (RIG-1569 populated the parked shells), so they ARE
representable — remove them from the reject set (leaving Replay/Config, still
empty shells). Without this the send from T-R1 is dropped before it reaches the
socket.

#### T-R0 — spawn-edge manager↔peer DM auto-provision (lane: implement-hard, Go store + comms)

On the spawn edge, get-or-create a per-pair manager↔peer DM channel so the
manager can post the peer's brief point-to-point (OQ-B ruling; §"Which channel
carries the case-2 'DM'"). Model it on the existing coordination reconcile hook:
`CoordinationHook` (`go/internal/store/coordination.go:10-23`) is a comms-layer
callback the store invokes INSIDE the `CreateAgent`/`ReparentAgent` tx right
after writing `parent_agent_id`, so the channel reconcile commits atomically
with the parent-edge write without the store importing comms types. This task
adds an analogous pairwise reconcile that get-or-creates a `ChannelKindDM`
channel (`go/internal/store/types.go:74-75`) for the `{manager, peer}` pair via
`CreateChannel` + `expandOwnerMembership` (`go/internal/store/channels.go:72-140`)
— a 2-member channel that carries both members' owning users, so the operator
keeps visibility (`channels.go:76-79`). It must be idempotent/get-or-create,
keyed on the pair, so a re-spawn resolves the SAME DM rather than minting a
duplicate. Reuses existing primitives only (`ChannelKindDM`, `CreateChannel`,
`expandOwnerMembership`, the hook precedent); the impl lane details the keying
and the get-or-create SQL. Lands in PR-A (additive, no proto removal) so PR-B's
harness re-model can post the first turn into this DM.

#### T-R3 — fresh-start barrier-lift (lane: implement-hard, Go runner + TS agent)

A FRESH (non-resume) session's agent barrier defaults CLOSED
(`packages/compass-agent/src/transport/control-source.ts:278`
`let replayComplete = false`) and refuses+acks-away a pre-`replayComplete`
deliver (`:373-390`), and nothing sends `replay_complete` on a fresh start
(`go/internal/runner/gateway/control.go:421-429`: `HoldForReplay` has no
production caller). Per Matt's OQ-A ruling, on a FRESH start the Runner sends
`AgentControl{replay_complete}` as the first control op after Bind (seq 1,
FIFO-first on the control stream, so it always drains before any deliver) — ONE
mechanism symmetric with the future resume path, needing no agent change. The
PR-A deliver-lane (T-R1/T-R2) is landable ahead of it, but case-2 delivery to a
fresh session is not green until T-R3 lands.

### PR-B — atomic removal + harness re-model (lands after PR-A)

### T1 — Proto removal + reservation + regeneration (lane: implement, Go/proto)

Remove the three fields, reserve numbers+names, regenerate all gen trees.

Interfaces:

- `proto/compass/v1/compass.proto` — `message StartAgentSessionRequest {
  string container_name = 1; reserved 2; reserved "initial_prompt"; string
  resume_session_id = 3; }`; `message SpawnAgentRequest { string
  agent_account_id = 1; reserved 2; reserved "initial_prompt"; string
  client_request_id = 3; }`.
- `proto/compass/v1/agent_gateway.proto` — `message SpawnPeerRequest { string
  handle = 1; string display_name = 2; reserved 3; reserved "initial_prompt";
  string client_request_id = 4; }`.
- Regenerate: `go/gen/compass/v1/compass.pb.go`,
  `go/internal/gen/compass/v1/agent_gateway.pb.go`,
  `packages/compass-agent/src/gen/compass/v1/{compass_pb.ts,
  agent_gateway_pb.ts}`, `packages/compass-client/src/gen/compass/v1/
  compass_pb.ts` — `GetInitialPrompt()`/`initialPrompt` members disappear.

### T2 — Server: drop the two forwarding sites (lane: implement, Go server)

Interfaces:

- `go/server/lifecycle.go:323-326` — `l.hub.Start(ctx, "",
  &compassv1.StartAgentSessionRequest{ContainerName: container})` (the
  `InitialPrompt:` line at `:325` deleted).
- `go/server/spawn.go:133-136` — `s.StartAgentSession(ctx,
  connect.NewRequest(&compassv1.StartAgentSessionRequest{ContainerName:
  container}))` (the `InitialPrompt:` line at `:135` deleted).
- Test fixtures drop `InitialPrompt: "go"`:
  `go/server/lifecycle_pgtest_test.go:79,130`,
  `go/server/lifecycle_e2e_pgtest_test.go:120,337`,
  `go/server/service_spawn_pgtest_test.go:39`.

### T3 — Runner: idle-start assert + gateway test (lane: implement, Go runner)

The `initial_prompt` removal itself is behavior-preserving in the runner
(`host.go:284-285` never read the field). The runner's PRODUCTION change for
first-turn delivery is NOT in this task — it is the additive deliver-lane build
in PR-A (T-R1 `dispatch.go` `SessionsResponse_DeliverControl` arm, T-R2
`representable()` fix, T-R3 barrier-lift). This T3 slice is only the
compile-against-regenerated-types proof plus gateway fixture cleanup.

Interfaces:

- `go/internal/runner/host.go:284` — signature unchanged: `Start(ctx
  context.Context, req *compassv1.StartAgentSessionRequest, resumeBody
  string) (string, error)`; compiles against the regenerated type.
- `go/internal/runner/gateway/lifecycle_test.go:65` — drop `InitialPrompt:
  "go"` from the `SpawnPeerRequest` fixture.

### T4 — Agent SDK: spawn tool sheds the parameter (lane: implement, TS agent)

Interfaces:

- `packages/compass-agent/src/lifecycle.ts` — `spawnParameters = type({
  handle: …, "display_name?": … })` (the `"initial_prompt?"` key at `:90-92`
  removed); the `SpawnPeerRequestSchema` create at `:156-166` loses
  `initialPrompt:` (`:159`); tool `description` (`:146-148`) rewritten to:
  "Spawn a new peer agent owned by your owner. Provide a unique handle and
  optionally a display name. The peer starts idle — post its brief to your DM
  channel with the new peer to start its first turn."
- `packages/compass-agent/src/lifecycle.test.ts:178-207` — remove
  `initial_prompt` from the params fixture and the `spawn.initialPrompt`
  asserts. Do NOT add an unknown-key reject assert: ArkType KEEPS/ignores
  undeclared keys by default (rejecting needs a `"+": "reject"` config
  `spawnParameters` does not set), so a stray `initial_prompt` key would be
  silently IGNORED, not rejected — the negative assert would be false. Dropping
  the key from the leg-3/4 canned JSON is pure hygiene (the ignored key is
  harmless), not a wire guard. See the arktype note under the harness re-model.

### T5 — UI: promptless start (lane: implement, UI)

Interfaces:

- `apps/ui/src/spawn.ts` — `SessionBinding` loses `initialPrompt` (`:35-38`);
  `SpawnSpec` becomes `{ readonly agentAccountId: string; readonly
  workstreamId: string }` (`:55` deleted); `beginSpawn(spec, requestId)`
  no longer captures a prompt (`:82`); `bindingDotState` `running` arm
  (`:168`) returns `"idle"` unconditionally (live `AgentSessionState` takes
  over from the first attributed status, per the existing DL-167 reconcile).
- `apps/ui/src/components/StartAgentDialog.tsx` + `.test.tsx` — its only input
  (the prompt textarea) is gone, so the dialog is **deleted** (Matt's
  design-PR-gate ruling on OQ-1): the board start affordance invokes the spawn
  action directly with `SpawnSpec` (`{agentAccountId, workstreamId}`; spawn is
  already guarded by the DL-164/168 live-session predicate and idempotent under
  its request id). This amends DL-185's "Kept: `StartAgentDialog`" clause — the
  one status-flip, ordered after #267/RIG-1932 merges (see below).
- `apps/ui/src/spawn.test.ts:24,38,60,65-67,177,183` — drop prompt fixtures;
  the two `bindingDotState` running-arm cases collapse to one (`running` →
  `"idle"`).
- `apps/ui/src/components/NewWorkstreamDialog.test.tsx:134` — keep the
  no-lifecycle-fields assert; update its comment (there is no prompt field
  anywhere anymore).

### T6 — e2e harness re-model onto the channel first-turn seam (lane: implement-hard, Go e2e) — unblocks RIG-1792 H8 / PR #256

Interfaces:

- `go/e2e/agent_ops.go` — `func (f *Fixture) StartSession(ctx context.Context,
  containerName string) (sessionID string, err error)`; `func (f *Fixture)
  Resume(ctx context.Context, containerName, resumeSessionID string)
  (sessionID string, err error)`; `func (f *Fixture) CreateAgent(ctx
  context.Context, handle, displayName string) (accountID, homeChannelID
  string, err error)` (home channel read from
  `CreateAgentResponse.account.agent.home_channel_id`, `comms.proto:173`).
- `func (f *Fixture) OpenSessionTail(ctx context.Context, sessionID string)
  (stream, error)` + `func (f *Fixture) AwaitTurnSettled(ctx context.Context,
  stream) error` — together REPLACE `AwaitSessionSettled` and fix the
  subscribe-after-post race. `OpenSessionTail` opens the
  `SubscribeAgentSession` stream (called BEFORE `PostMessage`, mirroring
  `legthreefour_test.go:176-180`); `AwaitTurnSettled` reads that already-open
  stream, skips frames until a WORKING state is seen, then returns on the next
  READY (WORKING→READY = the settled turn); same derived deadline contract as
  today (`agent_ops.go:92-93`).
- `go/e2e/legtwo_test.go` (`TestLegTwoRealTurn`) — new scenario order:
  `CreateAgent` → `Provision` → `StartSession(container)` →
  `OpenSessionTail(sessionID)` → `PostMessage(homeChannelID, "leg2", "say hello
  and stop")` (`comms_ops.go:22`) → `AwaitTurnSettled(stream)` → transcript
  asserts unchanged (canned reply present). The tail opens BEFORE the post so a
  fast canned turn cannot fan WORKING/READY into the post→subscribe gap.
  `TestLegTwoPrimitives` (`legtwo_test.go:56`) drops its prompt argument.
- `go/e2e/legthreefour_test.go:59-61` — canned spawn args JSON becomes
  `{"handle":%q,"display_name":%q}`.

### T7 — Ledger delta + case-1 follow-up reference (lane: driver)

The driver lands the ledger rows below in the same PR
(`design-ledger-gate`), sets this record's `Status:` header on merge. The
case-1 follow-up is already tracked as **RIG-1820** ("Auto-seed root Manager
'supervisor' on embedded first-launch (dogfood)", parent RIG-1681) — updated
with the thread+sender specifics (Compass-authored Setup thread in the root
manager's home channel; `@compass` reserved-alias system sender; no
`initial_prompt`). This record references RIG-1820 as the case-1 follow-up; it
files no new issue.

Exact `docs/designs/product/DECISIONS.md` delta (append under a new
`## First-turn delivery` section; DL-185 already landed on `main` (the RIG-1932
add-surface drop), so the highest row is now DL-185 and these are DL-186..189):

```markdown
## First-turn delivery

| ID | Decision | Status | Record |
| --- | --- | --- | --- |
| DL-186 | `initial_prompt` is REMOVED from the whole contract (`StartAgentSessionRequest` field 2, `SpawnAgentRequest` field 2, `SpawnPeerRequest` field 3 — numbers AND names reserved; server/runner/SDK/UI/e2e consumers cut over atomically, no fallback): an agent session ALWAYS starts idle and its first turn arrives as a channel message over the RIG-1569 deliver path (`DeliverControl` → the idle-deliver arm starts a turn); a provisioned peer's brief is a post from its provisioning manager into their per-pair DM channel (home channel for human owners) | Active (Matt, 2026-08-10) | [first-turn delivery §Approach](compass-first-turn-delivery/design.md#approach) |
| DL-187 | The `@compass` reserved alias is FROZEN as the system-sender mechanism for ANY system-level message sender (not just the root-manager Setup thread), requiring reserved-handle validation at account creation; case-1 root-manager boot (a Compass-authored initial Setup thread in the manager's home channel) uses it and is scoped OUT to follow-up RIG-1820, which owes only the sender representation + Setup flow; ratified in shape here so the interim can never revive a prompt field | Active (Matt, 2026-08-10) | [first-turn delivery §Case 1](compass-first-turn-delivery/design.md#case-1--root-manager-boot-scoped-out-to-a-follow-up-record) |
| DL-188 | Fresh-start barrier-lift: on a FRESH (non-resume) start the Runner sends `AgentControl{replay_complete}` as the first control op after Bind (seq 1, FIFO-first, drains before any deliver) — one mechanism symmetric with the resume path, no agent change; lifts the agent-side replay barrier so the first case-2 deliver is not refused-and-stranded (T-R3) | Active (Matt, 2026-08-10) | [first-turn delivery §the seam](compass-first-turn-delivery/design.md#the-seam-the-first-turn-rides-case-2--the-server--agent-halves-exist-the-runner-middle-leg-is-unbuilt) |
| DL-189 | The case-2 brief carrier is a PER-PAIR manager↔peer DM channel (`ChannelKindDM`, 2 members: spawning manager + new peer, both owners carried by `expandOwnerMembership` so the operator retains visibility), auto-provisioned on the spawn edge (T-R0) — NOT the manager's coordination channel, which is a broadcast to all reports. Token-minimization: siblings do not receive briefs they don't need. UI channel-proliferation UX is a known deferred problem | Active (Matt, 2026-08-10) | [first-turn delivery §Approach](compass-first-turn-delivery/design.md#approach) |
```

Note for the driver: DL-186's "first turn arrives over the RIG-1569 deliver
path" is a DECISION, but the path's Runner middle leg (and the T-R0 DM
auto-provision) is unbuilt today — PR-A (T-R0/T-R1/T-R2/T-R3) builds it. Do NOT
land DL-186 as "verified end-to-end": the Runner leg is PR-A, not yet exercised.

**Status flips: one (Matt-ruled).** No existing DL row rules on
`initial_prompt`, start-idle semantics, or first-turn carriage (verified:
`DECISIONS.md` has no occurrence of "initial" or "prompt" in any Decision cell
bearing on the prompt field; DL-166/DL-164 rule the composite-spawn and
start-affordance shapes without it). **DL-185 (RIG-1932, Active) explicitly
KEEPS `StartAgentDialog`** as the human's only start affordance — so T5's
**deletion** of that dialog (OQ-1, Matt-ruled delete at this design-PR gate)
amends DL-185's "Kept" clause. That amendment is NOT applied in this record: T5
lands it in PR-B, ordered after #267/RIG-1932 merges so the "Kept" clause exists
to be amended. The driver records the flip on DL-185 in the design-ledger-gate
PR at that point, not here.

The superseded content lives in frozen RECORDS, not
ledger rows. All six records carrying now-superseded `initial_prompt` content
are superseded-in-part by DL-186 (the citable overturn) — three already noted
plus three the completeness pass adds, so a reader of ANY of the six finds the
supersession:

- `compass-agent-spawn-despawn/design.md:836-841` ("spawn ships
  `initial_prompt` only…");
- `compass-spawn-control/design.md:538-541,692-696` (prompt-conditional dot +
  prompt textarea);
- `compass-dogfood-e2e/design.md:220` (`StartSession(container, prompt,
  resumeID)`);
- `compass-dogfood-loop/design.md:266,472-473,490-491` (the
  `StartAgentSession{initial_prompt}` scenario step);
- `compass-agent-session-persistence/design.md:676-677` (quotes `string
  initial_prompt = 2`);
- `compass-0.8-threading-and-session-renderer/design.md:594-595,777-778` (the
  request modeled as `container_name` + `initial_prompt`).

Per Constraint 5 those records are NOT edited here — only named, so DL-186 is
the single overturn point.

## Tasks

PRs are staged in two (see §"PR staging" below): **PR-A** = the additive Runner
deliver-lane + spawn-edge DM auto-provision build (T-R0/T-R1/T-R2/T-R3), lands
FIRST, greens nothing yet; **PR-B** = the atomic `initial_prompt` removal +
harness re-model (T1–T7), lands SECOND, greens leg-2.

PR-A (Runner deliver-lane + DM auto-provision — additive, independently landable):

- [ ] T-R0 — spawn-edge manager↔peer DM auto-provision: get-or-create a per-pair `ChannelKindDM` channel (`store/types.go:74-75`) for the `{manager, peer}` pair via `CreateChannel` + `expandOwnerMembership` (`store/channels.go:72-140`), modeled on the `CoordinationHook` reconcile (`store/coordination.go:10-23`, fired in the CreateAgent tx) but pairwise + idempotent, so the manager can post the brief point-to-point (implement-hard, Go store + comms)
- [ ] T-R1 — runner dispatch: add a `SessionsResponse_DeliverControl` arm in `dispatch.go` (`:359` switch) routing the wrapped op to the container's control producer, so a server-pushed deliver is no longer met by the `:449` "unrecognized session command variant" default (implement-hard, Go runner)
- [ ] T-R2 — gateway control: fix `representable()` (`gateway/control.go:192-203`) to admit payload-carrying `Deliver`/`Steer` (they carry a comms `Message`), so the send is no longer dropped as an empty shell (implement, Go runner)
- [ ] T-R3 — fresh-start barrier-lift: on a fresh (non-resume) start the Runner sends `AgentControl{replay_complete}` as the first control op after Bind (OQ-A ruling), so the first deliver is not refused-and-stranded by the agent barrier (`control-source.ts:278,373-390`) (implement-hard, Go runner + TS agent)

PR-B (atomic removal + harness re-model — greens leg-2, lands after PR-A):

- [ ] T1 — proto removal + `reserved` + regen of all four gen trees (implement)
- [ ] T2 — server: drop `InitialPrompt` at `lifecycle.go:325` and `spawn.go:135` + pgtest fixtures (implement)
- [ ] T3 — runner: compile against regenerated types; gateway fixture cleanup (no production change beyond PR-A's deliver-lane, which T-R1/T-R2 already landed) (implement)
- [ ] T4 — agent SDK: spawn tool loses `initial_prompt?`; tests updated (drop the JSON key as hygiene; NO unknown-key reject assert — see arktype note) (implement)
- [ ] T5 — UI: promptless `SpawnSpec`/binding, `running`→`idle` dot, delete `StartAgentDialog` + test (OQ-1 Matt-ruled delete; board start affordance calls the spawn action directly; amends DL-185's "Kept" clause after #267/RIG-1932 merges) (implement)
- [ ] T6 — e2e harness re-model: promptless Start/Resume, home-channel `PostMessage` first turn, split into `OpenSessionTail`(before post) + `AwaitTurnSettled`(WORKING→READY), leg-2/leg-3-4 scenario updates (implement-hard; unblocks RIG-1792 H8 / PR #256)
- [ ] T7 — ledger rows DL-186/DL-187/DL-188/DL-189 + `Status:` header; references case-1 follow-up RIG-1820 (no new issue filed) (driver)

## Open Questions

All five design-fork OQs are resolved. The four load-bearing ones (case-1
scope, deliver-lane carrier, barrier-lift mechanism, system-sender freeze) are
Matt-ruled and folded above (DL-186..189 + RIG-1820). OQ-1 (`StartAgentDialog`
disposition) is Matt-ruled at this design-PR gate:

1. **`StartAgentDialog` disposition — RULED: delete (Matt, design-PR gate).**
   Removing `initial_prompt` empties this dialog (its only input was the prompt
   textarea). DL-185 (RIG-1932, Active) explicitly KEEPS it, so this was a fork,
   not a driver call. Matt ruled **delete**: the board start affordance invokes
   the spawn action directly with `{agentAccountId, workstreamId}` (spawn is
   already guarded by the DL-164/168 live-session predicate and idempotent under
   its request id). T5 lands the deletion in PR-B, ordered after #267/RIG-1932
   merges; it amends DL-185's "Kept" clause (the one status-flip above).
   (Ground check at ruling time: `StartAgentDialog.tsx` + `.test.tsx` still
   present on `main@origin`; no open PR removes them — the deletion is this
   design's, landed by T5.)
2. **Settle-primitive naming → `AwaitTurnSettled` (driver-resolved).**
   The new name (WORKING→READY, one settled turn) is the accurate one T6 uses;
   the old `AwaitSessionSettled` named a session-ready wait the re-model no
   longer performs. Pure rename, settled in-record.
