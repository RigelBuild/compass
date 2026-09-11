#!/usr/bin/env bun
// Renovate postUpgradeTask: refresh the pinned Nix fixed-output-derivation (FOD)
// hashes a dependency bump invalidates, so a dep-bump PR lands green instead of
// red on a `hash mismatch in fixed-output derivation` build break (the RIG-2432
// easy-dep-bump goal, PR #579's failure class).
//
// Compass pins two FOD hash VALUES, each content-addressing a fetched dependency
// set that MOVES when a manifest bumps.
// The Go vendorHash is pinned in TWO files that share it by design (below):
//
//   guest-image/default.nix   vendorHash   compass-guestd's Go module set
//   flake.nix                 vendorHash   compass-app + cmd-binaries' module set
//                                          — the SAME proxyVendor hash over go/
//                                          (flake.nix:46-52 documents the equality),
//                                          both invalidated by a go/go.mod|go.sum
//                                          bump. The build vehicle realises ONLY
//                                          guestd's FOD; flake.nix is refreshed as a
//                                          MIRROR — the identical value, no second
//                                          realise — see FodEntry.mirrorFiles.
//   agent-image/entrypoint.nix outputHash  compass-agent's installed node_modules
//                                          tree (recursive FOD of `bun install`) —
//                                          invalidated by a bun.lock bump, and
//                                          refreshed on a devenv-nixpkgs channel
//                                          bump too (devenv.lock): the channel
//                                          moves pkgs.bun, the FOD's builder,
//                                          which MAY move the recursive tree — so
//                                          the entry gates on BOTH and the refresh
//                                          reconciles the pin whichever moved.
//
// Neither is a URL hash a `nix store prefetch-file` can recompute (that is
// refresh-toolchain-hashes.ts's job for the vendored-binary pins). A vendorHash /
// outputHash is only knowable by REALISING the derivation: build it, read the SRI
// Nix reports on the mismatch. Each entry names its OWN build vehicle, and with a
// deliberately-wrong pin the build fails FAST at the FOD, never proceeding to the
// heavy guestd compile, the erofs pack, or the agent bundle.
//
// ── ONE outputHash, TWO builders: why some entries WRITE and some VERIFY ──
// `agent-image/entrypoint.nix` carries a SINGLE `outputHash` literal and is
// imported by TWO consumers with two different nixpkgs pins:
//
//   guest-image/default.nix:66   with ROOT's `pkgs`   (root devenv.lock)
//   agent-image/devenv.nix:34    with AGENT-IMAGE's `pkgs` (agent-image/devenv.lock)
//
// The FOD's builder takes `nativeBuildInputs = [ pkgs.bun ]`, so the two consumers
// realise it with two bun derivations. One hash satisfies both only while those
// two buns produce a byte-identical install tree. The table therefore carries the
// entrypoint pin TWICE:
//
//   * an AUTHORITATIVE entry, realised through guest-image/default.nix (root's
//     pkgs), which WRITES the canonical SRI; and
//   * a VERIFY entry (`verifyOf`), realised through the agent-image vehicle
//     (agent-image's pkgs), which recomputes and COMPARES against what the
//     authoritative entry just wrote. Equal → log and no-op. Different → throw,
//     naming both SRIs, both vehicles and both channel revs.
//
// A verify entry must never write: two entries writing one marker would be
// last-write-wins, and the second value would silently overwrite the first —
// hiding the very divergence this pair exists to catch. The write-then-verify
// ORDER is therefore load-bearing, and refreshFodEntries() enforces it
// structurally (it partitions the run set; it never trusts table order), backed
// by the table invariants below (a verify entry must share its authoritative
// entry's file+marker+triggers and use a DIFFERENT vehicle).
//
// Mechanism, per gated entry:
//   1. Rewrite the entry's hash to a fixed FAKE value.
//   2. `nix build` the entry's vehicle (--keep-going so a co-stale sibling FOD
//      does not mask this one) — it fails with the entry's real `got:` SRI.
//   3. Parse the `got:` for THIS entry's derivation (matched by a drv-name
//      fragment, so a sibling FOD's mismatch in the same vehicle can never be
//      misattributed).
//   4. Write the real SRI back (authoritative), or compare it against the
//      committed value and throw on divergence (verify). Fail LOUD (exit 1) if no
//      `got:` is found — a silent no-op would ship the stale pin this task exists
//      to fix.
//
// Self-gating: for each entry, act only when one of its trigger manifests differs
// from the base branch (mirrors refresh-toolchain-hashes.ts's versions/*.nix
// gate). So it is a cheap no-op on every branch that touches no trigger manifest,
// a gomod bump refreshes only the Go vendorHash, and a bun.lock bump OR a
// devenv-nixpkgs channel bump (devenv.lock) refreshes the bun outputHash.
// Idempotent: re-running rewrites the same SRI — a no-op write when the realised
// tree is unchanged (so gating on devenv.lock costs at most one extra realise).
//
// Wired from config.json5 at FIVE sites, all the same
// `bun tools/renovate/refresh-fod-hashes.ts` command (allowlisted once in
// bot-config.json5; config.test.ts pins them together): top-level
// postUpgradeTasks (branch mode — gomod branches and pure-bun-first
// TypeScript-rollup branches), the catalog packageRule (update mode —
// catalog-first rollup branches, where the collapsed branch config evicts the
// top-level branch task), the devenv-nixpkgs channel rule (branch mode — a
// channel bump moves pkgs.bun; see that rule's note), the devenv fork (root)
// rule (branch mode — relocks devenv.lock; a declared trigger), and the go ↔
// go-overlay lockstep rule (branch mode — relocks devenv.lock via
// `devenv update go-overlay`; a declared trigger).
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
//   0 - hash(es) refreshed and every verify entry agreed, or a no-op branch (no
//       trigger manifest changed).
//   1 - a step failed (build produced no `got:`, a marker was missing, or a
//       verify entry's vehicle disagreed with the committed pin) — fail loud,
//       never ship a half-refreshed or one-builder-only pin set.

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
	// The build vehicle that realises this entry's FOD: the Nix file and the attr
	// in it, both repo-root-relative (the runner cwd = repo root; main() chdirs
	// there). Per ENTRY, not per module: the same pinned hash is realised through
	// two vehicles resolving two different nixpkgs revs, and a shared global would
	// make one of those unreachable.
	buildFile: string;
	buildTarget: string;
	// The devenv lock whose `nodes.nixpkgs.locked.rev` supplies `buildFile`'s
	// `pkgs` (repo-root-relative). Load-bearing, not decorative: a scope-specific
	// refresher selects its entries by this field, the table invariants below
	// check it names the lock `buildFile` actually reads, and the divergence error
	// quotes both revs so the reader does not have to go find them. Rewriting it
	// to an equivalent-looking spelling (a leading `./`, an absolute path) breaks
	// those lookups.
	vehicleChannelLock: string;
	// Manifests whose change invalidates this FOD (repo-root-relative). The
	// per-entry self-gate fires when any of these differs from the base branch.
	triggers: string[];
	// Extra files carrying the IDENTICAL pinned hash (same `marker`), equal to
	// `file`'s by construction — e.g. a second buildGoModule with the same
	// proxyVendor set over the same go/. They are NOT separately realised (the
	// build vehicle content-addresses only `file`'s FOD, so a faked mirror pin
	// would never surface in its output); each is rewritten to the SRI `file`'s
	// realise reports. Absent for a lone pin. Never set on a verify entry — a
	// verify entry writes nothing at all.
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
		// guest-image/default.nix — which imports the very same entrypoint.nix with
		// ROOT's `pkgs` (that file documents the divergence as deliberate). This is
		// the entry that WRITES the canonical value; the sibling below re-derives it
		// through the agent-image scope and only compares.
		buildFile: "guest-image/default.nix",
		buildTarget: "compass-guest-rootfs",
		vehicleChannelLock: "devenv.lock",
		// `bun.lock` moves the installed tree's version set. The two channel locks
		// are declared beside it because each moves a BUILDER of this same FOD:
		// `devenv.lock` supplies root's `pkgs` to the vehicle realised here, and
		// `agent-image/devenv.lock` supplies the agent-image scope's `pkgs` to
		// `agent-image/devenv.nix`, the OTHER importer of entrypoint.nix. All paths
		// are repo-root-relative, as the gate's `git diff` pathspec expects. The
		// verify entry below carries the IDENTICAL trigger list, which is what makes
		// the two always gate together.
		triggers: ["bun.lock", "devenv.lock", "agent-image/devenv.lock"],
	},
	{
		id: "agent-node-modules-agent-image-pkgs",
		file: "agent-image/entrypoint.nix",
		marker: 'outputHash = "sha256-',
		// The SAME drv name as the entry above — it is literally the same
		// derivation expression, evaluated against a different nixpkgs. Sharing the
		// fragment is safe because attribution is scoped to a vehicle's own build
		// output, and the two entries realise different vehicles (the table
		// invariants below enforce exactly that).
		drvFragment: "node-modules",
		// The agent-image scope's own view of the shared hash. This vehicle exists
		// solely for this check: it imports entrypoint.nix with the pkgs resolved
		// from agent-image/devenv.lock, which is the bun the OCI image build
		// actually uses. Without it, a channel-rev divergence that changes bun's
		// install tree would leave this pin right for guest-image, silently wrong
		// for the agent image, and unobservable until the image build.
		buildFile: "tools/renovate/agent-image-fod-vehicle.nix",
		buildTarget: "compass-agent",
		vehicleChannelLock: "agent-image/devenv.lock",
		triggers: ["bun.lock", "devenv.lock", "agent-image/devenv.lock"],
		verifyOf: "agent-node-modules-root-pkgs",
	},
];

// ── Table invariants, asserted at load so a bad edit fails HERE, not at run. ──
// Each one closes a way the write/verify pairing could silently do the wrong
// thing. One function per rule, so a failure's stack names the rule broken and
// each stays readable whole; assertFodTableInvariants below composes them in the
// order their diagnostics are most useful. Exported (the composer) so the test
// can prove each guard is real by feeding it a table that violates exactly one
// rule, rather than restating the properties in assertions that would drift.

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

// got: attribution. parseGotForFragment matches a mismatch block by
// `drvName.includes(fragment)`, so within ONE vehicle's output two fragments
// where one contains the other (e.g. "modules" vs "node-modules") would let one
// entry's got: bind the other's block and write the WRONG hash. Scoped to the
// vehicle because that is the only place the ambiguity can arise: entries
// realising different vehicles never read each other's output.
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

// ── Parse the `got:` SRI for the derivation whose name contains `fragment`. ──
// Nix prints, per mismatching FOD:
//     error: hash mismatch in fixed-output derivation '/nix/store/…-<frag>.drv':
//              specified: sha256-<fake>
//                 got:    sha256-<real>
// Scoping to the fragment means a co-stale sibling FOD's mismatch in the same
// vehicle (only possible defensively — the two triggers never change on one
// branch) is never misattributed. Returns undefined if this FOD did not report a
// mismatch.
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

// Refresh ONE authoritative entry's pin (and its mirrors) in place: read the
// current text, recompute the SRI by realising the entry's vehicle against a
// faked pin, write the real value back, then propagate it to every declared
// mirror. This is the per-entry body of main()'s gated loop, factored out so a
// scope-specific refresher can drive a single FOD directly —
// refresh-agent-image-nixpkgs.ts drives it (via refreshFodEntries) after its own
// relock, having already established by its own base diff that the agent-image
// entry's trigger moved. Extracting it keeps ONE realise-and-parse
// implementation: a second copy would drift from the got:-attribution and
// restore-on-failure discipline above.
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
// compare it against what is on disk — which, by refreshFodEntries' ordering, is
// the value `authoritative` has already written. Writes nothing on either path
// (recompute restores the file it faked), so the authoritative value survives
// intact whatever this finds.
//
// Equal is the expected outcome and the only quiet one. A difference means the
// two vehicles' bun derivations produce different install trees, so NO single
// `outputHash` literal can satisfy both consumers: throwing reds
// `renovate/artifacts` on the branch that moved the pin, which is the whole point
// of realising this second vehicle.
export async function verifyEntry(
	entry: FodEntry,
	authoritative: FodEntry,
): Promise<void> {
	if (entry.verifyOf !== authoritative.id) {
		throw new Error(
			`renovate-fod: '${entry.id}' verifies '${entry.verifyOf}', not '${authoritative.id}'`,
		);
	}
	// Read AFTER the authoritative write, so the baseline is the refreshed pin —
	// comparing against the pre-write text would report every ordinary refresh as
	// a divergence. ONE read serves both roles: it is the value being checked AND
	// the text recompute restores after faking the pin, so the file cannot end in
	// a state neither of them intended.
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

// Drive a set of gated entries in the ONE order that is correct: every
// authoritative write first, then every verify. The ordering is structural — the
// run set is partitioned here, never read in table order — because a verify that
// ran first would compare the second builder's value against the STALE pin and
// throw on an ordinary refresh, and a verify that was skipped would ship the
// unverified pin this pairing exists to catch.
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
