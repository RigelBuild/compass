# Compass surface composition specs (D6)

This document is the per-surface composition spec for the Compass ADE. Where
`components.md` owns the component vocabulary — the `.cx-*` classes, their
`data-*` variants, and the `--cx-*` tokens each consumes — this document owns how
those contracts compose into the surfaces a user actually lives in. The
information architecture is frozen upstream; structure comes from the IA
records, look and navigation come from here.

Each surface is specified along five facets:

- **Composition** — the component contracts (from `components.md`) that build it,
  by real class name.
- **Focus and keyboard** — which focus zone it lives in and how the keyboard
  drives it (the one-ring, roving-tabindex model of the focus system).
- **Empty states** — what renders before there is content, so no surface is a
  blank void.
- **Mark placement** — which brand asset appears, per the brand mark surface
  table. The default is none: the wordmark lives in exactly one place.
- **Flip checklist** — the ordered steps an adoption PR executes to re-clothe the
  surface from the legacy vocabulary to `.cx-*`, deleting the legacy in the same
  diff (no shims).

The migration is surface-by-surface, each surface its own PR, in this order: left
sidebar / tree, then the Bridge board, then comms, then the right sidebar, then
the workspace / trace, then Backlog / Done / Settings. Two flips carry an
ordering dependency on in-flight lanes and are called out where they apply.

Four surfaces carry the excellence bar — the Bridge board, the Manager tree
(inside the left sidebar), channels + threads, and the agent session trace. Each
starts from a concrete reference already built, cited inline, never a
blank-page redraw.

## Shell

The shell is the grid that hosts every other surface: topbar, left sidebar, main
view, right sidebar, and a usage bar. Its shape is inherited (the `.app`
`grid-template-areas` rule, `app.css:112-115`; region markup `App.tsx:40-120`);
this system re-clothes it, it does not re-lay it.

**Composition.** The region surfaces carry depth by color, not shadow: topbar and
usage bar on `--cx-bg`, sidebars on `--cx-bg-raised`, main on `--cx-bg`,
separated by 1px `--cx-border` rules. No box-shadow between docked regions —
shadows are reserved for genuinely floating layers (menu, dialog, palette,
toast). The topbar carries the wordmark treatment (see Mark placement), the
view-tabs as a horizontal `.cx-tabs` (`data-orientation="h"`, accent-underline
selection), a daemon status pip built from `.cx-pip`, and pane-toggle controls as
`.cx-btn` `data-variant="ghost"`. The usage bar is a display-only landmark; it
carries no interactive component.

**Focus and keyboard.** The shell is four interactive focus zones: left sidebar,
main view, right sidebar, and topbar. `Ctrl+1/2/3` jump to left / main / right;
`F6` and `Shift+F6` cycle; the topbar is reachable by `F6` cycle only. The usage
bar is a display-only landmark — reachable by screen-reader landmark navigation
but not in the `F6` rotation, because it carries no interactive control. Pane
toggles bind `Ctrl+\` (right sidebar) and `Ctrl+Shift+\` (left sidebar).

**Empty states.** The shell frame is never empty — it always renders its regions.
A region with no content delegates its empty state to the surface mounted in it
(tree-empty, pins-empty, and the board/comms empties below).

**Mark placement.** The topbar is the one surface that carries the brand mark:
the sigil-led wordmark, and it is the sole in-app purple (`brand identity.md`
§"Which mark on which surface"). The wordmark is set at or above its 24px-tall
floor; below that floor the mark is omitted, never shrunk. No other region
carries a mark, and no icon-beside-wordmark lockup is composed anywhere in the
shell — the wordmark stands alone (the one-R rule). The phosphor mark
(`icon-phosphor`, the only permitted purple-in-motion) belongs to the first-load
boot sequence and to the one sanctioned purple spinner, not to steady-state
chrome.

**Flip checklist.**

1. Land the type system on the shell chrome: topbar, sidebars, usage bar move to
   the mono body face and the `--cx-*` surface tokens.
2. Replace the region background/border rules with `--cx-bg` / `--cx-bg-raised` /
   `--cx-border`; remove any inter-region box-shadow.
3. Re-clothe the view-tabs as `.cx-tabs[data-orientation="h"]`, the daemon pip as
   `.cx-pip`, and the pane toggles as `.cx-btn[data-variant="ghost"]`.
4. Apply the topbar wordmark treatment; confirm it renders at or above 24px.
5. Fix the two focus defects — the `outline: none` rules at `app.css:2385-2388`
   and `app.css:3556-3559` — by applying `--cx-focus-ring` on `:focus-visible`.
6. Wire the four focus zones (`Ctrl+1/2/3`, `F6`) and the pane-toggle bindings
   through the command registry.
7. Verify the usage bar carries no interactive control; if a control is added
   later, it rejoins the rotation as a fifth focus zone.
8. Delete the legacy shell selectors in the same diff — no shims.

## Left sidebar — agent tree and channel rail

The left sidebar is the org surface: the agent tree above, the channel rail
below, both rendered with the same tree-row contract so a later data-level
unification is a data change, not a visual one. The agent tree is one of the four
excellence surfaces.

**Reference.** The starting point is `AgentTree.astro` (the production-quality
Rigel-site mockup, authored against the real `LeftSidebar`), not a redraw. Its
load-bearing structure to carry forward:

- **Every standing node is a Manager**, indented parent-above-children in
  depth-first tree order (`flatten` in the mockup, mirroring `board.ts`
  `treeOrder`); ephemeral worker subagents are not tree nodes. Each node shows a
  state glyph, handle, state, a live worker count, and the issue it currently
  owns (`AgentCard`).
- **The connecting spine** is the delegation cue: an elbow rule from the parent's
  indent line into the child row, drawn with the surface border, gated on depth
  (`.tree-spine`, `depth > 0`).
- **A chase-light pip travels down the spine** when the child is working
  (`data-flow="1"`, the `spine-flow` keyframe) — delegation flowing parent to
  child, in the blue flow color, never a state color. It is suppressed under
  `prefers-reduced-motion` and the `data-reduce="on"` mirror.

**Composition.** Tree rows are `.cx-tree-row` at the 26px contract height, using
`data-depth` (0–5) for indent and `.cx-tree-caret` for the expand affordance.
Each row carries a `.cx-state-dot` (the eight-glyph process-axis set), the agent
name, a role pip (`.cx-pip`, for a role such as supervisor — a pip, not a special row),
and an unread badge (`.cx-badge`). Selection is `--cx-bg-selected` plus the
accent left rule, never a raised background — the row contract's selected state.
The channel rail rows are the same `.cx-tree-row`, each channel followed by its
three most-recent topics as indented deep-nav sub-rows (a deeper `data-depth`),
with unread badges. Re-parenting is drag plus a palette command; the visual
target is the standard row hover/selected treatment, no bespoke drop styling.

**Focus and keyboard.** The tree is the left-sidebar focus zone, a single
roving-tabindex zone: arrow keys move selection, `Enter` opens/selects, `Space`
expands/collapses the caret, `Home`/`End` jump to first/last. The zone is one Tab
stop; the tab order does not grow with the tree.

**Empty states.** Tree-empty (no agents yet) renders a real empty-state row set: a
one-line explanation and the palette hint for spawning or connecting an agent, in
the row's faint text — not a blank column. The channel rail below shows its own
"no channels" line independently.

**Mark placement.** None. The agent avatar glyph and state dot are component
glyphs, not the brand mark. The state-dot column is the sanctioned
one-pulse-per-region exception: a scannable field of working pulses (green,
`--cx-st-working`) is brand-legal by the pulse-budget rule — that pulse is a
state signal, never the purple mark.

**Flip checklist.** This is the first surface flip and carries no dependency on an
in-flight lane.

1. Replace the legacy sidebar row markup with `.cx-tree-row` at `data-depth`,
   `.cx-tree-caret`, `.cx-state-dot`, `.cx-pip`, `.cx-badge`.
2. Move selection to `--cx-bg-selected` plus the accent left rule; drop any
   raised-background selection styling.
3. Render the channel rail with the same tree-row contract, three recent topics
   per channel as indented sub-rows.
4. Carry the spine + chase-light pip from the reference, gated on the working
   state and on the reduced-motion tokens.
5. Wire the zone's roving tabindex (arrows / `Enter` / `Space` / `Home` / `End`)
   and register the "Re-parent agent…" command.
6. Add the tree-empty and channel-rail-empty panes.
7. Delete the legacy tree/rail selectors in the same diff.

## Right sidebar — pinned agents, status, and issue detail

The right sidebar is a vertical activity bar of tabs: pinned agents, an
always-present Status tab, and the issue-detail tabs.

**Composition.** The activity bar is `.cx-tabs[data-orientation="v"]` (accent
left-rule selection): pinned-agent tabs (each an avatar glyph plus a
`.cx-state-dot`), the always-present Status tab, then the issue-detail tabs —
Files, VCS, PR, Checks. Pane bodies are `.cx-panel` / `.cx-pane` surfaces on
`--cx-bg-panel`; content within them reuses `.cx-card`, `.cx-badge`
(`data-status` for CI and review), and `.cx-chip` as the detail demands. An
unreachable pin keeps its tab and renders an "unreachable" pane rather than
dropping the tab.

**Focus and keyboard.** The right sidebar is its own focus zone (`Ctrl+3`, and in
the `F6` cycle). The tab strip is a roving-tabindex list: arrows move between
tabs, `Enter` activates. Pins are client-local; `Space` toggles a pin per the
list/tree keymap.

**Empty states.** The empty-pin default is a real empty-state pane: how to pin an
agent and the palette hint, on the panel surface — not a blank void. The Status
tab is always present, so the sidebar is never fully empty. An unreachable pin's
pane states the unreachable condition explicitly.

**Mark placement.** None. Pinned-agent tabs carry an avatar glyph plus a state
dot, both component glyphs, not the brand mark.

**Flip checklist.** This flip sequences after the unreachable-pin lane
(DL-113 / SEA-1645) merges; until then the right sidebar stays on the current
vocabulary.

1. Replace the activity bar with `.cx-tabs[data-orientation="v"]`; render pinned,
   Status, and issue-detail tabs.
2. Move pane bodies to `.cx-panel` / `.cx-pane` on `--cx-bg-panel`; reuse
   `.cx-card` / `.cx-badge` / `.cx-chip` inside.
3. Build the empty-pin pane and the unreachable-pin pane.
4. Wire the zone (`Ctrl+3`, `F6`) and the pin toggle (`Space`), registering
   pin/unpin and tab-switch commands.
5. Delete the legacy right-sidebar selectors in the same diff.

## Bridge — the Issues and PRs board

The Bridge is the central supervision board and one of the four excellence
surfaces: a swimlane grid of Managers against a lifecycle, with an Issues view
and a PRs view.

**Reference.** The starting point is `BridgeBoard.astro` (authored against the
real `Bridge.tsx` / `IssueCard.tsx` / `board.ts`), not a redraw. Its load-bearing
structure to carry forward:

- **Two views, one board shape.** A segmented control flips between Issues and
  PRs (the real `Bridge.tsx` `BoardTab`); both are the same swimlane grid, only
  the columns differ — Issues use the lifecycle columns (Queued, In progress, In
  review, Done), PRs use the PR-lifecycle columns (In progress, In review, Ready
  to merge, Merged).
- **Sticky-left agent gutters** (`.bridge-lane`): a state glyph plus the Manager
  name, one lane per Manager, in tree order.
- **Sticky-top status columns** (`.bridge-col-head`), tinted by the issue-state
  color at low alpha in the lane head only — contrast rationing, so the tint
  labels the column without flooding the grid.
- **Cells hold cards** (`.bridge-cell` of `card` articles): the issue key, an
  optional PR chip carrying a CI badge and a review badge and the PR number, the
  title, and a footer with the `@assignee` and a diff `+add / −del`.
- **PRs view rows** carry the PR's own facts — the CI badge, the review badge, and
  a `resolved/total threads` count — distinct from the issue's.
- **One advancing card** carries the chase-light (the `advancing` flag) as it
  crosses a column: work in motion.

**Composition.** Cells are `.cx-card` on the panel tier; selection is
`--cx-bg-selected` plus the accent left rule, never a raised background. Lane
heads carry a `.cx-state-dot` (Manager state) plus the name. The column-head tint
consumes `--cx-issue-*` (the issue-lifecycle namespace, separate from agent
state) at low alpha, in the lane head only. On-card badges: the CI badge is
`.cx-badge[data-status="ci-pass | ci-fail | ci-pending"]` on `--cx-ci-*`; the
review badge is
`.cx-badge[data-status="review-approved | review-changes | review-pending"]` on
`--cx-review-*`; the issue cross-link is a `.cx-chip`. The advancing card carries
the chase-light in the blue flow color as it crosses a column boundary; it is not
a state pulse.

**Focus and keyboard.** The board is the main-view focus zone and a
two-dimensional roving-tabindex grid: arrow keys move a card cursor across lanes
and columns, `Enter` selects the card, `Shift+Enter` opens the assigned agent's
workspace. `Ctrl+B` opens the Bridge view.

**Empty states.** An empty lane (a Manager with no cards in a column) renders as
an empty cell, not a placeholder card. A board with no issues at all renders a
single centered empty-state message on the panel surface. The segmented control
still shows both view labels with their counts (the PRs label carries the
open-PR count).

**Mark placement.** None. The board is glyph-dense with state dots and status
badges; the brand mark does not appear on it.

**Flip checklist.** This flip sequences after the board-remodel lane (SEA-1633)
merges. If the token and component contracts land first, that lane should build
directly against `.cx-*` and skip the build-on-legacy-then-re-skin double-work.

1. Render the swimlane grid: sticky-left `.cx-state-dot` lane heads in tree
   order, sticky-top column heads tinted from `--cx-issue-*` at low alpha in the
   head only.
2. Replace issue/PR cells with `.cx-card`; selection to `--cx-bg-selected` plus
   the accent left rule.
3. Put the CI and review badges on `.cx-badge[data-status=…]`; the issue
   cross-link on `.cx-chip`.
4. Add the two-view segmented control (Issues / PRs) with the PR-lifecycle
   columns for the PRs view and the `resolved/total threads` count.
5. Carry the advancing-card chase-light, blue flow color, reduced-motion honored.
6. Wire the 2-D card-cursor roving tabindex (arrows / `Enter` / `Shift+Enter`)
   and register the board commands (open card, open assigned agent).
7. Add the empty-lane and empty-board states.
8. Delete the legacy board selectors in the same diff.

## Comms — channel and topic

Comms is one of the four excellence surfaces. It takes Zulip's topic-threading
UX as its base model — a channel is a list of named topics, each topic a focused
flat stream — with Rigel excellence layered on. `ThreadPanel` and every
`.thread-*` selector are removed by this surface, not restyled.

**Reference.** The visual starting point is `ThreadView.astro`; its load-bearing
structure to carry forward:

- **A thread header** stamping channel and topic (`#channel › topic`), with the
  channel's live state glyph.
- **A message stream** where every message carries a header stamping its author
  and whether it is a Manager or a human (`msg-head`, `msg-tag`), plus inline
  flags for an async ASK and a mid-turn steer.
- **A pending affordance** for a Manager working after an async ask — a
  non-streaming placeholder, because a posted topic message is a complete post,
  never a live-streamed turn (streaming lives on the trace surface, below).

**Composition.** The channel view is a topic index — a `.cx-tree-row` list of
named topics, each with an unread `.cx-badge` and last-activity, plus a "New
topic" affordance (`.cx-btn`); the channel view has no composer. The topic view
is a flat message stream on `--cx-bg`: message bodies render as `.cx-md` content,
`.cx-ask` blocks appear inline (open → answered), and each message carries an
author-stamped header (Manager vs human). The composer is `.cx-composer`, pinned
to the bottom, with the topic name in its chrome — posting is topic-mandatory, so
the topic is visible at the point of send.

**Focus and keyboard.** The channel/topic surface is the main-view focus zone. The
topic index is a roving-tabindex list (arrows / `Enter` to open a topic). In the
composer, `Enter` sends and `Shift+Enter` inserts a newline; `Ctrl+Enter` is
reserved for a send-and-stay variant.

**Empty states.** An empty channel (no topics) renders a "no topics yet" line plus
the "New topic" affordance. An empty topic (no messages) renders the composer
with an inviting placeholder and the topic name in the chrome. A
posted-but-pending Manager reply shows the non-streaming pending affordance, not
a fake live stream.

**Mark placement.** None. Author identity is carried by the message-header author
stamp and the Manager/human tag, not the brand mark.

**Flip checklist.**

1. Split the surface into the channel view (topic index, no composer) and the
   topic view (flat messages + composer).
2. Render the topic index as `.cx-tree-row` with unread `.cx-badge`s and a "New
   topic" `.cx-btn`.
3. Render messages as `.cx-md`, `.cx-ask` inline, author-stamped headers;
   pin `.cx-composer` at the bottom with the topic name in its chrome.
4. Remove `ThreadPanel` and every `.thread-*` selector in the same diff — this is
   a delete, not a restyle.
5. Wire the composer keymap (`Enter` / `Shift+Enter`, `Ctrl+Enter` reserved) and
   register the post-to-topic and new-topic commands.
6. Add the empty-channel and empty-topic states.

## Agent workspace — home channel and session trace

The agent workspace is one of the four excellence surfaces, and the session trace
is where the signature streaming interaction runs. The workspace is the agent's
home channel plus its session trace, nothing more: two fixed panes, no arbitrary
split tree. There is no terminal pane and no file-viewer pane in dogfood — agents
run in isolated containers, and PR review happens on the user's forge.

**Reference.** The home-channel pane reuses the comms topic contract above (the
`ThreadView` lineage). The session-trace pane is the successor to the legacy
`LogPanel`, recast as a typed renderer, and it carries the brand streaming
treatment (the brand micro-excellence streaming system). Its load-bearing
behavior:

- **A typed renderer** with tool-status pips (styled from the CI-badge family),
  and Shiki-highlighted code and diffs through the editor-theme mapping so
  embedded content and chrome share one palette.
- **The streaming contract**: the network layer buffers bursty model output and
  the visual layer drains it at a steady cadence (`--cx-stream-char-ms`,
  rate-adaptive so it never falls behind); a freshly-revealed character ignites at
  the phosphor color and cools to fog behind the write-head; a block cursor
  blinks at `--cx-cursor-blink`; partial-markdown and code-fence safety hold
  dangling inline markup and defer a code block to Shiki until its fence closes,
  so nothing flickers mid-stream. The region is an `aria-live` surface
  (`aria-busy` while streaming), so motion is never the sole "streaming" signal.
- **Collapsible, minimized-rail ↔ full-panel** trace, so watching an agent think
  can be foregrounded or tucked to a rail.

**Composition.** Two fixed panes are `.cx-pane` surfaces carrying `data-focused`
(the accent inner rule marks the focused pane). A `.cx-tabs` strip switches the
main pane between the home channel and the trace. The home-channel pane reuses
the comms topic view: `.cx-md`, `.cx-ask`, author-stamped headers, `.cx-composer`.
The session-trace pane renders `.cx-md` content with Shiki via the `--cx-ed-*`
editor theme, tool-status pips from the `--cx-ci-*` family, and the streaming
treatment above (phosphor write-head, block cursor, decoupled cadence).

**Focus and keyboard.** The workspace is two fixed panes; the focused pane carries
`data-focused` and its accent inner rule, and `Ctrl+Alt+Arrow` moves focus
between the home channel and the trace. `Ctrl+Shift+A` opens the agent workspace
view. The pane tab strip is a roving-tabindex list. Within the panes, the comms
and content keymaps apply.

**Empty states.** A workspace whose agent has not yet streamed renders the trace
pane with a quiet "no activity yet" line and the home-channel pane with its
topic/composer empty state. The `aria-live` trace region announces the streaming
state so its emptiness is never ambiguous.

**Mark placement.** None in steady state. The one sanctioned purple-in-motion
moment reachable near this surface is a loading spinner (the mark-in-motion),
used at most once per surface and never adjacent to a state-dot field where a
purple-vs-green pair could read as state. The trace's phosphor write-head is the
streaming treatment's igniting character, not the brand mark.

**Flip checklist.**

1. Reduce the workspace to two fixed panes (home channel + trace); remove the
   arbitrary split tree.
2. Render both panes as `.cx-pane` with `data-focused`; add the `.cx-tabs` pane
   switcher.
3. Reuse the comms topic contract in the home-channel pane.
4. Recast the legacy `LogPanel` as the typed trace renderer: `.cx-md` + Shiki via
   `--cx-ed-*`, tool-status pips, the streaming treatment (decoupled cadence,
   phosphor write-head, block cursor, partial-markup safety, `aria-live`).
5. Retire the terminal pane: remove the terminal `PaneKind` arm and
   `newTerminalPane` in `store.ts` at this step.
6. Wire pane focus (`Ctrl+Alt+Arrow`) and register the pane-focus and
   view-open commands.
7. Add the trace-empty and home-channel-empty states.
8. Delete the legacy workspace/log selectors in the same diff.

## Backlog / Done / Settings

These are list and form surfaces with no bespoke styling: they reuse the shared
contracts directly.

**Composition.** Backlog and Done are list surfaces built from `.cx-card` (or
`.cx-tree-row` where a dense row reads better), `.cx-badge` (`data-status`) and a
`.cx-pip` priority indicator — the same card/row/badge vocabulary the board
uses, without the swimlane grid. Settings uses the form contracts: `.cx-input`,
`.cx-select`, and `.cx-btn`. The status-mapping editor renders as a two-column
`.cx-tree-row` table (source status ↔ mapped lane).

**Focus and keyboard.** Each is the main-view focus zone when open. Lists are
roving-tabindex (arrows / `Enter` / `Home` / `End`). Settings form controls follow
the standard focus-ring contract; `Ctrl+,` opens Settings.

**Empty states.** An empty Backlog or Done list renders a single centered
empty-state line on the panel surface. Settings is never empty — it renders its
sections with current values; an unconfigured mapping row shows a faint
"unmapped" placeholder rather than a blank cell.

**Mark placement.** None. These surfaces carry no brand mark.

**Flip checklist.**

1. Rebuild Backlog and Done from `.cx-card` / `.cx-tree-row` + `.cx-badge` /
   `.cx-pip`; no one-off styling.
2. Rebuild Settings from `.cx-input` / `.cx-select` / `.cx-btn`; render the
   status-mapping editor as a two-column `.cx-tree-row` table.
3. Wire list roving tabindex and register the open-Settings command (`Ctrl+,`).
4. Add the empty-list and unmapped-row states.
5. Delete the legacy list/settings selectors in the same diff.

## Window-scoped views

Every top-level surface — the Bridge, a channel and its topics, an agent
workspace, Backlog / Done, and Settings — is an independently mountable
window-scoped view. This is the decomposition that makes the desktop app
first-class multi-window: one window on the Bridge, another on a channel, another
on a specific agent's workspace.

The render contract:

- **Mounts standalone against its hash route.** Each view mounts against the
  DL-127 hash route for that surface; the route is the single entry point,
  identical whether the view is composed into the shell grid or hosted alone in a
  window.
- **Carries its own focus zones and command scope.** A window-scoped view brings
  the focus zones (the roving-tabindex model above) and the scoped commands it
  needs; scoped commands rank above global ones when the view is active.
- **Needs no sibling region to function.** A Bridge window renders without the
  sidebars; a channel window renders without the Bridge. No view depends on a
  sibling region being present.
- **Composition vs. hosting.** A single-window session composes these views into
  the shell grid; a multi-window session mounts one view per window. The same
  view code serves both.

**Deferred.** In-window tabs (Linear-style) and in-window split views are deferred
to the Beta milestone (SEA-1808) and are not built here. The decomposition is
designed to admit both later without rework — a tab strip or a splitter hosts the
same window-scoped views — but neither ships in the dogfood scope.

**Cross-lane seam.** Hosting these views in real OS windows depends on the native
shell. The frozen compass-native record (`compass-native-app/design.md`, DL-110)
is single-window today: one window loading the built UI. This decomposition
expands that scope, so compass-native's shell record needs a multi-window
amendment (SEA-1684's lane) before the views can be hosted in separate OS
windows. This is a dependency to track, not a blocker on the view decomposition
itself — the views are independently mountable regardless of how many windows
host them.
