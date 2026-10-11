import { describe, expect, test } from "bun:test";
import {
	AGENT_REPO,
	checkMember,
	checkMemberConfig,
	digestRef,
	EXIT,
	indexMembers,
	isBuildTag,
	isImageDigest,
	layerDigests,
	lockFromIndex,
	locksEqual,
	PinError,
	type PinLock,
	PLATFORMS,
	type Platform,
	renderLock,
	validatePin,
} from "./pin-core.ts";

const DIGEST =
	"sha256:a2c90fd55c3e015c64a279973121d03c91736eed68d0cb0260cea9ff9b4ab94e";
const AMD_MANIFEST =
	"sha256:1111111111111111111111111111111111111111111111111111111111111111";
const ARM_MANIFEST =
	"sha256:2222222222222222222222222222222222222222222222222222222222222222";
const LAYER_A =
	"sha256:989a42bd3ddf0028fce105bf936074ae1cad375bbabf868ba358161d6eddea56";
const LAYER_B =
	"sha256:50173e59368b86248a8fc447563bc692eb54cea61522d423c562037b27273b1f";
const TAG = "git-ec4954bd9400";
const MANIFEST_MEDIA_TYPE = "application/vnd.oci.image.manifest.v1+json";

const manifest = (layers: string[] = [LAYER_A, LAYER_B]) => ({
	schemaVersion: 2,
	mediaType: MANIFEST_MEDIA_TYPE,
	config: { mediaType: "application/vnd.oci.image.config.v1+json" },
	layers: layers.map((digest) => ({
		mediaType: "application/vnd.oci.image.layer.v1.tar+gzip",
		digest,
		size: 123,
	})),
});

const index = (
	descriptors: unknown[] = [
		{
			mediaType: MANIFEST_MEDIA_TYPE,
			digest: AMD_MANIFEST,
			platform: { os: "linux", architecture: "amd64" },
		},
		{
			mediaType: MANIFEST_MEDIA_TYPE,
			digest: ARM_MANIFEST,
			platform: { os: "linux", architecture: "arm64" },
		},
	],
) => ({
	schemaVersion: 2,
	mediaType: "application/vnd.oci.image.index.v1+json",
	manifests: descriptors,
});

const pinMember = (manifest: string, layers = [LAYER_A, LAYER_B]) => ({
	manifest,
	layers,
});

const lock = (over: Partial<PinLock> = {}): PinLock => ({
	repo: AGENT_REPO,
	tag: TAG,
	digest: DIGEST,
	platforms: {
		"linux/amd64": pinMember(AMD_MANIFEST),
		"linux/arm64": pinMember(ARM_MANIFEST),
	},
	...over,
});

const codeOf = (fn: () => unknown): number => {
	try {
		fn();
	} catch (err) {
		if (err instanceof PinError) return err.code;
		throw err;
	}
	throw new Error("expected a PinError, nothing was thrown");
};

describe("indexMembers", () => {
	test("returns both platform descriptors in lock order", () => {
		expect(indexMembers(index())).toEqual([
			{ platform: "linux/amd64", digest: AMD_MANIFEST },
			{ platform: "linux/arm64", digest: ARM_MANIFEST },
		]);
		expect(PLATFORMS).toEqual(["linux/amd64", "linux/arm64"]);
	});

	test("refuses a missing platform", () => {
		expect(
			codeOf(() =>
				indexMembers(
					index([
						{
							mediaType: MANIFEST_MEDIA_TYPE,
							digest: AMD_MANIFEST,
							platform: { os: "linux", architecture: "amd64" },
						},
					]),
				),
			),
		).toBe(EXIT.registryFailed);
	});

	test("refuses an extra platform", () => {
		expect(
			codeOf(() =>
				indexMembers(
					index([
						...index().manifests,
						{
							mediaType: MANIFEST_MEDIA_TYPE,
							digest: DIGEST,
							platform: { os: "linux", architecture: "s390x" },
						},
					]),
				),
			),
		).toBe(EXIT.registryFailed);
	});

	test("refuses a duplicate platform", () => {
		expect(
			codeOf(() =>
				indexMembers(
					index([
						...index().manifests,
						{
							mediaType: MANIFEST_MEDIA_TYPE,
							digest: DIGEST,
							platform: { os: "linux", architecture: "amd64" },
						},
					]),
				),
			),
		).toBe(EXIT.registryFailed);
	});

	test("refuses nested indexes and non-OCI index media types", () => {
		expect(
			codeOf(() =>
				indexMembers(
					index([
						{
							mediaType: "application/vnd.oci.image.index.v1+json",
							digest: AMD_MANIFEST,
							platform: { os: "linux", architecture: "amd64" },
						},
						{
							mediaType: MANIFEST_MEDIA_TYPE,
							digest: ARM_MANIFEST,
							platform: { os: "linux", architecture: "arm64" },
						},
					]),
				),
			),
		).toBe(EXIT.registryFailed);
		expect(
			codeOf(() =>
				indexMembers({
					...index(),
					mediaType:
						"application/vnd.docker.distribution.manifest.list.v2+json",
				}),
			),
		).toBe(EXIT.registryFailed);
	});

	test("refuses a top-level image manifest", () => {
		expect(codeOf(() => indexMembers(manifest()))).toBe(EXIT.registryFailed);
	});
});

describe("member validation", () => {
	test("checks descriptor hash and preserves layer order", () => {
		const member = {
			platform: "linux/amd64" as Platform,
			digest: AMD_MANIFEST,
		};
		expect(checkMember(member, AMD_MANIFEST, manifest())).toEqual(
			pinMember(AMD_MANIFEST),
		);
		expect(codeOf(() => checkMember(member, ARM_MANIFEST, manifest()))).toBe(
			EXIT.digestMismatch,
		);
	});

	test("reports which platform has an invalid member body", () => {
		expect(() =>
			checkMember(
				{ platform: "linux/arm64", digest: ARM_MANIFEST },
				ARM_MANIFEST,
				{ mediaType: "application/vnd.oci.image.index.v1+json" },
			),
		).toThrow("linux/arm64 member");
	});

	test("rejects a config whose architecture differs from the member", () => {
		expect(
			codeOf(() =>
				checkMemberConfig(
					{ platform: "linux/arm64", digest: ARM_MANIFEST },
					{ os: "linux", architecture: "amd64" },
				),
			),
		).toBe(EXIT.registryFailed);
		expect(() =>
			checkMemberConfig(
				{ platform: "linux/arm64", digest: ARM_MANIFEST },
				{ os: "linux", architecture: "arm64" },
			),
		).not.toThrow();
	});

	test("constructs a lock after validating provenance and members", () => {
		expect(lockFromIndex(AGENT_REPO, TAG, DIGEST, lock().platforms)).toEqual(
			lock(),
		);
		expect(
			codeOf(() =>
				lockFromIndex(AGENT_REPO, TAG, "malformed", lock().platforms),
			),
		).toBe(EXIT.registryFailed);
	});
});

describe("validatePin", () => {
	test("accepts the publish lane provenance and v2 lock", () => {
		expect(validatePin(lock())).toEqual(lock());
	});

	test("rejects a foreign repo or moving tag", () => {
		expect(
			codeOf(() => validatePin(lock({ repo: "ghcr.io/evil/agent" }))),
		).toBe(EXIT.provenance);
		expect(codeOf(() => validatePin(lock({ tag: "latest" })))).toBe(
			EXIT.provenance,
		);
	});

	test("rejects a malformed index digest and v1 lock", () => {
		expect(codeOf(() => validatePin(lock({ digest: "sha256:abc" })))).toBe(
			EXIT.badLock,
		);
		const v1 = {
			repo: AGENT_REPO,
			tag: TAG,
			digest: DIGEST,
			layers: [LAYER_A],
		};
		expect(codeOf(() => validatePin(v1))).toBe(EXIT.badLock);
	});

	test("rejects a missing or extra platform key", () => {
		const valid = lock().platforms;
		const { "linux/arm64": _arm, ...missing } = valid;
		expect(
			codeOf(() =>
				validatePin(lock({ platforms: missing as PinLock["platforms"] })),
			),
		).toBe(EXIT.badLock);
		expect(
			codeOf(() =>
				validatePin(
					lock({
						platforms: {
							...valid,
							"linux/s390x": pinMember(ARM_MANIFEST),
						} as PinLock["platforms"],
					}),
				),
			),
		).toBe(EXIT.badLock);
	});

	test("rejects malformed member manifests and empty or malformed layers", () => {
		expect(
			codeOf(() =>
				validatePin(
					lock({
						platforms: {
							...lock().platforms,
							"linux/amd64": pinMember("sha256:bad"),
						},
					}),
				),
			),
		).toBe(EXIT.badLock);
		expect(
			codeOf(() =>
				validatePin(
					lock({
						platforms: {
							...lock().platforms,
							"linux/arm64": pinMember(ARM_MANIFEST, []),
						},
					}),
				),
			),
		).toBe(EXIT.badLock);
		expect(
			codeOf(() =>
				validatePin(
					lock({
						platforms: {
							...lock().platforms,
							"linux/arm64": pinMember(ARM_MANIFEST, ["sha256:nope"]),
						},
					}),
				),
			),
		).toBe(EXIT.badLock);
	});

	test("returns ordered platform layer sets", () => {
		expect(validatePin(lock()).platforms["linux/amd64"].layers).toEqual([
			LAYER_A,
			LAYER_B,
		]);
	});

	test("rejects non-object values", () => {
		expect(codeOf(() => validatePin([lock()]))).toBe(EXIT.badLock);
		expect(codeOf(() => validatePin(null))).toBe(EXIT.badLock);
	});
});

describe("layerDigests", () => {
	test("refuses wrong manifest and layer media types", () => {
		expect(
			codeOf(() =>
				layerDigests({
					...manifest(),
					mediaType: "application/vnd.oci.image.index.v1+json",
				}),
			),
		).toBe(EXIT.registryFailed);
		expect(
			codeOf(() =>
				layerDigests({
					...manifest(),
					layers: [
						{
							mediaType: "application/vnd.oci.image.layer.v1.tar+zstd",
							digest: LAYER_A,
						},
					],
				}),
			),
		).toBe(EXIT.registryFailed);
	});

	test("refuses empty layers and preserves manifest layer order", () => {
		expect(codeOf(() => layerDigests(manifest([])))).toBe(EXIT.registryFailed);
		expect(layerDigests(manifest([LAYER_B, LAYER_A]))).toEqual([
			LAYER_B,
			LAYER_A,
		]);
	});
});

describe("lock comparison and rendering", () => {
	test("compares both members including layer order", () => {
		expect(locksEqual(lock(), lock())).toBe(true);
		expect(locksEqual(lock(), lock({ digest: ARM_MANIFEST }))).toBe(false);
		expect(
			locksEqual(
				lock(),
				lock({
					platforms: {
						...lock().platforms,
						"linux/arm64": pinMember(ARM_MANIFEST, [LAYER_B, LAYER_A]),
					},
				}),
			),
		).toBe(false);
	});

	test("renders literal v2 bytes with stable key order and round trips", () => {
		const rendered = renderLock(lock());
		expect(rendered).toBe(
			`{\n\t"repo": "${AGENT_REPO}",\n\t"tag": "${TAG}",\n\t"digest": "${DIGEST}",\n\t"platforms": {\n\t\t"linux/amd64": {\n\t\t\t"manifest": "${AMD_MANIFEST}",\n\t\t\t"layers": [\n\t\t\t\t"${LAYER_A}",\n\t\t\t\t"${LAYER_B}"\n\t\t\t]\n\t\t},\n\t\t"linux/arm64": {\n\t\t\t"manifest": "${ARM_MANIFEST}",\n\t\t\t"layers": [\n\t\t\t\t"${LAYER_A}",\n\t\t\t\t"${LAYER_B}"\n\t\t\t]\n\t\t}\n\t}\n}\n`,
		);
		expect(validatePin(JSON.parse(rendered))).toEqual(lock());
		expect(digestRef(lock())).toBe(`${AGENT_REPO}@${DIGEST}`);
	});
});

describe("digest and tag predicates", () => {
	test("requires full lowercase sha256 digests", () => {
		expect(isImageDigest(DIGEST)).toBe(true);
		expect(isImageDigest(DIGEST.slice(0, 20))).toBe(false);
		expect(isImageDigest(DIGEST.toUpperCase())).toBe(false);
		expect(isImageDigest(undefined)).toBe(false);
	});

	test("accepts only the immutable publish tag shape", () => {
		expect(isBuildTag(TAG)).toBe(true);
		expect(isBuildTag("latest")).toBe(false);
		expect(isBuildTag("git-ec4954bd940")).toBe(false);
		expect(isBuildTag(`${TAG}-dirty`)).toBe(false);
	});
});
