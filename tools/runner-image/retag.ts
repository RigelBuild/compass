#!/usr/bin/env bun
// Copy the newest published closure image at or before the release sha to its
// version tag; a registry write, never a build or `:latest`. Requires git, skopeo,
// the lane closure-path env var, and REGISTRY_AUTH_FILE for writes and copies.
// Usage: bun tools/runner-image/retag.ts --lane <runner|server> --repo <ghcr.io/owner/name> --tag <vX.Y.Z> --release-sha <40-hex sha>
import { appendFileSync } from "node:fs";
import { imageLaneConfig } from "./image-lane-core.ts";
import {
	assertCoherent,
	assertNoClosureChange,
	copyArgv,
	EXIT,
	inspectArgv,
	MAX_WALK,
	manifestIdentity,
	type ProbeResult,
	RetagError,
	releaseTag,
	resolveAncestor,
	sha12,
	splitNulPaths,
} from "./retag-core.ts";

async function run(argv: readonly string[]): Promise<ProbeResult> {
	const proc = Bun.spawn([...argv], { stdout: "pipe", stderr: "pipe" });
	const [stdout, stderr, exitCode] = await Promise.all([
		new Response(proc.stdout).text(),
		new Response(proc.stderr).text(),
		proc.exited,
	]);
	return { exitCode, stdout, stderr };
}

/** A skopeo call that must succeed; any failure is a registry fault. */
async function skopeo(args: readonly string[]): Promise<string> {
	const result = await run(["skopeo", ...args]);
	if (result.exitCode !== 0) {
		throw new RetagError(
			`skopeo ${args.join(" ")} failed (exit ${result.exitCode}): ${result.stderr.trim()}`,
			EXIT.registryFailed,
		);
	}
	return result.stdout;
}

/** A git call that must succeed, returning raw stdout. A failure here is
 * missing history, not a registry fault: the job must check out with fetch-depth 0. */
async function git(args: readonly string[]): Promise<string> {
	const result = await run(["git", ...args]);
	if (result.exitCode !== 0) {
		throw new RetagError(
			`git ${args.join(" ")} failed (exit ${result.exitCode}); the resolver needs full history (fetch-depth: 0): ${result.stderr.trim()}`,
			EXIT.usage,
		);
	}
	return result.stdout;
}

function arg(name: string): string | undefined {
	const i = process.argv.indexOf(`--${name}`);
	return i === -1 ? undefined : process.argv[i + 1];
}

async function main(): Promise<number> {
	const laneConfig = imageLaneConfig(arg("lane"));
	if (!laneConfig) {
		console.error("usage: --lane must be runner or server");
		return EXIT.usage;
	}
	const repo = arg("repo");
	const tag = arg("tag");
	const releaseSha = arg("release-sha");
	const authFile = process.env.REGISTRY_AUTH_FILE;
	if (!repo || tag === undefined || releaseSha === undefined || !authFile) {
		console.error(
			"usage: bun tools/runner-image/retag.ts --lane <runner|server> --repo <ghcr.io/owner/name> --tag <vX.Y.Z> --release-sha <sha> (REGISTRY_AUTH_FILE is required)",
		);
		return EXIT.usage;
	}
	const target = `${repo}:${releaseTag(tag)}`;
	// Fail on a short or ref-shaped sha before git resolves it to something else.
	sha12(releaseSha);

	const ancestors = await git([
		"rev-list",
		"--first-parent",
		`--max-count=${MAX_WALK}`,
		releaseSha,
	]);
	const resolved = await resolveAncestor(
		ancestors.split("\n").filter((line) => line.length > 0),
		(short) => run(["skopeo", ...inspectArgv(`${repo}:git-${short}`)]),
		laneConfig.lane,
	);
	const source = manifestIdentity(resolved.raw);
	console.log(
		`resolved source image ${repo}:git-${resolved.sha12} (walk position ${resolved.position}) = ${source.manifestDigest}`,
	);
	// Before any registry write: the ancestors the walk skipped must carry no
	// closure change, or the resolved image lacks it.
	// --no-renames: a rename prints only its destination, which hides a file
	// moved OUT of the closure. -z keeps odd names whole.
	assertNoClosureChange(
		process.env[laneConfig.closureEnv] ?? "",
		resolved.sha12,
		splitNulPaths(
			await git([
				"diff",
				"-z",
				"--no-renames",
				"--name-only",
				resolved.commit,
				releaseSha,
			]),
		),
		laneConfig.lane,
	);

	// Copy BY DIGEST so a re-push of the build tag between the probe and the
	// copy cannot change the source. --preserve-digests fails the copy rather
	// than let skopeo rewrite the manifest into different bytes.
	await skopeo(copyArgv(`${repo}@${source.manifestDigest}`, target, authFile));

	const minted = manifestIdentity(await skopeo(inspectArgv(target)));
	assertCoherent(source, minted);
	console.log(
		`verified: ${target} = ${repo}@${minted.manifestDigest} (from :git-${resolved.sha12})`,
	);

	const summary = process.env.GITHUB_STEP_SUMMARY;
	if (summary) {
		appendFileSync(
			summary,
			`${laneConfig.lane} release image: \`${target}\` = \`${repo}@${minted.manifestDigest}\` (from :git-${resolved.sha12})\n`,
		);
	}
	return 0;
}

try {
	process.exit(await main());
} catch (err) {
	if (err instanceof RetagError) {
		const lane = imageLaneConfig(arg("lane"))?.lane ?? "image-lane";
		console.error(`::error::${lane} retag: ${err.message}`);
		process.exit(err.code);
	}
	throw err;
}
