// Pure decision/transform core for refresh-devenv-nixpkgs.ts (RIG-2432): reading
// the inner nixpkgs rev out of devenv.lock and rewriting the biome catalog pin
// in package.json — unit-testable without a nix runner, network, or git tree.

// The catalog key whose pin mirrors the baked biome linter. Exact-version pin;
// the parity story keeps it string-equal to the biome baked from the channel.
// rumdl is baked from the same channel but carries no catalog pin, so the relock
// rewrites this one pin only.
export const BIOME_CATALOG_KEY = "@biomejs/biome";

/**
 * The concrete NixOS/nixpkgs rev the devenv-nixpkgs channel resolved, read from
 * devenv.lock's inner `nixpkgs-src` node. After `devenv update nixpkgs`
 * re-locks, this is the rev whose raw `legacyPackages` ship the baked linter
 * versions. Parsed as JSON (devenv.lock is JSON), throwing loudly if the node
 * or a 40-hex rev is absent — a shape change must fail the task, never silently
 * eval the wrong (or a stale) rev.
 */
export function innerNixpkgsRev(devenvLockText: string): string {
	let lock: unknown;
	try {
		lock = JSON.parse(devenvLockText);
	} catch (error) {
		throw new Error(
			`refresh-devenv-nixpkgs: devenv.lock is not valid JSON: ${error}`,
		);
	}
	// Narrow with `in`/`typeof` at each level so every access is actually
	// checked (devenv.lock is external-boundary data; no schema validator is in
	// the repo). A shape change surfaces as the loud throw below, never a
	// silently-wrong read.
	const isObj = (v: unknown): v is Record<string, unknown> =>
		typeof v === "object" && v !== null;
	let rev: unknown;
	if (isObj(lock) && "nodes" in lock && isObj(lock.nodes)) {
		const src = lock.nodes["nixpkgs-src"];
		if (isObj(src) && "locked" in src && isObj(src.locked)) {
			rev = src.locked.rev;
		}
	}
	if (typeof rev !== "string" || !/^[a-f0-9]{40}$/.test(rev)) {
		throw new Error(
			"refresh-devenv-nixpkgs: could not read a 40-hex nixpkgs-src rev from devenv.lock " +
				"(nodes['nixpkgs-src'].locked.rev) — devenv lock shape may have changed.",
		);
	}
	return rev;
}

/**
 * The devenv-nixpkgs CHANNEL rev the dev shell resolved, read from
 * devenv.lock's outer `nixpkgs` node (`nodes.nixpkgs.locked.rev`). This is the
 * rev flake.nix pins in `inputs.nixpkgs.url` and the rev the flake-parity gate
 * compares against flake.lock — DISTINCT from `innerNixpkgsRev`, which reads the
 * transitive `nixpkgs-src` node the channel resolves to. Same fail-loud shape
 * discipline: a moved lock shape throws rather than silently reading wrong.
 */
export function channelNixpkgsRev(devenvLockText: string): string {
	let lock: unknown;
	try {
		lock = JSON.parse(devenvLockText);
	} catch (error) {
		throw new Error(
			`refresh-devenv-nixpkgs: devenv.lock is not valid JSON: ${String(error)}`,
		);
	}
	const isObj = (v: unknown): v is Record<string, unknown> =>
		typeof v === "object" && v !== null;
	let rev: unknown;
	if (isObj(lock) && "nodes" in lock && isObj(lock.nodes)) {
		const node = lock.nodes.nixpkgs;
		if (isObj(node) && "locked" in node && isObj(node.locked)) {
			rev = node.locked.rev;
		}
	}
	if (typeof rev !== "string" || !/^[a-f0-9]{40}$/.test(rev)) {
		throw new Error(
			"refresh-devenv-nixpkgs: could not read a 40-hex nixpkgs rev from devenv.lock " +
				"(nodes.nixpkgs.locked.rev) — devenv lock shape may have changed.",
		);
	}
	return rev;
}

// The catalog object in package.json: "catalog": { … }. [^}]* stops at the first
// }, the same scope the catalog customManager in config.json5 trusts (no nested
// objects; config.test.ts guards this). Scoping here keeps the rewrite off the
// "catalog:" CONSUMER references elsewhere in the file.
const CATALOG_BLOCK_RE = /"catalog"\s*:\s*\{[^}]*\}/;

/**
 * Rewrite a single catalog pin to `newVersion`, scoped to the catalog block so
 * a same-named `"key": "catalog:"` consumer reference elsewhere is never
 * touched. Returns the full file text with the one pin replaced. Idempotent: a
 * pin already at `newVersion` yields identical text. Throws if the catalog
 * block or the key within it is absent (fail loud — a missing pin must not
 * silently no-op and ship a drifted gate).
 */
export function rewriteCatalogPin(
	packageJsonText: string,
	key: string,
	newVersion: string,
): string {
	const blockMatch = CATALOG_BLOCK_RE.exec(packageJsonText);
	if (blockMatch === null) {
		throw new Error(
			'refresh-devenv-nixpkgs: no "catalog" block found in package.json.',
		);
	}
	const block = blockMatch[0];
	// Match `"<key>": "<value>"` inside the block. The key is regex-escaped
	// (it contains `/` and `@`, both regex-safe, but `.`/`-` are not). Capture
	// the prefix (key + quote+colon+space + opening quote) so the replacement
	// preserves the exact spacing; swap only the value.
	const escapedKey = key.replace(/[.*+?^${}()|[\]\\]/g, "\\$&");
	const pinRe = new RegExp(`("${escapedKey}"\\s*:\\s*")([^"]+)(")`);
	if (!pinRe.test(block)) {
		throw new Error(
			`refresh-devenv-nixpkgs: catalog pin "${key}" not found in the package.json catalog block.`,
		);
	}
	const rewrittenBlock = block.replace(pinRe, `$1${newVersion}$3`);
	return (
		packageJsonText.slice(0, blockMatch.index) +
		rewrittenBlock +
		packageJsonText.slice(blockMatch.index + block.length)
	);
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
