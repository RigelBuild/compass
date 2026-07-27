import { describe, expect, test } from "bun:test";
import { usagePct } from "./UsageBar";

// usagePct feeds both the meter fill width (`${pct}%`) and the label. The store's
// accessors are the seam the real @compass/client fills, so the guard has to hold
// for account shapes the stub never contains — an unlimited/unloaded quota
// (limit 0) and a real over-limit account — not just the well-formed fixture.
describe("usagePct", () => {
	test("returns the rounded percentage for a normal quota", () => {
		expect(usagePct(50, 200)).toBe(25);
		expect(usagePct(3, 8)).toBe(38); // rounds 37.5
	});

	test("reads 0 for an unlimited or not-yet-loaded quota (limit 0), never Infinity", () => {
		// The bug: used/0 is Infinity, which rendered `width: Infinity%`.
		expect(usagePct(1000, 0)).toBe(0);
		expect(Number.isFinite(usagePct(1000, 0))).toBe(true);
	});

	test("clamps a real over-limit account to 100 so the meter can't overflow", () => {
		expect(usagePct(250, 200)).toBe(100);
	});

	test("never returns a negative percent", () => {
		expect(usagePct(0, 200)).toBe(0);
	});
});
