# Design: Leader/Mnemonic Keyboard Chords, Focus-Context-Gated (RIG-2484)

Status: Draft

Parent: RIG-1661 (keyboard-first product). Builds on the SHIPPED discoverability
net — all three impl PRs are merged to `main`: RIG-2482 `?` overlay (PR #526),
RIG-2483 Cmd+K palette + point-of-use chips (PR #529), RIG-2529 tier-3 scope
gate (PR #519). The net is live CODE now, not just frozen design records, so
every chord this record adds is already findable (the `?` overlay is a
render-time join over the live keymap, `apps/ui/src/keyboard/shortcuts-model.ts:44-78`)
and has a discoverable fallback (the palette). That was the precondition; it is
met at the code level.

## Problem / Intent

Compass's keymap is 100% modifier chords: every row of `DEFAULT_KEYMAP`
(`apps/ui/src/keyboard/keymap.ts:100-155`) is either `Mod+*`/`F6`, the bare `?`
overlay chord (`keymap.ts:109`), or a group-relative navigation key (arrows,
Enter, Space, Home/End) — e.g. `{ chord: "Mod+B", commandId: cmd("view.bridge") }`
(`keymap.ts:102`). The dispatcher resolves exactly ONE `KeyboardEvent` to ONE
chord string (`eventToChord`, `apps/ui/src/keyboard/dispatch.ts:36-53`) against
that table; the install handler is stateless across events — no two-key pending
state anywhere in `installKeymap` (`dispatch.ts:110-168`). Modifier space burns
fast (`Mod+\` vs `Mod+Shift+\`, `keymap.ts:116-117`, is already a strain;
`Mod+1..3` are spent on zones, `keymap.ts:112-114`), and Linear-class keyboard
products don't spend it: they use bare letters and "G then key" leader sequences
that read like little sentences.

**Matt has RATIFIED the fork (2026-08-23): adopt Linear's leader/mnemonic model,
focus-context-gated.** Heavy bare-letter + leader coverage on the board and other
non-text surfaces; the comms/channel surfaces naturally get fewer *active* bare
chords because a composer is usually focused there — an emergent property of
focus gating, NOT a per-route switch. This record designs HOW: the
leader-sequence runtime, the sequence authoring shape, the collision guard, the
bare-letter allocation, and coexistence with today's `Mod+*` bindings. The
whether is decided and not re-argued here.

## Global Constraints

- **SolidJS ^1.9.13 (v1)** — `apps/ui/package.json:25` `"solid-js": "^1.9.13"`.
  Vite + TS strict. v1-legal forward idioms only (no second convention).
- **Biome 2.5.4 pinned** — root `package.json:15` `"@biomejs/biome": "2.5.4"` via
  catalog (a workspace-ROOT dev dependency; `apps/ui` declares no Biome dep of its
  own).
- **Command ids** `noun.verbCamel`; board-scoped ids `board.*`.
- **Chords authored with the `Mod` token** — `keymap.ts:24-26`: *"Chords are
  authored with `Mod`, which resolves to `Cmd` on macOS and `Ctrl` everywhere
  else. Authoring with `Mod` (never a literal `Ctrl`/`Cmd`) is the convention
  that keeps the keymap portable."* Leader sequences are a new authoring shape
  this record defines (§A2); segments inside a sequence follow the same token
  rules.
- **One keydown path** — `App.tsx:54-60` installs the single window listener
  over the spine and wraps the uninstaller in `onCleanup`; the leader runtime
  lives INSIDE that listener's handler. NEVER a second window keydown listener.
- **Compose with the three-tier model** — the dispatcher's ratified resolution
  (tier 1 active roving group → tier 2 `when`-scoped zone → tier 3 window-global,
  `dispatch.ts:124-167`) and the documented scoped-over-global precedence
  (`keymap.ts:86-89`: *"When the same chord is bound both with and without a
  `when`, the scoped entry takes precedence while its zone is active … the
  consumer applies that precedence rather than double-firing."*) are unchanged.
  A completed leader sequence resolves through the SAME tiers as a single chord.
- **a11y — the shipped aria helpers are the seam** — the discoverability net
  shipped `resolveChordAria` (`keymap.ts:47-48`), `shortcutFor` (`keymap.ts:57-63`),
  and `shortcutForAria` (`keymap.ts:70-76`), and wired the latter two into three
  live `aria-keyshortcuts` writers (`App.tsx:88`, `LeftSidebar.tsx:453,469,487,500`,
  and the literal `"Space"` at `Bridge.tsx:525`). `aria-keyshortcuts` values must
  stay WAI-ARIA-valid; a press-then-press sequence has NO representation in the
  attribute's grammar (§A5 rules on this, and it is a real hazard because
  `shortcutForAria` today would emit a sequence string verbatim).
- **Tests** run `cd apps/ui && bun test --conditions browser <file>`; dispatcher
  tests dispatch real `KeyboardEvent`s on the window/focused element per
  `apps/ui/src/keyboard/dispatch.test.ts:9-12`.
- **@kobalte/core ^0.13.13** (`apps/ui/package.json:15`) where the shipped
  palette uses it; this record itself adds no Kobalte surface.

## Approach

### A1 — The collision guard is ALREADY BUILT: focus gating via the editable-target guard

The load-bearing observation, stated first because everything else leans on it:
the dispatcher already suppresses every modifier-less chord while focus is in a
text field, and bare-letter/leader chords are modifier-less, so they inherit the
guard with zero new code:

> ```ts
> // Editable-target guard: a modifier-less chord (arrows, Enter, Space,
> // Home/End, and bare Shift combos) never fires while focus is in a text
> // field — the composer keeps its local keys. Mod/Ctrl/Alt chords are
> // global and are NOT guarded.
> const hasCommandModifier = event.metaKey || event.ctrlKey || event.altKey;
> if (!hasCommandModifier && isEditableTarget(event.target)) return;
> ```
>
> — `apps/ui/src/keyboard/dispatch.ts:117-122`

with `isEditableTarget` covering `HTMLInputElement`, `HTMLTextAreaElement`, and
`isContentEditable` (`dispatch.ts:66-72`). Wave 1 EXTENDS `isEditableTarget` (a
small refinement to the shipped helper) to also cover a focused native `<select>`
(one exists at `SettingsView.tsx:166`, class `settings-select`) and elements
inside a `combobox`/`listbox`/`menu` role: those widgets own their letter keys (a
`<select>`'s type-ahead), and because arming unconditionally `preventDefault`s the
key (§A3 step 4), a bare `g` on a focused `<select>` would otherwise steal its
typeahead. The extension is strictly safe for existing modifier-less chords —
arrows/Enter in a `<select>` already fall through to native behavior — and is the
right boundary for "focus is somewhere with its own key handling".

**Decision (ratified by Matt's ruling): focus gating IS the surface-awareness
mechanism. There is no per-route switch.** This is exactly what Linear, Slack,
and Zulip all converge on: single-letter/leader chords live in "navigation mode"
(no text field focused) and yield entirely to text entry — Linear's help says to
press Escape to clear active inputs before using shortcuts; Slack's single-letter
message actions "do not work while you are typing in the message input field";
Zulip's design principle is "with the compose box closed, there is no need to use
the Ctrl key all the time". "Lighter on comms surfaces" — Matt's instinct — is
EMERGENT: a channel page usually has the composer focused, so bare chords are
mostly dormant there, while the board (no text inputs) gets the full set. Nobody
should ever add a redundant per-route disable flag; this record makes that a
ledgered decision.

Compass already has the Slack-style path INTO navigation mode: `F6 → zone.cycle`
(`keymap.ts:115`) rotates focus between zones, the direct parallel of Slack's F6
rotor out of the composer.

### A2 — Sequence authoring: space-separated segments in `KeymapEntry.chord`

A leader sequence is authored as a single `chord` string whose segments are
separated by one space: `{ chord: "G B", commandId: cmd("view.bridge") }`. No
new `KeymapEntry` field.

- `KeymapEntry` (`keymap.ts:91-95`) keeps its exact shape — `chord`, `commandId`,
  `when?` — so every existing consumer (the dispatcher's `DEFAULT_KEYMAP.filter`
  at `dispatch.ts:112-114`, the overlay's keymap join `buildShortcutGroups` at
  `shortcuts-model.ts:53-70`, `shortcutFor`/`shortcutForAria` at `keymap.ts:57-76`)
  continues to typecheck unchanged.
- Each segment is itself a full Mod-token chord, so `resolveChord`
  (`keymap.ts:37-38`, a pure `replaceAll(MOD, …)`) already resolves a sequence
  string correctly — `"Mod+G B"` would become `"Cmd+G B"` — though in practice
  every segment of a leader sequence is modifier-less by construction (a modified
  segment would bypass the editable guard, §A1, and is banned by the authoring
  rule below).
- Space is unambiguous as a separator because the literal Space key is already
  normalized to the multi-char token `"Space"` (`dispatch.ts:45`: `if (key === " ")
  key = "Space";`), so a raw `" "` can never appear as a key name inside a chord
  string.
- New pure helpers in `keymap.ts` (exact signatures in T1) split a sequence into
  segments and derive the leader-prefix set FROM the table, so the dispatcher
  never hard-codes `G`: adding a second leader later (e.g. an action leader) is a
  data change, not a runtime change.

**Authoring rules** (enforced by a T1 unit test over `DEFAULT_KEYMAP`, the same
way the table is already the single source of truth, `keymap.ts:1-10`): sequences
are exactly two segments; every segment of a sequence is modifier-less (guard
inheritance, §A1); a sequence's first segment must not also be bound as a
complete single chord (the leader key is reserved — see A3 fall-through for why
this keeps the runtime simple).

### A3 — The leader runtime: pending state inside the ONE keydown handler

The net-new control flow. Today the handler resolves one event to one chord and
is stateless across events (`dispatch.ts:110-168`). The leader runtime adds a
small closure state to `installKeymap`:

```ts
type PendingLeader = { leader: string; timer: number } | null;
```

Per keydown, ordered. The one structural subtlety: a bare leader key (`g`)
produces the chord `"G"`, which has NO single-chord row in `DEFAULT_KEYMAP`, so
today's `matching.length === 0` early return (`dispatch.ts:115`) would fire
before anything could arm. The runtime therefore resolves arming/completion
around that early return, not after it.

1. **Normalize** via the existing `eventToChord` (`dispatch.ts:36-53`),
   unchanged.
2. **Editable guard FIRST, before any leader logic.** A modifier-less key in a
   text field must type, never arm or complete: the existing guard
   (`dispatch.ts:121-122`) moves ahead of the pending-leader branch, so `g` in
   the composer inserts "g" and the runtime stays disarmed. Its `isEditableTarget`
   is extended (§A1, T3) to also cover a focused `<select>` or
   `combobox`/`listbox`/`menu` widget, so a bare letter never steals native
   typeahead by arming. (Today the guard runs AFTER the `matching.length === 0`
   early return at `dispatch.ts:115`; it must now run before both leader arming and
   the empty-matching return for modifier-less keys. Mod/Ctrl/Alt chords remain
   unguarded and also DISARM any pending leader — a deliberate command interrupts
   the sequence.)
3. **Completion.** If a leader is pending: pure-modifier keydowns (`event.key` of
   `Shift`/`Control`/`Alt`/`Meta`) are ignored — they neither complete nor disarm
   (holding Shift mid-sequence is human). `Escape` disarms and is consumed.
   Otherwise clear the timer, disarm, and resolve the two-segment chord
   `"<leader> <key>"` — where `<key>` is the FULL normalized chord from
   `eventToChord`, so a completion key pressed while Shift is still HELD resolves
   as `"Shift+B"` (a `"G Shift+B"` sequence, which matches no wave-1 row and falls
   through) — through the EXISTING three tiers (`dispatch.ts:124-167`) exactly as a
   single matched chord, with the documented scoped-over-global precedence
   (`keymap.ts:86-89`) intact. If the sequence matches no row, **fall through and
   process the key as a plain single chord in the same keydown**, RE-ENTERING the
   arming step first: a re-pressed leader (`g g`) RE-ARMS (Vim parity for a
   repeated prefix), and any other unmatched key resolves as its own single chord
   (so `g` then `ArrowDown` still moves the list cursor). This fall-through
   subsumes the Mod-chord-mid-sequence case — a Mod chord disarms via the guard
   exemption, then resolves normally.
4. **Arming.** No leader pending: if the normalized chord is in the leader-prefix
   set derived from the table (§A2) — and, per the A2 authoring rule, such a chord
   has no single-chord binding of its own — and the target is not an
   editable/interactive widget (§A1's extended guard already returned for those),
   arm: `pending = { leader: chord, timer: setTimeout(disarm, LEADER_TIMEOUT_MS) }`,
   `event.preventDefault()`, `event.stopPropagation()`, return. `event.repeat`
   keydowns never arm (holding `g` must not queue arms).
5. **Single-chord path.** Otherwise, today's resolution runs byte-for-byte
   unchanged (the `matching.length === 0` return at `dispatch.ts:115` and the
   three tiers at `:124-167`).

Disarm-on-timeout uses one `setTimeout` (default proposal **1000 ms**, OQ1); the
uninstaller (`dispatch.ts:171`) also clears any live timer so tests and HMR never
leak one. The state is per-install closure state — no module-level global,
matching the existing install/uninstall discipline (`App.tsx:54-60` wraps the
uninstaller in `onCleanup`).

No visual pending-leader affordance ships in wave 1: Linear and Zulip show
nothing, and a hint surface would be new chrome this record doesn't need (OQ3
offers a cheap follow-up).

### A4 — Bare-letter and sequence allocation (wave 1)

The allocation principle, mirroring Linear: **`G <letter>` = "go to" a
destination; bare letters = act on the current selection in a navigation
surface**. Wave 1 is deliberately small, and — critically — **all four target
commands are already registered by the shipped spine**, so wave 1 adds ONLY
keymap rows (no spine or store change; contrast the pre-net draft, which wrongly
believed these were unregistered):

| Chord | Command | Registration (verified) | Existing modifier row? |
| --- | --- | --- | --- |
| `G B` | `view.bridge` | `spine.ts:79-86` → `showBridge` (`store.ts:1923`) | `Mod+B` (`keymap.ts:102`) |
| `G L` | `view.backlog` | `spine.ts:111-118` → `showBacklog` (`store.ts:1927`) | none — sequence is its FIRST keyboard chord |
| `G D` | `view.done` | `spine.ts:119-126` → `showDone` (`store.ts:1931`) | none — sequence is its FIRST keyboard chord |
| `G S` | `view.settings` | `spine.ts:103-110` → `showSettings` (`store.ts:1935`) | `Mod+,` (`keymap.ts:104`), live + e2e-tested (`keyboard-e2e.test.tsx:106-118`) |

`createKeyboardSpine` already takes all six deps
(`showBridge`/`toggleShortcuts`/`showBacklog`/`showDone`/`showSettings`/`togglePalette`,
`spine.ts:70-77`) and `store.ts:1973-1980` already wires them, so the four `view.*`
commands resolve today — they simply have no leader chord yet. Adding the four
`G *` rows to `DEFAULT_KEYMAP` is the whole allocation. The `view.backlog`/`view.done`
rows are the ones that force the A5 aria-hardening task (their FIRST keyboard
chord is a sequence, which `shortcutForAria` must not emit).

The fifth view row, `view.agentWorkspace` (`Mod+Shift+A`, `keymap.ts:103`), gets
no `G A`: the shipped spine registers no `view.agentWorkspace` command
(`spine.ts:79-126` registers only the six above), so a `G A` chord would resolve
to nothing. The registered-commands-only rule keeps every allocated chord live.

**Bare letters on the board (tier 1)**: none in wave 1. The board's
group-relative chords (arrows/Enter/Space/Home/End/`Shift+Enter`,
`keymap.ts:121-140`) already cover movement and the board's two actions
(`board.openAssignedAgent`, `board.openCardCrossLink`). Linear's bare letters
(`a` assign, `s` status, `p` priority) act on issue FIELDS Compass's board does
not yet edit; allocating letters for actions that don't exist would mint dead
rows. The runtime + authoring shape this record ships makes each future bare
letter a one-row keymap addition claimed in tier 1 like any other group-relative
chord (`dispatch.ts:124-137`) — the allocation grows with the board's verbs,
reserved letters listed in OQ4.

### A5 — Display and `aria-keyshortcuts` for sequences

The WAI-ARIA `aria-keyshortcuts` grammar is a space-separated list where each
token is a SIMULTANEOUS-key chord and multiple tokens mean ALTERNATIVES — so
`"G B"` in the attribute reads as "press G or press B", NOT "press G then B".
Sequential press has no representation in the attribute. This is a live hazard:
the shipped `shortcutForAria` (`keymap.ts:70-76`) returns the FIRST
`DEFAULT_KEYMAP` row's resolved chord, and `LeftSidebar.tsx:469` writes it to
`aria-keyshortcuts` for `view.backlog` (today undefined → omitted, per the comment
at `LeftSidebar.tsx:435`). The instant `G L → view.backlog` lands,
`shortcutForAria("view.backlog")` would return `"G L"` and the sidebar would emit
invalid ARIA.

The two shipped helpers pull in OPPOSITE directions here, so they harden
ASYMMETRICALLY (this is the correction to the pre-net draft, which skipped BOTH
and so silently deleted point-of-use discoverability for the very commands wave 1
adds):

- **aria — skip (mandatory).** `shortcutForAria` (`keymap.ts:70-76`) SKIPS
  sequence rows (`chordSegments(entry.chord).length > 1`) when scanning. A command
  whose only binding is a sequence returns `undefined` → its writer omits the
  attribute, exactly as the existing "no keymap row yet" path
  (`LeftSidebar.tsx:435`). A dual-bound command (`view.bridge`: `Mod+B`,
  `keymap.ts:102`; `view.settings`: `Mod+,`, `keymap.ts:104`) returns the modifier
  chord — valid grammar. There is NO valid ARIA representation of a sequence, so
  `undefined` is the only correct value; the skip makes this independent of row
  order.
- **display — format, never skip.** `shortcutFor` (`keymap.ts:57-63`) returns
  `formatChordForDisplay` of the first matching row and does NOT skip sequences. A
  sequence-only command returns `"G then L"`, so its point-of-use surfaces stay
  discoverable: the sidebar button `title` (`LeftSidebar.tsx:471-472,488` renders
  `Backlog (${chord})`) reads `"Backlog (G then L)"`, and the palette chip
  (`Palette.tsx:118` → `ShortcutChip`) shows the sequence. A dual-bound command
  still shows its modifier chord because the T2 rows sit AFTER the modifier row
  (first match) and `formatChordForDisplay` of a single chord is just
  `resolveChord`.
- **overlay — format.** The overlay join (`shortcuts-model.ts:57`) renders every
  row via `resolveChord`; it applies `formatChordForDisplay` so a sequence row
  shows `"G then B"` instead of the misleading `"G B"`.

One pure formatter (`formatChordForDisplay`, T1) owns all three display surfaces;
the "then" makes press-order explicit where a bare `"G B"` chip would read as one
chord. `ShortcutChip` (`ShortcutChip.tsx:17`) splits its `chord` prop on `"+"` and
renders each part as a `<kbd>`; a formatted `"G then B"` has no `"+"`, so it
renders as a single `<kbd>` — acceptable for wave 1 (a segment-aware chip with a
separate "then" span is an optional follow-up, not blocking).

The one literal `aria-keyshortcuts` writer that names a raw key (`Bridge.tsx:525`,
the cursor card's `"Space"`) is single-chord and untouched.

### A6 — Coexistence with today's `Mod+*` bindings: additive, no removals

Wave 1 is purely additive: every existing `Mod+*` row keeps working, and the
sequences are SECOND bindings to the same command ids (the dispatcher already
tolerates one command with many rows — resolution is per-chord via
`DEFAULT_KEYMAP.filter`, `dispatch.ts:112-114`, and the overlay lists both rows
truthfully via its keymap join, `shortcuts-model.ts:53-70`). This matches Slack,
where modifier chords stay global regardless of focus while single-letter
shortcuts are focus-gated. Deprecating any `Mod+*` binding is a separate later
call (OQ5) with usage in hand; nothing in this design depends on it, and modifier
chords retain one real advantage the sequences never have: they fire even while a
text field is focused (§A1's guard exempts them via `hasCommandModifier`,
`dispatch.ts:121`), so `Mod+B` from the composer keeps working where `G B`
correctly does not.

## Alternatives considered

1. **Modifier-only status quo** (rejected — and the fork is ratified against it).
   Modifier space is nearly spent already (`Mod+\` vs `Mod+Shift+\`,
   `keymap.ts:116-117`; `Mod+1..3` burned on zones, `keymap.ts:112-114`); every
   new destination or board verb would mint an ever-less-memorable
   `Mod+Shift+Alt+*`. Linear demonstrates the mnemonic model scales and reads
   ("G then B" = go to Bridge).
2. **Full adoption with a per-route disable switch** (rejected as redundant). A
   route-level "bare chords off on channel pages" flag re-implements — worse —
   what the focus guard already does per-keystroke (`dispatch.ts:121-122`): none
   of Linear, Slack, or Zulip gates on the page; all three gate on whether a text
   field is focused, and Slack's modifier chords stay global even while typing. A
   per-route switch would also break the board-in-channel case (a board can be
   focused while a composer exists on the same page — the three-tier model's
   whole point is that precedence is focused-surface, not page).
3. **A separate leader-mode dispatcher (second keydown listener / mode object)**
   (rejected). The one-keydown-path invariant is structural (`App.tsx:54-60`
   installs exactly one listener over the spine) and the runtime is a few lines
   of closure state inside `installKeymap` — a parallel dispatcher would need its
   own guard, its own tier logic, and a coordination protocol with the existing
   one.
4. **New `KeymapEntry.sequence?: string[]` field instead of the space-separated
   `chord` string** (rejected). It forks the authoring surface into two shapes
   every consumer must handle (`filter` on `chord` at `dispatch.ts:112-114`, the
   overlay join at `shortcuts-model.ts:53-70`, `resolveChord`,
   `shortcutFor`/`shortcutForAria`), for zero expressive gain — the space
   separator is collision-free because the Space key is already the token
   `"Space"` (`dispatch.ts:45`).
5. **Minting parallel display/aria helpers** (brand-new `chordForAria`/
   `chordForDisplay` scanners **replacing** the shipped `shortcutFor`/
   `shortcutForAria`) (rejected — this was the pre-net draft's error).
   `shortcutFor`/`shortcutForAria` already exist (`keymap.ts:57-76`) and are the
   single derivation for every chip and every `aria-keyshortcuts` writer
   (`App.tsx`, `LeftSidebar.tsx`, `Palette.tsx`); a parallel scanner would leave
   the shipped ones to emit the ARIA bug. The correct move is to harden them
   asymmetrically (§A5): `shortcutForAria` skips sequence rows, `shortcutFor`
   formats them. `formatChordForDisplay` IS added — but as a pure display
   formatter both the shipped helper and the overlay call, not as a parallel
   scanner over the table.
6. **Hybrid via focus (CHOSEN)** — Linear's model, gated by the existing
   editable-target guard (`dispatch.ts:117-122`), heavy on the board and other
   non-text surfaces, naturally light on comms because the composer is usually
   focused. This is Matt's ruling, and the research shows it is the unanimous
   industry mechanism.

## Plan

Task ordering keeps EVERY intermediate commit green (`rule://red-green-testing`).
The four `DEFAULT_KEYMAP` sequence rows and the `shortcutFor`/`shortcutForAria`
hardening are coupled — adding a sequence-only row without the aria skip regresses
`keymap.test.ts:51` and emits invalid ARIA at `LeftSidebar.tsx:469` — so they land
in the SAME task (T2). The runtime (T3) lands after the rows exist, so arming
resolves against a populated leader-prefix set. T2 and T3 ship in one stack, so no
release ever advertises a chord that does not yet fire.

### T1 — Sequence grammar + pure helpers in `keymap.ts` (no table or helper change)

Define the sequence grammar and the pure helpers; touch no production caller and
no existing behavior.

- Add to `apps/ui/src/keyboard/keymap.ts`:
  - `export function chordSegments(chord: string): string[]` — split on a single
    space; a plain chord yields `[chord]`.
  - `export function leaderPrefixes(keymap, platform): ReadonlySet<string>` — the
    resolved first segments of every multi-segment row.
  - `export function formatChordForDisplay(chord, platform): string` — a single
    chord → `resolveChord` output (`"Cmd+B"`); a sequence → segments joined with
    `" then "` (`"G then B"`).
- Extend the `KeymapEntry` doc block (`keymap.ts:78-90`) with the sequence grammar
  and the three authoring rules (exactly two segments; modifier-less segments; a
  leader prefix never doubles as a complete chord).
- `Interfaces:`

  ```ts
  // keymap.ts (new exports; KeymapEntry unchanged)
  export function chordSegments(chord: string): string[];
  export function leaderPrefixes(
    keymap: readonly KeymapEntry[],
    platform: Platform,
  ): ReadonlySet<string>;
  export function formatChordForDisplay(chord: string, platform: Platform): string;
  ```

- Test cycle (red → green), extend `keymap.test.ts` over a FIXTURE keymap (the
  real table has no sequence rows until T2): `chordSegments("G B") → ["G","B"]`,
  `chordSegments("Mod+B") → ["Mod+B"]`; `leaderPrefixes(fixture, "other")` contains
  `"G"` and nothing else; `formatChordForDisplay("G B","mac") → "G then B"`,
  `formatChordForDisplay("Mod+B","mac") → "Cmd+B"`. GREEN — pure additions, no
  production caller yet.

### T2 — Sequence rows + display/aria hardening (landed together)

Add the rows and harden the two shipped scanners in one task so every commit is
green and no sequence ever reaches `aria-keyshortcuts`.

- Add the four A4 rows to `DEFAULT_KEYMAP` in a new `// Go-to sequences (RIG-2484)`
  block, each placed AFTER any existing modifier row for the same command: `G B →
  view.bridge`, `G L → view.backlog`, `G D → view.done`, `G S → view.settings`
  (all unscoped/global — view navigation like `Mod+B`, `keymap.ts:102-104`).
- **aria (skip):** harden `shortcutForAria` (`keymap.ts:70-76`) to SKIP sequence
  rows (`chordSegments(entry.chord).length > 1`) when scanning — a sequence-only
  command returns `undefined` (writer omits the attribute); a dual-bound command
  returns its modifier chord (§A5).
- **display (format, never skip):** harden `shortcutFor` (`keymap.ts:57-63`) to
  return `formatChordForDisplay` of the first matching row and NOT skip sequences,
  so `view.backlog`/`view.done` return `"G then L"`/`"G then D"` and keep their
  point-of-use chip + title (§A5).
- Apply `formatChordForDisplay` at the overlay join (`shortcuts-model.ts:57`) so a
  sequence row renders `"G then B"`, not the misleading `"G B"`.
- `Interfaces:` `shortcutFor`/`shortcutForAria` signatures unchanged; behavior per
  §A5 (aria skips sequences, display formats them).
- Test cycle (red → green): update `keymap.test.ts:33`
  (`shortcutFor("view.backlog")` now `"G then L"`, was `undefined`);
  `keymap.test.ts:51` (`shortcutForAria("view.backlog")` stays `undefined` — the
  skip's whole point); `shortcutFor("view.bridge","other") → "Ctrl+B"` and
  `shortcutForAria("view.bridge","other") → "Control+B"` unchanged (modifier row
  wins); a `DEFAULT_KEYMAP` authoring-invariant test (every sequence has two
  modifier-less segments; no leader prefix is also a complete single chord); a
  component test (extend `Palette.test.tsx:238-251`, which already tests the
  `LeftSidebar` buttons) that Backlog/Done carry NO `aria-keyshortcuts` but a
  `title` of `"Backlog (G then L)"`/`"Done (G then D)"`, while Bridge keeps
  `aria-keyshortcuts="Control+B"`. GREEN — rows + hardening atomic.
- **Overlay integration test** (`ShortcutsOverlay.test.tsx:66`): once the `G B`
  row lands, a `"bridge"` filter query matches BOTH the `Mod+B` row and the new
  `G B` row — the overlay model iterates `DEFAULT_KEYMAP` per-entry with no
  per-command dedup and matches on title (`shortcuts-model.ts:80-93`) — so
  update `expect(rows.length).toBe(1)` → `toBe(2)` and assert the second row
  renders the formatted `"G then B"` (usefully defending the §A5 overlay-format
  rule). Without this the T2 commit goes red, breaking the green-at-every-commit
  invariant.

### T3 — Leader runtime in the dispatcher

The pending-leader state machine of §A3 inside `installKeymap`
(`dispatch.ts:103-172`), single listener preserved; the rows from T2 make arming
live.

- Extend the arm-time guard: `isEditableTarget` (`dispatch.ts:66-72`) also covers
  `HTMLSelectElement` and elements inside a `combobox`/`listbox`/`menu` role, so a
  bare letter never arms (or is `preventDefault`ed) while a native `<select>`
  (`SettingsView.tsx:166`) or ARIA widget with its own key handling has focus
  (§A1).
- Move the editable guard (`dispatch.ts:117-122`) ahead of both leader logic and
  the `matching.length === 0` early return (`dispatch.ts:115`) for modifier-less
  keys; keep its semantics byte-identical for single chords.
- Arming (leader-set hit, `!event.repeat`, preventDefault + stopPropagation),
  completion (two-segment resolve through tiers 1→2→3 unchanged,
  `dispatch.ts:124-167`), dead-sequence fall-through that RE-ENTERS arming (a
  re-pressed leader re-arms; other keys resolve as their own single chord), Escape
  disarm, pure-modifier-key transparency, Mod-chord disarm, timeout disarm
  (`LEADER_TIMEOUT_MS = 1000`, OQ1), timer cleared by the uninstaller
  (`dispatch.ts:171`).
- `Interfaces:` (public signature unchanged — the runtime is internal closure
  state; the timeout test uses Bun fake timers, verified to fire `setTimeout`
  under the pinned Bun 1.4.0, so NO clock-injection seam is added)

  ```ts
  // dispatch.ts
  export const LEADER_TIMEOUT_MS: number; // 1000, OQ1
  export function installKeymap(
    registry: CommandRegistry,
    active: () => RovingGroupHandle | null,
    activeZone?: () => FocusZone | null,
  ): () => void; // unchanged
  ```

- Test cycle (red → green), extend `dispatch.test.ts` (real KeyboardEvents on
  window/focused elements, `dispatch.test.ts:9-12`):
  - `g` then `b` runs `G B`; the arming `g` is `defaultPrevented`.
  - **Timeout red-green:** `g`, `jest.useFakeTimers()` + `advanceTimersByTime`
    past `LEADER_TIMEOUT_MS`, then `b` — command NOT run (Bun 1.4.0 fake timers
    fire `setTimeout` — verified).
  - **Editable-guard non-regression:** focus an `<input>`, `g` then `b` — nothing
    armed/run/prevented; existing single-chord guard tests stay green.
  - **`<select>` non-regression:** focus a `<select>`, press `g` — not armed, not
    `defaultPrevented` (native typeahead intact).
  - **ARIA-widget non-regression:** focus an element inside a `role="listbox"`/
    `combobox` (the shipped Kobalte palette Search is a listbox), press `g` —
    not armed, not `defaultPrevented` (the role half of the extended §A1 guard,
    symmetric with the `<select>` case above).
  - **Dead-sequence fall-through:** `g` then `ArrowDown` with an active group →
    group receives `list.moveNext`.
  - **Re-arm:** `g g` then `b` runs `view.bridge` (the second `g` re-arms).
  - **Arm-then-refocus:** `g` on the board, focus the composer, `b` within the
    window — no completion, `b` not prevented (guarded), and a post-timeout `b`
    types normally.
  - `Escape` disarms; a `Shift` keydown alone mid-sequence does not disarm (`g`,
    Shift tapped + released, then `b` completes); `Mod+B` mid-sequence disarms AND
    runs `view.bridge`; held `g` (`repeat: true`) does not re-arm.
  - Uninstall while pending leaves no timer firing.
  - **Production wiring (e2e):** extend `keyboard-e2e.test.tsx` (its `Mod+,` →
    settings press-test spans `:106-118`): mount the real App, `g` then `s` →
    settings route; `g` then `l` → backlog. No registration — the spine already
    registers all four (`spine.ts:79-126`).

### T4 — Docs + ledger follow-through (driver-owned ledger flip)

- Extend the `dispatch.ts` module header (`dispatch.ts:1-10`) with the leader
  runtime's contract (arming/completion/disarm and the guard-first ordering).
- The DECISIONS.md rows for this record (DL-248..DL-252) ride this PR; the driver
  flips the ledger in the same PR per house process. No hand-maintained shortcut
  list — the overlay's keymap join (`shortcuts-model.ts:53-70`) picks up sequence
  rows automatically once T2 lands.

## Tasks

- [ ] T1 — sequence grammar helpers (`chordSegments`, `leaderPrefixes`,
      `formatChordForDisplay`) + `KeymapEntry` doc block, fixture-based tests; NO
      `DEFAULT_KEYMAP` or shipped-helper change
- [ ] T2 — add the four `G *` rows AND harden `shortcutForAria` (skip sequences)
      + `shortcutFor`/overlay (format sequences, never skip display) together;
      update `keymap.test.ts` + `LeftSidebar` aria-omission test + overlay
      row-count test (`ShortcutsOverlay.test.tsx:66`)
- [ ] T3 — pending-leader runtime in `installKeymap` (guard-first ordering,
      arm/complete/disarm/fall-through, `<select>`/combobox arm-guard), red-green
      timeout (Bun fake timers) + guard non-regression + `g s`/`g l` e2e press
- [ ] T4 — dispatcher doc header; ledger delta rides the PR (driver flips)

## Open Questions

1. **Leader key = `G`, timeout = 1000 ms.** (Load-bearing.) `G` is the
   Linear/Zulip "go-to" convention and collides with nothing in `DEFAULT_KEYMAP`
   (`keymap.ts:100-155` has no bare-letter rows). 1000 ms is a guess between
   "typing rhythm" and "read the next key off the overlay"; Vim's default
   `timeoutlen` is 1000 ms. Recommend: `G`, 1000 ms, constant exported so it's a
   one-line retune.
2. **Timeout at all vs pending-until-Escape?** (Load-bearing — it shapes the
   runtime: the `PendingLeader` timer field, the timeout-disarm arm, the
   uninstaller's timer clear, and the red-green timeout test exist only if a
   timeout does; a "no timeout" ruling deletes that machinery.) Ships a default
   regardless — keep the timeout: it is safer against a forgotten armed state
   swallowing a later `b`, and Linear does not appear to time out aggressively.
   Recommend: keep the timeout.
3. **Visual pending-leader hint?** (Non-load-bearing.) Wave 1 ships none
   (Linear/Zulip parity). A later nicety: a small `G …` chip in the topbar while
   armed. Recommend: defer.
4. **Bare-letter reservation list for future board verbs.** (Non-load-bearing
   now; load-bearing before anyone allocates one.) Proposal to reserve, mirroring
   Linear: `a` assign, `s` status, `p` priority, `c` create, `i` assign-to-me,
   `/` search-focus, and `j`/`k` as list aliases. Nothing ships in wave 1 (§A4);
   ratifying the reservation now prevents first-come squatting later.
5. **Migration policy for existing `Mod+*` bindings.** (Non-load-bearing — this
   record ships keep-both (§A6) and nothing in the design depends on the choice; a
   later deprecation is its own record.) Options: (a) keep both indefinitely —
   recommended (modifier chords keep the fire-while-typing property sequences
   can't have); (b) deprecate overlapping `Mod+*` (`Mod+B`, `Mod+,`) after the
   sequences prove out. Recommend (a).
