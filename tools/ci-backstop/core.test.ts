import { describe, expect, test } from "bun:test";
import { backstopAction } from "./core.ts";

const runUrl = "https://github.com/RigelBuild/compass/actions/runs/123";
const failedJobs = ["moon (bun)", "pgtest"];

const cases = [
	{
		name: "failure without an issue opens one",
		result: "failure",
		openIssue: null,
		newerSweep: false,
		kind: "open",
	},
	{
		name: "failure with an issue comments",
		result: "failure",
		openIssue: 42,
		newerSweep: false,
		kind: "comment",
	},
	{
		name: "success without an issue does nothing",
		result: "success",
		openIssue: null,
		newerSweep: false,
		kind: "none",
	},
	{
		name: "success with an issue closes it",
		result: "success",
		openIssue: 42,
		newerSweep: false,
		kind: "close",
	},
	{
		name: "newer sweep supersedes failure without an issue",
		result: "failure",
		openIssue: null,
		newerSweep: true,
		kind: "none",
	},
	{
		name: "newer sweep supersedes failure with an issue",
		result: "failure",
		openIssue: 42,
		newerSweep: true,
		kind: "none",
	},
	{
		name: "newer sweep supersedes success without an issue",
		result: "success",
		openIssue: null,
		newerSweep: true,
		kind: "none",
	},
	{
		name: "newer sweep supersedes success with an issue",
		result: "success",
		openIssue: 42,
		newerSweep: true,
		kind: "none",
	},
] as const;

describe("backstopAction", () => {
	for (const scenario of cases) {
		test(scenario.name, () => {
			const action = backstopAction({
				result: scenario.result,
				event: "push",
				runUrl,
				failedJobs,
				openIssue: scenario.openIssue,
				runStartedAt: "2026-10-15T10:00:00Z",
				newestSweepStartedAt: scenario.newerSweep
					? "2026-10-15T10:00:01Z"
					: "2026-10-15T10:00:00Z",
			});

			expect(action.kind).toBe(scenario.kind);
			if (action.kind !== "none") {
				expect(action.body).toContain(runUrl);
				expect(action.body).toContain("moon (bun), pgtest");
				expect(action.body.split("\n")).toHaveLength(2);
			}
			if (action.kind === "open") {
				expect(action.title).toBe("CI push full sweep failed");
			}
		});
	}
});
