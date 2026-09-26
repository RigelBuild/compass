// The pure core of the runner-image release lane: mint `:vX.Y.Z` by copying an
// already-published `:git-<sha12>` manifest, never by building. No I/O, so
// retag-core.test.ts drives every fail-closed edge without a registry.
//
// The per-push lane only publishes when a push touches the runner closure, so
// the release sha itself often has no image. The newest first-parent ancestor
// that DOES have one contains every closure change at or before it, so its
// bytes are the release's bytes.

import { isImageDigest } from "./publish-core.ts";

/** Exit codes, numbered so a failed run names its own fault in CI. 3 means no
 * published ancestor exists (dispatch publish-runner-image, then re-run), 4 is
 * a registry fault to re-run, 5 means the release tag does not carry the
 * source manifest, 6 means the registry returned something other than one
 * single-platform manifest. */
export const EXIT = {
	usage: 2,
	noAncestor: 3,
	registryFailed: 4,
	incoherent: 5,
	badManifest: 6,
} as const;

/** How many first-parent ancestors the walk probes before giving up. A bounded
 * walk makes a publish outage fail fast instead of probing all of history. */
export const MAX_WALK = 50;

/** A resolver fault carrying the exit code the CLI should die with. */
export class RetagError extends Error {
	readonly code: number;

	constructor(message: string, code: number) {
		super(message);
		this.name = "RetagError";
		this.code = code;
	}
}

/** One `skopeo inspect --raw` run, as the CLI observed it. */
export type ProbeResult = {
	exitCode: number;
	stdout: string;
	stderr: string;
};

export type Probe =
	| { kind: "found"; raw: string }
	| { kind: "absent" }
	| { kind: "ambiguous"; detail: string };

/** Only a definitive "no such tag" answer is `absent`. Everything else — a 5xx,
 * a throttle, an auth blip, a timeout — is `ambiguous`, because treating it as
 * absent would walk on and re-tag an OLDER, stale image. `\b404\b` needs a
 * boundary so the digits of a probed sha12 echoed in the error never match. */
export function classifyProbe(result: ProbeResult): Probe {
	if (result.exitCode === 0) return { kind: "found", raw: result.stdout };
	const detail = result.stderr.trim();
	if (/manifest unknown/i.test(detail) || /\b404\b/.test(detail)) {
		return { kind: "absent" };
	}
	return { kind: "ambiguous", detail };
}

/** The build tag suffix for a commit: a fixed 12-char truncation, not
 * `git rev-parse --short=12`, which grows past 12 on a prefix collision and
 * would then name a tag the publish lane never wrote. */
export function sha12(commit: string): string {
	if (!/^[0-9a-f]{40}$/.test(commit)) {
		throw new RetagError(`not a full commit sha: ${commit}`, EXIT.usage);
	}
	return commit.slice(0, 12);
}

/** The release tag to mint. Only `vX.Y.Z[-pre]` is accepted, so a bad input
 * can never move `:latest` or overwrite a `:git-<sha12>` build tag. */
export function releaseTag(tag: string): string {
	if (!/^v\d+\.\d+\.\d+(-[0-9A-Za-z.-]+)?$/.test(tag)) {
		throw new RetagError(
			`not a vX.Y.Z release tag: ${JSON.stringify(tag)}`,
			EXIT.usage,
		);
	}
	return tag;
}

export type Resolved = {
	/** The ancestor whose `:git-<sha12>` is the release's source image. */
	sha12: string;
	/** 1-based position in the first-parent walk. */
	position: number;
	/** The source manifest bytes, from the probe that found it. */
	raw: string;
};

/**
 * Walk `ancestors` (newest first, the release sha first) and return the first
 * whose `:git-<sha12>` resolves. Probes are sequential so the walk stops at the
 * first hit and never probes older ancestors. An ambiguous probe hard-fails
 * the walk rather than retrying or skipping it.
 */
export async function resolveAncestor(
	ancestors: readonly string[],
	probe: (sha12: string) => Promise<ProbeResult>,
): Promise<Resolved> {
	const walk = ancestors.slice(0, MAX_WALK);
	for (const [index, commit] of walk.entries()) {
		const short = sha12(commit);
		const result = classifyProbe(await probe(short));
		switch (result.kind) {
			case "found":
				return { sha12: short, position: index + 1, raw: result.raw };
			case "absent":
				continue;
			case "ambiguous":
				throw new RetagError(
					`ambiguous registry error probing :git-${short} (not a definitive 404); refusing to fall through to an older ancestor: ${result.detail}`,
					EXIT.registryFailed,
				);
			default:
				return assertNever(result);
		}
	}
	throw new RetagError(
		`no :git-<sha12> image resolved within ${MAX_WALK} first-parent ancestors (walked ${walk.length}). Remediation: workflow_dispatch the release workflow so publish-runner-image publishes the release sha, then re-run this job. Never rebuild here.`,
		EXIT.noAncestor,
	);
}

export type ManifestIdentity = {
	/** sha256 of the exact manifest bytes: the digest `repo@…` resolves. */
	manifestDigest: string;
	/** The image config the manifest names. */
	configDigest: string;
};

/** The identity of a single-platform manifest. The manifest digest is hashed
 * from the bytes locally, so it always describes the body that was read. An
 * index (no top-level config) is refused rather than resolved to a platform. */
export function manifestIdentity(raw: string): ManifestIdentity {
	let parsed: unknown;
	try {
		parsed = JSON.parse(raw);
	} catch (err) {
		throw new RetagError(
			`manifest is not JSON: ${String(err)}`,
			EXIT.badManifest,
		);
	}
	const config =
		parsed && typeof parsed === "object" && "config" in parsed
			? parsed.config
			: undefined;
	const configDigest =
		config && typeof config === "object" && "digest" in config
			? config.digest
			: undefined;
	if (typeof configDigest !== "string" || !isImageDigest(configDigest)) {
		throw new RetagError(
			"manifest carries no sha256 config digest (an image index, or malformed)",
			EXIT.badManifest,
		);
	}
	const manifestDigest = `sha256:${new Bun.CryptoHasher("sha256").update(raw).digest("hex")}`;
	return { manifestDigest, configDigest };
}

/** The release tag must name the source's exact manifest. The config check is
 * the same coherence shape the agent lane uses; the manifest check is what
 * makes the tag's digest equal to the per-push digest. */
export function assertCoherent(
	source: ManifestIdentity,
	release: ManifestIdentity,
): void {
	if (source.configDigest !== release.configDigest) {
		throw new RetagError(
			`re-tag incoherent: config ${release.configDigest} != source ${source.configDigest}`,
			EXIT.incoherent,
		);
	}
	if (source.manifestDigest !== release.manifestDigest) {
		throw new RetagError(
			`re-tag incoherent: manifest ${release.manifestDigest} != source ${source.manifestDigest}`,
			EXIT.incoherent,
		);
	}
}

function assertNever(value: never): never {
	throw new Error(`unhandled probe: ${JSON.stringify(value)}`);
}
