#!/usr/bin/env bun
import { backstopAction } from "./core.ts";

const LABEL = "ci-backstop";

type RunEnvironment = {
	result: "success" | "failure";
	event: "push" | "schedule";
	runId: string;
	repository: string;
	serverUrl: string;
};

function requiredEnv(name: string): string {
	const value = process.env[name];
	if (value === undefined || value.length === 0) {
		throw new Error(`${name} is required`);
	}
	return value;
}

function readEnvironment(): RunEnvironment {
	const result = requiredEnv("ROLLUP_RESULT");
	if (result !== "success" && result !== "failure") {
		throw new Error(`unexpected ROLLUP_RESULT: ${result}`);
	}

	const event = requiredEnv("GITHUB_EVENT_NAME");
	if (event !== "push" && event !== "schedule") {
		throw new Error(`unexpected GITHUB_EVENT_NAME: ${event}`);
	}

	const runId = requiredEnv("GITHUB_RUN_ID");
	if (!/^\d+$/.test(runId)) {
		throw new Error(`GITHUB_RUN_ID must be a positive integer: ${runId}`);
	}

	const repository = requiredEnv("GITHUB_REPOSITORY");
	if (!/^[^/]+\/[^/]+$/.test(repository)) {
		throw new Error(
			`GITHUB_REPOSITORY must be owner/repository: ${repository}`,
		);
	}

	const serverUrl = requiredEnv("GITHUB_SERVER_URL").replace(/\/$/, "");
	const parsedServerUrl = new URL(serverUrl);
	if (
		parsedServerUrl.protocol !== "https:" &&
		parsedServerUrl.protocol !== "http:"
	) {
		throw new Error("GITHUB_SERVER_URL must use HTTP or HTTPS");
	}
	return {
		result,
		event,
		runId,
		repository,
		serverUrl,
	};
}

async function gh(args: readonly string[]): Promise<string> {
	const child = Bun.spawn(["gh", ...args], {
		stdout: "pipe",
		stderr: "pipe",
	});
	const [stdout, stderr, exitCode] = await Promise.all([
		new Response(child.stdout).text(),
		new Response(child.stderr).text(),
		child.exited,
	]);
	if (exitCode !== 0) {
		throw new Error(
			`gh ${args.join(" ")} failed with exit code ${exitCode}: ${stderr.trim()}`,
		);
	}
	return stdout.trim();
}

function parseJson(value: string, command: string): unknown {
	try {
		return JSON.parse(value);
	} catch (error) {
		throw new Error(
			`gh ${command} returned invalid JSON: ${error instanceof Error ? error.message : String(error)}`,
		);
	}
}

function record(value: unknown): Record<string, unknown> {
	if (typeof value !== "object" || value === null || Array.isArray(value)) {
		throw new Error("gh returned an unexpected JSON object");
	}
	return Object.fromEntries(Object.entries(value));
}

function runStartedAt(value: unknown, source: string): string {
	const data = record(value);
	const startedAt = data.run_started_at;
	if (typeof startedAt !== "string" || Number.isNaN(Date.parse(startedAt))) {
		throw new Error(`${source} has no valid run_started_at`);
	}
	return startedAt;
}

// A rollup also fails on a timed-out or cancelled need, so name those jobs too.
const UNSUCCESSFUL = new Set(["failure", "timed_out", "cancelled"]);

export function failedJobNames(value: unknown): string[] {
	const response = record(value);
	if (!Array.isArray(response.jobs)) {
		throw new Error("gh jobs response has no jobs array");
	}
	return response.jobs.flatMap((job) => {
		const data = record(job);
		if (typeof data.conclusion !== "string") return [];
		if (!UNSUCCESSFUL.has(data.conclusion)) return [];
		if (typeof data.name !== "string") {
			throw new Error("gh returned a failed job without a name");
		}
		return [data.name];
	});
}

function openIssueNumber(value: unknown): number | null {
	if (!Array.isArray(value)) {
		throw new Error("gh issue list response is not an array");
	}
	if (value.length > 1) {
		throw new Error(`more than one open issue has the ${LABEL} label`);
	}
	const issue = value[0];
	if (issue === undefined) return null;
	const data = record(issue);
	if (typeof data.number !== "number" || !Number.isSafeInteger(data.number)) {
		throw new Error("gh returned an issue without a valid number");
	}
	return data.number;
}

async function startedAtForLatestCompletedSweep(
	repository: string,
	event: "push" | "schedule",
): Promise<string | null> {
	const endpoint = `repos/${repository}/actions/workflows/ci.yml/runs?event=${event}&status=completed&per_page=1`;
	const response = parseJson(
		await gh([
			"api",
			"--jq",
			".workflow_runs | map({id, run_started_at, status, event})",
			endpoint,
		]),
		`api ${endpoint}`,
	);
	if (!Array.isArray(response)) {
		throw new Error(`gh api ${endpoint} returned no workflow_runs array`);
	}
	const latestRun = response[0];
	if (latestRun === undefined) return null;
	const runData = record(latestRun);
	if (
		runData.status !== "completed" ||
		runData.event !== event ||
		typeof runData.id !== "number"
	) {
		throw new Error(`gh api ${endpoint} returned an invalid completed run`);
	}
	return runStartedAt(runData, "completed workflow run");
}

async function main(): Promise<void> {
	const dryRun = process.argv.slice(2);
	if (dryRun.length > 1 || (dryRun.length === 1 && dryRun[0] !== "--dry-run")) {
		throw new Error("usage: bun tools/ci-backstop/index.ts [--dry-run]");
	}
	const environment = readEnvironment();
	const { repository, runId } = environment;

	const runEndpoint = `repos/${repository}/actions/runs/${runId}`;
	const [runResponse, jobsResponse] = await Promise.all([
		gh(["api", runEndpoint]),
		gh(["api", `${runEndpoint}/jobs?per_page=100`]),
	]);
	const currentRunStartedAt = runStartedAt(
		parseJson(runResponse, `api ${runEndpoint}`),
		"current workflow run",
	);
	const failedJobs = failedJobNames(
		parseJson(jobsResponse, `api ${runEndpoint}/jobs`),
	);

	const [latestPush, latestSchedule] = await Promise.all([
		startedAtForLatestCompletedSweep(repository, "push"),
		startedAtForLatestCompletedSweep(repository, "schedule"),
	]);
	const newestSweepStartedAt =
		[latestPush, latestSchedule]
			.filter((startedAt): startedAt is string => startedAt !== null)
			.sort((left, right) => Date.parse(left) - Date.parse(right))
			.at(-1) ?? currentRunStartedAt;

	if (dryRun[0] !== "--dry-run") {
		await gh(["label", "create", LABEL, "--force", "--repo", repository]);
	}
	const issueResponse = await gh([
		"issue",
		"list",
		"--repo",
		repository,
		"--state",
		"open",
		"--label",
		LABEL,
		"--json",
		"number",
		"--limit",
		"2",
	]);
	const openIssue = openIssueNumber(parseJson(issueResponse, "issue list"));
	const action = backstopAction({
		result: environment.result,
		event: environment.event,
		runUrl: `${environment.serverUrl}/${repository}/actions/runs/${runId}`,
		failedJobs,
		openIssue,
		runStartedAt: currentRunStartedAt,
		newestSweepStartedAt,
	});

	if (dryRun[0] === "--dry-run") {
		console.log(JSON.stringify(action, null, 2));
		return;
	}

	switch (action.kind) {
		case "open":
			await gh([
				"issue",
				"create",
				"--repo",
				repository,
				"--title",
				action.title,
				"--body",
				action.body,
				"--label",
				LABEL,
			]);
			break;
		case "comment":
			await gh([
				"issue",
				"comment",
				String(action.issue),
				"--repo",
				repository,
				"--body",
				action.body,
			]);
			break;
		case "close":
			await gh([
				"issue",
				"close",
				String(action.issue),
				"--repo",
				repository,
				"--comment",
				action.body,
			]);
			break;
		case "none":
			console.log("No backstop issue action needed.");
			break;
	}
}

if (import.meta.main) {
	try {
		await main();
	} catch (error) {
		console.error(error instanceof Error ? error.message : String(error));
		process.exitCode = 1;
	}
}
