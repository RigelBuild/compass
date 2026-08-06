# Compass Issue model

Status: Active

Tracker: SEA (the spawning agent fills the issue id when the PR opens).

Ledger: this record's PR appends DL-067 … DL-070 plus DL-091 to
`docs/designs/product/DECISIONS.md` in the same diff (see §Ledger delta) and
supersedes no existing row, so it satisfies the ledger gate's touch-coupling
leg directly — no `Ledger-impact:` escape hatch is needed in the PR body.

Extends `compass-server-ownership-layer/design.md` (PR #995, frozen) — it never
rewrites it.

## Problem / Intent

`Workstream` is a UI-only wart. It exists nowhere but `apps/ui`: no proto
message, no server table, no daemon concept — just a TypeScript interface in
`compass/apps/ui/src/stub-data.ts:84-109` whose forge linkage is a bare tracker
string (`issue: string`, stub-data.ts:87) and whose PR is a hand-rolled local
shape (`PullRequest`, stub-data.ts:61-71). Matt ruled (2026-07-29, refined
2026-07-30): remove the `workstream` concept from the Compass UI **entirely** —
not demote, remove; no `workstream` type or vocabulary survives anywhere in
`apps/ui` — and replace it with a Compass **`Issue` / `PullRequest`** model: "a
Compass Issue or PullRequest is the underlying forge's one, PLUS all of the
Compass-specific machinery, like our own state tracking, assignee/author agent
attribution, etc." Attribution "should be provided automatically on the issues
and pull requests" — the UI consumes it, it never stamps it.

The 2026-07-31 ruling fixed the mechanism (FINAL): "the whole system should
work with the Compass Issue and PullRequest types, that have the agent
author/owner fields, etc. we translate the raw forge info into those types as
soon as we get them" — combined with the earlier ruling that "Issue and
PullRequest should be Compass owned, not the forge issue; we don't expose the
non-compass'ed types." So the model is one canonical type family, three
layers deep:

- **Proto:** `compass.v1` gains the canonical **`Issue`** and **`PullRequest`**
  messages — Compass-owned, carrying the forge artifact's fields PLUS the agent
  author/owner attribution PLUS the Compass machinery. There is no second
  family: the raw forge shape is **not a proto/wire type at all**.
- **Server:** the server **translates raw forge data into the canonical types
  at ingestion** — as soon as it gets them. The raw forge payload (the GitHub/
  Linear API response) is server-internal Go data that exists only inside the
  ingestion translation; it is never exposed, not to the UI and not as a
  separate `compass.v1` message.
- **UI:** consumes ONLY the canonical types, via the generated
  `@compass/client`. It never sees a raw forge artifact, never embeds forge
  shapes, never converts wire numerics — and the `workstream` removal still
  happens: the UI's board type becomes the generated canonical `Issue`,
  replacing the local `Workstream` shape.

The attribution semantics the server implements at ingestion are already
frozen: PR #995 (`compass-server-ownership-layer/design.md`, Status: Active,
design.md:3) defines the owner-header discipline — the `compass:owner` header
stamped at one chokepoint on write, parsed and stripped from the body on read,
with the parsed result treated as untrusted display metadata (DL-050; #995
design.md:377-383). But #995's forge reads carry only forge truth — its issue
`state` is `"open" | "closed"` (#995 design.md:366); there is **no board
lifecycle, no priority, no assignee, no tracker-state projection** — exactly
the Compass machinery the canonical types add. And #995 is **entirely unbuilt
in code today** (its own preconditions record "zero existing forge
integration", #995 design.md:194-195; no forge message exists under
`proto/compass/v1/` — verified by grep this run), so this record designs
against #995's frozen attribution **semantics**, with a fixture seeding
canonical-shaped objects until the server ingestion and RPCs land.

This record is that delta: the canonical `compass.v1` `Issue`/`PullRequest`
messages, the server projection that computes and streams them, the write path
that mutates them, and the UI consumption plus the rename of every
`workstream`-derived name. It **extends** frozen #995 — it never rewrites it —
and it composes with DL-055, which already anticipated this surface: "agent
ownership queries and **the Bridge boards** are local index scans over
Server-recorded truth" (DECISIONS.md:76). Per DL-032 (DECISIONS.md:107, Matt
2026-07-10), Compass state is canonical and the tracker is a projection — this
record moves that canonical state to where it belongs, the server; DL-033's
seven working states are unchanged, extended by a terminal `ARCHIVED` state
(DL-091) that folds the former archive marker into the lifecycle.

## Approach

**The model: one canonical, Compass-owned `compass.v1` type family — `Issue`
and `PullRequest`, the forge artifact's fields plus agent attribution plus the
Compass machinery — is the board unit; the server translates raw forge data
into it at ingestion, and the UI consumes it and nothing rawer.** The board
renders off canonical Issue objects (this is "board off issues"); the rows
stay agent swimlanes (`boardAgents`, board.ts:45-52); Backlog/promote
(store.ts:898-911), Done/archive (store.ts:916-924), the tracker projection
(tracker.ts:38-58, 69-84) and the Settings state-mapping surface
(SettingsView.tsx:17-25 — the mockup Matt referenced) are all **preserved** —
no functionality regression. The UI-side change is a rename (`Workstream`→the
canonical `Issue` type, and every derived name) plus a source change: the
board type is no longer a local interface but the generated proto type, seeded
by the fixture until the server streams it. Lifecycle writes move server-side
through a `CompassService` mutation surface (§The write path).

### The Compass Issue and PullRequest types

`proto/compass/v1/compass.proto` (package `compass.v1`, service
`CompassService`, compass.proto:11-14) gains the canonical messages **`Issue`**
and **`PullRequest`**, plus the lifecycle enum **`IssueState`** and small
carrier messages. The plain names are free and correct: there is no coexisting
forge-proxy family to disambiguate against — the raw forge shape is not a wire
type, so the Compass-owned types simply ARE `compass.v1`'s `Issue` and
`PullRequest`. (PR #995's earlier sketch of forge-proxy proto messages under
these names in `agent_gateway.proto` — #995 design.md:361-372, :403-417,
:1625 — is superseded by this model and reconciled in a sibling amendment to
that record; this record does not depend on any forge proto message existing.)

Field-for-field, what the canonical types carry and where each field's truth
lives (proto sketch — the proto slice below owns the final field numbering):

```proto
// The Compass issue lifecycle, server-owned. Follows the existing enum
// convention (prefix + UNSPECIFIED zero value, compass.proto:169-180).
// DL-033's seven working states plus a terminal ARCHIVED (DL-091): archiving
// is a lifecycle transition, not a side-marker.
enum IssueState {
  ISSUE_STATE_UNSPECIFIED = 0;
  ISSUE_STATE_BACKLOG = 1;
  ISSUE_STATE_TODO = 2;
  ISSUE_STATE_QUEUED = 3;
  ISSUE_STATE_BLOCKED = 4;
  ISSUE_STATE_IN_PROGRESS = 5;
  ISSUE_STATE_IN_REVIEW = 6;
  ISSUE_STATE_DONE = 7;
  ISSUE_STATE_ARCHIVED = 8;  // terminal: past Done; dropped from the active
                             // board, listed in the Done view's Archived section
}

// The agent attribution parsed from the owner header at ingestion — Compass's
// own message, following the #995/DL-050 semantics: the header is stamped
// server-side on write and parsed+stripped on read, and the parsed result is
// UNTRUSTED DISPLAY METADATA. It must never reach an authz, routing, or
// ownership decision. It carries handles plus a server-set trust bit — no
// account or session identifiers.
message AgentAttribution {
  string agent_handle = 1;  // the agent handle CLAIMED by the header; not proof
  string owner_handle = 2;  // the owning USER's handle CLAIMED by the header
  bool verified = 3;        // set by the server's forge-login cross-check at
                            // ingestion (#995 OQ-1): true only when the artifact's
                            // forge author login equals Compass's own forge
                            // identity. The UI hedges the claim unless verified.
}

// Which forge (and which host, for self-hosted instances) an artifact lives
// on. Carried on both Issue and PullRequest so a user with multiple connected
// forges never sees two artifacts collide on `repo` alone (Matt, 2026-07-31).
enum ForgeProvider {
  FORGE_PROVIDER_UNSPECIFIED = 0;
  FORGE_PROVIDER_GITHUB = 1;
  FORGE_PROVIDER_GITLAB = 2;
  FORGE_PROVIDER_FORGEJO = 3;
  FORGE_PROVIDER_LINEAR = 4;  // DL-051's issues-only forge source: a
                             // Linear-origin Issue whose repo is the project key
}
message ForgeRef {
  ForgeProvider provider = 1;
  string host = 2;  // "github.com", "gitlab.com", or a self-hosted host
                    // like "git.acme.internal"; disambiguates two instances
                    // of the same provider. For a SaaS-only tracker-as-forge
                    // (Linear) it is the constant service host, "linear.app"
}

// The board unit: a Compass Issue — the forge issue's fields PLUS the Compass
// agent attribution PLUS the Compass machinery, translated from raw forge data
// at server ingestion (Matt's ruling, 2026-07-31). The raw forge shape is
// never a wire type.
message Issue {
  // Compass-local id: the stable server-side key the projection and the
  // mutation RPC address (§The write path). Every issue is forge-backed;
  // this id is a join key, not a "no forge yet" placeholder.
  string id = 1;

  // ── Forge fields (translated from the raw forge payload at ingestion) ──
  ForgeRef forge = 2;           // which forge + host — multi-forge disambiguation
  string repo = 3;              // "<owner>/<name>" on GitHub, project key on Linear
  uint32 number = 4;            // the forge issue number; narrowed server-side (see below)
  string title = 5;
  string body = 6;              // owner header STRIPPED at ingestion (DL-050)
  string forge_state = 7;       // "open" | "closed" — forge truth, NOT the lifecycle
  string url = 8;
  AgentAttribution agent = 9;   // the Compass agent attribution parsed from the owner
                                // header; UNSET for a non-Compass (human) author
  string forge_account = 10;    // the native forge account that authored the artifact
                                // (e.g. the GitHub login); always set
  repeated string labels = 11;

  // ── Compass machinery (Compass-owned; none of this is on the forge) ──
  IssueState state = 12;        // the canonical lifecycle (DL-032/DL-033 + ARCHIVED), server-authoritative
  string priority = 13;         // "urgent" | "high" | "medium" | "low" (was Priority, stub-data.ts:52) — string in v1, ingested from the tracker (§The server projection)
  string assignee = 14;         // agent account id working it; empty = unassigned
  string summary = 15;          // latest-activity line for the card
  string branch = 16;           // the working head branch NAME (may exist before any PR)
  repeated PullRequest prs = 17; // every PR opened for this issue; empty before the first
  TrackerRef tracker = 18;      // the tracker projection target (DL-032); unset when unlinked
}

// A Compass pull request: the forge PR's fields plus the Compass agent
// attribution plus this PR's diffstat plus the full review state (reviews,
// threads, comments) the right-sidebar PR pane shows.
message PullRequest {
  ForgeRef forge = 1;           // which forge + host — multi-forge disambiguation
  string repo = 2;
  uint32 number = 3;
  string title = 4;
  string forge_state = 5;       // "open" | "closed" | "merged"
  string url = 6;
  string head_ref = 7;
  string base_ref = 8;
  AgentAttribution agent = 9;   // Compass agent attribution; unset for a non-Compass author
  string forge_account = 10;    // the native forge account that opened the PR; always set
  bool draft = 11;
  ChangedStats changed = 12;    // THIS PR's diffstat (a diff is a PR fact, not an issue fact); unset for no diff
  ChecksSummary checks = 13;    // rolled-up CI state, translated at ingestion; unset for a PR with no CI
  repeated Review reviews = 14; // submitted reviews (approve/request-changes/comment), each flagged is_bot
  repeated ReviewThread threads = 15; // review threads with their comments and resolution
}

// The rolled-up CI + status-check state on a PR head — Compass-owned,
// populated by the ingestion translation from the forge's check runs.
message ChecksSummary {
  string head_sha = 1;
  string state = 2;             // "pending" | "success" | "failure" — the roll-up
  repeated Check checks = 3;
}
message Check {
  string name = 1;
  string state = 2;             // "queued" | "in_progress" | "success" | "failure" | "neutral" | "cancelled"
  string url = 3;
  bool required = 4;
}

// A PR diffstat (files/additions/deletions), carried on PullRequest — mirrors
// the fixture's `changed` (stub-data.ts:98); a forge/VCS fact translated at
// ingestion.
message ChangedStats {
  uint32 files = 1;
  uint32 additions = 2;
  uint32 deletions = 3;
}

// The linked tracker issue — the projection target (DL-032); the
// no-write-to-self elision (§The write path) keys off `kind`+`id`. Mirrors
// the fixture's TrackerRef (stub-data.ts:113-120).
message TrackerRef {
  string kind = 1;              // "linear" | "jira" | "github" — the tracker family
  string id = 2;               // the tracker's native issue id, e.g. "SEA-1042"
  string status = 3;           // the tracker's native status name in the user's org
  string url = 4;
}

// The full review state the right-sidebar PR pane shows — every submitted
// review (human and bot) plus the review threads and their comments, fetched
// from the FORGE and translated at ingestion like `checks` (supersedes the
// bot-only telemetry sketch; mirrors and widens the fixture's threads/reviews,
// stub-data.ts:67-70). The UI's "N/M threads resolved" derives from `threads`;
// the bot review chips derive from `reviews` filtered by `is_bot`. Wire
// contract: `reviews` is submission-ordered (ingestion appends, newest last),
// so a reviewer's CURRENT verdict is its last entry — a reviewer who requests
// changes then approves has both, and the chip derivation takes the
// latest-per-author (enforced by ingestion, tested in S1). The canonical
// `verdict` vocabulary is the forge's ("changes_requested"); the UI maps it to
// the existing chip key ("changes") at the chip site (S2), like the checks 6->3
// mapping.
message Review {
  string author = 1;    // the reviewer's forge account, or a Compass agent handle
  bool is_bot = 2;      // true when the reviewer is a bot (e.g. CodeRabbit, cubic)
  string verdict = 3;   // "approved" | "changes_requested" | "commented"
  string body = 4;      // the review's summary comment (may be empty)
}
message ReviewThread {
  string path = 1;      // the file the thread anchors to; empty for a PR-level thread
  bool resolved = 2;
  repeated Comment comments = 3;
}
message Comment {
  string author = 1;
  bool is_bot = 2;
  string body = 3;
}
```

The delivery surface is settled: the canonical `Issue` rides
**`SubscribeEventsResponse` as a new oneof variant**. `SubscribeEvents` is
"the sole push path from the server to the UI" (compass.proto:21-22), the
oneof explicitly reserves the slot ("Later milestones add board/agent/audit
payloads here; new variants are backward-compatible additions behind the buf
breaking gate", compass.proto:116-117), and the existing board projection
already fans onto this stream with a snapshot-query sibling
(projection.go:3-9) — so the canonical type gets snapshot-as-events on connect
plus live tailing with `since_seq` resubscribe semantics for free, and no
second push path exists to keep gap-free:

```proto
// Added to the SubscribeEventsResponse payload oneof (compass.proto:118-128):
//   Issue issue = 16;  // the canonical board unit, pushed on every change
```

Removal is out of scope for v1: the stream carries upserts, and a board never
shrinks on the wire in v1 — an archived issue drops off the ACTIVE surfaces
because its `state` is `ARCHIVED` (still listed in the Done view), but a
forge-deleted, transferred, or membership-dropped issue is not tombstoned. A
removed/tombstone variant is a backward-compatible additive to the oneof when
a later milestone needs it (compass.proto:116-117).

An honest note on the drift surface. With one type family there are no two
coexisting wire shapes to drift against each other: the only translation is
the server's ingestion code — raw forge JSON in, canonical proto out — which
is Go, unit-testable, and the single conversion point. What can still drift is
that translation against the forge's actual API responses (a renamed forge
field, a new check-run state), and that is caught where it belongs: in the
server's ingestion tests against recorded forge fixtures, not in a proto
review. S1's acceptance test additionally proves no raw forge shape appears in
any UI-facing response type, so accidental exposure is a red test, not a
review catch.

**One family end to end.** The agent-facing forge tools (#995's
`forge_get_issue`/`forge_list_issues`/`forge_get_pull_request`, #995
design.md:2302-2304) return this SAME canonical `Issue`/`PullRequest` — there
is no slimmer agent-only twin, which would be a second family. When an agent
fetches an artifact that is not board-tracked, the Compass-machinery fields
are simply unset (`state = ISSUE_STATE_UNSPECIFIED`, empty
`assignee`/`priority`), which is one shape with absent optional data, not a
different shape. The sibling #995 amendment implements this: its
`ForgeCallResult` carries the canonical types, not forge-proxy proto messages.

Two typing calls, both settled:

- **`number` is `uint32`, narrowed server-side.** A forge issue/PR number
  cannot exceed `uint32` in practice, so the narrowing (from whatever width
  the forge API reports) is safe-by-construction inside the ingestion
  translation, and protobuf-es codegen emits the field as a plain `number` —
  every UI read site is conversion-free. No canonical field is 64-bit — every
  numeric field is `uint32` (issue/PR numbers, diffstat counts) and every
  other field is `string`/`bool`/`enum` — so the no-bigint preference holds
  with no exception, and there is no client-side conversion layer at all.
- **`forge_state` vs `state`.** The forge's own `open|closed(|merged)` is
  carried under a distinct name so the canonical lifecycle (`state`) can never
  be confused with forge truth. The display relationship between them is a
  pure derivation, settled in §Lifecycle: forge `closed`/`merged` is
  consistent with Compass `done`; the UI surfaces forge state only as a
  passive badge, and the Settings mapping editor stays tracker-only.

This is an **additive** `compass.v1` change: new messages, a new oneof
variant, and the mutation RPC of §The write path — exactly the non-breaking
append path the contract reserves (compass.proto:116-117).

### The server projection

The server computes the canonical types in a projection composing with the
existing `go/internal/board` pattern — the Server-side Bridge board that
already projects **agent-session** lifecycle onto `SubscribeEvents`
(`go/internal/board/projection.go`): it consumes lifecycle transitions, records
each one, and exposes the result "two ways off one source of truth: a
point-in-time snapshot for GetAgentStatus, and a live fan-out on
SubscribeEvents" (projection.go:3-9), with record-and-publish atomic under one
lock so a snapshot can never disagree with the stream (projection.go:65-72).

Two things to be precise about, because the existing projection is a
**precedent, not the same object**:

- **Distinct state axes.** `board.Projection` projects `AgentSessionStatus` —
  the agent *process* lifecycle (STARTING/READY/WORKING/…,
  compass.proto:169-180) — and "adds no proto surface" (projection.go:8-9)
  because it reuses that frozen payload. The issue projection projects the
  issue lifecycle (Backlog→…→Done→Archived, DL-033 + DL-091) — a different axis on a
  different object — and therefore DOES add proto surface: the canonical
  messages above. One session key = one agent; one canonical Issue = one
  issue; an agent relates to an issue through `assignee`, not identity.
- **What it composes.** The projection's inputs are (a) the **ingestion
  translation** — raw forge data fetched/streamed by the forge adapter and
  translated into canonical `Issue`/`PullRequest` values as soon as it
  arrives, with the owner header parsed into `AgentAttribution` and stripped
  from the body (the #995 Decision 2 / DL-050 semantics) — keyed through
  #995's DL-055 ownership index ("agent ownership queries and the Bridge
  boards are local index scans over Server-recorded truth … an ownership
  index, never a mirror of forge content", DECISIONS.md:76); and (b) the
  Compass-owned machinery — the canonical lifecycle, priority, assignee,
  tracker projection — which this record moves server-side (DL-032 alignment:
  canonical state now lives where the canon is computed). The output is the
  canonical type, snapshot + live fan-out off one source of truth, mirroring
  the recorded-state invariant the existing projection enforces
  (projection.go:59-94).

The projection is **board-wide, keyed by issue id** — settled. The board is
the unit (a list, not a per-agent lookup), DL-055 frames it as "local index
scans over Server-recorded truth" (DECISIONS.md:76), and an issue exists
before any agent session does. The existing `board.Projection` keys
per-session because its object *is* the session (projection.go:13-14); the
issue projection's object is the issue, so its natural key is the Compass
issue id, with `assignee` relating it to agents.

Unlike the agent-session `board.Projection` — whose object is an ephemeral
process status that needs no durable canon, so its live buffer is the event
ring, which evicts past its window (projection.go:11-13; the session map
itself is retain-all today, projection.go:42-46) — the issue projection's
canonical state
(`state`, `priority`, `assignee`) is DURABLE truth that
must survive a restart. Per DL-019 (Postgres is the store of record) and
DL-020 (the in-memory ring is a cache/fan-out, never a second store), the
canonical issue state is persisted in Postgres; the projection is the
read-through cache and live fan-out over it, not the store. On restart the
projection rehydrates from Postgres; nothing upstream is asked to rebuild
canon it never held (the tracker is a projection OF this state per DL-032,
and cannot be inverted to recover it).

Board membership is keyed on the artifacts the projection has ingested, and
the board renders them partitioned by `assignee` into agent swimlanes exactly
as today (board.ts:45-67 filters `boardAgents`/`cellItems` on `assignee`;
`laneTotal` is state-only, board.ts:69-74). Two discovery feeds bring artifacts into the projection,
reproducing today's two-population board (active + Backlog) server-side. (1)
The DL-055 ownership index — the artifacts Compass AUTHORED (a row per
artifact it authored: coordinate + agent + owner + session), a local index
scan over Server-recorded truth (DECISIONS.md:76). (2) The user's
tracker-assigned issues — the feed behind today's Backlog view
(`listAssignedIssues`, tracker.ts:25-26), which includes HUMAN-authored
issues assigned to the user and so is NOT covered by the authored index. The
union covers the board's populations because an agent-worked issue reaches the
projection through whichever feed discovered it — a Compass-authored issue via
the authored index, a human-authored issue the Dispatcher assigned to an agent
via that agent's owner having it in their tracker-assigned feed — and once
ingested it is placed in the swimlane its `assignee` names, NOT by which feed
found it. (Authorship and assignment are distinct: the DL-055 index is an
authorship index, so it is a discovery source, never the swimlane key.) Both
feeds are kept current by the DL-053 subscription/poll cursor; an issue that
is both authored and tracker-assigned is one board item keyed by its Compass
issue id, not two. "Every board item is forge-backed" (Global Constraints) is
the backing rule; these two feeds plus the assignee partition are the
membership rule.

Header parsing, forge-state translation, numeric narrowing — every conversion
happens at ingestion, server-side. The UI receives finished truth. Producers
of the machinery fields (all server-side, so the UI reads finished truth):

- `state` — the mutation RPC (§The write path); the only
  lifecycle-transition producer.
- `priority` — ingested from the tracker when the tracker exposes a native
  priority field, else defaulted; there is no separate priority editor in
  v1. Wire type: `string`
  (`"urgent" | "high" | "medium" | "low"`), NOT deferred — a candidate enum
  is a future additive tightening, but v1 freezes `string` so the contract is
  complete.
- `assignee` — set by the Dispatcher/agent-session binding server-side (the
  agent working the issue); empty when unassigned. Distinct from `agent`
  (§Attribution): `assignee` is Compass truth, `agent` is a parsed claim.
- `summary` — the latest-activity line, computed server-side from the
  artifact's recent events.
- `branch` — the working head branch name, a VCS fact translated at ingestion
  (the per-PR diffstat `changed` lives on `PullRequest`, not the issue).

### The write path

Lifecycle is server-authoritative, so lifecycle **writes** are a
`CompassService` surface — settled, not deferred. Today `CompassService` has
no issue/board mutation RPC: its surface is GetServerInfo, SubscribeEvents,
the agent-session RPCs, and IssueToken (compass.proto:17-74). The existing
projection's transition producer is `runnerhub.LifecycleSink`
(projection.go:3-5, :59-63) — the RunnerHub drives agent-session transitions
into `PublishSessionStatus` — the precedent transition producer for the
agent-session board. The issue projection has two kinds of input, and
only one of them moves the canonical lifecycle. The ingestion translation
produces FORGE-FIELD updates — title, `forge_state`, labels, checks, the PR's
reviews and threads — as forge data arrives; it never mutates the canonical
`state`, `priority`, or `assignee`. The SOLE producer
of a canonical lifecycle transition is the mutation RPC surface below
(human/UI/agent-driven). This is exactly the passive-badge relationship
§Lifecycle settles: forge `closed`/`merged` shows as a badge and is
"consistent with" `done`, but a forge event never auto-advances Compass
state — an issue can carry a `closed` forge badge while its Compass `state`
is still in progress, and that divergence is intended, not a bug. The issue
board's transition producer is the mutation RPC:

```proto
// Added to CompassService:

// Set an issue's canonical lifecycle state (promote is a state set to TODO;
// drag-to-column is a state set to the target column; ARCHIVE is a state set
// to ARCHIVED). Archiving is a lifecycle transition, not a separate marker
// RPC (DL-091) — there is ONE mutation surface. The board is a MANUAL
// lifecycle — the human/agent is authoritative over Compass state (DL-032) —
// so any of the eight real states is accepted as a target (any-to-any;
// DL-033's arrows are the normative flow, not a server-enforced constraint).
// The only rejected input is ISSUE_STATE_UNSPECIFIED (invalid_argument,
// never a silent no-op). A request whose target equals the current state is
// an idempotent no-op returning current truth (double-click safe).
rpc UpdateIssueState(UpdateIssueStateRequest) returns (UpdateIssueStateResponse);

message UpdateIssueStateRequest {
  string issue_id = 1;      // the Compass-local id (Issue.id)
  IssueState state = 2;     // the target state; UNSPECIFIED is invalid_argument
}
message UpdateIssueStateResponse {
  Issue issue = 1;          // post-transition truth (unchanged on a no-op)
}
```

The idempotency contract refines the guards the UI already encodes
(store.ts:894-924): promote is "a no-op unless it's currently `backlog`, so
the action is idempotent against a double-click" (store.ts:898-905), and
archive today no-ops unless the issue is `done` and not already archived
(store.ts:916-924). Server-side, the RPC DISTINGUISHES an idempotent repeat
from an illegal request — a refinement the UI-side guards did not need
because they never surfaced an error, but the RPC must, so a caller can tell
"already there" from "not allowed": already at the target state (`ARCHIVED`
included) → no-op returning current truth; an `ISSUE_STATE_UNSPECIFIED`
target → `invalid_argument`. Archiving is now just `UpdateIssueState` with
target `ARCHIVED`, so it inherits the any-to-any rule — no from-`done`
precondition and no separate archive error path; a re-archive is the
already-at-target no-op. The response returning the resulting `Issue` lets
the caller reconcile without a read RPC.

How a mutation lands: the transition is a compare-and-transition — read
current state, apply the target (rejecting only `UNSPECIFIED`), commit the new
canonical state to Postgres, then record+publish the result. Validation is NOT
a separate pre-lock step: concurrent `UpdateIssueState` calls on the same issue
can interleave, so validating outside the transition's own lock would let a
concurrent mutation make the decision stale (ingestion is not a staleness
source here — it writes only forge-fields, never the canonical `state` /
`priority` / `assignee` the transition reads and validates). The
read-and-validate must therefore be part of the same serialized transition.
Two locking facts, kept distinct
because the store is now durable (unlike the in-memory `board.Projection`
whose atomic record+publish this extends, projection.go:65-72): (a) the
per-issue transition is serialized so compare-and-transition is atomic — the
cleanest form is a short-lived per-issue mutex (or, equivalently, a Postgres
row-level `SELECT ... FOR UPDATE` in one transaction) that spans
read→validate→commit, NOT the projection's single map mutex, because a
blocking Postgres commit must never be held under the mutex the read-through
cache and the fan-out share (that would serialize every board read on a DB
round-trip). (b) The in-memory cache update and the non-blocking
`events.Bus.Publish` fan-out (a per-subscriber select/default under the
distinct `bus.mu`, events.go:195-201) stay atomic under the projection's map
lock exactly as `board.Projection` does today — that borrowed rationale is
in-memory-only and applies ONLY to this second step, never to the durable
commit. Ordering is commit-then-cache: Postgres is committed first (so a crash
after commit loses nothing — the rehydrate reads it back), then the cache
update + publish happen atomically under the map lock (so the snapshot and
every `SubscribeEvents` subscriber observe the same ordered sequence and the
mutating client sees its own write return on the stream). A durable-but-not-yet-
published transition is recovered by the rehydrate, never lost.

The tracker mirror moves server-side with the write: a real promote (state
actually changed) writes through the tracker
seam to the linked tracker issue, mapping through the user's
`TrackerStatusMapping` — the same only-on-real-transition rule the UI
enforces today (store.ts:906-910), now enforced where the canon lives. When
the linked tracker target IS the backing forge artifact — a Linear-origin
issue whose forge provider and tracker are the same Linear issue (repo = the
Linear project key, §The Compass types; DL-051) — the mirror is elided: the
canonical `state` write is already the source of truth for that issue, so
there is no separate tracker write to make (no write-to-self). The local
transition still records and publishes; only the redundant outbound mirror is
skipped. A GitHub-origin issue with a Linear tracker (distinct artifacts)
mirrors normally. An unlinked issue (no `tracker`) simply has no mirror
target; the local transition still happens.

A transition INTO `ARCHIVED` is the one working-vs-terminal exception to the
mirror rule: it is a real state change but has no tracker status to map to
(§Approach — the mapping domain is the seven working states; the tracker has
no archive concept), so its outbound tracker mirror is elided even when the
issue is linked. The local transition still records and publishes; the tracker
is left at whatever status the issue held on entering Done.

The UI consequence: `promoteToTodo` and the archive action become thin calls
to these RPCs, and the local signal updates from the `SubscribeEvents` stream
rather than by local mutation — nothing client-side races the
server-authoritative stream. Until S1 lands, the fixture keeps holding writes
locally (honest today: local transitions mutate the seed and nothing is on a
wire); the cutover replaces them rather than wrapping them.

### The UI: consume the canonical types, remove `workstream`

The UI's board type becomes the generated canonical `Issue` from
`@compass/client` — the same codegen path the fixture module already targets
("the shapes below intentionally mirror that eventual contract",
stub-data.ts:13-14). #995 is unbuilt and the server projection lands in its
own lane, so the seam is **fixture-first**: the fixture seeds canonical-shaped
objects into the existing reactive signal today ("Seeded from the fixture; the
real @compass/client stream replaces the seed later (the accessor stays the
seam)", store.ts:426-430). For **reads**, the generated client streaming real
canonical Issues when the server lands is a seed swap behind the same
accessor — no read-path change. For **writes**, the cutover is real: today's
local transitions (promote/archive) mutate the seeded list directly, which is
honest only while nothing is on a wire; once the server-authoritative stream
lands, those local mutations are REPLACED by the §The write path RPCs, not
left racing the stream. Components read canonical fields synchronously off the
reactive list; nothing anywhere in `apps/ui` types, converts, or embeds a raw
forge shape.

Attribution stays consumed-never-stamped (§Attribution). The lifecycle,
tracker projection and board partition are preserved and renamed
(§Lifecycle, projection, board). The rename blast radius is §Plan's S2.

### Attribution: consumed, never stamped

The canonical type's `agent` field arrives already parsed — the server stamps
the owner header at write and parses+strips it at ingestion (the #995 Decision
2 semantics, DL-050); the ingestion translation fills the type's own
`AgentAttribution` fields. The UI's whole job is display: the card and the
right-sidebar PR pane surface `agent.agentHandle` / `ownerHandle` as **agent
attribution**. The claim is rendered per the #995 OQ-1 ruling: the server
sets `verified` at ingestion from the forge-login cross-check (the artifact's
forge account login equals Compass's own forge identity, the single write
credential of DL-052/#995 Decision 4), and the UI hedges the attribution —
"claims to be @atlas (Compass agent, owned by @matt)" — only while `verified`
is false, dropping the hedge to the plain "@atlas (Compass agent, owned by
@matt)" when true (#995 design.md:2314-2318 pins both wordings; #995
design.md:2352-2357 pins the claim wording with a golden test). `verified` is
a trust bit, not an identifier — it does not reintroduce the account or
session ids OQ-1 dropped. A parsed header is still untrusted display metadata
that must not reach a routing or selection decision (#995 design.md:377-383).
Concretely: `assignee` (who is working it — Compass truth, server-computed)
and `agent` (who authored the artifact — a parsed claim) are different
fields with different trust, and neither the server nor the UI ever derives
one from the other. The native forge account that authored the artifact is a
separate field (`forge_account`), which the `verified` cross-check reads.

### Lifecycle, projection, board — preserved, renamed

- The lifecycle keeps DL-033's seven working states and adds a terminal
  `ARCHIVED` state (DL-091), and becomes
  **server-authoritative**: the canonical type's `state` field is computed and
  streamed by the server projection, superseding this UI tree's current
  UI-app-state scoping (tracker.ts:4-8 records the old call — "NOT a
  compass.v1 change" — which the server-computed ruling overrules; the comment
  is swept in S2). Until the server lands, the fixture holds the state and
  the existing local transitions (promote/archive) mutate the seeded list —
  the same objects the server will later own.
- **Lifecycle ↔ forge state is a pure derivation, settled.** Forge
  `closed`/`merged` is consistent with Compass `done`/`archived`; anything
  else is consistent with the non-terminal states. It surfaces only as a
  passive badge on the PR pane/card (`forge_state` is a fact the canonical
  type carries); it is not user-editable and not a settings surface — the
  Settings mapping editor stays tracker-only, because the tracker mapping is a
  user-org configuration while forge state is a fact. The canonical `state`
  is moved ONLY by the §The write path mutation RPC, never by ingestion;
  ingestion keeps `forge_state` current for the badge.
- The tracker projection is preserved, with one archive-driven narrowing:
  `toTrackerStatus`/`fromTrackerStatus` (tracker.ts:69-84),
  `LINEAR_STATUS_MAPPING` (tracker.ts:38-58), and the Settings mapping editor
  (SettingsView.tsx:17-25) keep their contracts, but their state domain is the
  SEVEN WORKING states, not the full eight — `ARCHIVED` carries no tracker
  status. Concretely the `toTracker` map's key type (a `Record` total over the
  state union today, stub-data.ts:127-128; the same totality is what
  `SettingsView`'s `STATE_ROWS` and `tracker.test.ts`'s exhaustiveness literal
  defend) is keyed on a `WorkingIssueState` subtype that excludes `ARCHIVED`,
  so no archive row is type-forced into the mapping. The write path mirrors
  only a real working-state transition; `UpdateIssueState(ARCHIVED)` is
  mirror-ELIDED — a Compass-local terminal sink writes nothing to the tracker,
  because the tracker has no archive concept (an archived issue was already at
  its tracker Done status when it entered Done; archiving does not re-write it).
  The rejected alternative — give `ARCHIVED` a mapping row defaulting to the
  Done status — is worse: it would re-issue a Done write on every archive and
  imply the tracker can round-trip an archive it cannot represent. Matt can
  add an archive→tracker write later if a tracker grows the concept
  (additive). The `TrackerSeam` (tracker.ts:24-30) is a separate seam —
  unchanged in contract, renamed in vocabulary; its mirror write moves
  server-side with the write path (§The write path).
- The board partition functions are preserved in contract: `ACTIVE_STATES`,
  `isActiveState`/`isBacklogState`, `activeWorkstreams`→`activeIssues`,
  `backlogWorkstreams`→`backlogIssues`, `boardAgents`, `cellItems`, `laneTotal`
  (board.ts:15-74). Swimlane rows stay agents; promote-to-Todo keeps its
  exact no-op/idempotency contract (store.ts:894-905) — locally until S1, then
  server-side via §The write path. **Archive is now a state, not a marker**
  (DL-091): the DoneView partition moves off the `archivedAt` timestamp onto
  `state` — Done is `state === "done"`, Archived is `state === "archived"`
  (DoneView.tsx:86-88, today `w.state === "done" && !w.archivedAt` vs
  `w.archivedAt`) — and `archiveWorkstream` becomes an `UpdateIssueState`
  call to `ARCHIVED` rather than an `archivedAt` stamp (store.ts:916-924); the
  `archivedAt` field and its ISO-string display (the `done-archived-mark`
  title/label, DoneView.tsx:70-71) are removed. `ACTIVE_STATES` INCLUDES
  `done` (it is a `BOARD_LANES` entry, constants.ts:22, and board.ts:43-44
  calls it an active column); `archived` stays off the active board because it
  is NOT a `BOARD_LANES` entry, not because `done` is excluded. This carries a
  real, intended behavior change (DL-091): today an archived row keeps
  `state: "done"` (store.ts:918-920 stamps `archivedAt` without touching
  `state`), so `cellItems`' state-only filter (board.ts:63-65) still renders it
  in the board's Done column and it counts in `laneTotal("done")`; under
  archived-as-state it moves to `state === "archived"`, leaves the Done column,
  and drops out of that count. That delta is carved out of the no-regression
  constraint (§Global constraints) alongside the commit history. Both the
  partition move and the state addition are S2 change sites.
- **Which PR the card renders.** An issue now carries `repeated prs`, so the
  card/board summary and the Done row render the issue's **primary PR** via one
  total selector, shared by the card, DoneView, and the RightSidebar PR pane.
  Its precedence is frozen here (no PR timestamp exists on the wire — the
  no-64-bit sweep removed them — so selection is by open-ness and `prs`
  ordering, not recency): the first `OPEN` PR in `prs` order, else the first
  `MERGED`, else the last element. `prs` ordering is a server contract —
  ingestion appends in discovery order, newest last — so "first"/"last" are
  well-defined; open-ness comes from `forge_state`, which every `PullRequest`
  carries. "Newest by number" is deliberately NOT the rule: PR numbers are
  per-repo and the record puts multi-forge first-class, so numbers are
  incomparable across a `repeated prs` that may span repos/forges. An issue
  with no PRs renders no PR chip, exactly as `pr: null` does today. The
  multi-PR switcher is a purely additive later surface, not a v1 shape change.
  Every former `ws.pr`/`pr()` read (WorkstreamCard.tsx:26, DoneView,
  RightSidebar PrPane) routes through this selector — enumerated in S2.
- **Card issue key.** Every board item is forge-backed, so the card key
  (`card-issue`, WorkstreamCard.tsx:25, currently the bare `ws.issue` string)
  becomes the tracker id when linked (`tracker.id`, rendered in the tracker's
  native form — e.g. `SEA-1042`, not `SEA#1042`), else the forge coordinate.
  The coordinate is `${repo}#${number}` in the single-forge common case, but
  qualifies with the `ForgeRef` host — `${host}/${repo}#${number}` — when the
  board holds artifacts from more than one `ForgeRef`, so the DL-091
  disambiguation reaches the user-facing key and two artifacts never collide on
  `repo` alone (e.g. github.com and a self-hosted Forgejo both at `acme/api#12`).
  Both forms are always renderable, no null branch. The Compass-local `id`
  stays the stable selection/join key (store keys, `STUB_FILES` keying), never
  a display fallback.
- **PR check pips — pip-per-check preserved.** The rendered PR is the primary
  PR (above); its canonical `PullRequest` carries the rolled-up `ChecksSummary`
  (translated at ingestion). Today the
  card and the Done view render **one pip per check** (`<For each={pr().checks}>`
  → one `.check-pip` each, WorkstreamCard.tsx:28-33, DoneView.tsx:26-29), so
  they keep doing exactly that: both iterate the per-check list
  `checks.checks` and map each 6-value `Check.state` to a pip class via
  a single small total function introduced by this design and shared by all
  three pip sites — the card, the Done view, and the `CheckRuns` per-check
  list (RightSidebar.tsx:163-177, which today forwards an already-3-valued
  `status` and gains the 6→3 mapping here): `success→success`,
  `failure|cancelled→failure`, `queued|in_progress|neutral→pending` — one
  mapping, three sites, pip count and CSS (app.css:766-774) byte-preserved.
  The summary-level roll-up `checks.state` is available but is NOT what the
  pips render — substituting it would collapse a 3-check PR's three pips into
  one. `app.css` needs **no new rule**. No `checks` (unset) → no pips, same
  as a PR with no `checks` today.
- **PR state badge — state + draft derivation.** The badge renders the primary
  PR (above). Today the PR pane's
  `data-state` (RightSidebar.tsx:189) drives CSS
  `.pr-state[data-state="draft"|"open"|"merged"|"closed"]` (app.css:1383-1406)
  off a single 4-value field where `draft` was a state (stub-data.ts:64);
  DoneView's `data-pr-state={pr().state}` (DoneView.tsx:25) is currently a
  **dead attribute** — no `[data-pr-state]` selector exists in app.css
  (verified by grep this run) — but DoneView also reads `pr().state` as LIVE
  output in two more places: the visible row text (`#<number> <state>`,
  DoneView.tsx:31) and the merge-summary line (which renders "merged" when the
  state is merged, else "PR " followed by the state, DoneView.tsx:45). The
  canonical type splits the field into `forge_state` (`open|closed|merged`)
  plus `draft: bool`, so every one of those reads is a mandatory change site —
  the field they read ceases to exist. A pure derivation restores the live
  badge without a CSS change: draft-and-open renders `draft`, everything else
  renders `forge_state`. It is applied identically at every `pr().state` read —
  the DoneView attribute, visible text, and merge summary (DoneView.tsx:25,
  :31, :45), and the RightSidebar PR pane (RightSidebar.tsx:189), all
  enumerated in S2 — for consistency across badge sites; at DoneView that
  consistency, not existing styling, is what the derivation preserves.

## Alternatives considered

The fork, updated for the 2026-07-31 ruling — four readings, one chosen —
plus two alternatives surfaced by design review (items 5 and 6):

1. **One canonical Compass-owned type family, forge translated at ingestion
   (CHOSEN — Matt's ruling).** `compass.v1` owns a single `Issue`/`PullRequest`
   family carrying forge fields, agent attribution, and the Compass machinery;
   the server translates raw forge data into it as soon as it arrives; the raw
   forge shape is never a wire type. Wins because it is the only shape that
   satisfies both rulings at once — "we don't expose the non-compass'ed types"
   and "translate the raw forge info into those types as soon as we get
   them" — while keeping the earlier ruling's halves: the forge fields (with
   auto-stamped, server-parsed attribution) and the Compass
   lifecycle/priority/assignee machinery as canonical board state — canonical
   *and* server-side, aligning DL-032 and composing with DL-055's
   board-as-index-scan. One family also means no drift surface between two
   wire shapes: the only translation is one unit-testable Go conversion.
2. **A separate forge-proxy proto family coexisting with the canonical type
   (REJECTED, 2026-07-31).** Keep forge-proxy `compass.v1` messages (per
   #995's earlier sketch) as server-internal-but-declared wire types, with the
   canonical type embedding or projecting from them, disambiguated by a
   `Board` prefix. Rejected by Matt's ruling: the non-compass'ed types are not
   exposed — not as a parallel proto family either. Two families in one
   package would force disambiguating names, invite hand-mirrored scalar
   drift between the shapes, and declare wire contracts for data that never
   rides the wire.
3. **Client-side embed (REJECTED, 2026-07-30 — was this record's prior
   draft).** The UI embeds forge shapes locally
   (`Issue.forge: ForgeIssue | null`), mirrors the proto field-for-field in
   TypeScript, rides the existing signal, and converts `bigint`→`number` at
   the client boundary. Rejected by Matt's ruling: it exposes the raw forge
   artifact on the UI wire, forces the UI to maintain hand-written mirrors of
   server truth (a drift surface the codegen exists to prevent), and leaves
   canonical lifecycle state client-side — the opposite of DL-032's direction
   of travel.
4. **Rename-only, keeping the bare tracker string.** `Workstream`→`Issue` as a
   pure identifier sweep, `issue: string` (stub-data.ts:87) surviving as
   `trackerId: string`. Rejected (unchanged from the prior draft): it
   satisfies the vocabulary constraint but not the ruling — a bare string
   embeds nothing; attribution and checks have nowhere to live, so the
   "attribution provided automatically" half of the ruling is unimplementable
   on this shape.
5. **A slimmer agent-only forge message alongside the canonical type
   (REJECTED — surfaced by design review).** Let the agent forge tools return
   a trimmed shape (forge fields only, no board machinery) so agent tool
   results never carry Compass state. Rejected: it is a second type family in
   all but name — the exact drift surface the one-family ruling eliminates —
   and an unset machinery field on the canonical type already expresses "not
   board-tracked" at zero cost. The canonical type is returned everywhere;
   the #995 amendment is constrained to it. (If Matt prefers a trimmed agent
   surface at the gate, it is an additive change to the amendment, not to
   this record's canonical type.)
6. **A constrained (adjacent-only) transition matrix (REJECTED — surfaced by
   design review).** Enforce DL-033's ordering server-side so only legal
   edges (e.g. Backlog→Todo, In Progress↔Blocked) are accepted and an
   off-graph target is rejected. Rejected for v1: the board is a manual
   surface and Compass state is human/agent-authoritative (DL-032), so a
   drag from any column to any column is a legitimate manual correction;
   DL-033 documents the normative flow, not a guard. Any-to-any is the
   least-mechanism reading and matches kanban norms. (A constrained matrix
   is a purely additive server-side tightening if Matt wants it at the
   gate — it removes accepted inputs, so it is not a breaking change to the
   RPC.)

## Plan

The design splits into two slices, implemented post-freeze in separate lanes:
**S1 (proto + server)** — the canonical messages, the ingestion translation,
the projection, and the mutation RPC — and **S2 (UI consumption)** — the
`workstream` removal and the canonical read paths. S2 does not block on S1:
the fixture seeds canonical-shaped objects until S1's stream exists (§The UI).

Blast radius for S2, verified this run by a case-insensitive `workstream` grep
over `apps/ui/src`: 13 source files (stub-data.ts, board.ts, constants.ts,
store.ts, tracker.ts, Bridge.tsx, WorkstreamCard.tsx, BacklogView.tsx,
DoneView.tsx, LeftSidebar.tsx, RightSidebar.tsx, SettingsView.tsx, **app.css**)
and 6 test files (board.test.ts, store.test.ts, tracker.test.ts,
identity.test.ts, settings-mapping.test.ts, RightSidebar.test.ts). `app.css`
carries `workstream` in four comment blocks (app.css:4, :667, :2093, :2234)
the grep catches — **and** the check-pip / `pr-state` rules (app.css:766-774,
:1383-1406) the grep does *not* flag but the shape change touches; the
state+draft badge and the per-check pip mapping (§Lifecycle) keep those
rules unchanged, so `app.css` is a comment sweep plus a verify-no-CSS-change,
not a rule rewrite. All `*.tsx` component cites are relative to
`apps/ui/src/components/`; the non-component files sit at `apps/ui/src/`.

### S1 — Proto + server: the canonical types, projection, and write path

The additive `compass.v1` delta, the ingestion translation, the server
projection, and the mutation RPC.

- **Proto:** add `Issue`, `PullRequest`, `IssueState` (with terminal
  `ARCHIVED`), `AgentAttribution`, `ForgeProvider`/`ForgeRef`,
  `ChecksSummary`/`Check`, and the small carrier messages (`ChangedStats`,
  `TrackerRef`, `Review`/`ReviewThread`/`Comment`) per §The Compass Issue and
  PullRequest types; the `Issue` variant on the `SubscribeEventsResponse` oneof
  (compass.proto:118-128); and the single `UpdateIssueState` mutation RPC per
  §The write path (archive is a state target, not a separate RPC).
- **Server:** the ingestion translation (raw forge data → canonical types, at
  the forge adapter boundary — owner-header parse/strip per DL-050, all
  numeric narrowing, forge-state strings) and a projection in
  `go/internal/board` (or a sibling package — implementation lane's call) that
  composes the translated artifacts with the Compass machinery over the
  DL-055 ownership index, keyed board-wide by issue id, exposing snapshot +
  live fan-out off one source of truth, following `Projection`'s
  recorded-state invariant (projection.go:59-94). The mutation RPC handler
  lands transitions into the same record-and-publish path and carries the
  promote/archive idempotency contract (§The write path).

`Interfaces:` the §The Compass Issue and PullRequest types proto sketch (field
numbering owned by this slice); the `UpdateIssueState` request/response pair;
Go-side, a `PublishIssue`-shaped recording entry point
and a `Snapshot` mirroring `board.Projection`'s contract
(projection.go:96-128).

`Acceptance:` `buf lint` + `buf breaking` green (additive-only); generated
`@compass/client` exports the canonical types with plain-`number` issue/PR
numbers; a projection unit test proves snapshot/stream agreement (the
recorded-state invariant) and that no raw forge shape appears in any UI-facing
response type; ingestion unit tests against recorded forge fixtures prove the
translation (header parse/strip, narrowing, checks roll-up); mutation-RPC
tests prove the error-vs-noop contract (§The write path: a repeat at the
target state — `ARCHIVED` included — is a no-op returning current truth;
an `UNSPECIFIED` target is `invalid_argument`) and that a mutation's effect
reaches snapshot and stream identically; the canonical `agent` is the
server-parsed attribution, never re-parsed client-side.

### S2 — UI consumption: remove `workstream`, read the canonical types

The rename plus the canonical read paths, against fixture-seeded objects.

- **Types + fixtures (stub-data.ts):** delete
  `Workstream`/`WorkstreamState`/the local `PullRequest`
  (stub-data.ts:24-31, :61-71, :84-109); the board type becomes the canonical
  shape (fixture-typed until `@compass/client` generates it, then the import
  flips — the seam). Rewrite `STUB_WORKSTREAMS`→`STUB_ISSUES` and
  `STUB_ASSIGNED_ISSUES` as canonical-shaped objects, deriving forge fields
  from the existing fixture narrative (e.g. ws-1022's PR #453,
  stub-data.ts:491-516) — every row forge-backed.
  `TrackerRef`/`TrackerStatusMapping`/`TrackerConfig` keep their shapes with
  the canonical state type substituted (stub-data.ts:113-140). `STUB_FILES`
  keying moves off "workstream id" wording (stub-data.ts:956-958).
  identity.test.ts's "workstream assignee migration" suite
  (identity.test.ts:310-322) moves with it.
- **Pure core (board.ts, tracker.ts, constants.ts):**
  `activeWorkstreams`→`activeIssues`, `backlogWorkstreams`→`backlogIssues`;
  `isActiveState`/`isBacklogState`/`boardAgents`/`cellItems`/`laneTotal` keep
  names and code with canonical parameters (board.ts:15-74 — the FUNCTIONS are
  byte-identical; the only rendered delta is data-driven, an archived row now
  carrying `state === "archived"` instead of `"done"`, per the §Lifecycle
  Done-column carve-out). `tracker.ts`: same contract, renamed signatures; the
  stale "NOT a compass.v1 change" scoping comment (tracker.ts:4-8) is rewritten
  for the server-authoritative model. `constants.ts`: `Lane.state` retyped
  (constants.ts:10-15), `BOARD_LANES`/`BACKLOG_STATES` values unchanged
  (constants.ts:17-28), `RightTabGroup = "fleet" | "issue"` replacing
  `"workstream"` (constants.ts:45). `board.test.ts`: its `ALL_STATES`
  exhaustiveness literal (board.test.ts:21-24) gains `archived`, classified as
  a THIRD tier — terminal, off-board (neither active nor backlog) — and its
  Invariant-4 partition test (board.test.ts:153-161, "cover every workstream
  exactly once") is restated as active ∪ backlog ∪ archived; the same
  eighth-member exhaustiveness applies to `tracker.test.ts`'s literal
  (tracker.test.ts:15-17), whose domain is the seven working states (§Approach).
- **Store (store.ts):** the accessor sweep — `workstreams`→`issues`,
  `selectedWorkstreamId`→`selectedIssueId`, `selectedWorkstream`→
  `selectedIssue`, `selectWorkstream`→`selectIssue`, `archiveWorkstream`→
  `archiveIssue`, `WorkstreamTab`→`IssueTab` (store.ts:72, :232-250,
  :396-407, :894-924). Contracts unchanged: `promoteToTodo` keeps its
  guard/idempotency (store.ts:898-905) against the fixture until S1, then
  becomes an §The write path RPC call (the seam write at store.ts:910 moves
  server-side with it). `archiveIssue` changes shape: it sets `state` to
  `"archived"` instead of stamping an `archivedAt` timestamp (store.ts:916-924)
  — locally until S1, then an `UpdateIssueState(ARCHIVED)` call; the DoneView
  Done/Archived partition follows `state`, not the dropped timestamp. The
  local fixture action KEEPS its done-only guard (store.ts:918-919, its only
  call site is the DoneView Done section), even though the server RPC is
  any-to-any (§The write path) — the guard is a UI-affordance constraint, not
  the server contract; store.test.ts's no-op-on-non-done test moves with that
  guard. `selectIssue` still syncs the roster without leaving the board
  (store.ts:682-686); `openAgent` anchoring and `setActiveBranch` keep their
  derivations over the renamed list (store.ts:628-649, :967-974).
  store.test.ts (115 `workstream`-carrying lines by case-insensitive grep this
  run, the largest test move; its archive test moves off the `archivedAt`
  stamp onto the `archived` state) renames with the accessors.
- **Components:** `WorkstreamCard.tsx`→`IssueCard.tsx`; the card key, the
  primary-PR selector, the per-check pip mapping, and the state+draft badge
  from §Lifecycle land at their sites — `WorkstreamCard.tsx:25-26`; DoneView's
  Done/Archived partition (off `state`, DoneView.tsx:86-88), its `archivedAt`
  display removal (DoneView.tsx:70-71), and its every `pr().state` read (the
  `data-pr-state` attribute :25, the visible row text :31, the merge-summary
  line :45) routing through the primary-PR selector. The three issue-level
  `changed` reads move with correction #4 (`ChangedStats` is now a
  `PullRequest` field): the card diff chip (WorkstreamCard.tsx:42-47), the Done
  row diff chip (DoneView.tsx:50-55), and the RightSidebar VcsPane "Changes"
  row (RightSidebar.tsx:130-132) all re-source from the primary PR's `changed`
  (unset — no PR — hides the chip / shows a dash, as `changed.files > 0` gates
  today). One diff scope is deliberately dropped: today a branch-in-progress
  with NO PR can carry a diffstat (fixture ws-965: `changed:{files:6,…}` with
  `pr:null`, stub-data.ts:553-554); under "a diff is a PR fact" that pre-PR
  diffstat has no home and disappears — carved out in §Global constraints next
  to the commit history. Review shape: `RightSidebar.tsx:163-177`, :189, and
  the PR pane's new review state (reviews + threads/comments, human-and-bot
  with `is_bot`, RightSidebar.tsx:180-221 — the bot-review chips filter
  `reviews` by `is_bot` and take the latest-per-author verdict, the "N/M
  threads resolved" derives from `threads`); the canonical `verdict`
  ("changes_requested") maps to the existing chip key ("changes") at the chip
  site (RightSidebar.tsx:202-206), so `.rv[data-v=…]` CSS (app.css:1470-1477)
  is verified-unchanged under that one-line mapping. Sweep `Bridge.tsx`,
  `BacklogView.tsx` (the `ws.issue` read at BacklogView.tsx:20),
  `LeftSidebar.tsx` (activeIssues/backlogIssues call sites,
  LeftSidebar.tsx:337-343), `RightSidebar.tsx` (the bare `w.issue` reads at
  RightSidebar.tsx:125, :237), `SettingsView.tsx` (state-type substitution;
  `STATE_ROWS` domain narrows to the seven working states per §Approach, so an
  `ARCHIVED` row is never type-forced into the mapping, SettingsView.tsx:17-25),
  and `app.css` (the four comment blocks; pip/badge rules verified unchanged).
  Attribution renders per §Attribution — `agent.agentHandle` hedged as a claim
  unless `verified`, never derived into `assignee`.
- **Vocabulary gate:** `grep -ric workstream apps/ui/src` returns **zero**
  matches across code, comments, CSS-comment text, and test descriptions.

`Interfaces:` the canonical `Issue`/`PullRequest` as consumed (generated, or
fixture-typed identically until S1 lands); the renamed board/store
signatures — same shapes as today with `Issue`-vocabulary types substituted.

`Acceptance:` the zero-match grep; every task's test cycle is the same triple
from the compass repo root — `direnv exec . moon run compass-ui:test`,
`direnv exec . moon run compass-ui:typecheck`, `bunx biome check apps/ui/src`
— plus a `vite dev` smoke: board, Backlog, Done, and Settings mapping behave
identically.

## Global Constraints

- **One canonical type family; the raw forge shape is never a wire type**
  (Matt, 2026-07-31). `compass.v1`'s `Issue`/`PullRequest` are Compass-owned;
  the server translates raw forge data into them at ingestion; no forge-proxy
  proto message exists, and no `apps/ui` code types, mirrors, or converts a
  raw forge shape. All numeric narrowing is server-side.
- **Every board item is forge-backed.** There are no compass-only backlog
  items: an issue exists on the board because a forge artifact backs it, so
  every canonical `Issue` carries a real forge coordinate, and no consumer
  needs a "no forge artifact yet" branch.
- **No `workstream` vocabulary survives in `apps/ui`** — type names,
  identifiers, comments, CSS-comment text, test descriptions. Gate: a
  case-insensitive grep returns zero.
- **Compass state is canonical and server-authoritative.** The lifecycle
  (DL-033's seven working states plus a terminal `ARCHIVED`, DL-091) is
  computed and streamed by the server projection, and mutated only through the
  §The write path RPC; the tracker stays a projection of it (DL-032), mirrored
  server-side on real transitions. The prior UI-app-state scoping
  (tracker.ts:4-8) is overruled.
- **Attribution is consumed, never stamped.** The canonical type's `agent`
  field is untrusted display metadata (DL-050, #995 design.md:377-383): parsed
  at ingestion, carried in Compass's own `AgentAttribution` message, rendered
  as a hedged claim unless `verified`, never fed into routing/selection, never
  derived into `assignee`. It carries handles plus the server-set `verified`
  trust bit — no account or session identifiers; the native forge account is
  the separate `forge_account` field.
- **Extends #995's semantics, never rewrites them.** The owner-header
  stamp/parse discipline, the untrusted-metadata rule, and the DL-055
  ownership index are #995's frozen decisions and this record builds on them
  unchanged. #995's earlier sketch of forge-proxy proto messages is
  reconciled to this model in a sibling amendment to that record; this record
  depends on no forge proto message existing.
- **Lifecycle ↔ forge state is a pure derivation** (settled): forge
  `closed`/`merged` is consistent with `done`; surfaced only as a passive
  badge; not user-editable; the Settings mapping editor stays tracker-only.
- **SolidJS + Vite + TypeScript** (not React): keep the `createMemo` accessor
  pattern, the store-context read path, and the fixture-seam convention
  (tracker.ts:19-23, store.ts:426-430).
- **Additive proto only:** new messages, oneof variants, and RPCs behind the
  buf breaking gate (compass.proto:116-117); no frozen payload changes.
- **No functionality regression — with four carve-outs, all forced by Matt's
  corrections.** Board, Backlog/promote, Done/archive, tracker projection, and
  the Settings mapping editor otherwise behave identically. The four
  intended deltas: (1) an **archived row leaves the board's Done column** —
  today it keeps `state: "done"` and renders in the Done lane (§Lifecycle);
  under archived-as-state (DL-091) it moves to `state === "archived"` and drops
  from the lane and from `laneTotal("done")`. (2) The **archive DATE display is
  removed** — dropping `archived_at_unix_ms` (DL-091) removes the Done view's
  "archived YYYY-MM-DD" mark (DoneView.tsx:70-71); the Archived section still
  lists the issue, without a date. (3) A **pre-PR branch diffstat disappears** —
  moving `changed` onto `PullRequest` (correction #4) means a branch-in-progress
  with no PR can no longer carry a diffstat (fixture ws-965, stub-data.ts:553-554);
  it returns when the repo/worktree surface below lands. (4) The **commit
  history** is carved out. The canonical type carries **no `commits`
  field** (settled): commits are repo/git truth, not forge-artifact or
  Compass-machinery truth, and are carved out to a future repo/worktree
  surface. `branch` (the head branch NAME, on the issue) and `changed` (the
  per-PR diffstat, on `PullRequest`) are different: single scalar/summary
  facts the forge/PR already
  exposes on the artifact, whereas the commit HISTORY is a variable-length
  repo/git walk that belongs to the future repo/worktree surface — that is
  the line: artifact-level summary facts stay; the git-history list is carved
  out. The VCS pane renders the branch's commit history off a `commits`
  list today (RightSidebar.tsx:141-157, over `commits?: Commit[]`,
  stub-data.ts:101-103); it keeps a fixture-side commits side-channel until
  that surface exists — the history is deferred to its own surface, never
  silently dropped.
- **Ledger touch-coupling**: the freezing PR appends the DL rows to
  DECISIONS.md in the same diff, which satisfies the gate's touch-coupling leg
  directly (no `Ledger-impact:` declaration needed).

## Tasks

- [ ] **S1** — proto + server: canonical `Issue`/`PullRequest`/`IssueState`/
  `AgentAttribution`/`ForgeRef`/`ChecksSummary` (+ carriers, incl.
  `Review`/`ReviewThread`/`Comment`) in `compass.v1`, the
  `SubscribeEventsResponse` oneof variant, the single `UpdateIssueState`
  mutation RPC (archive is a state target), the ingestion translation (raw
  forge → canonical, header parse/strip, narrowing), and the board-wide server
  projection composing `go/internal/board`'s recorded-state pattern;
  buf-additive, codegen emits plain-`number` coordinates, no raw forge shape
  on any UI-facing response.
- [ ] **S2** — UI consumption: fixture-seeded canonical objects, the full
  `workstream` removal across the 13 source + 6 test files, the card key /
  pip + badge derivations, ending in the zero-match `workstream` grep gate
  and the standard test triple + smoke.
  (The DL-067…DL-070 plus DL-091 ledger rows are appended by *this* design record's PR,
  in the same diff as the record — see §Ledger delta — not an
  implementation-lane task.)

## Ledger delta

Appended to `docs/designs/product/DECISIONS.md` in the same PR that freezes
this record (touch-coupling, mirroring #995 design.md:2495-2499), under the
**UI shell** section (DECISIONS.md:101 — where DL-031…DL-037, the board-shell
decisions this record remodels, already live):

| ID | Decision | Status | Record |
| --- | --- | --- | --- |
| DL-067 | The `workstream` concept is removed from the Compass UI entirely; the board unit becomes the canonical Compass `Issue` (and `PullRequest`) — the forge artifact's fields plus the Compass machinery (lifecycle state, priority, assignee, tracker projection), server-computed | Active (Matt, 2026-07-30) | [issue model §Approach](compass-issue-model/design.md#approach) |
| DL-068 | Agent attribution on issues and PRs is consumed from the canonical type's server-parsed `agent`/owner fields (parsed at ingestion per #995's stamping) and rendered as an untrusted claim, hedged unless the server's forge-login cross-check verifies it (DL-050, #995 OQ-1); the UI never stamps a header and never derives `assignee` (Compass truth) from `agent` (a parsed claim) | Active (Matt, 2026-07-30) | [issue model §Attribution](compass-issue-model/design.md#attribution-consumed-never-stamped) |
| DL-069 | Compass owns a single canonical `compass.v1` `Issue`/`PullRequest` type pair (forge fields + agent author/owner attribution + Compass machinery); the server translates raw forge data into these types at ingestion, the raw forge shape is never a proto/wire type, and the UI consumes only the generated canonical type from `@compass/client` | Active (Matt, 2026-07-31) | [issue model §The Compass Issue and PullRequest types](compass-issue-model/design.md#the-compass-issue-and-pullrequest-types) |
| DL-070 | The DL-033 issue lifecycle (its seven working states unchanged, extended by a terminal `ARCHIVED` state per DL-091) is server-authoritative: a server-side board projection — composing the existing `go/internal/board` recorded-state pattern and DL-055's board-as-local-index-scan — computes and streams the canonical type, moving DL-032's canonical Compass state server-side | Active (Matt, 2026-07-30) | [issue model §The server projection](compass-issue-model/design.md#the-server-projection) |
| DL-091 | Archiving a Compass issue is a lifecycle transition to a terminal `ARCHIVED` state via `UpdateIssueState`, not a separate `archived_at` marker field or a separate `ArchiveIssue` RPC; an archived issue drops off the active board and is listed in the Done view's Archived section, and every board item carries a forge identity (`ForgeRef` provider+host) so multi-forge artifacts never collide on `repo` alone | Active (Matt, 2026-07-31) | [issue model §The write path](compass-issue-model/design.md#the-write-path) |

No existing row is superseded. DL-032/DL-033/DL-034/DL-055 stay Active:
DL-070 *aligns* with DL-032 (canonical state, now computed where the canon
lives) rather than overturning it. DL-091 *extends* DL-033's lifecycle with a
terminal `ARCHIVED` state beyond Done: DL-033's seven working states and their
normative flow are unchanged — the addition is a terminal sink, not a
redefinition of any existing state or edge — so DL-033 stands Active,
extended rather than superseded. DL-033
and DL-034 describe their (unchanged) decisions using the retired word
"workstream" in their immutable Decision cells; this is deliberate
non-supersession — the rulings stand, only the code vocabulary moves. Because
this PR appends the rows in the same diff as the record it freezes, the
touch-coupling leg is satisfied directly — no `Ledger-impact:` escape hatch is
needed in the PR body.
