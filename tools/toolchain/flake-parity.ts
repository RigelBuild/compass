#!/usr/bin/env bun
// The flake nixpkgs-pin parity gate: fail the build when the repo-root flake.lock
// and devenv.lock pin different nixpkgs revisions (compass-distribution §T6). The
// two locks are independent, so a devenv pin bump silently skews the flake — this
// gate turns that drift into a red check.

// Thin execution shell — read the two locks, compare, exit. Parsing and the
// pass/fail decision live in ./flake-parity-core.ts (pure, unit-tested). Mirrors
// parity.ts. Run in CI (moon task flake-gate:flake-parity) or locally.

// Exit 0 = both locks pin the same nixpkgs rev; 1 = they differ or a rev could
// not be read. Unverifiable is a failure, never a skip.

import { readFileSync } from "node:fs";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";

import { compareRevs, nixpkgsLockedRev } from "./flake-parity-core.ts";

const repoRoot = join(dirname(fileURLToPath(import.meta.url)), "..", "..");

const flakeRev = nixpkgsLockedRev(
	readFileSync(join(repoRoot, "flake.lock"), "utf8"),
);
const devenvRev = nixpkgsLockedRev(
	readFileSync(join(repoRoot, "devenv.lock"), "utf8"),
);

const result = compareRevs(flakeRev, devenvRev);
console.log("flake nixpkgs pin parity — flake.lock vs devenv.lock\n");
console.log(result.report);
process.exit(result.ok ? 0 : 1);
