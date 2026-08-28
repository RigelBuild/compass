# Agent-primary issue-state write model (amendment)

Status: Active
Tracker: SEA

> **Amends frozen contract (#1018, DL-091 / DL-032).** This record is a
> sibling amendment to `docs/designs/product/compass-issue-model/design.md`
> (merged in #1018) and composes with the DL-070 server projection it froze.
> It ratifies Matt's 2026-08-04 ruling that the board lifecycle is
> **agent-primary**: "i'm not sure we should even have the UI move cards? i
> think that's primarily for the agents to do, the user can modify issue
> states via the issue tracker itself, not thru Compass." #1018 is NOT
> edited; the ledger routes readers of its stale write-path prose to this
> truth (the supersede-by-routing convention,
> `compass-attribution-simplification/design.md:96-100`). No wire build of
> the superseded *RPC* has shipped: grep of
> `compass/proto/compass/v1/*.proto` this run finds no `UpdateIssueState`
> RPC (verified absent). The `Issue`/`IssueState` READ types DID ship in
> RIG-1727 S1a (PR #145, `compass.proto:595` `enum IssueState`,
> `compass.proto:645` `message Issue`) and survive unchanged — only the
> `CompassService` write RPC was frozen on paper and is dropped before build.

## Problem / Intent

The frozen model made the **UI** the lifecycle driver: "Lifecycle is
server-authoritative, so lifecycle **writes** are a `CompassService`
surface" with a single mutation RPC — "The board is a MANUAL lifecycle — the
human/agent is authoritative over Compass state (DL-032)"
(`compass-issue-model/design.md:454-455`, `:480-482`), and DL-091 froze
archive as "a lifecycle transition to a terminal `ARCHIVED` state via
`UpdateIssueState`" (`DECISIONS.md:154`). Matt reversed the write model
(2026-08-04): the Compass UI does **not** move cards — it is read-only for
issue state; **agents** are the primary state mutators; **users** change
state in the external tracker, whose native status then flows INTO Compass.
The read path — the `Issue`/`PullRequest` type family, the `IssueState`
enum, and the `SubscribeEventsResponse.issue` stream variant — is not in
question and survives unchanged.

## Approach

One inversion, three mechanisms. The DL-070 server projection stays the
canon ("a server-side board projection … computes and streams the canonical
type, moving DL-032's canonical Compass state server-side",
`DECISIONS.md:153`); what changes is **who feeds it a lifecycle
transition**: an agent (over the gateway relay) or the tracker (via
ingestion) — never the Compass UI.

### (a) The agent write: a `BoardCall` family on the AgentGateway, relayed server-side

Agents cannot call `CompassService`: the container is egress-sealed —
`AgentGateway` runs "over a per-container bind-mounted Unix socket (a local
hop, no network path — the egress seal is untouched)"
(`compass/proto/compass/v1/agent_gateway.proto:8-10`), and DL-076 states the
consequence for lifecycle ops: "there is no public agent-callable spawn RPC
(the egress-sealed agent holds no server token)" (`DECISIONS.md:133`). So
the agent state-write is a **new sibling call family on the existing
`AgentGateway` socket**, mirroring the DL-049/DL-076 shape exactly: "Spawn
is a **sibling call family on the existing `AgentGateway` socket**, exactly
the DL-049 shape: a new `rpc Lifecycle(LifecycleCallRequest) returns
(LifecycleCallResult)` beside `rpc Comms(…)` … a new Runner→Server unary
`rpc RelayLifecycleCall(…)` beside `RelayCommsCall` … so the relay IS the
agent-facing RPC edge" (`compass-agent-spawn-despawn/design.md:125-135`).

The proposed wire shape (field numbers `<TBD by proto writer>` — compass-repo
owns final tag allocation; oneof tags are coordinated per the spawn/despawn
discipline, `compass-agent-spawn-despawn/design.md:447-448`):

```proto
// agent_gateway.proto — agent -> Runner, unary, beside Comms/Forge/Lifecycle.
// Named for the DL-070 board projection it mutates. Same envelope-sharing
// rationale as CommsCall* (agent_gateway.proto:22-29), extended by the
// ForgeCall*/LifecycleCall* precedents:
// the SAME messages ride the Runner->Server relay leg verbatim.
service AgentGateway {
  // … existing Comms / Forge / Lifecycle / Publish / PostConversationFrame / Control …
  rpc Board(BoardCallRequest) returns (BoardCallResult);
}

message BoardCallRequest {
  string call_id = 1;                       // agent-minted correlation id (SDK toolCallId)
  oneof call {
    SetIssueStateRequest set_issue_state = <TBD>;
  }
}

// Set an issue's canonical lifecycle state. Carries the FULL frozen
// UpdateIssueState semantics (compass-issue-model/design.md:474-511),
// re-homed: any of the eight real states is a legal target (any-to-any;
// DL-033's arrows are normative flow, not server-enforced), UNSPECIFIED is
// invalid_argument, a target equal to current state is an idempotent no-op
// returning current truth, ARCHIVED is just a target state (DL-091's
// archive-is-a-transition ruling survives on the new surface).
message SetIssueStateRequest {
  string issue_id = <TBD>;                  // the Compass-local id (Issue.id)
  IssueState state = <TBD>;                 // target; UNSPECIFIED -> in-band invalid_argument
}
message SetIssueStateResponse {
  Issue issue = <TBD>;                      // post-transition truth (unchanged on a no-op)
}

message BoardCallResult {
  string call_id = 1;
  oneof result {
    SetIssueStateResponse set_issue_state = <TBD>;
    BoardCallError error = <TBD>;           // in-band tool error, same shape as
  }                                         // CommsCallError/LifecycleCallError
}
message BoardCallError {
  string code = 1;                          // short stable token ("not_found", "invalid_argument")
  string message = 2;
}

// runner.proto — Runner -> Server, unary, sibling to RelayCommsCall /
// RelayForgeCall / RelayLifecycleCall (DL-049 convention). The Runner is a
// pure forwarder and asserts NO account; the Server resolves
// session_id -> account from its own Provision-originated binding.
rpc RelayBoardCall(RelayBoardCallRequest) returns (RelayBoardCallResponse);
message RelayBoardCallRequest {
  string session_id = <TBD>;                // the session the Runner structurally owns
  BoardCallRequest call = <TBD>;            // the agent's request verbatim
}
message RelayBoardCallResponse {
  BoardCallResult result = <TBD>;
}
```

Two cross-file notes, both with settled precedent:

- `SetIssueStateRequest`/`SetIssueStateResponse` reference the **public**
  canonical `IssueState`/`Issue` types from `compass.proto` inside an
  internal file. That import shape is already ratified for the `ForgeCall*`
  result arms: DL-092 retyped them "to the canonical `compass.v1` types"
  (`DECISIONS.md:136`), and the ownership-layer amendment settled the gen
  mechanics — "The TS internal lane regenerates the imported public types
  byte-identically via `--include-imports`, the Go lane M-redirects them to
  the public `go/gen` package — and the gen-fence must keep the canonical
  symbols unfenced"
  (`compass-server-ownership-layer-amendment/design.md:144-147`).
- The server-side transition semantics are **unchanged from the frozen
  record**: the compare-and-transition ("read current state, apply the
  target (rejecting only `UNSPECIFIED`), commit the new canonical state to
  Postgres, then record+publish the result", with read-and-validate inside
  the serialized transition, `compass-issue-model/design.md:513-521`) and
  the server-side tracker mirror on real transitions ("a real promote (state
  actually changed) writes through the tracker seam … mapping through the
  user's `TrackerStatusMapping`", `:543-546`; the ARCHIVED
  no-tracker-status carve-out, `:558-563`). Only the CALLER changes: the
  executor is invoked by the relay's `BoardCaller` seam instead of a
  `CompassService` handler.

### (b) The tracker write: native tracker status ingested into the projection

Matt's ruling makes the tracker the **user's** state-write surface, which
inverts the frozen direction. DL-032 said "Compass state is canonical; the
tracker is a projection of it" (`DECISIONS.md:144`), and the issue-model
record leaned on the one-way arrow: "the tracker is a projection OF this
state per DL-032, and cannot be inverted to recover it"
(`compass-issue-model/design.md:404-406`). Under this amendment the
**server projection remains the canon** (DL-070 unchanged), but the tracker
becomes a second lifecycle-transition PRODUCER feeding it:

- **Trigger**: the DL-053 machinery, reused as-is — forge/tracker
  subscriptions are already "change-detected by conditional polling in v1
  (webhooks are an additive accelerator)" (`DECISIONS.md:76`). The
  ingestion poll that today keeps forge-fields current gains the tracker's
  native status field as a watched input. No new transport.
- **Mapping**: the existing `TrackerStatusMapping` runs in reverse —
  `fromTrackerStatus` already exists as a contract
  (`tracker.ts:69-84`, preserved by the frozen record with the
  seven-working-states domain, `compass-issue-model/design.md:639-646`).
  A tracker status maps into one of the SEVEN working states. `ARCHIVED` has
  no tracker status of its own, but it is **not agent-only** (Matt,
  2026-08-04): a Done issue auto-transitions to `ARCHIVED` after 24h (see
  §(d)), and a user who REOPENS a linked tracker issue transitions it out of
  `ARCHIVED` back to a working state through normal ingestion — un-archive is
  a legal user action.
- **Echo suppression is NOT free — it needs a tracker-status-space rule**
  (Resolved decision 1, RULED by Matt). Both producers funnel into the same
  serialized compare-and-transition, but the frozen state-space idempotency
  rule alone is insufficient: the default Linear map is non-injective
  (`apps/ui/src/tracker.ts:44-60` — `queued`/`todo` both → `"Todo"`,
  `blocked`/`in_progress` both → `"In Progress"`), so an agent's own
  mirrored status polled back round-trips to the WRONG Compass state and
  fires a real reverting transition. The ruled rule: a poll observation is a
  no-op when the observed status equals `toTracker(current state)` —
  suppression in tracker-status space, not Compass-state space — and a
  **tracker-sourced transition never mirrors back out** (`source==tracker`).
  The outbound Compass→tracker mirror on AGENT transitions **survives** (the
  user watches the tracker, so agent moves must appear there); the sync is
  bidirectional with the server projection as the single serialization point,
  plus a recency guard against stale-poll lost updates.
- **Distinct from forge-state**: the passive `forge_state` badge rule is
  untouched — "a forge event never auto-advances Compass state"
  (`compass-issue-model/design.md:469-471`) still holds for the FORGE
  (`open`/`closed`/`merged`). The tracker's native STATUS is a different
  input with an explicit user-configured mapping; only it drives
  transitions.

### (c) The UI: read-only for state

The Compass UI drops every issue-state write affordance: Backlog promote
(`store.ts` `promoteToTodo` :1847-1860 + its `BacklogView.tsx:43` button),
drag-to-column, and Done/archive (`store.ts` `archiveIssue` :1862-1875 + its
`DoneView.tsx:79-83` button) — the guards the frozen record itemized as
preserved (`compass-issue-model/design.md:87-90`, `:498-511`) are removed
rather than re-homed, including the `seam.updateIssueStatus` mirror call
inside promote (`store.ts:1859`). The board renders lifecycle from the frozen
read path: the canonical `Issue` rides "`SubscribeEventsResponse` as a new
oneof variant … `Issue issue = 16;` // the canonical board unit, pushed on
every change" (`compass-issue-model/design.md:284-297`), shipping in RIG-1727
S1a (PR #145). The `UpdateIssueState` RPC is **dropped from the contract
before it is ever built** — grep of `compass/proto/compass/v1/` this run finds
no `UpdateIssueState` RPC, so there is no wire build, no migration, and
nothing to delete server-side beyond not building the planned handler (the
`Issue`/`IssueState` READ types shipped in S1a and stay). The Settings
tracker-mapping editor (`components/SettingsView.tsx`: `STATE_ROWS` :22-45,
component :76) survives — it now configures the ingestion mapping's both
directions.

### (d) Auto-archive and the no-unlinked-issues invariant

Two rulings (Matt, 2026-08-04) that shape the state machine and the issue
population:

- **Auto Done→ARCHIVED after 24h.** A Done issue auto-transitions to
  `ARCHIVED` 24h after it entered Done — a server-side timed transition, not
  an agent op and not a UI action. This is the PRIMARY archive path (the
  earlier "archive is agent-only" model is dropped); it runs as a sweep in
  the same server package as the transition executor, feeding the SAME
  executor as `source=auto` so record+publish and the outbound mirror
  behave uniformly. Re-entering Done (a user reopen then re-close, or an
  agent transition) restarts the 24h clock.
- **All issues flow from the tracker — no Compass-native / unlinked issues
  for the MVP.** Every board issue has a `TrackerRef`; there are no
  local-only issues without a tracker handle. This closes the
  unlinked-issue human-reachability hole by construction: the user always
  has a tracker write surface for every issue. (Compass-native issue
  creation is explicitly deferred — it "adds a lot of extra surface to an
  MVP"; a later record may add it.)

### What is explicitly NOT changing

- The `Issue`/`PullRequest` canonical type family, the `IssueState` enum,
  and the `SubscribeEventsResponse.issue` READ path (DL-067, DL-069,
  RIG-1727 S1a / PR #145) — untouched.
- DL-033's seven working states + DL-091's terminal `ARCHIVED` as the state
  MODEL (`DECISIONS.md:145`, `:153`) — untouched; only the write surface
  moves.
- DL-070's server-authoritative projection — strengthened, not amended: it
  gains a second input, it does not move.
- DL-031 ("UI shell is board-primary", `DECISIONS.md:143`) — a LAYOUT
  decision, not a write-path decision; explicitly not superseded here.

## Alternatives considered

1. **Keep `UpdateIssueState` on `CompassService`, gated (admin-only or
   feature-flagged off in the UI).** Rejected: Matt ruled the UI should not
   write state at all; keeping a public UI-door mutation RPC that nothing
   may call is dead contract surface, and the AdminGate would carry a
   classifier row for a procedure with no legitimate caller. Since no wire
   build shipped, dropping it pre-build is free.
2. **Dual write path (UI and agent both write).** Rejected: it is exactly
   the model Matt reversed ("i'm not sure we should even have the UI move
   cards?"). Two human-facing write surfaces (Compass UI + tracker) would
   also create a conflict surface between two user intents; the ruling
   gives the user ONE write surface (the tracker) and the agents another,
   with the projection serializing both.
3. **Fold the agent op into the RIG-1731 `ForgeCall*` family vs a sibling
   family.** Weighed both:
   - *Fold into `ForgeCall`*: cheapest wire delta — one new oneof variant on
     `ForgeCallRequest`/`ForgeCallResult`, no new RPCs, the gen-fence
     already covers `ForgeCall`. But it crosses a server seam: `ForgeCaller`
     dispatches to the forge Provider adapter
     (`compass-server-ownership-layer/design.md:514-516`), while a state
     write executes against the projection + Postgres — the forge is only a
     mirror target. Folding forces the forge seam to grow a store/projection
     dependency, and misnames the tool surface (`forge_set_issue_state` for
     an op that is not a forge write).
   - *Sibling family (`BoardCall*`)*: mirrors the DL-076 precedent — when
     spawn/despawn arrived, it became its OWN `LifecycleCall*` family beside
     `CommsCall*` rather than a variant of it ("a sibling `LifecycleCall*`
     family … mirroring DL-049", `DECISIONS.md:133`). Domain-pure families
     per subsystem is the established convention; the proto cost (~6
     messages, 2 RPCs) has an exact template to copy.
   - *Fold into the existing `LifecycleCall*` family*: "lifecycle" is the
     domain word the issue-state model uses, so this looks like the natural
     home — but it fails for the same seam reason: `LifecycleCaller`
     dispatches to spawn/despawn orchestration
     (`compass-agent-spawn-despawn/design.md:653-662`), not the board store,
     so a state write would cross into it exactly as the `ForgeCall` fold
     does. Named-and-rejected so the obvious option is not relitigated.
   - *The criterion being applied*: **one call family per server DISPATCH
     SEAM** — the caller interface that executes the op
     (`CommsCaller`/`ForgeCaller`/`LifecycleCaller`/the new `BoardCaller`),
     not per surface word. A state write executes against the projection +
     store, a distinct seam, so it earns its own family. Stating the rule
     bounds family proliferation: a new family is justified only by a new
     dispatch seam, not by a new verb.
   - **Recommended: sibling `BoardCall*` family** (named for the DL-070
     board projection it mutates). Load-bearing — flagged in Open
     Questions for Matt's freeze-time confirmation.

## Plan

### Global Constraints

- **The egress seal is inviolable.** The agent holds no server token and no
  network path to the Server; its only door is the per-container
  `AgentGateway` socket (`agent_gateway.proto:8-10`,
  `compass-agent-spawn-despawn/design.md:132-135`). The relay IS the
  agent-facing RPC edge; no task may add a public agent-callable state RPC.
- **The read path is frozen.** `Issue`/`PullRequest`/`IssueState` and
  `SubscribeEventsResponse.issue = 16` (RIG-1727 S1a, PR #145) are consumed
  as-is; no task edits them.
- **Additive proto only.** New RPCs, messages, and oneof variants at fresh
  tags behind the buf breaking gate; `UpdateIssueState` is never authored
  (nothing to remove — no wire build exists, grep-verified this run).
  Final field numbers and oneof tags are allocated by the compass-repo
  proto writer (coordinated, not read off the live proto —
  `compass-agent-spawn-despawn/design.md:447-448`).
- **Gen-fence discipline.** `BoardCall*`/`RelayBoardCall*`/`SetIssueState*`
  are internal-only symbols: extend the RIG-1267 gen-fence grep
  (`proto/moon.yml:158`) with the unanchored
  `BoardCall|RelayBoardCall|SetIssueState` family, matching the
  `LifecycleCall|SpawnPeer|DespawnPeer|RelayLifecycleCall` entries already
  present (`proto/moon.yml:158`). The canonical `Issue`/`IssueState` stay
  UNFENCED (public symbols that legitimately generate into public trees,
  `compass-server-ownership-layer-amendment/design.md:117`).
- **No AdminGate classifier row is needed — verified.** The classifier's
  exhaustiveness gate covers ONLY `compass.proto` + `comms.proto` services
  (`gatedFileDescriptors` returns `File_compass_v1_compass_proto` and
  `File_compass_v1_comms_proto`,
  `go/internal/auth/classify_exhaustive_test.go:38-43`); `AgentGateway` and
  `RunnerService` procedures are not network-door-gated. Since this
  amendment adds NO `CompassService`/`CommsService` RPC, no classifier row
  exists to write. (Had `UpdateIssueState` been built, it would have needed
  one — dropping it also drops that obligation.)
- **Transition semantics are carried, not redesigned.** Any-to-any targets,
  `UNSPECIFIED` → invalid_argument, idempotent no-op at target, ARCHIVED as
  a plain target, serialized compare-and-transition, Postgres commit then
  record+publish, tracker mirror on real transitions with the ARCHIVED
  carve-out — all verbatim from `compass-issue-model/design.md:474-563`.
- **Errors are in-band.** A state-write failure is a `BoardCallError` tool
  error the agent renders, never a transport teardown — the
  `CommsCallError`/`LifecycleCallError` split
  (`compass-agent-spawn-despawn/design.md:629-632`).
- **This PR is docs-only.** T1 is this PR; T2–T5 are downstream execution,
  enumerated at contract level per owning lane, explicitly NOT this PR.

### T1 — this record + ledger delta (THIS PR; parent agent executes the flip)

Author this record. Ledger intent (parent applies to `DECISIONS.md` in this
PR, per the same-PR-flip rule):

- **Add DL-129** (id verified as next free — max ledger id was DL-128): *The issue-state write model is
  agent-primary: the Compass UI is read-only for lifecycle state; agents
  set state via a sibling `BoardCall*` family on the `AgentGateway` socket
  relayed by `RelayBoardCall` (DL-049/DL-076 convention), and users set
  state in the external tracker, whose native status is ingested into the
  DL-070 server projection through the reverse `TrackerStatusMapping`
  (DL-053 polling; echo-suppressed in tracker-status space, tracker-sourced
  transitions never mirror back, stale polls dropped by a recency guard).
  All issues flow from the tracker (no Compass-native/unlinked issues in the
  MVP). `ARCHIVED` is not agent-only: a Done issue auto-transitions to
  `ARCHIVED` after 24h, and a user reopen un-archives via ingestion. MVP
  write-authz is single-trust-domain (any agent under the wave account may
  transition any issue; a hierarchical scope model is a filed follow-up).
  `UpdateIssueState` on `CompassService` is never built (no wire build
  shipped). DL-033's state model, DL-070's server-authoritative projection,
  and the `SubscribeEventsResponse.issue` read path are unchanged.*
  Status `Active (Matt, 2026-08-04)`, Record → this record §Approach.
- **Flip DL-091** → `Superseded by DL-129 (Matt, 2026-08-04)`. Its
  archive-is-a-lifecycle-transition ruling SURVIVES in DL-129's text; what
  dies is the `UpdateIssueState`-on-`CompassService` mechanism its row
  names (`DECISIONS.md:154`).
- **Flip DL-032** → `Superseded by DL-129 (Matt, 2026-08-04)`. "the tracker
  is a projection of it" (`DECISIONS.md:144`) is no longer the whole truth:
  the tracker is now also a state WRITE source. Compass (server projection)
  canon survives in DL-129's text. Decision cells stay untouched (immutable
  per `DECISIONS.md:28-29`).
- **Leave Active, unchanged**: DL-070 (`:153`, the projection — gains an
  input, doesn't move), DL-033 (`:145`, the state model), DL-067 (`:150`,
  the board unit), DL-049 (`:124`) and DL-076 (`:133`) (the mirrored relay
  patterns), and DL-031 (`:143` — a board-primary LAYOUT ruling, not a
  write-path ruling; explicitly not touched).

`Interfaces:` consumes the frozen records cited above; produces
`docs/designs/product/compass-agent-primary-lifecycle/design.md` + the
`DECISIONS.md` delta. Gate: `design-ledger-gate`.

### T2 — compass-repo: the `BoardCall` proto family + regen (NOT this PR)

- `agent_gateway.proto`: `rpc Board(BoardCallRequest) returns
  (BoardCallResult)` on `AgentGateway`; messages `BoardCallRequest`
  (`call_id`, oneof `set_issue_state`), `SetIssueStateRequest{issue_id,
  IssueState state}`, `SetIssueStateResponse{Issue issue}`,
  `BoardCallResult` (`call_id`, oneof `set_issue_state`/`error`),
  `BoardCallError{code, message}` — §Approach (a), verbatim, numbers
  allocated here. Imports `compass.proto` for `Issue`/`IssueState` (the
  DL-092 cross-file precedent).
- `runner.proto`: `rpc RelayBoardCall(RelayBoardCallRequest) returns
  (RelayBoardCallResponse)`; `RelayBoardCallRequest{session_id,
  BoardCallRequest call}`, `RelayBoardCallResponse{BoardCallResult
  result}`.
- Extend the gen-fence grep (`proto/moon.yml:158`) with
  `BoardCall|RelayBoardCall|SetIssueState`; regen all three lanes; do NOT
  fence `Issue`/`IssueState`.
- Do NOT author `UpdateIssueState*` on `compass.proto` (drop-before-build);
  consequently NO AdminGate classifier row (Global Constraints).
- Agent TS (also this repo): gateway `Board` endpoint beside
  `Comms`/`Lifecycle`; `BoardBroker` on the transport seam (the
  `LifecycleBroker` shape,
  `compass-agent-spawn-despawn/design.md:392-398`); one tool
  `issues_set_state` (`approval: "write"`; params `issue_id`, `state`),
  in-band `BoardCallError` rendered as a thrown tool failure.

`Interfaces:` the proto messages of §Approach (a); generated
`compassv1internal.RelayBoardCallRequest/Response`, TS `BoardCallRequest`
et al. in `packages/compass-agent/src/gen`; `moon run compass-proto:ci`
green (lint, breaking additive-clean, drift, gen-fence proving no leak and
canonical types unfenced).

### T3 — compass-server: relay handler + transition executor + tracker ingestion (NOT this PR)

- `Hub.RelayBoardCall` in a new `relay_board.go` mirroring
  `relay_comms.go`/`relay_lifecycle.go`: caller seam wired
  (`CodeUnavailable` otherwise) → `accountForSession(session_id)`
  fail-closed `CodeNotFound` → delegate to an injected `BoardCaller` seam
  (`type BoardCaller interface { SetIssueStateAsAccount(ctx, caller
  store.AccountID, req *compassv1internal.SetIssueStateRequest)
  (*compassv1internal.SetIssueStateResponse, error) }` — the
  `CommsCaller`/`ForgeCaller`/`LifecycleCaller` injection pattern,
  `compass-agent-spawn-despawn/design.md:653-662`). Authz/tool errors
  in-band as `BoardCallError`; transport failures are Connect errors.
- The transition executor: the frozen compare-and-transition
  (`compass-issue-model/design.md:513-521`) implemented once in the server
  package beside the projection, invoked by `BoardCaller` — Postgres
  commit, record+publish, outbound tracker mirror on real transitions
  (ARCHIVED has no tracker status, so it is elided from the outbound mirror,
  `:558-563`). It carries the transition SOURCE **and actor** (see
  Interfaces) so record+publish attributes which agent or
  tracker account moved the card. Per Resolved decision 2 (MVP scope), the
  caller `AccountID` is recorded for attribution but NO scope rejection ships
  in the MVP (single-trust-domain); the signature carries it so the later
  hierarchical scope check has its input without a migration.
- Tracker-status ingestion: the DL-053 poll watches the tracker's native
  status; on change, map through the reverse `TrackerStatusMapping`
  (`fromTrackerStatus` contract, seven working states) and feed the SAME
  transition executor as an ingestion-sourced transition (source=tracker,
  no caller account). **Echo suppression per Resolved decision 1** —
  because the shipped Linear map is non-injective
  (`apps/ui/src/tracker.ts:44-60`: `queued`/`todo` both →`Todo`,
  `blocked`/`in_progress` both →`In Progress`), suppress in tracker-status
  space (no-op when observed status == `toTracker(current state)`), never
  mirror a tracker-sourced transition back out, and drop a polled
  observation older than the issue's last transition commit.
- Do NOT build the planned `UpdateIssueState` `CompassService` handler.

`Interfaces:` `func (h *Hub) RelayBoardCall(ctx,
*compassv1internal.RelayBoardCallRequest)
(*compassv1internal.RelayBoardCallResponse, error)`; the `BoardCaller`
interface above; the executor `SetIssueState(ctx, caller store.AccountID,
issueID string, target IssueState, source TransitionSource) (Issue, error)`
where `TransitionSource` carries `kind` (agent | tracker | auto) + optional
actor `AccountID` (attribution now; the input for the later hierarchical
scope check, Resolved decision 2), consumed by both
producers. Red-first security tests per the `relay_comms_test.go` pattern
(unbound session `CodeNotFound`; nil caller `CodeUnavailable`; resolved
caller — never request-asserted — reaches a fake `BoardCaller`).

### T4 — compass-ui: read-only board (NOT this PR)

- Remove the promote affordance and its store guard (`store.ts`
  `promoteToTodo` :1847-1860 + `BacklogView.tsx:43`), the archive affordance
  and guard (`archiveIssue` :1862-1875 + `DoneView.tsx:79-83`), the
  `seam.updateIssueStatus` mirror in promote (`store.ts:1859`), and any
  drag-to-column state write; no replacement controls.
- The board renders lifecycle exclusively from the streamed canonical
  `Issue` (`SubscribeEventsResponse.issue`); lanes/partitions
  (`ACTIVE_STATES`, `boardAgents`, `cellItems`, `laneTotal`) unchanged as
  pure reads.
- The Settings tracker-mapping editor (`components/SettingsView.tsx`:
  `STATE_ROWS` :22-45, component :76) survives,
  reworded as the bidirectional mapping config; seven-working-states domain
  unchanged.

`Interfaces:` consumes the generated canonical `Issue` from
`@compass/client` only; produces no mutation calls (grep gate: no
`UpdateIssueState`/state-mutation RPC reference anywhere in `apps/ui`).

### T5 — compass-server: auto-archive Done→ARCHIVED after 24h (NOT this PR)

- A server-side timed sweep in the same package as the transition executor:
  an issue in `DONE` for ≥24h is transitioned to `ARCHIVED` via the SAME
  executor with `source=auto` (no caller account), so record+publish and the
  suppression rules apply uniformly. Re-entering `DONE` restarts the clock
  (track the Done-entry timestamp on the projection row).
- ARCHIVED has no tracker status, so an auto-archive transition is elided
  from the outbound mirror (Resolved decision 1 / §Approach (b)); a later
  user reopen on the tracker un-archives through ingestion.

`Interfaces:` a periodic sweep (interval a server config, not wire) invoking
`SetIssueState(ctx, systemCaller, issueID, ISSUE_STATE_ARCHIVED,
{kind: auto})`; the Done-entry timestamp read from the projection. Test:
a Done issue younger than the window is untouched; at/after the window it
archives exactly once (idempotent no-op on a second sweep).

## Tasks

- [x] T1 — this record authored; DL-129 appended; DL-091 + DL-032 flipped to
      `Superseded by DL-129`; DL-070/DL-033/DL-067/DL-049/DL-076/DL-031 left
      untouched (THIS PR; parent executes the ledger delta)
- [ ] T2 — compass-repo: `BoardCall*` family on `agent_gateway.proto`,
      `RelayBoardCall` on `runner.proto`, gen-fence extension, three-lane
      regen, `issues_set_state` agent tool; NO `UpdateIssueState`, NO
      classifier row (downstream PR)
- [ ] T3 — compass-server: `Hub.RelayBoardCall` + `BoardCaller` seam +
      single transition executor + tracker-status ingestion into the DL-070
      projection; planned `UpdateIssueState` handler not built
      (downstream PR)
- [ ] T4 — compass-ui: remove promote/drag/archive write affordances;
      board reads state off `SubscribeEventsResponse.issue`; Settings
      mapping editor retained (downstream PR)
- [ ] T5 — compass-server: auto-archive sweep (Done→ARCHIVED after 24h) via
      the shared executor with `source=auto` (downstream PR)

## Resolved decisions (Matt, 2026-08-04)

The load-bearing forks were batched to Matt and ruled; folded here as
decisions the record is now designed against.

1. **Echo suppression + two-producer conflict rule — RULED: tracker-status-space
   suppression + no mirror on tracker-sourced + recency guard.** A poll
   observation is a no-op when the observed status equals
   `toTracker(current state)` (suppress in tracker-status space, not
   Compass-state space); a tracker-sourced transition never mirrors back out
   (`source==tracker`); a recency guard drops a polled observation whose fetch
   predates the issue's last transition commit. Genuine concurrent intents
   stay last-writer-wins at the serialized executor. This is required — the
   shipped Linear map is non-injective (`apps/ui/src/tracker.ts:44-60`:
   `queued`/`todo` both → `"Todo"`, `blocked`/`in_progress` both → `"In
   Progress"`), so state-space idempotency alone would let an agent's own
   mirror echo revert its write. Realized in §Approach (b).
2. **Issue-write authorization scope — RULED: single-trust-domain MVP scope
   now, hierarchy later.** For the MVP, any agent under the wave's shared
   Compass account may transition any issue (single-trust-domain, cf. DL-063
   Garage acceptance). The executor still carries the caller `AccountID` (T3
   signature) so attribution is recorded and the later scope check has its
   input, but no scope rejection ships in the MVP. **Follow-up (filed):** a
   hierarchical board-authz model — an issue assigned to a leaf agent is
   transitionable by that leaf and each report up the chain to the
   root/supervisor — together with how agents pull new issues to work on and
   how a newly created issue is owned immediately (so it never falls out of
   the tree). That is its own design record (the ownership/assignment tree),
   not this amendment.
3. **Human reachability — RULED: no unlinked issues; users may un-archive;
   auto-archive Done after 24h.** All issues flow from the tracker (no
   Compass-native/unlinked issues for the MVP), so every issue has a tracker
   write surface by construction — the unlinked-issue wedge cannot occur.
   `ARCHIVED` is NOT agent-only: a user reopening a linked tracker issue
   un-archives it through normal ingestion, and a Done issue auto-transitions
   to `ARCHIVED` after 24h (the primary archive path). Realized in
   §Approach (b) + §Approach (d).
4. **Agent write-op family shape — RULED: sibling `BoardCall*` family** (not
   folded into `ForgeCall*`). Criterion: one call family per server dispatch
   seam (`BoardCaller` executes against the projection/store, distinct from
   the forge Provider seam). Realized in §Approach (a) + Alternatives (3).
5. **Tracker→Compass ingestion trigger — reuse the DL-053 conditional poll**
   (`DECISIONS.md:76`); the tracker's native status becomes one more watched
   input, webhooks stay the additive accelerator. No override from Matt; no
   new transport. Realized in §Approach (b).

## Open Questions

1. **[Load-bearing — BLOCKS this record's downstream forge alignment; raised
   with Matt, ruling pending] The forge/tracker access model: store-first
   read-through vs the frozen stateless relay.** Matt (2026-08-04): "the agent
   calls a tool [that] first hits the Compass store, where info can be returned
   immediately if the PR or issue is already tracked, and the Server
   continuously polls those … to keep the state up to date … If they call the
   tool and it isn't tracked by Compass already, Compass goes and fetches it,
   tracks it into the store, and then returns that." This is a **store-first
   read-through** model. The frozen `compass-server-ownership-layer` record
   (RIG-1728) specified the opposite for forge reads — a **stateless
   pass-through relay**: "nothing is stored, nothing is resolved, and no
   coordinate can drift" (`compass-server-ownership-layer/design.md:352-354`).
   This does not change THIS amendment's issue-state WRITE path (the BoardCall
   family + projection are unaffected), but it reshapes RIG-1731 (A1
   forge-carrier proto) and the RIG-1728 forge read path. **Impact:** the A1
   proto's `ForgeCall*` read arms may need to become store-reads with a
   fetch-on-miss, and the naming/shape may change. Parked here because it is
   Matt's ruling to make on the frozen forge contract; A1 authoring holds on
   it. (Non-blocking for this amendment's merge — the write model stands
   regardless.)
2. **[Non-load-bearing, deferrable] Specific tracker adapters for status
   ingestion.** Linear-first per DL-051's provider ordering; adapter details
   are Provider-internal and do not shape this contract.
