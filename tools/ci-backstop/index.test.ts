import { describe, expect, test } from "bun:test";
import { failedJobNames, newestVerdictStartedAt } from "./index.ts";

describe("failedJobNames", () => {
	test("names failed, timed-out and cancelled jobs only", () => {
		const jobs = [
			{ name: "moon (go)", conclusion: "failure" },
			{ name: "pgtest", conclusion: "timed_out" },
			{ name: "e2e", conclusion: "cancelled" },
			{ name: "setup", conclusion: "success" },
			{ name: "microvm", conclusion: "skipped" },
			{ name: "backstop", conclusion: null },
		];
		expect(failedJobNames({ jobs })).toEqual(["moon (go)", "pgtest", "e2e"]);
	});

	test("rejects a response without a jobs array", () => {
		expect(() => failedJobNames({})).toThrow("no jobs array");
	});
});

describe("newestVerdictStartedAt", () => {
	const run = (conclusion: string, at: string) => ({
		status: "completed",
		conclusion,
		run_started_at: at,
	});

	test("a newer cancelled run does not supersede an older verdict", () => {
		const runs = [
			run("cancelled", "2026-10-16T12:00:00Z"),
			run("failure", "2026-10-16T11:00:00Z"),
		];
		expect(newestVerdictStartedAt(runs)).toBe("2026-10-16T11:00:00Z");
	});

	test("the newest success or failure wins", () => {
		const runs = [
			run("success", "2026-10-16T12:00:00Z"),
			run("failure", "2026-10-16T11:00:00Z"),
		];
		expect(newestVerdictStartedAt(runs)).toBe("2026-10-16T12:00:00Z");
	});

	test("no verdict among the runs reads as none", () => {
		expect(
			newestVerdictStartedAt([run("cancelled", "2026-10-16T12:00:00Z")]),
		).toBeNull();
	});
});
