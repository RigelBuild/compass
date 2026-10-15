import { expect, test } from "bun:test";
import { render } from "@solidjs/testing-library";
import type { AgentBoard } from "../stub-data";
import { AgentBoardView } from "./AgentBoard";

const BOARD: AgentBoard = {
	agentId: "acc-compass-ui",
	handle: "compass-ui",
	focusTaskKey: "task-a",
	halt: { kind: "blocked", ref: "RIG-12", reason: "Needs review" },
	tasks: [
		{
			key: "task-a",
			title: "Ship status board",
			primaryRef: "RigelBuild/compass#12",
			refs: ["RigelBuild/compass#12", "RIG-9"],
			state: "blocked",
			note: "Waiting for checks",
			waiting: [
				{ on: "operator", what: "Review the patch", blocks: ["RIG-9"] },
				{ on: "agent", agentHandle: "builder", blocks: ["RIG-12"] },
				{ on: "external", blocks: [] },
			],
			subagents: [
				{
					id: "sub-1",
					agent: "builder",
					description: "Run integration tests",
					status: "working",
					startedAtUnixMs: 10,
				},
			],
		},
	],
	refs: [
		{
			ref: "RigelBuild/compass#12",
			kind: "pr",
			title: "Agent board panel",
			url: "https://github.com/RigelBuild/compass/pull/12",
			gate: "awaiting_enqueue",
			draft: true,
			checksState: "success",
			stackPosition: 2,
			stackTipRef: "RigelBuild/compass#13",
			owned: true,
			staleApproval: false,
		},
		{
			ref: "RigelBuild/compass#13",
			kind: "pr",
			title: "Next stack item",
			url: "https://github.com/RigelBuild/compass/pull/13",
			draft: false,
			stackPosition: 3,
			stackTipRef: "RigelBuild/compass#13",
			owned: true,
			staleApproval: false,
		},
		{
			ref: "RIG-9",
			kind: "issue",
			title: "Review board UX",
			draft: false,
			owned: true,
			staleApproval: false,
		},
		{
			ref: "RIG-20",
			kind: "issue",
			title: "Untracked issue",
			url: "https://linear.app/RIG/issue/RIG-20",
			draft: false,
			owned: true,
			staleApproval: false,
		},
	],
	untrackedRefs: ["RIG-20"],
	asks: [
		{
			askId: "ask-answered",
			messageId: "message-answered",
			channelId: "channel-answered",
			header: "Answered question",
			answered: true,
			postedAtUnixMs: 20,
		},
		{
			askId: "ask-pending",
			messageId: "message-pending",
			channelId: "channel-pending",
			header: "Choose a rollout",
			answered: false,
			postedAtUnixMs: 10,
		},
	],
	writtenAtUnixMs: Date.now() - 120_000,
	stale: true,
};

test("AgentBoardView renders board details and pending asks first", () => {
	const { container } = render(() => <AgentBoardView board={BOARD} />);
	const text = container.textContent ?? "";
	for (const value of [
		"blocked · RIG-12 · Needs review",
		"Ship status board",
		"updated 2 min ago",
		"Stale",
		"awaiting enqueue",
		"checks: success",
		"draft",
		"stack 2/3",
		"tip RigelBuild/compass#13",
		"Review the patch",
		"operator",
		"@builder",
		"external",
		"RIG-9",
		"Run integration tests",
		"Choose a rollout",
		"Answered question",
		"Untracked",
		"RIG-20",
	])
		expect(text).toContain(value);
	const asks = [...container.querySelectorAll(".agent-board-ask")];
	expect(asks.map((ask) => ask.textContent)).toEqual([
		"Choose a rollout",
		"Answered question",
	]);
});

const withPrimaryUrl = (url: string): AgentBoard => ({
	...BOARD,
	refs: BOARD.refs.map((ref) =>
		ref.ref === "RigelBuild/compass#12" ? { ...ref, url } : ref,
	),
});

test("AgentBoardView opens a safe primary ref externally", () => {
	const opened: string[] = [];
	const open = window.open;
	window.open = (url) => {
		opened.push(String(url));
		return null;
	};
	try {
		const { container } = render(() => <AgentBoardView board={BOARD} />);
		const link = container.querySelector<HTMLAnchorElement>(
			"a.agent-board-ref-primary",
		);
		expect(link?.getAttribute("href")).toBe(
			"https://github.com/RigelBuild/compass/pull/12",
		);
		expect(link?.getAttribute("rel")).toBe("noreferrer noopener");
		link?.click();
		expect(opened).toEqual(["https://github.com/RigelBuild/compass/pull/12"]);
	} finally {
		window.open = open;
	}
});

test("AgentBoardView renders an unsafe primary ref as plain text", () => {
	const { container } = render(() => (
		<AgentBoardView board={withPrimaryUrl("javascript:alert(1)")} />
	));
	expect(container.querySelector("a.agent-board-ref-primary")).toBeNull();
	expect(
		container.querySelector("span.agent-board-ref-primary")?.textContent,
	).toBe("RigelBuild/compass#12");
});
