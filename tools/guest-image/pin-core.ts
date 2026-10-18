// The published image is external to the rootfs build, so the lock enforces
// repository, immutable tag, manifest digest, and layer-digest provenance.

/** Exit codes, numbered so a failed run names its own fault in CI. Distinct
 * codes matter because the recoveries differ: 3 is a bad/hostile pin to
 * correct, 4 is a registry/transport fault to retry, 5 means the registry no
 * longer resolves the tag to the pinned digest (a re-pushed tag or a stale
 * lock), 6 means the lock on disk is malformed. */
export const EXIT = {
	usage: 2,
	provenance: 3,
	registryFailed: 4,
	digestMismatch: 5,
	badLock: 6,
} as const;

/** The ONLY repo a guest rootfs may derive from. The rootfs inherits this
 * image's entire userland, so accepting another repo would swap the agent
 * — and its nixpkgs closure — for something unreviewed. */
export const AGENT_REPO = "ghcr.io/rigelbuild/compass-agent";

/** The publish lane's immutable per-commit tag (`git-` + 12 hex). `:latest`
 * moves, so it is a DISCOVERY handle only and never what a lock pins. */
const BUILD_TAG_PATTERN = /^git-[0-9a-f]{12}$/;

const DIGEST_PATTERN = /^sha256:[0-9a-f]{64}$/;

const INDEX_MEDIA_TYPE = "application/vnd.oci.image.index.v1+json";
const MANIFEST_MEDIA_TYPE = "application/vnd.oci.image.manifest.v1+json";
const LAYER_MEDIA_TYPE = "application/vnd.oci.image.layer.v1.tar+gzip";

export const PLATFORMS = ["linux/amd64", "linux/arm64"] as const;
export type Platform = (typeof PLATFORMS)[number];
export type PinMember = { manifest: string; layers: string[] };
export type PinLock = {
	repo: string;
	tag: string;
	digest: string;
	platforms: Record<Platform, PinMember>;
};
export type IndexMember = { platform: Platform; digest: string };

const platformFrom = (
	os: unknown,
	architecture: unknown,
): Platform | undefined => {
	if (os !== "linux") return undefined;
	if (architecture === "amd64") return "linux/amd64";
	if (architecture === "arm64") return "linux/arm64";
	return undefined;
};

/** A pin fault that carries the exit code the CLI should die with, so the
 * shell never has to re-derive which kind of failure it caught. */
export class PinError extends Error {
	readonly code: number;

	constructor(message: string, code: number) {
		super(message);
		this.name = "PinError";
		this.code = code;
	}
}

export function isImageDigest(value: unknown): value is string {
	return typeof value === "string" && DIGEST_PATTERN.test(value);
}

export function isBuildTag(value: unknown): value is string {
	return typeof value === "string" && BUILD_TAG_PATTERN.test(value);
}

/** The immutable reference the nix fetches resolve. Never `repo:tag`: a tag is
 * a mutable pointer, and GHCR has no server-side tag immutability. */
export function digestRef(lock: PinLock): string {
	return `${lock.repo}@${lock.digest}`;
}

function checkedMember(platform: Platform, value: unknown): PinMember {
	if (typeof value !== "object" || value === null || Array.isArray(value)) {
		throw new PinError(
			`lock ${platform} member must be an object`,
			EXIT.badLock,
		);
	}
	const member = value as Record<string, unknown>;
	if (!isImageDigest(member.manifest)) {
		throw new PinError(
			`lock ${platform} member manifest must be sha256:<64 hex>`,
			EXIT.badLock,
		);
	}
	if (!Array.isArray(member.layers) || member.layers.length === 0) {
		throw new PinError(
			`lock ${platform} member layers must be a non-empty array`,
			EXIT.badLock,
		);
	}
	const layers = member.layers.filter(isImageDigest);
	if (layers.length !== member.layers.length) {
		throw new PinError(
			`lock ${platform} member layers must each be sha256:<64 hex>`,
			EXIT.badLock,
		);
	}
	return { manifest: member.manifest, layers };
}

function checkedIndexMember(value: unknown): IndexMember {
	if (typeof value !== "object" || value === null || Array.isArray(value)) {
		throw new PinError(
			"OCI index member is not an object",
			EXIT.registryFailed,
		);
	}
	const member = value as Record<string, unknown>;
	if (member.mediaType !== MANIFEST_MEDIA_TYPE) {
		throw new PinError(
			`OCI index member must be an image manifest, got ${JSON.stringify(member.mediaType)}`,
			EXIT.registryFailed,
		);
	}
	if (!isImageDigest(member.digest)) {
		throw new PinError(
			"OCI index member has a malformed digest",
			EXIT.registryFailed,
		);
	}
	if (
		typeof member.platform !== "object" ||
		member.platform === null ||
		Array.isArray(member.platform)
	) {
		throw new PinError("OCI index member has no platform", EXIT.registryFailed);
	}
	const platformData = member.platform as Record<string, unknown>;
	const platform = platformFrom(platformData.os, platformData.architecture);
	if (!platform) {
		throw new PinError(
			`OCI index has unsupported platform ${JSON.stringify(platformData.os)}/${JSON.stringify(platformData.architecture)}`,
			EXIT.registryFailed,
		);
	}
	return { platform, digest: member.digest };
}
export function validatePin(value: unknown): PinLock {
	if (typeof value !== "object" || value === null || Array.isArray(value)) {
		throw new PinError("lock is not a JSON object", EXIT.badLock);
	}
	const lock = value as Record<string, unknown>;

	if (lock.repo !== AGENT_REPO) {
		throw new PinError(
			`lock repo must be ${AGENT_REPO}, got ${JSON.stringify(lock.repo)}: the guest rootfs derives its whole userland from this image, so another repo would make it unreproducible`,
			EXIT.provenance,
		);
	}
	if (!isBuildTag(lock.tag)) {
		throw new PinError(
			`lock tag must match git-<12 hex>, got ${JSON.stringify(lock.tag)}: only the publish lane's per-commit tag is immutable, so a moving tag cannot be pinned`,
			EXIT.provenance,
		);
	}
	if (!isImageDigest(lock.digest)) {
		throw new PinError(
			`lock digest must be sha256:<64 hex>, got ${JSON.stringify(lock.digest)}`,
			EXIT.badLock,
		);
	}
	if (
		typeof lock.platforms !== "object" ||
		lock.platforms === null ||
		Array.isArray(lock.platforms)
	) {
		throw new PinError("lock platforms must be an object", EXIT.badLock);
	}
	const rawPlatforms = lock.platforms as Record<string, unknown>;
	const keys = Object.keys(rawPlatforms);
	if (
		keys.length !== PLATFORMS.length ||
		!PLATFORMS.every((platform) => Object.hasOwn(rawPlatforms, platform))
	) {
		throw new PinError(
			`lock platforms must contain exactly ${PLATFORMS.join(" and ")}`,
			EXIT.badLock,
		);
	}
	const platforms: Record<Platform, PinMember> = {
		"linux/amd64": { manifest: "", layers: [] },
		"linux/arm64": { manifest: "", layers: [] },
	};
	for (const platform of PLATFORMS) {
		platforms[platform] = checkedMember(platform, rawPlatforms[platform]);
	}
	return { repo: AGENT_REPO, tag: lock.tag, digest: lock.digest, platforms };
}

/** Validate that an OCI index has exactly one image member for each platform. */
export function indexMembers(index: unknown): IndexMember[] {
	if (typeof index !== "object" || index === null || Array.isArray(index)) {
		throw new PinError("OCI image index is not an object", EXIT.registryFailed);
	}
	const value = index as Record<string, unknown>;
	if (value.mediaType !== INDEX_MEDIA_TYPE) {
		throw new PinError(
			`expected an OCI image index (${INDEX_MEDIA_TYPE}), got ${JSON.stringify(value.mediaType)}`,
			EXIT.registryFailed,
		);
	}
	if (!Array.isArray(value.manifests)) {
		throw new PinError(
			"OCI image index has no manifest list",
			EXIT.registryFailed,
		);
	}
	const found: Partial<Record<Platform, string>> = {};
	for (const entry of value.manifests) {
		const member = checkedIndexMember(entry);
		if (Object.hasOwn(found, member.platform)) {
			throw new PinError(
				`OCI index has duplicate ${member.platform} member`,
				EXIT.registryFailed,
			);
		}
		found[member.platform] = member.digest;
	}
	if (
		!PLATFORMS.every((platform) => found[platform] !== undefined) ||
		value.manifests.length !== PLATFORMS.length
	) {
		throw new PinError(
			`OCI index platform set must contain exactly ${PLATFORMS.join(" and ")}`,
			EXIT.registryFailed,
		);
	}
	return PLATFORMS.map((platform) => ({
		platform,
		digest: found[platform] ?? "",
	}));
}

/** Check the member body digest before reading its ordered layer descriptors. */
export function checkMember(
	member: IndexMember,
	bodyDigest: string,
	manifest: unknown,
): PinMember {
	if (bodyDigest !== member.digest) {
		throw new PinError(
			`${member.platform} member body digest ${bodyDigest} does not match index descriptor ${member.digest}`,
			EXIT.digestMismatch,
		);
	}
	try {
		return { manifest: member.digest, layers: layerDigests(manifest) };
	} catch (error) {
		if (error instanceof PinError) {
			throw new PinError(
				`${member.platform} member: ${error.message}`,
				error.code,
			);
		}
		throw error;
	}
}

/** Confirm registry config metadata agrees with the platform descriptor. */
export function checkMemberConfig(member: IndexMember, config: unknown): void {
	if (typeof config !== "object" || config === null || Array.isArray(config)) {
		throw new PinError(
			`${member.platform} member config is not an object`,
			EXIT.registryFailed,
		);
	}
	const rawConfig = config as Record<string, unknown>;
	const architecture = member.platform === "linux/amd64" ? "amd64" : "arm64";
	if (rawConfig.os !== "linux" || rawConfig.architecture !== architecture) {
		throw new PinError(
			`${member.platform} member config reports ${JSON.stringify(rawConfig.os)}/${JSON.stringify(rawConfig.architecture)}`,
			EXIT.registryFailed,
		);
	}
}

/** Build a validated v2 lock after every index member has been checked. */
export function lockFromIndex(
	repo: string,
	tag: string,
	indexDigest: string,
	members: Record<Platform, PinMember>,
): PinLock {
	if (repo !== AGENT_REPO) {
		throw new PinError(
			`refusing to pin ${repo}: the guest rootfs may only derive from ${AGENT_REPO}`,
			EXIT.provenance,
		);
	}
	if (!isBuildTag(tag)) {
		throw new PinError(
			`refusing to pin tag ${JSON.stringify(tag)}: expected the publish lane's immutable git-<12 hex> tag`,
			EXIT.provenance,
		);
	}
	if (!isImageDigest(indexDigest)) {
		throw new PinError(
			`registry returned a malformed index digest for ${tag}: ${JSON.stringify(indexDigest)}`,
			EXIT.registryFailed,
		);
	}
	return validatePin({ repo, tag, digest: indexDigest, platforms: members });
}

/**
 * The layer descriptor digests, in manifest order.
 *
 * Order is load-bearing: the layers stack into the rootfs, so a reordered set
 * would fetch the same bytes and unpack a different filesystem.
 */
export function layerDigests(manifest: unknown): string[] {
	if (typeof manifest !== "object" || manifest === null) {
		throw new PinError("manifest is not a JSON object", EXIT.registryFailed);
	}
	const m = manifest as Record<string, unknown>;

	if (m.mediaType !== MANIFEST_MEDIA_TYPE) {
		throw new PinError(
			`expected an OCI image manifest (${MANIFEST_MEDIA_TYPE}), got ${JSON.stringify(m.mediaType)}`,
			EXIT.registryFailed,
		);
	}
	if (!Array.isArray(m.layers) || m.layers.length === 0) {
		throw new PinError("manifest carries no layers", EXIT.registryFailed);
	}

	return m.layers.map((entry, i) => {
		const layer: Record<string, unknown> =
			typeof entry === "object" && entry !== null
				? (entry as Record<string, unknown>)
				: {};
		if (layer.mediaType !== LAYER_MEDIA_TYPE) {
			throw new PinError(
				`layer ${i} has media type ${JSON.stringify(layer.mediaType)}, expected ${LAYER_MEDIA_TYPE}`,
				EXIT.registryFailed,
			);
		}
		const digest = layer.digest;
		if (!isImageDigest(digest)) {
			throw new PinError(
				`layer ${i} has a malformed digest: ${JSON.stringify(digest)}`,
				EXIT.registryFailed,
			);
		}
		return digest;
	});
}

/** Whether a relock would change anything. The relock self-gates on this so it
 * is a cheap no-op on every unrelated Renovate branch. */
export function locksEqual(a: PinLock, b: PinLock): boolean {
	return (
		a.repo === b.repo &&
		a.tag === b.tag &&
		a.digest === b.digest &&
		PLATFORMS.every((platform) => {
			const left = a.platforms[platform];
			const right = b.platforms[platform];
			return (
				left.manifest === right.manifest &&
				left.layers.length === right.layers.length &&
				left.layers.every((layer, i) => layer === right.layers[i])
			);
		})
	);
}

/** Render a lock with stable platform and field order. */
export function renderLock(lock: PinLock): string {
	return `${JSON.stringify(
		{
			repo: lock.repo,
			tag: lock.tag,
			digest: lock.digest,
			platforms: Object.fromEntries(
				PLATFORMS.map((platform) => [
					platform,
					{
						manifest: lock.platforms[platform].manifest,
						layers: lock.platforms[platform].layers,
					},
				]),
			),
		},
		null,
		"\t",
	)}\n`;
}
