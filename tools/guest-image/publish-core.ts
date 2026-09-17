// The pure core of the guest-image publish lane: no I/O, so every mapping and
// fail-closed edge is unit-testable without a registry. GHCR has no
// server-side tag immutability, so a tag only addresses a build and the
// deployed contract is `repo@sha256:…`.

/** Exit codes, numbered so a failed run names its own fault in CI. The
 * recoveries differ: 3 is a real secret to rotate, 4 is a registry/transport
 * fault, 5 means the registry resolved something other than what was built, 6
 * means the local layout is malformed (the artifact is wrong, not the argv). */
export const EXIT = {
	usage: 2,
	secretFound: 3,
	pushFailed: 4,
	digestMismatch: 5,
	badLayout: 6,
} as const;

/** The empty config every non-runnable artifact points at: the literal two
 * bytes `{}`, per the OCI 1.1 guidance for a manifest with no runnable config. */
export const EMPTY_CONFIG_BYTES = new TextEncoder().encode("{}");

export const ARTIFACT_TYPE = "application/vnd.compass.guest-image.v1";
export const EMPTY_CONFIG_MEDIA_TYPE = "application/vnd.oci.empty.v1+json";
const MANIFEST_MEDIA_TYPE = "application/vnd.oci.image.manifest.v1+json";
const INDEX_MEDIA_TYPE = "application/vnd.oci.image.index.v1+json";

const LAYER_MEDIA_TYPES = {
	kernel: "application/vnd.compass.guest-kernel.v1",
	rootfs: "application/vnd.compass.guest-rootfs.v1+erofs",
	initrd: "application/vnd.compass.guest-initrd.v1+cpio.zst",
} as const;

export type AssetName = keyof typeof LAYER_MEDIA_TYPES;

/** Layer order is contract: a materialiser reads the three positionally. */
export const ASSET_ORDER: readonly AssetName[] = ["kernel", "rootfs", "initrd"];

/** One realised asset. */
export type Asset = {
	readonly digest: string;
	readonly size: number;
};

export type Descriptor = {
	readonly mediaType: string;
	readonly digest: string;
	readonly size: number;
};

export type LayoutPlan = {
	readonly config: Descriptor;
	readonly configBytes: Uint8Array;
	readonly layers: readonly Descriptor[];
	readonly annotations: Readonly<Record<string, string>>;
	readonly manifest: string;
	readonly manifestDescriptor: Descriptor;
	readonly index: string;
};

/** Credential-shaped NAME segments whose presence must block the push. The
 * boundary spans `_`, `.` and `-`, because OCI annotation keys are
 * dot-and-dash separated (`org.compass.guest.registry-token`) — an
 * underscore-only boundary, the shape this pattern has in env-var land, is
 * inert against every key this lane actually emits. Matching on the name, not
 * the value: a value-shaped heuristic both misses an unusual token and fires
 * on a harmless path. The boundary still keeps a keyword from matching
 * mid-word, so `PAT` never fires on `PATH`. */
const SECRET_NAME_PATTERN =
	/(^|[_.-])(TOKEN|SECRET|PASSWORD|PASSWD|PASSPHRASE|APIKEY|API_KEY|KEY|CREDENTIAL|CREDENTIALS|PRIVATE_KEY|SESSION|AUTH|PAT|BEARER)([_.-]|$)/i;

const DIGEST_PATTERN = /^sha256:[0-9a-f]{64}$/;

/** The on-disk filenames a materialiser writes. `verifyImages` keys its
 * sha256sum manifest on `filepath.Base` of each configured path, so these —
 * not the nix store basenames, which carry a content hash — are what the
 * annotations must name. */
export const ASSET_FILENAMES = {
	kernel: "bzImage",
	rootfs: "compass-guest-rootfs.erofs",
	initrd: "compass-guest-initrd",
} as const;

/** Per-asset digest annotations carry the same filename→hex facts the Runner's
 * `--microvm-image-manifest` consumes, so a materialiser can write that file
 * straight from the manifest it pulled. Bare hex, not `sha256:…`, because that
 * is the sha256sum format the Runner parses. */
export const LAYER_ANNOTATION_PREFIX = "org.compass.guest.layer.";
export const AGENT_DIGEST_ANNOTATION = "org.compass.guest.agent-image-digest";

export function isDigest(value: string): boolean {
	return DIGEST_PATTERN.test(value);
}

/**
 * Map the three realised assets into the OCI layout that publishes them.
 *
 * Every descriptor carries its real byte length: a registry validates the
 * declared size against the blob it receives, so a placeholder size is
 * rejected at push — the one place a local `skopeo copy` will not catch it,
 * because the local path reads the blob and never compares.
 */
export function layoutPlan(
	assets: Readonly<Record<AssetName, Asset>>,
	provenance: { revision: string; agentImageDigest: string },
): LayoutPlan {
	for (const name of ASSET_ORDER) {
		const asset = assets[name];
		if (!isDigest(asset.digest)) {
			throw new Error(
				`${name}: digest must be sha256:<64 hex>, got ${asset.digest}`,
			);
		}
		if (!Number.isSafeInteger(asset.size) || asset.size <= 0) {
			throw new Error(
				`${name}: size must be a positive integer, got ${asset.size}`,
			);
		}
	}
	if (!isDigest(provenance.agentImageDigest)) {
		throw new Error(
			`agent image digest must be sha256:<64 hex>, got ${provenance.agentImageDigest}`,
		);
	}
	if (provenance.revision === "") throw new Error("revision must not be empty");

	const layers = ASSET_ORDER.map((name) => ({
		mediaType: LAYER_MEDIA_TYPES[name],
		digest: assets[name].digest,
		size: assets[name].size,
	}));
	const annotations: Record<string, string> = {
		"org.opencontainers.image.revision": provenance.revision,
		[AGENT_DIGEST_ANNOTATION]: provenance.agentImageDigest,
	};
	for (const name of ASSET_ORDER) {
		annotations[`${LAYER_ANNOTATION_PREFIX}${ASSET_FILENAMES[name]}`] = assets[
			name
		].digest.slice("sha256:".length);
	}

	const config: Descriptor = {
		mediaType: EMPTY_CONFIG_MEDIA_TYPE,
		digest: digestBytes(EMPTY_CONFIG_BYTES),
		size: EMPTY_CONFIG_BYTES.byteLength,
	};
	// schemaVersion is REQUIRED and must be 2; a manifest without it is
	// rejected by a conformant registry even though a local copy accepts it.
	const manifest = JSON.stringify({
		schemaVersion: 2,
		mediaType: MANIFEST_MEDIA_TYPE,
		artifactType: ARTIFACT_TYPE,
		config,
		layers,
		annotations,
	});
	const manifestBytes = new TextEncoder().encode(manifest);
	const manifestDescriptor: Descriptor = {
		mediaType: MANIFEST_MEDIA_TYPE,
		digest: digestBytes(manifestBytes),
		size: manifestBytes.byteLength,
	};
	const index = JSON.stringify({
		schemaVersion: 2,
		mediaType: INDEX_MEDIA_TYPE,
		manifests: [{ ...manifestDescriptor, artifactType: ARTIFACT_TYPE }],
	});
	return {
		config,
		configBytes: EMPTY_CONFIG_BYTES,
		layers,
		annotations,
		manifest,
		manifestDescriptor,
		index,
	};
}

/** The manifest's identity is the sha256 of its RAW bytes — the same identity
 * the registry computes, so re-serialising before hashing would diverge. */
export function manifestDigest(manifest: string): string {
	return digestBytes(new TextEncoder().encode(manifest));
}

/** Annotation entries that must block the push. The config is empty and the
 * layers are opaque binaries, so annotations are the only place this lane
 * could leak a credential. */
export function annotationViolations(
	annotations: Readonly<Record<string, string>>,
): string[] {
	const found: string[] = [];
	for (const [name, value] of Object.entries(annotations)) {
		if (SECRET_NAME_PATTERN.test(name) || SECRET_NAME_PATTERN.test(value))
			found.push(name);
	}
	return found;
}

/** The build-addressability tag for a commit. Nothing deployed resolves
 * through it. */
export function buildTag(repo: string, sha12: string): string {
	if (!/^[0-9a-f]{12}$/.test(sha12))
		throw new Error(`sha must be 12 lowercase hex characters, got ${sha12}`);
	if (repo === "") throw new Error("repo must not be empty");
	return `${repo}:git-${sha12}`;
}

/** The immutable reference a deployment pins. Never `repo:tag`. */
export function digestRef(repo: string, digest: string): string {
	if (!isDigest(digest))
		throw new Error(`digest must be sha256:<64 hex>, got ${digest}`);
	if (repo === "") throw new Error("repo must not be empty");
	return `${repo}@${digest}`;
}

/**
 * What to do about an existing `:git-<sha12>` tag, given what the registry
 * returned for it.
 *
 * `skip` is the idempotent re-run of the same commit. `abort` covers both a tag
 * holding different content and any inspect failure we cannot read as a plain
 * absence — an ambiguous registry answer must never be treated as "free to
 * push", or a transient fault would silently overwrite a published artifact.
 */
export function tagDisposition(
	probe: { exitCode: number; stdout: string; stderr: string },
	localManifestDigest: string,
): { action: "publish" | "skip" | "abort"; reason?: string } {
	if (probe.exitCode === 0) {
		const remote = manifestDigest(probe.stdout);
		if (remote === localManifestDigest) return { action: "skip" };
		return {
			action: "abort",
			reason: `tag already holds ${remote}, refusing to overwrite`,
		};
	}
	// Only a registry's own "this name/manifest does not exist" answer means the
	// tag is free. Anything else — auth, transport, a local creds problem — is
	// ambiguous, and treating it as absence would overwrite a published
	// artifact, so it aborts.
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

function digestBytes(bytes: Uint8Array): string {
	const hasher = new Bun.CryptoHasher("sha256");
	hasher.update(bytes);
	return `sha256:${hasher.digest("hex")}`;
}
