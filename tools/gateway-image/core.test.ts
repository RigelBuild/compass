import { describe, expect, test } from "bun:test";
import {
	baseBuildArgs,
	buildTag,
	digestRef,
	FORK_REPO,
	gatewayBuildArgs,
	healthzOk,
	layoutDigest,
	modelsListOk,
	parseForkPin,
	secretConfigViolations,
	tagDisposition,
} from "./core.ts";

const COMMIT = "76015c2d80691c8a9f46fb84a00cad652d4455c6";
const DIGEST = `sha256:${"a".repeat(64)}`;

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
			"--opt",
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

	test("passes the env of the built gateway image", () => {
		const env = [
			"LANG=C.UTF-8",
			"GPG_KEY=7169605F62C751356D054A26A821E680E5FA6305",
			"PYTHON_SHA256=5c8462af",
			"PATH=/opt/bun/bin:/usr/bin:/bin",
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
	// Non-canonical raw bytes and their independently computed sha256: a
	// parse-and-reserialize regression hashes different bytes.
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
