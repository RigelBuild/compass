import { describe, expect, test } from "bun:test";
import { type Agent, agentTree } from "./stub-data";

// agentTree derives the nested sidebar tree from each account's parentAgentId
// (Record C / DL-095, §T4). These tests defend the derivation's contract:
// roots = empty/absent OR dangling parentAgentId (promoted, never dropped),
// children nested under their parent, and — the point C-T5's treeOrder
// consumes — roots and siblings preserve the stable INPUT ORDER of the agents
// array. Depth-first alone is not a total order, so this tie-break is the
// determinism guarantee for a fixed input.

// A minimal Agent fixture: only account.id and account.parentAgentId matter to
// the derivation, so the other fields are filled with inert stubs.
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

	test("empty input yields an empty tree", () => {
		expect(agentTree([])).toEqual([]);
	});
});
