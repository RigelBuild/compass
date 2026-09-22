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
/** The name the shell is staged as in Contents/MacOS — also CFBundleExecutable. */
const SHELL_EXECUTABLE_NAME = "compass-app";

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
 * basename collides with the shell executable's staged name, or duplicate
 * sidecar basenames.
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
	assertSidecarBasenamesDistinct(sidecars);
	return {
		binary,
		dist: require_("--dist"),
		version: require_("--version"),
		out: require_("--out"),
		sidecars,
	};
}

/**
 * Reject a sidecar whose basename collides with the shell executable's staged
 * name or with another sidecar. All stage into Contents/MacOS/<basename> where
 * `cp` silently overwrites, so a collision would clobber the shell or a sibling
 * sidecar — this pure guard fails loud instead, mirroring build.sh's sanity
 * posture (a green bundle is a COMPLETE bundle). Keyed on the STAGED name
 * (SHELL_EXECUTABLE_NAME), not basename(--binary): the shell always stages as
 * SHELL_EXECUTABLE_NAME regardless of the source path the caller passes.
 */
function assertSidecarBasenamesDistinct(sidecars: string[]): void {
	const seen = new Set<string>();
	for (const sidecar of sidecars) {
		const name = basename(sidecar);
		if (name === SHELL_EXECUTABLE_NAME) {
			throw new Error(
				`macos-bundle: sidecar '${name}' collides with the shell executable '${SHELL_EXECUTABLE_NAME}'`,
			);
		}
		if (seen.has(name)) {
			throw new Error(`macos-bundle: duplicate sidecar basename '${name}'`);
		}
		seen.add(name);
	}
}

/** Extract one image block's `mount-point` values, in document order. */
function blockMountPoints(block: string): string[] {
	const mountRe = /<key>mount-point<\/key>\s*<string>([^<]*)<\/string>/g;
	const mounts: string[] = [];
	for (let m = mountRe.exec(block); m; m = mountRe.exec(block)) {
		if (m[1]) mounts.push(m[1]);
	}
	return mounts;
}

/**
 * Parse `hdiutil info -plist` text and return the mount points that a stale
 * attachment of THIS build holds — so the caller can force-detach them before
 * `hdiutil create`, which fails with "Resource busy" when an earlier run on a
 * reused runner leaked the attachment of our image path or volume. Pure: it
 * only reads text, so the parsing (where a bug would hide) is unit-testable.
 *
 * Conservative by construction: a block is selected ONLY when its image-path
 * equals `imagePath` or one of its mount points is named `volumeName`, so an
 * unrelated volume is never returned. Unparseable/empty input yields `[]`.
 */
export function staleMountPoints(
	hdiutilInfoPlist: string,
	target: { imagePath: string; volumeName: string },
): string[] {
	// Each attached image is a block introduced by its <key>image-path</key>;
	// splitting on that key isolates one image's system-entities per chunk.
	const blocks = hdiutilInfoPlist.split("<key>image-path</key>").slice(1);
	const found = new Set<string>();
	for (const block of blocks) {
		const imagePath = /^\s*<string>([^<]*)<\/string>/.exec(block)?.[1];
		const mounts = blockMountPoints(block);
		const matches =
			imagePath === target.imagePath ||
			mounts.some((mp) => basename(mp) === target.volumeName);
		if (matches) {
			for (const mp of mounts) found.add(mp);
		}
	}
	return [...found];
}

/**
 * Render what a failed `hdiutil create` had open, for the EBUSY path. Pure so
 * the formatting is unit-testable; the caller collects the probe output.
 *
 * `hdiutil create` can report "Resource busy" without naming the holder, so the
 * cause has to be probed rather than inferred: the previous fix (detaching a
 * leaked attachment, on both the CI step and the tool side) targeted the output
 * image, and the flake outlived it.
 *
 * An exit code rides in each section header because a silent `lsof` exit 1 means
 * "nothing holds the tree" — the most decisive result the probe can return, and
 * indistinguishable from a probe that failed without it.
 */
export function formatBusyDiagnosis(probes: {
	stageRoot: string;
	lsof: Probe;
	hdiutilInfo: Probe;
}): string {
	const section = (title: string, probe: Probe): string =>
		`── ${title} (exit ${probe.exitCode}) ──\n${probeOutput(probe)}`;
	return [
		`macos-bundle: hdiutil create failed; probing what holds ${probes.stageRoot}`,
		section(`lsof +D ${probes.stageRoot}`, probes.lsof),
		section("hdiutil info", probes.hdiutilInfo),
	].join("\n");
}

/**
 * Render the headline for a failed `hdiutil create`. Empty stderr says so
 * rather than trailing a bare colon, which reads as truncated output.
 */
export function formatCreateFailure(
	exitCode: number | null,
	stderr: string,
): string {
	return `macos-bundle: hdiutil create failed (exit ${exitCode}): ${stderr.trim() || "(no stderr)"}`;
}

/**
 * Emit the headline BEFORE the diagnosis, so the real error still reaches the
 * log if the probe stalls, and return it for the caller to throw.
 */
export async function reportCreateFailure(
	headline: string,
	diagnose: () => Promise<string>,
	emit: (line: string) => void,
): Promise<string> {
	emit(headline);
	emit(await diagnose());
	return headline;
}

/**
 * One probe's result. A probe that never produced an exit status says which
 * way it failed, so "(no output)" is never mistaken for "nothing holds it".
 */
export type Probe = {
	exitCode: number | "timed out" | "probe failed";
	stdout: string;
	stderr: string;
};

/** Join a probe's streams so a stdout without a trailing newline can't eat stderr. */
function probeOutput(probe: Probe): string {
	const parts = [probe.stdout, probe.stderr]
		.map((s) => s.trim())
		.filter(Boolean);
	return parts.length > 0 ? parts.join("\n") : "(no output)";
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

/**
 * Force-detach any stale attachment of our volume/image left by an earlier run
 * on a reused runner, so `hdiutil create` does not hit "Resource busy". Never
 * throws: a clean system (nothing to detach) and an unavailable/unparseable
 * `hdiutil info` both leave the build untouched — a cleanup that reds a green
 * system is worse than the leak.
 */
async function detachStaleAttachments(target: {
	imagePath: string;
	volumeName: string;
}): Promise<void> {
	const info = await $`hdiutil info -plist`.quiet().nothrow();
	if (info.exitCode !== 0) return;
	for (const mount of staleMountPoints(info.stdout.toString(), target)) {
		await $`hdiutil detach ${mount} -force`.quiet().nothrow();
	}
}

/**
 * Collect what holds the staging tree after a failed `hdiutil create`. Never
 * throws and never hangs: this runs only when the build has already failed, so
 * a diagnostic that blocks would erase the error it exists to explain.
 */
async function diagnoseBusy(stageRoot: string): Promise<string> {
	// Concurrent so the whole diagnostic is bounded once, not once per probe.
	// +D walks the tree to full depth and man lsof warns it can be slow.
	const [lsof, hdiutilInfo] = await Promise.all([
		probe(["lsof", "+D", stageRoot]),
		probe(["hdiutil", "info"]),
	]);
	return formatBusyDiagnosis({ stageRoot, lsof, hdiutilInfo });
}

const PROBE_TIMEOUT_MS = 10_000;

/** Run one bounded probe, reporting any failure as output instead of raising. */
async function probe(cmd: string[]): Promise<Probe> {
	try {
		const child = Bun.spawn(cmd, {
			stdout: "pipe",
			stderr: "pipe",
			timeout: PROBE_TIMEOUT_MS,
		});
		// Accumulate as it arrives: a probe that outruns the deadline has usually
		// already printed the holder line, which is the thing worth keeping.
		let stdout = "";
		let stderr = "";
		const drain = async (
			stream: ReadableStream<Uint8Array>,
			onChunk: (text: string) => void,
		): Promise<void> => {
			const decoder = new TextDecoder();
			for await (const chunk of stream) onChunk(decoder.decode(chunk));
		};
		// The spawn timeout signals the child alone, so a descendant holding the
		// inherited pipe can keep these reads open long past it. Race the reads
		// too, or the bound is only as good as the deepest grandchild.
		const finished = await Promise.race([
			Promise.all([
				drain(child.stdout, (t) => {
					stdout += t;
				}),
				drain(child.stderr, (t) => {
					stderr += t;
				}),
				child.exited,
			]).then(() => true),
			// biome-ignore lint/plugin: a real deadline on a subprocess read, not a test wait.
			Bun.sleep(PROBE_TIMEOUT_MS + 1_000).then(() => false),
		]);
		if (!finished) child.kill("SIGKILL");
		// `child.killed` is true for every spawn, so only the timeout's SIGTERM
		// distinguishes a bounded-out probe from a normal non-zero exit.
		const timedOut = !finished || child.signalCode === "SIGTERM";
		return {
			exitCode: timedOut ? "timed out" : (child.exitCode ?? "probe failed"),
			stdout,
			stderr,
		};
	} catch (err) {
		return { exitCode: "probe failed", stdout: "", stderr: String(err) };
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
	const stagedBinary = join(macosDir, SHELL_EXECUTABLE_NAME);
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
			executable: SHELL_EXECUTABLE_NAME,
			identifier: "build.rigel.compass",
			version: args.version,
		}),
	);

	// Ad-hoc sign (GC6 / DL-261): `--sign -` is the interim self-sign, mandatory
	// on Apple Silicon; real Developer-ID signing + notarization is T4. --deep
	// signs nested code; --force replaces any prior signature (idempotent re-run).
	await $`codesign --sign - --force --deep ${appDir}`;

	// Wrap the staging dir into a compressed (UDZO) .dmg. -ov overwrites an
	// existing image so a re-run is idempotent. detachStaleAttachments covers a
	// leaked mount of our own image; the observed EBUSY had no such mount, so
	// that cause is ruled out and the source tree is the leading suspect.
	await rm(args.out, { force: true });
	await detachStaleAttachments({ imagePath: args.out, volumeName: "Compass" });
	const created =
		await $`hdiutil create -volname Compass -srcfolder ${stageRoot} -ov -format UDZO ${args.out}`
			.quiet()
			.nothrow();
	if (created.exitCode !== 0) {
		// Never retry: the holder is not identified yet, and a retry would destroy
		// the evidence this probe exists to collect.
		throw new Error(
			await reportCreateFailure(
				formatCreateFailure(created.exitCode, created.stderr.toString()),
				() => diagnoseBusy(stageRoot),
				(line) => console.error(line),
			),
		);
	}

	console.log(`macos-bundle: wrote ${args.out}`);
}

if (import.meta.main) {
	await main();
}
