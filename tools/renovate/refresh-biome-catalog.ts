// Renovate postUpgradeTask: keep the `@biomejs/biome` catalog pin equal to the
// biome Meissa ships (the dev shell's biome comes from the `meissa` input in
// devenv.lock). No relock and no flake lockstep: the calling rule relocks first.

// 1. Build the locked Meissa flake ref from devenv.lock (by input name) and eval
// its exported biome's version (pure fetch+eval, no build). 2. Rewrite the
// catalog pin. 3. When it moved: bun install --lockfile-only, then biome migrate
// and format with that exact biome, so config and sources match the new version.

// Invoked by the devenv-nixpkgs and meissa packageRules (config.json5),
// allowlisted in bot-config.json5. Needs nix + bun + network on PATH.

// Exit 0 = pin matches (rewritten or already equal); 1 = a step failed — fail
// loud, never ship a pin that disagrees with the shell's biome.

import { readFileSync } from "node:fs";
import { $ } from "bun";
import {
	BIOME_CATALOG_KEY,
	meissaFlakeRef,
	rewriteCatalogPin,
} from "./refresh-biome-catalog.core.ts";

const DEVENV_LOCK = "devenv.lock";
const PACKAGE_JSON = "package.json";
// The system the version is read for; biome's version is system-independent.
const NIX_SYSTEM = "x86_64-linux";
// The extra flag keeps a local run (tests, a manual repro) independent of the
// ambient nix.conf.
const NIX_FEATURES = ["--extra-experimental-features", "nix-command flakes"];

async function main(): Promise<number> {
	// Every path below is repo-root-relative; Renovate's cwd is not guaranteed.
	const repoRoot = (await $`git rev-parse --show-toplevel`.text()).trim();
	process.chdir(repoRoot);

	const meissa = meissaFlakeRef(readFileSync(DEVENV_LOCK, "utf8"));
	const versionRef = `${meissa}#packages.${NIX_SYSTEM}.biome.version`;
	console.log(`refresh-biome-catalog: evaluating ${versionRef} ...`);
	const version = (
		await $`nix eval --raw ${NIX_FEATURES} ${versionRef}`.text()
	).trim();
	if (!/^\d+\.\d+\.\d+$/.test(version)) {
		throw new Error(
			`refresh-biome-catalog: eval of ${versionRef} yielded a non-version string ${JSON.stringify(version)}`,
		);
	}
	console.log(`refresh-biome-catalog: Meissa biome=${version}`);

	const before = readFileSync(PACKAGE_JSON, "utf8");
	const after = rewriteCatalogPin(before, BIOME_CATALOG_KEY, version);
	if (after === before) {
		console.log(
			"refresh-biome-catalog: catalog pin already matches Meissa's biome; no rewrite.",
		);
		return 0;
	}
	await Bun.write(PACKAGE_JSON, after);
	console.log("refresh-biome-catalog: rewrote the biome catalog pin.");

	// bun.lock mirrors the catalog, so the frozen-lockfile check needs it.
	await $`bun install --lockfile-only`;

	// Migrate config schemas and reformat with the NEW biome, not whatever biome
	// the runner has on PATH.
	const biome = [
		"nix",
		"shell",
		...NIX_FEATURES,
		`${meissa}#biome`,
		"-c",
		"biome",
	];
	console.log("refresh-biome-catalog: biome migrate + format ...");
	await $`${biome} migrate --write`;
	await $`${biome} format --write .`;

	console.log("refresh-biome-catalog: done.");
	return 0;
}

process.exit(await main());
