import { afterEach, beforeEach, expect, test } from "bun:test";
import { spawnSync } from "node:child_process";
import { createHash } from "node:crypto";
import {
	chmodSync,
	copyFileSync,
	mkdirSync,
	mkdtempSync,
	readFileSync,
	rmSync,
	writeFileSync,
} from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { EXIT } from "./pin-core.ts";

const cli = join(import.meta.dir, "pin-agent-image.ts");
const core = join(import.meta.dir, "pin-core.ts");
const REPO = "ghcr.io/rigelbuild/compass-agent";
const TAG = "git-0123456789ab";
const LAYER =
	"sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb";

let sandbox = "";
let binDir = "";

const hash = (body: string): string =>
	`sha256:${createHash("sha256").update(body).digest("hex")}`;

const manifest = (layerDigest = LAYER, architecture = "amd64") =>
	JSON.stringify({
		schemaVersion: 2,
		mediaType: "application/vnd.oci.image.manifest.v1+json",
		config: {
			mediaType: "application/vnd.oci.image.config.v1+json",
			digest: LAYER,
			size: 2,
			architecture,
		},
		layers: [
			{
				mediaType: "application/vnd.oci.image.layer.v1.tar+gzip",
				digest: layerDigest,
				size: 10,
			},
		],
	});

const amdManifest = manifest();
const armManifest = manifest(LAYER, "arm64");
const index = (includeArm = true) =>
	JSON.stringify({
		schemaVersion: 2,
		mediaType: "application/vnd.oci.image.index.v1+json",
		manifests: [
			{
				mediaType: "application/vnd.oci.image.manifest.v1+json",
				digest: hash(amdManifest),
				platform: { os: "linux", architecture: "amd64" },
			},
			...(includeArm
				? [
						{
							mediaType: "application/vnd.oci.image.manifest.v1+json",
							digest: hash(armManifest),
							platform: { os: "linux", architecture: "arm64" },
						},
					]
				: []),
		],
	});

function setupRegistry(indexBody: string, wrongMemberBody = false): void {
	const registryData = join(sandbox, "registry.json");
	writeFileSync(
		registryData,
		JSON.stringify({
			index: indexBody,
			amd: wrongMemberBody
				? manifest(
						"sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc",
					)
				: amdManifest,
			arm: armManifest,
		}),
	);
	writeFileSync(
		join(binDir, "skopeo"),
		`#!/usr/bin/env bun
import { readFileSync } from "node:fs";
const args = Bun.argv.slice(2);
if (args[1] !== "--no-creds") {
	console.error("credentials were not disabled");
	process.exit(1);
}
const data = JSON.parse(readFileSync(${JSON.stringify(registryData)}, "utf8"));
const reference = args.at(-1).replace("docker://${REPO}", "");
let output;
if (args.includes("--raw")) {
	if (reference === ":${TAG}" || reference === ":latest") output = data.index;
	else if (reference === "@${hash(amdManifest)}") output = data.amd;
	else if (reference === "@${hash(armManifest)}") output = data.arm;
	else throw new Error("unexpected raw reference " + reference);
} else if (args.includes("--config")) {
	const architecture = reference === "@${hash(amdManifest)}" ? "amd64" : "arm64";
	output = JSON.stringify({ os: "linux", architecture });
} else if (args[0] === "list-tags") {
	output = JSON.stringify({ Tags: ["${TAG}", "latest"] });
} else {
	throw new Error("unexpected skopeo args " + args.join(" "));
}
process.stdout.write(output);
`,
	);
	chmodSync(join(binDir, "skopeo"), 0o755);
}

function runCli(...args: string[]) {
	return spawnSync(
		"bun",
		[join(sandbox, "tools/guest-image/pin-agent-image.ts"), ...args],
		{
			encoding: "utf8",
			env: { ...process.env, PATH: `${binDir}:${process.env.PATH}` },
		},
	);
}
beforeEach(() => {
	sandbox = mkdtempSync(join(tmpdir(), "guest-pin-"));
	binDir = join(sandbox, "bin");
	mkdirSync(join(sandbox, "tools/guest-image"), { recursive: true });
	mkdirSync(join(sandbox, "guest-image"), { recursive: true });
	mkdirSync(binDir, { recursive: true });
	copyFileSync(cli, join(sandbox, "tools/guest-image/pin-agent-image.ts"));
	copyFileSync(core, join(sandbox, "tools/guest-image/pin-core.ts"));
});

afterEach(() => {
	rmSync(sandbox, { recursive: true, force: true });
});

test("--tag writes the expected v2 lock from anonymous index and member reads", () => {
	const indexBody = index();
	setupRegistry(indexBody);
	const result = runCli("--tag", TAG);
	if (result.status !== 0) throw new Error(result.stderr);
	expect(result.status).toBe(0);
	expect(result.stdout).toContain(`pinned ${REPO}:${TAG}`);
	expect(result.stdout).toContain(`index: ${hash(indexBody)}`);
	const written = JSON.parse(
		readFileSync(join(sandbox, "guest-image/agent-oci.lock"), "utf8"),
	);
	expect(written).toEqual({
		repo: REPO,
		tag: TAG,
		digest: hash(indexBody),
		platforms: {
			"linux/amd64": { manifest: hash(amdManifest), layers: [LAYER] },
			"linux/arm64": { manifest: hash(armManifest), layers: [LAYER] },
		},
	});
});

test("member digest mismatch leaves the sandbox lock byte-identical", () => {
	const indexBody = index();
	setupRegistry(indexBody, true);
	const lockPath = join(sandbox, "guest-image/agent-oci.lock");
	const original = '{"sentinel":true}\n';
	writeFileSync(lockPath, original);
	const result = runCli("--tag", TAG);
	expect(result.status).toBe(EXIT.digestMismatch);
	expect(result.stderr).toContain("linux/amd64 member body digest");
	expect(readFileSync(lockPath, "utf8")).toBe(original);
});

test("an index without linux/arm64 fails closed", () => {
	setupRegistry(index(false));
	const result = runCli("--tag", TAG);
	expect(result.status).toBe(EXIT.registryFailed);
	expect(result.stderr).toContain("platform set must contain exactly");
});

test("--relock discovers an immutable tag by raw index-body hash", () => {
	const indexBody = index();
	setupRegistry(indexBody);
	const lockPath = join(sandbox, "guest-image/agent-oci.lock");
	const initial = runCli("--tag", TAG);
	expect(initial.status).toBe(0);
	const relock = runCli("--relock");
	expect(relock.status).toBe(0);
	expect(relock.stdout).toContain("already pins");
	expect(JSON.parse(readFileSync(lockPath, "utf8")).digest).toBe(
		hash(indexBody),
	);
});
