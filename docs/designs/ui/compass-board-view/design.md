# Compass board: Issues/PRs tabs, cross-linking, PR-card badges

Status: Active

Tracker: RIG-1633.

Ledger: this record's PR appends DL-097 to
`docs/designs/product/DECISIONS.md` in the same diff (see §Ledger delta) and
supersedes no existing row (§Ledger delta states the call).

## Problem

The Bridge board is issues-only: there is no surface that answers "what PRs are
in flight and what state are they in" without clicking through cards one at a
time, and the issue card's PR status is a per-check pip strip
(`IssueCard.tsx:48-57`, one `check-pip` per check) that scales badly past a few
checks and buries the two facts that matter at board distance — is CI green,
and what did review say. Issues and their PRs also don't link: the card shows
`#{number}` as inert text, and no PR surface points back at its issue.

## Approach

A view remodel of the Bridge, three moves, no model change — the #1018
canonical types already carry everything
(`stub-data.ts:236-238` — `Issue.prs`: "Every PR opened for this issue,
discovery order (newest last)"; `:126-133` — `ChecksSummary.state: "pending" |
"success" | "failure"` ("The roll-up"); `:151-162` — `Review` with
submission-ordered `reviews` so "a reviewer's CURRENT verdict is its last
entry").

### 1. Issues / PRs tabs inside the Bridge

The split is a **peer tab strip inside the board view** — a new
`BoardTab = "issues" | "prs"` signal local to `Bridge.tsx`, exactly the shape
of the existing `BoardMode` toggle (`Bridge.tsx:14-16` "How the board groups
rows", `:23` `createSignal<BoardMode>("swimlane")`). Not a new top-level
`View` (the `View` union `store.ts:67-73` routes whole surfaces —
`"bridge" | "backlog" | "done" | …` — and the PRs tab is still the Bridge),
and not a third `BoardMode` (mode is *how issues group*; the tab is *which
artifact kind shows* — orthogonal axes, and conflating them would make
"status-grouped PRs" a reachable nonsense state).

- **Issues tab** = today's board unchanged: `BOARD_LANES` columns,
  swimlane/status `BoardMode` toggle, `boardAgents`/`cellItems`/`laneTotal`
  partition (`board.ts:45-71`). Its swimlane row order is Record C's frozen
  `treeOrder(agents)` contract (see §Merge order) — "the board takes
  `treeOrder`'s sequence and then keeps only `boardAgents` rows" (C record
  §T5). This record cites that contract; it does not redesign it.
- **PRs tab** = a flat row list, **one row per open PR** (an issue with two
  open PRs contributes two rows), grouped by assignee agent with the groups in
  the same `treeOrder` sequence and an "Unassigned" group last. One-row-per-PR
  is grounded in the model: `Issue.prs` is "Every PR opened for this issue"
  (`stub-data.ts:236-238`) — `primaryPr` (`board-render.ts:19-27`) is a
  card-level *compression* for surfaces that can show only one chip; the PRs
  tab is the surface where the compression must NOT apply, or a second open PR
  becomes invisible everywhere.
- **Open PRs only** (`forgeState === "open"`, drafts included): the PRs tab is
  an attention surface, and a PR needing attention is by definition open. This
  leaves a gap by design: a **non-primary** closed/merged PR — one that
  `primaryPr` does not pick (a closed-unmerged PR alongside an open one, or a
  non-primary merged/closed PR on a done issue) — surfaces **nowhere** in the
  UI today, because the card chip, the Done row, and the right-sidebar PR pane
  each render only `primaryPr` (`board-render.ts:19-27`, `DoneView.tsx:33-66`,
  `RightSidebar.tsx:582-588`). A *sole* closed-unmerged PR is the exception —
  it is `primaryPr` via the `?? prs[prs.length-1]` fallback, so it still shows
  on its card chip and Done row, just not on this open-only tab. Fully closing
  the gap needs an all-PRs surface, out of scope here (OQ-1's alternative).
  Issues in any lifecycle state contribute (the predicate is on the PR, not the
  issue); pre-active issues rarely have PRs (`prs` is "empty before the first").
- **PR-count on the tab** = the number of rows the tab shows, i.e. **open PRs
  across issues** (not issues-with-PRs), and — when C's subtree scope is active
  — only the rows that scope keeps: the count's job is "how much is in that
  tab", and a row is a PR. Counting issues-with-PRs, or counting unscoped while
  the rows are scoped, would each disagree with the visible row count.
- The `Swimlanes | Status` seg control renders only on the Issues tab — the
  PRs tab has one fixed layout, so showing a dead toggle would imply a choice
  that doesn't exist.

A **PR row** renders: `prBadge` state (`board-render.ts:50-54`), the forge
coordinate `{repo}#{number}` (host-qualified via the `isMultiForge` trigger,
same rule as `issueKey`), the PR title, the CI badge + review badge (below),
the thread tally `resolved/total` (counting `ReviewThread.resolved`,
`stub-data.ts:144-149`), and the owning issue's `issueKey` as the cross-link
chip.

### 2. Cross-linking issues ↔ PRs

Selection stays **issue-keyed** — no new PR-selection store state. Both tabs
share `store.selectedIssueId()` (`store.ts:243-244`) as the highlight key, and
both directions of the link ride the existing nav verbs
(`selectIssue`, `store.ts:870-873`, "Select an issue (card / swimlane cell)
and sync the roster"; `openAgent`, `:818`):

- **Issue card → PR:** the card's `card-pr` chip (today inert text
  `#{p().number}`, `IssueCard.tsx:60`) becomes interactive: clicking it
  selects the issue and flips the Bridge to the PRs tab, where that issue's
  row(s) carry the selected style. The tab signal is Bridge-local, so Bridge
  passes IssueCard an optional `onOpenPr?: () => void` callback prop; a card
  rendered anywhere without the prop keeps the chip inert. Because the whole
  card is a `<button>` (`IssueCard.tsx:33-40`), the chip must not nest a
  second button: it becomes a `<span role="link" tabindex="0">` with
  click/keydown handlers that `stopPropagation()`. That downgrades but does not
  eliminate the defect — a `tabindex="0"` span is still an interactive
  descendant, which the `<button>` content model forbids, so AT exposure stays
  imperfect; the clean fix (card as `div[role="button"]`) is deliberately out
  of this record's scope (it restructures the pre-existing card).
- **PR row → issue:** the row's `issueKey` chip calls
  `selectIssue(row.issue.id)` and flips back to the Issues tab; the row's
  agent group header reuses the swim-gutter behavior
  (`store.openAgent(agent.account.id)`, `Bridge.tsx:104-107`). The row body
  itself (like a card) selects the issue without leaving the tab, so the
  right-sidebar PR pane — which already renders from the selected issue —
  follows.

### 3. Card status collapses to CI + review badges

The per-check pip strip on the issue card (`IssueCard.tsx:48-57` — `<For
each={checks().checks}>` → `check-pip` per check) is replaced by two compact
badges, derived in `board-render.ts` alongside `checkPip`/`prBadge` so card,
Done view, and PR rows cannot drift:

- **CI badge** = the roll-up `ChecksSummary.state` read directly — it is
  already the 3-valued roll-up ("pending" | "success" | "failure",
  `stub-data.ts:126-133`); no new helper is needed, and inventing one would
  duplicate the model's own contract. No `checks` → no CI badge (same
  `Show when={p().checks}` guard as today).
- **Review badge** = a new pure helper `reviewBadge(pr)` rolling
  latest-per-author verdicts to one value. Precedence, stated explicitly:
  take each author's LAST review in submission order (the model's own rule:
  "a reviewer's CURRENT verdict is its last entry", `stub-data.ts:151-154`),
  bots included (the existing bot chips already take latest-per-author);
  then **`changes_requested` dominates** (one blocking reviewer blocks),
  **else `approved`** if any (someone signed off and nobody objects),
  **else `commented`**, and empty `reviews` → `undefined` (no badge). The
  helper returns the display vocabulary `"changes" | "approved" |
  "commented"` — the `changes_requested`→`changes` map moves INTO the total
  function (one owner) rather than repeating per chip site.
- The Done view's identical pip strip (`DoneView.tsx:37-46`) collapses to the
  same two badges pending OQ-2. The right-sidebar `CheckRuns` pane
  (`RightSidebar.tsx:188-205`) is the per-check DETAIL surface and keeps its
  pips — the collapse is a board-distance move, not a pip ban. This card/Done
  collapse supersedes the #1018 issue-model record's explicit pip-per-check
  prescription for those two surfaces (`compass-issue-model/design.md:712-728`,
  "substituting [the roll-up] would collapse a 3-check PR's three pips into
  one"), authorized by Matt's 2026-08-01 dispatch; `CheckRuns` keeps the
  per-check pips that record protects.

## Merge order

This record merges AFTER Record C (agent trees, PR #1058, DL-095) and Record A
(sidebar pins, PR #1059, DL-096). The Issues tab's swimlane ordering and the
PRs tab's group ordering both assume C's `board.ts` helpers exist:
`treeOrder(agents: readonly Agent[]): Agent[]` ("depth-first order over the
derived tree … the swimlane ordering", C record §T5) and
`subtreeAgentIds(agents, rootAgentId): ReadonlySet<string>` ("the subtree
membership set, the board filter predicate … includes `rootAgentId`") — when
C's subtree scope filter is active, the PRs tab honors the same predicate
(rows whose `issue.assignee` is in the set, plus the unassigned group only in
the unscoped view). C is now merged (PR #1058), so DL-095 is on `main` and the
DL-097 row's reference resolves; A (DL-096) merges before this record.

## Global Constraints

- **Sequencing:** merges AFTER Record C (#1058, DL-095) and Record A (#1059,
  DL-096); the swimlane/group ordering consumes C's `treeOrder` and
  `subtreeAgentIds` (`board.ts`) as frozen contracts — do not reimplement or
  fork them.
- **NO model change:** no proto, server, or `stub-data.ts` type edits. The
  #1018 canonical `Issue`/`PullRequest`/`ChecksSummary`/`Review` types
  (DL-067..070 + DL-091, frozen) carry every field this view reads. A task that seems
  to need a model field is a STOP-and-escalate, not a local addition.
- **Derivations live in `board-render.ts`; partitions in `board.ts`** — pure,
  total functions over injected lists (no store/fixture imports), matching
  `checkPip`/`prBadge`/`primaryPr` and `boardAgents`/`cellItems`. Every new
  helper ships with `board-render.test.ts` / `board.test.ts` cases in the
  existing table style.
- SolidJS + existing board patterns: local `createSignal` for view-local UI
  state (the tab, like `BoardMode`), store verbs (`selectIssue`, `openAgent`)
  for navigation; no new store state unless a task names it.
- No nested `<button>` inside the card button — interactive chips inside
  `IssueCard` use `role="link"` + keyboard handling + `stopPropagation()` (a
  content-model compromise inherited from the card-as-button, §2).
- Naming/copy: tab labels exactly `Issues` and `PRs`; the PRs tab count is the
  visible row count.

## Plan

### T1 — roll-up helpers (`board-render.ts`)

The pure derivations both tabs and the card share. No UI change yet.

- Interfaces:
  - `reviewBadge(pr: PullRequest): "changes" | "approved" | "commented" |
    undefined` — latest-per-author over `pr.reviews` (submission order, bots
    included), then precedence `changes_requested` > `approved` >
    `commented`; `[]` → `undefined`. Maps `changes_requested`→`changes`
    inside the helper (the one owner of that copy rule).
  - `ciBadge(pr: PullRequest): ChecksSummary["state"] | undefined` — a thin
    total accessor `pr.checks?.state`, exported so every badge site reads the
    roll-up through one named seam (and the "pips iterate `checks`, NOT the
    roll-up" doc comment inverts here on purpose).
  - `openPrs(issue: Issue): PullRequest[]` — `issue.prs.filter(p =>
    p.forgeState === "open")`, `prs` order preserved; the PRs-tab row source.
- Tests: `board-render.test.ts` — reuse its `pr()`/`issue()` fixture
  builders (`board-render.test.ts:23-39,43-63`); cases: same-author
  supersession in BOTH directions — author's later `approved` beats their
  earlier `changes_requested`, AND a later `commented` supersedes their earlier
  `approved` (the badge drops, pinning the model rule in the unflattering
  direction) — cross-author `changes_requested` dominance, approved vs
  commented, empty → undefined; `openPrs` keeps order and drops merged/closed.

### T2 — PR partition (`board.ts`)

The pure PRs-tab grouping, beside the issue partition.

- Interfaces:
  - `type PrRow = { issue: Issue; pr: PullRequest }` (exported from
    `board.ts`).
  - `prRows(all: readonly Issue[]): PrRow[]` — flat-maps `openPrs` over every
    issue (any lifecycle state), issue order then `prs` order preserved.
  - `prRowGroups(agents: readonly Agent[], all: readonly Issue[]):
    { agent: Agent | null; rows: PrRow[] }[]` — groups `prRows` by
    `issue.assignee`; group sequence = `treeOrder(agents)` (C's contract)
    filtered to agents with ≥1 row, then one `agent: null` ("Unassigned")
    group last iff non-empty.
  - `prCount(all: readonly Issue[], scope?: ReadonlySet<string>): number` —
    open-PR row count; when `scope` (C's `subtreeAgentIds`) is passed, counts
    only rows whose `issue.assignee` is in it, so the badge equals the visible
    row count. `prRows(all).length` when unscoped.
- Tests: `board.test.ts` — two-open-PR issue yields two rows; merged/closed
  excluded; group order follows `treeOrder` (parent/child fixture); unassigned
  group last and absent when empty; `prCount` equals total rows unscoped, and
  with a `scope` set counts only in-scope-assignee rows (unassigned excluded).

### T3 — Bridge tabs + PRs tab render (`Bridge.tsx`)

- Interfaces:
  - `type BoardTab = "issues" | "prs"`; `createSignal<BoardTab>("issues")`
    beside the existing `BoardMode` signal (`Bridge.tsx:23`).
  - Toolbar: a tab seg — `Issues`, and `PRs · {prCount(store.issues(),
    scope())}` where `scope()` is C's active `subtreeAgentIds` (or `undefined`
    when unscoped) — added to
    `bridge-toolbar` (`Bridge.tsx:45-66`); the `Swimlanes|Status` seg wrapped
    in `<Show when={tab() === "issues"}>`.
  - PRs tab body: `<For each={prRowGroups(STUB_AGENTS, store.issues())}>` →
    group header (agent handle, `openAgent` on click, reusing swim-gutter
    affordances) + `<For each={rows}>` → `PrRow` rows. Row body click →
    `store.selectIssue(row.issue.id)`; `classList={{ selected: row.issue.id
    === store.selectedIssueId() }}`.
  - A `PrRow` row component (in `Bridge.tsx` or a sibling `PrRowItem.tsx` if
    it crosses ~60 lines): `prBadge(pr)` state chip, `{pr.repo}#{pr.number}`
    (host-qualified when `isMultiForge(store.issues())`), `pr.title`, CI
    badge (`ciBadge`), review badge (`reviewBadge`), thread tally
    `{resolved}/{total}`, and the owning issue's
    `issueKey(row.issue, multiForge)` chip → `selectIssue` + flip to Issues
    tab.
  - When C's subtree scope is active, the PRs tab filters rows by
    `subtreeAgentIds` membership of `issue.assignee` (unassigned group only
    unscoped) — same predicate as the Issues tab — and the same `scope()` set
    feeds `prCount` (above), so the tab badge tracks the filtered rows.
- Tests: a new `Bridge.test.tsx` (none exists today — `App.test.tsx` covers
  routing): tab flip renders rows; PR count in the tab label matches
  `prCount`; mode seg hidden on PRs tab; issueKey chip flips tab + selects.
- CSS: `app.css` — tab seg reuses `.seg`; new `.pr-row`/`.pr-group` styles;
  badge styles in T4.

### T4 — card + Done badges (`IssueCard.tsx`, `DoneView.tsx`, `app.css`)

- Interfaces:
  - `IssueCard.tsx:48-57`: the `check-pips` `<For>` strip is REPLACED by
    `<span class="ci-badge" data-status={ciBadge(p())}>` (guarded by the
    existing `Show when={p().checks}`) + `<Show when={reviewBadge(p())}>` →
    `<span class="review-badge" data-verdict={…}>`. `checkPip` import drops
    from the card.
  - New prop `onOpenPr?: () => void` on `IssueCard`; the `card-pr` chip
    becomes `role="link"` + `tabindex="0"`, click/Enter →
    `stopPropagation()` + `onOpenPr`; without the prop the chip stays inert.
    Bridge passes `() => { store.selectIssue(issue.id); setTab("prs"); }`.
  - `DoneView.tsx:37-46`: same pip-strip → badges replacement (pending OQ-2's
    default: yes, collapse it too).
  - `app.css:763-782`: `.check-pips`/`.check-pip` rules stay (RightSidebar
    `CheckRuns` still uses them, `RightSidebar.tsx:188-205`); add
    `.ci-badge[data-status]` and `.review-badge[data-verdict]` color rules on
    the existing `--add`/`--del`/`--warn` vars.
- Tests: `board-render.test.ts` already covers the helpers (T1); card-level
  DOM assertions go in `Bridge.test.tsx` (selected-card badge presence) —
  there is no existing `IssueCard` test file to extend.

## Tasks

- [ ] T1 — `reviewBadge` / `ciBadge` / `openPrs` in `board-render.ts` +
      `board-render.test.ts` cases
- [ ] T2 — `PrRow` / `prRows` / `prRowGroups` / `prCount` in `board.ts` +
      `board.test.ts` cases
- [ ] T3 — `BoardTab` seg in `Bridge.tsx`, PRs tab render, count badge,
      cross-link chips, `Bridge.test.tsx`
- [ ] T4 — card + Done pip strips → CI/review badges, `onOpenPr` chip,
      `app.css` badge styles

## Ledger delta

This PR appends one row under `## UI shell` in
`docs/designs/product/DECISIONS.md` (after DL-091, where the board/issue-model
decisions sit). It supersedes no ledger ROW: DL-031 (board-primary shell),
DL-067/069 (issue as board unit, canonical types), and DL-091 (archive
lifecycle) all stand — this row composes a view layout onto them; no Active row
asserts the board's tab structure, the card's pip strip, or PR-surface shape.
It does supersede, at PROSE granularity, the #1018 issue-model record's
pip-per-check prescription for the card and Done view (§3; that record's
`CheckRuns` pips stand) — no ledger row change needed, per Matt's 2026-08-01
dispatch. C is merged (#1058), so this row's DL-095 reference resolves on
`main`; A (DL-096) merges before it (§Merge order).

> | DL-097 | The Bridge board splits into peer Issues/PRs tabs inside the board view (a Bridge-local tab signal, not a new `View` or a third `BoardMode`): the Issues tab is today's board ordered by Record C's `treeOrder`; the PRs tab is a flat one-row-per-OPEN-PR list (the `primaryPr` compression deliberately does not apply) grouped by assignee in `treeOrder` with the tab badge = the open-PR row count; issues ↔ PRs cross-link through the existing issue-keyed selection (`selectIssue`, no new PR-selection state); and the card's per-check pip strip collapses to a CI badge (the `ChecksSummary.state` roll-up) + a review badge (latest-per-author verdict, `changes_requested` > `approved` > `commented`) as pure `board-render.ts` helpers — a VIEW remodel consuming the frozen #1018 model, no proto/server/model change | Active (Matt, 2026-08-01) | [board view §Approach](compass-board-view/design.md#approach) |

## Open Questions

None are load-bearing; the record designs against the stated defaults.

- **OQ-1 — PRs tab scope: open-only vs all PRs.** Designed default: open PRs
  only (drafts included) — an attention surface should show actionable items,
  and the non-primary-closed-PR gap (§1) is a known limitation that no current
  surface covers.
  Alternative: show all with the badge counting open only; closes the gap but
  adds noise for little value. **Recommendation: open-only.**
- **OQ-2 — Done view pip strip.** The dispatch names the *card*; the Done view
  has the identical strip (`DoneView.tsx:37-46`). Designed default: collapse
  it to the same two badges (one derivation, no drift). Alternative: leave
  Done's pips as a historical-detail surface. **Recommendation: collapse both;
  the per-check detail home is the RightSidebar `CheckRuns` pane.**
- **OQ-3 — cross-link tab flip.** Designed default: the card's PR chip flips
  the Bridge to the PRs tab (a true cross-link between the tabs). Alternative:
  the chip only selects the issue and lets the right-sidebar PR pane show the
  detail, no tab flip. **Recommendation: flip — the link should land where the
  PR rows live; the sidebar already follows selection either way.**
