import { afterEach, beforeEach, describe, expect, test } from "bun:test";
import {
	chmod,
	mkdir,
	mkdtemp,
	readdir,
	readFile,
	rm,
	writeFile,
} from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { $ } from "bun";

// Drives the shipped devenv-lock-integrity.ts in a throwaway git repo with a stub
// nix on PATH: PR-mode selection, merge-base failure, ref dedupe, the single
// transient retry, the fetch environment, and cache cleanup. Offline.

const HERE = import.meta.dir;
const SHIPPED = [
	"devenv-lock-integrity.ts",
	"devenv-lock-integrity.core.ts",
	"refresh-devenv-lock.core.ts",
];
const ROOT_LOCK = "devenv.lock";
const AGENT_LOCK = "agent-image/devenv.lock";

const HERMETIC_GIT = {
	GIT_CONFIG_GLOBAL: "/dev/null",
	GIT_CONFIG_SYSTEM: "/dev/null",
	GIT_AUTHOR_NAME: "t",
	GIT_AUTHOR_EMAIL: "t@t",
	GIT_COMMITTER_NAME: "t",
	GIT_COMMITTER_EMAIL: "t@t",
};

const REV_A = "a".repeat(40);
const REV_B = "b".repeat(40);
const REV_C = "c".repeat(40);

// Truth the stub nix reports for each rev; a lock is consistent when it matches.
const TRUTH: Record<string, { hash: string; lastModified: number }> = {
	[REV_A]: { hash: "sha256-A", lastModified: 100 },
	[REV_B]: { hash: "sha256-B", lastModified: 200 },
	[REV_C]: { hash: "sha256-C", lastModified: 300 },
};

function node(
	repo: string,
	rev: string,
	over: Partial<{ narHash: string; lastModified: number }> = {},
) {
	return {
		locked: {
			lastModified: over.lastModified ?? TRUTH[rev]?.lastModified ?? 0,
			narHash: over.narHash ?? TRUTH[rev]?.hash ?? "sha256-?",
			owner: "o",
			repo,
			rev,
			type: "github",
		},
	};
}

function lockText(nodes: Record<string, unknown>): string {
	return `${JSON.stringify({ nodes: { root: { inputs: {} }, ...nodes }, root: "root", version: 7 }, null, 2)}\n`;
}

// Stub nix: logs one JSON line per call (argv + the fetch env), then answers from
// TRUTH. A rev listed in TRANSIENT_ONCE fails once with a 429 buried above 4 more lines.
const STUB_NIX = `#!/usr/bin/env bun
import { appendFileSync, existsSync, writeFileSync } from "node:fs";
const ref = process.argv.at(-1);
const rev = ref.split("/").at(-1);
appendFileSync(process.env.STUB_LOG, JSON.stringify({
	argv: process.argv.slice(2),
	NIX_CACHE_HOME: process.env.NIX_CACHE_HOME,
	XDG_CACHE_HOME: process.env.XDG_CACHE_HOME,
	NIX_CONFIG: process.env.NIX_CONFIG,
}) + "\\n");
const truth = ${JSON.stringify(TRUTH)};
const transient = (process.env.TRANSIENT_ONCE ?? "").split(",").filter(Boolean);
const marker = process.env.STUB_LOG + "." + rev;
if (transient.includes(rev) && !existsSync(marker)) {
	writeFileSync(marker, "");
	console.error("error: unable to download: HTTP error 429\\nl2\\nl3\\nl4\\nl5");
	process.exit(1);
}
if (process.env.FAIL_REVS?.split(",").includes(rev)) {
	console.error("error: HTTP error 404");
	process.exit(1);
}
const t = truth[rev];
if (!t) { console.error("error: unknown rev"); process.exit(1); }
console.log(JSON.stringify({ hash: t.hash, locked: { lastModified: t.lastModified } }));
`;

let repo: string;
let bin: string;
let log: string;
let tmp: string;

async function commitLocks(
	root: Record<string, unknown>,
	agent: Record<string, unknown>,
	msg: string,
) {
	await writeFile(join(repo, ROOT_LOCK), lockText(root));
	await writeFile(join(repo, AGENT_LOCK), lockText(agent));
	await $`git add -A && git commit -qm ${msg}`
		.cwd(repo)
		.env({ ...process.env, ...HERMETIC_GIT })
		.quiet();
}

async function run(env: Record<string, string> = {}) {
	const r = await $`bun tools/renovate/devenv-lock-integrity.ts`
		.cwd(repo)
		.env({
			PATH: `${bin}:${process.env.PATH}`,
			STUB_LOG: log,
			TMPDIR: tmp,
			LOCK_INTEGRITY_RETRY_DELAY_MS: "0",
			NIX_CONFIG: "experimental-features = nix-command",
			...HERMETIC_GIT,
			...env,
		})
		.nothrow()
		.quiet();
	const calls = (await readFile(log, "utf8").catch(() => ""))
		.split("\n")
		.filter(Boolean)
		.map(
			(l) =>
				JSON.parse(l) as {
					argv: string[];
					NIX_CACHE_HOME: string;
					XDG_CACHE_HOME: string;
					NIX_CONFIG: string;
				},
		);
	return {
		code: r.exitCode,
		out: r.stdout.toString() + r.stderr.toString(),
		calls,
	};
}

beforeEach(async () => {
	const base = await mkdtemp(join(tmpdir(), "lock-integrity-test-"));
	repo = join(base, "repo");
	bin = join(base, "bin");
	tmp = join(base, "tmp");
	log = join(base, "nix.log");
	await mkdir(join(repo, "tools", "renovate"), { recursive: true });
	await mkdir(join(repo, "agent-image"), { recursive: true });
	await mkdir(bin);
	await mkdir(tmp);
	for (const f of SHIPPED) {
		await writeFile(
			join(repo, "tools", "renovate", f),
			await readFile(join(HERE, f), "utf8"),
		);
	}
	await writeFile(join(bin, "nix"), STUB_NIX);
	await chmod(join(bin, "nix"), 0o755);
	await $`git init -q -b main`
		.cwd(repo)
		.env({ ...process.env, ...HERMETIC_GIT })
		.quiet();
	await commitLocks(
		{ devenv: node("devenv", REV_A) },
		{ devenv: node("devenv", REV_A), nixpkgs: node("nixpkgs", REV_B) },
		"base",
	);
	// The PR base the shell resolves as origin/main.
	await $`git update-ref refs/remotes/origin/main HEAD`.cwd(repo).quiet();
});

afterEach(async () => {
	await rm(join(repo, ".."), { recursive: true, force: true });
});

describe("full sweep", () => {
	test("checks every node once per unique ref and passes a consistent tree", async () => {
		const r = await run();
		expect(r.code).toBe(0);
		expect(r.out).toContain("full sweep, 3 node(s) to check");
		// devenv@A appears in both locks: one fetch serves both.
		expect(r.calls.map((c) => c.argv.at(-1)).sort()).toEqual([
			`github:o/devenv/${REV_A}`,
			`github:o/nixpkgs/${REV_B}`,
		]);
	});

	test("fetches with a fresh shared cache dir, a cleared token, and removes the dir", async () => {
		const r = await run();
		const call = r.calls[0];
		expect(call?.NIX_CACHE_HOME).toBe(call?.XDG_CACHE_HOME);
		expect(call?.NIX_CACHE_HOME.startsWith(tmp)).toBe(true);
		expect(call?.NIX_CONFIG).toBe(
			"experimental-features = nix-command\naccess-tokens =",
		);
		expect(call?.argv.slice(0, 2)).toEqual(["flake", "prefetch"]);
		expect(await readdir(tmp)).toEqual([]);
	});

	test("a stale narHash fails naming the lock and node", async () => {
		await commitLocks(
			{ devenv: node("devenv", REV_C, { narHash: "sha256-A" }) },
			{ devenv: node("devenv", REV_A), nixpkgs: node("nixpkgs", REV_B) },
			"half-relock",
		);
		const r = await run();
		expect(r.code).toBe(1);
		expect(r.out).toMatch(
			/FAIL devenv\.lock devenv .*narHash expected sha256-C, got sha256-A/,
		);
	});

	test("an unfetchable rev fails as unverified and is not retried", async () => {
		const r = await run({ FAIL_REVS: REV_B });
		expect(r.code).toBe(1);
		expect(r.out).toMatch(
			/FAIL agent-image\/devenv\.lock nixpkgs .*unverified: .*HTTP error 404/,
		);
		expect(r.calls.filter((c) => c.argv.at(-1)?.endsWith(REV_B))).toHaveLength(
			1,
		);
	});

	test("a transient error above the shown tail is retried once", async () => {
		const r = await run({ TRANSIENT_ONCE: REV_B });
		expect(r.code).toBe(0);
		expect(r.calls.filter((c) => c.argv.at(-1)?.endsWith(REV_B))).toHaveLength(
			2,
		);
	});

	test("an unverifiable node type fails naming the lock", async () => {
		await commitLocks(
			{
				devenv: node("devenv", REV_A),
				local: { locked: { type: "path", path: "/x" } },
			},
			{ devenv: node("devenv", REV_A), nixpkgs: node("nixpkgs", REV_B) },
			"path input",
		);
		const r = await run();
		expect(r.code).toBe(1);
		expect(r.out).toMatch(/unverifiable lock: devenv\.lock: node "local"/);
		expect(r.calls).toHaveLength(0);
	});
});

describe("PR mode", () => {
	const pr = { GITHUB_EVENT_NAME: "pull_request", GITHUB_BASE_REF: "main" };

	test("an unchanged tree checks nothing and makes no fetch", async () => {
		const r = await run(pr);
		expect(r.code).toBe(0);
		expect(r.out).toContain("0 node(s) to check");
		expect(r.calls).toHaveLength(0);
	});

	test("checks only the node whose locked object changed", async () => {
		await commitLocks(
			{ devenv: node("devenv", REV_A) },
			{ devenv: node("devenv", REV_A), nixpkgs: node("nixpkgs", REV_C) },
			"bump agent nixpkgs",
		);
		const r = await run(pr);
		expect(r.code).toBe(0);
		expect(r.out).toContain("1 node(s) to check");
		expect(r.calls.map((c) => c.argv.at(-1))).toEqual([
			`github:o/nixpkgs/${REV_C}`,
		]);
	});

	test("a half-relock on the PR fails", async () => {
		await commitLocks(
			{
				devenv: node("devenv", REV_C, {
					narHash: "sha256-A",
					lastModified: 100,
				}),
			},
			{ devenv: node("devenv", REV_A), nixpkgs: node("nixpkgs", REV_B) },
			"half-relock",
		);
		const r = await run(pr);
		expect(r.code).toBe(1);
		expect(r.out).toMatch(
			/FAIL devenv\.lock devenv .*narHash expected sha256-C.*lastModified expected 300/,
		);
	});

	test("an unresolvable base fails before any fetch", async () => {
		const r = await run({ ...pr, GITHUB_BASE_REF: "no-such-branch" });
		expect(r.code).toBe(1);
		expect(r.out).toContain(
			"cannot resolve merge-base with origin/no-such-branch",
		);
		expect(r.calls).toHaveLength(0);
	});
});
