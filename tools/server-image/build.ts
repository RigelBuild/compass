#!/usr/bin/env bun
import { spawnSync } from "node:child_process";
import {
	chmodSync,
	cpSync,
	existsSync,
	mkdirSync,
	readdirSync,
	rmSync,
	symlinkSync,
} from "node:fs";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";
import { hostPlatform, outputSpec } from "../runner-image/build-core.ts";
import { ENTRYPOINT_LINK, entrypointTarget } from "./build-core.ts";

const workspaceRoot = join(dirname(fileURLToPath(import.meta.url)), "..", "..");
const imageDir = join(workspaceRoot, "server-image");
const stageDir = join(imageDir, "store");
const ociDir = join(imageDir, "out");
let tag = "ghcr.io/rigelbuild/compass-server";
let mode: "oci" | "image" | "push" = "oci";
let metadataFile: string | undefined;

for (let index = 2; index < process.argv.length; index += 1) {
	const arg = process.argv[index];
	const value = process.argv[index + 1];
	if (arg === "--tag" && value !== undefined) {
		tag = value;
		index += 1;
	} else if (arg === "--output" && value !== undefined) {
		if (value !== "oci" && value !== "image" && value !== "push") {
			console.error(`--output must be 'oci', 'image' or 'push', got: ${value}`);
			process.exit(2);
		}
		mode = value;
		index += 1;
	} else if (arg === "--metadata-file" && value !== undefined) {
		metadataFile = value;
		index += 1;
	} else {
		console.error(`unknown argument: ${arg}`);
		process.exit(2);
	}
}

const buildkitHost = process.env.BUILDKIT_HOST;
if (!buildkitHost) {
	console.error("BUILDKIT_HOST is unset; start a buildkitd and point it here.");
	process.exit(1);
}

const platform = hostPlatform(process.arch);

function nixBuild(args: readonly string[]): string {
	const result = spawnSync(
		"nix",
		["build", "--no-link", "--print-out-paths", ...args],
		{
			cwd: workspaceRoot,
			encoding: "utf8",
			stdio: ["ignore", "pipe", "inherit"],
		},
	);
	if (result.status !== 0) {
		console.error(`nix build ${args.join(" ")} failed (exit ${result.status})`);
		process.exit(1);
	}
	const paths = result.stdout.trim().split("\n");
	const path = paths[0];
	if (paths.length !== 1 || path === undefined || path === "") {
		console.error(`nix build ${args.join(" ")} did not produce one out-path`);
		process.exit(1);
	}
	return path;
}

function makeWritable(dir: string): void {
	if (!existsSync(dir)) return;
	chmodSync(dir, 0o755);
	for (const entry of readdirSync(dir, { withFileTypes: true })) {
		if (entry.isDirectory()) makeWritable(join(dir, entry.name));
	}
}

console.error("server-image: realising compass-server closure…");
const server = nixBuild([".#compass-server"]);
makeWritable(stageDir);
rmSync(stageDir, { recursive: true, force: true });
if (mode !== "push") rmSync(ociDir, { recursive: true, force: true });
mkdirSync(stageDir, { recursive: true });

const closure = spawnSync("nix", ["path-info", "-r", server], {
	cwd: workspaceRoot,
	encoding: "utf8",
	stdio: ["ignore", "pipe", "inherit"],
});
if (closure.status !== 0) {
	console.error(`nix path-info -r failed (exit ${closure.status})`);
	process.exit(1);
}
const closurePaths = closure.stdout
	.split("\n")
	.map((path) => path.trim())
	.filter((path) => path !== "");
for (const path of closurePaths) {
	cpSync(path, join(stageDir, path.replace(/^\/nix\/store\//, "")), {
		recursive: true,
		verbatimSymlinks: true,
		preserveTimestamps: true,
	});
}
console.error(`server-image: staged ${closurePaths.length} store paths`);

const entrypoint = entrypointTarget(server, stageDir);
rmSync(join(stageDir, ENTRYPOINT_LINK), { force: true });
symlinkSync(entrypoint, join(stageDir, ENTRYPOINT_LINK));
mkdirSync(ociDir, { recursive: true });
console.error(`server-image: building ${tag}…`);
const build = spawnSync(
	"buildctl",
	[
		"build",
		"--frontend",
		"dockerfile.v0",
		"--local",
		`context=${imageDir}`,
		"--local",
		`dockerfile=${imageDir}`,
		"--opt",
		"filename=Dockerfile",
		"--opt",
		`platform=${platform}`,
		"--opt",
		"build-arg:SOURCE_DATE_EPOCH=1",
		"--output",
		outputSpec(mode, tag, ociDir),
		...(metadataFile ? ["--metadata-file", metadataFile] : []),
	],
	{
		cwd: workspaceRoot,
		stdio: "inherit",
		env: { ...process.env, BUILDKIT_HOST: buildkitHost },
	},
);
process.exit(build.status ?? 1);
