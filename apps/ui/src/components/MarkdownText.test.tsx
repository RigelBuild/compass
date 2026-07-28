import { afterEach, describe, expect, mock, test } from "bun:test";
import { render, waitFor } from "@solidjs/testing-library";
import * as realOpener from "@tauri-apps/plugin-opener";
import { createSignal, ErrorBoundary } from "solid-js";
import type { Account } from "../comms-stub";
import * as realHighlighter from "../markdown/highlighter";
import { MarkdownText } from "./MarkdownText";

// Captured before any mock.module swaps the highlighter registry, so a mocking
// test can restore the REAL implementation in afterEach (mock.module otherwise
// leaks to every later test IN THIS FILE and across files).
const realHighlightToHtml = realHighlighter.highlightToHtml;

// Tests for the message-surface renderer: it renders a text block as MARKDOWN
// (CommonMark + GFM), composes the existing @-mention chips by post-processing
// the markdown tree's TEXT nodes (markdown-first), renders code verbatim through
// a `code` override (so mentions never chip inside code), highlights fenced code
// via a lazy Shiki singleton with a plain-text fallback, stays stable while a
// string grows mid-stream, and routes link activation through the Tauri opener
// instead of navigating the app.
//
// happy-dom has no layout, but every assertion here is on rendered DOM
// STRUCTURE / text / classes / attributes / events — none needs pixels. The
// async Shiki swap is observed with waitFor (deterministic: the highlight
// promise resolves; only its timing is async).

// A byHandle map keyed lowercase, exactly as ChannelView builds it
// (ChannelView.tsx:364-365). "cook" is a known account; "everyone" is reserved
// (comms-stub.ts:175); "ghost" is unknown (absent from the map).
function byHandle(): Map<string, Account> {
	const cook: Account = {
		id: "acc-cook",
		handle: "cook",
		displayName: "Cook",
		kind: "agent",
	};
	return new Map([["cook", cook]]);
}

// A streaming fence is highlighted only once its text has been quiet for
// HIGHLIGHT_DEBOUNCE_MS (MarkdownText.tsx), so a test that asserts on highlight
// kickoff must let that window elapse first.
const DEBOUNCE_FLUSH_MS = 200;
const flushHighlightDebounce = () =>
	new Promise((r) => setTimeout(r, DEBOUNCE_FLUSH_MS));

describe("MarkdownText — markdown semantics", () => {
	test("renders bold, a list, and a link as semantic HTML", () => {
		const { container } = render(() => (
			<MarkdownText
				text={"**bold** text\n\n- one\n- two\n\n[site](https://example.com)"}
				byHandle={byHandle()}
			/>
		));
		// Bold → <strong>, list → <ul><li>, link → <a href>.
		expect(container.querySelector("strong")?.textContent).toBe("bold");
		const items = container.querySelectorAll("ul li");
		expect(items.length).toBe(2);
		expect(items[0]?.textContent?.trim()).toBe("one");
		const a = container.querySelector("a");
		expect(a?.getAttribute("href")).toBe("https://example.com");
		expect(a?.textContent).toBe("site");
	});

	test("GFM is on: a table renders as a real <table>", () => {
		const { container } = render(() => (
			<MarkdownText
				text={"| a | b |\n| - | - |\n| 1 | 2 |"}
				byHandle={byHandle()}
			/>
		));
		expect(container.querySelector("table")).not.toBeNull();
		expect(container.querySelectorAll("tbody td").length).toBe(2);
	});
});

describe("MarkdownText — mention composition", () => {
	test("a known @mention in prose chips (mention-chip, no modifier)", () => {
		const { container } = render(() => (
			<MarkdownText text={"hey @cook how are you"} byHandle={byHandle()} />
		));
		const chip = container.querySelector(".mention-chip");
		expect(chip).not.toBeNull();
		expect(chip?.textContent).toBe("@cook");
		// Known → neither reserved nor unknown modifier.
		expect(chip?.classList.contains("reserved")).toBe(false);
		expect(chip?.classList.contains("unknown")).toBe(false);
	});

	test("a reserved @everyone chips with the reserved modifier", () => {
		const { container } = render(() => (
			<MarkdownText text={"ping @everyone now"} byHandle={byHandle()} />
		));
		const chip = container.querySelector(".mention-chip");
		expect(chip?.textContent).toBe("@everyone");
		expect(chip?.classList.contains("reserved")).toBe(true);
	});

	test("an unknown @ghost chips with the unknown modifier", () => {
		const { container } = render(() => (
			<MarkdownText text={"who is @ghost"} byHandle={byHandle()} />
		));
		const chip = container.querySelector(".mention-chip");
		expect(chip?.textContent).toBe("@ghost");
		expect(chip?.classList.contains("unknown")).toBe(true);
		expect(chip?.classList.contains("reserved")).toBe(false);
	});

	test("a mention inside emphasis renders BOTH bold and the chip", () => {
		// **hey @cook!** — markdown-first keeps the emphasis, and the mention still
		// chips inside it (the mention-first composition would break the bold).
		const { container } = render(() => (
			<MarkdownText text={"**hey @cook!**"} byHandle={byHandle()} />
		));
		const strong = container.querySelector("strong");
		expect(strong).not.toBeNull();
		const chip = strong?.querySelector(".mention-chip");
		expect(chip?.textContent).toBe("@cook");
	});
});

describe("MarkdownText — code is verbatim, never chipped", () => {
	test("@cook inside an inline code span does NOT chip", () => {
		const { container } = render(() => (
			<MarkdownText text={"call `@cook` verbatim"} byHandle={byHandle()} />
		));
		const code = container.querySelector("code");
		expect(code?.textContent).toBe("@cook");
		// No chip anywhere in the rendered output.
		expect(container.querySelector(".mention-chip")).toBeNull();
	});

	test("@cook inside a fenced code block does NOT chip", () => {
		const { container } = render(() => (
			<MarkdownText text={"```\nping @cook here\n```"} byHandle={byHandle()} />
		));
		expect(container.textContent).toContain("@cook");
		expect(container.querySelector(".mention-chip")).toBeNull();
	});

	// The `br` interleaving that rescues a prose softbreak must NOT reach a code
	// element: `rawText` concatenates only `text` descendants, so a `br` inside
	// `<code>` contributes nothing and every newline in a multi-line block is
	// silently eaten ("line one\nline two" renders as "line oneline two"). Every
	// other code test here uses a single-line body, which is exactly why this
	// class of loss can pass a full suite.
	test("a multi-line fenced block keeps every newline", () => {
		const { container } = render(() => (
			<MarkdownText
				text={"```\nline one\nline two\nline three\n```"}
				byHandle={byHandle()}
			/>
		));
		const code = container.querySelector("pre code");
		expect(code?.textContent).toBe("line one\nline two\nline three\n");
		expect(code?.querySelector("br")).toBeNull();
	});

	test("a multi-line block streams its newlines through to the highlighter", async () => {
		// Shiki highlights whatever `rawText` produced, so newline loss upstream
		// shows up as one highlighted line instead of two. The fence's trailing
		// newline is part of the code text and Shiki keeps it.
		const { container } = render(() => (
			<MarkdownText
				text={"```ts\nconst a = 1;\nconst b = 2;\n```"}
				byHandle={byHandle()}
			/>
		));
		await waitFor(() => {
			expect(container.querySelector(".code-highlight")).not.toBeNull();
		});
		const pre = container.querySelector(".code-highlight pre");
		expect(pre?.textContent).toBe("const a = 1;\nconst b = 2;\n");
		// Shiki wraps each source line in its own `.line`, so the two statements
		// must land in two different ones — the observable proof it tokenized
		// multi-line source rather than one run-together line.
		const lines = [...(pre?.querySelectorAll(".line") ?? [])].map(
			(l) => l.textContent,
		);
		expect(lines).toContain("const a = 1;");
		expect(lines).toContain("const b = 2;");
	});
});

describe("MarkdownText — mid-stream growth stays stable", () => {
	test("a growing string ending in an unterminated fence renders a code block at every step, never a prose flip", () => {
		// Simulate the store re-emitting a longer .text each render.
		const [text, setText] = createSignal("intro paragraph\n\n```ts\n");
		const { container } = render(() => (
			<MarkdownText text={text()} byHandle={byHandle()} />
		));
		// Step 1: fence opened, no close yet → still a <pre><code>, and the intro
		// prose is NOT swallowed into the code block. CommonMark closes an
		// unterminated fence implicitly at end of input, so the partial parses as
		// a growing code block with the prose above left as prose.
		expect(container.querySelector("pre code")).not.toBeNull();
		expect(container.querySelector("p")?.textContent).toContain("intro");

		// Step 2: more code text streams in — still a code block, prose intact,
		// and the code block's content now REFLECTS the streamed-in code (not just
		// present — the actual latest text).
		setText("intro paragraph\n\n```ts\nconst x = 1;\n");
		expect(container.querySelector("pre code")).not.toBeNull();
		expect(container.querySelector("p")?.textContent).toContain("intro");
		// Exact, not `toContain`: a renderer that silently eats the newline still
		// satisfies a substring check on each line, so only the exact string
		// defends the multi-line fixture this test actually renders.
		expect(container.querySelector("pre code")?.textContent).toBe(
			"const x = 1;\n",
		);

		// Step 3: the code grows again — the block tracks the LATEST source, so the
		// newly appended line is present (and it is still a single code block, no
		// prose flip).
		setText("intro paragraph\n\n```ts\nconst x = 1;\nconst y = 2;\n```");
		expect(container.querySelector("pre code")).not.toBeNull();
		expect(container.querySelector("p")?.textContent).toContain("intro");
		expect(container.querySelector("pre code")?.textContent).toBe(
			"const x = 1;\nconst y = 2;\n",
		);
	});
});

describe("MarkdownText — code highlighting with plain fallback", () => {
	// Restore the real highlighter after every test here: the mock-based tests
	// below (deterministic unknown-lang, stale-resolution ordering) swap the
	// module via mock.module, which otherwise leaks to the real-Shiki tests above
	// and to other files. Re-registering the real namespace resets it.
	afterEach(() => {
		mock.module("../markdown/highlighter", () => ({
			...realHighlighter,
			highlightToHtml: realHighlightToHtml,
		}));
	});

	test("plain <pre><code> shows immediately, before async highlight resolves", () => {
		const { container } = render(() => (
			<MarkdownText text={"```ts\nconst x = 1;\n```"} byHandle={byHandle()} />
		));
		// Synchronously (first paint), the code text is present as plain code —
		// highlighting has not resolved yet.
		const code = container.querySelector("pre code");
		expect(code).not.toBeNull();
		expect(code?.textContent).toContain("const x = 1;");
	});

	test("a known-lang fence eventually carries highlighted token spans", async () => {
		const { container } = render(() => (
			<MarkdownText text={"```ts\nconst x = 1;\n```"} byHandle={byHandle()} />
		));
		// Shiki resolves asynchronously and swaps in styled token spans.
		await waitFor(() => {
			const styled = container.querySelector("pre code span[style]");
			expect(styled).not.toBeNull();
		});
		// The code text survives the swap.
		expect(container.querySelector("pre code")?.textContent).toContain(
			"const x = 1;",
		);
	});

	test("an unknown-lang fence stays plain (no token spans, no error)", async () => {
		// Deterministic replacement for the old `setTimeout(50)`-then-assert-absence
		// race. Wrap the REAL highlighter so the genuine unknown-lang → null
		// decision is exercised (teeth on highlightToHtml, not a hardcoded null),
		// but CAPTURE the promise it returns so we can await it and assert only
		// once the highlight has actually settled — no wall-clock wait.
		const settledPromises: Promise<string | null>[] = [];
		mock.module("../markdown/highlighter", () => ({
			...realHighlighter,
			highlightToHtml: (code: string, lang: string) => {
				const p = realHighlightToHtml(code, lang);
				settledPromises.push(p);
				return p;
			},
		}));
		const { container } = render(() => (
			<MarkdownText
				text={"```wubbalang\nsome code\n```"}
				byHandle={byHandle()}
			/>
		));
		// Immediate fallback: plain code is shown before the highlight settles.
		expect(container.querySelector("pre code")?.textContent).toContain(
			"some code",
		);
		// Await the REAL highlight resolution(s) — the unknown lang resolves to
		// null — then flush. The assertion runs against a genuinely-settled render.
		await flushHighlightDebounce();
		expect(settledPromises.length).toBeGreaterThan(0);
		const results = await Promise.all(settledPromises);
		expect(results.every((r) => r === null)).toBe(true); // unknown → null
		await Promise.resolve();
		const code = container.querySelector("pre code");
		expect(code?.textContent).toContain("some code");
		expect(code?.querySelector("span[style]")).toBeNull();
	});

	// T-MD-2: last-write-wins under out-of-order highlight resolution. A growing
	// fence kicks a fresh highlight per tick; createResource keys on [code, lang],
	// so a STALE earlier tick that resolves AFTER the latest must be dropped. We
	// control resolution order via the mock: resolve the LATEST tick first (paint
	// it), then resolve the earlier stale tick LATE and assert the DOM still shows
	// the latest, never the stale overwrite.
	test("a stale highlight resolving after the latest tick does not overwrite it", async () => {
		const pending: {
			code: string;
			resolve: (v: string | null) => void;
		}[] = [];
		mock.module("../markdown/highlighter", () => ({
			...realHighlighter,
			highlightToHtml: (code: string) =>
				new Promise<string | null>((resolve) => {
					pending.push({ code, resolve });
				}),
		}));
		const [text, setText] = createSignal("```ts\nAAA\n```");
		const { container } = render(() => (
			<MarkdownText text={text()} byHandle={byHandle()} />
		));
		// Each tick must clear the highlight debounce so it actually kicks a
		// fetch — that is what makes two competing in-flight requests exist.
		await flushHighlightDebounce();
		// Grow the fence → a second highlight fetch is kicked for the newer text.
		setText("```ts\nAAA\nBBB\n```");
		await flushHighlightDebounce();
		expect(pending.length).toBeGreaterThanOrEqual(2);
		const stale = pending[0]; // the earlier "AAA" tick
		const latest = pending[pending.length - 1]; // the "AAA\nBBB" tick
		expect(stale.code).not.toBe(latest.code);

		// Resolve the LATEST first and let it paint.
		latest.resolve(
			'<pre class="shiki"><code><span style="color:green" data-tick="latest">AAA BBB</span></code></pre>',
		);
		await waitFor(() =>
			expect(container.querySelector('[data-tick="latest"]')).not.toBeNull(),
		);
		// Now resolve the STALE tick LATE — it must be discarded, not painted over.
		stale.resolve(
			'<pre class="shiki"><code><span style="color:red" data-tick="stale">AAA</span></code></pre>',
		);
		await Promise.resolve();
		await Promise.resolve();
		expect(container.querySelector('[data-tick="latest"]')).not.toBeNull();
		expect(container.querySelector('[data-tick="stale"]')).toBeNull();
	});

	// Each growth tick must issue a request carrying ITS OWN text — collapse the
	// per-tick keying and `asked[0]` stops being "AAA".
	//
	// The stronger property the name might suggest — that an in-flight fetcher
	// cannot retarget to the newest text — is NOT what this defends, and the
	// distinction is established by mutation rather than assumed: with the
	// highlight source DEBOUNCED, `[code(), lang()]` is snapshotted into the
	// debounced signal before the timer fires, so by the time the fetcher runs
	// its keyed `src` and the live `code()` are the same string. Pointing the
	// fetcher at the live signal leaves this suite green — that mutation is
	// unreachable through the public component, not merely uncaught here. The
	// stale-resolution test above is what guards the painted result.
	test("each growth tick issues its own highlight request", async () => {
		const asked: string[] = [];
		mock.module("../markdown/highlighter", () => ({
			...realHighlighter,
			highlightToHtml: (code: string) => {
				asked.push(code);
				return new Promise<string | null>(() => {});
			},
		}));
		const [text, setText] = createSignal("```ts\nAAA\n```");
		render(() => <MarkdownText text={text()} byHandle={byHandle()} />);
		// One debounce window per tick, so each issues its own request.
		await flushHighlightDebounce();
		setText("```ts\nAAA\nBBB\n```");
		await flushHighlightDebounce();

		expect(asked.length).toBeGreaterThanOrEqual(2);
		// The first request was kicked for "AAA" alone and must still say so.
		// A fenced block's text keeps its trailing newline (CommonMark), so
		// compare on trimmed content — the point is WHICH tick, not the newline.
		expect(asked[0].trim()).toBe("AAA");
		expect(asked[0]).not.toContain("BBB");
		// The newest request carries the grown text.
		expect(asked[asked.length - 1]).toContain("BBB");
	});

	test("a plain block renders exactly one <pre> (no nested pre)", () => {
		const { container } = render(() => (
			<MarkdownText text={"```\nplain code\n```"} byHandle={byHandle()} />
		));
		expect(container.querySelectorAll("pre").length).toBe(1);
		expect(container.querySelector("pre pre")).toBeNull();
	});

	test("a highlighted block renders exactly one <pre> (no nested pre)", async () => {
		const { container } = render(() => (
			<MarkdownText text={"```ts\nconst x = 1;\n```"} byHandle={byHandle()} />
		));
		await waitFor(() => {
			expect(container.querySelector("span[style]")).not.toBeNull();
		});
		// After the Shiki swap, still exactly one <pre> — Shiki's own — never
		// wrapped in the markdown `pre` override.
		expect(container.querySelectorAll("pre").length).toBe(1);
		expect(container.querySelector("pre pre")).toBeNull();
	});
});

describe("MarkdownText — link safety", () => {
	// mock.module leaks past this describe and across FILES (bun runs one
	// process), so the opener stub must be torn down — the same hazard this
	// file's header documents and the highlighting describe already guards.
	afterEach(() => {
		mock.module("@tauri-apps/plugin-opener", () => realOpener);
	});

	test("activating a message link routes through the Tauri opener and does NOT navigate the app", () => {
		const opened: string[] = [];
		mock.module("@tauri-apps/plugin-opener", () => ({
			openUrl: (url: string) => {
				opened.push(url);
				return Promise.resolve();
			},
		}));

		const { container } = render(() => (
			<MarkdownText
				text={"see [docs](https://example.com/docs)"}
				byHandle={byHandle()}
			/>
		));
		const a = container.querySelector("a") as HTMLAnchorElement;
		expect(a).not.toBeNull();

		// Activate: the click is intercepted (default prevented → no app nav) and
		// routed to openUrl.
		const evt = new MouseEvent("click", { bubbles: true, cancelable: true });
		a.dispatchEvent(evt);
		expect(evt.defaultPrevented).toBe(true);
		expect(opened).toEqual(["https://example.com/docs"]);
	});

	// T-MD-1: dangerous-scheme links are neutralized. solid-markdown does NOT
	// strip schemes (its transformLinkUri default is null), so an agent-authored
	// `javascript:` / `file:` / `data:` href would otherwise reach the DOM as a
	// live attribute AND `openUrl`. The `a` override must render it inert
	// (`href="#"`) and never hand the raw href to the opener on activation.
	// Pre-fix (`href={p.href ?? "#"}`, `const href = p.href`) the live href
	// reaches the DOM and openUrl → red.
	for (const scheme of [
		"javascript:alert(1)",
		"file:///etc/passwd",
		"data:text/html,<script>alert(1)</script>",
	]) {
		test(`a ${scheme.split(":")[0]}: link renders inert and never reaches the opener`, () => {
			const opened: string[] = [];
			mock.module("@tauri-apps/plugin-opener", () => ({
				openUrl: (url: string) => {
					opened.push(url);
					return Promise.resolve();
				},
			}));

			const { container } = render(() => (
				<MarkdownText text={`[x](${scheme})`} byHandle={byHandle()} />
			));
			const a = container.querySelector("a") as HTMLAnchorElement;
			expect(a).not.toBeNull();
			// (a) The live href is neutralized — never the dangerous scheme.
			expect(a.getAttribute("href")).toBe("#");
			// (b) Activating it does not hand anything to the external opener.
			const evt = new MouseEvent("click", { bubbles: true, cancelable: true });
			a.dispatchEvent(evt);
			expect(evt.defaultPrevented).toBe(true);
			expect(opened).toEqual([]);
		});
	}

	// T-MD-1 companion: the safe path is unaffected — an https link keeps its
	// live href AND still routes to the opener. This pins that the sanitizer
	// gates on scheme, not by neutering all links.
	test("a safe https link keeps its href and still calls the opener", () => {
		const opened: string[] = [];
		mock.module("@tauri-apps/plugin-opener", () => ({
			openUrl: (url: string) => {
				opened.push(url);
				return Promise.resolve();
			},
		}));

		const { container } = render(() => (
			<MarkdownText
				text={"[x](https://example.com/safe)"}
				byHandle={byHandle()}
			/>
		));
		const a = container.querySelector("a") as HTMLAnchorElement;
		expect(a.getAttribute("href")).toBe("https://example.com/safe");
		const evt = new MouseEvent("click", { bubbles: true, cancelable: true });
		a.dispatchEvent(evt);
		expect(evt.defaultPrevented).toBe(true);
		expect(opened).toEqual(["https://example.com/safe"]);
	});

	test("a GFM autolink of user@host.com renders a plain link with NO mention chip", () => {
		// remark-gfm turns user@host.com into a mailto autolink whose label text
		// still contains @host.com; the `a` override renders labels from raw text,
		// so the mention override never sees it → no chip.
		const { container } = render(() => (
			<MarkdownText
				text={"mail me at cook@host.com ok"}
				byHandle={byHandle()}
			/>
		));
		const a = container.querySelector("a");
		expect(a).not.toBeNull();
		expect(a?.textContent).toBe("cook@host.com");
		expect(container.querySelector(".mention-chip")).toBeNull();
	});

	test("a link label with inline markup renders as flattened plain text, no chip", () => {
		// [**@cook**](url): the `a` override renders the label from the link node's
		// raw text value, so the emphasis is intentionally flattened and the
		// mention never chips (accepted tradeoff).
		const { container } = render(() => (
			<MarkdownText
				text={"[**@cook**](https://example.com)"}
				byHandle={byHandle()}
			/>
		));
		const a = container.querySelector("a");
		expect(a?.getAttribute("href")).toBe("https://example.com");
		expect(a?.textContent).toBe("@cook");
		expect(container.querySelector(".mention-chip")).toBeNull();
		// Flattened: no <strong> inside the label.
		expect(a?.querySelector("strong")).toBeNull();
	});

	test("an agent-authored <img> does not render (images disallowed)", () => {
		const { container } = render(() => (
			<MarkdownText
				text={"![pixel](https://tracker.example.com/x.png)"}
				byHandle={byHandle()}
			/>
		));
		expect(container.querySelector("img")).toBeNull();
	});

	test("a safe link is bypass-proof: target=_blank and rel strip opener/referrer", () => {
		const { container } = render(() => (
			<MarkdownText
				text={"see [docs](https://example.com/docs)"}
				byHandle={byHandle()}
			/>
		));
		const a = container.querySelector("a");
		expect(a?.getAttribute("target")).toBe("_blank");
		expect(a?.getAttribute("rel")).toBe("noreferrer noopener");
	});

	test("an unsafe link stays inert with no target/rel", () => {
		const { container } = render(() => (
			<MarkdownText text={"[x](javascript:alert(1))"} byHandle={byHandle()} />
		));
		const a = container.querySelector("a");
		expect(a?.getAttribute("href")).toBe("#");
		expect(a?.getAttribute("target")).toBeNull();
		expect(a?.getAttribute("rel")).toBeNull();
	});
});

describe("MarkdownText — content preservation", () => {
	// solid-markdown pipes remarkRehype with `allowDangerousHtml: true`
	// (solid-markdown/dist/index.jsx:307), so HTML in a message becomes hast
	// `raw` nodes — and its child renderer has Match arms only for `element`
	// and `text` (dist/index.jsx:110-125), so a `raw` node renders as NOTHING.
	// `<` is a raw-HTML opener to CommonMark, so ordinary agent prose about
	// generics or JSX silently loses characters. The fix must restore the text
	// WITHOUT making the HTML live: these assertions pin both halves, so a
	// future `rehype-raw` swap (which would restore text by injecting real
	// elements) reddens on the second half.
	test("raw HTML in prose renders as literal text, never as live elements", () => {
		const { container } = render(() => (
			<MarkdownText
				text={"hi <script>alert(1)</script> bye"}
				byHandle={byHandle()}
			/>
		));
		expect(container.textContent).toContain("<script>alert(1)</script>");
		expect(container.querySelectorAll("script").length).toBe(0);
	});

	test("an <img> tag in raw HTML is inert text, not a remote fetch", () => {
		const { container } = render(() => (
			<MarkdownText
				text={"text with <img src=x onerror=alert(1)> here"}
				byHandle={byHandle()}
			/>
		));
		expect(container.textContent).toContain("<img src=x onerror=alert(1)>");
		expect(container.querySelectorAll("img").length).toBe(0);
	});

	test("prose containing generics keeps every character", () => {
		const { container } = render(() => (
			<MarkdownText
				text={"use Vec<T> and Map<K, V> here"}
				byHandle={byHandle()}
			/>
		));
		expect(container.textContent).toBe("use Vec<T> and Map<K, V> here");
	});

	// `textContent` is the wrong instrument for a line break: per spec it
	// concatenates descendant text and ignores `<br>` entirely, so it reads
	// "T4T5" whether the break is rendered or not. Assert the DOM structure —
	// a real `br` BETWEEN the two runs — which is what the reader sees.
	test("a single newline separates words instead of joining them", () => {
		const { container } = render(() => (
			<MarkdownText
				text={"Done: **T4**\n**T5** is next."}
				byHandle={byHandle()}
			/>
		));
		const p = container.querySelector("p");
		expect(p).not.toBeNull();
		const kinds = [...(p?.childNodes ?? [])].map((n) =>
			n.nodeType === 1 ? (n as Element).tagName.toLowerCase() : "#text",
		);
		// …<strong>T4</strong><br><strong>T5</strong>… — a break separates them.
		const firstStrong = kinds.indexOf("strong");
		const br = kinds.indexOf("br");
		const secondStrong = kinds.indexOf("strong", firstStrong + 1);
		expect(firstStrong).toBeGreaterThanOrEqual(0);
		expect(br).toBeGreaterThan(firstStrong);
		expect(secondStrong).toBeGreaterThan(br);
	});

	test("a plain two-line message renders as two visual lines", () => {
		const { container } = render(() => (
			<MarkdownText text={"line one\nline two"} byHandle={byHandle()} />
		));
		const p = container.querySelector("p");
		expect(p?.querySelectorAll("br").length).toBe(1);
		// The break sits BETWEEN the two lines, not at either edge. (Compare child
		// nodes, not innerHTML: Solid stamps a `key` attribute on the element.)
		const kids = [...(p?.childNodes ?? [])];
		expect(kids.length).toBe(3);
		expect(kids[0]?.textContent).toBe("line one");
		expect((kids[1] as Element).tagName.toLowerCase()).toBe("br");
		expect(kids[2]?.textContent).toBe("line two");
	});

	// A `raw` node is retyped to `text` so its characters survive, and it must
	// then go through the SAME newline→`br` split a plain text node gets: raw
	// HTML is a block-level chunk of source, so a multi-line one is the common
	// case, and skipping the split lets `white-space: normal` collapse its
	// newlines and render every line joined on one.
	test("a multi-line raw HTML block renders its lines as separate lines", () => {
		const { container } = render(() => (
			<MarkdownText
				text={"<details>\nline one\nline two\n</details>"}
				byHandle={byHandle()}
			/>
		));
		// `textContent` ignores `br` per spec, so assert the STRUCTURE: the two
		// lines are separate text runs with a break between them. (The opening and
		// closing tag lines get their own breaks too — the whole raw chunk is one
		// multi-line node — so match on the two prose lines rather than on a
		// fixed child count.)
		const kids = [
			...(container.querySelector(".markdown-content > div")?.childNodes ?? []),
		];
		const at = (text: string) => kids.findIndex((n) => n.textContent === text);
		const one = at("line one");
		const two = at("line two");
		expect(one).toBeGreaterThanOrEqual(0);
		expect(two).toBe(one + 2);
		expect((kids[one + 1] as Element).tagName.toLowerCase()).toBe("br");
		// Still inert: the markup is text, never a live element.
		expect(container.textContent).toContain("<details>");
		expect(container.querySelector("details")).toBeNull();
	});

	// The verbatim-in-code rule outranks the break rescue, exactly as it does
	// for plain text: `white-space: pre` already renders the newline, and the
	// `code` override reads through `rawText`, which concatenates text
	// descendants and IGNORES `br` — so an interleaved break would not just be
	// redundant, it would collapse the block onto one line.
	test("a multi-line raw HTML node inside a code block stays verbatim", () => {
		const { container } = render(() => (
			<MarkdownText text={"```\n<a>\n<b>\n```"} byHandle={byHandle()} />
		));
		const code = container.querySelector("pre code");
		expect(code?.textContent).toBe("<a>\n<b>\n");
		expect(container.querySelectorAll("br").length).toBe(0);
	});

	test("a disallowed image keeps its alt text as a visible placeholder", () => {
		const { container } = render(() => (
			<MarkdownText
				text={"see ![the architecture diagram](https://x/d.png) above"}
				byHandle={byHandle()}
			/>
		));
		// Security contract unchanged: no live <img>, no src ever emitted.
		expect(container.querySelector("img")).toBeNull();
		expect(container.innerHTML).not.toContain("https://x/d.png");
		// But the sentence must not have a hole in it.
		expect(container.textContent).toContain("the architecture diagram");
	});

	// mdast→hast emits a bare "\n" text node BETWEEN block children (ul→li,
	// table→tr, and inside a loose li around its p). solid-markdown drops those
	// with its own `value !== "\n"` guard, but the rehype plugin runs first —
	// turning each into a `br` smuggles an element past that guard into a parent
	// where phrasing content is invalid. Browsers foster-parent a `br` out of
	// table internals, and each one adds a blank line the virtualizer measures.
	test("a list gets no stray <br> between its items", () => {
		const { container } = render(() => (
			<MarkdownText text={"- one\n- two\n- three"} byHandle={byHandle()} />
		));
		expect(container.querySelectorAll("ul > br").length).toBe(0);
		expect(container.querySelectorAll("li").length).toBe(3);
	});

	test("an ordered list gets no stray <br> between its items", () => {
		const { container } = render(() => (
			<MarkdownText text={"1. a\n2. b"} byHandle={byHandle()} />
		));
		expect(container.querySelectorAll("ol > br").length).toBe(0);
	});

	test("a GFM table gets no stray <br> anywhere in its structure", () => {
		const { container } = render(() => (
			<MarkdownText
				text={"| a | b |\n| - | - |\n| 1 | 2 |"}
				byHandle={byHandle()}
			/>
		));
		expect(container.querySelectorAll("table br").length).toBe(0);
		expect(container.querySelectorAll("td").length).toBe(2);
	});

	// `li` and `blockquote` are DUAL-MODE — phrasing content in a tight list,
	// block content in a loose one — so the separator/softbreak call cannot be
	// made from the parent tag. These four pin both halves of the sibling rule
	// that decides it: a bare "\n" next to a block is whitespace, everything
	// else is prose.
	test("a nested list gets no stray <br> around the inner list", () => {
		const { container } = render(() => (
			<MarkdownText text={"- a\n  - b\n- d"} byHandle={byHandle()} />
		));
		expect(container.querySelectorAll("br").length).toBe(0);
		expect(container.querySelectorAll("li").length).toBe(3);
	});

	test("a loose list gets no stray <br> around its paragraphs", () => {
		const { container } = render(() => (
			<MarkdownText text={"- a\n\n- b"} byHandle={byHandle()} />
		));
		expect(container.querySelectorAll("li > br").length).toBe(0);
		expect(container.querySelectorAll("li > p").length).toBe(2);
	});

	test("a blockquote inside a list item gets no stray <br>", () => {
		const { container } = render(() => (
			<MarkdownText text={"- item\n  > quoted"} byHandle={byHandle()} />
		));
		expect(container.querySelectorAll("br").length).toBe(0);
		expect(container.querySelector("blockquote")?.textContent).toContain(
			"quoted",
		);
	});

	test("a fenced block inside a list item keeps its newlines and adds no <br>", () => {
		const { container } = render(() => (
			<MarkdownText
				text={"- step\n\n  ```ts\n  const a = 1;\n  const b = 2;\n  ```\n"}
				byHandle={byHandle()}
			/>
		));
		expect(container.querySelectorAll("br").length).toBe(0);
		expect(container.querySelector("pre code")?.textContent).toBe(
			"const a = 1;\nconst b = 2;\n",
		);
	});

	test("a softbreak inside a list item still renders a break", () => {
		const { container } = render(() => (
			<MarkdownText text={"- first\n  second"} byHandle={byHandle()} />
		));
		expect(container.querySelectorAll("li br").length).toBe(1);
		expect(container.querySelectorAll("ul > br").length).toBe(0);
	});

	// A hard break arrives as a PAIR — `[br, text("\n")]` — so splitting the
	// trailing newline doubles the gap. Rationale for the one-sided absorb lives
	// with the `isBr` clause in MarkdownText.tsx; these two pin its behavior.
	test.each([
		["trailing-space form", "a  \nb"],
		["backslash form", "a\\\nb"],
	])("a hard break (%s) renders exactly one <br>, not two", (_label, text) => {
		const { container } = render(() => (
			<MarkdownText text={text} byHandle={byHandle()} />
		));
		expect(container.querySelectorAll("br").length).toBe(1);
		expect(container.querySelector("p")?.textContent).toBe("ab");
	});

	// Pins the OTHER side of the asymmetry: only a PRECEDING `br` absorbs the
	// newline. The bare "\n" has to be its own node with the `br` on its RIGHT,
	// which needs an inline-element boundary — `"a\nb  \nc"` will not do, since
	// its softbreak stays embedded in the text node and never reaches the drop
	// clause. `"*a*\n\\\nb"` yields p[em, "\n", br, "\n", "b"]: relax the clause
	// to either side and the softbreak's own break disappears.
	test("a softbreak running into a hard break renders both", () => {
		const { container } = render(() => (
			<MarkdownText text={"*a*\n\\\nb"} byHandle={byHandle()} />
		));
		expect(container.querySelectorAll("br").length).toBe(2);
	});

	// The GFM footnote block is a `section`, and it is the LAST root child — so
	// the separator before it is only dropped if `section` is a known block. A
	// raw-HTML block just before the footnotes leaves the other neighbour a
	// text node, which is what exposes a gap in the table.
	test("a footnote section after a raw HTML block gets no stray <br>", () => {
		const { container } = render(() => (
			<MarkdownText
				text={"a[^1]\n\n<div>h</div>\n\n[^1]: n"}
				byHandle={byHandle()}
			/>
		));
		expect(container.querySelectorAll("br").length).toBe(0);
		expect(container.querySelector("section")).not.toBeNull();
	});

	// A label that is only an image flattens to "" through the raw-text label
	// path, leaving a zero-width but clickable anchor on a live external href.
	test("a link whose label is only an image still renders a visible label", () => {
		const { container } = render(() => (
			<MarkdownText
				text={"[![build status](https://x/badge.svg)](https://example.com)"}
				byHandle={byHandle()}
			/>
		));
		const a = container.querySelector("a");
		expect(a?.getAttribute("href")).toBe("https://example.com");
		expect(a?.textContent).toBe("[image: build status]");
		// The security contract holds: still no live <img>, still no src emitted.
		expect(container.querySelector("img")).toBeNull();
		expect(container.innerHTML).not.toContain("https://x/badge.svg");
	});
});

describe("MarkdownText — highlight failure is contained", () => {
	afterEach(() => {
		mock.module("../markdown/highlighter", () => ({
			...realHighlighter,
			highlightToHtml: realHighlightToHtml,
		}));
	});

	// `createResource` RETHROWS a rejected fetcher's error when the accessor is
	// read during render (solid-js read(): `if (err !== undefined && !pr) throw
	// err`). There is no ErrorBoundary anywhere in the app, so one failed
	// dynamic import (a stale Vite chunk after redeploy, an offline webview)
	// would unmount the whole Compass window.
	test("a rejected highlight degrades to plain code, taking nothing else down", async () => {
		mock.module("../markdown/highlighter", () => ({
			...realHighlighter,
			highlightToHtml: () =>
				Promise.reject(
					new Error("Failed to fetch dynamically imported module"),
				),
		}));
		const { container } = render(() => (
			<ErrorBoundary fallback={<span>BOUNDARY</span>}>
				<div>sibling content</div>
				<MarkdownText text={"```ts\nconst x = 1;\n```"} byHandle={byHandle()} />
			</ErrorBoundary>
		));
		// Drain the microtask queue AND a macrotask so the rejection has actually
		// propagated before asserting. Waiting on the fallback <pre> alone is
		// vacuous: it renders synchronously, before the rejection lands, so the
		// assertion would pass against the crashing implementation too.
		for (let i = 0; i < 20; i++) await Promise.resolve();
		await new Promise((r) => setTimeout(r, 50));
		expect(container.querySelector("pre code")?.textContent).toContain(
			"const x = 1;",
		);
		expect(container.textContent).not.toContain("BOUNDARY");
		expect(container.textContent).toContain("sibling content");
	});
});
