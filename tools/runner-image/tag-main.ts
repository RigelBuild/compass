#!/usr/bin/env bun
// Re-tag both lanes' newest published images by digest, without rebuilding.

import { appendFileSync } from "node:fs";
import {
	assertCoherent,
	copyArgv,
	EXIT,
	type ImageLane,
	inspectArgv,
	MAX_WALK,
	mainTag,
	manifestIdentity,
	type ProbeResult,
	RetagError,
	resolveMainPair,
	sha12,
	splitNulPaths,
	tagMainPair,
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

async function git(args: readonly string[]): Promise<string> {
	const result = await run(["git", ...args]);
	if (result.exitCode !== 0) {
		throw new RetagError(
			`git ${args.join(" ")} failed (exit ${result.exitCode}): ${result.stderr.trim()}`,
			EXIT.usage,
		);
	}
	return result.stdout;
}

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

function arg(name: string): string | undefined {
	const index = process.argv.indexOf(`--${name}`);
	return index === -1 ? undefined : process.argv[index + 1];
}

async function main(): Promise<number> {
	const sha = arg("sha");
	const repos: Record<ImageLane, string | undefined> = {
		server: arg("server-repo"),
		runner: arg("runner-repo"),
	};
	const authFile = process.env.REGISTRY_AUTH_FILE;
	if (!sha || !repos.server || !repos.runner || !authFile) {
		console.error(
			"usage: bun tools/runner-image/tag-main.ts --sha <40-hex> --server-repo <repo> --runner-repo <repo> (REGISTRY_AUTH_FILE is required)",
		);
		return EXIT.usage;
	}
	sha12(sha);
	const firstParentCount = Number(
		(await git(["rev-list", "--count", "--first-parent", sha])).trim(),
	);
	const tag = mainTag(firstParentCount, sha);
	const ancestors = (
		await git(["rev-list", "--first-parent", `--max-count=${MAX_WALK}`, sha])
	)
		.split("\n")
		.filter((line) => line.length > 0);
	const closurePaths = {
		server: process.env.SERVER_IMAGE_CLOSURE_PATHS ?? "",
		runner: process.env.RUNNER_IMAGE_CLOSURE_PATHS ?? "",
	};
	await tagMainPair({
		resolve: () =>
			resolveMainPair({
				ancestors,
				probe: {
					server: async (short) =>
						run(["skopeo", ...inspectArgv(`${repos.server}:git-${short}`)]),
					runner: async (short) =>
						run(["skopeo", ...inspectArgv(`${repos.runner}:git-${short}`)]),
				},
				changedSince: async (commit) =>
					splitNulPaths(
						await git([
							"diff",
							"-z",
							"--no-renames",
							"--name-only",
							commit,
							sha,
						]),
					),
				closurePaths,
			}),
		write: async (lane, source) => {
			const repo = repos[lane];
			if (!repo) throw new RetagError(`missing ${lane} repository`, EXIT.usage);
			const target = `${repo}:${tag}`;
			const sourceIdentity = manifestIdentity(source.raw);
			await skopeo(
				copyArgv(`${repo}@${sourceIdentity.manifestDigest}`, target, authFile),
			);
			const minted = manifestIdentity(await skopeo(inspectArgv(target)));
			assertCoherent(sourceIdentity, minted);
			console.log(
				`verified ${lane}: ${target} = ${repo}@${minted.manifestDigest} (from :git-${source.sha12})`,
			);
			const summary = process.env.GITHUB_STEP_SUMMARY;
			if (summary) {
				appendFileSync(
					summary,
					`${lane} main image: \`${target}\` = \`${repo}@${minted.manifestDigest}\` (from :git-${source.sha12})\n`,
				);
			}
		},
	});
	return 0;
}

try {
	process.exit(await main());
} catch (err) {
	if (err instanceof RetagError) {
		console.error(`::error::tag main images: ${err.message}`);
		process.exit(err.code);
	}
	throw err;
}
