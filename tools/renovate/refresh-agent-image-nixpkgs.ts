// Renovate postUpgradeTask for the agent-image devenv scope: relock its devenv
// lock at a new devenv-nixpkgs channel rev, then refresh the FOD hash that
// relock can move. The two devenv scopes' channel revs are governed
// independently (config.json5 customManager + packageRule per pin).

// Separate from refresh-devenv-nixpkgs.ts: that script's root-only tail (biome
// catalog eval, bun.lock, flake.nix lockstep) has no analogue here — agent-image
// bakes nothing into a shell and is not expressed as a flake.

// entrypoint.nix's single outputHash is realised through TWO nixpkgs revs
// (agent-image's and root's), so one hash is safe only while both buns produce a
// byte-identical install tree. refresh-fod-hashes.ts writes the authoritative SRI
// then verifies via the agent-image vehicle, failing loud if they disagree.

// Self-provisions the scope-correct devenv by nix run-ing the fork flakeref read
// from this lock (an ambient devenv would relock under root's fork rev, which
// differs by design under RD-1). Needs renovate.yml's devenv+cachix substituters
// or the fork closure cold-compiles from source.

// Non-zero exit does NOT abort the branch — Renovate commits the regex-bumped
// lock regardless; it buys a red renovate/artifacts status + mandatory human
// review. Full argument in refresh-devenv-lock.ts's header.

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

// BOTH entrypoint.nix FOD entries: the authoritative one (root's pkgs) and the
// verify one (this lock's pkgs). Resolved from refresh-fod-hashes.ts's shipped
// table by the file they pin, so a table edit that moves an entry fails loud
// here instead of silently skipping the refresh.
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
	// One entry must realise a vehicle resolved from THIS lock, else the relock
	// moves a bun nothing then builds against — the blind spot the verify entry
	// closes. Checked so dropping the vehicle reds this task.
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

	// Step 1: self-gate on the agent-image lock changing vs the base branch.
	// RENOVATE_BASE_BRANCH is a LOCAL/test override only — Renovate does not set
	// it; in production this resolves to main. Kept identical to sibling refresh
	// tasks so they share one convention.
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

	// Step 2: relock the channel input in the agent-image scope. The regex update
	// moved only the rev string; devenv update nixpkgs re-resolves the input and
	// rewrites narHash/lastModified/nixpkgs-src consistently.
	const before = readFileSync(AGENT_IMAGE_LOCK, "utf8");
	// Provision the scope-correct devenv CLI from this lock's own content, so the
	// lock is written by the devenv version it pins. Mirrors ci.yml's
	// nix run "$src" -- <subcommand> idiom.
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
	// A relock that wrote NOTHING is the silent half-relock this task surfaces:
	// any real re-resolution rewrites narHash + lastModified, so byte-identical
	// content means the relock did not run.
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

	// Step 3: recompute the shared FOD hash and check BOTH builders. A stale
	// outputHash fails the image build with hash mismatch. refreshFodEntries
	// drives refresh-fod-hashes.ts's machinery, sharing got:-attribution and
	// restore-on-failure.

	// Two entries, one pin, fixed order: the authoritative realise (root's pkgs)
	// writes the canonical SRI (a no-op on an agent-image-only branch); the verify
	// realise re-derives with the just-relocked bun and compares. Disagreement
	// throws — reddening renovate/artifacts rather than waiting for the OCI build.
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
