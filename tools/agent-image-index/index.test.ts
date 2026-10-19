import { afterAll, describe, expect, test } from "bun:test";
import { createHash } from "node:crypto";
import { mkdtempSync, readFileSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { publishImageIndex } from "./index.ts";

const SHA = "abcdef0123456789abcdef0123456789abcdef01";
const SHA12 = SHA.slice(0, 12);
const AUTH = "/tmp/auth.json";
const TEMP = mkdtempSync(join(tmpdir(), "agent-image-index-"));
const REPO = "ghcr.io/rigelbuild/compass-agent";
function imageRef(repo: string): string {
	return `docker://${repo}`;
}
const REF = imageRef(REPO);

function localDigest(raw: string): string {
	return `sha256:${createHash("sha256").update(raw).digest("hex")}`;
}

const EMPTY_CONFIG_DIGEST = localDigest("{}");
const AMD64_RAW = JSON.stringify({
	schemaVersion: 2,
	mediaType: "application/vnd.oci.image.manifest.v1+json",
	config: {
		mediaType: "application/vnd.oci.image.config.v1+json",
		digest: `sha256:${"c".repeat(64)}`,
		size: 1,
	},
	layers: [],
});
const ARM64_RAW = JSON.stringify({
	schemaVersion: 2,
	mediaType: "application/vnd.oci.image.manifest.v1+json",
	config: {
		mediaType: "application/vnd.oci.image.config.v1+json",
		digest: `sha256:${"d".repeat(64)}`,
		size: 1,
	},
	layers: [],
});
const AMD64 = localDigest(AMD64_RAW);
const ARM64 = localDigest(ARM64_RAW);

const GUEST_AMD64_RAW = JSON.stringify({
	schemaVersion: 2,
	mediaType: "application/vnd.oci.image.manifest.v1+json",
	artifactType: "application/vnd.compass.guest-image.v1",
	config: {
		mediaType: "application/vnd.oci.empty.v1+json",
		digest: EMPTY_CONFIG_DIGEST,
		size: 2,
	},
	layers: [],
	annotations: { "org.compass.test.arch": "amd64" },
});
const GUEST_ARM64_RAW = JSON.stringify({
	schemaVersion: 2,
	mediaType: "application/vnd.oci.image.manifest.v1+json",
	artifactType: "application/vnd.compass.guest-image.v1",
	config: {
		mediaType: "application/vnd.oci.empty.v1+json",
		digest: EMPTY_CONFIG_DIGEST,
		size: 2,
	},
	layers: [],
	annotations: { "org.compass.test.arch": "arm64" },
});
const GUEST_AMD64 = localDigest(GUEST_AMD64_RAW);
const GUEST_ARM64 = localDigest(GUEST_ARM64_RAW);

const LOCAL_RAW = JSON.stringify({
	mediaType: "application/vnd.oci.image.index.v1+json",
	manifests: [
		{ digest: AMD64, platform: { os: "linux", architecture: "amd64" } },
		{ digest: ARM64, platform: { os: "linux", architecture: "arm64" } },
	],
});
const GUEST_RAW = JSON.stringify({
	mediaType: "application/vnd.oci.image.index.v1+json",
	manifests: [
		{
			digest: GUEST_AMD64,
			artifactType: "application/vnd.compass.guest-image.v1",
			platform: { os: "linux", architecture: "amd64" },
		},
		{
			digest: GUEST_ARM64,
			artifactType: "application/vnd.compass.guest-image.v1",
			platform: { os: "linux", architecture: "arm64" },
		},
	],
});

const DIFFERENT_RAW = JSON.stringify({
	mediaType: "application/vnd.oci.image.index.v1+json",
	manifests: [],
});

type FakeOptions = {
	newerCommits?: string[];
	newerRaw?: string;
	latestRaw?: string;
	immutable?: "equal" | "different" | "unknown" | "error";
	walkingError?: string;
	localIndex?: string;
	memberConfigMismatch?: boolean;
	repo?: string;
	emptyMemberConfig?: boolean;
	rejectArtifactInspection?: boolean;
};

type FakeCommandResult = { exitCode: number; stdout: string; stderr: string };

function fakeResult(stdout = "", exitCode = 0, stderr = ""): FakeCommandResult {
	return { exitCode, stdout, stderr };
}

function handleGitCommand(
	command: string[],
	newerCommits: string[],
): FakeCommandResult | undefined {
	if (command[0] !== "git") return undefined;
	if (command[1] === "merge-base") return fakeResult();
	if (command[1] === "rev-list") return fakeResult(newerCommits.join("\n"));
	return undefined;
}

function handlePodmanCommand(
	command: string[],
	options: FakeOptions,
	state: {
		immutableRaw: string;
		latestRaw: string;
		latestPushes: number;
		immutablePushes: number;
	},
): FakeCommandResult | undefined {
	if (command[0] !== "podman") return undefined;
	const ref = imageRef(options.repo ?? REPO);
	if (command[2] === "inspect")
		return fakeResult(options.localIndex ?? LOCAL_RAW);
	if (command[2] === "create" || command[2] === "add") return fakeResult();
	if (command[2] === "push") {
		const target = command.at(-1);
		if (target === `${ref}:latest`) {
			state.latestPushes += 1;
			state.latestRaw = options.localIndex ?? LOCAL_RAW;
		} else if (target === `${ref}:git-${SHA12}`) {
			state.immutablePushes += 1;
			state.immutableRaw = options.localIndex ?? LOCAL_RAW;
		}
	}
	return fakeResult();
}

function fakeImmutableInspection(
	tag: string,
	options: FakeOptions,
	state: { immutableRaw: string },
): FakeCommandResult | undefined {
	const ref = imageRef(options.repo ?? REPO);
	if (tag !== `${ref}:git-${SHA12}`) return undefined;
	if (options.immutable === "error") return fakeResult("", 1, "unauthorized");
	if (options.immutable === "different") return fakeResult(DIFFERENT_RAW);
	if ((options.immutable ?? "unknown") === "unknown" && !state.immutableRaw) {
		return fakeResult("", 1, "manifest unknown");
	}
	return fakeResult(state.immutableRaw || options.localIndex || LOCAL_RAW);
}

function fakeLatestInspection(
	tag: string,
	options: FakeOptions,
	state: { latestRaw: string },
): FakeCommandResult | undefined {
	const ref = imageRef(options.repo ?? REPO);
	if (tag !== `${ref}:latest`) return undefined;
	return fakeResult(state.latestRaw, state.latestRaw ? 0 : 1, "missing latest");
}

function fakeNewerInspection(
	tag: string,
	options: FakeOptions,
): FakeCommandResult | undefined {
	const ref = imageRef(options.repo ?? REPO);
	if (!tag.startsWith(`${ref}:git-`)) return undefined;
	if (options.walkingError) return fakeResult("", 1, options.walkingError);
	return options.newerRaw
		? fakeResult(options.newerRaw)
		: fakeResult("", 1, "manifest unknown");
}

function fakeRawInspection(
	tag: string,
	options: FakeOptions,
	state: { immutableRaw: string; latestRaw: string },
): FakeCommandResult {
	return (
		fakeImmutableInspection(tag, options, state) ??
		fakeLatestInspection(tag, options, state) ??
		fakeNewerInspection(tag, options) ??
		fakeResult("", 1, "unexpected skopeo inspect")
	);
}

function isArtifactInspectionFailure(
	command: string[],
	options: FakeOptions,
	tag: string,
): boolean {
	const guestRepo = (options.repo ?? REPO).endsWith("compass-guest-image");
	const artifactRef =
		guestRepo &&
		(tag.endsWith("-amd64") ||
			tag.endsWith("-arm64") ||
			tag.endsWith(GUEST_AMD64) ||
			tag.endsWith(GUEST_ARM64));
	return (
		options.rejectArtifactInspection === true &&
		artifactRef &&
		(command.includes("--format") || command.includes("--config"))
	);
}

function fakeConfigInspection(
	tag: string,
	options: FakeOptions,
): FakeCommandResult {
	const arch = tag.endsWith(AMD64) ? "amd64" : "arm64";
	const configArch =
		options.memberConfigMismatch && arch === "amd64" ? "arm64" : arch;
	return fakeResult(
		options.emptyMemberConfig
			? "{}"
			: JSON.stringify({ os: "linux", architecture: configArch }),
	);
}

function fakeRawMemberInspection(
	tag: string,
	options: FakeOptions,
): FakeCommandResult | undefined {
	if (!tag.endsWith("-amd64") && !tag.endsWith("-arm64")) return undefined;
	const isGuest = (options.repo ?? REPO).endsWith("compass-guest-image");
	if (tag.endsWith("-amd64"))
		return fakeResult(isGuest ? GUEST_AMD64_RAW : AMD64_RAW);
	return fakeResult(isGuest ? GUEST_ARM64_RAW : ARM64_RAW);
}

function handleSkopeoCommand(
	command: string[],
	options: FakeOptions,
	state: { immutableRaw: string; latestRaw: string },
): FakeCommandResult {
	const tag = command.at(-1) ?? "";
	if (isArtifactInspectionFailure(command, options, tag)) {
		return fakeResult(
			"",
			1,
			"skopeo inspect cannot inspect OCI artifact members as images",
		);
	}
	if (command.includes("--format"))
		return fakeResult(tag.endsWith("-amd64") ? AMD64 : ARM64);
	if (command.includes("--config")) return fakeConfigInspection(tag, options);
	if (command.includes("--raw")) {
		const member = fakeRawMemberInspection(tag, options);
		if (member) return member;
		if (tag.endsWith(GUEST_AMD64)) return fakeResult(GUEST_AMD64_RAW);
		if (tag.endsWith(GUEST_ARM64)) return fakeResult(GUEST_ARM64_RAW);
	}
	return fakeRawInspection(tag, options, state);
}

function fakeCommandResult(
	command: string[],
	options: FakeOptions,
	state: {
		immutableRaw: string;
		latestRaw: string;
		latestPushes: number;
		immutablePushes: number;
	},
): FakeCommandResult {
	if (command[0] === "cat")
		return fakeResult(`${localDigest(options.localIndex ?? LOCAL_RAW)}\n`);
	if (command[0] === "git") {
		const gitResult = handleGitCommand(command, options.newerCommits ?? []);
		if (gitResult) return gitResult;
	}
	if (command[0] === "podman") {
		const podmanResult = handlePodmanCommand(command, options, state);
		if (podmanResult) return podmanResult;
	}
	if (command[0] === "skopeo")
		return handleSkopeoCommand(command, options, state);
	return fakeResult();
}

function makeRunner(options: FakeOptions = {}): {
	run: (command: string[]) => Promise<FakeCommandResult>;
	commands: string[][];
	latestPushes: number;
	immutablePushes: number;
} {
	const commands: string[][] = [];
	const immutable = options.immutable ?? "unknown";
	const state = {
		immutableRaw:
			immutable === "equal"
				? (options.localIndex ?? LOCAL_RAW)
				: immutable === "different"
					? DIFFERENT_RAW
					: "",
		latestRaw: options.latestRaw ?? "",
		latestPushes: 0,
		immutablePushes: 0,
	};
	const run: (command: string[]) => Promise<FakeCommandResult> = async (
		command,
	) => {
		commands.push(command);
		return fakeCommandResult(command, options, state);
	};
	return {
		commands,
		get latestPushes() {
			return state.latestPushes;
		},
		get immutablePushes() {
			return state.immutablePushes;
		},
		run,
	};
}

const NEWER_SHA = "1234567890abcdef1234567890abcdef12345678";
const NEWER_RAW = JSON.stringify({
	mediaType: "application/vnd.oci.image.index.v1+json",
	manifests: [],
});

describe("publishImageIndex latest ownership", () => {
	test("compass-server repo composes its architecture tags", async () => {
		const repo = "ghcr.io/rigelbuild/compass-server";
		const fake = makeRunner({ repo });
		await publishImageIndex(
			{ repo, sha: SHA, authFile: AUTH, runnerTemp: TEMP },
			fake.run,
		);
		expect(
			fake.commands.some((command) =>
				command.includes(`docker://${repo}:git-${SHA12}-amd64`),
			),
		).toBe(true);
		expect(
			fake.commands.some((command) =>
				command.includes(`docker://${repo}:git-${SHA12}-arm64`),
			),
		).toBe(true);
		expect(
			fake.commands.some((command) =>
				command.includes(`docker://${repo}:git-${SHA12}`),
			),
		).toBe(true);
	});
	test("no newer index pushes :git before :latest", async () => {
		const fake = makeRunner();
		await publishImageIndex(
			{ repo: REPO, sha: SHA, authFile: AUTH, runnerTemp: TEMP },
			fake.run,
		);
		expect(fake.latestPushes).toBe(1);
		const pushes = fake.commands
			.filter((command) => command[0] === "podman" && command[2] === "push")
			.map((command) => command.at(-1));
		expect(pushes.slice(-2)).toEqual([`${REF}:git-${SHA12}`, `${REF}:latest`]);
	});

	test("published newer index leaves :latest untouched and verifies its digest", async () => {
		const fake = makeRunner({
			newerCommits: [NEWER_SHA],
			newerRaw: NEWER_RAW,
			latestRaw: NEWER_RAW,
		});
		await publishImageIndex(
			{ repo: REPO, sha: SHA, authFile: AUTH, runnerTemp: TEMP },
			fake.run,
		);
		expect(fake.latestPushes).toBe(0);
		expect(
			fake.commands.some((command) => command.at(-1) === `${REF}:latest`),
		).toBe(true);
	});

	test("published newer index fails when :latest is stale", async () => {
		const fake = makeRunner({
			newerCommits: [NEWER_SHA],
			newerRaw: NEWER_RAW,
			latestRaw: LOCAL_RAW,
		});
		await expect(
			publishImageIndex(
				{ repo: REPO, sha: SHA, authFile: AUTH, runnerTemp: TEMP },
				fake.run,
			),
		).rejects.toThrow("latest index differs from newest immutable tag");
		expect(fake.latestPushes).toBe(0);
	});

	test("non-ancestor sha aborts before moving :latest", async () => {
		const fake = makeRunner();
		const run = fake.run;
		fake.run = async (command) => {
			if (command[0] === "git" && command[1] === "merge-base") {
				fake.commands.push(command);
				return { exitCode: 1, stdout: "", stderr: "" };
			}
			return run(command);
		};
		await expect(
			publishImageIndex(
				{ repo: REPO, sha: SHA, authFile: AUTH, runnerTemp: TEMP },
				fake.run,
			),
		).rejects.toThrow("is not on origin/main");
		expect(
			fake.commands.some((command) => command.at(-1) === `${REF}:latest`),
		).toBe(false);
	});

	test("ambiguous registry error while walking aborts without moving :latest", async () => {
		const fake = makeRunner({
			newerCommits: [NEWER_SHA],
			walkingError: "temporary registry failure",
		});
		await expect(
			publishImageIndex(
				{ repo: REPO, sha: SHA, authFile: AUTH, runnerTemp: TEMP },
				fake.run,
			),
		).rejects.toThrow("ambiguous inspect failure probing newer");
		expect(fake.latestPushes).toBe(0);
	});
});
test("writes the verified digest-pinned image to the step summary", async () => {
	const summaryPath = join(TEMP, "step-summary.md");
	const previousSummary = process.env.GITHUB_STEP_SUMMARY;
	process.env.GITHUB_STEP_SUMMARY = summaryPath;
	try {
		await publishImageIndex(
			{ repo: REPO, sha: SHA, authFile: AUTH, runnerTemp: TEMP },
			makeRunner().run,
		);
		expect(readFileSync(summaryPath, "utf8")).toBe(
			`compass-agent image: \`${REPO}@${localDigest(LOCAL_RAW)}\`\n`,
		);
	} finally {
		if (previousSummary === undefined) delete process.env.GITHUB_STEP_SUMMARY;
		else process.env.GITHUB_STEP_SUMMARY = previousSummary;
	}
});

afterAll(() => rmSync(TEMP, { recursive: true, force: true }));

test("raw artifact digest includes every returned byte", async () => {
	const repo = "ghcr.io/rigelbuild/compass-guest-image";
	const fake = makeRunner({
		repo,
		localIndex: GUEST_RAW,
		emptyMemberConfig: true,
		rejectArtifactInspection: true,
	});
	const run = fake.run;
	fake.run = async (command) => {
		const result = await run(command);
		return command[0] === "skopeo" &&
			command.includes("--raw") &&
			(command.at(-1)?.endsWith("-amd64") || command.at(-1)?.endsWith("-arm64"))
			? { ...result, stdout: `${result.stdout}\n\n` }
			: result;
	};
	await expect(
		publishImageIndex(
			{ repo, sha: SHA, authFile: AUTH, runnerTemp: TEMP },
			fake.run,
		),
	).rejects.toThrow("index member digest set differs");
});

test("resolves and verifies OCI artifact members through raw manifests", async () => {
	const repo = "ghcr.io/rigelbuild/compass-guest-image";
	const fake = makeRunner({
		repo,
		localIndex: GUEST_RAW,
		emptyMemberConfig: true,
		rejectArtifactInspection: true,
	});
	const result = await publishImageIndex(
		{ repo, sha: SHA, authFile: AUTH, runnerTemp: TEMP },
		fake.run,
	);
	const adds = fake.commands.filter(
		(command) => command[0] === "podman" && command[2] === "add",
	);
	expect(adds).toHaveLength(2);
	expect(adds[0]).toEqual([
		"podman",
		"manifest",
		"add",
		"--authfile",
		join(TEMP, "anonymous-auth.json"),
		"--os",
		"linux",
		"--arch",
		"amd64",
		`localhost/compass-guest-image:${SHA12}`,
		`${imageRef(repo)}@${GUEST_AMD64}`,
	]);
	expect(adds[1]).toContain(`${imageRef(repo)}@${GUEST_ARM64}`);
	expect(
		fake.commands.some(
			(command) =>
				command[0] === "skopeo" &&
				(command.includes("--format") || command.includes("--config")) &&
				(command.at(-1)?.endsWith("-amd64") ||
					command.at(-1)?.endsWith("-arm64") ||
					command.at(-1)?.endsWith(GUEST_AMD64) ||
					command.at(-1)?.endsWith(GUEST_ARM64)),
		),
	).toBe(false);
	expect(result.stdout).toContain(
		`verified OCI index ${localDigest(GUEST_RAW)}: linux/amd64,linux/arm64; :latest matches :git-${SHA12}`,
	);
});

describe("registry authentication boundaries", () => {
	test("authenticates only the destination probe and manifest pushes", async () => {
		const fake = makeRunner();
		await publishImageIndex(
			{ repo: REPO, sha: SHA, authFile: AUTH, runnerTemp: TEMP },
			fake.run,
		);

		const inspections = fake.commands.filter(
			(command) => command[0] === "skopeo" && command.includes("inspect"),
		);
		const destinationProbe = inspections.filter(
			(command) => command.includes("--authfile") && command.includes(AUTH),
		);
		expect(destinationProbe).toHaveLength(1);
		expect(destinationProbe[0]).toEqual([
			"skopeo",
			"inspect",
			"--raw",
			"--authfile",
			AUTH,
			`${REF}:git-${SHA12}`,
		]);
		for (const command of inspections.filter(
			(entry) => entry !== destinationProbe[0],
		)) {
			expect(command).toContain("--no-creds");
			expect(command).not.toContain("--authfile");
		}

		for (const command of fake.commands.filter(
			(entry) => entry[0] === "podman" && entry[2] === "push",
		)) {
			expect(command).toContain(AUTH);
		}
	});
});

describe("immutable tag guard", () => {
	test("matching immutable raw digest skips the immutable push", async () => {
		const fake = makeRunner({ immutable: "equal" });
		await publishImageIndex(
			{ repo: REPO, sha: SHA, authFile: AUTH, runnerTemp: TEMP },
			fake.run,
		);
		expect(fake.immutablePushes).toBe(0);
	});

	test("different immutable raw digest fails", async () => {
		const fake = makeRunner({ immutable: "different" });
		await expect(
			publishImageIndex(
				{ repo: REPO, sha: SHA, authFile: AUTH, runnerTemp: TEMP },
				fake.run,
			),
		).rejects.toThrow("immutable :git-abcdef012345 index differs");
		expect(fake.immutablePushes).toBe(0);
	});

	test("manifest unknown allows immutable push", async () => {
		const fake = makeRunner({ immutable: "unknown" });
		await publishImageIndex(
			{ repo: REPO, sha: SHA, authFile: AUTH, runnerTemp: TEMP },
			fake.run,
		);
		expect(fake.immutablePushes).toBe(1);
	});

	test("other immutable inspect error aborts", async () => {
		const fake = makeRunner({ immutable: "error" });
		await expect(
			publishImageIndex(
				{ repo: REPO, sha: SHA, authFile: AUTH, runnerTemp: TEMP },
				fake.run,
			),
		).rejects.toThrow("refusing to push: unauthorized");
		expect(fake.immutablePushes).toBe(0);
	});
});

describe("index and member verification", () => {
	test("wrong member count fails", async () => {
		const fake = makeRunner({ localIndex: DIFFERENT_RAW });
		await expect(
			publishImageIndex(
				{ repo: REPO, sha: SHA, authFile: AUTH, runnerTemp: TEMP },
				fake.run,
			),
		).rejects.toThrow("composed index has 0 members, expected exactly 2");
	});

	test("wrong platform set fails", async () => {
		const wrongPlatforms = JSON.stringify({
			mediaType: "application/vnd.oci.image.index.v1+json",
			manifests: [
				{ digest: AMD64, platform: { os: "linux", architecture: "amd64" } },
				{ digest: ARM64, platform: { os: "linux", architecture: "arm64v8" } },
			],
		});
		const fake = makeRunner({ localIndex: wrongPlatforms });
		await expect(
			publishImageIndex(
				{ repo: REPO, sha: SHA, authFile: AUTH, runnerTemp: TEMP },
				fake.run,
			),
		).rejects.toThrow("composed platform set is 'linux/amd64,linux/arm64v8'");
	});

	test("member config architecture mismatch fails", async () => {
		const fake = makeRunner({ memberConfigMismatch: true });
		await expect(
			publishImageIndex(
				{ repo: REPO, sha: SHA, authFile: AUTH, runnerTemp: TEMP },
				fake.run,
			),
		).rejects.toThrow("config is linux/arm64, declared linux/amd64");
	});
});
