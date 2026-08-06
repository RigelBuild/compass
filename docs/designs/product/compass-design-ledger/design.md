# Compass design-decision ledger

Status: Active

> Internal design record — July 2026. Establishes the canonical
> `docs/designs/product/DECISIONS.md` decision ledger, machine-checkable
> `Status:` headers on every product design record, and a bun/TS CI gate
> against dangling supersessions. The ledger shape (over the alternatives
> below) was ratified by Matt via `ask` on 2026-07-22 — this record does not
> reopen that choice; it specifies it.

## Problem / Intent

The Compass design corpus is 19 records / ~13,086 lines under
`docs/designs/product/` (verified this session: `ls docs/designs/product/*/design.md
docs/designs/product/*.md | wc -l` → 19 before this record; total line count
13,086 before this record's skeleton). Records freeze on merge and later
records supersede specific decisions **by citation** — the convention is
stated in `docs/designs/product/compass-0.5/design.md:10-12`:

> "This record captures only what v0.5 changes and why, and **supersedes
> specific prior decisions by citation** — it does not rewrite any frozen
> record"

and restated as a Global Constraint in
`docs/designs/product/compass-0.6/design.md:1116-1118`:

> "**Frozen-record convention.** This record freezes on merge; later changes
> supersede by citation, never rewrite"

The failure mode: supersession is only visible **forward** — the superseding
record cites the superseded one, but nothing in the superseded record (or
near it) points the other way. An agent grounding on a single record cannot
tell that a decision in it was overturned elsewhere. This bit hard on
2026-07-22: an agent answered a load-bearing security question (#855 OQ-2)
citing `packages/compass-agent/src/mapping.ts:16-19`:

> "SESSION (opaque, OMP-owned): the execution trace (thinking chunks, tool
> calls, todos, edits) is OMP-native session data — the mapper wraps each raw
> SDK event verbatim as `SessionFrame.event` bytes and OMP's own renderer
> inflates it"

— but that ownership model was overturned by
`docs/designs/product/compass-0.8-threading-and-session-renderer/design.md:18-20`:

> "Matt has now ruled the opposite: Compass builds a **first-party typed
> session renderer**, with session events crossing a **typed gRPC stream** —
> not opaque bytes, and explicitly **not ACP**."

The sibling record even flags the comment as stale
(`docs/designs/product/compass-message-surface-rendering/design.md:448-451`:
"Compass-0.8 ruled the trace 'not opaque bytes, and explicitly **not ACP**'
… the comment's vintage is stale") — but no surface an agent reads *first*
carried the supersession. Supersession churn is real and concentrated:
`compass-0.6/design.md` carries 50 case-insensitive "supersed" mentions,
`compass-0.8-threading-and-session-renderer/design.md` 12,
`compass-agent-container-runtime.md` and `compass-0.7-channel-workspace` 8
each (counted this session with `grep -ci supersed` per record).

Intent: give every Compass agent ONE canonical read-first surface carrying
current truth — every ratified decision as a one-liner with a live/superseded
status and a link to the frozen record holding the rationale — plus
machine-checkable per-record status headers and a CI gate so a dangling
supersession can never land silently.

## Approach

**A decision ledger over the existing records** (ratified by Matt,
2026-07-22). Four parts:

### 1. `docs/designs/product/DECISIONS.md` — the canonical ledger

One file agents read FIRST. Every ratified decision is one table row: a
stable ID, a one-line decision statement, a status, and a link to the record
that holds the full rationale. Row schema (leading + trailing pipes,
markdownlint-style):

```markdown
| ID | Decision | Status | Record |
| --- | --- | --- | --- |
| DL-014 | Session trace is OMP-native opaque bytes; OMP renders it | Superseded by DL-041 (Matt, 2026-07-22) | [v0.6 §Approach](compass-0.6/design.md#approach) |
| DL-041 | Session events cross a typed gRPC stream; Compass renders first-party | Active (Matt, 2026-07-22) | [v0.8 §Approach](compass-0.8-threading-and-session-renderer/design.md#approach) |
```

- `ID` — `DL-<zero-padded int>`, globally unique, append-only, never reused.
- `Status` — exactly `Active (<who>, YYYY-MM-DD)` or
  `Superseded by DL-<n> (<who>, YYYY-MM-DD)`. **Every** row carries
  provenance (who + date), not only superseded ones — overlapping one-liners
  need a recency signal to arbitrate. Machine-parseable; the gate (part 3)
  verifies every `Superseded by DL-<n>` targets an existing row.
- `Decision` — a one-line paraphrase, **immutable after append**: a new
  ruling is a new row plus a `Superseded` flip on the old, never an in-place
  reword (the cell is a truth surface the gate cannot re-derive from the
  record, so it must not silently drift).
- `Record` — a relative markdown link into the frozen record (path must
  resolve; a `#anchor` is checked when present and **required** for links
  into large records (>~50 KB, e.g. `compass-0.6`), so rationale is genuinely
  one hop away, not a hunt through a 118 KB file).
- Rows are grouped under `##` topic headings (transport, session rendering,
  storage, UI shell, agent runtime, …) so the ledger stays scannable; the
  gate treats headings as opaque.

The existing per-record docs STAY as frozen-per-PR rationale. The ledger
replaces nothing and merges nothing; it is an index of current truth. The
accepted cost is two hops (ledger → record for detail) — deliberate, see
Alternatives.

### 2. Per-record `Status:` headers

Every record under `docs/designs/product/` gets a machine-checkable header
line directly under its H1 (two records already carry an informal version —
`compass-0.8-threading-and-session-renderer/design.md:3` reads
"Status: draft (freezes on merge)."; most records carry none, only a prose
blockquote preamble, e.g. `compass-0.4/design.md:3` "> Internal design
record — July 2026. …" — verified this session). Grammar:

```text
Status: Draft | Active | Historical | Superseded by <relative-path>
```

The gate matches this **anchored**: `^Status: (Draft|Active|Historical|Superseded by \S+)$`.
The `$` is load-bearing — unanchored, the informal
`Status: draft (freezes on merge).` line T2 rewrites would itself pass, so
the gate would never force the rewrite. No trailing text (not even a period)
is legal.

- `Draft` — pre-merge. Flips to `Active`/`Historical` in the freeze PR. NOT
  gate-checked on `main` (a merged record left at `Draft` is a lifecycle
  slip the T4 same-PR rule catches procedurally, not the gate — the gate
  validates grammar + the `Historical` set, not lifecycle timeliness).
- `Active` — frozen, and at least one of its decisions is current truth.
- `Historical` — mechanically assigned: a record is `Historical` **iff** it
  is in the version-narrative chain (v0.3 `compass.md`, 0.4, 0.5, 0.5-server,
  0.6, 0.7, 0.8). The gate checks this assignment, not just the grammar, so a
  non-`Draft` record can't drift to a wrong-but-grammatical value (an
  out-of-chain record marked `Historical`, or a chain record marked `Active`).
  `Draft` stays an ungated lifecycle state the T4 same-PR rule catches
  procedurally, so the enforced invariant is one-directional (`Historical` ⇒
  in-chain; a non-`Draft` chain record ⇒ `Historical`), not a full iff. A
  record can be `Historical` while many of its decisions are still
  `Active` in the ledger — record-level status is the record's lifecycle;
  per-decision truth lives ONLY in the ledger.
- `Superseded by <path>` — the record is wholly overturned; `<path>` must
  resolve to an existing record (gate-checked forward pointer).

This is a **one-time mechanical amendment** to frozen records — a status
*label*, not a rewrite of any decision content. The record authorizing it is
this one; the freeze convention ("a later change adds a new record, never
rewrites one", `AGENTS.md:59-62`) is preserved for decision content.

### 3. `tools/design-ledger-gate` — the CI gate

A bun/TypeScript tool in the repo's established gate shape (copy
`tools/no-bash-gate`: `index.ts` + `index.test.ts` + `moon.yml` +
`biome.json` + own `bun.lock`; pure `evaluate()` core with injected file
reads, exit codes `0` pass / `1` violation / `2` usage error —
`tools/no-bash-gate/index.ts:19-22` documents exactly this contract). Never
bash: the no-bash gate fails CI on any new `.sh`
(`tools/no-bash-gate/index.ts:11-14`).

It fails CI when:

- a ledger row's `Superseded by DL-<n>` targets a nonexistent row ID;
- a ledger row's `Record` link does not resolve: the file is missing, or a
  present `#anchor` names no heading in the target, or a link into a large
  record (>~50 KB) omits the required `#anchor`;
- a record's `Status: Superseded by <path>` pointer does not resolve;
- a record under `docs/designs/product/` is missing a parseable `Status:`
  header, or carries a malformed/unanchored/wrong-`Historical`-set value;
- a ledger row's `Status` cell matches neither the
  `Active (<who>, YYYY-MM-DD)` nor the
  `Superseded by DL-<n> (<who>, YYYY-MM-DD)` grammar;
- a `DL-<n>` ID is duplicated, self-superseded, or in a supersession cycle
  of any length — a `Superseded` chain that loops back instead of ending at
  an `Active` row (`DL-a`→`DL-b`→`DL-a`, or longer
  `DL-a`→`DL-b`→`DL-c`→`DL-a`);
- a PR's changed set touches a `docs/designs/product/**` record but not
  `DECISIONS.md` and carries no `Ledger-impact: none` declaration
  (touch-coupling, DL-Q1).

`DECISIONS.md` is parsed as the ledger, never scanned as a record — it
matches the `product/*.md` glob but carries no `Status:` header, so without
the carve-out the missing-header check fails on the ledger itself. Beyond
pointer syntax + touch-coupling, what the snapshot gate does NOT prove
(append-only rows, `Decision`-cell immutability) is review-enforced in v1; a
fast-follow diff-aware core promotes it to gate-checked (merge-base compare).

### 4. The workflow rule — same-PR ledger flip + code-comment citation

The ledger is the ONE living exception to freeze-on-merge, BY DESIGN. It
stays consistent with the freeze model the same way specs do — the sealed
convention "update the matching `docs/specs/` doc *in the same PR* as the
code" (`AGENTS.md:73-74`) extends to the ledger: **the PR that freezes a
record also appends its new decision rows and flips any rows it supersedes.**
A design PR is not merge-ready until its ledger delta is in the same diff —
and the T3 touch-coupling check enforces this mechanically (DL-Q1), not by
convention alone. The same rule carries the code-comment clause (DL-Q2): a
code comment stating design-truth cites its `DL-<n>`, and flipping a row
obligates a same-PR grep-sweep for that ID across the codebase. Amend
`AGENTS.md` §"Docs & specs" and the design skill
(`personal/matt/agents/skills/design/SKILL.md`) to state both.

## Alternatives considered

### Single 13k-line super-doc (merge all records into one living file)

Rejected. It destroys the one-PR-one-decision + freeze-on-merge model
(`docs/designs/platform/docs-system.md:31-33`: records are "point-in-time …
Frozen when decided"), and a single hot file is a permanent merge-conflict
magnet on a running multi-agent wave. The ledger keeps the frozen records
and pays a two-hop read instead.

### Status headers only (no ledger)

Rejected. A header marks a *record's* lifecycle, but supersession is
per-decision: `compass-0.6/design.md` carries 50 supersession mentions and
is simultaneously vintage and load-bearing — a single record-level flag
cannot say *which* of its decisions still hold. The triggering failure was
exactly a per-decision overturn inside an otherwise-current surface. Headers
are kept, but as the machine-checkable substrate under the ledger, not the
truth surface.

### Per-record "superseded-by" backlinks only

Rejected as the sole mechanism. Backlinks fix the forward-only-citation
problem record-by-record, but an agent still has to know which record to
open — there is no read-first surface, and 19 records is already past the
point where "read them all" is a real instruction. Backlinks at decision
granularity also require editing frozen prose, which the convention forbids.
The record-level `Status: Superseded by <path>` header is the bounded,
mechanical form of this idea and is included.

## Plan

**Build order (one stack, T3 at the base).** T1, T2, and T3 land as a single
stack merged together, with T3 (the gate) at the base: T1's and T2's test
cycles both run `moon run design-ledger-gate:ci`, so the gate tool must exist
first, and — critically — T3's live CI wiring (the tree-scan under `GATE_ROOT`)
must NOT reach `main` ahead of T2's header backfill, or the missing-`Status:`
check hard-fails on all 19 header-less records and reddens `main`. A sequential
`T3 → T1 → T2` split is therefore rejected: it wires the gate live at the T3
boundary while the records it scans are still header-less, so T3's own
acceptance (`design-ledger-gate:ci` green) is unsatisfiable as a standalone PR.
Folding all three into one merge is the only order where every boundary is
green. (T2 still stacks logically on T1 — the ledger must exist before
`Historical` can delegate truth to it — but they merge together.) T4
(docs-only) lands last. The numbered tasks below are definition order, not PR
order.

### Global Constraints

- **No-bash gate.** Any CI check is a bun/TypeScript tool
  (`tools/<name>/` with `index.ts` + `index.test.ts` + `moon.yml`), never a
  bash script — the repo convention has teeth
  (`tools/no-bash-gate/index.ts:11-14`: "A NEW bash script therefore fails
  CI immediately: convert it to a bun/TS tool (see tools/wait-for-reviews
  for the pattern)"). Copy the `tools/no-bash-gate` project shape, including
  its own `bun.lock`, `biome.json`, `tsconfig.json`, and the
  `install`/`lint`/`typecheck`/`test`/`gate`/`ci` moon tasks
  (`tools/no-bash-gate/moon.yml:10-43`).
- **Pure decision core.** The gate's rule logic is a pure exported
  `evaluate()` over injected inputs (no I/O), unit-tested without a real
  filesystem — the pattern `tools/no-bash-gate/index.ts:163-176` uses
  (`evaluate(files, allow, isShell)`) and `AGENTS.md:97-98` names for the
  spec-impact gate ("the decision core is a pure `evaluate()` in `gate.ts`
  (no I/O)").
- **markdownlint-clean.** Every touched `.md` passes the repo config
  (`.markdownlint.json` + `.markdownlint-cli2.jsonc`, run by the root
  `markdownlint` moon task, `moon.yml:73-76`): blank lines around
  headings/lists/fences/tables, a language on every fence, leading +
  trailing table pipes.
- **Freeze-on-merge, with one exception.** `DECISIONS.md` is the single
  living document under `docs/designs/` — continuously appended/flipped,
  never frozen. Every other record stays frozen; the only in-place edit this
  design authorizes on a frozen record is adding/normalizing the one
  `Status:` header line (T2) and later flipping it via the same-PR rule
  (T4). Decision prose in frozen records is never edited.
- **Ledger rows are append-only.** A superseded row is never deleted; its
  `Status` cell flips. IDs (`DL-<n>`) are never reused.
- **Spec impact.** Each PR carries an explicit `Spec-impact:` declaration
  (`AGENTS.md:83-94`); T1–T3 are docs/tooling (`Spec-impact: none` unless a
  spec names the design workflow), T4 amends workflow docs.

### T1 — `DECISIONS.md` format + initial population

Create `docs/designs/product/DECISIONS.md`: a short preamble and `##` topic
sections with the decision table(s) populated from the existing 19 records.
The preamble states the reading rule AND its closed-world limit: "read this
first; per-decision truth for *ledgered* decisions lives here, rationale in
the linked frozen records. **Absence of a row is not evidence of no ruling** —
  the ledger covers named, citable decisions (DL-Q3 granularity); for anything
not here, the linked records remain authoritative." (The naive "truth lives
here" phrasing is a trap: it turns an omission at population time into false
confidence.)

Population pass: walk each record's Decision/Global Constraints/Approach
rulings at the DL-Q3 granularity; every ratified, still-referenced decision
gets a row; each known overturn (the v0.6 opaque-trace → v0.8 typed-stream
flip; the v0.6 §T7 comms-primary shell → v0.7 board-primary flip,
`compass-0.7-channel-workspace/design.md:3-5`; the compass-0.5 D5 Ask shape
→ `compass-ask-typed-derivation.md:5` "This record supersedes design
compass-0.5 D5's `Ask` shape by citation") gets a Superseded row pointing at
its Active successor.

Completeness bar (machine-anchored, not spot-audited): grep the whole product
corpus for every case-insensitive `supersed` mention (~116 today, concentrated
in `compass-0.6` (50) and `compass-0.8` (12) but spread across ~14 records) and
require each to map to a ledger
`Superseded`/`Active` row pair OR be explicitly waived with a reason in the
population PR. Plus: every record contributes at least one row or is listed
in a "no live decisions" note; every `Superseded by DL-<n>` resolves. This
converts "did we catch every overturn?" from reviewer judgment into a
checklist derived from the corpus itself.

Test cycle: run the T3 gate against the populated ledger (green); root
`markdownlint` task green; the grep-derived supersession list is fully
mapped-or-waived (not just the three known overturns); a spot-read of those
three reads correctly ledger-first.

Interfaces:

- Produces `docs/designs/product/DECISIONS.md`.
- Row schema: `| DL-<n> | <one-line decision> | <status> | <record-link> |`
  where `<status>` is `Active (<who>, YYYY-MM-DD)` or
  `Superseded by DL-<n> (<who>, YYYY-MM-DD)` — **every** row carries
  provenance (who + date), not only superseded ones, so overlapping
  one-liners have a recency signal to arbitrate. Leading + trailing pipes,
  one decision per row, IDs zero-padded to three digits (`DL-001`). A
  `Record` link into a large record (>~50 KB, e.g. `compass-0.6`) MUST carry
  a resolving `#anchor` so rationale is genuinely one hop away. The
  `Decision` cell is immutable after append (a new ruling is a new row + a
  `Superseded` flip, never a reword).
- Consumed by: every Compass agent brief (read-first), the T3 gate (parses
  the table), the T4 workflow rule.

### T2 — `Status:` header schema + backfill across all 19 records

Normalize/add the header line `Status: <value>` immediately after the H1 of
every record under `docs/designs/product/` (both layouts: `<record>.md` and
`<record>/design.md`). Values per the grammar in §Approach. Suggested
backfill: `Historical` for the version chain (`compass.md`, 0.4, 0.5,
0.5-server, 0.6, 0.7-channel-workspace, 0.8-threading) — their live rulings
are ledger rows; `Active` for merged feature records; `Draft` stays only on
genuinely unmerged records. The two existing informal lines
(`compass-0.8-threading-and-session-renderer/design.md:3`,
`compass-message-surface-rendering/design.md:3` — "Status: draft (freezes on
merge).") are rewritten to the grammar; prose preambles stay untouched.

Test cycle: T3 gate green over the full corpus (it fails on any missing or
malformed header, so backfill completeness is machine-verified);
`markdownlint` green.

Interfaces:

- Produces: one `Status:` line per record, matching the anchored regex
  `^Status: (Draft|Active|Historical|Superseded by \S+)$` on the first
  non-blank line after the H1. **The `$` anchor is load-bearing:** unanchored,
  the informal `Status: Draft (freezes on merge).` this task rewrites would
  itself pass as `Draft`, so the gate would never force the rewrite. No
  trailing text (including a period) is legal.
- `Historical` is mechanically assignable: a record is `Historical` iff it is
  in the version-narrative chain (`compass.md`, 0.4, 0.5, 0.5-server, 0.6,
  0.7-channel-workspace, 0.8-threading). `Active` is every other merged
  record. The gate checks this assignment, not just the grammar — no
  judgment-call `Active`/`Historical` drift. `Draft` on a merged record is
  NOT gate-checked (the freeze flip stays procedural, folded by the T4
  same-PR rule); the gate validates grammar + the Historical set, not
  lifecycle timeliness.
- Consumed by: the T3 gate; agents skimming a record top.
- Depends on T1 (ledger must exist before `Historical` can delegate truth to
  it) and on T3 (the gate); all three merge in the one stack (see §Plan).

### T3 — `tools/design-ledger-gate` (bun/TS) + unit tests

New tool cloned from the `tools/no-bash-gate` shape. Core:
`evaluate(ledger: LedgerRow[], records: RecordHeader[], changed: { files: string[]; body: string | null }, readRecord: (p: string) => { headings: string[]; sizeBytes: number } | null): Violation[]`
— pure, no I/O; `runOnce(deps)` wires real reads + the PR's changed `{files, body}`;
`if (import.meta.main)` entry. `readRecord` returns the target's heading slug
set + byte size, or `null` when the path does not resolve (subsuming the old
existence check) — this is what lets the link checks below verify anchors and
apply the size rule; `runOnce` wires it to real reads under `GATE_ROOT`.
Checks (from §Approach part 3): dangling `Superseded by DL-<n>` row targets;
unresolvable `Record` links — path missing (`readRecord` returns `null`), or
(when a `#anchor` is present) the slug absent from the target's `headings`, or
a link into a large record (`sizeBytes` > 50 KB) that omits the **required**
`#anchor`; unresolvable record-level `Superseded by <path>` pointers;
missing/malformed `Status:` headers; malformed row status cells. Also: a
record marked `Superseded by` must carry a resolvable forward pointer (the
grammar makes this structural); duplicate `DL-<n>` IDs fail; `DL-n` superseded
by itself fails; and a **supersession cycle of any length** fails — a
`Superseded` chain that loops (`DL-a`→`DL-b`→`DL-a`, or longer) instead of
terminating at an `Active` row.

**Touch-coupling check (DL-Q1, ruled (a); v1-required).** Any PR whose
changed set touches `docs/designs/product/**/design.md` (or a `product/*.md`
record) MUST also touch `docs/designs/product/DECISIONS.md`, unless the PR
carries a `Ledger-impact: none` declaration — mirroring the `Spec-impact:`
escape hatch (`AGENTS.md:83-94`) and the deterministic spec-impact gate
(`ci/workflows/meta.ts:175-185`). Because that declaration lives in the PR
body, not the file set, `changed` carries both `files` and `body`: the pure
core reads `Ledger-impact:` from `changed.body` via a start-of-line regex
(exactly as `spec-impact-gate`'s `declaredSpecImpact(body)` reads `Spec-impact:`
from `PullRequestFacts.body`, `tools/spec-impact-gate/gate.ts`), so the hatch is
decided inside the tested core, not bolted on outside it. This is the mechanical
half of the T4 same-PR-flip rule: it catches the forgot-the-flip-entirely
failure that a procedural rule leaves green. This diff-aware `{files, body}`
pair is the only diff-aware input v1 needs; the finer merge-base checks below
are a fast-follow.

**`DECISIONS.md` is excluded from the record scan.** It matches the
`docs/designs/product/*.md` glob but is the ledger, not a design record — it
carries no `Status:` header and is parsed as the ledger table instead.
Without this carve-out the missing-header check fails on the ledger itself
and T1's "gate green" test cycle is unsatisfiable.

**Enforcement boundary (honest scoping).** Beyond touch-coupling, the pure
snapshot `evaluate()` checks pointer/grammar integrity over a single tree
state. Two Global Constraints — rows are append-only, and frozen
`Decision`-cell prose is immutable-after-append — are **not** snapshot-checkable
and, in v1, are review-enforced (stated so nobody believes the gate proves
more than pointer syntax + touch-coupling). The fast-follow diff-aware core
promotes them to gate-checked via merge-base comparison: no row deleted, no
`Decision` cell reworded, no `Superseded`→`Active` reversal — the gate runs
with git available (`GATE_ROOT` = git toplevel).

Test cycle (red → green): `index.test.ts` unit-tests `evaluate()` with
fixture rows/headers — happy path, each violation class, duplicate IDs,
self-supersession, supersession cycles (2- and ≥3-length), a dead
`#anchor` (slug absent from the target's `headings` → fail), a >50 KB
record link that omits the required anchor
(`sizeBytes` over threshold → fail), trailing-junk `Status:` line (must fail), a
`DECISIONS.md`-shaped input (must NOT trip the missing-header check), and
touch-coupling (a `changed` set whose `files` touch a record but not
`DECISIONS.md`, with `changed.body` lacking a `Ledger-impact:` line, must fail;
with the declaration in `body`, or `files` touching both, must pass). `moon
run design-ledger-gate:ci` green; wire the gate into the CI workflow beside
the spec-impact gate (`ci/workflows/meta.ts:181-182` runs that gate via
`cd tools/spec-impact-gate && bun run index.ts` — follow the same wiring).
ID-collision safety (two PRs both minting `DL-042`) assumes the gate re-runs
in the merge queue on the merged result — the repo has one (`gtmq_` branches,
`AGENTS.md:83-88`), so the duplicate-ID check catches it there.

Interfaces:

- Produces `tools/design-ledger-gate/{index.ts,index.test.ts,moon.yml,package.json,tsconfig.json,biome.json,bun.lock}`.
- Input contract (env): `GATE_ROOT` — directory to scan (default: git
  toplevel), mirroring `tools/no-bash-gate/index.ts:17-18`; parses
  `docs/designs/product/DECISIONS.md` as the ledger, and scans
  `docs/designs/product/**/design.md` + `docs/designs/product/*.md` (excluding
  `DECISIONS.md`) as records.
- Changed-set contract (touch-coupling only): the diff-aware `changed` input
  (`{files, body}`) is sourced the way `tools/spec-impact-gate` does — `REPO`
  (`$CI_REPO`), `PR_NUMBER` (`$CI_COMMIT_PULL_REQUEST`), and `GH_TOKEN` (the
  `META_GITHUB_TOKEN` PAT): `files` via `gh api .../pulls/<n>/files` and `body`
  via `gh pr view --json body` (one `gh pr view --json headRefName,body` call
  serves both, exactly `tools/spec-impact-gate/index.ts`'s `fetchFacts`), wired
  as a `when: { event: ["pull_request"] }` job (`ci/workflows/meta.ts:175-185`).
  Touch-coupling is therefore a **PR-event-only** check: on local `moon ci`,
  push, and merge-queue runs there is no PR context, so `changed` is
  `{files: [], body: null}` and the check no-ops — the snapshot pointer/grammar
  checks (driven by `GATE_ROOT`) still run and gate those events.
  This tool clones `no-bash-gate`'s project
  *shape* for the snapshot scan but the `spec-impact-gate` *wiring* for the
  diff-aware touch-coupling leg.
- Output contract: exit `0` all checks pass, `1` violations (printed one per
  line as `<file>:<line>: <message>`), `2` usage/internal error — the
  `tools/no-bash-gate/index.ts:19-22` code convention.

### T4 — Workflow rule: same-PR ledger flip + code-comment citation

Amend `AGENTS.md` §"Docs & specs" (`AGENTS.md:54-81`) and the design skill
(`personal/matt/agents/skills/design/SKILL.md`) with the rule: a design PR
that freezes a record MUST, in the same diff, (a) append the record's new
decision rows to `DECISIONS.md`, (b) flip the `Status` cell of every ledger
row that record supersedes, and (c) set/flip the affected records'
`Status:` headers. Also state the agent-side reading rule: ground on
`DECISIONS.md` first, follow links for rationale. Mirrors the existing
same-PR spec rule (`AGENTS.md:73-74`: "update the matching `docs/specs/`
doc *in the same PR* as the code").

**Code-comment citation (DL-Q2, ruled (b)).** Two clauses close the
code-comment staleness channel the exemplar traversed:

- A code comment that states a design-truth cites its ledger ID inline
  (`per DL-<n>`), so the comment is greppable by the decision it rests on.
- Flipping a ledger row (clause (b) above) obligates, in the SAME PR, a
  grep-and-sweep for `DL-<n>` across the codebase — every code comment citing
  the flipped ID is updated or an explicit follow-up issue is filed. This is
  what `compass-message-surface-rendering/design.md:448-458` already does ad
  hoc; the rule makes it standing.

**Same-PR enforcement is mechanical (DL-Q1, ruled (a)).** The touch-coupling
check lands in T3 v1 (below), so clauses (a)/(b)/(c) are gate-checked, not
prose-only. Existing code comments are swept to cite `DL-<n>` opportunistically
as records are touched, not in one mechanical pass (the citation rule binds new
and edited comments; a big-bang backfill of every comment is out of scope —
DL-Q4-style deferral).

Test cycle: docs-only for the rule text — `markdownlint` green; the T3 gate
stays green (rule prose changes no parsed surface). The touch-coupling
behavior is tested in T3.

Interfaces:

- Produces: amended `AGENTS.md` §Docs & specs; amended design-skill SKILL.md
  (its "Ship it as a reviewed PR" / freeze section); the code-comment
  `per DL-<n>` citation + grep-sweep-on-flip convention.
- Consumed by: every future design PR author; review bots quoting AGENTS.md.

## Tasks

- [ ] T1 — Create `docs/designs/product/DECISIONS.md`; populate from all 19
  records; known overturns carried as Superseded rows; gate + markdownlint
  green.
- [ ] T2 — Backfill grammar-conformant `Status:` headers on all 19 records
  (Historical for the version chain, Active for merged feature records);
  gate-verified complete.
- [ ] T3 — Build `tools/design-ledger-gate` (bun/TS, pure `evaluate()` core,
  unit tests red→green, exit-code contract 0/1/2), including the DL-Q1
  touch-coupling check; wire into CI beside the spec-impact gate.
- [ ] T4 — Amend `AGENTS.md` + design-skill SKILL.md with the same-PR
  ledger-flip rule, the ledger-first reading rule, and the DL-Q2 code-comment
  `per DL-<n>` citation + grep-sweep-on-flip convention.

## Decisions

Batched to Matt; **ruled 2026-07-22**. All four folded here as frozen
decisions this record merges on.

### DL-Q1 — Same-PR flip is enforced mechanically in v1 (RESOLVED — Matt, 2026-07-22)

T3 as drafted verifies pointer *integrity*, not *timeliness*: a PR could
freeze a superseding record and forget its ledger delta with the gate green.
That is the design's load-bearing hole — a missed flip makes `DECISIONS.md`
affirmatively assert the overturned decision is `Active`, so the read-first
surface emits a false positive; a stale-`Active` ledger is *worse* than no
ledger. The procedural mitigations are the same human-attention mechanism
that already failed on 2026-07-22 (the mapping.ts miss happened *despite*
`compass-message-surface-rendering/design.md:448-451` flagging staleness).

**Ruled (a) — mechanical, in v1.** T3 v1 carries a touch-coupling check: any
PR touching `docs/designs/product/**/design.md` (or `*.md`) MUST also touch
`DECISIONS.md`, with a `Ledger-impact: none` escape hatch mirroring
`Spec-impact:` (`AGENTS.md:83-94`). Deterministic and cheap — the spec-impact
gate is the exact pattern (`ci/workflows/meta.ts:175-185`). The finer
diff-aware core (merge-base compare: no row deleted, no `Superseded`→`Active`
reversal, no `Decision`-cell reword) is a fast-follow, not required for v1.
This is folded into T3's scope and T4's rule below.

### DL-Q2 — Code-comment staleness convention folded into T4 now (RESOLVED — Matt, 2026-07-22)

The triggering exemplar's proximate surface was a CODE comment
(`packages/compass-agent/src/mapping.ts:16-19`, still asserting
the overturned "SESSION opaque, OMP-owned" model in-tree today), read while
answering a question from source. The ledger-first reading rule alone covers
*design* research; an agent reading `src/` to answer a code question may never
open `docs/designs/` and re-hits the identical trap.

**Ruled (b) — fold the code convention now.** T4 is extended: a code comment
that states a design-truth cites its `DL-<n>` ("per DL-014"), and flipping a
ledger row obligates a same-PR grep-and-sweep (or a filed follow-up issue)
for code comments restating the flipped decision.
`compass-message-surface-rendering/design.md:448-458` already does this sweep
ad hoc, proving the class is real. This closes the code-comment channel the
exemplar actually traversed, not only the design-record channel.

### DL-Q3 — Initial population at named-citable-ruling granularity (RESOLVED — Matt, 2026-07-22)

**Ruled: named, citable rulings + known overturns (~60–100 rows).** Populate
at the granularity of anything a record labels a Decision (D-numbers), a
Global Constraint, or a §-level ruling another record cites — plus every
known overturn. Skip inline micro-choices; promote them to rows the first
time a later record supersedes them (the same-PR rule adds both rows then).
Completeness is machine-anchored, not spot-audited: T1's grep-derived bar
requires every corpus "supersed" mention to map to a ledger row pair or be
explicitly waived in the population PR.

### DL-Q4 — Ship product-only now (RESOLVED — Matt, 2026-07-22)

`platform/` records are cited as frozen constraints from product records
(e.g. `compass-0.6/design.md:1112-1114` conforms to
`../../platform/go-toolchain-default.md`), so cross-domain supersession will
eventually exist. **Ruled: product-only now** (matches the ratified shape and
the pain). The row schema already lets a `Record` link point outside
`product/` (relative paths); the gate's scan roots can extend to a repo-wide
`docs/designs/DECISIONS.md` later. Non-load-bearing; deferred by design.
