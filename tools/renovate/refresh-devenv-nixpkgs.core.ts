// Pure decision/transform core for refresh-devenv-nixpkgs.ts (RIG-2432).
//
// Split out from the entry point so the load-bearing string/JSON transforms —
// reading the inner nixpkgs rev out of devenv.lock and rewriting the biome
// catalog pin in package.json — are unit-testable without a nix runner, a
// network, or a git tree. The entry point owns the shell-outs (re-lock, eval,
// bun install); this file owns the parsing and rewriting.

// The catalog key whose pin mirrors the baked biome linter (package.json
// workspaces.catalog). Exact-version pin today; the dev-shell parity story
// keeps it string-equal to the biome baked from the devenv-nixpkgs channel
// (devenv.nix). Compass bakes markdownlint-cli2 from the same channel, but it
// carries no catalog pin (package.json has no markdownlint-cli2 entry), so the
// relock rewrites this one pin only.
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

// The catalog object in the root package.json: `"catalog": { … }`. `[^}]*`
// stops at the first `}` — the same scope the catalog customManager in
// config.json5 already trusts (the block carries no nested objects; a nested
// object there would break both this and that manager, and config.test.ts
// guards against it). Scoping to this block is what keeps the rewrite off the
// `"@biomejs/biome": "catalog:"` CONSUMER references elsewhere in the file,
// which carry the literal `catalog:` value, not a version.
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

// The nixpkgs input URL in the repo-root flake.nix, which hard-codes the
// devenv-nixpkgs channel rev in the URL itself:
//   inputs.nixpkgs.url = "github:cachix/devenv-nixpkgs/<40-hex-rev>";
// The rev is captured; `flake.lock` records this same rev, and the
// flake-parity gate (tools/toolchain/flake-parity.ts) fails CI when it skews
// from devenv.lock's nixpkgs rev. A devenv-nixpkgs bump moves devenv.lock but
// leaves this literal stale, so the refresh task rewrites it in lockstep.
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
