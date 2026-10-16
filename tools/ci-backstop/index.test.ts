import { describe, expect, test } from "bun:test";
import { failedJobNames } from "./index.ts";

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
