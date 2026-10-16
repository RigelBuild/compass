import { describe, expect, test } from "bun:test";
import type { JobRecord, RunRecord } from "./core.ts";
import { classify, summarize } from "./core.ts";

function job(
	name: string,
	createdAt: number,
	startedAt: number,
	completedAt: number,
	conclusion: JobRecord["conclusion"] = "success",
): JobRecord {
	return { name, conclusion, createdAt, startedAt, completedAt };
}

function run(
	id: number,
	jobs: JobRecord[],
	overrides: Partial<RunRecord> = {},
): RunRecord {
	return {
		id,
		event: "pull_request",
		headBranch: "feature/example",
		conclusion: "success",
		runStartedAt: 100,
		updatedAt: 200,
		jobs,
		...overrides,
	};
}

describe("classify", () => {
	test("covers every run class, including Trunk merge-queue branches", () => {
		expect(classify({ event: "pull_request", headBranch: "feature/a" })).toBe(
			"pr",
		);
		expect(
			classify({ event: "pull_request", headBranch: "trunk-merge/123" }),
		).toBe("queue");
		expect(classify({ event: "merge_group", headBranch: "main" })).toBe(
			"merge_group",
		);
		expect(classify({ event: "push", headBranch: "main" })).toBe("push");
		expect(classify({ event: "schedule", headBranch: "main" })).toBe(
			"schedule",
		);
		expect(classify({ event: "workflow_dispatch", headBranch: "main" })).toBe(
			"dispatch",
		);
	});
});

describe("summarize", () => {
	test("counts cancelled runs separately and excludes them from measurements", () => {
		const summary = summarize([
			run(1, [job("build", 100, 110, 150)], { updatedAt: 160 }),
			run(2, [job("build", 100, 110, 190)], {
				conclusion: "cancelled",
				updatedAt: 200,
			}),
		]);

		expect(summary.pr.kept).toBe(1);
		expect(summary.pr.cancelled).toBe(1);
		expect(summary.pr.wall).toEqual({
			n: 1,
			p50: 60,
			p90: 60,
			p99: 60,
			max: 60,
		});
		expect(summary.pr.jobDuration.build?.n).toBe(1);
	});

	test("computes per-job skip rate and long-pole share over three runs", () => {
		const summary = summarize([
			run(1, [job("build", 100, 101, 110), job("deploy", 100, 102, 120)]),
			run(2, [
				job("build", 100, 100, 100, "skipped"),
				job("deploy", 100, 103, 115),
			]),
			run(3, [job("build", 100, 101, 130), job("deploy", 100, 102, 125)]),
		]);

		expect(summary.pr.skipRate.build).toBeCloseTo(1 / 3);
		expect(summary.pr.longPole.build).toBeCloseTo(1 / 3);
		expect(summary.pr.longPole.deploy).toBeCloseTo(2 / 3);
	});
	test("computes max wait and runner minutes from job timestamps", () => {
		const summary = summarize([
			run(1, [job("build", 100, 110, 170), job("test", 100, 125, 185)]),
		]);

		expect(summary.pr.maxWait).toEqual({
			n: 1,
			p50: 25,
			p90: 25,
			p99: 25,
			max: 25,
		});
		expect(summary.pr.runnerMinutes).toEqual({
			n: 1,
			p50: 2,
			p90: 2,
			p99: 2,
			max: 2,
		});
	});

	test("uses nearest-rank quantiles for one and two values", () => {
		const one = summarize([run(1, [], { runStartedAt: 100, updatedAt: 150 })]);
		const two = summarize([
			run(1, [], { runStartedAt: 100, updatedAt: 110 }),
			run(2, [], { runStartedAt: 100, updatedAt: 120 }),
		]);

		expect(one.pr.wall).toEqual({ n: 1, p50: 50, p90: 50, p99: 50, max: 50 });
		expect(two.pr.wall).toEqual({ n: 2, p50: 10, p90: 20, p99: 20, max: 20 });
	});

	test("buckets waits over 120 seconds by UTC hour", () => {
		const firstHour = Date.parse("2026-10-11T00:30:00Z") / 1000;
		const secondHour = Date.parse("2026-10-11T01:01:00Z") / 1000;
		const summary = summarize([
			run(1, [
				job("slow-a", firstHour, firstHour + 121, firstHour + 130),
				job("slow-b", firstHour + 1000, firstHour + 1120, firstHour + 1130),
				job("slow-c", secondHour, secondHour + 121, secondHour + 130),
			]),
		]);

		expect(summary.pr.hoursOver120s).toEqual([
			"2026-10-11T00",
			"2026-10-11T01",
		]);
	});
});
