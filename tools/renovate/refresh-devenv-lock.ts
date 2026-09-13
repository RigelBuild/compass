// Renovate postUpgradeTask: relock a compass devenv lock at the new
// RigelBuild/devenv fork rev (RIG-2815, RIG-2546 T7). Two independent devenv
// scopes (root + agent-image), each governed on its own cadence: one
// customManager + packageRule + branch per lock (config.json5).

// Not lockFileMaintenance (verified against renovate@44.46.2): custom.regex
// exports no supportsLockFileMaintenance/updateArtifacts (silently ignored),
// the native nix manager only handles flake.lock, and postUpgradeTasks skip
// maintenance branches. A normal digest upgrade does carry them.

// So a custom.regex manager surfaces the fork rev as a git-refs digest and this
// task, on that branch: 1. self-gate unless a lock differs from base; 2. scope
// by cwd (a two-lock branch fails loud); 3. relock via nix run <fork flakeref>
// -- update devenv, rewriting the whole lock (rev-only leaves a stale narHash).

// Design: docs/designs/repo/compass-renovate-migration.md (§T7), RD-1. Invoked
// by both devenv-fork packageRules; one command string, one bot-config
// allowlist entry. Needs nix + bun + network + a writable HOME. Self-provisions
// the scope-correct devenv from the lock it relocks, keeping writer==pinned rev.

// That nix run needs the runner's nix.conf naming the devenv + cachix
// substituters and keys (RIG-2815 M2): the fork closure is not on
// cache.nixos.org, so absent them the realise cold-compiles from source.
// Provided by renovate.yml, asserted by config.test.ts.

// Exit 0 = relocked (or no-op); 1 = a step failed. A non-zero exit does NOT
// abort the branch (verified against renovate@44.46.2): Renovate commits the
// regex-bumped lock regardless. It buys a red renovate/artifacts status;
// fail-closed rests on that required check plus human review (no automerge).

import { readFileSync } from "node:fs";
import { $ } from "bun";
import { devenvSource, flakeref } from "../toolchain/devenv-cli/core.ts";
import {
	changedDevenvLock,
	DEVENV_LOCK_PATHS,
	DEVENV_LOCK_SCOPES,
	devenvForkLockedRev,
} from "./refresh-devenv-lock.core.ts";

// The devenv input name, identical in both devenv.yaml files. Naming the single
// input (rather than a bare devenv update) keeps the relock off every other
// input, so the PR diff stays the fork advance the branch is about.
const DEVENV_INPUT = "devenv";

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

	// ── Step 2: which lock? (throws loudly on a two-lock branch) ──
	const scope = changedDevenvLock(changedPaths);
	if (scope === null) {
		console.log(
			`refresh-devenv-lock: no devenv lock differs from ${baseRef}; nothing to do.`,
		);
		return 0;
	}
	const { lock, cwd } = DEVENV_LOCK_SCOPES[scope];

	// Step 3: relock that scope at the fork's current HEAD. The regex update moved
	// only the rev string; devenv update devenv, run in this scope's directory,
	// re-resolves the input and rewrites the whole lock consistently.
	const before = readFileSync(lock, "utf8");
	// Provision the SCOPE-CORRECT devenv CLI from this lock's own (regex-bumped)
	// content, so the lock is written by the devenv version it pins. An ambient
	// devenv would relock agent-image under root's devenv (the two revs differ by
	// design — RD-1 unifies the source, not the locks). Mirrors ci.yml's idiom.
	const src = flakeref(devenvSource(before));
	// Read the pre-relock rev BEFORE the log, so a malformed pre-relock lock
	// throws with the scope already known rather than mid-log-line.
	const beforeRev = devenvForkLockedRev(before);
	console.log(
		`refresh-devenv-lock: ${lock} changed vs ${baseRef}; relocking the ` +
			`'${DEVENV_INPUT}' input in ${cwd}/ via ${src} (was ${beforeRev}) ...`,
	);
	await $`nix run ${src} -- update ${DEVENV_INPUT}`.cwd(cwd);

	const after = readFileSync(lock, "utf8");
	// A relock that wrote NOTHING is the silent half-relock this task exists to
	// surface: the rev the regex bumped would still sit beside the base lock's
	// narHash. Any real re-resolution rewrites narHash + lastModified, so
	// byte-identical content means the relock did not actually run.
	if (after === before) {
		throw new Error(
			`refresh-devenv-lock: \`devenv update ${DEVENV_INPUT}\` left ${lock} byte-identical — ` +
				"the regex-bumped rev still sits beside the base lock's narHash. Exiting non-zero to " +
				"red the `renovate/artifacts` status; note that Renovate still commits the regex bump " +
				"(a postUpgradeTask exit does not abort the branch), so the human review gate is what " +
				"keeps this half-relock from merging.",
		);
	}
	// Shape guard: the relocked file must still be a devenv lock pinning a
	// 40-hex fork rev (throws otherwise).
	console.log(
		`refresh-devenv-lock: ${lock} relocked — '${DEVENV_INPUT}' now at ${devenvForkLockedRev(after)}. done.`,
	);
	return 0;
}

process.exit(await main());
