// Pure decision core for refresh-devenv-lock.ts (RIG-2815): which devenv locks a
// branch touched, and which rev each pins — unit-testable without a devenv runner,
// network, or git tree. The wrong scope relocks the sibling and leaves this lock
// unrelocked.

/**
 * The two separately-locked devenv scopes in this repo. A fork bump moves both
 * locks in one PR, but each lock remains written by its own devenv CLI.
 */
export type DevenvLockScope = "root" | "agent-image";

/**
 * Per-scope geometry: the repo-root-relative lock path (the file the
 * customManager rewrites and this task's self-gate trigger) and the
 * repo-root-relative directory to run `devenv update devenv` from. devenv
 * resolves devenv.yaml/devenv.lock relative to its cwd, so the cwd IS the
 * scope selector — `.` for the root dev shell, `agent-image` for the agent
 * base image's own devenv.
 */
export const DEVENV_LOCK_SCOPES: Record<
	DevenvLockScope,
	{ readonly lock: string; readonly cwd: string }
> = {
	root: { lock: "devenv.lock", cwd: "." },
	"agent-image": { lock: "agent-image/devenv.lock", cwd: "agent-image" },
};

/**
 * Every scope's lock path, in scope order — the git-diff pathspec. `readonly`
 * for symmetry with the `readonly lock`/`readonly cwd` fields above; it still
 * spreads into a `$`-template pathspec fine.
 */
export const DEVENV_LOCK_PATHS: readonly string[] = Object.values(
	DEVENV_LOCK_SCOPES,
).map((s) => s.lock);

/**
 * Every scope whose lock is in changedPaths (exact match), in DEVENV_LOCK_SCOPES
 * key order; [] when none. Never throws.
 */
export function changedDevenvLocks(
	changedPaths: readonly string[],
): readonly DevenvLockScope[] {
	return (Object.keys(DEVENV_LOCK_SCOPES) as DevenvLockScope[]).filter(
		(scope) => changedPaths.includes(DEVENV_LOCK_SCOPES[scope].lock),
	);
}

/**
 * The concrete `github:RigelBuild/devenv` fork rev a devenv lock pins, read
 * from its `devenv` node (`nodes.devenv.locked.rev`) — the SAME field the
 * customManager's matchString surfaces as a git-refs digest. Read before and
 * after the relock so the advance is observable in the task log, and so a
 * relock that moved nothing fails loud rather than shipping a rev-only rewrite
 * with a stale narHash. Parsed as JSON (devenv.lock is JSON), throwing loudly
 * if the node or a 40-hex rev is absent — a lock-shape change must fail the
 * task, never silently read the wrong rev.
 */
export function devenvForkLockedRev(devenvLockText: string): string {
	let lock: unknown;
	try {
		lock = JSON.parse(devenvLockText);
	} catch (error) {
		throw new Error(
			`refresh-devenv-lock: devenv.lock is not valid JSON: ${String(error)}`,
		);
	}
	// Narrow with `in`/`typeof` at each level so every access is actually
	// checked (devenv.lock is external-boundary data; no schema validator is in
	// the repo). A shape change surfaces as the loud throw below, never a
	// silently-wrong read.
	const isObj = (v: unknown): v is Record<string, unknown> =>
		typeof v === "object" && v !== null;
	let rev: unknown;
	if (isObj(lock) && "nodes" in lock && isObj(lock.nodes)) {
		const node = lock.nodes.devenv;
		if (isObj(node) && "locked" in node && isObj(node.locked)) {
			rev = node.locked.rev;
		}
	}
	if (typeof rev !== "string" || !/^[a-f0-9]{40}$/.test(rev)) {
		throw new Error(
			"refresh-devenv-lock: could not read a 40-hex devenv fork rev from devenv.lock " +
				"(nodes.devenv.locked.rev) — devenv lock shape may have changed.",
		);
	}
	return rev;
}
