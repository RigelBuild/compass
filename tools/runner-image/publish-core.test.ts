import { describe, expect, test } from "bun:test";
import {
	buildTag,
	digestFromMetadata,
	digestRef,
	isImageDigest,
	secretEnvViolations,
} from "./publish-core.ts";

const DIGEST =
	"sha256:2125c2a158a8329c40ac7c1daa8909f09fe82a5231ff0df05319d0be4d4c4bb6";

describe("secretEnvViolations", () => {
	test("blocks a populated secret-shaped name", () => {
		expect(secretEnvViolations(["GITHUB_TOKEN=ghp_x"])).toEqual([
			"GITHUB_TOKEN",
		]);
	});

	test("blocks a bare name, which inherits the builder's value at runtime", () => {
		expect(secretEnvViolations(["REGISTRY_PASSWORD"])).toEqual([
			"REGISTRY_PASSWORD",
		]);
	});

	test("allows a secret-shaped name explicitly emptied", () => {
		// The image ships the name with no value, so there is nothing to leak.
		expect(secretEnvViolations(["GITHUB_TOKEN="])).toEqual([]);
	});

	test("allows the runner's own config env", () => {
		expect(
			secretEnvViolations([
				"COMPASS_RUNTIME_BACKEND=microvm",
				"COMPASS_MICROVM_KERNEL=/nix/store/x/bzImage",
				"PATH=/nix/store/y/bin",
			]),
		).toEqual([]);
	});

	test("does not fire on a substring inside a larger word", () => {
		// AUTHORITY and TOKENIZER are not credentials; matching bare substrings
		// would make the scan cry wolf and get switched off.
		expect(secretEnvViolations(["AUTHORITY=x", "TOKENIZER=y"])).toEqual([]);
	});

	test("reports every violation, not just the first", () => {
		expect(
			secretEnvViolations(["A_SECRET=1", "PATH=/bin", "B_API_KEY=2"]),
		).toEqual(["A_SECRET", "B_API_KEY"]);
	});
});

describe("isImageDigest", () => {
	test("accepts a full sha256 digest", () => {
		expect(isImageDigest(DIGEST)).toBe(true);
	});

	test("rejects a truncated digest", () => {
		// A prefix-equal comparison would read as a match, so a short form is
		// rejected rather than normalised.
		expect(isImageDigest("sha256:2125c2a1")).toBe(false);
	});

	test("rejects hex with no algorithm and a tag", () => {
		expect(isImageDigest(DIGEST.slice("sha256:".length))).toBe(false);
		expect(isImageDigest("latest")).toBe(false);
	});

	test("rejects uppercase hex, which never round-trips a registry", () => {
		expect(isImageDigest(DIGEST.toUpperCase())).toBe(false);
	});
});

describe("digestRef", () => {
	test("pins by digest, never by tag", () => {
		expect(digestRef("ghcr.io/rigelbuild/compass-runner", DIGEST)).toBe(
			`ghcr.io/rigelbuild/compass-runner@${DIGEST}`,
		);
	});

	test("throws on anything that is not a digest", () => {
		expect(() => digestRef("ghcr.io/x/y", "latest")).toThrow(
			"not a sha256 digest",
		);
	});
});

describe("buildTag", () => {
	test("builds the git-<sha12> addressability tag", () => {
		expect(buildTag("ghcr.io/x/y", "c724a9ee03a8")).toBe(
			"ghcr.io/x/y:git-c724a9ee03a8",
		);
	});

	test("rejects a short or non-hex sha", () => {
		expect(() => buildTag("ghcr.io/x/y", "c724a9ee")).toThrow(
			"not a 12-char commit sha",
		);
		expect(() => buildTag("ghcr.io/x/y", "HEAD~1")).toThrow(
			"not a 12-char commit sha",
		);
	});
});

describe("digestFromMetadata", () => {
	test("reads the exporter's containerimage.digest", () => {
		expect(digestFromMetadata({ "containerimage.digest": DIGEST })).toBe(
			DIGEST,
		);
	});

	test("returns undefined when the key is absent", () => {
		// A missing digest must not read as a successful push.
		expect(digestFromMetadata({ "image.name": "ghcr.io/x/y:tag" })).toBe(
			undefined,
		);
	});

	test("returns undefined for a malformed digest value", () => {
		expect(digestFromMetadata({ "containerimage.digest": "sha256:xyz" })).toBe(
			undefined,
		);
	});

	test("returns undefined for non-object metadata", () => {
		expect(digestFromMetadata(null)).toBe(undefined);
		expect(digestFromMetadata("sha256:…")).toBe(undefined);
	});
});
