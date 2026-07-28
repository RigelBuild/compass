import { openUrl } from "@tauri-apps/plugin-opener";
import type {
	Element as HastElement,
	Parent as HastParent,
	RootContent as HastRootContent,
	Text as HastText,
} from "hast";
import remarkGfm from "remark-gfm";
import {
	type Component,
	createEffect,
	createResource,
	createSignal,
	For,
	onCleanup,
	Show,
} from "solid-js";
import { SolidMarkdown, type SolidMarkdownComponents } from "solid-markdown";
import type { Account } from "../comms-stub";
import { highlightToHtml } from "../markdown/highlighter";
import { mentionRuns } from "../markdown/mention-runs";

/** solid-markdown pipes `remarkRehype` with `allowDangerousHtml: true`
 *  (solid-markdown/dist/index.jsx:307), so any `<…>` in a message becomes a hast
 *  `raw` node — and its child renderer has `Match` arms only for `element` and
 *  `text` (dist/index.jsx:110-125). A `raw` node therefore renders as NOTHING:
 *  `"use Vec<T>"` loses `<T>`, and a `<details>` block loses its whole body.
 *  Since `<` is a raw-HTML opener to CommonMark, ordinary agent prose about
 *  generics or JSX silently loses characters.
 *
 *  Retyping each `raw` node as `text` renders the source characters verbatim,
 *  escaped by the DOM text path. This restores the content WITHOUT making the
 *  markup live — the inertness that makes the XSS surface clean today comes
 *  from `raw` never becoming an element, and that still holds. Do NOT replace
 *  this with `rehype-raw`, which would restore the text by parsing it into real
 *  elements and hand agent-authored `<script>`/`<img onerror>` a live DOM.
 *
 *  Softbreaks are the same class of silent loss, one node type over. A single
 *  newline does NOT arrive as its own `"\n"` text node — mdast→hast leaves it
 *  EMBEDDED in a longer text node (verified: `"Done: **T4**\nT5 is next."`
 *  yields `text("Done: ") strong(T4) text("\nT5 is next.")`). The renderer emits
 *  that node's value as one DOM text node, and `.markdown-content` sets
 *  `white-space: normal` (its reset in app.css), so the newline collapses and
 *  the words join: `"T4T5 is next."`. Splitting such a node on `\n` and
 *  interleaving real `br` elements restores the line break under
 *  `white-space: normal`. That rescue is for PROSE only, and two guards keep
 *  it there.
 *
 *  Inside `pre`/`code` the newline already survives — `white-space: pre`
 *  renders it verbatim — and the split is actively destructive: the `code`
 *  override renders from `rawText`, which concatenates text descendants and
 *  ignores `br`, so an interleaved break vanishes and a multi-line block
 *  collapses onto one line (both plain and, since Shiki highlights that same
 *  string, highlighted).
 *
 *  Between BLOCK children mdast→hast also emits bare `"\n"` separator nodes
 *  (`ul`→`li`, `table`→`tr`, and inside a loose `li` around its `p`).
 *  solid-markdown drops those with its own `child.value !== "\n"` guard, but
 *  this plugin runs FIRST, and a `br` is an element, so it sails past that
 *  guard — into a parent where phrasing content is illegal (browsers
 *  foster-parent a `br` out of table internals) or simply as a blank line of
 *  height the virtualizer then measures.
 *
 *  What separates the two cases is the SIBLINGS, not the parent: `li` and
 *  `blockquote` are dual-mode (phrasing in a tight list, blocks in a loose
 *  one), so no parent-tag list can decide it. A bare `"\n"` sitting next to a
 *  block-level element is layout whitespace; anything else is prose. Keying on
 *  the value ALONE would be wrong — `"**T4**\n**T5**"` yields a bare `"\n"`
 *  between the two `strong`s, and that one is a genuine softbreak. */

/** The block-level tags mdast→hast can emit, read off its handlers rather than
 *  off HTML's block list. `section` is the GFM footnote block; `dl`/`dt`/`dd`
 *  are absent because no definition-list handler exists, and `tfoot` because
 *  the table handler builds only `thead`/`tbody`.
 *
 *  `div` is here but unreachable today: its only emitter is
 *  `defaultUnknownHandler`, for an unknown mdast node carrying `data.hChildren`
 *  /`hProperties`, which nothing in this pipeline produces. It is NOT what raw
 *  HTML becomes — raw HTML stays a `raw` node and this plugin inerts it to
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

function rehypeInertRawAndBreaks() {
	// `raw` is not part of the stock hast content union — it is contributed by
	// mdast-util-to-hast's `allowDangerousHtml` extension, which is exactly the
	// mode solid-markdown runs remarkRehype in. Widen the child type to admit it
	// rather than asserting, so the `raw` arm type-checks on its own terms.
	type RawNode = { type: "raw"; value: string };
	type Child = HastRootContent | RawNode;
	const isBlock = (child: Child | undefined): boolean =>
		child?.type === "element" && BLOCK_TAGS[child.tagName] === true;
	// A hard break arrives as a PAIR — mdast-util-to-hast's break handler emits
	// `[br, text("\n")]` — so the trailing newline is that `br`'s own source
	// formatting, already rendered. Splitting it would emit a SECOND `br` and
	// double the gap the author asked for. Only a PRECEDING `br` absorbs it: a
	// `"\n"` BEFORE one is a genuine softbreak running into a hard break.
	const isBr = (child: Child | undefined): boolean =>
		child?.type === "element" && child.tagName === "br";
	// `inCode` is INHERITED: a code subtree is verbatim all the way down.
	const visit = (node: HastParent, inCode: boolean) => {
		const children = node.children as Child[];
		const out: HastRootContent[] = [];
		for (const [i, source] of children.entries()) {
			// Inert-retype `raw` to `text` HERE, at the top of the loop, so the
			// newline rules below see it as the prose text it now is. Handling it
			// in its own arm instead would skip the split: a multi-line raw block
			// (`<div>` around two lines, or `<b>` spanning a softbreak) would reach
			// the DOM as one text node whose newlines then collapse under
			// `white-space: normal`, joining the lines onto one.
			//
			// The retype cannot disturb the `inCode` guard below, because a `raw`
			// node never reaches a code subtree: fenced and indented code become
			// mdast `code` nodes and only an mdast `html` node becomes `raw`
			// (mdast-util-to-hast handlers/html.js), so the two are disjoint by
			// construction. That guard exists for plain text, which is verbatim in
			// code and whose `br` `rawText` would eat.
			//
			// The lookarounds below read the UNCONVERTED neighbours, which is
			// harmless: `isBlock` and `isBr` both require `type === "element"`, and
			// the retype only maps `"raw"` to `"text"`, so neither predicate can
			// change its answer.
			const child: Child =
				source.type === "raw" ? { type: "text", value: source.value } : source;
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

// The message-surface renderer: renders a text block as markdown (CommonMark +
// GFM) and composes the existing @-mention chips by post-processing the markdown
// tree's TEXT nodes (markdown-first, so emphasis spanning a mention survives and
// a mention inside code stays verbatim). Code is highlighted via the lazy Shiki
// singleton with an immediate plain fallback; links open externally through the
// Tauri opener instead of navigating the app; images render as their alt text
// and are never fetched. A growing string renders a valid partial every step:
// CommonMark closes an unterminated fence implicitly at end of input, so a
// half-streamed fence already parses as a growing code block, and `reconcile`
// keeps the same DOM nodes across ticks (so selection and scroll survive).

/** The visible stand-in for an image, which this surface never renders as an
 *  element (no `src` ever reaches the DOM, so nothing is fetched) — the alt
 *  text keeps the sentence around it readable. */
function imgPlaceholder(alt: string | undefined): string {
	return alt ? `[image: ${alt}]` : "[image]";
}

/** The concatenated raw text of a hast element's descendants — the verbatim
 *  source for code and link labels, so the mention override never descends into
 *  either. For a code element this is the single text child's value; for a link
 *  label with inline markup (`[**@cook**](url)`) it flattens the nested text
 *  (emphasis intentionally dropped — an accepted tradeoff of rendering labels
 *  from raw text).
 *
 *  An `img` descendant contributes its placeholder rather than nothing: a label
 *  that is ONLY an image (`[![alt](i.png)](url)` — a badge, common in agent
 *  prose) would otherwise flatten to `""` and render a zero-width but clickable
 *  anchor carrying a live external href. The `img` override never sees it,
 *  because this label path does not render children. */
function rawText(node: HastElement | undefined): string {
	if (!node?.children) return "";
	let out = "";
	for (const child of node.children) {
		if (child.type === "text") out += child.value;
		else if (child.type === "element")
			out +=
				child.tagName === "img"
					? imgPlaceholder(child.properties?.alt as string | undefined)
					: rawText(child);
	}
	return out;
}

/** One prose text run rendered through the shared mention splitter: plain runs
 *  as text, mentions as `mention-chip` spans carrying the `reserved`/`unknown`
 *  modifiers (the `.mention-chip` rules in app.css). This is the `text`
 *  component override, so it fires on every literal prose text node in the
 *  markdown tree — never on code (rendered from raw value) or link labels
 *  (same). */
function MentionRuns(props: { text: string; byHandle: Map<string, Account> }) {
	const runs = () => mentionRuns(props.text, props.byHandle);
	return (
		<For each={runs()}>
			{(run) => (
				<Show when={run.mention} fallback={run.text}>
					{(m) => (
						<span
							class="mention-chip"
							classList={{
								reserved: m().reserved,
								unknown: !m().reserved && !m().known,
							}}
						>
							{run.text}
						</span>
					)}
				</Show>
			)}
		</For>
	);
}

/** How long a streaming code fence must be quiet before it is highlighted.
 *  Long enough to collapse a burst of growth ticks into one pass, short enough
 *  that a settled block colorizes without a perceptible wait. */
const HIGHLIGHT_DEBOUNCE_MS = 150;

/** The `code` override — inline and block. Renders the code's raw text verbatim
 *  (so `@handle` inside code never chips), then swaps in Shiki's highlighted
 *  markup when the async highlighter resolves. Plain text shows immediately;
 *  the swap is last-write-wins keyed by the code text identity (a streaming
 *  fence re-renders per growth tick, each kicking a fresh highlight — a stale
 *  resolution for superseded text is dropped by createResource's source keying).
 *  Inline code stays plain (no language, no highlight). */
function CodeBlock(props: {
	node?: HastElement;
	inline?: boolean;
	class?: string;
}) {
	const code = () => rawText(props.node);
	// The language tag lives in the code element's class as `language-xxx`
	// (remark-rehype convention); absent for inline or untagged blocks.
	const lang = () => {
		const cls = props.class ?? "";
		const m = /language-([\w-]+)/.exec(cls);
		return m ? m[1] : "";
	};
	// Only block code with a language tag is highlighted, and the source is
	// DEBOUNCED: a streaming fence changes `code()` on every growth tick, and
	// highlighting is O(text), so re-tokenizing the whole block each tick makes a
	// growing block quadratic (measured: 30 ticks of a 465-char block =
	// 30 highlights over 6,465 chars). Sampling the text once it has been quiet
	// briefly collapses that to a handful of passes; the plain `<pre>` fallback
	// stays visible until one resolves, so nothing regresses visually.
	const [settled, setSettled] = createSignal<readonly [string, string] | null>(
		null,
	);
	createEffect(() => {
		if (props.inline) return;
		const next = [code(), lang()] as const;
		const t = setTimeout(() => setSettled(next), HIGHLIGHT_DEBOUNCE_MS);
		onCleanup(() => clearTimeout(t));
	});
	const [html] = createResource(
		() => (props.inline ? null : settled()),
		async (src) => {
			if (!src) return null;
			const [text, language] = src;
			if (language === "") return null;
			// A rejected fetcher is RETHROWN by createResource when the accessor is
			// read during render, and the app mounts no ErrorBoundary — so an
			// unhandled rejection here unmounts the whole window, not just this
			// block. A failed highlight (stale Vite chunk after a redeploy, offline
			// webview) must degrade to the plain `<pre>` fallback below.
			try {
				return await highlightToHtml(text, language);
			} catch {
				return null;
			}
		},
	);

	// Inline code is a bare `<code>`; a block owns exactly one `<pre>` — either
	// the plain fallback's `<pre class="code-block"><code>` or Shiki's own `<pre>`
	// after the async swap. The `pre` override is a pass-through (no element), so
	// there is never a `<pre>` nested in a `<pre>`.
	return (
		<Show
			when={!props.inline}
			fallback={<code class={props.class}>{code()}</code>}
		>
			<Show
				when={html()}
				fallback={
					<pre class="code-block">
						<code class={props.class}>{code()}</code>
					</pre>
				}
			>
				{(rendered) => (
					// Shiki emits its own `<pre class="shiki"><code>…tokens…</code></pre>`;
					// set it as innerHTML on a wrapper. Safe: the HTML is generated from
					// Shiki's token AST (no user HTML passthrough), rendered into a leaf
					// with no reactive children. This is the one sink on this surface
					// where markup goes live, and its safety rests ENTIRELY on Shiki
					// escaping metacharacters in code content — so if the highlighter
					// ever gains a transformer or any pass-through path, re-review here
					// before shipping it.
					<div class="code-highlight" innerHTML={rendered()} />
				)}
			</Show>
		</Show>
	);
}

/** Link schemes safe to render as a live `href` and hand to the external opener.
 *  solid-markdown's default `transformLinkUri` is `null` (it does NOT strip
 *  dangerous schemes like react-markdown does), so agent-authored
 *  `[x](javascript:…)` / `data:` / `file:` would otherwise reach the DOM and
 *  `openUrl` verbatim. `new URL` parses `javascript:` fine — the protocol check,
 *  not the throw, is the gate; the catch drops relative/malformed hrefs. */
const SAFE_LINK_SCHEMES = new Set(["http:", "https:", "mailto:"]);

/** `href` if its scheme is safe to render and open externally, else null (the
 *  link renders inert: `href="#"` + a no-op click). */
function safeHref(href: string | undefined): string | null {
	if (!href) return null;
	try {
		return SAFE_LINK_SCHEMES.has(new URL(href).protocol) ? href : null;
	} catch {
		return null;
	}
}

/** The markdown-first renderer. */
export const MarkdownText: Component<{
	text: string;
	byHandle: Map<string, Account>;
}> = (props) => {
	const components: SolidMarkdownComponents = {
		// Prose text → mention chips (the composition seam).
		text: (p: { node: HastText }) => (
			<MentionRuns text={p.node.value} byHandle={props.byHandle} />
		),
		// Code verbatim + async highlight — mentions never chip here.
		code: (p: { node?: HastElement; inline?: boolean; class?: string }) => (
			<CodeBlock node={p.node} inline={p.inline} class={p.class} />
		),
		// The `code` override owns the block's single `<pre>` (plain or Shiki), so
		// this `pre` override is a pass-through — rendering its `<code>` child
		// directly avoids a `<pre>` nested inside a `<pre>`.
		pre: (p: { children?: unknown }) => <>{p.children as never}</>,
		// Links open externally via the Tauri opener; label rendered from raw text
		// (so a mention in a label never chips, and inline markup is flattened —
		// accepted). Until the Tauri shell registers the opener capability, openUrl
		// rejects → the link is inert (intercepted, no nav).
		a: (p: { node?: HastElement; href?: string }) => {
			const label = () => rawText(p.node);
			// Scheme-sanitize on BOTH the DOM attribute and the opener call: a
			// `javascript:`/`data:`/`file:` href must never be a live attribute
			// (middle-click / keyboard-activate / context-menu can bypass onClick
			// in a webview) nor reach `openUrl` (a `file://` would open a local
			// file in the OS default app). Unsafe → inert `#` + no-op click.
			const safe = () => safeHref(p.href);
			return (
				<a
					href={safe() ?? "#"}
					// The onClick below is the normal path, but a middle-click,
					// keyboard activation, or context-menu "open" bypasses it and lets
					// the live href act. `target`/`rel` bound that bypass: it cannot
					// replace the app document, and the remote page gets no
					// `window.opener` handle and no referrer.
					target={safe() ? "_blank" : undefined}
					rel={safe() ? "noreferrer noopener" : undefined}
					onClick={(e) => {
						e.preventDefault();
						const href = safe();
						if (href) void openUrl(href).catch(() => {});
					}}
				>
					{label()}
				</a>
			);
		},
		// An `img` renders as its alt text, never as an element: no `src` is ever
		// emitted, so the no-remote-fetch / no-tracking-pixel guarantee is the same
		// as dropping it — but the sentence keeps its meaning instead of showing a
		// hole. (Filtering it out via `disallowedElements` splices the node and its
		// alt text away entirely.)
		img: (p: { alt?: string }) => (
			<span class="md-img-omitted">{imgPlaceholder(p.alt)}</span>
		),
	};

	return (
		<div class="msg-text markdown-content">
			<SolidMarkdown
				renderingStrategy="reconcile"
				remarkPlugins={[remarkGfm]}
				rehypePlugins={[rehypeInertRawAndBreaks]}
				components={components}
			>
				{props.text}
			</SolidMarkdown>
		</div>
	);
};
