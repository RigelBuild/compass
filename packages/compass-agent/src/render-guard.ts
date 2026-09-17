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

// `attr` guards a tag attribute; `flat` guards a marker LINE — a line break in an untrusted
// value would split a one-line `[ask]`/`[answered]` record into a second line with no fence
// or marker. Tab and plain spaces survive for display fidelity; every other control and
// space separator collapses, since Zs (U+3000, NBSP) can forge alignment inside the line.
export const flat = (v: string): string =>
	v.replaceAll(/(?:(?![\t ])[\p{Cc}\p{Zs}\p{Zl}\p{Zp}]|\r|\n)+/gu, " ");

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
