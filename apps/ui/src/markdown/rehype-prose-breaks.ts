// Prose softbreak rescue (markdown design A3/R2). Softbreaks are a class of
// silent content loss, one node type over from raw HTML. A single newline does
// NOT arrive as its own `"\n"` text node — mdast→hast leaves it EMBEDDED in a
// longer text node (verified: `"Done: **T4**\nT5 is next."` yields
// `text("Done: ") strong(T4) text("\nT5 is next.")`). The renderer emits that
// node's value as one DOM text node, and `.markdown-content` sets
// `white-space: normal` (its reset in app.css), so the newline collapses and
// the words join: `"T4T5 is next."`. Splitting such a node on `\n` and
// interleaving real `br` elements restores the line break under
// `white-space: normal`. That rescue is for PROSE only, and two guards keep it
// there.
//
// Inside `pre`/`code` the newline already survives — `white-space: pre` renders
// it verbatim — and the split is actively destructive: the `code` override
// renders from `rawText`, which concatenates text descendants and ignores `br`,
// so an interleaved break vanishes and a multi-line block collapses onto one
// line (both plain and, since Shiki highlights that same string, highlighted).
//
// Between BLOCK children mdast→hast also emits bare `"\n"` separator nodes
// (`ul`→`li`, `table`→`tr`, and inside a loose `li` around its `p`).
// solid-markdown drops those with its own `child.value !== "\n"` guard, but
// this plugin runs FIRST, and a `br` is an element, so it sails past that guard
// — into a parent where phrasing content is illegal (browsers foster-parent a
// `br` out of table internals) or simply as a blank line of height the
// virtualizer then measures.
//
// What separates the two cases is the SIBLINGS, not the parent: `li` and
// `blockquote` are dual-mode (phrasing in a tight list, blocks in a loose one),
// so no parent-tag list can decide it. A bare `"\n"` sitting next to a
// block-level element is layout whitespace; anything else is prose. Keying on
// the value ALONE would be wrong — `"**T4**\n**T5**"` yields a bare `"\n"`
// between the two `strong`s, and that one is a genuine softbreak.
//
// This is the SECOND pass of the rehype pipeline, run AFTER `rehypeInertRaw`.
// Because that prior pass already retyped every `raw` node to `text`, this pass
// operates on a tree with no `raw` nodes left — it needs no inline retype of
// its own; a multi-line raw block reaches it as an ordinary `\n`-bearing text
// node and splits like any other prose.

import type {
	Parent as HastParent,
	RootContent as HastRootContent,
} from "hast";

/** The block-level tags mdast→hast can emit, read off its handlers rather than
 *  off HTML's block list. `section` is the GFM footnote block; `dl`/`dt`/`dd`
 *  are absent because no definition-list handler exists, and `tfoot` because
 *  the table handler builds only `thead`/`tbody`.
 *
 *  `div` is here but unreachable today: its only emitter is
 *  `defaultUnknownHandler`, for an unknown mdast node carrying `data.hChildren`
 *  /`hProperties`, which nothing in this pipeline produces. It is NOT what raw
 *  HTML becomes — raw HTML stays a `raw` node and `rehypeInertRaw` inerts it to
 *  text, which is the invariant the header above depends on. Kept as insurance
 *  against a future remark plugin that sets `hName`, where a missing entry
 *  would resurface as a stray break.
 *
 *  A bare `"\n"` adjacent to one of these is inter-block whitespace, never a
 *  line break the reader asked for. */
const BLOCK_TAGS: Record<string, true> = {
	blockquote: true,
	div: true,
	h1: true,
	h2: true,
	h3: true,
	h4: true,
	h5: true,
	h6: true,
	hr: true,
	li: true,
	ol: true,
	p: true,
	pre: true,
	section: true,
	table: true,
	tbody: true,
	td: true,
	th: true,
	thead: true,
	tr: true,
	ul: true,
};

/** A rehype attacher that rescues prose softbreaks: it drops bare `"\n"`
 *  layout-whitespace between blocks and splits remaining `\n`-bearing prose
 *  text nodes into interleaved `br` elements, skipping code subtrees. A
 *  zero-arg attacher (`() => transformer`), so it drops straight into the
 *  `rehypePlugins` array as a bare entry beside `rehypeInertRaw`. */
export function rehypeProseBreaks() {
	const isBlock = (child: HastRootContent | undefined): boolean =>
		child?.type === "element" && BLOCK_TAGS[child.tagName] === true;
	// A hard break arrives as a PAIR — mdast-util-to-hast's break handler emits
	// `[br, text("\n")]` — so the trailing newline is that `br`'s own source
	// formatting, already rendered. Splitting it would emit a SECOND `br` and
	// double the gap the author asked for. Only a PRECEDING `br` absorbs it: a
	// `"\n"` BEFORE one is a genuine softbreak running into a hard break.
	const isBr = (child: HastRootContent | undefined): boolean =>
		child?.type === "element" && child.tagName === "br";
	// `inCode` is INHERITED: a code subtree is verbatim all the way down.
	const visit = (node: HastParent, inCode: boolean) => {
		const children = node.children;
		const out: HastRootContent[] = [];
		for (const [i, child] of children.entries()) {
			// A bare `"\n"` between blocks is layout whitespace: drop it, exactly as
			// solid-markdown's own renderer would have.
			if (
				child.type === "text" &&
				child.value === "\n" &&
				(isBlock(children[i - 1]) ||
					isBlock(children[i + 1]) ||
					isBr(children[i - 1]))
			) {
				continue;
			}
			if (!inCode && child.type === "text" && child.value.includes("\n")) {
				// Split on the newline and interleave `br`, so the break survives
				// `white-space: normal`. Empty segments (a leading/trailing newline)
				// contribute no text node, only the break.
				const parts = child.value.split("\n");
				parts.forEach((part, partIndex) => {
					if (partIndex > 0) {
						out.push({
							type: "element",
							tagName: "br",
							properties: {},
							children: [],
						});
					}
					if (part !== "") out.push({ type: "text", value: part });
				});
				continue;
			}
			if (child.type === "element")
				visit(
					child,
					inCode || child.tagName === "code" || child.tagName === "pre",
				);
			out.push(child);
		}
		node.children = out;
	};
	return (tree: HastParent) => {
		visit(tree, false);
	};
}
