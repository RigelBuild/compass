export type RunClass =
	| "pr"
	| "queue"
	| "merge_group"
	| "push"
	| "schedule"
	| "dispatch";

export type JobRecord = {
	name: string;
	conclusion: "success" | "failure" | "cancelled" | "skipped";
	createdAt: number;
	startedAt: number;
	completedAt: number;
};

export type RunRecord = {
	id: number;
	event: string;
	headBranch: string;
	conclusion: "success" | "failure" | "cancelled";
	runStartedAt: number;
	updatedAt: number;
	jobs: JobRecord[];
};

// Jobs that wait on every other job always finish last, so they can never be the long pole.
export const AGGREGATOR_JOBS: readonly string[] = ["rollup"];

export function classify(
	run: Pick<RunRecord, "event" | "headBranch">,
): RunClass {
	if (run.event === "pull_request") {
		return run.headBranch.startsWith("trunk-merge/") ? "queue" : "pr";
	}

	switch (run.event) {
		case "merge_group":
			return "merge_group";
		case "push":
			return "push";
		case "schedule":
			return "schedule";
		case "workflow_dispatch":
			return "dispatch";
		default:
			throw new Error(`Unsupported workflow event: ${run.event}`);
	}
}

export type Quantiles = {
	n: number;
	p50: number;
	p90: number;
	p99: number;
	max: number;
};

export type ClassSummary = {
	kept: number;
	cancelled: number;
	wall: Quantiles;
	maxWait: Quantiles;
	runnerMinutes: Quantiles;
	jobDuration: Record<string, Quantiles>;
	jobWait: Record<string, Quantiles>;
	skipRate: Record<string, number>;
	longPole: Record<string, number>;
	hoursOver120s: string[];
};

type MutableClassSummary = {
	kept: number;
	cancelled: number;
	wall: number[];
	maxWait: number[];
	runnerMinutes: number[];
	jobDuration: Map<string, number[]>;
	jobWait: Map<string, number[]>;
	jobCount: Map<string, number>;
	skippedCount: Map<string, number>;
	longPoleCount: Map<string, number>;
	hoursOver120s: Set<string>;
};

function emptyClassSummary(): MutableClassSummary {
	return {
		kept: 0,
		cancelled: 0,
		wall: [],
		maxWait: [],
		runnerMinutes: [],
		jobDuration: new Map(),
		jobWait: new Map(),
		jobCount: new Map(),
		skippedCount: new Map(),
		longPoleCount: new Map(),
		hoursOver120s: new Set(),
	};
}

function appendValue(
	values: Map<string, number[]>,
	key: string,
	value: number,
): void {
	const samples = values.get(key);
	if (samples) {
		samples.push(value);
	} else {
		values.set(key, [value]);
	}
}

function increment(values: Map<string, number>, key: string): void {
	values.set(key, (values.get(key) ?? 0) + 1);
}

function quantiles(values: readonly number[]): Quantiles {
	const sorted = [...values].sort((left, right) => left - right);
	if (sorted.length === 0) {
		return { n: 0, p50: 0, p90: 0, p99: 0, max: 0 };
	}
	const nearestRank = (p: number): number =>
		sorted[Math.ceil(p * sorted.length) - 1] ?? 0;
	return {
		n: sorted.length,
		p50: nearestRank(0.5),
		p90: nearestRank(0.9),
		p99: nearestRank(0.99),
		max: sorted[sorted.length - 1] ?? 0,
	};
}

function summarizeClass(summary: MutableClassSummary): ClassSummary {
	const jobDuration: Record<string, Quantiles> = {};
	const jobWait: Record<string, Quantiles> = {};
	const skipRate: Record<string, number> = {};
	const longPole: Record<string, number> = {};

	for (const [name, samples] of summary.jobDuration) {
		jobDuration[name] = quantiles(samples);
	}
	for (const [name, samples] of summary.jobWait) {
		jobWait[name] = quantiles(samples);
	}
	for (const [name, total] of summary.jobCount) {
		skipRate[name] = (summary.skippedCount.get(name) ?? 0) / total;
	}
	for (const [name, count] of summary.longPoleCount) {
		longPole[name] = summary.kept === 0 ? 0 : count / summary.kept;
	}

	return {
		kept: summary.kept,
		cancelled: summary.cancelled,
		wall: quantiles(summary.wall),
		maxWait: quantiles(summary.maxWait),
		runnerMinutes: quantiles(summary.runnerMinutes),
		jobDuration,
		jobWait,
		skipRate,
		longPole,
		hoursOver120s: [...summary.hoursOver120s].sort(),
	};
}

function summarizeJob(
	job: JobRecord,
	summary: MutableClassSummary,
	current: {
		maxWait: number;
		runnerSeconds: number;
		latestCompletion: number;
		lastJobs: string[];
	},
): void {
	increment(summary.jobCount, job.name);
	if (job.conclusion === "skipped") {
		increment(summary.skippedCount, job.name);
	}
	if (job.startedAt <= 0) {
		return;
	}
	const wait = job.startedAt - job.createdAt;
	appendValue(summary.jobWait, job.name, wait);
	current.maxWait = Math.max(current.maxWait, wait);
	if (wait > 120) {
		summary.hoursOver120s.add(
			new Date(job.createdAt * 1000).toISOString().slice(0, 13),
		);
	}
	if (job.conclusion === "skipped" || job.completedAt <= 0) {
		return;
	}

	const duration = job.completedAt - job.startedAt;
	appendValue(summary.jobDuration, job.name, duration);
	current.runnerSeconds += duration;
	if (AGGREGATOR_JOBS.includes(job.name)) {
		return;
	}
	if (job.completedAt > current.latestCompletion) {
		current.latestCompletion = job.completedAt;
		current.lastJobs.length = 0;
		current.lastJobs.push(job.name);
	} else if (job.completedAt === current.latestCompletion) {
		current.lastJobs.push(job.name);
	}
}

function summarizeRun(summary: MutableClassSummary, run: RunRecord): void {
	if (run.conclusion === "cancelled") {
		summary.cancelled += 1;
		return;
	}

	summary.kept += 1;
	summary.wall.push(run.updatedAt - run.runStartedAt);
	const current = {
		maxWait: 0,
		runnerSeconds: 0,
		latestCompletion: Number.NEGATIVE_INFINITY,
		lastJobs: [] as string[],
	};
	for (const job of run.jobs) {
		summarizeJob(job, summary, current);
	}

	summary.maxWait.push(current.maxWait);
	summary.runnerMinutes.push(current.runnerSeconds / 60);
	for (const name of current.lastJobs) {
		increment(summary.longPoleCount, name);
	}
}

export function summarize(
	runs: readonly RunRecord[],
): Record<RunClass, ClassSummary> {
	const summaries: Record<RunClass, MutableClassSummary> = {
		pr: emptyClassSummary(),
		queue: emptyClassSummary(),
		merge_group: emptyClassSummary(),
		push: emptyClassSummary(),
		schedule: emptyClassSummary(),
		dispatch: emptyClassSummary(),
	};

	for (const run of runs) {
		summarizeRun(summaries[classify(run)], run);
	}

	const result: Record<RunClass, ClassSummary> = {
		pr: summarizeClass(summaries.pr),
		queue: summarizeClass(summaries.queue),
		merge_group: summarizeClass(summaries.merge_group),
		push: summarizeClass(summaries.push),
		schedule: summarizeClass(summaries.schedule),
		dispatch: summarizeClass(summaries.dispatch),
	};
	return result;
}
