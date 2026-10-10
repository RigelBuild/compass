import { afterEach, beforeEach, describe, expect, test } from "bun:test";
import {
	AccountSchema,
	AgentAccountSchema,
	AgentSessionState,
	create,
} from "@compass/client";
import { createRoot, flush } from "solid-js";
import { createFakeComms, wireAccount } from "./live/comms-fake";
import { createFakeCompass } from "./live/compass-fake";
import { type AppStore, createAppStore } from "./store";
import { testQueryClient } from "./test-support";

const CALLER = "acc-matt";
const AGENT = "acc-compass-ui";
const WORKSPACE = "ws-lifecycle";
const STORAGE_KEY = `compass.lastOpened.${WORKSPACE}`;

function agentAccount(id: string) {
	return create(AccountSchema, {
		id,
		handle: id,
		displayName: id,
		kind: {
			case: "agent",
			value: create(AgentAccountSchema, { ownerUserId: CALLER }),
		},
	});
}

async function settle(): Promise<void> {
	for (let i = 0; i < 50; i++) await Promise.resolve();
	flush();
}

async function withStore(
	body: (
		store: AppStore,
		compass: ReturnType<typeof createFakeCompass>,
	) => Promise<void>,
): Promise<void> {
	const compass = createFakeCompass();
	const comms = createFakeComms({
		accounts: [wireAccount(CALLER), agentAccount(AGENT)],
	});
	let dispose!: () => void;
	const store = createRoot((d) => {
		dispose = d;
		return createAppStore({
			queryClient: testQueryClient(),
			comms: comms.client,
			compass: compass.client,
			callerId: CALLER,
			workspaceKey: WORKSPACE,
		});
	});
	try {
		await settle();
		await body(store, compass);
	} finally {
		comms.close();
		dispose();
	}
}

beforeEach(() => globalThis.localStorage.clear());
afterEach(() => globalThis.localStorage.clear());

describe("agent last-opened lifecycle", () => {
	test("openAgent records last-opened in workspace storage", async () => {
		await withStore(async (store) => {
			store.openAgent(AGENT);
			await settle();
			const opened = JSON.parse(
				globalThis.localStorage.getItem(STORAGE_KEY) ?? "{}",
			);
			expect(typeof opened[AGENT]).toBe("number");
			expect(opened[AGENT]).toBe(store.lastOpened().get(AGENT));
		});
	});

	test("dispatchLayout open records last-opened for the focused agent", async () => {
		await withStore(async (store) => {
			store.dispatchLayout({ kind: "open", path: `/agent/${AGENT}` });
			await settle();
			expect(
				JSON.parse(globalThis.localStorage.getItem(STORAGE_KEY) ?? "{}"),
			).toHaveProperty(AGENT);
		});
	});

	test("a background agent tab does not record last-opened", async () => {
		await withStore(async (store) => {
			store.dispatchLayout({
				kind: "open",
				path: `/agent/${AGENT}`,
				background: true,
			});
			await settle();
			expect(globalThis.localStorage.getItem(STORAGE_KEY)).toBeNull();
		});
	});

	test("a new store over the same storage keeps an opened turn idle", async () => {
		let openedAt = 0;
		await withStore(async (store) => {
			store.openAgent(AGENT);
			await settle();
			openedAt = store.lastOpened().get(AGENT) ?? 0;
			expect(openedAt).toBeGreaterThan(0);
		});

		await withStore(async (store, compass) => {
			compass.pushSessionStatus(
				AGENT,
				"sess-lifecycle",
				AgentSessionState.WORKING,
				openedAt - 1,
			);
			await settle();
			compass.pushSessionStatus(
				AGENT,
				"sess-lifecycle",
				AgentSessionState.READY,
				openedAt,
			);
			await settle();
			// Without the hydrated time the turn end would read as unopened.
			expect(store.lastOpened().get(AGENT)).toBe(openedAt);
			expect(store.agentById(AGENT)?.lifecycle).toBe("idle");
		});
	});

	test("a new turn end while the agent is focused stays idle", async () => {
		await withStore(async (store, compass) => {
			store.openAgent(AGENT);
			await settle();
			const turnEndedAt =
				(store.lastOpened().get(AGENT) ?? Date.now()) + 10_000;
			compass.pushSessionStatus(
				AGENT,
				"sess-live",
				AgentSessionState.WORKING,
				turnEndedAt - 1,
			);
			await settle();
			compass.pushSessionStatus(
				AGENT,
				"sess-live",
				AgentSessionState.READY,
				turnEndedAt,
			);
			await settle();
			expect(store.agentById(AGENT)?.lifecycle).toBe("idle");
			expect(store.lastOpened().get(AGENT)).toBe(turnEndedAt);
		});
	});

	test("a new turn end after focus leaves shows done", async () => {
		await withStore(async (store, compass) => {
			compass.pushSessionStatus(
				AGENT,
				"sess-live",
				AgentSessionState.WORKING,
				1,
			);
			await settle();
			store.openAgent(AGENT);
			await settle();
			const openedAt = store.lastOpened().get(AGENT) ?? 0;
			store.showBridge();
			await settle();
			compass.pushSessionStatus(
				AGENT,
				"sess-live",
				AgentSessionState.READY,
				openedAt + 10_000,
			);
			await settle();
			expect(store.agentById(AGENT)?.lifecycle).toBe("done");
		});
	});
});
