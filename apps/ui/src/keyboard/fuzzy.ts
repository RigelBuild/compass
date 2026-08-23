/**
 * In-house fuzzy scorer for the command palette's action mode (RIG-2483, D2).
 *
 * The weighting is explicitly compass-ui's to choose (components.md:427-428;
 * commands.ts:79-82 pins only the inputs), so this is a small case-insensitive
 * subsequence matcher — no dependency for a tens-of-rows list. It returns a
 * score (higher = better) or `null` when `query` is not a subsequence of
 * `haystack` at all. An empty query matches everything with a neutral score so a
 * freshly-opened palette lists its whole inventory.
 *
 * Bonuses, high→low signal:
 *   - start-of-string: the first matched char is index 0 of the haystack.
 *   - word-boundary: a matched char follows a separator (space/`.`/`-`/`_`/`/`)
 *     or a lower→upper camelCase step.
 *   - contiguity: a matched char immediately follows the previous match.
 * A trailing penalty by leftover length keeps a tight match ("bri" → "Bridge")
 * ahead of a sparse one across a long string.
 */

const START_BONUS = 12;
const BOUNDARY_BONUS = 8;
const CONTIGUITY_BONUS = 6;
const MATCH_BASE = 1;
const WORD_SEPARATORS: Record<string, true> = {
	" ": true,
	".": true,
	"-": true,
	_: true,
	"/": true,
};

function isBoundary(haystack: string, index: number): boolean {
	if (index === 0) return true;
	const prev = haystack[index - 1];
	const cur = haystack[index];
	if (WORD_SEPARATORS[prev]) return true;
	// camelCase step: lower/digit followed by an upper — measured on the ORIGINAL
	// (un-lowercased) chars, so this is computed against the raw haystack.
	return /[a-z0-9]/.test(prev) && /[A-Z]/.test(cur);
}

/**
 * Score `query` against `haystack`. `null` = no subsequence match; a non-null
 * number ranks matches (higher = better). An empty (or whitespace-only) query
 * scores 0 — every candidate passes.
 */
export function fuzzyScore(query: string, haystack: string): number | null {
	const q = query.trim();
	if (q.length === 0) return 0;

	const needle = q.toLowerCase();
	const hayLower = haystack.toLowerCase();

	let score = 0;
	let h = 0; // cursor into haystack
	let prevMatch = -2; // index of the previous matched char

	for (let i = 0; i < needle.length; i++) {
		const target = needle[i];
		let found = -1;
		for (let j = h; j < hayLower.length; j++) {
			if (hayLower[j] === target) {
				found = j;
				break;
			}
		}
		if (found === -1) return null; // not a subsequence

		score += MATCH_BASE;
		if (found === 0) score += START_BONUS;
		else if (isBoundary(haystack, found)) score += BOUNDARY_BONUS;
		if (found === prevMatch + 1) score += CONTIGUITY_BONUS;

		prevMatch = found;
		h = found + 1;
	}

	// Penalize matches spread across a long haystack: fewer leftover chars ranks
	// higher, so a short exact-ish target beats a sparse hit in a long string.
	score -= (haystack.length - needle.length) * 0.1;
	return score;
}
