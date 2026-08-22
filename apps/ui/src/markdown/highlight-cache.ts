/** The synchronous `(lang, code)` → highlighted-HTML cache (design R1).
 *
 *  The react-markdown-10 renderer rebuilds a message's whole subtree — a fresh
 *  code-block component instance — on every changed streaming tick (under
 *  `renderingStrategy="reconcile"`). Without a cache a settled fence would flash
 *  back to the plain `<pre>` fallback on each tick until its async highlight
 *  re-resolves. This module-level map lets a rebuilt instance read an
 *  already-computed result SYNCHRONOUSLY at its initial render — a hit paints
 *  highlighted markup on the first frame, bypassing the debounce + async
 *  highlight; a miss runs that path and populates the cache on resolution.
 *
 *  Keyed on `lang\ncode` (a `\n` cannot appear in a `language-…` class token, so
 *  it is a safe separator): a language change or a growth tick (new code text)
 *  misses and re-highlights. Unbounded, but a chat scrollback is finite and each
 *  entry is one message's fence. */
const cache = new Map<string, string>();

/** The cache key for a `(lang, code)` pair. */
function key(lang: string, code: string): string {
	return `${lang}\n${code}`;
}

/** A cached highlight for `(lang, code)`, or `undefined` on a miss. */
export function getCachedHighlight(
	lang: string,
	code: string,
): string | undefined {
	return cache.get(key(lang, code));
}

/** Record a resolved highlight for `(lang, code)`. */
export function setCachedHighlight(
	lang: string,
	code: string,
	html: string,
): void {
	cache.set(key(lang, code), html);
}

/** Empty the cache. The cache is module-level and persists across component
 *  instances by design (that persistence is what R1 buys); a test that asserts
 *  on the highlight path must clear it between cases so a prior render's entry
 *  does not seed a later one. */
export function clearHighlightCache(): void {
	cache.clear();
}
