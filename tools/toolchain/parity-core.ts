// Pure parsing and comparison for the toolchain parity gate. No I/O, no process
// exec — everything here is a total function over strings, so the interesting
// half of the gate is unit-testable (parity-core.test.ts) and the executable
// shell (parity.ts) stays thin.
//
// THE ASYMMETRY THIS FILE EXISTS TO HANDLE. The dev toolchain is pinned in two
// places with two different shapes:
//
//   .prototools  — bun/node/moon/go, each a literal version string. The pin IS
//                  the text, so the check is: does the binary CI actually put on
//                  PATH report that exact version?
//   devenv.nix   — buf, protoc, the Go battery, the linters: bare nixpkgs
//                  attribute names with NO version literal anywhere in the repo.
//                  Their versions are whatever the nixpkgs revision pinned in
//                  devenv.lock resolves to. Two of them (go-licenses, nilaway)
//                  do not implement a version flag at all, so "run it and parse
//                  the version" cannot be the check for that half.
//
// So the two halves get two different, and in each case exact, verdicts:
//
//   self-report — the .prototools half. Runtime's own reported version must
//                 equal the literal in .prototools.
//   store-path  — the devenv.nix half. `realpath` of the binary on PATH must be
//                 inside the store path that the devenv.lock-pinned nixpkgs
//                 resolves that attribute to. This is STRICTLY STRONGER than a
//                 version-string match (it identifies the exact derivation, not
//                 a coincidence of version numbers), it is uniform across the
//                 whole half, and it is the only method that works for the two
//                 tools that cannot report a version.
//
// A tool that fits neither method is NOT skipped — `verdict()` returns
// `unverifiable`, which the caller treats as a failure. Silently omitting a tool
// it could not check is the precise failure mode this gate exists to prevent: a
// green that proves nothing.

/** A version pin read from `.prototools`: a tool name and its literal version. */
export interface ProtoPin {
	readonly tool: string;
	readonly version: string;
}

/** How a tool's identity was established. */
export type CheckMethod = "self-report" | "store-path";

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
 * Parse `.prototools` into its version pins.
 *
 * The format is a TOML subset: `name = "version"` lines, `#` comments, blanks.
 * Parsing it directly (rather than pulling a TOML dependency) keeps the gate
 * dependency-free — it must run before `bun install` has necessarily happened.
 * Only bare top-level keys are pins; a `[section]` header ends the top-level
 * table, and anything inside one is configuration, not a toolchain pin.
 */
export function parseProtoTools(source: string): ProtoPin[] {
	const pins: ProtoPin[] = [];
	for (const raw of source.split("\n")) {
		const line = raw.trim();
		if (line === "" || line.startsWith("#")) continue;
		// A table header ends the top-level pin table; nothing after it is a pin.
		if (line.startsWith("[")) break;
		const match = /^([A-Za-z0-9_-]+)\s*=\s*"([^"]+)"/.exec(line);
		if (match?.[1] !== undefined && match[2] !== undefined) {
			pins.push({ tool: match[1], version: match[2] });
		}
	}
	return pins;
}

/**
 * Extract the nixpkgs attribute names from devenv.nix's `packages = with pkgs; [
 * … ];` list.
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
 * A dotted attribute path (`nodePackages.prettier`) THROWS rather than being
 * skipped. Skipping is the tempting behaviour and the wrong one: the same parse
 * feeds both `--print-nix-attrs` (what CI installs) and the expected-identity
 * set (what the gate checks), so a dropped attribute is simultaneously absent
 * from PATH and unreported by the gate — it resurfaces as `command not found`
 * inside some later task, or not at all. Refusing keeps the promise above
 * honest: the gate covers the list, or it says why it cannot.
 */
export function parseDevenvPackages(source: string): string[] {
	const open = source.indexOf("packages = with pkgs; [");
	if (open === -1) return [];
	const start = source.indexOf("[", open) + 1;
	const end = source.indexOf("];", start);
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
		if (/^[A-Za-z][A-Za-z0-9_-]*(\.[A-Za-z][A-Za-z0-9_-]*)+$/.test(token)) {
			throw new Error(
				`devenv.nix packages entry "${token}" is a dotted attribute path, which this gate cannot resolve. ` +
					`Skipping it would leave the tool uninstalled in CI AND unreported here. ` +
					`Teach nixIdentities to resolve dotted paths (lib.getAttrFromPath) before adding it.`,
			);
		}
	}
	return attrs;
}

/**
 * Pull a version out of a tool's `--version` output.
 *
 * Every runtime in the .prototools half decorates its version differently
 * (`1.3.13`, `v24.18.0`, `moon 2.4.2`, `go version go1.26.5 linux/amd64`), so
 * match the first dotted numeric run and drop a `v`/`go` prefix rather than
 * carrying a per-tool parser. Returns null when the output contains no version,
 * which the caller must surface as `unverifiable` — never as a pass.
 */
export function extractVersion(output: string): string | null {
	const match = /(?:^|[^0-9A-Za-z.])(?:v|go)?(\d+\.\d+(?:\.\d+)?)/.exec(output);
	return match?.[1] ?? null;
}

/**
 * Verify a `.prototools`-pinned runtime: what it reports must equal the literal.
 *
 * `expected` comes from the file, `probeOutput` from running the binary that is
 * actually on PATH — so this catches a `setup-*` action resolving something
 * other than the pin, a stale tool cache, and a shim shadowed by a host install,
 * none of which reading the file alone would ever notice.
 */
export function verifySelfReport(
	tool: string,
	expected: string,
	probeOutput: string | null,
): Verdict {
	if (probeOutput === null) {
		return { kind: "unverifiable", tool, reason: "not on PATH" };
	}
	const actual = extractVersion(probeOutput);
	if (actual === null) {
		return {
			kind: "unverifiable",
			tool,
			reason: `no version in output: ${probeOutput.trim().split("\n")[0] ?? ""}`,
		};
	}
	return actual === expected
		? { kind: "match", tool, method: "self-report", actual }
		: { kind: "mismatch", tool, method: "self-report", expected, actual };
}

/**
 * Verify a nixpkgs-provided tool: the resolved binary must live inside the store
 * path the devenv.lock-pinned nixpkgs builds that attribute to.
 *
 * `expectedStorePath` is that derivation's outPath; `resolvedBinary` is
 * `realpath` of what PATH resolves. Containment (not equality) because the
 * binary is `<outPath>/bin/<name>`. This is the whole devenv.nix half's method,
 * including the two tools that implement no version flag — their derivation
 * identity is checkable even though their self-report is not.
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
