import { describe, expect, test } from "bun:test";
import {
	BASE_CONTEXT,
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
const pin = (fields: Record<string, unknown>) => JSON.stringify(fields);
const sha256 = (text: string) =>
	`sha256:${new Bun.CryptoHasher("sha256").update(text).digest("hex")}`;

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
		expect(() => parseForkPin(pin(fields))).toThrow();
	});

	test("rejects a non-object", () => {
		expect(() => parseForkPin("[]")).toThrow();
	});
});

describe("buildctl argv", () => {
	test("stage 1 targets pi-runtime with reproducible OCI output", () => {
		const argv = baseBuildArgs("/src", "/base");
		expect(argv).toContain("target=pi-runtime");
		expect(argv).toContain("build-arg:SOURCE_DATE_EPOCH=1");
		expect(argv).toContain(
			"type=oci,dest=/base,tar=false,rewrite-timestamp=true",
		);
	});

	test("stage 2 binds the stage-1 layout as the PI_BASE context by digest", () => {
		const argv = gatewayBuildArgs(
			"/src",
			"/base",
			DIGEST,
			"/oci",
			"/meta.json",
		);
		expect(argv).toContain("filename=Dockerfile.gateway");
		expect(argv).toContain("pibase=/base");
		expect(argv).toContain(
			`context:${BASE_CONTEXT}=oci-layout://pibase@${DIGEST}`,
		);
		expect(argv).toContain(
			"type=oci,dest=/oci,tar=false,rewrite-timestamp=true,oci-mediatypes=true",
		);
		expect(argv.slice(-2)).toEqual(["--metadata-file", "/meta.json"]);
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

	test("flags a name-only env entry, which inherits a value at runtime", () => {
		expect(scan(["API_KEY"])).toEqual(["API_KEY"]);
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
	const manifest = '{"schemaVersion":2}';

	test("publishes when the registry says the tag is unknown", () => {
		const probe = { exitCode: 1, stdout: "", stderr: "manifest unknown" };
		expect(tagDisposition(probe, DIGEST).action).toBe("publish");
	});

	test("skips a re-run whose raw manifest has the local digest", () => {
		const probe = { exitCode: 0, stdout: manifest, stderr: "" };
		expect(tagDisposition(probe, sha256(manifest)).action).toBe("skip");
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
