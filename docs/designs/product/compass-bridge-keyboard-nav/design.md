# Design: Compass Bridge keyboard navigation — 2-D roving-tabindex grid + empty-board state (RIG-2130)

Status: Draft
Owner lane: compass-ux (design) → compass-ui (execution)
Refs: RIG-2130. Deferred here by the Bridge re-clothe record — "**D4 — roving-tabindex
2-D keyboard grid is OUT of scope** (was OQ-5): filed as a follow-up interaction
issue at dispatch" (`compass-bridge-reclothe/design.md:114-116`) — together with
the empty-board message: "**The board-empty centered message**
(`surfaces.md:242-243`) — the stub store always has issues; deferred with the
roving-tabindex follow-up" (`compass-bridge-reclothe/design.md:432-433`).
Governing spec: `apps/ui/src/design/surfaces.md` §"Bridge — the Issues and PRs
board", "Focus and keyboard" + "Empty states" + flip item 6
(`surfaces.md:236-245,264-266`).
Frozen contracts consumed: `apps/ui/src/keyboard/zones.ts`, `keymap.ts`,
`commands.ts` (compass-ux-foundation D4/D5).

## Problem / Intent

The Bridge board's frozen spec requires that "the board is the main-view focus
zone and a two-dimensional roving-tabindex grid: arrow keys move a card cursor
across lanes and columns, `Enter` selects the card, `Shift+Enter` opens the
assigned agent's workspace. `Ctrl+B` opens the Bridge view"
(`surfaces.md:236-239`), and that "a board with no issues at all renders a
single centered empty-state message on the panel surface" (`surfaces.md:242-243`).
Neither exists: today every card is its own Tab stop and the only board keyboard
handling is per-chip Enter/Space (`Bridge.tsx:88-91`, `IssueCard.tsx:78-84`),
and an issueless board renders an all-`dim` empty grid. This record designs both.

## Approach

### The central fork: where does the keyboard runtime live?

The Compass keyboard layer is deliberately **contracts-only with zero runtime**.
`zones.ts:4-7` states it in its header:

> "CONTRACTS ONLY: interfaces, type unions, and documented ordering. No runtime
> behavior, no DOM, no Solid components — compass-ui owns the implementation."

`zones.ts` defines `FocusZone`, `RovingGroup { zone, id }`,
`RovingDirection = "prev" | "next" | "first" | "last"`, and the
`FocusZoneController` interface (`focusZone()`, `registerRovingGroup()`,
`moveWithinGroup(direction)`, `focusPane()`); `commands.ts:108-111` defines
`CommandRegistry` (`register(cmd)` / `get(id)` / `all()`); `keymap.ts` carries
`DEFAULT_KEYMAP` with the exact chords this record needs already tabled —
`{ chord: "Mod+B", commandId: cmd("view.bridge") }`,
`{ chord: "ArrowUp", commandId: cmd("list.movePrev") }` through
`ArrowLeft/ArrowRight → list.moveLeft/list.moveRight`, `Enter →
list.openOrSelect`, `Home/End → list.moveFirst/list.moveLast` — but no
dispatcher reads that table. Grep proof this session: a search for
`from ".*keyboard/(zones|keymap|commands)"` across `apps/ui/src` returns **zero
consumers**, and `FocusController|createFocus|useFocus|CommandRegistry` matches
only the interface declaration at `commands.ts:108`.

**Decision (ratified — RD-1): Fork A-minimal.** Stand up the
shared keyboard-spine runtime, but only the slice the board exercises:

1. a `CommandRegistry` implementation (`createCommandRegistry`, commands.ts
   shape verbatim),
2. a keymap **dispatcher** — one window-level keydown listener that resolves a
   `KeyboardEvent` to a chord string, looks it up in `DEFAULT_KEYMAP`
   (respecting the documented precedence: "the scoped entry takes precedence
   while its zone is active", `keymap.ts` KeymapEntry doc), and runs the
   registered command,
3. a **roving-group primitive** (`createRovingGroup`) that owns the
   one-tab-stop bookkeeping (`tabindex="0"` on exactly one element, `-1` on the
   rest — `zones.ts:28-31`: "One tab stop per zone… arrow keys move the
   selection (and the single tab stop) within the group") and routes the
   `list.*` commands to the active group's movement handler.

NOT in this slice: zone cycling (`Ctrl+1..3` / `F6`), the escape ladder, pane
focus, the palette. Those stay contracts-only until their own lanes flip — the
foundation plan already frames this incrementality: "Keyboard spine lands as
pure addition… each surface registers commands as it flips"
(`compass-ux-foundation/design.md:800-802`). The board is the spine's first
consumer; the tree, the topic list, and the tab strips (all specced on the
same roving model —
`surfaces.md:124-127` tree, `:170-173` right-sidebar tab strip, `:297-300`
topic list, `:361-365` workspace pane strip) then have a home instead of each
re-inventing a local keydown handler.

**Why not Fork B (board-scoped roving, defer the spine)?** B is smaller now but
guarantees rework. `Ctrl+B` alone does not settle it — flip item 6 lists only
"register the board commands (open card, open assigned agent)"
(`surfaces.md:264-265`); the global `Ctrl+B` lives in the Focus-and-keyboard
prose (`surfaces.md:239`) and is deliverable under B via a one-off windowed
listener (the B-shrink path). The honest case for A is twofold: four more
surfaces are specced on the same roving model
(`surfaces.md:124-127,170-173,297-300,361-365`), so the bookkeeping is built
once here instead of re-invented per surface; and B would stand up a second
movement convention beside the frozen contract — a local model the spine later
has to absorb. A-minimal costs roughly one extra right-sized task (T1) over B
and eliminates that debt.

**The 1-D contract vs the 2-D board.** `FocusZoneController.moveWithinGroup`
takes only `RovingDirection = "prev" | "next" | "first" | "last"`
(`zones.ts:43`, `:104-108`) — 1-D — while the keymap already binds
`list.moveLeft`/`list.moveRight`. Resolution: the dispatcher routes `list.*`
commands to the **active group's own handler**; the group owns its movement
semantics (1-D lists implement prev/next; the board grid implements the 2-D
model below). `moveWithinGroup` stays satisfiable as the 1-D convenience path;
the frozen `zones.ts` file is not edited (a frozen record/contract is amended
by addition, never rewritten). This is a design decision here, not a contract
change.

### The 2-D sparse-grid cursor model

The board DOM (`Bridge.tsx:197-288` issues tab, `:291-368` PRs tab) is a CSS
grid of (lane rows × `BOARD_LANES` columns), where each `.bridge-cell` holds
0..N cards (`<For each={items}>… <IssueCard/>`, `Bridge.tsx:269-279`) and empty
cells render dim (`classList={{ dim: items.length === 0 }}`,
`Bridge.tsx:265-267`). Swimlane mode prepends a `.bridge-lane` gutter button
per agent (`Bridge.tsx:244-247`); status mode has one row and no gutter. The
five columns are `BOARD_LANES` (`constants.ts:17-31`: queued / blocked /
in_progress / in_review / done); the PRs tab uses `PR_LANES`
(`constants.ts:38-52`).

**Cursor stops are cards + gutter heads, never cells.** An empty cell holds
nothing actionable, so it is not a stop; traversal skips it. (This honors the
spec's empty-cell rule — "an empty lane… renders as an empty cell, not a
placeholder card", `surfaces.md:241-242` — a placeholder stop would be a
placeholder card in keyboard space.)

**Coordinate model.** The cursor is a card id (or a gutter agent id), with its
position derived from board data: `(row, col, indexInCell)` where `row` = lane
(agent in swimlane mode, the single row in status mode), `col` = column index
into `BOARD_LANES`/`PR_LANES`, and the gutter is **column −1** (swimlane mode
only). Deriving from the id keeps the cursor stable across reactive updates:
a card that moves columns keeps the cursor; a card that disappears drops the
cursor to the nearest remaining stop in the same column (next card in the
flattened column order, else previous, else the first stop on the board).

**Traversal:**

- **`ArrowDown` / `ArrowUp` (`list.moveNext` / `list.movePrev`)** — move
  through the current **column flattened top-to-bottom**: down through the
  cards of the current cell's stack, then into the next row's cell in the same
  column, skipping empty cells. A column is thus one continuous list, matching
  the 1-D `prev`/`next` semantics of `RovingDirection`. From a gutter head
  (column −1), Down/Up moves to the adjacent row's gutter head.
- **`ArrowRight` / `ArrowLeft` (`list.moveRight` / `list.moveLeft`)** — move
  along the current **row** to the nearest non-empty cell in that direction,
  landing on that cell's card whose `indexInCell` matches the departing card's
  (clamped to the stack length — the "nearest visual neighbor" rule, cheap and
  predictable). Empty cells are skipped. `ArrowLeft` from the first non-empty
  column reaches the gutter head (column −1) in swimlane mode; `ArrowRight`
  from the gutter enters the row's first non-empty cell. No wrap: at the row's
  last stop, `ArrowRight` is a no-op (clamp), likewise `ArrowLeft` at the
  gutter (or first column in status mode).
- **`Home` / `End` (`list.moveFirst` / `list.moveLast`)** — first / last stop
  of the **current column** (flattened). Rationale: a kanban column is the
  scanning unit; whole-board Home/End would teleport across unrelated lanes.
- **`Enter` (`list.openOrSelect`)** — on a card: `store.selectIssue(id)`
  (`store.ts:1268-1271` — selects and syncs the roster, stays on the board,
  mirroring today's `onClick`, `IssueCard.tsx:57`); on a PR card:
  `store.selectIssue(row.issue.id)` (mirrors `onSelect`, `Bridge.tsx:352`); on
  a gutter head: `store.openAgent(agentId)` (mirrors `onClick`,
  `Bridge.tsx:247`).
- **`Shift+Enter`** — on a card: open the assigned agent's workspace
  (`store.openAgent(issue.assignee)`, the keyboard twin of today's
  `onDblClick={openAssignedAgent}`, `IssueCard.tsx:29-32,58`; no-assignee falls
  back to select, same as the dblclick fallback). `Shift+Enter` is a
  **board-group-relative** binding (`board.openAssignedAgent`), resolved by the
  active roving group ahead of the frozen `Shift+Enter → comms.newline`
  `when:"main"` entry (`keymap.ts:99`) — the board and comms both live in the
  `main` zone (`surfaces.md:236`), so a whole-zone precedence rule cannot pick
  between them; the dispatcher disambiguates by focused group, not by zone (the
  tiered dispatch model, T1 + RD-2). `Enter` and `Space` resolve
  the same way.
- **`Ctrl/Mod+B` (`view.bridge`)** — global: `store.showBridge()` (the same
  call the topbar view tab makes, `App.tsx:61`). Already tabled:
  `{ chord: "Mod+B", commandId: cmd("view.bridge") }` (`keymap.ts` Views
  block).

**One tab stop — reconciling today's per-card stops.** The whole board grid is
ONE roving group: `tabindex="0"` lives only on the cursor element; every other
card `<button>`, gutter `<button>`, and nested chip is `tabindex="-1"`. Today's
model violates this three ways, all reconciled here:

1. every `IssueCard`/`PrCard` is a `<button>` (a native Tab stop) —
   `IssueCard.tsx:50`, `Bridge.tsx:59`; they get managed `tabindex`;
2. the gutter `.bridge-lane` buttons (`Bridge.tsx:244`) join the group as
   column −1 stops rather than staying independent Tab stops;
3. the nested chips — the PR chip `role="link" tabIndex={0}`
   (`IssueCard.tsx:71-72`, the DL-097 §2 content-model compromise) and the
   PRs-tab issue-link `role="link" tabIndex={0}` (`Bridge.tsx:82-83`) — drop
   to `tabIndex={-1}` when the card participates in the board group. Their
   pointer path is untouched; their keyboard path becomes a board command:
   `Space` on the cursor card fires `board.openCardCrossLink` (the PR chip's
   `onOpenPr` on the issues tab, the issue-link's `onOpenIssue` on the PRs
   tab). `Space` is free on the board — the keymap's
   `Space → list.expandOrToggle` is "expand/collapse or toggle pin"
   (foundation D5, `compass-ux-foundation/design.md:450-451`) and the board
   has nothing to expand — so the board group maps it to the cross-link.
   On a card with no cross-link chip (no PR on the issues tab, no linked issue
   on the PRs tab), `Space` does nothing visible — but it is still **claimed**
   by the board group: the handler reports handled and the dispatcher calls
   `preventDefault` (T1), so a chipless `Space` neither selects the card nor
   scrolls the panel (every stop is a native `<button>`, `IssueCard.tsx:50`,
   whose native `Space` would otherwise fire `onClick`/scroll). Decline
   (return `false`, fall through to the next tier) is reserved strictly for a
   chord the group does not own — the `Enter → comms.send` deferral, T1/RD-2 —
   never for a claimed chord whose action is a no-op. Because nothing visually
   announces the `Space` binding, the cursor card carries `aria-keyshortcuts`
   naming it, so a screen-reader focus-mode user is told the cross-link is
   reachable.

**Focus vs selection.** The cursor is focus, not selection — moving the cursor
does NOT call `selectIssue` (Enter does). `data-selected` (the accent rule,
`IssueCard.tsx:53-55`) and `:focus-visible` (the `--cx-focus-ring`) remain the
two distinct treatments foundation D4 defines. On entering the group by Tab,
the initial cursor is the selected card if visible, else the first stop.
The cursor element scrolls into view (`scrollIntoView({ block: "nearest" })`)
— the board scrolls with sticky heads and the cursor must never move
off-screen invisibly.

**No ARIA `grid` role.** The APG grid pattern mandates uniform row/cell
structure; the board is a sparse stack-of-cards-in-cells where a cell holds
0..N interactive buttons, and forcing `role="grid"`/`row`/`gridcell` onto the
swimlane markup would misdescribe it. The stops stay what they are — buttons —
with the roving tabindex carrying the keyboard model; the container gets
`aria-roledescription="kanban board"` and an `aria-label` ("Board grid").
Refusing `role="grid"` is not the same as being accessible, so each card stop
carries a positional `aria-label`/`aria-description` derived from the same
`(row, col, indexInCell)` the cursor model already computes — e.g. "RIG-123 ·
In review · agent kestrel · 2 of 3" — so an arrowing screen-reader user hears
lane, column, and stack position, not a bare "button". This announcement
contract is what "a11y verified" (the RIG-2130 acceptance) tests in T4.

**Both tabs.** The model covers the PRs tab with the same grid shape — gutter
per `prGroups()` row, `PR_LANES` columns, `dim` empty cells
(`Bridge.tsx:291-368`) — with two PR-specific rules the stop model must state
(they sit in T3's contract). First, a PR row is an `(issue, pr)` pair and an
issue with two non-closed PRs (open + merged, per DL-196's board-ified PRs
tab) yields two rows sharing `issue.id` (`prBoardRows`, `board.ts:159-165`,
whose `flatMap` keeps every `forgeState !== "closed"` PR), so a PR card's
**stop id is composite**
(`${issue.id}::${pr.repo}#${pr.number}`), never the bare issue id — otherwise
the cursor is ambiguous between the two rows. (The same duplicate-id latent bug
already exists in selection rendering — `selected={row.issue.id ===
store.selectedIssueId()}` highlights both rows, `Bridge.tsx:349-352`; the
composite stop id keeps keyboard traversal deterministic and does not worsen
it. Fixing the selection-render double-highlight is out of this lane's scope —
noted for a follow-up.) Second, the trailing **Unassigned group**
(`agent: null`, `board.ts:172-175`) renders a non-interactive
`<div class="bridge-lane unassigned">` (`Bridge.tsx:314-317`), not a button and
with no agent to open — so its gutter head is **not a stop**: the gutter track
skips that row, and `ArrowLeft` within the Unassigned row clamps at the first
card column. Switching tabs rebuilds the group's stop list and resets the
cursor to the tab's selected/first stop.

### The empty-board message

When the active tab renders **zero stops** — the gate is the built stop list
being empty (`boardStops(...).length === 0`, T3), not `activeIssues` emptiness
directly, so both board modes agree (issues tab: `activeIssues(store.issues())`
empty, `board.ts:39-41`; PRs tab: `prGroups()` yields no rows) — the grid is
replaced by a single centered `.bridge-empty` message on the panel surface
(`--cx-bg-panel`), per `surfaces.md:241-245`: "A board with no issues at all
renders a single centered empty-state message on the panel surface. The
segmented control still shows both view labels with their counts." The toolbar
(heading, counts, segmented controls) keeps rendering — only the grid body is
replaced. Copy follows the tree-empty precedent's voice
(`LeftSidebar.tsx:406-408`: "No agents in the fleet yet — spawn one from the
command palette."):

- Issues tab: **"No issues on the board yet — promote work from the Backlog to
  see it here."**
- PRs tab: **"No open PRs yet — cards appear here when an agent opens one."**

The empty board registers no roving stops; Tab passes through the toolbar's
existing stops. Empty-state text is faint (`--cx-fg-dim` vocabulary, same as
`.tree-empty`), never a placeholder card.

Gating on the rendered stop count (not `activeIssues` emptiness) also keeps
the message exact in one swimlane edge: `boardAgents` (`board.ts:53-59`)
admits an agent only if it holds an active issue assigned to it, so active
issues with a `null` assignee render in status mode but nowhere in swimlane
mode — a board whose only active issues are all unassigned yields zero
swimlane stops while `activeIssues` is non-empty. That unassigned-in-swimlane
drop is pre-existing Bridge behavior, out of this lane's scope; gating on the
stop count means the empty-board message tracks what is actually reachable in
either mode rather than inheriting the quirk.

## Alternatives considered

- **Fork A-full (whole spine now: zones, F6 cycling, escape ladder, panes).**
  Rejected: the board exercises none of zone cycling / escape / panes; building
  them without a consuming surface is speculative and unverifiable. A-minimal
  keeps the spine honest — every runtime piece shipped has a consumer in the
  same train.
- **Fork A-thinner (dispatcher + `createRovingGroup`, defer the
  `CommandRegistry`).** The registry's `register`/`get`/`all`
  (`commands.ts:104-111`) exists to serve menus, buttons, and the palette —
  none of which ship in this lane — so the dispatcher could bind chord→handler
  through a plain map and the registry could land with the palette lane that
  consumes `all()`. Rejected: the registry is a ~10-line map wrapper whose shape
  is already frozen; deferring it splits the spine's core across two lanes for
  no real saving, and the dispatcher wants a single resolution point commands
  register into. Weighed and kept minimal-plus-registry.
- **Fork B (board-local keydown, no spine).** Rejected (RD-1: Matt ratified
  A-minimal): four sibling surfaces are specced on the same roving model, so B
  would re-invent the bookkeeping per surface and stand up a second movement
  convention beside the frozen contracts. (`Ctrl+B` is deliverable under B via a
  windowed special case, so it is not the deciding factor — the sibling-surface
  reuse is.)
- **Cell-addressed cursor (cursor names a cell, second axis to enter cards).**
  Rejected: it makes empty cells stops (a keyboard placeholder card), adds a
  mode (cell-level vs card-level), and the spec's language is a *card* cursor
  ("arrow keys move a card cursor", `surfaces.md:237`).
- **Whole-board flattening for Up/Down (row-major through all columns).**
  Rejected: crossing a column boundary on ArrowDown teleports the user across
  lifecycle states mid-scan; column-flattened Up/Down matches how a kanban is
  read.
- **`role="grid"` ARIA pattern.** Rejected above (sparse stacks misfit the APG
  uniform-grid contract); revisitable if a screen-reader audit lane later
  demands it.

## Global Constraints

Every task inherits these; none restates them.

- **Stack:** SolidJS ^1.9.13 + Vite + TypeScript; no new dependencies. Pure
  client UI — the board reads the store's reactive fleet/issue list; no server
  work in this lane.
- **Frozen contracts are read-only:** `keyboard/zones.ts`, `keyboard/commands.ts`
  are not edited. `keyboard/keymap.ts` accepts ONLY additive `DEFAULT_KEYMAP`
  entries for the new board commands (`board.openAssignedAgent`,
  `board.openCardCrossLink`), minted via the file's own `cmd()` boundary
  (`keymap.ts:19-20`).
- **Command ids:** new ids follow the tabled `noun.verbCamel` shape
  (`view.bridge`, `list.moveNext` — `keymap.ts` DEFAULT_KEYMAP); board-scoped
  ids live under `board.*`.
- **Tokens:** consume the `--cx-*` semantic tier only; `tokens.css` is
  read-only for this lane (no new token; the focus ring is the existing
  `--cx-focus-ring`, foundation D4:385-388). The stylelint guard
  (`apps/ui/.stylelintrc.cjs`) stays green.
- **Existing pointer behavior survives unchanged:** click-select
  (`IssueCard.tsx:57`), dblclick-open (`IssueCard.tsx:58`), chip clicks, gutter
  clicks. Keyboard is additive; no pointer regression.
- **One tab stop per group** (`zones.ts:28-31`): after this lane, Tab crosses
  the board in exactly one stop.
- **Ledger coupling:** product record — its freeze PR carries a same-diff
  `DECISIONS.md` delta (candidate rows in §Ledger; the driver lands them at PR
  time).
- **Verification:** biome + `moon run compass-ui:stylelint` + vitest per slice;
  keyboard behavior is unit/component-tested in vitest (fake DOM events on the
  mounted Bridge), not screenshot-tested — the visual harness is touched only
  by the empty-board task.

## Plan

### T1 — Keyboard spine runtime: registry + dispatcher (Fork A-minimal core)

Implement `createCommandRegistry(): CommandRegistry` (map-backed, last-write
wins with a dev-mode duplicate warning) and the keymap dispatcher: a
`installKeymap(registry, active)` that adds ONE window keydown listener,
normalizes the event to a chord string (`Mod` per `resolveChord`,
`keymap.ts:37-38`; modifier order Mod+Shift+Alt+Key), and resolves it through a
**three-tier model** (the ratified dispatch decision, RD-2):

1. **Active-group tier** — if a roving group is active (`active()` non-null) and
   the chord is one the group claims (the group-relative Lists-block chords plus
   the group's own extras, e.g. the board's `Shift+Enter`/`Space`), route the
   chord to the group's `handleCommand` and stop. This is why the board's
   `Enter`/`Shift+Enter`/`Space` fire board commands, not the `when:"main"`
   `comms.*` entries: the board and comms share the `main` zone
   (`surfaces.md:236`), so the frozen whole-zone precedence rule
   (`keymap.ts:48-51,95-97`) cannot disambiguate two surfaces in one zone — the
   dispatcher disambiguates by **focused group**. This refines, not contradicts,
   the KeymapEntry doc's own framing that the Lists block is "resolved against
   the active roving group at dispatch" (`keymap.ts:45-48`).
2. **Scoped tier** — else, a `when`-scoped entry whose zone is active wins over
   a window-global one (the frozen D5 ranking, `keymap.ts:48-51`). Its live
   consumers are zone-scoped commands on a non-input focus target; the one
   scoped family in today's keymap, `comms.*` (`when:"main"`), does not route
   here — the comms composer handles its own keys locally (see the guard note
   below).
3. **Global tier** — else, a window-global unscoped entry fires anywhere
   (`view.bridge`/`palette.open`/the zone chords).

A matched entry whose command is unregistered, or whose handler returns
`false` (declines), **falls through** to the next tier — so a chord the active
board group does not claim (e.g. `Mod+B` while the board is focused) reaches the
global `view.bridge`, and a claimed chord whose command is not yet registered
does not swallow it. Decline/`false` means "this tier does not own the chord's
action", never a claimed no-op (a claimed no-op reports handled — §cursor model,
Space). **`preventDefault`/`stopPropagation` on any handled group-relative
chord**: every stop is a native `<button>` (`IssueCard.tsx:50`,
`Bridge.tsx:59,244`), so an unsuppressed `Enter` fires native click AND the
command (double-select) and `Space` fires the cross-link command AND native
click — the dispatcher suppresses native activation on handled chords.
Editable-target guard: keys without modifiers (arrows, Enter, Space, Home/End)
never fire while `event.target` is an input/textarea/contenteditable — so the
one `when`-scoped family in today's keymap, `comms.*` (`when:"main"`,
`keymap.ts:98-100`), is not dispatched through tier-2 at all: the comms composer
is a text `<input>` (`ChannelView.tsx:316`) that handles `Enter`/`Shift+Enter`
in its own `onKeyDown` (`ChannelView.tsx:322-327`), so comms send/newline stay
composer-local, and board `Enter` (tier-1, board focused) vs comms `Enter`
(composer-local, board unfocused) are focus-exclusive, never contending. Register
`view.bridge` → `store.showBridge()` as
the first command; install at the App root (`App.tsx:35-42`, where the store's
router seam already binds).
New files `apps/ui/src/keyboard/registry.ts`, `keyboard/dispatch.ts`.
Interfaces: consumes `CommandRegistry`/`Command`/`CommandId` (`commands.ts`),
`DEFAULT_KEYMAP`/`KeymapEntry`/`resolveChord` (`keymap.ts`), `store.showBridge`;
produces `createCommandRegistry(): CommandRegistry`,
`installKeymap(registry: CommandRegistry, active: () => RovingGroupHandle | null): () => void`
(returns the uninstaller), and the registered `view.bridge` command.
Test cycle: vitest units — chord normalization (incl. Mod resolution both
platforms); the three-tier resolution (active-group beats scoped beats global;
board `Enter` routes to the group while `comms.send` is registered; fall-through
when a matched command is unregistered/declines); native-activation suppressed
on handled chords; editable-target guard; `Mod+B` fires `view.bridge`.

### T2 — Roving-group primitive

`createRovingGroup(opts)` in `apps/ui/src/keyboard/roving.ts`: owns the
one-tab-stop invariant over a reactive, ordered stop list (each stop an element
with a stable id), exposes the active-group handle the dispatcher routes `list.*`
commands to, moves DOM focus + `tabindex` on cursor change, and
`scrollIntoView({ block: "nearest" })`s the cursor element. Group-owned
movement: the group receives the command id (`list.moveNext`, `list.moveLeft`,
`list.openOrSelect`, …) and maps it through its own `onCommand` — 2-D semantics
stay the board's (T3), 1-D groups get a default prev/next implementation
satisfying `RovingDirection`.
Interfaces: consumes `RovingGroup` (`zones.ts:33-38`) as its identity,
`CommandId`; produces
`createRovingGroup(opts: { group: RovingGroup; stops: () => Stop[]; cursor: () => string | null; setCursor: (id: string) => void; onCommand: (id: CommandId) => boolean }): RovingGroupHandle`
where `Stop = { id: string; el: HTMLElement }` and `RovingGroupHandle`
exposes `{ group, handleCommand(id): boolean, focus(): void }`.
Test cycle: vitest units — exactly one `tabindex="0"` across stops, cursor
move refocuses, stale-cursor (stop removed) falls back per the nearest-stop
rule, Tab entry lands on the cursor stop.

### T3 — Board cursor model (pure)

`apps/ui/src/board-nav.ts`: pure functions computing the stop list and 2-D
traversal over board data — no DOM, no Solid. Stop list = gutter heads
(swimlane mode, column −1) + cards in (row, col, indexInCell) order, both tabs.
**Stop ids:** an issues-tab card's id is `issue.id`; a **PRs-tab card's id is
composite** (`${issue.id}::${pr.repo}#${pr.number}`) because an issue with two
non-closed PRs yields two rows sharing `issue.id` (`prBoardRows`,
`board.ts:159-165`) and a bare id
would be an ambiguous cursor. The trailing **Unassigned group** (`agent: null`,
`board.ts:172-175`; a non-interactive `<div>`, `Bridge.tsx:314-317`) contributes
**no gutter stop** — the gutter track skips that row and `ArrowLeft` in it clamps
at the first card column.
Movement per §Approach: column-flattened Up/Down (skip empty cells; gutter
column is its own Up/Down track), row-wise Left/Right with indexInCell clamp
and empty-cell skip, gutter at column −1 (swimlane only, non-null agents), clamp
at edges (no wrap; Left/Right into a shorter stack clamps `indexInCell` and does
not round-trip — a named, accepted asymmetry), Home/End = column first/last,
nearest-stop fallback for a vanished cursor.
Interfaces: consumes `boardAgents`/`cellItems` shapes (`board.ts:53-68`),
`prBoardGroups` (`board.ts:172-175`), `BOARD_LANES`/`PR_LANES`
(`constants.ts:17-31,38-52`); produces
`boardStops(input: BoardNavInput): BoardStop[]` and
`moveCursor(stops: BoardStop[], cursorId: string, dir: "up" | "down" | "left" | "right" | "home" | "end"): string | null`
with `BoardStop = { id: string; kind: "card" | "gutter"; row: number; col: number; index: number }`
(`id` composite for PR cards per above) and `BoardNavInput` = the mode/tab + the
same agent/issue arrays the Bridge already derives.
Test cycle: vitest units — the sparse-grid table cases: multi-card cell
stack traversal, empty-cell skip both axes, indexInCell clamp (incl. the
non-reversible Left/Right asymmetry), gutter entry/exit, the Unassigned-row gutter
hole (no stop; ArrowLeft clamps), status-mode (no gutter), PRs-tab composite-id
shape with a duplicate-issue-id row, edge clamps, vanished cursor fallback.

### T4 — Bridge wiring: the board becomes one roving group

Wire T2+T3 into `Bridge.tsx`: register the board group
(`{ zone: "main", id: "bridge-board" }` per `RovingGroup`, `zones.ts:33-38`),
managed `tabindex` on card/gutter buttons, cursor state (card id signal,
initialized to the selected card else first stop, rebuilt on tab/mode switch),
`onCommand` mapping — `list.move*` → T3 `moveCursor`, `list.openOrSelect` →
select (card) / open (gutter), `board.openAssignedAgent` (Shift+Enter) →
`store.openAgent(assignee)` with select fallback, `board.openCardCrossLink`
(Space) → the cursor card's `onOpenPr`/`onOpenIssue`. This is the one place the
three movement vocabularies meet — DOM chord (`ArrowLeft`), keymap command id
(`list.moveLeft`), and T3 direction (`"left"`) — so T4 owns the single
chord→command→direction mapping table; nothing else translates between them.
Chips drop to `tabIndex={-1}` when the card is board-hosted: `IssueCard` gains an
optional `inRovingGroup?: boolean` prop gating the chip's `tabIndex`
(`IssueCard.tsx:72`) — pointer + chip keydown handlers stay for non-board hosts;
the PrCard issue-link (`Bridge.tsx:83`) does the same. Each card stop gets the
positional `aria-label`/`aria-description` and the container the
`aria-roledescription="kanban board"` + `aria-label` per §Approach (the "a11y
verified" contract). Register the two new commands and add their `DEFAULT_KEYMAP`
entries (additive), plus `aria-keyshortcuts` on the cursor card naming the
`Space` cross-link.
Interfaces: consumes T1 `installKeymap`/registry, T2 `createRovingGroup`, T3
`boardStops`/`moveCursor`, `store.selectIssue`/`store.openAgent`
(`store.ts:1233-1235,1268-1271`); produces the wired Bridge, the
`IssueCard.inRovingGroup` prop, commands `board.openAssignedAgent` +
`board.openCardCrossLink`, and the keymap rows
`{ chord: "Shift+Enter", commandId: cmd("board.openAssignedAgent") }`,
`{ chord: "Space", commandId: cmd("board.openCardCrossLink") }` (both
group-relative — claimed by the active board group in the dispatcher's tier 1,
T1 — the Lists-block convention, `keymap.ts:45-48,78-82`).
Test cycle: `Bridge.test.tsx` component tests — one tab stop on the mounted
board, arrow traversal across a fixture with a multi-card cell + an empty cell,
Enter selects, Shift+Enter opens agent (while a stub `comms.send` is registered,
proving tier-1 wins), Space fires the chip action (and is a no-op on a
chip-less card), gutter Enter opens agent, the positional `aria-label` is
present on the cursor stop, tab-switch resets cursor, pointer paths unregressed.

### T5 — Empty-board centered message

`Show`-gate each tab's `.bridge-grid` on its stop source (issues:
`activeIssues(store.issues()).length`; PRs: `prGroups()` rows) with a
`.bridge-empty` centered fallback on the panel surface; toolbar + segmented
counts render unchanged. Copy per §Approach. CSS: a `.bridge-empty` block in
the board's stylesheet — flex-centered, faint text, panel background tokens
only. Extend the stub/fixture path so an empty board is constructible in tests
and the visual harness captures `bridge-empty.png`.
Interfaces: consumes `activeIssues` (`board.ts:39-41`), `prGroups`
(`Bridge.tsx:127-134`); produces the `.bridge-empty` markup + CSS + copy, a
`Bridge.test.tsx` empty-board assertion (message shown, zero roving stops,
toolbar intact), and the harness shot.

Order: T1 → T2 → T3 (T3 parallel to T2) → T4 → T5 (T5 independent of T2-T4,
can run parallel after T1's test scaffolding exists — it needs no keyboard
runtime).

## Tasks

- [ ] T1 — spine runtime: `createCommandRegistry` + `installKeymap` dispatcher
      + `view.bridge` (Ctrl/Mod+B) registered at App root; vitest units
- [ ] T2 — `createRovingGroup` one-tab-stop primitive with group-owned command
      routing; vitest units
- [ ] T3 — `board-nav.ts` pure 2-D sparse-grid stop list + traversal; vitest
      units for the sparse cases
- [ ] T4 — Bridge wiring: board roving group, Enter/Shift+Enter/Space
      commands, chip tab-stop demotion, additive keymap rows; component tests
- [ ] T5 — empty-board centered message (both tabs) + fixture + harness shot

## Resolved decisions

Both load-bearing forks were ratified by Matt (2026-08-21); recorded here as the
frozen calls. The remaining `## Open Questions` below hold only non-load-bearing
deferrals.

- **RD-1 (was OQ-1) — the runtime fork: Fork A-minimal.** RIG-2130 stands up the
  shared keyboard-spine runtime slice (a `CommandRegistry` implementation + a
  keymap dispatcher + a `createRovingGroup` roving-group primitive; T1+T2) with
  the Bridge board as its first consumer, then builds the board 2-D roving on
  top (T3+T4). Zone cycling, the escape ladder, pane focus, and the palette stay
  contracts-only until their consuming lanes flip. Rationale: four sibling
  surfaces are specced on the same roving model
  (`surfaces.md:124-127,170-173,297-300,361-365`), so the bookkeeping is built
  once rather than re-invented per surface, and A avoids standing up a second
  movement convention beside the frozen contracts, which say "compass-ui owns
  the implementation" (`zones.ts:6-7`). (Matt: "A, whatever gives us the best
  end state".) The Plan below is written against A-minimal.
- **RD-2 (was OQ-4) — the main-zone dispatch model: a three-tier dispatcher
  with fall-through.** The board and the comms composer both live in the `main`
  focus zone (`surfaces.md:236`), and the frozen keymap binds
  `Enter`/`Shift+Enter` both unscoped (Lists block, `list.*`) and `when:"main"`
  (`comms.send`/`comms.newline`, `keymap.ts:98-99`) — so a literal whole-zone
  precedence rule (`keymap.ts:48-51,95-97`) would fire the comms commands on the
  board's `Enter`/`Shift+Enter` and the board's own commands would never fire.
  **Decision:** the dispatcher (T1) resolves a chord in three tiers — (1) an
  **active roving group** claims its group-relative chords first (the board's
  `Enter`/`Shift+Enter`/`Space` fire board commands while the board is the
  focused group), (2) else a `when`-scoped entry whose zone is active wins over
  a window-global one, (3) else a window-global entry — with **fall-through**: a
  matched command that is unregistered or declines (returns `false`) falls to
  the next tier, so a chord the board group does not claim reaches the global
  binding. The board↔comms `Enter` collision the whole-zone rule could not
  resolve is resolved by **focus-exclusivity**: board `Enter` is tier-1 while
  the board is the focused group, and comms `Enter` is handled composer-locally
  (the comms composer is a text `<input>` with its own `onKeyDown`,
  `ChannelView.tsx:316,322-327`) and never dispatched through tier-2 anyway (the
  editable-target guard, T1). The two are focus-exclusive, so they never
  contend. This reads the frozen D5 "scoped wins while its zone is
  active" ranking as *focused-surface* precedence, not literal whole-zone
  precedence, and touches no frozen contract. (Matt ratified the three-tier
  model.) The rejected alternatives — a literal whole-zone rule forcing the
  board's verbs off `Enter`/`Shift+Enter`, and extending `when` with sub-zone
  surface scoping (which would edit the frozen contract) — are recorded in
  §Approach/T1.

## Open Questions

Only non-load-bearing deferrals remain (the record is freeze-ready with these
open — each is correct-without-resolving, per skill://design's pre-freeze gate).

- **OQ-2 (non-load-bearing, deferred): chip keyboard chord.** The nested-chip
  action rides `Space` (`board.openCardCrossLink`) because the board has no
  expand/toggle semantics for the tabled `Space → list.expandOrToggle`. If a
  later board affordance claims Space (e.g. a card peek), the chip command
  remaps; the command indirection makes that a one-line keymap change. The
  design is correct without resolving this now. (That `Space` fires at all for
  the board is settled by RD-2's active-group tier; OQ-2 is only *which* chord.)
- **OQ-3 (non-load-bearing, deferred): gutter as column −1.** The lane-head
  buttons join the roving group as a −1 column rather than staying separate
  Tab stops — chosen for the one-tab-stop invariant and spatial coherence
  (ArrowLeft from the first column reaches the lane's agent). If real usage
  wants gutters out of the arrow plane, they can be demoted to a skip-listed
  stop kind without touching the traversal core (they are a distinct
  `kind: "gutter"` in T3's stop model).

## Ledger

This record's decisions are ledgered in `DECISIONS.md` (UX foundation section)
as **DL-219..222**, landed in the same PR that freezes this record (the
design-ledger touch-coupling rule). The rows:

1. **DL-219 (candidate):** the keyboard spine lands minimal-first — a
   `CommandRegistry` implementation, a `DEFAULT_KEYMAP` dispatcher, and a
   roving-group primitive with group-owned movement semantics — with the
   Bridge board as first consumer; zone cycling, the escape ladder, pane
   focus, and the palette stay contracts-only until their consuming lanes
   flip. The spine's movement API is command-id routing through a
   `RovingGroupHandle` (T2), which is a second surface beside the frozen 1-D
   `FocusZoneController.moveWithinGroup(RovingDirection)` (`zones.ts:104-108`):
   when the zone controller later lands it must delegate `moveWithinGroup` into
   the handle or amend `zones.ts` additively — recorded here so the foundation
   lane inherits the relationship deliberately. (RD-1: Fork A-minimal, ratified
   by Matt.)
2. **DL-220 (candidate):** the Bridge 2-D cursor model — stops are cards +
   gutter heads (gutter = column −1, swimlane only), never cells; Up/Down
   column-flattened, Left/Right row-wise with indexInCell clamp, empty cells
   skipped, edges clamp (no wrap); focus is distinct from selection; no ARIA
   `grid` role on the sparse board.
3. **DL-221 (candidate):** nested card chips (the DL-097 `role="link"`
   compromise) leave the Tab order when the card is hosted in a roving group
   (`tabIndex={-1}`); their keyboard path is a registered board command on the
   cursor card. Refines DL-097 §2's keyboard clause for roving-group hosts;
   pointer behavior unchanged.
4. **DL-222 (candidate):** the keymap dispatcher resolves a chord in three
   tiers — active roving group (group-relative chords) → `when`-scoped zone
   entry → window-global entry — with fall-through when a matched command is
   unregistered or declines; this reinterprets D5's "scoped wins while its zone
   is active" (`keymap.ts:48-51`) as focused-surface precedence so the board and
   the comms composer, both in the `main` zone, disambiguate by focus rather
   than collide. (RD-2: three-tier dispatch, ratified by Matt.)
