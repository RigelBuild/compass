import type { HighlighterCore } from "shiki/core";

// Shiki code highlighting with a fine-grained bundle.
//
// The Compass UI ships inside the Wails shell, so Shiki's pre-composed bundles
// (full: 1.2 MB gzip; web: 695 KB) are disqualifying. Instead: `shiki/core`
// (~12 KB) + the JavaScript regex engine (no WASM asset to ship or load in the
// webview) + ONLY the grammars/themes this UI actually shows, imported as
// dynamic `import()` so Vite splits each into an async chunk resolved on first
// use. One lazily-created instance module-wide (a highlighter loads grammars and
// holds an engine, so Shiki's guidance is create-one-and-reuse); the async-create
// race is contained here.

/** The active theme (UI is dark-leaning). Only this one is loaded — a payload
 *  paid for an unbuilt light/dark toggle is exactly the cost this module's
 *  fine-grained bundling exists to avoid. Adding the toggle means adding the
 *  second theme's dynamic `import()` alongside this one. */
const CODE_THEME = "github-dark-default";

// The code this UI shows (Compass / seal stack). Unknown tags fall back to plain
// `<pre><code>` — no highlight, no error.
let instance: Promise<HighlighterCore> | undefined;

/** The lazily-created module-singleton highlighter. First call kicks the
 *  grammar/theme/engine load; every later call reuses the same promise. The
 *  grammars/themes are dynamic `import()`s issued HERE (not at module scope), so
 *  Vite splits each into an async chunk that loads only on the first code block
 *  that needs highlighting — importing this module stays free. */
function getHighlighter(): Promise<HighlighterCore> {
	if (!instance) {
		// `shiki/core` + the engine are dynamic-imported HERE so the Shiki runtime
		// (core + JS regex engine) is a lazy chunk too, not just the grammars —
		// importing this module (which every message-render path does) stays free
		// of Shiki until the first code block actually needs highlighting.
		instance = (async () => {
			const [{ createHighlighterCore }, { createJavaScriptRegexEngine }] =
				await Promise.all([
					import("shiki/core"),
					import("shiki/engine/javascript"),
				]);
			return createHighlighterCore({
				themes: [import("@shikijs/themes/github-dark-default")],
				langs: [
					import("@shikijs/langs/typescript"),
					import("@shikijs/langs/tsx"),
					import("@shikijs/langs/javascript"),
					import("@shikijs/langs/json"),
					import("@shikijs/langs/bash"),
					import("@shikijs/langs/go"),
					import("@shikijs/langs/rust"),
					import("@shikijs/langs/python"),
					import("@shikijs/langs/yaml"),
					import("@shikijs/langs/toml"),
					import("@shikijs/langs/sql"),
					import("@shikijs/langs/diff"),
					import("@shikijs/langs/markdown"),
				],
				// The JS regex engine — no WASM, smaller and faster to start in the
				// webview. Escape hatch if a grammar mis-tokenizes: swap to
				// `createOnigurumaEngine(import("shiki/wasm"))` here.
				engine: createJavaScriptRegexEngine(),
			});
		})();
	}
	return instance;
}

/** Highlight `code` for `lang`, or resolve `null` when the language is not in
 *  the fine-grained set (the caller keeps its plain `<pre><code>` fallback). The
 *  loaded-language list carries aliases (`ts`, `tsx`, …), so an alias tag
 *  resolves too. Never throws for an unknown tag — that is the plain-fallback
 *  path, not an error. */
export async function highlightToHtml(
	code: string,
	lang: string,
): Promise<string | null> {
	const normalized = lang.trim().toLowerCase();
	if (normalized === "") return null;
	const hl = await getHighlighter();
	if (!hl.getLoadedLanguages().includes(normalized)) return null;
	return hl.codeToHtml(code, { lang: normalized, theme: CODE_THEME });
}
