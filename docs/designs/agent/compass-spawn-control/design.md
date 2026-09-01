# Compass spawn control — start/stop a workstream agent from the Bridge board

Status: Active

This record freezes on merge; later changes supersede by citation, never
rewrite.

Ported from the internal monorepo's prior record with
Matt-ruled amendments, re-grounded against the current compass tree
(`main@origin`, 2026-08-07). The internal monorepo's prior decisions are
re-expressed here as DL-164..DL-171.

> **Amendment (DL-185, Matt 2026-08-07):** the manual add-a-workstream /
> create-agent surface is dropped from the Compass UI — `NewWorkstreamDialog`,
> `store.addWorkstream`, the `WorkstreamSpec` type, and the "＋ New workstream"
> toolbar button. Cards come from the tracker and the Manager builds the tree
> (issues are forge-ingested per DL-069/DL-161; the Manager builds the rest per
> DL-095/DL-134; positive replacement RIG-1820). The sections below are
> reconciled to the surviving start/stop surface — `StartAgentDialog`, the
> spawn/stop phase machine, and stop control stay.

## Problem / Intent

The Bridge board can observe and stop, but not start: the server's
agent-lifecycle RPCs (`ProvisionAgentWorkspace`, `StartAgentSession`,
`StopAgentSession`, `ReloadAgentSession` —
`proto/compass/v1/compass.proto:54-79`) exist and are implemented, and the UI
ships a **real, wired stop** — `stopAgent` (`apps/ui/src/store.ts:1816-1849`)
awaits `client.stopAgentSession({ sessionId })` on the live `CompassClient`,
refusing fixture-sourced sessions and offline stores with a surfaced reason,
triggered by the LogPanel "■ stop" button (`components/LogPanel.tsx:73-98`).
But there is **no start/provision trigger anywhere in `apps/ui/src`**: nothing
in the UI calls `ProvisionAgentWorkspace` or `StartAgentSession`. This record
designs the human-facing
control that starts a workstream agent (provision + start, with an optional
initial prompt) from the board, and hardens the existing stop into the same
control model.

*(Port note — superseding the internal monorepo's premise: the source record
described stop as a no-op stub, `const stopAgent = () => {};`. That was true of
the internal monorepo's snapshot; compass has since shipped the real
`stopAgent`, so every stop-side task below hardens or extends shipped behavior
rather than replacing a stub.)*

## Spec impact

Spec-impact: `docs/specs/product/compass.md` — reconciliations land in the
**T0** proto+server PR. Two are additions to the compass.v1 contract, both
Matt-authorized on the prior record (DL-166, DL-167): a new `SpawnAgent` RPC
(+ `SpawnAgentRequest`/`SpawnAgentResponse` messages) on `CompassService`, and
an `agent_account_id` field on `AgentSessionStatus`. The third is a **new
SHALL binding one container per agent account** (DL-170, Matt ruled
2026-07-26), which makes the reject-on-live rule agent-scoped *by contract*
rather than by an incidental Runner naming property; §DL-170 carries the full
argument. The spec doc is reconciled in T0 (where the wire change actually
lands), per the "spec updated as the last step of implementation" convention —
not this design-only PR, which has no code. The reject-on-live rule **agrees
with** the container-scoped SHALL at `compass.md:368-370`.

*(Port note: the prior record carried a fourth reconciliation — relaxing
`ProvisionAgentWorkspace`'s repo requirement for self-clone. That already
shipped in compass under RIG-1527: `ProvisionAgentWorkspaceRequest` no longer
carries a repo (`compass.proto:506-509` — "Repo carriage removed … the agent
self-clones"), so it drops from T0's scope entirely, along with the prior
record's clone-target-validation and agent-credential prerequisites, which
were consequences of the now-shipped change.)*

## Approach

### Posture: live-wired from the start (amends the walking-skeleton posture — DL-165)

The internal monorepo's prior record froze a walking-skeleton posture (its
OQ-A) on three facts of that tree: `createAppStore` took no arguments,
`SubscribeEvents` had zero UI consumers, and no live client seam existed.
**All three are false in compass today**: `createAppStore(options:
AppStoreOptions)` already takes an options bag carrying `comms` and `compass`
clients (`store.ts:551-589`, `:667`), the store already runs the live comms
stream and the SubscribeEvents-backed board read when clients are present
(`store.ts:897-912`, `:922-932`), and the shipped `stopAgent` already dials
`stopAgentSession` on that seam (`store.ts:1835-1848`). The rationale for
fixture-first is gone, so this port amends the posture: **the control lane
builds against the live seam directly** — store actions await the real RPCs
when `options.compass` is present and degrade to the documented offline
refusal (the shipped `stopAgent` precedent: refuse with a surfaced reason,
never a silent no-op) when it is not. The UI tasks still gate on T0 (the
`SpawnAgent` RPC must exist to be called), but there is no separate stacked
"wiring lane": the prior record's T6 folds into T2/T4. Recorded as DL-165,
superseding the walking-skeleton ruling whose premises no longer hold.

### The wire contract (two changes, Matt-authorized on the prior record; re-grounded here)

Baseline RPCs from `proto/compass/v1/compass.proto`, verified this run:

**Baseline (unchanged):**

- `rpc ProvisionAgentWorkspace(...)` (`compass.proto:54`):
  `agent_account_id` + `client_request_id` → `container_name`. Repo-less
  since RIG-1527 — the container is provisioned with a git credential +
  workspace and the agent self-clones (`compass.proto:506-509`). *(An
  internal server step of `SpawnAgent`, not a client call — see Control
  flow.)*
- `rpc StartAgentSession(...)` (`compass.proto:59`): `container_name` +
  `initial_prompt` → `session_id`. *(Likewise internal to `SpawnAgent`.)*
- `rpc StopAgentSession(...)` (`compass.proto:64`): "Idempotent — stopping an
  unknown/already-stopped session succeeds" (`compass.proto:61-63`; served at
  `go/server/service.go:252-256`). **Already consumed by the shipped
  `stopAgent`** (`store.ts:1845`).
- `rpc ReloadAgentSession(...)` (`compass.proto:79`): restart-in-place.
- `rpc GetAgentStatus(...)` (`compass.proto:82`): snapshot; "Empty = every
  live session" (`compass.proto:592-593`); served from the Bridge board
  projection (`go/internal/board/projection.go:114-122`), not a Runner relay.
- `rpc SubscribeEvents(...)` (`compass.proto:33`): the sole push path from
  the server to the UI; `AgentSessionStatus` (`compass.proto:324-327`) pushed
  on every transition. **Already consumed by the store's live board read**
  (`store.ts:926-932`, `runEventStream`).

**Change 1 (DL-166) — new `rpc SpawnAgent` on CompassService** (after
`StartAgentSession`). Two new messages; the request carries the agent
account, an initial prompt, and the idempotency key — no `repo`/`ref`
(self-clone is already the shipped provision contract):

```proto
rpc SpawnAgent(SpawnAgentRequest) returns (SpawnAgentResponse);

message SpawnAgentRequest {
  string agent_account_id = 1;
  string initial_prompt = 2;    // empty = start idle
  string client_request_id = 3; // end-to-end idempotency key
}
message SpawnAgentResponse {
  string session_id = 1;
  string container_name = 2;    // intermediate handle, surfaced for lifecycle parity
}
```

The response keeps `container_name` alongside `session_id` for
lifecycle/debug parity with the two-call path; the UI consumes only
`session_id`.

**New behavior the composite must add (contract-load-bearing, T0
acceptance):** a `SpawnAgent` retried with the same `client_request_id` MUST
return the same `session_id` and provision no second container. The two
individual RPCs don't compose idempotency for free — Start mints a fresh id
(`orNewRequestID`, `go/internal/runnerhub/commands.go:219-224`, used at
`:70`) — so T0 threads the one id through `provisionDedupID`
(`commands.go:229-244`, used at `:50`) AND makes the orchestrated Start
idempotent on an already-live container. **Build-order:** this end-to-end
idempotency composes three primitives already built. `provisionDedupID`
binds a non-empty `client_request_id` to the agent account through a
domain-separated hash, so a retry of the same provision dedups (since
RIG-1527 removed repo carriage, the key derives from account +
`client_request_id` alone — correct: `client_request_id` is the explicit
idempotency key, though two concurrent repo-less spawns for one account are
distinguished only by that id). The command router joins a retry whose id is
already in flight to the existing call (`runnerhub/router.go:117-132`;
`waitCall` deliberately leaves a timed-out call joinable, `:222-228`). And
the Runner's dispatcher returns the recorded result for an id it has already
handled (`runner/dispatch.go:223-236`, the `handled` map at `:79-88`). What
is missing is the **composite span**: `orNewRequestID` mints a fresh id per
relayed command, so Provision and Start dedup independently today.
`SpawnAgent` threads the one id across both steps; T0 lands as a small
standalone compass PR — the freeze does not wait on the build.

**Change 2 (DL-167) — `agent_account_id` on `AgentSessionStatus`.** Append
`string agent_account_id = 3;` to `AgentSessionStatus`
(`compass.proto:324-327`, today `session_id` + `state` only) and populate it
at every construction site — the `GetAgentStatus` projection, the
SubscribeEvents push, and the Runner-side `Status` answer — so a status
reattaches to an agent for **any** session whose account the server can
still resolve: post-refresh, other-client, another agent's.
Backward-compatible append (buf: non-breaking). **This is a join, not new
tracking** — the session→account binding already lives in the Server's hub
state: `containerAccounts` (the Provision..Start window), `sessionAccounts`
(the live-session map), and the reverse `accountSessions` map
(`go/internal/runnerhub/hub.go:287-305`), promoted Provision→Start at
`relay_comms.go:53-65`. It is **in-memory and live-scoped** — Start adds an
entry; Stop removes it (`relay_comms.go:112-122`) and a Runner re-enroll
clears all three maps (`hub.go:707-711`). That scoping bounds the claim: the
reconcile is full-fleet **across clients while the server stays up and the
binding stands**; a status emitted after a Runner reconnect has cleared the
maps carries no account and reconciles nothing — the UI holds its optimistic
phase in that case. The terminal STOPPED status is the case this bites
hardest, since `Hub.Stop` unbinds as soon as the Runner answers
(`commands.go:91-104`, `unbindSession` at `:102`), so T0 resolves the
account **before** the binding drops — capture it in `Hub.Stop` prior to
`unbindSession`, or keep the binding alive until the terminal status has
published. (Compass already fires a terminal presence edge at exactly this
seam — `unbindSession`'s DISCONNECTED presence publish,
`relay_comms.go:95-124` — so T0 has an in-tree precedent for
resolve-before-unbind.) T0 carries the account into the status
`GetAgentStatus` emits (`board/projection.go:143-145`, `statusOf` — the sole
Server-side builder, reached from `Snapshot` at `:114`/`:122`) and into the
sole push-path synthesis site, `Hub.deliverSession` (`hub.go:533-546`),
which today builds
`&compassv1.AgentSessionStatus{SessionId: sessionID, State: state}`
(`:543-546`) from a frame carrying only a session id.

A server built with no Runner door answers `Unavailable` on every lifecycle
RPC (`go/server/service.go:51-53`, checked per-handler — e.g. `:131-132`,
`:188-189`, `:252-253`), so the error surface renders a transport-level
failure, not only per-session errors — the shipped `stopAgent` already routes
exactly this refusal (`store.ts:1808-1815`).

### Control flow: start an agent (`SpawnAgent`)

Starting an agent is the lifecycle operation, one RPC — the server
orchestrates Provision → Start (DL-166). Cards come from the tracker (issues
are forge-ingested, not minted in the UI — DL-185); the human's only
lifecycle affordance is start/stop on an existing card:

1. Start = mint one `client_request_id` (UUID); call
   `SpawnAgent(agent_account_id, initial_prompt, client_request_id)` →
   `session_id`. The server runs `ProvisionAgentWorkspace` then
   `StartAgentSession` internally (the existing RunnerHub Provision+Start
   path, `runnerhub/commands.go`) and owns dedup across both steps under the
   one id. **`SpawnAgent` rejects when that agent already holds a live
   session**, and the rejection must be a **pre-Provision short-circuit**.
   The check runs **after** the `client_request_id` dedup-join lookup and
   only on a cache miss — a retry of an in-flight or completed spawn joins
   the original and returns its `session_id`, so reject-on-live never fires
   for it (without this ordering the short-circuit would swallow the retry
   case and return `AlreadyExists`, breaking the T0 idempotency criterion).
   On a miss: consult **the Runner**, which is authoritative for
   live-session truth, via `Hub.Status` (`commands.go:141-151`) with an
   empty session id — "Empty = every live session" (`compass.proto:592-593`),
   answered by the Runner's all-sessions arm (`runner/host.go:487-501`) —
   and scan the returned statuses for one whose `agent_account_id` matches
   the request's, returning `connect.CodeAlreadyExists` on a hit. The
   **mechanism is a full-fleet scan** because the request shape admits
   nothing else: `GetAgentStatusRequest` carries only
   `string session_id = 1` (`compass.proto:591-594`). This makes **Change 2
   a prerequisite of the reject rule**, not merely of the UI: today's
   `AgentSessionStatus` carries only `session_id` + `state`, so the scan has
   no field to match on. The account must reach the *Runner-side*
   construction too (`host.go:495`, `:499`) — the hub binding is
   `package runnerhub`, unreachable from the Runner — and the Runner keeps
   no account of its own today (`liveSession` holds
   sessionID/containerName/containerID/… with no account field,
   `runner/host.go:84-88`), so Change 2's T0 brief carries it onto
   Runner-local state (`AgentSpec` → the registered handle → `liveSession`)
   rather than parsing it out of the container name (`NamePrefix` is
   Runner-owned config, `runner/spec.go:31-33`; the name derivation
   `name := d.NamePrefix + accountID` at `spec.go:85` is exactly the
   incidental property DL-170 exists to stop leaning on).

   The check **must not** be sourced from Server-side in-memory state.
   Compass now carries the reverse index the internal monorepo lacked —
   `Hub.SessionForAccount` (`relay_comms.go:157-171`, over
   `accountSessions`) — but it is still **live-scoped and cleared on every
   Runner re-enroll** (`hub.go:707-711`, pinned by
   `TestEnrollClearsReverseAccountSessions`,
   `relay_comms_test.go:430-458`). That clear is a deliberate fail-**closed**
   for the comms relay, but the same absence read as "nothing is live" makes
   a reject-on-live check fail **open**: after a reconnect the containers
   still run, the check passes, and the spawn collides on the container name
   during Provision — `Launch` is create-first, so the collision surfaces as
   an internal error, not `AlreadyExists`. The existing `errAlreadyRunning`
   guard (`runner/host.go:224-238`) cannot carry the rule either: it sits
   inside `StartAgentSession`, and a composite Provision→Start against a
   live agent never reaches it.
2. Stop = `StopAgentSession(session_id)` — **already shipped**
   (`store.ts:1816-1849`): stops the observed session, refuses fixture
   sessions and offline stores with a surfaced reason routed to the error
   surface, idempotent server-side so a double-click or a stop racing a
   server-side death is safe. This record's stop-side work is confinement
   into the same control model (two-step confirm + binding-aware enablement,
   T4), not a new action.
3. Retry a failed start = re-send `SpawnAgent` with the **same**
   `client_request_id` — the server's composite dedup decides
   join-vs-reattempt internally, so the client never distinguishes
   provision-failed from start-failed and juggles no retry id.

### Board state model: optimistic spawn, then server-confirmed lifecycle (DL-167)

Two pieces of state with an explicit precedence rule:

1. **`SessionBinding.phase` (`SpawnPhase`)** — the *spawn-window* optimistic
   state: `spawning` on click, `running` once `SpawnAgent` resolves with a
   `session_id`, plus `spawn-failed`, `stopping`, `stop-failed`, `stopped`.
   Spawn is one composite call, so the phase machine has no
   provisioning-vs-starting split — those are live states the server
   streams, not client await points.
2. **The live `AgentSessionState`**, rendered through `agentDotState`
   (`apps/ui/src/agent-state.ts:53-56` — the compass signature takes
   `(sessionState, refinement)`; the binding side feeds only the session
   state). STARTING / READY / WORKING / ERRORED / DISCONNECTED live here,
   pushed on SubscribeEvents.

**Precedence:** the binding phase is the card's state until a live
`AgentSessionStatus` **attributed to that agent** arrives (Change 2); from
then on the live state wins and the binding phase holds at `running`. A
precedence *switch*, not an overwrite — `SpawnPhase` does not grow the
live-session variants. A card with no binding (never started) shows no pill.
Because the live event stream is already wired in compass
(`store.ts:926-932`), the switch is **attribution-gated, not lane-gated**
(an amendment to the prior record's "T6-gated" phrasing): it happens the moment the
first attributed status for that agent lands, which requires T0's Change 2
on the wire — until T0 ships, statuses carry no account and the binding dot
holds.

The store gains one new piece of state — a `SessionBinding` map keyed by
`workstreamId` (per DL-164: the board is workstream-keyed and an agent owns
multiple cards — Matt confirmed agents need multiple workstreams — so an
agent-keyed map would bleed one card's pill onto siblings). The binding
carries its `agentAccountId` for the RPC. Bindings are store-internal
wire-lifecycle bookkeeping, not a fixture-shape change: the compass board's
`Issue` (`stub-data.ts:202-240`) and `Agent` shapes stay frozen.
*(Terminology port note: the internal monorepo's UI had a `Workstream` type; the compass
board renders `Issue` cards (`board.ts:39-56`, `components/IssueCard.tsx`)
and has no `Workstream` symbol. This record keeps "workstream" for the
concept — an issue promoted to a unit of work — and `workstreamId` binds to
the compass `Issue.id` (`stub-data.ts:203-205`).)*

### Surfaces

- **Start** is its own affordance on the card (T4) for a card whose agent is
  not live: it opens a **StartAgentDialog** whose only field is the optional
  initial prompt, and submits the store's `startAgent`. It is absent when
  the agent already holds a live session, because `SpawnAgent` would reject
  (`CodeAlreadyExists`).
- **Stop** stays where it is: the LogPanel "■ stop" button
  (`LogPanel.tsx:73-98`), already wired to the real `store.stopAgent()`
  (`:85`) and already disabled for fixture sessions (`:79`), gains a
  two-step inline confirm (DL-168: the wire has a single "deliberately kill"
  semantic — `compass.proto:61-63` — so "graceful" is a UI confirm, not a
  wire flag) and binding-aware enablement (T4). `ReloadAgentSession` is
  surfaced later as a distinct "restart" action, out of this record's build
  scope.
- A failed start surfaces on the board card (state pill + retry affordance)
  and, for transport-level failures (`Unavailable`), as a dismissible banner
  hosted by `Bridge` — board-level, so no card owns it. Retry re-sends
  `SpawnAgent` (the server owns join-vs-reattempt). The compass card renders
  as a single `<button>` (`IssueCard.tsx`), and interactive content (a Retry
  button) cannot nest inside a `<button>`, so the card becomes a
  `<div role="button">` carrying the existing select/open handlers, with the
  pill and Retry as sibling interactive elements (`stopPropagation` on
  Retry). A real T4 sub-task, not a free rider.

### Distinct from `compass-agent-spawn-despawn` (deliberately)

Compass already carries a spawn design:
[`compass-agent-spawn-despawn/design.md`](../compass-agent-spawn-despawn/design.md)
— the **agent-facing** path, where a supervisor agent spawns and despawns
peer agents at runtime **through a Compass tool** (the agent→Server lifecycle
relay + the agent-side tool pair, owner-scoped per its F2 ruling). This
record is the **human-facing** path: a person clicks Start/Stop on the Bridge
board over the network door. The two are orthogonal, not duplicates —
different callers (agent tool vs Bridge UI), different doors (the agent relay
vs the admin-gated network door, DL-171), different composition (that record
relays the existing Provision/Start/Stop/Remove RPCs; this one adds the
composite `SpawnAgent` and the status attribution both paths can later
consume). Where they meet — the one-container-per-agent SHALL (DL-170) and
the reject-on-live rule — this record's contract additions bind both paths
equally.

### Alternatives considered

- **Walking-skeleton-first** (the internal monorepo's prior ruling, Matt-ruled there): build the
  control UX as fixture mutations, wire later in a stacked lane. Superseded
  by the shipped compass store: the injection seam and live streams the
  skeleton waited on already exist (`store.ts:551`, `:667`, `:897-932`), and
  `stopAgent` is already live — a fixture-first lane would now *regress* the
  stop side. Recorded as DL-165.
- **Client-orchestrated Provision → Start** (the prior draft's original
  control flow): the client makes two calls and juggles a split retry-id.
  Rejected (Matt ruled the composite `SpawnAgent`, DL-166): it pushes wire
  dedup semantics into the UI; the composite keeps them server-side where
  the dedup state already lives.
- **Accept self-spawned-only reconcile scope**: no wire change, but a
  session started by another client or surviving a refresh can't reconcile.
  Rejected (Matt ruled the wire attribution, DL-167).
- **Server-side reject-on-live check over `SessionForAccount`**: compass now
  has the reverse map the internal monorepo lacked (`relay_comms.go:157-171`), but it fails
  open after a Runner re-enroll (`hub.go:707-711`); the Runner scan is
  authoritative. Rejected — the Control-flow section carries the argument.
- **Browser `confirm()` for stop**: rejected — blocks the event loop,
  untestable under `bun test`; the two-step inline confirm is
  component-local state.

## Global Constraints

Every task below inherits these; task briefs do not restate them.

- **Citation scope**: a bare `file.ext:NNN` citation in this record resolves
  inside the compass repo root — `store.ts` is `apps/ui/src/store.ts`,
  `host.go` is `go/internal/runner/host.go`, `hub.go` / `commands.go` /
  `relay_comms.go` / `router.go` are under `go/internal/runnerhub/`,
  `dispatch.go` and `spec.go` are under `go/internal/runner/`, `service.go`
  is `go/server/service.go`, `projection.go` is
  `go/internal/board/projection.go`. A bare `compass.md` is the spec,
  `docs/specs/product/compass.md`. Names with no candidate (`spawn.ts`,
  `spawn.test.ts`, `StartAgentDialog.tsx`) are
  files this record proposes. All line-cited coordinates were verified
  against `main@origin` in the porting run (2026-08-07); citations are
  additionally anchored on symbol names, stable across drift.
- **Stack: SolidJS + Vite**, UI at `apps/ui/src/`. No new framework or state
  library; all cross-component state lives in the one `AppStore`
  (`store.ts`), read through context (`context.ts`).
- **Wire contract**: reuse `compass.v1` as generated, with **two changes
  Matt authorized** (DL-166 `SpawnAgent` composite RPC; DL-167
  `agent_account_id` on `AgentSessionStatus`) — landed by the compass
  service-owner's lane (T0). Any *further* proto need stops and escalates (a
  Matt `ask`).
- **Live seam, offline honest**: store actions await the real client when
  `options.compass` / `options.comms` is present and refuse with a surfaced
  reason when absent — the shipped `stopAgent` shape (`store.ts:1816-1849`),
  never a silent no-op. (Amends the walking-skeleton constraint;
  DL-165.)
- **Fixture shapes stay frozen**: no new fields on `Issue` / `Agent` in
  `stub-data.ts`; wire-lifecycle bookkeeping lives in the store's
  `SessionBinding` map.
- **Tests: `moon run compass-ui:test`** = `bun test --conditions browser`
  (the browser condition is load-bearing). Red→green per
  `rule://red-green-testing`: tests first, watch them fail, then implement.
  Run only the suites a task adds/touches.
- **Lint/format**: biome (TS); this record markdownlint-clean.
- **Frozen-record convention**: freezes on merge; later changes supersede by
  citation.

## Plan

Tasks below. **T0** is the proto+server change (compass service-owner's
lane); T1–T4 are the UI build, live-wired per DL-165 and stacked behind T0;
T5 is the docs/cleanup sweep. (The prior record's separate T6 wiring lane is dissolved
into T2/T4 — see the Posture section.)

### T1 — Session-binding state + spawn/stop domain types

The store-internal model that holds wire handles and the spawn lifecycle.
A new module `apps/ui/src/spawn.ts` (pure types + pure reducers, no Solid
imports) plus store signals. The composite `SpawnAgent` RPC (DL-166) means
the client sees spawn as *one* call, so the phase machine has no
provisioning-vs-starting split and no client-side retry-id state.

```ts
/** One started workstream's wire-lifecycle bookkeeping. Store-internal —
 *  never a fixture shape (stub-data.ts Issue/Agent stay frozen). Keyed by
 *  workstreamId (= Issue.id) in the store map (per DL-164: the board is
 *  workstream-keyed and an agent owns multiple cards, so an agent key would
 *  bleed one start's pill onto the agent's other cards). A binding exists
 *  only for a card the agent was actually started on. */
export interface SessionBinding {
  /** The card this start targets — the store map key (Issue.id). */
  readonly workstreamId: string;
  /** The agent account the start is for — the SpawnAgent input, and the
   *  join key to a pushed AgentSessionStatus.agent_account_id (per DL-167
   *  attribution). */
  readonly agentAccountId: string;
  /** Set once SpawnAgent resolves; the cursor for Stop/Reload/status. An
   *  agent holds at most one live session (per DL-164/DL-170) and
   *  SpawnAgent rejects a second start, so at most one of an agent's cards
   *  ever carries a sessionId. */
  readonly sessionId?: string;
  /** The SpawnAgent idempotency key (one per spawn; the server owns the
   *  internal Provision+Start dedup, so a retry re-sends this verbatim). */
  readonly clientRequestId: string;
  readonly phase: SpawnPhase;
  /** Captured from `SpawnSpec.initialPrompt` at `beginSpawn`, so
   *  `bindingDotState` stays a pure function of the binding alone (an empty
   *  prompt = "start idle", which the optimistic `running` dot must
   *  reflect). */
  readonly initialPrompt: string;
  /** Human-readable failure, set only in the two failure phases. */
  readonly error?: string;
}

export type SpawnPhase =
  | "spawning"      // SpawnAgent in flight (server runs Provision→Start)
  | "running"       // sessionId live; live AgentSessionState now wins the dot
  | "spawn-failed"  // SpawnAgent errored; retry re-sends the same request id
  | "stopping"      // Stop in flight
  | "stop-failed"   // Stop returned an error; session still held
  | "stopped";

export interface SpawnSpec {
  /** The target agent account — the card's assignee. */
  readonly agentAccountId: string;
  /** Empty = start idle. */
  readonly initialPrompt: string;
  /** The existing card to start the agent on — cards come from the tracker,
   *  not minted in the UI (DL-185). */
  readonly workstreamId: string;
}
```

Pure reducers (unit-testable without Solid):

- `beginSpawn(spec, requestId) → SessionBinding` — mints `spawning`,
  capturing `spec.initialPrompt` onto the binding.
- `applySpawned(b, sessionId)` — `spawning` → `running` (the composite RPC
  resolved; one transition, not Provision-then-Start).
- `beginStop(b)` — `running` **|** `stop-failed` → `stopping`. The second
  arm exists because on `stop-failed` **the session is still held** — a
  retry is a fresh stop attempt on a live session, not a new spawn (per
  DL-169's recovery routing). Every stop attempt, first or retried, is
  uniformly a `stopping` window; a retry lands at `stopped` or back at
  `stop-failed`.
- `applySpawnError(b, at, error)` — `at: "spawn"` → `spawn-failed`,
  `"stopping"` → `stop-failed`. Both failure sites map to a real phase.
- `applyStopped(b)` — from `running`/`stopping`/`stop-failed` → `stopped`.
- `applySessionStatus(b, state: AgentSessionState) → SessionBinding` — the
  reconcile reducer for attributed statuses. Only meaningful once `running`;
  it does **not** widen `SpawnPhase` — the live state lands on the agent's
  lifecycle and `agentDotState` renders it (Board state model precedence).
- `bindingDotState(b): AgentState` — the pre-reconcile dot, total over
  **every** phase including `running`; the board keeps reading it until an
  attributed live status arrives for that agent (the switch is
  attribution-gated). `spawning` → `working`; `spawn-failed`/`stop-failed` →
  `error`; `stopping` → `working`; `stopped` → `stopped`; `running` →
  `working` **only if** `b.initialPrompt` was non-empty, else `idle` (an
  empty-prompt spawn is "start idle", so a working dot is knowably wrong at
  mint time).

Interfaces:

```ts
// spawn.ts (new)
export interface SessionBinding { /* as above */ }
export type SpawnPhase = /* as above */;
export interface SpawnSpec { /* as above */ }
export function beginSpawn(spec: SpawnSpec, requestId: string): SessionBinding;
export function applySpawned(b: SessionBinding, sessionId: string): SessionBinding;
// Domain: running | stop-failed → stopping (a stop retry re-enters stopping
// from the still-held session).
export function beginStop(b: SessionBinding): SessionBinding;
export function applySpawnError(b: SessionBinding, at: "spawn" | "stopping", error: string): SessionBinding;
export function applyStopped(b: SessionBinding): SessionBinding;
export function applySessionStatus(b: SessionBinding, state: AgentSessionState): SessionBinding;
export function bindingDotState(b: SessionBinding): AgentState;
```

Test cycle (red→green): new `spawn.test.ts` asserting the phase machine —
`beginSpawn` mints `spawning`; `applySpawned` requires `spawning` and moves
to `running`; `beginStop` accepts `running` **and** `stop-failed`, moves each
to `stopping`, and rejects every other phase; `applySpawnError` maps both
sites; `applyStopped` from `running`/`stopping`/`stop-failed` only;
`bindingDotState` total over every phase, with `running` → `idle` when the
captured `initialPrompt` is empty and `working` otherwise;
`applySessionStatus` leaves `SpawnPhase` at `running`. Watch fail (module
absent), then implement.

### T2 — Store actions: `startAgent`, stop-binding integration, `retrySpawn`

Extend `AppStore` and `createAppStore` (`store.ts:667`) with the control
actions, **live-wired** against `options.compass` / `options.comms` per
DL-165 — awaiting the real RPCs when clients are present, refusing with a
surfaced reason when absent (the shipped `stopAgent` shape).

Starting an agent is the lifecycle operation, against an existing
(forge-ingested) card — adding cards from the UI is dropped (DL-185). A start
or a stop happens independently on a card:

- `startAgent(spec)` — the **lifecycle** operation, against an existing
  card. Mint a `client_request_id`; create the `SessionBinding` keyed by
  `spec.workstreamId` (phase `spawning`); await
  `options.compass.spawnAgent(...)` (T0's generated method) and drive the T1
  reducers from the response — `applySpawned` on success, `applySpawnError`
  on failure. Map a `CodeAlreadyExists` rejection to `spawnAlert` kind
  `rejected` scoped to that card (no second binding minted) and an
  `Unavailable` to kind `transport`. **Client-side guard mirrors the server
  rule**: rejects when that agent already owns a binding in a started phase
  (`spawning`/`running`/`stopping`/`stop-failed`), setting the `rejected`
  alert without dialing — the same predicate the T4 affordance reads, so the
  control is never enabled-but-rejecting.
- **Stop-binding integration.** The shipped `stopAgent`
  (`store.ts:1816-1849`) keeps its contract — zero-arg on every real call,
  observed-session targeting, fixture/offline refusals — and gains binding
  awareness: when the selected agent's started binding exists, a stop drives
  `beginStop` → `applyStopped` / `applySpawnError(b, "stopping", …)` around
  the existing `stopAgentSession` await, resolving the binding the spawn
  minted. Target resolution is **agent-scoped**
  (`selectedAgentStartedBinding()`): the LogPanel is mounted in the agent
  workspace over the selected agent, and the selection can re-anchor to a
  card that never started, so a workstream-scoped target would mis-fire.
  An agent holds at most one live session (DL-164/DL-170) and only the card
  it was started on has a binding, so the started-phase card is unique —
  stopping ends that one session and resolves that one binding; no fan-out.
  On `stop-failed` a second stop re-enters `stopping` on the still-held
  session (DL-169's recovery arm). Sessions with no binding (pre-spawn live
  sessions) keep today's shipped path untouched.
- `retrySpawn(workstreamId)`: from `spawn-failed`, re-enter `spawning` and
  re-send `SpawnAgent` with the **same** `clientRequestId` — the server owns
  join-vs-reattempt (DL-166), so the client has no retry-id branch. A retry
  is a dedup join on the original start, not a second start, so
  reject-on-live does not fire for it.
- `sessionBinding(workstreamId)`: accessor the board card reads;
  `selectedAgentStartedBinding()`: the started-phase binding among the
  selected agent's cards — the LogPanel stop target (undefined when no agent
  is selected or none of its cards is started).

Interfaces:

```ts
// store.ts — AppStore additions
/** Start the agent on an existing card via the composite SpawnAgent RPC
 *  (per DL-166). Rejects when that agent already holds a live session
 *  (CodeAlreadyExists, per DL-164) — the rejection surfaces on spawnAlert
 *  as kind "rejected". Live-wired per DL-165: awaits options.compass;
 *  offline refuses with a surfaced reason. */
startAgent: (spec: SpawnSpec) => Promise<void>;
/** (Existing, shipped — store.ts:1816-1849.) Gains binding integration:
 *  resolves the selected agent's started binding and drives the T1
 *  reducers around the stopAgentSession await. Signature unchanged. */
stopAgent: () => Promise<void>;
/** Retry a failed start: re-send SpawnAgent with the same request id (the
 *  server owns join-vs-reattempt). */
retrySpawn: (workstreamId: string) => Promise<void>;
/** A card's wire-lifecycle binding, or undefined when never started. */
sessionBinding: (workstreamId: string) => SessionBinding | undefined;
/** The started-phase binding among the selected agent's cards (the
 *  LogPanel stop target), or undefined. */
selectedAgentStartedBinding: () => SessionBinding | undefined;
```

Test cycle: a new `store-spawn.test.ts`, driving the actions through a fake
`CompassClient` / `CommsClient` (method-stubbed objects injected through the
existing `AppStoreOptions` — the pattern `store.live.test.ts` already uses) —
`startAgent` calls `spawnAgent` once with the spec's fields and drives
the binding to `running`; `startAgent` against an agent that already holds a
started binding is rejected locally — no dial, no second binding, `spawnAlert`
kind `rejected`; a `CodeAlreadyExists` from the fake maps to the same alert;
an `Unavailable` maps to kind `transport`; a spawn error parks `spawn-failed`;
`stopAgent` drives the selected agent's started binding
running→stopping→stopped and leaves the agent's other cards alone; a failing
fake stop takes stopping→stop-failed, and a second `stopAgent` re-enters
`stopping` and reaches `stopped` (DL-169 recovery); offline (no clients)
refusals surface reasons and mint no binding; `retrySpawn` re-sends the same
`clientRequestId`. Red first against the missing actions.

### T3 — Start-agent dialog

One dialog — the start operation. Cards come from the tracker (DL-185), so
there is no add-a-card dialog and no Bridge toolbar entry.

**`components/StartAgentDialog.tsx`** — opened by the card's "▶ start"
affordance (T4), which fixes the agent and the workstream, so the only field
is the initial prompt (textarea; empty = start idle). Submit calls
`store.startAgent({ agentAccountId, initialPrompt, workstreamId })` and
closes. The affordance is absent when that agent already holds a live
session (DL-164/DL-166), and present on a `stopped` card, which `startAgent`
accepts — the same predicate on both sides (DL-168).

Dialog open/closed is component-local state in `Bridge.tsx` / the card
(matching the local-`createSignal` convention the compass components use),
not store state.

Interfaces:

```ts
// components/StartAgentDialog.tsx (new)
export const StartAgentDialog: Component<{
  spec: Omit<SpawnSpec, "initialPrompt">;
  onSubmit: (spec: SpawnSpec) => void;
  onCancel: () => void;
}>;
```

Test cycle: new `StartAgentDialog.test.tsx` — an empty prompt submits (start
idle) and the spec carries the fixed agent + workstream.

### T4 — Start affordance + stop confirm + status surfaces (LogPanel, board card)

- LogPanel: the "■ stop" button (`LogPanel.tsx:73-98`) gains a two-step
  inline confirm (first click arms — label "confirm stop", auto-disarms on
  blur/timeout; second click calls the existing `store.stopAgent()`).
  Enablement and the `stopAgent` binding-target guard read the **same**
  predicate, in the **agent** scope the panel is mounted in: enabled when
  `selectedAgentStartedBinding()` is in phase `running` **or** `stop-failed`,
  **or** (pre-spawn) the selected agent has a running non-fixture session —
  the states `stopAgent` actually acts on — so the control is never
  enabled-but-inert. The `stop-failed` arm makes DL-169's recovery
  reachable: the session is still held, so re-issuing the stop is a live
  stop attempt, and disabling Stop there would strand the card. The
  existing fixture-session disable (`LogPanel.tsx:79`) stays.
- Board: the card shows a spawn-status pill for its binding in **every**
  phase — spawning / running / spawn-failed / stopping / stop-failed /
  stopped — with a Retry affordance on **`spawn-failed` only** (DL-169),
  wired to `store.retrySpawn(id)`. On `stop-failed` the session is still
  held, so re-sending `SpawnAgent` is semantically wrong and reject-on-live
  would bounce it; a `stop-failed` card recovers via the LogPanel stop,
  which is enabled on exactly that card. The pill's state is
  `bindingDotState(binding)` until the first attributed live status for
  that agent arrives (Board state model precedence), then
  `agentDotState(sessionState)`. **This restructures the card**: today a
  single `<button>` (`IssueCard.tsx`), it becomes a
  `<div role="button" tabindex="0">` carrying the existing handlers plus
  keyboard activation, with pill and Retry as sibling interactive elements
  that `stopPropagation`. A real sub-task with its own tests (card still
  selects/opens as before; Retry does neither).
- Board: the card also carries a "▶ start" affordance, shown when the card
  has no binding **or** its binding is in the terminal `stopped` phase,
  **and** its agent holds no live session (no other card of that agent in a
  started phase) — the same predicate `startAgent` guards on, so it is
  never enabled-but-rejecting **and never rejecting-but-hidden** (DL-168:
  a stopped card stays restartable; `applyStopped` returns a binding rather
  than clearing it, and nothing in this record removes one, so a
  no-binding-only predicate would make a stopped card permanently
  unstartable). Restarting mints a fresh binding via `beginSpawn`. Opens
  the T3 `StartAgentDialog`; sibling interactive element, `stopPropagation`.
- Spawn failures surface through one store alert that **carries its
  class**: transport-level failure (`Unavailable` from a Runner-less
  server, `service.go:51-53`) is kind `transport` and renders as a
  dismissible board-level banner hosted by `Bridge`; a reject-on-live
  `CodeAlreadyExists` is kind `rejected` and renders card-local on the card
  named by `workstreamId`. A bare string accessor could not tell the two
  apart. Both kinds are producible in this lane (live-wired per DL-165):
  `rejected` by the local guard or the server's rejection, `transport` by
  the fake client's `Unavailable`.

Interfaces:

```ts
// store.ts — AppStore additions
/** The pending spawn failure, or null. `kind` selects the presentation:
 *  "transport" → dismissible board-level banner; "rejected" → card-local on
 *  `workstreamId` (always set for that kind). */
spawnAlert: Accessor<{
  kind: "transport" | "rejected";
  message: string;
  workstreamId?: string;
} | null>;
dismissSpawnAlert: () => void;
```

Test cycle: `LogPanel.test.tsx` — stop requires two clicks; disarm on
timeout; disabled only when `selectedAgentStartedBinding()` is absent (or in
none of `running`/`stop-failed`) **and** the selected agent has no running
non-fixture session; the DL-169 recovery guard red-first: with the binding
parked at `stop-failed` (arranged through a failing fake stop), the button
is **enabled** and re-issuing recovers `stop-failed` → `stopping` →
`stopped`. Card tests — pill renders on every phase including `running`;
Retry renders on `spawn-failed` and **not** `stop-failed`, calls `retrySpawn`
and does not select/open; "▶ start" shows on a binding-less card **and** a
`stopped` binding — both only when the agent has no live session — and a
start→stop→start cycle reaches `spawning` again (the DL-168 dead-end
regression); the card still selects/opens as before. `Bridge.test.tsx` — a
`transport` alert renders the board-level banner and `dismissSpawnAlert`
clears it; a `rejected` alert renders card-local on the named card and on no
sibling.

### T5 — Record cleanup + docs sweep

Verify no component reads `spawn.ts` state except through store accessors;
sweep any store/LogPanel doc comments whose wording predates the binding
integration; biome + markdownlint pass; changelog entry.

Interfaces: none new.

Test cycle: the T1–T4 suites re-run green (`moon run compass-ui:test` scoped
to the touched suites); **no new tests, a deliberate exemption** from the
red→green Global Constraint — T5 adds no behavior. Stated so the absence
reads as a decision, not an omission.

### T0 — Proto + server: `SpawnAgent` RPC + `AgentSessionStatus` attribution (compass service-owner's lane)

Not this record's UI PR — the proto/server change T2/T4 consume, owned by
the compass service-owner. Independent of the UI tasks, so no freeze block.

- **Change 1 (DL-166):** add `rpc SpawnAgent(SpawnAgentRequest) returns
  (SpawnAgentResponse)` to `CompassService` after `StartAgentSession`
  (`compass.proto:59`). Request = `agent_account_id`=1, `initial_prompt`=2,
  `client_request_id`=3; Response = `session_id`=1, `container_name`=2.
  Server orchestrates the existing RunnerHub `Provision` then `Start`
  (`runnerhub/commands.go:40-88`). *(The internal monorepo's T0 additionally relaxed the
  repo requirement in `runner/spec.go` and guarded `cloneRepo` — all shipped
  in compass under RIG-1527; `BuildSpec` at `spec.go:76-88` already builds a
  repo-less spec. Dropped from scope.)*
  **Acceptance (end-to-end idempotency):** a retry with the same
  `client_request_id` returns the same `session_id` and provisions no
  second container. Not a new primitive — `provisionDedupID`
  (`commands.go:229-244`), the router's in-flight join
  (`router.go:117-132`), and the dispatcher's `handled` map
  (`dispatch.go:223-236`) exist; what T0 adds is the **composite span**:
  thread the one id across both steps (today `orNewRequestID` mints a fresh
  id per relayed command, `commands.go:70`) and make the orchestrated Start
  idempotent on an already-live container.
- **Change 2 (DL-167):** append `string agent_account_id = 3;` to
  `AgentSessionStatus` (`compass.proto:324-327`) and populate every
  construction site — three functions build the message: Server-side, the
  board projection's `statusOf` (`board/projection.go:143-145`, the sole
  builder behind `GetAgentStatus`, reached from `Snapshot` at
  `:114`/`:122`) and the push-path synthesis in `Hub.deliverSession`
  (`hub.go:533-546`, builds `{SessionId, State}` at `:543-546`);
  Runner-side, `agentHost.Status` (`runner/host.go:487-501`, both the
  single-session arm at `:495` and the all-sessions arm at `:499`), which
  is what the reject-on-live scan reads. **The two sides have different
  sources.** Server-side is a join onto the live hub binding
  (`sessionAccounts`/`accountForSession`, `relay_comms.go:146-155`) — and
  the board projection must carry the account through `Snapshot`, not only
  the handler. Runner-side cannot reach `package runnerhub`, and the Runner
  keeps no account today (`liveSession`, `host.go:84-88`, has no account
  field; the account appears in the Runner tree only as a `BuildSpec` local
  consumed into the container name, `spec.go:85`) — so T0 carries the
  account onto `runtime.AgentSpec` at `BuildSpec` (where
  `req.GetAgentAccountId()` is in hand), exposes it through an
  `AgentHandle` accessor, and `Start` copies it onto a new `liveSession`
  field when it resolves the handle by container name
  (`host.go:227-241`). Deliberately **not** a `NamePrefix` name-parse off
  `containerName` (DL-170). **Resolve the account before the binding is
  dropped**: `Hub.Stop` calls `unbindSession` as soon as the Runner answers
  (`commands.go:91-104`), and enroll clears the maps on re-attach
  (`hub.go:707-711`) — so the terminal STOPPED/ERRORED statuses would
  otherwise publish with no account. T0 either captures the account in
  `Hub.Stop` prior to `unbindSession` (the `unbindSession` presence edge,
  `relay_comms.go:95-124`, is the in-tree precedent) or keeps the binding
  alive until the terminal status publishes.
- **Change 3 (network-door classification, build-gating):** classify
  `SpawnAgent` in `classifyProcedure`
  (`go/internal/auth/admin_gate.go:47-127`). Not optional polish:
  `TestClassifyProcedureCoversEveryGeneratedProcedure`
  (`classify_exhaustive_test.go:49-65`) walks every generated procedure and
  fails on any `classifyProcedure` does not recognize, so adding the RPC in
  Change 1 without this lands T0 **red**. Ship it `adminOnly` (DL-171),
  alongside the sibling lifecycle RPCs (`admin_gate.go:49-64`) —
  `SpawnAgent` is strictly more powerful than any of them, since it
  provisions *and* starts. Consequence, ruled rather than decided by
  omission: under `adminOnly` the whole Bridge control surface works only
  for the bootstrap admin; opening it later is a one-line
  reclassification (or, if board users should spawn, `authenticatedOpen`
  with per-account authorization in the handler, the
  `SubscribeAgentSession` shape).
- **Change 4 (DL-170 — spec: one container per agent account):** add a
  SHALL to `docs/specs/product/compass.md` binding **one container per
  agent account**, so the agent-scoped reject-on-live is contract-backed
  rather than resting on the Runner's container naming
  (`spec.go:85`). Lands alongside Changes 1–3 in the same T0 PR, per the
  "spec updated as the last step of implementation" convention. The
  existing SHALL at `compass.md:368-370` is container-scoped; the two
  coincide only via the naming property, which is the defect.
- Server tests: `SpawnAgent` runs Provision→Start; is idempotent on a
  repeated `client_request_id`; **rejects with `CodeAlreadyExists` when the
  target agent already holds a live session** — asserted against the
  **pre-Provision** short-circuit: the fake Runner records the commands it
  receives and the test asserts **zero** `Provision` commands were
  dispatched for the rejected call, in addition to asserting the code (an
  implementation that collides on the container name mid-Provision would
  return an internal error and still churn a container per rejected spawn);
  maps a mid-sequence failure to the right Connect status; every
  `AgentSessionStatus` emitted while the session's hub binding is live
  carries its `agent_account_id` — including the terminal STOPPED status. A
  status emitted after a Runner reconnect has cleared the maps carries none
  and reconciles nothing; that is the stated residual gap, not a test
  failure.

## Tasks

- [ ] T0 — (compass service-owner's lane) proto: `SpawnAgent` RPC +
  `agent_account_id` on `AgentSessionStatus`; server orchestration +
  attribution (Server hub join + Runner-local account carriage); `adminOnly`
  classification (DL-171); the one-container-per-agent-account SHALL in
  `compass.md` (DL-170); server tests incl. the pre-Provision
  zero-Provision-commands assertion. **Blocked on** nothing.
- [ ] T1 — `spawn.ts`: `SessionBinding` / `SpawnPhase` / `SpawnSpec` types +
  pure reducers + `bindingDotState`; `spawn.test.ts` red→green. **Blocked
  on** nothing.
- [ ] T2 — store actions `startAgent` (reject-on-live, live-wired) /
  `stopAgent` binding integration / `retrySpawn` + `sessionBinding` /
  `selectedAgentStartedBinding` accessors + `spawnAlert`;
  `store-spawn.test.ts` red→green with fake clients. **Blocked on** T1, and
  on T0 for the generated `spawnAgent` method (DL-165: no fixture lane).
- [ ] T3 — `StartAgentDialog.tsx`; the dialog suite red→green. **Blocked on**
  T2.
- [ ] T4 — card "▶ start" affordance (no-binding **or** `stopped`, DL-168);
  LogPanel two-step confirm (guard = enablement, agent-scoped, `stop-failed`
  covered per DL-169); card restructure to `<div role="button">` + pill +
  Retry on `spawn-failed` only; `spawnAlert` presentation (transport banner
  / card-local rejection); component tests red→green. **Blocked on** T2.
- [ ] T5 — docs sweep, biome + markdownlint, changelog; touched suites
  green. **Blocked on** T4.

## Resolved decisions

The internal monorepo's prior record resolved eight load-bearing forks with
Matt and ratified two deferrals. This port re-expresses them as compass ledger
rows DL-164..DL-171, plus one port-amendment row (DL-165). The prior rulings'
full arguments live in the source record; the compass-grounded substance is
folded into the sections above. Mapping:

### DL-164 (prior decision) — start-an-agent; multiple cards per agent; reject-on-live

Starting an agent is the **lifecycle** operation (`startAgent` →
`SpawnAgent`), and `SpawnAgent` rejects when the agent already holds a live
session. An agent owns multiple cards, so the `SessionBinding` map is keyed
by `workstreamId` (= `Issue.id`); one container per agent account, one live
session in it; the agent clones and works its several repos inside that one
container. Bindings for one agent never share a session: at most one of an
agent's cards carries a `sessionId`, every other card has no binding.

**Add-half superseded by DL-185 (Matt, 2026-08-07):** the manual
add-a-workstream board mutation (`addWorkstream`) and UI-side agent creation
(`CommsService.CreateAgent`) are dropped — cards come from the tracker
(issues forge-ingested, DL-069/DL-161) and the Manager builds the tree
(DL-095/DL-134; positive replacement RIG-1820), not a UI mutation.

### DL-165 (port amendment, this record) — live-wired posture

Supersedes the walking-skeleton ruling (the prior record's): its premises
(no injection seam, no live streams, stub stop) are all false in compass —
`AppStoreOptions` + live comms/board streams + real `stopAgent` shipped. The
control lane builds against the live seam; the prior record's stacked T6 dissolves into
T2/T4. Offline behavior is the shipped refusal shape, never a silent no-op.

### DL-166 (prior decision) — server-side composite `SpawnAgent`

One `CompassService.SpawnAgent` RPC runs Provision→Start server-side under a
single `client_request_id`; the server owns the internal dedup (composing
`provisionDedupID`, the router join, and the dispatcher's `handled` map);
the client has no provisioning/starting split and no retry-id juggling.
Reject-on-live is a pre-Provision short-circuit ordered after the dedup-join
lookup, sourced from the Runner's authoritative status scan. Rejected
alternative: client-orchestrated two-call flow.

### DL-167 (prior decision) — `agent_account_id` on `AgentSessionStatus`

The wire attribution making the reconcile full-fleet within the live hub
binding's scope; joined Server-side from `sessionAccounts`, carried
Runner-side on `AgentSpec`/`liveSession`; the terminal STOPPED status
resolves its account before `unbindSession` drops the binding. Also the
prerequisite of the reject-on-live scan. Rejected alternative:
self-spawned-only reconcile scope.

### DL-168 (prior decision) — a `stopped` card stays restartable

The "▶ start" affordance shows on no-binding **or** a `stopped` binding
(agent not live), matching the `startAgent` guard exactly — `applyStopped`
returns a binding rather than clearing it, so a no-binding-only affordance
would strand every stopped card. Restart mints a fresh binding via
`beginSpawn`.

### DL-169 (prior decision) — Retry is `spawn-failed`-only

On `stop-failed` the session is still held, so re-sending `SpawnAgent` is
semantically wrong (and reject-on-live would bounce it); recovery is
re-issuing the idempotent stop, so `beginStop` accepts `running` |
`stop-failed` and the LogPanel enablement covers `stop-failed`.

### DL-170 (prior decision) — one-container-per-agent-account SHALL

T0 adds a SHALL to `compass.md` binding one container per agent account, so
the agent-scoped reject-on-live is contract-backed. Today the guarantee is
incidental — the container name is a pure function of the account
(`spec.go:85`) and the spec SHALL at `compass.md:368-370` is
container-scoped; a future Runner naming containers differently would
satisfy every existing SHALL while breaking the rule.

### DL-171 (prior decision) — `SpawnAgent` ships `adminOnly`

Every generated procedure must be classified (`admin_gate.go:47-127`;
exhaustiveness enforced by `classify_exhaustive_test.go:49-65`), so T0 must
pick. `adminOnly`, alongside the sibling lifecycle RPCs — `SpawnAgent`
provisions *and* starts, strictly more powerful. Accepted consequence: the
Bridge control surface works only for the bootstrap admin for now; the
future non-admin shape is `authenticatedOpen` + per-account authorization in
the handler, not a bare reclassification.

### Deferrals (ratified, non-load-bearing)

- **Graceful vs hard stop** (prior deferral): the wire has one "deliberately
  kill" semantic (`compass.proto:61-63`); "graceful" is the T4 two-step UI
  confirm. `ReloadAgentSession` surfaces later as a distinct restart action.
- **Injection-seam signature** (prior deferral): dissolved by shipped code —
  `AppStoreOptions` exists (`store.ts:551`) and the disposal question is
  settled by the store's owner-scoped cleanup (`store.ts:899-900`,
  `:924-925`). Nothing left to coordinate.

## Open Questions

None load-bearing beyond the recorded decisions. One port-level note for
Matt's awareness at review (not a blocker; designed against the stated
assumption):

- **OQ-P1 — posture amendment (DL-165).** This port supersedes the prior
  Matt-ruled walking-skeleton posture because its factual premises no longer
  hold in compass (live seam + real stop shipped). Recommendation: ratify
  DL-165 as stated. If Matt prefers to preserve the fixture-first lane
  anyway, T2/T4 split back into fixture actions + a stacked wiring lane and
  DL-165 is struck — the rest of the record is unaffected.
