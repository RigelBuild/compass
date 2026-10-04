// Unit tests for the sql-migration-gate's pure core + I/O orchestration.
//
// This gate is a CI oracle: three checks decide whether the first-party
// migrations pass squawk (safety), sqruff (style), and append-only byte checks.
// Its original shell form was double-expanded by moon to a constant `exit 0`.
// This suite defends the fail-closed contract and proves all three checks run
// before their exit codes are combined.
//
// Conventions (mirroring tools/inline-sql-gate/index.test.ts):
// - Literal expectations, not values derived from the module.

import { describe, expect, test } from "bun:test";
import { mkdirSync, mkdtempSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import {
	checkMigrationImmutability,
	combineExitCodes,
	type Deps,
	findMigrationViolations,
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
	test("all checks pass -> 0", () => {
		expect(
			combineExitCodes([
				ok("squawk"),
				ok("sqruff"),
				ok("migration-immutability"),
			]),
		).toBe(0);
	});

	test("a finding from any check -> 1", () => {
		expect(
			combineExitCodes([
				fail("squawk"),
				ok("sqruff"),
				ok("migration-immutability"),
			]),
		).toBe(1);
		expect(
			combineExitCodes([
				ok("squawk"),
				fail("sqruff"),
				ok("migration-immutability"),
			]),
		).toBe(1);
		expect(
			combineExitCodes([
				ok("squawk"),
				ok("sqruff"),
				fail("migration-immutability"),
			]),
		).toBe(1);
	});

	test("an internal failure (2) dominates", () => {
		expect(
			combineExitCodes([broke("migration-immutability"), fail("sqruff")]),
		).toBe(2);
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
		runMigrationCheck: async () => {
			ran.push("migration-immutability");
			const code = codes["migration-immutability"] ?? 0;
			return {
				name: "migration-immutability",
				code,
				output: code === 0 ? "" : "migration findings",
			};
		},
		log: (m) => logs.push(m),
		err: (m) => errs.push(m),
	};
	return { deps, ran, errs, logs };
}

describe("runOnce", () => {
	test("runs all checks even when a linter fails, surfacing their outputs", async () => {
		const { deps, ran, errs } = harness({ squawk: 1, sqruff: 1 });
		await runOnce(deps);
		expect(ran).toEqual(["squawk", "sqruff", "migration-immutability"]);
		const joined = errs.join("\n");
		expect(joined).toContain("squawk findings");
		expect(joined).toContain("sqruff findings");
	});

	test("returns 1 when a check finds and still runs all checks", async () => {
		const { deps, ran, logs } = harness({ squawk: 0, sqruff: 1 });
		expect(await runOnce(deps)).toBe(1);
		expect(ran).toEqual(["squawk", "sqruff", "migration-immutability"]);
		expect(logs.join("\n")).not.toContain("OK");
	});

	test("returns 0 and logs OK when all checks pass", async () => {
		const { deps, logs } = harness({ squawk: 0, sqruff: 0 });
		expect(await runOnce(deps)).toBe(0);
		expect(logs.join("\n")).toContain("OK");
	});

	test("clean run emits no blank output lines", async () => {
		const { deps, errs } = harness({ squawk: 0, sqruff: 0 });
		await runOnce(deps);
		expect(errs).toEqual([]);
	});

	test("propagates an internal failure as 2", async () => {
		const { deps } = harness({ squawk: 2, sqruff: 0 });
		expect(await runOnce(deps)).toBe(2);
	});
});

describe("findMigrationViolations", () => {
	const original = new TextEncoder().encode("SELECT 1;\n");
	const same = new TextEncoder().encode("SELECT 1;\n");
	const edited = new TextEncoder().encode("SELECT 2;\n");
	const path = "go/internal/store/migrations/0001_init.sql";

	test("unchanged base migrations pass", () => {
		expect(
			findMigrationViolations(
				new Map([[path, original]]),
				new Map([[path, same]]),
			),
		).toEqual([]);
	});

	test("an edited base migration fails by path", () => {
		expect(
			findMigrationViolations(
				new Map([[path, original]]),
				new Map([[path, edited]]),
			),
		).toEqual([path]);
	});

	test("a deleted base migration fails by path", () => {
		expect(
			findMigrationViolations(new Map([[path, original]]), new Map()),
		).toEqual([path]);
	});

	test("a renamed base migration fails by its old path", () => {
		expect(
			findMigrationViolations(
				new Map([[path, original]]),
				new Map([["go/internal/store/migrations/0002_init.sql", original]]),
			),
		).toEqual([path]);
	});

	test("new migrations are not checked even when edited", () => {
		const added = "go/internal/store/migrations/0002_new.sql";
		expect(
			findMigrationViolations(
				new Map([[path, original]]),
				new Map([
					[path, same],
					[added, edited],
				]),
			),
		).toEqual([]);
	});

	test("a trailing-newline-only change fails", () => {
		expect(
			findMigrationViolations(
				new Map([[path, original]]),
				new Map([[path, new TextEncoder().encode("SELECT 1;")]]),
			),
		).toEqual([path]);
	});
});

describe("checkMigrationImmutability", () => {
	test("real git path rejects edited base migration and accepts unchanged file", async () => {
		const root = mkdtempSync(join(tmpdir(), "sql-gate-git-"));
		const path = "go/internal/store/migrations/0001_init.sql";
		const file = join(root, path);
		const git = (args: string[]) => Bun.$`git ${args}`.cwd(root).quiet();
		try {
			mkdirSync(join(root, "go/internal/store/migrations"), {
				recursive: true,
			});
			writeFileSync(file, "SELECT 1;\n");
			await git(["init", "-q"]);
			await git(["config", "user.email", "test@example.com"]);
			await git(["config", "user.name", "Migration test"]);
			await git(["add", path]);
			await git(["commit", "-qm", "base"]);
			await git(["branch", "base"]);
			expect(
				(await checkMigrationImmutability(root, { GATE_BASE_REF: "base" }))
					.code,
			).toBe(0);
			writeFileSync(file, "SELECT 2;\n");
			const result = await checkMigrationImmutability(root, {
				GATE_BASE_REF: "base",
			});
			expect(result.code).toBe(1);
			expect(result.output).toContain(path);
		} finally {
			rmSync(root, { recursive: true, force: true });
		}
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
