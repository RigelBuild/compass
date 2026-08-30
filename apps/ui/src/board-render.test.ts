import { describe, expect, test } from "bun:test";
import {
	authorLabel,
	checkPip,
	ciBadge,
	isMultiForge,
	issueKey,
	openPrs,
	prBadge,
	primaryPr,
	prLifecycle,
	reviewBadge,
} from "./board-render";
import type {
	AgentAttribution,
	Check,
	ForgeRef,
	Issue,
	PullRequest,
	Review,
	ReviewThread,
} from "./stub-data";

const GITHUB: ForgeRef = { provider: "github", host: "github.com" };

// A minimal PullRequest carrying only the fields the derivations read
// (forgeState + draft); the rest are filled with inert defaults so the fixture
// satisfies the canonical type without steering behavior.
function pr(over: Partial<PullRequest>): PullRequest {
	return {
		forge: GITHUB,
		repo: "RigelBuild/compass",
		number: 0,
		title: "",
		forgeState: "open",
		url: "",
		headRef: "",
		baseRef: "",
		forgeAccount: "octocat",
		draft: false,
		reviews: [],
		threads: [],
		...over,
	};
}

// A minimal Issue carrying only the fields issueKey / isMultiForge read (forge,
// repo, number, tracker, prs); the rest are inert defaults.
function issue(over: Partial<Issue>): Issue {
	return {
		id: "cmp-0",
		forge: GITHUB,
		repo: "RigelBuild/compass",
		number: 41,
		title: "",
		body: "",
		forgeState: "open",
		url: "",
		forgeAccount: "octocat",
		labels: [],
		state: "in_progress",
		priority: "medium",
		assignee: null,
		summary: "",
		branch: "",
		prs: [],
		...over,
	};
}

// primaryPr picks the rendered PR by a FROZEN precedence (DL-071): first OPEN in
// `prs` order, else first MERGED, else the last element; empty prs → undefined.
// Selection is never by PR number. An inversion (merged before open, or first
// instead of last for the fallback) reddens these.
describe("primaryPr", () => {
	test("first OPEN wins over an earlier-index merged PR", () => {
		const merged = pr({ number: 1, forgeState: "merged" });
		const open = pr({ number: 2, forgeState: "open" });
		expect(primaryPr(issue({ prs: [merged, open] }))).toBe(open);
	});

	test("first MERGED when no open PR exists", () => {
		const closed = pr({ number: 1, forgeState: "closed" });
		const merged = pr({ number: 2, forgeState: "merged" });
		expect(primaryPr(issue({ prs: [closed, merged] }))).toBe(merged);
	});

	test("last element when neither open nor merged", () => {
		const first = pr({ number: 1, forgeState: "closed" });
		const last = pr({ number: 2, forgeState: "closed" });
		expect(primaryPr(issue({ prs: [first, last] }))).toBe(last);
	});

	test("undefined on empty prs", () => {
		expect(primaryPr(issue({ prs: [] }))).toBeUndefined();
	});
});

// checkPip collapses the 6-valued forge check state to a 3-valued pip class
// (DL-071): success→success; failure & cancelled→failure; everything else
// (queued|in_progress|neutral)→pending. Inverting any arm (e.g. neutral→failure)
// reddens the matching case.
describe("checkPip", () => {
	const cases: [Check["state"], "success" | "failure" | "pending"][] = [
		["success", "success"],
		["failure", "failure"],
		["cancelled", "failure"],
		["queued", "pending"],
		["in_progress", "pending"],
		["neutral", "pending"],
	];
	for (const [state, pip] of cases) {
		test(`${state} → ${pip}`, () => {
			expect(checkPip(state)).toBe(pip);
		});
	}
});

// prBadge derives the PR state badge (DL-071): draft-and-open → 'draft', else
// `forgeState`. The open-guard holds — a draft that is merged/closed renders its
// forgeState, not 'draft'. Dropping the guard reddens the draft&&merged case.
describe("prBadge", () => {
	test("draft && open → draft", () => {
		expect(prBadge(pr({ draft: true, forgeState: "open" }))).toBe("draft");
	});

	test("draft && merged → merged (open-guard holds)", () => {
		expect(prBadge(pr({ draft: true, forgeState: "merged" }))).toBe("merged");
	});

	test("non-draft open → open", () => {
		expect(prBadge(pr({ draft: false, forgeState: "open" }))).toBe("open");
	});

	test("closed → closed", () => {
		expect(prBadge(pr({ draft: false, forgeState: "closed" }))).toBe("closed");
	});
});

// issueKey renders the user-facing card key (DL-071): the tracker id when
// linked, else the forge coordinate `${repo}#${number}` — host-qualified only
// when the board spans more than one distinct ForgeRef. isMultiForge is that
// disambiguation trigger: true iff the list spans >1 distinct provider:host.
describe("issueKey + isMultiForge", () => {
	test("tracker id wins when the issue is linked", () => {
		const linked = issue({
			tracker: {
				kind: "linear",
				id: "RIG-1042",
				status: "In Progress",
				url: "",
			},
		});
		expect(issueKey(linked, true)).toBe("RIG-1042");
	});

	test("coordinate repo#number when no tracker and single-forge", () => {
		const it = issue({ repo: "RigelBuild/compass", number: 41 });
		expect(issueKey(it, false)).toBe("RigelBuild/compass#41");
	});

	test("host-qualified when multiForge", () => {
		const it = issue({ repo: "RigelBuild/compass", number: 41 });
		expect(issueKey(it, true)).toBe("github.com/RigelBuild/compass#41");
	});

	test("isMultiForge is false for a one-ForgeRef list", () => {
		expect(isMultiForge([issue({}), issue({ id: "cmp-1" })])).toBe(false);
	});

	test("isMultiForge is true across two distinct provider:host pairs", () => {
		const gh = issue({});
		const linear = issue({
			id: "cmp-1",
			forge: { provider: "linear", host: "linear.app" },
		});
		expect(isMultiForge([gh, linear])).toBe(true);
	});
});

// authorLabel renders the boarded artifact's Compass agent as a bare `@handle`.
// Only Compass artifacts from trusted agent accounts are boarded (Matt's
// 2026-07-31 ruling), so there is no untrusted owner-header spoof surface: the
// label carries no owner text and no verified/claims hedge. This golden string
// guards that copy — any re-introduction of the owner suffix or a hedge reddens
// here.
describe("authorLabel", () => {
	const agent = (agentHandle: string, verified: boolean): AgentAttribution => ({
		agentHandle,
		ownerHandle: "matt",
		verified,
	});

	test("renders the agent handle as @handle, with no owner text or hedge", () => {
		expect(authorLabel(agent("atlas", true))).toBe("@atlas");
	});

	// A different handle with verified=false proves the label is DERIVED from
	// agentHandle (not a hardcoded "@atlas") AND is independent of the verified
	// bit — one assertion carrying both facts.
	test("tracks agentHandle and ignores the verified bit", () => {
		expect(authorLabel(agent("nemo", false))).toBe("@nemo");
	});
});

// reviewBadge rolls the review set to one display verdict: latest-per-author
// over the submission-ordered `reviews` (bots INCLUDED), then precedence
// changes_requested > approved > commented, with changes_requested mapped to the
// display word "changes" INSIDE the helper (its one owner). Empty → undefined.
describe("reviewBadge", () => {
	const review = (
		author: string,
		verdict: Review["verdict"],
		isBot = false,
	): Review => ({ author, isBot, verdict, body: "" });

	test("empty reviews → undefined (no badge)", () => {
		expect(reviewBadge(pr({ reviews: [] }))).toBeUndefined();
	});

	test("a lone changes_requested maps to the display word 'changes'", () => {
		expect(
			reviewBadge(pr({ reviews: [review("a", "changes_requested")] })),
		).toBe("changes");
	});

	// Same-author supersession, favorable direction: a later approved beats the
	// same author's earlier changes_requested — only the LAST entry per author
	// counts, so the block clears.
	test("same author: later approved supersedes earlier changes_requested", () => {
		expect(
			reviewBadge(
				pr({
					reviews: [review("a", "changes_requested"), review("a", "approved")],
				}),
			),
		).toBe("approved");
	});

	// Same-author supersession, UNFLATTERING direction: a later commented
	// supersedes the same author's earlier approved — the badge DROPS to
	// commented (not stickily "approved"). Pins the latest-wins rule where it
	// costs a green signal.
	test("same author: later commented supersedes earlier approved (badge drops)", () => {
		expect(
			reviewBadge(
				pr({
					reviews: [review("a", "approved"), review("a", "commented")],
				}),
			),
		).toBe("commented");
	});

	// Cross-author precedence: one blocking reviewer dominates the others'
	// approvals regardless of submission order.
	test("cross-author: changes_requested dominates approved", () => {
		expect(
			reviewBadge(
				pr({
					reviews: [review("a", "approved"), review("b", "changes_requested")],
				}),
			),
		).toBe("changes");
	});

	// With no block, an approval beats a bare comment.
	test("cross-author: approved beats commented", () => {
		expect(
			reviewBadge(
				pr({
					reviews: [review("a", "commented"), review("b", "approved")],
				}),
			),
		).toBe("approved");
	});

	// Bots are INCLUDED in the roll-up — a bot's block blocks.
	test("a bot review counts (changes_requested from a bot blocks)", () => {
		expect(
			reviewBadge(
				pr({
					reviews: [
						review("human", "approved"),
						review("ci-bot", "changes_requested", true),
					],
				}),
			),
		).toBe("changes");
	});
});

// ciBadge is the thin total accessor for the roll-up CI state — pr.checks?.state
// read through one named seam. No checks → undefined (no badge).
describe("ciBadge", () => {
	test("no checks → undefined", () => {
		expect(ciBadge(pr({ checks: undefined }))).toBeUndefined();
	});

	test("reads the roll-up state directly", () => {
		expect(
			ciBadge(pr({ checks: { headSha: "a", state: "failure", checks: [] } })),
		).toBe("failure");
		expect(
			ciBadge(pr({ checks: { headSha: "a", state: "success", checks: [] } })),
		).toBe("success");
		expect(
			ciBadge(pr({ checks: { headSha: "a", state: "pending", checks: [] } })),
		).toBe("pending");
	});
});

// openPrs keeps only the OPEN PRs of an issue (forgeState === "open"), in `prs`
// order — the PRs-tab row source. Merged/closed drop; draft-open stays (draft
// is a separate axis from forgeState).
describe("openPrs", () => {
	test("keeps only open PRs, dropping merged and closed", () => {
		const open1 = pr({ number: 1, forgeState: "open" });
		const merged = pr({ number: 2, forgeState: "merged" });
		const open2 = pr({ number: 3, forgeState: "open" });
		const closed = pr({ number: 4, forgeState: "closed" });
		expect(openPrs(issue({ prs: [open1, merged, open2, closed] }))).toEqual([
			open1,
			open2,
		]);
	});

	test("preserves prs order and includes a draft-open PR", () => {
		const draftOpen = pr({ number: 1, forgeState: "open", draft: true });
		const open = pr({ number: 2, forgeState: "open" });
		expect(openPrs(issue({ prs: [draftOpen, open] }))).toEqual([
			draftOpen,
			open,
		]);
	});

	test("an empty prs list → []", () => {
		expect(openPrs(issue({ prs: [] }))).toEqual([]);
	});
});

// prLifecycle places a PR in a PRs-board column (design D1 / T6). Precedence:
// merged > ready > in_review > in_progress, defined over the reviewBadge/ciBadge
// roll-ups. D1a: an approved + CI-green PR with an UNRESOLVED thread is NOT
// ready — it stays "in review". Total over board rows (forgeState !== "closed").
describe("prLifecycle", () => {
	const review = (author: string, verdict: Review["verdict"]): Review => ({
		author,
		isBot: false,
		verdict,
		body: "",
	});
	const thread = (resolved: boolean): ReviewThread => ({
		path: "",
		resolved,
		comments: [],
	});
	const green = { headSha: "a", state: "success" as const, checks: [] };

	test("merged wins even over approved + green + all-resolved", () => {
		expect(
			prLifecycle(
				pr({
					forgeState: "merged",
					reviews: [review("a", "approved")],
					checks: green,
					threads: [thread(true)],
				}),
			),
		).toBe("merged");
	});

	test("ready: approved + CI success + all threads resolved", () => {
		expect(
			prLifecycle(
				pr({
					reviews: [review("a", "approved")],
					checks: green,
					threads: [thread(true)],
				}),
			),
		).toBe("ready");
	});

	test("ready: approved + CI success + zero threads (vacuously resolved)", () => {
		expect(
			prLifecycle(
				pr({ reviews: [review("a", "approved")], checks: green, threads: [] }),
			),
		).toBe("ready");
	});

	test("D1a gate: approved + green but an UNRESOLVED thread → in_review, not ready", () => {
		expect(
			prLifecycle(
				pr({
					reviews: [review("a", "approved")],
					checks: green,
					threads: [thread(true), thread(false)],
				}),
			),
		).toBe("in_review");
	});

	test("in_review via a verdict alone (commented, no threads)", () => {
		expect(
			prLifecycle(pr({ reviews: [review("a", "commented")], threads: [] })),
		).toBe("in_review");
	});

	test("in_review via a changes verdict (not approved-green)", () => {
		expect(
			prLifecycle(
				pr({
					reviews: [review("a", "changes_requested")],
					checks: green,
					threads: [],
				}),
			),
		).toBe("in_review");
	});

	test("in_review via open threads with no verdict", () => {
		expect(prLifecycle(pr({ reviews: [], threads: [thread(false)] }))).toBe(
			"in_review",
		);
	});

	test("in_progress: draft-open with no reviews or threads", () => {
		expect(prLifecycle(pr({ draft: true, reviews: [], threads: [] }))).toBe(
			"in_progress",
		);
	});

	test("in_progress: a bare open PR (no reviews, no threads, no checks)", () => {
		expect(prLifecycle(pr({ reviews: [], threads: [] }))).toBe("in_progress");
	});
});
