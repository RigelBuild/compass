import { describe, expect, test } from "bun:test";
import {
	type AllowlistEntry,
	type AuditFinding,
	evaluateAuditFindings,
} from "./index.ts";

const ALLOWED_ID = "GHSA-vfj7-8cjw-p6xm";
const allowlist: AllowlistEntry[] = [
	{
		id: ALLOWED_ID,
		package: "braces",
		reason: "No fixed release; reached through dependencies.",
	},
];

function finding(id: string, packageName: string): AuditFinding {
	return {
		id,
		package: packageName,
		title: "test advisory",
		severity: "high",
		vulnerableVersions: "<1.0.0",
	};
}

describe("evaluateAuditFindings", () => {
	test("fails for an advisory that is not allowlisted", () => {
		const failures = evaluateAuditFindings(
			[
				{
					directory: ".",
					findings: [finding("GHSA-unlisted-0000-0000", "left-pad")],
				},
			],
			[],
		);
		expect(failures).toHaveLength(1);
		expect(failures[0]).toContain("GHSA-unlisted-0000-0000");
	});

	test("passes when an allowlisted advisory is present", () => {
		expect(
			evaluateAuditFindings(
				[{ directory: ".", findings: [finding(ALLOWED_ID, "braces")] }],
				allowlist,
			),
		).toEqual([]);
	});

	test("fails when an allowlist entry is stale", () => {
		const failures = evaluateAuditFindings(
			[{ directory: ".", findings: [] }],
			allowlist,
		);
		expect(failures).toEqual([
			`stale allowlist entry ${ALLOWED_ID} for braces: no lockfile reports this advisory`,
		]);
	});

	test("combines findings from two lockfiles before checking stale entries", () => {
		const failures = evaluateAuditFindings(
			[
				{ directory: ".", findings: [finding(ALLOWED_ID, "braces")] },
				{
					directory: "apps/example",
					findings: [finding("GHSA-unlisted-0000-0000", "left-pad")],
				},
			],
			allowlist,
		);
		expect(failures).toHaveLength(1);
		expect(failures[0]).toContain("GHSA-unlisted-0000-0000");
		expect(failures[0]).toContain("apps/example");
	});
});
