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

import { afterAll, beforeAll, describe, expect, test } from "bun:test";
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
	migrationBaseRef,
	movableMigrations,
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

	test("a migration-check finding fails the gate and surfaces its output", async () => {
		const { deps, errs } = harness({ "migration-immutability": 1 });
		expect(await runOnce(deps)).toBe(1);
		expect(errs.join("\n")).toContain("migration findings");
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

	const dupA = "go/internal/store/migrations/0003_a.sql";
	const dupB = "go/internal/store/migrations/0003_b.sql";
	const movedB = "go/internal/store/migrations/0005_b.sql";
	const other = new TextEncoder().encode("SELECT 3;\n");

	test("the later duplicate may move byte-identical to a new number", () => {
		expect(
			findMigrationViolations(
				new Map([
					[dupA, original],
					[dupB, other],
				]),
				new Map([
					[dupA, same],
					[movedB, other],
				]),
				new Set([dupB]),
			),
		).toEqual([]);
	});

	test("a migration outside the movable set may not move", () => {
		expect(
			findMigrationViolations(
				new Map([
					[dupA, original],
					[dupB, other],
				]),
				new Map([
					["go/internal/store/migrations/0005_a.sql", original],
					[dupB, other],
				]),
				new Set([dupB]),
			),
		).toEqual([dupA]);
	});

	test("a movable migration may not move with edited bytes", () => {
		expect(
			findMigrationViolations(
				new Map([
					[dupA, original],
					[dupB, other],
				]),
				new Map([
					[dupA, same],
					[movedB, edited],
				]),
				new Set([dupB]),
			),
		).toEqual([dupB]);
	});

	test("one added copy accounts for only one moved migration", () => {
		const dupC = "go/internal/store/migrations/0003_c.sql";
		expect(
			findMigrationViolations(
				new Map([
					[dupA, original],
					[dupB, other],
					[dupC, other],
				]),
				new Map([
					[dupA, same],
					[movedB, other],
				]),
				new Set([dupB, dupC]),
			),
		).toEqual([dupC]);
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

describe("movableMigrations", () => {
	const bytes = new TextEncoder().encode("SELECT 1;\n");
	const init = "go/internal/store/migrations/0001_init.sql";
	const first = "go/internal/store/migrations/0003_first.sql";
	const second = "go/internal/store/migrations/0003_second.sql";
	const base = new Map([
		[init, bytes],
		[first, bytes],
		[second, bytes],
	]);

	test("only the later-added duplicate is movable", () => {
		expect(movableMigrations(base, [init, first, second])).toEqual(
			new Set([second]),
		);
	});

	test("add order, not name order, picks the movable file", () => {
		expect(movableMigrations(base, [init, second, first])).toEqual(
			new Set([first]),
		);
	});

	test("unique versions are never movable", () => {
		expect(movableMigrations(new Map([[init, bytes]]), [init])).toEqual(
			new Set(),
		);
	});
});

describe("migrationBaseRef", () => {
	test("GATE_BASE_REF wins over the PR target", () => {
		expect(
			migrationBaseRef({ GATE_BASE_REF: "base", GITHUB_BASE_REF: "release" }),
		).toBe("base");
	});

	test("the PR target resolves on origin", () => {
		expect(migrationBaseRef({ GITHUB_BASE_REF: "release" })).toBe(
			"origin/release",
		);
	});

	test("defaults to origin/main", () => {
		expect(migrationBaseRef({})).toBe("origin/main");
	});
});

describe("checkMigrationImmutability", () => {
	// An inherited GIT_DIR or GIT_WORK_TREE would point every git call at the outer repo.
	const gitOverrides = ["GIT_DIR", "GIT_WORK_TREE", "GIT_INDEX_FILE"] as const;
	const saved = new Map<string, string | undefined>();
	beforeAll(() => {
		for (const key of gitOverrides) {
			saved.set(key, process.env[key]);
			delete process.env[key];
		}
	});
	afterAll(() => {
		for (const [key, value] of saved) {
			if (value !== undefined) process.env[key] = value;
		}
	});

	async function withRepo(
		files: Record<string, string>,
		body: (root: string) => Promise<void>,
	) {
		const root = mkdtempSync(join(tmpdir(), "sql-gate-git-"));
		const git = (args: string[]) => Bun.$`git ${args}`.cwd(root).quiet();
		try {
			mkdirSync(join(root, "go/internal/store/migrations"), {
				recursive: true,
			});
			for (const [path, text] of Object.entries(files))
				writeFileSync(join(root, path), text);
			await git(["init", "-q"]);
			await git(["config", "user.email", "test@example.com"]);
			await git(["config", "user.name", "Migration test"]);
			await git(["add", "."]);
			await git(["commit", "-qm", "base"]);
			await git(["branch", "base"]);
			await body(root);
		} finally {
			rmSync(root, { recursive: true, force: true });
		}
	}

	test("real git path rejects edited base migration and accepts unchanged file", async () => {
		const path = "go/internal/store/migrations/0001_init.sql";
		await withRepo({ [path]: "SELECT 1;\n" }, async (root) => {
			expect(
				(await checkMigrationImmutability(root, { GATE_BASE_REF: "base" }))
					.code,
			).toBe(0);
			writeFileSync(join(root, path), "SELECT 2;\n");
			const result = await checkMigrationImmutability(root, {
				GATE_BASE_REF: "base",
			});
			expect(result.code).toBe(1);
			expect(result.output).toContain(path);
		});
	});

	test("inherited GIT_DIR/GIT_WORK_TREE cannot redirect the check to another repo", async () => {
		const path = "go/internal/store/migrations/0001_init.sql";
		// The decoy's base holds the edited bytes, so a redirected check would pass.
		await withRepo({ [path]: "SELECT 2;\n" }, async (decoy) => {
			await withRepo({ [path]: "SELECT 1;\n" }, async (root) => {
				writeFileSync(join(root, path), "SELECT 2;\n");
				// Overrides go in the spawn env: Bun children ignore later process.env writes.
				const script = `import { checkMigrationImmutability } from ${JSON.stringify(join(import.meta.dir, "index.ts"))};
console.log(JSON.stringify(await checkMigrationImmutability(${JSON.stringify(root)}, { GATE_BASE_REF: "base" })));`;
				const child = Bun.spawn([process.execPath, "-e", script], {
					env: {
						...process.env,
						GIT_DIR: join(decoy, ".git"),
						GIT_WORK_TREE: decoy,
					},
					stdout: "pipe",
					stderr: "pipe",
				});
				const [out, stderr] = await Promise.all([
					new Response(child.stdout).text(),
					new Response(child.stderr).text(),
				]);
				expect(await child.exited, stderr).toBe(0);
				const result = JSON.parse(out) as LinterResult;
				expect(result.code).toBe(1);
				expect(result.output).toContain(path);
			});
		});
	});

	test("a migration name git would C-quote is still checked", async () => {
		const path = "go/internal/store/migrations/0002_café.sql";
		await withRepo({ [path]: "SELECT 1;\n" }, async (root) => {
			writeFileSync(join(root, path), "SELECT 2;\n");
			const result = await checkMigrationImmutability(root, {
				GATE_BASE_REF: "base",
			});
			expect(result.code).toBe(1);
			expect(result.output).toContain(path);
		});
	});

	test("an unresolvable base ref fails closed and names the ref", async () => {
		await withRepo(
			{ "go/internal/store/migrations/0001_init.sql": "SELECT 1;\n" },
			async (root) => {
				const result = await checkMigrationImmutability(root, {
					GATE_BASE_REF: "no-such-ref",
				});
				expect(result.code).toBe(2);
				expect(result.output).toContain("no-such-ref");
			},
		);
	});

	test("the duplicate merged last is movable even when its commit is older", async () => {
		const dir = "go/internal/store/migrations";
		await withRepo(
			{ [`${dir}/0001_init.sql`]: "SELECT 1;\n" },
			async (root) => {
				const git = (args: string[], env: Record<string, string> = {}) =>
					Bun.$`git ${args}`
						.cwd(root)
						.env({ ...process.env, ...env })
						.quiet();
				const at = (date: string) => ({
					GIT_AUTHOR_DATE: date,
					GIT_COMMITTER_DATE: date,
				});
				// The late PR's commit is older than the one that merged first.
				await git(["checkout", "-qb", "late"]);
				writeFileSync(join(root, `${dir}/0002_late.sql`), "SELECT 'late';\n");
				await git(["add", "."]);
				await git(["commit", "-qm", "late"], at("2026-01-01T00:00:00Z"));
				await git(["checkout", "-q", "base"]);
				writeFileSync(join(root, `${dir}/0002_early.sql`), "SELECT 'early';\n");
				await git(["add", "."]);
				await git(["commit", "-qm", "early"], at("2026-02-01T00:00:00Z"));
				await git(
					["merge", "-q", "--no-ff", "--no-edit", "late"],
					at("2026-03-01T00:00:00Z"),
				);
				await git(["checkout", "-qb", "fix"]);

				await git(["mv", `${dir}/0002_late.sql`, `${dir}/0003_late.sql`]);
				expect(
					(await checkMigrationImmutability(root, { GATE_BASE_REF: "base" }))
						.code,
				).toBe(0);

				await git(["mv", `${dir}/0003_late.sql`, `${dir}/0002_late.sql`]);
				await git(["mv", `${dir}/0002_early.sql`, `${dir}/0003_early.sql`]);
				const wrong = await checkMigrationImmutability(root, {
					GATE_BASE_REF: "base",
				});
				expect(wrong.code).toBe(1);
				expect(wrong.output).toContain(`${dir}/0002_early.sql`);
			},
		);
	});

	test("a shallow clone is deepened before add order is read", async () => {
		const dir = "go/internal/store/migrations";
		await withRepo(
			{ [`${dir}/0001_init.sql`]: "SELECT 1;\n" },
			async (upstream) => {
				const git = (cwd: string, args: string[]) =>
					Bun.$`git ${args}`.cwd(cwd).quiet();
				// Added later but sorts first, so tree order would pick the wrong file.
				for (const name of ["0002_early", "0002_a_late"]) {
					writeFileSync(
						join(upstream, `${dir}/${name}.sql`),
						`SELECT '${name}';\n`,
					);
					await git(upstream, ["add", "."]);
					await git(upstream, ["commit", "-qm", name]);
				}
				const clone = mkdtempSync(join(tmpdir(), "sql-gate-shallow-"));
				try {
					await git(clone, [
						"clone",
						"-q",
						"--depth=1",
						`file://${upstream}`,
						".",
					]);
					await git(clone, [
						"mv",
						`${dir}/0002_a_late.sql`,
						`${dir}/0003_a_late.sql`,
					]);
					const result = await checkMigrationImmutability(clone, {
						GATE_BASE_REF: "origin/HEAD",
					});
					expect(result.output).toBe("");
					expect(result.code).toBe(0);
				} finally {
					rmSync(clone, { recursive: true, force: true });
				}
			},
		);
	});

	test("outside a git worktree: skipped locally, fails closed on GitHub Actions", async () => {
		const root = mkdtempSync(join(tmpdir(), "sql-gate-nogit-"));
		try {
			const local = await checkMigrationImmutability(root, {});
			expect(local.code).toBe(0);
			expect(local.output).toContain("skipped");
			expect(
				(await checkMigrationImmutability(root, { GITHUB_ACTIONS: "true" }))
					.code,
			).toBe(2);
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
