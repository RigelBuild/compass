#!/usr/bin/env bun

// macos-bundle (compass-distribution T3) — the macOS .app/.dmg bundler.
//
// PURE CORE: `renderInfoPlist(opts)` templates the CFBundle Info.plist XML from
// the app name/executable/identifier/version, and `parseArgs(argv)` parses the
// CLI flags into a typed BundleArgs. Both are pure: no I/O, no clock, no
// `process`/`env`/`Bun` access. They throw an Error naming the offending flag on
// a missing/duplicate input.
//
// THE EDGE: `main()` (guarded by `import.meta.main`) stages
// `Compass.app/Contents/{MacOS,Resources}`, copies the darwin compass-app binary
// + the UI dist into it, writes Info.plist from the pure renderer, ad-hoc signs
// the app (`codesign --sign -`, mandatory on Apple Silicon — the interim
// self-sign per GC6 / DL-261; real Developer-ID signing is T4), and wraps the
// staging dir in a UDZO .dmg via `hdiutil`. Guarding behind `import.meta.main`
// lets the test import the pure core without firing the edge.
//
// The .app layout mirrors the Linux tarball bundle (GC9 / §A4): the shell, the
// three embedded sidecars, and the UI dist. The dist lands at
// Contents/Resources/dist beside the executable at Contents/MacOS/compass-app,
// satisfying the shell's beside-the-executable dist resolution
// (go/cmd/compass-app/main.go resolveAssetsDir); each sidecar lands at
// Contents/MacOS/<name> BESIDE the shell, where resolveStackBin's sibling probe
// finds it. No compass-postgres — embedded's postgres is a container (DL-260).

import { cp, mkdir, rm } from "node:fs/promises";
import { basename, dirname, join } from "node:path";
import { $ } from "bun";

// ── Pure-core types ────────────────────────────────────────────────────────

/** The inputs the Info.plist template needs. */
export type InfoPlistOptions = {
	/** CFBundleName — the display name (Compass). */
	name: string;
	/** CFBundleExecutable — the binary name under Contents/MacOS (compass-app). */
	executable: string;
	/** CFBundleIdentifier — reverse-DNS bundle id (build.rigel.compass). */
	identifier: string;
	/** CFBundleShortVersionString + CFBundleVersion — the clean semver. */
	version: string;
};

/** The CLI arguments the edge parses from argv. */
export type BundleArgs = {
	/** Path to the built darwin compass-app binary. */
	binary: string;
	/** Path to the UI dist directory (apps/ui/dist). */
	dist: string;
	/** The clean semver stamped into Info.plist. */
	version: string;
	/** Path the produced .dmg is written to. */
	out: string;
	/**
	 * Paths to the sidecar binaries staged beside the shell in Contents/MacOS,
	 * in the order the repeated `--sidecar` flags were given. Optional: the
	 * required contract is binary/dist/version/out, so a caller that passes no
	 * `--sidecar` gets an empty array (a shell-only .app) rather than a parse
	 * error — WHICH sidecars a release carries is the release lane's call
	 * (release.yml passes the three), not a grammar constant. Every path given
	 * is still assertExists-checked before staging.
	 */
	sidecars: string[];
};

// ── Pure-core constants ────────────────────────────────────────────────────

/** The minimum macOS the arm64 shell targets (Big Sur — first Apple Silicon). */
const LS_MINIMUM_SYSTEM_VERSION = "11.0";
/** The Info.plist format version key value (always 6.0). */
const CF_BUNDLE_INFO_DICTIONARY_VERSION = "6.0";

// ── Pure core ──────────────────────────────────────────────────────────────

/** XML-escape a value so it cannot break out of the plist string element. */
function escapeXml(value: string): string {
	return value
		.replace(/&/g, "&amp;")
		.replace(/</g, "&lt;")
		.replace(/>/g, "&gt;");
}

/**
 * Render the CFBundle Info.plist XML for the Compass .app. Pure — the same
 * inputs always produce the same bytes. Carries the minimum CFBundle keys a
 * launchable arm64 .app needs (GC6/§163-183): name, executable, identifier,
 * package type, the two version keys (both the clean semver, GC4), the minimum
 * system version, and the info-dictionary format version.
 */
export function renderInfoPlist(opts: InfoPlistOptions): string {
	const name = escapeXml(opts.name);
	const executable = escapeXml(opts.executable);
	const identifier = escapeXml(opts.identifier);
	const version = escapeXml(opts.version);
	return `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>CFBundleName</key>
	<string>${name}</string>
	<key>CFBundleExecutable</key>
	<string>${executable}</string>
	<key>CFBundleIdentifier</key>
	<string>${identifier}</string>
	<key>CFBundlePackageType</key>
	<string>APPL</string>
	<key>CFBundleShortVersionString</key>
	<string>${version}</string>
	<key>CFBundleVersion</key>
	<string>${version}</string>
	<key>CFBundleInfoDictionaryVersion</key>
	<string>${CF_BUNDLE_INFO_DICTIONARY_VERSION}</string>
	<key>LSMinimumSystemVersion</key>
	<string>${LS_MINIMUM_SYSTEM_VERSION}</string>
</dict>
</plist>
`;
}

/**
 * Parse `--binary <path> --dist <dir> --version <semver> --out <dmg>` plus zero
 * or more repeated `--sidecar <path>` into a typed BundleArgs. Pure; throws an
 * Error naming the offending flag on a missing value, an unknown flag, a
 * duplicate of a single-valued flag, a missing required flag, a sidecar whose
 * basename collides with the shell executable, or duplicate sidecar basenames.
 * `--sidecar` is exempt from the duplicate check by design (it accumulates),
 * but a `--sidecar` with no value fails as loud as any other. Fail-loud on any
 * malformed input mirrors build.sh's sanity posture.
 */
export function parseArgs(argv: string[]): BundleArgs {
	const flags = new Map<string, string>();
	const sidecars: string[] = [];
	// Static flag table → Record membership (a fixed literal set, not a runtime
	// collection).
	const known: Record<string, true> = {
		"--binary": true,
		"--dist": true,
		"--version": true,
		"--out": true,
		"--sidecar": true,
	};
	for (let i = 0; i < argv.length; i++) {
		// biome-ignore lint/style/noNonNullAssertion: index < argv.length.
		const flag = argv[i]!;
		if (known[flag] !== true) {
			throw new Error(`macos-bundle: unknown or misplaced argument '${flag}'`);
		}
		const value = argv[i + 1];
		if (value === undefined || known[value] === true) {
			throw new Error(`macos-bundle: flag '${flag}' expects a value`);
		}
		if (flag === "--sidecar") {
			sidecars.push(value);
		} else {
			if (flags.has(flag)) {
				throw new Error(`macos-bundle: flag '${flag}' given more than once`);
			}
			flags.set(flag, value);
		}
		i++;
	}
	const require_ = (flag: string): string => {
		const value = flags.get(flag);
		if (value === undefined || value === "") {
			throw new Error(`macos-bundle: missing required flag '${flag}'`);
		}
		return value;
	};
	const binary = require_("--binary");
	assertSidecarBasenamesDistinct(binary, sidecars);
	return {
		binary,
		dist: require_("--dist"),
		version: require_("--version"),
		out: require_("--out"),
		sidecars,
	};
}

/**
 * Reject a sidecar whose basename collides with the shell executable or with
 * another sidecar. Both stage into Contents/MacOS/<basename> where `cp`
 * silently overwrites, so a collision would clobber the shell or a sibling
 * sidecar — this pure guard fails loud instead, mirroring build.sh's sanity
 * posture (a green bundle is a COMPLETE bundle).
 */
function assertSidecarBasenamesDistinct(
	binary: string,
	sidecars: string[],
): void {
	const binaryBasename = basename(binary);
	const seen = new Set<string>();
	for (const sidecar of sidecars) {
		const name = basename(sidecar);
		if (name === binaryBasename) {
			throw new Error(
				`macos-bundle: sidecar '${name}' collides with the shell executable '${binaryBasename}'`,
			);
		}
		if (seen.has(name)) {
			throw new Error(`macos-bundle: duplicate sidecar basename '${name}'`);
		}
		seen.add(name);
	}
}

// ── The edge (impure) ──────────────────────────────────────────────────────

/** Fail loud if a required input path does not exist (build.sh sanity posture). */
async function assertExists(path: string, what: string): Promise<void> {
	// Callers pass a FILE path (the binary, the dist's index.html) — Bun.file().
	// exists() reports false for a directory, so the dist is probed via its
	// index.html sentinel, matching build.sh's index.html assertion.
	if (!(await Bun.file(path).exists())) {
		throw new Error(`macos-bundle: ${what} not found at ${path}`);
	}
}

async function main(): Promise<void> {
	const args = parseArgs(Bun.argv.slice(2));

	// Fail loud on missing inputs BEFORE staging (build.sh §256-261 posture): a
	// green bundle means a COMPLETE bundle.
	await assertExists(args.binary, "compass-app binary");
	await assertExists(join(args.dist, "index.html"), "UI dist (index.html)");
	for (const sidecar of args.sidecars) {
		await assertExists(sidecar, `sidecar binary (${basename(sidecar)})`);
	}

	// Stage the .app beside the requested dmg output so the staging dir and the
	// dmg share a parent and cleanup is local.
	const stageRoot = join(dirname(args.out), "macos-bundle-stage");
	const appDir = join(stageRoot, "Compass.app");
	const macosDir = join(appDir, "Contents", "MacOS");
	const resourcesDir = join(appDir, "Contents", "Resources");
	await rm(stageRoot, { recursive: true, force: true });
	await mkdir(macosDir, { recursive: true });
	await mkdir(resourcesDir, { recursive: true });

	// Contents/MacOS/compass-app — the darwin shell binary.
	const stagedBinary = join(macosDir, "compass-app");
	await cp(args.binary, stagedBinary);
	// Ensure the executable bit survives (cp preserves mode; assert anyway by
	// chmod +x via node, which is a no-op if already set).
	await $`chmod +x ${stagedBinary}`.quiet();

	// Contents/MacOS/<name> — the embedded sidecars, BESIDE the shell so
	// resolveStackBin's sibling probe (go/cmd/compass-app) resolves them and
	// prependExecDirToPath threads them onto the supervised stack's $PATH.
	for (const sidecar of args.sidecars) {
		const stagedSidecar = join(macosDir, basename(sidecar));
		await cp(sidecar, stagedSidecar);
		await $`chmod +x ${stagedSidecar}`.quiet();
	}

	// Contents/Resources/dist/ — the UI dist, beside-the-executable per the
	// shell's resolveAssetsDir (dist under the executable's dir → Resources).
	await cp(args.dist, join(resourcesDir, "dist"), { recursive: true });

	// Contents/Info.plist — from the pure renderer.
	await Bun.write(
		join(appDir, "Contents", "Info.plist"),
		renderInfoPlist({
			name: "Compass",
			executable: "compass-app",
			identifier: "build.rigel.compass",
			version: args.version,
		}),
	);

	// Ad-hoc sign (GC6 / DL-261): `--sign -` is the interim self-sign, mandatory
	// on Apple Silicon; real Developer-ID signing + notarization is T4. --deep
	// signs nested code; --force replaces any prior signature (idempotent re-run).
	await $`codesign --sign - --force --deep ${appDir}`;

	// Wrap the staging dir into a compressed (UDZO) .dmg. -ov overwrites an
	// existing image so a re-run is idempotent.
	await rm(args.out, { force: true });
	await $`hdiutil create -volname Compass -srcfolder ${stageRoot} -ov -format UDZO ${args.out}`;

	console.log(`macos-bundle: wrote ${args.out}`);
}

if (import.meta.main) {
	await main();
}
