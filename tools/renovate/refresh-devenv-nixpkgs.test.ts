import { afterEach, beforeEach, describe, expect, test } from "bun:test";
import { chmod, mkdir, mkdtemp, readFile, rm } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { $ } from "bun";
import { nixpkgsLockedRev } from "../toolchain/flake-parity-core.ts";

// Orchestration harness for refresh-devenv-nixpkgs.ts (RIG-2432). The pure
// transforms are unit-tested in refresh-devenv-nixpkgs.core.test.ts. This drives
// the SHIPPED entry point end to end in a throwaway git repo with stub
// devenv/nix/bun, exercising the step sequencing offline (real nix runs in CI).

// The shipped entry point, invoked as Renovate will: `bun tools/renovate/…ts`,
// cwd = repo root. Copied into the throwaway repo so the SHIPPED file runs, not
// a re-implementation.
const SCRIPT_REL = "tools/renovate/refresh-devenv-nixpkgs.ts";
const CORE_REL = "tools/renovate/refresh-devenv-nixpkgs.core.ts";
const REAL_SCRIPT = join(import.meta.dir, "refresh-devenv-nixpkgs.ts");
const REAL_CORE = join(import.meta.dir, "refresh-devenv-nixpkgs.core.ts");
// Step 3 imports nixpkgsLockedRev from ../toolchain/flake-parity-core.ts, so the
// throwaway repo must carry it at the same repo-root-relative path.
const PARITY_CORE_REL = "tools/toolchain/flake-parity-core.ts";
const REAL_PARITY_CORE = join(
	import.meta.dir,
	"..",
	"toolchain",
	"flake-parity-core.ts",
);

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
// the flake lockstep provably follows the OUTER channel rev.
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

// Minimal root package.json with the biome catalog pin. biome follows Meissa,
// not this channel, so the channel task must leave it byte-equal.
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

// Minimal root flake.nix whose inputs.nixpkgs.url hard-codes the devenv-nixpkgs
// channel (OUTER) rev — the literal step 3 must rewrite in lockstep with a
// channel bump. A second, prose mention of the rev guards that the rewrite
// touches ONLY the URL, not documentation that legitimately names the rev.
function flakeNix(outerRev: string): string {
	return `{
  # Pinned to devenv.lock's channel rev (${outerRev}); the parity gate asserts
  # flake.lock's rev == devenv.lock's.
  inputs.nixpkgs.url = "github:cachix/devenv-nixpkgs/${outerRev}";
  outputs = { self, nixpkgs }: { };
}
`;
}

// Minimal flake.lock recording the nixpkgs channel rev in the `nodes.nixpkgs.
// locked.rev` path the parity gate compares (flake-parity-core.nixpkgsLockedRev).
// Seeded at the BASE rev so a bump makes step 3's `flakeLockRev !== channelRev`
// gate fire; the stub `nix flake update` rewrites it to the bumped rev.
function flakeLock(outerRev: string): string {
	return `${JSON.stringify(
		{
			nodes: {
				nixpkgs: { locked: { rev: outerRev, type: "github" } },
				root: { inputs: { nixpkgs: "nixpkgs" } },
			},
			root: "root",
			version: 7,
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

// Stub `nix`: only the flake re-lock is legal; any other call fails the run.
const STUB_NIX = `#!/usr/bin/env bash
set -euo pipefail
# Step 3 re-locks the flake: \`nix flake update nixpkgs …\`. Simulate the real
# re-lock offline: read the rev flake.nix now pins (step 3 rewrote it first) and
# write a flake.lock whose nodes.nixpkgs.locked.rev matches — the exact field the
# parity gate compares — then record it ran so the harness can assert step 3
# fired. No network.
if [ "\${1:-}" = "flake" ] && [ "\${2:-}" = "update" ]; then
  touch .nix-flake-update-ran
  rev=$(grep -oE 'devenv-nixpkgs/[a-f0-9]{40}' flake.nix | head -1 | cut -d/ -f2)
  cat > flake.lock <<LOCK
{ "nodes": { "nixpkgs": { "locked": { "rev": "$rev", "type": "github" } }, "root": { "inputs": { "nixpkgs": "nixpkgs" } } }, "root": "root", "version": 7 }
LOCK
  exit 0
fi
# This task evaluates nothing: the biome pin moved to refresh-biome-catalog.ts.
echo "unexpected nix invocation: $*" >&2
exit 1
`;

async function buildRepo(): Promise<string> {
	const repo = await mkdtemp(join(tmpdir(), "rig2432-"));
	await mkdir(join(repo, "tools", "renovate"), { recursive: true });
	await mkdir(join(repo, "stubbin"), { recursive: true });

	// Ship the REAL script + its core + the flake-parity-core module step 3
	// imports (nixpkgsLockedRev), so the SHIPPED file runs unmodified.
	await Bun.write(join(repo, SCRIPT_REL), await readFile(REAL_SCRIPT, "utf8"));
	await Bun.write(join(repo, CORE_REL), await readFile(REAL_CORE, "utf8"));
	await mkdir(join(repo, "tools", "toolchain"), { recursive: true });
	await Bun.write(
		join(repo, PARITY_CORE_REL),
		await readFile(REAL_PARITY_CORE, "utf8"),
	);

	await Bun.write(
		join(repo, "devenv.lock"),
		devenvLock(OUTER_REV_BASE, INNER_REV_BASE),
	);
	await Bun.write(join(repo, "package.json"), packageJson("2.4.16"));
	await Bun.write(join(repo, "flake.nix"), flakeNix(OUTER_REV_BASE));
	await Bun.write(join(repo, "flake.lock"), flakeLock(OUTER_REV_BASE));

	for (const [name, body] of [
		["devenv", STUB_DEVENV],
		["nix", STUB_NIX],
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

// Run the shipped script as Renovate does: bun tools/renovate/…ts, cwd = repo
// root, stubs first on PATH.
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
	// — no re-lock, no flake rewrite. Guards the gate against being dropped
	// (an unconditional run would re-lock + rewrite on EVERY Renovate branch).
	test("is a no-op when devenv.lock is unchanged vs base", async () => {
		const res = await runRefresh(repo);
		expect(res.exitCode).toBe(0);
		expect(res.stdout.toString()).toContain("nothing to do");
		// flake.nix untouched.
		const flake = await readFile(join(repo, "flake.nix"), "utf8");
		expect(flake).toContain(`github:cachix/devenv-nixpkgs/${OUTER_REV_BASE}`);
	});

	// The end-to-end happy path: bump devenv.lock's outer rev, run the script, and
	// assert it re-locked and kept flake.nix + flake.lock on the channel rev
	// without touching the biome pin, which Meissa owns now.
	test("re-locks and lockstep-updates the flake, leaving the biome pin", async () => {
		// Simulate the regex update: rewrite ONLY the outer channel rev.
		await Bun.write(
			join(repo, "devenv.lock"),
			devenvLock(OUTER_REV_BUMP, INNER_REV_BASE),
		);

		const res = await runRefresh(repo);
		expect(res.exitCode).toBe(0);
		expect(res.stdout.toString()).not.toContain("nothing to do");

		// The relock ran (the stub devenv wrote the BUMPED inner rev).
		const lock = await readFile(join(repo, "devenv.lock"), "utf8");
		expect(lock).toContain(INNER_REV_BUMP);
		// The biome pin is byte-equal: refresh-biome-catalog.ts owns it.
		const pkg = await readFile(join(repo, "package.json"), "utf8");
		expect(pkg).toBe(packageJson("2.4.16"));
		// Step 3: flake.nix rewritten to the BUMPED OUTER (channel) rev, and the
		// flake re-lock ran. Renovate moved devenv.lock's channel rev; step 6
		// keeps flake.nix's inputs.nixpkgs.url + flake.lock in lockstep so the
		// flake-parity gate does not red on the skew.
		const flake = await readFile(join(repo, "flake.nix"), "utf8");
		expect(flake).toContain(`github:cachix/devenv-nixpkgs/${OUTER_REV_BUMP}`);
		expect(flake).not.toContain(
			`github:cachix/devenv-nixpkgs/${OUTER_REV_BASE}`,
		);
		expect(await Bun.file(join(repo, ".nix-flake-update-ran")).exists()).toBe(
			true,
		);
		// flake.lock ends pinned at the bumped channel rev — the exact field the
		// parity gate compares. Closes the loop on the side the gate reads (the
		// script-only tests otherwise never check flake.lock's end state).
		const flakeLockText = await readFile(join(repo, "flake.lock"), "utf8");
		expect(nixpkgsLockedRev(flakeLockText)).toBe(OUTER_REV_BUMP);
	});

	// Fail-loud: a failed re-lock must stop the run before the flake moves, so
	// a half-relocked devenv.lock never ships beside a lockstepped flake.
	test("fails loud (exit≠0) when the re-lock fails", async () => {
		await Bun.write(
			join(repo, "devenv.lock"),
			devenvLock(OUTER_REV_BUMP, INNER_REV_BASE),
		);
		await Bun.write(
			join(repo, "stubbin", "devenv"),
			`#!/usr/bin/env bash\nexit 1\n`,
		);
		await chmod(join(repo, "stubbin", "devenv"), 0o755);

		const res = await runRefresh(repo);
		expect(res.exitCode).not.toBe(0);
		const flake = await readFile(join(repo, "flake.nix"), "utf8");
		expect(flake).toContain(`github:cachix/devenv-nixpkgs/${OUTER_REV_BASE}`);
	});
});
