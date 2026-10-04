// The pure core of the gateway-image lane: no I/O, so every mapping and
// fail-closed edge is unit-testable without buildkitd, podman, or a registry.
// GHCR has no server-side tag immutability, so the deployed reference is
// always `repo@sha256:…`; a tag only addresses a build.

/** Exit codes, numbered as in the runner and guest lanes so a failed run names
 * its own fault in CI. 7 and 8 are this lane's own: a failed pre-push smoke,
 * and a pin that is malformed or not on fork main. */
export const EXIT = {
	usage: 2,
	secretFound: 3,
	pushFailed: 4,
	digestMismatch: 5,
	badLayout: 6,
	smokeFailed: 7,
	badPin: 8,
} as const;

export const FORK_REPO = "https://github.com/RigelBuild/oh-my-pi.git";

/** Must equal the default of `ARG PI_BASE` in the fork's `Dockerfile.gateway`:
 * stage 2 resolves its base through a named context of this exact name. */
export const BASE_CONTEXT = "oh-my-pi/pi:dev";
export const PLATFORM = "linux/amd64";
export const SOURCE_DATE_EPOCH = 1;

export type ForkPin = { repo: string; commit: string };

const PIN_KEYS: Record<string, true> = { repo: true, commit: true };
const DIGEST_PATTERN = /^sha256:[0-9a-f]{64}$/;

export function parseForkPin(text: string): ForkPin {
	const raw: unknown = JSON.parse(text);
	if (typeof raw !== "object" || raw === null || Array.isArray(raw)) {
		throw new Error("fork pin must be a JSON object");
	}
	for (const key of Object.keys(raw)) {
		if (!Object.hasOwn(PIN_KEYS, key)) {
			throw new Error(`fork pin: unknown key ${key}`);
		}
	}
	const repo = "repo" in raw ? raw.repo : undefined;
	const commit = "commit" in raw ? raw.commit : undefined;
	if (repo !== FORK_REPO) {
		throw new Error(`fork pin: repo must be ${FORK_REPO}, got ${String(repo)}`);
	}
	if (typeof commit !== "string" || !/^[0-9a-f]{40}$/.test(commit)) {
		throw new Error(
			`fork pin: commit must be 40 lowercase hex characters, got ${String(commit)}`,
		);
	}
	return { repo, commit };
}

/** Stage 1: the fork's `pi-runtime` target, the gateway image's base. */
export function baseBuildArgs(forkDir: string, ociDir: string): string[] {
	return [
		"build",
		"--frontend",
		"dockerfile.v0",
		"--local",
		`context=${forkDir}`,
		"--local",
		`dockerfile=${forkDir}`,
		"--opt",
		"filename=Dockerfile",
		"--opt",
		"target=pi-runtime",
		"--opt",
		`platform=${PLATFORM}`,
		"--opt",
		`build-arg:SOURCE_DATE_EPOCH=${SOURCE_DATE_EPOCH}`,
		"--output",
		`type=oci,dest=${ociDir},tar=false,rewrite-timestamp=true`,
	];
}

/** Stage 2: `Dockerfile.gateway`, with stage 1's layout bound as the base by
 * digest, so stage 2 cannot pull a different base by name. */
export function gatewayBuildArgs(
	forkDir: string,
	baseOciDir: string,
	baseDigest: string,
	ociDir: string,
	metadataFile: string,
): string[] {
	if (!DIGEST_PATTERN.test(baseDigest)) {
		throw new Error(`base digest must be sha256:<64 hex>, got ${baseDigest}`);
	}
	return [
		"build",
		"--frontend",
		"dockerfile.v0",
		"--local",
		`context=${forkDir}`,
		"--local",
		`dockerfile=${forkDir}`,
		"--oci-layout",
		`pibase=${baseOciDir}`,
		"--opt",
		"filename=Dockerfile.gateway",
		"--opt",
		`context:${BASE_CONTEXT}=oci-layout://pibase@${baseDigest}`,
		"--opt",
		`platform=${PLATFORM}`,
		"--opt",
		`build-arg:SOURCE_DATE_EPOCH=${SOURCE_DATE_EPOCH}`,
		"--output",
		`type=oci,dest=${ociDir},tar=false,rewrite-timestamp=true,oci-mediatypes=true`,
		"--metadata-file",
		metadataFile,
	];
}

/** The single sha256 manifest an OCI layout's `index.json` names. Zero or
 * several manifests throw: the lane builds exactly one platform. */
export function layoutDigest(indexJson: string): string {
	const index: unknown = JSON.parse(indexJson);
	if (typeof index !== "object" || index === null || !("manifests" in index)) {
		throw new Error("OCI index has no manifests array");
	}
	const manifests = index.manifests;
	if (!Array.isArray(manifests) || manifests.length !== 1) {
		throw new Error("OCI index must name exactly one manifest");
	}
	const entry: unknown = manifests[0];
	const digest =
		typeof entry === "object" && entry !== null && "digest" in entry
			? entry.digest
			: undefined;
	if (typeof digest !== "string" || !DIGEST_PATTERN.test(digest)) {
		throw new Error(
			`OCI index manifest digest is not sha256: ${String(digest)}`,
		);
	}
	return digest;
}

/** Benign names the gateway config ships: a public-key fingerprint the
 * `python` base sets, and a path. Exempt on an exact match only. */
export const SECRET_NAME_ALLOWLIST: readonly string[] = [
	"GPG_KEY",
	"COMPASS_GATEWAY_TOKEN_FILE",
];

/** Credential-shaped name segments. The `(^|_)…($|_)` boundary keeps `PAT`
 * from firing on `PATH` while `SSH_KEY` still matches. */
const SECRET_NAME_PATTERN =
	/(^|_)(TOKEN|SECRET|PASSWORD|PASSWD|PASSPHRASE|APIKEY|API_KEY|KEY|CREDENTIAL|CREDENTIALS|PRIVATE_KEY|SESSION|AUTH|PAT|BEARER)($|_)/i;

const flagged = (name: string): boolean =>
	!SECRET_NAME_ALLOWLIST.includes(name) && SECRET_NAME_PATTERN.test(name);

/**
 * Config entries that must block the push, from the built config's `Env` and
 * `Labels`. Name-level only: it does not scan layer contents or build history.
 */
export function secretConfigViolations(config: {
	env: readonly string[];
	labels: Readonly<Record<string, string>>;
}): string[] {
	const found: string[] = [];
	for (const entry of config.env) {
		const eq = entry.indexOf("=");
		const name = eq === -1 ? entry : entry.slice(0, eq);
		// A name-only entry inherits the builder's value at runtime; an empty
		// value ships nothing.
		const value = eq === -1 ? null : entry.slice(eq + 1);
		if (flagged(name) && value !== "") found.push(name);
	}
	for (const [key, value] of Object.entries(config.labels)) {
		if (value === "") continue;
		if (flagged(key) || flagged(value)) found.push(key);
	}
	return found;
}

/** The build-addressability tag for a compass commit. */
export function buildTag(repo: string, sha12: string): string {
	if (!/^[0-9a-f]{12}$/.test(sha12)) {
		throw new Error(`sha must be 12 lowercase hex characters, got ${sha12}`);
	}
	if (repo === "") throw new Error("repo must not be empty");
	return `${repo}:git-${sha12}`;
}

/** The immutable reference a deployment pins. Never `repo:tag`. */
export function digestRef(repo: string, digest: string): string {
	if (!DIGEST_PATTERN.test(digest)) {
		throw new Error(`digest must be sha256:<64 hex>, got ${digest}`);
	}
	if (repo === "") throw new Error("repo must not be empty");
	return `${repo}@${digest}`;
}

/**
 * What to do about an existing `:git-<sha12>` tag, given `skopeo inspect --raw`
 * output. Only a registry's own "not found" frees the tag; any other failure
 * aborts, so a transient fault never overwrites a published image.
 */
export function tagDisposition(
	probe: { exitCode: number; stdout: string; stderr: string },
	localDigest: string,
): { action: "publish" | "skip" | "abort"; reason?: string } {
	if (probe.exitCode === 0) {
		// The manifest identity is the sha256 of its RAW bytes, as the registry
		// computes it; re-serialising first would diverge.
		const remote = `sha256:${new Bun.CryptoHasher("sha256").update(probe.stdout).digest("hex")}`;
		if (remote === localDigest) return { action: "skip" };
		return {
			action: "abort",
			reason: `tag already holds ${remote}, refusing to overwrite`,
		};
	}
	if (
		/(^|\W)(manifest unknown|name unknown|manifest not found)(\W|$)/i.test(
			probe.stderr,
		)
	) {
		return { action: "publish" };
	}
	return {
		action: "abort",
		reason: `registry probe failed ambiguously: ${probe.stderr.trim()}`,
	};
}

/** The gateway's `/healthz`: 200 with `{"ok": true}`. */
export function healthzOk(status: number, body: string): boolean {
	return status === 200 && jsonField(body, "ok") === true;
}

/** An authorized `/v1/models`: 200 with an OpenAI-shaped list. */
export function modelsListOk(status: number, body: string): boolean {
	return status === 200 && jsonField(body, "object") === "list";
}

function jsonField(body: string, key: string): unknown {
	let parsed: unknown;
	try {
		parsed = JSON.parse(body);
	} catch {
		return undefined;
	}
	return typeof parsed === "object" && parsed !== null && key in parsed
		? Object.getOwnPropertyDescriptor(parsed, key)?.value
		: undefined;
}
