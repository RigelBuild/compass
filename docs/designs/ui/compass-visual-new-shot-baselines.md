# Visual regression: first baselines for new shots

Tracker: RIG-4421.

Ledger: this record's PR appends DL-399 to `docs/designs/DECISIONS.md` and
flips DL-341 to `Superseded by DL-399` in the same diff.

## Problem

The [visual regression gate](compass-visual-regression-gate/design.md#global-constraints)
(DL-341) commits every baseline PNG from the `regen-visual-baselines` lane
only. That lane cannot produce the first baseline for a new shot:

- The job runs only when `github.ref == 'refs/heads/main'`. The condition
  exists so an arbitrary branch's copy of the workflow cannot mint the App
  token that pushes and opens PRs.
- A shot with no committed baseline fails the `visual-gate` check. So a new
  shot cannot merge first and get its baseline from a regen run on `main`
  afterwards without `main` going red.

PRs #1552 and #1596 already committed local captures for this reason, and
CI's gate re-rendered both green.

## Approach

A new shot's **first** baseline may be a local capture:

- Capture under the repo's pinned dev shell (`direnv exec`), which pins the
  same nix Chromium and fontconfig as CI. Under those pins a dev-box capture
  is byte-identical to CI's (measured, RIG-3929).
- CI's `visual-gate` must re-render the shot green against the committed PNG
  on the PR's own head.
- The PR body states that the PNG is a local first capture and names the
  shot.

Every other baseline change stays regen-lane only: a refresh of an existing
shot is still a `regen-visual-baselines` bot PR. The rest of DL-341
(thresholds, hard gate, image-diff adjudication) is unchanged.

## Alternatives considered

### Allow visual regen on feature branches

Drop the `main` condition for the visual lane, or allowlist `compass-*/`
branches. This would restore the bot PR as the only baseline source. It lost
because any branch with write access could then run its own copy of the
workflow and mint the App token.

### Land a new shot in two PRs

Land the shot as `test.fixme`, run regen on `main`, then remove the `fixme`.
It lost because the first PR's shot is inert, which rule no-inert-gating
bans.

## Plan

One task: RIG-4088's markdown-message shot (#1638) uses this path. No code or
workflow change is needed. The stale `ci.yml` comments that say the bot PR
targets the dispatching feature branch are corrected in this PR.

## Tasks

- [ ] T1 — #1638 lands its `topic-markdown.png` first capture under this rule
  (Owner: compass-ui).
