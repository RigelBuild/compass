import { describe, expect, test } from "bun:test";
import {
	buildTag,
	digestFromMetadata,
	digestRef,
	EXIT,
	isImageDigest,
	type PublishIO,
	publishVerified,
	secretConfigViolations,
	tagArgs,
} from "./publish-core.ts";

const DIGEST =
	"sha256:2125c2a158a8329c40ac7c1daa8909f09fe82a5231ff0df05319d0be4d4c4bb6";

describe("secretConfigViolations", () => {
	const noLabels = { labels: {} as Record<string, string> };

	test("blocks a populated secret-shaped name", () => {
		expect(
			secretConfigViolations({ env: ["GITHUB_TOKEN=ghp_x"], ...noLabels }),
		).toEqual(["GITHUB_TOKEN"]);
	});

	test("blocks a bare name, which inherits the builder's value at runtime", () => {
		expect(
			secretConfigViolations({ env: ["REGISTRY_PASSWORD"], ...noLabels }),
		).toEqual(["REGISTRY_PASSWORD"]);
	});

	test("allows a secret-shaped name explicitly emptied", () => {
		// The image ships the name with no value, so there is nothing to leak.
		expect(
			secretConfigViolations({ env: ["GITHUB_TOKEN="], ...noLabels }),
		).toEqual([]);
	});

	test("allows the runner's own config env", () => {
		expect(
			secretConfigViolations({
				env: [
					"COMPASS_RUNTIME_BACKEND=microvm",
					"COMPASS_MICROVM_KERNEL=/nix/store/x/bzImage",
					"PATH=/nix/store/y/bin",
				],
				...noLabels,
			}),
		).toEqual([]);
	});

	test("does not fire on a substring inside a larger word", () => {
		// AUTHORITY and TOKENIZER are not credentials; matching bare substrings
		// would make the scan cry wolf and get switched off.
		expect(
			secretConfigViolations({
				env: ["AUTHORITY=x", "TOKENIZER=y"],
				...noLabels,
			}),
		).toEqual([]);
	});

	test("catches the broadened keyword forms KEY, PASSPHRASE, BEARER", () => {
		expect(
			secretConfigViolations({
				env: ["SSH_KEY=x", "DEPLOY_KEY=y", "PASSPHRASE=z", "BEARER_TOKEN=b"],
				...noLabels,
			}),
		).toEqual(["SSH_KEY", "DEPLOY_KEY", "PASSPHRASE", "BEARER_TOKEN"]);
	});

	test("matches a *_PAT name but never PATH", () => {
		// PATH is set in essentially every image config, so a bare PAT keyword
		// that fired on it would break every publish — the word boundary must
		// keep PATH out while still catching a real personal-access-token name.
		expect(
			secretConfigViolations({
				env: ["GITHUB_PAT=x", "PATH=/nix/store/y/bin"],
				...noLabels,
			}),
		).toEqual(["GITHUB_PAT"]);
	});

	test("reports every violation, not just the first", () => {
		expect(
			secretConfigViolations({
				env: ["A_SECRET=1", "PATH=/bin", "B_API_KEY=2"],
				...noLabels,
			}),
		).toEqual(["A_SECRET", "B_API_KEY"]);
	});

	test("scans label keys and values, not just env", () => {
		expect(
			secretConfigViolations({
				env: [],
				labels: {
					"org.opencontainers.image.source": "https://example.test/repo",
					deploy_token: "abc",
					note: "value is a GITHUB_TOKEN",
				},
			}),
		).toEqual(["deploy_token", "note"]);
	});

	test("allows an empty-valued secret-shaped label", () => {
		// A label with no value ships nothing, mirroring the emptied-env case.
		expect(
			secretConfigViolations({ env: [], labels: { api_key: "" } }),
		).toEqual([]);
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

describe("tagArgs", () => {
	test("copies the verified digest to the build tag with explicit auth", () => {
		expect(
			tagArgs("ghcr.io/x/y", DIGEST, "c724a9ee03a8", "/run/docker/config.json"),
		).toEqual([
			"copy",
			"--preserve-digests",
			"--authfile",
			"/run/docker/config.json",
			`docker://ghcr.io/x/y@${DIGEST}`,
			"docker://ghcr.io/x/y:git-c724a9ee03a8",
		]);
	});

	test("refuses a non-digest source, so the tag cannot follow another tag", () => {
		expect(() =>
			tagArgs("ghcr.io/x/y", "latest", "c724a9ee03a8", "/a"),
		).toThrow();
	});
});

describe("publishVerified", () => {
	const OTHER =
		"sha256:3125c2a158a8329c40ac7c1daa8909f09fe82a5231ff0df05319d0be4d4c4bb6";
	const input = {
		repo: "ghcr.io/x/y",
		sha12: "c724a9ee03a8",
		localDigest: DIGEST,
		authFile: "/auth.json",
	};

	/** A fake runner that records the order of registry writes. */
	function fake(
		push: { status: number | null; metadata: unknown },
		skopeoStatus = 0,
	): { io: PublishIO; calls: string[] } {
		const calls: string[] = [];
		return {
			calls,
			io: {
				push: () => {
					calls.push("push");
					return push;
				},
				skopeo: (args) => {
					calls.push(`skopeo ${args.join(" ")}`);
					return { status: skopeoStatus, stderr: "denied: no write\n" };
				},
			},
		};
	}

	test("tags only after the pushed digest matches, and reports the digest ref", () => {
		const { io, calls } = fake({
			status: 0,
			metadata: { "containerimage.digest": DIGEST },
		});
		expect(publishVerified(input, io)).toEqual({
			ok: true,
			ref: `ghcr.io/x/y@${DIGEST}`,
		});
		expect(calls).toEqual([
			"push",
			`skopeo ${tagArgs("ghcr.io/x/y", DIGEST, "c724a9ee03a8", "/auth.json").join(" ")}`,
		]);
	});

	test("a digest mismatch fails and creates no tag", () => {
		// The tag is what retag.ts promotes, so unverified bytes must never get one.
		const { io, calls } = fake({
			status: 0,
			metadata: { "containerimage.digest": OTHER },
		});
		const result = publishVerified(input, io);
		expect(result).toMatchObject({ ok: false, code: EXIT.digestMismatch });
		expect(calls).toEqual(["push"]);
	});

	test("a missing pushed digest fails as a push fault and creates no tag", () => {
		const { io, calls } = fake({ status: 0, metadata: {} });
		expect(publishVerified(input, io)).toMatchObject({
			ok: false,
			code: EXIT.pushFailed,
		});
		expect(calls).toEqual(["push"]);
	});

	test("a failed push fails and creates no tag", () => {
		const { io, calls } = fake({ status: 1, metadata: undefined });
		expect(publishVerified(input, io)).toMatchObject({
			ok: false,
			code: EXIT.pushFailed,
		});
		expect(calls).toEqual(["push"]);
	});

	test("a failed tag copy is a push fault that carries skopeo's stderr", () => {
		const { io } = fake(
			{ status: 0, metadata: { "containerimage.digest": DIGEST } },
			1,
		);
		const result = publishVerified(input, io);
		expect(result).toMatchObject({ ok: false, code: EXIT.pushFailed });
		expect(result.ok ? "" : result.message).toContain("denied: no write");
	});

	test("a bad sha fails before anything reaches the registry", () => {
		const { io, calls } = fake({
			status: 0,
			metadata: { "containerimage.digest": DIGEST },
		});
		expect(() => publishVerified({ ...input, sha12: "abc" }, io)).toThrow();
		expect(calls).toEqual([]);
	});
});
