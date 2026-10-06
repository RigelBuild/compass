import { describe, expect, test } from "bun:test";
import {
	type AgentSessionFrame,
	AgentSessionFrameSchema,
	AgentSessionState,
	Code,
	CompassService,
	ConnectError,
	create,
	createCompassClient,
	createRouterTransport,
	SessionEventSchema,
} from "@compass/client";
import { runSessionTail, type SessionFrameUpdate } from "./session-tail";

// The tail driver against an in-memory CompassService: each subscribe serves the
// next scripted attempt, which either yields frames then ends, yields then holds
// open until abort, or throws.

type Attempt =
	| { frames: AgentSessionFrame[]; hold?: boolean }
	| { error: Error };

function ack(sessionId: string): AgentSessionFrame {
	return create(AgentSessionFrameSchema, { sessionId });
}

function textFrame(sessionId: string, text: string): AgentSessionFrame {
	return create(AgentSessionFrameSchema, {
		sessionId,
		event: create(SessionEventSchema, {
			eventId: `ev-${text}`,
			atUnixMs: 5n,
			event: { case: "assistantText", value: { text, messageId: "m" } },
		}),
	});
}

function stateFrame(
	sessionId: string,
	state: AgentSessionState,
): AgentSessionFrame {
	return create(AgentSessionFrameSchema, { sessionId, state });
}

function scripted(attempts: Attempt[]) {
	const requests: string[] = [];
	const transport = createRouterTransport(({ service }) => {
		service(CompassService, {
			subscribeAgentSession: async function* (req, ctx) {
				requests.push(req.sessionId);
				const attempt = attempts.shift() ?? { frames: [], hold: true };
				if ("error" in attempt) throw attempt.error;
				for (const frame of attempt.frames) yield frame;
				if (!attempt.hold) return;
				const { promise, resolve } = Promise.withResolvers<void>();
				ctx.signal.addEventListener("abort", () => resolve(), { once: true });
				await promise;
			},
		});
	});
	return { client: createCompassClient(transport), requests };
}

async function drainUntil(predicate: () => boolean): Promise<void> {
	for (let i = 0; i < 500 && !predicate(); i++) await Promise.resolve();
}

describe("runSessionTail", () => {
	test("skips the registration ack and hands on mapped events and states", async () => {
		const { client, requests } = scripted([
			{
				frames: [
					ack("s1"),
					textFrame("s1", "hello"),
					stateFrame("s1", AgentSessionState.WORKING),
				],
				hold: true,
			},
		]);
		const abort = new AbortController();
		const updates: SessionFrameUpdate[] = [];
		const run = runSessionTail({
			client,
			sessionId: "s1",
			onFrame: (u) => updates.push(u),
			signal: abort.signal,
		});
		await drainUntil(() => updates.length >= 2);
		abort.abort();
		await run;

		expect(requests).toEqual(["s1"]);
		expect(updates).toEqual([
			{
				event: {
					id: "ev-hello",
					atUnixMs: 5,
					kind: "assistant_text",
					messageId: "m",
					text: "hello",
				},
			},
			{ state: AgentSessionState.WORKING },
		]);
	});

	test("a terminal state ends the tail without resubscribing", async () => {
		const { client, requests } = scripted([
			{
				frames: [
					ack("s1"),
					textFrame("s1", "bye"),
					stateFrame("s1", AgentSessionState.STOPPED),
				],
			},
		]);
		const updates: SessionFrameUpdate[] = [];
		await runSessionTail({
			client,
			sessionId: "s1",
			onFrame: (u) => updates.push(u),
		});
		expect(requests).toEqual(["s1"]);
		expect(updates.at(-1)).toEqual({ state: AgentSessionState.STOPPED });
	});

	test("a non-terminal clean end after progress resubscribes", async () => {
		const { client, requests } = scripted([
			{ frames: [ack("s1"), textFrame("s1", "a")] },
			{ frames: [ack("s1"), textFrame("s1", "b")], hold: true },
		]);
		const abort = new AbortController();
		const texts: string[] = [];
		const run = runSessionTail({
			client,
			sessionId: "s1",
			onFrame: (u) => {
				if (u.event?.kind === "assistant_text") texts.push(u.event.text);
			},
			signal: abort.signal,
		});
		await drainUntil(() => texts.length >= 2);
		abort.abort();
		await run;
		expect(texts).toEqual(["a", "b"]);
		expect(requests).toEqual(["s1", "s1"]);
	});

	test("NotFound is final: reported once, never resubscribed", async () => {
		const { client, requests } = scripted([
			{ error: new ConnectError("no such session", Code.NotFound) },
		]);
		const errors: unknown[] = [];
		await runSessionTail({
			client,
			sessionId: "s1",
			onFrame: () => {},
			onError: (e) => errors.push(e),
		});
		expect(requests).toEqual(["s1"]);
		expect(errors.length).toBe(1);
		expect(ConnectError.from(errors[0]).code).toBe(Code.NotFound);
	});

	test("another error is reported and retried after backoff", async () => {
		const { client, requests } = scripted([
			{ error: new ConnectError("down", Code.Unavailable) },
		]);
		const abort = new AbortController();
		const errors: unknown[] = [];
		const run = runSessionTail({
			client,
			sessionId: "s1",
			onFrame: () => {},
			// Abort during the backoff wait so the test never waits it out.
			onError: (e) => {
				errors.push(e);
				queueMicrotask(() => abort.abort());
			},
			signal: abort.signal,
		});
		await run;
		expect(errors.length).toBe(1);
		expect(requests).toEqual(["s1"]);
	});
});
