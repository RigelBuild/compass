#!/usr/bin/env bun
// Renovate postUpgradeTask: refresh the pinned Nix fixed-output-derivation (FOD)
// hashes a dependency bump invalidates, so a dep-bump PR lands green instead of
// red on a `hash mismatch in fixed-output derivation` build break (the RIG-2432
// easy-dep-bump goal, PR #579's failure class).
//
// Compass pins exactly two FOD hashes outside the vendored forks/ trees, each
// content-addressing a fetched dependency set that MOVES when a manifest bumps:
//
//   guest-image/default.nix   vendorHash   compass-guestd's Go module set
//                                          (buildGoModule; no vendor/ dir) —
//                                          invalidated by a go/go.mod|go.sum bump
//   agent-image/entrypoint.nix outputHash  compass-agent's installed node_modules
//                                          tree (recursive FOD of `bun install`) —
//                                          invalidated by a bun.lock bump
//
// Neither is a URL hash a `nix store prefetch-file` can recompute (that is
// refresh-toolchain-hashes.ts's job for the vendored-binary pins). A vendorHash /
// outputHash is only knowable by REALISING the derivation: build it, read the SRI
// Nix reports on the mismatch. Both FODs live in the guest-image rootfs closure
// (guest-image/default.nix imports agent-image/entrypoint.nix with root's pkgs),
// so ONE build vehicle realises both — and with a deliberately-wrong pin the
// build fails FAST at the FOD, never proceeding to the heavy guestd compile or
// erofs pack.
//
// Mechanism, per gated entry:
//   1. Rewrite the entry's hash to a fixed FAKE value.
//   2. `nix build` the rootfs vehicle (--keep-going so a co-stale sibling FOD
//      does not mask this one) — it fails with the entry's real `got:` SRI.
//   3. Parse the `got:` for THIS entry's derivation (matched by a drv-name
//      fragment, so a sibling FOD's mismatch can never be misattributed).
//   4. Write the real SRI back. Fail LOUD (exit 1) if no `got:` is found — a
//      silent no-op would ship the stale pin this task exists to fix.
//
// Self-gating: for each entry, act only when one of its trigger manifests differs
// from the base branch (mirrors refresh-toolchain-hashes.ts's versions/*.nix
// gate). So it is a cheap no-op on every branch that touches neither manifest,
// a gomod bump refreshes only the Go vendorHash, and a bun bump only the bun
// outputHash. Idempotent: re-running rewrites the same SRI.
//
// Wired from config.json5 both at top-level postUpgradeTasks (branch mode —
// covers gomod branches and pure-bun-first TypeScript-rollup branches) and on the
// catalog packageRule (update mode — covers catalog-first rollup branches, where
// the collapsed branch config evicts the top-level branch task; see that rule's
// note). Both are the same `bun tools/renovate/refresh-fod-hashes.ts` command,
// allowlisted once in bot-config.json5 (config.test.ts pins the two together).
//
// Requires `nix` (nix-command) + `bun` + `git` on PATH and network. The build is
// self-contained: `nix build` fetches the Go/bun toolchains it needs into the
// store itself, so only nix + network are load-bearing for the realise step (bun
// runs this script; git drives the self-gate). The Renovate workflow's toolchain
// bootstrap provides all of them (.github/workflows/renovate.yml).
//
// Design: docs/designs/repo/compass-renovate-migration.md
//
// Exit codes:
//   0 - hash(es) refreshed, or a no-op branch (no trigger manifest changed).
//   1 - a step failed (build produced no `got:`, or a marker was missing) —
//       fail loud, never ship a half-refreshed pin set.

import { $ } from "bun";

// The build vehicle that realises both FODs, and the Nix file it is expressed in.
// Repo-root-relative (the runner cwd = repo root; main() chdirs there).
export const BUILD_FILE = "guest-image/default.nix";
export const BUILD_TARGET = "compass-guest-rootfs";

// A syntactically-valid but deliberately-wrong SRI. Building any FOD against it
// forces the `hash mismatch … got: <real>` error we parse. All-A base64 is a
// canonical fake (matches the lib.fakeHash convention the nix files document).
const FAKE_SRI = "sha256-AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=";

// The two pinned FODs. Exported so the test derives its fixtures from the shipped
// table rather than restating it — a rebase that edits the table keeps the test
// honest with no second edit.
export type FodEntry = {
	// Nix file carrying the pinned hash (repo-root-relative).
	file: string;
	// Unique substring on the hash line — the attr name + `= "sha256-` prefix, so
	// it matches exactly one line and cannot bind a comment or a sibling attr.
	marker: string;
	// A fragment of this FOD's derivation name, used to attribute the correct
	// `got:` when --keep-going reports multiple mismatches. Distinct per entry.
	drvFragment: string;
	// Manifests whose change invalidates this FOD (repo-root-relative). The
	// per-entry self-gate fires when any of these differs from the base branch.
	triggers: string[];
};

export const FOD_ENTRIES: FodEntry[] = [
	{
		file: "guest-image/default.nix",
		marker: 'vendorHash = "sha256-',
		drvFragment: "go-modules",
		triggers: ["go/go.mod", "go/go.sum"],
	},
	{
		file: "agent-image/entrypoint.nix",
		marker: 'outputHash = "sha256-',
		drvFragment: "node-modules",
		triggers: ["bun.lock"],
	},
];

// Fragment attribution (parseGotForFragment) matches a mismatch block by
// `drvName.includes(fragment)`, so two fragments where one contains the other
// (e.g. "modules" vs "node-modules") would let one entry's got: bind the other's
// block and write the WRONG hash. The current two fragments are mutually
// disjoint; assert it at load time so a future table edit that reintroduces an
// ambiguous fragment fails loud here rather than silently misattributing at run.
for (const a of FOD_ENTRIES) {
	for (const b of FOD_ENTRIES) {
		if (a !== b && b.drvFragment.includes(a.drvFragment)) {
			throw new Error(
				`renovate-fod: ambiguous drvFragment '${a.drvFragment}' is a substring of ` +
					`'${b.drvFragment}' — fragments must be mutually disjoint for got: attribution`,
			);
		}
	}
}

// ── Pure rewrite: replace the `sha256-…` SRI on the line carrying `marker`. ──
// Unlike refresh-toolchain-hashes.ts (whose hash sits on the line AFTER the URL
// marker), a vendorHash/outputHash IS the marker line. Throws on an empty SRI or
// a missing marker, so a silent no-op can't ship a stale pin. Idempotent.
export function rewriteInlineHash(
	fileText: string,
	marker: string,
	newSri: string,
	file: string,
): string {
	if (!newSri) {
		throw new Error(
			`renovate-fod: empty recomputed hash for '${marker}' in ${file}`,
		);
	}
	const lines = fileText.split("\n");
	const i = lines.findIndex((l) => l.includes(marker));
	if (i === -1) {
		throw new Error(`renovate-fod: marker '${marker}' not found in ${file}`);
	}
	const line = lines[i] as string;
	// Function replacer: newSri is inserted LITERALLY. A string replacement would
	// interpret `$&`/`$1`/`$$` sequences in it — inert for a base64 SRI today
	// (alphabet A-Za-z0-9+/=), but a latent silent-corruption path if the parse
	// ever widened, so keep the write literal.
	lines[i] = line.replace(/sha256-[^"]*/, () => newSri);
	return lines.join("\n");
}

// ── Parse the `got:` SRI for the derivation whose name contains `fragment`. ──
// Nix prints, per mismatching FOD:
//     error: hash mismatch in fixed-output derivation '/nix/store/…-<frag>.drv':
//              specified: sha256-<fake>
//                 got:    sha256-<real>
// Scoping to the fragment means a co-stale sibling FOD's mismatch (only possible
// defensively — the two triggers never change on one branch) is never
// misattributed. Returns undefined if this FOD did not report a mismatch.
export function parseGotForFragment(
	nixOutput: string,
	fragment: string,
): string | undefined {
	const lines = nixOutput.split("\n");
	for (let i = 0; i < lines.length; i++) {
		const l = lines[i] as string;
		if (
			l.includes("hash mismatch in fixed-output derivation") &&
			l.includes(fragment)
		) {
			// Scan from this header to the NEXT mismatch header (or end of output),
			// not a fixed N-line window: the `got:` line's offset below the header is
			// nix's mismatch-block shape, and pinning it to a magic count would break
			// silently if a future nix wrapped the drv path or added context lines.
			for (let j = i + 1; j < lines.length; j++) {
				const lj = lines[j] as string;
				if (lj.includes("hash mismatch in fixed-output derivation")) break;
				const m = lj.match(/got:\s*(sha256-\S+)/);
				if (m?.[1]) return m[1];
			}
		}
	}
	return undefined;
}

// Recompute one entry's SRI: fake the pin, build the vehicle, read the reported
// `got:` for this FOD, then restore the file to the ORIGINAL text (the caller
// writes the real SRI). Restores on every path so a failure never leaves a fake
// pin in the tree.
async function recompute(entry: FodEntry, origText: string): Promise<string> {
	await Bun.write(
		entry.file,
		rewriteInlineHash(origText, entry.marker, FAKE_SRI, entry.file),
	);
	try {
		const res =
			await $`nix build -f ${BUILD_FILE} ${BUILD_TARGET} --no-link --keep-going`
				.nothrow()
				.quiet();
		const out = `${res.stdout.toString()}\n${res.stderr.toString()}`;
		const got = parseGotForFragment(out, entry.drvFragment);
		if (!got) {
			throw new Error(
				`renovate-fod: no 'got:' SRI for '${entry.drvFragment}' after building ` +
					`${BUILD_TARGET} with a faked ${entry.marker} — build output:\n` +
					out.split("\n").slice(-30).join("\n"),
			);
		}
		return got;
	} finally {
		await Bun.write(entry.file, origText);
	}
}

async function main(): Promise<void> {
	// Resolve the repo root from git, not a hardcoded depth: Renovate invokes this
	// as a postUpgradeTask and the paths above are repo-root-relative, so a wrong
	// cwd would silently no-op the gate. git is already required (the gate diffs),
	// so this adds no dependency and is move-proof.
	const repoRoot = (await $`git rev-parse --show-toplevel`.text()).trim();
	process.chdir(repoRoot);

	const baseBranch = process.env.RENOVATE_BASE_BRANCH || "main";
	let baseRef = baseBranch;
	if (
		(await $`git rev-parse --verify -q origin/${baseBranch}`.nothrow().quiet())
			.exitCode === 0
	) {
		baseRef = `origin/${baseBranch}`;
	}

	// Per-entry gate: act only on FODs whose trigger manifests this branch changed
	// vs base. A gomod bump opens the Go entry alone; a bun bump the bun entry
	// alone; a branch touching neither manifest no-ops with no build.
	const gated: FodEntry[] = [];
	for (const entry of FOD_ENTRIES) {
		let changed = false;
		for (const trigger of entry.triggers) {
			const gate = await $`git diff --quiet ${baseRef} -- ${trigger}`
				.nothrow()
				.quiet();
			if (gate.exitCode !== 0) {
				changed = true;
				break;
			}
		}
		if (changed) gated.push(entry);
	}

	if (gated.length === 0) {
		console.log(
			`renovate-fod: no FOD trigger manifest changed vs ${baseRef}; nothing to do.`,
		);
		return;
	}

	for (const entry of gated) {
		const origText = await Bun.file(entry.file).text();
		console.log(
			`renovate-fod: ${entry.file} (${entry.drvFragment}) trigger changed; recomputing ${entry.marker.replace(/ = .*/, "")} ...`,
		);
		const got = await recompute(entry, origText);
		await Bun.write(
			entry.file,
			rewriteInlineHash(origText, entry.marker, got, entry.file),
		);
		console.log(`renovate-fod: ${entry.file} -> ${got}`);
	}

	console.log("renovate-fod: FOD hashes refreshed.");
}

// Run only when executed directly (not imported by the test file).
if (import.meta.main) {
	await main();
}
