// Unit tests for the sql-migration-gate's pure core + I/O orchestration.
//
// This gate is a CI oracle: it decides whether the first-party migrations pass
// the squawk (safety) + sqruff (style) batteries. Its whole reason to be a
// script is that the previous inline-`bash -c` form combined the two exit codes
// with a shell expression moon double-expanded to a constant `exit 0`, so the
// gate ran fail-OPEN. This suite defends the machine-readable contract the bug
// violated: the exit-code combination is fail-closed, and runOnce runs BOTH
// linters before combining.
//
// Conventions (mirroring tools/inline-sql-gate/index.test.ts):
// - Literal expectations, not values derived from the module.

import { describe, expect, test } from "bun:test";
import {
	combineExitCodes,
	type Deps,
	formatVerdict,
	type LinterResult,
	runOnce,
} from "./index.ts";

const ok = (name: string): LinterResult => ({ name, code: 0, output: "" });
const fail = (name: string): LinterResult => ({
	name,
	code: 1,
	output: `${name} findings`,
});
const broke = (name: string): LinterResult => ({
	name,
	code: 2,
	output: `${name} could not run`,
});

// ---------------------------------------------------------------------------
// combineExitCodes — the fail-closed contract the false-green bug violated.
// ---------------------------------------------------------------------------

describe("combineExitCodes", () => {
	test("both pass -> 0", () => {
		expect(combineExitCodes([ok("squawk"), ok("sqruff")])).toBe(0);
	});

	test("squawk finds, sqruff clean -> 1 (fail-closed on either)", () => {
		expect(combineExitCodes([fail("squawk"), ok("sqruff")])).toBe(1);
	});

	test("squawk clean, sqruff finds -> 1 (the case the old gate hid)", () => {
		expect(combineExitCodes([ok("squawk"), fail("sqruff")])).toBe(1);
	});

	test("both find -> 1", () => {
		expect(combineExitCodes([fail("squawk"), fail("sqruff")])).toBe(1);
	});

	test("a spawn failure (2) dominates so an un-run gate is never green", () => {
		expect(combineExitCodes([broke("squawk"), ok("sqruff")])).toBe(2);
		expect(combineExitCodes([ok("squawk"), broke("sqruff")])).toBe(2);
		expect(combineExitCodes([broke("squawk"), fail("sqruff")])).toBe(2);
	});

	test("no results -> 0 (vacuous; runOnce guards the empty-glob case)", () => {
		expect(combineExitCodes([])).toBe(0);
	});
});

// ---------------------------------------------------------------------------
// formatVerdict — the human-readable line.
// ---------------------------------------------------------------------------

describe("formatVerdict", () => {
	test("all-pass names every linter", () => {
		expect(formatVerdict([ok("squawk"), ok("sqruff")])).toContain("OK");
		expect(formatVerdict([ok("squawk"), ok("sqruff")])).toContain(
			"squawk + sqruff",
		);
	});

	test("failure names only the failing linters", () => {
		const v = formatVerdict([ok("squawk"), fail("sqruff")]);
		expect(v).toContain("FAIL");
		expect(v).toContain("sqruff");
		expect(v).not.toContain("squawk + sqruff");
	});
});

// ---------------------------------------------------------------------------
// runOnce — orchestration: BOTH linters run, output streamed, code combined.
// ---------------------------------------------------------------------------

function harness(codes: Record<string, number>) {
	const ran: string[] = [];
	const errs: string[] = [];
	const logs: string[] = [];
	const deps: Deps = {
		runLinter: async (name) => {
			ran.push(name);
			const code = codes[name] ?? 0;
			return { name, code, output: code === 0 ? "" : `${name} findings` };
		},
		log: (m) => logs.push(m),
		err: (m) => errs.push(m),
	};
	return { deps, ran, errs, logs };
}

describe("runOnce", () => {
	test("runs BOTH linters even when the first fails (no short-circuit)", async () => {
		const { deps, ran } = harness({ squawk: 1, sqruff: 1 });
		await runOnce(deps);
		expect(ran).toEqual(["squawk", "sqruff"]);
	});

	test("returns 1 when only sqruff finds — the exact regression", async () => {
		const { deps, ran, logs } = harness({ squawk: 0, sqruff: 1 });
		expect(await runOnce(deps)).toBe(1);
		expect(ran).toEqual(["squawk", "sqruff"]);
		// A failing gate must NOT emit the OK line.
		expect(logs.join("\n")).not.toContain("OK");
	});

	test("returns 0 and logs OK when both pass", async () => {
		const { deps, logs } = harness({ squawk: 0, sqruff: 0 });
		expect(await runOnce(deps)).toBe(0);
		expect(logs.join("\n")).toContain("OK");
	});

	test("propagates a spawn failure as 2", async () => {
		const { deps } = harness({ squawk: 2, sqruff: 0 });
		expect(await runOnce(deps)).toBe(2);
	});
});
