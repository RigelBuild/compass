import { describe, expect, test } from "bun:test";
import {
	AgentSessionState,
	AgentSessionStatusSchema,
	create,
	IssueSchema,
	SubscribeEventsResponseSchema,
} from "@compass/client";
import { flush } from "solid-js";
import type { Issue as DomainIssue } from "../stub-data";
import { createFakeCompass } from "./compass-fake";
import type { AccountSession } from "./events";
import { runEventStream } from "./events";

const AGENT_ID = "acc-compass-ui";

async function settleUntil(predicate: () => boolean): Promise<void> {
	for (let i = 0; i < 500 && !predicate(); i++) {
		await Promise.resolve();
		flush();
	}
}

describe("createFakeCompass scripted event replay", () => {
	test("replays event frames through runEventStream and serves the board snapshot", async () => {
		const issue = create(IssueSchema, {
			id: "fixture-issue",
			title: "Replay board",
		});
		const frame = create(SubscribeEventsResponseSchema, {
			seq: 1n,
			instanceEpoch: 1n,
			payload: {
				case: "agentSessionStatus",
				value: create(AgentSessionStatusSchema, {
					sessionId: "session-replay",
					agentAccountId: AGENT_ID,
					state: AgentSessionState.WORKING,
				}),
			},
		});
		const fake = createFakeCompass({ events: [frame], board: [issue] });
		const abort = new AbortController();
		let sessions: ReadonlyMap<string, AccountSession> = new Map();
		let issues: DomainIssue[] = [];
		const running = runEventStream({
			client: fake.client,
			onIssues: (next) => {
				issues = next;
			},
			onSessions: (next) => {
				sessions = next;
			},
			signal: abort.signal,
		});

		try {
			await settleUntil(
				() =>
					sessions.get(AGENT_ID)?.state === AgentSessionState.WORKING &&
					issues.some((row) => row.id === issue.id),
			);

			expect(sessions.get(AGENT_ID)).toMatchObject({
				sessionId: "session-replay",
				state: AgentSessionState.WORKING,
			});
			expect(issues.map((row) => row.id)).toEqual([issue.id]);
			expect(issues[0]?.title).toBe("Replay board");
		} finally {
			abort.abort();
			await running;
		}
	});
});
