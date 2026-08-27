#!/usr/bin/env bun
// The devenv-CLI source tool (RIG-2546): the single place that turns "the
// devenv node of a named devenv.lock" into a usable devenv CLI. Shared by
// .github/workflows/renovate.yml (mode=bin-dir → PATH) and ci.yml (mode=flakeref
// → `nix run`), so neither carries a hand-pinned rev or its own jq/nix blob.
//
// This is the thin execution shell — parse argv, read the lock, resolve, maybe
// build, print one line. All parsing and validation lives in ./core.ts, which
// is pure and unit-tested (./core.test.ts).
//
//   bun tools/toolchain/devenv-cli/index.ts --lock <path> --mode <flakeref|bin-dir>
//     mode=flakeref → print `github:<owner>/<repo>/<rev>#devenv` (no build, no network)
//     mode=bin-dir  → `nix build --no-link --print-out-paths <flakeref>`, create a
//                     temp dir holding a single `devenv` symlink → its bin, print that dir
//
// stdout: exactly one line (the value); all diagnostics to stderr; exit 1 on
// any failure (bad args, missing/invalid lock, failed build).

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
		// stdout stays 'pipe' (we read the out-path below); nix's stderr is
		// inherited so its real build diagnostic streams straight through
		// instead of being swallowed into error.stderr and lost to the
		// generic "Command failed" message the outer catch would print.
		{ encoding: "utf8", stdio: ["ignore", "pipe", "inherit"] },
	).trim();
	if (out === "") {
		throw new Error(`devenv-cli: nix build produced no out-path for ${ref}.`);
	}
	// One symlink named `devenv`, not the raw `<out>/bin` — appending the whole
	// closure bin dir to $GITHUB_PATH could shadow the parity-pinned toolchain
	// (RD-3). shimPlan encodes that single-binary invariant.
	// Intentionally never removed: the caller appends this dir to $GITHUB_PATH
	// and needs it after this process exits (CI runners are ephemeral, so no
	// unlink is wanted — cleaning it up would break the PATH contract).
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
