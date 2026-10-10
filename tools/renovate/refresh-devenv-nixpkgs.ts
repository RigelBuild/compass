// Renovate postUpgradeTask: finish a devenv-nixpkgs channel bump (RIG-2432).
// The customManager rewrites only the rev string, leaving devenv.lock and the
// repo-root flake stale.

// This task, on that branch, makes the PR consistent: 1. self-gate unless
// devenv.lock differs from base; 2. devenv update nixpkgs re-locks at the new
// rev; 3. lockstep flake.nix's nixpkgs url + nix flake update, so the
// flake-parity gate does not red on the skew. The biome catalog pin follows
// Meissa, not this channel; refresh-biome-catalog.ts runs after this task.

// Design: docs/designs/repo/compass-renovate-migration.md. Invoked by the
// devenv-nixpkgs packageRule (config.json5), allowlisted in bot-config.json5.
// Requires nix + devenv + bun + network on the runner PATH (the workflow
// provisions nix and the vendored devenv shim; no ambient devenv on compass CI).

// Exit 0 = pin refreshed (or no-op branch); 1 = a step failed — fail loud, never
// ship a half-refreshed lock/pin set.

import { readFileSync } from "node:fs";
import { $ } from "bun";
// The flake-parity gate's own rev extractor — reused here so step 6 keys its
// re-lock off flake.lock's ACTUAL recorded rev (the exact value the gate
// compares), not merely off flake.nix's text having changed.
import { nixpkgsLockedRev } from "../toolchain/flake-parity-core.ts";
import {
	channelNixpkgsRev,
	rewriteFlakeNixpkgsUrl,
} from "./refresh-devenv-nixpkgs.core.ts";

// The devenv channel lock. Repo-root-relative (the runner cwd = repo root).
const DEVENV_LOCK = "devenv.lock";
// The repo-root distribution flake, whose inputs.nixpkgs.url hard-codes the
// devenv-nixpkgs channel rev. flake.lock records the same rev; the flake-parity
// gate (tools/toolchain/flake-parity.ts) reds CI when it skews from devenv.lock.
const FLAKE_NIX = "flake.nix";
const FLAKE_LOCK = "flake.lock";

async function main(): Promise<number> {
	// Resolve the repo root from git, not a hardcoded depth — Renovate invokes
	// this as a postUpgradeTask and every path below is repo-root-relative, so a
	// wrong cwd would silently no-op the gate. git is already a hard dependency
	// here.
	const repoRoot = (await $`git rev-parse --show-toplevel`.text()).trim();
	process.chdir(repoRoot);

	// ── Step 1: self-gate on devenv.lock changing vs the base branch. ──
	const baseBranch = process.env.RENOVATE_BASE_BRANCH || "main";
	// Prefer the remote-tracking ref (matches the toolchain hook); fall back to
	// the bare branch name when origin/<base> is absent (a local run).
	let baseRef = baseBranch;
	if (
		(
			await $`git rev-parse --verify -q ${`origin/${baseBranch}`}`
				.nothrow()
				.quiet()
		).exitCode === 0
	) {
		baseRef = `origin/${baseBranch}`;
	}
	const lockUnchanged =
		(await $`git diff --quiet ${baseRef} -- ${DEVENV_LOCK}`.nothrow().quiet())
			.exitCode === 0;
	if (lockUnchanged) {
		console.log(
			`refresh-devenv-nixpkgs: ${DEVENV_LOCK} unchanged vs ${baseRef}; nothing to do.`,
		);
		return 0;
	}

	// Step 2: re-lock devenv.lock consistently at the new rev. The regex update
	// rewrote only the outer rev; devenv update nixpkgs re-locks that input (and
	// its transitive nixpkgs-src) without building. Fail loud: a stale lock must
	// never ship.
	console.log(
		"refresh-devenv-nixpkgs: re-locking devenv.lock (devenv update nixpkgs) ...",
	);
	await $`devenv update nixpkgs`;

	// Step 3: lockstep the repo-root flake to the new channel rev. Step 2 moved
	// devenv.lock's outer nixpkgs rev, but flake.nix hard-codes it and flake.lock
	// records it independently, so without this the flake-parity gate reds.
	// Rewrite the URL rev, then nix flake update nixpkgs re-locks flake.lock.

	// Gated on flake.lock's ACTUAL recorded rev, not just flake.nix's text: a
	// half-aligned state (flake.nix at the new rev but flake.lock stale, from a
	// prior run that committed the rewrite then failed the update) must still
	// re-lock. Idempotent; fail loud — a half-aligned flake ships a red gate.
	const channelRev = channelNixpkgsRev(readFileSync(DEVENV_LOCK, "utf8"));
	const flakeBefore = readFileSync(FLAKE_NIX, "utf8");
	const flakeAfter = rewriteFlakeNixpkgsUrl(flakeBefore, channelRev);
	const flakeNixRewritten = flakeAfter !== flakeBefore;
	if (flakeNixRewritten) {
		await Bun.write(FLAKE_NIX, flakeAfter);
		console.log(
			`refresh-devenv-nixpkgs: rewrote flake.nix nixpkgs pin to channel rev ${channelRev}.`,
		);
	}
	// flake.lock's recorded rev — null if the lock is absent/misshapen, which we
	// treat as "needs a re-lock" (the update will (re)create it) rather than a
	// silent skip.
	const flakeLockRev = nixpkgsLockedRev(readFileSync(FLAKE_LOCK, "utf8"));
	if (flakeNixRewritten || flakeLockRev !== channelRev) {
		console.log(
			"refresh-devenv-nixpkgs: re-locking flake.lock (nix flake update nixpkgs) ...",
		);
		await $`nix flake update nixpkgs --extra-experimental-features ${"nix-command flakes"}`;
	} else {
		console.log(
			"refresh-devenv-nixpkgs: flake.nix + flake.lock already at the channel rev; no flake re-lock.",
		);
	}

	console.log("refresh-devenv-nixpkgs: done.");
	return 0;
}

process.exit(await main());
