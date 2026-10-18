import { describe, expect, test } from "bun:test";
import { publicVerdict } from "./assert-public-core.ts";

const REF = "ghcr.io/rigelbuild/compass-agent:git-abcdef012345";

describe("publicVerdict", () => {
	test("accepts a successful anonymous manifest probe", () => {
		expect(
			publicVerdict({ exitCode: 0, stdout: "{}", stderr: "" }, REF),
		).toEqual({
			ok: true,
		});
	});

	test.each([
		["403 Forbidden", "Set package visibility to public"],
		["manifest unknown", "Set package visibility to public"],
		["i/o timeout", "i/o timeout"],
	])("rejects a failed anonymous probe: %s", (stderr, detail) => {
		const verdict = publicVerdict({ exitCode: 1, stdout: "", stderr }, REF);
		expect(verdict.ok).toBe(false);
		if (!verdict.ok) {
			expect(verdict.message).toContain("compass-agent");
			expect(verdict.message).toContain(detail);
		}
	});
});
