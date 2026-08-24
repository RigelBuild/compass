import { describe, expect, test } from "bun:test";
import type {
	ElementContent as HastElementContent,
	Parent as HastParent,
	RootContent as HastRootContent,
} from "hast";
import { rehypeInertRaw } from "./rehype-inert-raw";

// `raw` is a non-stock hast node contributed by mdast-util-to-hast's
// `allowDangerousHtml` mode; the test trees below build it structurally so no
// full markdown parse is needed. `rehypeInertRaw()` returns a transformer that
// mutates the tree in place; we run it on a `root` and assert `children`.
type RawNode = { type: "raw"; value: string };

function run(children: HastRootContent[]): HastRootContent[] {
	const tree: HastParent = { type: "root", children };
	rehypeInertRaw()(tree);
	return tree.children;
}

describe("rehypeInertRaw", () => {
	test("a top-level `raw` node becomes a `text` node with the same value", () => {
		const raw = { type: "raw", value: "<T>" } as unknown as HastRootContent;
		const out = run([raw]);
		expect(out).toEqual([{ type: "text", value: "<T>" }]);
	});

	test("a `raw` node nested inside an element is retyped", () => {
		const raw = { type: "raw", value: "<b>" } as unknown as RawNode;
		const out = run([
			{
				type: "element",
				tagName: "p",
				properties: {},
				children: [raw as unknown as HastElementContent],
			},
		]);
		expect(out).toEqual([
			{
				type: "element",
				tagName: "p",
				properties: {},
				children: [{ type: "text", value: "<b>" }],
			},
		]);
	});

	test("a tree with no `raw` nodes is structurally unchanged", () => {
		const input: HastRootContent[] = [
			{ type: "text", value: "hello\nworld" },
			{
				type: "element",
				tagName: "code",
				properties: {},
				children: [{ type: "text", value: "x\ny" }],
			},
		];
		const out = run(structuredClone(input));
		expect(out).toEqual(input);
	});

	test("existing `text`/`element` nodes are untouched alongside a retyped `raw`", () => {
		const raw = { type: "raw", value: "<i>" } as unknown as HastRootContent;
		const out = run([
			{ type: "text", value: "before" },
			raw,
			{
				type: "element",
				tagName: "span",
				properties: {},
				children: [{ type: "text", value: "kept" }],
			},
		]);
		expect(out).toEqual([
			{ type: "text", value: "before" },
			{ type: "text", value: "<i>" },
			{
				type: "element",
				tagName: "span",
				properties: {},
				children: [{ type: "text", value: "kept" }],
			},
		]);
	});
});
