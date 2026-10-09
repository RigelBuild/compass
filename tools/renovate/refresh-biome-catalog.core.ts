// Pure core for refresh-biome-catalog.ts: the locked Meissa flake ref in
// devenv.lock, and the scoped package.json catalog-pin rewrite —
// unit-testable without a nix runner, network, or git tree.

// The catalog key whose pin mirrors Meissa's biome. Exact-version pin, kept
// string-equal to the biome the dev shell runs. rumdl has no catalog pin.
export const BIOME_CATALOG_KEY = "@biomejs/biome";

/**
 * The exact locked Meissa flake ref, `github:<owner>/<repo>/<rev>?narHash=…`,
 * from root's `meissa` input (resolved by input name, like gate-tools.nix).
 * Its `packages.<sys>.biome` is the derivation the dev shell runs. Throws on any
 * missing field: a shape change must fail the task, never read a stale biome.
 */
export function meissaFlakeRef(devenvLockText: string): string {
	let lock: unknown;
	try {
		lock = JSON.parse(devenvLockText);
	} catch (error) {
		throw new Error(`devenv.lock is not valid JSON: ${String(error)}`);
	}
	const isObj = (v: unknown): v is Record<string, unknown> =>
		typeof v === "object" && v !== null;
	const nodes = isObj(lock) && isObj(lock.nodes) ? lock.nodes : {};
	const root = nodes.root;
	const key = isObj(root) && isObj(root.inputs) ? root.inputs.meissa : null;
	const node = typeof key === "string" ? nodes[key] : undefined;
	const locked = isObj(node) && isObj(node.locked) ? node.locked : {};
	const { owner, repo, rev, narHash } = locked;
	if (
		locked.type !== "github" ||
		typeof owner !== "string" ||
		typeof repo !== "string" ||
		typeof rev !== "string" ||
		!/^[a-f0-9]{40}$/.test(rev) ||
		typeof narHash !== "string" ||
		!narHash.startsWith("sha256-")
	) {
		throw new Error(
			"devenv.lock has no locked github owner/repo/40-hex rev/narHash at root.inputs → meissa — devenv lock shape may have changed.",
		);
	}
	return `github:${owner}/${repo}/${rev}?narHash=${narHash}`;
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
			'refresh-biome-catalog: no "catalog" block found in package.json.',
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
			`refresh-biome-catalog: catalog pin "${key}" not found in the package.json catalog block.`,
		);
	}
	const rewrittenBlock = block.replace(pinRe, `$1${newVersion}$3`);
	return (
		packageJsonText.slice(0, blockMatch.index) +
		rewrittenBlock +
		packageJsonText.slice(blockMatch.index + block.length)
	);
}
