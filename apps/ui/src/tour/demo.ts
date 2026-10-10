// The tour's demo dataset: a small fleet, a few board issues, and one channel, so
// the tour has something to point at in an empty workspace. The store merges these
// rows into its read accessors only while the tour is open; they never reach the
// server, the query cache, or storage.

import type { Channel, Message, Topic } from "../comms-stub";
import type { AgentPresenceInfo } from "../live/adapt";
import { joinAgents } from "../roster";
import type { Account, Agent, Issue, PullRequest } from "../stub-data";

/** Every demo row id starts with this prefix; it is how write paths refuse them
 *  and how the overlay decides to render a "Demo" badge. */
export const DEMO_PREFIX = "demo:";

/** Whether an id names a demo row. */
export function isDemoId(id: string | null | undefined): boolean {
	return id?.startsWith(DEMO_PREFIX) ?? false;
}

const LEAD = "demo:acc-lead";
const BUILDER = "demo:acc-builder";
const REVIEWER = "demo:acc-reviewer";
const CHANNEL = "demo:ch-launch";
const TOPIC = "demo:top-launch";

/** The demo agents' accounts; they also author the demo channel's messages. */
export const DEMO_ACCOUNTS: readonly Account[] = [
	{
		id: LEAD,
		handle: "demo-lead",
		displayName: "demo-lead",
		kind: "agent",
	},
	{
		id: BUILDER,
		handle: "demo-builder",
		displayName: "demo-builder",
		kind: "agent",
		parentAgentId: LEAD,
	},
	{
		id: REVIEWER,
		handle: "demo-reviewer",
		displayName: "demo-reviewer",
		kind: "agent",
		parentAgentId: LEAD,
	},
];

/** Presence for the demo agents, keyed like the live presence map. */
export const DEMO_PRESENCE: ReadonlyMap<string, AgentPresenceInfo> = new Map([
	[LEAD, { lifecycle: "working", activity: "planning the sample-app launch" }],
	[BUILDER, { lifecycle: "working", activity: "adding the signup form" }],
	[REVIEWER, { lifecycle: "waiting", activity: "waiting on CI to finish" }],
]);

/** The demo agents, joined the same way the live roster is. */
export const DEMO_AGENTS: readonly Agent[] = joinAgents(
	DEMO_ACCOUNTS,
	DEMO_PRESENCE,
	new Map(),
);

const REPO = "demo/sample-app";
const FORGE = { provider: "github", host: "github.com" } as const;

// Empty urls keep demo links inert: safeHref renders nothing for "".
function demoIssue(
	n: number,
	fields: Pick<
		Issue,
		"title" | "state" | "priority" | "assignee" | "summary" | "branch"
	> & { prs?: PullRequest[] },
): Issue {
	return {
		id: `demo:issue-${n}`,
		forge: FORGE,
		repo: REPO,
		number: n,
		body: "",
		forgeState: "open",
		url: "",
		forgeAccount: "demo-bot",
		labels: [],
		prs: [],
		...fields,
	};
}

/** Demo issues spread across the board columns; one carries an open PR. */
export const DEMO_ISSUES: readonly Issue[] = [
	demoIssue(1, {
		title: "Plan the sample-app launch",
		state: "queued",
		priority: "medium",
		assignee: LEAD,
		summary: "Waiting for a free agent.",
		branch: "demo-lead-1-launch-plan",
	}),
	demoIssue(2, {
		title: "Add a signup form",
		state: "in_progress",
		priority: "high",
		assignee: BUILDER,
		summary: "Form and validation are in; wiring the submit.",
		branch: "demo-builder-2-signup",
	}),
	demoIssue(3, {
		title: "Fix the flaky login test",
		state: "in_review",
		priority: "urgent",
		assignee: REVIEWER,
		summary: "PR open; waiting on CI.",
		branch: "demo-reviewer-3-login-test",
		prs: [
			{
				forge: FORGE,
				repo: REPO,
				number: 31,
				title: "fix: stabilise the login test",
				forgeState: "open",
				url: "",
				headRef: "demo-reviewer-3-login-test",
				baseRef: "main",
				forgeAccount: "demo-bot",
				draft: false,
				changed: { files: 2, additions: 18, deletions: 7 },
				reviews: [],
				threads: [],
			},
		],
	}),
	demoIssue(4, {
		title: "Set up the project",
		state: "done",
		priority: "low",
		assignee: BUILDER,
		summary: "Scaffold merged.",
		branch: "demo-builder-4-scaffold",
	}),
];

/** The one demo channel, filed under the demo lead in the agent tree. */
export const DEMO_CHANNELS: readonly Channel[] = [
	{
		id: CHANNEL,
		name: "demo-launch",
		kind: "channel",
		memberAccountIds: [LEAD, BUILDER, REVIEWER],
		topic: "Demo channel for the tour.",
		membership: "subscribed",
		postPolicy: "open",
		parentAgentId: LEAD,
		membershipMode: "tree",
	},
];

const T0 = Date.UTC(2026, 6, 18, 9, 0, 0);
const at = (m: number): number => T0 + m * 60_000;

export const DEMO_TOPICS: readonly Topic[] = [
	{
		id: TOPIC,
		channelId: CHANNEL,
		name: "launch plan",
		createdAtUnixMs: at(0),
		createdByAccountId: LEAD,
		archived: false,
	},
];

export const DEMO_MESSAGES: readonly Message[] = [
	{
		id: "demo:msg-1",
		topicId: TOPIC,
		authorAccountId: LEAD,
		atUnixMs: at(0),
		blocks: [
			{
				kind: "text",
				text: "Plan for today: signup form first, then the login test fix.",
			},
		],
	},
	{
		id: "demo:msg-2",
		topicId: TOPIC,
		authorAccountId: BUILDER,
		atUnixMs: at(4),
		blocks: [{ kind: "text", text: "On the signup form now." }],
	},
	{
		id: "demo:msg-3",
		topicId: TOPIC,
		authorAccountId: REVIEWER,
		atUnixMs: at(9),
		blocks: [
			{
				kind: "text",
				text: "Login test fix is up for review. CI is running.",
			},
		],
	},
];
