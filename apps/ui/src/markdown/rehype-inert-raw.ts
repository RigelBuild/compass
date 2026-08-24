// Raw-HTML inertion (markdown design A3/R2). `@rigelbuild/solid-markdown` pipes
// `remarkRehype` with `allowDangerousHtml: true` (dist/index.jsx:14), so any
// `<…>` in a message becomes a hast `raw` node — and its renderer,
// `hast-util-to-jsx-runtime`, handles only `element`, `text`, and
// `mdxJsx*`/`comment` nodes: a `raw` node is ignored and therefore renders as
// NOTHING. `"use Vec<T>"` loses `<T>`, and a `<details>` block loses its whole
// body. Since `<` is a raw-HTML opener to CommonMark, ordinary agent prose
// about generics or JSX silently loses characters.
//
// Retyping each `raw` node as `text` renders the source characters verbatim,
// escaped by the DOM text path. This restores the content WITHOUT making the
// markup live — the inertness that makes the XSS surface clean today comes from
// `raw` never becoming an element, and that still holds. Do NOT replace this
// with `rehype-raw`, which would restore the text by parsing it into real
// elements and hand agent-authored `<script>`/`<img onerror>` a live DOM.
//
// This is the FIRST pass of the rehype pipeline: an unconditional whole-tree
// retype that reaches EVERY node (there is no `inCode` guard — a `raw` node
// never actually occurs inside a code subtree, since fenced/indented code
// becomes an mdast `code` node and only an mdast `html` node becomes `raw`, so
// the retype is a no-op there rather than a hazard). The guarded softbreak
// rescue is a SEPARATE second pass (`rehypeProseBreaks`), which sees a tree
// with no `raw` nodes left.

import type {
	Parent as HastParent,
	RootContent as HastRootContent,
} from "hast";

/** A rehype attacher that retypes every `raw` HTML node to a `text` node,
 *  in place, across the whole tree. A zero-arg attacher (`() => transformer`),
 *  so it drops straight into the `rehypePlugins` array as a bare entry. */
export function rehypeInertRaw() {
	// `raw` is not part of the stock hast content union — it is contributed by
	// mdast-util-to-hast's `allowDangerousHtml` extension, which is exactly the
	// mode solid-markdown runs remarkRehype in. Widen the child type to admit it
	// rather than asserting, so the `raw` arm type-checks on its own terms.
	type RawNode = { type: "raw"; value: string };
	type Child = HastRootContent | RawNode;
	const visit = (node: HastParent) => {
		const children = node.children as Child[];
		const out: HastRootContent[] = [];
		for (const source of children) {
			const child: HastRootContent =
				source.type === "raw" ? { type: "text", value: source.value } : source;
			if (child.type === "element") visit(child);
			out.push(child);
		}
		node.children = out;
	};
	return (tree: HastParent) => {
		visit(tree);
	};
}
