import { describe, expect, test } from "bun:test";
import {
	AgentSessionState,
	AgentToolCallStatus,
	create,
	SessionEventSchema,
	type SessionEvent as WireSessionEvent,
} from "@compass/client";
import { createRoot, flush } from "solid-js";
import { createFakeCompass, type FakeCompass } from "./live/compass-fake";
import { foldSession } from "./session-events";
import { STUB_SESSION_EVENTS } from "./session-events-stub";
import { type AppStore, createAppStore } from "./store";
import { testQueryClient } from "./test-support";

// The live Session Log: a store built with a compass client learns session ids
// from SubscribeEvents and tails SubscribeAgentSession for the agent on screen.
// Driven end to end through the fake's scripted streams.

// A real stub agent with no fixture session, so a hit can only come from the tail.
const AGENT = "acc-compass-comms";
const OTHER = "acc-compass-ui-bridge";

function text(id: string, value: string): WireSessionEvent {
	return create(SessionEventSchema, {
		eventId: id,
		atUnixMs: 1n,
		event: { case: "assistantText", value: { text: value, messageId: "m1" } },
	});
}

function toolCall(id: string): WireSessionEvent {
	return create(SessionEventSchema, {
		eventId: id,
		atUnixMs: 2n,
		event: {
			case: "toolCall",
			value: {
				toolCallId: "t1",
				title: "bun test",
				status: AgentToolCallStatus.IN_PROGRESS,
			},
		},
	});
}

async function settle(until: () => boolean = () => false): Promise<void> {
	for (let i = 0; i < 200 && !until(); i++) {
		await Promise.resolve();
		flush();
	}
}

function liveStore(fake: FakeCompass): {
	store: AppStore;
	dispose: () => void;
} {
	let dispose!: () => void;
	const store = createRoot((d) => {
		dispose = d;
		return createAppStore({
			queryClient: testQueryClient(),
			compass: fake.client,
		});
	});
	return { store, dispose };
}

describe("store live agent session (SubscribeAgentSession)", () => {
	test("the open agent's tail folds into its session trace", async () => {
		expect(STUB_SESSION_EVENTS[AGENT]).toBeUndefined();
		const fake = createFakeCompass();
		const { store, dispose } = liveStore(fake);
		try {
			fake.pushSessionStatus(AGENT, "sess-1", AgentSessionState.WORKING);
			await settle();
			store.openAgent(AGENT);
			await settle(() => fake.openSessionTails().includes("sess-1"));
			fake.pushSessionFrame("sess-1", { event: text("e1", "hello ") });
			fake.pushSessionFrame("sess-1", { event: text("e2", "world") });
			fake.pushSessionFrame("sess-1", { event: toolCall("e3") });
			await settle(() => (store.focusedView().agentSession()?.events.length ?? 0) >= 3);

			const session = store.focusedView().agentSession();
			expect(session?.sessionId).toBe("sess-1");
			expect(session?.agentAccountId).toBe(AGENT);
			expect(session?.running).toBe(true);
			expect(session?.fixture).toBeUndefined();
			expect(foldSession(session?.events ?? [])).toEqual([
				{ kind: "text", messageId: "m1", text: "hello world" },
				{
					kind: "tool",
					toolCallId: "t1",
					call: expect.objectContaining({ title: "bun test" }),
					status: "in_progress",
				},
			]);
		} finally {
			dispose();
		}
	});

	test("no tail opens for an agent that is not on screen", async () => {
		const fake = createFakeCompass();
		const { store, dispose } = liveStore(fake);
		try {
			fake.pushSessionStatus(AGENT, "sess-1", AgentSessionState.WORKING);
			fake.pushSessionStatus(OTHER, "sess-2", AgentSessionState.WORKING);
			await settle();
			expect(fake.sessionSubscribes).toEqual([]);
			store.openAgent(OTHER);
			await settle(() => fake.openSessionTails().length > 0);
			expect(fake.sessionSubscribes.map((s) => s.sessionId)).toEqual([
				"sess-2",
			]);
		} finally {
			dispose();
		}
	});

	test("navigating away aborts the tail; returning keeps what was seen", async () => {
		const fake = createFakeCompass();
		const { store, dispose } = liveStore(fake);
		try {
			fake.pushSessionStatus(AGENT, "sess-1", AgentSessionState.WORKING);
			await settle();
			store.openAgent(AGENT);
			await settle(() => fake.openSessionTails().includes("sess-1"));
			fake.pushSessionFrame("sess-1", { event: text("e1", "seen") });
			await settle(() => (store.focusedView().agentSession()?.events.length ?? 0) >= 1);

			store.showBacklog();
			await settle(() => fake.openSessionTails().length === 0);
			expect(fake.openSessionTails()).toEqual([]);

			store.openAgent(AGENT);
			await settle(() => fake.openSessionTails().includes("sess-1"));
			expect(store.focusedView().agentSession()?.events.map((e) => e.id)).toEqual(["e1"]);
			expect(fake.sessionSubscribes.length).toBe(2);
		} finally {
			dispose();
		}
	});

	test("a terminal state ends the tail and the session stops running", async () => {
		const fake = createFakeCompass();
		const { store, dispose } = liveStore(fake);
		try {
			fake.pushSessionStatus(AGENT, "sess-1", AgentSessionState.WORKING);
			await settle();
			store.openAgent(AGENT);
			await settle(() => fake.openSessionTails().includes("sess-1"));
			expect(store.focusedView().agentSession()?.running).toBe(true);

			fake.pushSessionFrame("sess-1", { state: AgentSessionState.STOPPED });
			await settle(() => store.focusedView().agentSession()?.running === false);
			expect(store.focusedView().agentSession()?.running).toBe(false);
			await settle(() => fake.openSessionTails().length === 0);
			expect(fake.openSessionTails()).toEqual([]);
		} finally {
			dispose();
		}
	});

	test("a new session id for the shown agent replaces the old tail", async () => {
		const fake = createFakeCompass();
		const { store, dispose } = liveStore(fake);
		try {
			fake.pushSessionStatus(AGENT, "sess-1", AgentSessionState.WORKING);
			await settle();
			store.openAgent(AGENT);
			await settle(() => fake.openSessionTails().includes("sess-1"));

			fake.pushSessionStatus(AGENT, "sess-2", AgentSessionState.STARTING);
			await settle(() => fake.openSessionTails().join() === "sess-2");
			expect(fake.openSessionTails()).toEqual(["sess-2"]);
			expect(store.focusedView().agentSession()?.sessionId).toBe("sess-2");
		} finally {
			dispose();
		}
	});

	test("disposing the store aborts the open tail", async () => {
		const fake = createFakeCompass();
		const { store, dispose } = liveStore(fake);
		fake.pushSessionStatus(AGENT, "sess-1", AgentSessionState.WORKING);
		await settle();
		store.openAgent(AGENT);
		await settle(() => fake.openSessionTails().includes("sess-1"));
		dispose();
		await settle(() => fake.openSessionTails().length === 0);
		expect(fake.openSessionTails()).toEqual([]);
	});
});
