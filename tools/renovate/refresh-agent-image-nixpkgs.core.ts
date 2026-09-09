// Pure decision core for refresh-agent-image-nixpkgs.ts.
//
// Split out from the entry point so the load-bearing read — which nixpkgs
// channel rev the agent base image's devenv lock currently pins — is
// unit-testable without a devenv runner, a network, or a git tree. The entry
// point owns the shell-outs (the base diff, the `nix run <fork flakeref> --
// update nixpkgs` relock, the FOD refresh); this file owns the parse. NO
// shell-outs, NO fs beyond text passed in.
//
// Deliberately its own module rather than a reuse of
// refresh-devenv-nixpkgs.core.ts's `channelNixpkgsRev`: that module's throws are
// named for the ROOT channel task and its scope's coupling story (biome catalog,
// bun.lock, flake.nix), none of which exists here. A fail-loud script's value is
// its diagnosis, so each scope names itself — the same per-script-core
// convention refresh-devenv-lock.core.ts and refresh-go-overlay.core.ts follow.

/**
 * The agent base image's devenv lock, repo-root-relative: the file the
 * customManager rewrites and this task's self-gate trigger. INDEPENDENT of the
 * root `devenv.lock` — RD-1 unifies the devenv SOURCE across the two scopes but
 * deliberately does NOT reconcile the two locks, so the two nixpkgs channel
 * revs are legitimately skewed and are never compared here.
 */
export const AGENT_IMAGE_LOCK = "agent-image/devenv.lock";

/**
 * The directory to run the relock from. devenv resolves devenv.yaml/devenv.lock
 * relative to its cwd, so the cwd IS the scope selector — relocking from the
 * repo root would rewrite the ROOT lock, which this rule's `fileFilters` cannot
 * commit, leaving the agent-image rev bump shipped unrelocked.
 */
export const AGENT_IMAGE_DIR = "agent-image";

/**
 * The devenv input name this task re-resolves — `inputs.nixpkgs.url =
 * github:cachix/devenv-nixpkgs/rolling` in agent-image/devenv.yaml. Naming the
 * single input (rather than a bare `devenv update`) keeps the relock off every
 * other input in the lock, so the PR's diff stays the channel advance the branch
 * is about.
 */
export const NIXPKGS_INPUT = "nixpkgs";

/**
 * The devenv-nixpkgs CHANNEL rev the agent base image resolved, read from the
 * lock's outer `nixpkgs` node (`nodes.nixpkgs.locked.rev`) — the SAME field the
 * customManager's matchString surfaces as a git-refs digest. DISTINCT from the
 * transitive `nixpkgs-src` node (upstream NixOS/nixpkgs), which the relock
 * refreshes from this outer rev and which this function must never return.
 *
 * Read before and after the relock so the advance is observable in the task
 * log, and so a lock-shape change fails the task loudly rather than reporting a
 * bogus rev. Parsed as JSON (devenv.lock is JSON), narrowing with `in`/`typeof`
 * at each level so every access is actually checked — the lock is
 * external-boundary data and no schema validator is in the repo.
 */
export function agentImageNixpkgsRev(devenvLockText: string): string {
	let lock: unknown;
	try {
		lock = JSON.parse(devenvLockText);
	} catch (error) {
		throw new Error(
			`refresh-agent-image-nixpkgs: ${AGENT_IMAGE_LOCK} is not valid JSON: ${String(error)}`,
		);
	}
	const isObj = (v: unknown): v is Record<string, unknown> =>
		typeof v === "object" && v !== null;
	let rev: unknown;
	if (isObj(lock) && "nodes" in lock && isObj(lock.nodes)) {
		const node = lock.nodes.nixpkgs;
		if (isObj(node) && "locked" in node && isObj(node.locked)) {
			rev = node.locked.rev;
		}
	}
	if (typeof rev !== "string" || !/^[a-f0-9]{40}$/.test(rev)) {
		throw new Error(
			`refresh-agent-image-nixpkgs: could not read a 40-hex nixpkgs channel rev from ` +
				`${AGENT_IMAGE_LOCK} (nodes.nixpkgs.locked.rev) — devenv lock shape may have changed.`,
		);
	}
	return rev;
}
