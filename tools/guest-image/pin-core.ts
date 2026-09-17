// The pure core of the agent-image pin. Every function is a total map over its
// inputs with no I/O, so pin-core.test.ts can drive each mapping — and each
// fail-closed edge — without a registry or a subprocess.
//
// The guest rootfs derives from a PUBLISHED agent image rather than from the
// agent's nix expressions, so the lock is the whole provenance story: it names
// the one repo the rootfs may come from, an immutable per-commit tag, the
// manifest digest those resolve to, and each layer descriptor digest the
// fixed-output fetches key on. Provenance is enforced here rather than
// documented, because a lock that pointed somewhere else would silently make
// the rootfs unreproducible.

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

/** The agent image is a single-platform OCI manifest whose layers are gzipped
 * tarballs; the pin refuses anything else rather than guessing, because an
 * index would need a platform choice the lock has nowhere to record. */
const MANIFEST_MEDIA_TYPE = "application/vnd.oci.image.manifest.v1+json";
const LAYER_MEDIA_TYPE = "application/vnd.oci.image.layer.v1.tar+gzip";

export type PinLock = {
	repo: string;
	tag: string;
	digest: string;
	layers: string[];
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

/**
 * Validate a lock parsed off disk, returning it narrowed.
 *
 * Throws rather than returning a partial lock: every caller (the nix eval's
 * shape re-check, the relock's consistency gate) treats a malformed lock as
 * fatal, and a lenient parse would let a half-valid lock reach a fetch.
 */
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
	if (!Array.isArray(lock.layers) || lock.layers.length === 0) {
		throw new PinError(
			"lock layers must be a non-empty array: they are the fixed-output fetch keys, so an empty set would fetch nothing",
			EXIT.badLock,
		);
	}
	const layers = lock.layers.filter(isImageDigest);
	if (layers.length !== lock.layers.length) {
		const bad = lock.layers.find((l) => !isImageDigest(l));
		throw new PinError(
			`lock layers must each be sha256:<64 hex>, got ${JSON.stringify(bad)}`,
			EXIT.badLock,
		);
	}

	return {
		repo: AGENT_REPO,
		tag: lock.tag,
		digest: lock.digest,
		layers,
	};
}

/**
 * Build a lock from a `skopeo inspect --raw` manifest plus the digest the
 * registry resolved for `tag`.
 *
 * The manifest digest is an INPUT, not something derived from the manifest
 * body: it is what the registry returned for the tag, and recomputing it here
 * would only re-assert our own hashing rather than what the registry serves.
 */
export function lockFromInspect(
	repo: string,
	tag: string,
	digest: string,
	manifest: unknown,
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
	if (!isImageDigest(digest)) {
		throw new PinError(
			`registry returned a malformed digest for ${tag}: ${JSON.stringify(digest)}`,
			EXIT.registryFailed,
		);
	}

	return {
		repo,
		tag,
		digest,
		layers: layerDigests(manifest),
	};
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
			`expected a single-platform OCI manifest (${MANIFEST_MEDIA_TYPE}), got ${JSON.stringify(m.mediaType)}: an index would need a platform choice the lock cannot record`,
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
		a.layers.length === b.layers.length &&
		a.layers.every((l, i) => l === b.layers[i])
	);
}

/** Render a lock as the committed file body: stable key order and a trailing
 * newline, so a relock that changed nothing produces a byte-identical file. */
export function renderLock(lock: PinLock): string {
	return `${JSON.stringify(
		{
			repo: lock.repo,
			tag: lock.tag,
			digest: lock.digest,
			layers: lock.layers,
		},
		null,
		"\t",
	)}\n`;
}
