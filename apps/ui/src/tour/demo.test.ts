import { describe, expect, test } from "bun:test";
import { prRows } from "../board";
import { agentTree } from "../stub-data";
import {
	DEMO_ACCOUNTS,
	DEMO_AGENTS,
	DEMO_CHANNELS,
	DEMO_ISSUES,
	DEMO_MESSAGES,
	DEMO_TOPICS,
	isDemoId,
} from "./demo";

describe("demo dataset", () => {
	// The prefix is what every write guard keys on; an unprefixed row would
	// reach the server or storage.
	test("every id carries the demo prefix", () => {
		const ids = [
			...DEMO_ACCOUNTS.map((a) => a.id),
			...DEMO_ACCOUNTS.map((a) => a.parentAgentId).filter(
				(id) => id !== undefined,
			),
			...DEMO_ISSUES.map((i) => i.id),
			...DEMO_ISSUES.map((i) => i.assignee),
			...DEMO_CHANNELS.map((c) => c.id),
			...DEMO_CHANNELS.flatMap((c) => c.memberAccountIds),
			...DEMO_TOPICS.map((t) => t.id),
			...DEMO_MESSAGES.map((m) => m.id),
			...DEMO_MESSAGES.map((m) => m.topicId),
			...DEMO_MESSAGES.map((m) => m.authorAccountId),
		];
		expect(ids.filter((id) => !isDemoId(id))).toEqual([]);
	});

	test("isDemoId matches the prefix only", () => {
		expect(isDemoId("demo:acc-lead")).toBe(true);
		expect(isDemoId("acc-demo:x")).toBe(false);
		expect(isDemoId(null)).toBe(false);
	});

	test("agents carry presence and form one tree with the channel under it", () => {
		expect(DEMO_AGENTS.every((a) => a.lifecycle !== "stopped")).toBe(true);
		const tree = agentTree(DEMO_AGENTS, DEMO_CHANNELS);
		expect(tree.length).toBe(1);
		expect(tree[0]?.children.length).toBe(DEMO_AGENTS.length - 1);
		expect(tree[0]?.channels.map((c) => c.id)).toEqual(
			DEMO_CHANNELS.map((c) => c.id),
		);
	});

	test("issues span several board columns and one has an open PR", () => {
		expect(new Set(DEMO_ISSUES.map((i) => i.state)).size).toBeGreaterThan(2);
		const agentIds = DEMO_AGENTS.map((a) => a.account.id);
		expect(DEMO_ISSUES.every((i) => agentIds.includes(i.assignee ?? ""))).toBe(
			true,
		);
		expect(prRows(DEMO_ISSUES).length).toBe(1);
	});

	test("messages resolve to the demo topic and demo authors", () => {
		const topicIds = DEMO_TOPICS.map((t) => t.id);
		const accountIds = DEMO_ACCOUNTS.map((a) => a.id);
		expect(
			DEMO_MESSAGES.every(
				(m) =>
					topicIds.includes(m.topicId) &&
					accountIds.includes(m.authorAccountId),
			),
		).toBe(true);
		const channelIds = DEMO_CHANNELS.map((c) => c.id);
		expect(DEMO_TOPICS.every((t) => channelIds.includes(t.channelId))).toBe(
			true,
		);
	});
});
