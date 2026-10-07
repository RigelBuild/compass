#!/usr/bin/env bun
// The version.txt guard-parity gate: fail the build when flake.nix's versionBase
// and devenv.nix's process-script guard would disagree about a candidate
// version.txt. The two guards are independent hand-written expressions in two
// languages feeding one -X main.version, with nothing enforcing agreement.

// This is the thin execution shell — extract each guard, run it, compare, exit.
// The extraction and comparison live in ./version-guard-core.ts (pure,
// unit-tested). Mirrors flake-parity.ts.

// It runs the REAL guards: the flake half via nix eval on the expression lifted
// verbatim out of flake.nix, the devenv half via bash on the snippet from
// devenv.nix. A model would let the gate stay green while comparing two fictions.

// Run in CI (moon task flake-gate:version-guard) or locally. Exit 0 = both
// guards agree on every candidate; 1 = they disagree or a guard could not be
// extracted or run. Unverifiable is a failure, never a skip.

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
		devenvGuard === null
			? "devenv.nix version_file seed + trim+case guard"
			: null,
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

// The flake half, as ONE nix eval over every candidate: getFlake re-evaluates
// the whole flake per invocation, which turned a 24-row table into ~6 minutes.
// Batching pays that cost once. nixpkgs lib comes from THIS repo's flake.lock
// node, so the gate runs the same lib.strings.trim the real build uses.
// getFlake on the locked nixpkgs node, not on the repo: the repo form copies
// the working tree, and a file another gate renames mid-copy fails the eval.

// tryEval catches throw and assert — both of the guard's reject branches — so
// success=false IS the reject verdict. It does NOT catch readFile I/O/encoding
// errors, so content nix cannot represent as a string (a NUL byte, UTF-16) is
// OUTSIDE the comparable domain and aborts the batch into a harness error.

// The devenv lane refuses the same input up front (its NUL check, pinned by
// version-guard-core.test.ts), so both lanes fail closed on it; it stays out of
// CANDIDATES only because a row would red the gate with an opaque "could not
// run" from the flake half's batch.
const flakeVerdicts = (): Verdict[] | Error => {
	// JSON.stringify, not bare interpolation: a path holding a " would break out
	// of the nix string literal. Nothing can execute either way (nix has no
	// command substitution, spawnSync is argv-form), so this is hardening against
	// a quoted path failing the gate for the wrong reason.
	const paths = CANDIDATES.map(
		(_row, index) => `(/. + ${JSON.stringify(candidatePath(index))})`,
	).join(" ");
	const lock = `builtins.fromJSON (builtins.readFile ${JSON.stringify(join(repoRoot, "flake.lock"))})`;
	const expr =
		`let lock = ${lock}; ` +
		"node = (builtins.getAttr lock.nodes.root.inputs.nixpkgs lock.nodes).locked; " +
		'nixpkgs = builtins.getFlake ("github:" + node.owner + "/" + node.repo + "/" + node.rev); ' +
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

// The devenv half. The lifted snippet runs verbatim under bash — seed, NUL
// refusal and all — with the candidate path as $1, then echoes the surviving
// value, so the stamp compared is the one the ldflag would carry.

// shopt -s globasciiranges and export LC_ALL=C both make the comparison
// locale-invariant. With NEITHER, [!0-9A-Za-z.+-] under a UTF-8 locale accepts
// 0.1.0é, which the flake side (builtins.match) rejects — a verdict differing by
// environment. Either pin alone restores REJECT.

// Both are kept because they close it by different mechanisms: LC_ALL=C fixes the
// collation locale but is an env var something could displace, while
// globasciiranges forces ASCII-code-point ranges but is a bash build default.
// Together the verdict is a property of the guards, not the environment.
const devenvVerdict = (index: number): Verdict | Error => {
	const script =
		"set -u\nshopt -s globasciiranges\nexport LC_ALL=C\n" +
		`${devenvGuard}\nprintf '%s' "$version_base"\n`;
	// argv-form: the path is $1, never spliced into the script text.
	const run = spawnSync(
		"bash",
		["-c", script, "version-guard", candidatePath(index)],
		{ encoding: "utf8" },
	);
	if (run.error !== undefined) {
		return run.error;
	}
	if (run.status === 0) {
		return { kind: "accept", stamp: run.stdout };
	}
	// The NUL message is reached only by version-guard-core.test.ts, not CANDIDATES.
	return /version\.txt (missing or not a version string|contains a NUL byte)/.test(
		run.stderr,
	)
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
