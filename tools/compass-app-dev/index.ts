#!/usr/bin/env bun
// Linux needs the pinned GTK/WebKitGTK closure because the host toolchain is too
// old; macOS uses system WebKit. Server URL and credentials remain user config.
import { spawnSync } from "node:child_process";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";
import {
	buildEnv,
	devBinPath,
	goBuildArgv,
	gtkBuildEnv,
	nixClosureAttrs,
	parseMode,
	parseOutPaths,
	runEnv,
	spawnOutcome,
} from "./dev-core.ts";

const workspaceRoot = join(dirname(fileURLToPath(import.meta.url)), "..", "..");
const gtkHelper = join(workspaceRoot, "tools", "toolchain", "gtk-e2e-env.nix");
const platform = process.platform;
const outBin = devBinPath(workspaceRoot);

const mode = parseMode(process.argv[2]);
if (mode === null) {
	console.error(
		`compass-app-dev: usage: index.ts <build|run> (got '${process.argv[2] ?? ""}')`,
	);
	process.exit(2);
}

/**
 * Act on a spawnSync result via the pure spawnOutcome decision: name an ENOENT
 * (missing `go`/`nix` on PATH outside a dev shell) and exit, propagate a nonzero
 * status, or fall through on success. Keeps the process.exit/console.error edge
 * thin over the unit-tested decision in dev-core.
 */
function orExit(
	result: { status: number | null; error?: Error },
	what: string,
): void {
	const outcome = spawnOutcome(result, `compass-app-dev: ${what}`);
	if (outcome.action === "error") {
		console.error(outcome.message);
		process.exit(outcome.code);
	}
	if (outcome.action === "exit") {
		process.exit(outcome.code);
	}
}

/** Realize one gtk-e2e-env.nix attr and return its single store out-path. */
function nixBuild(attr: string): string {
	const result = spawnSync(
		"nix",
		["build", "--no-link", "--print-out-paths", "-f", gtkHelper, attr],
		{ cwd: workspaceRoot, stdio: ["ignore", "pipe", "inherit"] },
	);
	orExit(result, `nix build -f ${gtkHelper} ${attr}`);
	const paths = parseOutPaths(result.stdout.toString());
	if (paths.length !== 1 || paths[0] === undefined) {
		console.error(
			`compass-app-dev: nix build ${attr} produced ${paths.length} out-paths, expected 1`,
		);
		process.exit(1);
	}
	return paths[0];
}

// --- Realize the linux gtk4 closure (no-op on darwin). ---
let overlay: Record<string, string> = {};
const closure = nixClosureAttrs(platform);
if (closure !== null) {
	console.error(
		`compass-app-dev: realizing gtk4 closure (${closure.pc}, ${closure.cc})`,
	);
	overlay = gtkBuildEnv(nixBuild(closure.pc), nixBuild(closure.cc));
}

// --- Compile the dev binary (both modes). ---
const [goCmd, ...goArgs] = goBuildArgv(platform, outBin);
console.error(`compass-app-dev: ${goCmd} ${goArgs.join(" ")}`);
const build = spawnSync(goCmd, goArgs, {
	cwd: workspaceRoot,
	env: buildEnv(process.env, overlay),
	stdio: "inherit",
});
orExit(build, [goCmd, ...goArgs].join(" "));

if (mode === "build") {
	console.error(`compass-app-dev: built ${outBin}`);
	process.exit(0);
}

// --- Launch it, serving the checked-out UI dist in place. ---
console.error(
	`compass-app-dev: launching ${outBin} (COMPASS_ASSETS_DIR=apps/ui/dist)`,
);
const run = spawnSync(outBin, [], {
	cwd: workspaceRoot,
	env: runEnv(process.env, workspaceRoot),
	stdio: "inherit",
});
orExit(run, outBin);
process.exit(0);
