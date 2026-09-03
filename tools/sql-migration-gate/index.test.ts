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
import { mkdirSync, mkdtempSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import {
	combineExitCodes,
	type Deps,
	formatVerdict,
	type LinterResult,
	MIGRATION_GLOB,
	makeSpawnLinter,
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
	test("runs BOTH linters even when the first fails, surfacing both outputs", async () => {
		const { deps, ran, errs } = harness({ squawk: 1, sqruff: 1 });
		await runOnce(deps);
		expect(ran).toEqual(["squawk", "sqruff"]);
		// Both batteries' findings must surface in one push — the old bug hid
		// one half; dropping either err() call would re-hide it.
		const joined = errs.join("\n");
		expect(joined).toContain("squawk findings");
		expect(joined).toContain("sqruff findings");
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

	test("clean run emits no blank output lines (only the OK verdict)", async () => {
		const { deps, errs } = harness({ squawk: 0, sqruff: 0 });
		await runOnce(deps);
		// Clean linters produce empty output; the guard must suppress those so
		// stderr carries no blank noise ahead of the OK line.
		expect(errs).toEqual([]);
	});

	test("propagates a spawn failure as 2", async () => {
		const { deps } = harness({ squawk: 2, sqruff: 0 });
		expect(await runOnce(deps)).toBe(2);
	});
});

// ---------------------------------------------------------------------------
// makeSpawnLinter — the REAL spawn path: empty-glob and missing-binary both
// resolve to the documented code 2 (never an escaping throw, never green).
// ---------------------------------------------------------------------------

describe("makeSpawnLinter", () => {
	test("empty glob (no migrations under root) -> code 2", async () => {
		const root = mkdtempSync(join(tmpdir(), "sql-gate-empty-"));
		try {
			const linter = makeSpawnLinter(root);
			const res = await linter("squawk", [MIGRATION_GLOB]);
			expect(res.code).toBe(2);
			expect(res.output).toContain("no migrations matched");
		} finally {
			rmSync(root, { recursive: true, force: true });
		}
	});

	test("missing binary throws in spawn -> mapped to code 2, not an escaping rejection", async () => {
		const root = mkdtempSync(join(tmpdir(), "sql-gate-nobin-"));
		const migDir = join(root, "go/internal/store/migrations");
		mkdirSync(migDir, { recursive: true });
		writeFileSync(join(migDir, "0001_init.sql"), "SELECT 1;\n");
		try {
			// A binary that cannot exist on PATH; Bun.spawn throws synchronously.
			const linter = makeSpawnLinter(root);
			const res = await linter("squawk-does-not-exist-xyz", [MIGRATION_GLOB]);
			expect(res.code).toBe(2);
			expect(res.output).toContain("could not spawn");
		} finally {
			rmSync(root, { recursive: true, force: true });
		}
	});
});
