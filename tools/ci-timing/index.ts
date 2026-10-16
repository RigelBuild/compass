#!/usr/bin/env bun
import type { ClassSummary, Quantiles, RunRecord } from "./core.ts";
import { summarize } from "./core.ts";

const RUN_CLASSES = [
	"pr",
	"queue",
	"merge_group",
	"push",
	"schedule",
	"dispatch",
] as const;
const GH_REPOSITORY = "RigelBuild/compass";
const GH_WORKFLOW = "ci.yml";

type Options = { since: string; until: string; json: boolean };
type JsonObject = Record<string, unknown>;

function isObject(value: unknown): value is JsonObject {
	return typeof value === "object" && value !== null && !Array.isArray(value);
}

function parseJson(text: string, label: string): unknown {
	try {
		return JSON.parse(text);
	} catch (error) {
		throw new Error(`${label} returned invalid JSON`, { cause: error });
	}
}

function parseDate(value: string, option: string): string {
	if (!/^\d{4}-\d{2}-\d{2}$/.test(value)) {
		throw new Error(`${option} must use YYYY-MM-DD`);
	}
	const timestamp = Date.parse(`${value}T00:00:00Z`);
	if (
		!Number.isFinite(timestamp) ||
		new Date(timestamp).toISOString().slice(0, 10) !== value
	) {
		throw new Error(`${option} is not a valid calendar date`);
	}
	return value;
}

function setDateOption(
	options: Options,
	argument: string,
	value: string,
): void {
	if (argument === "--since") {
		options.since = parseDate(value, argument);
	} else {
		options.until = parseDate(value, argument);
	}
}

function readOptionValue(
	args: readonly string[],
	index: number,
	argument: string,
): string {
	const value = args[index + 1];
	if (value === undefined || value.startsWith("--")) {
		throw new Error(`${argument} requires a YYYY-MM-DD value`);
	}
	return value;
}

function parseOptions(args: readonly string[]): Options {
	const options: Options = { since: "", until: "", json: false };
	let hasSince = false;
	for (let index = 0; index < args.length; index += 1) {
		const argument = args[index];
		if (argument === "--json") {
			options.json = true;
		} else if (argument === "--since" || argument === "--until") {
			setDateOption(options, argument, readOptionValue(args, index, argument));
			hasSince ||= argument === "--since";
			index += 1;
		} else {
			throw new Error(`Unknown argument: ${String(argument)}`);
		}
	}

	if (!hasSince) {
		throw new Error(
			"Usage: bun tools/ci-timing/index.ts --since YYYY-MM-DD [--until YYYY-MM-DD] [--json]",
		);
	}
	if (options.until === "") {
		options.until = new Date().toISOString().slice(0, 10);
	}
	if (options.since > options.until) {
		throw new Error("--since must not be after --until");
	}
	return options;
}

async function ghApi(args: readonly string[], label: string): Promise<unknown> {
	const child = Bun.spawn(
		["gh", "api", "--paginate", "--slurp", "--method", "GET", ...args],
		{
			stdout: "pipe",
			stderr: "pipe",
		},
	);
	const [stdout, stderr, status] = await Promise.all([
		child.stdout.text(),
		child.stderr.text(),
		child.exited,
	]);
	if (status !== 0) {
		throw new Error(`gh api ${label} exited ${status}: ${stderr.trim()}`);
	}
	return parseJson(stdout, `gh api ${label}`);
}

function pages(value: unknown, label: string): JsonObject[] {
	const candidates = Array.isArray(value) ? value : [value];
	const result: JsonObject[] = [];
	for (const candidate of candidates) {
		if (!isObject(candidate)) {
			throw new Error(`${label} page is not an object`);
		}
		result.push(candidate);
	}
	return result;
}

function pageItems(page: JsonObject, key: string, label: string): JsonObject[] {
	const items = page[key];
	if (!Array.isArray(items)) {
		throw new Error(`${label} response has no ${key} array`);
	}
	const result: JsonObject[] = [];
	for (const item of items) {
		if (!isObject(item)) {
			throw new Error(`${label} contains a non-object ${key} item`);
		}
		result.push(item);
	}
	return result;
}

function requiredString(
	object: JsonObject,
	key: string,
	label: string,
): string {
	const value = object[key];
	if (typeof value !== "string") {
		throw new Error(`${label} is missing string field ${key}`);
	}
	return value;
}

function requiredNumber(
	object: JsonObject,
	key: string,
	label: string,
): number {
	const value = object[key];
	if (typeof value !== "number" || !Number.isFinite(value)) {
		throw new Error(`${label} is missing numeric field ${key}`);
	}
	return value;
}

function timestamp(value: unknown, label: string, optional: boolean): number {
	if (optional && (value === null || value === undefined)) {
		return 0;
	}
	if (typeof value !== "string") {
		throw new Error(`${label} is not a timestamp`);
	}
	const milliseconds = Date.parse(value);
	if (!Number.isFinite(milliseconds)) {
		throw new Error(`${label} is not a valid timestamp`);
	}
	return milliseconds / 1000;
}

function knownConclusion(value: unknown): RunRecord["conclusion"] | undefined {
	if (value === "success" || value === "failure" || value === "cancelled") {
		return value;
	}
	return undefined;
}

function readRuns(value: unknown): RunRecord[] {
	const result: RunRecord[] = [];
	let listed = 0;
	let total = 0;
	for (const page of pages(value, "workflow runs")) {
		total = requiredNumber(page, "total_count", "workflow runs");
		listed += pageItems(page, "workflow_runs", "workflow runs").length;
	}
	// The runs search stops at 1000 results; a silent cut would skew every quantile.
	if (listed < total) {
		throw new Error(
			`workflow runs listed ${listed} of ${total}; narrow --since/--until below 1000 runs`,
		);
	}
	for (const page of pages(value, "workflow runs")) {
		for (const run of pageItems(page, "workflow_runs", "workflow runs")) {
			const conclusion = knownConclusion(run.conclusion);
			if (conclusion === undefined) {
				continue;
			}
			result.push({
				id: requiredNumber(run, "id", "workflow run"),
				event: requiredString(run, "event", "workflow run"),
				headBranch: requiredString(run, "head_branch", "workflow run"),
				conclusion,
				runStartedAt: timestamp(
					run.run_started_at,
					"workflow run run_started_at",
					true,
				),
				updatedAt: timestamp(run.updated_at, "workflow run updated_at", false),
				jobs: [],
			});
		}
	}
	return result;
}

function readJobs(value: unknown, runId: number): RunRecord["jobs"] {
	const result: RunRecord["jobs"] = [];
	for (const page of pages(value, `jobs for run ${runId}`)) {
		for (const job of pageItems(page, "jobs", `jobs for run ${runId}`)) {
			const conclusion = job.conclusion;
			if (
				conclusion !== "success" &&
				conclusion !== "failure" &&
				conclusion !== "cancelled" &&
				conclusion !== "skipped"
			) {
				throw new Error(
					`Job in run ${runId} has unsupported conclusion: ${String(conclusion)}`,
				);
			}
			result.push({
				name: requiredString(job, "name", `job in run ${runId}`),
				conclusion,
				createdAt: timestamp(
					job.created_at,
					`job in run ${runId} created_at`,
					false,
				),
				startedAt: timestamp(
					job.started_at,
					`job in run ${runId} started_at`,
					true,
				),
				completedAt: timestamp(
					job.completed_at,
					`job in run ${runId} completed_at`,
					true,
				),
			});
		}
	}
	return result;
}

async function mapRuns(runs: readonly RunRecord[]): Promise<RunRecord[]> {
	let cursor = 0;
	const workers = Array.from({ length: Math.min(8, runs.length) }, async () => {
		while (cursor < runs.length) {
			const index = cursor;
			cursor += 1;
			const run = runs[index];
			if (run === undefined) {
				throw new Error("Run cursor exceeded workflow run list");
			}
			if (run.conclusion !== "cancelled") {
				const value = await ghApi(
					[
						`repos/${GH_REPOSITORY}/actions/runs/${run.id}/jobs`,
						"--field",
						"filter=latest",
						"--field",
						"per_page=100",
					],
					`jobs for run ${run.id}`,
				);
				run.jobs = readJobs(value, run.id);
			}
		}
	});
	await Promise.all(workers);
	return [...runs];
}

function fixed(value: number): string {
	return value.toFixed(2);
}

function markdownCell(value: string): string {
	return value.replaceAll("|", "\\|").replaceAll("\n", " ");
}

function metricRow(name: string, values: Quantiles, unit = "s"): string {
	return `| ${name} | ${values.n} | ${fixed(values.p50)} ${unit} | ${fixed(values.p90)} ${unit} | ${fixed(values.p99)} ${unit} | ${fixed(values.max)} ${unit} |`;
}

function renderMarkdown(
	summary: Record<(typeof RUN_CLASSES)[number], ClassSummary>,
): string {
	const lines: string[] = [];
	for (const runClass of RUN_CLASSES) {
		const current = summary[runClass];
		lines.push(
			`## ${runClass}`,
			"",
			`Kept: ${current.kept}; cancelled: ${current.cancelled}.`,
			"",
			"| Metric | n | p50 | p90 | p99 | max |",
			"| --- | ---: | ---: | ---: | ---: | ---: |",
			metricRow("Wall", current.wall),
			metricRow("Per-run max wait", current.maxWait),
			metricRow("Runner minutes", current.runnerMinutes, "min"),
			"",
			"| Job | Skip rate | Long-pole share | Wait p50 / p90 / p99 / max (s) | Duration p50 / p90 / p99 / max (s) |",
			"| --- | ---: | ---: | ---: | ---: |",
		);
		const jobNames = [
			...new Set([
				...Object.keys(current.skipRate),
				...Object.keys(current.jobDuration),
			]),
		].sort();
		for (const name of jobNames) {
			const waits = current.jobWait[name];
			const durations = current.jobDuration[name];
			const waitText = waits
				? `${fixed(waits.p50)} / ${fixed(waits.p90)} / ${fixed(waits.p99)} / ${fixed(waits.max)}`
				: "—";
			const durationText = durations
				? `${fixed(durations.p50)} / ${fixed(durations.p90)} / ${fixed(durations.p99)} / ${fixed(durations.max)}`
				: "—";
			lines.push(
				`| ${markdownCell(name)} | ${((current.skipRate[name] ?? 0) * 100).toFixed(1)}% | ${((current.longPole[name] ?? 0) * 100).toFixed(1)}% | ${waitText} | ${durationText} |`,
			);
		}
		lines.push(
			"",
			`UTC hours with any job wait over 120 s: ${current.hoursOver120s.join(", ") || "none"}.`,
			"",
		);
	}
	return lines.join("\n");
}

async function main(): Promise<void> {
	const options = parseOptions(process.argv.slice(2));
	const rawRuns = await ghApi(
		[
			`repos/${GH_REPOSITORY}/actions/workflows/${GH_WORKFLOW}/runs`,
			"--field",
			`created=${options.since}..${options.until}`,
			"--field",
			"per_page=100",
		],
		"workflow runs",
	);
	const runs = await mapRuns(readRuns(rawRuns));
	const summary = summarize(runs);
	if (options.json) {
		console.log(JSON.stringify(summary, null, 2));
	} else {
		console.log(renderMarkdown(summary));
	}
}

if (import.meta.main) {
	try {
		await main();
	} catch (error) {
		const message = error instanceof Error ? error.message : String(error);
		console.error(message);
		process.exitCode = 1;
	}
}
