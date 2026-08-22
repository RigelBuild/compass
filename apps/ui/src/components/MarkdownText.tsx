import Markdown, { type Components } from "@rigelbuild/solid-markdown";
import { Browser } from "@wailsio/runtime";
import type {
	Element as HastElement,
	Parent as HastParent,
	RootContent as HastRootContent,
} from "hast";
import remarkGfm from "remark-gfm";
import {
	type Component,
	createEffect,
	createMemo,
	createSignal,
	Show,
} from "solid-js";
import type { Account } from "../comms-stub";
import {
	getCachedHighlight,
	setCachedHighlight,
} from "../markdown/highlight-cache";
import { highlightToHtml } from "../markdown/highlighter";
import { rehypeMentionChips } from "../markdown/rehype-mention-chips";
import { safeHref } from "../safe-url";

/** `@rigelbuild/solid-markdown` pipes `remarkRehype` with
 *  `allowDangerousHtml: true` (dist/index.jsx:14), so any `<…>` in a message
 *  becomes a hast `raw` node — and its renderer, `hast-util-to-jsx-runtime`,
 *  handles only `element`, `text`, and `mdxJsx*`/`comment` nodes: a `raw` node
 *  is ignored and therefore renders as NOTHING. `"use Vec<T>"` loses `<T>`, and
 *  a `<details>` block loses its whole body. Since `<` is a raw-HTML opener to
 *  CommonMark, ordinary agent prose about generics or JSX silently loses
 *  characters.
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
// `openExternal` seam (the Wails runtime's `Browser.OpenURL` inside the shell, a
// plain `window.open` in the browser dev build) instead of navigating the app;
// images render as their alt text
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

/** Open a link OUTSIDE the app — never as an in-page navigation. The single seam
 *  the `a` override routes activation through, with the framework dependency
 *  (`@wailsio/runtime`) confined here (design §A2 / DL-109 zone discipline).
 *
 *  Inside the Wails shell (`window._wails.environment` is injected by the native
 *  runtime, prod and dev alike) the OS default browser opens via
 *  `Browser.OpenURL`; in the plain-browser dev build — where no Wails runtime is
 *  reachable and calling it would throw — a `window.open` with the same
 *  bypass-proofing (`noreferrer,noopener`) opens a new tab. The caller has
 *  already scheme-sanitized the href (`safeHref`), so this only ever receives a
 *  vetted absolute URL. */
function isNativeShell(): boolean {
	// The native Wails runtime injects `window._wails.environment` into the page
	// (prod and dev alike, internal/runtime/runtime_{prod,dev}.go); a plain
	// browser build has no such object. Narrow with `in`/`typeof` so the property
	// read is actually checked rather than asserted onto `window`.
	if (typeof window === "undefined") return false;
	if (!("_wails" in window)) return false;
	const wails = window._wails;
	return typeof wails === "object" && wails !== null && "environment" in wails;
}

function openExternal(href: string): void {
	if (isNativeShell()) {
		void Browser.OpenURL(href).catch(() => {});
		return;
	}
	window.open(href, "_blank", "noreferrer,noopener");
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

/** How long a streaming code fence must be quiet before it is highlighted.
 *  Long enough to collapse a burst of growth ticks into one pass, short enough
 *  that a settled block colorizes without a perceptible wait. */
const HIGHLIGHT_DEBOUNCE_MS = 150;

/** Read the language tag off a `code` element's `language-…` class (the
 *  remark-rehype convention); empty for an untagged block. */
function langOf(codeClass: string | undefined): string {
	const m = /language-([\w-]+)/.exec(codeClass ?? "");
	return m ? m[1] : "";
}

/** The block code path — owned by the `pre` override (design A4). Renders the
 *  fenced block's raw text verbatim (so `@handle` inside code never chips), then
 *  swaps in Shiki's highlighted markup when the async highlighter resolves.
 *  Plain text shows immediately; a cache hit (R1) paints highlighted markup on
 *  the first frame with no fallback flash. The swap is last-write-wins keyed by
 *  the code text: a streaming fence re-renders per growth tick, each kicking a
 *  fresh highlight, and a stale resolution for superseded text is ignored
 *  because a newer effect run has already moved `settled` past it. */
function BlockCode(props: { code: string; codeClass?: string }) {
	const lang = () => langOf(props.codeClass);
	// R1 synchronous seed: a rebuilt instance of an already-highlighted block
	// reads the cache at construction and paints highlighted markup immediately,
	// so #44's per-tick teardown never flashes the plain fallback for a block we
	// have already tokenized.
	const [html, setHtml] = createSignal<string | null>(
		getCachedHighlight(lang(), props.code) ?? null,
	);
	// The debounced highlight source. A streaming fence changes `props.code` on
	// every growth tick, and highlighting is O(text), so re-tokenizing the whole
	// block each tick makes a growing block quadratic; sampling the text once it
	// has been quiet briefly collapses that to a handful of passes. The plain
	// `<pre>` fallback (or an R1 cache hit) stays visible until one resolves.
	const [settled, setSettled] = createSignal<readonly [string, string] | null>(
		null,
	);
	// Static-dep effect: track code/lang in the compute phase, keep the
	// setTimeout/cleanup cycle in the (untracked) apply phase so it only re-runs
	// when a tracked source changes. Solid 2's two-arg createEffect returns its
	// cleanup from the apply phase.
	createEffect(
		() => [props.code, lang()] as const,
		([nextCode, nextLang]) => {
			const t = setTimeout(
				() => setSettled([nextCode, nextLang] as const),
				HIGHLIGHT_DEBOUNCE_MS,
			);
			return () => clearTimeout(t);
		},
	);
	// The async highlight, driven off the debounced `settled` source. Replaces
	// Solid 1's `createResource`: a two-arg effect awaits the highlighter and
	// writes the resolved markup into the signal (and the R1 cache). A failed
	// highlight (stale Vite chunk after a redeploy, offline webview) is swallowed
	// so the block degrades to the plain `<pre>` fallback rather than throwing —
	// there is no ErrorBoundary on this surface, so an unhandled rejection would
	// unmount the whole window. A settled source that arrives while an earlier
	// highlight is still in flight starts a newer run; `disposed` gates the stale
	// resolution out so last-write-wins holds.
	createEffect(
		() => settled(),
		(src) => {
			if (!src) return;
			const [text, language] = src;
			if (language === "") return;
			const cached = getCachedHighlight(language, text);
			if (cached !== undefined) {
				setHtml(cached);
				return;
			}
			let disposed = false;
			void (async () => {
				try {
					const rendered = await highlightToHtml(text, language);
					if (disposed || rendered === null) return;
					setCachedHighlight(language, text, rendered);
					setHtml(rendered);
				} catch {
					// degrade to the plain fallback
				}
			})();
			return () => {
				disposed = true;
			};
		},
	);

	// A block owns exactly one `<pre>` — either the plain fallback's
	// `<pre class="code-block"><code>` or Shiki's own `<pre>` after the async
	// swap. This component IS the `pre` override's body, so there is never a
	// `<pre>` nested in a `<pre>`.
	return (
		<Show
			when={html()}
			fallback={
				<pre class="code-block">
					<code class={props.codeClass}>{props.code}</code>
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
	);
}

/** Narrow a react-markdown-10 override prop to a plain string. #44 types
 *  override props with the full JSX attribute union (`href`/`alt` widen to
 *  `string | SerializableAttributeValue | false | undefined`, `class` to
 *  `ClassValue | false`), but the values hast actually delivers are strings.
 *  Everything non-string (the serializable-object / `false` remove-sentinel
 *  cases, unreachable here) collapses to `undefined`. */
function stringAttr(value: unknown): string | undefined {
	return typeof value === "string" ? value : undefined;
}

/** The inline `code` path (design A4). Under react-markdown-10 no `inline` prop
 *  is passed and block-vs-inline is structural — a block is always
 *  `<pre><code class="language-…">`, owned by the `pre` override below. This
 *  override therefore renders inline code ONLY: the raw text verbatim (so
 *  `@handle` inside code never chips), no language, no highlight. It MUST stay
 *  computation-free (no effect/resource/timer): the block path's `pre` renders
 *  from raw text and never mounts this inner `code`'s output, and that
 *  discard-safety depends on this override doing no reactive work. */
function InlineCode(props: { node?: HastElement; class?: string }) {
	return <code class={props.class}>{rawText(props.node)}</code>;
}

/** The block `pre` path (design A4). react-markdown-10's `pre` override receives
 *  the block's `<pre>` node; its single child is the `<code class="language-…">`
 *  element. This finds that child, reads its language class + raw text, and
 *  hands them to `BlockCode` (the debounced Shiki body + R1 cache). It renders
 *  from raw text and never renders `p.children`, so the inner `code` override's
 *  output is never mounted and no `<pre>` nests. */
function PreBlock(props: { node?: HastElement }) {
	const codeEl = () =>
		props.node?.children?.find(
			(c): c is HastElement => c.type === "element" && c.tagName === "code",
		);
	const code = () => rawText(codeEl());
	const codeClass = () => {
		const cls = codeEl()?.properties?.className;
		return Array.isArray(cls) ? cls.join(" ") : (cls as string | undefined);
	};
	return <BlockCode code={code()} codeClass={codeClass()} />;
}

/** The markdown-first renderer. */
export const MarkdownText: Component<{
	text: string;
	byHandle: Map<string, Account>;
}> = (props) => {
	// Chips are injected by a rehype plugin (design A3), not a `text` component
	// override — react-markdown-10 renders hast text nodes as raw strings, never
	// routing them through `components`, so the old `text` override would never
	// fire. Ordered AFTER `rehypeInertRawAndBreaks` so retyped raw-HTML text
	// chips like any prose. Memoized on `byHandle`: #44's processor is a memo
	// over its options, so a new map identity re-creates the processor and
	// re-parses, refreshing chip known/reserved state (coarser than the old
	// per-chip live resolution — an accepted tradeoff, account-map changes are
	// rare and a full re-parse is what every streaming tick already costs).
	const rehypePlugins = createMemo(() => [
		rehypeInertRawAndBreaks,
		rehypeMentionChips({ byHandle: props.byHandle }),
	]);
	const components: Components = {
		// Inline code verbatim — mentions never chip here (A4).
		code: (p) => <InlineCode node={p.node} class={stringAttr(p.class)} />,
		// The block `<pre>` owns the block code path (plain or Shiki); it renders
		// from its `code` child's raw text and never mounts `p.children`, so no
		// `<pre>` nests and the inline `code` override's output is discarded (A4).
		pre: (p) => <PreBlock node={p.node} />,
		// Links open externally via the `openExternal` seam (Wails `Browser.OpenURL`
		// in the shell, `window.open` in the dev build); label rendered from raw
		// text (so a mention in a label never chips, and inline markup is flattened
		// — accepted).
		a: (p) => {
			const label = () => rawText(p.node);
			// Scheme-sanitize on BOTH the DOM attribute and the opener call: a
			// `javascript:`/`data:`/`file:` href must never be a live attribute
			// (middle-click / keyboard-activate / context-menu can bypass onClick
			// in a webview) nor reach `openExternal` (a `file://` would open a local
			// file in the OS default app). Unsafe → inert `#` + no-op click.
			const safe = () => safeHref(stringAttr(p.href));
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
						if (href) openExternal(href);
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
		img: (p) => (
			<span class="md-img-omitted">{imgPlaceholder(stringAttr(p.alt))}</span>
		),
	};

	return (
		<div class="msg-text markdown-content">
			<Markdown
				renderingStrategy="reconcile"
				remarkPlugins={[remarkGfm]}
				rehypePlugins={rehypePlugins()}
				components={components}
			>
				{props.text}
			</Markdown>
		</div>
	);
};
