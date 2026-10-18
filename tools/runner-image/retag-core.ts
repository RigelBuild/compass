// Retag the newest published closure image at or before the release sha; its
// embedded version may lag because version.txt is outside that closure.

import type { Platform } from "./build-core.ts";
import { isImageDigest } from "./publish-core.ts";

export function inspectArgv(ref: string): string[] {
	return ["inspect", "--no-creds", "--raw", `docker://${ref}`];
}

export function copyArgv(
	sourceRef: string,
	destRef: string,
	destAuthFile: string,
): string[] {
	return [
		"copy",
		"--multi-arch",
		"all",
		"--preserve-digests",
		"--src-no-creds",
		"--dest-authfile",
		destAuthFile,
		`docker://${sourceRef}`,
		`docker://${destRef}`,
	];
}

/** Exit codes, numbered so a failed run names its own fault in CI. 3 means no
 * usable published ancestor exists (see REMEDIATION), 4 is a registry fault
 * to re-run, 5 means the release tag does not carry the source manifest, 6
 * means the registry returned something other than one single-platform
 * manifest. */
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

/** The retag lane whose publish job repairs a missing ancestor image. */
export type ImageLane = "runner" | "server";

/** The fix for exit 3. A main dispatch publishes main's HEAD, so it helps only
 * while that HEAD is still the release sha. */
function remediation(lane: ImageLane): string {
	return `Remediation: while the release sha is still main's HEAD, workflow_dispatch the release workflow on main so publish-${lane}-image publishes it, then re-run this job. Once main has moved on, that dispatch publishes a different commit and cannot fix this. Never rebuild here.`;
}

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

/** Only the registry saying the probed tag has no manifest is `absent`. That
 * is `manifest unknown`, or a 404 with an empty body, on skopeo's read of this
 * exact tag. Everything else is `ambiguous`: a 5xx, a 403, a timeout, a missing
 * repository, or a 404 page from a proxy. Reading one of those as absent would
 * walk on and re-tag an older, stale image. */
export function classifyProbe(result: ProbeResult, tag: string): Probe {
	if (result.exitCode === 0) return { kind: "found", raw: result.stdout };
	const detail = result.stderr.trim();
	const scope = `reading manifest ${tag} in `;
	const at = detail.indexOf(scope);
	if (at !== -1) {
		// Drop the `<repo>: ` that follows, leaving the registry's own reason.
		const reason = detail.slice(at + scope.length).replace(/^\S+: /, "");
		if (
			reason.startsWith("manifest unknown") ||
			reason.startsWith('StatusCode: 404, \\"\\"')
		) {
			return { kind: "absent" };
		}
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
	/** The full ancestor sha, for the closure diff against the release sha. */
	commit: string;
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
	lane: ImageLane,
): Promise<Resolved> {
	const walk = ancestors.slice(0, MAX_WALK);
	for (const [index, commit] of walk.entries()) {
		const short = sha12(commit);
		const result = classifyProbe(await probe(short), `git-${short}`);
		switch (result.kind) {
			case "found":
				return { commit, sha12: short, position: index + 1, raw: result.raw };
			case "absent":
				continue;
			case "ambiguous":
				throw new RetagError(
					`ambiguous registry error probing :git-${short} (not a definitive missing-tag answer); refusing to fall through to an older ancestor: ${result.detail}`,
					EXIT.registryFailed,
				);
			default:
				return assertNever(result);
		}
	}
	throw new RetagError(
		`no :git-<sha12> image resolved within ${MAX_WALK} first-parent ancestors (walked ${walk.length}). ${remediation(lane)}`,
		EXIT.noAncestor,
	);
}

/** The names `git diff -z --name-only` printed. Every name ends in NUL, so the
 * last split entry is empty. -z keeps a name with a newline whole. */
export function splitNulPaths(stdout: string): string[] {
	const names = stdout.split("\0");
	if (names.at(-1) === "") names.pop();
	return names;
}

/** The changed paths that fall in the closure set. Same rules as the publish
 * gate: `dir/**` matches anything under `dir/`, any other entry matches exactly.
 * An empty set is refused, so an unset env var cannot pass every diff. */
export function closureChanges(
	closurePaths: string,
	changed: readonly string[],
	lane: ImageLane,
): string[] {
	const patterns = closurePaths
		.split("\n")
		.map((line) => line.trim())
		.filter((line) => line.length > 0);
	if (patterns.length === 0) {
		throw new RetagError(`the ${lane} closure path set is empty`, EXIT.usage);
	}
	return changed.filter((path) =>
		patterns.some((pattern) =>
			pattern.endsWith("/**")
				? path.startsWith(pattern.slice(0, -2))
				: path === pattern,
		),
	);
}

/** An absent tag can mean a failed publish, not "no closure change". So any
 * closure change between the resolved ancestor and the release sha fails the
 * mint rather than tag an image that lacks it. */
export function assertNoClosureChange(
	closurePaths: string,
	resolvedSha12: string,
	changed: readonly string[],
	lane: ImageLane,
): void {
	const hits = closureChanges(closurePaths, changed, lane);
	if (hits.length > 0) {
		throw new RetagError(
			`:git-${resolvedSha12} predates ${lane} closure changes it lacks (${hits.join(", ")}); a newer publish is missing. ${remediation(lane)}`,
			EXIT.noAncestor,
		);
	}
}

export type ManifestIdentity =
	| { kind: "manifest"; manifestDigest: string; configDigest: string }
	| {
			kind: "index";
			manifestDigest: string;
			members: { platform: Platform; digest: string }[];
	  };

/** Return the checked identity of a single manifest or its two-platform index. */
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
	const manifestDigest = `sha256:${new Bun.CryptoHasher("sha256").update(raw).digest("hex")}`;
	if (parsed && typeof parsed === "object" && "manifests" in parsed) {
		const rawMembers = parsed.manifests;
		if (!Array.isArray(rawMembers)) {
			throw new RetagError("index manifests are malformed", EXIT.badManifest);
		}
		const members: { platform: Platform; digest: string }[] = [];
		for (const member of rawMembers) {
			if (
				!member ||
				typeof member !== "object" ||
				!("platform" in member) ||
				!("digest" in member)
			) {
				throw new RetagError("index member is malformed", EXIT.badManifest);
			}
			const platform = member.platform;
			const digest = member.digest;
			if (
				!platform ||
				typeof platform !== "object" ||
				!("os" in platform) ||
				!("architecture" in platform)
			) {
				throw new RetagError(
					"index member platform is malformed",
					EXIT.badManifest,
				);
			}
			const { os, architecture } = platform;
			if (
				os !== "linux" ||
				(architecture !== "amd64" && architecture !== "arm64") ||
				typeof digest !== "string" ||
				!isImageDigest(digest)
			) {
				throw new RetagError(
					"index member platform or digest is malformed",
					EXIT.badManifest,
				);
			}
			members.push({
				platform: architecture === "amd64" ? "linux/amd64" : "linux/arm64",
				digest,
			});
		}
		const platforms = members.map((member) => member.platform).sort();
		if (
			platforms.length !== 2 ||
			platforms[0] !== "linux/amd64" ||
			platforms[1] !== "linux/arm64"
		) {
			throw new RetagError(
				"index must contain linux/amd64 and linux/arm64",
				EXIT.badManifest,
			);
		}
		return { kind: "index", manifestDigest, members };
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
	return { kind: "manifest", manifestDigest, configDigest };
}

/** Compare both the copied bytes and any per-platform descriptors. */
export function assertCoherent(
	source: ManifestIdentity,
	release: ManifestIdentity,
): void {
	if (source.manifestDigest !== release.manifestDigest) {
		throw new RetagError(
			`re-tag incoherent: manifest ${release.manifestDigest} != source ${source.manifestDigest}`,
			EXIT.incoherent,
		);
	}
	if (source.kind !== release.kind) {
		throw new RetagError(
			"re-tag incoherent: manifest type differs",
			EXIT.incoherent,
		);
	}
	if (source.kind === "manifest" && release.kind === "manifest") {
		if (source.configDigest !== release.configDigest) {
			throw new RetagError(
				`re-tag incoherent: config ${release.configDigest} != source ${source.configDigest}`,
				EXIT.incoherent,
			);
		}
		return;
	}
	if (source.kind === "index" && release.kind === "index") {
		const sourceMembers = [...source.members].sort((a, b) =>
			a.platform.localeCompare(b.platform),
		);
		const releaseMembers = [...release.members].sort((a, b) =>
			a.platform.localeCompare(b.platform),
		);
		if (JSON.stringify(sourceMembers) !== JSON.stringify(releaseMembers)) {
			throw new RetagError(
				"re-tag incoherent: index members differ",
				EXIT.incoherent,
			);
		}
	}
}
/** Flux orders each shared tag by this first-parent sequence number. */
export function mainTag(firstParentCount: number, commit: string): string {
	if (!Number.isInteger(firstParentCount) || firstParentCount < 1) {
		throw new RetagError(
			`not a positive first-parent count: ${firstParentCount}`,
			EXIT.usage,
		);
	}
	return `main-${firstParentCount}-${sha12(commit)}`;
}

/** Reject stale lane images before pairing tags for a commit. */
export async function resolveMainPair(input: {
	ancestors: readonly string[];
	probe: Record<ImageLane, (sha12: string) => Promise<ProbeResult>>;
	changedSince: (commit: string) => Promise<string[]>;
	closurePaths: Record<ImageLane, string>;
}): Promise<Record<ImageLane, Resolved>> {
	const resolveLane = async (lane: ImageLane): Promise<Resolved> => {
		const resolved = await resolveAncestor(
			input.ancestors,
			input.probe[lane],
			lane,
		);
		assertNoClosureChange(
			input.closurePaths[lane],
			resolved.sha12,
			await input.changedSince(resolved.commit),
			lane,
		);
		return resolved;
	};
	const [server, runner] = await Promise.all([
		resolveLane("server"),
		resolveLane("runner"),
	]);
	return { runner, server };
}

/** Resolve both lanes first so failure cannot leave only one new tag. */
export async function tagMainPair(input: {
	resolve: () => Promise<Record<ImageLane, Resolved>>;
	write: (lane: ImageLane, resolved: Resolved) => Promise<void>;
}): Promise<void> {
	const pair = await input.resolve();
	await input.write("server", pair.server);
	await input.write("runner", pair.runner);
}
function assertNever(value: never): never {
	throw new Error(`unhandled probe: ${JSON.stringify(value)}`);
}
