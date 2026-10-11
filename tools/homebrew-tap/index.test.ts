import { describe, expect, test } from "bun:test";
import { parseSha256Sums, renderFormula } from "./index.ts";

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
	test("pins each platform's url to the digest of the same asset", () => {
		const rb = renderFormula("v1.2.3", parseSha256Sums(SUMS));
		expect(rb).toContain('version "1.2.3"');
		const mac = rb.indexOf("compass_v1.2.3_darwin-arm64");
		const linux = rb.indexOf("compass_v1.2.3_linux-amd64");
		expect(rb.indexOf(MAC)).toBeGreaterThan(mac);
		expect(rb.indexOf(MAC)).toBeLessThan(linux);
		expect(rb.indexOf(LINUX)).toBeGreaterThan(linux);
		expect(rb).toContain(
			"https://github.com/RigelBuild/compass/releases/download/v1.2.3/compass_v1.2.3_linux-amd64",
		);
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
