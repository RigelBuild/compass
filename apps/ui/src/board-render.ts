// Shared render derivations over the canonical Issue/PullRequest types (DL-069).
// One home for the four total functions the card, the Done view, and the
// right-sidebar PR pane must all agree on, so the surfaces can never drift.

import type { AgentAttribution, Check, Issue, PullRequest } from "./stub-data";

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

/** The agent-attribution label for a card/PR-pane (DL-068). Attribution is
 *  UNTRUSTED display metadata parsed from the owner header — it is rendered as
 *  a hedged CLAIM unless the server-set `verified` bit is true, and is NEVER
 *  derived into `assignee` or fed to a routing/selection decision. The three
 *  wordings are frozen (pinned verbatim in the design record with a golden
 *  test): the verified form is the unverified form minus the leading
 *  `claims to be ` hedge, and the no-agent form is the bare forge login. One
 *  total function so the card, the PR pane, and the test share one wording. */
export function attributionLabel(
	agent: AgentAttribution | undefined,
	forgeAccount: string,
): string {
	if (!agent) return `${forgeAccount} (not a Compass agent)`;
	const claim = `@${agent.agentHandle} (Compass agent, owned by @${agent.ownerHandle})`;
	return agent.verified ? claim : `claims to be ${claim}`;
}
