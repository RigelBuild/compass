---
name: design
description: "Lean up-front design for a change: one pass, one record (Problem, Approach, Plan, Tasks), depth-scaled with a fast path. A design subagent drafts it into the repo; a design-critic subagent red-teams the draft; it ships as its own PR, reviewed by the human and the review agent, then frozen on merge as the contract executing agents read."
---

# Lean design

Use before implementing a non-trivial change. Past the fast path, you as the
driving Manager **delegate the design pass to a `design` subagent** — it grounds
itself in the codebase and writes the design record into the repo. It ships as
its own PR: the operator **and** the review agent review it, and the **merge
freezes** it. Execution then proceeds against the frozen record. Deliberately
light: a design subagent drafts, the operator freezes, execution runs against the
frozen record. This skill is **only the design layer**.

## Fast path

If the change fits in one sentence and has no real design fork, **skip the
artifact — just do it**. A one-line diff, a config tweak, a rename needs no
design doc. (This is the explicit inverse of "design everything.")

## Delegate the design pass

For any non-fast-path change, spawn the design subagent instead of drafting the
artifact inline:

- `task(agent: "design", …)` — it does its own codebase recon and writes the
  record into the repo at `docs/designs/<domain>/<record>.md`.
- Coding then proceeds against the frozen artifact.

The subagent drafts, the operator freezes, execution follows — you don't
hand-write the design when you can delegate the pass.

## Adversarial critique (post-draft red-team)

Once the `design` subagent has drafted the record, run **one** adversarial pass
over it before the operator sees it — so the operator ratifies a design that has
already survived a red-team, not a first draft. This is a single pass, **not** a
loop or a new gate: the operator freeze stays the real gate, and the critic
exists only to make that one ratification better-informed. (A bounded
review-fix-re-review loop belongs in *code* review — `skill://review` — where a
machine all-clear terminates the loop; here nothing does but the human.)

1. **Spawn the `design-critic` subagent** on the drafted record. It reads the
   record **and grounds in the same codebase**, then returns a **structured
   critique** — per core choice: is this the best solution? what alternatives
   exist and why were they not taken? what is the load-bearing weakness? The
   critic **does not edit the record** — adversary is not author, so the attack
   stays honest and the record stays single-authored.
2. **Fold the critique** — the designer (you, or a re-spawned `design` subagent),
   never the critic, does the folding:
   - **Clear improvements** — a weighed alternative the draft omitted, a sharper
     framing, a missing constraint → fold into `## Approach` or a new
     `## Alternatives considered` subsection.
   - **Genuine forks** — the critic proposes a materially different approach the
     designer can't dismiss → add as a load-bearing `## Open Questions` entry:
     the critic's alternative plus the designer's recommendation.

**Advisory-that-can-promote.** The critique never blocks the merge by itself —
but a fork it promotes to a load-bearing Open Question blocks via the existing
pre-freeze gate (**No merge with open questions** below): the critic is a
*source* of load-bearing open questions, not a second gate with its own veto. The
critic runs only when a record is authored — the fast path (trivial change → no
artifact) skips it entirely.

## One pass, one artifact

Write a single design record **into the repo** under `docs/designs/<domain>/`
(`<domain>` = the repo's design-doc bucket for the change's concern — match the
buckets the repo already uses, never a fixed list) — either a flat
`<record>.md` (a short kebab slug) or a `<record>/design.md` directory when the
design owns supporting files, matching the layout its sibling records already
use. **You pick that exact path and pass it in the subagent's brief** — it is the
same path you poll for liveness, so the caller owns it, never the subagent. It is
a committed file that ships as a PR (see **Ship it as a reviewed PR**), never a
local scratch artifact. Four short sections:

- **Problem / Intent** — what and why, 1-3 sentences.
- **Approach** — the chosen approach; list alternatives only when the choice
  isn't obvious (one recommended plus why).
- **Alternatives considered** *(optional)* — when the Approach line above isn't
  enough, or the critique surfaced a weighed alternative worth recording, give
  each its own subsection: what it was, why it lost. Omit when the choice is
  obvious.
- **Plan** — decomposed tasks (see Plan discipline).
- **Tasks** — a checklist mirroring the plan.

**Flush-early — skeleton first, then edit.** The subagent `write`s the target
path with the four section headers above plus an empty `## Open Questions`
**before** its deep recon, then `edit`s it section by section. The record is a
valid, growing file on disk from the first minute — never composed in-context and
held to a single terminal `write` at the end. That deferred-write shape is what
wedges the design pass: a long recon-and-compose pass over-iterates in context
and the one big `write` never lands, so the run can reach its end with zero bytes
on disk. Skeleton-first makes a zero-progress run structurally impossible, and
you can poll that same brief-given path and watch it grow.

## Clarifications: batched, asked directly

The `design` subagent is headless — it cannot prompt the human. So it batches
**all** open questions and assumptions into an **Open Questions** section of the
record (and its returned summary), designing against a stated assumption rather
than stalling. **You then ask the operator directly on your home channel in a
single batched post** — every question with a recommendation — never a
one-question-per-turn loop, and never routed through a parent Manager (that
buries the decision in a coordination stream). The operator answers once; the
design is updated and frozen. Forks promoted by the **Adversarial critique** ride
this same single post — the critic adds no separate human round-trip.

## No merge with open questions (the pre-freeze gate)

The merge freezes the record, and executing agents build against it as the
contract — so an unresolved question in a merged record is an ambiguous contract.
Screen the **Open Questions** section against one bar before the PR can merge:

- **Load-bearing** — an executor building against the frozen record would hit
  real ambiguity (which API, which value, which of two designs). It **blocks the
  merge**. Ask the operator directly (batched, see above), get the answer, and
  **fold it into the record as a Decision** — replace the question with the
  decided outcome — *before* the merge-freeze.
- **Non-load-bearing** — explicitly marked as such, deferred with a rationale
  (the design is correct without it; it's an optional later optimization). This
  is a **documented deferral, not a blocker**: the record may merge with it, and
  the merge ratifies the deferral.

So at merge the `## Open Questions` section holds **only** explicit
non-load-bearing deferrals — never a live load-bearing question. If folding in
the last load-bearing answer leaves it empty, **delete the heading** — a frozen
record never carries an empty or stale `## Open Questions` beside its decided
outcomes. It is a **pre-merge staging area, not part of the frozen contract**.

**There is no folding-in after merge.** The merged record is frozen — a later
change ADDS a new record, never rewrites the merged one — so a load-bearing
question that reaches merge unresolved cannot be patched in place afterward.
Resolve it before the freeze, or defer it explicitly.

## Ship it as a reviewed PR

The design record is reviewed **on a pull request**, not in a local buffer —
that's what lets the operator **and** the review agent read the design before any
implementation exists. Once the subagent has drafted the record into the repo:

- **Its own branch/PR, separate from the implementation** — commit the record on
  its own so the design is reviewed as pure design with zero code noise. Follow
  the jj-colocated, stacked-PR workflow (`skill://jj`).
- **Open the PR, then drive `skill://review`** — run the review, triage
  findings, iterate. The same `CI` check gates the design PR as any other.
- **The merge is the freeze.** The design PR merging to `main` is what freezes
  the contract; execution starts from the merged record. The operator merges —
  you never do.

No mandatory self-review loops, no per-section approval gates, no sub-skill chain
— the single PR review is the whole gate (the subagent drafts, the review agent
plus operator review, the merge freezes, execution runs against it).

## Freeze, file, dispatch (the closing gate)

The merge freezes the record, but the design task is not done until its plan is
tracked work. Before closing the issue that produced the design record, the owner
MUST, in order:

1. **File the impl issues** — turn the record's `## Tasks` into concrete tracker
   issues, one per right-sized task or lane. Each carries: `Owner:` (the
   executing lane), the record path plus the task's `Interfaces:`/scope, and the
   dependency order. Parent them under the producing issue so they're visible.
   [TODO SEA-1734] name the concrete issue-filing tool once it lands; until then,
   file on the operator-provisioned tracker surface the wave uses.
2. **Dispatch them** — hand each filed issue to its owning lane (post to the
   owner on the coordination channel). A filed-but-orphaned issue is not
   dispatched.
3. **Then close the design issue** — done only once the impl issues exist and are
   dispatched. A frozen record with no filed work is an unfinished design task,
   exactly like a merged PR that only half-satisfies its issue
   (`rule://own-your-issue`, merged is not done).

This is the design-layer analogue of `rule://pre-finish-checks`. A record that
legitimately owes no code (a ledger-only doc, an amendment) closes with a
one-line note saying so — the gate is "filed or explicitly none," never silently
skipped.

## Plan discipline (the one hard requirement)

The plan/tasks MUST carry — this is what makes the artifact a clean,
cheap-to-execute contract:

- **Right-sized tasks** — the smallest unit that carries its own test cycle and
  is worth a fresh reviewer's gate.
- **A `## Global Constraints` header** — version floors, naming/copy rules,
  platform requirements — so every task brief inherits them (never buried in
  prose, where they get missed).
- **Per-task `Interfaces:`** — what it consumes/produces, with exact signatures,
  so the executor doesn't burn calls re-discovering context.
- **No placeholders** — every task is concrete and complete.
- *(optional)* a per-task model-tier hint.
