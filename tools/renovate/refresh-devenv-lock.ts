// Renovate postUpgradeTask: relock a compass devenv lock at the new
// RigelBuild/devenv fork rev (RIG-2815, RIG-2546 T7).
//
// Context. Compass carries TWO independent devenv scopes, both resolving the
// shared canonical fork `github:RigelBuild/devenv` at its default branch:
// the root dev shell (devenv.yaml/devenv.lock) and the agent base image
// (agent-image/devenv.yaml + agent-image/devenv.lock). Nothing tracked either
// pin, so both drifted behind the fork's `main` until someone relocked by
// hand. RD-1 unifies the SOURCE but deliberately does NOT reconcile the two
// locks, so each is governed on its OWN cadence: one customManager + one
// packageRule + one branch per lock (config.json5).
//
// ── WHY THIS SCRIPT EXISTS AT ALL (do not "simplify" it to
//    lockFileMaintenance) ──
// The obvious mechanism — `lockFileMaintenance` on the lock files — is
// UNIMPLEMENTABLE here. Source-verified against the pinned renovate@44.46.2:
//
//   * `lockFileMaintenance` is MANAGER-scoped: flatten.ts gates it on
//     `manager.supportsLockFileMaintenance`. The `custom.regex` manager
//     exports neither that flag nor an `updateArtifacts`
//     (modules/manager/custom/regex/index.ts), so the option is silently
//     IGNORED for it — no branch, no error, no PR.
//   * The native `nix` manager does support it, but only over
//     `lockFileNames = ['flake.lock']` (modules/manager/nix/index.ts) — never
//     `devenv.lock`, which is devenv's own lock format, and compass has no
//     root flake.nix for it to anchor on either.
//   * And postUpgradeTasks do NOT run on lockFileMaintenance branches at all:
//     they are filtered off them
//     (workers/repository/update/branch/execute-post-upgrade-commands.ts:387-400)
//     because a maintenance branch calls `updateArtifacts()` directly. So even
//     a manager that supported maintenance could not carry this relock.
//
// A NORMAL digest upgrade DOES carry postUpgradeTasks. So the only mechanism
// that works is the repo's own existing pattern (the devenv-nixpkgs channel
// lockstep, refresh-devenv-nixpkgs.ts): a `custom.regex` manager surfaces the
// fork rev as a git-refs digest so Renovate opens a reviewable branch that
// rewrites the rev string, and this task — on that branch — makes the lock
// genuinely consistent:
//
//   1. Self-gate — exit 0 unless a devenv lock differs from the base branch,
//      so it's a cheap no-op on every unrelated Renovate branch (mirrors
//      refresh-devenv-nixpkgs.ts's devenv.lock gate and
//      refresh-go-overlay.ts's go.nix gate).
//   2. Scope — decide WHICH lock the branch touched (root vs agent-image) and
//      relock in that lock's own directory. devenv resolves
//      devenv.yaml/devenv.lock relative to its cwd, so the cwd IS the scope
//      selector. A two-lock branch fails loud: each rule's `fileFilters` names
//      exactly ONE lock, so Renovate would commit one relock and silently drop
//      the other.
//   3. Relock — `nix run <this lock's own fork flakeref> -- update devenv`
//      re-resolves the fork input to its current `main` HEAD and rewrites the
//      WHOLE lock (narHash + lastModified + the fork's transitive nodes), not
//      just the rev the regex bumped. That whole-lock currency is the point: a
//      rev-only rewrite leaves a narHash that does not match the rev it sits
//      beside. Networked, but light: it re-locks inputs, it does NOT build the
//      shell or the image.
//
// Design: docs/designs/repo/compass-renovate-migration.md (§T7), RD-1.
//
// Invoked by BOTH devenv-fork packageRules' postUpgradeTasks (config.json5) as
// `bun tools/renovate/refresh-devenv-lock.ts` — one command string, so ONE
// allowlist entry in bot-config.json5 covers both rules. Requires `nix` +
// `bun` + network on the runner PATH, and a writable HOME (bot-config
// `customEnvVariables.HOME`, which devenv needs for its state dir). It needs
// NO ambient/PATH `devenv`: the task SELF-PROVISIONS the scope-correct devenv
// CLI by `nix run`-ing the fork flakeref read out of the very lock it is about
// to relock (so the root scope runs the root lock's devenv and the agent-image
// scope runs agent-image's — the two revs differ by design under RD-1). That
// mirrors every other agent-image devenv site in the tree (ci.yml:2058-2059)
// and is what keeps the writer of a lock identical to the devenv version that
// lock pins.
//
// Exit codes:
//   0 - the changed lock was relocked (or a no-op branch: neither lock differs
//       from base).
//   1 - a step failed (the relock itself, a lock-shape change, a relock that
//       wrote nothing, an unresolvable base ref, or an unexpected two-lock
//       branch).
//
// What a non-zero exit actually BUYS — do NOT overclaim it (source-verified
// against the pinned renovate@44.46.2):
//   * It does NOT abort the Renovate branch. The failure is caught at
//     execute-post-upgrade-commands.js:112-117, pushed onto `artifactErrors`,
//     and execution CONTINUES. branch/index.js:408-417 reaches the
//     MANAGER_LOCKFILE_ERROR throw only inside its `releaseTimestamp` arm, and
//     a git-refs digest carries NO release timestamp — so this branch always
//     falls through to `commitFilesToBranch` and Renovate commits the
//     regex-bumped lock regardless.
//   * Nor could the script repair the tree instead: `prepareCommit`
//     (util/git/index.js:996-1035) does `git reset --hard` + `git clean -fd`
//     and then RE-WRITES the files from Renovate's in-memory
//     `updatedPackageFiles` — which is where the regex rev-bump lives. So
//     nothing a postUpgradeTask does on disk can suppress that bump.
//   * What the non-zero exit DOES buy is a red `renovate/artifacts` commit
//     status (`setArtifactErrorStatus`, branch/index.js:452). The fail-CLOSED
//     guarantee is that status being a required check, plus the PR's mandatory
//     human review — compass configures NO automerge (config.json5:30).
// So: fail loud → red `renovate/artifacts` status + the human review gate. The
// branch is NOT auto-aborted (see the follow-up issue to make
// `renovate/artifacts` a required check).

import { readFileSync } from "node:fs";
import { $ } from "bun";
import { devenvSource, flakeref } from "../toolchain/devenv-cli/core.ts";
import {
	changedDevenvLock,
	DEVENV_LOCK_PATHS,
	DEVENV_LOCK_SCOPES,
	devenvForkLockedRev,
} from "./refresh-devenv-lock.core.ts";

// The devenv input name, identical in both devenv.yaml files
// (`inputs.devenv.url = github:RigelBuild/devenv`) — the input this task
// re-resolves. Naming the single input (rather than a bare `devenv update`)
// keeps the relock off every other input in the lock, so the PR's diff stays
// the fork advance the branch is about.
const DEVENV_INPUT = "devenv";

async function main(): Promise<number> {
	// Resolve the repo root from git, not a hardcoded depth — Renovate invokes
	// this as a postUpgradeTask and every path below is repo-root-relative, so a
	// wrong cwd would silently no-op the gate. git is already a hard dependency
	// here.
	const repoRoot = (await $`git rev-parse --show-toplevel`.text()).trim();
	process.chdir(repoRoot);

	// ── Step 1: self-gate on a devenv lock changing vs the base branch. ──
	// `RENOVATE_BASE_BRANCH` is a LOCAL/test override ONLY — Renovate does not
	// set it (and could not hand it to a child process through the
	// postUpgradeTask env allowlist anyway); in production this resolves to
	// `main`. The read is kept identical to the sibling
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
		// Fail LOUD with a named diagnosis rather than letting the `git diff`
		// below surface a raw git error (mirrors ci.yml's "Fail LOUD if the base
		// ref does not resolve" gate): an unresolvable base means the changed-set
		// is UNKNOWN, which must never be read as "no lock changed" and skipped
		// green.
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

	// ── Step 3: relock that scope at the fork's current HEAD. ──
	// The regex update moved only the rev string, leaving the lock's narHash /
	// lastModified (and the fork's transitive nodes) describing the OLD rev.
	// `devenv update devenv`, run in this scope's directory, re-resolves the
	// input and rewrites the whole lock consistently.
	const before = readFileSync(lock, "utf8");
	// Provision the SCOPE-CORRECT devenv CLI from this lock's OWN (regex-bumped)
	// content: `before` is what Renovate wrote to disk before this task ran, so
	// its pinned rev is the fork's target HEAD, and the lock therefore gets
	// written by the very devenv version it pins. Using an ambient/PATH devenv
	// instead would relock agent-image under the ROOT lock's devenv (the two
	// revs differ by design — RD-1 unifies the source, not the locks). Mirrors
	// ci.yml's `nix run "$src" -- <subcommand>` idiom.
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
