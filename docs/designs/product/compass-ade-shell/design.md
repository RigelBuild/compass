# Compass ADE shell — Orca-mirror UI + a real, tracker-mapped state model

Status: Active

> Internal design record — July 2026. A UI/UX + state-model design for the next
> iteration of the Compass ADE dev shell, building on the explorable Bridge
> walking-skeleton (#453, #460). The product vision it serves is the frozen v0.3
> record ([`../compass.md`](../compass.md)); the strategic posture is
> [`../compass-0.4/design.md`](../compass-0.4/design.md). This record captures the
> shell's interaction model and the workstream/agent state model — not the daemon
> or the `compass.v1` wire contract, which stay as those records define them.

## Problem / Intent

The Compass dev UI is a walking skeleton: a SolidJS shell where every surface
reads an in-memory fixture (`apps/ui/src/stub-data.ts`) through one
store (`store.ts`), rendering in `vite dev` with no daemon. #460 decomposed it
into a full ADE shell — topbar, left agent tree, central Bridge board / agent
view, right files/VCS/PR sidebar, a pinned Dispatcher pane, a usage bar.

Two gaps now block it from reading as a real ADE:

1. **The state model is a placeholder.** The board columns
   (`backlog → dispatched → in_progress → in_review → blocked → merged`,
   `constants.ts:14-21`) don't match how work actually flows, don't map to any
   issue tracker, and mix pre-active backlog items into the live agent board.
2. **The shell diverges from the ADE it's modelled on.** The code already calls
   itself "an Orca-inspired layout" (`App.tsx:12`, `app.css:1`), but the right
   sidebar stacks content vertically, the agent view has no tab/split model, the
   Dispatcher is a read-only metrics pane, and the agent-state vocabulary is
   ad-hoc. The reference ADE (Orca, `stablyai/orca`, MIT) has solved each of
   these; mirroring it is faster and more legible than inventing our own.

Intent: make the shell a faithful Orca-mirror with a **real workstream lifecycle
that maps natively to the user's issue tracker** (Linear first), so the dev UI
reads true and the eventual daemon has a concrete contract to fill.

### Baseline: this record targets the post-#460 tree

Every file reference below (`store.ts`/`AppStore`, `constants.ts`, `Bridge.tsx`,
`RightSidebar.tsx`, `AgentView.tsx`, `DispatcherPane.tsx`, `LeftSidebar.tsx`, and
the `AgentState` union with `paused`/`error`) is a **post-#460** location. #460
("decompose the Bridge dev UI into a full ADE shell") introduces the single-store
seam and the component split; on `main` before #460 lands, the skeleton keeps its
state in `App.tsx` + `stub-data.ts` with a narrower `AgentState` (`idle` /
`working` / `waiting` / `paused`). **#460 is a hard prerequisite for T1–T12: the
plan MUST NOT begin until #460 is merged to `main`.** Starting from the pre-#460
skeleton would send an implementer into missing files (`store.ts`, `constants.ts`,
the split components) or a parallel store. Once #460 lands, the `store.ts` seam
and the `error` state exist, and every task below refactors those files rather
than creating new ones.

## Approach

Extend the existing single-store seam (`store.ts` `AppStore`) — never introduce a
second state pattern — across nine coordinated moves, each behind that seam so the
fixture→`@compass/client` swap stays a one-file change:

1. **A real state model.** Rewrite the workstream lifecycle to
   `Backlog → Todo → Queued → Blocked ⇄ In Progress → In Review → Done`, with the
   board showing only the **active** subset and a separate Backlog view owning the
   pre-active tier. Track the Compass state **independently** of the tracker's
   status, projected through a user-editable mapping.
2. **Mirror Orca's right sidebar** — an icon-per-tab activity bar, not stacked
   sections — with Files as a real file viewer and changed-files under a VCS tab,
   plus the one Compass-specific addition: a repo/branch dropdown that spans an
   agent's clones.
3. **Mirror Orca's agent view** — a tab group (session tab + terminal tabs +
   file tabs), splittable, terminals hidden by default.
4. **Mirror Orca's agent-state vocabulary** — the grey/emerald/amber/red/spinner
   semantics, adding a `done` state so a finished-but-unopened agent is visually
   distinct from idle.
5. **A communicable Dispatcher + a Warden tab** in a tabbed bottom dock.
6. **Topbar cleanup** — drop the duplicate view-tabs; the left sidebar is nav.
7. **Backlog, Done/archive, and Settings views** — the Linear-style issue list,
   the archive destination, and the Compass↔tracker status-mapping editor.

Alternatives weighed only where the choice isn't obvious (below); everything else
follows Orca because the explicit goal is to mirror it.

## Decisions

### D1 — Workstream state model

The lifecycle becomes seven states:

```text
Backlog → Todo → Queued → Blocked ⇄ In Progress → In Review → Done
```

- **Board columns (active work only), in display order:**
  `Queued → Blocked → In Progress → In Review → Done`.
  - Rename current `dispatched` → **`queued`**, `merged` → **`done`**.
  - **`blocked` moves before `in_progress`** (current order has it fifth,
    `constants.ts:19`).
  - **`backlog` is dropped from the board grid** and moves to the Backlog view.
- **Pre-active tier (Backlog view only):** `Backlog` + `Todo`. **Todo is the
  global pool** of promoted-but-unassigned tasks — the Dispatcher's input. The
  human promotes `Backlog → Todo` (or asks the Dispatcher to); the Dispatcher then
  assigns Todo tasks into agents' queues.
- **`Queued` is per-agent** — that agent's up-next, the tasks it will pick up. So
  Queued renders as a **per-agent swimlane cell** like the other active columns.
  (The "global dispatch queue" is Todo, a different scope — not a board column.)
- **Swimlane = active agent tasks only.** Because Todo (the unassigned pool) lives
  in the Backlog view and every board column (Queued included) is per-agent, the
  swimlane never needs an "unassigned" row — this is the correct resolution of the
  #460 deferred finding (the row simply doesn't exist), not a gutter workaround.

### D2 — Compass state is canonical; the tracker is a projection

Each issue carries a **Compass lifecycle state** tracked independently of its
issue-tracker status, because a Compass state (e.g. `Queued`) may not exist in the
user's tracker org. A user-editable **status mapping** projects Compass↔tracker in
both directions:

- A Compass state change (human or Dispatcher, e.g. `Backlog → Todo`) is
  **mirrored to the tracker** through `toTracker`.
- A tracker status change is read back through `fromTracker` (many-to-one:
  Linear's `Cancelled` and `Duplicate` both map to `Done` for now).
- The mapping lives in a **Settings screen** (D8). Trackers: Linear now, the shape
  is tracker-agnostic for Jira/GitHub later.

### D3 — Backlog view

A distinct view mirroring Linear's vertical issue list: **Todo issues first, then
Backlog issues, in collapsible sections**. Promotion actions (`Backlog → Todo`)
live here. It **also lists the user's tracker-assigned issues** (the user supplies
a tracker handle), so the human's own queue is visible alongside the fleet's.

### D4 — Done / archive view

An **archive** action clears Done issues out of the active surfaces; a **Done
view** (sibling to Backlog) lists archived/Done issues. Archiving sets an
`archivedAt` marker; it does not delete.

### D5 — Right sidebar mirrors Orca

> **Amended by
> [`../compass-dock-in-sidebar/design.md`](../compass-dock-in-sidebar/design.md).**
> The activity-bar model below stands; the tab set gains a fleet group
> (Supervisor/Warden/Status) above the workstream tabs (Files/VCS/PR), and the
> sidebar widens to host a full-height agent conversation.

Orca's right sidebar is an **icon-per-tab activity bar**, not stacked sections.
Evidence — `src/renderer/src/components/right-sidebar/activity-bar-buttons.tsx:18-29`:

```ts
export type ActivityBarItem = {
  id: ActiveRightSidebarTab
  icon: React.ComponentType<{ size?: number; className?: string }>
  title: string
  shortcut: string
  gitOnly?: boolean; folderOnly?: boolean; sshOnly?: boolean
}
```

with a per-tab status dot (`activity-bar-buttons.tsx:31-36`):

```ts
const STATUS_DOT_COLOR: Record<CheckStatus, string> = {
  success: 'bg-emerald-500', failure: 'bg-rose-500',
  pending: 'bg-amber-500', neutral: 'bg-muted-foreground'
}
```

Compass mirrors this with tabs **Files · VCS · PR**:

> **Amended (PR #467 review).** The frozen record specified six tabs
> (Files · VCS · Checks · History · Search · PR); implementation consolidated to
> **three**, and the maintainer ratified the consolidation: **Search** folds into
> a search box within Files, **History** (commit log) lives under the **VCS** tab
> beside the changed-file list, and **Checks** fold into the **PR** tab (a PR
> carries its own checks). Shipped as `RightSidebarTab = "files" | "vcs" | "pr"`
> (`store.ts:39`) with three activity-bar items (`constants.ts:71-73`). CodeRabbit
> flagged the deviation from the six-tab contract; this note reconciles the record
> to the ratified design.

- **Files is a full file viewer**, not files-changed (with a search box — the
  Search tab folds in here). The changed-file list moves
  to the **VCS** tab (commit + diff + PR compose/review), matching Orca's split
  between `FileExplorer.tsx` (a file explorer) and `SourceControl.tsx` (the
  VCS/changed-files surface) under `right-sidebar/`.
- **PR content** (description + threads/comments + reviewers + checks) gets its own
  tab rather than stacking under one column — the rationale Matt gave: a PR has too
  much content to stack.
- **Keep the workstream detail** the sidebar shows when a Bridge card is selected
  (unintended in #460 but wanted).
- **Sole Compass-specific addition:** a **repo + branch dropdown** that recognizes
  **multiple clones** in an agent's container and the **branches within each**
  (Orca is one-worktree-per-view; a Compass agent container may hold several
  clones, so the dropdown spans them).

### D6 — Agent view mirrors Orca's tab group

The agent view becomes a **tab group**:

- The **agent session (ACP conversation) is a tab**, and takes the full space by
  default. **Terminals are hidden by default** — not auto-opened.
- A **terminal tab** opens a full terminal; clicking back to the session tab
  returns to the conversation. **File tabs** render a file — a **read-only Markdown
  viewer first**; other file types and editing are deferred (resolved decision 2).
- **Split panes within the tab group** (row/column, nestable), so a session and a
  terminal (or a file) can sit side by side or stacked.

This extends the existing `TerminalLayout = "tab" | "split"` seam (`store.ts:27`)
into a general tab-group + split-tree model. Orca implements the same concept in
`src/renderer/src/store/slices/tabs.ts`, `tab-group-state.ts`, and
`headless-tab-group-split-layout.ts` (cited as prior art for the concept; Compass
builds its own SolidJS model, does not port the code).

### D7 — Bottom dock is tabbed: Dispatcher + Warden

> **Superseded by
> [`../compass-dock-in-sidebar/design.md`](../compass-dock-in-sidebar/design.md).**
> The bottom dock is removed: its Supervisor/Warden agent views (and a new Status
> tab for the metrics/feed facet) move into the right-sidebar activity bar as
> fleet tabs. The reasoning below is kept as the record of the earlier call.

The pinned bottom dock becomes **tabbed `[Dispatcher | Warden]`**:

- The **Dispatcher is a communicable full agent view** — the human can converse
  with it (an ACP conversation like any agent), not just read its metrics feed.
  The current metrics/feed (`DispatcherPane.tsx:12-96`) becomes one facet of that
  view.
- **Warden is a second tab** in the same dock — the security auditor
  (`compass-0.4` §seal/Warden) surfaced as a peer of the Dispatcher.

### D8 — Topbar cleanup

Drop the topbar **`.view-tabs`** nav (`App.tsx:39-68`) — the Bridge button plus
the last-selected-agent button. It duplicates the left sidebar, which already
pins a Bridge link and lists the agent tree (`LeftSidebar.tsx:98-129`). View
routing stays in the store; only the redundant topbar control is removed.

### D9 — Two state axes: workstream state vs agent state

Compass separates two axes that a one-agent-per-worktree ADE (Orca) conflates,
because Compass has a fleet of agents working a board of tasks — the *task* and
the *process* are distinct:

- **Workstream state** — where the *task* sits (the board columns, D1):
  `Backlog → Todo → Queued → Blocked ⇄ In Progress → In Review → Done`. **`Blocked`
  belongs to this axis** — the work is waiting on a dependency (another PR, a
  decision). It is **not** an agent state.
- **Agent state** — what the *running process* is doing (the dot beside the agent
  icon in the tree/roster/dock): `working / idle / waiting / done / paused / error`.

Orca's `AgentStateDot.tsx:17-30` is the reference for the *visual vocabulary* (the
glyphs and colors), not the membership — its union folds board-level `blocked`
into the agent dot because in Orca one agent *is* one task:

```ts
export type AgentDotState =
  | 'working' | 'blocked' | 'waiting' | 'interrupted' | 'done' | 'idle' | 'permission'
```

The grey-vs-green question is answered directly by its header comment
(`AgentStateDot.tsx:5-14`):

> "the dashboard uses a check icon so completion is visually distinct from 'idle'
> (grey dot) and the sidebar's 'active' (emerald dot) … one for *who* (agent icon)
> and one for *what state* (this indicator)."

Compass's **agent** axis borrows Orca's colors (`AgentStateDot.tsx:75,96,105-111`,
confirmed by `AgentStateDot.test.ts`):

| Agent state | Glyph | Meaning |
| --- | --- | --- |
| `working` | yellow spinner (`border-yellow-500`) | actively running |
| `idle` | **grey dot** (`bg-neutral-500/40`) | nothing to do |
| `waiting` | amber dot (`bg-amber-500`, "Waiting for input") | **asked for input (the "ask tool" state)** |
| `done` | **emerald check** (`text-emerald-500`, `CircleCheck`) | **finished a turn, not yet opened** |
| `paused` | amber attention | Warden security-pause (`compass-0.4` `pause_agent`) |
| `error` | red dot (`bg-red-500`) | **crashed/failed session** — provider error, tool-failure loop, unrecoverable exception; the human restarts or investigates |

**The agent dot is a UI projection over the daemon's `AgentSessionState`, not a
parallel enum.** #443 (SEA-1023, at the merge gate) lands the authoritative
agent-liveness contract in `compass.v1`
(`crates/compass-proto/proto/compass/v1/compass.proto` on
`livingstone-sea-1023-acp-impl`) — deliberately **coarse**, daemon-owned:

```proto
// AgentSessionStatus { session_id, state } streams on the event oneof (tag 12).
enum AgentSessionState {
  AGENT_SESSION_STATE_UNSPECIFIED = 0;
  AGENT_SESSION_STATE_STARTING = 1;
  AGENT_SESSION_STATE_READY = 2;
  AGENT_SESSION_STATE_WORKING = 3;
  AGENT_SESSION_STATE_STOPPED = 4;
  AGENT_SESSION_STATE_ERRORED = 5;
}
```

The daemon keeps this enum coarse **by design** — the UI derives the fine-grained
dot from the `AgentSessionState` transitions plus the `session/update` event
stream (`AgentMessageChunk`/`AgentToolCall`, tags 13–14). The UI dot is a
**presentation projection**, and MUST NOT redefine or extend `AgentSessionState`:

| UI dot | Derived from `AgentSessionState` / event stream |
| --- | --- |
| `working` | `WORKING` |
| `idle` | `READY` with no in-flight turn |
| `waiting` | `WORKING` + an ACP permission/`ask` request on the `session/update` stream (UI-only refinement) |
| `done` | a turn completed (end-of-turn on the stream) and `READY`, not yet opened by the human (UI-only refinement) |
| `error` | `ERRORED` |
| `paused` | Warden `pause_agent` (`compass-0.4`) — a Compass overlay, not an ACP/`AgentSessionState` value |

`STARTING` renders as `working` (spinner) until `READY`; `STOPPED` renders as
`idle`/absent. `waiting`, `done`, and `paused` are **UI/overlay refinements the
daemon enum doesn't carry** — they're derived client-side, never written back as
new `AgentSessionState` variants. If a future milestone needs any of them on the
wire, that is an **additive delta on the merged #443 contract, reviewed by the
agent-state owner** (D11), not a redefinition here.

This maps Matt's two named states exactly: **"ask tool used" → `waiting`** (amber);
**"just finished a turn, unopened" → `done`** (emerald check, deliberately *not*
idle grey). Reconciling the **post-#460** Compass `AgentState` (`stub-data.ts:28`
once #460 lands, `"working" | "idle" | "waiting" | "paused" | "error"`; the
pre-#460 skeleton on `main` has the narrower `"idle" | "working" | "waiting" |
"paused"`): the post-#460 five are kept and **`done` is added**. `error` is
retained as the crashed-session state (the human action — restart/investigate —
differs from unblocking a dependency), and is the *only* red agent state now that
`blocked` lives on the workstream axis. Orca's `permission`
maps to `waiting` and its `blocked`/`interrupted` have no agent-axis analogue
(board `Blocked` covers the task; `error` covers the crash).

### D10 — Issue-card → agent-UI jump

A workstream card (Bridge cell or Backlog row) opens the assigned agent's view via
a primary **"Open agent" button** in the workstream detail, plus **double-click**
on the card as an accelerator. Single-click keeps its current meaning (select →
populate the right sidebar); right/alt-click is reserved for a future context menu.
This reuses the existing `openAgent(agentId)` action (`store.ts:52`); the card
resolves its `assignee` to the agent id. A card with no assignee (none on the
active board, since every column is per-agent) has no jump target.

### D11 — Contract boundary: what stays UI-app-state vs what touches `compass.v1`

This record is overwhelmingly **UI/app-state** (the `AppStore` seam, the fixture,
the SolidJS surfaces) and touches no wire contract on its own. Two seams cross
into daemon territory and are governed explicitly so this design does not collide
with the `compass.v1` owner's in-flight work (#443, SEA-1023):

- **Agent liveness** is owned by `compass.v1` `AgentSessionState` (#443). The UI
  *consumes* it and derives the presentation dot (D9); it never redefines it. The
  UI-only refinements (`waiting`/`done`/`paused`) stay client-side.
- **Workstream/board state** (D1: `Backlog … Done`, the Linear mapping) is **not
  in `compass.v1` today** (absent from the proto on both `main` and #443). The
  maintainer has decided the board is **shared / daemon-persisted** (a fleet view
  the Dispatcher assigns on, not a per-user render), so board-state **will** need a
  daemon contract eventually — an **additive delta on #443**, owner-reviewed. That
  daemon board contract is a **separate later milestone**, not this record: here
  board-state is **UI-app-state**, modelled through the `AppStore` seam + fixture,
  with the `TrackerSeam` (T12) a UI-side interface; the fixture→`@compass/client`
  swap is where the persisted board contract lands when that milestone ships.
- **When either seam must cross onto the wire**, the change is an **additive delta
  on the merged #443 contract** — new payloads/RPCs at tags above #443's agent
  payloads (currently ≤15), behind the buf-breaking gate, following the same
  additive discipline #443 used over #332's `instance_epoch`. Any `compass.v1`
  edit touching the agent-state seam is **reviewed by the `compass.v1`/agent-state
  owner**, not landed unilaterally from the UI lane.

**Sequencing:** #443 lands the agent-state contract first; this record's
implementation consumes the merged enum/payloads. Nothing here proposes a
`.proto` edit — it is a docs-only design; any contract delta it later implies is a
separate, owner-reviewed PR.

## Plan

**Prerequisite: #460 must be merged first** (see Baseline). Right-sized slices,
each carrying its own `bun test` cycle. All types live in
`apps/ui/src/` and are consumed through the `AppStore` seam that #460
establishes.

### T1 — Workstream state model + constants

Rewrite the lifecycle union and the board/backlog partitions.

Interfaces:

```ts
// stub-data.ts
export type WorkstreamState =
  | "backlog" | "todo" | "queued" | "blocked"
  | "in_progress" | "in_review" | "done";

// constants.ts — board columns are the ACTIVE subset, in display order.
export const BOARD_LANES: Lane[] = [
  { state: "queued",      label: "Queued",      color: "var(--st-paused)" },
  { state: "blocked",     label: "Blocked",     color: "var(--st-blocked)" },
  { state: "in_progress", label: "In progress", color: "var(--st-working)" },
  { state: "in_review",   label: "In review",   color: "var(--st-review)" },
  { state: "done",        label: "Done",        color: "var(--st-merged)" },
];
/** Pre-active tier, Backlog-view order (Todo first). */
export const BACKLOG_STATES: readonly WorkstreamState[] = ["todo", "backlog"];
```

### T2 — Swimlane reads active-only

`Bridge.tsx` reads `BOARD_LANES` (5 columns); `boardAgents()` unchanged; no
backlog cells. Every column (Queued included) is per-agent, so the #460
unassigned-row finding is resolved by construction — no unassigned row exists.

Interfaces: no new types; `Bridge.tsx` `LANES` → `BOARD_LANES`; `laneTotal` /
`cellItems` operate over the active subset.

### T3 — Tracker-independent state + mapping type

Compass state is canonical; the tracker status is a projection.

Interfaces:

```ts
// stub-data.ts
export type TrackerKind = "linear" | "jira" | "github";

export interface Workstream {
  // …existing fields…
  state: WorkstreamState;               // canonical Compass state
  tracker?: {                            // the linked tracker issue, if any
    kind: TrackerKind;
    id: string;                          // e.g. "SEA-1042"
    status: string;                      // the tracker's native status name
    url: string;
  };
  archivedAt?: string;                   // set by the archive action (T5)
}

export interface TrackerStatusMapping {
  kind: TrackerKind;
  /** Compass state → the tracker's status name in this org. */
  toTracker: Record<WorkstreamState, string>;
  /** Tracker status name → Compass state (many-to-one). */
  fromTracker: Record<string, WorkstreamState>;
}
```

### T4 — Backlog view

New view: Linear-style vertical list, Todo section then Backlog section,
collapsible; promotion action; the user's tracker-assigned issues.

Interfaces:

```ts
// store.ts — View union + actions
export type View = "bridge" | "agent" | "backlog" | "done";
export interface AppStore {
  // …existing…
  showBacklog: () => void;
  /** Promote Backlog → Todo (and mirror to the tracker via the mapping). */
  promoteToTodo: (workstreamId: string) => void;
  /** The current user's tracker-assigned issues, for the Backlog view. */
  assignedIssues: Accessor<Workstream[]>;
}
```

### T5 — Done / archive view

Archive action + Done view.

Interfaces:

```ts
// store.ts
export interface AppStore {
  showDone: () => void;
  /** Archive a Done workstream (sets archivedAt; drops it from active surfaces). */
  archiveWorkstream: (workstreamId: string) => void;
}
```

### T6 — Right sidebar Orca-mirror (activity bar + tabs)

Icon-tab activity bar; **Files** (viewer + search box), **VCS** (changed files +
commit history), **PR** (description + threads + checks); keep the Bridge
workstream detail; add the repo/branch dropdown.

> **Amended (PR #467 review):** three tabs, not the originally-frozen six — see
> the D5 amendment note.

Interfaces:

```ts
// store.ts
export type RightSidebarTab = "files" | "vcs" | "pr";

export interface RepoClone {
  id: string;
  name: string;            // "sealedsecurity/sealed"
  branches: string[];
  currentBranch: string;
}

export interface AppStore {
  activeRightTab: Accessor<RightSidebarTab>;
  setActiveRightTab: (tab: RightSidebarTab) => void;
  /** Clones present in the selected agent's container. */
  agentRepos: Accessor<RepoClone[]>;
  activeRepoId: Accessor<string | null>;
  setActiveRepo: (repoId: string) => void;
  setActiveBranch: (branch: string) => void;   // within the active repo
}

// The activity-bar item shape mirrors Orca's ActivityBarItem.
export interface ActivityBarItem {
  id: RightSidebarTab;
  icon: Component<{ size?: number }>;
  title: string;
  shortcut?: string;
}
```

Note: this **replaces** the current `activeBranchWorkstreamId` /
`agentWorkstreams` branch model (`store.ts:79-83`) with the repo-aware
`agentRepos` / `activeRepoId` / branch model.

### T7 — Agent view tab group + splits

Session tab + terminal tabs + file tabs; terminals hidden by default; split tree.

Interfaces:

```ts
// store.ts
export type AgentTabKind = "session" | "terminal" | "file";

export interface AgentTab {
  id: string;
  kind: AgentTabKind;
  title: string;
  terminalId?: string;     // kind === "terminal"
  filePath?: string;       // kind === "file"
}

/** A binary split tree over agent tabs (leaves reference a tab by id). A split
 *  has exactly two children; deeper layouts nest splits. */
export type SplitNode =
  | { kind: "leaf"; tabId: string }
  | { kind: "split"; direction: "row" | "column"; left: SplitNode; right: SplitNode };

export interface AppStore {
  agentTabs: Accessor<AgentTab[]>;        // session tab first; terminals not auto-opened
  activeAgentTabId: Accessor<string | null>;
  openAgentTab: (tab: AgentTab) => void;
  closeAgentTab: (tabId: string) => void;
  splitLayout: Accessor<SplitNode>;
  setSplitLayout: (layout: SplitNode) => void;
}
```

Removes `terminalLayout` / `activeTerminalId` (`store.ts:73-78`) in favor of the
tab-group model.

### T8 — Bottom dock tabs (Dispatcher + Warden)

Tabbed dock; Dispatcher gains a communicable agent view.

Interfaces:

```ts
// store.ts
export type DockTab = "dispatcher" | "warden";
export interface AppStore {
  activeDockTab: Accessor<DockTab>;
  setActiveDockTab: (tab: DockTab) => void;
}
```

The Dispatcher and Warden are represented as `Agent`s (they already have
`role: "dispatcher" | "warden"`, `stub-data.ts:31`), so the communicable view
reuses the T7 agent view; the metrics feed becomes a facet.

### T9 — Topbar cleanup

Remove the `.view-tabs` nav (`App.tsx:39-68`) and its CSS. Left sidebar is nav.
No new types; `store.view()` routing retained.

### T10 — Agent-state vocabulary

Reconcile `AgentState`; the state-dot component + labels.

Interfaces:

```ts
// stub-data.ts
export type AgentState =
  | "working" | "idle" | "waiting" | "done" | "paused" | "error";

// constants.ts
export const AGENT_STATE_LABEL: Record<AgentState, string> = {
  working: "Working",
  idle: "Idle",
  waiting: "Waiting for input",
  done: "Done",
  paused: "Paused",
  error: "Error",
};
```

Dot rendering (Orca's colors, `AgentStateDot.tsx`): `working`→yellow stepped
spinner, `idle`→grey, `waiting`→amber, `done`→emerald check icon, `paused`→amber
attention (Warden-pause), `error`→red (crashed session). `blocked` is a workstream
state (D1/D9), not rendered on the agent dot. The dot is **derived from #443's
`AgentSessionState` stream** (D9 mapping table), not a parallel enum — this task
writes the projection + the component, not a new wire state.

### T11 — Settings screen (status mapping + tracker handle)

Editor for `TrackerStatusMapping` + the user's tracker handle.

Interfaces:

```ts
// store.ts
export interface TrackerConfig {
  kind: TrackerKind;
  handle: string;                        // the user's tracker identity
  mapping: TrackerStatusMapping;
}
export interface AppStore {
  trackerConfig: Accessor<TrackerConfig | null>;
  setTrackerConfig: (cfg: TrackerConfig) => void;
}
```

Document the thin seam the store calls; the fixture implements it in-memory now,
`@compass/client` implements it against the daemon later. **This is a UI-side
interface, not a `compass.v1` change** (D11): board/tracker state is UI-app-state
for this record, so `TrackerSeam` is a client seam. If a later milestone moves it
onto the wire, that PR carries the additive `compass.v1` delta under the
agent-state owner's review — not this record.

Interfaces:

```ts
export interface TrackerSeam {
  listAssignedIssues(handle: string): Promise<Workstream[]>;
  /** Map compassState via TrackerStatusMapping.toTracker, then write it. */
  updateIssueStatus(id: string, compassState: WorkstreamState): Promise<void>;
}
```

## Tasks

- [ ] T1 — `WorkstreamState` union + `BOARD_LANES`/`BACKLOG_STATES` (`stub-data.ts`, `constants.ts`).
- [ ] T2 — `Bridge.tsx` reads `BOARD_LANES`; active-only swimlane.
- [ ] T3 — `Workstream.state`/`tracker`/`archivedAt` + `TrackerStatusMapping`.
- [ ] T4 — Backlog view (`View: "backlog"`, `promoteToTodo`, `assignedIssues`).
- [ ] T5 — Done/archive view (`View: "done"`, `archiveWorkstream`).
- [ ] T6 — Right sidebar activity bar + tabs + repo/branch dropdown.
- [ ] T7 — Agent view tab group + split tree; terminals hidden by default.
- [ ] T8 — Bottom dock tabs; communicable Dispatcher + Warden tab.
- [ ] T9 — Drop topbar `.view-tabs`.
- [ ] T10 — `AgentState` reconcile + state-dot component.
- [ ] T11 — Settings screen (status mapping + tracker handle).
- [ ] T12 — `TrackerSeam` documented; fixture implementation.
- [ ] markdownlint this record clean; update the living spec pointer if warranted.

## Resolved decisions (from maintainer review)

These forks were surfaced during design and resolved by the maintainer; the record
is written to match. Recorded here so the reasoning survives the freeze.

1. **Issue-card → agent-UI jump gesture.** A primary **"Open agent" button** in
   the workstream detail, **plus double-click** on the card as an accelerator
   (right/alt-click reserved for a context menu).
2. **File tabs in the agent view.** **Markdown viewer first, read-only** — the
   agent owns the git clone, so concurrent human edits are fraught; other file
   types then editing are deferred to a later record.
3. **Multi-repo model.** The repo/branch dropdown is built **multi-repo-capable
   now**, but the fixture shows a single clone until the daemon reports more.
4. **Backlog vs Done view shape.** Two views: a **Backlog view** (Todo + Backlog)
   and a **separate Done/archive view** (D3/D4).
5. **Two state axes (`blocked`, `error`, `paused`).** `blocked` is a **workstream**
   state, not an agent state (D9). The agent axis is
   `working / idle / waiting / done / paused / error`, where **`error` = a crashed
   session** (the red agent state; human restarts/investigates) and **`paused` =**
   a Warden security-pause (`compass-0.4` `pause_agent`).
6. **Record slug.** `compass-ade-shell` — a shell/UX record, not a
   strategic-posture revision like `compass-0.4`.
7. **Dispatcher/Warden dock vs the Bridge workstream detail.** Independent — the
   bottom dock is agent-conversation, the right sidebar is workstream/VCS; both may
   be open at once, no coupling.
8. **`Queued` scope.** **Per-agent** — that agent's up-next queue, rendered as a
   per-agent swimlane cell (D1). The global pool of promoted-but-unassigned tasks
   the Dispatcher assigns from is **`Todo`**, a distinct scope in the Backlog view,
   not a board column.
9. **Board-state scope: shared / daemon-persisted.** The Compass board is a shared
   fleet view (all observers on a workstream see the same state), not a per-user
   render — so board-state must be daemon-persisted, landing as an **additive
   `compass.v1` delta on #443** in a **separate later daemon board-state milestone**
   (not this record's UI slices), owner-reviewed (D11, §Spec impact). This record
   models the board as UI-app-state (fixture); the agent-state seam is untouched —
   only board/workstream state is added later, never overloading `AgentSessionState`.

## Spec impact

**`Spec-impact: none` for this docs-only PR** — it changes no `compass.v1`
contract and adds no `### Requirement:` to `docs/specs/product/compass.md`. But
the record has a **contract dependency and a decided future impact** that the
implementing PRs will carry:

- **Depends on #443 (SEA-1023).** #443 lands the `AgentSessionState` contract +
  agent-session payloads in `compass.v1` and updates `docs/specs/product/compass.md`
  in that PR. This UI *consumes* it and derives the agent dot (D9); it never
  modifies it. The agent-state seam is untouched by this record.
- **Board-state is shared / daemon-persisted (maintainer decision).** The Compass
  board is a shared fleet view — the Dispatcher assigns work and multiple observers
  (and the daemon) must agree on each workstream's state — so board-state is **not**
  client-local. It MUST be daemon-persisted, i.e. an **additive delta on the merged
  #443 contract** (new board/workstream payloads + RPCs, at tags above #443's agent
  payloads, behind the buf-breaking gate, reviewed by the `compass.v1`/agent-state
  owner). A **future daemon board-state milestone** — separate from this record's
  T1–T12 UI slices — carries that `.proto` delta + the `specs/product/compass.md`
  requirements, never this docs-only record, and never as a redefinition of the
  agent-state enum (D11). This record's T1–T12 stay UI-app-state (fixture).
- **The `TrackerSeam` (T12) is UI-side**; the Compass↔tracker projection lives in
  the daemon behind the same additive delta when the board-state contract lands.
- When T1–T12 land, each slice's PR reconciles `docs/specs/product/compass.md` with
  the built behavior, per the docs convention.

## Global Constraints

- **`docs/designs/<domain>/` records are frozen once decided** (`docs/README.md`,
  `docs/designs/platform/docs-system.md`): this is a **new** record; it never
  rewrites `../compass.md` (v0.3) or `../compass-0.4/design.md`.
- **The living spec** (`docs/specs/product/compass.md`) states **only built
  behavior** as `### Requirement:` + `#### Scenario:`. This UI is largely unbuilt,
  so the spec gets at most a design-record pointer + forward-looking prose — **no
  fabricated contracts.**
- **One store, one pattern.** All state extends the `AppStore` seam (`store.ts`);
  no second state-management library or parallel store.
- **The fixture is the seam.** New shapes go in `stub-data.ts` and are read through
  `store.ts` accessors, so the eventual swap to the generated `@compass/client` is
  a one-module change (`stub-data.ts:10-14`).
- **No agent-product or persona names** in this record. Orca is named as the
  mirrored external ADE (real, cited); Linear/Jira/GitHub as real trackers.
  Coding-agent references only as the existing `AgentKind` literals
  (`omp`/`claude`/`codex`/`opencode`/`seal`, `stub-data.ts:35`).
- **planning-evidence:** every claim about Orca carries a file + line + quoted
  snippet (`rule://planning-evidence`); the quotes above were read this session
  from `raw.githubusercontent.com/stablyai/orca/HEAD/...`.
- Commit `docs(product): <change>` + `Co-Authored-By: seal <noreply@sealedsecurity.com>`;
  markdownlint clean.

## Risks

- **Store churn.** T6–T8 replace existing store members (`terminalLayout`,
  `activeTerminalId`, `activeBranchWorkstreamId`, `agentWorkstreams`). Sequence
  T1–T3 (state) before T6–T8 (view) so the board is stable while the view model
  changes; land each slice with its tests green.
- **Tracker mapping surface.** The Compass↔tracker projection (D2) is easy to
  over-build. Scope the first cut to Linear + the identity/`Cancelled`+`Duplicate`
  cases; the `TrackerKind` union keeps Jira/GitHub open without building them.
- **Orca drift.** Mirroring pins to Orca's current UI; its `HEAD` moves. The cited
  lines are a point-in-time snapshot — re-verify on implementation, don't treat the
  mirror as a live contract.
