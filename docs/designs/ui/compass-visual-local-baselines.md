# Visual regression: local baseline captures

Tracker: RIG-4421 (new shots), RIG-4447 (refreshes).

Ledger: this record's PR appends DL-399 to `docs/designs/DECISIONS.md` and
flips DL-341 to `Superseded by DL-399` in the same diff.

## Problem

The [visual regression gate](compass-visual-regression-gate/design.md#global-constraints)
(DL-341) commits every baseline PNG from the `regen-visual-baselines` lane
only. That lane cannot produce a baseline for a branch's UI:

- The job runs only when `github.ref == 'refs/heads/main'`. The condition
  exists so an arbitrary branch's copy of the workflow cannot mint the App
  token that pushes and opens PRs. It therefore renders `main`'s UI, never a
  branch's.
- `visual-gate` is a required check. A branch that adds a shot, or that
  intentionally changes an existing one, is red until its baselines change,
  and it cannot merge red to get a regen run on `main` afterwards.

PRs #1552 and #1596 already committed local captures for this reason, and
CI's gate re-rendered both green.

## Approach

A branch may commit a locally captured baseline, for a new shot or a refresh
of an existing one:

- Capture under the repo's pinned dev shell (`direnv exec`), which pins the
  same nix Chromium and fontconfig as CI. Under those pins a dev-box capture
  is byte-identical to CI's (measured, RIG-3929).
- CI's `visual-gate` must re-render every committed shot green on the PR's
  own head.
- The PR body states that the PNGs are local captures and names each
  changed shot. Review adjudicates the change on GitHub's image diff of the
  committed PNGs (2-up, swipe, onion skin), as DL-341 set out.

The `regen-visual-baselines` lane stays as the operator path for refreshing
`main` itself, for example after a toolchain bump moves the renderer. The
rest of DL-341 is unchanged: Playwright `toHaveScreenshot` (not a hosted
service) as the `visual-gate` moon task, its thresholds, the hard gate, and
image-diff adjudication with the CI failure artifact kept only for an
unintended red.

## Alternatives considered

### Allow visual regen on feature branches

Drop the `main` condition for the visual lane, or allowlist `compass-*/`
branches. This would restore the bot PR as the only baseline source. It lost
because any branch with write access could then run its own copy of the
workflow and mint the App token.

### Main-authored regen of a PR revision

Workflow code from `main` checks out a named PR head, captures without the
App token, uploads the PNGs, and a second job holding the token opens a bot
PR against that branch. This restores bot provenance for every change. It
lost for now on cost: real workflow work, and a careful token split around
rendering PR code. It can be revisited if bot provenance is wanted back.

### Land a new shot in two PRs

Land the shot as `test.fixme`, run regen on `main`, then remove the `fixme`.
It lost because the first PR's shot is inert, which rule no-inert-gating
bans. It also does nothing for a refresh.

## Plan

No code or workflow change is needed; the rule is applied by review. This PR
corrects the stale `ci.yml` comments on the regen lane. RIG-4088's
markdown-message shot (#1638) is the first PR to use the rule.

## Tasks

- [ ] T1 — #1638 lands its `topic-markdown.png` first capture under this rule
  (Owner: compass-ui).
