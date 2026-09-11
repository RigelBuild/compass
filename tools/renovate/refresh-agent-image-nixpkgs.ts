// Renovate postUpgradeTask: relock the agent base image's devenv lock at a new
// devenv-nixpkgs CHANNEL rev, then refresh the FOD hash that relock can move.
//
// Context. Compass carries TWO independent devenv scopes, and each pins BOTH a
// `cachix/devenv-nixpkgs` channel rev and a `RigelBuild/devenv` fork rev:
// the root dev shell (devenv.yaml/devenv.lock) and the agent base image
// (agent-image/devenv.yaml + agent-image/devenv.lock). Three of those four pins
// were tracked; the agent-image CHANNEL rev was not, so it only ever moved when
// someone relocked by hand. This task is the fourth pin's relock, riding the
// customManager + packageRule pair that surfaces it as a git-refs digest
// (config.json5). RD-1 unifies the devenv SOURCE but deliberately does NOT
// reconcile the two locks, so the two channel revs are legitimately skewed and
// are never compared: each is governed on its OWN cadence, one manager + one
// rule + one branch per pin.
//
// ── WHY THIS IS A SEPARATE SCRIPT FROM refresh-devenv-nixpkgs.ts ──
// That script relocks the ROOT channel and then does root-only lockstep work,
// none of whose couplings exist in this scope:
//
//   * It evals `biome` from the resolved raw nixpkgs and rewrites the biome
//     CATALOG pin in package.json, then re-resolves bun.lock. That exists
//     because the ROOT dev shell BAKES biome from the channel (devenv.nix) and
//     the parity story requires baked==catalog. agent-image bakes nothing into
//     a shell: `agent-image/devenv.nix` is `packages = [ ]` — that devenv exists
//     only to express the container — so there is no baked tool to mirror and no
//     manifest to rewrite.
//   * It locksteps the repo-root `flake.nix` (`inputs.nixpkgs.url` hard-codes
//     the channel rev) and re-locks flake.lock, because the flake-parity gate
//     reds CI on the skew. There is NO `agent-image/flake.nix` — the image is
//     not expressed as a flake — so there is nothing to lockstep.
//
// So this task does the relock and NOTHING of that: no biome eval, no catalog
// pin, no bun.lock, no flake work. Widening the root script to take a scope
// would put the whole root-only tail behind a conditional on a path two live
// Renovate rules already depend on, for no shared behaviour beyond the relock
// itself.
//
// ── WHAT THIS REV GOVERNS, AND THE HAZARD IT CARRIES ──
// The agent-image channel rev resolves the image's toolchain closure:
// `agent-image/toolchain.nix` overlays the repo's own vendored bun, but
// `pkgs.cacert`, `pkgs.devenv`, and `getent` come straight from this pin — and
// `agent-image/entrypoint.nix`'s FOD builder takes `nativeBuildInputs =
// [ pkgs.bun ]` from it too.
//
// That last one is why this task refreshes the FOD hash. entrypoint.nix carries
// a SINGLE `outputHash` literal over the installed `node_modules` tree, and it
// is realised through TWO nixpkgs revs: `agent-image/devenv.nix` imports
// entrypoint.nix with AGENT-IMAGE's `pkgs`, while `guest-image/default.nix`
// imports the SAME file with ROOT's `pkgs` (a divergence that file documents as
// deliberate). One hash for two builders is safe only while both revs resolve to
// a bun producing a byte-identical install tree.
//
// BOTH builders are checked, and that is what makes this step a verification
// rather than a bookkeeping rewrite. refresh-fod-hashes.ts's table carries the
// entrypoint pin TWICE: an AUTHORITATIVE entry realised through
// `guest-image/default.nix` (ROOT's pkgs), which writes the canonical SRI, and a
// VERIFY entry realised through `tools/renovate/agent-image-fod-vehicle.nix` —
// a plain-nix file that imports the SAME entrypoint.nix with the pkgs resolved
// from THIS lock, i.e. the bun the OCI image build actually uses. The verify
// entry recomputes and compares; it never writes.
//
// So on a branch that moves ONLY the agent-image channel, the two vehicles are
// realised in order and their SRIs must agree. Agreement is the quiet path (the
// authoritative rewrite is a no-op and the check logs a confirmation).
// Disagreement means the two channel revs' buns produce different install trees,
// so NO single `outputHash` literal can satisfy both consumers: the refresh
// throws, naming both SRIs, both vehicles and both channel revs, and this task
// exits non-zero. That is a red `renovate/artifacts` on the very PR that moved
// the pin — instead of a green PR whose break waits for the agent-image OCI
// build to find it.
//
// Steps:
//   1. Self-gate — exit 0 unless agent-image/devenv.lock differs from the base
//      branch, so it's a cheap no-op on every unrelated Renovate branch (mirrors
//      refresh-devenv-lock.ts's gate).
//   2. Relock — `nix run <this lock's own fork flakeref> -- update nixpkgs`, run
//      in agent-image/, re-resolves the channel input and rewrites the whole
//      lock (narHash + lastModified + the transitive nixpkgs-src node), not just
//      the rev the regex bumped. Networked, but light: it re-locks inputs, it
//      does NOT build the image.
//   3. FOD refresh + cross-builder verify — recompute agent-image/entrypoint.nix's
//      `outputHash` through refresh-fod-hashes.ts's own machinery (its exported
//      table entries + refreshFodEntries), so there is ONE realise-and-parse
//      implementation rather than a second copy that drifts from its
//      got:-attribution and restore-on-failure discipline. refreshFodEntries
//      writes the authoritative value first, then verifies it through the
//      agent-image vehicle; it fails loud if either build reports no `got:`, or
//      if the two vehicles disagree.
//
// Invoked by the agent-image channel packageRule's postUpgradeTasks
// (config.json5) as `bun tools/renovate/refresh-agent-image-nixpkgs.ts`,
// allowlisted in bot-config.json5. Requires `nix` + `bun` + network on the
// runner PATH, and a writable HOME (bot-config `customEnvVariables.HOME`, which
// devenv needs for its state dir). It needs NO ambient/PATH `devenv`: the task
// SELF-PROVISIONS the scope-correct devenv CLI by `nix run`-ing the fork
// flakeref read out of the very lock it is about to relock, so this lock is
// written by the devenv version IT pins. An ambient devenv would relock
// agent-image under the ROOT lock's devenv version (the two fork revs differ by
// design under RD-1). That mirrors refresh-devenv-lock.ts and every other
// agent-image devenv site in the tree.
//
// That `nix run` also depends on the runner's nix.conf naming the devenv +
// cachix SUBSTITUTERS and their trusted public keys — the fork's `#devenv`
// closure is not on cache.nixos.org and nix ignores the fork flake's own
// `nixConfig.extra-substituters` non-interactively, so without those caches the
// realise cold-compiles the Nix fork + full closure from source and can exhaust
// the runner. They are provided by .github/workflows/renovate.yml's
// `extra_nix_config` block, NOT by anything in this file; config.test.ts asserts
// that block still names both caches + keys.
//
// Exit codes:
//   0 - the lock was relocked, the FOD hash refreshed, and both builders agreed
//       on it (or a no-op branch: the lock does not differ from base).
//   1 - a step failed (the relock itself, a lock-shape change, a relock that
//       wrote nothing, an unresolvable base ref, a FOD realise, or the two
//       vehicles disagreeing about the shared outputHash).
//
// What a non-zero exit actually BUYS — do NOT overclaim it. It does NOT abort
// the Renovate branch: the failure is caught in Renovate's post-upgrade command
// runner, pushed onto `artifactErrors`, and execution continues, so Renovate
// commits the regex-bumped lock regardless. Nor could this script repair the
// tree instead — Renovate's `prepareCommit` resets hard and re-writes the files
// from its in-memory `updatedPackageFiles`, which is where the regex rev-bump
// lives. What the non-zero exit DOES buy is a red `renovate/artifacts` commit
// status plus the PR's mandatory human review (compass configures NO automerge).
// The full source-verified argument is in refresh-devenv-lock.ts's header.

import { readFileSync } from "node:fs";
import { $ } from "bun";
import { devenvSource, flakeref } from "../toolchain/devenv-cli/core.ts";
import {
	AGENT_IMAGE_DIR,
	AGENT_IMAGE_LOCK,
	agentImageNixpkgsRev,
	NIXPKGS_INPUT,
} from "./refresh-agent-image-nixpkgs.core.ts";
import {
	FOD_ENTRIES,
	type FodEntry,
	refreshFodEntries,
} from "./refresh-fod-hashes.ts";

// The FOD entries over the pin this scope's bun realises — BOTH of them: the
// authoritative one (realised through guest-image/default.nix, root's pkgs) and
// the verify one (realised through the agent-image vehicle, THIS lock's pkgs).
// Resolved from refresh-fod-hashes.ts's SHIPPED table by the file they pin,
// rather than restated here, so a table edit that moves or renames an entry
// fails LOUD at this lookup instead of silently skipping the refresh — the
// half-refreshed PR this step exists to prevent.
const ENTRYPOINT_NIX = "agent-image/entrypoint.nix";
function agentImageFodEntries(): FodEntry[] {
	const entries = FOD_ENTRIES.filter((e) => e.file === ENTRYPOINT_NIX);
	if (entries.length === 0) {
		throw new Error(
			`refresh-agent-image-nixpkgs: no FOD_ENTRIES entry pins ${ENTRYPOINT_NIX} — ` +
				"refresh-fod-hashes.ts's table moved, so this relock cannot refresh the outputHash " +
				"its new bun may have invalidated. Refusing to ship a possibly-stale FOD pin.",
		);
	}
	// One of them must realise a vehicle resolved from THIS lock, or the relock
	// below would move a bun that nothing then builds against — exactly the blind
	// spot the verify entry was added to close. Checked here rather than assumed,
	// so dropping the vehicle from the table reds this task instead of quietly
	// reverting it to a one-builder rewrite.
	if (!entries.some((e) => e.vehicleChannelLock === AGENT_IMAGE_LOCK)) {
		throw new Error(
			`refresh-agent-image-nixpkgs: no ${ENTRYPOINT_NIX} FOD entry realises a vehicle ` +
				`resolved from ${AGENT_IMAGE_LOCK}, so this relock's new bun would never build the ` +
				"pin it can invalidate. Restore the agent-image verification vehicle before shipping.",
		);
	}
	// Every entry must ALSO be gated on this lock, or a real Renovate run of
	// refresh-fod-hashes.ts on this branch would no-op while we refresh here —
	// two sites disagreeing about what invalidates the pin.
	for (const entry of entries) {
		if (!entry.triggers.includes(AGENT_IMAGE_LOCK)) {
			throw new Error(
				`refresh-agent-image-nixpkgs: the ${ENTRYPOINT_NIX} FOD entry '${entry.id}' does not ` +
					`list ${AGENT_IMAGE_LOCK} among its triggers, so refresh-fod-hashes.ts no longer ` +
					"agrees that an agent-image channel relock can move that outputHash. Reconcile the " +
					"table with this task before shipping.",
			);
		}
	}
	return entries;
}

async function main(): Promise<number> {
	// Resolve the repo root from git, not a hardcoded depth — Renovate invokes
	// this as a postUpgradeTask and every path below is repo-root-relative, so a
	// wrong cwd would silently no-op the gate. git is already a hard dependency
	// here.
	const repoRoot = (await $`git rev-parse --show-toplevel`.text()).trim();
	process.chdir(repoRoot);

	// ── Step 1: self-gate on the agent-image lock changing vs the base branch. ──
	// `RENOVATE_BASE_BRANCH` is a LOCAL/test override ONLY — Renovate does not
	// set it (and could not hand it to a child process through the
	// postUpgradeTask env allowlist anyway); in production this resolves to
	// `main`. The read is kept identical to the sibling refresh tasks so they
	// share one convention.
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
		// below surface a raw git error: an unresolvable base means the changed-set
		// is UNKNOWN, which must never be read as "the lock did not change" and
		// skipped green.
		throw new Error(
			`refresh-agent-image-nixpkgs: base ref does not resolve — neither \`${remoteRef}\` nor ` +
				`\`${baseBranch}\` names a commit in this repo, so whether ${AGENT_IMAGE_LOCK} changed ` +
				"cannot be computed. Refusing to guess (an unresolvable base must not read as " +
				"'nothing changed').",
		);
	}
	const lockUnchanged =
		(
			await $`git diff --quiet ${baseRef} -- ${AGENT_IMAGE_LOCK}`
				.nothrow()
				.quiet()
		).exitCode === 0;
	if (lockUnchanged) {
		console.log(
			`refresh-agent-image-nixpkgs: ${AGENT_IMAGE_LOCK} unchanged vs ${baseRef}; nothing to do.`,
		);
		return 0;
	}

	// Resolve the FOD entries BEFORE the relock: a table drift must fail the task
	// without having networked or rewritten the lock, so the branch is left in
	// the state Renovate handed it rather than half-processed.
	const fodEntries = agentImageFodEntries();

	// ── Step 2: relock the channel input in the agent-image scope. ──
	// The regex update moved only the rev string, leaving the lock's narHash /
	// lastModified (and the transitive nixpkgs-src node) describing the OLD rev.
	// `devenv update nixpkgs`, run in agent-image/, re-resolves the input and
	// rewrites the whole lock consistently.
	const before = readFileSync(AGENT_IMAGE_LOCK, "utf8");
	// Provision the SCOPE-CORRECT devenv CLI from this lock's OWN content:
	// `before` is what Renovate wrote to disk before this task ran, so the lock
	// gets written by the very devenv version it pins. Mirrors ci.yml's
	// `nix run "$src" -- <subcommand>` idiom.
	const src = flakeref(devenvSource(before));
	// Read the pre-relock rev BEFORE the log, so a malformed pre-relock lock
	// throws with a clean diagnosis rather than mid-log-line.
	const beforeRev = agentImageNixpkgsRev(before);
	console.log(
		`refresh-agent-image-nixpkgs: ${AGENT_IMAGE_LOCK} changed vs ${baseRef}; relocking the ` +
			`'${NIXPKGS_INPUT}' input in ${AGENT_IMAGE_DIR}/ via ${src} (was ${beforeRev}) ...`,
	);
	await $`nix run ${src} -- update ${NIXPKGS_INPUT}`.cwd(AGENT_IMAGE_DIR);

	const after = readFileSync(AGENT_IMAGE_LOCK, "utf8");
	// A relock that wrote NOTHING is the silent half-relock this task exists to
	// surface: the rev the regex bumped would still sit beside the base lock's
	// narHash. Any real re-resolution rewrites narHash + lastModified, so
	// byte-identical content means the relock did not actually run.
	if (after === before) {
		throw new Error(
			`refresh-agent-image-nixpkgs: \`devenv update ${NIXPKGS_INPUT}\` left ${AGENT_IMAGE_LOCK} ` +
				"byte-identical — the regex-bumped rev still sits beside the base lock's narHash. " +
				"Exiting non-zero to red the `renovate/artifacts` status; note that Renovate still " +
				"commits the regex bump (a postUpgradeTask exit does not abort the branch), so the " +
				"human review gate is what keeps this half-relock from merging.",
		);
	}
	// Shape guard: the relocked file must still pin a 40-hex channel rev (throws
	// otherwise).
	console.log(
		`refresh-agent-image-nixpkgs: ${AGENT_IMAGE_LOCK} relocked — '${NIXPKGS_INPUT}' now at ${agentImageNixpkgsRev(after)}.`,
	);

	// ── Step 3: recompute the shared FOD hash, and check BOTH builders. ──
	// entrypoint.nix's `outputHash` content-addresses the installed node_modules
	// tree, and a stale value fails the image build with `hash mismatch in
	// fixed-output derivation`. refreshFodEntries drives refresh-fod-hashes.ts's
	// own machinery, unchanged, so got:-attribution and restore-on-failure are
	// shared rather than re-implemented. It fails loud if a build reports no
	// `got:`.
	//
	// Two entries, one pin, in a fixed order refreshFodEntries enforces:
	//
	//   * the authoritative realise through guest-image/default.nix (ROOT's pkgs)
	//     WRITES the canonical SRI. On an agent-image-channel-only branch that
	//     rewrite is a no-op — root's bun did not move — which is expected.
	//   * the verify realise through tools/renovate/agent-image-fod-vehicle.nix
	//     re-derives the same pin with the pkgs from the lock JUST relocked above,
	//     i.e. the bun this image actually builds with, and COMPARES. This is the
	//     leg that makes the relock's effect on the pin observable at all.
	//
	// Agreement is the quiet path. Disagreement throws — one `outputHash` literal
	// cannot serve two buns with different install trees — reddening
	// `renovate/artifacts` on the branch that moved the pin, rather than letting
	// the break wait for the agent-image OCI build.
	console.log(
		`refresh-agent-image-nixpkgs: recomputing the ${ENTRYPOINT_NIX} outputHash and verifying it ` +
			`through this scope's own bun (${fodEntries.length} vehicle(s)) ...`,
	);
	await refreshFodEntries(fodEntries);

	console.log("refresh-agent-image-nixpkgs: done.");
	return 0;
}

if (import.meta.main) {
	process.exit(await main());
}
