#!/usr/bin/env bun
// Decide whether a main push must publish a lane image; writes should_publish to GITHUB_OUTPUT.

import { appendFileSync } from "node:fs";
import { imageLaneConfig } from "./image-lane-core.ts";
import { decidePublish } from "./publish-gate-core.ts";
import {
	EXIT,
	inspectArgv,
	MAX_WALK,
	type ProbeResult,
	RetagError,
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

function arg(name: string): string | undefined {
	const index = process.argv.indexOf(`--${name}`);
	return index === -1 ? undefined : process.argv[index + 1];
}

async function main(): Promise<number> {
	const config = imageLaneConfig(arg("lane"));
	const repo = arg("repo");
	const sha = arg("sha");
	const output = process.env.GITHUB_OUTPUT;
	if (!config || !repo || !sha || !output) {
		console.error(
			"usage: bun tools/runner-image/publish-gate.ts --lane <runner|server> --repo <repo> --sha <40-hex> (GITHUB_OUTPUT is required)",
		);
		return EXIT.usage;
	}
	sha12(sha);
	const ancestors = (
		await git(["rev-list", "--first-parent", `--max-count=${MAX_WALK}`, sha])
	)
		.split("\n")
		.filter((line) => line.length > 0);
	const decision = await decidePublish({
		ancestors,
		probe: (short) => run(["skopeo", ...inspectArgv(`${repo}:git-${short}`)]),
		changedSince: async (commit) =>
			splitNulPaths(
				await git(["diff", "-z", "--no-renames", "--name-only", commit, sha]),
			),
		closurePaths: process.env[config.closureEnv] ?? "",
		lane: config.lane,
	});
	appendFileSync(output, `should_publish=${decision.shouldPublish}\n`);
	console.log(
		`${config.lane} publish gate: should_publish=${decision.shouldPublish} (${decision.reason})`,
	);
	return 0;
}

try {
	process.exit(await main());
} catch (err) {
	if (err instanceof RetagError) {
		console.error(`::error::publish gate: ${err.message}`);
		process.exit(err.code);
	}
	throw err;
}
