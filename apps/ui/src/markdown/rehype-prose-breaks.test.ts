import { describe, expect, test } from "bun:test";
import type {
	Element as HastElement,
	ElementContent as HastElementContent,
	Parent as HastParent,
	RootContent as HastRootContent,
} from "hast";
import { rehypeProseBreaks } from "./rehype-prose-breaks";

// `rehypeProseBreaks()` returns a transformer that mutates the tree in place;
// each test builds a small `root` tree, runs it, and asserts `children`.
function run(children: HastRootContent[]): HastRootContent[] {
	const tree: HastParent = { type: "root", children };
	rehypeProseBreaks()(tree);
	return tree.children;
}

function el(tagName: string, children: HastElementContent[] = []): HastElement {
	return { type: "element", tagName, properties: {}, children };
}

const br: HastElement = {
	type: "element",
	tagName: "br",
	properties: {},
	children: [],
};

describe("rehypeProseBreaks", () => {
	test("a text node containing `\\n` splits into text/`br`/text", () => {
		const out = run([{ type: "text", value: "a\nb" }]);
		expect(out).toEqual([
			{ type: "text", value: "a" },
			br,
			{ type: "text", value: "b" },
		]);
	});

	test("a bare `\\n` between two block elements is dropped (no `br`)", () => {
		const out = run([el("p"), { type: "text", value: "\n" }, el("ul")]);
		expect(out).toEqual([el("p"), el("ul")]);
	});

	test("a bare `\\n` before a block element is dropped (forward branch)", () => {
		// prev sibling is neither block nor `br`, so only the `isBlock(next)`
		// branch of the drop clause can fire — pins the forward path in isolation.
		const out = run([
			{ type: "text", value: "hi" },
			{ type: "text", value: "\n" },
			el("p"),
		]);
		expect(out).toEqual([{ type: "text", value: "hi" }, el("p")]);
	});

	test("a `\\n` inside a `code`/`pre` subtree is NOT split", () => {
		const out = run([
			el("pre", [el("code", [{ type: "text", value: "x\ny" }])]),
		]);
		expect(out).toEqual([
			el("pre", [el("code", [{ type: "text", value: "x\ny" }])]),
		]);
	});

	test("a preceding `br` absorbs a trailing `\\n` (no double `br`)", () => {
		// A hard break arrives as [br, text("\n")]; the bare "\n" after a br is
		// dropped rather than producing a second br.
		const out = run([
			{ type: "text", value: "line" },
			br,
			{ type: "text", value: "\n" },
		]);
		expect(out).toEqual([{ type: "text", value: "line" }, br]);
	});

	test("a softbreak inside an `li` still splits into a `br`", () => {
		const out = run([el("ul", [el("li", [{ type: "text", value: "a\nb" }])])]);
		expect(out).toEqual([
			el("ul", [
				el("li", [
					{ type: "text", value: "a" },
					br,
					{ type: "text", value: "b" },
				]),
			]),
		]);
	});
});
