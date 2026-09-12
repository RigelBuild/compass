// The pure core of the runner-image publish lane (R2). Every function is a
// total map over its inputs with no I/O, so publish-core.test.ts can drive each
// mapping — and each fail-closed edge — without a registry or a subprocess.
//
// The lane publishes the image the R1 build lane produces and reports the
// RESOLVED DIGEST. GHCR has no server-side tag immutability: anyone holding
// packages:write can re-push a tag, so a tag is only a build-addressability
// handle and the deployed contract is `repo@sha256:…`.

/** Exit codes, numbered so a failed run names its own fault in CI. Distinct
 * codes matter because the recoveries differ: 3 is a real secret to rotate,
 * 4 is a registry/transport fault, 5 means the registry resolved something
 * other than what was built. */
export const EXIT = {
	usage: 2,
	secretFound: 3,
	pushFailed: 4,
	digestMismatch: 5,
} as const;

/** Env var names whose VALUE must never be baked into a published image. Match
 * is on the name, not the value: a value-shaped heuristic both misses an
 * unusual token and fires on a harmless path. */
const SECRET_NAME_PATTERN =
	/(^|_)(TOKEN|SECRET|PASSWORD|PASSWD|APIKEY|API_KEY|CREDENTIAL|CREDENTIALS|PRIVATE_KEY|SESSION|AUTH)($|_)/i;

/**
 * The env entries that must block the push, given an image config's `Env`.
 * Scanning the BUILT config rather than the Dockerfile is deliberate: several
 * inputs can set env (a base-image ENV, a build-arg default), so only the
 * assembled config proves what ships.
 */
export function secretEnvViolations(env: readonly string[]): string[] {
	const found: string[] = [];
	for (const entry of env) {
		const eq = entry.indexOf("=");
		const name = eq === -1 ? entry : entry.slice(0, eq);
		// An empty value ships nothing, so it is not a leak — but a NAME-only
		// entry (no `=`) inherits the builder's value at runtime, which is.
		const value = eq === -1 ? null : entry.slice(eq + 1);
		if (SECRET_NAME_PATTERN.test(name) && value !== "") found.push(name);
	}
	return found;
}

/** A digest is the sha256 form the registry returns. Anything else — a tag, a
 * truncated hex, a bare hex without the algorithm — is rejected rather than
 * normalised: silently accepting a short form would let a mismatch read as a
 * match on its shared prefix. */
export function isImageDigest(value: string): boolean {
	return /^sha256:[0-9a-f]{64}$/.test(value);
}

/** The immutable reference the DaemonSet pins. Never `repo:tag`. */
export function digestRef(repo: string, digest: string): string {
	if (!isImageDigest(digest)) {
		throw new Error(`not a sha256 digest: ${digest}`);
	}
	return `${repo}@${digest}`;
}

/** The build-addressability tag for a commit. A tag is a convenience for
 * finding the build; nothing deployed resolves through it. */
export function buildTag(repo: string, sha12: string): string {
	if (!/^[0-9a-f]{12}$/.test(sha12)) {
		throw new Error(`not a 12-char commit sha: ${sha12}`);
	}
	return `${repo}:git-${sha12}`;
}

/**
 * The digest BuildKit recorded for the pushed image, read from a
 * `--metadata-file` payload. This is the registry's own answer — the exporter
 * reports what the registry accepted, so it is the check that the bytes now
 * addressable are the bytes built.
 */
export function digestFromMetadata(metadata: unknown): string | undefined {
	if (!metadata || typeof metadata !== "object") return undefined;
	if (!("containerimage.digest" in metadata)) return undefined;
	const digest = metadata["containerimage.digest"];
	if (typeof digest !== "string" || !isImageDigest(digest)) return undefined;
	return digest;
}
