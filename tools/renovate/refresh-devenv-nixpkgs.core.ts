// Pure decision/transform core for refresh-devenv-nixpkgs.ts (RIG-2432): reading
// locked revs out of devenv.lock and rewriting flake.nix's channel pin —
// unit-testable without a nix runner, network, or git tree.

/**
 * The locked rev of the node reached from devenv.lock's root by following
 * `inputPath` input NAMES (e.g. ["meissa", "nixpkgs", "nixpkgs-src"]). Node
 * keys are not stable — an unfollowed input's own nixpkgs can take the bare
 * `nixpkgs` key and rename root's to `nixpkgs_2` — so never index by key.
 * Throws loudly on any missing hop or a non-40-hex rev: a shape change must
 * fail the task, never silently read the wrong (or a stale) rev.
 */
export function lockedRevByInputs(
	devenvLockText: string,
	inputPath: readonly string[],
): string {
	let lock: unknown;
	try {
		lock = JSON.parse(devenvLockText);
	} catch (error) {
		throw new Error(`devenv.lock is not valid JSON: ${String(error)}`);
	}
	const isObj = (v: unknown): v is Record<string, unknown> =>
		typeof v === "object" && v !== null;
	const nodes = isObj(lock) && isObj(lock.nodes) ? lock.nodes : {};
	const where = `root.inputs → ${inputPath.join(" → ")}`;
	let node: unknown = nodes.root;
	for (const name of inputPath) {
		const key = isObj(node) && isObj(node.inputs) ? node.inputs[name] : null;
		if (typeof key !== "string") {
			throw new Error(
				`devenv.lock has no '${name}' input along ${where} — devenv lock shape may have changed.`,
			);
		}
		node = nodes[key];
	}
	const rev = isObj(node) && isObj(node.locked) ? node.locked.rev : undefined;
	if (typeof rev !== "string" || !/^[a-f0-9]{40}$/.test(rev)) {
		throw new Error(
			`devenv.lock has no 40-hex locked rev at ${where} — devenv lock shape may have changed.`,
		);
	}
	return rev;
}

/**
 * The devenv-nixpkgs CHANNEL rev the dev shell resolved: root's `nixpkgs`
 * input. This is the rev flake.nix pins in `inputs.nixpkgs.url` and the rev the
 * flake-parity gate compares against flake.lock — DISTINCT from the transitive
 * `nixpkgs-src` the channel resolves to, and from Meissa's own channel.
 */
export function channelNixpkgsRev(devenvLockText: string): string {
	return lockedRevByInputs(devenvLockText, ["nixpkgs"]);
}

// The nixpkgs input URL in flake.nix, which hard-codes the channel rev:
//   inputs.nixpkgs.url = "github:cachix/devenv-nixpkgs/<40-hex-rev>";
// flake.lock records this same rev; the flake-parity gate reds when it skews.
// A devenv-nixpkgs bump leaves this literal stale, so the task rewrites it.
const FLAKE_NIXPKGS_URL_RE =
	/("github:cachix\/devenv-nixpkgs\/)([a-f0-9]{40})(")/;

/**
 * Rewrite the devenv-nixpkgs rev pinned in flake.nix's `inputs.nixpkgs.url` to
 * `newRev`. Returns the full file text with the one rev replaced. Idempotent: a
 * URL already at `newRev` yields identical text. Throws if the pinned URL is
 * absent or `newRev` is not a 40-hex rev (fail loud — a missing pin must not
 * silently no-op and ship a drifted flake.lock the parity gate then reds on).
 */
export function rewriteFlakeNixpkgsUrl(
	flakeNixText: string,
	newRev: string,
): string {
	if (!/^[a-f0-9]{40}$/.test(newRev)) {
		throw new Error(
			`refresh-devenv-nixpkgs: rewriteFlakeNixpkgsUrl given a non-40-hex rev ${JSON.stringify(newRev)}.`,
		);
	}
	if (!FLAKE_NIXPKGS_URL_RE.test(flakeNixText)) {
		throw new Error(
			"refresh-devenv-nixpkgs: no github:cachix/devenv-nixpkgs/<rev> pin found in flake.nix inputs.nixpkgs.url.",
		);
	}
	return flakeNixText.replace(FLAKE_NIXPKGS_URL_RE, `$1${newRev}$3`);
}
