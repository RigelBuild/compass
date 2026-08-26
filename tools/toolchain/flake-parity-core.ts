// Pure parsing and comparison for the flake nixpkgs-pin parity gate. No I/O, no
// process exec — total functions over strings, so the interesting half is
// unit-testable (flake-parity-core.test.ts) and the executable shell
// (flake-parity.ts) stays thin. Mirrors the parity.ts / parity-core.ts split.
//
// THE INVARIANT THIS GATE ENFORCES (design record compass-distribution §T6). The
// repo carries TWO independent nixpkgs locks: devenv.lock (the dev shell + the
// app-bundle build) and flake.lock (the repo-root flake). The "one closure"
// claim — that flake-built binaries are byte-for-byte the bundle-built ones —
// holds ONLY if the two locks resolve the same nixpkgs revision, and nothing
// enforces that by construction. A devenv pin bump silently skews flake.lock.
// This gate reads the nixpkgs revision each lock records and fails on a
// mismatch, so the drift is a red CI check rather than a silent divergence.
//
// A rev that cannot be extracted is NOT skipped — it is a failure, the same
// false-green refusal parity-core.ts makes: a gate that cannot read one side
// proves nothing.

/** The parity verdict plus a legible one-block report of the two revs. */
export interface FlakeParityReport {
	readonly report: string;
	readonly ok: boolean;
}

/**
 * Extract nixpkgs's locked revision from a flake-lock-shaped document
 * (devenv.lock and flake.lock share the shape): the `nixpkgs` node's
 * `locked.rev`. Narrows each step with `in`/`typeof` rather than an unchecked
 * cast, so a lock whose shape moved yields null — which the caller treats as a
 * failure — instead of a fabricated read. Returns null when the node or a
 * non-empty string rev is absent.
 */
export function nixpkgsLockedRev(source: string): string | null {
	let root: unknown;
	try {
		root = JSON.parse(source);
	} catch {
		// A corrupt / merge-conflicted lock is unreadable — route it through the
		// same null→UNVERIFIABLE fail-closed path as the other malformed cases
		// rather than let the SyntaxError escape as a raw stacktrace.
		return null;
	}
	if (!isObject(root) || !("nodes" in root) || !isObject(root.nodes)) {
		return null;
	}
	const { nodes } = root;
	if (!("nixpkgs" in nodes) || !isObject(nodes.nixpkgs)) {
		return null;
	}
	const nixpkgs = nodes.nixpkgs;
	if (!("locked" in nixpkgs) || !isObject(nixpkgs.locked)) {
		return null;
	}
	const { locked } = nixpkgs;
	if (
		!("rev" in locked) ||
		typeof locked.rev !== "string" ||
		locked.rev.length === 0
	) {
		return null;
	}
	return locked.rev;
}

/** Narrow an unknown to a plain record so `in`-guarded reads are checked. */
function isObject(value: unknown): value is Record<string, unknown> {
	return typeof value === "object" && value !== null;
}

/**
 * Decide the gate and render its report.
 *
 * ok is true ONLY when both revisions were extracted AND they are equal. A
 * missing rev on either side is a failure (unverifiable is never a pass), and a
 * mismatch names both revs so the log states which lock drifted.
 */
export function compareRevs(
	flakeRev: string | null,
	devenvRev: string | null,
): FlakeParityReport {
	if (flakeRev === null || devenvRev === null) {
		const missing = [
			flakeRev === null ? "flake.lock" : null,
			devenvRev === null ? "devenv.lock" : null,
		]
			.filter((s): s is string => s !== null)
			.join(" and ");
		return {
			ok: false,
			report: `UNVERIFIABLE  could not read nixpkgs locked.rev from ${missing}`,
		};
	}
	if (flakeRev === devenvRev) {
		return {
			ok: true,
			report: `ok  flake.lock and devenv.lock both pin nixpkgs ${flakeRev}`,
		};
	}
	return {
		ok: false,
		report: [
			"MISMATCH  flake.lock and devenv.lock pin different nixpkgs revisions:",
			`  flake.lock   ${flakeRev}`,
			`  devenv.lock  ${devenvRev}`,
			"Re-lock the flake to the devenv.lock rev:",
			"  nix flake update nixpkgs   (after aligning flake.nix's inputs.nixpkgs.url)",
		].join("\n"),
	};
}
