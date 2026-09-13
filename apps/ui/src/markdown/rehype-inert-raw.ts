// Raw-HTML inertion (markdown design A3/R2). solid-markdown pipes `allowDangerousHtml`,
// so any `<…>` becomes a hast `raw` node the renderer ignores, rendering as NOTHING.
// Retyping each `raw` node as `text` renders the source verbatim WITHOUT making it live
// — do NOT use `rehype-raw`, which would hand agent-authored `<script>` a live DOM.

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
