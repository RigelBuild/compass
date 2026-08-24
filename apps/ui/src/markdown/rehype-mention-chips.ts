// Consumer-side @-mention chip injection (markdown design A3/R2). Under the
// react-markdown-10 fork the `Components` map is HTML-tags-only — there is no
// `text` component key to route prose text through (hast text nodes render as
// raw strings inside `hast-util-to-jsx-runtime`, never through `components`).
// So mention chips are injected at the hast stage instead: this rehype plugin
// walks the tree AFTER `rehypeInertRaw` and `rehypeProseBreaks` and, for every
// text node NOT inside a code subtree, splits it through the existing pure
// `mentionRuns` splitter and replaces it with an interleaved sequence of text
// nodes and `span.mention-chip` elements — so chips are real hast elements
// before `toJsxRuntime` and render with zero component overrides.
//
// Ordering matters (design A3): this runs AFTER `rehypeInertRaw`, which retypes
// `raw` HTML nodes to `text`, so raw-HTML prose text chips like any other prose
// text — matching the old `text`-override behavior — and after
// `rehypeProseBreaks`, which has already rescued softbreaks into `br`.

import type {
	Element as HastElement,
	Parent as HastParent,
	RootContent as HastRootContent,
	Text as HastText,
} from "hast";
import type { Account } from "../comms-stub";
import { mentionRuns } from "./mention-runs";

/** The class list a mention chip carries, mirroring exactly what the old
 *  `MentionRuns` component emitted (`mention-chip` with `reserved` / `unknown`
 *  modifiers) so the `.mention-chip` CSS rules style both identically. A known,
 *  non-reserved mention is the bare `mention-chip`. */
function chipClasses(mention: { known: boolean; reserved: boolean }): string[] {
	const classes = ["mention-chip"];
	if (mention.reserved) classes.push("reserved");
	else if (!mention.known) classes.push("unknown");
	return classes;
}

/** Split one prose text node into interleaved text nodes and chip `span`
 *  elements. A run with no mention becomes a plain text node; a mention run
 *  becomes `<span class="mention-chip …">@handle</span>`. Returns the original
 *  node untouched (as a single-element array) when the text carries no
 *  mention, so a mention-free tree is structurally unchanged. */
function splitTextNode(
	node: HastText,
	byHandle: Map<string, Account>,
): HastRootContent[] {
	const runs = mentionRuns(node.value, byHandle);
	if (runs.length <= 1 && !runs[0]?.mention) return [node];
	return runs.map((run) =>
		run.mention
			? ({
					type: "element",
					tagName: "span",
					properties: { className: chipClasses(run.mention) },
					children: [{ type: "text", value: run.text }],
				} satisfies HastElement)
			: ({ type: "text", value: run.text } satisfies HastText),
	);
}

/** A rehype plugin factory that injects @-mention chips into prose text.
 *  `byHandle` resolves each mention's known/reserved state. Returns a unified
 *  ATTACHER (`() => transformer`) — the shape `rehypePlugins` expects for a bare
 *  entry, so `rehypeMentionChips({ byHandle })` drops straight into the array
 *  beside `rehypeInertRaw`/`rehypeProseBreaks` (each itself a zero-arg
 *  attacher). Code subtrees are skipped verbatim (a `@handle` inside
 *  `code`/`pre` never chips), inheriting the `inCode` flag down the tree
 *  exactly as `rehypeProseBreaks` does. The visitor never descends into the chip
 *  spans it injects (it builds a fresh children array from the split output
 *  rather than re-walking it), so an injected `@handle` is never re-chipped. */
export function rehypeMentionChips(options: {
	byHandle: Map<string, Account>;
}): () => (tree: HastParent) => void {
	const { byHandle } = options;
	const visit = (node: HastParent, inCode: boolean): void => {
		const out: HastRootContent[] = [];
		for (const child of node.children) {
			if (!inCode && child.type === "text") {
				out.push(...splitTextNode(child, byHandle));
				continue;
			}
			if (child.type === "element") {
				visit(
					child,
					inCode || child.tagName === "code" || child.tagName === "pre",
				);
			}
			out.push(child);
		}
		node.children = out;
	};
	return () => (tree: HastParent) => {
		visit(tree, false);
	};
}
