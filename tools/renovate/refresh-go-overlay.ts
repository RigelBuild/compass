// Renovate postUpgradeTask: couple a go toolchain pin bump to a go-overlay
// input refresh (RIG-3100).
//
// Context. Compass single-sources the go version in
// tools/toolchain/versions/go.nix (`{ version = "1.26.6"; }`, version-only —
// the per-platform hashes come from the go-overlay flake input). Both the dev
// shell (devenv.nix, `go_X_Y_Z` overlay attr) and the parity gate
// (tools/toolchain/gate-tools.nix, `go-bin.versions.<ver>`) select go-overlay
// at that pinned version, and both build over the devenv.lock-pinned
// go-overlay rev. The go customManager (config.json5) opens a branch that
// rewrites ONLY the version string in go.nix. But go-overlay's `versions` set
// only carries go releases at or before the rev pinned in devenv.lock — so when
// Renovate bumps go.nix to a version newer than that rev provides, CI evals
// `go-bin.versions.<new>` against a stale overlay and dies with `attribute
// '"<ver>"' missing` / `gate-tools.nix langs produced no store paths`. This
// task, run on that branch, makes the bump land green in one PR:
//
//   1. Self-gate — exit 0 unless go.nix differs from the base branch, so it's a
//      cheap no-op on every non-go Renovate branch (mirrors
//      refresh-devenv-nixpkgs.ts's devenv.lock gate; the trigger file here is
//      go.nix, the file the go manager rewrites — NOT devenv.lock, which the go
//      bump does not touch).
//   2. Read target — the go version go.nix now pins (the bump's new value).
//   3. Advance the overlay — `devenv update go-overlay` re-locks the go-overlay
//      input in devenv.lock to its latest upstream rev (which provides the new
//      go release). Networked, but light: it re-locks the input, it does NOT
//      build the toolchain. Fail loud: a stale lock must never ship.
//   4. Validate build-free — eval that the new go version now resolves through
//      the SAME path CI uses: gate-tools.nix's `langs.go.version`, which selects
//      `go-bin.versions.<goPin>` over the (now-advanced) overlay rev. A
//      `.version` selector is eval-only (no toolchain build); a still-too-old
//      overlay makes the attr missing and the eval FAILS here, loud and
//      in-branch, instead of red CI on the PR. Assert the evaluated version ===
//      the go.nix target — the core correctness guarantee.
//
// Design: docs/designs/repo/compass-renovate-migration.md
//
// Invoked by the go packageRule's postUpgradeTasks command (config.json5) as
// `bun tools/renovate/refresh-go-overlay.ts`, allowlisted in bot-config.json5.
// Requires `nix` (nix-command) + `devenv` + `bun` + network on the runner PATH
// — the Renovate workflow provisions nix and the vendored devenv shim (there is
// no ambient devenv on compass CI).
//
// Exit codes:
//   0 - overlay advanced + validated (or a no-op branch: go.nix unchanged vs
//       base).
//   1 - a step failed (re-lock or the validation eval) — fail loud, never ship a
//       go bump the overlay cannot resolve.

import { readFileSync } from "node:fs";
import { $ } from "bun";
import { goOverlayLockedRev, goPinVersion } from "./refresh-go-overlay.core.ts";

// The go toolchain pin — the file the go manager rewrites and this task's
// self-gate trigger. The gate file CI evals `langs` from — the same resolver
// the dev shell and the parity gate select go through, so validating its
// `langs.go.version` proves the real CI path. Repo-root-relative (the runner
// cwd = repo root).
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

	// ── Step 3: advance the go-overlay input to a rev that provides it. ──
	// The regex bump moved only go.nix's version string; the overlay rev pinned
	// in devenv.lock may predate that go release, so `go-bin.versions.<new>` is a
	// missing attr. `devenv update go-overlay` re-locks that one input to its
	// latest upstream rev without building the toolchain. Fail loud — a stale
	// lock ships a go bump the overlay cannot resolve.
	const overlayRevBefore = goOverlayLockedRev(
		readFileSync(DEVENV_LOCK, "utf8"),
	);
	console.log(
		`refresh-go-overlay: advancing go-overlay from ${overlayRevBefore} (devenv update go-overlay) ...`,
	);
	await $`devenv update go-overlay`;
	const overlayRevAfter = goOverlayLockedRev(readFileSync(DEVENV_LOCK, "utf8"));
	console.log(`refresh-go-overlay: go-overlay now at ${overlayRevAfter}.`);

	// ── Step 4: validate the new version resolves through the CI path. ──
	// Eval gate-tools.nix's langs.go.version — the same resolver CI runs (ci.yml
	// evals `langs` from this file) — so a pass here proves the PR's CI eval
	// passes. A still-too-old overlay makes the attr missing and this eval throws
	// non-zero, in-branch. Assert the resolved version matches the target so a
	// partial/wrong overlay advance never slips through green.
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
