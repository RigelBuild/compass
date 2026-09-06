// Tests for the pure half of the version.txt guard-parity gate.
//
// The property under test throughout is the one the gate exists for: it must be
// CAPABLE OF FAILING on a genuine skew, and it must never turn "I could not
// read a guard" into a pass. So the extractors are tested against the real
// flake.nix and devenv.nix in the tree and against every way a guard can be
// unliftable, and the comparator against agreement, each direction of
// disagreement, a same-verdict-different-stamp split, and a harness error.

import { describe, expect, test } from "bun:test";
import { readFileSync } from "node:fs";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";

import {
	CANDIDATES,
	compareVerdicts,
	extractDevenvGuard,
	extractFlakeGuard,
	type ParityRow,
} from "./version-guard-core.ts";

const repoRoot = join(dirname(fileURLToPath(import.meta.url)), "..", "..");
const realFlake = readFileSync(join(repoRoot, "flake.nix"), "utf8");
const realDevenv = readFileSync(join(repoRoot, "devenv.nix"), "utf8");

describe("extractFlakeGuard", () => {
	// Against the REAL file, not a fixture: the gate's whole value is that it
	// runs the shipped expression, so the extractor must track the shipped file.
	// A rename or restructure that breaks lifting fails here rather than
	// silently degrading the gate to a no-op.
	//
	// Asserted structurally — that the guard trims, applies SOME class, and has
	// both reject branches — never against the class's literal bytes. Pinning
	// `[0-9A-Za-z.+-]` here would red this suite on a synchronized widening of
	// both lanes, which preserves parity and is a correct change; a suite that
	// fails on correct maintenance gets its assertions relaxed, and then the
	// extractor's tracking guarantee is gone. Which bytes each lane admits is
	// the candidate table's business to compare, not this test's to freeze.
	test("lifts versionBase out of the real flake.nix", () => {
		const guard = extractFlakeGuard(realFlake);
		expect(guard).not.toBeNull();
		expect(guard).toContain("lib.strings.trim");
		expect(guard).toMatch(/builtins\.match "\[.+\]\+?"/);
		expect(guard).toContain("version.txt is missing or empty");
		expect(guard).toContain("version.txt is not a version string");
	});

	// The lifted text is fed to `nix eval --expr`, which needs an expression.
	// A trailing `;` (the attribute binding's terminator) is a syntax error
	// there, and a lifted `readFile ./version.txt` would evaluate the COMMITTED
	// file for every candidate — a gate that always compares the same input.
	test("returns an evaluable expression pointed at the candidate", () => {
		const guard = extractFlakeGuard(realFlake) as string;
		expect(guard.endsWith(";")).toBe(false);
		expect(guard).toContain("builtins.readFile candidate");
		expect(guard).not.toContain("builtins.readFile ./version.txt");
	});

	test.each([
		["the binding is absent", "{ outputs = { }; }"],
		[
			"the following version binding is absent",
			"versionBase =\n  let v = nixpkgs.lib.strings.trim (builtins.readFile ./version.txt);\n  in v;\n",
		],
		[
			"the readFile call moved",
			"versionBase =\n  let v = builtins.readFile ./VERSION;\n  in v;\n      version =\n",
		],
	])("yields null when %s", (_label, source) => {
		expect(extractFlakeGuard(source)).toBeNull();
	});
});

describe("extractDevenvGuard", () => {
	test("lifts the trim loop and validating case out of the real devenv.nix", () => {
		const guard = extractDevenvGuard(realDevenv);
		expect(guard).not.toBeNull();
		expect(guard).toContain("while :; do");
		// Structural, per the note on the flake extractor above: a negated
		// bracket expression must be present, but not which bytes it names.
		expect(guard).toMatch(/\*\[!.+\]\*/);
		expect(guard).toContain("version.txt missing or not a version string");
	});

	// The snippet runs under bash, which does not know nix's `''${` escape —
	// leaving it in place would make every parameter expansion a literal.
	test("unescapes nix string interpolation to the bash the script receives", () => {
		const guard = extractDevenvGuard(realDevenv) as string;
		expect(guard).not.toContain("''${");
		// biome-ignore-start lint/suspicious/noTemplateCurlyInString: these are
		// bash parameter expansions in the lifted shell snippet, not JS template
		// placeholders — the literal `${...}` text is exactly what is asserted.
		expect(guard).toContain("${version_base#?}");
		expect(guard).toContain("${version_base%?}");
		// biome-ignore-end lint/suspicious/noTemplateCurlyInString: see above.
	});

	// Two `esac`s close the guard (the trim loop's and the validator's). Ending
	// at the FIRST one would lift a trim with no validation, so the gate would
	// compare the flake's guard against no guard at all and report agreement
	// only by accident.
	test("ends at the validating case, not the trim loop's esac", () => {
		const guard = extractDevenvGuard(realDevenv) as string;
		expect(guard.match(/esac/g)).toHaveLength(2);
		expect(guard.trimEnd().endsWith("esac")).toBe(true);
	});

	test.each([
		[
			"the trim loop is absent",
			'case "$version_base" in\n  "") exit 1 ;;\nesac\n',
		],
		[
			"the validating case is absent",
			'        while :; do\n          case "$v" in\n            *) break ;;\n          esac\n        done\n',
		],
		[
			"the character class moved",
			'        while :; do\n          case "$v" in\n            *) break ;;\n          esac\n        done\n        case "$v" in\n          "") exit 1 ;;\n        esac\n',
		],
	])("yields null when %s", (_label, source) => {
		expect(extractDevenvGuard(source)).toBeNull();
	});
});

describe("compareVerdicts", () => {
	const accept = (stamp: string): ParityRow["flake"] => ({
		kind: "accept",
		stamp,
	});
	const reject: ParityRow["flake"] = { kind: "reject" };

	test("passes when both lanes accept the same stamp", () => {
		const result = compareVerdicts([
			{ label: "ok", flake: accept("0.1.0"), devenv: accept("0.1.0") },
		]);
		expect(result.ok).toBe(true);
		expect(result.report).toContain("all 1 candidates agree");
	});

	test("passes when both lanes reject", () => {
		expect(
			compareVerdicts([{ label: "ok", flake: reject, devenv: reject }]).ok,
		).toBe(true);
	});

	// Both directions, because a gate that only catches one is half a gate. The
	// shipped skew was flake-accepts/devenv-rejects (a CRLF version.txt); the
	// reverse arises whenever devenv's trim set is the wider one, which is
	// exactly what the leading-vertical-tab and trailing-form-feed candidates
	// are in CANDIDATES to witness.
	test.each([
		["flake accepts, devenv rejects", accept("0.1.0"), reject],
		["devenv accepts, flake rejects", reject, accept("0.1.0")],
	])("fails when %s", (_label, flake, devenv) => {
		const result = compareVerdicts([{ label: "skew", flake, devenv }]);
		expect(result.ok).toBe(false);
		expect(result.report).toContain("SKEW");
	});

	// The subtlest failure: both lanes build, so nothing looks broken, but one
	// version.txt yielded two different `-X main.version` values.
	test("fails when both accept but the stamps differ", () => {
		const result = compareVerdicts([
			{ label: "split", flake: accept("0.1.0"), devenv: accept("0.1.0+dev") },
		]);
		expect(result.ok).toBe(false);
		expect(result.report).toContain("1 of 1 candidates SKEW");
	});

	// Unverifiable is a failure, never a skip: a harness error means the gate
	// did not observe the invariant, which is not the same as observing it hold.
	test("fails when the harness could not run, even with agreeing rows", () => {
		const result = compareVerdicts(
			[{ label: "ok", flake: accept("0.1.0"), devenv: accept("0.1.0") }],
			"nix eval exploded",
		);
		expect(result.ok).toBe(false);
		expect(result.report).toContain("gate could not run: nix eval exploded");
	});

	test("fails when no candidate was evaluated", () => {
		expect(compareVerdicts([]).ok).toBe(false);
	});
});

describe("CANDIDATES", () => {
	// The whitespace-boundary rows are the ones that caught the shipped skew;
	// losing them would make the table look thorough while retiring its
	// load-bearing half.
	test.each([
		["CRLF", "0.1.0\r\n"],
		["lone CR", "0.1.0\r"],
		["tab-padded", "\t0.1.0\t\n"],
		["empty", ""],
		["inner space", "0.1.0 rc1\n"],
		// The trim-set discriminators: class-legal core, padded with a byte the
		// trim sets exclude. Padding with two such bytes at once cannot witness a
		// trim-set change (the untrimmed one keeps the value out of the class), so
		// each must stay a row of its own.
		["leading vertical tab only", "\v0.1.0\n"],
		["trailing form feed only", "0.1.0\f\n"],
		// LF must be positioned LEADING: `$(cat)` strips trailing newlines before
		// devenv's trim loop runs, so only a leading one can witness LF leaving
		// the trim set.
		["leading newline only", "\n0.1.0\n"],
	])("covers %s", (_label, content) => {
		expect(CANDIDATES.some((row) => row.content === content)).toBe(true);
	});

	test("every label is unique, so a SKEW row names one candidate", () => {
		const labels = CANDIDATES.map((row) => row.label);
		expect(new Set(labels).size).toBe(labels.length);
	});
});
