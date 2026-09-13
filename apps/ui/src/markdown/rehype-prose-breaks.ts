// Prose softbreak rescue (markdown design A3/R2). A single newline is embedded in a
// longer text node, and `white-space: normal` collapses it so words join; splitting on
// `\n` into real `br` elements restores it — PROSE ONLY (destructive in `pre`/`code`,
// illegal between block children). What separates the cases is the SIBLINGS, not the parent.

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
	// A hard break arrives as a PAIR — `[br, text("\n")]` — so the trailing newline is the
	// `br`'s own formatting, already rendered; splitting it would double the gap. Only a
	// PRECEDING `br` absorbs it: a `"\n"` BEFORE one is a softbreak running into a hard break.
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
