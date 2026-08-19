// Shared render derivations over the canonical Issue/PullRequest types (DL-069).
// One home for the total functions the card, the Done view, and the right-sidebar
// PR pane must all agree on, so those surfaces can never drift.
//
// One deliberate exception: `authorLabel` (below) is the PR pane's ARTIFACT-author
// label. The issue card foot does NOT share it — the card shows the issue's
// assignee (the agent currently on it), a different fact from the artifact's
// author, so a reassigned issue names its current holder on the card and its
// original author in the PR pane. That divergence is intended, not drift.

import type { PrLifecycle } from "./constants";
import type {
	AgentAttribution,
	Check,
	ChecksSummary,
	Issue,
	PullRequest,
	Review,
} from "./stub-data";

/** The primary PR the card / Done row / PR pane renders for an issue (DL-071).
 *  Total, precedence frozen: the first OPEN pr in `prs` order, else the first
 *  MERGED, else the last element. Open-ness comes from `forgeState` (no PR
 *  timestamp exists on the wire, so selection is by open-ness and `prs`
 *  ordering, never PR number — numbers are per-repo and incomparable across a
 *  multi-forge `prs`). An empty `prs` → no PR (no chip). */
export function primaryPr(issue: Issue): PullRequest | undefined {
	const prs = issue.prs;
	if (prs.length === 0) return undefined;
	return (
		prs.find((p) => p.forgeState === "open") ??
		prs.find((p) => p.forgeState === "merged") ??
		prs[prs.length - 1]
	);
}

/** The 3-valued pip class for a 6-valued forge check state (DL-071): one map,
 *  used at all three pip sites (card, Done row, RightSidebar CheckRuns). The
 *  pips iterate the per-check list, NOT the roll-up `checks.state`. */
export function checkPip(
	state: Check["state"],
): "success" | "failure" | "pending" {
	switch (state) {
		case "success":
			return "success";
		case "failure":
		case "cancelled":
			return "failure";
		default:
			return "pending";
	}
}

/** The PR state badge (DL-071): the canonical type splits the old 4-value field
 *  into `forgeState` (open|closed|merged) + `draft`, so the badge derives —
 *  draft-and-open renders `draft`, everything else renders `forgeState`. Applied
 *  at every former `pr().state` read. */
export function prBadge(
	pr: PullRequest,
): "draft" | "open" | "closed" | "merged" {
	return pr.draft && pr.forgeState === "open" ? "draft" : pr.forgeState;
}

/** The user-facing card key for a board item (DL-071). The tracker id in its
 *  native form when linked (e.g. `SEA-1042`), else the forge coordinate
 *  `${repo}#${number}` — host-qualified `${host}/${repo}#${number}` only when
 *  the board holds artifacts from more than one distinct ForgeRef, so two
 *  artifacts never collide on `repo` alone. Both forms always renderable, no
 *  null branch; the Compass-local `id` is never a display fallback. */
export function issueKey(issue: Issue, multiForge: boolean): string {
	if (issue.tracker) return issue.tracker.id;
	const coord = `${issue.repo}#${issue.number}`;
	return multiForge ? `${issue.forge.host}/${coord}` : coord;
}

/** Whether a board list spans more than one distinct ForgeRef (provider+host) —
 *  the disambiguation trigger for `issueKey`. */
export function isMultiForge(issues: readonly Issue[]): boolean {
	const seen = new Set<string>();
	for (const i of issues) {
		seen.add(`${i.forge.provider}:${i.forge.host}`);
		if (seen.size > 1) return true;
	}
	return false;
}

/** The author label for a card / PR pane: the boarded artifact's Compass agent
 *  as `@handle`. Only Compass artifacts from trusted agent accounts are boarded
 *  (Matt's 2026-07-31 ruling), so there is no untrusted owner-header spoof
 *  surface to hedge — the label is the bare handle, with no owner text and no
 *  verified/claims hedge. An agent's owner is a property of the agent account
 *  (AgentAccount.owner_user_id), never restated per artifact. */
export function authorLabel(agent: AgentAttribution): string {
	return `@${agent.agentHandle}`;
}

/** The one review verdict a board surface shows for a PR: latest-per-author over
 *  the submission-ordered `reviews` (a reviewer's CURRENT verdict is its last
 *  entry, DL-069; bots INCLUDED), rolled by precedence
 *  `changes_requested` > `approved` > `commented`. Empty `reviews` → `undefined`
 *  (no badge). This helper is the ONE owner of the `changes_requested`→`changes`
 *  copy rule, returning the display vocabulary so no chip site repeats it. */
export function reviewBadge(
	pr: PullRequest,
): "changes" | "approved" | "commented" | undefined {
	const latest = new Map<string, Review["verdict"]>();
	for (const r of pr.reviews) latest.set(r.author, r.verdict);
	const verdicts = [...latest.values()];
	if (verdicts.length === 0) return undefined;
	if (verdicts.includes("changes_requested")) return "changes";
	if (verdicts.includes("approved")) return "approved";
	return "commented";
}

/** The CI badge for a PR: the roll-up `ChecksSummary.state` read directly — it
 *  is already the 3-valued roll-up ("pending" | "success" | "failure"), so no
 *  new mapping is invented. Exported so every badge site reads the roll-up
 *  through one named seam (the inverse of `checkPip`, which iterates the
 *  per-check list). No `checks` → `undefined` (no badge). */
export function ciBadge(pr: PullRequest): ChecksSummary["state"] | undefined {
	return pr.checks?.state;
}

/** The PR-lifecycle column a PR sits in on the PRs board (design D1 / T6).
 *  Precedence: merged > ready > in_review > in_progress. Defined over the
 *  reviewBadge/ciBadge roll-ups so it inherits their latest-per-author rule.
 *  D1a: an approved + CI-green PR with an UNRESOLVED thread is NOT ready — it
 *  stays "in review". Total over board rows (forgeState !== "closed"); the
 *  defensive default keeps it total if the type widens. */
export function prLifecycle(pr: PullRequest): PrLifecycle {
	if (pr.forgeState === "merged") return "merged";
	const approvedGreen =
		reviewBadge(pr) === "approved" && ciBadge(pr) === "success";
	const allThreadsResolved = pr.threads.every((t) => t.resolved);
	if (approvedGreen && allThreadsResolved) return "ready";
	const hasOpenThreads = pr.threads.some((t) => !t.resolved);
	if (reviewBadge(pr) !== undefined || hasOpenThreads) return "in_review";
	return "in_progress"; // incl. draft-open + the defensive default
}

/** The open PRs of an issue — `forgeState === "open"` (drafts included), `prs`
 *  order preserved. The PRs-tab row source: unlike `primaryPr` (a card-level
 *  compression to one chip), this keeps every open PR so a second open PR is not
 *  invisible. */
export function openPrs(issue: Issue): PullRequest[] {
	return issue.prs.filter((p) => p.forgeState === "open");
}
