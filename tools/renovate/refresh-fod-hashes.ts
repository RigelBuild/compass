#!/usr/bin/env bun
// Renovate postUpgradeTask: refresh the pinned Nix fixed-output-derivation (FOD)
// hashes a dependency bump invalidates, so a dep-bump PR lands green instead of
// red on a "hash mismatch in fixed-output derivation" build break (RIG-2432,
// PR #579's failure class).

// Compass pins two FOD hash VALUES. The Go vendorHash lives in two files that
// share it by design: guest-image/default.nix and flake.nix — the SAME
// proxyVendor hash over go/, both moved by a go.mod|go.sum bump. The vehicle
// realises only guestd's FOD; flake.nix is a MIRROR (FodEntry.mirrorFiles).

// agent-image/entrypoint.nix's outputHash content-addresses compass-agent's
// installed node_modules (a bun install FOD). Invalidated by a bun.lock bump AND
// by a devenv-nixpkgs channel bump (the channel moves pkgs.bun, the FOD builder),
// so the entry gates on both and reconciles the pin whichever moved.

// Neither is a URL hash prefetch-file can recompute — a vendorHash/outputHash is
// only knowable by REALISING the derivation and reading the SRI Nix reports on the
// mismatch. Each entry names its own build vehicle; a wrong pin fails FAST at the
// FOD, before the heavy guestd compile or agent bundle.

// entrypoint.nix carries ONE outputHash but is imported by two consumers with two
// nixpkgs pins (guest-image/default.nix with root's pkgs, agent-image/devenv.nix
// with agent-image's), so two buns realise it. One hash satisfies both only while
// they produce a byte-identical tree, so the table carries the entrypoint pin twice:

//   * an AUTHORITATIVE entry (root's pkgs) that WRITES the canonical SRI; and
//   * a VERIFY entry (verifyOf, agent-image's pkgs) that recomputes and COMPARES.
//     Equal → no-op; different → throw, naming both SRIs, vehicles and channel revs.

// A verify entry must never write (two writers would be last-write-wins, hiding
// the divergence this pair catches), so the write-then-verify ORDER is load-bearing.
// refreshFodEntries() enforces it structurally (it partitions the run set, never
// trusts table order), backed by the table invariants below.

// Mechanism, per gated entry: 1. rewrite the hash to a FAKE value; 2. nix build the
// vehicle (--keep-going so a co-stale sibling FOD does not mask it), which fails with
// the real got: SRI; 3. parse got: for THIS entry's drv (matched by name fragment);
// 4. write it back (authoritative) or compare and throw (verify). Fail LOUD if no got:.

// Self-gating: act only when a trigger manifest differs from base. A gomod bump
// refreshes only the Go vendorHash; a bun.lock OR devenv-nixpkgs channel bump
// refreshes the bun outputHash. Idempotent: re-running rewrites the same SRI.

// Wired from config.json5 at FIVE sites, all the same command (allowlisted once,
// config.test.ts pins them together): top-level postUpgradeTasks, the catalog
// packageRule, the devenv-nixpkgs channel rule, the devenv fork (root) rule, and
// the go ↔ go-overlay lockstep rule.

// Requires nix + bun + git on PATH and network; nix build fetches the toolchains
// itself. Provided by renovate.yml's bootstrap.
// Design: docs/designs/repo/compass-renovate-migration.md

// Exit 0 = hash(es) refreshed and every verify entry agreed (or a no-op branch);
// 1 = a step failed (no got:, a missing marker, or a verify disagreement) — fail
// loud, never ship a half-refreshed or one-builder-only pin set.

import { $ } from "bun";

// A syntactically-valid but deliberately-wrong SRI. Building any FOD against it
// forces the `hash mismatch … got: <real>` error we parse. All-A base64 is a
// canonical fake (matches the lib.fakeHash convention the nix files document).
const FAKE_SRI = "sha256-AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=";

// The pinned FODs. Exported so the test derives its fixtures from the shipped
// table rather than restating it — a rebase that edits the table keeps the test
// honest with no second edit.
export type FodEntry = {
	// Stable identity of this table row, used by `verifyOf` and by every log line
	// and error message. Unique across the table.
	id: string;
	// Nix file carrying the pinned hash (repo-root-relative).
	file: string;
	// Unique substring on the hash line — the attr name + `= "sha256-` prefix, so
	// it matches exactly one line and cannot bind a comment or a sibling attr.
	marker: string;
	// A fragment of this FOD's derivation name, used to attribute the correct
	// `got:` when --keep-going reports multiple mismatches. Distinct per entry
	// WITHIN one vehicle; two entries realising different vehicles may share it
	// (that is exactly the two-builders-one-hash case above).
	drvFragment: string;
	// The build vehicle that realises this entry's FOD: the Nix file and attr,
	// repo-root-relative. Per ENTRY, not per module: the same hash is realised
	// through two vehicles at two nixpkgs revs, so a shared global would make one
	// unreachable.
	buildFile: string;
	buildTarget: string;
	// The devenv lock whose nodes.nixpkgs.locked.rev supplies buildFile's pkgs
	// (repo-root-relative). Load-bearing: a scope-specific refresher selects by
	// this field, the invariants check it names the lock buildFile reads, and the
	// divergence error quotes both revs. Do not respell it (leading ./, absolute).
	vehicleChannelLock: string;
	// Manifests whose change invalidates this FOD (repo-root-relative). The
	// per-entry self-gate fires when any of these differs from the base branch.
	triggers: string[];
	// Extra files carrying the IDENTICAL hash (same marker), equal to file's by
	// construction. NOT separately realised (the vehicle content-addresses only
	// file's FOD); each is rewritten to the SRI file's realise reports. Absent for
	// a lone pin. Never set on a verify entry.
	mirrorFiles?: string[];
	// Set => this entry VERIFIES the pin another entry writes, and never writes
	// itself. The value is that authoritative entry's `id`. See the header: it
	// exists to check a SECOND builder's view of one shared hash.
	verifyOf?: string;
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
		id: "agent-node-modules-root-pkgs",
		file: "agent-image/entrypoint.nix",
		marker: 'outputHash = "sha256-',
		drvFragment: "node-modules",
		// The AUTHORITATIVE realise of the shared outputHash, through
		// guest-image/default.nix (imports entrypoint.nix with ROOT's pkgs). This
		// entry WRITES the canonical value; the sibling below re-derives it through
		// the agent-image scope and only compares.
		buildFile: "guest-image/default.nix",
		buildTarget: "compass-guest-rootfs",
		vehicleChannelLock: "devenv.lock",
		// bun.lock moves the installed tree. The two channel locks are declared
		// beside it because each moves a BUILDER of this FOD: devenv.lock supplies
		// root's pkgs here, agent-image/devenv.lock supplies the other importer's.
		// The verify entry carries the IDENTICAL trigger list, gating them together.
		triggers: ["bun.lock", "devenv.lock", "agent-image/devenv.lock"],
	},
	{
		id: "agent-node-modules-agent-image-pkgs",
		file: "agent-image/entrypoint.nix",
		marker: 'outputHash = "sha256-',
		// The SAME drv name as the entry above — literally the same derivation
		// against a different nixpkgs. Safe because attribution is scoped to a
		// vehicle's own output and the two entries realise different vehicles (the
		// invariants enforce that).
		drvFragment: "node-modules",
		// The agent-image scope's own view of the shared hash. Imports entrypoint.nix
		// with pkgs from agent-image/devenv.lock, the bun the OCI build uses. Without
		// it a channel-rev divergence that changes bun's tree would leave this pin
		// right for guest-image, silently wrong for the agent image, until build.
		buildFile: "tools/renovate/agent-image-fod-vehicle.nix",
		buildTarget: "compass-agent",
		vehicleChannelLock: "agent-image/devenv.lock",
		triggers: ["bun.lock", "devenv.lock", "agent-image/devenv.lock"],
		verifyOf: "agent-node-modules-root-pkgs",
	},
];

// Table invariants, asserted at load so a bad edit fails HERE, not at run.
// Each closes a way the write/verify pairing could silently misbehave. One
// function per rule so a failure's stack names the rule; the composer is
// exported so the test can feed it a table violating exactly one rule.

// Ids identify a row to `verifyOf` and to every diagnostic.
function assertUniqueIds(entries: FodEntry[]): void {
	const seenIds = new Set<string>();
	for (const entry of entries) {
		if (seenIds.has(entry.id)) {
			throw new Error(
				`renovate-fod: duplicate FodEntry id '${entry.id}' — ids identify a row to ` +
					"`verifyOf` and to every diagnostic, so they must be unique",
			);
		}
		seenIds.add(entry.id);
	}
}

// One writer per pin. Two authoritative entries over the same file+marker would
// be last-write-wins: the second realise's value would overwrite the first with
// no diagnostic, which is precisely the divergence-hiding failure the verify
// entry exists to prevent.
function assertOneWriterPerPin(entries: FodEntry[]): void {
	const writers = new Map<string, FodEntry>();
	for (const entry of entries) {
		if (entry.verifyOf) continue;
		const key = `${entry.file}\u0000${entry.marker}`;
		const prior = writers.get(key);
		if (prior) {
			throw new Error(
				`renovate-fod: '${entry.id}' and '${prior.id}' both WRITE ${entry.marker} in ` +
					`${entry.file} — a second writer silently clobbers the first; the later one ` +
					"must carry `verifyOf` instead",
			);
		}
		writers.set(key, entry);
	}
}

// A verify entry must actually verify the pin it claims to, through a DIFFERENT
// builder, and must be gated identically so the two can never fire apart (a
// verify run whose authoritative sibling did not run would compare against a
// pre-write baseline).
function assertVerifyPairing(entries: FodEntry[]): void {
	for (const entry of entries) {
		if (!entry.verifyOf) continue;
		const target = entries.find((e) => e.id === entry.verifyOf);
		if (!target || target.verifyOf) {
			throw new Error(
				`renovate-fod: verify entry '${entry.id}' names verifyOf='${entry.verifyOf}', ` +
					"which is not an authoritative (writing) entry in this table",
			);
		}
		assertVerifiesTarget(entry, target);
	}
}

// The four properties one verify/authoritative PAIR must hold, split out so each
// rule reads on its own and the enclosing scan stays a scan.
function assertVerifiesTarget(entry: FodEntry, target: FodEntry): void {
	if (entry.file !== target.file || entry.marker !== target.marker) {
		throw new Error(
			`renovate-fod: verify entry '${entry.id}' must check the SAME pin as '${target.id}' ` +
				`— got ${entry.file}:${entry.marker} vs ${target.file}:${target.marker}`,
		);
	}
	if (
		entry.buildFile === target.buildFile &&
		entry.buildTarget === target.buildTarget
	) {
		throw new Error(
			`renovate-fod: verify entry '${entry.id}' realises the SAME vehicle as '${target.id}' ` +
				`(${entry.buildFile}#${entry.buildTarget}) — it would re-derive the identical value ` +
				"and check nothing; a verify entry exists to exercise a SECOND builder",
		);
	}
	const sameTriggers =
		entry.triggers.length === target.triggers.length &&
		new Set(entry.triggers).size === new Set(target.triggers).size &&
		entry.triggers.every((t) => target.triggers.includes(t));
	if (!sameTriggers) {
		throw new Error(
			`renovate-fod: verify entry '${entry.id}' and '${target.id}' must declare the SAME ` +
				"triggers, or the gate could open one without the other and the verify would " +
				`compare against an unrefreshed pin — got [${entry.triggers}] vs [${target.triggers}]`,
		);
	}
	if (entry.mirrorFiles?.length) {
		throw new Error(
			`renovate-fod: verify entry '${entry.id}' declares mirrorFiles — a verify entry ` +
				"writes nothing, so mirrors would never be propagated from it",
		);
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
	assertVerifyPairing(entries);
	// LAST, so the pairing rules above give their specific diagnosis first: a
	// verify entry wrongly sharing its target's vehicle also trips the fragment
	// rule, and "same vehicle" is the message that names the actual mistake.
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

// ── Pure read: the `sha256-…` SRI currently on the line carrying `marker`. ──
// The inverse of rewriteInlineHash, and the baseline a verify entry compares its
// recomputed value against. Throws rather than returning undefined: a verify that
// cannot find the pin must fail loud, never "no mismatch observed".
export function hashOnMarker(
	fileText: string,
	marker: string,
	file: string,
): string {
	const line = fileText.split("\n").find((l) => l.includes(marker));
	if (line === undefined) {
		throw new Error(`renovate-fod: marker '${marker}' not found in ${file}`);
	}
	const sri = line.match(/sha256-[^"]*/)?.[0];
	if (!sri) {
		throw new Error(
			`renovate-fod: no sha256- SRI on the '${marker}' line in ${file}`,
		);
	}
	return sri;
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

const isObj = (v: unknown): v is Record<string, unknown> =>
	typeof v === "object" && v !== null;

// The nixpkgs channel rev a vehicle's `pkgs` resolves from, for DIAGNOSTICS only.
// Never throws: it is called while building an error message, and losing the
// real failure to a secondary parse error would be strictly worse than printing
// a placeholder.
async function vehicleChannelRev(lockFile: string): Promise<string> {
	try {
		let cur: unknown = JSON.parse(await Bun.file(lockFile).text());
		for (const key of ["nodes", "nixpkgs", "locked", "rev"]) {
			if (!isObj(cur)) return `<unreadable: ${lockFile} has no ${key}>`;
			cur = cur[key];
		}
		return typeof cur === "string" ? cur : `<unreadable: ${lockFile}>`;
	} catch (error) {
		return `<unreadable: ${lockFile} (${String(error)})>`;
	}
}

// Refresh ONE authoritative entry's pin (and its mirrors) in place: recompute the
// SRI by realising the vehicle against a faked pin, write it back, propagate to
// mirrors. Factored out of main()'s loop so refresh-agent-image-nixpkgs.ts can
// drive a single FOD, keeping ONE realise-and-parse implementation.
export async function refreshEntry(entry: FodEntry): Promise<void> {
	if (entry.verifyOf) {
		throw new Error(
			`renovate-fod: '${entry.id}' is a verify entry and must never write ${entry.file} — ` +
				`route it through verifyEntry (its write would clobber '${entry.verifyOf}'s value)`,
		);
	}
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

// Check ONE verify entry: recompute the shared pin through the SECOND builder and
// compare against disk — which, by refreshFodEntries' ordering, is the value
// authoritative already wrote. Writes nothing (recompute restores its fake).

// Equal is the quiet, expected outcome. A difference means the two vehicles' buns
// produce different install trees, so no single outputHash satisfies both:
// throwing reds renovate/artifacts on the branch that moved the pin.
export async function verifyEntry(
	entry: FodEntry,
	authoritative: FodEntry,
): Promise<void> {
	if (entry.verifyOf !== authoritative.id) {
		throw new Error(
			`renovate-fod: '${entry.id}' verifies '${entry.verifyOf}', not '${authoritative.id}'`,
		);
	}
	// Read AFTER the authoritative write, so the baseline is the refreshed pin
	// (pre-write text would report every refresh as a divergence). ONE read is
	// both the value checked and the text recompute restores after faking.
	const currentText = await Bun.file(entry.file).text();
	const committed = hashOnMarker(currentText, entry.marker, entry.file);
	console.log(
		`renovate-fod: verifying ${entry.file} ${entry.marker.replace(/ = .*/, "")} against ${entry.buildTarget} (${entry.buildFile}) ...`,
	);
	const got = await recompute(entry, currentText);
	if (got === committed) {
		console.log(
			`renovate-fod: ${entry.file} verified — ${entry.buildFile} and ${authoritative.buildFile} ` +
				`agree on ${committed}`,
		);
		return;
	}
	const [verifyRev, authRev] = await Promise.all([
		vehicleChannelRev(entry.vehicleChannelLock),
		vehicleChannelRev(authoritative.vehicleChannelLock),
	]);
	throw new Error(
		`renovate-fod: ${entry.file}'s ${entry.marker.replace(/ = .*/, "")} cannot satisfy both of ` +
			"its builders — one outputHash literal, two nixpkgs revs whose bun produces a " +
			"different installed tree.\n" +
			`  ${authoritative.id} (authoritative, wrote it): ${committed}\n` +
			`    vehicle: ${authoritative.buildTarget} (${authoritative.buildFile})\n` +
			`    channel: ${authoritative.vehicleChannelLock} @ ${authRev}\n` +
			`  ${entry.id} (verify): ${got}\n` +
			`    vehicle: ${entry.buildTarget} (${entry.buildFile})\n` +
			`    channel: ${entry.vehicleChannelLock} @ ${verifyRev}\n` +
			"The committed value is the authoritative one, so the agent-image OCI build would " +
			"fail `hash mismatch in fixed-output derivation`. Reconcile the two channel revs " +
			"onto one bun, or split the pin so each consumer carries its own.",
	);
}

// Drive gated entries in the ONE correct order: every authoritative write first,
// then every verify. Structural — the run set is partitioned here, never read in
// table order — because a verify running first would compare against the STALE
// pin and throw on an ordinary refresh, and a skipped verify ships unverified.
export async function refreshFodEntries(entries: FodEntry[]): Promise<void> {
	const authoritative = entries.filter((e) => !e.verifyOf);
	const verifiers = entries.filter((e) => e.verifyOf);

	for (const entry of authoritative) {
		await refreshEntry(entry);
	}
	for (const entry of verifiers) {
		// Resolve against the RUN SET, not the whole table: if the caller gated the
		// verify entry without its writer, nothing refreshed the pin above and the
		// comparison would be against a pre-write baseline. The table invariants
		// make the two gate together, so reaching this is a caller bug — fail loud.
		const target = authoritative.find((e) => e.id === entry.verifyOf);
		if (!target) {
			throw new Error(
				`renovate-fod: verify entry '${entry.id}' was gated without its authoritative ` +
					`entry '${entry.verifyOf}', so there is no refreshed value to verify against`,
			);
		}
		await verifyEntry(entry, target);
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
