# Design: Scope-aware tier-3 command dispatch (RIG-2529)

Status: Draft
Owner lane: compass-ux (design) → compass-ui (execution)
Refs: RIG-2529. Closes the tier-3 focus-exclusivity hole RIG-2130 ratified as
DEFER (RD-4, was OQ-6 — `compass-keyboard-spine-app-root/design.md:451-462`).
Shared substrate for the RIG-2482 shortcuts overlay and the RIG-2483 command
palette (see Cross-record note).

## Problem / Intent

The keyboard dispatcher's tier 3 (window-global) runs any registered command
whose keymap entry has no `when`, regardless of the command's declared
`scope` and regardless of where focus is. `dispatch.ts:120-129` finds the
unscoped entry and unconditionally calls `command.run()` +
`event.preventDefault()`:

```ts
// Tier 3 — global. A window-global unscoped entry fires anywhere. An
// unregistered global command does not swallow the event.
const globalEntry = matching.find((entry) => entry.when === undefined);
if (globalEntry) {
  const command = registry.get(globalEntry.commandId);
  if (command) {
    command.run();
    event.preventDefault();
  }
}
```

The concrete leak, walked through: the board registers
`board.openAssignedAgent` with `scope: "main"` (`Bridge.tsx:433-439`), and the
keymap binds `Shift+Enter → board.openAssignedAgent` with no `when`
(`keymap.ts:98`). With the board **mounted but unfocused** — say a topbar
segmented button holds focus (a non-board, non-editable element) — pressing
`Shift+Enter`:

1. Tier 1 skips: no roving group is focused, so `active()` is `null`
   (`dispatch.ts:93-94`; the spine's `activeGroup()` scans `isFocused()`,
   `spine.ts:81-84`).
2. Tier 2 skips: `activeZone()` derives from the active group
   (`spine.ts:95-97`), so it is `null` too (`dispatch.ts:107-108`).
3. Tier 3 fires: the entry is unscoped, the command is registered →
   `board.openAssignedAgent` runs (`Bridge.tsx:393-401` — every branch
   returns `true`). The board's cursor persists while the board is
   unfocused, so the run acts on the cursor card (`store.openAgent` /
   `store.selectIssue`); cursorless, the branch is a no-op (`:396`
   `if (!action) return true`) but `preventDefault()` still fires — a
   silent swallow of the button's native activation either way.

A `scope: "main"` command fires from anywhere in the app. RIG-2130's tier-1
focus gate is structurally sound *at tier 1* — RD-4 ratified exactly this
qualification:

> **RD-4 — the tier-3 escape: DEFER with eyes open** *(was OQ-6; option B)*.
> `dispatch.ts` stays frozen (unmodified). The tier-1 focus gate does not
> cover tier 3, so a registered `board.*` command (`Shift+Enter →
> board.openAssignedAgent`, keymap.ts:98, no `when`) is reachable from a
> non-editable target while the board is mounted-but-unfocused — a
> **pre-existing** hole on `main` this record neither worsens nor fixes. […]
> a follow-up issue to the palette/zone lane (which owns cross-tier focus
> semantics) tracks the cross-tier gate.
> — `compass-keyboard-spine-app-root/design.md:451-462`

The hole is pinned, not silent: `Bridge.test.tsx:537-572` presses
`Shift+Enter` from a focused toolbar button and asserts the leak fires
(`:568-571`: `defaultPrevented === true`, `store.view() === "agent"`), with a
comment (`:559-567`) naming the follow-up lane that flips the assertion. This
record is that lane. **This record closes RD-4/OQ-6.**

Why now: the discoverability net (RIG-2482 keyboard-shortcuts overlay,
RIG-2483 command palette) needs the eight group-relative `list.*` commands
registered in the shared `CommandRegistry` so they earn overlay/palette rows.
Under today's tier 3, registering them would open eight NEW copies of the same
leak — every `list.*` chord (`Enter`, `Space`, arrows, `Home`/`End`,
`keymap.ts:79-86`) is unscoped, so a registered `list.openOrSelect` would fire
window-globally from any non-editable target. The scope gate is what makes
*register = safe*: after it, a `scope: "main"` registration is inert unless
the main zone actually holds focus.

## Global Constraints

- SolidJS ^1.9.13 (v1 on main); Bun test toolchain, `cd apps/ui && bun test
  --conditions browser`; single root `/biome.json` (2.5.4, relative imports
  sorted alphabetically).
- Styling (if any surface work were touched — none is here): `--cx-*` tokens
  only; the mono face is `--cx-font-ui: var(--rigel-mono)` (tokens.css:195 —
  there is NO `--cx-font-mono`).
- Command ids follow `noun.verbCamel`.
- **ONE window keydown path**: `App.tsx:48-54` installs the single production
  listener over `store.keyboard`; this record changes the dispatcher's tier-3
  resolution, never adds a listener.
- Every new global-chord contract needs a **production-wiring test**: mount
  the real App via `mountApp`, press the chord, observe the store — not a
  unit stub alone.
- **This modifies a frozen contract.** `dispatch.ts` was frozen by RIG-2130
  RD-2 and explicitly left unmodified by RIG-2456 ("`apps/ui/src/keyboard/
  dispatch.ts` is **not modified**", `compass-keyboard-spine-app-root/
  design.md:38-42`) and by RD-4 ("`dispatch.ts` stays frozen"). Amending a
  frozen contract requires its own record + PR + review — this record is
  that amendment, and the flipped RD-4 pin test (T2) is the visible,
  reviewable proof that the contract changed deliberately.
- Ledger: new decisions are record-local (Decisions below); the driver
  assembles DL rows into `DECISIONS.md` at PR-assembly time (DL ids are global
  across all sections; the true max on main is DL-226, `DECISIONS.md:154`, so
  this record's rows take DL-227+). Do not edit `DECISIONS.md` from this record.

## Approach

One sentence: tier 3 runs a matched global entry's command **only if the
command's declared scope permits it from the current focus context** —
`command.scope === "global" || command.scope === zone` — turning the
`Command.scope` field (`commands.ts:18,40`, present since D5 but never read by
the dispatcher) into the load-bearing tier-3 gate.

### The gate — exact before/after of `dispatch.ts:120-129`

Before (current `main`, cf5d3e69):

```ts
    // Tier 3 — global. A window-global unscoped entry fires anywhere. An
    // unregistered global command does not swallow the event.
    const globalEntry = matching.find((entry) => entry.when === undefined);
    if (globalEntry) {
      const command = registry.get(globalEntry.commandId);
      if (command) {
        command.run();
        event.preventDefault();
      }
    }
```

After:

```ts
    // Tier 3 — global, scope-gated (RIG-2529). A window-global unscoped
    // entry fires anywhere its COMMAND's scope allows: a `scope:'global'`
    // command runs from any target; a zone-scoped command runs only while
    // its zone is active. An unregistered global command does not swallow
    // the event, and neither does a scoped command whose zone is inactive —
    // both fall out without preventDefault, so native activation survives.
    const globalEntry = matching.find((entry) => entry.when === undefined);
    if (globalEntry) {
      const command = registry.get(globalEntry.commandId);
      if (command && (command.scope === "global" || command.scope === zone)) {
        command.run();
        event.preventDefault();
      }
    }
```

`zone` is already in scope: tier 2 computes `const zone = activeZone()` at
handler top level (`dispatch.ts:107`), so tier 3 reuses it — no second
accessor call, no allocation. The gate is one added conjunct on the existing
`if (command)` check.

The `installKeymap` signature (`dispatch.ts:65-69`) is unchanged; `activeZone`
already defaults to `() => null`, under which the gate degrades exactly right:
only `scope: "global"` commands pass — the conservative direction.

### (a) Unregistered global entries: unchanged

The `matching.find(e => e.when === undefined)` selection and the
no-swallow-on-unregistered behavior (`dispatch.test.ts:233-240`) are
untouched: an unregistered command still falls out without `preventDefault`.
The gate adds a second way to fall out — registered but out-of-scope — with
the same no-swallow property (the toolbar button keeps its native `Enter`).

### (b) `scope:'main'` with NO active zone: does not run

`activeZone()` is `null` whenever no registered roving group holds DOM focus
(`spine.ts:95-97` derives it from `activeGroup()`). Then
`command.scope === zone` is `"main" === null` → false → the command does not
run. This is precisely the leak scenario (toolbar button focused ⇒ no group ⇒
no zone), so the correct outcome falls out of the predicate with no special
case: **null zone means only global-scoped commands dispatch at tier 3.**

### (c) Tiers 1 and 2: untouched

- Tier 1 (`dispatch.ts:90-103`): group-relative routing via
  `group.handleCommand` — no registry lookup, no scope involved. Unchanged.
- Tier 2 (`dispatch.ts:105-118`): already zone-gated by construction (`entry
  .when === zone`). Unchanged. (See Open Questions for why tier 2 needs no
  symmetric command-scope check.)
- Precedence is protected structurally by tier order, not by the scenario a
  reviewer might first read. While the board is focused, tier 1 claims
  `Enter` as the group-relative `list.openOrSelect` (`dispatch.ts:93-98`; the
  board never declines it, `Bridge.tsx:385-392`). Only on a decline — or under
  a future decoupled zone controller — does `Enter` reach tier 2, where the
  `when:'main'` `comms.send` (`keymap.ts:110`) outranks the unscoped tier-3
  `list.openOrSelect` (`keymap.ts:83`). Registering `list.*` adds tier-3
  candidates only and cannot shadow the comms rows in either path.
- Fall-through composition: if a focused group *declines* a group-relative
  chord (returns `false`, `dispatch.ts:98`), the chord now reaches a
  scope-gated tier 3 where the group's zone IS active — so a registered
  `scope:'main'` `list.*` command may run. For the board this is benign by
  construction: its registered `run` mirrors `onCommand`
  (`Bridge.tsx:438,445`), the same function tier 1 just consulted — a decline
  means "this id does nothing here," and calling it again does nothing again.
  Recorded as part of D3's contract: a group-relative command's `run` MUST be
  the same behavior the group handler exposes, never a divergent second path.

### (d) The `list.*` registration story

With the gate in place, Bridge registers the eight group-relative Lists-block
ids — enumerated from `keymap.ts:79-86`:

1. `list.movePrev` (ArrowUp)
2. `list.moveNext` (ArrowDown)
3. `list.moveLeft` (ArrowLeft)
4. `list.moveRight` (ArrowRight)
5. `list.openOrSelect` (Enter)
6. `list.expandOrToggle` (Space)
7. `list.moveFirst` (Home)
8. `list.moveLast` (End)

Each registers beside the existing `board.*` registrations
(`Bridge.tsx:433-446`), `scope: "main"`, `run: () => onCommand(id)` (the same
mirror pattern `board.*` uses, `Bridge.tsx:438`), and is retracted in the same
`onCleanup` block (`Bridge.tsx:447-450` grows to unregister all ten ids).
Registration is what earns the overlay/palette rows (the discoverability net
reads the registry); the gate is what makes it safe (an unfocused board's
`list.*` registrations are tier-3 inert). Titles/keywords follow the existing
`board.*` style (`"Open assigned agent"` / lowercase keyword arrays,
`Bridge.tsx:434-436`).

## Plan

### T1 — the scope gate in `dispatch.ts` + unit cases

**Interfaces:**

- `installKeymap(registry: CommandRegistry, active: () => RovingGroupHandle |
  null, activeZone: () => FocusZone | null = () => null): () => void` —
  signature unchanged (`dispatch.ts:65-69`).
- The only production diff is the tier-3 block, `dispatch.ts:120-129`, exactly
  as shown in Approach (one added conjunct + comment update).

**Red-first test cycle** (in `dispatch.test.ts`, slotting into the
tier-resolution suite at `:150-240`, reusing `makeCommand`/`stubGroup`/
`keydown` from `:14-63`; `makeCommand` currently hardcodes `scope: "global"`
(`:21`) — add a `scope` parameter defaulting to `"global"`):

1. *(green before AND after — regression guard)* a `scope:'global'` command
   fires from tier 3 with no group and no zone (`Ctrl+B → view.bridge`,
   mirrors `:219-231`).
2. *(RED first)* a `scope:'main'` command bound unscoped in the keymap does
   NOT run and does NOT `preventDefault` when `activeZone` yields `null` —
   the leak closure. Use `Shift+Enter → board.openAssignedAgent` registered
   `scope:'main'`, `installKeymap(registry, () => null)` (default zone stub).
3. *(RED first)* same registration, `activeZone: () => "main"` → the command
   RUNS and `preventDefault`s — scoped commands still work from their zone.
4. *(RED first — the decline→tier-3 composition)* a declining group
   (`stubGroup(() => false)`, group zone `"main"`) + `activeZone: () =>
   "main"` + a `scope:'main'` `list.openOrSelect` registered → the command
   RUNS and `preventDefault`s. This pins the one genuinely new reachable path
   the gate creates (a group-relative command re-invoked at tier 3 after its
   group declined) and matches production, where the zone is derived from the
   very group that declined (`spine.ts:95-97`). Contrast case 3, which reaches
   the same run through an `activeZone` stub with no declining group.
5. *(green before and after)* unregistered global entry still does not
   swallow (`:233-240` stays green untouched).

### T2 — flip the RD-4 pin in `Bridge.test.tsx`

**Interfaces:** `Bridge.test.tsx:537-572`, the focus-exclusivity test. The
first half (`:546-557`, toolbar button keeps Enter/Space/arrows) is unchanged.
The RD-4 pin (`:559-571`) flips:

- Comment: replace the DEFER narrative (`:559-567`) with the closure
  narrative (RIG-2529: tier 3 is scope-gated; `board.openAssignedAgent` is
  `scope:'main'` and no zone is active from the toolbar button, so the chord
  falls out of tier 3 un-consumed).
- Assertions: `press({ key: "Enter", shiftKey: true })` →
  `expect(escapeEvent.defaultPrevented).toBe(false)` (native activation
  survives) and `expect(store.view()).toBe(<view before the press>)` (the
  assigned-agent escape did NOT fire). This IS the production-wiring test for
  the changed global-chord contract: real App via `mountApp("/")`, real
  chord, real store observation.
- Non-regression companion in the same suite: focus a board stop, press
  `Shift+Enter`, assert the assigned-agent action still fires (tier 1 claims
  it — `Bridge.tsx:393-401`; today this path is covered by the tier-1 suite,
  keep or add one explicit case so the flip demonstrably narrows tier 3
  without breaking the focused-board behavior).

**Red-first:** the flipped assertions fail on unmodified `dispatch.ts` (the
current `:568-571` proves it — they assert the opposite), and pass with T1.
Land T1+T2 in one commit ordering: red test → gate → green.

### T3 — register the eight `list.*` commands in Bridge

**Interfaces:**

- `Bridge.tsx:433-450`: after the two `board.*` registrations, register the
  eight ids from Approach (d), each
  `{ id, title, keywords, scope: "main", run: () => onCommand(id as
  CommandId) }`; extend the `onCleanup` (`:447-450`) to unregister all ten.
  Prefer a `const LIST_COMMANDS: ReadonlyArray<{id: string; title: string;
  keywords: string[]}>` table + a loop over ten literal register/unregister
  pairs (Biome-clean, one place to read the inventory).
- `Bridge.test.tsx:582-618` (mount→unmount→remount lifecycle): widen
  `boardCommandIds()` (`:584-588`) to also collect `list.*` ids and assert
  all ten present after mount, `[]` after unmount, all ten after remount —
  the no-leak proof that a stale `scope:'main'` registration can never
  outlive its surface.

**Red-first:** the widened lifecycle assertions fail before the
registrations exist. Depends on T1 (the gate is what makes these
registrations safe); enforce commit order T1 → T3.

## Tasks

- [ ] T1 — scope gate in `dispatch.ts:120-129`; unit cases in
      `dispatch.test.ts` (global fires anywhere; `scope:'main'` blocked with
      null zone, red-first; `scope:'main'` fires with `"main"` zone,
      red-first; decline→tier-3 with the zone active runs, red-first;
      unregistered no-swallow stays green).
- [ ] T2 — flip the RD-4 pin (`Bridge.test.tsx:559-571`) to assert
      `defaultPrevented === false` + view unchanged; keep the focused-board
      `Shift+Enter` non-regression case; production wiring via `mountApp`.
- [ ] T3 — register the eight `list.*` ids in `Bridge.tsx:433-450` beside
      `board.*` (`scope:'main'`, `run` mirrors `onCommand`), retract in
      `onCleanup`; widen the lifecycle no-leak test to all ten ids.

## Decisions

- **D1 — the scope-gate predicate: `command.scope === "global" ||
  command.scope === zone`, evaluated at tier 3 on the registered command.**
  Chosen because it makes the existing, already-authored `Command.scope`
  field (`commands.ts:18,40`) load-bearing with a one-conjunct diff, reuses
  tier 2's already-computed `zone` (`dispatch.ts:107`), and degrades
  conservatively when `activeZone` is the `() => null` default. Rejected
  alternative: a tier-3 `isGroupRelative(entry.commandId)` skip (the
  "close-here shape" RIG-2456 named and rejected, `compass-keyboard-spine-
  app-root/design.md:293-296`) — it special-cases two id prefixes
  (`dispatch.ts:45-47`) instead of honoring the declared scope, would NOT
  protect a future zone-scoped non-group command (e.g. a `scope:'right'`
  sidebar command bound unscoped), and leaves `Command.scope` decorative.
- **D2 — close RD-4/OQ-6: an explicit reversal of a ratified DEFER.**
  RIG-2456 RD-4 chose defer-with-eyes-open and routed the cross-tier gate to
  "the palette/zone lane (which owns cross-tier focus semantics)"
  (`design.md:459-461`); this record is that lane arriving. The reversal is
  deliberate and Matt-ratified (2026-08-22): the discoverability net turns
  the hole from one pinned leak into eight new ones the moment `list.*`
  registers, so the defer's cost basis flipped. The pinned test flips with
  it — the hole was never silent, and its closure is not either. Rejected
  alternative: keep deferring and have the overlay/palette render
  group-relative rows WITHOUT registering the commands (a display-only
  workaround) — rejected by Matt's R2 ruling because it forks the inventory
  (palette dispatch path diverges from the keymap/overlay display path) and
  leaves the RD-4 hole open besides.
- **D3 — `list.*` registration-beside-behavior.** The eight `list.*` ids
  register in `Bridge.tsx` beside the existing `board.*` block
  (`:433-450`), `scope:'main'`, `run` mirroring `onCommand`, retracted
  `onCleanup` — never in some central manifest. A command is registered
  where its behavior lives (the spine did the same with `view.bridge` beside
  `showBridge`, `spine.ts:60-77`), so registration lifetime equals behavior
  lifetime and the unmount retraction is structural (the lifecycle test
  proves it). Contract rider from Approach (c): a group-relative command's
  `run` MUST be the same behavior the group handler exposes — the decline/
  fall-through path re-invokes it harmlessly; this rider codifies the existing
  `board.*` mirror pattern (`Bridge.tsx:438,445` already set `run` to
  `onCommand`), it does not add a new constraint. Rejected alternative: register
  `list.*` app-wide at spine creation — orphans the commands from any
  behavior when no list surface is mounted and breaks the
  retraction-on-unmount invariant DL-225 exists for.
- **D4 — the commands-as-inventory rule (shared with RIG-2482/RIG-2483).**
  *Register = actionable-from-anywhere; a dispatching surface (the palette's
  action mode) reads the registry; a display surface (the overlay) reads the
  keymap; a command is registered beside its behavior, never purely for
  discoverability — and tier 3 only runs a command whose scope is global or
  matches the active zone, so registering a `scope:'main'` command is safe.*
  This gate is what reconciles the first clause with zone-scoped commands:
  "actionable from anywhere" is bounded by declared scope, so the registry
  can be the palette's single honest inventory without any surface leaking
  global dispatch. Rejected alternative: a registry `discoverable: true`
  flag for display-only rows — a second registration semantic that lets an
  entry lie about being actionable, exactly the fork D2's rejected
  workaround creates.
- **D5 — the safety proof's load-bearing invariant, named as a contract.**
  The gate is safe *today* because `activeZone` is derived from `activeGroup`
  (`spine.ts:95-97`): `zone === "main"` implies a main-zone roving group holds
  focus, which implies tier 1 already saw any group-relative chord — so the
  only way a `scope:'main'` command reaches the scope-gated tier 3 with the
  zone live is a tier-1 decline, and D3's mirror rider makes that harmless.
  The codebase declares this derivation temporary, though: `dispatch.ts:61-63`
  notes `activeZone` "defaults to `() => null` (no live zone controller in
  this wave)", and `keymap.ts:70-73` already reserves `zone.focus*`/`zone.cycle`
  rows for one. **Contract:** any lane that introduces an independent zone
  controller — one that can make `activeZone()` non-null while no roving group
  is focused (e.g. focus on a plain main-zone button) — inherits the
  obligation to re-establish tier-3 safety for zone-scoped commands, because
  the decline chain no longer holds: a registered `scope:'main'`
  `list.*`/`board.*` command would again become dispatchable from a non-group
  main-zone target (an in-zone sibling of the RD-4 hole this record closes).
  This obligation is routed to that future zone lane exactly as RD-4 routed
  the cross-tier gate here, and T1 mirrors the note in the `activeZone` doc
  comment so it is unmissable at the impl site.
- **D5 corollary — one roving group per zone.** The gate keys on `zone`, not
  on which group is focused, so its per-surface safety additionally assumes a
  single roving group per zone (today: one main-zone board group). A future
  second main-zone surface reusing the generic `list.*` ids would register the
  same ids in the shared app-lifetime registry and could re-invoke their tier-3
  `run` while a *different* main-zone surface is focused — the same-id
  cross-surface overlap DL-225 already flags as the trigger for its
  `register()`-returns-disposer fallback. Not reachable today (single main-zone
  group; route flips dispose the old surface before the new mounts; the
  RIG-2482/2483 lanes only read the registry, add no groups), but a second
  main-zone group inherits DL-225's overlap obligation.

## Cross-record note

- RIG-2482 (keyboard-shortcuts overlay) and RIG-2483 (command palette) both
  cite this record as **external frozen substrate**, the same way they cite
  the merged RIG-2456 spine — they consume the D4 rule, they do not redefine
  it.
- The two siblings depend on this record differently:
  - **RIG-2482 (overlay) depends code-level:** its `list.*` completeness is
    load-bearing on the gate + the eight registrations, so the overlay impl PR
    **stacks on this record's impl MERGE** (a design freeze is not a stackable
    base when the dependency is running code — the overlay's `list.*`-backed
    rows are unsafe until the merged gate exists).
  - **RIG-2483 (palette) depends design-level only:** its seed commands are
    `scope: "global"` view commands, so the palette is correct on `main`
    without this record — it cites the D4 rule so the two records never
    diverge, and it inherits the `list.*` rows for free once this record ships,
    but its impl PR is **based on `main`, not stacked on this merge**.
- This record's own impl PR is independent, based on `main` — files disjoint
  from the two sibling records' PRs.
- Reminder for the impl wave (owned by the sibling records, noted here for
  the driver): overlay + palette impls still share the four App-root wiring
  touchpoints (spine.ts deps+signature, store.ts:1896 spine creation,
  App.tsx imports+`<Show>`, spine.test.ts:37-43) — resolved by set-union;
  whoever merges second rebases. This record touches none of those four.

## Open Questions

- **Should tier 2 gain a symmetric command-scope check?** Likely NO, and
  none is specified. Tier 2 is already zone-gated by construction — it only
  consults entries whose `when` equals the live `activeZone()`
  (`dispatch.ts:105-118`), so a scoped entry can never fire outside its
  zone. A mismatch between an entry's `when` and its command's `scope`
  (e.g. `when:'main'` entry naming a `scope:'right'` command) would be an
  authoring bug in the keymap table, not a focus leak — the command still
  only fires while `main` is active. Adding the check would double-encode
  the same fact and mask the authoring bug instead of surfacing it in
  review. Revisit only if keymap entries ever become user-authored.
