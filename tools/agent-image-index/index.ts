#!/usr/bin/env bun

import { createHash } from "node:crypto";
import { appendFileSync, writeFileSync } from "node:fs";

export type CommandResult = {
	exitCode: number;
	stdout: string;
	stderr: string;
};

export type CommandRunner = (command: string[]) => Promise<CommandResult>;

export type PublishInput = {
	repo: string;
	sha: string;
	authFile: string;
	runnerTemp: string;
};

export type PublishResult = { stdout: string[]; stderr: string[] };

type IndexMember = {
	digest: string;
	platform: { os: string; architecture: string };
	artifactType?: string;
};

type ImageIndex = {
	mediaType: string | undefined;
	manifests: IndexMember[];
};

function imageRef(repo: string): string {
	return `docker://${repo}`;
}

function localIndexRef(repo: string, sha12: string): string {
	const name = repo.slice(repo.lastIndexOf("/") + 1);
	return `localhost/${name}:${sha12}`;
}
const EXPECTED_PLATFORMS = "linux/amd64,linux/arm64";
const INDEX_MEDIA_TYPES = [
	"application/vnd.oci.image.index.v1+json",
	"application/vnd.docker.distribution.manifest.list.v2+json",
];
const MANIFEST_UNKNOWN = /(^|[^a-z0-9_])manifest unknown([^a-z0-9_]|$)/i;

class CommandFailure extends Error {
	readonly stderr: string;

	constructor(command: string[], result: CommandResult) {
		const stderr = result.stderr.trimEnd();
		super(
			stderr || `${command.join(" ")} exited with status ${result.exitCode}`,
		);
		this.stderr = stderr;
	}
}

class PublishFailure extends Error {
	constructor(
		cause: unknown,
		readonly stdout: string[],
		readonly stderr: string[],
	) {
		super(cause instanceof Error ? cause.message : String(cause), { cause });
	}
}

async function runChecked(
	run: CommandRunner,
	command: string[],
): Promise<string> {
	const result = await run(command);
	if (result.exitCode !== 0) throw new CommandFailure(command, result);
	return result.stdout;
}

function sha256(raw: string): string {
	return `sha256:${createHash("sha256").update(raw).digest("hex")}`;
}

function requireDigest(digest: string, message: string): void {
	if (!/^sha256:[0-9a-f]{64}$/.test(digest)) {
		throw new Error(`::error::${message}: ${digest}`);
	}
}

function isRecord(value: unknown): value is Record<string, unknown> {
	return typeof value === "object" && value !== null && !Array.isArray(value);
}

function parseIndex(raw: string, description: string): ImageIndex {
	let parsed: unknown;
	try {
		parsed = JSON.parse(raw);
	} catch (error) {
		const detail = error instanceof Error ? error.message : String(error);
		throw new Error(`invalid ${description}: ${detail}`);
	}
	if (!isRecord(parsed) || !Array.isArray(parsed.manifests)) {
		throw new Error(`invalid ${description}: manifests is not an array`);
	}
	const manifests: IndexMember[] = parsed.manifests.map((member) => {
		if (
			!isRecord(member) ||
			typeof member.digest !== "string" ||
			!isRecord(member.platform) ||
			typeof member.platform.os !== "string" ||
			typeof member.platform.architecture !== "string" ||
			(member.artifactType !== undefined &&
				typeof member.artifactType !== "string")
		) {
			throw new Error(`invalid ${description}: malformed manifest member`);
		}
		return {
			digest: member.digest,
			platform: {
				os: member.platform.os,
				architecture: member.platform.architecture,
			},
			...(typeof member.artifactType === "string"
				? { artifactType: member.artifactType }
				: {}),
		};
	});
	return {
		mediaType:
			typeof parsed.mediaType === "string" ? parsed.mediaType : undefined,
		manifests,
	};
}

function platforms(index: ImageIndex): string {
	return index.manifests
		.map(({ platform }) => `${platform.os}/${platform.architecture}`)
		.sort()
		.join(",");
}

function requireExpectedPlatforms(index: ImageIndex, prefix: string): void {
	const actual = platforms(index);
	if (actual !== EXPECTED_PLATFORMS) {
		throw new Error(
			`::error::${prefix} platform set is '${actual}', expected ${EXPECTED_PLATFORMS}`,
		);
	}
}

function inspectCommand(tag: string, ...args: string[]): string[] {
	return ["skopeo", "inspect", ...args, "--no-creds", tag];
}

function destinationProbeCommand(authFile: string, tag: string): string[] {
	return ["skopeo", "inspect", "--raw", "--authfile", authFile, tag];
}

function isManifestUnknown(message: string): boolean {
	return MANIFEST_UNKNOWN.test(message);
}

async function resolveArchDigests(
	repo: string,
	run: CommandRunner,
	sha12: string,
): Promise<[string, string]> {
	const ref = imageRef(repo);
	const amd64Raw = await runChecked(
		run,
		inspectCommand(`${ref}:git-${sha12}-amd64`, "--raw"),
	);
	const arm64Raw = await runChecked(
		run,
		inspectCommand(`${ref}:git-${sha12}-arm64`, "--raw"),
	);
	const amd64Digest = sha256(amd64Raw);
	const arm64Digest = sha256(arm64Raw);
	for (const digest of [amd64Digest, arm64Digest]) {
		requireDigest(digest, "invalid per-arch manifest digest");
	}
	return [amd64Digest, arm64Digest];
}

async function composeLocalIndex(
	repo: string,
	authFile: string,
	runnerTemp: string,
	sha12: string,
	[amd64Digest, arm64Digest]: [string, string],
	run: CommandRunner,
): Promise<{ localList: string; localDigest: string }> {
	const ref = imageRef(repo);
	const localList = localIndexRef(repo, sha12);
	const anonymousAuthFile = `${runnerTemp}/anonymous-auth.json`;
	writeFileSync(anonymousAuthFile, "{}");
	await runChecked(run, ["podman", "manifest", "create", localList]);
	for (const [digest, architecture] of [
		[amd64Digest, "amd64"],
		[arm64Digest, "arm64"],
	] as const) {
		await runChecked(run, [
			"podman",
			"manifest",
			"add",
			"--authfile",
			anonymousAuthFile,
			"--os",
			"linux",
			"--arch",
			architecture,
			localList,
			`${ref}@${digest}`,
		]);
	}
	const localRaw = await runChecked(run, [
		"podman",
		"manifest",
		"inspect",
		localList,
	]);
	const localIndex = parseIndex(localRaw, "local composed index");
	if (localIndex.manifests.length !== 2) {
		throw new Error(
			`::error::composed index has ${localIndex.manifests.length} members, expected exactly 2`,
		);
	}
	requireExpectedPlatforms(localIndex, "composed");
	const digestFile = `${runnerTemp}/local-index.digest`;
	await runChecked(run, [
		"podman",
		"manifest",
		"push",
		"--format",
		"oci",
		"--digestfile",
		digestFile,
		"--authfile",
		authFile,
		localList,
		`oci:${runnerTemp}/local-index:index`,
	]);
	const localDigest = (await runChecked(run, ["cat", digestFile])).trim();
	requireDigest(localDigest, "podman returned an invalid local index digest");
	return { localList, localDigest };
}

async function guardImmutableTag(
	repo: string,
	authFile: string,
	localList: string,
	sha12: string,
	localDigest: string,
	run: CommandRunner,
): Promise<PublishResult> {
	const tag = `${imageRef(repo)}:git-${sha12}`;
	const result = await run(destinationProbeCommand(authFile, tag));
	const output: PublishResult = {
		stdout: [],
		stderr: result.stderr.trimEnd() ? [result.stderr.trimEnd()] : [],
	};
	if (result.exitCode === 0) {
		const remoteDigest = sha256(result.stdout);
		if (remoteDigest !== localDigest) {
			throw new Error(
				`::error::immutable :git-${sha12} index differs: remote=${remoteDigest} local=${localDigest}`,
			);
		}
		output.stdout.push(`immutable index already matches: ${localDigest}`);
		return output;
	}
	if (isManifestUnknown(result.stderr)) {
		await runChecked(run, [
			"podman",
			"manifest",
			"push",
			"--format",
			"oci",
			"--authfile",
			authFile,
			localList,
			tag,
		]);
		return output;
	}
	throw new Error(
		`::error::ambiguous inspect failure probing :git-${sha12}; refusing to push: ${result.stderr.trimEnd()}`,
	);
}

async function findNewerIndex(
	repo: string,
	sha: string,
	run: CommandRunner,
): Promise<string> {
	const ancestor = await run([
		"git",
		"merge-base",
		"--is-ancestor",
		sha,
		"origin/main",
	]);
	if (ancestor.exitCode !== 0) {
		throw new Error(
			`::error::${sha} is not on origin/main (rewritten?); refusing to move :latest`,
		);
	}
	const commits = await runChecked(run, [
		"git",
		"rev-list",
		"--first-parent",
		`${sha}..origin/main`,
	]);
	for (const commit of commits.split("\n")) {
		if (!commit) continue;
		const newerSha12 = commit.slice(0, 12);
		const probe = await run(
			inspectCommand(`${imageRef(repo)}:git-${newerSha12}`, "--raw"),
		);
		if (probe.exitCode === 0) return newerSha12;
		if (!isManifestUnknown(probe.stderr)) {
			throw new Error(
				`::error::ambiguous inspect failure probing newer :git-${newerSha12}; refusing to move :latest: ${probe.stderr.trimEnd()}`,
			);
		}
	}
	return "";
}

async function verifyArtifactConfig(
	memberRef: string,
	member: IndexMember,
	run: CommandRunner,
): Promise<void> {
	const manifestRaw = await runChecked(run, inspectCommand(memberRef, "--raw"));
	let manifest: unknown;
	try {
		manifest = JSON.parse(manifestRaw);
	} catch (error) {
		const detail = error instanceof Error ? error.message : String(error);
		throw new Error(
			`invalid artifact manifest for ${member.digest}: ${detail}`,
		);
	}
	if (!isRecord(manifest) || !isRecord(manifest.config)) {
		throw new Error(`invalid artifact config descriptor for ${member.digest}`);
	}
	const config = manifest.config;
	if (
		manifest.artifactType !== member.artifactType ||
		config.mediaType !== "application/vnd.oci.empty.v1+json" ||
		config.digest !== sha256("{}") ||
		config.size !== 2
	) {
		throw new Error(`invalid artifact config descriptor for ${member.digest}`);
	}
}

async function verifyImageMemberConfig(
	memberRef: string,
	member: IndexMember,
	run: CommandRunner,
): Promise<void> {
	const configRaw = await runChecked(
		run,
		inspectCommand(memberRef, "--config"),
	);
	let config: unknown;
	try {
		config = JSON.parse(configRaw);
	} catch (error) {
		const detail = error instanceof Error ? error.message : String(error);
		throw new Error(`invalid member config for ${member.digest}: ${detail}`);
	}
	if (
		!isRecord(config) ||
		typeof config.os !== "string" ||
		typeof config.architecture !== "string"
	) {
		throw new Error(`invalid member config for ${member.digest}`);
	}
	if (
		config.os !== member.platform.os ||
		config.architecture !== member.platform.architecture
	) {
		throw new Error(
			`::error::member ${member.digest} config is ${config.os}/${config.architecture}, declared ${member.platform.os}/${member.platform.architecture}`,
		);
	}
}

async function verifyMemberConfig(
	repo: string,
	member: IndexMember,
	run: CommandRunner,
): Promise<void> {
	const memberRef = `${imageRef(repo)}@${member.digest}`;
	if (member.artifactType !== undefined) {
		await verifyArtifactConfig(memberRef, member, run);
		return;
	}
	await verifyImageMemberConfig(memberRef, member, run);
}

async function verifyPublishedIndex(
	repo: string,
	sha12: string,
	localDigest: string,
	archDigests: [string, string],
	run: CommandRunner,
): Promise<{ gitDigest: string; index: ImageIndex }> {
	const gitRaw = await runChecked(
		run,
		inspectCommand(`${imageRef(repo)}:git-${sha12}`, "--raw"),
	);
	const gitDigest = sha256(gitRaw);
	if (gitDigest !== localDigest) {
		throw new Error(
			`::error::pushed :git-${sha12} index changed: local=${localDigest} remote=${gitDigest}`,
		);
	}
	const index = parseIndex(gitRaw, "published index");
	if (!index.mediaType || !INDEX_MEDIA_TYPES.includes(index.mediaType)) {
		throw new Error(
			`::error::unexpected index mediaType: ${index.mediaType ?? ""}`,
		);
	}
	requireExpectedPlatforms(index, "published");
	const actualDigests = index.manifests
		.map(({ digest }) => digest)
		.sort()
		.join(",");
	const expectedDigests = [...archDigests].sort().join(",");
	if (actualDigests !== expectedDigests) {
		throw new Error(
			`::error::index member digest set differs: got=${actualDigests} expected=${expectedDigests}`,
		);
	}
	for (const member of index.manifests) {
		await verifyMemberConfig(repo, member, run);
	}
	return { gitDigest, index };
}

async function verifyLatest(
	repo: string,
	sha12: string,
	newerIndex: string,
	gitDigest: string,
	run: CommandRunner,
): Promise<string> {
	const expectedTag = `git-${newerIndex || sha12}`;
	let expectedDigest = gitDigest;
	if (newerIndex) {
		const newerRaw = await runChecked(
			run,
			inspectCommand(`${imageRef(repo)}:${expectedTag}`, "--raw"),
		);
		expectedDigest = sha256(newerRaw);
	}
	const latestRaw = await runChecked(
		run,
		inspectCommand(`${imageRef(repo)}:latest`, "--raw"),
	);
	const latestDigest = sha256(latestRaw);
	if (latestDigest !== expectedDigest) {
		throw new Error(
			`::error::latest index differs from newest immutable tag: :${expectedTag}=${expectedDigest} :latest=${latestDigest}`,
		);
	}
	return expectedTag;
}

/** Compose and publish an OCI index using only the injected command runner. */
export async function publishImageIndex(
	input: PublishInput,
	run: CommandRunner,
): Promise<PublishResult> {
	const { repo, sha, authFile, runnerTemp } = input;
	const sha12 = sha.slice(0, 12);
	const result: PublishResult = { stdout: [], stderr: [] };
	try {
		const archDigests = await resolveArchDigests(repo, run, sha12);
		const { localList, localDigest } = await composeLocalIndex(
			repo,
			authFile,
			runnerTemp,
			sha12,
			archDigests,
			run,
		);
		const guarded = await guardImmutableTag(
			repo,
			authFile,
			localList,
			sha12,
			localDigest,
			run,
		);
		result.stdout.push(...guarded.stdout);
		result.stderr.push(...guarded.stderr);
		const newerIndex = await findNewerIndex(repo, sha, run);
		if (!newerIndex) {
			await runChecked(run, [
				"podman",
				"manifest",
				"push",
				"--format",
				"oci",
				"--authfile",
				authFile,
				localList,
				`${imageRef(repo)}:latest`,
			]);
		} else {
			result.stderr.push(
				`::warning::newer :git-${newerIndex} is published; leaving :latest to it`,
			);
		}
		const { gitDigest } = await verifyPublishedIndex(
			repo,
			sha12,
			localDigest,
			archDigests,
			run,
		);
		const expectedTag = await verifyLatest(
			repo,
			sha12,
			newerIndex,
			gitDigest,
			run,
		);
		result.stdout.push(
			`verified OCI index ${gitDigest}: ${EXPECTED_PLATFORMS}; :latest matches :${expectedTag}`,
		);
		const summary = process.env.GITHUB_STEP_SUMMARY;
		if (summary) {
			const lane = repo.slice(repo.lastIndexOf("/") + 1);
			appendFileSync(summary, `${lane} image: \`${repo}@${gitDigest}\`\n`);
		}
		return result;
	} catch (error) {
		if (error instanceof CommandFailure && error.stderr)
			result.stderr.push(error.stderr);
		throw new PublishFailure(error, result.stdout, result.stderr);
	}
}

async function runCommand(command: string[]): Promise<CommandResult> {
	const child = Bun.spawn({ cmd: command, stdout: "pipe", stderr: "pipe" });
	const [stdout, stderr, exitCode] = await Promise.all([
		new Response(child.stdout).text(),
		new Response(child.stderr).text(),
		child.exited,
	]);
	return { exitCode, stdout, stderr };
}

async function main(): Promise<void> {
	const repo = process.env.IMAGE_REPO;
	const sha = process.env.GH_SHA;
	const authFile = process.env.REGISTRY_AUTH_FILE;
	const runnerTemp = process.env.RUNNER_TEMP;
	if (!repo || !sha || !authFile || !runnerTemp) {
		throw new Error(
			"IMAGE_REPO, GH_SHA, REGISTRY_AUTH_FILE, and RUNNER_TEMP are required",
		);
	}
	const result = await publishImageIndex(
		{ repo, sha, authFile, runnerTemp },
		runCommand,
	);
	for (const line of result.stdout) console.log(line);
	for (const line of result.stderr) console.error(line);
}

if (import.meta.main) {
	try {
		await main();
	} catch (error) {
		if (error instanceof PublishFailure) {
			for (const line of error.stdout) console.log(line);
			for (const line of error.stderr) console.error(line);
		}
		console.error(error instanceof Error ? error.message : String(error));
		process.exitCode = 1;
	}
}
