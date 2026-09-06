#!/usr/bin/env bun
// The version.txt guard-parity gate: fail the build when flake.nix's
// `versionBase` and devenv.nix's process-script guard would disagree about a
// candidate version.txt. The two guards are independent hand-written
// expressions in two languages, both feeding `-X main.version` from one file,
// and nothing enforces their agreement by construction — so a skew means one
// lane builds green while the other hard-fails on the same tree, or both build
// and stamp different versions.
//
// This is the thin execution shell — extract each guard, run it, compare,
// exit. The extraction and the comparison live in ./version-guard-core.ts,
// which is pure and unit-tested (./version-guard-core.test.ts). Mirrors
// flake-parity.ts.
//
// It runs the REAL guards, not models of them: the flake half is evaluated by
// `nix eval` on the expression lifted verbatim out of flake.nix, and the devenv
// half by `bash` on the snippet lifted out of devenv.nix. A model would let the
// gate stay green while comparing two fictions.
//
// Run it anywhere: in CI (moon task flake-gate:version-guard) or locally (`bun
// tools/toolchain/version-guard.ts`), where it should always pass.
//
// Exit 0 = both guards agree on every candidate. Exit 1 = they disagree OR a
// guard could not be extracted or run. Unverifiable is a failure, never a skip.

import { spawnSync } from "node:child_process";
import {
	mkdirSync,
	mkdtempSync,
	readFileSync,
	rmSync,
	writeFileSync,
} from "node:fs";
import { tmpdir } from "node:os";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";

import {
	CANDIDATES,
	compareVerdicts,
	extractDevenvGuard,
	extractFlakeGuard,
	type ParityRow,
	type Verdict,
} from "./version-guard-core.ts";

const repoRoot = join(dirname(fileURLToPath(import.meta.url)), "..", "..");

const flakeGuard = extractFlakeGuard(
	readFileSync(join(repoRoot, "flake.nix"), "utf8"),
);
const devenvGuard = extractDevenvGuard(
	readFileSync(join(repoRoot, "devenv.nix"), "utf8"),
);

console.log("version.txt guard parity — flake.nix vs devenv.nix\n");

if (flakeGuard === null || devenvGuard === null) {
	const which = [
		flakeGuard === null ? "flake.nix `versionBase`" : null,
		devenvGuard === null ? "devenv.nix trim+case guard" : null,
	]
		.filter((v) => v !== null)
		.join(" and ");
	console.log(
		`could not extract ${which} — the guard moved or was rewritten past ` +
			"what this gate knows how to lift. Update version-guard-core.ts's " +
			"extractors (and its tests) rather than deleting the gate.",
	);
	process.exit(1);
}

// One scratch dir for the whole run, with a numbered version.txt per
// candidate. Both guards are pointed at these paths, so neither can read the
// committed file by accident.
const scratch = mkdtempSync(join(tmpdir(), "version-guard-"));
const candidatePath = (index: number): string =>
	join(scratch, `${index}`, "version.txt");

CANDIDATES.forEach(({ content }, index) => {
	const path = candidatePath(index);
	mkdirSync(dirname(path), { recursive: true });
	writeFileSync(path, content);
});

// The flake half, as ONE `nix eval` over every candidate rather than one per
// candidate: `getFlake` re-evaluates the whole flake each invocation, which
// turned a 24-row table into ~6 minutes of wall clock — too slow to sit in
// `:ci`. Batching pays that cost once.
//
// The lifted binding becomes a function of `candidate` and is applied to each
// path under `builtins.tryEval`, which catches exactly the guard's own
// `throw` — so `success = false` IS the reject verdict, with no stderr
// scraping. `nixpkgs` is resolved from THIS repo's flake.lock so the gate
// exercises the same `lib.strings.trim` the real build uses, not an ambient
// channel's.
const flakeVerdicts = (): Verdict[] | Error => {
	const paths = CANDIDATES.map(
		(_row, index) => `(/. + "${candidatePath(index)}")`,
	).join(" ");
	const expr =
		`let nixpkgs = (builtins.getFlake "${repoRoot}").inputs.nixpkgs; ` +
		`guard = candidate: ${flakeGuard}; ` +
		"probe = p: let r = builtins.tryEval (guard p); " +
		'in { inherit (r) success; stamp = if r.success then r.value else ""; }; ' +
		`in builtins.toJSON (map probe [ ${paths} ])`;
	const run = spawnSync("nix", ["eval", "--raw", "--impure", "--expr", expr], {
		encoding: "utf8",
		maxBuffer: 16 * 1024 * 1024,
	});
	if (run.error !== undefined) {
		return run.error;
	}
	// A non-zero exit here is a harness failure, never a verdict: every guard
	// throw was already absorbed by tryEval.
	if (run.status !== 0) {
		return new Error(`nix eval failed:\n${run.stderr}`);
	}
	let parsed: unknown;
	try {
		parsed = JSON.parse(run.stdout);
	} catch (cause) {
		return new Error(`nix eval returned unparseable output: ${String(cause)}`);
	}
	if (!Array.isArray(parsed) || parsed.length !== CANDIDATES.length) {
		return new Error(
			`nix eval returned ${Array.isArray(parsed) ? parsed.length : "non-array"} ` +
				`results for ${CANDIDATES.length} candidates`,
		);
	}
	return parsed.map((row: { success: boolean; stamp: string }) =>
		row.success ? { kind: "accept", stamp: row.stamp } : { kind: "reject" },
	);
};

// The devenv half. The lifted snippet runs verbatim under bash with
// version_base seeded exactly as the process script seeds it, then echoes the
// surviving value — so the stamp compared is the one the ldflag would carry.
// Cheap enough per candidate to stay a loop.
const devenvVerdict = (index: number): Verdict | Error => {
	const script = `set -u\nversion_base="$(cat "${candidatePath(index)}")"\n${devenvGuard}\nprintf '%s' "$version_base"\n`;
	const run = spawnSync("bash", ["-c", script], { encoding: "utf8" });
	if (run.error !== undefined) {
		return run.error;
	}
	if (run.status === 0) {
		return { kind: "accept", stamp: run.stdout };
	}
	return run.stderr.includes("version.txt missing or not a version string")
		? { kind: "reject" }
		: new Error(`bash guard failed without rejecting:\n${run.stderr}`);
};

const rows: ParityRow[] = [];
let harnessError: string | undefined;
try {
	const flake = flakeVerdicts();
	if (flake instanceof Error) {
		harnessError = `flake half: ${flake.message}`;
	} else {
		for (const [index, { label }] of CANDIDATES.entries()) {
			const devenv = devenvVerdict(index);
			if (devenv instanceof Error) {
				harnessError = `${label} (devenv): ${devenv.message}`;
				break;
			}
			rows.push({ label, flake: flake[index] as Verdict, devenv });
		}
	}
} finally {
	rmSync(scratch, { recursive: true, force: true });
}

const result = compareVerdicts(rows, harnessError);
console.log(result.report);
process.exit(result.ok ? 0 : 1);
