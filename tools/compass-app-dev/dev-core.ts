// Pure helpers for the compass-app-dev lane — the per-OS go build argv, the
// gitignored dev-binary and UI-dist paths, the gtk4 nix-closure attrs, and the
// build/run env overlays. No I/O and no process spawn: unit-tested in
// dev-core.test.ts, while the spawning edge (nix build + go build + exec) lives
// in index.ts.
import { join } from "node:path";

/**
 * Build tags for the cgo compile of go/cmd/compass-app on the host OS. darwin
 * links the system WebKit framework and needs no build tag (T2 restructured the
 * tags so a bare `go build ./cmd/compass-app` compiles on darwin); linux needs
 * `-tags gtk4`. The native shell builds on darwin or linux only.
 */
export function buildTags(platform: NodeJS.Platform): readonly string[] {
	switch (platform) {
		case "darwin":
			return [];
		case "linux":
			return ["gtk4"];
		default:
			throw new Error(
				`compass-app-dev: unsupported platform '${platform}' — the native shell builds on darwin or linux only`,
			);
	}
}

/**
 * The full `go` argv (run from the workspace root) that compiles the dev binary
 * to `outBin`. Mirrors the ci.yml darwin lane / app-bundle build flags
 * (-trimpath, the gtk4 tag on linux) without the release -ldflags stamp: a dev
 * build keeps main.go's default version.
 */
export function goBuildArgv(
	platform: NodeJS.Platform,
	outBin: string,
): [string, ...string[]] {
	const tags = buildTags(platform);
	return [
		"go",
		"-C",
		"go",
		"build",
		"-trimpath",
		...(tags.length > 0 ? ["-tags", tags.join(",")] : []),
		"-o",
		outBin,
		"./cmd/compass-app",
	];
}

/**
 * The gtk-e2e-env.nix attrs the linux compile must realize before the cgo build:
 * the WebKitGTK pkg-config closure (`pkgConfig`) and the nixpkgs C toolchain
 * (`cc.out`). Returns null on darwin — it links the system WebKit framework with
 * the platform clang and realizes nothing. `cc.out`, not a bare `cc`: the
 * cc-wrapper is multi-output, so the `.out` selector pins the one output carrying
 * bin/cc (mirrors ci.yml). A typed pair rather than an array so the caller
 * destructures without an unchecked cast.
 */
export function nixClosureAttrs(
	platform: NodeJS.Platform,
): { pc: string; cc: string } | null {
	// buildTags rejects any non-darwin/linux platform; reuse it as the guard.
	buildTags(platform);
	return platform === "linux" ? { pc: "pkgConfig", cc: "cc.out" } : null;
}

/**
 * The compile env overlay from the realized linux gtk4 closure: point cgo's
 * pkg-config at the WebKitGTK `.pc` closure (both lib/ and share/ subdirs — zlib
 * ships zlib.pc under share/, which gtk4 transitively requires) and its CC/CXX at
 * the nixpkgs cc-wrapper (the runner's glibc is too old to link the nixpkgs
 * WebKitGTK). Mirrors ci.yml's gtk4 e2e step and app-bundle/build.sh.
 */
export function gtkBuildEnv(
	pcOut: string,
	ccOut: string,
): Record<string, string> {
	return {
		PKG_CONFIG_PATH: `${pcOut}/lib/pkgconfig:${pcOut}/share/pkgconfig`,
		CC: `${ccOut}/bin/cc`,
		CXX: `${ccOut}/bin/c++`,
	};
}

/** Split `nix build --print-out-paths` stdout into trimmed non-empty paths. */
export function parseOutPaths(stdout: string): string[] {
	return stdout
		.split("\n")
		.map((line) => line.trim())
		.filter((line) => line.length > 0);
}

/** The gitignored path the dev binary compiles to (under tools/compass-app-dev/). */
export function devBinPath(workspaceRoot: string): string {
	return join(workspaceRoot, "tools", "compass-app-dev", "compass-app");
}

/** The built UI dist the resolver serves via COMPASS_ASSETS_DIR. */
export function assetsDir(workspaceRoot: string): string {
	return join(workspaceRoot, "apps", "ui", "dist");
}

/**
 * The compile env: cgo on, plus any platform overlay (the linux gtk4 closure's
 * PKG_CONFIG_PATH/CC/CXX; empty on darwin).
 */
export function buildEnv(
	base: NodeJS.ProcessEnv,
	overlay: Record<string, string> = {},
): NodeJS.ProcessEnv {
	return { ...base, CGO_ENABLED: "1", ...overlay };
}

/**
 * The launch env: point the asset resolver at the built UI dist so the shell
 * serves the UI straight from the checkout, with no staged .app/tarball layout.
 */
export function runEnv(
	base: NodeJS.ProcessEnv,
	workspaceRoot: string,
): NodeJS.ProcessEnv {
	return { ...base, COMPASS_ASSETS_DIR: assetsDir(workspaceRoot) };
}

/** The two run modes index.ts dispatches on. */
export type Mode = "build" | "run";

/**
 * Validate the CLI mode argument. Returns the mode, or null for anything else
 * (index.ts turns null into a usage error + exit 2). Pure so the reject path is
 * unit-testable without spawning.
 */
export function parseMode(arg: string | undefined): Mode | null {
	return arg === "build" || arg === "run" ? arg : null;
}

/**
 * The decision for how index.ts should react to a spawnSync result, factored out
 * of the process.exit/console.error edge so it is unit-testable:
 *   - `error`  — the process could not run at all (spawnSync set `error`, status
 *     null): a missing executable on PATH (ENOENT), e.g. `go`/`nix` outside an
 *     active dev shell. A bare status check would exit silently; this names it.
 *   - `exit`   — the process ran and returned a nonzero status; propagate it.
 *   - `ok`     — the process ran and returned 0.
 */
export type SpawnOutcome =
	| { action: "error"; code: 1; message: string }
	| { action: "exit"; code: number }
	| { action: "ok" };

export function spawnOutcome(
	result: { status: number | null; error?: Error },
	what: string,
): SpawnOutcome {
	if (result.error !== undefined) {
		return {
			action: "error",
			code: 1,
			message: `${what} could not run: ${result.error.message}`,
		};
	}
	if (result.status !== 0) {
		return { action: "exit", code: result.status ?? 1 };
	}
	return { action: "ok" };
}
