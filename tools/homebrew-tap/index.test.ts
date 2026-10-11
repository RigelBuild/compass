import { describe, expect, test } from "bun:test";
import { isNewerThanFormula, parseSha256Sums, renderFormula } from "./index.ts";

const MAC = "a".repeat(64);
const LINUX = "b".repeat(64);
const SUMS = `${MAC}  compass_v1.2.3_darwin-arm64
${LINUX}  compass_v1.2.3_linux-amd64
${"c".repeat(64)}  compass-server_v1.2.3_linux-amd64
`;

describe("parseSha256Sums", () => {
	test("accepts text and binary mode markers", () => {
		const sums = parseSha256Sums(`${MAC} *bin-file\n${LINUX}  text-file\n`);
		expect(sums.get("bin-file")).toBe(MAC);
		expect(sums.get("text-file")).toBe(LINUX);
	});

	test("rejects a malformed line instead of skipping it", () => {
		expect(() => parseSha256Sums("not-a-digest  file\n")).toThrow(/malformed/);
	});
});

describe("renderFormula", () => {
	// Each OS/arch guard block, keyed by its guard pair, with the url and sha inside it.
	const platformBlocks = (rb: string) =>
		[
			...rb.matchAll(
				/ {2}(on_\w+) do\n {4}(on_\w+) do\n {6}url "([^"]+)"\n {6}sha256 "(\w+)"/g,
			),
		].map((m) => ({ guard: `${m[1]}/${m[2]}`, url: m[3], sha: m[4] }));

	test("pairs each OS/arch guard with its own asset url and digest", () => {
		const rb = renderFormula("v1.2.3", parseSha256Sums(SUMS));
		expect(rb).toContain('version "1.2.3"');
		const base =
			"https://github.com/RigelBuild/compass/releases/download/v1.2.3";
		expect(platformBlocks(rb)).toEqual([
			{
				guard: "on_macos/on_arm",
				url: `${base}/compass_v1.2.3_darwin-arm64`,
				sha: MAC,
			},
			{
				guard: "on_linux/on_intel",
				url: `${base}/compass_v1.2.3_linux-amd64`,
				sha: LINUX,
			},
		]);
	});

	test("refuses a prerelease tag so build-* never bumps the tap", () => {
		expect(() =>
			renderFormula("build-0123456789ab", parseSha256Sums(SUMS)),
		).toThrow(/semver/);
		expect(() => renderFormula("v1.2.3-rc.1", parseSha256Sums(SUMS))).toThrow(
			/semver/,
		);
	});

	test("fails when a CLI asset is missing from SHA256SUMS", () => {
		const sums = parseSha256Sums(`${MAC}  compass_v1.2.3_darwin-arm64\n`);
		expect(() => renderFormula("v1.2.3", sums)).toThrow(
			/compass_v1.2.3_linux-amd64/,
		);
	});
});

describe("isNewerThanFormula", () => {
	const tap = renderFormula("v1.2.3", parseSha256Sums(SUMS));

	test("compares numerically, not lexically", () => {
		expect(isNewerThanFormula("v1.10.0", tap)).toBe(true);
		expect(isNewerThanFormula("v1.2.10", tap)).toBe(true);
		expect(isNewerThanFormula("v2.0.0", tap)).toBe(true);
	});

	test("an older or equal release never replaces the tap formula", () => {
		expect(isNewerThanFormula("v1.2.3", tap)).toBe(false);
		expect(isNewerThanFormula("v1.2.2", tap)).toBe(false);
		expect(isNewerThanFormula("v0.9.9", tap)).toBe(false);
	});

	test("fails on a tap formula with no version line", () => {
		expect(() =>
			isNewerThanFormula("v1.2.3", "class X < Formula\nend\n"),
		).toThrow(/no semver version/);
	});
});
