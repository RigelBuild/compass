#!/usr/bin/env bun
// Build the prebuilt runner image with rootless BuildKit; Nix provides the
// closure but is not the runtime image. Pure mapping logic lives in build-core.ts.
// Usage: bun tools/runner-image/build.ts [--tag <ref>] [--output oci|image]

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
import {
	buildctlArgs,
	closureRoots,
	hostPlatform,
	outputSpec,
	parseOutPaths,
	type RunnerImageOutputs,
} from "./build-core.ts";

const workspaceRoot = join(dirname(fileURLToPath(import.meta.url)), "..", "..");
const imageDir = join(workspaceRoot, "runner-image");
const stageDir = join(imageDir, "store");
const ociDir = join(imageDir, "out");

function parseArgs(argv: readonly string[]): {
	tag: string;
	mode: "oci" | "image" | "push";
	metadataFile: string | undefined;
} {
	let tag = "compass-runner:dev";
	let mode: "oci" | "image" | "push" = "oci";
	let metadataFile: string | undefined;
	for (let i = 0; i < argv.length; i += 1) {
		const arg = argv[i];
		const value = argv[i + 1];
		if (arg === "--tag" && value !== undefined) {
			tag = value;
			i += 1;
		} else if (arg === "--metadata-file" && value !== undefined) {
			metadataFile = value;
			i += 1;
		} else if (arg === "--output" && value !== undefined) {
			if (value !== "oci" && value !== "image" && value !== "push") {
				console.error(
					`--output must be 'oci', 'image' or 'push', got: ${value}`,
				);
				process.exit(2);
			}
			mode = value;
			i += 1;
		} else {
			console.error(`unknown argument: ${arg}`);
			process.exit(2);
		}
	}
	return { tag, mode, metadataFile };
}

/** Run a nix build and return its realised out-paths in argument order. Exits on
 * failure or on an out-path count that does not match what was asked for, so a
 * drifted attr set is a named, fail-closed abort rather than a silently
 * incomplete image. */
function nixBuild(
	args: readonly string[],
	expected: number,
	cwd: string,
): string[] {
	const result = spawnSync(
		"nix",
		["build", "--no-link", "--print-out-paths", ...args],
		{
			cwd,
			encoding: "utf8",
			stdio: ["ignore", "pipe", "inherit"],
		},
	);
	if (result.status !== 0) {
		console.error(`nix build ${args.join(" ")} failed (exit ${result.status})`);
		process.exit(1);
	}
	const paths = parseOutPaths(result.stdout);
	if (paths.length !== expected) {
		console.error(
			`nix build ${args.join(" ")} produced ${paths.length} out-paths, expected ${expected}`,
		);
		process.exit(1);
	}
	return paths;
}

/** Restore owner-write on a staged tree so it can be removed. Copying from the
 * nix store preserves its read-only directory modes, which would otherwise make
 * the stage dir undeletable on the next run. A no-op when the path is absent. */
function makeWritable(dir: string): void {
	if (!existsSync(dir)) return;
	chmodSync(dir, 0o755);
	for (const entry of readdirSync(dir, { withFileTypes: true })) {
		// readdirSync withFileTypes uses lstat semantics, so a symlink already
		// reports isDirectory() === false and is never followed here.
		if (entry.isDirectory()) {
			makeWritable(join(dir, entry.name));
		}
	}
}

const { tag, mode, metadataFile } = parseArgs(process.argv.slice(2));

const platform = hostPlatform(process.arch);

// buildctl is a CLIENT; it needs a reachable buildkitd. This only checks the
// var is SET, so an unreachable daemon still fails at the build step after the
// realise — it catches the common "forgot to start one" case early, not a dead
// socket. There is deliberately no `docker build` fallback.
const buildkitHost = process.env.BUILDKIT_HOST;
if (buildkitHost === undefined || buildkitHost === "") {
	console.error(
		"BUILDKIT_HOST is unset — start a buildkitd and point it here.\n" +
			"  This lane never falls back to `docker build`: rootless BuildKit is the\n" +
			"  ruled mechanism for a prebuilt-application image, not a preference.",
	);
	process.exit(1);
}

// Realize the flake outputs and guest assets before staging the image closure.
console.error("runner-image: realising closure…");
const [runner] = nixBuild([".#compass-runner"], 1, workspaceRoot) as [string];
const [stack] = nixBuild([".#compass-stack-env"], 1, workspaceRoot) as [string];
const guest = nixBuild(
	[
		"-f",
		"default.nix",
		"compass-guest-kernel",
		"compass-guest-rootfs",
		"compass-guest-initrd",
	],
	3,
	join(workspaceRoot, "guest-image"),
);
const [kernelDir, rootfs, initrd] = guest as [string, string, string];
const outputs: RunnerImageOutputs = {
	runner,
	stack,
	kernelDir,
	rootfs,
	initrd,
};

// Stage the deduplicated transitive closure; root paths alone omit runtime libraries.
console.error("runner-image: staging closure…");
// Nix store paths are read-only, and a recursive copy preserves that — so a
// previously staged tree cannot be removed until its directories are made
// writable again. Without this, the SECOND run of this lane fails EACCES on its
// own leftovers.
makeWritable(stageDir);
rmSync(stageDir, { recursive: true, force: true });
// Only a layout-producing run may clear the layout. A `push` run must NOT: the
// publish lane compares the pushed digest against the layout this directory
// already holds, and wiping it leaves that comparison with nothing to read.
if (mode !== "push") {
	rmSync(ociDir, { recursive: true, force: true });
}
mkdirSync(stageDir, { recursive: true });

const closure = spawnSync(
	"nix",
	["path-info", "-r", ...closureRoots(outputs)],
	{
		cwd: workspaceRoot,
		encoding: "utf8",
		stdio: ["ignore", "pipe", "inherit"],
	},
);
if (closure.status !== 0) {
	console.error(`nix path-info -r failed (exit ${closure.status})`);
	process.exit(1);
}
const closurePaths = parseOutPaths(closure.stdout);
for (const path of closurePaths) {
	// Preserve symlinks and Nix mtimes so runtime links resolve and image digests reproduce.
	cpSync(path, join(stageDir, path.replace(/^\/nix\/store\//, "")), {
		recursive: true,
		verbatimSymlinks: true,
		preserveTimestamps: true,
	});
}
console.error(`runner-image: staged ${closurePaths.length} store paths`);

// The exec-form entrypoint needs a stable path despite the runner store hash changing.
// Keep its relative symlink inside store/: BuildKit resolves context links before COPY.
rmSync(join(stageDir, ".compass-runner"), { force: true });
symlinkSync(
	join(runner.replace(/^\/nix\/store\//, ""), "bin", "compass-runner"),
	join(stageDir, ".compass-runner"),
);

// ---------------------------------------------------------------------------
// 3. Build the image from the staged directory.
// ---------------------------------------------------------------------------
mkdirSync(ociDir, { recursive: true });
console.error(`runner-image: building ${tag}…`);
// `--metadata-file` is how the digest leaves this process: the exporter writes
// containerimage.digest, which for a push is what the registry accepted. The
// publish lane asserts that against a prior local build.
const build = spawnSync(
	"buildctl",
	[
		...buildctlArgs(imageDir, outputs, platform, outputSpec(mode, tag, ociDir)),
		...(metadataFile ? ["--metadata-file", metadataFile] : []),
	],
	{ cwd: workspaceRoot, stdio: "inherit" },
);
process.exit(build.status ?? 1);
