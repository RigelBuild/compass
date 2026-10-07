import { afterEach, beforeEach, describe, expect, test } from "bun:test";
import { watch } from "node:fs";
import { chmod, mkdir, mkdtemp, readFile, rm } from "node:fs/promises";
import { tmpdir } from "node:os";
import { dirname, join } from "node:path";
import { pathToFileURL } from "node:url";
import { $ } from "bun";
import {
	BUILD_FILE,
	PIN_FILE,
	TOOL_ENTRIES,
	type ToolEntry,
} from "./refresh-go-analysis-hashes.ts";

const repoRoot = join(import.meta.dir, "..", "..");
const SCRIPT_REL = "tools/renovate/refresh-go-analysis-hashes.ts";
const SCRIPT_SOURCE = join(import.meta.dir, "refresh-go-analysis-hashes.ts");
const FOD_HELPER_SOURCE = join(import.meta.dir, "refresh-fod-hashes.ts");
const CALLS_FILE = ".scratch/nix-calls.jsonl";

const HERMETIC_ENV = {
	...process.env,
	GIT_CONFIG_GLOBAL: "/dev/null",
	GIT_CONFIG_SYSTEM: "/dev/null",
	GIT_AUTHOR_NAME: "Go analysis test",
	GIT_AUTHOR_EMAIL: "go-analysis@example.invalid",
	GIT_COMMITTER_NAME: "Go analysis test",
	GIT_COMMITTER_EMAIL: "go-analysis@example.invalid",
};

const STUB_NIX = `#!/usr/bin/env bun
import { appendFile } from "node:fs/promises";
import { createHash } from "node:crypto";
import { readFileSync } from "node:fs";
import { join } from "node:path";

const args = process.argv.slice(2);
const pinText = readFileSync("tools/toolchain/versions/go-analysis.nix", "utf8");
const names = ["nilaway", "golangci-lint"];

function blockFor(tool) {
  const lines = pinText.split("\\n");
  const start = lines.findIndex((line) => line.trimStart().startsWith(tool + " = {"));
  if (start < 0) throw new Error("missing pin block: " + tool);
  const end = lines.findIndex((line, index) => index > start && line.trim() === "};");
  if (end < 0) throw new Error("missing pin block end: " + tool);
  return lines.slice(start, end + 1).join("\\n");
}

function field(block, name) {
  const match = block.match(new RegExp('^\\\\s*' + name + ' = "([^"]+)"', "m"));
  if (!match || !match[1]) throw new Error("missing pin field: " + name);
  return match[1];
}

function drvPath(tool, kind) {
  const block = blockFor(tool);
  const version = field(block, "version");
  const sourceHash = field(block, "hash");
  const id = createHash("sha256").update([tool, kind, version, sourceHash].join("|")).digest("hex").slice(0, 12);
  if (kind === "src") return "/nix/store/" + id + "-source.drv";
  return "/nix/store/" + id + "-" + tool + "-" + version + "-go-modules.drv";
}

async function record(call) {
  if (process.env.NIX_CALLS) await appendFile(process.env.NIX_CALLS, JSON.stringify(call) + "\\n");
}

if (args[0] === "eval") {
  const expression = args[args.length - 1] || "";
  const parts = expression.split(".");
  const tool = names.find((name) => name === parts[1]);
  const kind = parts[2];
  if (!tool || (kind !== "src" && kind !== "goModules")) throw new Error("unexpected eval: " + expression);
  const path = drvPath(tool, kind === "src" ? "src" : "goModules");
  await record({ action: "eval", expression, path });
  process.stdout.write(path + "\\n");
  process.exit(0);
}

if (args[0] === "build") {
  const expression = args.find((arg) => arg.startsWith("analysis.")) || "";
  const tool = names.find((name) => expression === "analysis." + name);
  if (!tool) throw new Error("unexpected build target: " + expression);
  await record({ action: "build", expression });
  if (process.env.STUB_NIX_BLOCK_BUILD === "1") {
    const readyDir = process.env.STUB_NIX_READY_DIR;
    if (!readyDir) throw new Error("missing stub readiness directory");
    await Bun.write(join(readyDir, "nix-ready-" + process.pid), "ready\\n");
    Atomics.wait(new Int32Array(new SharedArrayBuffer(4)), 0, 0);
  }
  if (process.env.STUB_NIX_NO_GOT === "1") {
    process.stderr.write("error: build failed without a hash mismatch\\n");
    process.exit(1);
  }
  const block = blockFor(tool);
  const sourceHash = field(block, "hash");
  const vendorHash = field(block, "vendorHash");
  const sourceGot = "sha256-stub-" + tool + "-source";
  const vendorGot = "sha256-stub-" + tool + "-vendor";
  const kind = sourceHash !== sourceGot ? "src" : vendorHash !== vendorGot ? "goModules" : "";
  if (kind === "") {
    process.stdout.write("build succeeded\\n");
    process.exit(0);
  }
  const targetPath = drvPath(tool, kind);
  process.stderr.write("error: hash mismatch in fixed-output derivation '/nix/store/00000000000000000000-foreign-source.drv':\\n");
  process.stderr.write("         specified: sha256-fake\\n            got:    sha256-foreign\\n");
  process.stderr.write("error: hash mismatch in fixed-output derivation '" + targetPath + "':\\n");
  process.stderr.write("         specified: sha256-AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=\\n");
  process.stderr.write("            got:    " + (kind === "src" ? sourceGot : vendorGot) + "\\n");
  process.exit(1);
}

throw new Error("unexpected nix arguments: " + args.join(" "));
`;

type StubCall = { action: string; expression: string; path?: string };

let scratchRoot = "";
let repo = "";
let nilawayFetchUrl = "";
let nilawayFixtureRev = "";

function entryFor(tool: ToolEntry["tool"]): ToolEntry {
	const entry = TOOL_ENTRIES.find((candidate) => candidate.tool === tool);
	if (!entry)
		throw new Error(`fixture drift: missing ${tool} from TOOL_ENTRIES`);
	return entry;
}

function blockFrom(text: string, entry: ToolEntry): string {
	const lines = text.split("\n");
	const start = lines.findIndex((line) =>
		line.trimStart().startsWith(`${entry.attr} = {`),
	);
	if (start < 0)
		throw new Error(`fixture drift: missing ${entry.attr} pin block`);
	const end = lines.findIndex(
		(line, index) => index > start && line.trim() === "};",
	);
	if (end < 0)
		throw new Error(`fixture drift: unterminated ${entry.attr} pin block`);
	return lines.slice(start, end + 1).join("\n");
}

function replaceBlock(
	text: string,
	entry: ToolEntry,
	updatedBlock: string,
): string {
	const original = blockFrom(text, entry);
	const start = text.indexOf(original);
	if (start < 0)
		throw new Error(`fixture drift: cannot locate ${entry.attr} block`);
	return `${text.slice(0, start)}${updatedBlock}${text.slice(start + original.length)}`;
}

function setField(
	text: string,
	entry: ToolEntry,
	field: string,
	value: string,
): string {
	const block = blockFrom(text, entry);
	const pattern = new RegExp(`(^\\s*${field} = ")[^"]+(";\\s*$)`, "m");
	const updated = block.replace(
		pattern,
		(_line, before: string, after: string) => `${before}${value}${after}`,
	);
	if (updated === block)
		throw new Error(`fixture drift: missing ${field} in ${entry.attr}`);
	return replaceBlock(text, entry, updated);
}

async function writeFixtureFile(
	root: string,
	relativePath: string,
	contents: string,
): Promise<void> {
	const absolutePath = join(root, relativePath);
	await mkdir(dirname(absolutePath), { recursive: true });
	await Bun.write(absolutePath, contents);
}

async function buildBaselineRepo(): Promise<void> {
	repo = join(scratchRoot, "repo");
	await mkdir(repo, { recursive: true });
	await mkdir(join(repo, ".scratch"), { recursive: true });
	await Bun.write(join(repo, CALLS_FILE), "");
	await writeFixtureFile(
		repo,
		PIN_FILE,
		await readFile(join(repoRoot, PIN_FILE), "utf8"),
	);
	await writeFixtureFile(repo, BUILD_FILE, "{ }\n");
	await writeFixtureFile(
		repo,
		SCRIPT_REL,
		await readFile(SCRIPT_SOURCE, "utf8"),
	);
	await writeFixtureFile(
		repo,
		"tools/renovate/refresh-fod-hashes.ts",
		await readFile(FOD_HELPER_SOURCE, "utf8"),
	);
	const stubPath = join(repo, "stubbin", "nix");
	await writeFixtureFile(repo, "stubbin/nix", STUB_NIX);
	await chmod(stubPath, 0o755);

	await $`git init -q -b main`.cwd(repo).env(HERMETIC_ENV).quiet();
	await $`git add -A`.cwd(repo).env(HERMETIC_ENV).quiet();
	await $`git commit -q -m baseline`.cwd(repo).env(HERMETIC_ENV).quiet();
	await $`git remote add origin .`.cwd(repo).env(HERMETIC_ENV).quiet();
	await $`git update-ref refs/remotes/origin/main main`
		.cwd(repo)
		.env(HERMETIC_ENV)
		.quiet();
}

async function createNilawayUpstream(): Promise<void> {
	const upstream = join(scratchRoot, "nilaway-upstream");
	await mkdir(upstream, { recursive: true });
	await Bun.write(join(upstream, "fixture.txt"), "local upstream fixture\n");
	const commitDate = "2026-01-02T23:30:00-0800";
	const commitEnv = {
		...HERMETIC_ENV,
		GIT_AUTHOR_DATE: commitDate,
		GIT_COMMITTER_DATE: commitDate,
	};
	await $`git init -q`.cwd(upstream).env(commitEnv).quiet();
	await $`git add fixture.txt`.cwd(upstream).env(commitEnv).quiet();
	await $`git commit -q -m upstream`.cwd(upstream).env(commitEnv).quiet();
	nilawayFixtureRev = (
		await $`git rev-parse HEAD`.cwd(upstream).env(commitEnv).text()
	).trim();
	nilawayFetchUrl = pathToFileURL(upstream).href;
}

async function runRefresh(
	extraEnv: Record<string, string> = {},
	baseBranch: string | undefined = "main",
) {
	const env: Record<string, string> = {
		...HERMETIC_ENV,
		PATH: `${join(repo, "stubbin")}:${process.env.PATH}`,
		NIX_CALLS: join(repo, CALLS_FILE),
		NILAWAY_FETCH_URL: nilawayFetchUrl,
		...extraEnv,
	};
	if (baseBranch === undefined) delete env.RENOVATE_BASE_BRANCH;
	else env.RENOVATE_BASE_BRANCH = baseBranch;
	return await $`bun ${SCRIPT_REL}`.cwd(repo).env(env).quiet().nothrow();
}

async function commitMainPin(contents: string): Promise<void> {
	await Bun.write(join(repo, PIN_FILE), contents);
	await $`git add ${PIN_FILE}`.cwd(repo).env(HERMETIC_ENV).quiet();
	await $`git commit -q -m diverged-main-pin`
		.cwd(repo)
		.env(HERMETIC_ENV)
		.quiet();
	await $`git update-ref refs/remotes/origin/main HEAD`
		.cwd(repo)
		.env(HERMETIC_ENV)
		.quiet();
	await $`git update-ref refs/heads/main HEAD^`
		.cwd(repo)
		.env(HERMETIC_ENV)
		.quiet();
}

async function waitForNixBuildReady(): Promise<number> {
	return await new Promise<number>((resolve, reject) => {
		const watcher = watch(join(repo, ".scratch"), (_eventType, filename) => {
			const name = filename?.toString() ?? "";
			if (!name.startsWith("nix-ready-")) return;
			watcher.close();
			const pid = Number(name.slice("nix-ready-".length));
			if (!Number.isInteger(pid)) reject(new Error("invalid stub nix pid"));
			else resolve(pid);
		});
		watcher.on("error", reject);
	});
}

async function readCalls(): Promise<StubCall[]> {
	const contents = await readFile(join(repo, CALLS_FILE), "utf8");
	return contents
		.split("\n")
		.filter((line) => line.length > 0)
		.map((line) => JSON.parse(line) as StubCall);
}

describe("tools/renovate/refresh-go-analysis-hashes.ts", () => {
	beforeEach(async () => {
		scratchRoot = await mkdtemp(join(tmpdir(), "go-analysis-"));
		await buildBaselineRepo();
		await createNilawayUpstream();
	});

	afterEach(async () => {
		if (scratchRoot) await rm(scratchRoot, { recursive: true, force: true });
	});

	test("refreshes against origin/main when RENOVATE_BASE_BRANCH is unset", async () => {
		const entry = entryFor("golangci-lint");
		const before = await readFile(join(repo, PIN_FILE), "utf8");
		await commitMainPin(setField(before, entry, "version", "2.14.9"));
		await Bun.write(join(repo, PIN_FILE), before);
		const bumped = setField(before, entry, "version", "2.14.0");
		await Bun.write(join(repo, PIN_FILE), bumped);
		const result = await runRefresh({}, undefined);
		const block = blockFrom(
			await readFile(join(repo, PIN_FILE), "utf8"),
			entry,
		);
		expect(result.exitCode).toBe(0);
		expect(result.stdout.toString()).toContain("refreshed golangci-lint");
		expect(block).toContain('hash = "sha256-stub-golangci-lint-source";');
		expect(block).toContain('vendorHash = "sha256-stub-golangci-lint-vendor";');
	});

	test("no-ops against origin/main when RENOVATE_BASE_BRANCH is unset", async () => {
		const entry = entryFor("golangci-lint");
		const before = await readFile(join(repo, PIN_FILE), "utf8");
		const basePin = setField(before, entry, "version", "2.14.9");
		await commitMainPin(basePin);
		await Bun.write(join(repo, PIN_FILE), basePin);

		const result = await runRefresh({}, undefined);
		expect(result.exitCode).toBe(0);
		expect(result.stdout.toString()).toContain(
			`${PIN_FILE} unchanged vs origin/main; nothing to do.`,
		);
		expect(await readFile(join(repo, PIN_FILE), "utf8")).toBe(basePin);
		expect(await readCalls()).toEqual([]);
	});

	test("restores the original pin file when SIGTERM interrupts a build", async () => {
		const entry = entryFor("golangci-lint");
		const before = await readFile(join(repo, PIN_FILE), "utf8");
		const bumped = setField(before, entry, "version", "2.14.8");
		await Bun.write(join(repo, PIN_FILE), bumped);
		const ready = waitForNixBuildReady();
		const env = {
			...HERMETIC_ENV,
			PATH: `${join(repo, "stubbin")}:${process.env.PATH}`,
			NIX_CALLS: join(repo, CALLS_FILE),
			NILAWAY_FETCH_URL: nilawayFetchUrl,
			STUB_NIX_BLOCK_BUILD: "1",
			STUB_NIX_READY_DIR: join(repo, ".scratch"),
		};
		const child = Bun.spawn([process.execPath, SCRIPT_REL], {
			cwd: repo,
			env,
			stdout: "pipe",
			stderr: "pipe",
		});
		let nixPid: number | undefined;
		try {
			nixPid = await ready;
			child.kill("SIGTERM");
			const [exitCode, stderr] = await Promise.all([
				child.exited,
				new Response(child.stderr).text(),
			]);
			expect(exitCode).not.toBe(0);
			expect(stderr).toContain("interrupted by SIGTERM");
			expect(await readFile(join(repo, PIN_FILE), "utf8")).toBe(bumped);
		} finally {
			if (nixPid !== undefined) process.kill(nixPid, "SIGTERM");
		}
	});

	test("refreshes a golangci-lint bump and leaves nilaway byte-identical", async () => {
		const entry = entryFor("golangci-lint");
		const other = entryFor("nilaway");
		const before = await readFile(join(repo, PIN_FILE), "utf8");
		const otherBefore = blockFrom(before, other);
		const bumped = setField(before, entry, "version", "2.14.0");
		await Bun.write(join(repo, PIN_FILE), bumped);

		const result = await runRefresh();
		const after = await readFile(join(repo, PIN_FILE), "utf8");
		const block = blockFrom(after, entry);
		expect(result.exitCode).toBe(0);
		expect(block).toContain('version = "2.14.0";');
		expect(block).toContain('tag = "v2.14.0";');
		expect(block).toContain('hash = "sha256-stub-golangci-lint-source";');
		expect(block).toContain('vendorHash = "sha256-stub-golangci-lint-vendor";');
		expect(blockFrom(after, other)).toBe(otherBefore);

		const calls = await readCalls();
		expect(calls.filter((call) => call.action === "build")).toHaveLength(2);
		expect(
			calls.some(
				(call) => call.expression === "analysis.golangci-lint.src.drvPath",
			),
		).toBe(true);
		expect(
			calls.some(
				(call) =>
					call.expression === "analysis.golangci-lint.goModules.drvPath" &&
					call.path?.includes("2.14.0-go-modules.drv"),
			),
		).toBe(true);

		const secondRun = await runRefresh();
		expect(secondRun.exitCode).toBe(0);
		expect(await readFile(join(repo, PIN_FILE), "utf8")).toBe(after);
	});

	test("refreshes nilaway hashes and UTC version from a negative-offset commit", async () => {
		const entry = entryFor("nilaway");
		const other = entryFor("golangci-lint");
		const before = await readFile(join(repo, PIN_FILE), "utf8");
		const oldVersion = blockFrom(before, entry).match(
			/^\s*version = "([^"]+)"/m,
		)?.[1];
		const otherBefore = blockFrom(before, other);
		const bumped = setField(before, entry, "rev", nilawayFixtureRev);
		await Bun.write(join(repo, PIN_FILE), bumped);

		const result = await runRefresh();
		const after = await readFile(join(repo, PIN_FILE), "utf8");
		const block = blockFrom(after, entry);
		expect(result.exitCode).toBe(0);
		expect(block).toContain(`version = "0-unstable-2026-01-03";`);
		expect(block).toContain(`rev = "${nilawayFixtureRev}";`);
		expect(block).toContain('hash = "sha256-stub-nilaway-source";');
		expect(block).toContain('vendorHash = "sha256-stub-nilaway-vendor";');
		expect(blockFrom(after, other)).toBe(otherBefore);
		const calls = await readCalls();
		expect(
			calls.some(
				(call) =>
					call.expression === "analysis.nilaway.goModules.drvPath" &&
					call.path?.includes("0-unstable-2026-01-03-go-modules.drv"),
			),
		).toBe(true);
		expect(
			calls.some(
				(call) =>
					call.expression === "analysis.nilaway.goModules.drvPath" &&
					oldVersion !== undefined &&
					call.path?.includes(oldVersion),
			),
		).toBe(false);
	});

	test("attributes co-emitted foreign source mismatches to the runtime drv basename", async () => {
		const entry = entryFor("golangci-lint");
		const before = await readFile(join(repo, PIN_FILE), "utf8");
		await Bun.write(
			join(repo, PIN_FILE),
			setField(before, entry, "version", "2.14.1"),
		);

		const result = await runRefresh();
		const block = blockFrom(
			await readFile(join(repo, PIN_FILE), "utf8"),
			entry,
		);
		expect(result.exitCode).toBe(0);
		expect(block).toContain('hash = "sha256-stub-golangci-lint-source";');
		expect(block).not.toContain("sha256-foreign");
	});

	test("prefixes missing-marker failures and restores the pre-run pin text", async () => {
		const entry = entryFor("golangci-lint");
		const before = await readFile(join(repo, PIN_FILE), "utf8");
		const bumped = setField(before, entry, "version", "2.14.2");
		const block = blockFrom(bumped, entry).replace(
			/^\s*vendorHash = .*\n/m,
			"",
		);
		if (block === blockFrom(bumped, entry))
			throw new Error("fixture drift: missing vendorHash");
		await Bun.write(join(repo, PIN_FILE), replaceBlock(bumped, entry, block));
		const changed = await readFile(join(repo, PIN_FILE), "utf8");

		const result = await runRefresh();
		expect(result.exitCode).not.toBe(0);
		expect(result.stderr.toString()).toMatch(/^renovate-go-analysis:/);
		expect(result.stderr.toString()).toContain("renovate-fod:");
		expect(await readFile(join(repo, PIN_FILE), "utf8")).toBe(changed);
	});

	test("prefixes missing-got failures and restores the pre-run pin text", async () => {
		const entry = entryFor("golangci-lint");
		const before = await readFile(join(repo, PIN_FILE), "utf8");
		const bumped = setField(before, entry, "version", "2.14.3");
		await Bun.write(join(repo, PIN_FILE), bumped);

		const result = await runRefresh({ STUB_NIX_NO_GOT: "1" });
		expect(result.exitCode).not.toBe(0);
		expect(result.stderr.toString()).toMatch(/^renovate-go-analysis:/);
		expect(result.stderr.toString()).toContain("no 'got:' SRI");
		expect(await readFile(join(repo, PIN_FILE), "utf8")).toBe(bumped);
	});

	test("prefixes nilaway fetch failures and restores the pre-run pin text", async () => {
		const entry = entryFor("nilaway");
		const before = await readFile(join(repo, PIN_FILE), "utf8");
		const bumped = setField(before, entry, "rev", nilawayFixtureRev);
		await Bun.write(join(repo, PIN_FILE), bumped);

		const result = await runRefresh({
			NILAWAY_FETCH_URL: "file:///missing-nilaway-fixture",
		});
		expect(result.exitCode).not.toBe(0);
		expect(result.stderr.toString()).toMatch(/^renovate-go-analysis:/);
		expect(result.stderr.toString()).toContain("commit-date fetch failed");
		expect(await readFile(join(repo, PIN_FILE), "utf8")).toBe(bumped);
	});

	test("no-ops when go-analysis pin blocks match the base branch", async () => {
		const result = await runRefresh();
		expect(result.exitCode).toBe(0);
		expect(result.stdout.toString()).toContain("nothing to do");
		expect(await readCalls()).toEqual([]);
	});
});
