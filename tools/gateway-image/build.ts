#!/usr/bin/env bun

import { spawnSync } from "node:child_process";
import { mkdirSync, readFileSync, rmSync } from "node:fs";
import { dirname, join, resolve } from "node:path";
import { fileURLToPath } from "node:url";
import { hostPlatform } from "../runner-image/build-core.ts";
import type { ForkPin, ImageConfigEntries } from "./core.ts";
import {
	baseBuildArgs,
	blobPath,
	EXIT,
	FORK_BRANCH,
	gatewayBuildArgs,
	imageConfigEntries,
	layoutDigest,
	manifestConfigDigest,
	metadataDigest,
	parseForkPin,
	secretConfigViolations,
} from "./core.ts";

const workspaceRoot = join(dirname(fileURLToPath(import.meta.url)), "..", "..");
const laneDir = dirname(fileURLToPath(import.meta.url));
const pinPath = join(laneDir, "fork-pin.json");

function fail(code: number, message: string): never {
	console.error(`gateway-image: ${message}`);
	process.exit(code);
}

function parseOut(argv: readonly string[]): string {
	let out: string | undefined;
	for (let index = 0; index < argv.length; index += 1) {
		const arg = argv[index];
		const value = argv[index + 1];
		if (
			arg === "--out" &&
			value !== undefined &&
			value !== "" &&
			out === undefined
		) {
			out = value;
			index += 1;
		} else {
			fail(EXIT.usage, "usage: build.ts [--out <dir>]");
		}
	}
	return resolve(workspaceRoot, out ?? join(laneDir, "out"));
}

function runGit(args: readonly string[], cwd: string): void {
	const result = spawnSync("git", [...args], {
		cwd,
		encoding: "utf8",
		stdio: ["ignore", "pipe", "pipe"],
	});
	const stdout = result.stdout ?? "";
	const stderr =
		result.stderr ??
		(result.error instanceof Error ? result.error.message : "");
	if (stdout !== "") console.error(stdout.trimEnd());
	if (stderr !== "") console.error(stderr.trimEnd());
	if (result.status !== 0) {
		fail(1, `git ${args.join(" ")} failed (exit ${result.status ?? 1})`);
	}
}

const outDir = parseOut(process.argv.slice(2));
if (
	process.env.BUILDKIT_HOST === undefined ||
	process.env.BUILDKIT_HOST === ""
) {
	fail(EXIT.usage, "BUILDKIT_HOST is unset");
}

let pinText: string;
try {
	pinText = readFileSync(pinPath, "utf8");
} catch (error) {
	fail(EXIT.badPin, `reading fork-pin.json failed: ${String(error)}`);
}
let pin: ForkPin;
try {
	pin = parseForkPin(pinText);
} catch (error) {
	fail(
		EXIT.badPin,
		`invalid fork-pin.json: ${error instanceof Error ? error.message : String(error)}`,
	);
}

const srcDir = join(outDir, "src");
const baseOciDir = join(outDir, "base-oci");
const ociDir = join(outDir, "oci");
const metadataFile = join(outDir, "metadata.json");
rmSync(srcDir, { recursive: true, force: true });
mkdirSync(srcDir, { recursive: true });
runGit(["init"], srcDir);
runGit(["remote", "add", "origin", pin.repo], srcDir);
runGit(["fetch", "--depth=1", "origin", pin.commit], srcDir);
runGit(["checkout", "--detach", "FETCH_HEAD"], srcDir);
runGit(
	[
		"fetch",
		"--filter=tree:0",
		"origin",
		`+refs/heads/${FORK_BRANCH}:refs/remotes/origin/${FORK_BRANCH}`,
	],
	srcDir,
);
const ancestry = spawnSync(
	"git",
	[
		"merge-base",
		"--is-ancestor",
		pin.commit,
		`refs/remotes/origin/${FORK_BRANCH}`,
	],
	{ cwd: srcDir, encoding: "utf8", stdio: ["ignore", "pipe", "pipe"] },
);
if (ancestry.status === 1) {
	fail(
		EXIT.badPin,
		`fork pin ${pin.commit} is not an ancestor of ${FORK_BRANCH}`,
	);
}
if (ancestry.status !== 0) {
	if (ancestry.stdout !== "") console.error(ancestry.stdout.trimEnd());
	const stderr =
		ancestry.stderr ??
		(ancestry.error instanceof Error ? ancestry.error.message : "");
	if (stderr !== "") console.error(stderr.trimEnd());
	fail(1, `git merge-base --is-ancestor failed (exit ${ancestry.status ?? 1})`);
}

rmSync(baseOciDir, { recursive: true, force: true });
rmSync(ociDir, { recursive: true, force: true });
rmSync(metadataFile, { force: true });
console.error("gateway-image: building base image…");
const base = spawnSync(
	"buildctl",
	baseBuildArgs(srcDir, baseOciDir, hostPlatform(process.arch)),
	{
		cwd: workspaceRoot,
		stdio: "inherit",
	},
);
if (base.status !== 0) process.exit(base.status ?? 1);

let baseDigest: string;
try {
	baseDigest = layoutDigest(
		readFileSync(join(baseOciDir, "index.json"), "utf8"),
	);
} catch (error) {
	fail(
		EXIT.badLayout,
		`reading base OCI layout failed: ${error instanceof Error ? error.message : String(error)}`,
	);
}

console.error("gateway-image: building gateway image…");
const gateway = spawnSync(
	"buildctl",
	gatewayBuildArgs(
		srcDir,
		baseOciDir,
		baseDigest,
		ociDir,
		metadataFile,
		hostPlatform(process.arch),
	),
	{ cwd: workspaceRoot, stdio: "inherit" },
);
if (gateway.status !== 0) process.exit(gateway.status ?? 1);

let digest: string;
let config: ImageConfigEntries;
try {
	const metadata = metadataDigest(readFileSync(metadataFile, "utf8"));
	digest = layoutDigest(readFileSync(join(ociDir, "index.json"), "utf8"));
	if (metadata !== digest) {
		fail(
			EXIT.badLayout,
			`BuildKit metadata digest ${metadata} differs from OCI layout digest ${digest}`,
		);
	}
	const manifestJson = readFileSync(blobPath(ociDir, digest), "utf8");
	const configDigest = manifestConfigDigest(manifestJson);
	config = imageConfigEntries(
		readFileSync(blobPath(ociDir, configDigest), "utf8"),
	);
} catch (error) {
	fail(
		EXIT.badLayout,
		`reading gateway OCI layout failed: ${error instanceof Error ? error.message : String(error)}`,
	);
}
const violations = secretConfigViolations(config);
if (violations.length > 0) {
	fail(
		EXIT.secretFound,
		`image config contains secret-shaped names: ${violations.join(", ")}`,
	);
}
console.error(`gateway-image: built ${digest}`);
console.log(digest);
