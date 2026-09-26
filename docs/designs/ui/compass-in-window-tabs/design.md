# Design: In-Window Tabs and Split Views

Builds on: [UX foundation §D6.1](../compass-ux-foundation/design.md) (DL-160), [shell routing](../compass-shell-routing/design.md) (DL-127), [multi-window](../compass-multi-window/design.md)
Refs: RIG-1808 (Beta milestone)

## Problem / Intent

A Compass window shows one surface at a time. A user who watches a channel
while reading an agent's trace must switch back and forth, or open a second OS
window. This record adds Linear-style **tabs** inside one window and a
**two-pane split**, in both hosts (desktop app and browser, DL-159).

D6.1 said the window-scoped-view decomposition admits tabs and splits "without
rework". That is not true of the code on `main`, and this record says so. The
view components mount standalone, but the state they read is not per view. The
route-derived selection is a set of store-wide signals with one writer:

- `view`, `selectedChannelId`, `selectedTopicId` and `selectedAgentId` are
  single `createSignal`s in `createAppStore` (`apps/ui/src/store.ts`). They are
  written only by `applyRoute`, which parses one path (`store.ts`, the
  router-seam block and `applyRoute`).
- Surfaces read those globals: `TopicView` reads `store.selectedTopic()` and
  `store.selectedChannel()` (`components/TopicView.tsx`). `AgentView` reads
  `store.selectedAgent()` and `store.agentTabs()` (`components/AgentView.tsx`).
  `ChannelView` falls back to `store.selectedChannel()` when it has no
  `channel` prop (`components/ChannelView.tsx`).
- `agentSession` is keyed on `selectedAgentId()` (`store.ts`), so `LogPanel`
  follows the global selection, not the view that hosts it.
- The agent-workspace state is global and is reset whenever the selected agent
  changes: `tabs`, `activeAgentTabId`, `agentViewAgentId` and `activeRepoId`
  (`applyAgentRoute` in `store.ts`).
- One router instance per window: `mountShell` builds one `createRouter` over
  `hashHistory` (`apps/ui/src/mount.tsx`). `App` renders the one matched route
  as `props.children` into `<main>` (`apps/ui/src/App.tsx`).

Two views of the same surface type in one window would therefore share one
selection, and would reset each other's agent tabs. The rework is to move the
routed selection from the store into a per-view scope. That rework is task T1
and task T2 below. Everything after it is new UI.

## Approach

### A1 — A view instance owns its route

A **view instance** is one mounted surface. It has an id and a DL-127 path
(`/channel/ch-1/topic/t-9`, `/agent/acc-x`, …). The path is the view's whole
identity; no other per-view state is persisted.

- A pure `parseRoute(path): RouteMatch` holds the parse that `applyRoute` does
  inline today. `RouteMatch` is a discriminated union over the seven `View`
  kinds plus their params. An unknown path parses to the Bridge. This keeps the
  `*all` → `/` redirect of `routes.tsx` (`RedirectHome`).
- A `ViewScope` is created per view instance and provided to its subtree through
  a `ViewContext`. It exposes the parsed route, the resolved selection
  (`channel`, `topic`, `agent`), a scoped `navigate(path)` that moves only this
  view, and the per-view agent-workspace state (`tabs`, `activeAgentTabId`,
  `activeRepoId`).
- Surfaces read `useView()`, never the store's routed signals. The store keeps
  the data that is truly window-wide: comms state, agents, issues, daemon
  status, pins, sidebars, palette and overlays.
- **Issue selection stays window-wide.** `selectedIssueId` is not in the route.
  It is written by `selectIssue` (Bridge, Backlog, Done) and anchored by
  `applyAgentRoute`, and it drives the right sidebar's card-scoped panes
  (`RightSidebar.tsx`). One right sidebar shows one issue, so a Bridge tab and
  a Backlog tab share it by design. When the focused view becomes an agent view,
  the store re-anchors the issue selection as `applyAgentRoute` does today.
- The store's routed accessors (`view`, `selectedChannelId`, …) stay, but become
  derived from the **focused** view. The window chrome reads them: the
  `LeftSidebar` highlight, the `RightSidebar` card-scoped panes, the topbar.
  Their one writer becomes the focused view's route instead of `applyRoute`.

The pending-aware resolution of `applyChannelRoute` and `applyTopicRoute` (hold
an unknown id until the first comms snapshot, then fall back) moves into the
`ViewScope` unchanged. A fallback navigates that view only.

### A2 — The router stays the URL bridge, not the pane renderer

One `hashHistory` router cannot show two different paths at once. The router
therefore stops choosing what `<main>` renders. It keeps one job: mirror the
**focused view's path** into the URL hash, and apply a hash change (back,
forward, a pasted deep link) to the focused view.

A `ViewHost` renders a view instance: it calls `parseRoute(path)` and mounts the
matching component from the existing route table, inside that view's
`ViewContext`. `appRoutes` stays the single component map, so production, tests
(`test-router.tsx`) and the pane renderer cannot drift.

Browser back and forward move the focused view through its own history. Each
view keeps a small history stack in its `ViewScope`. Switching tabs replaces the
URL without pushing an entry, so back never jumps between tabs.

### A3 — Window layout: tabs, each a single view or a two-pane split

```ts
type ViewInstance = { id: string; path: string };
type TabLayout =
  | { kind: "single"; view: ViewInstance }
  | {
      kind: "split";
      direction: "row" | "column";
      first: ViewInstance;
      second: ViewInstance;
      ratio: number; // share of the first pane, 0.2..0.8
      focused: "first" | "second";
    };
type WindowLayout = { tabs: { id: string; layout: TabLayout }[]; activeTabId: string };
```

- **Bounded to two panes.** The bounded type makes three-pane states impossible
  to represent. The agent workspace already has an unbounded binary pane tree
  (`SplitNode`, `splitPaneOnce` in `store.ts`) for terminals. That tree stays
  inside the agent view and is not reused here: a tab split is chrome, a
  terminal split is content.
- **Open.** Opening a path focuses an existing tab whose single view has the
  same path (dedupe). Otherwise it opens a new tab after the active one.
- **Navigate in place.** A sidebar click, a board card, a palette destination
  and a `G` leader chord navigate the **focused view** in place, as today.
  `Mod`+click and middle-click open the path in a new tab. This keeps today's
  one-surface behavior for a user who never opens a tab.
- **Close.** Closing a tab focuses the tab to its right, else to its left. The
  last tab cannot close; closing it navigates it to `/`. Closing one pane of a
  split turns the tab back into a single view.
- **Reorder** by dragging a tab, or with the palette commands "Move tab left"
  and "Move tab right".
- **Tab cap: 10.** Opening an eleventh tab refuses with a notice. The cap bounds
  the keep-alive cost (A4).
- **Title.** A tab title comes from `routeTitle(match, store)`: the channel
  name, the topic name, the agent's display name, or the fixed view name. The
  window title (`document.title`) is the focused view's title.

### A4 — Inactive tabs stay mounted

Inactive tabs stay mounted and hidden (`hidden` attribute), so a half-typed
composer draft (the component-local `draft` signal in `ChannelView.tsx`),
scroll position and an open agent tab survive a tab switch. The cost is one
mounted surface per tab. Comms data is store-wide, so a hidden channel adds no
stream. The cap in A3 bounds it.

Hidden views take no keys, and this needs no new gate. The spine's
`activeGroup()` returns only a group whose `isFocused()` is true
(`apps/ui/src/keyboard/spine.ts`), and an element under `hidden` cannot hold DOM
focus. Hiding a tab must move focus out of it first, to the newly shown view.

### A5 — Focus and command scope

The `FocusZone` set (`"left" | "main" | "right" | "topbar"`,
`apps/ui/src/keyboard/zones.ts`) is unchanged. A split is two views inside the
`main` zone. The window layout records which pane is focused. `zone.focusMain`
(`Mod+2`) focuses the focused pane. Pointer focus inside a pane makes it the
focused pane.

`when: "main"` bindings (`comms.send`, …) resolve inside the focused pane.
Scoped palette commands rank for the focused view's surface. The single keydown
listener installed by `App` over the spine (`installKeymap`,
`apps/ui/src/keyboard/dispatch.ts`) stays the only one.

### A6 — Keyboard

The browser host cannot intercept `Ctrl+T`, `Ctrl+W`, `Ctrl+N` or `Ctrl+Tab`:
the browser handles them before the page sees them. Tab and split chords
therefore use a second leader, `W`, under the leader-chord rules (modifier-less
segments, focus-gated, one listener, `compass-leader-chords` §A2). `W` is not in
that record's reserved-letter list. Adding a leader is a data change there.

| Chord | Command |
| --- | --- |
| `W N` | New tab (opens `/`) |
| `W X` | Close tab |
| `W ]` / `W [` | Next / previous tab |
| `W 1` … `W 9` | Go to tab N |
| `W V` | Split right (the second pane opens on the focused path) |
| `W S` | Split down |
| `W O` | Close the other pane |
| `W H` / `W L` | Focus the left / right pane (top / bottom for a column split) |

Every command is also a palette command. The shortcuts overlay lists the rows
from `DEFAULT_KEYMAP` as it does today.

`Mod+Alt+ArrowLeft/Right` stays bound to the agent workspace's inner panes
(`workspace.focusPaneLeft/Right`, `keymap.ts`). The tab split uses `W H`/`W L`,
so the two pane systems never share a chord.

### A7 — The topbar tab strip

The tab strip replaces the topbar `.view-tabs` nav in `App.tsx`. That nav holds
a Bridge tab and a selected-agent tab today. It already acts as a two-slot tab
strip, and the new strip generalizes it. The Bridge tab is no longer pinned:
Bridge is a path like any other.

The right-sidebar tabs (`activeRightTab`) and the agent workspace's inner tabs
(`AgentTab`) are unchanged. In the UI and in code the new unit is a **view
tab**, so it cannot be confused with an agent tab.

### A8 — Persistence and the two hosts

The window layout persists to `sessionStorage` under one key, as the list of
tab paths plus split shape. Only paths persist; per-view scroll and drafts do
not.

- **Browser.** `sessionStorage` is per browser tab and survives a reload, so a
  reload restores the layout. A URL copied to another tab opens as a single view
  of its hash path.
- **Desktop.** Each Wails window is its own JS runtime with its own boot
  (multi-window §A1), so each window has its own layout with no cross-window
  work. `sessionStorage` survives a webview reload. It does not survive an app
  restart. The multi-window record restores a window **set** of Bridge windows
  and persists no per-window route (its §A2). A restarted window opens one
  Bridge tab, which matches that record.
- On boot, a hash that does not match any restored tab opens as a new focused
  tab, so a deep link always wins over the restored layout.

## Alternatives considered

### One router per pane (memory history)

Each pane gets its own `createRouter` over `memoryHistory`, and a coordinator
mirrors the focused pane into the hash. It lost because it still needs the
store selection moved into a per-view scope (the router is not where the state
lives today; `applyRoute` is). It also adds a router instance per tab, with two
navigation sources to keep in sync. A2 keeps one router and one component map.

### Keep the store globals and swap them on tab focus

Save and restore the store's routed signals when the focused tab changes. It
lost because a split shows two views at the same time, so there is no single
"current" selection to swap. It also leaves the agent-tab reset in
`applyAgentRoute` firing on every tab switch.

### Unmount inactive tabs

Lower memory, but a tab switch loses composer drafts and scroll, and re-runs
the agent-workspace reset. The 10-tab cap makes keep-alive cheap enough.

### An unbounded pane tree (reuse `SplitNode`)

It lost because the issue asks for two views side by side, and a bounded type
keeps focus movement, persistence and the splitter UI simple. A later record can
widen it.

## Global Constraints

- `solid-js` 2 and `@solidjs/router` 2 as pinned in `apps/ui/package.json`. The
  router stays in hash mode (DL-127). The route shapes in `routes.tsx` do not
  change.
- One keydown listener (`installKeymap`). Leader rows follow the
  `compass-leader-chords` authoring rules and its `DEFAULT_KEYMAP` invariant
  test.
- DS tokens only. The D7 stylelint guard (`apps/ui/.stylelintrc.cjs`) applies:
  no raw hex, no `--rigel-*` references, motion from `--cx-motion-*`.
- Both hosts (DL-159). No Wails-only API in the layout code.
- Visual baselines regenerate only through the CI `regen-visual-baselines` lane
  (DL-341), never from a developer machine.
- A user with one tab sees no behavior change except the tab strip itself.

## Plan

Order: T1 → T2 → T3 → T4 → T5 → T6. T1 and T2 change no user-visible behavior
and can land before Beta planning ends. Lane for all tasks: compass-ui.

### T1 — `parseRoute`, `ViewScope` and `useView()`

Extract the path parse from `applyRoute` into a pure function. Add the
`ViewScope` and its context. `applyRoute` becomes a thin caller of `parseRoute`.
No surface changes yet.

- `Interfaces:`

  ```ts
  // apps/ui/src/view-route.ts
  export type RouteMatch =
    | { view: "bridge" } | { view: "backlog" } | { view: "done" } | { view: "settings" }
    | { view: "channel"; channelId: string }
    | { view: "topic"; channelId: string; topicId: string }
    | { view: "agent"; agentId: string };
  export function parseRoute(path: string): RouteMatch;
  export function routePath(match: RouteMatch): string;
  // apps/ui/src/view-scope.ts
  export interface ViewScope {
    id: string;
    path: Accessor<string>;
    route: Accessor<RouteMatch>;
    channel: Accessor<Channel | undefined>;
    topic: Accessor<Topic | undefined>;
    agent: Accessor<Agent | undefined>;
    navigate: (path: string) => void;
  }
  export function createViewScope(store: AppStore, id: string, initialPath: string): ViewScope;
  export const ViewContext: Context<ViewScope | undefined>;
  export function useView(): ViewScope;
  ```

- Tests: a table test of `parseRoute`/`routePath` round-trips over every
  `appRoutes` shape, with unknown paths going to the Bridge. A `ViewScope` test
  proving pending-aware hold then fallback, moved from the existing
  `applyChannelRoute`/`applyTopicRoute` tests.

### T2 — Surfaces read `useView()`; agent-workspace state moves per view

Move `TopicView`, `ChannelView`, `AgentView` and `LogPanel` to `useView()`.
Bridge, Backlog, Done and Settings read no routed selection and do not change.
Move `tabs`, `activeAgentTabId`,
`agentViewAgentId` and `activeRepoId` into the `ViewScope`, and key
`agentSession` on the view's agent. The store's routed accessors become derived
from the focused view. There is still one view per window.

- `Interfaces:`

  ```ts
  interface ViewScope {
    agentTabs: Accessor<AgentTab[]>;
    activeAgentTab: Accessor<AgentTab | undefined>;
    agentSession: Accessor<AgentSession | undefined>;
    openTab(pane: Pane): void;
    closeTab(tabId: string): void;
    splitFocused(pane: Pane, direction: "row" | "column"): void;
  }
  interface AppStore {
    focusedView: Accessor<ViewScope>; // chrome reads selection through this
  }
  ```

- Red-green test: mount two `ViewHost`s in one store on `/agent/a` and
  `/agent/b`. Each renders its own agent. Opening a terminal tab in one does not
  change the other. This test fails on `main` because both read
  `store.selectedAgent()`.
- The existing surface tests move from `store.selectedX()` to the focused view's
  scope. The route-sync tests in `routing.test.tsx` stay green.

### T3 — Window layout model, hash sync and persistence

A pure layout reducer plus its store wiring. The router mirrors the focused
view's path and applies hash changes to it (A2). Layout persists to
`sessionStorage` (A8).

- `Interfaces:`

  ```ts
  // apps/ui/src/window-layout.ts
  export const MAX_TABS = 10;
  export type LayoutAction =
    | { kind: "open"; path: string; background?: boolean }
    | { kind: "close"; tabId: string }
    | { kind: "focusTab"; tabId: string }
    | { kind: "move"; tabId: string; toIndex: number }
    | { kind: "split"; direction: "row" | "column" }
    | { kind: "closeOtherPane" }
    | { kind: "focusPane"; pane: "first" | "second" }
    | { kind: "resize"; ratio: number }
    | { kind: "navigateFocused"; path: string };
  export function reduceLayout(layout: WindowLayout, action: LayoutAction): WindowLayout | { refused: "tab-cap" };
  export function loadLayout(storage: Storage | undefined, hashPath: string): WindowLayout;
  export function saveLayout(storage: Storage | undefined, layout: WindowLayout): void;
  ```

- Tests: reducer cases for dedupe, close focus order, the last-tab rule, the
  tab cap, move bounds, and a split whose pane closes back to a single view.
  `loadLayout` with a corrupt value falls back to one Bridge tab. A deep-link
  hash that is not in the restored layout opens as the focused tab.

### T4 — Tab strip UI, keep-alive and sidebar open modes

Replace `.view-tabs` in `App.tsx` with the tab strip. Render every tab's
`ViewHost`, and hide the inactive ones. Move focus into the shown view before
hiding the old one (A4). Sidebar and board links navigate in place; `Mod`+click and middle-click
open a new tab.

- `Interfaces:` `components/TabStrip.tsx` (`TabStrip: Component`, reads
  `store.layout()` and dispatches `LayoutAction`s). `store.layout:
  Accessor<WindowLayout>`, `store.dispatchLayout(action: LayoutAction): void`.
- Acceptance: Playwright e2e in `apps/ui/e2e`. Open a channel and an agent in
  two tabs, type a composer draft, switch tabs and back, and the draft is intact.
  Reload, and both tabs are restored. A new `tab-strip` visual baseline, taken
  through the CI regen lane.

### T5 — `W` leader chords and palette commands

Add the A6 rows to `DEFAULT_KEYMAP` and register the commands. Add the palette
entries.

- `Interfaces:` command ids `tab.new`, `tab.close`, `tab.next`, `tab.prev`,
  `tab.goto.1` … `tab.goto.9`, `tab.moveLeft`, `tab.moveRight`, `pane.splitRight`,
  `pane.splitDown`, `pane.closeOther`, `pane.focusFirst`, `pane.focusSecond`.
- Tests: the existing `DEFAULT_KEYMAP` authoring-invariant test covers the new
  rows. A dispatcher test shows that `w n` in a focused composer types text and
  runs nothing. An e2e presses `w n`, then `w x`.

### T6 — Two-pane split

Render a split tab as two `ViewHost`s with a draggable splitter. Keep the ratio
within 0.2–0.8 and persist it. Pointer and chord focus between panes. A 1px
`--cx-border` divider marks the focused pane with `--cx-border-focus`.

- `Interfaces:` `components/SplitPane.tsx`
  (`SplitPane: Component<{ tabId: string }>`), consuming `reduceLayout`'s
  `split`, `focusPane`, `resize` and `closeOtherPane` actions.
- Acceptance: Playwright e2e. `w v` on a channel, navigate the second pane to an
  agent, send a message from the first pane's composer, and the second pane does
  not move. A `split-view` visual baseline through the CI regen lane.

## Tasks

- [ ] T1 — `parseRoute` / `routePath`, `ViewScope`, `ViewContext`, `useView()`
- [ ] T2 — surfaces on `useView()`; agent-workspace state and `agentSession` per view; store routed accessors derived from the focused view
- [ ] T3 — `reduceLayout`, hash sync to the focused view, `sessionStorage` persistence
- [ ] T4 — tab strip replacing `.view-tabs`, keep-alive, `Mod`+click / middle-click open-in-tab, e2e + visual baseline
- [ ] T5 — `W` leader rows, tab/pane commands, palette entries
- [ ] T6 — two-pane split with splitter, pane focus, e2e + visual baseline

## Open Questions

1. **Leader `W` for tab and split chords.** (Load-bearing: T5 builds it.) The
   browser reserves `Ctrl+T`/`W`/`N`/`Tab`, so modifier chords cannot be the
   same in both hosts. Options: (a) leader `W` sequences in both hosts
   (recommended; one keymap, focus-gated like `G`); (b) `Mod+T`-style chords on
   desktop, with leader chords only in the browser (two keymaps, and the shortcut
   overlay differs per host); (c) palette only, no chords. Recommend (a).
2. **Sidebar clicks navigate in place, or open a tab.** (Load-bearing: T4.)
   Options: (a) in place, with `Mod`+click for a new tab (recommended; Linear
   behavior, and no change for a one-tab user); (b) always a new tab, with dedupe
   (tabs pile up fast, and the 10-tab cap is hit in minutes). Recommend (a).
3. **Splits ship with tabs in Beta, or later.** (Load-bearing for milestone
   planning, not for the design.) T6 depends only on T3, so it can move out
   alone. Recommend both in Beta; T1–T2 are the expensive part and are shared.
4. **Layout restore across an app restart.** (Non-load-bearing, deferred.) It
   needs a per-window identity that survives a restart. The multi-window record
   persists only a set of Bridge windows (its §A2). The design is correct
   without it. A restarted window opens one Bridge tab.
