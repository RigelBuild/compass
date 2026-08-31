// Unit tests for the macos-bundle pure core (index.ts).
//
// These defend the bundler's pure contract: the Info.plist template carries
// every required CFBundle key with the passed values (name/executable/
// identifier), both version keys carry the clean semver (GC4), the package type
// is APPL, and the arg parser accepts the release-grammar flags + fails loud on
// missing/duplicate/unknown input (build.sh sanity posture).
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
		});
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
