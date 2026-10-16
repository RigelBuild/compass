import { describe, expect, test } from "bun:test";
import { readJobs, readRuns } from "./index.ts";

function runItem(id: number) {
	return {
		id,
		event: "pull_request",
		head_branch: "feature",
		conclusion: "success",
		run_started_at: "2026-10-15T00:00:00Z",
		updated_at: "2026-10-15T00:10:00Z",
	};
}

function runPage(totalCount: number, ids: readonly number[]) {
	return { total_count: totalCount, workflow_runs: ids.map(runItem) };
}

function jobItem(conclusion: string) {
	return {
		name: "e2e",
		conclusion,
		created_at: "2026-10-15T00:00:00Z",
		started_at: "2026-10-15T00:00:05Z",
		completed_at: "2026-10-15T00:05:00Z",
	};
}

describe("readRuns", () => {
	test("accepts a complete multi-page listing", () => {
		const runs = readRuns([runPage(3, [1, 2]), runPage(3, [3])]);
		expect(runs.map((run) => run.id)).toEqual([1, 2, 3]);
	});

	test("rejects a listing cut short of total_count", () => {
		expect(() => readRuns([runPage(1200, [1, 2])])).toThrow(
			"workflow runs listed 2 of 1200",
		);
	});
});

describe("readJobs", () => {
	test("reads a timed-out job as a failure that ran", () => {
		const [job] = readJobs([{ jobs: [jobItem("timed_out")] }], 7);
		expect(job?.conclusion).toBe("failure");
		expect(job?.completedAt).toBeGreaterThan(job?.startedAt ?? 0);
	});

	test("rejects an unknown conclusion", () => {
		expect(() => readJobs([{ jobs: [jobItem("exploded")] }], 7)).toThrow(
			"unsupported conclusion: exploded",
		);
	});
});
