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
 *  misses and re-highlights. A streaming fence therefore caches every
 *  intermediate growth snapshot it settles at, not just the final text — so the
 *  map is FIFO-bounded at `MAX_ENTRIES`: the R1 correctness property only needs
 *  the currently-visible/recently-rebuilt fences, never the whole session
 *  history, and a long-lived agent session streams unbounded code otherwise. */
const MAX_ENTRIES = 512;
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

/** Record a resolved highlight for `(lang, code)`. Evicts the oldest entry
 *  first when the map is at `MAX_ENTRIES` (Map preserves insertion order, so the
 *  first key is the oldest) — re-setting an existing key refreshes its value
 *  without changing its age, which is fine for a bound this coarse. */
export function setCachedHighlight(
	lang: string,
	code: string,
	html: string,
): void {
	if (cache.size >= MAX_ENTRIES) {
		const oldest = cache.keys().next().value;
		if (oldest !== undefined) cache.delete(oldest);
	}
	cache.set(key(lang, code), html);
}

/** The recent `(lang, code)` snapshots blocks have scheduled a highlight for, in
 *  schedule order (oldest first) — the cross-instance thread the leading-edge
 *  gate needs (RIG-1422).
 *
 *  `BlockCode` is reconstructed on every streaming growth tick (solid-markdown
 *  rebuilds the fenced subtree under `renderingStrategy="reconcile"`), so a
 *  per-instance "first run" flag reads true every tick and cannot tell a fresh
 *  settled block from a growth tick. A module-level record can — but a SINGLE
 *  slot is wrong: a message with more than one fence rebuilds every fence in the
 *  same reconcile tick, in document order, so an earlier fence clobbers the slot
 *  before a later, still-growing fence reads it; the grower never sees its own
 *  prior code and (mis)fires an immediate re-tokenize every tick — the exact
 *  O(n²) the debounce exists to collapse. A short list, matched against ANY
 *  recent same-lang snapshot, lets each fence find its own prior code regardless
 *  of siblings scheduled in between. Bounded by the debounce window (stale
 *  entries pruned) with a coarse size cap as a backstop. */
const recentScheduled: { lang: string; code: string; at: number }[] = [];
const MAX_RECENT = 64;

/** Whether `(lang, code)` should highlight on the LEADING edge — immediately,
 *  no debounce — vs. be debounced as a streaming growth tick. Leading edge for a
 *  fresh block and for a batch of distinct settled blocks (a history load, where
 *  no two are a prefix of each other); debounced only when this code strictly
 *  extends a recent same-lang snapshot within `windowMs` (an active stream).
 *
 *  Records `(lang, code)` as a recent snapshot as a side effect, so later calls
 *  see it. `windowMs` guards against a stale snapshot: two blocks far apart in
 *  time that happen to be prefix-related are independent, not a stream. A
 *  settled block that coincidentally extends a sibling settled block rendered in
 *  the same paint is misread as a growth tick (debounced ~one window, then
 *  colorized) — a rare one-time timing shift, never a wrong render. Uses
 *  `performance.now()` (monotonic) so a wall-clock adjustment can't reorder
 *  snapshots. */
export function isLeadingEdgeHighlight(
	lang: string,
	code: string,
	windowMs: number,
): boolean {
	const now = performance.now();
	// Scan newest-first: an active stream matches its own latest prior snapshot
	// even when sibling fences were scheduled in between. Snapshots are appended
	// in (monotonic) time order, so once one is older than the window every
	// earlier one is too — stop there.
	let isGrowthTick = false;
	for (let i = recentScheduled.length - 1; i >= 0; i--) {
		const prev = recentScheduled[i];
		if (now - prev.at >= windowMs) break;
		if (
			prev.lang === lang &&
			code !== prev.code &&
			code.startsWith(prev.code)
		) {
			isGrowthTick = true;
			break;
		}
	}
	recentScheduled.push({ lang, code, at: now });
	// Prune snapshots older than the window (a prefix of the array, by time
	// order); cap the length as a coarse backstop against a dense burst.
	const cutoff = now - windowMs;
	let stale = 0;
	while (stale < recentScheduled.length && recentScheduled[stale].at <= cutoff)
		stale++;
	if (stale > 0) recentScheduled.splice(0, stale);
	if (recentScheduled.length > MAX_RECENT)
		recentScheduled.splice(0, recentScheduled.length - MAX_RECENT);
	return !isGrowthTick;
}

/** Empty the cache AND clear the leading-edge snapshots. The cache is
 *  module-level and persists across component instances by design (that
 *  persistence is what R1 buys); a test that asserts on the highlight path must
 *  clear it between cases so a prior render's entry does not seed a later one —
 *  and must reset the leading-edge snapshots (RIG-1422) too, so a prior test's
 *  recently-scheduled code cannot make a later test's fresh block read as a
 *  growth tick. */
export function clearHighlightCache(): void {
	cache.clear();
	recentScheduled.length = 0;
}
