# Compass keyboard spine at the App root (RIG-2456)

Status: Draft

Realization design for the slice the parent record froze and RIG-2130
deferred: the parent record
`docs/designs/product/compass-bridge-keyboard-nav/design.md` T1 (§352-416)
froze the WHAT — install the keymap dispatcher **once at the App root**
("install at the App root (`App.tsx:35-42`, where the store's router seam
already binds)", §403-405) and register `view.bridge → store.showBridge()` as
the first command (§403-404, checklist §585-586) — and the RIG-2130 as-built
footnote (§537-558) records that the shipped implementation installed the
spine **board-locally** in `Bridge.tsx` and deferred the App-root mount +
`view.bridge` to this follow-up. This record designs the HOW. It does not
re-decide the WHAT and does not re-litigate DL-219..222 or the parent's RD-1
(A-minimal runtime) / RD-2 (three-tier dispatch with fall-through).

## Problem / Intent

`Mod+B` is a dead chord: the keymap tables it
(`apps/ui/src/keyboard/keymap.ts:64` — `{ chord: "Mod+B", commandId:
cmd("view.bridge") }`) but no production code registers the `view.bridge`
command, and the only `installKeymap` call lives inside the Bridge component
(`apps/ui/src/components/Bridge.tsx:428-468`) — so the keyboard spine exists
only while the board is mounted, and no chord works from the agent / channel /
backlog surfaces. Lift the install to the App root, register `view.bridge`
globally, and migrate the Bridge off its per-component install **without
regressing the RIG-2130 focus-exclusivity contract** (board tier-1 claim only
while a board stop holds DOM focus — the `() => rovingGroup.isFocused() ?
rovingGroup : null` gate, `Bridge.tsx:463-467`, locked by the
`Bridge.test.tsx` focus-exclusivity test).

## Global Constraints

- SolidJS ^1.9.13 (v1 on main); Bun 1.4 test toolchain; tests run
  `cd apps/ui && bun test --conditions browser`; single root `/biome.json`
  (2.5.4).
- `apps/ui/src/keyboard/dispatch.ts` is **not modified**: the
  `installKeymap(registry, active, activeZone?)` signature and the three-tier
  fall-through semantics (RD-2) are frozen and already correct for a root
  install — this record only changes *who calls it* and *what the accessors
  close over*.
- The RIG-2130 focus gate is inviolable: whatever publishes a roving group to
  the root dispatcher, `RovingGroupHandle.isFocused()`
  (`apps/ui/src/keyboard/roving.ts:31`, `stops().some(s => s.el ===
  document.activeElement)`) remains the sole predicate deciding a tier-1
  claim. The shipped focus-exclusivity test and the tier-1-beats-`comms.*`
  precedence test (`Bridge.test.tsx:376-413`) must stay green (possibly
  re-hosted, see T3 — the *assertions* are the contract, not the mount
  shape).
- Do not edit the frozen parent record; reference it. The frozen zone
  contract `apps/ui/src/keyboard/zones.ts` stays contracts-only except where
  this record explicitly realizes a slice of it.
- Exactly ONE `window` keydown listener in production at any time (the
  dispatcher's own invariant, `dispatch.ts:2-3`): no overlap window in which
  both the App root and the Bridge install.
- Ledger: the PR freezing this record must, in the same diff, append its DL
  rows to `docs/designs/product/DECISIONS.md` (DL-MAX is currently DL-222)
  and carry a `Ledger-impact:` line. The coordinator writes the ledger delta
  at PR time — it is intentionally **not** authored here. Expected rows: the
  spine-home decision (store-hosted keyboard seam), the group-publication
  model (registered-set + `isFocused()` scan), and the additive
  `CommandRegistry.unregister` contract extension.

## Approach

One sentence: hang a small **keyboard spine** off the app store
(`store.keyboard`: the shared registry + a registered-set of roving groups),
install `installKeymap` exactly once in `App.tsx` with accessors that derive
the active group by scanning the registered set with the RIG-2130
`isFocused()` gate, register `view.bridge → store.showBridge()` at spine
creation, and migrate the Bridge to a pure *consumer* (register commands +
publish/retract its group; no install, no own registry).

### A1 — Where the shared registry lives: on the store (`store.keyboard`)

Add `keyboard: KeyboardSpine` to `AppStore` (`apps/ui/src/store.ts`), created
inside `createAppStore` by a new `createKeyboardSpine()`
(`apps/ui/src/keyboard/spine.ts`). Rationale:

- The store is already the app's one shared seam: both production
  (`index.tsx:124-128`) and the test harness (`test-router.tsx:42-53`) create
  exactly one `createAppStore()` and provide it via `StoreContext`; every
  surface reaches it with `useStore()` (`context.ts:13-19`). A store-hosted
  spine needs **zero new providers** and is reachable from any surface that
  can already act (a command's `run()` almost always calls a store method
  anyway).
- `view.bridge`'s behavior IS a store method (`store.showBridge`,
  `store.ts:1883`), so the store registering it at spine creation puts the
  registration next to the behavior — and makes `Mod+B` testable through the
  real wiring with no App-specific setup.
- The "standalone board window" story behind Bridge's fresh-registry default
  (`Bridge.tsx:168`, parent §430) survives: a future standalone window has
  its own store, hence its own registry.

Alternative considered — a separate `KeyboardContext` provider: keeps input
plumbing out of the (large) store, but adds a second provider that
`index.tsx`, `test-router.tsx`, and every direct component-test mount must
now thread, and splits "the seam surfaces read" in two. Rejected for churn
with no capability gain. Alternative — App-local
`createCommandRegistry()` inside the App component: cheapest diff, but then
sibling surfaces (agent/channel, the palette's `registry.all()`) cannot
register or enumerate commands without prop drilling through the router,
which is exactly the per-component shape being retired. Rejected.

### A2 — How a surface publishes its active roving group: a registered set, scanned by `isFocused()`

`KeyboardSpine` keeps a `Set<RovingGroupHandle>`; a surface calls
`registerGroup(handle)` in its component body and
`onCleanup(() => unregisterGroup(handle))`. The root install's `active`
accessor is:

```ts
() => {
  for (const g of groups) if (g.isFocused()) return g;
  return null;
}
```

- **The tier-1 focus gate is preserved structurally, not by convention**: the
  RIG-2130 gate (`Bridge.tsx:465`) moves from a per-surface closure into the
  one root accessor, applied uniformly to every group any surface will ever
  publish. A surface *cannot* opt out of it — the class of bug the PR #472
  review caught (an unconditional **tier-1** claim) becomes unrepresentable
  *at tier 1*. This does **not** gate tier 3: a group-relative command
  registered in the shared registry (e.g. `board.openAssignedAgent`) stays
  reachable via the window-global tier with no focus evidence — a
  pre-existing hole this record neither worsens nor (by default) fixes.
  OQ-6 is the fork on whether to close it here or defer it; `unregister`
  (A5.3) is what bounds its blast radius to *while the surface is mounted*.
- **Multiple simultaneously-mounted groups need no arbitration protocol.**
  The left tree, the board, and the right sidebar are specced as sibling
  roving groups mounted at the same time (parent RD-1 rationale,
  `surfaces.md:124-127,170-173,297-300,361-365`). A single
  `setActiveGroup(handle)` "current group" signal (the obvious alternative)
  forces mount-order/last-writer semantics and still needs `isFocused()` to
  be correct — the set + scan derives the answer from the only ground truth
  there is, `document.activeElement`. Two invariants make the scan safe, and
  both belong in `spine.ts`'s doc + the DL row: **(a) one group per stop
  element** — the predicate is `s.el === document.activeElement` (element
  *identity*, not containment), so at most one group matches *provided a stop
  element belongs to at most one registered group* (specced surfaces have
  disjoint DOM — board cards, tree rows, sidebar tabs — and Set iteration is
  insertion-order deterministic, so even a pathological shared element
  resolves deterministically); **(b) a stale handle is dispatch-inert** — a
  handle left in the set after a missed `onCleanup` holds only detached
  elements, and a detached element is never `document.activeElement`, so a
  leak degrades to a dead per-keydown scan, never a misfire. Cost is
  O(groups × stops) per keydown with matching chords — single-digit groups,
  tens of stops: negligible.
- **Relation to the frozen `FocusZoneController` seam** (`zones.ts:91-119`):
  `registerRovingGroup(group: RovingGroup)` (zones.ts:102) is the frozen
  sketch of exactly this registration. This record realizes the
  *registration slice only*, taking the runtime `RovingGroupHandle` (which
  carries its `RovingGroup` identity at `roving.ts:29` plus the
  dispatch-required `handleCommand`/`isFocused`) rather than the bare
  `RovingGroup` descriptor — the handle is what dispatch needs, and the
  contracts-only file stays untouched. `activeZone` / `focusZone` /
  `moveWithinGroup` / pane focus remain contracts-only until zone-cycling
  lands (out of scope here, parent §70-71).

### A3 — `activeZone` accessor: derived from the focused group, not from `store.view()`

The root install passes `activeZone: () => spine.activeGroup()?.group.zone ??
null`. Not `store.view()`-derived: every routed center surface would map to
`"main"`, making the zone unconditionally active whenever the app is mounted
— which would let a `when:"main"` tier-2 entry fire while DOM focus sits in
the left sidebar, a zone-activation claim with no focus evidence. Not the
Bridge's shipped constant `() => "main"` either, for the same reason lifted
app-wide. Deriving from the focused group keeps tier-2 exactly as live as it
is today where it matters (board stop focused → group zone `"main"` active →
the frozen `Shift+Enter → comms.newline {when:"main"}` entry is a real
contender that tier-1 must beat — the load-bearing precedence test keeps its
teeth) and keeps it dormant without focus evidence. Observable delta in
production today: none — no `when`-scoped command is registered in prod
(`comms.*` is composer-local per RD-2, and the editable-target guard blocks
tier-2 for it regardless, parent §392-400). OQ-4 records the deferral of a
richer zone model.

### A4 — App-root install and `view.bridge` registration

- `createKeyboardSpine()` registers `view.bridge` as its first command —
  `{ id: cmd("view.bridge"), title: "Go to Bridge", keywords: ["board",
  "bridge", "kanban"], scope: "global", run: () => store.showBridge() }` —
  satisfying the frozen §403-404 (registration lives with the behavior; the
  spine is created inside `createAppStore` where `showBridge` is in scope).
- `App.tsx` (the router root layout, mounted once under both HashRouter and
  MemoryRouter) installs in its component body next to the existing
  `bindRouter` seam (App.tsx:39-42):
  `onCleanup(installKeymap(store.keyboard.registry,
  store.keyboard.activeGroup, store.keyboard.activeZone))`. `onCleanup`
  guarantees the test harness's repeated `render()`/dispose cycles never
  stack listeners (dispatch already returns the exact uninstaller,
  `dispatch.ts:59-60,132-133`).
- Only `view.bridge` is registered by this record. `view.agentWorkspace`,
  `view.settings`, `palette.open` are tabled chords (`keymap.ts:65-67`) whose
  commands stay unregistered — unregistered globals fall through harmlessly
  (`dispatch.ts:120-129`) — and belong to their owning lanes (OQ-5).

### A5 — Bridge migration: consumer, not installer

`Bridge.tsx:428-468` changes to:

1. `const registry = store.keyboard.registry` — delete the
   `props.registry ?? createCommandRegistry()` line and the `registry?:
   CommandRegistry` prop (`Bridge.tsx:162-169`). The prop existed to let
   tests seed a competing `comms.*` binding into "the registry the board
   installs against"; the board no longer installs, and tests reach the one
   real registry via `store.keyboard.registry` (T3). Clean cutover — no
   dead seam left behind.
2. Keep `createRovingGroup({...})` and `onCommand` exactly as shipped
   (`Bridge.tsx:429-435`, T2/T3/T4 of the parent are untouched); replace the
   `installKeymap(...)` + `onCleanup(uninstall)` block (`Bridge.tsx:463-468`)
   with `store.keyboard.registerGroup(rovingGroup)` +
   `onCleanup(() => store.keyboard.unregisterGroup(rovingGroup))`.
3. Keep the two `registry.register(board.*)` calls (`Bridge.tsx:436-449`) —
   now into the shared registry — and pair them with
   `onCleanup`-time `registry.unregister(id)` for both ids. This needs an
   **additive `unregister(id: CommandId): void` on the `CommandRegistry`
   contract** (`commands.ts:108-112`) and implementation
   (`registry.ts:18-37`). The **load-bearing** reason is not hygiene: the
   shared registry is app-lifetime, so without `unregister` a `board.*`
   command stays registered after the Bridge unmounts, and its `Shift+Enter`
   → `board.openAssignedAgent` row (no `when`, keymap.ts:98) is then
   dispatchable at **tier 3** from any non-editable target on *any* route
   (`/backlog`, `/settings` — board not mounted), running a stale closure
   over a disposed component's cursor and calling `store.openAgent` — a live,
   user-reachable misfire (dispatch.ts:120-129, and OQ-6). Secondary:
   without it every remount trips the dev duplicate-id warning
   (`registry.ts:23-27`), and the registry is the palette's specced source of
   truth (`commands.ts:104-106`). So `unregister` is currently the *only*
   containment of the tier-3 escape — which couples it to OQ-6's ruling.
   The contract file is frozen-by-D5 contracts-only; this is an additive
   method with no behavior change for existing callers — recorded as a DL row
   (see Global Constraints). **Contract-shape alternative (weigh at freeze):**
   have `register()` return a `() => void` disposer that removes *only if the
   id still resolves to this command* (remove-if-still-mine), instead of a
   raw `unregister(id)`. Same additive-compatibility for existing callers
   (they ignore the void return today), but it structurally eliminates the
   delete-by-id hazard — if surface A registers id X, B re-registers X
   (last-write-wins), then A's cleanup `unregister(X)` would delete B's live
   command. Not reachable today (route flips dispose the old Bridge before the
   new mounts; no two surfaces share an id), so `unregister(id)` is the
   recommendation; the disposer is the fallback if a same-id overlap ever
   becomes reachable.

No-overlap invariant: the Bridge's install is deleted in the same task (T3)
that lands after the root install exists (T2) — within one PR, so no
intermediate commit ships two listeners or zero. The interim (post-T2,
pre-T3) genuinely has **two** window listeners — dispatch adds one per
`installKeymap` and only ever calls `stopPropagation`, not
`stopImmediatePropagation`, so a sibling listener on the same target is not
suppressed (dispatch.ts:100-133). That interim is single-fire **not** because
"both accessors gate identically" — that is precisely the *unsafe* condition
— but because the two installs are **disjoint**: the App install reads the
spine registry (`{view.bridge}` only) over an **empty** group set (Bridge
publishes its group only in T3), while the Bridge install reads its own fresh
registry (`{board.*}`); no chord resolves to a command in *both*, and no
group is claimable by *both* accessors. The safety rule, stated correctly:
two concurrent installs are single-fire **iff they share no registered
command id AND at most one install's accessor can yield any given group**. It
follows that **T3's "publish the group to the spine" and "delete the Bridge
install" are one indivisible edit** — splitting them (publish while the local
install still exists) makes both accessors return the same focused group and
a focused-board `Enter`/`Shift+Enter` fires **twice**. Do not split them.

### A6 — Test strategy

- **The global-chord test proves the REAL wiring**: mount the full shell via
  the existing `mountApp` harness (`test-router.tsx:35-56` — production
  shape: store + `StoreContext` + `MemoryRouter root={App}` + shared
  `AppRoutes`) on a non-board route (`/backlog`), dispatch a window
  `keydown` `Ctrl+B` (and `Meta+B` under the mac platform stub), `flush()`,
  and assert `store.view() === "bridge"` and `.bridge` rendered. The test
  registers **nothing** — if App-root registration or install is missing,
  it reds. Companion: `Mod+B` pressed while a board stop is focused still
  switches (fall-through past the board's tier-1 decline, parent §381-384 —
  already unit-locked at `dispatch.test.ts:219`, now proven through the real
  mount).
- **The RIG-2130 board suite stays green, re-hosted where it must be**: the
  keyboard-driving `Bridge.test.tsx` cases (tier-1 precedence over
  `comms.newline`, focus-exclusivity, Enter/Space/arrows) currently depend
  on Bridge itself installing; they re-mount through `mountApp("/")` so the
  App-root spine is the installer, seeding the competing `comms.*` commands
  via `store.keyboard.registry.register(...)`. The assertions — board wins
  tier-1 while focused, board claims nothing while a toolbar button is
  focused — are unchanged; only the mount preamble moves. Non-keyboard
  Bridge tests keep their direct mounts.
- **Pin the tier-3 escape (OQ-6 / RD-4)**: extend the focus-exclusivity case
  to also press `Shift+Enter` with focus on a non-board, non-editable target
  while the board is mounted. Per the ratified **defer** ruling (RD-4) the
  test *documents* the current behavior — the assigned-agent action still
  fires from tier 3, so the hole is pinned, not silently present. (The
  rejected **close-here** shape — a tier-3 `isGroupRelative` skip — would
  instead assert the chord does **not** fire; that is the assertion the
  follow-up lane flips to if it closes the hole.) Either way the chord is no
  longer untested (today `Bridge.test.tsx:517-539` presses only
  Enter/Space/arrows).
- **Spine units** (`spine.test.ts`): register/unregister group round-trip;
  `activeGroup()` picks the focused group among several and `null` when
  none is focused; `activeZone()` mirrors it; `view.bridge` present in a
  fresh spine's registry; `unregister` removes and `all()` reflects it.
- Lifecycle: a mount→unmount→remount cycle leaves exactly one listener and
  no stale `board.*` commands (asserted via `registry.all()`).

## Plan

Order: T1 → T2 → T3. One PR (stacked commits per task); T2 must precede T3
within it so no commit has zero installers, and T3 deletes the Bridge install
so no commit has two.

### T1 — Keyboard spine: `createKeyboardSpine` + store exposure + registry `unregister`

New `apps/ui/src/keyboard/spine.ts`. Extend the `CommandRegistry` contract
and implementation with additive `unregister`. Hang the spine off the store.

Interfaces — consumes: `CommandRegistry`/`Command`/`CommandId`
(`keyboard/commands.ts`), `createCommandRegistry` (`keyboard/registry.ts`),
`RovingGroupHandle` (`keyboard/roving.ts`), `FocusZone` (`keyboard/zones.ts`),
`store.showBridge` (`store.ts:1883`). Produces:

```ts
// keyboard/spine.ts
export interface KeyboardSpine {
  readonly registry: CommandRegistry;
  registerGroup(handle: RovingGroupHandle): void;
  unregisterGroup(handle: RovingGroupHandle): void;
  /** The focused registered group, per handle.isFocused(); null when none. */
  activeGroup(): RovingGroupHandle | null;
  /** activeGroup()?.group.zone ?? null — the tier-2 accessor. */
  activeZone(): FocusZone | null;
}
export function createKeyboardSpine(deps: { showBridge: () => void }): KeyboardSpine;

// keyboard/commands.ts (additive)
export interface CommandRegistry { /* existing */ unregister(id: CommandId): void; }

// store.ts (AppStore, additive)
readonly keyboard: KeyboardSpine; // created in createAppStore after showBridge exists
```

`createKeyboardSpine` registers `view.bridge` (title "Go to Bridge", scope
`"global"`, `run: deps.showBridge`) before returning. Group storage is a
plain `Set<RovingGroupHandle>` (no signal needed — `activeGroup()` is called
per keydown, not tracked reactively).

Test cycle: `spine.test.ts` units (A6 bullet 3) + `registry.test.ts`
additions for `unregister` (removes; `get` → undefined; `all()` order;
unknown id is a no-op). `cd apps/ui && bun test --conditions browser`.

### T2 — App-root install + global-chord test

`App.tsx`: after the `bindRouter` call (App.tsx:39-42), add
`onCleanup(installKeymap(store.keyboard.registry, store.keyboard.activeGroup,
store.keyboard.activeZone));` (import `onCleanup` from solid-js,
`installKeymap` from `./keyboard/dispatch`). No other App change.

Interfaces — consumes: `installKeymap` (`keyboard/dispatch.ts:65-69`,
signature unchanged), `store.keyboard` (T1), `mountApp`/`flush`
(`test-router.tsx`). Produces: the single production install; a new
`App.test.tsx` (or `keyboard-e2e.test.tsx`) case set: (a) `Ctrl+B` from
`/backlog` flips to the board; (b) mac `Meta+B` equivalent under the platform
stub; (c) `Mod+B` while a board stop is focused still flips (fall-through);
(d) unmount removes the listener (dispatch after dispose is inert).

Test cycle: the new cases red before the App edit (command unregistered →
chord dead), green after. Full `bun test --conditions browser` stays green —
Bridge still installs its own duplicate listener until T3, single-fire in the
interim by the **disjointness** invariant (A5 no-overlap paragraph), not by
identical gating; T3 lands in the same PR before merge.

### T3 — Bridge migration + test re-hosting

`Bridge.tsx`: apply A5 items 1-3 (delete the `registry` prop + fresh-registry
default + `installKeymap`/`onCleanup(uninstall)` block; add
`registerGroup`/`unregisterGroup` + `unregister` of the two `board.*` ids on
cleanup). `Bridge.test.tsx`: re-host the keyboard-driving cases onto
`mountApp("/")` + `store.keyboard.registry` seeding per A6 bullet 2; drop the
`<Bridge registry={...}/>` mounts they used. Update the Bridge comment block
(`Bridge.tsx:423-427,450-462`) to describe the publish/retract model — the
focus-exclusivity rationale comment moves to the spine accessor where the
gate now lives.

Interfaces — consumes: `store.keyboard` (T1), `mountApp` (T2's pattern).
Produces: Bridge as pure consumer; zero `installKeymap` callers outside
`App.tsx` (grep-assertable); the re-hosted green board suite including
focus-exclusivity and tier-1-beats-`comms.newline`; the remount-hygiene test
(A6 bullet 4). Two executor traps to pre-empt — **(a)** `mountApp` takes
**no store options** (`test-router.tsx:35-56` hard-codes `STUB_COMMS_STATE` +
`testQueryClient`), so the re-hosted keyboard suite works today on the stub
store — but if a case needs a store fixture (e.g. the empty-board suite's
`initialIssues: []`, which stays on its direct non-keyboard mount), add a
one-arg options passthrough to `mountApp` rather than forking a second
harness. **(b)** `enterBoard`'s bare `container.querySelector('[tabindex="0"]')`
(`Bridge.test.tsx:306-308`) must scope to the board grid (`.bridge-grid ...`,
or `button.cx-card`) under the full shell — the shell container now includes
LeftSidebar/topbar DOM that precedes `.bridge-grid`, and any shell element
with an explicit `tabindex="0"` would otherwise steal the helper's landing.

Test cycle: full board suite + spine + App suites green; grep
`installKeymap` in `src/` yields `dispatch.ts` + `App.tsx` + tests only.

## Tasks

- [ ] T1 — `keyboard/spine.ts` (`createKeyboardSpine`: shared registry,
      group set, `activeGroup`/`activeZone`, `view.bridge` registered) +
      additive `CommandRegistry.unregister` (contract + impl) +
      `store.keyboard`; spine + registry units
- [ ] T2 — one `installKeymap` at the App root (`App.tsx`, beside the router
      seam) + real-wiring global-chord tests (`Ctrl/Mod+B` from a non-board
      surface, fall-through from a focused board, uninstall on dispose)
- [ ] T3 — Bridge off its local install: publish/retract the roving group,
      shared-registry `board.*` registration with cleanup-time `unregister`,
      delete the `registry` prop; re-host the keyboard-driving board tests
      on `mountApp`; focus-exclusivity + tier-1 precedence stay green
- [ ] (coordinator, PR time) DL rows appended to
      `docs/designs/product/DECISIONS.md` (spine home; group-publication
      model; `unregister` contract extension) + `Ledger-impact:` line

## Resolved decisions

Ratified by Matt (2026-08-22), folded from the pre-freeze Open Questions —
these are the frozen contract; the DL rows (Global Constraints) memorialize
the load-bearing three.

- **RD-1 — spine home: on the store (`store.keyboard`)** *(was OQ-1; A1)*.
  The shared registry + roving-group set hang off `AppStore` as one field,
  created in `createAppStore` beside `showBridge`. Zero new providers (both
  roots already provide one store via `StoreContext`); `view.bridge`
  registration lives next to its behavior; the standalone-window story keeps
  per-store isolation. The `KeyboardContext`-provider alternative was rejected
  for provider churn through both roots and every keyboard test mount with no
  capability gain.
- **RD-2 — group publication: a registered `Set<RovingGroupHandle>` scanned
  by `isFocused()`** *(was OQ-2; A2)*. The root `active` accessor scans the
  set for the group whose `isFocused()` is true, so the RIG-2130 focus gate is
  structural in one root accessor (no per-surface opt-out) *at tier 1*, and
  sibling groups need no mount-order arbitration. Safe by two invariants: one
  group per stop element (element-identity predicate); a stale handle is
  dispatch-inert (detached elements never hold focus). The `setActiveGroup`
  signal alternative was rejected — it re-opens the unconditional-claim bug
  class PR #472's review closed.
- **RD-3 — delete `Bridge.props.registry`; re-host the keyboard tests onto
  `mountApp`** *(was OQ-3; A5.1 / A6)*. The prop's sole purpose ("the registry
  the board installs against") dies with the board-local install. The
  keyboard-driving `Bridge.test.tsx` cases re-mount through `mountApp` and seed
  competing `comms.*` into `store.keyboard.registry`, exercising the true root
  wiring (higher fidelity); focus-exclusivity + tier-1-beats-`comms.newline`
  assertions are preserved verbatim. Non-keyboard Bridge tests keep their
  direct mounts.
- **RD-4 — the tier-3 escape: DEFER with eyes open** *(was OQ-6; option B)*.
  `dispatch.ts` stays frozen (unmodified). The tier-1 focus gate does not
  cover tier 3, so a registered `board.*` command (`Shift+Enter →
  board.openAssignedAgent`, keymap.ts:98, no `when`) is reachable from a
  non-editable target while the board is mounted-but-unfocused — a
  **pre-existing** hole on `main` this record neither worsens nor fixes. A2's
  claim reads "unrepresentable *at tier 1*"; the re-hosted focus-exclusivity
  test pins the hole with a `Shift+Enter` case (A6); `unregister` (A5.3) bounds
  the blast radius to *while the surface is mounted*; and a follow-up issue to
  the palette/zone lane (which owns cross-tier focus semantics) tracks the
  cross-tier gate. The DL row for the group-publication model reads
  "unrepresentable at tier 1," never the unqualified claim.

## Open Questions

- **OQ-4 (non-load-bearing) — richer `activeZone` later.** This record
  derives the active zone from the focused group (A3), which leaves tier-2
  dormant without focus evidence. When zone-cycling / `FocusZoneController`
  runtime lands, its controller becomes the authoritative `activeZone`
  source and the spine accessor swaps to it. Deferrable: no `when`-scoped
  command is registered in production today, so no observable behavior
  hangs on the richer model. Recommendation: defer to the zone-cycling
  lane's record.
- **OQ-5 (non-load-bearing) — the other tabled global chords
  (`view.agentWorkspace`, `view.settings`, `palette.open`,
  `keymap.ts:65-67`).** Unregistered globals fall through harmlessly by
  frozen design (`dispatch.ts:120-129`). Registering them is one-liner work
  *after* this record lands the spine, but each belongs to its owning
  surface/lane and none is in RIG-2456's scope. Recommendation: defer;
  file follow-up issues per lane when this record's impl merges.
