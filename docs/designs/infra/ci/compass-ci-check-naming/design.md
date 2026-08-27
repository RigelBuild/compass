# Compass CI check naming — the check-name convention, and suppressing the no-op `edited` phantom run

Status: Draft

> **Design record** for RIG-2791. Two decisions, ruled by Matt (2026-08-26):
> **A1** — a slash-namespaced kebab check-name convention for every CI job —
> and **B1** — a no-op PR body/title edit stops polluting the PR checks
> surface. Builds directly on the concern-matrix decomposition record
> (`compass-ci-job-decomposition/design.md`), which froze the current job
> graph this record renames. Evidence PR: #591.

## Problem / Intent

The concern-matrix split gave every CI leg its own named check, but the names
grew ad hoc — mixed casing, prose glosses in the name itself
(`pgtest (real-Postgres suites)`, `Moon battery (bun)`), no shared grammar
with the sibling workflows (`Renovate`, `Publish`, `deploy`). Worse, jj-vine
edits every PR's body ~1s after opening it, and ci.yml listens to
`pull_request: edited` (load-bearing for stacked-PR base re-points,
ci.yml:61–70), so every PR gets a second, phantom workflow run in which every
job self-skips via the shared no-op-`edited` guard (ci.yml:121–125). GitHub
posts a `skipped` check-run for every `if:`-skipped job, so the phantom run
double-lists every check on the PR — including a raw un-interpolated
`Moon battery (${{ matrix.group }})` (the matrix never expands when the `moon`
job skips, ci.yml:212) and a `skipped` `CI`. On #591 the real run
(32939436950, all green) and the phantom (32939617083, all skipped) both
posted their full check set.

Today the damage is cosmetic — the real `CI:success` posts *after* the phantom
`CI:skipped` because the battery takes ~10 minutes — but the latent risk is
real: branch protection reads the **latest** check-run per required context and
treats a skipped required check as passing, so a no-op `edited` firing after a
*red* real run would post `CI:skipped` as the latest `CI` and flip a genuine
red to mergeable. This record fixes the names (A) and closes both the noise
and the latent false-green (B).

## Approach

### A — the check-name convention (A1, ruled)

Check names adopt the slash-namespaced kebab grammar already present on this
repo's checks surface (`renovate/stability-days`): a check is
`<workflow-namespace> / <job>`, all kebab, with the prose gloss demoted to the
job's header comment (every gate job already carries one — verified per job in
ci.yml). The rename table, all in `.github/workflows/ci.yml` `name:` fields:

| Job id | Current check name | New check name |
| --- | --- | --- |
| `setup` | `Setup (concern matrix)` (ci.yml:113) | `ci / setup` |
| `moon` | `Moon battery (${{ matrix.group }})` (ci.yml:212) | `ci / moon (${{ matrix.group }})` → `ci / moon (bun)`, `(go)`, `(nix)`, `(forks)` |
| `pgtest` | `pgtest (real-Postgres suites)` (ci.yml:382) | `ci / pgtest` |
| `microvm` | `microvm (KVM boot suite)` (ci.yml:554) | `ci / microvm` |
| `forge-oracle` | `forge-oracle (live-contract oracle)` (ci.yml:780) | `ci / forge` |
| `gtk3-e2e` | `gtk3-e2e (multi-window native app gate)` (ci.yml:1035) | `ci / gtk3` |
| `dogfood-e2e` | `Dogfood e2e (deterministic full-stack tier)` (ci.yml:1223) | `ci / e2e` |
| `regen-forge-fixtures` | `Regenerate forge golden fixtures` (ci.yml:1782) | `ci / regen-fixtures` |
| `CI` | `CI` (ci.yml:1655) | **`CI` — unchanged** |

Load-bearing constraints the table honors:

- **`CI` stays `CI`.** It is the sole required check (ruleset 20090117,
  verified by the driver); renaming it would force a ruleset edit and a window
  where no required check matches. Keeping it means the whole rename touches
  zero branch-protection state.
- **Placeholder-leg name stability is preserved.** The moon check name comes
  from the workflow's `name:` interpolating `${{ matrix.group }}`, never from
  the generator: `tools/ci-matrix/index.ts` emits
  `MatrixEntry { group, run, targets }` and emits *every* group as a
  placeholder (`run: 'false'`) even when unaffected, precisely so the check
  names stay identical across PRs. The rename changes only the template text
  around the same interpolation — no generator change, and a docs PR still
  shows `ci / moon (go)` as a fast no-op.
- **The raw-template display is not fully fixable by naming.** When the `moon`
  job itself skips (the matrix never expands), GitHub renders the literal
  template. Decision B removes the dominant path (the no-op `edited` run); the
  residual paths are mechanism-dependent (OQ1): under dispatch-mode the only
  remaining path is the operator-only `workflow_dispatch` regen run, whose
  skipped gate-job contexts attach to the dispatched ref, not a PR; under the
  label-bounce variant, any non-sentinel `labeled` event on a PR also renders
  it — including every Renovate PR (`tools/renovate/config.json5:60` labels
  each `dependencies`) unless that config drops its `labels:`.
- **`ci / e2e` absorbs RIG-2739** ("rename dogfood-e2e → e2e"): the convention
  supersedes that issue's naming slice. The job *id* `dogfood-e2e` may rename
  to `e2e` in the same pass (ids are not check names; the rollup's `needs:`
  and `needs.dogfood-e2e.result` reads move with it).
- **Sibling workflows normalize under the minimal rule**: a multi-job workflow
  namespaces as `<stem> / <job>`; a single-job workflow's one check is the bare
  kebab workflow stem. So `renovate.yml`'s job `renovate` (no display name)
  gains `name: renovate`; `publish-agent-image.yml`'s `Publish` →
  `name: publish-agent-image`; `eng-docs-deploy.yml`'s `deploy` →
  `name: eng-docs-deploy`. None is a required check, so all rename freely.

**Verify on #591 before freeze:** the `renovate/stability-days` precedent is
a commit *status* context, not an Actions check-run — a different rendering
class, so it does not prove how the merge box renders these names. GitHub may
render Actions checks as `<workflow name> / <job name>`, in which case, with
the workflow still named `CI`, `ci / setup` would display as
`CI / ci / setup`. Before the rename freezes, push one rename to #591 and
read the actual checks surface; if the workflow-name prefix renders, A4
(Alternatives) is the contingency.

### B — suppressing the no-op `edited` phantom (B1 goal ruled; mechanism open — OQ1)

The platform fact that shapes everything: **a triggered workflow run cannot
post zero check contexts.** GitHub Actions has no workflow-level `if:`; a
job-level `if:` skip still posts a `skipped` check-run, and a guard job that
every other job `needs:` still leaves each downstream job posting `skipped`.
So while `edited` remains in ci.yml's `types:`, a no-op body edit necessarily
re-posts the full check set — including the `skipped` `CI` that carries the
latent false-green. Reaching (near-)zero pollution therefore requires moving
`edited` **out of ci.yml** without losing the base-re-point coverage it was
added for (ci.yml:61–70: a stacked PR whose base flips fires *only*
`pull_request.edited`; the pre-`edited` failure mode was a frozen stale merge
SHA that only close+reopen escaped).

**Forced shape — split-trigger:** drop `edited` from ci.yml's `types:` and
add a tiny dedicated workflow, `.github/workflows/pr-base-repoint.yml`, on
`pull_request: types: [edited]`, with a single job (`repoint / guard`) whose
`if:` is `github.event.changes.base != null`:

- **No-op body/title edit** (the jj-vine per-PR case): ci.yml never runs — the
  event is not in its `types:`. The only artifact is one `skipped` context
  from the guard job, which is not required and not named `CI`. Pollution
  drops from ~9 duplicate contexts (incl. the raw moon template and a
  `skipped` `CI`) to one, and the required `CI` context is never re-posted by
  the no-op-edit path — that latent false-green path is closed under every
  mechanism below.
- **Real base re-point**: the guard must re-trigger a full, fresh-merge-SHA
  ci.yml run. *How* it does so is the load-bearing open fork in B, governed
  by a platform rule: **events created with the workflow's default
  `GITHUB_TOKEN` do not trigger new workflow runs** (sole exceptions:
  `workflow_dispatch` and `repository_dispatch`). That rule credential-costs
  every candidate — see OQ1 (load-bearing; Matt rules) for the three options
  and the driver's updated recommendation (dispatch-mode merge-ref checkout).

Mechanism-dependent consequences in ci.yml once `edited` leaves its `types:`
(the dispatch variant leaves `types:` at `[opened, synchronize, reopened]`
and adds dispatch arms instead — OQ1):

- Under label-bounce, the shared no-op-`edited` guard on setup/moon/pgtest/
  microvm/forge-oracle/gtk3-e2e/dogfood-e2e and the `CI` rollup rewrites from
  the `edited`/`changes.base` test to the sentinel-label test (same shape, one
  arm:
  `github.event.action != 'labeled' || github.event.label.name == 'ci-base-repoint'`).
  But because Renovate labels every dependency PR
  (`tools/renovate/config.json5:60` sets `labels: ["dependencies"]`, applied
  via a GitHub App token whose events *do* trigger workflows,
  `.github/workflows/renovate.yml:111-113`), `labeled` in `types:` creates a
  fresh skipped phantom run on every Renovate PR unless Renovate's `labels:`
  is dropped; the rollup's guard keeps `CI` skipped (not red) on those,
  exactly as today (ci.yml:1690–1697). Under dispatch-mode no such phantom
  exists, and the eight guard arms simplify instead of rewriting.
- The concurrency key (ci.yml:104–106) keeps `github.event.action` — the
  `opened` vs re-trigger separation is as load-bearing as the old `opened` vs
  `edited` one (the re-trigger run must not cancel a still-running `opened`
  run).
- The `!cancelled()` rollup semantics (ci.yml:1662–1685) are untouched under
  either mechanism: the rollup's three-part `if:` keeps its shape, only the
  guard arm's predicate changes.

## Alternatives considered

### A2 — keep prose names, normalize casing only

Leaves `Moon battery (bun)` and friends as-is with consistent capitalization.
Rejected: the checks surface stays unsorted (no shared prefix groups the CI
legs together in GitHub's alphabetical check list), and the gloss-in-name
duplication with the job header comments persists.

### A3 — rename the rollup too (`ci / rollup` or `ci`)

A fully uniform grammar including the required check. Rejected: it forces an
edit to ruleset 20090117 and a cutover window where in-flight PRs' runs post
the old name against a ruleset requiring the new one (or vice versa) — real
merge-gate risk purchased for symmetry. `CI` as the one un-namespaced name is
self-describing.

### A4 — rename the workflow itself (`name: CI` → `ci`), bare kebab job names

If the #591 verification (§A) shows the merge box rendering Actions checks as
`<workflow name> / <job name>`, A1's embedded prefix double-renders
(`CI / ci / setup`). A4 instead renames the *workflow* `name: CI` → `ci` and
gives jobs bare kebab names (`setup`, `moon (${{ matrix.group }})`,
`pgtest`, ...) — yielding `ci / setup` natively. Required-check contexts bind
to the check-run (job) name, so the rollup job's name stays `CI` and ruleset
20090117 is still untouched. Not weighed in the first draft; it may dominate
A1 if the prefix renders. The ruled naming stays A1 unless the #591 evidence
forces A4 — recorded as the contingency, not a flip of the ruled decision.

### B — rejected mechanisms

- **Narrow the existing per-job `if:` further (keep `edited` in `types:`).**
  Cannot work: a skipped job still posts a `skipped` context, so the phantom
  check set — and the `skipped` `CI` latent false-green — survives any guard
  refinement. This is the shape shipped today and the shape B1 exists to
  replace.
- **Workflow-level guard job every other job `needs:`.** Same platform fact:
  `needs`-skipped jobs still post `skipped` contexts. Adds a job, removes
  nothing.
- **Concurrency/dedup — let the phantom cancel or be cancelled.** The
  concurrency key *deliberately* separates event actions (ci.yml:88–95): a
  shared group would let jj-vine's ~1s-later `edited` cancel the real `opened`
  run, ending the required check cancelled+skipped — a false green. Cancelled
  contexts also interact with the `!cancelled()` rollup arm. Rejected.
- **Mirror-rollup** — keep `edited`, but on the no-op case the rollup runs and
  mirrors (via the checks API) the latest real `CI` conclusion for the SHA, so
  the latest `CI` context always equals the real verdict. It is the only
  alternative that closes the false-green for *all* trigger shapes at once
  (not per-event) and does so with the smallest diff, but every cosmetic
  artifact (duplicate check set, raw moon template name) survives — it fails
  B1's actual, ruled pollution goal — and it grants the rollup
  `checks: write` (creating/updating a check run needs write) plus a polling
  dependency on its own run history. Rejected.
- **Drop `edited` with no replacement.** Regresses the documented stacked-PR
  base-re-point fix (ci.yml:61–70): a base flip would again leave the PR
  gated by a frozen stale merge SHA with close+reopen the only escape. jj-vine
  stacks make this a common event, not an edge. Rejected outright.
- **B2 (make the skipped contexts cosmetically clean but keep the run).** Not
  picked by Matt; strictly dominated by the split-trigger, which removes the
  run rather than beautifying it. Noted only for provenance.

## Global Constraints

- **Public repo.** The record and every artifact it plans obey
  `docs/designs/CONTRIBUTING.md` (bare tracker IDs, no private links).
- **`CI` keeps its name.** The required-check context in ruleset 20090117 is
  never edited; every other check renames freely.
- **Placeholder-leg check-name stability.** Every concern group's check name
  appears on every PR regardless of the affected set — the rename must not
  make names conditional on expansion.
- **Base-re-point coverage is load-bearing.** Whatever replaces `edited` in
  ci.yml MUST produce a fresh-merge-SHA full run when a PR's base flips
  (jj-vine stacks), with no manual step.
- **Rollup semantics frozen.** `!cancelled()` (never `success()`/`always()`),
  the paired-flag skip excusal, and the fail-red-on-empty-flag posture
  (ci.yml:1655–1707) are untouched by both decisions.
- **Concurrency separation by event action** stays (a re-trigger or `edited`
  run must never cancel the real run, under any OQ1 mechanism).
- **RIG-2739 is absorbed** by the `ci / e2e` rename; it does not proceed
  separately.

## Plan

### T1 — rename the ci.yml gate checks (Decision A)

Rename every job `name:` in `.github/workflows/ci.yml` per the table in
§Approach; rename the `dogfood-e2e` job id to `e2e` and update the rollup's
`needs:` list and `needs.dogfood-e2e.result` read (ci.yml:1701,
`dogfood='${{ needs.dogfood-e2e.result }}'`) to match (the job-id blast
radius is grep-verified contained to ci.yml plus prose docs). Sweep the *live*
surfaces for literal references to the old check names —
`app-bundle/SMOKE.md`, `docs/architecture`, eng-docs, tooling — and update
them; explicitly exclude `docs/designs/` frozen historical records (corpus
convention: add by citation, never rewrite frozen prose). Confirm each
dropped prose gloss survives in the job's header comment (all eight already
do; keep them).

- **Interfaces:** consumes `.github/workflows/ci.yml` (nine `name:` fields, the
  `CI` job's `needs:` + result reads); produces the renamed workflow. No change
  to `tools/ci-matrix/index.ts` (`MatrixEntry.group` values `bun`/`go`/`nix`/
  `forks` are unchanged; the name template interpolates them as before).

### T2 — normalize the sibling workflow check names (Decision A)

Apply the single-job rule: `renovate.yml` job `renovate` gains
`name: renovate`; `publish-agent-image.yml` job `publish` `name: Publish` →
`name: publish-agent-image`; `eng-docs-deploy.yml` job `deploy` gains
`name: eng-docs-deploy`. None is required; no ruleset interaction.

- **Interfaces:** consumes/produces `.github/workflows/renovate.yml`,
  `.github/workflows/publish-agent-image.yml`,
  `.github/workflows/eng-docs-deploy.yml` (job `name:` fields only).

### T3 — the repoint re-trigger workflow (Decision B; finalized against OQ1)

T3 and T4 are mechanism-dependent; their final shape follows OQ1's
resolution. Under the label-bounce variant: add
`.github/workflows/pr-base-repoint.yml` (`on: pull_request: types: [edited]`,
one job `name: repoint / guard`, `if: github.event.changes.base != null`)
whose steps (a) mint the GitHub App installation token the bounce requires
(`actions/create-github-app-token`, precedent
`.github/workflows/renovate.yml:111-113`) — a label added with the default
`GITHUB_TOKEN` does not trigger ci.yml's `labeled` event — gated to
same-repo PRs only (a `pull_request`-triggered workflow minting a write
token must never run against fork PRs); (b) create the sentinel label
`ci-base-repoint` if missing (`gh pr edit --add-label` fails on a nonexistent
label, and a human deleting the label would otherwise break the bounce
permanently); (c) add then remove the label; and (d) actively verify a fresh
ci.yml run started — poll the runs API and fail the guard red if none
appears within a window ("loud to a log" is not loud to the gate; without
this the PR keeps showing green stale checks). Idempotent under retry. Under
the dispatch-mode variant, T3 is instead the guard dispatching ci.yml with
the PR number (the default token suffices — `workflow_dispatch` is exempt
from the no-trigger rule), plus the same fresh-run verification.

- **Interfaces:** produces `.github/workflows/pr-base-repoint.yml`; consumes
  the GitHub labels/runs APIs (App token under label-bounce; default
  `GITHUB_TOKEN` under dispatch-mode); produces the event T4's ci.yml
  consumes. Depends on T4 landing in the same PR (the two halves are one
  atomic cutover — neither is correct alone).

### T4 — cut ci.yml over from `edited` (Decision B; finalized against OQ1)

Common to both mechanisms: `edited` leaves ci.yml's `types:`, and the header
comment block (ci.yml:61–70, 88–103) documents the split-trigger design in
place of the `edited` rationale. Under label-bounce: `types:` gains
`labeled`, and the shared guard arm at the eight `edited`-guard sites
(ci.yml:124, 231, 396, 568, 794, 1048, 1263, 1703 — setup, moon, pgtest,
microvm, forge-oracle, gtk3-e2e, dogfood-e2e/e2e, CI) rewrites from
`github.event.action != 'edited' || github.event.changes.base != null` to
`github.event.action != 'labeled' || github.event.label.name ==
'ci-base-repoint'`; Renovate's `labels: ["dependencies"]`
(`tools/renovate/config.json5:60`) is dropped, or the every-Renovate-PR
skipped phantom is explicitly accepted. Under dispatch-mode: `types:`
becomes `[opened, synchronize, reopened]`; ci.yml gains a `workflow_dispatch`
input carrying the PR number; every currently
`github.event_name == 'pull_request'`-gated path (setup, forge-oracle,
dogfood-e2e gates; the affected query's base ref) grows a dispatch arm
checking out `refs/pull/N/merge`; and the new dispatch lane must not collide
with the existing dispatch-only regen lane's `if:` gating. The concurrency
key and the rollup's `!cancelled()` structure are unchanged either way.

- **Interfaces:** consumes/produces `.github/workflows/ci.yml` (the `on:`
  block, the eight guard sites, two comment blocks). Same-PR atomic partner
  of T3. No `tools/ci-matrix/index.ts` change (the generator is
  event-agnostic; setup still runs on every gating event).

### T5 — validate on a live stack

On the PR carrying T1–T4 (and a stacked child if available): confirm (1) the
checks surface shows exactly the new names once — including that `ci / setup`
renders without a `CI /` workflow-name prefix (the §A verify-before-freeze
check; if it double-prefixes, A4 is the contingency), (2) a body edit
produces only the single `repoint / guard` skipped context and no new ci.yml
run, (3) a real base re-point (merge the parent PR under a stacked child)
produces the OQ1-resolved re-trigger and a fresh full run against the new
merge SHA, (4) `CI` remains the required context and gates as before.

- **Interfaces:** consumes the live PR checks surface and Actions run list;
  produces evidence on the PR (run links) — no repo files.

### Task sizing note

T1+T2 are one mechanical rename slice; T3+T4 are one atomic behavioral slice;
T5 is the evidence gate. Ship as one PR (the rename and the cutover touch the
same `name:`/`if:` lines; splitting them buys review granularity nothing).

## Tasks

- [ ] T1 — rename the ci.yml gate checks per the A1 table; `dogfood-e2e` job id → `e2e`; sweep old-name references on live surfaces (excl. `docs/designs/`)
- [ ] T2 — normalize `renovate` / `publish-agent-image` / `eng-docs-deploy` job names
- [ ] T3 — add `pr-base-repoint.yml` (guard on `changes.base`; re-trigger per OQ1, with create-label-if-missing + active fresh-run verification under label-bounce)
- [ ] T4 — ci.yml drops `edited` from `types:`; mechanism-dependent cutover per OQ1 (eight label guard arms + Renovate `labels:` call, or dispatch arms + merge-ref checkout)
- [ ] T5 — live validation: names once (no double prefix), no-op edit ⇒ one guard context, base re-point ⇒ fresh full run

## Ledger delta

No existing DECISIONS.md row links either sibling record
(`compass-ci-job-decomposition`, `compass-pr-validation`) — verified by
grepping the ledger for `](infra/` (zero matches) — so **no sibling-row Status
flips**. The ledger has no infra/CI section (the nearest CI row, DL-263 darwin
CI cadence, lives under Desktop shell), so these rows introduce a new
`## Infrastructure & CI` section heading. These ids are provisional: several
design PRs append to the ledger concurrently, so the id is reconciled to the
next free pair at merge time (this record sits behind OQ1's ruling, so it
merges last). Proposed rows (the driver applies these to
`docs/designs/DECISIONS.md` in the same PR):

| ID | Decision | Status | Record |
| --- | --- | --- | --- |
| DL-269 | CI check names adopt the slash-namespaced kebab convention `ci / <job>` (`ci / setup`, `ci / moon (<group>)`, `ci / pgtest`, `ci / microvm`, `ci / forge`, `ci / gtk3`, `ci / e2e` — absorbing RIG-2739 — `ci / regen-fixtures`); the required rollup stays `CI` so ruleset 20090117 is never edited; prose glosses live in job header comments, not names; sibling single-job workflows take the bare kebab workflow stem | Active (Matt, 2026-08-26) | [CI check naming §A](infra/ci/compass-ci-check-naming/design.md#a--the-check-name-convention-a1-ruled) |
| DL-270 | The no-op `pull_request.edited` phantom run is suppressed by split-trigger: `edited` leaves ci.yml's `types:` for a dedicated `pr-base-repoint.yml` guard workflow (re-trigger mechanism per OQ1) — a body/title edit posts one non-required skipped context instead of the full check set, so the every-PR jj-vine `edited` phantom is removed and the latent skipped-required-as-passing false green is closed for that path (fully under dispatch-mode; with a label-event residual under the label-bounce variant unless Renovate's `labels:` is dropped) | Active (Matt, 2026-08-26) | [CI check naming §B](infra/ci/compass-ci-check-naming/design.md#b--suppressing-the-no-op-edited-phantom-b1-goal-ruled-mechanism-open--oq1) |

## Open Questions

### OQ1 (load-bearing — Matt rules) — the B1 re-trigger mechanism for a real base re-point

Matt ruled B1's *goal* (a no-op edit stops polluting the checks surface); the
grounding established zero-contexts is impossible while `edited` stays in
ci.yml's `types:`, so the split-trigger shape is forced. The genuine fork is
how the repoint guard re-triggers a real run, since a plain workflow re-run
replays the frozen stale merge SHA (the exact failure `edited` was added to
fix). Every option is governed by the platform rule that events created with
the workflow's default `GITHUB_TOKEN` do not trigger new workflow runs (sole
exceptions: `workflow_dispatch` and `repository_dispatch`):

1. **Sentinel-label bounce.** Guard adds+removes the `ci-base-repoint` label;
   ci.yml adds `labeled` to `types:` gated to that label. Any fresh
   `pull_request` event recomputes the merge SHA, so the run tests against
   the new base. **Credential: requires a minted GitHub App token (or PAT)**
   — a label added with the default `GITHUB_TOKEN` does not trigger ci.yml's
   `labeled` event. Precedent exists: `renovate.yml` mints one via
   `actions/create-github-app-token` (`.github/workflows/renovate.yml:111-113`).
   Costs: the minted credential and its fork-PR security posture (a
   `pull_request`-triggered workflow minting a write token — must be gated to
   same-repo PRs only); `labeled` in `types:` gives every Renovate PR a fresh
   skipped phantom run (Renovate labels each dependency PR `dependencies`,
   `tools/renovate/config.json5:60`, via an App token whose events *do*
   trigger workflows) unless Renovate's `labels:` is dropped; eight new guard
   arms; and an active "did a fresh run actually start" verification, without
   which the failure mode is silent (the PR keeps showing green stale checks).
2. **Automated close/reopen.** Broken by the same platform rule — a
   `reopened` event created via the default `GITHUB_TOKEN` also does not
   trigger ci.yml — so it needs the same minted credential as option 1 while
   adding *more* PR-state churn: close/reopen notifications on every stack
   advance, a transient `closed` state stack tooling may react to, and a
   fail-open risk (a crash between close and reopen leaves the PR closed).
   **Strictly dominated by option 1.**
3. **Dispatch-mode merge-ref checkout.** ci.yml gains a `workflow_dispatch`
   input carrying the PR number; every currently
   `github.event_name == 'pull_request'`-gated path (setup, forge-oracle,
   dogfood-e2e gates; the affected query's base ref) grows a dispatch arm
   checking out `refs/pull/N/merge`, and the new lane must not collide with
   the existing dispatch-only regen lane's `if:` gating. **Credential: none**
   — `workflow_dispatch` is exempt from the no-trigger rule, so the default
   token suffices — and `labeled` never enters `types:`, so no Renovate
   phantom. Cost: the largest ci.yml diff by far.

Driver's updated recommendation: **option 3**. The two verified blockers —
the default-token no-trigger rule and Renovate labeling every dependency PR —
shifted the balance: dispatch-mode is the only mechanism that avoids both a
new credential/security-posture change and an every-Renovate-PR phantom, at
the cost of a bigger ci.yml diff. Option 1 + App token remains viable if Matt
prefers the smaller ci.yml diff and accepts minting a credential plus
dropping Renovate's `labels:`. Matt rules.

### Non-load-bearing deferrals

- The raw `ci / moon (${{ matrix.group }})` template still renders whenever
  the `moon` job skips (setup skips ⇒ matrix never expands): under
  dispatch-mode only on the operator-only regen run (those contexts attach to
  the dispatched ref, not a PR gate); under label-bounce also on non-sentinel
  `labeled` events on PRs, including every Renovate PR unless its `labels:`
  is dropped. Accepted residual per OQ1's resolution; revisit only if the
  skipped-run surface grows.
- Whether `renovate/stability-days`-style third-party check names should also
  be catalogued in a conventions doc under `docs/` — out of scope; this record
  governs names this repo emits.
