// The render guards are the boundary every untrusted value crosses to become
// text the model reads as authoritative harness output, so their negative space
// is pinned directly — one forbidden character at a time — not only through a
// consumer. A consumer test that packs several forbidden chars into one payload
// (e.g. a url that is `"` AND newline AND space at once) still degrades if the
// class is later widened to re-admit just one of them, so it cannot catch that
// regression. These tests fail the instant the class admits a `"`, a `<`, or a
// line break — the exact breakouts the guards exist to deny.

import { describe, expect, test } from "bun:test";
import { ref } from "./render-guard";

describe("ref render guard", () => {
	test("passes a well-formed permalink, repo slug, and query/fragment url", () => {
		for (const ok of [
			"https://github.com/octo/repo/issues/7",
			"octo/repo",
			"https://x/1#frag?q=1&x=2",
			"a.b:c-d~e+f@h%20",
		]) {
			expect(ref(ok)).toBe(ok);
			expect(ref(ok, "abc12345")).toBe(ok);
		}
	});

	// Each of these, admitted, is a live breakout: a `"` closes the attribute the
	// ref sits in and injects a second one; a `<`/`>` forges a record tag; any
	// line break splits the single ack/opener line so the tail carries no fence.
	test("degrades on any quote, angle bracket, whitespace, or control char", () => {
		for (const bad of [
			'x"y',
			"x<y",
			"x>y",
			"x y",
			" x",
			"x ",
			"x\ny",
			"x\ry",
			"x\u2028y",
			"x\u2029y",
			"x\vy",
			"x\fy",
			"x\u0085y",
			"x\0y",
			"x\ty",
		]) {
			expect(ref(bad)).toBe("(malformed)");
		}
	});

	// A trailing newline is the classic `$`-anchored bypass: JS `$` matches
	// before a final `\n` unless the regex is multiline, so a guard spelled with
	// `$` alone would pass `"abc\n"`. This class uses no `$` cheat — it must fail.
	test("degrades on a trailing newline (no end-of-line bypass)", () => {
		expect(ref("abc\n")).toBe("(malformed)");
	});

	// An empty value is degraded, not passed: an empty ref would render as a
	// real-looking but blank permalink rather than a visibly broken one. The
	// create-ack's own empty-url dedup-hit branch is handled by its caller before
	// reaching here (see forge.ts createAck), so this bound never mishandles it.
	test("degrades an empty value rather than rendering a blank ref", () => {
		expect(ref("")).toBe("(malformed)");
	});

	// In a render pass the degraded token names the render's fence (so a body
	// cannot type `(malformed)` and collapse two hostile values onto a mintable
	// string); outside one (a single-line write ack) it takes the bare form.
	test("names the fence in a render pass and omits it without one", () => {
		expect(ref('x"y', "abc12345")).toBe("(malformed abc12345)");
		expect(ref('x"y')).toBe("(malformed)");
	});
});
