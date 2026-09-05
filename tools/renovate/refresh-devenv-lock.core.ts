// Pure decision core for refresh-devenv-lock.ts (RIG-2815).
//
// Split out from the entry point so the two load-bearing decisions — WHICH of
// the two devenv locks a Renovate branch touched, and which rev that lock
// currently pins — are unit-testable without a devenv runner, a network, or a
// git tree. Picking the wrong scope here would relock the WRONG lock file, and
// the packageRule's `fileFilters` would then silently drop the write (it is an
// INCLUDE allowlist naming exactly one lock), shipping a PR whose rev bump was
// never followed by a real relock. The entry point owns the shell-outs (the
// base diff, the `nix run <fork flakeref> -- update devenv` relock); this file
// owns the decisions. NO shell-outs, NO fs beyond text passed in.

/**
 * The two independently-locked devenv scopes in this repo. RD-1 unifies the
 * devenv SOURCE (both `github:RigelBuild/devenv`) but deliberately does NOT
 * reconcile the two locks — each tracks the fork on its own cadence, so each
 * gets its own manager, its own packageRule, and its own branch.
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
 * Which devenv lock this branch changed, decided from the paths a diff against
 * the branch point reported.
 *
 * - Exactly one lock changed → that scope (the normal Renovate branch: one
 *   manager, one rule, one lock).
 * - No lock changed → `null`, the self-gate no-op. This is the common case:
 *   the task rides one specific rule, but a maintainer copy-pasting it onto
 *   another rule (or a manual run) must be a cheap no-op, not a spurious
 *   relock.
 * - BOTH locks changed → throws. The two rules carry distinct groupNames
 *   precisely so they never share a branch, so this shape means an assumption
 *   broke. Relocking either one would be worse than useless: each rule's
 *   `fileFilters` names ONE lock, so Renovate would commit one relock and
 *   silently discard the other — a PR that bumped a rev without relocking it.
 *   Exit non-zero instead. That exit does NOT abort the branch (Renovate
 *   commits the regex bump regardless); it reds the `renovate/artifacts`
 *   status, and the human review gate is what stops the merge. See the
 *   entry point's "what a non-zero exit actually buys" note.
 *
 * Paths are compared exactly (repo-root-relative, as `git diff --name-only`
 * emits them), so an unrelated `foo/devenv.lock` can never be mistaken for
 * either scope.
 */
export function changedDevenvLock(
	changedPaths: readonly string[],
): DevenvLockScope | null {
	const changed = (Object.keys(DEVENV_LOCK_SCOPES) as DevenvLockScope[]).filter(
		(scope) => changedPaths.includes(DEVENV_LOCK_SCOPES[scope].lock),
	);
	if (changed.length > 1) {
		throw new Error(
			`refresh-devenv-lock: ${changed.length} devenv locks changed vs the branch point ` +
				`(${changed.map((s) => DEVENV_LOCK_SCOPES[s].lock).join(", ")}) — each packageRule's ` +
				"fileFilters names exactly ONE lock, so a two-lock branch would commit one relock and " +
				"silently drop the other. The two rules carry distinct groupNames so they never share a " +
				"branch; this shape means that invariant broke. Exiting non-zero reds the " +
				"`renovate/artifacts` status — it does not abort the branch — so the mandatory human " +
				"review of the PR is what keeps this shape from merging.",
		);
	}
	return changed[0] ?? null;
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
