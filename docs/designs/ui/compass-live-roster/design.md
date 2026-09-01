# Compass Live Roster — the agent tree reads the live roster (RIG-2033)

Status: Draft

## Problem / Intent

The Compass UI's agent surfaces (workspaces tree, board swimlanes, fleet
sidebar) still render the hardcoded `STUB_AGENTS` fixture even on a live
connection: `LeftSidebar.tsx:400` renders `<For each={agentTree(STUB_AGENTS)}>`
directly from the fixture module, and the store pins a non-reactive
`const agents = STUB_AGENTS` (`store.ts:669`) with no live accessor. The
comms/board streams are already live (accounts, issues); this record designs
the missing source swap — the agent roster reads the live server (identity +
tree + presence + activity), and `STUB_AGENTS` is demoted from render source to
offline seed.

## Approach

### Ground truth (what exists today)

**The live connection succeeds; only the agent source is fixture.** Boot
resolves the door and streams comms + board events:

- `store.ts:897-911` runs the comms driver when `options.comms` is set:

  ```ts
  if (options.comms) {
      const client = options.comms;
      ...
      void runCommsStream({ client, callerId, mapMessage: adaptMessage,
          onState: adoptComms, signal: abort.signal, ... })
  ```

- `store.ts:935-942` runs the board event stream (`runEventStream({ client,
  onIssues: setIssues, ... })`), replacing the `STUB_ISSUES` seed — the pattern
  this record repeats for agents. The issues seam is explicitly documented as
  the template (`store.ts:670-672`): "Seeded from the fixture; the real
  @compass/client stream replaces the seed later (the accessor stays the
  seam)."

**The agent source is a dead const.** `store.ts:669`:

```ts
const agents = STUB_AGENTS;
```

Three store reads resolve against it — `agentById` (`store.ts:796-797`),
`selectedAgent` (`store.ts:987-989`), and `agentView` (`store.ts:993-994`) —
and the store's own comment names the owed migration (`store.ts:793-795`):
"`agents` is a static const today, so this cannot flip live→unreachable yet;
the live-agents migration owes converting `agents` to a signal/store read
flowing through this one seam (not a non-reactive snapshot)." The store
interface documents the join seam (`store.ts:281-286`): "The pure seam
(`joinAgents` in the real era) the workspace WILL read once the
SubscribeComms/SubscribeEvents join lands."

**Two components bypass the store entirely:**

- `LeftSidebar.tsx:400`: `<For each={agentTree(STUB_AGENTS)}>` — the workspaces
  tree.
- `Bridge.tsx:110` (`prRowGroups(STUB_AGENTS, store.issues())`) and
  `Bridge.tsx:121` (`boardAgentsOf(STUB_AGENTS, store.issues())`) — the board's
  swimlane/PR groupings, with the comment "STUB_AGENTS stays direct — agents
  aren't mutated here" (`Bridge.tsx:120`).

**The derivation is already live-ready.** `agentTree(agents: readonly
Agent[]): AgentTreeNode[]` (`stub-data.ts:387-426`) is a pure, total,
cycle/dangling-safe derivation over `parentAgentId` — source-agnostic. The
board helpers are likewise pure over an injected agent list (`board.ts:53-56`
`boardAgents(agents, all)`, `board.ts:87` `treeOrder(agents)`,
`board.ts:139-142` `prRowGroups(agents, all)`). Only the INPUT is hardcoded.
The agent-trees record designed exactly this derivation
(`docs/designs/agent/compass-agent-trees/design.md:383-417`, §T4) against
the fixture carrying `parent_agent_id`; the fixture→live source swap is the
layer it left open — this record. Its derivation contract (stable input order,
dangling-promotes-to-root) is untouched here.

**Live identity already flows.** `CommsState.accounts` is live
(`comms-state.ts:54-59`), snapshot-reduced via `adaptAccount`
(`comms-state.ts:97`) and event-updated via the `accountChanged` case
(`stream.ts:209-214`). `adaptAccount` (`adapt.ts:118-143`) lifts the agent
arm's `ownerUserId` / `homeChannelId` / `parentAgentId` onto the flat domain
`Account`, normalizing empty-string parent to `undefined` "so the domain's
'absent = a root' contract holds and agentTree derives it as top-level"
(`adapt.ts:133-136`).

**The live presence source exists server-side but is unconsumed:**

- `GetRoster` RPC (`comms.proto:113-114`: "the live presence projection
  with the durable activity string"; generated client `comms_pb.ts:2567-2573`
  `getRoster: { methodKind: "unary"; ... }`). `RosterEntry`
  (`comms.proto:705-713`): `agent_account_id`, `handle`, `display_name`,
  `parent_agent_id`, `presence`, `activity`, `activity_at_unix_ms`.
  `RosterScope` (`comms.proto:690-697`): `NEIGHBORHOOD` / `SUBTREE` / `OWNER`
  ("the whole agent set owned by the vantage agent's owner");
  `GetRosterRequest.agent_account_id` is "Optional for human/UI callers naming
  a vantage" (`comms.proto:684-686`). Ratified as DL-135 ("Agent roster is a
  pull ... reading the DL-074 in-memory presence enum joined with the agent
  tree; the activity string is DURABLE").
- `AgentPresence` (`comms.proto:548-554`) is the DL-074 four-state enum:
  `UNSPECIFIED / IDLE / WORKING / WAITING / OFFLINE`.
- `AgentPresenceChanged` (`comms.proto:536-544`: `agent_account_id`,
  `presence`, `activity`) flows on SubscribeComms (field 17,
  `comms.proto:471-472`, "PUBLIC — UI board state consumes it") but the UI
  driver drops it today in `decodeEvent`'s default arm (`stream.ts:224-227`).
- `AgentSessionStatus` on SubscribeEvents is the richer session lifecycle; the
  events driver deliberately ignores it (`events.ts:34-37`: "gated on other
  lands and are safely ignored").

**The view-model.** `Agent` (`stub-data.ts:344-361`) = durable `account:
Account` + optional `lifecycle?: AgentLifecycle` (`= AgentState`,
`stub-data.ts:336`) + optional `activity?: string` (already documented as
"AgentPresenceChanged.activity", `stub-data.ts:347-352`) + UI-only `role:
AgentRole` / `model: string` / `cwd: string` / `terminals: Terminal[]`
(`stub-data.ts:353-360`: "UI-only roster config" / "Fixture-only").
`AgentState` (`stub-data.ts:47-55`) is the eight-value union `working | idle |
waiting | done | paused | stopped | error | disconnected` the `StateDot`
renders (`StateDot.tsx:16`, `AGENT_STATE_LABEL` at `constants.ts:32`), per the
ux-foundation D3 vocabulary ("the eight `AgentState` values, keyed 1:1 against
the frozen brand vocabulary", `design/components.md:90-92`).

### The chosen approach

**Live agents = a reactive join of the already-live `accounts` with a new
presence map; `GetRoster` seeds the map; `AgentPresenceChanged` updates it;
`STUB_AGENTS` becomes the offline seed only.**

Concretely (each is a settled fork; the full rationale is in Resolved
decisions):

1. **Roster source — join, not replace.** `RosterEntry` carries identity
   fields (`handle`, `display_name`, `parent_agent_id`) but NOT
   `homeChannelId` / `ownerUserId` / `kind` — and the store's `openAgent`
   path and the comms rail depend on `homeChannelId` (e.g.
   `store.test.ts:350-352` resolves `account.homeChannelId` to open the home
   DM). So `Account` (from the live `SubscribeComms` accounts, already wired)
   stays the single identity + tree source, and `GetRoster` is consumed only
   for what accounts don't carry: the initial presence + activity join.
   Live updates ride the `AgentPresenceChanged` event the stream already
   receives and drops. This avoids a second identity source that could drift
   from `accountChanged`, and reuses the existing snapshot+tail reducer
   (`comms-state.ts` / `stream.ts`) rather than adding a parallel driver.
2. **Presence→AgentState mapping** (total over the 4-state enum):
   `WORKING → "working"`, `IDLE → "idle"`, `WAITING → "waiting"`,
   `OFFLINE → "stopped"`, `UNSPECIFIED → undefined`. The `OFFLINE → "stopped"`
   arm is a ruled decision (Resolved decision R2): the server defaults EVERY
   agent absent from its in-memory presence source to `OFFLINE`
   (`roster.go:79-82` "Live presence enum (absent → OFFLINE)", pinned by
   `roster_pgtest_test.go:188-190`), so `UNSPECIFIED` is unreachable on the
   `GetRoster` path and `OFFLINE` covers BOTH "deliberately stopped" and
   "never started" — the 4-state enum cannot split them client-side. Mapping
   `OFFLINE → "stopped"` renders such an agent with the hollow-ring "stopped"
   dot (`agent-state.ts:41` "terminated; distinct from live idle",
   `StateDot.tsx`); the known day-one cost is that a freshly-seeded-but-
   unstarted agent (the DL-192 root supervisor, or a whole fleet just after a
   runner restart) reads "stopped" rather than today's grey idle dot
   (`AgentLeaf` defaults `lifecycle ?? "idle"`, `LeftSidebar.tsx:47`) — and
   because the activity string is durable (`roster_pgtest_test.go:254-258`),
   such an agent can even carry a live-looking activity string beside a
   "stopped" dot. Splitting "terminated" from "never started" is owed to the
   deferred `AgentSessionStatus` lane; the 4-state enum alone cannot.
   `UNSPECIFIED → undefined` stays in the total mapping as a defensive arm only
   (unreachable from GetRoster, matching `agent-state.ts:43-44`). Reachable dot
   subset from the roster stream: `working / idle / waiting / stopped`. The
   remaining four (`done`, `paused`, `error`, `disconnected`) stay reachable
   only via `AgentSessionStatus` (`agent-state.ts:47-56`
   `agentDotState(...): AgentState` already implements that projection) and the
   spawn phase machine (`spawn.ts:142` `bindingDotState`), both separate lands —
   consuming `AgentSessionStatus` in the events driver stays deferred (ignored
   today, `events.ts:34-37`; wiring it is a SubscribeEvents-lane change, not a
   roster change).
3. **UI-only fields.** Grounded consumers: `role` → only the LeftSidebar
   role-pip (`LeftSidebar.tsx:49-53`, shown when `role !== "worker"`);
   `model` + `cwd` → only the AgentView header (`AgentView.tsx:219-220`);
   `terminals` → only AgentView panes (`AgentView.tsx:22-23,181`; the
   zero-terminal path is already proven — `acc-supervisor` has `terminals:
   []`, `stub-data.ts:481`, and `AgentView.test.tsx:193-195` pins it). So:
   `role`, `model`, `cwd` become OPTIONAL on `Agent` (`role?: AgentRole`,
   `model?: string`, `cwd?: string`); a live agent carries none of them; the
   pip and the header spans render only when present (`<Show>`). `terminals`
   stays required but a live agent gets `[]` (the already-proven empty path).
   Deriving `role` from tree position is rejected: "has children" ≠
   supervisor (any worker can spawn), and no server field exists — an honest
   absent beats a guessed pip.
4. **Empty roster.** A live connection with zero agent accounts renders the
   tree-empty state the surfaces spec already defines
   (`design/surfaces.md:129-131`: "Tree-empty (no agents yet) renders a real
   empty-state row set: a one-line explanation and the palette hint ... not a
   blank column"). No client-side fake supervisor: RIG-1820 / DL-192 seed the
   root manager server-side (`seedRootSupervisor`, DL-192), so a live empty
   tree is a legitimate transient, not a state to paper over. (The empty-state
   VISUALS are the disjoint compass-ux styling lane; this record owes only the
   conditional render seam.)
5. **Stub story.** `STUB_AGENTS` survives as the OFFLINE seed, exactly the
   `STUB_ISSUES` pattern (`store.ts:670-673`): with `options.comms` absent the
   store's `agents` accessor returns the fixture; with it present, the live
   join (which starts empty — `EMPTY_COMMS_STATE`, `comms-state.ts:63-69` —
   until the first snapshot, so no fixture flash on a live boot). The ~7 test
   files that construct offline stores or read the fixture
   (`store.test.ts`, `identity.test.ts`, `App.test.tsx`,
   `LeftSidebar.test.tsx`, `AgentView.test.tsx`, `RightSidebar.test.ts`,
   plus `comms-stub.ts` deriving `STUB_ACCOUNTS` from it,
   `comms-stub.ts:283-285`) keep working unchanged: offline stores keep
   resolving fixture agents through the SAME accessor the components now read.
   The `vite dev` walking skeleton (`App.tsx:20-25`) is preserved by the same
   arm.

### Why not the alternatives

- **`GetRoster` as the whole roster source** (poll or snapshot+events):
  duplicates identity already streaming via `accountChanged`, lacks
  `homeChannelId`/`kind` the UI requires, and adds a second source that can
  drift from the accounts the comms rail renders. Rejected.
- **A separate roster driver/signal outside `CommsState`:** the presence event
  arrives ON the comms stream, and the reducer already owns
  snapshot-boundary + tail-overlap semantics (`comms-state.ts:47-53`). A
  parallel driver would re-implement resync/backoff for one map. Rejected —
  presence joins `CommsState` as a fifth collection.
- **Consume `AgentSessionStatus` now for the full 8-state dot:** a
  SubscribeEvents-lane change with its own attribution/precedence design
  (spawn-binding vs live-state precedence, `spawn.ts:13-14`); the roster swap
  neither needs it nor blocks it. Deferred.
- **Presence/activity on `Account` + `accountChanged` (server-side),
  eliminating the client join:** structurally the cleanest long-term shape —
  one identity source, no client join — but a proto + comms-server lane far
  outside RIG-2033's UI scope. Noted as the rejected long-term direction, not
  taken now.

No new abstraction is introduced: the change is one new `CommsState`
collection, one adapter, one mapping function, one store memo behind the
already-documented `joinAgents` seam, and component cutovers to the accessor.

## Plan

### Global Constraints

- **Domain shapes above the adapt seam** — components and the store never see
  wire types; all wire→domain mapping lives in `apps/ui/src/live/adapt.ts`
  (the existing convention, `adapt.ts` header + `stub-data.ts:338-339` "NEVER
  a wire shape").
- **Offline-constructible store** — `createAppStore` with no
  `options.comms`/`options.compass` must keep working with fixture data and no
  network (the walking skeleton, `App.tsx:20-25`; every existing happy-dom
  test constructs it this way).
- **`agentTree` / `treeOrder` / `boardAgents` / `prRowGroups` contracts are
  frozen** (agent-trees record §T4/T5) — this record changes their INPUT
  plumbing only, never their semantics.
- **No styling work** — `app.css` / token/empty-state visuals are the
  compass-ux lane; this record adds render seams only.
- **Mixed liveness is out of scope** — a store constructed with one client but
  not the other (comms live, board not, or vice versa) renders per-arm: live
  agents join `store.agents()` behind `options.comms` while issues stay live
  behind `options.compass` (`store.ts:895-921`), so Bridge's agent×issue join
  (`Bridge.tsx:110`) can show live agents against fixture issues. A degenerate
  dev configuration, deliberately not reconciled here.
- **Repo:** `RigelBuild/compass`; UI at `apps/ui`; tests are happy-dom
  bun/vitest-style suites beside their modules; Biome formatting.
- **Every task lands with its own tests** (rule://red-green-testing) and
  keeps the full existing suite green.

### T1 — presence mapping + adapter (pure, no store)

Add the wire→domain presence pieces in `apps/ui/src/live/adapt.ts`.

- Interfaces:
  - `export function presenceLifecycle(p: AgentPresence): AgentState | undefined`
    — total over the generated `AgentPresence` enum (`@compass/client`):
    `WORKING → "working"`, `IDLE → "idle"`,
    `WAITING → "waiting"`, `OFFLINE → "stopped"`,
    `UNSPECIFIED → undefined`; default arm throws on an unmodeled numeric
    (the `agent-state.ts:75-78` exhaustiveness convention).
  - `export interface AgentPresenceInfo { readonly lifecycle?: AgentState; readonly activity?: string; }`
    — the domain presence value, keyed by agent account id (empty-string
    activity normalizes to `undefined`, matching the `Agent.activity`
    contract `stub-data.ts:350-351`).
  - `export function adaptRosterEntry(w: RosterEntry): [string, AgentPresenceInfo]`
    — maps one wire entry to its map entry (id + info). Identity fields
    (`handle`, `displayName`, `parentAgentId`) are deliberately DROPPED —
    accounts own identity (Approach fork 1).
- Tests: a sibling block in the existing adapt tests — mapping totality,
  empty-activity normalization, unmodeled-enum throw.

### T2 — presence joins CommsState (reducer + stream driver)

Extend the comms reduction so presence is a fifth reduced collection, seeded
by `GetRoster` and tailed by `AgentPresenceChanged`.

- Interfaces:
  - `CommsState` (`apps/ui/src/live/comms-state.ts:54-60`) gains
    `readonly presence: ReadonlyMap<string, AgentPresenceInfo>;`
    (and `EMPTY_COMMS_STATE` gains `presence: new Map()`).
  - `CommsSnapshot` (`comms-state.ts:79-85`) gains
    `readonly roster: readonly RosterEntry[];`; `reduceSnapshot`
    (`comms-state.ts:93-104`) reduces it via `adaptRosterEntry`.
  - The snapshot fetch lives in `fetchSnapshot` (`stream.ts:87-129`), which
    runs the existing `listAccounts`/`listChannelGroups`/`listChannels` reads
    in one `Promise.all` (`stream.ts:94-98`). Add
    `client.getRoster({ scope: RosterScope.OWNER })` to that SAME failure
    domain: the existing reads are NOT best-effort — a rejection throws, is
    caught in `runCommsStream` (`stream.ts:357-361`), and retries the whole
    snapshot with backoff. Roster joins them so a failed read gets
    retry-with-backoff for free, rather than a swallow that would leave
    presence permanently empty (nothing re-reads `GetRoster` on a tail
    resubscribe — the snapshot only runs when `tailSeq === 0n`,
    `stream.ts:293,315-326`).
  - Consistency: `GetRosterRequest` carries only `scope` + `agent_account_id`
    (`comms.proto:683-688`), no `snapshotSeq` unlike the other reads
    (`stream.ts:95-97`). So the presence seed is unversioned — it races the
    accounts snapshot boundary and converges via the seq'd tail replay
    (last-write-wins, `stream.ts:322-333`), by design.
  - `decodeEvent` (`stream.ts:173-227`) gains a
    `case "agentPresenceChanged"` arm producing a domain event
    `{ kind: "presenceChanged"; accountId: string; info: AgentPresenceInfo }`;
    the reducer applies it as a map upsert (a fresh Map instance — the
    "every transition returns a fresh object" contract,
    `comms-state.ts:43-45`).
  - `GetRoster` is called with no `agent_account_id`: the server defaults the
    vantage to the caller and resolves a user caller to its own owned set
    (`roster.go:34-36` + `ownerOf` `roster.go:144-153`) — see R6 (confirmed).
- Tests: reducer tests beside the existing comms-state suite — snapshot seeds
  the map; a presence event upserts; structural sharing (untouched collections
  keep identity); a roster-read rejection aborts the snapshot like its siblings
  (surfaces to `onError` + backoff), not a silent empty map.

### T3 — the store's reactive agents seam (`joinAgents`)

Convert the dead const to the documented reactive seam.

- Interfaces:
  - `export function joinAgents(accounts: readonly Account[], presence: ReadonlyMap<string, AgentPresenceInfo>): Agent[]`
    — pure, in a new small module `apps/ui/src/roster.ts` (sibling of
    `board.ts`, same pure-over-injected-inputs shape, `board.ts:6`): filters
    `kind === "agent"`, preserves account order (the `agentTree` stable-order
    contract, `stub-data.ts:372-377`), and composes
    `{ account, lifecycle: info ? info.lifecycle : "stopped", activity: info?.activity, terminals: [] }`.
    A presence-map MISS maps to `"stopped"`, mirroring the server's
    absent→OFFLINE→stopped default at the client seam (R2 / DL-194): an account
    present in `accounts` but absent from the presence seed — a snapshot-
    boundary race, or a post-snapshot `accountChanged` arrival never re-seeded
    (`GetRoster` runs only at `tailSeq === 0n`, `stream.ts:293`, and
    `AgentPresenceChanged` fires only on a real transition, `presence.go:36-38`,
    so the miss is durable, not a boot flicker) — is an at-rest/unstarted
    agent, so it MUST render the "stopped" dot, never the false-live grey idle
    dot the components' `lifecycle ?? "idle"` fallback (`LeftSidebar.tsx:47`,
    `AgentView.tsx:217`) would otherwise show; the first real
    `AgentPresenceChanged` upserts the map and flips the dot. A present-but-
    `UNSPECIFIED` entry keeps `lifecycle: undefined` → the defensive idle arm
    (unreachable on the GetRoster path).
  - `Agent` (`stub-data.ts:344-361`): `role`, `model`, `cwd` become optional
    (`role?: AgentRole; model?: string; cwd?: string`). `terminals:
    Terminal[]` stays required (live = `[]`).
  - `store.ts:669` becomes a memo pair. First an intermediate presence memo
    beside the existing per-collection memos (`store.ts:841-845`):
    `const presence = createMemo(() => comms().presence);` — so it re-notifies
    only when the presence map's identity actually changes, not on every comms
    event (each posted message replaces the whole `CommsState` via `adoptComms`,
    `store.ts:869-871`; the per-collection memos lean on `createMemo`'s `===`
    equality + the reducer's structural sharing to absorb that). Then:
    `const agents = createMemo<readonly Agent[]>(() => options.comms ? joinAgents(accounts(), presence()) : STUB_AGENTS);`
    — the roster re-joins only when accounts or presence change, not on every
    chat message. (On a genuine presence tick the join builds fresh `Agent`
    objects and Solid's keyed `<For>` rebuilds the tree DOM; that residual
    churn is bounded and matches the existing Bridge-over-issues behavior,
    `Bridge.tsx:110/121`.) `agentById` (`store.ts:796-797`), `selectedAgent`
    (`store.ts:987-989`), `agentView` (`store.ts:993-994`) and the
    fixture-derived clones accessor read `agents()` instead of the const,
    discharging the owed migration note (`store.ts:793-795`).
  - `AppStore` interface gains
    `agents: Accessor<readonly Agent[]>;` (beside `accounts`,
    `store.ts:368-370`), the accessor the components cut over to in T4.
- Tests: `roster.ts` unit tests — `joinAgents` over accounts + a presence map:
  a present entry projects its lifecycle/activity; a map MISS projects
  `lifecycle: "stopped"` (NOT idle) so an unseeded / just-arrived account
  renders the stopped dot; account order is preserved. Store tests — offline
  store returns the fixture through `agents()`; a store with a fake comms state
  joins live accounts + presence; `agentById` reacts to an agent-set change
  (the RIG-1645 reactivity the comment at `store.ts:790-795` owes).

### T4 — component cutover (retire STUB_AGENTS as render source)

- Interfaces:
  - `LeftSidebar.tsx:400`: `<For each={agentTree(store.agents())}>`; drop the
    `STUB_AGENTS` import (`LeftSidebar.tsx:20`).
  - `Bridge.tsx:110/121`: `prRowGroups(store.agents(), store.issues())` /
    `boardAgentsOf(store.agents(), store.issues())`; drop the import
    (`Bridge.tsx:20`) and the stale ":STUB_AGENTS stays direct" comment
    (`Bridge.tsx:119-120`).
  - `LeftSidebar.tsx:49-53`: the role-pip `Show` condition becomes
    `a().role !== undefined && a().role !== "worker"` (equivalently: pip only
    for a present, non-worker role).
  - `AgentView.tsx:219-220`: wrap `av-model` / `av-cwd` spans in
    `<Show when={...}>` so an absent field renders nothing.
  - `comms-stub.ts` is UNCHANGED (it derives fixture accounts for the offline
    arm, which survives).
- Tests: existing suites keep passing via the offline store (they exercise
  the same accessor). Add: a LeftSidebar test with a live-shaped store
  (agents from accounts+presence) renders the joined tree + activity; an
  AgentView test that an agent without `model`/`cwd` renders no
  `av-model`/`av-cwd` span.

### T5 — tree-empty state seam

- Interfaces:
  - `AgentsSection` (`LeftSidebar.tsx:382-407`): render a `.tree-empty` row —
    one-line explanation + palette hint copy per `design/surfaces.md:129-131`,
    instead of the bare `<For>` — ONLY when the roster is genuinely empty, NOT
    merely not-yet-loaded. Gate on
    `store.firstSnapshotArrived() && agentTree(store.agents()).length === 0`:
    on a live boot the join is empty until the first snapshot lands
    (`EMPTY_COMMS_STATE`), so a bare `length === 0` would flash "no agents yet"
    during every connect window for a fleet that HAS agents.
    `firstSnapshotArrived` already exists internally (`store.ts:860`, set in
    `adoptComms` `store.ts:871`) but is not yet on `AppStore` — T5 exposes it as
    `firstSnapshotArrived: Accessor<boolean>`. Offline stores are unaffected
    (the fixture is never empty and `firstSnapshotArrived` stays false). Class
    name only; visual styling is the compass-ux lane (Global Constraints).
- Tests: an empty live roster PAST the first snapshot renders `.tree-empty`; a
  pre-first-snapshot live store renders no `.tree-empty`; a non-empty roster
  renders none.

### Task dependency order

T1 → T2 → T3 → T4 → T5 (T5 depends only on T3/T4's accessor; T4 and T5 can
land together if the driver prefers).

## Tasks

- [ ] **T1** — `presenceLifecycle` + `AgentPresenceInfo` + `adaptRosterEntry`
  in `live/adapt.ts`, with mapping/normalization tests.
- [ ] **T2** — `CommsState.presence` map: `GetRoster(OWNER)` in the snapshot
  fetch, `agentPresenceChanged` in `decodeEvent`, reducer upsert + tests.
- [ ] **T3** — `joinAgents` (`roster.ts`) + `store.agents` reactive accessor;
  `Agent.role/model/cwd` optional; `agentById`/`selectedAgent`/`agentView`
  flow through the memo.
- [ ] **T4** — LeftSidebar + Bridge cut over to `store.agents()`; role-pip and
  model/cwd renders gated on presence of the optional fields; `STUB_AGENTS`
  imports removed from components.
- [ ] **T5** — tree-empty render seam in `AgentsSection`.

## Ledger delta

Appended to `docs/designs/product/DECISIONS.md` (UI shell section) in this PR
as DL-193/194/195; no existing row flips — DL-095 (tree primitive), DL-074
(4-state presence), DL-135 (GetRoster pull), DL-111 (WhoAmI), DL-077 (accounts
persist) are all composed with, not superseded:

- **DL-193** — The UI's live agent roster is a reactive JOIN: identity +
  tree from the live `SubscribeComms` accounts (the single identity source),
  presence + activity from a `CommsState` presence map seeded by one
  `GetRoster(OWNER)` per snapshot boundary and tailed by
  `AgentPresenceChanged`; `STUB_AGENTS` is demoted to the offline-store seed
  (the `STUB_ISSUES` pattern), never a live render source.
- **DL-194** — The presence→dot projection is the total 4-state mapping
  `WORKING→working, IDLE→idle, WAITING→waiting, OFFLINE→stopped`, plus a
  defensive `UNSPECIFIED→undefined` (unreachable on the GetRoster path, which
  defaults every absent/unstarted agent to `OFFLINE`, `roster.go:79-82`; the
  client `joinAgents` mirrors this, mapping a presence-map miss to `stopped` so
  the absent→stopped invariant holds end-to-end). The
  4-state enum cannot distinguish a deliberately-stopped agent from a
  never-started one; splitting them is owed to the deferred `AgentSessionStatus`
  lane, which is also the only source for the remaining four `AgentState`
  values (`done/paused/error/disconnected`) — its consumption stays deferred to
  its own lane.
- **DL-195** — `Agent.role/model/cwd` are optional view-model fields with no
  server source: a live agent renders without them (no derived/guessed role
  pip), and `terminals` is `[]` for live agents until a terminal stream
  exists.

## Resolved decisions

Every fork below is settled: R2 (`OFFLINE` dot) by Matt's ruling; R1/R3/R4/R5
survived the design-critic's attack as the recommended choice; R6 is confirmed
by server source. None is a live open question — the record carries these as
the frozen contract.

- **R1 (roster source) — join, not replace.** Join the live `accounts`
  (identity/tree) with a presence map seeded by `GetRoster(OWNER)` + tailed by
  `AgentPresenceChanged`, rather than `GetRoster` as the whole roster source:
  `RosterEntry` lacks `homeChannelId`/`kind` the UI requires
  (`store.test.ts:350-352` opens the home DM; `adapt.ts:124-137` lifts them from
  accounts), accounts already stream live, and a single identity source can't
  drift.
- **R2 (presence→AgentState) — RULED by Matt: `OFFLINE → "stopped"` now, split
  later.** Two parts. (a) Defer `AgentSessionStatus` and map only the 4-state
  presence enum for now — the richer 8-state projection already exists as
  `agentDotState` (`agent-state.ts:47-56`) for the session-events lane, a
  separate land (`events.ts:34-37`). (b) `OFFLINE → "stopped"` (offline = no
  live session, the lifecycle's terminal rest state; `disconnected` would
  wrongly connote a transport fault, which OFFLINE is not, per DL-074). Because
  the server defaults every agent absent from its presence source to `OFFLINE`
  (`roster.go:79-82`, pinned `roster_pgtest_test.go:188-190`), a
  freshly-seeded-but-unstarted agent (the DL-192 root supervisor; a whole fleet
  after a runner restart) renders the "stopped/terminated" dot rather than
  today's grey idle dot — a known, named day-one cost. Splitting "terminated"
  from "never started" is owed to the deferred `AgentSessionStatus` lane; the
  4-state enum alone cannot. The client `joinAgents` preserves this invariant
  at its own seam: an account absent from the presence map (a snapshot race or
  a post-snapshot `accountChanged` arrival) also maps to `"stopped"`, not the
  components' `?? "idle"` fallback, so "not live → stopped" holds end-to-end
  (see T3).
- **R3 (UI-only fields) — optional and render-gated.** `role`/`model`/`cwd`
  become optional; live agents get `terminals: []`; the role pip is NOT derived
  from tree position ("has children" is not "supervisor" and a guessed pip is
  worse than none). A live `role`/`model`/`cwd` later is a server-field lane
  (proto addition), parked.
- **R4 (empty roster) — surfaces-spec tree-empty row, no client-seeded fake
  supervisor.** Render the tree-empty row (`surfaces.md:129-131`); RIG-1820/
  DL-192 seed the root manager server-side, so a live-empty tree is transient
  and honest. (Gated on `firstSnapshotArrived()` so the connect-window is not a
  false empty — see T5.)
- **R5 (stub story) — `STUB_AGENTS` is the offline seed behind the same
  accessor.** The `STUB_ISSUES` pattern (`store.ts:670-673`); all existing
  offline tests keep constructing stores unchanged; the live arm starts empty
  (no fixture flash). Full retirement would kill the `vite dev` walking skeleton
  (`App.tsx:20-25`) and force a rewrite of every store-constructing suite for
  zero product value.
- **R6 (GetRoster vantage for a UI caller) — confirmed by server source.**
  `GetRoster(scope: OWNER)` with no `agent_account_id` from a user-credential
  caller returns exactly the caller's owned agent set: the handler defaults the
  vantage to the caller (`roster.go:34-36`) and `ownerOf` treats a user vantage
  as its own owner (`roster.go:144-153` — `AgentOwner`'s not-found signals "use
  vantage directly"). No server accommodation or events-only fallback is needed.
  One residue for the comms lane (not this record): no pgtest pins the
  empty-vantage + user-caller + OWNER path — every existing OWNER test names an
  explicit `AgentAccountId` (`roster_pgtest_test.go:142`); a one-line server
  test is owed.
