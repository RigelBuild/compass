// Pure parsing and comparison for the version.txt guard-parity gate. No I/O, no
// process exec — total functions over strings, so the interesting half is
// unit-testable (version-guard-core.test.ts) and the executable shell
// (version-guard.ts) stays thin. Mirrors the flake-parity-core.ts split.
//
// THE INVARIANT THIS GATE ENFORCES. Two independent build paths read
// version.txt and stamp `-X main.version` from it, each guarding it in its own
// language:
//
//   flake.nix    `lib.strings.trim`, then `builtins.match "[0-9A-Za-z.+-]+"`,
//                throwing on an empty or non-matching value.
//   devenv.nix   a bash loop trimming the same four whitespace bytes, then a
//                `case` rejecting `""` or `*[!0-9A-Za-z.+-]*`.
//
// They must accept exactly the same file. When they disagree one lane builds
// green while the other hard-fails on the identical tree — `nix build
// .#compass-server` succeeding while `devenv up` dies — or, worse, both build
// and stamp DIFFERENT versions from one source, breaking the one-stamp promise
// (Global Constraint 4). Nothing enforces the agreement by construction: the
// guards are two hand-written expressions in two languages, and editing either
// silently skews it. That skew already shipped once (the flake matched its
// class against the TRIMMED value while devenv matched the raw `$(cat)`, so a
// CRLF or tab-padded version.txt passed the flake and failed devenv).
//
// This module does NOT re-model the guards. A restatement would leave the gate
// green while comparing two fictions of its own making. It EXTRACTS each real
// guard from its source file and the shell executes it — `nix eval` for the
// flake expression, `bash` for the devenv snippet — so the gate tests the code
// that ships. Extraction failure is a gate FAILURE, never a skip: the
// unverifiable-is-a-failure rule flake-parity-core.ts and parity-core.ts both
// take.

/** What a guard did with one candidate version.txt content. */
export type Verdict =
	| { readonly kind: "accept"; readonly stamp: string }
	| { readonly kind: "reject" };

/** One row of the parity table: a candidate plus both lanes' verdicts. */
export interface ParityRow {
	readonly label: string;
	readonly flake: Verdict;
	readonly devenv: Verdict;
}

/** The gate verdict plus a legible per-row report. */
export interface ParityReport {
	readonly report: string;
	readonly ok: boolean;
}

/**
 * Candidate version.txt contents the gate drives both real guards over. Each is
 * a way the file plausibly ends up on disk or gets hand-edited. The whitespace
 * boundary rows carry the most weight: the repo has no .gitattributes and no
 * .editorconfig, so nothing normalizes line endings on a mixed-tooling
 * checkout — a CRLF version.txt is a realistic input, and it is exactly what
 * the shipped skew mishandled.
 */
export const CANDIDATES: readonly {
	readonly label: string;
	readonly content: string;
}[] = [
	{ label: "committed shape (trailing LF)", content: "0.1.0\n" },
	{ label: "no trailing newline", content: "0.1.0" },
	{ label: "CRLF", content: "0.1.0\r\n" },
	{ label: "lone CR", content: "0.1.0\r" },
	{ label: "tab-padded", content: "\t0.1.0\t\n" },
	{ label: "space-padded", content: "  0.1.0  \n" },
	{ label: "empty", content: "" },
	{ label: "whitespace only", content: "   \n" },
	{ label: "CRLF only", content: "\r\n" },
	{ label: "release+metadata", content: "1.2.3-rc.1+build.9\n" },
	{ label: "dirty rev suffix", content: "0.1.0+g10c5fa7-dirty\n" },
	{ label: "inner space", content: "0.1.0 rc1\n" },
	{ label: "embedded newline", content: "0.1.0\n9.9.9\n" },
	{ label: "underscore", content: "0.1.0_x\n" },
	{ label: "semicolon", content: "0.1.0;id\n" },
	{
		label: "command substitution",
		content: "$(touch /tmp/version-guard-pwned)\n",
	},
	{ label: "backtick", content: "`id`\n" },
	{ label: "slash", content: "0.1.0/x\n" },
	{ label: "quote", content: '0.1.0"x\n' },
	{ label: "non-ASCII", content: "0.1.0é\n" },
	// Trim-set discriminators. A row can only witness a change to either lane's
	// TRIM SET if its core is class-legal and its padding is a byte the trim
	// sets currently EXCLUDE — then widening one lane's set flips that lane to
	// accept while the other still rejects. Padding with two such bytes at once
	// (`\v0.1.0\f`) cannot do this: whichever byte is still untrimmed keeps the
	// value outside the class, so the row reads reject/reject however the trim
	// sets move. Hence one row per byte, each alone. `\v` and `\f` are the
	// whitespace bytes `[[:space:]]` includes and ` \t\r\n` does not, so these
	// two rows are exactly what reds the gate if a lane reaches for a broader
	// whitespace class. Trim NARROWING is already witnessed from the other
	// side by the CR/tab/space rows, whose cores are class-legal too.
	{ label: "leading vertical tab only", content: "\v0.1.0\n" },
	{ label: "trailing form feed only", content: "0.1.0\f\n" },
	{ label: "bare word", content: "banana\n" },
	{ label: "dashes only", content: "----\n" },
	{ label: "coreless metadata", content: "+g10c5fa7\n" },
];

/**
 * Extract flake.nix's `versionBase` binding — the `let`/`in`/if-chain from
 * `versionBase =` up to the `version =` binding that follows it. Returned
 * verbatim so the shell evaluates the shipped expression rather than a copy
 * kept in sync by hand. `readFile ./version.txt` is rewritten to read a
 * caller-supplied path so each candidate can be fed through it; that rewrite is
 * asserted to have happened, so a renamed source stops the gate instead of
 * silently evaluating a hardcoded read of the committed file.
 *
 * Returns null when the binding or the readFile call is absent — which the
 * caller treats as a failure, not a pass.
 */
export function extractFlakeGuard(flakeNix: string): string | null {
	const start = flakeNix.indexOf("versionBase =");
	if (start === -1) {
		return null;
	}
	const rest = flakeNix.slice(start);
	const end = rest.indexOf("\n      version =");
	if (end === -1) {
		return null;
	}
	// `.trim()` then drop the binding's terminating `;`: the lifted text must be
	// a standalone EXPRESSION for `nix eval --expr`, and the semicolon that
	// closes the attribute binding in flake.nix is a syntax error there.
	const binding = rest
		.slice("versionBase =".length, end)
		.trim()
		.replace(/;$/, "");
	if (!binding.includes("builtins.readFile ./version.txt")) {
		return null;
	}
	return binding.replace(
		"builtins.readFile ./version.txt",
		"builtins.readFile candidate",
	);
}

/**
 * Extract devenv.nix's guard — the trim loop through the closing `esac` of the
 * validating `case`, starting at the `while :; do` that opens the trim. Nix
 * `''`-string escapes (`''${`) are unescaped to the bash the process script
 * actually receives, so the snippet runs as the rendered script runs it.
 *
 * Returns null when either delimiter is absent.
 */
export function extractDevenvGuard(devenvNix: string): string | null {
	const start = devenvNix.indexOf("        while :; do");
	if (start === -1) {
		return null;
	}
	const rest = devenvNix.slice(start);
	// The trim loop's own `esac` comes first; the validating `case` closes at
	// the second one, which is where the guard ends.
	const firstEsac = rest.indexOf("esac");
	if (firstEsac === -1) {
		return null;
	}
	const secondEsac = rest.indexOf("esac", firstEsac + 4);
	if (secondEsac === -1) {
		return null;
	}
	const snippet = rest.slice(0, secondEsac + "esac".length);
	// Landmark on the guard's STRUCTURE, not on the character class text. The
	// class is the thing most likely to change legitimately, and a synchronized
	// widening in both lanes preserves parity — landmarking on its literal
	// would red the gate on a correct edit, and the predictable response to a
	// gate that fails on correct maintenance is to relax the assertion, which
	// retires the tracking guarantee altogether. A negated bracket expression
	// against `$version_base` is what must be present; which bytes it names is
	// the CANDIDATE TABLE's job to compare, not the extractor's to pin.
	if (!/case "\$version_base" in/.test(snippet)) {
		return null;
	}
	if (!/\*\[!.+\]\*/.test(snippet)) {
		return null;
	}
	return snippet.replaceAll("''${", "${");
}

/**
 * Decide the gate and render its report. Two verdicts agree when both reject,
 * or both accept the SAME stamp — a differing stamp is a disagreement even
 * though both lanes built, because it means one source file yielded two
 * different `-X main.version` values.
 *
 * A row whose either side failed to produce a verdict must be passed as a
 * reject by the caller only when the guard genuinely rejected it; a harness
 * error is reported through `harnessError` instead, which fails the gate
 * outright rather than being scored as agreement.
 */
export function compareVerdicts(
	rows: readonly ParityRow[],
	harnessError?: string,
): ParityReport {
	const scored = rows.map((row) => {
		const agree =
			row.flake.kind === "reject" && row.devenv.kind === "reject"
				? true
				: row.flake.kind === "accept" &&
					row.devenv.kind === "accept" &&
					row.flake.stamp === row.devenv.stamp;
		const shape = (v: Verdict): string =>
			v.kind === "accept" ? `accept(${JSON.stringify(v.stamp)})` : "reject";
		return {
			agree,
			line:
				`  ${agree ? "ok  " : "SKEW"}  ${row.label.padEnd(38)} ` +
				`flake=${shape(row.flake).padEnd(28)} devenv=${shape(row.devenv)}`,
		};
	});
	const skewed = scored.filter((row) => !row.agree).length;
	const lines = scored.map((row) => row.line).join("\n");
	if (harnessError !== undefined) {
		return {
			ok: false,
			report: `${lines}\n\ngate could not run: ${harnessError}`,
		};
	}
	if (rows.length === 0) {
		return { ok: false, report: "no candidates were evaluated" };
	}
	const verdict =
		skewed === 0
			? `all ${rows.length} candidates agree`
			: `${skewed} of ${rows.length} candidates SKEW between the two lanes`;
	return { ok: skewed === 0, report: `${lines}\n\n${verdict}` };
}
