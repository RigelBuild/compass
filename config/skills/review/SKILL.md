---
name: review
description: "Spawn a single review subagent on a PR diff, aggregate its findings, auto-fix the mechanical and surface the judgment, and loop to an all-clear. The sole code review on every PR — one reviewer, every lens, nothing behind it."
---

# Code review

Run on **every PR you own** — the review loop is mandatory before a PR is
merge-ready. After you push (or mark a PR ready), you as the driving Manager run
a single `review` subagent over the diff with the `task` tool. It is **the**
review: one adversarial reviewer that reads the whole diff in one context against
every lens, with no second reviewer and no external backstop behind it. You
aggregate its findings, fix the mechanical, surface the judgment, and loop until
an all-clear.

Because it is the only reviewer, its **negative** result carries no more weight
than the lenses it ran: a clean pass means the reviewer found nothing at or above
the floor, never that the diff is safe. It reviews for *readiness*, and the merge
button is never gated by it — that stays the `CI` check green plus the human
merge. What the review gates is the **hand-off**: an open finding at or above the
`high`+`medium` floor is a hard block. Do not present the PR as ready; say
plainly that the review is blocking and on what. The operator's merge is the only
override, and it is the operator's to invoke.

## The loop

1. **Fetch the diff.** Resolve the PR's diff and touched-file list. Compass
   product CI and PRs live on GitHub, so the diff is the PR's GitHub diff.
   [TODO RIG-1734] issue/PR tools land pre-Dogfood; until then resolve the diff
   through the operator-provisioned GitHub surface the wave already uses.
   Both the diff and the touched-file list go to the reviewer in its spawn brief.
2. **Spawn the `review` subagent — one call per round.** Use the `task` tool,
   one subagent (`agent: review`), handing it the diff plus the touched-file
   list. The subagent holds every lens at once and scales its own depth to the
   diff shape: a docs- or formatting-only diff gets a cheap style/docs pass, but
   a lockfile-only diff and any diff touching `docs/designs/**` are explicitly
   NOT trivial, and a code diff touching auth or input handling gets the security
   lens at full depth. One reviewer, one round is one subagent latency.
3. **Collect findings.** The subagent returns structured findings in the shape
   `{ severity, location, finding, suggested_fix }` as its result — directly to
   you. That single result channel is the aggregation point.
4. **Aggregate and act** — the driver's autonomy boundary:
   - **Auto-fix** clear, mechanical findings (a guard, a rename, a missing test,
     a dead-code removal) as a new commit on the PR tip. Delegate the fix work
     to an `implementer` subagent, then re-review.
   - **Surface judgment calls** — a design fork, a contract change, a
     disagreement with a finding, anything security-sensitive, and every design
     or file-structure finding (structural, not mechanical) — to the operator on
     your home channel via one batched post. Never free-hand a decision the
     human owns.
5. **Re-review loop.** After a fix commit, re-spawn the `review` subagent on the
   **new** diff. Repeat until the **all-clear criterion** below, then continue to
   step 6.
6. **CI, then hand off.** When the `CI` check is red, read the failing task from
   the check's log — the CI job runs one battery and names the failing target in
   its output. Triage any human PR threads; on any bounce the owner re-triages
   and holds the lane (`rule://hold-your-lane`). Then the PR is merge-ready and
   the operator does the final review plus merge.

This is the only review the PR gets; there is no bot pass behind it.

## All-clear termination (the loop control)

The load-bearing guard against both an infinite nit-loop and a rubber-stamp. It
guards the driver's re-review loop however many findings a round produces.

- **Severity floor = `high` + `medium`.** A round is all-clear when it returns
  **zero findings at or above the floor**, on a diff the previous round's fixes
  produced. `low`/nit findings are reported, not gating.
- **Iteration bound K = 3.** Hitting K with only sub-floor findings is all-clear
  (report the nits). Hitting K with a **at-or-above-floor finding still open** →
  **stop and surface to the operator** with the open finding; never loop past K.
- **No re-litigation.** A finding at a location an earlier round already
  addressed, re-raised without new evidence, is dropped — it does not restart the
  loop.
- **No rubber-stamp.** A `review` subagent that returns **zero findings on the
  first round** of a non-trivial diff is re-prompted once ("you found nothing —
  justify against the diff"); a justified clean pass then counts, an unjustified
  one is a spawn defect (re-spawn).

## Disposition — every finding, before hand-off

**Every finding is dispositioned before you hand the PR to the operator: fixed,
or explicitly deferred with a filed follow-up issue.** This is broader than the
severity floor — at all-clear a `low` finding can no longer be silently dropped;
it is fixed or filed. A deferral without an issue number is not a deferral, it is
a dropped finding.
[TODO RIG-1734] name the concrete issue-filing tool once it lands; until then,
file the follow-up on the operator-provisioned tracker surface the wave uses.

## Recall — the escaped-defect retro

With no external baseline, a recall regression is otherwise undetectable. Any
defect the operator or production finds post-merge is retro'd against that PR's
review findings — which lens should have caught it, and why it was missed. That
keeps the sole review a monitored posture rather than an article of faith.

## The sole review of record

- **This loop is the review of every PR.** One reviewer, every lens, no second
  opinion behind it. There is no advisory backstop — if this pass misses a
  defect, nothing else catches it, which is why the subagent scales depth rather
  than skipping lenses.
- **A clean pass means one thing: the reviewer found nothing at or above the
  floor.** It is not an approval and not a merge signal.
- **The merge button is the operator's.** The review produces findings; the
  merge gate stays the `CI` check green plus the human merge. An open finding at
  or above the floor is a hard block on the **hand-off**: the PR is not ready
  until that finding is fixed or deferred with a filed issue.

## Boundaries

- **Adversary is not author.** The `review` subagent finds; **you** (the driver)
  fix and re-spawn. The subagent never edits the branch — that keeps the review
  honest.
- **Findings come to the driver as the subagent result.** The subagent does not
  post to the PR; aggregation and any PR interaction is yours.
- **Never merge.** The operator merges every PR. You drive the loop to
  merge-ready and hand off.
