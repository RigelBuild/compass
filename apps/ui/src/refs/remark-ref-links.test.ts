import { describe, expect, test } from "bun:test";
import type { RefLinkRule } from "./grammar";
import { remarkRefLinks } from "./remark-ref-links";

interface TestNode {
	type: string;
	value?: string;
	url?: string;
	title?: string | null;
	children?: TestNode[];
}

const rules: readonly RefLinkRule[] = [
	{
		kind: "github",
		prefix: "github.com",
		urlTemplate: "https://github.com/{owner}/{repo}/issues/{number}",
	},
	{
		kind: "linear",
		prefix: "RIG",
		urlTemplate: "https://linear.app/rigel/issue/{key}-{number}",
	},
];

function transform(children: TestNode[]): TestNode[] {
	const tree: TestNode = { type: "root", children };
	const plugin = remarkRefLinks(() => rules);
	plugin()(tree);
	return tree.children ?? [];
}

function linked(nodes: readonly TestNode[]): Array<[string, string]> {
	const links: Array<[string, string]> = [];
	for (const node of nodes) {
		if (node.type === "link") {
			links.push([
				node.url ?? "",
				node.children?.map((child) => child.value ?? "").join("") ?? "",
			]);
		} else if (
			node.children &&
			node.type !== "linkReference" &&
			node.type !== "inlineCode" &&
			node.type !== "code"
		) {
			links.push(...linked(node.children));
		}
	}
	return links;
}

describe("remarkRefLinks", () => {
	test("links refs in prose and preserves adjacent text", () => {
		const tree = transform([
			{ type: "text", value: "Fix RIG-12 and owner/repo#3." },
		]);
		expect(linked(tree)).toEqual([
			["https://linear.app/rigel/issue/RIG-12", "RIG-12"],
			["https://github.com/owner/repo/issues/3", "owner/repo#3"],
		]);
		expect(tree.map((node) => node.value ?? node.children?.[0]?.value)).toEqual(
			["Fix ", "RIG-12", " and ", "owner/repo#3", "."],
		);
	});

	test("does not link refs in inline code, code blocks, or existing links", () => {
		const tree = transform([
			{ type: "inlineCode", value: "RIG-1" },
			{ type: "code", value: "RIG-2" },
			{
				type: "paragraph",
				children: [
					{
						type: "link",
						url: "https://example.com",
						children: [{ type: "text", value: "RIG-3" }],
					},
				],
			},
		]);
		expect(linked(tree)).toEqual([["https://example.com", "RIG-3"]]);
		expect(tree[2]?.children?.[0]).toEqual({
			type: "link",
			url: "https://example.com",
			children: [{ type: "text", value: "RIG-3" }],
		});
	});

	test("leaves refs with no matching Linear team as text", () => {
		const tree = transform([{ type: "text", value: "OPS-7 stays plain." }]);
		expect(linked(tree)).toEqual([]);
		expect(tree.map((node) => node.value ?? "").join("")).toBe(
			"OPS-7 stays plain.",
		);
	});
});
