// Pure parsing and comparison for the toolchain parity gate. No I/O, no process
// exec — everything here is a total function over strings, so the interesting
// half of the gate is unit-testable (parity-core.test.ts) and the executable
// shell (parity.ts) stays thin.
//
// THE METHOD THIS FILE IMPLEMENTS. Every dev toolchain — the language runtimes
// (bun/node/moon/go) and everything else (buf, protoc, the Go battery, the
// linters) — resolves to a nix derivation, so one uniform verdict covers them
// all:
//
//   store-path — `realpath` of the binary on PATH must be inside the store path
//                that the devenv.lock-pinned nix derivation resolves to. This
//                identifies the exact derivation, not a coincidence of version
//                numbers, and it is the only method that works for the tools
//                (go-licenses, nilaway) that implement no version flag at all.
//                An ambient runtime shadowing the pinned one resolves outside
//                the expected store path and fails.
//
// The two halves differ only in where the expected store path comes from: the
// nixpkgs attrs are parsed out of devenv.nix's `packages = (with pkgs; [ … ])`
// literal and resolved through gate-tools.nix's `identity`; the language
// toolchains are the closed set gate-tools.nix's `langs` output builds. Both
// are checked by the identical containment test below.
//
// A tool that cannot be checked is NOT skipped — the verdict is `unverifiable`,
// which the caller treats as a failure. Silently omitting a tool it could not
// check is the precise failure mode this gate exists to prevent: a green that
// proves nothing.

/** How a tool's identity was established. */
export type CheckMethod = "store-path";

/** One tool's parity result. */
export type Verdict =
	| {
			readonly kind: "match";
			readonly tool: string;
			readonly method: CheckMethod;
			readonly actual: string;
	  }
	| {
			readonly kind: "mismatch";
			readonly tool: string;
			readonly method: CheckMethod;
			readonly expected: string;
			readonly actual: string;
	  }
	| {
			readonly kind: "unverifiable";
			readonly tool: string;
			readonly reason: string;
	  };

/**
 * Extract the nixpkgs attribute names from devenv.nix's
 * `packages = (with pkgs; [ … ]) ++ [ … ]` list.
 *
 * Only the parenthesised `with pkgs; [ … ]` half is parsed — the bare nixpkgs
 * attributes. The appended `++ [ … ]` list holds the language toolchains as
 * dotted references (`toolchainTools.bun`, `goToolchain`), which are NOT bare
 * attribute names; they are covered by the store-path `langs` verdict instead
 * and must never reach this parser (a dotted token THROWS, see below).
 *
 * Reading the list rather than hand-copying it is what makes the gate cover the
 * dev shell as it actually is: adding a tool to devenv.nix extends the gate with
 * no edit here, to parity.ts, or to the workflow. A hand-maintained list would
 * drift the moment someone adds a tool — and a parity gate that silently stops
 * covering a tool is the same false green as having no gate.
 *
 * Comments are stripped first, so an attribute name merely MENTIONED in the
 * block's prose (they are mentioned constantly) is never mistaken for an entry.
 *
 * Anything that is not a bare attribute name THROWS rather than being skipped.
 * Skipping is the tempting behaviour and the wrong one: the same parse feeds
 * both `--print-nix-attrs` (what CI installs) and the expected-identity set
 * (what the gate checks), so a dropped entry is simultaneously absent from PATH
 * and unreported by the gate — it resurfaces as `command not found` inside some
 * later task, or not at all. Refusing keeps the promise above honest: the gate
 * covers the list, or it says why it cannot.
 *
 * The refusal is deliberately on the DEFAULT branch rather than on an
 * enumeration of known-bad shapes. A dotted path (`nodePackages.prettier`) is
 * only the form we happened to hit first; a parenthesised call
 * (`(python3.withPackages …)`), an interpolation (`${myTool}`), and a quoted
 * string are equally unresolvable and equally silent. Enumerating the bad
 * shapes leaves every shape nobody thought of falling through to a silent drop,
 * which is the failure this function exists to prevent.
 */
export function parseDevenvPackages(source: string): string[] {
	const open = source.indexOf("packages = (with pkgs; [");
	if (open === -1) return [];
	const start = source.indexOf("[", open) + 1;
	const end = source.indexOf("])", start);
	if (end === -1) return [];
	const body = source
		.slice(start, end)
		.split("\n")
		.map((line) => {
			const comment = line.indexOf("#");
			return comment === -1 ? line : line.slice(0, comment);
		})
		.join("\n");
	const attrs: string[] = [];
	for (const token of body.split(/\s+/)) {
		if (token === "") continue;
		if (/^[A-Za-z][A-Za-z0-9_-]*$/.test(token)) {
			attrs.push(token);
			continue;
		}
		throw new Error(
			`devenv.nix packages entry ${JSON.stringify(token)} is not a bare nixpkgs ` +
				`attribute name, so this gate cannot resolve it. Skipping it would leave the ` +
				`tool uninstalled in CI AND unreported here. Teach nixIdentities to resolve ` +
				`this form (for a dotted path, lib.getAttrFromPath) before adding it.`,
		);
	}
	return attrs;
}

/**
 * Verify a nix-provided tool: the resolved binary must live inside the store
 * path the devenv.lock-pinned derivation builds it to.
 *
 * `expectedStorePath` is that derivation's outPath; `resolvedBinary` is
 * `realpath` of what PATH resolves. Containment (not equality) because the
 * binary is `<outPath>/bin/<name>`. This is the gate's whole method — it works
 * uniformly for the language toolchains, the codegen tools, and the two tools
 * (go-licenses, nilaway) that implement no version flag, since it checks
 * derivation identity rather than a self-reported version string.
 */
export function verifyStorePath(
	tool: string,
	expectedStorePath: string,
	resolvedBinary: string | null,
): Verdict {
	if (resolvedBinary === null) {
		return { kind: "unverifiable", tool, reason: "not on PATH" };
	}
	return resolvedBinary.startsWith(`${expectedStorePath}/`)
		? { kind: "match", tool, method: "store-path", actual: expectedStorePath }
		: {
				kind: "mismatch",
				tool,
				method: "store-path",
				expected: expectedStorePath,
				actual: resolvedBinary,
			};
}

/** A rendered coverage table plus the pass/fail decision it justifies. */
export interface Report {
	readonly table: string;
	readonly ok: boolean;
}

/**
 * Render every verdict and decide the build.
 *
 * The table always lists EVERY tool with the method used, so the run's own log
 * states its coverage rather than leaving it to be inferred. `ok` is false for
 * both a mismatch and an unverifiable: a tool the gate could not check is a
 * failure, not an omission.
 */
export function renderReport(verdicts: readonly Verdict[]): Report {
	// Size the name column to the widest label present, so the table stays a
	// table when a long attr:bin label (protobuf:protoc-gen-upb_minitable-35.1.0)
	// shows up rather than ragging every row after it.
	const width = verdicts.reduce(
		(widest, v) => Math.max(widest, v.tool.length),
		0,
	);
	const rows = verdicts.map((v) => {
		const name = v.tool.padEnd(width);
		if (v.kind === "match") {
			return `  ok           ${name}  ${v.method.padEnd(11)}  ${v.actual}`;
		}
		if (v.kind === "mismatch") {
			return `  MISMATCH     ${name}  ${v.method.padEnd(11)}  expected ${v.expected}, got ${v.actual}`;
		}
		return `  UNVERIFIABLE ${name}  ${"-".padEnd(11)}  ${v.reason}`;
	});
	const bad = verdicts.filter((v) => v.kind !== "match");
	const summary =
		bad.length === 0
			? `\nAll ${verdicts.length} pinned tools match the dev shell.`
			: `\n${bad.length} of ${verdicts.length} pinned tools failed parity with the dev shell.`;
	return { table: [...rows, summary].join("\n"), ok: bad.length === 0 };
}
