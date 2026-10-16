export type BackstopAction =
	| { kind: "open"; title: string; body: string }
	| { kind: "comment" | "close"; issue: number; body: string }
	| { kind: "none" };

export function backstopAction(input: {
	result: "success" | "failure";
	event: "push" | "schedule";
	runUrl: string;
	failedJobs: readonly string[];
	openIssue: number | null;
	runStartedAt: string;
	newestSweepStartedAt: string;
}): BackstopAction {
	if (Date.parse(input.newestSweepStartedAt) > Date.parse(input.runStartedAt)) {
		return { kind: "none" };
	}

	const failedJobs =
		input.failedJobs.length > 0 ? input.failedJobs.join(", ") : "none";
	const body = [
		`CI ${input.event} full sweep: ${input.result}. Run: ${input.runUrl}`,
		`Failed jobs: ${failedJobs}`,
	].join("\n");

	if (input.result === "failure") {
		if (input.openIssue !== null) {
			return { kind: "comment", issue: input.openIssue, body };
		}
		return {
			kind: "open",
			title: `CI ${input.event} full sweep failed`,
			body,
		};
	}

	return input.openIssue === null
		? { kind: "none" }
		: { kind: "close", issue: input.openIssue, body };
}
