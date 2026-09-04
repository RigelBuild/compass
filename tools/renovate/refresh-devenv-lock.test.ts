import { afterEach, beforeEach, describe, expect, test } from "bun:test";
import { chmod, mkdir, mkdtemp, readFile, rm } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { $ } from "bun";

// Orchestration harness for tools/renovate/refresh-devenv-lock.ts (RIG-2815).
//
// The pure decisions (which lock changed, the fork-rev read) are unit-tested in
// refresh-devenv-lock.core.test.ts. This drives the SHIPPED entry point end to
// end inside a throwaway git repo with a stub `devenv` on PATH, so the step
// SEQUENCING is exercised offline + deterministically: self-gate → pick the
// scope → relock in THAT scope's directory → verify the lock actually moved.
// The real `devenv update devenv` runs for real against the workflow-built
// devenv CLI on the PR's own branch; here it is stubbed so the failure mode
// under test is the script's own control flow, not the network.
//
// The load-bearing assertion is the CWD one: the stub records the directory it
// was invoked from, because relocking in the wrong directory would rewrite the
// sibling scope's lock while the rule's one-lock `fileFilters` names this one —
// so Renovate would commit nothing and the rev bump would ship unrelocked. A
// cwd-agnostic stub would pass that bug green.

// The shipped entry point, invoked as Renovate will: `bun tools/renovate/…ts`,
// cwd = repo root. Copied into the throwaway repo so the SHIPPED file runs, not
// a re-implementation.
const SCRIPT_REL = "tools/renovate/refresh-devenv-lock.ts";
const CORE_REL = "tools/renovate/refresh-devenv-lock.core.ts";
const REAL_SCRIPT = join(import.meta.dir, "refresh-devenv-lock.ts");
const REAL_CORE = join(import.meta.dir, "refresh-devenv-lock.core.ts");

const ROOT_LOCK_REL = "devenv.lock";
const AGENT_LOCK_REL = "agent-image/devenv.lock";

// Hermetic git: identity from env only, no user/global/system config leakage.
const HERMETIC_ENV = {
	GIT_CONFIG_GLOBAL: "/dev/null",
	GIT_CONFIG_SYSTEM: "/dev/null",
	GIT_AUTHOR_NAME: "t",
	GIT_AUTHOR_EMAIL: "t@t",
	GIT_COMMITTER_NAME: "t",
	GIT_COMMITTER_EMAIL: "t@t",
	HOME: "/dev/null",
};

// Three revs per scope: the base lock's, the one the regex update writes (a
// rev-only rewrite — narHash still describes the base rev), and the one the
// stubbed relock resolves.
const BASE_REV = "1111111111111111111111111111111111111111";
const BUMPED_REV = "2222222222222222222222222222222222222222";
const RELOCKED_REV = "3333333333333333333333333333333333333333";

// A minimal devenv lock carrying the `devenv` fork node the core reads
// (nodes.devenv.locked.rev) plus the `original` block whose repeated
// `"repo": "devenv"` (followed by `"type"`, never `"rev"`) is what makes the
// config.json5 matchString anchor unique — kept here so the fixture has the
// same ambiguity the real locks do.
function devenvLock(rev: string, narHash: string): string {
	return `${JSON.stringify(
		{
			nodes: {
				devenv: {
					locked: {
						lastModified: 1,
						narHash: `sha256-${narHash}`,
						owner: "RigelBuild",
						repo: "devenv",
						rev,
						type: "github",
					},
					original: {
						owner: "RigelBuild",
						repo: "devenv",
						type: "github",
					},
				},
				root: { inputs: { devenv: "devenv" } },
			},
			root: "root",
			version: 7,
		},
		null,
		2,
	)}\n`;
}

// Stub `devenv`: on `devenv update devenv`, record the cwd it ran in and
// rewrite ./devenv.lock (relative to that cwd — exactly how the real devenv
// resolves the lock) to the RELOCKED rev with a fresh narHash, simulating a
// real re-resolution. `.force-noop-relock` in the repo root instead makes it
// exit 0 having written NOTHING, modelling a relock that did not move — the
// half-relock the script must refuse. Offline.
const STUB_DEVENV = `#!/usr/bin/env bash
set -euo pipefail
if [ "\${1:-}" = "update" ]; then
  echo "$PWD" >> "$REPO_ROOT/.devenv-update-cwds"
  echo "$@" >> "$REPO_ROOT/.devenv-update-args"
  if [ -f "$REPO_ROOT/.force-noop-relock" ]; then
    exit 0
  fi
  cat > devenv.lock <<'LOCK'
${devenvLock(RELOCKED_REV, "CCCC")}
LOCK
fi
exit 0
`;

async function buildRepo(): Promise<string> {
	const repo = await mkdtemp(join(tmpdir(), "rig2815-"));
	await mkdir(join(repo, "tools", "renovate"), { recursive: true });
	await mkdir(join(repo, "agent-image"), { recursive: true });
	await mkdir(join(repo, "stubbin"), { recursive: true });

	// Ship the REAL script + its core, so the SHIPPED file runs unmodified.
	await Bun.write(join(repo, SCRIPT_REL), await readFile(REAL_SCRIPT, "utf8"));
	await Bun.write(join(repo, CORE_REL), await readFile(REAL_CORE, "utf8"));

	await Bun.write(join(repo, ROOT_LOCK_REL), devenvLock(BASE_REV, "AAAA"));
	await Bun.write(join(repo, AGENT_LOCK_REL), devenvLock(BASE_REV, "BBBB"));

	await Bun.write(join(repo, "stubbin", "devenv"), STUB_DEVENV);
	await chmod(join(repo, "stubbin", "devenv"), 0o755);

	await $`git init -q -b main`.cwd(repo).env(HERMETIC_ENV).quiet();
	await $`git add -A`.cwd(repo).env(HERMETIC_ENV).quiet();
	await $`git commit -q -m baseline`.cwd(repo).env(HERMETIC_ENV).quiet();
	await $`git remote add origin .`.cwd(repo).env(HERMETIC_ENV).quiet();
	await $`git update-ref refs/remotes/origin/main main`
		.cwd(repo)
		.env(HERMETIC_ENV)
		.quiet();
	return repo;
}

// The rev-only rewrite the customManager's regex update performs: it moves the
// rev string and NOTHING else, so narHash still describes the base rev. This is
// precisely the inconsistent state the relock task exists to repair.
async function applyRegexBump(repo: string, lockRel: string, narHash: string) {
	await Bun.write(join(repo, lockRel), devenvLock(BUMPED_REV, narHash));
}

// Run the shipped script as Renovate does: `bun tools/renovate/…ts`, cwd = repo
// root, the stub first on PATH so no real devenv/network is touched. The real
// `bun` runs the script itself; git stays the real binary.
async function runRefresh(repo: string) {
	return await $`bun ${SCRIPT_REL}`
		.cwd(repo)
		.env({
			...HERMETIC_ENV,
			PATH: `${join(repo, "stubbin")}:${process.env.PATH}`,
			RENOVATE_BASE_BRANCH: "main",
			REPO_ROOT: repo,
		})
		.quiet()
		.nothrow();
}

describe("tools/renovate/refresh-devenv-lock.ts relock (RIG-2815)", () => {
	let repo: string;
	beforeEach(async () => {
		repo = await buildRepo();
	});
	afterEach(async () => {
		if (repo) await rm(repo, { recursive: true, force: true });
	});

	// Self-gate: with neither lock changed vs base, the script is a cheap no-op —
	// no relock, no network. Guards the gate against being dropped (an
	// unconditional run would relock on EVERY Renovate branch).
	test("is a no-op when neither devenv lock changed vs base", async () => {
		const res = await runRefresh(repo);
		expect(res.exitCode).toBe(0);
		expect(res.stdout.toString()).toContain("nothing to do");
		// devenv was never invoked.
		expect(await Bun.file(join(repo, ".devenv-update-cwds")).exists()).toBe(
			false,
		);
		expect(await readFile(join(repo, ROOT_LOCK_REL), "utf8")).toContain(
			BASE_REV,
		);
	});

	// The end-to-end happy path per scope, and the CWD assertion that is the
	// whole point of two scopes: the relock must run in the changed lock's OWN
	// directory (devenv resolves the lock relative to cwd), and must leave the
	// SIBLING lock untouched — the rule's fileFilters admits only the one.
	test.each([
		{ label: "root", lockRel: ROOT_LOCK_REL, dir: ".", narHash: "AAAA" },
		{
			label: "agent-image",
			lockRel: AGENT_LOCK_REL,
			dir: "agent-image",
			narHash: "BBBB",
		},
	])(
		"relocks the $label lock in its own directory, leaving the sibling alone",
		async ({ lockRel, dir, narHash }) => {
			await applyRegexBump(repo, lockRel, narHash);

			const res = await runRefresh(repo);
			expect(res.exitCode).toBe(0);
			expect(res.stdout.toString()).not.toContain("nothing to do");

			// The relock re-resolved the input: the whole lock moved to the relocked
			// rev, not merely the rev string the regex bumped.
			const relocked = await readFile(join(repo, lockRel), "utf8");
			expect(relocked).toContain(RELOCKED_REV);
			expect(relocked).not.toContain(BUMPED_REV);
			expect(res.stdout.toString()).toContain(`now at ${RELOCKED_REV}`);

			// It ran `devenv update devenv` EXACTLY ONCE, in this scope's directory.
			// A wrong cwd would rewrite the sibling's lock (which the rule cannot
			// commit) and leave this one rev-bumped-but-unrelocked.
			const cwds = (await readFile(join(repo, ".devenv-update-cwds"), "utf8"))
				.trim()
				.split("\n");
			expect(cwds).toHaveLength(1);
			expect(cwds[0]).toBe(dir === "." ? repo : join(repo, dir));
			const args = (
				await readFile(join(repo, ".devenv-update-args"), "utf8")
			).trim();
			// The SINGLE named input, not a bare `devenv update` (which would
			// re-lock every input and bloat the PR's diff).
			expect(args).toBe("update devenv");

			// The sibling scope is untouched — the two locks are independent (RD-1).
			const sibling =
				lockRel === ROOT_LOCK_REL ? AGENT_LOCK_REL : ROOT_LOCK_REL;
			expect(await readFile(join(repo, sibling), "utf8")).toContain(BASE_REV);
		},
	);

	// Fail-loud: a relock that wrote NOTHING leaves the regex-bumped rev beside
	// the base lock's narHash — the silent half-relock this task exists to
	// prevent. It must die non-zero in-branch rather than ship it.
	test("fails loud (exit≠0) when the relock leaves the lock byte-identical", async () => {
		await applyRegexBump(repo, ROOT_LOCK_REL, "AAAA");
		await Bun.write(join(repo, ".force-noop-relock"), "");

		const res = await runRefresh(repo);
		expect(res.exitCode).not.toBe(0);
		expect(res.stderr.toString()).toContain("byte-identical");
	});

	// Fail-loud: BOTH locks changed. The two rules carry distinct groupNames so
	// they never share a branch; if that invariant ever breaks, relocking either
	// one is wrong — each rule's fileFilters names ONE lock, so Renovate would
	// commit one relock and silently drop the other. Die instead of guessing.
	test("fails loud (exit≠0) when BOTH locks changed on one branch", async () => {
		await applyRegexBump(repo, ROOT_LOCK_REL, "AAAA");
		await applyRegexBump(repo, AGENT_LOCK_REL, "BBBB");

		const res = await runRefresh(repo);
		expect(res.exitCode).not.toBe(0);
		expect(res.stderr.toString()).toContain("devenv locks changed");
		// And it relocked NEITHER — no partial write.
		expect(await Bun.file(join(repo, ".devenv-update-cwds")).exists()).toBe(
			false,
		);
	});
});
