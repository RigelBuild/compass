#!/usr/bin/env bun
// Mint the compass-runner `:vX.Y.Z` release tag by copying an already-published
// `:git-<sha12>` manifest: a registry-side write, never a build, never `:latest`.
// The source is the newest first-parent ancestor of the release sha that has a
// published image (see retag-core.ts for why that is the release's bytes).
//
// Needs `git` (full history) and `skopeo` on PATH. The runner package is
// private, so every call reads REGISTRY_AUTH_FILE (the file `skopeo login` wrote).
//
// Usage:
//   bun tools/runner-image/retag.ts --repo <ghcr.io/owner/name> \
//     --tag <vX.Y.Z> --release-sha <40-hex sha>

import { appendFileSync } from "node:fs";
import {
	assertCoherent,
	EXIT,
	MAX_WALK,
	manifestIdentity,
	type ProbeResult,
	RetagError,
	releaseTag,
	resolveAncestor,
	sha12,
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

/** The first MAX_WALK first-parent ancestors of `sha`, newest first. */
async function firstParentAncestors(sha: string): Promise<string[]> {
	const result = await run([
		"git",
		"rev-list",
		"--first-parent",
		`--max-count=${MAX_WALK}`,
		sha,
	]);
	if (result.exitCode !== 0) {
		throw new RetagError(
			`git rev-list ${sha} failed (exit ${result.exitCode}); the walk needs full history (fetch-depth: 0): ${result.stderr.trim()}`,
			EXIT.usage,
		);
	}
	return result.stdout.split("\n").filter((line) => line.length > 0);
}

function arg(name: string): string | undefined {
	const i = process.argv.indexOf(`--${name}`);
	return i === -1 ? undefined : process.argv[i + 1];
}

async function main(): Promise<number> {
	const repo = arg("repo");
	const tag = arg("tag");
	const releaseSha = arg("release-sha");
	if (!repo || tag === undefined || releaseSha === undefined) {
		console.error(
			"usage: bun tools/runner-image/retag.ts --repo <ghcr.io/owner/name> --tag <vX.Y.Z> --release-sha <sha>",
		);
		return EXIT.usage;
	}
	const target = `${repo}:${releaseTag(tag)}`;
	// Fail on a short or ref-shaped sha before git resolves it to something else.
	sha12(releaseSha);

	const resolved = await resolveAncestor(
		await firstParentAncestors(releaseSha),
		(short) =>
			run(["skopeo", "inspect", "--raw", `docker://${repo}:git-${short}`]),
	);
	const source = manifestIdentity(resolved.raw);
	console.log(
		`resolved source image ${repo}:git-${resolved.sha12} (walk position ${resolved.position}) = ${source.manifestDigest}`,
	);

	// Copy BY DIGEST so a re-push of the build tag between the probe and the
	// copy cannot change the source. --preserve-digests fails the copy rather
	// than let skopeo rewrite the manifest into different bytes.
	await skopeo([
		"copy",
		"--preserve-digests",
		`docker://${repo}@${source.manifestDigest}`,
		`docker://${target}`,
	]);

	const minted = manifestIdentity(
		await skopeo(["inspect", "--raw", `docker://${target}`]),
	);
	assertCoherent(source, minted);
	console.log(
		`verified: ${target} = ${repo}@${minted.manifestDigest} (from :git-${resolved.sha12})`,
	);

	const output = process.env.GITHUB_OUTPUT;
	if (output) {
		appendFileSync(
			output,
			`image_digest=${minted.manifestDigest}\nresolved_sha12=${resolved.sha12}\n`,
		);
	}
	return 0;
}

try {
	process.exit(await main());
} catch (err) {
	if (err instanceof RetagError) {
		console.error(`::error::runner-image retag: ${err.message}`);
		process.exit(err.code);
	}
	throw err;
}
