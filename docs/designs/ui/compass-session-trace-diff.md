# Compass session-trace DiffBlock: real line diff (RIG-1350)

Status: Active

Tracking: RIG-1350 (two advisory findings from the PR #847 review-of-record,
Matt-ruled deferred to this follow-up). UI-only; walking-skeleton renderer
scope. Parent contract: the frozen compass-0.8 threading/renderer record
scopes the Compass UI as a no-daemon walking skeleton
(`docs/designs/product/compass-0.8-threading-and-session-renderer/design.md:246,806`)
— nothing here touches transport, `compass.v1`, or the daemon.

## Problem

`DiffBlock` in `apps/ui/src/components/SessionTrace.tsx` renders a
tool call's inline file diff by **set membership**, not a sequence diff.
Verbatim, `SessionTrace.tsx:12-22`:

```ts
const rows = () => {
 const olds = oldLines();
 const news = newLines();
 const dels = olds
  .filter((line) => !news.includes(line))
  .map((text) => ({ kind: "del" as const, text }));
 const adds = news
  .filter((line) => !olds.includes(line))
  .map((text) => ({ kind: "add" as const, text }));
 return [...dels, ...adds];
};
```

with the inputs split naively (`SessionTrace.tsx:9-11`):

```ts
const oldLines = () =>
 props.diff.oldText === null ? [] : props.diff.oldText.split("\n");
const newLines = () => props.diff.newText.split("\n");
```

Two correctness defects follow:

- **P1 — reorders and duplicate-line edits render nothing.** A pure reorder
  `"a\nb"` → `"b\na"`: every line is a member of both arrays, so both
  `filter`s yield `[]` and the diff block shows only the path despite a real
  change. A duplicate-line edit (`"a"` → `"a\na"`) collapses identically — a
  line present in both texts is never emitted even when its count changed.
  All dels also render before all adds (`[...dels, ...adds]`,
  `SessionTrace.tsx:21`), so even detected changes lose their positions.
- **P2 — empty / trailing-newline text yields a phantom blank row.**
  `"".split("\n")` is `[""]` and `"a\n".split("\n")` is `["a", ""]`; the
  trailing `""` survives the membership filter whenever the other side lacks
  one and renders as a spurious blank `+`/`-` row.

The input type is fixed. `apps/ui/src/session-events.ts:22-27`:

```ts
/** A file change carried by a tool update. `oldText` is null for a new file. */
export interface FileDiff {
 path: string;
 oldText: string | null;
 newText: string;
}
```

`FileDiff` mirrors a proto shape on the RIG-1342 go/proto lane; the UI only
consumes it. The diff is computed client-side from the whole-file old/new
pair, for display only.

## Approach

Extract the row derivation into a pure, unit-tested module —
`apps/ui/src/line-diff.ts` — that runs a **line-based Myers diff
from the `diff` (jsdiff) library** over our own line tokens, and have
`DiffBlock` call it from the same derived accessor it uses today. Three
decisions, all three forks from the brief:

**(a) Algorithm: jsdiff's `diffArrays` over `splitLines` tokens.** Matt ruled
use a diff library. `apps/ui/package.json:7-11` carries three
runtime deps (`@compass/client`, `@tauri-apps/api`, `solid-js`); this adds
one, `diff` (jsdiff) — verified resolvable from the workspace registry at
`9.0.0` (`bun pm view diff version`), so the offline-leaning workspace (sealed
monorepo, LAN registry) is not a blocker. jsdiff ships a battle-tested Myers
O(ND) implementation: **linear space** (no `m×n` table), but O((m+n)·D) *time*
in the edit distance D. The hand-rolled table's load-bearing risk was
**memory** — `FileDiff` holds whole-file texts (`session-events.ts:22-27`) with
no size bound, and an O(m·n)-table LCS on a large-churn edit to a generated file
or lockfile would allocate ~100M cells synchronously in a Solid derived accessor
on the render path. Myers deletes that memory cliff. It does **not** delete a
*time* cliff on the same input: a fully disjoint churn has D ≈ m+n, so the diff
degenerates to quadratic time — measured at ~720ms for 2000×2000 disjoint lines
(jsdiff 9.0.0), which scales ~quadratically to a multi-second freeze on a large
generated-file rewrite, still computed synchronously in `rows()` on the render
path. So a guard is still needed, but a cheaper and library-native one: pass
`diffArrays` a `maxEditLength` budget; when D exceeds it, jsdiff returns
`undefined` (verified 9.0.0), and `line-diff.ts` falls back to a coarse
all-del-then-all-add rendering of the two whole files — the same coarse output
the hand-rolled size guard produced, via the adopted engine's own bound rather
than a bespoke `if`. This bounds worst-case render latency deterministically.
The exact fallback *rendering* (coarse rows vs. a single "diff too large" row)
and the budget value are a user-visible call — Open Question 2 below.

The one subtlety, verified empirically (jsdiff 9.0.0, probed on every fixture
below): jsdiff's own `diffLines` keeps the trailing `\n` *inside* each token
(`"a\n"` ≠ `"a"`), which makes a pure reorder `"a\nb"` → `"b\na"` render four
rows (`-a -b +b +a`) instead of the minimal two, and inverts the intended
trailing-newline handling. So `line-diff.ts` does **not** call `diffLines`; it
tokenizes with our own newline-*terminated* `splitLines` (below) and runs
`diffArrays(oldLines, newLines)`. That reproduces the intended line model
exactly — minimal reorder (`-a +a`), correct dup-line, and the P2 fixes — while
the Myers engine and linear space come from the library. (jsdiff's `ArrayDiff`
overrides `removeEmpty` to the identity, so empty-string line tokens survive
`diffArrays` — load-bearing for the EOF-blank-line fixture below.)

**(b) Row model: changed-lines-only, positionally interleaved. No context
rows.** `diffArrays` returns change parts in document order (a removed run
before the added run that replaces it — standard hunk order) and marks
unchanged parts, which we drop. This keeps the existing DOM contract
byte-for-byte — `.diff-line[data-kind="add"|"del"]` with
`.diff-gutter`/`.diff-body` (`SessionTrace.tsx:26-33`) and the existing CSS
(`app.css:1095-1137`, which styles exactly `data-kind="add"` and
`data-kind="del"`) — so no new `data-kind` value, no CSS change, and the
new-file test's exact add-row count stays valid. Context rows are a natural
later extension (jsdiff also returns the unchanged parts; emitting them is
additive), but they are a UX call — recorded as the single Open Question below,
recommended "not now".

**(c) Dup-line correctness bar: sequence-diff positional correctness is
sufficient.** `diffArrays` operates on sequences, not sets, so duplicate-line
and reorder edits fall out correctly for free: `"a"` → `"a\na"` emits one add;
`"a\nb"` → `"b\na"` emits one del + one add (verified). Nothing beyond the
line-level sequence diff — no move detection, no intra-line highlighting — is
warranted for a read-only observation trace. Folded, not an open question.

**Line splitting (fixes P2):** a dedicated `splitLines` treats text as
newline-*terminated*: `""` → `[]`, `"a\n"` → `["a"]`, `"a"` → `["a"]`,
`"a\nb"` → `["a", "b"]` (i.e. `split("\n")` with one trailing `""`
dropped). This kills the phantom blank row while keeping the new-file
render test's invariant — `"const answer = 42;\nexport default answer;"`
has no trailing newline, so `splitLines` yields the same 2 lines the test
counts via `newText.split("\n").length`
(`SessionTrace.test.tsx:149,182`).

Accepted blind spot: because a lone trailing newline is dropped,
`diffRows("a", "a\n")` is `[]` — a tool edit whose *only* effect is adding
a missing final newline (a common formatter fix) renders as an empty diff
block (path shown, zero rows), a narrow recurrence of the P1 symptom class.
Git models this distinction with its `\ No newline at end of file` marker.
For a read-only observation trace the miss is tolerable and it buys the P2
phantom-row fix; it is a deliberate limitation, not an oversight — noted here
and on the fixture so a maintainer reads it as a choice. (A blank line at EOF
is *not* affected: `"a\n\n"` → `["a", ""]` preserves the empty line, so
`"a\n"` vs `"a\n\n"` correctly diffs as one added blank line.)

**Kept-green contract:** the two existing DiffBlock render tests —
`"tool item with a diff renders add/del lines and the path"`
(`SessionTrace.test.tsx:106-142`: edit with both texts newline-terminated →
at least one add and one del render, path shown) and `"tool item with a
new-file diff (null oldText) renders only add rows"`
(`SessionTrace.test.tsx:148-183`: new file, `oldText: null`, no trailing
newline → zero del rows, exactly `newText.split("\n").length` = 2 add
rows). Both hold under `diffArrays` + `splitLines` by construction: the edit
changes exactly one line (one del + one add), and a new file is all adds
with the same line count.

**SolidJS reactivity:** `rows` stays a derived accessor —
`const rows = () => diffRows(props.diff.oldText, props.diff.newText);` —
read inside `<For each={rows()}>` exactly as today
(`SessionTrace.tsx:26`), so reactivity to `props.diff` is unchanged.

## Alternatives considered

- **Hand-rolled O(m·n)-table LCS, no npm dep.** The record's prior approach: a
  ~40-line prefix/suffix-trimmed LCS table under our own tests, zero new
  dependency. Declined per Matt's ruling to use a diff library. The table's
  load-bearing weakness was **memory** — an `m×n` allocation (~100M cells on a
  large-churn edit to a generated file) built synchronously on the Solid render
  path — which a size-guard `if` only papered over by dropping to coarse
  del-run/add-run granularity above a cell budget. jsdiff's Myers is linear
  *space*, so the memory cliff disappears — but its O((m+n)·D) *time* keeps a
  freeze cliff on the same disjoint-churn input, so a guard is still needed
  (jsdiff's own `maxEditLength`, Approach a), not eliminated. The one added
  dependency is the accepted cost.
- **jsdiff `diffLines` directly (instead of `diffArrays` over our tokens).**
  Simpler call, but wrong output: jsdiff keeps the trailing `\n` inside each
  line token, so a pure reorder renders four rows not two and the
  trailing-newline handling inverts (both verified on 9.0.0). Declined —
  `diffArrays` over `splitLines` tokens is the same Myers engine with the
  intended line model.
- **`fast-diff` / `diff-match-patch`.** Character-oriented; would need a
  line-tokenizing wrapper to emit line rows, reinventing what jsdiff's array
  mode gives directly. Declined in favor of `diff`'s native `diffArrays`.
- **Unified diff with context rows now.** More readable for long files, but
  requires a new `data-kind="context"` plus a CSS rule
  (`app.css:1126-1137` styles only add/del), changes the rendered-row-count
  contract the new-file test pins, and expands a walking-skeleton renderer
  fix into a UX change. `diffArrays` keeps the unchanged parts available, so
  this layers on later without reworking the algorithm. Deferred (Open
  Question).
- **Patch the set-membership filter in place (multiset counting).** Fixes
  the dup-line collapse but still cannot express a reorder or position
  rows correctly — P1's core. Declined.
- **Move detection / intra-line (word) diff.** Correctness beyond what a
  read-only observation trace needs; cost without a consumer. Declined
  (fork c).

## Global Constraints

- TypeScript throughout; `moon run compass-ui:typecheck compass-ui:test`
  must pass; biome-clean.
- UI-only: no `compass.v1` contract change, no transport change, no
  `FileDiff` shape change (`session-events.ts:22-27` mirrors a proto on the
  RIG-1342 lane), no daemon.
- One new npm dependency: `diff` (jsdiff), the line-level Myers engine
  (fork a — Matt ruled use a diff library). `diff@9.0.0`: BSD-3-Clause
  (compatible with the UI's AGPL-3.0-only), zero deps and zero peer deps,
  bundled TS types (no `@types/diff`), ESM entry `libesm/index.js` — already
  resolved in the root `bun.lock` via `@oh-my-pi/hashline`. No other runtime
  dep added.
- SolidJS reactivity: `rows` remains a derived accessor read inside
  `<For each={rows()}>`; no memo/signal machinery added.
- Existing DOM contract unchanged: `.block-diff` / `.diff-path` /
  `.diff-line[data-kind="add"|"del"]` / `.diff-gutter` / `.diff-body`
  (`SessionTrace.tsx:24-34`); no new `data-kind` value, no CSS change
  (`app.css:1095-1137`).
- The two existing DiffBlock render tests stay green:
  `"tool item with a diff renders add/del lines and the path"`
  (`SessionTrace.test.tsx:106-142`: add + del both render for an edit) and
  `"tool item with a new-file diff (null oldText) renders only add rows"`
  (`SessionTrace.test.tsx:148-183`: new file: 0 dels, exactly
  `newText.split("\n").length` adds).
- Red-first discipline (rule://red-green-testing): every new test is
  written and observed red against the current set-membership code before
  the implementing change lands.

## Plan

### T1 — `line-diff.ts`: pure line diff (jsdiff) + row model (red → green)

Create `apps/ui/src/line-diff.ts` exporting the row type and
the diff function, plus `apps/ui/src/line-diff.test.ts` with
the suite written first and red (red against a temporary re-implementation
is meaningless — the module is new, so "red" here means: the suite is
committed to expectations derived from the P1/P2 defects, and the T2
component fixtures below are red against the *current* `DiffBlock`).

Interfaces:

```ts
// apps/ui/src/line-diff.ts

/** One rendered diff row. Matches DiffBlock's existing data-kind vocab. */
export interface DiffRow {
 kind: "add" | "del";
 text: string;
}

/** Split newline-terminated text into content lines:
 *  "" -> [], "a" -> ["a"], "a\n" -> ["a"], "a\nb" -> ["a","b"]. */
export function splitLines(text: string): string[];

/** Production edit-distance budget for the default (no-arg-budget) call path.
 *  The concrete number is Open Question 2 (parked for Matt); the guard
 *  mechanism (bound + coarse fallback) is committed regardless of the value. */
export const DEFAULT_MAX_EDIT_LENGTH: number;

/** Line diff of a FileDiff's texts, via jsdiff `diffArrays` over `splitLines`
 *  tokens. `oldText` null = new file (all adds). Rows are emitted in document
 *  order: for each change part, dels then adds; unchanged lines are omitted.
 *  `maxEditLength` bounds the jsdiff edit distance (defaults to
 *  `DEFAULT_MAX_EDIT_LENGTH`); when the edit distance exceeds it, jsdiff
 *  returns `undefined` and we fall back to a coarse all-del/all-add rendering
 *  of the whole files, so worst-case latency stays bounded. Tests pass an
 *  explicit budget straddling a fixture's edit distance to exercise both paths
 *  deterministically, decoupled from the production default. */
export function diffRows(
  oldText: string | null,
  newText: string,
  maxEditLength?: number,
): DiffRow[];
```

Algorithm: `splitLines(oldText ?? "")` and `splitLines(newText)` to line
tokens, `diffArrays(oldLines, newLines, { maxEditLength: maxEditLength ??
DEFAULT_MAX_EDIT_LENGTH })` (from `diff`), then flatten: for each returned
part, skip unchanged parts, and for an `added`/`removed` part push one
`DiffRow` (`add`/`del`) per element of `part.value`. jsdiff emits a removed run
before the added run that replaces it, giving standard hunk order. When
`diffArrays` returns `undefined` (edit distance exceeds the effective budget),
fall back to coarse rows: one `del` per `oldLines` element followed by one
`add` per `newLines` element (the exact fallback rendering is Open Question 2).
Pure function; imports only `diffArrays` from `diff`, no Solid imports.

Test cycle (bun test via
`direnv exec . moon run compass-ui:test`),
concrete fixtures:

- reorder: `diffRows("a\nb", "b\na")` → exactly 2 rows, one `del` and one
  `add`, with `del.text === add.text` and that text ∈ {`"a"`, `"b"`}. jsdiff
  9.0.0's removal-leaning tie-break emits `[{del "a"}, {add "a"}]` (keeps `b`),
  but *which* line Myers keeps is an undocumented heuristic a future major could
  legally flip while staying minimal — so assert the two-row del-then-add shape
  and the `del.text === add.text` invariant, not the pinned line. (One exact
  `=== "a"` assertion may be kept if commented as jsdiff-9.0.0-pinned.)
- dup-line edit: `diffRows("a", "a\na")` → `[{kind:"add", text:"a"}]`;
  `diffRows("a\na", "a")` → `[{kind:"del", text:"a"}]`.
- empty text: `diffRows("", "")` → `[]`; `diffRows(null, "")` → `[]`;
  `diffRows("a", "")` → `[{kind:"del", text:"a"}]`.
- trailing newline: `diffRows("a\n", "a\nb\n")` →
  `[{kind:"add", text:"b"}]` (no phantom blank row);
  `diffRows("a", "a\n")` → `[]` — the accepted trailing-newline blind spot
  (Approach §Line splitting), asserted so the choice is pinned;
  `diffRows("a\n", "a\n\n")` → `[{kind:"add", text:""}]` (a real EOF blank
  line IS rendered — guards against over-trimming; relies on jsdiff's
  `ArrayDiff.removeEmpty` identity override keeping the `""` token).
- new file: `diffRows(null, "x\ny")` → two adds, in order.
- changed run ordering: `diffRows("a\nold\nz", "a\nnew\nz")` →
  `[{kind:"del", text:"old"}, {kind:"add", text:"new"}]`.
- large input under budget (full-granularity discrimination): old and new are
  large inputs (e.g. 2000 lines) that SHARE every even-indexed line and differ
  on every odd-indexed line (changed lines interleaved with unchanged), passed
  with an explicit `maxEditLength` above the resulting edit distance →
  `diffArrays` runs the real diff (the shared lines keep the edit distance well
  below the disjoint worst case, so it returns quickly, no `m×n` allocation)
  and yields interleaved single-line `del`/`add` pairs around the retained even
  lines. That output is STRUCTURALLY distinct from the coarse fallback, which
  emits every line — unchanged ones included — as all-del-then-all-add: assert
  a known unchanged even line's text renders in NEITHER a `del` nor an `add`
  row (full granularity drops unchanged lines; the fallback would emit each
  twice). That assertion actually discriminates the non-fallback path — a
  disjoint input cannot, since there the full-granularity output equals the
  coarse shape.
- over-budget fallback: a fully disjoint input (old = `range(2000)`, new = a
  disjoint `range(2000)` sharing no line — edit distance D ≈ 4000, the same
  large-churn cliff measured in Approach) passed with an explicit
  `maxEditLength` *below* D → `diffArrays` returns `undefined` and `diffRows`
  yields the coarse all-2000-dels-then-all-2000-adds shape without a freeze,
  proving the guard fires and the fallback renders (the exact wrong-input the
  deleted hand-rolled size guard existed to catch, now via the library's own
  bound). For a zero-common-subsequence input the coarse shape also equals the
  full-granularity output, so this fixture pins the *fallback path* via the
  below-D budget, not the output shape.

### T2 — `DiffBlock` wiring + component-level red fixtures

Write the component-level red tests FIRST in
`apps/ui/src/components/SessionTrace.test.tsx` (they fail
against the set-membership code), then swap `DiffBlock`'s derivation to
`diffRows` and delete `oldLines`/`newLines`/the inline `rows` body
(`SessionTrace.tsx:9-22`) plus the now-wrong doc comment
(`SessionTrace.tsx:4-7`).

Interfaces:

```ts
// apps/ui/src/components/SessionTrace.tsx (DiffBlock)
import { type DiffRow, diffRows } from "../line-diff";

const rows = (): DiffRow[] =>
  diffRows(props.diff.oldText, props.diff.newText);
```

The production call omits the third argument, so it uses
`DEFAULT_MAX_EDIT_LENGTH` (the value parked in Open Question 2); only the T1
unit fixtures pass an explicit budget. JSX unchanged (`SessionTrace.tsx:23-35`).

Test cycle (same runner), red against current `DiffBlock`:

- reorder fixture: `oldText: "a\nb"`, `newText: "b\na"` → at least one
  `.diff-line` renders (red today: current code renders zero rows).
- dup-line fixture: `oldText: "a"`, `newText: "a\na"` → exactly one
  `[data-kind="add"]`, zero dels (red today: zero rows).
- trailing-newline fixture: `oldText: "a\n"`, `newText: "a\nb"` → exactly
  one add with text `"b"` and **no blank-bodied row** (red today: the
  membership filter emits a phantom `del ""` for the vanished trailing
  `""`).
- empty-text fixture: `oldText: null`, `newText: ""` → zero `.diff-line`
  rows (red today: one phantom blank add row from `"".split("\n")` →
  `[""]`).
- regression gate: the two existing tests (`"...renders add/del lines and
  the path"`, `"...new-file diff (null oldText) renders only add rows"` —
  `SessionTrace.test.tsx:106-142,148-183`) stay green.

Green = all of the above plus
`direnv exec . moon run compass-ui:typecheck compass-ui:test`
clean and biome-clean.

## Tasks

- [ ] T1: `line-diff.ts` (`splitLines`, `diffRows`, `DiffRow`) + full unit
  suite (reorder, dup-line, empty, trailing-newline, EOF-blank-line,
  new-file, run-order, large-input-under-budget, over-budget-fallback
  fixtures) green; `diff` added to `package.json` deps; the `maxEditLength`
  seam (optional param + `DEFAULT_MAX_EDIT_LENGTH` const) wired with the
  coarse fallback.
- [ ] T2: component red fixtures added to `SessionTrace.test.tsx` and
  observed red; `DiffBlock` rewired to `diffRows`; stale derivation +
  doc comment removed; existing two DiffBlock tests green;
  `moon run compass-ui:typecheck compass-ui:test` + biome clean.

## Open Questions

1. **How to disambiguate non-adjacent changed runs?** (fork b) Changed-only
   rows have a real readability cost the binary "context rows vs nothing"
   framing hides: two unrelated single-line changes far apart in a file
   render as four adjacent rows `-x +y -p +q` with no boundary — visually
   identical to one contiguous 2-line rewrite. The reorder case is worse:
   `a\nb\nc` → `c\nb\na` renders (e.g., on jsdiff 9.0.0) `-a -b +b +a`, where
   the `-b +b` adjacency reads as a pointless delete-and-re-add when `b` is
   actually the kept line
   between them. Three options, cheapest to richest:
   - **(i) Nothing — changed-only, as designed.** Ships P1's real value
     (something correct renders for every change) at zero DOM/CSS/test cost.
   - **(ii) Hunk-separator row between changed runs.** One new
     `data-kind` (a `⋯` divider) + one CSS rule, no context text. Separators
     render only *between* runs, so a single-hunk diff (and every new file)
     has none — both existing kept-green tests stay untouched. Resolves the
     misleading adjacency at a fraction of context rows' cost.
   - **(iii) Full unified-diff context rows.** Most readable on long files;
     needs `data-kind="context"` + CSS (`app.css:1126-1137` styles only
     add/del) and changes the row-count contract the new-file test pins.
   **Recommendation: (i) not now**, defensible for a fixture-backed walking
   skeleton — but (ii) is the cheap middle ground if the trace proves hard to
   read once real diff data flows, and jsdiff already returns the unchanged
   runs either extension needs. Non-load-bearing: the record is correct and
   shippable at (i); this is a later UX call, deferred with a rationale.

2. **What to render when the diff exceeds the `maxEditLength` budget, and what
   the budget is?** (Surfaced by the design-critic red-team.) jsdiff's Myers is
   linear space but O((m+n)·D) time, so a fully disjoint large-churn edit (a
   regenerated lockfile, a wholesale file rewrite — an in-scope input, since
   `FileDiff` has no size bound) degenerates toward quadratic time computed
   synchronously in `rows()` on the Solid render path (~720ms measured for
   2000×2000 disjoint lines, jsdiff 9.0.0). The design bounds this with
   `diffArrays`'s own `maxEditLength`; two sub-decisions are user-visible and
   left for review:
   - **Fallback rendering.** (a) Coarse all-del-then-all-add of both whole
     files — matches the deleted hand-rolled guard's behavior, every line still
     visible, but a huge two-block wall. (b) A single "diff too large to render
     (N lines changed)" placeholder row — cheapest, but hides the content.
     **Recommendation: (a)** — preserves the "something correct renders" P1
     value; (b) is a one-line change if (a) proves too heavy in practice.
   - **Budget value.** A concrete `maxEditLength` (e.g. cap D at a few thousand,
     ~sub-100ms) vs. a wall-clock `timeout`. **Recommendation:** a fixed
     `maxEditLength` — deterministic and test-pinnable, unlike a timeout. The
     chosen number populates `DEFAULT_MAX_EDIT_LENGTH` (T1); the guard
     mechanism and the unit fixtures do not depend on its value, so this call
     tunes production behavior only.
   Load-bearing: unlike OQ 1, leaving this unguarded ships a real freeze, so the
   guard is in the plan now (T1 over-budget fixture); only the *rendering* and
   *budget number* are the open call.
