// sql-migration-gate — SQL migration lint and append-only protection for the
// first-party migrations under go/internal/store/migrations/. Three checks run
// UNCONDITIONALLY and their exit codes are OR'd, so one push surfaces every
// finding and the gate fails if ANY check fails:
//
//   squawk  migration-SAFETY analysis (unsafe DDL). Config in /.squawk.toml.
//   sqruff  SQL style/lint (structural + correctness + capitalisation). Config
//           in /.sqruff.
//   migration-immutability  base migration byte check (append-only policy).
//
// WHY THIS IS A SCRIPT, NOT AN INLINE `bash -c`:
// The gate was a moon `command: 'bash -c "squawk …; rc=$?; sqruff …; rc2=$?;
// exit $(( rc | rc2 ))"'`. moon wraps every task command in its OWN
// `bash -c "<command>"`, and the nested double quotes collide: the OUTER shell
// expands `$?`, `$rc`, `$rc2`, and `$(( rc | rc2 ))` (all unset → 0) BEFORE the
// inner shell runs, so the inner shell received a literal `…; rc=0; …; rc2=0;
// exit 0` and ran fail-OPEN — it printed every finding and always exited 0. The
// bug went unseen because 0001_init.sql genuinely passes both linters; the
// first migration to actually trip a finding would have shipped green. A script
// makes the exit-code combination real code (rule://scripts-ts-over-bash + the
// no-bash-gate CI task forbid this logic in bash) and unit-testable.
//
// Inputs (env):
//   GATE_ROOT - workspace root to run the linters from. Default: the git
//               toplevel, falling back to process.cwd() when git reports none
//               (a jj workspace's .git lives in the colocated clone). moon runs
//               this with runFromWorkspaceRoot:true, so cwd is the repo root in
//               CI. The linters discover their repo-root configs (/.squawk.toml,
//               /.sqruff) and the migration glob resolves repo-relative from
//               here.
//   GATE_BASE_REF - optional git ref to compare migrations against.
//   GITHUB_BASE_REF - PR target branch, used when GATE_BASE_REF is unset.
//   GITHUB_ACTIONS - when set, a root outside a git worktree fails the check;
//        otherwise (a local jj workspace) that check is skipped with a note.
// Exit codes:
//   0 - all checks passed (no findings).
//   1 - one or more checks reported findings.
//   2 - a check could not run / internal error.

import { $ } from "bun";

/** The migration glob both linters lint, relative to the gate root. */
export const MIGRATION_GLOB = "go/internal/store/migrations/*.sql";

/** One linter's result: its name, exit code, and combined stdout+stderr. */
export interface LinterResult {
	name: string;
	code: number;
	output: string;
}

/**
 * Combine the checks' exit codes into the gate's exit code. Fail-closed: the
 * gate fails (1) if ANY check reported findings (non-zero), passes (0) only
 * when every check passed. A spawn/internal failure (code 2) dominates so a
 * gate that could not actually run never reads as green.
 *
 * Pure and exported: this is the contract the false-green bug violated, so it
 * is the unit-tested core.
 */
export function combineExitCodes(results: LinterResult[]): number {
	let exit = 0;
	for (const { code } of results) {
		if (code === 2) return 2;
		if (code !== 0) exit = 1;
	}
	return exit;
}

/** Render the human-readable gate verdict from the linters' results. */
export function formatVerdict(results: LinterResult[]): string {
	const failed = results.filter((r) => r.code !== 0).map((r) => r.name);
	if (failed.length === 0) {
		return `sql-migration-gate: OK — ${results.map((r) => r.name).join(" + ")} passed.`;
	}
	return `sql-migration-gate: FAIL — findings from ${failed.join(" + ")}. See the annotations above.`;
}

export interface Deps {
	/** Run one linter over the glob; returns its exit code + combined output. */
	runLinter: (name: string, argv: string[]) => Promise<LinterResult>;
	runMigrationCheck: () => Promise<LinterResult>;
	log: (msg: string) => void;
	err: (msg: string) => void;
}

/**
 * Run all three checks and combine their exit codes. Every check ALWAYS runs
 * (all findings surface in one push) before the codes are combined.
 */
export async function runOnce(deps: Deps): Promise<number> {
	const { runLinter, runMigrationCheck, log, err } = deps;
	const squawk = await runLinter("squawk", [MIGRATION_GLOB]);
	if (squawk.output) err(squawk.output);
	const sqruff = await runLinter("sqruff", ["lint", MIGRATION_GLOB]);
	if (sqruff.output) err(sqruff.output);
	const migrations = await runMigrationCheck();
	if (migrations.output) err(migrations.output);
	const results = [squawk, sqruff, migrations];
	const exit = combineExitCodes(results);
	const verdict = formatVerdict(results);
	if (exit === 0) log(verdict);
	else err(verdict);
	return exit;
}

export type MigrationFileMap = ReadonlyMap<string, Uint8Array>;

/**
 * Return base migrations missing or byte-changed in the current tree. One
 * exception: when two concurrent PRs landed the same version, the one added
 * last (`movable`) may move unchanged to a new number. Only it can be unapplied:
 * any deploy built between the two merges already ran the earlier one.
 */
export function findMigrationViolations(
	baseFiles: MigrationFileMap,
	currentFiles: MigrationFileMap,
	movable: ReadonlySet<string> = new Set(),
): string[] {
	const unclaimed = [...currentFiles]
		.filter(([path]) => !baseFiles.has(path))
		.map(([, bytes]) => Buffer.from(bytes));
	const violations: string[] = [];
	for (const [path, baseBytes] of baseFiles) {
		const currentBytes = currentFiles.get(path);
		if (currentBytes !== undefined) {
			if (!Buffer.from(baseBytes).equals(Buffer.from(currentBytes)))
				violations.push(path);
			continue;
		}
		const match = movable.has(path)
			? unclaimed.findIndex((bytes) => bytes.equals(Buffer.from(baseBytes)))
			: -1;
		if (match < 0) violations.push(path);
		else unclaimed.splice(match, 1);
	}
	return violations;
}

/**
 * The base migrations that may move: for each version held by two or more
 * files, every file except the first added. `addedInOrder` is the base
 * branch's migration paths in the order commits added them.
 */
export function movableMigrations(
	baseFiles: MigrationFileMap,
	addedInOrder: readonly string[],
): Set<string> {
	const firstByVersion = new Map<string, string>();
	for (const path of addedInOrder) {
		if (!baseFiles.has(path)) continue;
		const version = migrationVersion(path);
		if (!firstByVersion.has(version)) firstByVersion.set(version, path);
	}
	const movable = new Set<string>();
	for (const path of baseFiles.keys()) {
		const first = firstByVersion.get(migrationVersion(path));
		if (first !== undefined && first !== path) movable.add(path);
	}
	return movable;
}

function migrationVersion(path: string): string {
	return (path.split("/").pop() ?? path).split("_", 1)[0] ?? "";
}

/** The ref migrations are compared against: explicit, then the PR target, then main. */
export function migrationBaseRef(env: NodeJS.ProcessEnv): string {
	if (env.GATE_BASE_REF) return env.GATE_BASE_REF;
	if (env.GITHUB_BASE_REF) return `origin/${env.GITHUB_BASE_REF}`;
	return "origin/main";
}

// Variables that redirect git away from the cwd repository; an inherited one
// would let the check read another repo's migrations and pass.
const GIT_ROUTING_VARS = new Set([
	"GIT_DIR",
	"GIT_WORK_TREE",
	"GIT_INDEX_FILE",
	"GIT_OBJECT_DIRECTORY",
	"GIT_ALTERNATE_OBJECT_DIRECTORIES",
	"GIT_COMMON_DIR",
	"GIT_NAMESPACE",
	"GIT_CEILING_DIRECTORIES",
	"GIT_DISCOVERY_ACROSS_FILESYSTEM",
]);

async function runGit(
	root: string,
	args: string[],
): Promise<{ code: number; out: Uint8Array; stderr: string }> {
	const env = Object.fromEntries(
		Object.entries(process.env).filter(([key]) => !GIT_ROUTING_VARS.has(key)),
	);
	const proc = Bun.spawn(["git", ...args], {
		cwd: root,
		env,
		stdout: "pipe",
		stderr: "pipe",
	});
	const [out, stderr] = await Promise.all([
		new Response(proc.stdout).arrayBuffer(),
		new Response(proc.stderr).text(),
	]);
	return { code: await proc.exited, out: new Uint8Array(out), stderr };
}

async function gitStdout(root: string, args: string[]): Promise<Uint8Array> {
	const { code, out, stderr } = await runGit(root, args);
	if (code !== 0)
		throw new Error(`git ${args.join(" ")} exited ${code}: ${stderr.trim()}`);
	return out;
}

/** Read merge-base migration blobs from git and compare them with disk. */
export async function checkMigrationImmutability(
	root: string,
	env: NodeJS.ProcessEnv = process.env,
): Promise<LinterResult> {
	const name = "migration-immutability";
	const inWorktree = await runGit(root, ["rev-parse", "--is-inside-work-tree"]);
	// A jj workspace has no git worktree; CI always does, so only CI must fail closed.
	if (inWorktree.code !== 0 && !env.GITHUB_ACTIONS) {
		return {
			name,
			code: 0,
			output: `sql-migration-gate: skipped migration immutability: ${root} is not a git worktree (GitHub Actions enforces it)`,
		};
	}
	try {
		const baseRef = migrationBaseRef(env);
		const decoder = new TextDecoder();
		const mergeBase = decoder
			.decode(await gitStdout(root, ["merge-base", baseRef, "HEAD"]))
			.trim();
		if (!mergeBase)
			throw new Error(`git merge-base returned no commit for ${baseRef}`);
		const listed = decoder.decode(
			await gitStdout(root, [
				"ls-tree",
				"-r",
				"-z",
				"--name-only",
				mergeBase,
				"--",
				"go/internal/store/migrations",
			]),
		);
		const baseFiles = new Map<string, Uint8Array>();
		for (const path of listed.split("\0").filter(Boolean)) {
			if (!path.endsWith(".sql")) continue;
			// go:embed takes only top-level *.sql; a nested one is not a migration.
			if (path.slice("go/internal/store/migrations/".length).includes("/"))
				continue;
			baseFiles.set(
				path,
				await gitStdout(root, ["show", `${mergeBase}:${path}`]),
			);
		}
		const currentFiles = new Map<string, Uint8Array>();
		for await (const name of new Bun.Glob("*.sql").scan(
			`${root}/go/internal/store/migrations`,
		)) {
			const path = `go/internal/store/migrations/${name}`;
			currentFiles.set(
				path,
				new Uint8Array(await Bun.file(`${root}/${path}`).arrayBuffer()),
			);
		}
		// First-parent: each step is one integration into the base branch, so the
		// order is merge order, not the commit dates of the branches merged in.
		const addedInOrder = decoder
			.decode(
				await gitStdout(root, [
					"log",
					"--first-parent",
					"--diff-merges=first-parent",
					"--no-renames",
					"--diff-filter=A",
					"--reverse",
					"--format=",
					"--name-only",
					mergeBase,
					"--",
					"go/internal/store/migrations",
				]),
			)
			.split("\n")
			.filter(Boolean);
		const violations = findMigrationViolations(
			baseFiles,
			currentFiles,
			movableMigrations(baseFiles, addedInOrder),
		);
		if (violations.length === 0) return { name, code: 0, output: "" };
		// Runtime verification rejects changed bytes, so an allowlist would ship a boot failure.
		return {
			name,
			code: 1,
			output: violations
				.map(
					(path) =>
						`${path}: migrations are append-only; add a new numbered migration instead.`,
				)
				.join("\n"),
		};
	} catch (error) {
		return {
			name,
			code: 2,
			output: `sql-migration-gate: could not check migration immutability: ${error instanceof Error ? error.message : String(error)}`,
		};
	}
}

/**
 * Build the production `runLinter`: resolve the migration glob to real file
 * paths under `root` and spawn the linter over them. Exported so the real
 * spawn path (including the missing-binary throw → code 2) is unit-testable.
 */
export function makeSpawnLinter(
	root: string,
): (name: string, argv: string[]) => Promise<LinterResult> {
	return async (name, argv) => {
		// Glob expansion is the shell's job in the original gate; do it here
		// so each linter receives real file paths, not a literal glob. A glob
		// that matches nothing is a hard error — a gate with no subject must
		// not read as green.
		const glob = new Bun.Glob(MIGRATION_GLOB);
		const files = [...glob.scanSync({ cwd: root, onlyFiles: true })]
			.map((f) => f.replaceAll("\\", "/"))
			.sort();
		if (files.length === 0) {
			return {
				name,
				code: 2,
				output: `sql-migration-gate: no migrations matched ${MIGRATION_GLOB} under ${root}`,
			};
		}
		// argv is [<glob>] for squawk, ["lint", <glob>] for sqruff — replace
		// the glob token with the resolved file list.
		const resolved = argv.flatMap((a) => (a === MIGRATION_GLOB ? files : [a]));
		try {
			const proc = Bun.spawn([name, ...resolved], {
				cwd: root,
				stdout: "pipe",
				stderr: "pipe",
			});
			const [stdout, stderr] = await Promise.all([
				new Response(proc.stdout).text(),
				new Response(proc.stderr).text(),
			]);
			const code = await proc.exited;
			return { name, code, output: (stdout + stderr).trimEnd() };
		} catch (e) {
			// Bun.spawn throws synchronously when the binary is missing
			// (ENOENT — e.g. a PATH regression or running outside the dev
			// shell). Map it to the documented code 2 with a clean message
			// instead of letting it escape as an unhandled rejection with a
			// raw stack trace. Still fail-closed: 2 dominates the combine.
			return {
				name,
				code: 2,
				output: `sql-migration-gate: could not spawn ${name}: ${e instanceof Error ? e.message : String(e)}`,
			};
		}
	};
}

if (import.meta.main) {
	// Resolve the root the linters run from and the migration glob resolves
	// against, in priority order: explicit GATE_ROOT, then the git toplevel,
	// then process.cwd(). The git toplevel is empty in a jj workspace (its .git
	// lives in the colocated clone, not the workspace) — a legitimate local dev
	// context, not an error — and moon runs this with runFromWorkspaceRoot:true
	// so cwd is the repo root in CI. Falling back to cwd (never "") keeps every
	// valid environment working without the empty-string-as-path fragility.
	const gitTop = (
		await $`git rev-parse --show-toplevel`.nothrow().quiet().text()
	).trim();
	const root = process.env.GATE_ROOT ?? (gitTop || process.cwd());

	process.exit(
		await runOnce({
			runLinter: makeSpawnLinter(root),
			log: (msg) => console.log(msg),
			err: (msg) => console.error(msg),
			runMigrationCheck: () => checkMigrationImmutability(root),
		}),
	);
}
