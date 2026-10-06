import { describe, expect, test } from "bun:test";
import { createHash } from "node:crypto";
import { publishImageIndex } from "./index.ts";

const SHA = "abcdef0123456789abcdef0123456789abcdef01";
const SHA12 = SHA.slice(0, 12);
const AMD64 = `sha256:${"a".repeat(64)}`;
const ARM64 = `sha256:${"b".repeat(64)}`;
const AUTH = "/tmp/auth.json";
const TEMP = "/tmp/agent-image-index";
const REF = "docker://ghcr.io/rigelbuild/compass-agent";
const LOCAL_RAW = JSON.stringify({
	mediaType: "application/vnd.oci.image.index.v1+json",
	manifests: [
		{ digest: AMD64, platform: { os: "linux", architecture: "amd64" } },
		{ digest: ARM64, platform: { os: "linux", architecture: "arm64" } },
	],
});
const LOCAL_DIGEST = `sha256:${createHash("sha256").update(LOCAL_RAW).digest("hex")}`;
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
	if (command[2] === "inspect")
		return fakeResult(options.localIndex ?? LOCAL_RAW);
	if (command[2] === "push") {
		const target = command.at(-1);
		if (target === `${REF}:latest`) {
			state.latestPushes += 1;
			state.latestRaw = LOCAL_RAW;
		} else if (target === `${REF}:git-${SHA12}`) {
			state.immutablePushes += 1;
			state.immutableRaw = LOCAL_RAW;
		}
	}
	return fakeResult();
}

function fakeImmutableInspection(
	tag: string,
	options: FakeOptions,
	state: { immutableRaw: string },
): FakeCommandResult | undefined {
	if (tag !== `${REF}:git-${SHA12}`) return undefined;
	if (options.immutable === "error") return fakeResult("", 1, "unauthorized");
	if (options.immutable === "different") return fakeResult(DIFFERENT_RAW);
	if ((options.immutable ?? "unknown") === "unknown" && !state.immutableRaw) {
		return fakeResult("", 1, "manifest unknown");
	}
	return fakeResult(state.immutableRaw || LOCAL_RAW);
}

function fakeLatestInspection(
	tag: string,
	state: { latestRaw: string },
): FakeCommandResult | undefined {
	if (tag !== `${REF}:latest`) return undefined;
	return fakeResult(state.latestRaw, state.latestRaw ? 0 : 1, "missing latest");
}

function fakeNewerInspection(
	tag: string,
	options: FakeOptions,
): FakeCommandResult | undefined {
	if (!tag.startsWith(`${REF}:git-`)) return undefined;
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
		fakeLatestInspection(tag, state) ??
		fakeNewerInspection(tag, options) ??
		fakeResult("", 1, "unexpected skopeo inspect")
	);
}

function handleSkopeoCommand(
	command: string[],
	options: FakeOptions,
	state: { immutableRaw: string; latestRaw: string },
): FakeCommandResult {
	const tag = command.at(-1) ?? "";
	if (command.includes("--format")) {
		return fakeResult(tag.endsWith("-amd64") ? AMD64 : ARM64);
	}
	if (command.includes("--config")) {
		const arch = tag.endsWith(AMD64) ? "amd64" : "arm64";
		const configArch =
			options.memberConfigMismatch && arch === "amd64" ? "arm64" : arch;
		return fakeResult(
			JSON.stringify({ os: "linux", architecture: configArch }),
		);
	}
	return fakeRawInspection(tag, options, state);
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
				? LOCAL_RAW
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
		if (command[0] === "cat") return fakeResult(`${LOCAL_DIGEST}\n`);
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
	test("no newer index pushes :git before :latest", async () => {
		const fake = makeRunner();
		await publishImageIndex(
			{ sha: SHA, authFile: AUTH, runnerTemp: TEMP },
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
			{ sha: SHA, authFile: AUTH, runnerTemp: TEMP },
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
				{ sha: SHA, authFile: AUTH, runnerTemp: TEMP },
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
				{ sha: SHA, authFile: AUTH, runnerTemp: TEMP },
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
				{ sha: SHA, authFile: AUTH, runnerTemp: TEMP },
				fake.run,
			),
		).rejects.toThrow("ambiguous inspect failure probing newer");
		expect(fake.latestPushes).toBe(0);
	});
});

describe("immutable tag guard", () => {
	test("matching immutable raw digest skips the immutable push", async () => {
		const fake = makeRunner({ immutable: "equal" });
		await publishImageIndex(
			{ sha: SHA, authFile: AUTH, runnerTemp: TEMP },
			fake.run,
		);
		expect(fake.immutablePushes).toBe(0);
	});

	test("trailing newlines on raw inspect output do not change the digest", async () => {
		const fake = makeRunner({ immutable: "equal" });
		const run = fake.run;
		fake.run = async (command) => {
			const result = await run(command);
			return command[0] === "skopeo" && command.includes("--raw")
				? { ...result, stdout: `${result.stdout}\n\n` }
				: result;
		};
		await publishImageIndex(
			{ sha: SHA, authFile: AUTH, runnerTemp: TEMP },
			fake.run,
		);
		expect(fake.immutablePushes).toBe(0);
	});

	test("different immutable raw digest fails", async () => {
		const fake = makeRunner({ immutable: "different" });
		await expect(
			publishImageIndex(
				{ sha: SHA, authFile: AUTH, runnerTemp: TEMP },
				fake.run,
			),
		).rejects.toThrow("immutable :git-abcdef012345 index differs");
		expect(fake.immutablePushes).toBe(0);
	});

	test("manifest unknown allows immutable push", async () => {
		const fake = makeRunner({ immutable: "unknown" });
		await publishImageIndex(
			{ sha: SHA, authFile: AUTH, runnerTemp: TEMP },
			fake.run,
		);
		expect(fake.immutablePushes).toBe(1);
	});

	test("other immutable inspect error aborts", async () => {
		const fake = makeRunner({ immutable: "error" });
		await expect(
			publishImageIndex(
				{ sha: SHA, authFile: AUTH, runnerTemp: TEMP },
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
				{ sha: SHA, authFile: AUTH, runnerTemp: TEMP },
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
				{ sha: SHA, authFile: AUTH, runnerTemp: TEMP },
				fake.run,
			),
		).rejects.toThrow("composed platform set is 'linux/amd64,linux/arm64v8'");
	});

	test("member config architecture mismatch fails", async () => {
		const fake = makeRunner({ memberConfigMismatch: true });
		await expect(
			publishImageIndex(
				{ sha: SHA, authFile: AUTH, runnerTemp: TEMP },
				fake.run,
			),
		).rejects.toThrow("config is linux/arm64, declared linux/amd64");
	});
});
