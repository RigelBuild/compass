// Renovate postUpgradeTask: couple a go toolchain pin bump to a go-overlay input
// refresh (RIG-3100). Compass single-sources the go version in go.nix
// (version-only; hashes from the go-overlay flake input). The dev shell and
// parity gate select go-overlay at that version over the pinned go-overlay rev.

// The go customManager rewrites only the version string. But go-overlay's
// versions set only carries releases at or before the pinned rev, so bumping
// go.nix past that rev makes CI eval go-bin.versions.<new> against a stale
// overlay and die with attribute missing. This task makes the bump land green:

//   1. Self-gate — exit 0 unless go.nix differs from base (trigger is go.nix).
//   2. Read target — the go version go.nix now pins.
//   3. Advance the overlay — devenv update go-overlay re-locks the input in
//      devenv.lock to its latest rev. Networked, no build. Fail loud on stale.

//   4. Validate build-free — eval gate-tools.nix's langs.go.version (the same
//      path CI uses) — a .version selector is eval-only; a still-too-old overlay
//      fails HERE. Assert evaluated === the go.nix target.

// Design: docs/designs/repo/compass-renovate-migration.md. Invoked by the go
// packageRule (config.json5), allowlisted in bot-config.json5. Requires nix +
// devenv + bun + network (the workflow provisions nix and the vendored devenv
// shim; no ambient devenv on compass CI).

// Exit 0 = overlay advanced + validated (or no-op branch); 1 = a step failed —
// fail loud, never ship a go bump the overlay cannot resolve.

import { readFileSync } from "node:fs";
import { $ } from "bun";
import { goOverlayLockedRev, goPinVersion } from "./refresh-go-overlay.core.ts";

// The go toolchain pin — the file the go manager rewrites and this task's
// self-gate trigger. The gate file CI evals langs from — the same resolver the
// dev shell and parity gate use, so validating langs.go.version proves the real
// CI path. Repo-root-relative.
const GO_NIX = "tools/toolchain/versions/go.nix";
const GO_NIX_GATE_FILE = "tools/toolchain/gate-tools.nix";
// The devenv lock the overlay refresh re-locks; read to log the go-overlay rev
// before/after the advance so the coupling is observable in the task log.
const DEVENV_LOCK = "devenv.lock";

/**
 * Read the pinned go version resolved through gate-tools.nix's `langs.go` — the
 * EXACT path CI uses (ci.yml `nix eval -f tools/toolchain/gate-tools.nix
 * langs`), so a validation pass here proves the real CI eval will pass. The
 * `.version` selector is eval-only: it reads the derivation's version attribute
 * without building the toolchain. A go-overlay rev too old to carry the pinned
 * version makes `go-bin.versions.<ver>` a missing attribute, so this eval FAILS
 * — which is the signal.
 *
 * Empirically confirmed against the current tree (go.nix=1.26.6, overlay pinned
 * at 675c971d) — this exact invocation returns `1.26.6`:
 *   nix eval --raw --extra-experimental-features 'nix-command flakes' \
 *     -f tools/toolchain/gate-tools.nix langs.go.version
 */
async function evalResolvedGoVersion(): Promise<string> {
	// --raw so the value is the bare version string, no JSON quoting. The extra
	// experimental-features flag matches how the toolchain hook invokes nix; the
	// runner enables nix-command but we pass it explicitly so a local run
	// (tests, a manual repro) works without relying on the ambient nix.conf.
	const version = (
		await $`nix eval --raw --extra-experimental-features ${"nix-command flakes"} -f ${GO_NIX_GATE_FILE} ${"langs.go.version"}`.text()
	).trim();
	if (!/^\d+\.\d+/.test(version)) {
		throw new Error(
			`refresh-go-overlay: eval of ${GO_NIX_GATE_FILE} langs.go.version yielded a non-version string ${JSON.stringify(version)}`,
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

	// ── Step 1: self-gate on go.nix changing vs the base branch. ──
	// The go bump rewrites go.nix (NOT devenv.lock — the overlay refresh below is
	// what will write devenv.lock), so gate on go.nix, the file the go manager
	// touches.
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
	const goNixUnchanged =
		(await $`git diff --quiet ${baseRef} -- ${GO_NIX}`.nothrow().quiet())
			.exitCode === 0;
	if (goNixUnchanged) {
		console.log(
			`refresh-go-overlay: ${GO_NIX} unchanged vs ${baseRef}; nothing to do.`,
		);
		return 0;
	}

	// ── Step 2: read the target go version the bump now pins. ──
	const targetVersion = goPinVersion(readFileSync(GO_NIX, "utf8"));
	console.log(`refresh-go-overlay: go.nix target version=${targetVersion}`);

	// Step 3: advance the go-overlay input to a rev that provides the new version.
	// The overlay rev in devenv.lock may predate that go release, making
	// go-bin.versions.<new> a missing attr. devenv update go-overlay re-locks that
	// input to its latest rev without building. Fail loud on a stale lock.
	const overlayRevBefore = goOverlayLockedRev(
		readFileSync(DEVENV_LOCK, "utf8"),
	);
	console.log(
		`refresh-go-overlay: advancing go-overlay from ${overlayRevBefore} (devenv update go-overlay) ...`,
	);
	await $`devenv update go-overlay`;
	const overlayRevAfter = goOverlayLockedRev(readFileSync(DEVENV_LOCK, "utf8"));
	console.log(`refresh-go-overlay: go-overlay now at ${overlayRevAfter}.`);

	// Step 4: validate the new version resolves through the CI path. Eval
	// gate-tools.nix's langs.go.version — the same resolver CI runs — so a pass
	// proves the PR's CI eval passes. A still-too-old overlay throws in-branch.
	// Assert resolved === target so a partial overlay advance never slips green.
	console.log(
		`refresh-go-overlay: validating go ${targetVersion} resolves via ${GO_NIX_GATE_FILE} langs.go ...`,
	);
	const resolved = await evalResolvedGoVersion();
	if (resolved !== targetVersion) {
		throw new Error(
			`refresh-go-overlay: overlay resolves go ${resolved} but go.nix pins ${targetVersion} — ` +
				"the go-overlay refresh did not land the pinned version (rev still too old, or a mismatched advance).",
		);
	}
	console.log(
		`refresh-go-overlay: validated — go-overlay now resolves go ${resolved}. done.`,
	);
	return 0;
}

process.exit(await main());
