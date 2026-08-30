import { afterEach, describe, expect, mock, test } from "bun:test";
import { render, waitFor } from "@solidjs/testing-library";
import type { JSX } from "@solidjs/web";
import * as realRuntime from "@wailsio/runtime";
import { createErrorBoundary, createSignal, flush } from "solid-js";
import type { Account } from "../comms-stub";
import { clearHighlightCache } from "../markdown/highlight-cache";
import * as realHighlighter from "../markdown/highlighter";
import { MarkdownText } from "./MarkdownText";

// Captured before any mock.module swaps the highlighter registry, so a mocking
// test can restore the REAL implementation in afterEach (mock.module otherwise
// leaks to every later test IN THIS FILE and across files).
const realHighlightToHtml = realHighlighter.highlightToHtml;

// The R1 highlight cache (highlight-cache.ts) is module-level and persists
// across component instances by design. Clear it after every test so a prior
// render's cached `(lang, code)` → HTML entry never seeds a later test's
// highlight path (which would make an assertion on the debounce/fetch cycle
// read a synchronous cache hit instead).
afterEach(() => {
	clearHighlightCache();
});

// Tests for the message-surface renderer: it renders a text block as MARKDOWN
// (CommonMark + GFM), composes the existing @-mention chips by post-processing
// the markdown tree's TEXT nodes (markdown-first), renders code verbatim through
// a `code` override (so mentions never chip inside code), highlights fenced code
// via a lazy Shiki singleton with a plain-text fallback, stays stable while a
// string grows mid-stream, and routes link activation through the Wails
// `Browser.OpenURL` seam instead of navigating the app.
//
// happy-dom has no layout, but every assertion here is on rendered DOM
// STRUCTURE / text / classes / attributes / events — none needs pixels. The
// async Shiki swap is observed with waitFor (deterministic: the highlight
// promise resolves; only its timing is async).

// A byHandle map keyed lowercase, exactly as ChannelView builds it
// (ChannelView.tsx:364-365). "cook" is a known account; "compass" is the known
// system sender (RIG-1820 — resolves like any known account, NOT reserved);
// "everyone" is reserved (comms-stub.ts:175); "ghost" is unknown (absent).
function byHandle(): Map<string, Account> {
	const cook: Account = {
		id: "acc-cook",
		handle: "cook",
		displayName: "Cook",
		kind: "agent",
	};
	const compass: Account = {
		id: "acc-sys-compass",
		handle: "compass",
		displayName: "Compass",
		kind: "system",
	};
	return new Map([
		["cook", cook],
		["compass", compass],
	]);
}

// A streaming fence is highlighted only once its text has been quiet for
// HIGHLIGHT_DEBOUNCE_MS (MarkdownText.tsx), so a test that asserts on highlight
// kickoff must let that window elapse first.
const DEBOUNCE_FLUSH_MS = 200;
const flushHighlightDebounce = () =>
	// biome-ignore lint/style/noRestrictedGlobals: waits out the component's real HIGHLIGHT_DEBOUNCE_MS debounce; the debounce is a real setTimeout with no injectable clock here, and the suite uses no fake timers (fake-timer conversion tracked in RIG-3016)
	new Promise((r) => setTimeout(r, DEBOUNCE_FLUSH_MS));

// The async Shiki highlight (150ms debounce + async tokenize) is observed with
// `waitFor`, whose default ceiling is 1000ms. On a loaded CI box (concurrent go
// + nix builds) the tokenize has resolved as late as ~1350ms — correct, just
// slow — so give the highlight-observing waits a headroom ceiling. This is not a
// retry: `waitFor` still polls the real highlighter to genuine completion; the
// wider ceiling only tolerates a contended runner, and a highlighter that never
// resolves still fails.
const HIGHLIGHT_WAIT_MS = 5000;

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

	test("the @compass system sender chips as known — not reserved, not unknown", () => {
		// RIG-1820: @compass resolves like any known account (accent chip), NOT a
		// reserved broadcast target. The `reserved` modifier is purple (mark-only
		// per DL-155), so a system-sender mention must never carry it; and being a
		// resolved account it is not `unknown` either.
		const { container } = render(() => (
			<MarkdownText text={"ping @compass for setup"} byHandle={byHandle()} />
		));
		const chip = container.querySelector(".mention-chip");
		expect(chip?.textContent).toBe("@compass");
		expect(chip?.classList.contains("reserved")).toBe(false);
		expect(chip?.classList.contains("unknown")).toBe(false);
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
		await waitFor(
			() => {
				expect(container.querySelector(".code-highlight")).not.toBeNull();
			},
			{ timeout: HIGHLIGHT_WAIT_MS },
		);
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
		// Solid 2's reconcile (the fork's renderingStrategy="reconcile" store
		// update) is scheduled off the setter, not synchronous to it as in Solid 1
		// — drain the effect queue so the grown tree is committed before asserting.
		flush();
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
		flush();
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
		await waitFor(
			() => {
				const styled = container.querySelector("pre code span[style]");
				expect(styled).not.toBeNull();
			},
			{ timeout: HIGHLIGHT_WAIT_MS },
		);
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

	// R1: the synchronous `(lang, code)` highlight cache. #44's reconcile renderer
	// rebuilds the whole message subtree — a fresh `CodeBlock` instance — on every
	// changed streaming tick, so a settled fence would flash back to the plain
	// `<pre>` fallback until its async highlight re-resolves. The module-level
	// cache lets a rebuilt instance seed `html` synchronously at construction, so
	// an already-highlighted fence stays highlighted across a growth tick with no
	// fallback frame.
	test("a settled fence stays highlighted across a growth tick (R1 cache, no fallback frame)", async () => {
		// Deterministic highlighter: resolves to known markup for the ts fence so
		// the R1 cache is populated once it settles. `data-r1` survives the
		// innerHTML swap (same pattern as the stale-resolution test's `data-tick`).
		const HIGHLIGHTED =
			'<pre class="shiki"><code><span style="color:green" data-r1="hit">const x = 1;</span></code></pre>';
		mock.module("../markdown/highlighter", () => ({
			...realHighlighter,
			highlightToHtml: (code: string, lang: string) =>
				Promise.resolve(
					lang === "ts" && code.includes("const x = 1;") ? HIGHLIGHTED : null,
				),
		}));
		const [text, setText] = createSignal("```ts\nconst x = 1;\n```");
		const { container } = render(() => (
			<MarkdownText text={text()} byHandle={byHandle()} />
		));
		// Let the debounce + async highlight settle so the (ts, "const x = 1;\n")
		// cache entry is populated.
		await waitFor(
			() => expect(container.querySelector('[data-r1="hit"]')).not.toBeNull(),
			{ timeout: HIGHLIGHT_WAIT_MS },
		);
		// Grow the message: append prose BELOW the settled fence. Under reconcile
		// this rebuilds the subtree, reconstructing the fence's CodeBlock — the R1
		// cache is what survives across instances. `flush()` drains Solid 2's
		// scheduled reconcile so the rebuilt tree is committed before asserting.
		setText("```ts\nconst x = 1;\n```\n\nmore prose below");
		flush();
		// The rebuilt fence ALREADY shows the highlighted markup, seeded from the
		// R1 cache at construction: no plain `<pre class="code-block">` fallback
		// frame is ever committed. Without R1 the rebuilt instance would paint the
		// fallback and only re-highlight after the 150ms debounce.
		expect(container.querySelector('[data-r1="hit"]')).not.toBeNull();
		expect(container.querySelector("pre.code-block")).toBeNull();
		// The appended prose rendered too (the grow actually took effect).
		expect(container.textContent).toContain("more prose below");
	});

	// RIG-1422: leading-edge first highlight. A settled (non-streaming) block —
	// every historical message in a channel — must colorize on FIRST paint, not
	// sit on the plain `<pre>` fallback for HIGHLIGHT_DEBOUNCE_MS. This is the
	// cache-MISS counterpart to the R1 test above (which proves the sync-cache
	// paint): here the (lang, code) pair is NOT cached (afterEach cleared it), so
	// the highlight runs the async path — but on the leading edge, kicked
	// immediately rather than gated behind the 150ms trailing timer. The proof is
	// that the highlighter is asked, and its markup paints, WITHOUT ever elapsing
	// `flushHighlightDebounce()`. A debounced-from-the-first-tick implementation
	// would leave `asked` empty until the 150ms window passed.
	test("a settled fence highlights on first paint, not gated behind the debounce (leading edge, cache miss)", async () => {
		const HIGHLIGHTED =
			'<pre class="shiki"><code><span style="color:green" data-le="hit">const x = 1;</span></code></pre>';
		const asked: string[] = [];
		mock.module("../markdown/highlighter", () => ({
			...realHighlighter,
			highlightToHtml: (code: string, lang: string) => {
				asked.push(code);
				return Promise.resolve(
					lang === "ts" && code.includes("const x = 1;") ? HIGHLIGHTED : null,
				);
			},
		}));
		const { container } = render(() => (
			<MarkdownText text={"```ts\nconst x = 1;\n```"} byHandle={byHandle()} />
		));
		// The highlight is kicked on the leading edge: the highlighter was asked
		// synchronously at render, with NO debounce window elapsed. (A trailing-only
		// debounce would have asked nothing yet.)
		flush();
		expect(asked.length).toBe(1);
		expect(asked[0]).toContain("const x = 1;");
		// And the resolved markup paints — still without ever elapsing the debounce
		// flush, so no plain-fallback frame is gated behind the 150ms timer.
		await waitFor(
			() => expect(container.querySelector('[data-le="hit"]')).not.toBeNull(),
			{ timeout: HIGHLIGHT_WAIT_MS },
		);
		expect(container.querySelector("pre.code-block")).toBeNull();
	});

	// RIG-1422: the leading-edge gate must NOT reintroduce the per-tick re-tokenize
	// the debounce exists to collapse. A single fence grown across ticks that
	// arrive inside the debounce window (no `flushHighlightDebounce` between them)
	// must issue ONE leading-edge pass, then collapse the whole growth burst into
	// exactly ONE trailing pass — not one per tick.
	test("a fence's within-window growth ticks collapse to one trailing highlight", async () => {
		const asked: string[] = [];
		mock.module("../markdown/highlighter", () => ({
			...realHighlighter,
			// Never resolves: we count HOW MANY times the highlighter is asked, not
			// what it returns.
			highlightToHtml: (code: string) => {
				asked.push(code);
				return new Promise<string | null>(() => {});
			},
		}));
		const [text, setText] = createSignal("```ts\nL0\n```");
		render(() => <MarkdownText text={text()} byHandle={byHandle()} />);
		flush();
		// Leading edge: the fresh fence is asked once, immediately.
		expect(asked.length).toBe(1);
		// Three growth ticks inside the window (flush() is synchronous, far under
		// HIGHLIGHT_DEBOUNCE_MS) — each is a growth tick, debounced, so no new ask.
		setText("```ts\nL0\nL1\n```");
		flush();
		setText("```ts\nL0\nL1\nL2\n```");
		flush();
		setText("```ts\nL0\nL1\nL2\nL3\n```");
		flush();
		expect(asked.length).toBe(1);
		// Once the window elapses, the burst collapses to exactly ONE trailing pass
		// carrying the latest text — not one pass per tick.
		await flushHighlightDebounce();
		expect(asked.length).toBe(2);
		expect(asked[1]).toContain("L3");
	});

	// RIG-1422 regression: the leading-edge gate is a MODULE-LEVEL record, so a
	// message with more than one fence rebuilds every fence in the same reconcile
	// tick. A naive single-slot record lets an earlier fence clobber the slot
	// before a later, still-growing fence reads it, so the grower never matches
	// its OWN prior code and (mis)fires an immediate re-tokenize every tick — the
	// exact O(n²) the debounce collapses. The gate must match a growing fence
	// against its own recent snapshot regardless of siblings scheduled between.
	test("a second streaming fence beside a settled one still debounces (no per-tick re-tokenize)", async () => {
		const asked: string[] = [];
		mock.module("../markdown/highlighter", () => ({
			...realHighlighter,
			highlightToHtml: (code: string) => {
				asked.push(code);
				return new Promise<string | null>(() => {});
			},
		}));
		// F1 is settled; F2 streams. Count only F2's asks (its "GROW" marker) so
		// F1's own per-tick reconstruction (harmless — it hits the R1 cache in
		// production) does not pollute the assertion.
		const F1 = "```ts\nconst settled = 1;\n```";
		const grows = () => asked.filter((c) => c.includes("GROW")).length;
		const [text, setText] = createSignal(`${F1}\n\n\`\`\`ts\nGROW\n\`\`\``);
		render(() => <MarkdownText text={text()} byHandle={byHandle()} />);
		flush();
		// F2's leading edge: asked once.
		expect(grows()).toBe(1);
		// Grow ONLY F2, within the window. Under a single-slot gate F1 clobbers the
		// slot each tick, so F2 reads as a leading edge and re-asks every tick
		// (grows() climbs). The window-list gate matches F2 against its own prior
		// snapshot → growth tick → debounced → no new ask.
		setText(`${F1}\n\n\`\`\`ts\nGROW\nMORE\n\`\`\``);
		flush();
		setText(`${F1}\n\n\`\`\`ts\nGROW\nMORE\nEVEN\n\`\`\``);
		flush();
		expect(grows()).toBe(1);
	});
});

describe("MarkdownText — link safety", () => {
	// The `openExternal` seam routes to the Wails `Browser.OpenURL` only inside
	// the native shell (detected via `window._wails.environment`, injected by the
	// Wails runtime); in a plain browser it falls back to `window.open`. happy-dom
	// is not a shell, so each test that observes the opener installs a fake shell
	// marker to drive the native path, then tears it down.
	const asShell = () => {
		(window as unknown as { _wails?: unknown })._wails = { environment: {} };
	};

	// mock.module leaks past this describe and across FILES (bun runs one
	// process), so the opener stub AND the fake shell marker must be torn down —
	// the same hazard this file's header documents and the highlighting describe
	// already guards.
	afterEach(() => {
		mock.module("@wailsio/runtime", () => realRuntime);
		(window as unknown as { _wails?: unknown })._wails = undefined;
	});

	test("activating a message link routes through the Wails opener and does NOT navigate the app", () => {
		asShell();
		const opened: string[] = [];
		mock.module("@wailsio/runtime", () => ({
			...realRuntime,
			Browser: {
				OpenURL: (url: string) => {
					opened.push(url);
					return Promise.resolve();
				},
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
		// routed to Browser.OpenURL.
		const evt = new MouseEvent("click", { bubbles: true, cancelable: true });
		a.dispatchEvent(evt);
		expect(evt.defaultPrevented).toBe(true);
		expect(opened).toEqual(["https://example.com/docs"]);
	});

	// T-MD-1: dangerous-scheme links are neutralized. solid-markdown does NOT
	// strip schemes (its transformLinkUri default is null), so an agent-authored
	// `javascript:` / `file:` / `data:` href would otherwise reach the DOM as a
	// live attribute AND the opener. The `a` override must render it inert
	// (`href="#"`) and never hand the raw href to the opener on activation.
	// Pre-fix (`href={p.href ?? "#"}`, `const href = p.href`) the live href
	// reaches the DOM and the opener → red.
	for (const scheme of [
		"javascript:alert(1)",
		"file:///etc/passwd",
		"data:text/html,<script>alert(1)</script>",
	]) {
		test(`a ${scheme.split(":")[0]}: link renders inert and never reaches the opener`, () => {
			asShell();
			const opened: string[] = [];
			mock.module("@wailsio/runtime", () => ({
				...realRuntime,
				Browser: {
					OpenURL: (url: string) => {
						opened.push(url);
						return Promise.resolve();
					},
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
		asShell();
		const opened: string[] = [];
		mock.module("@wailsio/runtime", () => ({
			...realRuntime,
			Browser: {
				OpenURL: (url: string) => {
					opened.push(url);
					return Promise.resolve();
				},
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
		// fixed child count.) react-markdown-10 renders the content directly under
		// `.markdown-content` — the old solid-markdown's extra `<div>` wrapper is
		// gone — so the raw chunk's text/`br` runs are its direct children.
		const kids = [
			...(container.querySelector(".markdown-content")?.childNodes ?? []),
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

	// The verbatim-in-code rule outranks the break rescue: `white-space: pre`
	// already renders the newline, and the `code` override reads through
	// `rawText`, which concatenates text descendants and IGNORES `br` — so an
	// interleaved break would not just be redundant, it would collapse the block
	// onto one line.
	//
	// This covers PLAIN TEXT in code, which is all it can cover: a `raw` node
	// never reaches a code subtree. Fenced and indented code become mdast `code`
	// nodes, and only an mdast `html` node becomes `raw` (mdast-util-to-hast
	// handlers/html.js), so the two are disjoint by construction — verified by
	// resolving `` ```\n<a>\n<b>\n``` `` through the real from-markdown/to-hast
	// stack: zero raw nodes in the tree at all. The angle brackets below are
	// therefore code text, not markup, and the `inCode` guard they exercise is
	// the pre-existing plain-text one.
	test("a fenced block keeps its newlines and adds no break", () => {
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

	// The async highlight failure is caught INSIDE the block's highlight effect
	// (a try/catch around `highlightToHtml`) and never surfaces into render, so a
	// failed dynamic import (a stale Vite chunk after redeploy, an offline
	// webview) degrades to the plain `<pre>` fallback instead of throwing. This
	// test proves the containment holds even when an error boundary IS present:
	// nothing propagates to it, so a real deployment (which mounts none) is safe.
	test("a rejected highlight degrades to plain code, taking nothing else down", async () => {
		mock.module("../markdown/highlighter", () => ({
			...realHighlighter,
			highlightToHtml: () =>
				Promise.reject(
					new Error("Failed to fetch dynamically imported module"),
				),
		}));
		// Solid 2 has no `ErrorBoundary` component — build one from the
		// `createErrorBoundary` primitive: it renders `fn`, and swaps to the
		// fallback if any descendant throws during render/effect. If the rejection
		// were to propagate, this boundary would show "BOUNDARY".
		const Boundary = (props: { children: JSX.Element }) =>
			createErrorBoundary(
				() => props.children,
				() => <span>BOUNDARY</span>,
			)();
		const { container } = render(() => (
			<Boundary>
				<div>sibling content</div>
				<MarkdownText text={"```ts\nconst x = 1;\n```"} byHandle={byHandle()} />
			</Boundary>
		));
		// Drain the microtask queue AND a macrotask so the rejection has actually
		// propagated before asserting. Waiting on the fallback <pre> alone is
		// vacuous: it renders synchronously, before the rejection lands, so the
		// assertion would pass against the crashing implementation too.
		for (let i = 0; i < 20; i++) await Promise.resolve();
		const macrotask = Promise.withResolvers<void>();
		// biome-ignore lint/style/noRestrictedGlobals: one macrotask hop (delay 0) after draining microtasks, to let the async highlight rejection propagate before asserting the degraded-to-plain fallback
		setTimeout(macrotask.resolve, 0);
		await macrotask.promise;
		expect(container.querySelector("pre code")?.textContent).toContain(
			"const x = 1;",
		);
		expect(container.textContent).not.toContain("BOUNDARY");
		expect(container.textContent).toContain("sibling content");
	});
});
