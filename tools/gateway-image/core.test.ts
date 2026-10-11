import { describe, expect, test } from "bun:test";
import {
	baseBuildArgs,
	blobPath,
	buildTag,
	digestRef,
	FORK_BRANCH,
	FORK_REPO,
	gatewayBuildArgs,
	healthzOk,
	imageConfigEntries,
	layoutDigest,
	manifestConfigDigest,
	metadataDigest,
	modelsListOk,
	parseForkPin,
	secretConfigViolations,
	tagDisposition,
} from "./core.ts";

const COMMIT = "76015c2d80691c8a9f46fb84a00cad652d4455c6";
const DIGEST = `sha256:${"a".repeat(64)}`;

test("uses the protected fork release branch", () => {
	expect(FORK_BRANCH).toBe("rigel-release");
});

describe("parseForkPin", () => {
	test("accepts the committed pin file", async () => {
		const text = await Bun.file(
			new URL("./fork-pin.json", import.meta.url),
		).text();
		expect(parseForkPin(text).repo).toBe(FORK_REPO);
	});

	test.each([
		["an unknown key", { repo: FORK_REPO, commit: COMMIT, ref: "main" }],
		["an inherited key name", { repo: FORK_REPO, commit: COMMIT, toString: 1 }],
		[
			"a foreign repo",
			{ repo: "https://github.com/can1357/oh-my-pi.git", commit: COMMIT },
		],
		["a missing repo", { commit: COMMIT }],
		["a short commit", { repo: FORK_REPO, commit: COMMIT.slice(0, 12) }],
		["an uppercase commit", { repo: FORK_REPO, commit: COMMIT.toUpperCase() }],
		["a branch name", { repo: FORK_REPO, commit: "main" }],
		["a missing commit", { repo: FORK_REPO }],
	])("rejects %s", (_, fields) => {
		expect(() => parseForkPin(JSON.stringify(fields))).toThrow();
	});

	test("rejects a non-object", () => {
		expect(() => parseForkPin("[]")).toThrow();
	});
});

describe("buildctl argv", () => {
	test("stage 1 builds pi-runtime for amd64 with reproducible OCI output", () => {
		expect(baseBuildArgs("/src", "/base")).toEqual([
			"build",
			"--frontend",
			"dockerfile.v0",
			"--local",
			"context=/src",
			"--local",
			"dockerfile=/src",
			"--opt",
			"filename=Dockerfile",
			"--opt",
			"target=pi-runtime",
			"--opt",
			"platform=linux/amd64",
			"--opt",
			"build-arg:SOURCE_DATE_EPOCH=1",
			"--output",
			"type=oci,dest=/base,tar=false,rewrite-timestamp=true",
		]);
	});

	test("stage 2 binds the stage-1 layout as oh-my-pi/pi:dev by digest", () => {
		expect(
			gatewayBuildArgs("/src", "/base", DIGEST, "/oci", "/meta.json"),
		).toEqual([
			"build",
			"--frontend",
			"dockerfile.v0",
			"--local",
			"context=/src",
			"--local",
			"dockerfile=/src",
			"--oci-layout",
			"pibase=/base",
			"--opt",
			"filename=Dockerfile.gateway",
			`--opt`,
			`context:oh-my-pi/pi:dev=oci-layout://pibase@${DIGEST}`,
			"--opt",
			"platform=linux/amd64",
			"--opt",
			"build-arg:SOURCE_DATE_EPOCH=1",
			"--output",
			"type=oci,dest=/oci,tar=false,rewrite-timestamp=true,oci-mediatypes=true",
			"--metadata-file",
			"/meta.json",
		]);
	});

	test("stage 2 refuses a base that is not a digest", () => {
		expect(() =>
			gatewayBuildArgs("/src", "/base", "latest", "/oci", "/m"),
		).toThrow();
	});
});

describe("layoutDigest", () => {
	test("returns the one manifest digest", () => {
		expect(
			layoutDigest(JSON.stringify({ manifests: [{ digest: DIGEST }] })),
		).toBe(DIGEST);
	});

	test.each([
		["no manifests", {}],
		["zero manifests", { manifests: [] }],
		["two manifests", { manifests: [{ digest: DIGEST }, { digest: DIGEST }] }],
		["a non-sha256 digest", { manifests: [{ digest: "sha512:abc" }] }],
	])("rejects %s", (_, index) => {
		expect(() => layoutDigest(JSON.stringify(index))).toThrow();
	});
});

describe("metadataDigest", () => {
	test("reads the BuildKit containerimage.digest field", () => {
		expect(
			metadataDigest(JSON.stringify({ "containerimage.digest": DIGEST })),
		).toBe(DIGEST);
	});

	test.each([
		["a non-object metadata", "[]"],
		["a missing digest", "{}"],
		["a non-string digest", '{"containerimage.digest":7}'],
		["an invalid digest", '{"containerimage.digest":"sha512:abc"}'],
	])("rejects %s", (_, metadata) => {
		expect(() => metadataDigest(metadata)).toThrow();
	});
});

describe("manifestConfigDigest", () => {
	const descriptor = (digest: unknown) => ({
		mediaType: "application/vnd.oci.image.config.v1+json",
		size: 1,
		digest,
	});
	const manifest = (config: unknown, layers: unknown[] = []) =>
		JSON.stringify({
			schemaVersion: 2,
			mediaType: "application/vnd.oci.image.manifest.v1+json",
			config,
			layers,
		});

	test("reads the OCI config descriptor digest", () => {
		expect(manifestConfigDigest(manifest(descriptor(DIGEST)))).toBe(DIGEST);
	});

	test.each([
		["a non-object manifest", "[]"],
		["a missing config descriptor", manifest(undefined)],
		["a non-object config descriptor", manifest(null)],
		[
			"a missing digest",
			manifest({
				mediaType: "application/vnd.oci.image.config.v1+json",
				size: 1,
			}),
		],
		["a non-string digest", manifest(descriptor(7))],
		["an invalid digest", manifest(descriptor("sha512:abc"))],
		["an invalid layer descriptor", manifest(descriptor(DIGEST), [{}])],
		[
			"an invalid manifest shape",
			JSON.stringify({ schemaVersion: 2, config: descriptor(DIGEST) }),
		],
	])("rejects %s", (_, value) => {
		expect(() => manifestConfigDigest(value)).toThrow();
	});
});

describe("imageConfigEntries", () => {
	test("returns env and labels from image config", () => {
		expect(
			imageConfigEntries(
				JSON.stringify({
					config: { Env: ["PATH=/bin"], Labels: { version: "1" } },
				}),
			),
		).toEqual({ env: ["PATH=/bin"], labels: { version: "1" } });
	});

	test("defaults absent env and labels to empty", () => {
		expect(imageConfigEntries('{"config":{}}')).toEqual({
			env: [],
			labels: {},
		});
	});

	test.each([
		["a non-object document", "[]"],
		["a missing config", "{}"],
		["a non-object config", '{"config":[]}'],
		["a non-array Env", '{"config":{"Env":"PATH=/bin"}}'],
		["a non-string Env entry", '{"config":{"Env":[1]}}'],
		["a non-object Labels", '{"config":{"Labels":[]}}'],
		["a non-string label value", '{"config":{"Labels":{"version":1}}}'],
	])("rejects %s", (_, config) => {
		expect(() => imageConfigEntries(config)).toThrow();
	});
});
describe("blobPath", () => {
	test("returns the sha256 OCI blob path", () => {
		expect(blobPath("/out/oci", DIGEST)).toBe(
			`/out/oci/blobs/sha256/${"a".repeat(64)}`,
		);
	});

	test.each(["sha512:abc", `sha256:${"A".repeat(64)}`, "sha256:short"])(
		"rejects invalid digest %s",
		(digest) => expect(() => blobPath("/out/oci", digest)).toThrow(),
	);
});

describe("secretConfigViolations", () => {
	const scan = (env: string[], labels: Record<string, string> = {}) =>
		secretConfigViolations({ env, labels });

	test("passes the allowlisted names the gateway config ships", () => {
		expect(
			scan([
				"GPG_KEY=A035C8C1",
				"COMPASS_GATEWAY_TOKEN_FILE=/run/token",
				"PATH=/bin",
			]),
		).toEqual([]);
	});

	test("still flags a real secret name", () => {
		expect(scan(["OMP_AUTH_BROKER_TOKEN=abc"])).toEqual([
			"OMP_AUTH_BROKER_TOKEN",
		]);
	});

	test("allows only an exact allowlist match", () => {
		expect(scan(["GPG_KEY_X=1", "COMPASS_GATEWAY_TOKEN_FILE_2=/x"])).toEqual([
			"GPG_KEY_X",
			"COMPASS_GATEWAY_TOKEN_FILE_2",
		]);
	});

	test("passes an empty value but flags a name-only entry", () => {
		expect(scan(["API_KEY="])).toEqual([]);
		expect(scan(["API_KEY"])).toEqual(["API_KEY"]);
	});

	test("applies the exact-match allowlist to label keys and values", () => {
		expect(
			scan([], { GPG_KEY: "x", note: "COMPASS_GATEWAY_TOKEN_FILE" }),
		).toEqual([]);
		expect(scan([], { GPG_KEY_X: "x" })).toEqual(["GPG_KEY_X"]);
	});

	test("flags dotted and dashed label keys", () => {
		expect(
			scan([], {
				"org.example.registry-token": "x",
				"org.example.api.key": "y",
			}),
		).toEqual(["org.example.registry-token", "org.example.api.key"]);
	});

	test("passes every env name of the built gateway image", () => {
		const env = [
			"LANG=C.UTF-8",
			"GPG_KEY=7169605F62C751356D054A26A821E680E5FA6305",
			"PYTHON_VERSION=3.12.14",
			"PYTHON_SHA256=5c8462af5790baf43a321a1559dbe0db06d1be4300fb85fb53c40060668e548a",
			"PYTHONDONTWRITEBYTECODE=1",
			"PYTHONUNBUFFERED=1",
			"PIP_NO_CACHE_DIR=1",
			"PIP_DISABLE_PIP_VERSION_CHECK=1",
			"BUN_INSTALL=/opt/bun",
			"CARGO_HOME=/data/cache/cargo",
			"CARGO_TARGET_DIR=/data/cache/cargo-target",
			"RUSTUP_HOME=/data/cache/rustup",
			"PATH=/opt/bun/bin:/usr/local/cargo/bin:/usr/local/bin:/usr/bin:/bin",
			"PI_ROOT=/pi",
			"HOME=/tmp",
			"COMPASS_GATEWAY_TOKEN_FILE=/run/compass/gateway.token",
			"COMPASS_GATEWAY_BIND=0.0.0.0:4000",
			"COMPASS_GATEWAY_DRAIN_MS=20000",
		];
		expect(scan(env, { "io.buildah.version": "1.43.2" })).toEqual([]);
	});

	test("flags a label whose value names a secret", () => {
		expect(scan([], { note: "GITHUB_TOKEN" })).toEqual(["note"]);
	});
});

describe("tags and refs", () => {
	test("builds the git-<sha12> tag and the digest ref", () => {
		expect(buildTag("ghcr.io/r/g", "0123456789ab")).toBe(
			"ghcr.io/r/g:git-0123456789ab",
		);
		expect(digestRef("ghcr.io/r/g", DIGEST)).toBe(`ghcr.io/r/g@${DIGEST}`);
	});

	test("rejects a short sha and a tag-shaped digest", () => {
		expect(() => buildTag("r", "0123456")).toThrow();
		expect(() => digestRef("r", "git-0123456789ab")).toThrow();
	});
});

describe("tagDisposition", () => {
	const manifest = '{\n  "schemaVersion": 2 }';
	const manifestDigest =
		"sha256:e36b365ae05ced79e2d32ee8d290cc999fe07aff11ce21bb2caf1f69428e93f9";

	test.each(["manifest unknown", "name unknown", "manifest not found"])(
		"publishes when the registry answers %s",
		(stderr) => {
			expect(
				tagDisposition({ exitCode: 1, stdout: "", stderr }, DIGEST).action,
			).toBe("publish");
		},
	);

	test("skips a re-run whose raw manifest bytes hash to the local digest", () => {
		const probe = { exitCode: 0, stdout: manifest, stderr: "" };
		expect(tagDisposition(probe, manifestDigest).action).toBe("skip");
	});

	test("aborts when the tag holds a different digest", () => {
		const probe = { exitCode: 0, stdout: manifest, stderr: "" };
		expect(tagDisposition(probe, DIGEST).action).toBe("abort");
	});

	test("aborts on an ambiguous probe failure", () => {
		const probe = {
			exitCode: 1,
			stdout: "",
			stderr: "unauthorized: authentication required",
		};
		expect(tagDisposition(probe, DIGEST).action).toBe("abort");
	});
});

describe("smoke predicates", () => {
	test("healthz needs 200 and ok:true", () => {
		expect(healthzOk(200, '{"ok":true}')).toBe(true);
		expect(healthzOk(200, '{"ok":false}')).toBe(false);
		expect(healthzOk(503, '{"ok":true}')).toBe(false);
		expect(healthzOk(200, "ok")).toBe(false);
	});

	test("models needs 200 and object:list", () => {
		expect(modelsListOk(200, '{"object":"list","data":[]}')).toBe(true);
		expect(modelsListOk(401, '{"object":"list"}')).toBe(false);
		expect(modelsListOk(200, '{"error":"unauthorized"}')).toBe(false);
	});
});
