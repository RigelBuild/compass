import { describe, expect, test } from "bun:test";
import { mkdtempSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { SessionManager } from "@oh-my-pi/pi-coding-agent";
import { TurnSequence } from "./turn-sequence";

interface Entry {
	readonly type: "custom";
	readonly customType: string;
	readonly data: unknown;
}

class FakeSession {
	sessionId: string;
	entries: Entry[];

	constructor(sessionId: string, entries: Entry[] = []) {
		this.sessionId = sessionId;
		this.entries = entries;
	}

	getSessionId(): string {
		return this.sessionId;
	}

	getEntries(): Entry[] {
		return this.entries;
	}

	appendCustomEntry(customType: string, data?: unknown): string {
		this.entries.push({ type: "custom", customType, data });
		return String(this.entries.length);
	}
}

function turnSequence(session: FakeSession): TurnSequence {
	return new TurnSequence(session as unknown as SessionManager);
}

describe("TurnSequence", () => {
	test("increments once for each agent_start and retains the current turn", () => {
		const session = new FakeSession("session-1");
		const sequence = turnSequence(session);
		expect(sequence.current()).toBe(0n);
		expect(sequence.start()).toBe(1n);
		expect(sequence.current()).toBe(1n);
		expect(sequence.start()).toBe(2n);
		expect(sequence.start()).toBe(3n);
	});

	test("resumes after the highest persisted sequence in the same transcript", () => {
		const session = new FakeSession("session-1", [
			{
				type: "custom",
				customType: "compass_turn_sequence",
				data: { turnSequence: "4" },
			},
		]);
		expect(turnSequence(session).start()).toBe(5n);
	});

	test("never decreases across an in-place identity change", () => {
		const session = new FakeSession("first");
		const sequence = turnSequence(session);
		expect(sequence.start()).toBe(1n);
		expect(sequence.start()).toBe(2n);
		session.sessionId = "empty";
		session.entries = [];
		expect(sequence.start()).toBe(3n);
		session.sessionId = "stored";
		session.entries = [
			{
				type: "custom",
				customType: "compass_turn_sequence",
				data: { turnSequence: "8" },
			},
		];
		expect(sequence.start()).toBe(9n);
	});
	test("refuses a sequence beyond the signed 64-bit maximum", () => {
		const session = new FakeSession("session-1", [
			{
				type: "custom",
				customType: "compass_turn_sequence",
				data: { turnSequence: "9223372036854775807" },
			},
		]);
		expect(() => turnSequence(session).start()).toThrow("int64 maximum");
	});

	function assistantReply(): Parameters<SessionManager["appendMessage"]>[0] {
		const zero = { input: 0, output: 0, cacheRead: 0, cacheWrite: 0 };
		return {
			role: "assistant",
			content: [{ type: "text", text: "ok" }],
			api: "test",
			provider: "test",
			model: "test",
			usage: { ...zero, totalTokens: 0, cost: { ...zero, total: 0 } },
			stopReason: "stop",
			timestamp: Date.now(),
		} as Parameters<SessionManager["appendMessage"]>[0];
	}

	test("a reopened session file continues its persisted sequence", async () => {
		const cwd = mkdtempSync(join(tmpdir(), "compass-turnseq-"));
		try {
			const sessionDir = join(cwd, "sessions");
			const first = SessionManager.create(cwd, sessionDir);
			const sequence = new TurnSequence(first);
			sequence.start();
			sequence.start();
			// The SDK writes the file only once an assistant reply exists.
			first.appendMessage(assistantReply());
			const file = first.getSessionFile();
			if (!file) throw new Error("no session file");
			await first.flush();
			await first.close();

			const reopened = await SessionManager.open(file, sessionDir);
			try {
				expect(reopened.getSessionId()).toBe(first.getSessionId());
				expect(new TurnSequence(reopened).start()).toBe(3n);
			} finally {
				await reopened.close();
			}
		} finally {
			rmSync(cwd, { recursive: true, force: true });
		}
	});
});
