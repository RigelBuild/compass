// Unit tests for the macos-bundle pure core (index.ts).
//
// These defend the bundler's pure contract: the Info.plist template carries
// every required CFBundle key with the passed values (name/executable/
// identifier), both version keys carry the clean semver (GC4), the package type
// is APPL, and the arg parser accepts the release-grammar flags (including the
// repeatable --sidecar) + fails loud on missing/duplicate/unknown input
// (build.sh sanity posture).
//
// Only the PURE core is exercised — the edge (codesign / hdiutil / staging) is
// import.meta.main-guarded, so importing index.ts never runs it.

import { describe, expect, test } from "bun:test";
import { parseArgs, renderInfoPlist } from "./index.ts";

/** The canonical render inputs used across the plist cases. */
function plistOpts() {
	return {
		name: "Compass",
		executable: "compass-app",
		identifier: "build.rigel.compass",
		version: "1.4.2",
	};
}

/** Extract the <string> value following a given <key> from the plist XML. */
function plistValue(xml: string, key: string): string | undefined {
	const re = new RegExp(`<key>${key}</key>\\s*<string>([^<]*)</string>`);
	return re.exec(xml)?.[1];
}

describe("renderInfoPlist — required CFBundle keys carry the passed values", () => {
	test("CFBundleName is the display name", () => {
		expect(plistValue(renderInfoPlist(plistOpts()), "CFBundleName")).toBe(
			"Compass",
		);
	});

	test("CFBundleExecutable is the binary name", () => {
		expect(plistValue(renderInfoPlist(plistOpts()), "CFBundleExecutable")).toBe(
			"compass-app",
		);
	});

	test("CFBundleIdentifier is the reverse-DNS id", () => {
		expect(plistValue(renderInfoPlist(plistOpts()), "CFBundleIdentifier")).toBe(
			"build.rigel.compass",
		);
	});

	test("CFBundlePackageType is APPL", () => {
		expect(
			plistValue(renderInfoPlist(plistOpts()), "CFBundlePackageType"),
		).toBe("APPL");
	});

	test("CFBundleInfoDictionaryVersion is 6.0", () => {
		expect(
			plistValue(renderInfoPlist(plistOpts()), "CFBundleInfoDictionaryVersion"),
		).toBe("6.0");
	});

	test("LSMinimumSystemVersion is present and non-empty", () => {
		const v = plistValue(
			renderInfoPlist(plistOpts()),
			"LSMinimumSystemVersion",
		);
		expect(v).toBeDefined();
		expect(v).not.toBe("");
	});
});

describe("renderInfoPlist — both version keys carry the clean semver (GC4)", () => {
	test("CFBundleShortVersionString is the passed semver", () => {
		expect(
			plistValue(
				renderInfoPlist({ ...plistOpts(), version: "2.0.0" }),
				"CFBundleShortVersionString",
			),
		).toBe("2.0.0");
	});

	test("CFBundleVersion is the passed semver", () => {
		expect(
			plistValue(
				renderInfoPlist({ ...plistOpts(), version: "2.0.0" }),
				"CFBundleVersion",
			),
		).toBe("2.0.0");
	});

	test("both version keys agree — ONE version stamp", () => {
		const xml = renderInfoPlist({ ...plistOpts(), version: "3.1.4" });
		expect(plistValue(xml, "CFBundleShortVersionString")).toBe(
			plistValue(xml, "CFBundleVersion"),
		);
	});
});

describe("renderInfoPlist — well-formed plist envelope", () => {
	test("carries the plist DOCTYPE + a single dict", () => {
		const xml = renderInfoPlist(plistOpts());
		expect(xml).toContain("<!DOCTYPE plist PUBLIC");
		expect(xml).toContain('<plist version="1.0">');
		expect(xml).toContain("<dict>");
		expect(xml).toContain("</dict>");
	});

	test("XML-escapes a value that would otherwise break the string element", () => {
		const xml = renderInfoPlist({ ...plistOpts(), name: "A & B <C>" });
		expect(xml).toContain("<string>A &amp; B &lt;C&gt;</string>");
	});
});

describe("parseArgs — accepts the release-grammar flags", () => {
	test("parses all four flags into a typed BundleArgs", () => {
		const args = parseArgs([
			"--binary",
			"/tmp/compass-app",
			"--dist",
			"apps/ui/dist",
			"--version",
			"1.4.2",
			"--out",
			"/tmp/out.dmg",
		]);
		expect(args).toEqual({
			binary: "/tmp/compass-app",
			dist: "apps/ui/dist",
			version: "1.4.2",
			out: "/tmp/out.dmg",
			sidecars: [],
		});
	});
});

describe("parseArgs — repeatable --sidecar collects the embedded sidecars", () => {
	/** The required grammar every sidecar case is layered onto. */
	function requiredArgs(): string[] {
		return [
			"--binary",
			"/tmp/compass-app",
			"--dist",
			"apps/ui/dist",
			"--version",
			"1.4.2",
			"--out",
			"/tmp/out.dmg",
		];
	}

	test("collects repeated --sidecar values in the order given", () => {
		const args = parseArgs([
			...requiredArgs(),
			"--sidecar",
			"/tmp/compass-stack",
			"--sidecar",
			"/tmp/compass-server",
			"--sidecar",
			"/tmp/compass-runner",
		]);
		expect(args.sidecars).toEqual([
			"/tmp/compass-stack",
			"/tmp/compass-server",
			"/tmp/compass-runner",
		]);
	});

	test("the duplicate check still fires for single-valued flags", () => {
		expect(() => parseArgs([...requiredArgs(), "--dist", "other"])).toThrow(
			/more than once/,
		);
	});

	test("no --sidecar yields an empty array, not a parse error", () => {
		expect(parseArgs(requiredArgs()).sidecars).toEqual([]);
	});

	test("a single --sidecar still collects into an array", () => {
		const args = parseArgs([...requiredArgs(), "--sidecar", "/tmp/x"]);
		expect(args.sidecars).toEqual(["/tmp/x"]);
	});

	test("--sidecar with no value fails loud like any other flag", () => {
		expect(() => parseArgs([...requiredArgs(), "--sidecar"])).toThrow(
			/'--sidecar' expects a value/,
		);
	});

	test("--sidecar swallowing the next flag as its value fails loud", () => {
		expect(() =>
			parseArgs(["--sidecar", "--binary", "/tmp/compass-app"]),
		).toThrow(/'--sidecar' expects a value/);
	});

	test("rejects a sidecar whose basename collides with the shell", () => {
		expect(() =>
			parseArgs([...requiredArgs(), "--sidecar", "/other/compass-app"]),
		).toThrow(/collides with the shell/);
	});

	test("a sidecar basenamed compass-app throws even when --binary is basenamed otherwise", () => {
		expect(() =>
			parseArgs([
				"--binary",
				"/tmp/compass-app-darwin-arm64",
				"--dist",
				"/d",
				"--version",
				"1.0.0",
				"--out",
				"/o.dmg",
				"--sidecar",
				"/tmp/compass-app",
			]),
		).toThrow(/collides with the shell executable/);
	});

	test("a --binary basenamed otherwise with a same-named sidecar does NOT throw", () => {
		expect(
			parseArgs([
				"--binary",
				"/b/shell-x",
				"--dist",
				"/d",
				"--version",
				"1.0.0",
				"--out",
				"/o.dmg",
				"--sidecar",
				"/s/shell-x",
			]).sidecars,
		).toEqual(["/s/shell-x"]);
	});

	test("rejects two sidecars sharing a basename", () => {
		expect(() =>
			parseArgs([
				...requiredArgs(),
				"--sidecar",
				"/a/compass-stack",
				"--sidecar",
				"/b/compass-stack",
			]),
		).toThrow(/duplicate sidecar basename/);
	});
});

describe("parseArgs — fails loud on malformed input", () => {
	test("throws naming a missing required flag", () => {
		expect(() =>
			parseArgs(["--binary", "/tmp/b", "--dist", "d", "--version", "1.0.0"]),
		).toThrow(/--out/);
	});

	test("throws on an unknown flag", () => {
		expect(() => parseArgs(["--nope", "x"])).toThrow(/unknown/);
	});

	test("throws on a flag missing its value", () => {
		expect(() => parseArgs(["--binary"])).toThrow(/expects a value/);
	});

	test("throws on a flag given twice", () => {
		expect(() => parseArgs(["--binary", "a", "--binary", "b"])).toThrow(
			/more than once/,
		);
	});

	test("throws when a flag's value is another flag (missing value)", () => {
		expect(() => parseArgs(["--binary", "--dist", "d"])).toThrow(
			/expects a value/,
		);
	});
});
