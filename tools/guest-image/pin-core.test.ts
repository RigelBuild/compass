import { describe, expect, test } from "bun:test";
import {
	AGENT_REPO,
	digestRef,
	EXIT,
	isBuildTag,
	isImageDigest,
	layerDigests,
	lockFromInspect,
	locksEqual,
	PinError,
	type PinLock,
	renderLock,
	validatePin,
} from "./pin-core.ts";

const DIGEST =
	"sha256:a2c90fd55c3e015c64a279973121d03c91736eed68d0cb0260cea9ff9b4ab94e";
const LAYER_A =
	"sha256:989a42bd3ddf0028fce105bf936074ae1cad375bbabf868ba358161d6eddea56";
const LAYER_B =
	"sha256:50173e59368b86248a8fc447563bc692eb54cea61522d423c562037b27273b1f";
const TAG = "git-ec4954bd9400";

const lock = (over: Partial<PinLock> = {}): PinLock => ({
	repo: AGENT_REPO,
	tag: TAG,
	digest: DIGEST,
	layers: [LAYER_A, LAYER_B],
	...over,
});

const manifest = (over: Record<string, unknown> = {}) => ({
	schemaVersion: 2,
	mediaType: "application/vnd.oci.image.manifest.v1+json",
	config: { mediaType: "application/vnd.oci.image.config.v1+json" },
	layers: [
		{
			mediaType: "application/vnd.oci.image.layer.v1.tar+gzip",
			digest: LAYER_A,
			size: 898639,
		},
		{
			mediaType: "application/vnd.oci.image.layer.v1.tar+gzip",
			digest: LAYER_B,
			size: 146238,
		},
	],
	...over,
});

/** The exit code a thrown PinError carries, so a test names the fault class
 * rather than just asserting that something threw. */
const codeOf = (fn: () => unknown): number => {
	try {
		fn();
	} catch (err) {
		if (err instanceof PinError) return err.code;
		throw err;
	}
	throw new Error("expected a PinError, nothing was thrown");
};

describe("validatePin provenance", () => {
	test("rejects a repo other than the agent image", () => {
		// The rootfs inherits this image's whole userland, so another repo
		// silently swaps the agent out.
		expect(
			codeOf(() => validatePin(lock({ repo: "ghcr.io/evil/agent" }))),
		).toBe(EXIT.provenance);
	});

	test("rejects a moving tag", () => {
		expect(codeOf(() => validatePin(lock({ tag: "latest" })))).toBe(
			EXIT.provenance,
		);
	});

	test("rejects a git tag of the wrong hex width", () => {
		expect(codeOf(() => validatePin(lock({ tag: "git-abc" })))).toBe(
			EXIT.provenance,
		);
	});

	test("rejects a non-hex git tag", () => {
		expect(codeOf(() => validatePin(lock({ tag: "git-zzzzzzzzzzzz" })))).toBe(
			EXIT.provenance,
		);
	});

	test("accepts the publish lane's tag shape", () => {
		expect(validatePin(lock()).tag).toBe(TAG);
	});
});

describe("validatePin shape", () => {
	test("rejects a malformed manifest digest", () => {
		expect(codeOf(() => validatePin(lock({ digest: "sha256:abc" })))).toBe(
			EXIT.badLock,
		);
	});

	test("rejects a bare hex digest without its algorithm", () => {
		expect(codeOf(() => validatePin(lock({ digest: DIGEST.slice(7) })))).toBe(
			EXIT.badLock,
		);
	});

	test("rejects an empty layer set, which would fetch nothing", () => {
		expect(codeOf(() => validatePin(lock({ layers: [] })))).toBe(EXIT.badLock);
	});

	test("rejects a malformed layer digest", () => {
		expect(
			codeOf(() => validatePin(lock({ layers: [LAYER_A, "sha256:nope"] }))),
		).toBe(EXIT.badLock);
	});

	test("rejects a non-object lock", () => {
		expect(codeOf(() => validatePin([lock()]))).toBe(EXIT.badLock);
		expect(codeOf(() => validatePin(null))).toBe(EXIT.badLock);
	});

	test("returns the layers in order", () => {
		expect(validatePin(lock()).layers).toEqual([LAYER_A, LAYER_B]);
	});
});

describe("lockFromInspect", () => {
	test("builds a lock from the resolved digest and manifest", () => {
		expect(lockFromInspect(AGENT_REPO, TAG, DIGEST, manifest())).toEqual(
			lock(),
		);
	});

	test("refuses a foreign repo before touching the manifest", () => {
		expect(
			codeOf(() =>
				lockFromInspect("ghcr.io/other/img", TAG, DIGEST, manifest()),
			),
		).toBe(EXIT.provenance);
	});

	test("refuses a moving tag", () => {
		expect(
			codeOf(() => lockFromInspect(AGENT_REPO, "latest", DIGEST, manifest())),
		).toBe(EXIT.provenance);
	});

	test("treats a malformed registry digest as a registry fault", () => {
		// Distinct from a bad lock: nothing on disk is wrong, the registry
		// answered with something unusable.
		expect(
			codeOf(() => lockFromInspect(AGENT_REPO, TAG, "garbage", manifest())),
		).toBe(EXIT.registryFailed);
	});
});

describe("layerDigests", () => {
	test("refuses a multi-platform index", () => {
		// An index would need a platform choice the lock has nowhere to record.
		expect(
			codeOf(() =>
				layerDigests(
					manifest({ mediaType: "application/vnd.oci.image.index.v1+json" }),
				),
			),
		).toBe(EXIT.registryFailed);
	});

	test("refuses a docker v2 manifest", () => {
		expect(
			codeOf(() =>
				layerDigests(
					manifest({
						mediaType: "application/vnd.docker.distribution.manifest.v2+json",
					}),
				),
			),
		).toBe(EXIT.registryFailed);
	});

	test("refuses an unexpected layer media type", () => {
		expect(
			codeOf(() =>
				layerDigests(
					manifest({
						layers: [
							{
								mediaType: "application/vnd.oci.image.layer.v1.tar+zstd",
								digest: LAYER_A,
							},
						],
					}),
				),
			),
		).toBe(EXIT.registryFailed);
	});

	test("refuses a manifest with no layers", () => {
		expect(codeOf(() => layerDigests(manifest({ layers: [] })))).toBe(
			EXIT.registryFailed,
		);
	});

	test("preserves manifest order, which determines the stacked filesystem", () => {
		// Asserted in BOTH directions on purpose: LAYER_B sorts before LAYER_A,
		// so an extractor that sorted would still satisfy the flipped case alone.
		expect(layerDigests(manifest())).toEqual([LAYER_A, LAYER_B]);

		const flipped = manifest({
			layers: [
				{
					mediaType: "application/vnd.oci.image.layer.v1.tar+gzip",
					digest: LAYER_B,
				},
				{
					mediaType: "application/vnd.oci.image.layer.v1.tar+gzip",
					digest: LAYER_A,
				},
			],
		});
		expect(layerDigests(flipped)).toEqual([LAYER_B, LAYER_A]);
	});
});

describe("locksEqual", () => {
	test("an unchanged lock compares equal, so a relock no-ops", () => {
		expect(locksEqual(lock(), lock())).toBe(true);
	});

	test("a moved digest compares unequal", () => {
		expect(
			locksEqual(lock(), lock({ digest: `sha256:${"b".repeat(64)}` })),
		).toBe(false);
	});

	test("a reordered layer set compares unequal", () => {
		// The bug the relock exists to catch: same bytes, different filesystem.
		expect(locksEqual(lock(), lock({ layers: [LAYER_B, LAYER_A] }))).toBe(
			false,
		);
	});

	test("a dropped layer compares unequal", () => {
		expect(locksEqual(lock(), lock({ layers: [LAYER_A] }))).toBe(false);
	});
});

describe("rendering and refs", () => {
	test("renders stable bytes, so an unchanged relock writes an identical file", () => {
		expect(renderLock(lock())).toBe(renderLock(lock()));
	});

	test("renders valid JSON that round-trips through validatePin", () => {
		expect(validatePin(JSON.parse(renderLock(lock())))).toEqual(lock());
	});

	test("ends with a newline", () => {
		expect(renderLock(lock()).endsWith("}\n")).toBe(true);
	});

	test("pins by digest, never by tag", () => {
		expect(digestRef(lock())).toBe(`${AGENT_REPO}@${DIGEST}`);
		expect(digestRef(lock())).not.toContain(TAG);
	});
});

describe("digest and tag predicates", () => {
	test("isImageDigest requires the algorithm and full width", () => {
		expect(isImageDigest(DIGEST)).toBe(true);
		expect(isImageDigest(DIGEST.slice(0, 20))).toBe(false);
		expect(isImageDigest(DIGEST.toUpperCase())).toBe(false);
		expect(isImageDigest(undefined)).toBe(false);
	});

	test("isBuildTag accepts only the immutable publish tag", () => {
		expect(isBuildTag(TAG)).toBe(true);
		expect(isBuildTag("latest")).toBe(false);
		expect(isBuildTag("git-ec4954bd940")).toBe(false);
		expect(isBuildTag(`${TAG}-dirty`)).toBe(false);
	});
});
