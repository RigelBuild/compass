#!/usr/bin/env bun
// Build the compass-runner container image (R1 of the Compass Runner
// containerization record): realise the nix closure, stage it into a build
// context, and hand that plain directory to a rootless BuildKit Dockerfile
// build.
//
// THE MECHANISM IS THE RULED ONE, and it is part of the reviewed surface rather
// than an implementation detail. The fleet's first-party image-builds spec
// splits mechanisms by what the image's RUNTIME is, not by what built the
// artifact: an image that IS a Nix environment earns nix2container; a PREBUILT
// APPLICATION on a minimal base is a Dockerfile built by rootless BuildKit. The
// Runner is the latter — it runs no toolchain, it exec's four binaries — so nix
// is the BUILDER here and never the runtime. The prescribed shape is `nix
// build` then COPY, which is exactly the two halves below.
//
// TypeScript, not bash: this has real logic — four nix builds whose out-paths
// are parsed and mapped to distinct build-args, a transitive closure staged
// path-by-path, and a fail-fast on every missing output — so the parsing and
// mapping core is pure and unit-tested (./build-core.test.ts) while this file is
// the thin I/O shell.
//
// Usage:
//   bun tools/runner-image/build.ts [--tag <ref>] [--output oci|image]

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
	outputSpec,
	parseOutPaths,
	type RunnerImageOutputs,
} from "./build-core.ts";

// This file is tools/runner-image/build.ts, so `../..` is the workspace root.
const workspaceRoot = join(dirname(fileURLToPath(import.meta.url)), "..", "..");
const imageDir = join(workspaceRoot, "runner-image");
const stageDir = join(imageDir, "store");
const ociDir = join(imageDir, "out");

// Single-arch linux/amd64: the Runner's microVM backend needs KVM on the node,
// and the cluster nodes are amd64. A second leg would need its own KVM-capable
// builder, so it is added when a node arch is, not speculatively.
const IMAGE_PLATFORM = "linux/amd64";

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

// The platform below is a manifest LABEL; BuildKit applies it without checking
// what the COPY'd files actually are. Today only flake.nix's systems list keeps
// the two honest — a separate file this lane never reads — so assert it here
// rather than inherit an unchecked invariant.
if (process.arch !== "x64") {
	console.error(
		`runner-image targets ${IMAGE_PLATFORM}, but this host is ${process.arch}.\n` +
			"  Building here would label the image amd64 while staging this host's binaries.",
	);
	process.exit(1);
}

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

// ---------------------------------------------------------------------------
// 1. Realise every carried artifact.
// ---------------------------------------------------------------------------
// The Runner and the VMM env are flake outputs; the three guest assets come from
// guest-image/default.nix, which is a bare nix file and NOT a flake, so it takes
// the `-f` form (no `#attr` flake-ref exists for it) and runs from guest-image/.
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

// ---------------------------------------------------------------------------
// 2. Stage the transitive closure.
// ---------------------------------------------------------------------------
// `nix path-info -r` over all five roots emits the DEDUPED union, so a path
// shared by several roots (glibc, most obviously) is staged once. The transitive
// set — not just the five roots — is what makes the image runnable: see
// closureRoots' note on the measured "missing dynamic library" control.
console.error("runner-image: staging closure…");
// Nix store paths are read-only, and a recursive copy preserves that — so a
// previously staged tree cannot be removed until its directories are made
// writable again. Without this, the SECOND run of this lane fails EACCES on its
// own leftovers.
makeWritable(stageDir);
rmSync(stageDir, { recursive: true, force: true });
rmSync(ociDir, { recursive: true, force: true });
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
	// Each basename is unique by construction (it carries a content hash), and
	// the store's symlinks are preserved verbatim so the symlinkJoin'd stack env
	// still resolves inside the image.
	//
	// preserveTimestamps is load-bearing for the DIGEST, not just tidiness. Nix
	// normalises every store mtime to 1; without this, cpSync stamps wall-clock
	// mtimes into the staged tree, they land in the layer tar, and two builds of
	// identical inputs produce different digests. The publish lane asserts a
	// stable digest, so this is part of what makes that assertion meaningful.
	cpSync(path, join(stageDir, path.replace(/^\/nix\/store\//, "")), {
		recursive: true,
		verbatimSymlinks: true,
		preserveTimestamps: true,
	});
}
console.error(`runner-image: staged ${closurePaths.length} store paths`);

// The Dockerfile's ENTRYPOINT is exec-form and so cannot expand a build ARG,
// while the Runner's own store path carries a hash that moves on every Go
// rebuild. A stable relative symlink INSIDE the staged tree resolves both: the
// entrypoint is always /nix/store/.compass-runner, and what it points at moves
// with the build.
//
// It must live inside store/ and be RELATIVE. An absolute symlink at the
// context root dangles on the host (nothing is mounted at the target yet), and
// BuildKit checksums context entries before any COPY runs, so it fails the
// build with "not found" rather than deferring to runtime. Relative-and-inside
// keeps the link resolvable in both places.
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
		...buildctlArgs(
			imageDir,
			outputs,
			IMAGE_PLATFORM,
			outputSpec(mode, tag, ociDir),
		),
		...(metadataFile ? ["--metadata-file", metadataFile] : []),
	],
	{ cwd: workspaceRoot, stdio: "inherit" },
);
process.exit(build.status ?? 1);
