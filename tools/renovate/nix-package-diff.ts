#!/usr/bin/env bun
import { execFileSync } from "node:child_process";
import {
	copyFileSync,
	mkdtempSync,
	readFileSync,
	rmSync,
	writeFileSync,
} from "node:fs";
import { tmpdir } from "node:os";
import { join, resolve } from "node:path";
import {
	type CLOSURE_TARGETS,
	type DiffArtifact,
	diffVersions,
	type NameVersion,
	type PackageChange,
	parseArtifact,
	parseDevenvEval,
	parsePathInfoVersions,
	renderSection,
	sectionBudget,
	splitDrvName,
	type TargetResult,
	targetsFor,
	upsertSection,
} from "./nix-package-diff.core.ts";

const repoRoot = resolve(import.meta.dir, "../..");
const localModule = join(import.meta.dir, "nix-package-diff.local.nix");
const ARTIFACT_PATH = "diff.json";

function requiredEnv(name: string): string {
	const value = process.env[name];
	if (!value) throw new Error(`${name} is required.`);
	return value;
}

function command(
	program: string,
	args: readonly string[],
	cwd: string,
): string {
	return execFileSync(program, [...args], {
		cwd,
		encoding: "utf8",
		stdio: ["ignore", "pipe", "inherit"],
	}).trim();
}

function storePaths(json: string): NameVersion[] {
	return parsePathInfoVersions(json);
}

function buildOutPath(output: string, description: string): string {
	const path = output
		.split("\n")
		.map((line) => line.trim())
		.filter(Boolean)
		.at(-1);
	if (!path?.startsWith("/nix/store/")) {
		throw new Error(`${description} did not print a Nix store path.`);
	}
	return path;
}

function parseJsonObject(
	raw: string,
	description: string,
): Record<string, unknown> {
	let value: unknown;
	try {
		value = JSON.parse(raw);
	} catch (error) {
		throw new Error(`${description} returned invalid JSON: ${String(error)}`);
	}
	if (typeof value !== "object" || value === null || Array.isArray(value)) {
		throw new Error(`${description} returned an invalid JSON object.`);
	}
	return Object.fromEntries(Object.entries(value));
}

function runCollect(): number {
	const prNumber = requiredEnv("PR_NUMBER");
	const headSha = requiredEnv("HEAD_SHA");
	const repo = requiredEnv("GH_REPO");
	if (!/^[0-9]+$/.test(prNumber)) throw new Error("PR_NUMBER must be numeric.");
	if (!/^[a-fA-F0-9]{40}$/.test(headSha))
		throw new Error("HEAD_SHA must be a 40-hex SHA.");
	const changedFiles = command(
		"gh",
		[
			"api",
			"--paginate",
			`repos/${repo}/pulls/${prNumber}/files`,
			"--jq",
			".[].filename",
		],
		repoRoot,
	)
		.split("\n")
		.filter(Boolean);
	const selectedTargets = targetsFor(changedFiles);
	const artifact: DiffArtifact = { headSha, skip: false, results: [] };
	const temporary = mkdtempSync(join(tmpdir(), "nix-package-diff-"));
	const baseWorktree = join(temporary, "base");
	const cacheOutput = process.env.GITHUB_OUTPUT;
	let hasErrors = true;
	try {
		hasErrors = collectTargets(
			selectedTargets,
			prNumber,
			headSha,
			repo,
			temporary,
			baseWorktree,
			artifact,
		);
	} finally {
		writeFileSync(ARTIFACT_PATH, `${JSON.stringify(artifact, null, 2)}\n`);
		if (cacheOutput)
			writeFileSync(cacheOutput, "collected=true\n", { flag: "a" });
	}
	return hasErrors ? 1 : 0;
}

function collectTargets(
	targets: readonly (typeof CLOSURE_TARGETS)[number][],
	prNumber: string,
	headSha: string,
	repo: string,
	temporary: string,
	baseWorktree: string,
	artifact: DiffArtifact,
): boolean {
	let hasErrors = false;
	let worktreeAdded = false;
	try {
		const baseSha = command(
			"gh",
			["api", `repos/${repo}/pulls/${prNumber}`, "--jq", ".base.sha"],
			repoRoot,
		);
		const mergeBase = command(
			"git",
			["merge-base", headSha, baseSha],
			repoRoot,
		);
		command(
			"git",
			["worktree", "add", "--detach", baseWorktree, mergeBase],
			repoRoot,
		);
		worktreeAdded = true;
		for (const target of targets) {
			const result = collectTarget(
				target,
				resolve(baseWorktree, target.dir),
				resolve(repoRoot, target.dir),
				temporary,
			);
			artifact.results.push(result);
			if ("error" in result) hasErrors = true;
		}
	} catch (error) {
		console.error(
			`nix-package-diff: unable to prepare merge-base worktree: ${String(error)}`,
		);
		if (error instanceof Error) console.error(error.message);
		hasErrors = true;
	} finally {
		try {
			if (worktreeAdded)
				command(
					"git",
					["worktree", "remove", "--force", baseWorktree],
					repoRoot,
				);
		} finally {
			rmSync(temporary, { recursive: true, force: true });
		}
	}
	return hasErrors;
}
function collectTarget(
	target: (typeof CLOSURE_TARGETS)[number],
	baseDir: string,
	headDir: string,
	temporary: string,
): TargetResult {
	let baseCli: string;
	let headCli: string;
	try {
		copyFileSync(localModule, join(baseDir, "devenv.local.nix"));
		copyFileSync(localModule, join(headDir, "devenv.local.nix"));
		baseCli = resolveDevenv(baseDir, target, "base", temporary);
		headCli = resolveDevenv(headDir, target, "head", temporary);
	} catch (error) {
		console.error(`nix-package-diff: ${target.label}: ${String(error)}`);
		return { label: target.label, error: "eval-failed" };
	}
	let direct: PackageChange[];
	try {
		direct = diffVersions(
			evaluateDirect(baseCli, baseDir),
			evaluateDirect(headCli, headDir),
		);
	} catch (error) {
		console.error(`nix-package-diff: ${target.label}: ${String(error)}`);
		return { label: target.label, error: "eval-failed" };
	}
	let headClosure: string;
	let headPathInfo: NameVersion[];
	try {
		headClosure = evaluateClosure(headCli, target, headDir, "head");
		headPathInfo = pathInfo(headClosure);
	} catch (error) {
		const code =
			error instanceof Error && error.message.startsWith("path-info-failed:")
				? "path-info-failed"
				: "build-failed";
		console.error(`nix-package-diff: ${target.label}: ${String(error)}`);
		return { label: target.label, error: code };
	}
	try {
		command("nix", ["store", "delete", headClosure], repoRoot);
		command("nix", ["store", "gc"], repoRoot);
		const baseClosure = evaluateClosure(baseCli, target, baseDir, "base");
		const basePathInfo = pathInfo(baseClosure);
		return {
			label: target.label,
			direct,
			transitive: diffVersions(basePathInfo, headPathInfo),
		};
	} catch (error) {
		const code =
			error instanceof Error && error.message.startsWith("path-info-failed:")
				? "path-info-failed"
				: "build-failed";
		console.error(`nix-package-diff: ${target.label}: ${String(error)}`);
		return { label: target.label, error: code };
	}
}

function resolveDevenv(
	directory: string,
	target: (typeof CLOSURE_TARGETS)[number],
	side: string,
	temporary: string,
): string {
	const lock = join(directory, "devenv.lock");
	const reference = command(
		"bun",
		[
			join(repoRoot, "tools/toolchain/devenv-cli/index.ts"),
			"--lock",
			lock,
			"--mode",
			"flakeref",
		],
		directory,
	);
	const outLink = join(
		temporary,
		`devenv-${target.label.replaceAll(" ", "-")}-${side}`,
	);
	command("nix", ["build", "--out-link", outLink, reference], directory);
	return outLink;
}

function evaluateDirect(cli: string, directory: string): NameVersion[] {
	try {
		const output = command(
			join(cli, "bin/devenv"),
			["eval", "packageDiff.direct"],
			directory,
		);
		return parseDevenvEval(output, "packageDiff.direct").map(splitDrvName);
	} catch (error) {
		throw new Error(`eval-failed: ${String(error)}`);
	}
}

function evaluateClosure(
	cli: string,
	target: (typeof CLOSURE_TARGETS)[number],
	directory: string,
	side: string,
): string {
	let output: string;
	if (target.closure.kind === "build") {
		output = command(
			join(cli, "bin/devenv"),
			["build", target.closure.attr],
			directory,
		);
	} else {
		output = command(
			join(cli, "bin/devenv"),
			["container", "build", target.closure.name],
			directory,
		);
	}
	return buildOutPath(output, `${target.label} ${side} closure build`);
}

function pathInfo(root: string): NameVersion[] {
	try {
		const output = command(
			"nix",
			["path-info", "-r", "--json", "--json-format", "1", root],
			repoRoot,
		);
		return storePaths(output);
	} catch (error) {
		throw new Error(`path-info-failed: ${String(error)}`);
	}
}

function validPullRequest(
	value: Record<string, unknown>,
	repo: string,
): boolean {
	const head = parseJsonObject(JSON.stringify(value.head), "pull request head");
	const headRepo = parseJsonObject(
		JSON.stringify(head.repo),
		"pull request head repository",
	);
	return (
		value.state === "open" &&
		typeof head.ref === "string" &&
		head.ref.startsWith("renovate/") &&
		typeof headRepo.full_name === "string" &&
		headRepo.full_name.toLowerCase() === repo.toLowerCase() &&
		typeof head.sha === "string" &&
		/^[a-fA-F0-9]{40}$/.test(head.sha) &&
		typeof value.body === "string"
	);
}

function parsePullRequestNumbers(raw: string): number {
	let value: unknown;
	try {
		value = JSON.parse(raw);
	} catch (error) {
		throw new Error(`PR_NUMBERS is invalid JSON: ${String(error)}`);
	}
	if (!Array.isArray(value) || value.length !== 1) {
		throw new Error("PR_NUMBERS must name exactly one pull request.");
	}
	const number = value[0];
	if (typeof number !== "number" || !Number.isInteger(number) || number < 1) {
		throw new Error("PR_NUMBERS must contain a positive integer.");
	}
	return number;
}

function currentPullRequestHeadSha(
	pullRequest: Record<string, unknown>,
): string | undefined {
	const head = pullRequest.head;
	if (
		typeof head !== "object" ||
		head === null ||
		Array.isArray(head) ||
		!("sha" in head) ||
		typeof head.sha !== "string"
	) {
		return undefined;
	}
	return head.sha;
}

function runPublish(): number {
	const artifactFile = requiredEnv("ARTIFACT_FILE");
	const runHeadSha = requiredEnv("RUN_HEAD_SHA");
	const runUrl = requiredEnv("RUN_URL");
	const repo = requiredEnv("GH_REPO");
	const prNumber = parsePullRequestNumbers(requiredEnv("PR_NUMBERS"));
	const prApiPath = `repos/${repo}/pulls/${prNumber}`;
	const pull = parseJsonObject(
		command("gh", ["api", prApiPath], repoRoot),
		"pull request",
	);
	if (!validPullRequest(pull, repo)) {
		throw new Error(
			"Pull request is closed, cross-repository, or not a Renovate branch.",
		);
	}
	if (currentPullRequestHeadSha(pull) !== runHeadSha) return 0;
	const artifact = parseArtifact(readFileSync(artifactFile, "utf8"));
	if (artifact.headSha !== runHeadSha) {
		throw new Error("Artifact head SHA does not match the workflow run.");
	}
	if (artifact.skip || artifact.results.length === 0) return 0;
	const freshPull = parseJsonObject(
		command("gh", ["api", prApiPath], repoRoot),
		"pull request",
	);
	if (
		!validPullRequest(freshPull, repo) ||
		currentPullRequestHeadSha(freshPull) !== runHeadSha
	)
		return 0;
	if (typeof freshPull.body !== "string")
		throw new Error("Pull request body is not a string.");
	const section = renderSection(
		artifact,
		sectionBudget(freshPull.body),
		runUrl,
	);
	const updatedBody = upsertSection(freshPull.body, section);
	if (updatedBody.length > 65_536) {
		throw new Error("Rendered pull request body exceeds 65536 characters.");
	}
	if (updatedBody === freshPull.body) return 0;
	const bodyFile = join(tmpdir(), `nix-package-diff-body-${process.pid}.md`);
	try {
		writeFileSync(bodyFile, updatedBody);
		command(
			"gh",
			["api", "--method", "PATCH", prApiPath, "-F", `body=@${bodyFile}`],
			repoRoot,
		);
	} finally {
		rmSync(bodyFile, { force: true });
	}
	return 0;
}

const commandName = process.argv[2];
try {
	const exitCode =
		commandName === "collect"
			? runCollect()
			: commandName === "publish"
				? runPublish()
				: (() => {
						throw new Error("Usage: nix-package-diff.ts <collect|publish>");
					})();
	process.exit(exitCode);
} catch (error) {
	console.error(error instanceof Error ? error.message : String(error));
	process.exit(1);
}
