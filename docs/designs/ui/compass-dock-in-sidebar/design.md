# Compass ADE shell — fold the bottom dock into the right sidebar

Status: Active

> Internal design record — July 2026. A UI/UX design pass on the Compass ADE dev
> shell, amending the frozen shell record
> ([`../compass-ade-shell/design.md`](../compass-ade-shell/design.md), merged in
> #461, built by #467). It supersedes that record's **D7** (the bottom dock),
> amends its **D5** (the right sidebar), and deliberately reverses its
> **resolved decision 7** (dock/sidebar independence). The frozen record is not
> edited — records freeze on merge; this record carries the delta.

## Problem / Intent

The maintainer's hands-on feedback on the shipped shell, verbatim:

> The bottom pane is too small to read anything in an agent session. Just making
> it taller eats too much screen. Change it so it's a sidebar — bake it into the
> right-hand sidebar (so you can have Supervisor, Warden, OR the status info
> open). The right sidebar then needs to be larger by default. We'll also need to
> resize both sidebars, but that can come later. The goal: the user can look at
> the swimlane board while also talking with the supervisor.

The pinned bottom dock (ADE-shell D7, `../compass-ade-shell/design.md:192-201`:
"The pinned bottom dock becomes **tabbed `[Dispatcher | Warden]`**", shipped as
`[Supervisor | Warden]`) is capped at `max-height: 260px` (`app.css:1229`) — too
short to read an ACP conversation, and stealing that height from the board.
Intent: remove the dock row entirely and move its two communicable agent views
into the right sidebar's activity bar (ADE-shell D5,
`../compass-ade-shell/design.md:134-172`) as first-class tabs, alongside a new
Status tab carrying the dock's metrics/feed facet — so a full-height, readable
Supervisor conversation can sit beside the swimlane board.

This **reverses ADE-shell resolved decision 7**
(`../compass-ade-shell/design.md:654-656`: "Independent — the bottom dock is
agent-conversation, the right sidebar is workstream/VCS; both may be open at
once, no coupling"). The maintainer made that reversal deliberately: the dock
was too short to read, and a full-height sidebar column fixes it. The cost —
you can no longer see a fleet conversation and a workstream pane at the same
time — is accepted; the sidebar becomes one radio group over both.

All file references are relative to `apps/ui/src/` unless prefixed,
and cite the post-#467 tree (branch `cook-compass-ade-tabs`, head `ad29c49f`).
**#467 must be merged before T1 begins.**

## Approach

Extend the existing activity-bar seam rather than inventing a second container:
the right sidebar already renders an icon-per-tab activity bar
(`RightSidebar.tsx:409-427`) driven by one store signal
(`activeRightTab`, `store.ts:347-348`) and one exhaustive tab table
(`RIGHT_SIDEBAR_TAB_BY_ID`, `constants.ts:68-74`). The dock's two agent views
become three new tabs in that same bar (Supervisor, Warden, Status), grouped
above the existing workstream tabs with a divider. The dock's grid row, CSS
block, component, and store state are then deleted outright — no shims.

Two alternatives were pre-empted by the feedback itself or the brief:

- **Taller / resizable dock** — rejected verbatim ("just making it taller eats
  too much screen"). The vertical dock competes with the board for the scarce
  axis; the sidebar column is the abundant one.
- **A second active-tab signal** (keep `activeDockTab` beside `activeRightTab`)
  — rejected: one activity bar is one radio group, so one signal
  (`activeRightTab`) absorbs the fleet tabs and the dock signals are removed
  (D1). Two signals would imply two independently-visible surfaces, which is
  exactly the layout being removed.

A structural constraint shapes the plan: the tab table is a mapped object keyed
on the full `RightSidebarTab` union — "TypeScript rejects the module unless
EVERY tab has an activity-bar entry" (`constants.ts:64-67`), and the rendered
bar derives from it ("adding a tab to the union forces an entry above, and that
entry appears here automatically", `constants.ts:76-79`). So a union change, its
tab entries, and their rendered panes are compile-coupled and must land in the
same slice. Tasks below are therefore vertical (store + constants + render +
CSS + tests), split by tab set, with the dock deletion as its own slice.

## Decisions

D1–D7 below are this record's decisions. The superseded record's decisions are
always cited as *ADE-shell Dn*.

### D1 — One activity bar, one signal: the dock state is removed

`activeRightTab` absorbs the fleet tabs. The union
(`store.ts:39`: `export type RightSidebarTab = "files" | "vcs" | "pr";`) widens
to six members, split into named subsets so the grouped bar and the
chrome-hiding rule (D5) key off types, not string lists:

```ts
export type FleetTab = "supervisor" | "warden" | "status";
export type WorkstreamTab = "files" | "vcs" | "pr";
export type RightSidebarTab = FleetTab | WorkstreamTab;
```

The dock's store surface is deleted, not aliased — every member, signal, and
exposure site:

- `export type DockTab = "supervisor" | "warden";` (`store.ts:34`)
- `dockOpen: Accessor<boolean>` / `toggleDock: () => void`
  (`store.ts:216-217`; signal `store.ts:339`, definition `store.ts:511`,
  exposed `store.ts:644-645`)
- `activeDockTab: Accessor<DockTab>` / `setActiveDockTab: (tab: DockTab) => void`
  (`store.ts:219-220`; signal `store.ts:340`, exposed `store.ts:646-647`)

`rightOpen`/`toggleRight` (`store.ts:213-214`) are retained unchanged — closing
the sidebar hides the fleet tabs too, and the existing topbar pane toggle
restores it (`App.tsx:101-109`).

### D2 — Grouped activity bar: fleet on top, divider, workstream below

Resolved by the maintainer (not an open fork): the bar renders two groups —
the **fleet** group (Supervisor · Warden · Status) on **top**, a divider, then
the **workstream** group (Files · VCS · PR). Rationale: the fleet agents are
always-on and are the new primary use (talk to the Supervisor while watching
the board); the workstream tabs are card-scoped (they follow the selected
Bridge card).

`ActivityBarItem` (`constants.ts:53-59`) gains a group discriminator and an
optional agent reference:

```ts
export type RightTabGroup = "fleet" | "workstream";

export interface ActivityBarItem {
  id: RightSidebarTab;
  /** Single-glyph icon. */
  icon: string;
  /** Short label under the icon / for the tooltip. */
  title: string;
  /** Activity-bar group: fleet renders above the divider, workstream below. */
  group: RightTabGroup;
  /** Fleet agent tabs: the agent whose StateDot badges the tab icon. */
  agentId?: string;
}
```

The per-tab agent state dot carries over from the dock, which renders a
`StateDot` inside each dock tab today (`BottomDock.tsx:93-95`); the fleet
activity-bar tabs keep that at-a-glance liveness by overlaying the same
`StateDot` component (`StateDot.tsx:16`,
`export const StateDot: Component<{ state: AgentState }>`) on the tab icon,
resolved through `agentId`. This also matches the per-tab status dot the
frozen record already established as the external ADE mirror for activity-bar
tabs (ADE-shell D5, `../compass-ade-shell/design.md:149-156`) — no new mirror
claim is made here.

Icons: Supervisor `◆` and Warden `🛡` are the dock's existing glyphs
(`BottomDock.tsx:19-22`); Status gets `▦` ("Fleet status"). The tab buttons
reuse the existing `.r-tab` pattern — `classList` active state, `title` +
`aria-label` + `aria-pressed` (`RightSidebar.tsx:412-424`).

### D3 — Supervisor/Warden are pure full-height conversations; Status is its own tab

Resolved by the maintainer (not an open fork): the Supervisor and Warden tabs
render **only** a full-height ACP conversation — no metrics facet stealing
height; a readable conversation is the whole point. The Supervisor's fleet
metrics + recent-decision feed move to a separate **Status** tab.

Salvage map from the deleted dock:

| Dock piece | Source | New home |
| --- | --- | --- |
| `AcpConversation` render | `BottomDock.tsx:130` | Supervisor/Warden panes render `<AcpConversation agent={…} />` full-height; the component is already shared and exported (`AgentView.tsx:51-52`: "Shared by the agent-view session pane and the bottom dock (D7)") |
| `AGENT_BY_DOCK_TAB` | `BottomDock.tsx:14-17` | Dissolved into `ActivityBarItem.agentId` (D2) — one source of truth, no separate map |
| `SupervisorFacet` (metrics + feed) | `BottomDock.tsx:26-65` | `StatusPane` in `RightSidebar.tsx`, full-width; metrics logic extracted as a pure, exported `fleetMetrics` helper (T2) |
| `.dock-facet/.dock-metrics/.dock-feed` CSS | `app.css:1295-1354` | Rehomed under a `.r-status*` namespace (T2); the 300px facet width cap (`app.css:1297`) is dropped — the pane fills the sidebar |
| `.dock`, `.dock-head`, `.dock-tab*`, `.dock-spacer`, `.dock-sub`, `.dock-body` CSS | `app.css:1221-1293`, `1357-1361` | Deleted (T3) |

Accepted losses, deliberate: the dock's collapse-to-header-row affordance
(`BottomDock.tsx:109-117`) and its `model · state` subtitle line
(`BottomDock.tsx:104-107`) have no sidebar equivalent. State stays visible via
the activity-bar `StateDot` (its `title`/`aria-label` carry the human label,
`StateDot.tsx:22-23`); the model name is no longer shown on this surface.

The Status pane's content is an exact salvage: workstream counts (`active` =
`in_progress` + `in_review`, `queued`, `todo`, `blocked` — the `countState`
logic at `BottomDock.tsx:28-30,33-52`) over `store.workstreams()`, plus the
Supervisor's `feed` (`BottomDock.tsx:53-62`; `Agent.feed`, `stub-data.ts:195`).
It works with no workstream selected, so no fleet tab ever shows the
`SelectPrompt` fallback the workstream tabs use (`RightSidebar.tsx:378`).

### D4 — Grid loses the dock row; the sidebar widens via `--right-w`

The shell grid (`app.css:101-111`) currently reserves row 3 for the dock:

```css
grid-template-rows: var(--topbar-h) 1fr auto var(--usage-h);
grid-template-areas:
  "topbar   topbar  topbar"
  "left     main    right"
  "dispatch dispatch dispatch"
  "usage    usage   usage";
```

The `dispatch` row and area are removed — 4 rows become 3:

```css
grid-template-rows: var(--topbar-h) 1fr var(--usage-h);
grid-template-areas:
  "topbar topbar topbar"
  "left   main   right"
  "usage  usage  usage";
```

Since `.right` occupies only the middle row (`grid-area: right`,
`app.css:998`), deleting the dock row automatically extends both sidebars and
the main pane to full height between topbar and usage bar — that is the
mechanism giving the conversation its height.

Width: `.right { width: 300px; … }` (`app.css:997-1005`) is too narrow for a
readable conversation (the dock gave its facet alone 300px,
`app.css:1295-1302`). A `--right-w` variable is added to `:root`
(`app.css:6-52`) and `.right` consumes it:

```css
:root {
  --right-w: 400px;
}

.right {
  width: var(--right-w);
}
```

**400px** is the locked default — the midpoint of the delegated 380–420px
range, comfortable for the 13px/1.55 conversation text (`.block-text`,
`app.css:878-881`). The variable is the single knob the deferred resize task
(T4) will drive.

### D5 — Card-scoped chrome hides on fleet tabs

`.r-main` unconditionally renders the workstream detail header and the
repo/branch dropdown above the pane (`RightSidebar.tsx:373-374`:
`<Show when={ws()}>{(w) => <WorkstreamDetailHead w={w()} />}</Show>` +
`<RepoBranchDropdown />`; components at `RightSidebar.tsx:222` and
`RightSidebar.tsx:252`). Both are card-scoped and meaningless above a
Supervisor chat, so when the active tab's group is `fleet` they are not
rendered; when a workstream tab is active the sidebar renders exactly as
today. The predicate derives from the tab table — no parallel list:

```ts
const fleetActive = (): boolean =>
  RIGHT_SIDEBAR_TAB_BY_ID[store.activeRightTab()].group === "fleet";
```

### D6 — Default tab is Supervisor; card selection does not steal the tab

Two locked implementer choices on the new signal's behavior:

- **Boot default `"supervisor"`** (today `"files"`, `store.ts:347-348`). The
  shell boots onto the Bridge board (`store.test.ts:67`), so the default
  layout is exactly the maintainer's goal: board + Supervisor conversation
  side by side. It also preserves the dock's default surface
  (`createSignal<DockTab>("supervisor")`, `store.ts:340`).
- **No auto-switch on card select.** Selecting a Bridge card
  (`selectWorkstream`, `store.ts:206`) keeps the active tab; it only updates
  what the workstream tabs will show when visited. Auto-jumping to a
  workstream tab would yank the Supervisor conversation away mid-thought —
  the opposite of the always-on-fleet rationale (D2). Maintainer-confirmed
  (Resolved decisions #1).

### D7 — Sidebar resize is deferred; the variable seam lands now

Resizing both sidebars is explicitly deferred by the maintainer ("can come
later"). This record does **not** design the drag interaction; it only lays the
seam: `--right-w` ships in D4/T1, and the left sidebar's hardcoded
`width: 244px` (`app.css:255-257`) is noted as the future `--left-w`
counterpart. T4 is the stub carrying the follow-up contract.

## Plan

Prerequisite: **#467 merged** (this record cites its tree). Vertical,
compile-coupled slices (see Approach), each landing with its own `bun test`
cycle in `apps/ui` — tests first, red → green, written via the
Tester. All changes live under `apps/ui/src/` and extend the
`AppStore` seam; no new state pattern.

### T1 — Fleet conversations move into the sidebar

The sidebar grows the fleet group with the two conversation tabs; the dock
still exists (untouched) until T3, so this slice is purely additive and the UI
stays functional at every commit.

Scope: split/widen the union + flip the default (`store.ts:39`, `store.ts:347-348`;
update the stale tab-list doc comments at `store.ts:36-38` and `store.ts:228`);
add the group model + two fleet entries (`constants.ts:53-82`); render groups +
divider + state dots in the activity bar and the two ACP panes
(`RightSidebar.tsx:361-431`); hide card-scoped chrome on fleet tabs (D5); add
the fleet CSS + `--right-w` (D4 width only — the grid row survives until T3).

Interfaces:

```ts
// store.ts — replaces RightSidebarTab ("files" | "vcs" | "pr", store.ts:39).
// T2 widens FleetTab with "status".
export type FleetTab = "supervisor" | "warden";
export type WorkstreamTab = "files" | "vcs" | "pr";
export type RightSidebarTab = FleetTab | WorkstreamTab;

// store.ts:347-348 — the boot default flips "files" → "supervisor" (D6):
const [activeRightTab, setActiveRightTab] =
  createSignal<RightSidebarTab>("supervisor");

// constants.ts — ActivityBarItem (constants.ts:53-59) gains group + agentId:
export type RightTabGroup = "fleet" | "workstream";

export interface ActivityBarItem {
  id: RightSidebarTab;
  icon: string;
  title: string;
  group: RightTabGroup;
  agentId?: string;
}

// constants.ts:68-74 — the mapped object gains the fleet entries (declaration
// order = render order, fleet first) and is now EXPORTED for the D5 predicate:
export const RIGHT_SIDEBAR_TAB_BY_ID: {
  [K in RightSidebarTab]: ActivityBarItem & { id: K };
} = {
  supervisor: {
    id: "supervisor",
    icon: "◆",
    title: "Supervisor",
    group: "fleet",
    agentId: "agent-supervisor",
  },
  warden: {
    id: "warden",
    icon: "🛡",
    title: "Warden",
    group: "fleet",
    agentId: "agent-warden",
  },
  files: { id: "files", icon: "🗀", title: "Files", group: "workstream" },
  vcs: { id: "vcs", icon: "⎇", title: "Version control", group: "workstream" },
  pr: { id: "pr", icon: "⇄", title: "Pull request", group: "workstream" },
};

// constants.ts:80-82 — RIGHT_SIDEBAR_TABS (flat) is REPLACED by the grouped
// view; its sole consumer is the activity bar (RightSidebar.tsx:9,410):
export const RIGHT_SIDEBAR_TAB_GROUPS: readonly {
  group: RightTabGroup;
  items: readonly ActivityBarItem[];
}[] = (["fleet", "workstream"] as const).map((group) => ({
  group,
  items: Object.values(RIGHT_SIDEBAR_TAB_BY_ID).filter(
    (t) => t.group === group,
  ),
}));

// RightSidebar.tsx — component-local helpers. Agent resolution mirrors the
// dock's STUB_AGENTS.find pattern (BottomDock.tsx:69-70,78-79); no new store
// surface, no new fixture shape.
const fleetActive = (): boolean =>
  RIGHT_SIDEBAR_TAB_BY_ID[store.activeRightTab()].group === "fleet";
const agentFor = (item: ActivityBarItem): Agent | undefined =>
  item.agentId ? STUB_AGENTS.find((a) => a.id === item.agentId) : undefined;
```

```css
/* app.css — new, alongside the existing .r-activity/.r-tab block
 * (app.css:2074-2106). --right-w + .right per D4. */
.r-activity-divider {
  height: 1px;
  margin: 4px 6px;
  background: var(--border-strong);
}

.r-tab {
  position: relative; /* added property; block otherwise as app.css:2084-2095 */
}

.r-tab .state-dot {
  position: absolute;
  right: 3px;
  bottom: 3px;
}

/* Fleet panes host a flex conversation, not a scrolling document — the
 * counterpart of .r-pane { overflow-y: auto } (app.css:1945-1948). The
 * border-right suppression mirrors the dock's own .acp override
 * (app.css:1357-1361) against .acp's base border (app.css:835). */
.r-pane.fleet {
  display: flex;
  overflow: hidden;
}

.r-pane.fleet .acp {
  flex: 1;
  min-height: 0;
  border-right: none;
}
```

Render: the `.r-activity` nav (`RightSidebar.tsx:409-427`) iterates
`RIGHT_SIDEBAR_TAB_GROUPS` with a `.r-activity-divider` between groups; fleet
tabs append `<StateDot state={agentFor(item).state} />`. The pane `Switch`
(`RightSidebar.tsx:376-405`) gains supervisor/warden matches rendering
`<AcpConversation agent={…} />` (import from `./AgentView` exactly as the dock
does, `BottomDock.tsx:5`); `.r-pane` gains `classList={{ fleet: fleetActive() }}`;
the two chrome components wrap in `<Show when={!fleetActive()}>` (D5).

Tests (red first): `store.test.ts` — boot default is `"supervisor"`;
`setActiveRightTab` reaches a fleet and a workstream value.
`RightSidebar.test.ts` (currently pure-helper tests only, `filterFileTree`,
`RightSidebar.test.ts:3,28`) — `RIGHT_SIDEBAR_TAB_GROUPS` partitions the union
exactly: fleet group first, every `RightSidebarTab` appears exactly once, fleet
items carry `agentId`.

### T2 — The Status tab

Scope: widen `FleetTab` with `"status"` (the mapped object then rejects the
module until the sixth entry exists — the exhaustiveness lever,
`constants.ts:64-67`); add the entry; add `StatusPane` + the extracted pure
metrics helper; rehome the facet CSS. The dock still renders its own facet
until T3 — one slice of deliberate duplication, then the original dies.

Interfaces:

```ts
// store.ts — FleetTab gains the status tab (D3):
export type FleetTab = "supervisor" | "warden" | "status";

// constants.ts — the compile-forced sixth entry (fleet group, no agentId —
// Status is a pane, not an agent conversation):
status: { id: "status", icon: "▦", title: "Fleet status", group: "fleet" },

// RightSidebar.tsx — the salvaged SupervisorFacet metrics (BottomDock.tsx:26-52),
// extracted pure and exported for tests exactly like filterFileTree
// (RightSidebar.test.ts:3):
export interface FleetMetrics {
  /** in_progress + in_review, the dock's "active" count (BottomDock.tsx:35). */
  active: number;
  queued: number;
  todo: number;
  blocked: number;
}
export function fleetMetrics(workstreams: readonly Workstream[]): FleetMetrics;

// RightSidebar.tsx — the pane: metrics strip over store.workstreams(), then
// the Supervisor's feed (Agent.feed, stub-data.ts:195), resolved via
// RIGHT_SIDEBAR_TAB_BY_ID.supervisor.agentId:
const StatusPane: Component = () => JSX;
```

```css
/* app.css — the .dock-facet/.dock-metrics/.dock-feed styles
 * (app.css:1295-1354) rehomed; the 300px facet cap (app.css:1297) and the
 * facet border-right (app.css:1300) are dropped — the pane fills the sidebar.
 * Inner .m-val/.m-label/.at class names carry over unchanged. */
.r-status {
  display: flex;
  flex-direction: column;
  min-height: 0;
}

.r-status-metrics {
  /* from .dock-metrics, app.css:1304-1311 */
}

.r-status-metric {
  /* from .dock-metric (+ .m-val/.m-label), app.css:1313-1330 */
}

.r-status-feed {
  /* from .dock-feed, app.css:1332-1339 */
}

.r-status-feed-item {
  /* from .dock-feed-item (+ .at), app.css:1341-1354 */
}
```

Tests (red first): `fleetMetrics` — counts by state over a hand-built
workstream list (active sums `in_progress` + `in_review`; `blocked` counted
separately; states outside the four buckets ignored); groups-partition test
updated for the sixth tab.

### T3 — Remove the dock

Scope: delete the component, its store state, its render sites, its CSS block,
and the grid row — the change that makes the fold real. After T1/T2 the
sidebar already carries every dock capability, so this slice is pure deletion.

Interfaces (removals, with every definition/exposure site):

```ts
// store.ts — REMOVED:
export type DockTab = "supervisor" | "warden"; // store.ts:34
dockOpen: Accessor<boolean>; // store.ts:216; signal store.ts:339; exposed store.ts:644
toggleDock: () => void; // store.ts:217; defined store.ts:511; exposed store.ts:645
activeDockTab: Accessor<DockTab>; // store.ts:219; signal store.ts:340; exposed store.ts:646
setActiveDockTab: (tab: DockTab) => void; // store.ts:220; exposed store.ts:647
```

File-level removals:

- `components/BottomDock.tsx` — deleted whole (`AGENT_BY_DOCK_TAB`,
  `DOCK_TABS`, `SupervisorFacet`, and the render body go with it; their
  survivors were rehomed in T1/T2 per the D3 salvage map).
- `App.tsx` — the `BottomDock` import (`App.tsx:5`), the unconditional
  `<BottomDock />` (`App.tsx:138`), and the dock pane-toggle button
  (`App.tsx:92-100`, the `▂` button calling `store.toggleDock()`); update the
  shell header comment (`App.tsx:16-26`, which still describes "a pinned
  bottom dock").
- `app.css` — the grid row + `dispatch` area (`app.css:101-111`, per D4); the
  whole dock block (`app.css:1221-1361`); the file header comment
  (`app.css:1-4`, "a pinned bottom dock ([Supervisor | Warden])").

Tests (updated in the same slice — the removal breaks them by design):

- `store.test.ts:76` — the boot assertion `expect(s.dockOpen()).toBe(true)`.
- `store.test.ts:350-398` — the pane-toggles matrix: the dock case
  (`store.test.ts:372-377`) is removed and the `others` arrays for left/right
  (`store.test.ts:364,370`) drop their `dockOpen` probes.
- `store.test.ts:400-413` — the "dock tab selection (D7)" describe is removed;
  its intent (both fleet agents reachable) is already covered by T1's
  `setActiveRightTab` tests.

`bun test` green across the package closes the slice; `vite dev` smoke: board +
full-height Supervisor conversation side by side, no dock row.

### T4 — Deferred: sidebar resize handles

**Deferred — not part of this change's implementation PRs.** Recorded so the
follow-up lands on a clean seam instead of re-designing the widths.

Scope when picked up: drag handles on both sidebars writing the two width
variables; `--right-w` exists from T1; `--left-w` is introduced then (the left
sidebar's width is hardcoded `244px` today, `app.css:255-257`). The drag
interaction, clamping bounds, and persistence are intentionally **not**
designed here.

Interfaces (the follow-up's store contract, fixed now so the variable seam
stays stable):

```ts
// store.ts — deferred follow-up only:
leftWidth: Accessor<number>; // px, drives --left-w
rightWidth: Accessor<number>; // px, drives --right-w
setLeftWidth: (px: number) => void; // clamped
setRightWidth: (px: number) => void; // clamped
```

### Final gate

`bun test` green in `apps/ui`; `markdownlint` clean on this record;
living-spec check: `docs/specs/product/compass.md` contains no dock references
(zero matches for `dock|Dock` this session), so no spec pointer needs updating
— re-verify at execution time.

## Tasks

- [ ] T1 — Fleet conversations into the sidebar: union split + `"supervisor"`
      default (`store.ts:39,347-348`), grouped `ActivityBarItem` +
      `RIGHT_SIDEBAR_TAB_GROUPS` (`constants.ts:53-82`), activity-bar groups +
      divider + state dots + ACP panes + chrome hide
      (`RightSidebar.tsx:361-431`), `--right-w: 400px` + fleet CSS. Tests:
      store default/reachability; group partition.
- [ ] T2 — Status tab: `FleetTab` + `"status"`, sixth tab entry, `StatusPane` +
      exported `fleetMetrics`, `.dock-*` facet CSS rehomed to `.r-status*`.
      Tests: `fleetMetrics`; partition updated.
- [ ] T3 — Remove the dock: store members (`store.ts:34,216-220,339-340,511,644-647`),
      `components/BottomDock.tsx`, `App.tsx:5,16-26,92-100,138`, grid row
      (`app.css:101-111`) + dock CSS (`app.css:1221-1361`) + header comments;
      update `store.test.ts:76,364,370,372-377,400-413`.
- [ ] T4 — Deferred: sidebar resize handles (`--left-w`/`--right-w` seam;
      contract above; no drag design here).
- [ ] markdownlint this record clean; re-verify the living spec needs no
      pointer update.

## Global Constraints

- **The frozen record's reasoning is never rewritten.** This record supersedes
  ADE-shell D7, amends ADE-shell D5, and reverses ADE-shell resolved decision 7.
  The only edit to `docs/designs/ui/compass-ade-shell/design.md` is the
  bidirectional supersede/amend **pointer** the house convention requires — a
  one-blockquote note atop D7 and D5 pointing forward to this record, so a
  reader of the frozen decision in isolation learns it was overridden (the
  sanctioned move in `../../platform/docsite.md:283-288`, "avoid contradictory
  records on `main`"). No decision prose below those pointers is altered;
  records still freeze on merge (`docs/README.md`).
- **One store, one pattern.** Every state change extends the `AppStore` seam
  (`store.ts`); no second store, no parallel signal for the same axis (D1).
- **The fixture is the seam.** No new fixture shapes are needed; agent
  resolution keeps the existing `STUB_AGENTS` read pattern the dock used
  (`BottomDock.tsx:4,69-70`), and workstream counts read
  `store.workstreams()`.
- **No agent-product or persona names.** The moat agents appear only by their
  in-app roles ("Supervisor", "Warden"); the external ADE mirror is cited
  through the frozen record (ADE-shell D5), never re-cited from its source
  here.
- **Evidence discipline.** Every claim about current code carries file+line
  from this repo, read this session on `cook-compass-ade-tabs` @ `ad29c49f`.
  No external-ADE source was read this session, so no new external citations
  are made — external-mirror rationale is inherited from the frozen record's
  already-established D5 evidence.
- **Prerequisite #467.** All cited lines are post-#467; T1 starts only after
  it merges.
- **Tests-first.** Each task's new behavior lands red → green via the Tester;
  `bun test` in `apps/ui` gates every slice.
- **Commit convention.** `docs(product): …` for this record;
  `Co-Authored-By: seal <noreply@rigel.build>`; markdownlint clean
  (blank lines around headings/lists/fences/tables, languages on fences,
  leading+trailing table pipes).

## Spec impact

`Spec-impact: none`. This is UI-app-state only — the `AppStore` seam, the
components, and CSS. No `compass.v1` payload, RPC, or enum changes; the
agent-liveness contract and its UI projection (ADE-shell D9/D11) are untouched.
The living spec (`docs/specs/product/compass.md`) contains no dock references
to reconcile (checked this session); implementing PRs re-verify per the docs
convention.

## Risks

- **Store churn breaks existing tests by design.** The union widening and the
  dock-member removal touch `store.test.ts` at known sites
  (`store.test.ts:76,364,370,372-377,400-413`). Mitigation: T1/T2 are purely
  additive (dock untouched, all existing tests stay green); every removal and
  its test updates land together in T3, so a red test always points at its own
  slice.
- **A one-slice duplication window.** Between T2 and T3 both the dock facet and
  the Status pane render the same metrics/feed. Accepted: it keeps every
  intermediate commit functional and the deletion atomic; the window is one
  slice long.
- **The 400px default is a guess until resize lands.** On narrow viewports a
  fixed 400px column may crowd the board. `--right-w` is deliberately the
  single knob (D4), and T4 (deferred) replaces the guess with user control.
- **Conditional card-select feedback.** With a fleet tab active, clicking a
  Bridge card no longer visibly changes the sidebar (D6). If this reads as
  dead-click confusion in use, the remedy is a small selection cue on the
  workstream tab icons — not an auto-switch (Resolved decisions #1).

## Resolved decisions (maintainer-confirmed)

Two forks were surfaced to the maintainer during the design pass and **confirmed
as designed**; they are recorded here as settled decisions (the reasoning
survives the freeze). This record poses no open questions.

1. **Card select never auto-switches the sidebar tab (confirmed).** Selecting a
   Bridge card keeps the active tab — it only updates what the workstream tabs
   show when visited; it never yanks a Supervisor conversation away. The stated
   goal is watching the board *while* talking to the Supervisor, so conversation
   persistence wins (D6). If dead-click feedback ever proves confusing, the
   remedy is a selection cue on the workstream tab icons — not an auto-switch.
2. **Status tab shows fleet metrics + the Supervisor feed, not a merged
   fleet-wide feed (confirmed).** An exact salvage of the dock facet — the
   metrics + the Supervisor's recent-decision feed as shown today
   (`BottomDock.tsx:26-65`) — is what the Status tab renders (D3). Merging other
   agents' feeds (e.g. the Warden's, `stub-data.ts:289-292`) into one stream is
   a scope expansion this record deliberately avoids.
