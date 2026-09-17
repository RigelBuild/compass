import { describe, expect, test } from "bun:test";
import {
	AGENT_DIGEST_ANNOTATION,
	ARTIFACT_TYPE,
	annotationViolations,
	buildTag,
	digestRef,
	EMPTY_CONFIG_MEDIA_TYPE,
	EXIT,
	layoutPlan,
	manifestDigest,
	tagDisposition,
} from "./publish-core.ts";

const hex = (c: string) => c.repeat(64);
const assets = {
	kernel: { digest: `sha256:${hex("1")}`, size: 11_534_336 },
	rootfs: { digest: `sha256:${hex("2")}`, size: 2_264_924_160 },
	initrd: { digest: `sha256:${hex("3")}`, size: 9_437_184 },
} as const;
const provenance = {
	revision: "d3adb33fd3ad",
	agentImageDigest: `sha256:${hex("a")}`,
};
const plan = () => layoutPlan(assets, provenance);

describe("layoutPlan", () => {
	test("marks the artifact non-runnable: artifactType plus the empty config", () => {
		const manifest = JSON.parse(plan().manifest);
		expect(manifest.artifactType).toBe(ARTIFACT_TYPE);
		expect(manifest.config.mediaType).toBe(EMPTY_CONFIG_MEDIA_TYPE);
		// The two-byte `{}` blob, so nothing can resolve a runnable config.
		expect(manifest.config.size).toBe(2);
		expect(manifest.config.digest).toBe(manifestDigest("{}"));
	});

	test("carries schemaVersion 2, which a conformant registry requires", () => {
		expect(JSON.parse(plan().manifest).schemaVersion).toBe(2);
		expect(JSON.parse(plan().index).schemaVersion).toBe(2);
	});

	test("every layer descriptor declares the asset's real byte length", () => {
		expect(
			JSON.parse(plan().manifest).layers.map((l: { size: number }) => l.size),
		).toEqual([11_534_336, 2_264_924_160, 9_437_184]);
	});

	test("layers are kernel, rootfs, initrd in that order with their own media types", () => {
		expect(
			JSON.parse(plan().manifest).layers.map(
				(l: { mediaType: string }) => l.mediaType,
			),
		).toEqual([
			"application/vnd.compass.guest-kernel.v1",
			"application/vnd.compass.guest-rootfs.v1+erofs",
			"application/vnd.compass.guest-initrd.v1+cpio.zst",
		]);
	});

	test("annotations key on the materialised FILENAME, never a hash-prefixed store basename", () => {
		const { annotations } = plan();
		expect(
			annotations["org.compass.guest.layer.compass-guest-rootfs.erofs"],
		).toBe(hex("2"));
		expect(annotations["org.compass.guest.layer.bzImage"]).toBe(hex("1"));
		expect(annotations["org.compass.guest.layer.compass-guest-initrd"]).toBe(
			hex("3"),
		);
		// A store basename would carry a content hash and never match
		// verifyImages' filepath.Base lookup.
		expect(
			Object.keys(annotations).some((k) => /layer\.[a-z0-9]{32}-/.test(k)),
		).toBe(false);
		expect(annotations[AGENT_DIGEST_ANNOTATION]).toBe(`sha256:${hex("a")}`);
		expect(annotations["org.opencontainers.image.revision"]).toBe(
			"d3adb33fd3ad",
		);
	});

	test("the index descriptor's size and digest match the manifest bytes", () => {
		const built = plan();
		const entry = JSON.parse(built.index).manifests[0];
		expect(entry.digest).toBe(manifestDigest(built.manifest));
		expect(entry.size).toBe(
			new TextEncoder().encode(built.manifest).byteLength,
		);
	});

	test.each([
		[
			"a tag instead of a digest",
			{ ...assets, rootfs: { ...assets.rootfs, digest: "latest" } },
		],
		[
			"a truncated digest",
			{ ...assets, rootfs: { ...assets.rootfs, digest: "sha256:abc" } },
		],
		["a zero size", { ...assets, kernel: { ...assets.kernel, size: 0 } }],
		["a negative size", { ...assets, kernel: { ...assets.kernel, size: -1 } }],
		[
			"a fractional size",
			{ ...assets, initrd: { ...assets.initrd, size: 1.5 } },
		],
	])(
		"rejects %s rather than publishing a manifest a registry will refuse",
		(_label, broken) => {
			expect(() => layoutPlan(broken, provenance)).toThrow();
		},
	);

	test("rejects provenance that would publish an unusable pin", () => {
		expect(() =>
			layoutPlan(assets, { ...provenance, agentImageDigest: "git-abc" }),
		).toThrow();
		expect(() => layoutPlan(assets, { ...provenance, revision: "" })).toThrow();
	});
});

describe("manifestDigest", () => {
	test("hashes the raw bytes, matching the registry's manifest identity", () => {
		expect(manifestDigest("{}")).toBe(
			"sha256:44136fa355b3678a1146ad16f7e8649e94fb4fc21fe77e8310c060f61caaff8a",
		);
	});

	test("a one-byte manifest change moves the digest", () => {
		expect(manifestDigest('{"a":1}')).not.toBe(manifestDigest('{"a":2}'));
	});
});

describe("annotationViolations", () => {
	test("blocks secret-shaped names and values", () => {
		expect(
			annotationViolations({ GITHUB_TOKEN: "x", note: "DEPLOY_KEY" }),
		).toEqual(["GITHUB_TOKEN", "note"]);
	});

	test("the real provenance annotation set is clean", () => {
		expect(annotationViolations(plan().annotations)).toEqual([]);
	});

	test("word boundaries keep PATH and KEYBOARD from firing", () => {
		expect(annotationViolations({ PATH: "/usr/bin", KEYBOARD: "us" })).toEqual(
			[],
		);
	});
});

describe("tagDisposition", () => {
	const local = `sha256:${hex("b")}`;

	test("an absent tag publishes", () => {
		expect(
			tagDisposition(
				{ exitCode: 1, stdout: "", stderr: "manifest unknown" },
				local,
			).action,
		).toBe("publish");
	});

	test("a tag already holding this exact manifest is an idempotent skip", () => {
		const body = '{"schemaVersion":2}';
		expect(
			tagDisposition(
				{ exitCode: 0, stdout: body, stderr: "" },
				manifestDigest(body),
			).action,
		).toBe("skip");
	});

	test("a tag holding different content aborts rather than overwriting", () => {
		expect(
			tagDisposition(
				{ exitCode: 0, stdout: '{"other":true}', stderr: "" },
				local,
			).action,
		).toBe("abort");
	});

	test("an unreadable probe failure aborts, so a transient fault cannot overwrite a published artifact", () => {
		const probe = {
			exitCode: 1,
			stdout: "",
			stderr: "i/o timeout talking to registry",
		};
		expect(tagDisposition(probe, local).action).toBe("abort");
	});
});

describe("references", () => {
	test("digestRef pins by digest and buildTag only addresses the build", () => {
		expect(digestRef("ghcr.io/x/y", `sha256:${hex("c")}`)).toBe(
			`ghcr.io/x/y@sha256:${hex("c")}`,
		);
		expect(buildTag("ghcr.io/x/y", "0123456789ab")).toBe(
			"ghcr.io/x/y:git-0123456789ab",
		);
	});

	test("rejects a tag-shaped digest and a non-12-hex sha", () => {
		expect(() => digestRef("ghcr.io/x/y", "latest")).toThrow();
		expect(() => buildTag("ghcr.io/x/y", "not-hex")).toThrow();
	});
});

test("exit codes distinguish usage, secret, transport, mismatch, and layout faults", () => {
	expect(EXIT).toEqual({
		usage: 2,
		secretFound: 3,
		pushFailed: 4,
		digestMismatch: 5,
		badLayout: 6,
	});
});
