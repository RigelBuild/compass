// Single-source render guards for untrusted values interpolated into text the
// model reads as authoritative harness output. Extracted from comms.ts so the
// comms tools and the lifecycle tools share ONE copy of each guard rather than
// carrying divergent re-implementations: comms.ts once held a second copy of the
// `flat` regex at its error-render site, and that copy kept the LF-only spelling
// when `flat` was widened. Two guards against one threat drift apart silently,
// and the weaker one is the one nobody re-reads — so there is one of each, here.

// The tag's second untrusted-shaped channel. The fence makes a record's
// OPENING unforgeable from a body, but the opener interpolates two values —
// `id` and `author` — and a `"` in either closes the attribute early and
// injects a second `author=`, which an XML/HTML-shaped reader resolves to the
// FIRST one. That is the misattribution the fence exists to prevent, reached
// without guessing anything, because the injection rides inside a legitimately
// fenced tag. A newline is worse: it splits the opener into two records with
// mismatched fences.
//
// Both fields are server-minted today (`store.newID()` — 16 crypto/rand bytes
// hex-encoded; `PostMessageRequest` carries no id field and the author comes
// from the authenticated actor), so nothing can currently reach this. That is
// an invariant of a different language, package, and repo layer, stated
// nowhere here — exactly the kind of accidental safety that stops holding
// silently the moment a display name, a handle, or a federated id reaches
// these fields. A boundary that holds by accident is not a boundary.
//
// Constrain rather than escape. An escape must enumerate what to escape and
// every missed spelling is a live hole — the trap the fence was built to leave.
// A shape test has no such set: a value that cannot contain a quote, a newline,
// or an angle bracket cannot break out of an attribute, whatever it contains.
// Server ids satisfy this trivially, so a conforming value renders unchanged
// and only a value that has stopped being id-shaped degrades — visibly inert
// rather than silently forged.
//
// The bound is `+`, not `*`: an empty value would otherwise render as a real
// attribution that happens to name nobody (`author=""`), which reads as a
// genuine record rather than a broken one. And the degraded value names the
// render's fence, for the same reason the markers do — otherwise a body could
// type `(malformed)` and two distinct hostile values would collapse onto a
// string anything can mint. Callers outside a render pass no fence and get the
// bare form, which is correct there: the post return and the error text are
// single lines with no fence to name.
export const attr = (v: string, fence?: string): string =>
	/^[\w.:-]+$/.test(v)
		? v
		: fence === undefined
			? "(malformed)"
			: `(malformed ${fence})`;

// `attr` guards a tag attribute; `flat` guards a marker LINE. Every untrusted
// value interpolated into a one-line `[ask]`/`[answered]` record passes through
// here, because a line break in any of them splits that line into two and the
// second carries no fence and no marker — a forgery that needs no guessing.
// One collapse at the merge point rather than one per field, so a value added
// to that line later cannot arrive raw by omission.
//
// Constrain rather than enumerate, for the reason `attr` argues above. A
// list of the breaks to collapse must spell every one of them, and `\n` alone
// missed six: a lone `\r`, U+2028 and U+2029 (both formal line terminators),
// VT, FF, and NEL. Widening that list to those six still admits every other
// C0 control — including ESC, which in a terminal is an ANSI escape rather
// than a character. So the class is the property, not the roster: everything
// in `Cc`/`Zl`/`Zp` plus whitespace collapses, and a break spelled in some
// encoding nobody here thought of is already covered.
export const flat = (v: string): string =>
	v.replaceAll(/[\p{Cc}\p{Zl}\p{Zp}\s]+/gu, " ");

// `attr` guards an id-shaped value; `ref` guards a URL or an `<owner>/<name>`
// repo slug — the two shapes a forge write-ack line interpolates that `attr`
// rejects. `attr`'s class is `[\w.:-]+`, which excludes `/`, so every
// well-formed `https://…` permalink and every `owner/name` repo would degrade
// to `(malformed)` under it — the field the ack exists to surface, silently
// dropped. So `ref` widens the shape to the characters a URL and a repo slug
// legitimately carry (`/ ? # = & % ~ + @` on top of `attr`'s set) while keeping
// the SAME constrain-don't-escape doctrine: the class still admits no quote, no
// angle bracket, and no whitespace-or-control (`\s`/`Cc`/`Zl`/`Zp`), so a value
// that conforms cannot close an attribute, forge a tag, or split the single ack
// line, and a value that has stopped being URL/slug-shaped degrades visibly
// inert rather than breaking out. The bound is `+`, not `*`, for the reason
// `attr` states — an empty value would render as a real-looking but empty ref;
// the create-ack's own dedup-hit branch (an empty `url`) is handled by the
// caller BEFORE reaching here, never by degrading to `(malformed)`.
export const ref = (v: string, fence?: string): string =>
	/^[\w.:/?#=&%~+@-]+$/.test(v)
		? v
		: fence === undefined
			? "(malformed)"
			: `(malformed ${fence})`;
