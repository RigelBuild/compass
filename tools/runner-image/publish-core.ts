// The pure core of the runner-image publish lane. Every function is a total map
// over its inputs with no I/O, so publish-core.test.ts can drive each mapping —
// and each fail-closed edge — without a registry or a subprocess.
//
// The lane publishes the image the build lane produces and reports the RESOLVED
// DIGEST. GHCR has no server-side tag immutability: anyone holding
// packages:write can re-push a tag, so a tag is only a build-addressability
// handle and the deployed contract is `repo@sha256:…`.

/** Exit codes, numbered so a failed run names its own fault in CI. Distinct
 * codes matter because the recoveries differ: 3 is a real secret to rotate,
 * 4 is a registry/transport fault, 5 means the registry resolved something
 * other than what was built, 6 means the local OCI layout is malformed (a
 * distinct fault from a usage mistake — the artifact is wrong, not the argv). */
export const EXIT = {
	usage: 2,
	secretFound: 3,
	pushFailed: 4,
	digestMismatch: 5,
	badLayout: 6,
} as const;

/** Env/label NAME shapes whose presence must block the push. Match is on the
 * name, not the value: a value-shaped heuristic both misses an unusual token
 * and fires on a harmless path. The `(^|_)…($|_)` boundary keeps a keyword from
 * matching mid-word — so `PAT` never fires on `PATH` and `KEY` never fires on
 * `KEYBOARD`, while `SSH_KEY` and `DEPLOY_KEY` still match. */
const SECRET_NAME_PATTERN =
	/(^|_)(TOKEN|SECRET|PASSWORD|PASSWD|PASSPHRASE|APIKEY|API_KEY|KEY|CREDENTIAL|CREDENTIALS|PRIVATE_KEY|SESSION|AUTH|PAT|BEARER)($|_)/i;

/**
 * The config entries that must block the push, given an image config's `Env`
 * and `Labels`. Scanning the BUILT config rather than the Dockerfile is
 * deliberate: several inputs can set env or a label (a base-image ENV/LABEL, a
 * build-arg default), so only the assembled config shows what ships.
 *
 * SCOPE, stated honestly. This matches secret-shaped NAMES in the config's
 * `Env` and `Labels`, plus label VALUES. It does NOT scan layer file contents,
 * the build-arg values recorded in the config `history`, or env/label value
 * shapes — so it is a name-level guard, not a proof of everything the image
 * ships.
 */
export function secretConfigViolations(config: {
	env: readonly string[];
	labels: Readonly<Record<string, string>>;
}): string[] {
	const found: string[] = [];
	for (const entry of config.env) {
		const eq = entry.indexOf("=");
		const name = eq === -1 ? entry : entry.slice(0, eq);
		// An empty value ships nothing, so it is not a leak — but a NAME-only
		// entry (no `=`) inherits the builder's value at runtime, which is.
		const value = eq === -1 ? null : entry.slice(eq + 1);
		if (SECRET_NAME_PATTERN.test(name) && value !== "") found.push(name);
	}
	// Labels carry no runtime-inherit case, so an empty-valued label ships
	// nothing and a secret-shaped KEY is only a leak with a value. The VALUE is
	// also matched: a label recording a secret NAME (e.g. `note=GITHUB_TOKEN`)
	// is as much a tell as the key.
	for (const [key, value] of Object.entries(config.labels)) {
		if (value === "") continue;
		if (SECRET_NAME_PATTERN.test(key) || SECRET_NAME_PATTERN.test(value)) {
			found.push(key);
		}
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
