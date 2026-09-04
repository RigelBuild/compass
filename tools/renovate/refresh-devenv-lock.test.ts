import { afterEach, beforeEach, describe, expect, test } from "bun:test";
import { chmod, mkdir, mkdtemp, readFile, rm } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { $ } from "bun";

// Orchestration harness for tools/renovate/refresh-devenv-lock.ts (RIG-2815).
//
// The pure decisions (which lock changed, the fork-rev read) are unit-tested in
// refresh-devenv-lock.core.test.ts. This drives the SHIPPED entry point end to
// end inside a throwaway git repo with a stub `nix` on PATH, so the step
// SEQUENCING is exercised offline + deterministically: self-gate → pick the
// scope → relock in THAT scope's directory, under THAT scope's own devenv →
// verify the lock actually moved. The real relock
// (`nix run github:RigelBuild/devenv/<rev>#devenv -- update devenv`) runs for
// real on the PR's own branch; here `nix` is stubbed so the failure mode under
// test is the script's own control flow, not the network.
//
// Two assertions are load-bearing:
//   * the CWD one — the stub records the directory it was invoked from, because
//     relocking in the wrong directory would rewrite the sibling scope's lock
//     while the rule's one-lock `fileFilters` names this one, so Renovate would
//     commit nothing and the rev bump would ship unrelocked; and
//   * the FLAKEREF one — the stub records its full argv, and the rev inside the
//     flakeref must be the CHANGED scope's OWN bumped rev. The script
//     self-provisions the devenv CLI from the lock it is about to relock, so
//     each scope is written by the devenv version IT pins; an ambient/PATH
//     devenv would relock agent-image under the ROOT lock's devenv (the two
//     revs differ by design — RD-1 unifies the source, not the locks).
// A cwd-agnostic or argv-agnostic stub would pass both bugs green.

// The shipped entry point, invoked as Renovate will: `bun tools/renovate/…ts`,
// cwd = repo root. Copied into the throwaway repo so the SHIPPED file runs, not
// a re-implementation — together with the two modules it imports.
const SCRIPT_REL = "tools/renovate/refresh-devenv-lock.ts";
const CORE_REL = "tools/renovate/refresh-devenv-lock.core.ts";
const DEVENV_CLI_CORE_REL = "tools/toolchain/devenv-cli/core.ts";
const REAL_SCRIPT = join(import.meta.dir, "refresh-devenv-lock.ts");
const REAL_CORE = join(import.meta.dir, "refresh-devenv-lock.core.ts");
const REAL_DEVENV_CLI_CORE = join(
	import.meta.dir,
	"..",
	"toolchain",
	"devenv-cli",
	"core.ts",
);

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

// A lock whose devenv node carries a SHORT (non-40-hex) rev — what a corrupt
// relock write looks like to the script's post-relock shape guard.
const CORRUPT_LOCK = JSON.stringify({
	nodes: {
		devenv: {
			locked: {
				lastModified: 2,
				narHash: "sha256-DDDD",
				owner: "RigelBuild",
				repo: "devenv",
				rev: "abc123",
				type: "github",
			},
		},
		root: { inputs: { devenv: "devenv" } },
	},
	root: "root",
	version: 7,
});

// Stub `nix`: the script relocks via
// `nix run github:RigelBuild/devenv/<rev>#devenv -- update devenv`, so in bash
// $1=`run`, $2=the flakeref, then `--`, then the devenv subcommand. On that
// shape the stub records the cwd it ran in AND its full argv (so the tests can
// assert both the scope directory and WHICH devenv the script provisioned),
// then rewrites ./devenv.lock (relative to that cwd — exactly how the real
// devenv resolves the lock) to the RELOCKED rev with a fresh narHash,
// simulating a real re-resolution.
//
// Sentinel files in the repo root model the failure modes, so each is exercised
// through the script's REAL control flow rather than a mocked seam:
//   .force-noop-relock    → exit 0 having written NOTHING (a relock that did
//                           not move — the half-relock the script must surface).
//   .force-fail-relock    → exit 1 (the relock itself failed).
//   .force-corrupt-relock → write a lock with a short rev (a lock-shape drift
//                           the post-relock guard must catch).
// And the write is gated on the post-`--` args being exactly `update devenv`:
// any OTHER input name writes nothing, so a wrong-input regression fails
// through BEHAVIOUR (the byte-identical guard fires), not merely an argv-log
// assertion. Offline throughout.
const STUB_NIX = `#!/usr/bin/env bash
set -euo pipefail
if [ "\${1:-}" = "run" ]; then
  echo "$PWD" >> "$REPO_ROOT/.devenv-update-cwds"
  echo "$@" >> "$REPO_ROOT/.devenv-update-args"
  if [ -f "$REPO_ROOT/.force-noop-relock" ]; then
    exit 0
  fi
  if [ -f "$REPO_ROOT/.force-fail-relock" ]; then
    echo "stub nix: forced relock failure" >&2
    exit 1
  fi
  # Drop \`run\` + the flakeref, then the \`--\` separator: what remains is the
  # devenv subcommand the script asked for.
  shift 2
  if [ "\${1:-}" = "--" ]; then
    shift
  fi
  if [ "\${1:-}" != "update" ] || [ "\${2:-}" != "devenv" ]; then
    exit 0
  fi
  if [ -f "$REPO_ROOT/.force-corrupt-relock" ]; then
    printf '%s' '${CORRUPT_LOCK}' > devenv.lock
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
	await mkdir(join(repo, "tools", "toolchain", "devenv-cli"), {
		recursive: true,
	});
	await mkdir(join(repo, "agent-image"), { recursive: true });
	await mkdir(join(repo, "stubbin"), { recursive: true });

	// Ship the REAL script + the modules it imports, so the SHIPPED file runs
	// unmodified (including the devenv-cli resolvers it reuses for the flakeref).
	await Bun.write(join(repo, SCRIPT_REL), await readFile(REAL_SCRIPT, "utf8"));
	await Bun.write(join(repo, CORE_REL), await readFile(REAL_CORE, "utf8"));
	await Bun.write(
		join(repo, DEVENV_CLI_CORE_REL),
		await readFile(REAL_DEVENV_CLI_CORE, "utf8"),
	);

	await Bun.write(join(repo, ROOT_LOCK_REL), devenvLock(BASE_REV, "AAAA"));
	await Bun.write(join(repo, AGENT_LOCK_REL), devenvLock(BASE_REV, "BBBB"));

	await Bun.write(join(repo, "stubbin", "nix"), STUB_NIX);
	await chmod(join(repo, "stubbin", "nix"), 0o755);

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
// root, the stub first on PATH so no real nix/devenv/network is touched. The
// real `bun` runs the script itself; git stays the real binary.
async function runRefresh(repo: string, baseBranch = "main") {
	return await $`bun ${SCRIPT_REL}`
		.cwd(repo)
		.env({
			...HERMETIC_ENV,
			PATH: `${join(repo, "stubbin")}:${process.env.PATH}`,
			RENOVATE_BASE_BRANCH: baseBranch,
			REPO_ROOT: repo,
		})
		.quiet()
		.nothrow();
}

// The flakeref rev the script provisioned its devenv CLI from, read out of the
// stub's argv log: `run github:<owner>/<repo>/<rev>#devenv -- update devenv`.
async function provisionedRev(repo: string): Promise<string> {
	const args = (
		await readFile(join(repo, ".devenv-update-args"), "utf8")
	).trim();
	const rev = /^run github:RigelBuild\/devenv\/([a-f0-9]+)#devenv /.exec(args);
	expect(rev).not.toBeNull();
	return rev?.[1] as string;
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
		// The relock was never invoked.
		expect(await Bun.file(join(repo, ".devenv-update-cwds")).exists()).toBe(
			false,
		);
		expect(await readFile(join(repo, ROOT_LOCK_REL), "utf8")).toContain(
			BASE_REV,
		);
	});

	// The end-to-end happy path per scope, and the two assertions that are the
	// whole point of two scopes: the relock must run in the changed lock's OWN
	// directory (devenv resolves the lock relative to cwd) under the devenv that
	// same lock pins, and must leave the SIBLING lock untouched — the rule's
	// fileFilters admits only the one.
	test.each([
		{ label: "root", lockRel: ROOT_LOCK_REL, dir: ".", narHash: "AAAA" },
		{
			label: "agent-image",
			lockRel: AGENT_LOCK_REL,
			dir: "agent-image",
			narHash: "BBBB",
		},
	])(
		"relocks the $label lock in its own directory, under its own devenv, leaving the sibling alone",
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

			// It relocked EXACTLY ONCE, in this scope's directory. A wrong cwd would
			// rewrite the sibling's lock (which the rule cannot commit) and leave
			// this one rev-bumped-but-unrelocked.
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
			expect(args.endsWith(" -- update devenv")).toBe(true);

			// H2 regression guard: the devenv CLI the script provisioned came from
			// THIS scope's own lock — its flakeref rev is this lock's bumped rev,
			// the fork HEAD the branch is moving to. Running under an ambient/PATH
			// devenv (or the sibling scope's) would put a different rev here, which
			// is exactly the cross-scope coupling this shape removes.
			expect(await provisionedRev(repo)).toBe(BUMPED_REV);

			// The sibling scope is untouched — the two locks are independent (RD-1).
			const sibling =
				lockRel === ROOT_LOCK_REL ? AGENT_LOCK_REL : ROOT_LOCK_REL;
			expect(await readFile(join(repo, sibling), "utf8")).toContain(BASE_REV);
		},
	);

	// Fail-loud: a relock that wrote NOTHING leaves the regex-bumped rev beside
	// the base lock's narHash — the silent half-relock this task exists to
	// surface. The non-zero exit does NOT abort the Renovate branch (Renovate
	// catches a postUpgradeTask failure and still commits the regex bump); it
	// reds the `renovate/artifacts` status, and the human review gate is what
	// stops the merge. So the resulting on-disk lock IS the unrepaired
	// rev-bumped-but-not-relocked state — asserted below so the real contract is
	// pinned, not the imagined "refuses to ship" one.
	test("exits non-zero when the relock leaves the lock byte-identical, and the lock stays rev-bumped-but-unrelocked", async () => {
		await applyRegexBump(repo, ROOT_LOCK_REL, "AAAA");
		await Bun.write(join(repo, ".force-noop-relock"), "");

		const res = await runRefresh(repo);
		expect(res.exitCode).not.toBe(0);
		expect(res.stderr.toString()).toContain("byte-identical");

		// The rev moved (the regex bump) but the narHash did NOT — nothing on disk
		// was repaired, and nothing could be: Renovate re-writes the file from its
		// in-memory updatedPackageFiles after a reset --hard anyway.
		const lock = await readFile(join(repo, ROOT_LOCK_REL), "utf8");
		expect(lock).toContain(BUMPED_REV);
		expect(lock).toContain("sha256-AAAA");
		expect(lock).not.toContain(RELOCKED_REV);
	});

	// Fail-loud: the relock command itself failed. The script must surface that
	// non-zero and NOT leave a bogus/half-written lock behind.
	test("exits non-zero when the relock command fails, leaving no bogus lock", async () => {
		await applyRegexBump(repo, ROOT_LOCK_REL, "AAAA");
		await Bun.write(join(repo, ".force-fail-relock"), "");

		const res = await runRefresh(repo);
		expect(res.exitCode).not.toBe(0);
		// The lock is exactly the regex-bumped input the task was handed — the
		// failed relock wrote nothing.
		expect(await readFile(join(repo, ROOT_LOCK_REL), "utf8")).toBe(
			devenvLock(BUMPED_REV, "AAAA"),
		);
	});

	// Fail-loud: the relock wrote a lock whose devenv node no longer carries a
	// 40-hex fork rev (a devenv lock-format drift). The post-relock shape guard
	// must catch it rather than reporting a bogus "now at …".
	test("exits non-zero when the relock writes a lock with no 40-hex fork rev", async () => {
		await applyRegexBump(repo, ROOT_LOCK_REL, "AAAA");
		await Bun.write(join(repo, ".force-corrupt-relock"), "");

		const res = await runRefresh(repo);
		expect(res.exitCode).not.toBe(0);
		expect(res.stderr.toString()).toContain("devenv fork rev");
		expect(res.stdout.toString()).not.toContain("now at");
	});

	// Fail-loud: BOTH locks changed. The two rules carry distinct groupNames so
	// they never share a branch; if that invariant ever breaks, relocking either
	// one is wrong — each rule's fileFilters names ONE lock, so Renovate would
	// commit one relock and silently drop the other. Exit non-zero instead of
	// guessing.
	test("exits non-zero when BOTH locks changed on one branch", async () => {
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

	// The base-ref fallback DIRECTION: with no `origin/<base>` remote-tracking
	// ref (a local clone, or a runner that never fetched it), the gate must fall
	// back to the bare branch name and still decide correctly — not error out
	// and not silently read "nothing changed".
	test("falls back to the bare base branch when origin/<base> is absent", async () => {
		await $`git update-ref -d refs/remotes/origin/main`
			.cwd(repo)
			.env(HERMETIC_ENV)
			.quiet();
		await applyRegexBump(repo, ROOT_LOCK_REL, "AAAA");

		const res = await runRefresh(repo);
		expect(res.exitCode).toBe(0);
		// The bare ref is what it diffed against…
		expect(res.stdout.toString()).toContain("changed vs main;");
		expect(res.stdout.toString()).not.toContain("origin/main");
		// …and the relock still happened normally.
		expect(await readFile(join(repo, ROOT_LOCK_REL), "utf8")).toContain(
			RELOCKED_REV,
		);
	});

	// Neither `origin/<base>` nor the bare `<base>` resolves: the changed set is
	// UNKNOWN, so the script must fail with its OWN named diagnosis rather than
	// letting a raw `git diff` error surface (and above all never read as
	// "nothing changed" and skip green).
	test("fails with a named error when the base ref does not resolve at all", async () => {
		await applyRegexBump(repo, ROOT_LOCK_REL, "AAAA");

		const res = await runRefresh(repo, "no-such-base");
		expect(res.exitCode).not.toBe(0);
		const stderr = res.stderr.toString();
		expect(stderr).toContain("base ref does not resolve");
		expect(stderr).toContain("origin/no-such-base");
		// The named diagnosis, not a raw git error.
		expect(stderr).not.toContain("unknown revision");
		// And no relock was attempted on an unknown changed set.
		expect(await Bun.file(join(repo, ".devenv-update-cwds")).exists()).toBe(
			false,
		);
	});
});
