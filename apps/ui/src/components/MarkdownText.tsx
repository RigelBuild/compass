import Markdown, { type Components } from "@rigelbuild/solid-markdown";
import type { Element as HastElement } from "hast";
import remarkGfm from "remark-gfm";
import {
	type Component,
	createEffect,
	createMemo,
	createSignal,
	Show,
} from "solid-js";
import type { Account } from "../comms-stub";
import { useRefLinkRules } from "../context";
import {
	getCachedHighlight,
	isLeadingEdgeHighlight,
	setCachedHighlight,
} from "../markdown/highlight-cache";
import { highlightToHtml } from "../markdown/highlighter";
import { rehypeInertRaw } from "../markdown/rehype-inert-raw";
import { rehypeMentionChips } from "../markdown/rehype-mention-chips";
import { rehypeProseBreaks } from "../markdown/rehype-prose-breaks";
import { openExternal } from "../open-external";
import { remarkRefLinks } from "../refs/remark-ref-links";
import { safeHref } from "../safe-url";

// Render markdown with safe links and images, mention chips, and streaming code highlights.
/** The visible stand-in for an image, which this surface never renders as an
 *  element (no `src` ever reaches the DOM, so nothing is fetched) — the alt
 *  text keeps the sentence around it readable. */
function imgPlaceholder(alt: string | undefined): string {
	return alt ? `[image: ${alt}]` : "[image]";
}

/** Concatenate descendant text for code and link labels; bypass mention rendering.
 * Image placeholders keep image-only links visible without rendering an image. */
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
	// Seed rebuilt blocks from the module cache to avoid flashing the plain fallback.
	const [html, setHtml] = createSignal<string | null>(
		getCachedHighlight(lang(), props.code) ?? null,
	);
	// Debounce streaming fences to avoid O(n²) re-tokenization; keep the plain fallback until a highlight resolves.
	const [settled, setSettled] = createSignal<readonly [string, string] | null>(
		null,
	);
	// Highlight fresh fences immediately and debounce growth; module-level state distinguishes
	// reconstructed streaming blocks from new fences to avoid per-tick re-tokenization.
	const immediate = isLeadingEdgeHighlight(
		lang(),
		props.code,
		HIGHLIGHT_DEBOUNCE_MS,
	);
	createEffect(
		() => [props.code, lang()] as const,
		([nextCode, nextLang]) => {
			if (immediate) {
				setSettled([nextCode, nextLang] as const);
				return;
			}
			// biome-ignore lint/style/noRestrictedGlobals: production UI debounce (HIGHLIGHT_DEBOUNCE_MS), cleared on re-run; not a test wait
			const t = setTimeout(
				() => setSettled([nextCode, nextLang] as const),
				HIGHLIGHT_DEBOUNCE_MS,
			);
			return () => clearTimeout(t);
		},
	);
	// Failed imports keep the plain fallback; ignore stale resolutions so the latest source wins.
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
				// Keep user HTML out of this sink; safety depends on Shiki escaping code.
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
	// Retype raw HTML before softbreak conversion; add mention chips last so raw text is included.
	// Memoize by account-map identity so changes re-parse and refresh known/reserved handles.
	const refLinkRules = useRefLinkRules();
	const remarkPlugins = createMemo(() => {
		const rules = refLinkRules();
		return [remarkGfm, remarkRefLinks(() => rules)];
	});
	const rehypePlugins = createMemo(() => [
		rehypeInertRaw,
		rehypeProseBreaks,
		rehypeMentionChips({ byHandle: props.byHandle }),
	]);
	const components: Components = {
		// Inline code verbatim — mentions never chip here (A4).
		code: (p) => <InlineCode node={p.node} class={stringAttr(p.class)} />,
		// The block `<pre>` owns the block code path (plain or Shiki); it renders
		// from its `code` child's raw text and never mounts `p.children`, so no
		// `<pre>` nests and the inline `code` override's output is discarded (A4).
		pre: (p) => <PreBlock node={p.node} />,
		// Open links externally and flatten labels so mention markup stays inert.
		a: (p) => {
			const label = () => rawText(p.node);
			// Sanitize href and opener: context actions bypass onClick, and file: can launch local files.
			const safe = () => safeHref(stringAttr(p.href));
			return (
				<a
					href={safe() ?? "#"}
					// Prevent context actions from replacing the app or exposing opener/referrer.
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
		// Render alt text instead of an image, preserving meaning without fetching remote content.
		img: (p) => (
			<span class="md-img-omitted">{imgPlaceholder(stringAttr(p.alt))}</span>
		),
	};

	return (
		<div class="msg-text markdown-content">
			<Markdown
				renderingStrategy="reconcile"
				remarkPlugins={remarkPlugins()}
				rehypePlugins={rehypePlugins()}
				components={components}
			>
				{props.text}
			</Markdown>
		</div>
	);
};
