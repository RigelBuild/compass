// Tests for the pure half of the toolchain parity gate.
//
// The property under test throughout is the one the gate exists for: it must be
// CAPABLE OF FAILING, and it must never turn "I could not check this" into a
// pass. So every parser is tested against the shapes the real files have, every
// comparator against a genuine skew, and renderReport specifically against the
// case where the only problem is an unverifiable tool.

import { describe, expect, test } from "bun:test";
import {
	extractVersion,
	parseDevenvPackages,
	parseProtoTools,
	renderReport,
	type Verdict,
	verifySelfReport,
	verifyStorePath,
} from "./parity-core.ts";

describe("the Postgres image pin", () => {
	// Two files name the image the real-Postgres suites run against: the CI
	// service (.github/workflows/ci.yml) and the local harness (pgtest.go). Their
	// comments each assert the two are the same image, and nothing enforced it —
	// so a digest bump to one would leave CI and a local run silently exercising
	// different databases, which is precisely the divergence the pin exists to
	// prevent. An invariant asserted only in prose is not an invariant.
	const digestOf = (source: string, label: string): string => {
		const matches =
			source.match(/postgres:[0-9]+-alpine@(sha256:[0-9a-f]{64})/g) ?? [];
		expect(
			matches,
			`${label} must pin the Postgres image by digest, not by a mutable tag`,
		).toHaveLength(1);
		const [digest] = matches;
		// Not a `!`: under noUncheckedIndexedAccess the length assertion above does
		// not narrow the index, and asserting here keeps a zero-match file failing
		// with the message above rather than comparing two `undefined`s as equal.
		if (digest === undefined)
			throw new Error(`${label}: no Postgres image digest found`);
		return digest;
	};

	test("is identical in ci.yml and pgtest.go, and is a digest in both", async () => {
		const root = new URL("../../", import.meta.url).pathname;
		const [ci, harness] = await Promise.all([
			Bun.file(`${root}.github/workflows/ci.yml`).text(),
			Bun.file(`${root}go/internal/pgtest/pgtest.go`).text(),
		]);
		expect(digestOf(harness, "pgtest.go")).toBe(
			digestOf(ci, ".github/workflows/ci.yml"),
		);
	});
});

describe("parseProtoTools", () => {
	test("reads every pin from the real .prototools shape", () => {
		const pins = parseProtoTools(
			[
				"# Language/runtime toolchains, pinned per the proto/devenv split.",
				"#",
				'bun = "1.3.13"',
				'node = "24.18.0"',
				"# moon is pinned HERE rather than taken from nixpkgs.",
				'moon = "2.4.2"',
				"",
				'go = "1.26.5"',
			].join("\n"),
		);
		expect(pins).toEqual([
			{ tool: "bun", version: "1.3.13" },
			{ tool: "node", version: "24.18.0" },
			{ tool: "moon", version: "2.4.2" },
			{ tool: "go", version: "1.26.5" },
		]);
	});

	test("stops at a table header, so section settings are never read as pins", () => {
		const pins = parseProtoTools(
			['bun = "1.3.13"', "", "[settings]", 'auto-install = "true"'].join("\n"),
		);
		expect(pins).toEqual([{ tool: "bun", version: "1.3.13" }]);
	});

	test("yields nothing for a file with no pins, so the caller can refuse a vacuous pass", () => {
		expect(parseProtoTools("# only a comment\n\n")).toEqual([]);
	});
});

describe("parseDevenvPackages", () => {
	const devenv = [
		"{",
		"  packages = with pkgs; [",
		"    # Language/runtime manager. Pins bun/node/moon/go via .prototools.",
		"    proto",
		"",
		"    # buf drives the pipeline; protobuf supplies protoc.",
		"    buf",
		"    protobuf # protoc",
		"    protoc-gen-go",
		"  ];",
		"",
		"  enterShell = ''",
		"    bun install --frozen-lockfile",
		"  '';",
		"}",
	].join("\n");

	test("reads the attribute list", () => {
		expect(parseDevenvPackages(devenv)).toEqual([
			"proto",
			"buf",
			"protobuf",
			"protoc-gen-go",
		]);
	});

	test("ignores attribute names that only appear in comments", () => {
		// The real block's prose names golangci-lint, nilaway and friends
		// constantly; treating a mention as an entry would make the gate demand
		// tools the shell does not provide.
		expect(parseDevenvPackages(devenv)).not.toContain("protoc");
		expect(parseDevenvPackages(devenv)).not.toContain("bun");
	});

	test("stops at the closing bracket, so nothing after the list leaks in", () => {
		expect(parseDevenvPackages(devenv)).not.toContain("enterShell");
	});

	test("yields nothing when the block is absent, so the caller can refuse a vacuous pass", () => {
		expect(parseDevenvPackages("{ packages = [ ]; }")).toEqual([]);
	});

	// A skipped entry is absent from BOTH what CI installs and what the gate
	// expects, so the tool goes uncovered in silence — the false green this gate
	// exists to prevent. Refusing is the only safe behaviour.
	//
	// Every form here is a real thing a devenv.nix can legally contain. The
	// dotted path is only the one we hit first; each of the others was silently
	// dropped until the refusal moved to the default branch, so they are listed
	// individually rather than folded into one representative case.
	test.each([
		["a dotted attribute path", "nodePackages.prettier"],
		["a parenthesised call", "(python3.withPackages (ps: [ps.requests]))"],
		// Nix interpolation syntax in a plain string is the fixture — deliberately
		// NOT a JS template literal, which is what makes it unparseable to the gate.
		// biome-ignore lint/suspicious/noTemplateCurlyInString: see above
		["an interpolation", "${myTool}"],
		["a quoted string", '"weird"'],
	])("throws on %s rather than silently dropping it", (_label, entry) => {
		const source = [
			"{",
			"  packages = with pkgs; [",
			"    buf",
			`    ${entry}`,
			"  ];",
			"}",
		].join("\n");
		expect(() => parseDevenvPackages(source)).toThrow(
			/not a bare nixpkgs attribute name/,
		);
	});

	test("still yields the bare entries it can resolve", () => {
		// Guards the refusal against over-reach: a block of ordinary entries must
		// keep parsing, or the gate refuses everything and covers nothing.
		const source = [
			"{",
			"  packages = with pkgs; [",
			"    buf",
			"    protobuf",
			"  ];",
			"}",
		].join("\n");
		expect(parseDevenvPackages(source)).toEqual(["buf", "protobuf"]);
	});
});

describe("extractVersion", () => {
	test.each([
		["1.3.13", "1.3.13"],
		["v24.18.0", "24.18.0"],
		["moon 2.4.2", "2.4.2"],
		["go version go1.26.5 linux/amd64", "1.26.5"],
		["markdownlint-cli2 v0.22.1 (markdownlint v0.40.0)", "0.22.1"],
	])("parses %p", (output, expected) => {
		expect(extractVersion(output)).toBe(expected);
	});

	test("returns null when there is no version to find", () => {
		expect(
			extractVersion("flag provided but not defined: -version"),
		).toBeNull();
	});
});

describe("verifySelfReport", () => {
	test("matches when the runtime reports the pinned version", () => {
		expect(verifySelfReport("bun", "1.3.13", "1.3.13\n")).toEqual({
			kind: "match",
			tool: "bun",
			method: "self-report",
			actual: "1.3.13",
		});
	});

	test("fails on a skew — the gate's whole reason to exist", () => {
		expect(verifySelfReport("bun", "1.3.13", "1.3.14\n")).toEqual({
			kind: "mismatch",
			tool: "bun",
			method: "self-report",
			expected: "1.3.13",
			actual: "1.3.14",
		});
	});

	test("an absent tool is unverifiable, not a pass", () => {
		expect(verifySelfReport("moon", "2.4.2", null).kind).toBe("unverifiable");
	});

	test("unparseable output is unverifiable, not a pass", () => {
		expect(verifySelfReport("go", "1.26.5", "command failed").kind).toBe(
			"unverifiable",
		);
	});
});

describe("verifyStorePath", () => {
	const store = "/nix/store/k31ahmn6j47ay77xacl08a1cb96lnr3c-buf-1.72.0";

	test("matches a binary inside the pinned derivation", () => {
		expect(verifyStorePath("buf", store, `${store}/bin/buf`)).toEqual({
			kind: "match",
			tool: "buf",
			method: "store-path",
			actual: store,
		});
	});

	test("fails when PATH resolves a different derivation of the same version", () => {
		// Same version string, different build: a version-string comparison would
		// call this a pass. The store path is what makes the check exact.
		const other = "/nix/store/aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa-buf-1.72.0";
		expect(verifyStorePath("buf", store, `${other}/bin/buf`).kind).toBe(
			"mismatch",
		);
	});

	test("fails when PATH resolves an ambient system binary", () => {
		expect(verifyStorePath("buf", store, "/usr/bin/buf").kind).toBe("mismatch");
	});

	test("does not match a sibling store path sharing the expected path as a prefix", () => {
		expect(verifyStorePath("buf", store, `${store}-extra/bin/buf`).kind).toBe(
			"mismatch",
		);
	});

	test("an absent tool is unverifiable, not a pass", () => {
		expect(verifyStorePath("nilaway", store, null).kind).toBe("unverifiable");
	});

	test("checks a tool that reports no version at all", () => {
		// go-licenses and nilaway implement no version flag, so self-report cannot
		// cover them. Store-path identity can, which is why it is the method for
		// the whole nixpkgs half.
		const nilaway =
			"/nix/store/wxbsyzz3jv13c9sf0brcy293k0w28h2h-nilaway-0-unstable-2025-03-07";
		expect(
			verifyStorePath("nilaway", nilaway, `${nilaway}/bin/nilaway`).kind,
		).toBe("match");
	});
});

describe("renderReport", () => {
	const ok: Verdict = {
		kind: "match",
		tool: "bun",
		method: "self-report",
		actual: "1.3.13",
	};

	test("passes only when every verdict matched", () => {
		expect(renderReport([ok]).ok).toBe(true);
	});

	test("fails on a mismatch", () => {
		const report = renderReport([
			ok,
			{
				kind: "mismatch",
				tool: "moon",
				method: "self-report",
				expected: "2.4.2",
				actual: "2.4.5",
			},
		]);
		expect(report.ok).toBe(false);
		expect(report.table).toContain("expected 2.4.2, got 2.4.5");
	});

	test("fails on an unverifiable tool — an unchecked tool is not a pass", () => {
		const report = renderReport([
			ok,
			{ kind: "unverifiable", tool: "nilaway", reason: "not on PATH" },
		]);
		expect(report.ok).toBe(false);
		expect(report.table).toContain("UNVERIFIABLE");
	});

	test("lists every tool and its method, so the log states the gate's own coverage", () => {
		const table = renderReport([
			ok,
			{
				kind: "match",
				tool: "nilaway",
				method: "store-path",
				actual: "/nix/store/x-nilaway",
			},
		]).table;
		expect(table).toContain("bun");
		expect(table).toContain("self-report");
		expect(table).toContain("nilaway");
		expect(table).toContain("store-path");
	});

	test("an empty verdict list is not a pass to celebrate — it reports zero coverage", () => {
		expect(renderReport([]).table).toContain("All 0 pinned tools");
	});
});
