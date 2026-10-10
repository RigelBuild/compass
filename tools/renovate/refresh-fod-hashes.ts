#!/usr/bin/env bun
// Renovate postUpgradeTask: refresh the pinned Nix fixed-output-derivation (FOD)
// hashes a dependency bump invalidates, so a dep-bump PR lands green instead of
// red on a "hash mismatch in fixed-output derivation" build break (RIG-2432,
// PR #579's failure class).

// Compass pins three FOD hash VALUES. The Go vendorHash lives in two files that
// share it by design: guest-image/default.nix and flake.nix — the SAME
// proxyVendor hash over go/, both moved by a go.mod|go.sum bump. The vehicle
// realises only guestd's FOD; flake.nix is a MIRROR (FodEntry.mirrorFiles).

// agent-image/entrypoint.nix's outputHash content-addresses compass-agent's
// installed node_modules (a bun install FOD) built by the pinned bun. Invalidated
// by a bun.lock or bun-pin bump; the agent-image and root devenv.lock channel
// bumps stay fail-safe triggers.

// apps/ui/dist.nix's outputHash pins the bun-workspace node_modules closure.
// It moves with bun.lock, root and workspace package manifests, the bun pin, or
// flake.lock, which supplies the flake vehicle's pkgs.

// Neither is a URL hash prefetch-file can recompute — a vendorHash/outputHash is
// only knowable by REALISING the derivation and reading the SRI Nix reports on the
// mismatch. Each entry names its own build vehicle; a wrong pin fails FAST at the
// FOD, before the heavy guestd compile or agent bundle.

// entrypoint.nix's only image consumer is agent-image/devenv.nix, so its pin is realised
// through a vehicle on the agent-image lock's pkgs (the bun the OCI build uses).

// Mechanism, per gated entry: 1. rewrite the hash to a FAKE value; 2. nix build the
// vehicle (--keep-going so a co-stale sibling FOD does not mask it), which fails with
// the real got: SRI; 3. parse got: for THIS entry's drv (matched by name fragment);
// 4. write it back. Fail LOUD if no got:.

// Self-gating: act only when a trigger manifest differs from base. A gomod bump
// refreshes only the Go vendorHash; a bun.lock, bun-pin OR devenv-nixpkgs channel
// bump refreshes the bun outputHash. Idempotent: re-running rewrites the same SRI.

// Wired from config.json5 at FIVE sites, all the same command (allowlisted once,
// config.test.ts pins them together): top-level postUpgradeTasks, the catalog
// packageRule, the devenv-nixpkgs channel rule, the devenv fork (root) rule, and
// the go ↔ go-overlay lockstep rule.

// Requires nix + bun + git on PATH and network; nix build fetches the toolchains
// itself. Provided by renovate.yml's bootstrap.
// Design: docs/designs/repo/compass-renovate-migration.md

// Exit 0 = hash(es) refreshed (or a no-op branch); 1 = a step failed (no got:,
// a missing marker) — fail loud, never ship a half-refreshed pin set.

import { $ } from "bun";

// A syntactically-valid but deliberately-wrong SRI. Building any FOD against it
// forces the `hash mismatch … got: <real>` error we parse. All-A base64 is a
// canonical fake (matches the lib.fakeHash convention the nix files document).
const FAKE_SRI = "sha256-AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=";

// The pinned FODs. Exported so the test derives its fixtures from the shipped
// table rather than restating it — a rebase that edits the table keeps the test
// honest with no second edit.
export type FodEntry = {
	// Stable identity of this table row, used by every log line and error
	// message. Unique across the table.
	id: string;
	// Nix file carrying the pinned hash (repo-root-relative).
	file: string;
	// Unique substring on the hash line — the attr name + `= "sha256-` prefix, so
	// it matches exactly one line and cannot bind a comment or a sibling attr.
	marker: string;
	// A fragment of this FOD's derivation name, used to attribute the correct
	// `got:` when --keep-going reports multiple mismatches. Distinct per entry
	// WITHIN one vehicle; two entries realising different vehicles may share it.
	drvFragment: string;
	// The build vehicle that realises this entry's FOD: the Nix file and attr,
	// repo-root-relative. Per ENTRY, not per module: each pin has its own vehicle.
	buildFile: string;
	buildTarget: string;
	// The devenv lock whose root nixpkgs input supplies buildFile's pkgs
	// (repo-root-relative). Load-bearing: a scope-specific refresher selects by
	// this field. Do not respell it (leading ./, absolute).
	vehicleChannelLock: string;
	// Manifests whose change invalidates this FOD (repo-root-relative). The
	// per-entry self-gate fires when any of these differs from the base branch.
	triggers: string[];
	// Extra files carrying the IDENTICAL hash (same marker), equal to file's by
	// construction. NOT separately realised (the vehicle content-addresses only
	// file's FOD); each is rewritten to the SRI file's realise reports. Absent for
	// a lone pin.
	mirrorFiles?: string[];
};

export const FOD_ENTRIES: FodEntry[] = [
	{
		id: "guestd-go-vendor",
		file: "guest-image/default.nix",
		marker: 'vendorHash = "sha256-',
		drvFragment: "go-modules",
		buildFile: "guest-image/default.nix",
		buildTarget: "compass-guest-rootfs",
		vehicleChannelLock: "devenv.lock",
		triggers: ["go/go.mod", "go/go.sum"],
		mirrorFiles: ["flake.nix"],
	},
	{
		id: "agent-node-modules-agent-image-pkgs",
		file: "agent-image/entrypoint.nix",
		marker: 'outputHash = "sha256-',
		drvFragment: "node-modules",
		// Imports entrypoint.nix with pkgs from agent-image/devenv.lock, the bun the
		// OCI build uses. Root devenv.lock stays a trigger: config.json5 gates on it.
		buildFile: "tools/renovate/agent-image-fod-vehicle.nix",
		buildTarget: "compass-agent",
		vehicleChannelLock: "agent-image/devenv.lock",
		triggers: [
			"bun.lock",
			"devenv.lock",
			"agent-image/devenv.lock",
			"tools/toolchain/versions/bun.nix",
		],
	},
	{
		id: "ui-node-modules",
		file: "apps/ui/dist.nix",
		marker: 'outputHash = "sha256-',
		drvFragment: "compass-ui-node-modules",
		buildFile: "tools/renovate/ui-fod-vehicle.nix",
		buildTarget: "compass-ui",
		vehicleChannelLock: "flake.lock",
		triggers: [
			"bun.lock",
			"package.json",
			"packages/*/package.json",
			"apps/*/package.json",
			"tools/*/package.json",
			"flake.lock",
			"tools/toolchain/versions/bun.nix",
		],
	},
];

// Table invariants, asserted at load so a bad edit fails HERE, not at run.
// One function per rule so a failure's stack names the rule; the composer is
// exported so the test can feed it a table violating exactly one rule.

// Ids identify a row in every diagnostic.
function assertUniqueIds(entries: FodEntry[]): void {
	const seenIds = new Set<string>();
	for (const entry of entries) {
		if (seenIds.has(entry.id)) {
			throw new Error(
				`renovate-fod: duplicate FodEntry id '${entry.id}' — ids identify a row ` +
					"in every diagnostic, so they must be unique",
			);
		}
		seenIds.add(entry.id);
	}
}

// One writer per pin. Two entries over the same file+marker would be
// last-write-wins: the second realise silently overwrites the first.
function assertOneWriterPerPin(entries: FodEntry[]): void {
	const writers = new Map<string, FodEntry>();
	for (const entry of entries) {
		const key = `${entry.file}\u0000${entry.marker}`;
		const prior = writers.get(key);
		if (prior) {
			throw new Error(
				`renovate-fod: '${entry.id}' and '${prior.id}' both WRITE ${entry.marker} in ` +
					`${entry.file} — a second writer silently clobbers the first`,
			);
		}
		writers.set(key, entry);
	}
}

// got: attribution. parseGotForFragment matches a block by drvName.includes,
// so within ONE vehicle two fragments where one contains the other (e.g.
// "modules" vs "node-modules") would misbind. Scoped to the vehicle since
// entries realising different vehicles never read each other's output.
function assertDisjointFragmentsPerVehicle(entries: FodEntry[]): void {
	for (const a of entries) {
		for (const b of entries) {
			if (a === b) continue;
			if (a.buildFile !== b.buildFile || a.buildTarget !== b.buildTarget) {
				continue;
			}
			if (b.drvFragment.includes(a.drvFragment)) {
				throw new Error(
					`renovate-fod: ambiguous drvFragment '${a.drvFragment}' (${a.id}) is a substring ` +
						`of '${b.drvFragment}' (${b.id}) and both realise ${a.buildFile}#${a.buildTarget} — ` +
						"fragments must be mutually disjoint within a vehicle for got: attribution",
				);
			}
		}
	}
}

export function assertFodTableInvariants(entries: FodEntry[]): void {
	assertUniqueIds(entries);
	assertOneWriterPerPin(entries);
	assertDisjointFragmentsPerVehicle(entries);
}

assertFodTableInvariants(FOD_ENTRIES);

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

// Parse the got: SRI for the derivation whose name contains fragment. Nix prints
// "hash mismatch in fixed-output derivation '…-<frag>.drv': specified: <fake>
// got: <real>". Scoping to the fragment keeps a co-stale sibling FOD's mismatch
// in the same vehicle from being misattributed. undefined if no mismatch.
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

// Recompute one entry's SRI: fake the pin, build THIS entry's vehicle, read the
// reported `got:` for this FOD, then restore the file to the ORIGINAL text (the
// caller decides whether to write the value or merely compare it). Restores on
// every path so a failure never leaves a fake pin in the tree.
async function recompute(entry: FodEntry, origText: string): Promise<string> {
	await Bun.write(
		entry.file,
		rewriteInlineHash(origText, entry.marker, FAKE_SRI, entry.file),
	);
	try {
		const res =
			await $`nix build -f ${entry.buildFile} ${entry.buildTarget} --no-link --keep-going`
				.nothrow()
				.quiet();
		const out = `${res.stdout.toString()}\n${res.stderr.toString()}`;
		const got = parseGotForFragment(out, entry.drvFragment);
		if (!got) {
			throw new Error(
				`renovate-fod: no 'got:' SRI for '${entry.drvFragment}' after building ` +
					`${entry.buildTarget} (${entry.buildFile}) with a faked ${entry.marker} — ` +
					`build output:\n${out.split("\n").slice(-30).join("\n")}`,
			);
		}
		return got;
	} finally {
		await Bun.write(entry.file, origText);
	}
}

// Refresh ONE entry's pin (and its mirrors) in place: recompute the
// SRI by realising the vehicle against a faked pin, write it back, propagate to
// mirrors. Factored out of main()'s loop so refresh-agent-image-nixpkgs.ts can
// drive a single FOD, keeping ONE realise-and-parse implementation.
export async function refreshEntry(entry: FodEntry): Promise<void> {
	const origText = await Bun.file(entry.file).text();
	console.log(
		`renovate-fod: ${entry.file} (${entry.drvFragment}) trigger changed; recomputing ${entry.marker.replace(/ = .*/, "")} via ${entry.buildTarget} (${entry.buildFile}) ...`,
	);
	const got = await recompute(entry, origText);
	await Bun.write(
		entry.file,
		rewriteInlineHash(origText, entry.marker, got, entry.file),
	);
	console.log(`renovate-fod: ${entry.file} -> ${got}`);
	for (const mirror of entry.mirrorFiles ?? []) {
		const mirrorText = await Bun.file(mirror).text();
		await Bun.write(
			mirror,
			rewriteInlineHash(mirrorText, entry.marker, got, mirror),
		);
		console.log(`renovate-fod: ${mirror} (mirror) -> ${got}`);
	}
}

export async function refreshFodEntries(entries: FodEntry[]): Promise<void> {
	for (const entry of entries) {
		await refreshEntry(entry);
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
	// vs base. A gomod bump opens the Go entry alone; a bun bump the bun entries
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

	await refreshFodEntries(gated);

	console.log("renovate-fod: FOD hashes refreshed.");
}

// Run only when executed directly (not imported by the test file).
if (import.meta.main) {
	await main();
}
