import { afterEach, beforeEach, describe, expect, test } from "bun:test";
import { spawnSync } from "node:child_process";
import { createHash } from "node:crypto";
import {
	chmodSync,
	existsSync,
	mkdirSync,
	mkdtempSync,
	readFileSync,
	rmSync,
	writeFileSync,
} from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { EXIT } from "./core.ts";

const cli = join(import.meta.dir, "publish.ts");
const REPO = "ghcr.io/rigelbuild/compass-gateway";
const SHA = "0123456789ab";
const MANIFEST =
	'{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json"}';
const MANIFEST_DIGEST = `sha256:${createHash("sha256").update(MANIFEST).digest("hex")}`;
const IMPOSTOR = '{"schemaVersion":2,"impostor":true}';

let sandbox = "";
let binDir = "";
let outDir = "";
let logPath = "";

function stubSkopeo(body: string): void {
	writeFileSync(
		join(binDir, "skopeo"),
		`#!/usr/bin/env bash\nprintf '%s\\n' "$*" >> "${logPath}"\n${body}\n`,
	);
	chmodSync(join(binDir, "skopeo"), 0o755);
}

function publish(sha = SHA, env: Readonly<Record<string, string>> = {}) {
	const childEnv: NodeJS.ProcessEnv = {
		...process.env,
		PATH: `${binDir}:${process.env.PATH}`,
		...env,
	};
	if (!("REGISTRY_AUTH_FILE" in env)) delete childEnv.REGISTRY_AUTH_FILE;
	const result = spawnSync(
		"bun",
		[cli, "--repo", REPO, "--sha", sha, "--out", outDir],
		{ encoding: "utf8", env: childEnv },
	);
	return {
		code: result.status ?? -1,
		stdout: result.stdout ?? "",
		stderr: result.stderr ?? "",
	};
}

beforeEach(() => {
	sandbox = mkdtempSync(join(tmpdir(), "gateway-publish-"));
	binDir = join(sandbox, "bin");
	outDir = join(sandbox, "out");
	logPath = join(sandbox, "skopeo.log");
	mkdirSync(binDir, { recursive: true });
	mkdirSync(join(outDir, "oci"), { recursive: true });
	writeFileSync(
		join(outDir, "oci", "index.json"),
		JSON.stringify({
			schemaVersion: 2,
			manifests: [{ digest: MANIFEST_DIGEST }],
		}),
	);
	writeFileSync(join(sandbox, "manifest"), MANIFEST);
	writeFileSync(join(sandbox, "impostor"), IMPOSTOR);
});

afterEach(() => {
	rmSync(sandbox, { recursive: true, force: true });
});

describe("publish contract", () => {
	test("copies the OCI layout preserving digests and prints its verified ref", () => {
		stubSkopeo(`if [ "$1" = inspect ]; then
  if [ -f "${join(sandbox, "copied")}" ]; then cat "${join(sandbox, "manifest")}"; exit 0; fi
  echo "manifest unknown" >&2; exit 1
fi
if [ "$1" = copy ]; then touch "${join(sandbox, "copied")}"; exit 0; fi
exit 9`);
		const { code, stdout } = publish();
		expect(code).toBe(0);
		expect(stdout).toBe(`${REPO}@${MANIFEST_DIGEST}\n`);
		const calls = readFileSync(logPath, "utf8");
		expect(calls).toContain(
			`copy --src-no-creds --preserve-digests oci:${join(outDir, "oci")} docker://${REPO}:git-${SHA}`,
		);
		expect(calls.match(/inspect/g)).toHaveLength(2);
	});
	test("exits badLayout without calling skopeo when the index is missing", () => {
		stubSkopeo("exit 9");
		rmSync(join(outDir, "oci", "index.json"));
		expect(publish().code).toBe(EXIT.badLayout);
		expect(existsSync(logPath)).toBe(false);
	});

	test("exits badLayout without calling skopeo when the index is invalid", () => {
		stubSkopeo("exit 9");
		writeFileSync(join(outDir, "oci", "index.json"), "not-json");
		expect(publish().code).toBe(EXIT.badLayout);
		expect(existsSync(logPath)).toBe(false);
	});

	test("skips when the tag already holds the layout digest", () => {
		stubSkopeo(`if [ "$1" = inspect ]; then cat "${join(sandbox, "manifest")}"; exit 0; fi
exit 9`);
		const { code, stdout } = publish();
		expect(code).toBe(0);
		expect(stdout).toBe(`${REPO}@${MANIFEST_DIGEST}\n`);
		expect(readFileSync(logPath, "utf8")).not.toContain("copy");
	});

	test("aborts without copying when the tag holds a different digest", () => {
		stubSkopeo(`if [ "$1" = inspect ]; then cat "${join(sandbox, "impostor")}"; exit 0; fi
exit 9`);
		const { code } = publish();
		expect(code).toBe(EXIT.pushFailed);
		expect(readFileSync(logPath, "utf8")).not.toContain("copy");
	});

	test("aborts without copying after an ambiguous probe failure", () => {
		stubSkopeo('echo "i/o timeout" >&2; exit 1');
		const { code } = publish();
		expect(code).toBe(EXIT.pushFailed);
		expect(readFileSync(logPath, "utf8")).not.toContain("copy");
	});

	test("maps a failed copy to exit 4", () => {
		stubSkopeo(`if [ "$1" = inspect ]; then echo "manifest unknown" >&2; exit 1; fi
if [ "$1" = copy ]; then echo "denied" >&2; exit 1; fi
exit 9`);
		expect(publish().code).toBe(EXIT.pushFailed);
	});

	test("maps a post-copy digest mismatch to exit 5", () => {
		stubSkopeo(`if [ "$1" = inspect ]; then
  if [ -f "${join(sandbox, "copied")}" ]; then cat "${join(sandbox, "impostor")}"; exit 0; fi
  echo "manifest unknown" >&2; exit 1
fi
if [ "$1" = copy ]; then touch "${join(sandbox, "copied")}"; exit 0; fi
exit 9`);
		expect(publish().code).toBe(EXIT.digestMismatch);
	});
	test("maps a failed post-copy inspect to exit 4", () => {
		stubSkopeo(`if [ "$1" = inspect ]; then
  if [ -f "${join(sandbox, "copied")}" ]; then echo "i/o timeout" >&2; exit 1; fi
  echo "manifest unknown" >&2; exit 1
fi
if [ "$1" = copy ]; then touch "${join(sandbox, "copied")}"; exit 0; fi
exit 9`);
		expect(publish().code).toBe(EXIT.pushFailed);
	});

	test("passes destination auth to probe and copy, then re-reads anonymously", () => {
		stubSkopeo(`if [ "$1" = inspect ]; then
  if [ -f "${join(sandbox, "copied")}" ]; then cat "${join(sandbox, "manifest")}"; exit 0; fi
  echo "manifest unknown" >&2; exit 1
fi
if [ "$1" = copy ]; then touch "${join(sandbox, "copied")}"; exit 0; fi
exit 9`);
		const { code } = publish(SHA, {
			REGISTRY_AUTH_FILE: "/tmp/registry-auth.json",
		});
		expect(code).toBe(0);
		expect(readFileSync(logPath, "utf8").trim().split("\n")).toEqual([
			`inspect --authfile /tmp/registry-auth.json --raw docker://${REPO}:git-${SHA}`,
			`copy --src-no-creds --dest-authfile /tmp/registry-auth.json --preserve-digests oci:${join(outDir, "oci")} docker://${REPO}:git-${SHA}`,
			`inspect --no-creds --raw docker://${REPO}:git-${SHA}`,
		]);
	});

	test("rejects a sha that is not 12 lowercase hex characters", () => {
		stubSkopeo("exit 9");
		expect(publish("0123456789aB").code).toBe(EXIT.usage);
	});
});
