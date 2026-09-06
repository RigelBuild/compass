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
// path under `builtins.tryEval`. `nixpkgs` is resolved from THIS repo's
// flake.lock so the gate exercises the same `lib.strings.trim` the real build
// uses, not an ambient channel's.
//
// `tryEval` catches `throw` and `assert` — which is what both of the guard's
// reject branches are, so `success = false` IS the reject verdict, with no
// stderr scraping. It does NOT catch `readFile` I/O or encoding errors, and
// `readFile` is the guard's first act. So content nix cannot represent as a
// string — a NUL byte, or UTF-16 — is OUTSIDE this gate's comparable domain:
// it aborts the whole batched eval into a harness error rather than scoring as
// a verdict. That is a real skew the gate cannot see (bash silently drops NUL
// from `$(cat)` and stamps, while the flake lane dies), so it is deliberately
// not in CANDIDATES: a row for it would red the gate with an opaque "could not
// run" that reads as harness breakage rather than the finding. Both lanes fail
// closed on it in production — the flake hard-errors and devenv's stamp still
// has to survive the character class — so the exposure is a confusing error,
// not a bad stamp. Normalizing version.txt encoding is RIG-3439.
const flakeVerdicts = (): Verdict[] | Error => {
	// JSON.stringify, not bare interpolation: a checkout or TMPDIR path holding
	// a `"` would otherwise break out of the nix string literal. Nothing can
	// execute either way (nix has no command substitution, and both spawnSync
	// calls are argv-form with no intervening shell), so this is hardening — it
	// keeps a path with a quote in it from failing the gate for the wrong
	// reason.
	const paths = CANDIDATES.map(
		(_row, index) => `(/. + ${JSON.stringify(candidatePath(index))})`,
	).join(" ");
	const expr =
		`let nixpkgs = (builtins.getFlake ${JSON.stringify(repoRoot)}).inputs.nixpkgs; ` +
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
//
// `shopt -s globasciiranges` and LC_ALL=C make the comparison locale-invariant.
// bash bracket ranges like `[!0-9A-Za-z.+-]` collate per-locale unless that
// option forces ASCII; it is on by default in the nix bash, but the flake side
// (`builtins.match`) is locale-invariant unconditionally, so pinning it here
// makes the parity verdict a property of the two expressions rather than of the
// bash build options and environment the gate happens to run under.
const devenvVerdict = (index: number): Verdict | Error => {
	const script =
		"set -u\nshopt -s globasciiranges\nexport LC_ALL=C\n" +
		// Single-quoted so a scratch path containing `"`, `$`, or a backtick is
		// inert; `'` itself cannot occur in an mkdtemp path, and the quote-escape
		// dance would obscure the line for a byte that never appears.
		`version_base="$(cat '${candidatePath(index)}')"\n` +
		`${devenvGuard}\nprintf '%s' "$version_base"\n`;
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
