import { afterEach, beforeEach, describe, expect, test } from "bun:test";
import { chmod, mkdir, mkdtemp, readFile, rm } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { $ } from "bun";

// Orchestration harness for tools/renovate/refresh-devenv-nixpkgs.ts
// (RIG-2432).
//
// The pure transforms (rev extraction, pin rewrite) are unit-tested in
// refresh-devenv-nixpkgs.core.test.ts. This drives the SHIPPED entry point end
// to end inside a throwaway git repo with stub `devenv`/`nix`/`bun` on PATH, so
// the step SEQUENCING is exercised offline + deterministically: self-gate →
// re-lock → raw-nixpkgs eval → catalog rewrite → lockfile re-resolve. The real
// nix eval + devenv re-lock run for real against the vendored devenv in the
// PR's own CI; here they are stubbed so the failure mode under test is the
// script's own control flow, not the network.

// The shipped entry point, invoked as Renovate will: `bun tools/renovate/…ts`,
// cwd = repo root. Copied into the throwaway repo so the SHIPPED file runs, not
// a re-implementation.
const SCRIPT_REL = "tools/renovate/refresh-devenv-nixpkgs.ts";
const CORE_REL = "tools/renovate/refresh-devenv-nixpkgs.core.ts";
const REAL_SCRIPT = join(import.meta.dir, "refresh-devenv-nixpkgs.ts");
const REAL_CORE = join(import.meta.dir, "refresh-devenv-nixpkgs.core.ts");

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

// A devenv.lock with distinct outer (channel) and inner (nixpkgs-src) revs, so
// the eval targets the INNER one. The stub `nix` returns versions keyed off the
// inner rev, proving the script evaluated nixpkgs-src, not the channel node.
const INNER_REV_BASE = "1111111111111111111111111111111111111111";
const INNER_REV_BUMP = "2222222222222222222222222222222222222222";
const OUTER_REV_BASE = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa";
const OUTER_REV_BUMP = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb";

function devenvLock(outerRev: string, innerRev: string): string {
	return JSON.stringify(
		{
			nodes: {
				nixpkgs: {
					inputs: { "nixpkgs-src": "nixpkgs-src" },
					locked: {
						lastModified: 1,
						narHash: "sha256-AAAA",
						owner: "cachix",
						repo: "devenv-nixpkgs",
						rev: outerRev,
						type: "github",
					},
					original: {
						owner: "cachix",
						ref: "rolling",
						repo: "devenv-nixpkgs",
						type: "github",
					},
				},
				"nixpkgs-src": {
					flake: false,
					locked: {
						lastModified: 2,
						narHash: "sha256-BBBB",
						owner: "NixOS",
						repo: "nixpkgs",
						rev: innerRev,
						type: "github",
					},
					original: {
						owner: "NixOS",
						ref: "nixpkgs-unstable",
						repo: "nixpkgs",
						type: "github",
					},
				},
				root: { inputs: { nixpkgs: "nixpkgs" } },
			},
			root: "root",
			version: 7,
		},
		null,
		2,
	);
}

// Minimal root package.json with a catalog block carrying the biome pin plus a
// same-named `catalog:` CONSUMER ref that must survive untouched. Compass bakes
// markdownlint-cli2 from the same channel, but it carries NO catalog pin, so
// the fixture — like the real manifest — only pins biome.
function packageJson(biome: string): string {
	return `${JSON.stringify(
		{
			name: "@compass/workspace",
			workspaces: {
				catalog: {
					"@biomejs/biome": biome,
				},
			},
			devDependencies: {
				"@biomejs/biome": "catalog:",
			},
		},
		null,
		2,
	)}\n`;
}

// Stub `devenv`: on `devenv update nixpkgs`, rewrite devenv.lock to the BUMPED
// inner rev — simulating the real re-lock resolving the channel's nixpkgs-src.
// Any other invocation is a no-op success. Offline.
const STUB_DEVENV = `#!/usr/bin/env bash
set -euo pipefail
if [ "\${1:-}" = "update" ]; then
  cat > devenv.lock <<'LOCK'
${devenvLock(OUTER_REV_BUMP, INNER_REV_BUMP)}
LOCK
fi
exit 0
`;

// Stub `nix`: `nix eval --raw … NixOS/nixpkgs/<rev>#…<attr>.version` → a version
// string keyed off BOTH the rev and the attr, so the assertion proves the
// script evaluated the bumped INNER rev (not the outer/base) for biome.
// Emits GARBAGE for an unknown rev so the fail-loud path is reachable.
const STUB_NIX = `#!/usr/bin/env bash
set -euo pipefail
# last arg is the flake ref: github:NixOS/nixpkgs/<rev>#legacyPackages.<sys>.<attr>.version
ref="\${@: -1}"
rev="\${ref#github:NixOS/nixpkgs/}"; rev="\${rev%%#*}"
attrpath="\${ref#*#}"; attr="\${attrpath%.version}"; attr="\${attr##*.}"
if [ "$rev" = "${INNER_REV_BUMP}" ]; then
  case "$attr" in
    biome) printf '2.5.6' ;;
    *) printf 'UNKNOWN-ATTR' ;;
  esac
else
  printf 'WRONG-REV-%s' "$rev"
fi
`;

// Stub `bun`: swallow `bun install --lockfile-only` (record that it ran via a
// marker file) so the harness can assert step 5 fired, offline. The passthrough
// execs the REAL bun by its absolute path (process.execPath, the interpreter
// running this test), NOT `env bun` — a bare `bun` re-resolves through PATH,
// which is prepended with this stub dir, so under Bun ≥1.4 (where Bun-Shell's
// `$` resolves a bare command via PATH rather than the running executable) the
// passthrough would re-enter the stub and recurse until timeout. An absolute
// path can never loop back through the stub.
const STUB_BUN = `#!/usr/bin/env bash
set -euo pipefail
if [ "\${1:-}" = "install" ]; then
  touch .bun-install-ran
  exit 0
fi
exec ${JSON.stringify(process.execPath)} "$@"
`;

async function buildRepo(): Promise<string> {
	const repo = await mkdtemp(join(tmpdir(), "rig2432-"));
	await mkdir(join(repo, "tools", "renovate"), { recursive: true });
	await mkdir(join(repo, "stubbin"), { recursive: true });

	// Ship the REAL script + its core.
	await Bun.write(join(repo, SCRIPT_REL), await readFile(REAL_SCRIPT, "utf8"));
	await Bun.write(join(repo, CORE_REL), await readFile(REAL_CORE, "utf8"));

	await Bun.write(
		join(repo, "devenv.lock"),
		devenvLock(OUTER_REV_BASE, INNER_REV_BASE),
	);
	await Bun.write(join(repo, "package.json"), packageJson("2.4.16"));

	for (const [name, body] of [
		["devenv", STUB_DEVENV],
		["nix", STUB_NIX],
		["bun", STUB_BUN],
	] as const) {
		await Bun.write(join(repo, "stubbin", name), body);
		await chmod(join(repo, "stubbin", name), 0o755);
	}

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

// Run the shipped script as Renovate does: `bun tools/renovate/…ts`, cwd = repo
// root, stubs first on PATH so no real nix/devenv/network is touched. The real
// `bun` runs the script itself; the stub `bun` only intercepts `bun install`
// (its first arg), so we keep the real bun on PATH too — the stub `exec`s it
// for non-install calls, but the script is launched with the real bun here.
async function runRefresh(repo: string) {
	return await $`bun ${SCRIPT_REL}`
		.cwd(repo)
		.env({
			...HERMETIC_ENV,
			PATH: `${join(repo, "stubbin")}:${process.env.PATH}`,
			RENOVATE_BASE_BRANCH: "main",
		})
		.quiet()
		.nothrow();
}

describe("tools/renovate/refresh-devenv-nixpkgs.ts lockstep (RIG-2432)", () => {
	let repo: string;
	beforeEach(async () => {
		repo = await buildRepo();
	});
	afterEach(async () => {
		if (repo) await rm(repo, { recursive: true, force: true });
	});

	// Self-gate: with devenv.lock unchanged vs base, the script is a cheap no-op
	// — no re-lock, no eval, no rewrite. Guards the gate against being dropped
	// (an unconditional run would re-lock + rewrite on EVERY Renovate branch).
	test("is a no-op when devenv.lock is unchanged vs base", async () => {
		const res = await runRefresh(repo);
		expect(res.exitCode).toBe(0);
		expect(res.stdout.toString()).toContain("nothing to do");
		// package.json untouched.
		const pkg = await readFile(join(repo, "package.json"), "utf8");
		expect(pkg).toContain('"@biomejs/biome": "2.4.16"');
	});

	// The end-to-end happy path: bump devenv.lock's outer rev (what the
	// customManager's regex update does), run the script, and assert it
	// re-locked, evaluated the BUMPED INNER rev, rewrote the biome catalog pin to
	// the evaluated version, left the `catalog:` consumer alone, and ran the
	// lockfile re-resolve.
	test("re-locks, evaluates inner rev, and rewrites the biome catalog pin", async () => {
		// Simulate the regex update: rewrite ONLY the outer channel rev.
		await Bun.write(
			join(repo, "devenv.lock"),
			devenvLock(OUTER_REV_BUMP, INNER_REV_BASE),
		);

		const res = await runRefresh(repo);
		expect(res.exitCode).toBe(0);
		expect(res.stdout.toString()).not.toContain("nothing to do");

		// Pin rewritten to the stub-nix version for the BUMPED inner rev.
		const pkg = await readFile(join(repo, "package.json"), "utf8");
		expect(pkg).toContain('"@biomejs/biome": "2.5.6"');
		// Consumer ref preserved.
		expect(pkg).toContain('"@biomejs/biome": "catalog:"');
		// Step 5 fired (bun install --lockfile-only).
		expect(await Bun.file(join(repo, ".bun-install-ran")).exists()).toBe(true);
	});

	// No-op-rewrite branch: a channel bump that does NOT move biome (the stub
	// still evaluates to the CURRENT pin) leaves package.json byte-equal and
	// SKIPS the lockfile re-resolve. Force this by seeding package.json to the
	// version the stub returns for the bumped inner rev, then bump.
	test("skips rewrite + lockfile when biome did not move", async () => {
		await Bun.write(join(repo, "package.json"), packageJson("2.5.6"));
		await $`git add -A`.cwd(repo).env(HERMETIC_ENV).quiet();
		await $`git commit -q -m seed`.cwd(repo).env(HERMETIC_ENV).quiet();
		await $`git update-ref refs/remotes/origin/main main`
			.cwd(repo)
			.env(HERMETIC_ENV)
			.quiet();
		await Bun.write(
			join(repo, "devenv.lock"),
			devenvLock(OUTER_REV_BUMP, INNER_REV_BASE),
		);

		const res = await runRefresh(repo);
		expect(res.exitCode).toBe(0);
		expect(res.stdout.toString()).toContain("already match");
		// Lockfile re-resolve skipped (no pin change).
		expect(await Bun.file(join(repo, ".bun-install-ran")).exists()).toBe(false);
	});

	// Fail-loud: if the version eval yields a non-version string (a broken rev,
	// a nix error), the script must die non-zero, never write a garbage pin.
	// Force it by bumping the inner rev to one the stub doesn't recognize.
	test("fails loud (exit≠0) when the version eval yields garbage", async () => {
		await Bun.write(
			join(repo, "devenv.lock"),
			devenvLock(OUTER_REV_BUMP, "9999999999999999999999999999999999999999"),
		);
		// Stub devenv would rewrite to INNER_REV_BUMP; override so the re-lock
		// keeps the unrecognized inner rev the stub nix returns garbage for.
		await Bun.write(
			join(repo, "stubbin", "devenv"),
			`#!/usr/bin/env bash\nexit 0\n`,
		);
		await chmod(join(repo, "stubbin", "devenv"), 0o755);

		const res = await runRefresh(repo);
		expect(res.exitCode).not.toBe(0);
		// package.json pin untouched (no garbage written).
		const pkg = await readFile(join(repo, "package.json"), "utf8");
		expect(pkg).toContain('"@biomejs/biome": "2.4.16"');
	});
});
