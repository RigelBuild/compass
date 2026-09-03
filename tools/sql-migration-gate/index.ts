// sql-migration-gate — SQL migration lint over the first-party migrations
// under go/internal/store/migrations/. Two nix-pinned linters run
// UNCONDITIONALLY and their exit codes are OR'd, so one push surfaces both
// batteries' findings together and the gate fails if EITHER fails:
//
//   squawk  migration-SAFETY analysis (unsafe DDL). Config in /.squawk.toml.
//   sqruff  SQL style/lint (structural + correctness + capitalisation). Config
//           in /.sqruff.
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
// Exit codes:
//   0 - both linters passed (no findings).
//   1 - one or both linters reported findings.
//   2 - a linter could not be spawned / internal error.

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
 * Combine the linters' exit codes into the gate's exit code. Fail-closed: the
 * gate fails (1) if ANY linter reported findings (non-zero), passes (0) only
 * when every linter passed. A spawn/internal failure (code 2) dominates so a
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
	log: (msg: string) => void;
	err: (msg: string) => void;
}

/**
 * Run both linters over the migration glob and combine their exit codes.
 * Both ALWAYS run (findings from both batteries surface in one push) before the
 * codes are combined.
 */
export async function runOnce(deps: Deps): Promise<number> {
	const { runLinter, log, err } = deps;

	const squawk = await runLinter("squawk", [MIGRATION_GLOB]);
	if (squawk.output) err(squawk.output);
	const sqruff = await runLinter("sqruff", ["lint", MIGRATION_GLOB]);
	if (sqruff.output) err(sqruff.output);

	const results = [squawk, sqruff];
	const exit = combineExitCodes(results);
	const verdict = formatVerdict(results);
	if (exit === 0) log(verdict);
	else err(verdict);
	return exit;
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
		}),
	);
}
