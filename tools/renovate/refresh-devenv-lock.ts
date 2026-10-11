// Renovate postUpgradeTask relocks every changed devenv lock at the fork's new
// HEAD. The shared fork rule handles root and agent-image in one branch.

// Not lockFileMaintenance (verified against renovate@44.46.2): custom.regex
// exports no supportsLockFileMaintenance/updateArtifacts (silently ignored),
// the native nix manager only handles flake.lock, and postUpgradeTasks skip
// maintenance branches. A normal digest upgrade does carry them.

// A custom.regex manager surfaces the fork rev as a git-refs digest. This task
// self-gates, relocks each changed scope in its own cwd, and fails fast.

// Design: docs/designs/repo/compass-renovate-devenv-fork-group/design.md.
// The single fork rule invokes this task; each scope uses its own pinned devenv.

// That nix run needs the runner's nix.conf naming the devenv + cachix
// substituters and keys (RIG-2815 M2): the fork closure is not on
// cache.nixos.org, so absent them the realise cold-compiles from source.
// Provided by renovate.yml, asserted by config.test.ts.

// Exit 0 = relocked (or no-op); 1 = a step failed. A non-zero exit does NOT
// abort the branch (verified against renovate@44.46.2): Renovate commits the
// regex-bumped lock regardless. It reds the advisory renovate/artifacts status;
// renovate:lock-integrity in the required rollup is what blocks the merge.

import { readFileSync } from "node:fs";
import { $ } from "bun";
import { devenvSource, flakeref } from "../toolchain/devenv-cli/core.ts";
import type { DevenvLockScope } from "./refresh-devenv-lock.core.ts";
import {
	changedDevenvLocks,
	DEVENV_LOCK_PATHS,
	DEVENV_LOCK_SCOPES,
	devenvForkLockedRev,
} from "./refresh-devenv-lock.core.ts";

// The devenv input name, identical in both devenv.yaml files. Naming the single
// input (rather than a bare devenv update) keeps the relock off every other
// input, so the PR diff stays the fork advance the branch is about.
const DEVENV_INPUT = "devenv";

/** Today's per-scope relock steps; throws on any failed step. */
async function relockScope(
	scope: DevenvLockScope,
	baseRef: string,
): Promise<void> {
	const { lock, cwd } = DEVENV_LOCK_SCOPES[scope];
	const before = readFileSync(lock, "utf8");
	// Provision the CLI from this lock's own bumped content, so the lock is
	// written by the devenv version it pins. A hand relock can leave revs different.
	const src = flakeref(devenvSource(before));
	const beforeRev = devenvForkLockedRev(before);
	console.log(
		`refresh-devenv-lock: ${lock} changed vs ${baseRef}; relocking the ` +
			`'${DEVENV_INPUT}' input in ${cwd}/ via ${src} (was ${beforeRev}) ...`,
	);
	await $`nix run ${src} -- update ${DEVENV_INPUT}`.cwd(cwd);

	const after = readFileSync(lock, "utf8");
	if (after === before) {
		throw new Error(
			`refresh-devenv-lock: \`devenv update ${DEVENV_INPUT}\` left ${lock} byte-identical — ` +
				"the regex-bumped rev still sits beside the base lock's narHash. Exiting non-zero to " +
				"red the `renovate/artifacts` status; note that Renovate still commits the regex bump " +
				"(a postUpgradeTask exit does not abort the branch), so renovate:lock-integrity in the " +
				"required rollup is what keeps this half-relock from merging.",
		);
	}
	console.log(
		`refresh-devenv-lock: ${lock} relocked — '${DEVENV_INPUT}' now at ${devenvForkLockedRev(after)}. done.`,
	);
}

async function main(): Promise<number> {
	// Resolve the repo root from git, not a hardcoded depth — every path below is
	// repo-root-relative, so a wrong cwd would silently no-op the gate.
	const repoRoot = (await $`git rev-parse --show-toplevel`.text()).trim();
	process.chdir(repoRoot);

	// Step 1: self-gate on a devenv lock changing vs the base branch.
	// RENOVATE_BASE_BRANCH is a LOCAL/test override only — Renovate does not set
	// it; in production this resolves to main. Kept identical to sibling
	// refresh-devenv-nixpkgs.ts so the two tasks share one convention.
	const baseBranch = process.env.RENOVATE_BASE_BRANCH || "main";
	// Prefer the remote-tracking ref (matches the sibling refresh tasks); fall
	// back to the bare branch name when origin/<base> is absent (a local run).
	const resolves = async (ref: string): Promise<boolean> =>
		(await $`git rev-parse --verify -q ${ref}`.nothrow().quiet()).exitCode ===
		0;
	const remoteRef = `origin/${baseBranch}`;
	let baseRef: string;
	if (await resolves(remoteRef)) {
		baseRef = remoteRef;
	} else if (await resolves(baseBranch)) {
		baseRef = baseBranch;
	} else {
		// Fail LOUD with a named diagnosis rather than a raw git error (mirrors
		// ci.yml's base-ref gate): an unresolvable base means the changed-set is
		// UNKNOWN, which must never read as "no lock changed" and skip green.
		throw new Error(
			`refresh-devenv-lock: base ref does not resolve — neither \`${remoteRef}\` nor ` +
				`\`${baseBranch}\` names a commit in this repo, so the changed devenv-lock set ` +
				"cannot be computed. Refusing to guess (an unresolvable base must not read as " +
				"'nothing changed').",
		);
	}
	const changedPaths = (
		await $`git diff --name-only ${baseRef} -- ${DEVENV_LOCK_PATHS}`.text()
	)
		.split("\n")
		.map((line) => line.trim())
		.filter((line) => line.length > 0);

	// Relock every changed lock in declared scope order; a failure stops the loop.
	const scopes = changedDevenvLocks(changedPaths);
	if (scopes.length === 0) {
		console.log(
			`refresh-devenv-lock: no devenv lock differs from ${baseRef}; nothing to do.`,
		);
		return 0;
	}
	for (const scope of scopes) {
		await relockScope(scope, baseRef);
	}
	return 0;
}

process.exit(await main());
