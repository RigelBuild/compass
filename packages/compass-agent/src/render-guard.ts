// Single-source render guards for untrusted values interpolated into text the model
// reads as authoritative harness output. Extracted from comms.ts so the comms and
// lifecycle tools share ONE copy of each guard — a second copy of the `flat` regex
// once drifted, keeping the LF-only spelling after `flat` was widened.

// The tag's second untrusted-shaped channel. The fence makes a record's OPENING
// unforgeable, but the opener interpolates `id` and `author`: a `"` injects a second
// `author=` (misattribution), a newline splits it into mismatched-fence records. Both
// are server-minted today, but that safety belongs to another layer and can lapse silently.

// Constrain rather than escape: a shape test admitting no quote/newline/angle-bracket
// cannot break out, where an escape list's every miss is a live hole. Bound `+` not `*`
// (empty would render `author=""`); the degraded value names the fence so two hostile
// values cannot collapse onto one mintable string. Callers outside a render get bare form.
export const attr = (v: string, fence?: string): string =>
	/^[\w.:-]+$/.test(v)
		? v
		: fence === undefined
			? "(malformed)"
			: `(malformed ${fence})`;

// `attr` guards a tag attribute; `flat` guards a marker LINE — an untrusted value must not
// split the one-line `[ask]`/`[answered]` record or forge structure inside it. Tab and space
// survive for display fidelity; every other control, format (BOM, bidi overrides) and space
// separator collapses, and a long run is bounded so padding cannot exhaust a caller's budget.
export const flat = (v: string): string =>
	v
		.replaceAll(/(?:(?![\t ])[\p{Cc}\p{Cf}\p{Zs}\p{Zl}\p{Zp}])+/gu, " ")
		.replaceAll(/[\t ]{12,}/g, " ")
		.trim();

// `attr` guards an id-shaped value; `ref` guards a URL or `<owner>/<name>` slug that
// `attr`'s `[\w.:-]+` rejects (no `/`). `ref` widens to `/ ? # = & % ~ + @` but keeps the
// SAME doctrine: no quote, angle bracket, or whitespace/control. Bound `+` not `*`; the
// create-ack's empty-`url` dedup-hit is handled by the caller first.
export const ref = (v: string, fence?: string): string =>
	/^[\w.:/?#=&%~+@-]+$/.test(v)
		? v
		: fence === undefined
			? "(malformed)"
			: `(malformed ${fence})`;
