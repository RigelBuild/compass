// Renovate postUpgradeTask: lockstep the baked-biome catalog pin to a
// devenv-nixpkgs channel bump (RIG-2432).
//
// Context. The dev shell bakes biome + markdownlint-cli2 from nixpkgs
// (devenv.nix), whose versions are governed by devenv's nixpkgs channel
// (devenv.yaml → github:cachix/devenv-nixpkgs/rolling), locked by git rev in
// devenv.lock. The customManager in config.json5 surfaces that rev as a
// git-refs digest, so Renovate opens a branch that rewrites ONLY the rev string
// in devenv.lock. That leaves the lock's narHash/lastModified and the inner
// nixpkgs-src node stale, and does nothing about the dev-shell parity story,
// where the baked biome must match the package.json catalog pin. This task, run
// on that branch, makes the PR consistent in one shot:
//
//   1. Self-gate — exit 0 unless devenv.lock differs from the base branch, so
//      it's a cheap no-op on every non-devenv Renovate branch (mirrors
//      refresh-toolchain-hashes.ts's versions/*.nix pin gate).
//   2. Re-lock — `devenv update nixpkgs` re-locks devenv.lock consistently at
//      the new rev (refreshing narHash + the inner nixpkgs-src node the
//      regex-only rewrite left stale). Networked, but light: it re-locks
//      inputs, it does NOT build the dev shell.
//   3. Read baked version — eval the biome version from the RAW inner nixpkgs
//      the channel resolved (devenv.lock's nixpkgs-src rev), not the patched
//      channel flake. Raw nixpkgs never routes .version through a derivation,
//      so this is a pure fetch+eval (seconds, no build) for every channel rev
//      — the patch-independent path.
//   4. Rewrite the biome catalog pin in the root package.json to the evaluated
//      version (no-op when unchanged). Compass bakes markdownlint-cli2 from the
//      same channel, but it carries no catalog pin, so only biome is rewritten.
//   5. `bun install --lockfile-only` — re-resolve bun.lock so the fail-closed
//      `bun install --frozen-lockfile` root-check passes.
//   6. Lockstep the repo-root flake: rewrite flake.nix's inputs.nixpkgs.url to
//      the new devenv.lock channel rev and `nix flake update nixpkgs` to
//      re-lock flake.lock, so the flake-parity gate (flake-gate:flake-parity)
//      does not red on the skew a channel bump otherwise leaves behind.
//
// Design: docs/designs/repo/compass-renovate-migration.md
//
// Invoked by the devenv-nixpkgs packageRule's postUpgradeTasks command
// (config.json5) as `bun tools/renovate/refresh-devenv-nixpkgs.ts`, allowlisted
// in bot-config.json5. Requires `nix` (nix-command) + `devenv` + `bun` + network
// on the runner PATH — the Renovate workflow provisions nix and the vendored
// devenv shim (there is no ambient devenv on compass CI).
//
// Exit codes:
//   0 - pin refreshed (or a no-op branch: devenv.lock unchanged vs base).
//   1 - a step failed (re-lock, eval, or a rewrite) — fail loud, never ship a
//       half-refreshed lock/pin set.

import { readFileSync } from "node:fs";
import { $ } from "bun";
import {
	BIOME_CATALOG_KEY,
	channelNixpkgsRev,
	innerNixpkgsRev,
	rewriteCatalogPin,
	rewriteFlakeNixpkgsUrl,
} from "./refresh-devenv-nixpkgs.core.ts";

// The devenv channel lock + the root manifest whose catalog pin mirrors the
// baked biome. Repo-root-relative (the runner cwd = repo root).
const DEVENV_LOCK = "devenv.lock";
const PACKAGE_JSON = "package.json";
// The repo-root distribution flake, whose inputs.nixpkgs.url hard-codes the
// devenv-nixpkgs channel rev. flake.lock records the same rev; the flake-parity
// gate (tools/toolchain/flake-parity.ts) reds CI when it skews from devenv.lock.
const FLAKE_NIX = "flake.nix";

// The nixpkgs system the dev shell bakes for; eval the same attr set the baked
// derivations come from.
const NIX_SYSTEM = "x86_64-linux";

/** Read a raw-nixpkgs package version at a pinned rev — pure fetch+eval. */
async function evalRawNixpkgsVersion(
	innerRev: string,
	attr: string,
): Promise<string> {
	const flakeRef = `github:NixOS/nixpkgs/${innerRev}#legacyPackages.${NIX_SYSTEM}.${attr}.version`;
	// --raw so the value is the bare version string, no JSON quoting. The extra
	// experimental-features flag matches how the toolchain hook invokes nix; the
	// runner enables nix-command but we pass it explicitly so a local run
	// (tests, a manual repro) works without relying on the ambient nix.conf.
	const version = (
		await $`nix eval --raw --extra-experimental-features ${"nix-command flakes"} ${flakeRef}`.text()
	).trim();
	if (!/^\d+\.\d+/.test(version)) {
		throw new Error(
			`refresh-devenv-nixpkgs: eval of ${flakeRef} yielded a non-version string ${JSON.stringify(version)}`,
		);
	}
	return version;
}

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

	// ── Step 2: re-lock devenv.lock consistently at the new rev. ──
	// The regex update rewrote only the outer nixpkgs rev; narHash, lastModified,
	// and the inner nixpkgs-src node are now inconsistent. `devenv update
	// nixpkgs` re-locks that input (and its transitive nixpkgs-src) without
	// building the shell. Fail loud: a stale lock must never ship.
	console.log(
		"refresh-devenv-nixpkgs: re-locking devenv.lock (devenv update nixpkgs) ...",
	);
	await $`devenv update nixpkgs`;

	// ── Step 3: read the baked version the new rev ships. ──
	// After the re-lock, devenv.lock's inner nixpkgs-src node holds the concrete
	// NixOS/nixpkgs rev the channel resolved. Eval the biome version from THAT
	// raw rev (patch-independent, build-free).
	const innerRev = innerNixpkgsRev(readFileSync(DEVENV_LOCK, "utf8"));
	console.log(
		`refresh-devenv-nixpkgs: evaluating biome from raw nixpkgs ${innerRev} ...`,
	);
	const biomeVersion = await evalRawNixpkgsVersion(innerRev, "biome");
	console.log(`refresh-devenv-nixpkgs: baked biome=${biomeVersion}`);

	// ── Step 4: rewrite the biome catalog pin to the evaluated version. ──
	// No-op when unchanged (a channel bump that doesn't move biome leaves the
	// pin alone). rewriteCatalogPin fails loud if the pin key is absent.
	const before = readFileSync(PACKAGE_JSON, "utf8");
	const after = rewriteCatalogPin(before, BIOME_CATALOG_KEY, biomeVersion);
	if (after !== before) {
		await Bun.write(PACKAGE_JSON, after);
		console.log(
			"refresh-devenv-nixpkgs: rewrote the biome catalog pin in package.json.",
		);
	} else {
		console.log(
			"refresh-devenv-nixpkgs: biome catalog pin already matches the baked version; no rewrite.",
		);
	}

	// ── Step 5: re-resolve bun.lock so the frozen-lockfile root-check passes. ──
	// Same command the catalog-coupling rule runs; idempotent, writes only
	// bun.lock. Skip when step 4 was a no-op (nothing to re-resolve).
	if (after !== before) {
		console.log("refresh-devenv-nixpkgs: bun install --lockfile-only ...");
		await $`bun install --lockfile-only`;
	}

	// ── Step 6: lockstep the repo-root flake to the new channel rev. ──
	// The re-lock in step 2 moved devenv.lock's outer nixpkgs (channel) rev, but
	// flake.nix hard-codes that rev in inputs.nixpkgs.url and flake.lock records
	// it independently — so without this the flake-parity gate
	// (flake-gate:flake-parity) reds on the skew. Rewrite the URL rev to the new
	// channel rev, then re-lock flake.lock to match. `nix flake update nixpkgs`
	// re-locks only the nixpkgs input (no build). Idempotent: a channel bump that
	// somehow left the rev unchanged rewrites nothing and the flake update is a
	// no-op. Fail loud — a half-aligned flake ships a red parity gate.
	const channelRev = channelNixpkgsRev(readFileSync(DEVENV_LOCK, "utf8"));
	const flakeBefore = readFileSync(FLAKE_NIX, "utf8");
	const flakeAfter = rewriteFlakeNixpkgsUrl(flakeBefore, channelRev);
	if (flakeAfter !== flakeBefore) {
		await Bun.write(FLAKE_NIX, flakeAfter);
		console.log(
			`refresh-devenv-nixpkgs: rewrote flake.nix nixpkgs pin to channel rev ${channelRev}.`,
		);
		console.log(
			"refresh-devenv-nixpkgs: re-locking flake.lock (nix flake update nixpkgs) ...",
		);
		await $`nix flake update nixpkgs --extra-experimental-features ${"nix-command flakes"}`;
	} else {
		console.log(
			"refresh-devenv-nixpkgs: flake.nix nixpkgs pin already at the channel rev; no rewrite.",
		);
	}

	console.log("refresh-devenv-nixpkgs: done.");
	return 0;
}

process.exit(await main());
