import { afterEach, beforeEach, describe, expect, test } from "bun:test";
import { chmod, mkdir, mkdtemp, readFile, rm } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { $ } from "bun";

// Orchestration harness for refresh-biome-catalog.ts. The pure transforms are
// unit-tested in refresh-biome-catalog.core.test.ts. This drives the SHIPPED
// entry point in a throwaway git repo with stub nix/bun, offline.

const SCRIPT_REL = "tools/renovate/refresh-biome-catalog.ts";
const CORE_REL = "tools/renovate/refresh-biome-catalog.core.ts";
// The writer's core resolves lock inputs with the channel core's helper.
const LOCK_CORE_REL = "tools/renovate/refresh-devenv-nixpkgs.core.ts";

const HERMETIC_ENV = {
	GIT_CONFIG_GLOBAL: "/dev/null",
	GIT_CONFIG_SYSTEM: "/dev/null",
	GIT_AUTHOR_NAME: "t",
	GIT_AUTHOR_EMAIL: "t@t",
	GIT_COMMITTER_NAME: "t",
	GIT_COMMITTER_EMAIL: "t@t",
	HOME: "/dev/null",
};

// Meissa's raw nixpkgs (the eval target) vs root's own channel src (a decoy
// under the bare `nixpkgs-src` key, as in the real renamed lock).
const MEISSA_SRC_REV = "1111111111111111111111111111111111111111";
const ROOT_SRC_REV = "2222222222222222222222222222222222222222";

function devenvLock(meissaSrcRev: string): string {
	return JSON.stringify(
		{
			nodes: {
				meissa: { inputs: { nixpkgs: "nixpkgs" } },
				nixpkgs: { inputs: { "nixpkgs-src": "nixpkgs-src" } },
				"nixpkgs-src": { locked: { rev: meissaSrcRev } },
				nixpkgs_2: { inputs: { "nixpkgs-src": "nixpkgs-src_2" } },
				"nixpkgs-src_2": { locked: { rev: ROOT_SRC_REV } },
				root: { inputs: { meissa: "meissa", nixpkgs: "nixpkgs_2" } },
			},
			root: "root",
			version: 7,
		},
		null,
		2,
	);
}

// Root package.json with the biome pin plus a same-named `catalog:` CONSUMER
// ref that must survive untouched.
function packageJson(biome: string): string {
	return `${JSON.stringify(
		{
			name: "@compass/workspace",
			workspaces: { catalog: { "@biomejs/biome": biome } },
			devDependencies: { "@biomejs/biome": "catalog:" },
		},
		null,
		2,
	)}\n`;
}

// Stub nix. `eval` answers biome's version keyed off the rev, so a pass proves
// the script evaluated Meissa's src rev; any other rev yields garbage. `shell`
// appends its argv to a log so the harness can assert which biome ran what.
const STUB_NIX = `#!/usr/bin/env bash
set -euo pipefail
if [ "\${1:-}" = "shell" ]; then
  echo "$*" >> .nix-shell.log
  exit 0
fi
ref="\${@: -1}"
case "$ref" in
  "github:NixOS/nixpkgs/${MEISSA_SRC_REV}#legacyPackages.x86_64-linux.biome.version") printf '2.5.9' ;;
  *) printf 'WRONG-REF-%s' "$ref" ;;
esac
`;

// Stub bun: record install, exec the real bun by absolute path for anything
// else (a bare bun would resolve back to this stub via PATH).
const STUB_BUN = `#!/usr/bin/env bash
set -euo pipefail
if [ "\${1:-}" = "install" ]; then
  echo "$*" >> .bun-install.log
  exit 0
fi
exec ${JSON.stringify(process.execPath)} "$@"
`;

async function buildRepo(biomePin: string, meissaSrcRev: string) {
	const repo = await mkdtemp(join(tmpdir(), "biome-catalog-"));
	await mkdir(join(repo, "tools", "renovate"), { recursive: true });
	await mkdir(join(repo, "stubbin"), { recursive: true });
	for (const rel of [SCRIPT_REL, CORE_REL, LOCK_CORE_REL]) {
		await Bun.write(
			join(repo, rel),
			await readFile(join(import.meta.dir, "..", "..", rel), "utf8"),
		);
	}
	await Bun.write(join(repo, "devenv.lock"), devenvLock(meissaSrcRev));
	await Bun.write(join(repo, "package.json"), packageJson(biomePin));
	for (const [name, body] of [
		["nix", STUB_NIX],
		["bun", STUB_BUN],
	] as const) {
		await Bun.write(join(repo, "stubbin", name), body);
		await chmod(join(repo, "stubbin", name), 0o755);
	}
	await $`git init -q -b main`.cwd(repo).env(HERMETIC_ENV).quiet();
	return repo;
}

async function runWriter(repo: string) {
	return await $`bun ${SCRIPT_REL}`
		.cwd(repo)
		.env({
			...HERMETIC_ENV,
			PATH: `${join(repo, "stubbin")}:${process.env.PATH}`,
		})
		.quiet()
		.nothrow();
}

const logOf = async (repo: string, name: string) =>
	(await Bun.file(join(repo, name)).exists())
		? await readFile(join(repo, name), "utf8")
		: "";

describe("tools/renovate/refresh-biome-catalog.ts", () => {
	let repo = "";
	afterEach(async () => {
		if (repo) await rm(repo, { recursive: true, force: true });
	});

	describe("when Meissa's biome moved", () => {
		beforeEach(async () => {
			repo = await buildRepo("2.5.4", MEISSA_SRC_REV);
		});

		// Pin follows Meissa's biome, then bun.lock re-resolves, then the NEW
		// biome migrates config and formats sources — in that order.
		test("rewrites the pin, relocks bun, then migrates and formats", async () => {
			const res = await runWriter(repo);
			expect(res.exitCode).toBe(0);
			const pkg = await readFile(join(repo, "package.json"), "utf8");
			expect(pkg).toContain('"@biomejs/biome": "2.5.9"');
			expect(pkg).toContain('"@biomejs/biome": "catalog:"');
			expect(await logOf(repo, ".bun-install.log")).toBe(
				"install --lockfile-only\n",
			);
			const biome = `shell --extra-experimental-features nix-command flakes github:NixOS/nixpkgs/${MEISSA_SRC_REV}#biome -c biome`;
			expect(await logOf(repo, ".nix-shell.log")).toBe(
				`${biome} migrate --write\n${biome} format --write .\n`,
			);
		});
	});

	// Already equal: nothing to re-resolve, migrate, or format.
	test("is a no-op when the pin already matches Meissa's biome", async () => {
		repo = await buildRepo("2.5.9", MEISSA_SRC_REV);
		const res = await runWriter(repo);
		expect(res.exitCode).toBe(0);
		expect(res.stdout.toString()).toContain("already matches");
		expect(await readFile(join(repo, "package.json"), "utf8")).toBe(
			packageJson("2.5.9"),
		);
		expect(await logOf(repo, ".bun-install.log")).toBe("");
		expect(await logOf(repo, ".nix-shell.log")).toBe("");
	});

	// A garbage version (wrong rev, nix error text) must exit non-zero and
	// never write a pin.
	test("fails loud (exit≠0) when the version eval yields garbage", async () => {
		repo = await buildRepo("2.5.4", "9".repeat(40));
		const res = await runWriter(repo);
		expect(res.exitCode).not.toBe(0);
		expect(await readFile(join(repo, "package.json"), "utf8")).toBe(
			packageJson("2.5.4"),
		);
		expect(await logOf(repo, ".bun-install.log")).toBe("");
	});
});
