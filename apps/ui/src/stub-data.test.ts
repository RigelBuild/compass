import { describe, expect, test } from "bun:test";
import type { Channel } from "./comms-stub";
import { type Agent, agentTree } from "./stub-data";

// Preserve input order and promote dangling parents while keeping cycles visible.

// The minimal fixture sets only the fields used by agentTree.
const agent = (id: string, parentAgentId?: string): Agent => ({
	account: {
		id,
		handle: id,
		displayName: id,
		kind: "agent",
		parentAgentId,
	},
	role: "worker",
	model: "test-model",
	cwd: "/tmp",
	terminals: [],
});

const channel = (id: string, fields: Partial<Channel> = {}): Channel => ({
	id,
	name: id,
	kind: "channel",
	memberAccountIds: [],
	membership: "joined",
	postPolicy: "open",
	...fields,
});

// Read a node's id and its children's ids, for terse structural assertions.
const ids = (nodes: { agent: Agent }[]): string[] =>
	nodes.map((n) => n.agent.account.id);

describe("agentTree", () => {
	test("flat: all roots preserve input order", () => {
		const agents = [agent("a"), agent("b"), agent("c")];
		const tree = agentTree(agents);
		expect(ids(tree)).toEqual(["a", "b", "c"]);
		expect(tree.every((n) => n.children.length === 0)).toBe(true);
	});

	test("nested: a child is placed under its parent", () => {
		const agents = [agent("root"), agent("child", "root")];
		const tree = agentTree(agents);
		expect(ids(tree)).toEqual(["root"]);
		expect(ids(tree[0].children)).toEqual(["child"]);
	});

	test("multi-root with a nested child", () => {
		const agents = [agent("r1"), agent("r1-c", "r1"), agent("r2")];
		const tree = agentTree(agents);
		expect(ids(tree)).toEqual(["r1", "r2"]);
		expect(ids(tree[0].children)).toEqual(["r1-c"]);
		expect(tree[1].children).toEqual([]);
	});

	test("sibling order == input order", () => {
		// Children are declared out of alphabetical order on purpose: the
		// derivation must follow input order, not sort.
		const agents = [
			agent("p"),
			agent("z", "p"),
			agent("a", "p"),
			agent("m", "p"),
		];
		const tree = agentTree(agents);
		expect(ids(tree)).toEqual(["p"]);
		expect(ids(tree[0].children)).toEqual(["z", "a", "m"]);
	});

	test("root order == input order even when a child appears before its parent", () => {
		const agents = [
			agent("early-child", "late-parent"),
			agent("plain-root"),
			agent("late-parent"),
		];
		const tree = agentTree(agents);
		// Roots collected in input order: late-parent's child is not a root, so
		// the roots are plain-root then late-parent.
		expect(ids(tree)).toEqual(["plain-root", "late-parent"]);
		expect(ids(tree[1].children)).toEqual(["early-child"]);
	});

	test("dangling parentAgentId is promoted to a root, not dropped", () => {
		const agents = [agent("kept"), agent("orphan", "acc-missing")];
		const tree = agentTree(agents);
		expect(ids(tree)).toEqual(["kept", "orphan"]);
		expect(tree.every((n) => n.children.length === 0)).toBe(true);
	});

	test("depth-3 nesting keeps each generation under its parent", () => {
		const agents = [
			agent("gen0"),
			agent("gen1", "gen0"),
			agent("gen2", "gen1"),
		];
		const tree = agentTree(agents);
		expect(ids(tree)).toEqual(["gen0"]);
		expect(ids(tree[0].children)).toEqual(["gen1"]);
		expect(ids(tree[0].children[0].children)).toEqual(["gen2"]);
	});

	test("a self-parent is promoted to a root, not dropped", () => {
		const agents = [agent("kept"), agent("selfie", "selfie")];
		const tree = agentTree(agents);
		expect(ids(tree)).toEqual(["kept", "selfie"]);
		expect(tree.every((n) => n.children.length === 0)).toBe(true);
	});

	test("a two-node cycle survives: both members become roots", () => {
		// Both nodes in a cycle become roots in their original input order.
		const agents = [agent("x", "y"), agent("y", "x")];
		const tree = agentTree(agents);
		expect(ids(tree)).toEqual(["x", "y"]);
		expect(tree.every((n) => n.children.length === 0)).toBe(true);
	});

	test("slots home and parented channels once, leaving dangling channel parents unclaimed", () => {
		const homeAgent = agent("home-owner");
		homeAgent.account.homeChannelId = "home";
		const agents = [homeAgent, agent("attached")];
		const channels = [
			channel("attached-channel", { parentAgentId: "attached" }),
			channel("home"),
			channel("orphan-channel", { parentAgentId: "missing-agent" }),
		];
		const tree = agentTree(agents, channels);
		expect(tree[0]?.channels.map((item) => item.id)).toEqual(["home"]);
		expect(tree[1]?.channels.map((item) => item.id)).toEqual([
			"attached-channel",
		]);
		expect(
			tree.flatMap((node) => node.channels).map((item) => item.id),
		).toEqual(["home", "attached-channel"]);
	});

	test("home ownership takes precedence over a different parent agent", () => {
		const homeAgent = agent("home-owner");
		homeAgent.account.homeChannelId = "home";
		const agents = [homeAgent, agent("parent-agent")];
		const tree = agentTree(agents, [
			channel("home", { parentAgentId: "parent-agent" }),
		]);
		expect(tree[0]?.channels.map((item) => item.id)).toEqual(["home"]);
		expect(tree[1]?.channels).toEqual([]);
	});
	test("empty input yields an empty tree", () => {
		expect(agentTree([])).toEqual([]);
	});
});
