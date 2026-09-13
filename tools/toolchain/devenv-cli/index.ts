#!/usr/bin/env bun
// The devenv-CLI source tool (RIG-2546): the single place that turns "the devenv
// node of a named devenv.lock" into a usable devenv CLI. Shared by
// renovate.yml (mode=bin-dir → PATH) and ci.yml (mode=flakeref → nix run), so
// neither carries a hand-pinned rev or its own jq/nix blob.

// Thin execution shell — parse argv, read the lock, resolve, maybe build, print
// one line; all parsing lives in ./core.ts. stdout is exactly one line;
// diagnostics to stderr; exit 1 on any failure.

//   --lock <path> --mode <flakeref|bin-dir>
//     flakeref → print github:<owner>/<repo>/<rev>#devenv (no build, no network)
//     bin-dir  → nix build the flakeref, temp dir with one devenv symlink → bin

import { execFileSync } from "node:child_process";
import { mkdtempSync, symlinkSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { devenvSource, flakeref, parseArgs, shimPlan } from "./core.ts";

async function main(): Promise<void> {
	const request = parseArgs(Bun.argv.slice(2));
	const lockText = await Bun.file(request.lockPath).text();
	const ref = flakeref(devenvSource(lockText));

	if (request.mode === "flakeref") {
		console.log(ref);
		return;
	}

	// mode=bin-dir: realize the store path and expose a single `devenv` binary.
	const out = execFileSync(
		"nix",
		["build", "--no-link", "--print-out-paths", ref],
		// stdout stays 'pipe' (we read the out-path); nix's stderr is inherited so
		// its real build diagnostic streams through instead of being swallowed into
		// the generic "Command failed" message.
		{ encoding: "utf8", stdio: ["ignore", "pipe", "inherit"] },
	).trim();
	if (out === "") {
		throw new Error(`devenv-cli: nix build produced no out-path for ${ref}.`);
	}
	// One symlink named devenv, not the raw <out>/bin — appending the whole closure
	// bin dir to $GITHUB_PATH could shadow the parity-pinned toolchain (RD-3).
	// Never removed: the caller appends this dir to $GITHUB_PATH and needs it after
	// this process exits (ephemeral runners; unlinking would break the contract).
	const shimDir = mkdtempSync(join(tmpdir(), "devenv-shim-"));
	for (const { link, target } of shimPlan(out)) {
		symlinkSync(target, join(shimDir, link));
	}
	console.log(shimDir);
}

try {
	await main();
} catch (error) {
	console.error(error instanceof Error ? error.message : String(error));
	process.exit(1);
}
