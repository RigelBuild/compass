import { describe, expect, test } from "bun:test";
import {
	attributionLabel,
	checkPip,
	isMultiForge,
	issueKey,
	prBadge,
	primaryPr,
} from "./board-render";
import type {
	AgentAttribution,
	Check,
	ForgeRef,
	Issue,
	PullRequest,
} from "./stub-data";

const GITHUB: ForgeRef = { provider: "github", host: "github.com" };

// A minimal PullRequest carrying only the fields the derivations read
// (forgeState + draft); the rest are filled with inert defaults so the fixture
// satisfies the canonical type without steering behavior.
function pr(over: Partial<PullRequest>): PullRequest {
	return {
		forge: GITHUB,
		repo: "sealedsecurity/compass",
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
		repo: "sealedsecurity/compass",
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
				id: "SEA-1042",
				status: "In Progress",
				url: "",
			},
		});
		expect(issueKey(linked, true)).toBe("SEA-1042");
	});

	test("coordinate repo#number when no tracker and single-forge", () => {
		const it = issue({ repo: "sealedsecurity/compass", number: 41 });
		expect(issueKey(it, false)).toBe("sealedsecurity/compass#41");
	});

	test("host-qualified when multiForge", () => {
		const it = issue({ repo: "sealedsecurity/compass", number: 41 });
		expect(issueKey(it, true)).toBe("github.com/sealedsecurity/compass#41");
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

// attributionLabel renders UNTRUSTED agent attribution (DL-068) as a hedged
// claim unless the server-set `verified` bit is true. The three wordings are
// frozen in the design record (pinned verbatim with a golden test). These
// golden strings guard the frozen copy: any silent promotion of a claim to a
// fact — dropping the `claims to be ` hedge while `verified` is false, or
// re-adding an `@` to the no-agent forge login — reddens the case that changed.
describe("attributionLabel", () => {
	const agent = (verified: boolean): AgentAttribution => ({
		agentHandle: "atlas",
		ownerHandle: "matt",
		verified,
	});

	test("unverified agent is hedged as a claim", () => {
		expect(attributionLabel(agent(false), "compass-bot")).toBe(
			"claims to be @atlas (Compass agent, owned by @matt)",
		);
	});

	test("verified agent drops the hedge", () => {
		expect(attributionLabel(agent(true), "compass-bot")).toBe(
			"@atlas (Compass agent, owned by @matt)",
		);
	});

	test("no agent renders the bare forge login", () => {
		expect(attributionLabel(undefined, "octocat")).toBe(
			"octocat (not a Compass agent)",
		);
	});
});
