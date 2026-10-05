import { afterEach, beforeEach, describe, expect, test } from "bun:test";
import { TourOutcome } from "@compass/client";
import { createRoot, flush } from "solid-js";
import { STUB_COMMS_STATE } from "./comms-stub";
import {
	createFakeComms,
	type FakeComms,
	type FakeCommsSnapshot,
	wireAskMessage,
	wireChannel,
} from "./live/comms-fake";
import { createFakeCompass, type FakeCompass } from "./live/compass-fake";
import {
	type AppStore,
	type AppStoreOptions,
	createAppStore,
	type TourClient,
} from "./store";
import { testQueryClient } from "./test-support";
import {
	DEMO_ACCOUNTS,
	DEMO_AGENTS,
	DEMO_CHANNELS,
	DEMO_ISSUES,
	DEMO_MESSAGES,
	DEMO_TOPICS,
} from "./tour/demo";
import { TOUR_STEPS } from "./tour/state";

// The tour controller and the demo read seam. A fake TourClient records every
// write and scripts the boot read + claim, so the tests can tell a claimed
// first run from a lost or failed one.

interface TourFake {
	readonly client: TourClient;
	readonly claims: string[];
	readonly writes: { outcome: TourOutcome; stepId: string }[];
}

function tourFake(script: {
	outcome?: TourOutcome;
	stepId?: string;
	claim?: boolean | Error;
	getError?: Error;
	setError?: Error;
}): TourFake {
	const claims: string[] = [];
	const writes: { outcome: TourOutcome; stepId: string }[] = [];
	return {
		claims,
		writes,
		client: {
			getTourState: async () => {
				if (script.getError) throw script.getError;
				return {
					outcome: script.outcome ?? TourOutcome.UNSPECIFIED,
					stepId: script.stepId ?? "",
				};
			},
			claimTourStart: async ({ stepId }) => {
				claims.push(stepId);
				if (script.claim instanceof Error) throw script.claim;
				return { claimed: script.claim ?? true };
			},
			setTourState: async (req) => {
				writes.push({ outcome: req.outcome, stepId: req.stepId });
				if (script.setError) throw script.setError;
				return {};
			},
		},
	};
}

// Drain the boot read + claim promise chain.
async function settle(): Promise<void> {
	for (let i = 0; i < 20; i++) {
		await Promise.resolve();
		flush();
	}
}

function withStore(
	opts: Partial<AppStoreOptions>,
	body: (store: AppStore) => Promise<void> | void,
): Promise<void> {
	let dispose!: () => void;
	const store = createRoot((d) => {
		dispose = d;
		return createAppStore({
			queryClient: testQueryClient(),
			initialComms: STUB_COMMS_STATE,
			...opts,
		});
	});
	return Promise.resolve(body(store)).finally(() => dispose());
}

const LAST = TOUR_STEPS.length - 1;
const stepId = (i: number): string => TOUR_STEPS[i]?.id ?? "";
const DEMO_AGENT = DEMO_AGENTS[0]?.account.id ?? "";

describe("tour first-run arming", () => {
	test("a claimed first run arms auto-start and start opens the tour", async () => {
		const fake = tourFake({ claim: true });
		await withStore({ tour: fake.client }, async (store) => {
			await settle();
			expect(fake.claims).toEqual([stepId(0)]);
			expect(store.tour.shouldAutoStart()).toBe(true);
			store.tour.start("first-run");
			flush();
			expect(store.tour.open()).toBe(true);
			expect(store.tour.stepIndex()).toBe(0);
			expect(store.tour.shouldAutoStart()).toBe(false);
			// The claim already wrote the started row.
			expect(fake.writes).toEqual([]);
		});
	});

	test("a lost claim arms nothing and first-run start stays closed", async () => {
		const fake = tourFake({ claim: false });
		await withStore({ tour: fake.client }, async (store) => {
			await settle();
			expect(fake.claims.length).toBe(1);
			expect(store.tour.shouldAutoStart()).toBe(false);
			store.tour.start("first-run");
			flush();
			expect(store.tour.open()).toBe(false);
		});
	});

	test("a failed claim arms nothing and is reported", async () => {
		const fake = tourFake({ claim: new Error("claim down") });
		const errors: unknown[] = [];
		await withStore(
			{ tour: fake.client, onCommsError: (e) => errors.push(e) },
			async (store) => {
				await settle();
				expect(store.tour.shouldAutoStart()).toBe(false);
				store.tour.start("first-run");
				flush();
				expect(store.tour.open()).toBe(false);
				expect(String(errors[0])).toMatch(/claim down/);
			},
		);
	});

	test("a stored outcome is read for resume and never claims", async () => {
		const fake = tourFake({
			outcome: TourOutcome.DISMISSED,
			stepId: stepId(3),
		});
		await withStore({ tour: fake.client }, async (store) => {
			await settle();
			expect(fake.claims).toEqual([]);
			expect(store.tour.shouldAutoStart()).toBe(false);
			store.tour.start("resume");
			flush();
			expect(store.tour.open()).toBe(true);
			expect(store.tour.stepIndex()).toBe(3);
			expect(fake.writes).toEqual([
				{ outcome: TourOutcome.STARTED, stepId: stepId(3) },
			]);
		});
	});

	test("an offline store never arms", async () => {
		await withStore({}, async (store) => {
			await settle();
			expect(store.tour.shouldAutoStart()).toBe(false);
		});
	});
});

describe("tour transitions", () => {
	test("step changes write the step id as STARTED", async () => {
		const fake = tourFake({ claim: false });
		await withStore({ tour: fake.client }, async (store) => {
			await settle();
			store.tour.start("replay");
			flush();
			store.tour.next();
			flush();
			store.tour.next();
			flush();
			store.tour.back();
			flush();
			expect(store.tour.stepIndex()).toBe(1);
			expect(fake.writes).toEqual([
				{ outcome: TourOutcome.STARTED, stepId: stepId(0) },
				{ outcome: TourOutcome.STARTED, stepId: stepId(1) },
				{ outcome: TourOutcome.STARTED, stepId: stepId(2) },
				{ outcome: TourOutcome.STARTED, stepId: stepId(1) },
			]);
		});
	});

	test("dismiss writes DISMISSED with the current step id and closes", async () => {
		const fake = tourFake({ claim: false });
		await withStore({ tour: fake.client }, async (store) => {
			await settle();
			store.tour.start("replay");
			flush();
			store.tour.next();
			flush();
			store.tour.dismiss();
			flush();
			expect(store.tour.open()).toBe(false);
			expect(store.tour.demoActive()).toBe(false);
			expect(fake.writes.at(-1)).toEqual({
				outcome: TourOutcome.DISMISSED,
				stepId: stepId(1),
			});
		});
	});

	test("complete writes COMPLETED and closes", async () => {
		const fake = tourFake({ claim: false });
		await withStore({ tour: fake.client }, async (store) => {
			await settle();
			store.tour.start("replay");
			flush();
			store.tour.complete();
			flush();
			expect(store.tour.open()).toBe(false);
			expect(store.tour.demoActive()).toBe(false);
			expect(fake.writes.at(-1)?.outcome).toBe(TourOutcome.COMPLETED);
		});
	});

	test("next past the last step completes", async () => {
		const fake = tourFake({ claim: false });
		await withStore({ tour: fake.client }, async (store) => {
			await settle();
			store.tour.start("replay");
			flush();
			for (let i = 0; i < LAST; i++) {
				store.tour.next();
				flush();
			}
			expect(store.tour.stepIndex()).toBe(LAST);
			expect(store.tour.open()).toBe(true);
			store.tour.next();
			flush();
			expect(store.tour.open()).toBe(false);
			expect(fake.writes.at(-1)?.outcome).toBe(TourOutcome.COMPLETED);
		});
	});

	test("close writes no outcome", async () => {
		const fake = tourFake({ claim: false });
		await withStore({ tour: fake.client }, async (store) => {
			await settle();
			store.tour.start("replay");
			flush();
			const before = fake.writes.length;
			store.tour.close();
			flush();
			expect(store.tour.open()).toBe(false);
			expect(store.tour.demoActive()).toBe(false);
			expect(fake.writes.length).toBe(before);
		});
	});

	test("a failed write is reported once, not retried", async () => {
		const fake = tourFake({ claim: false, setError: new Error("write down") });
		const errors: unknown[] = [];
		await withStore(
			{ tour: fake.client, onCommsError: (e) => errors.push(e) },
			async (store) => {
				await settle();
				store.tour.start("replay");
				await settle();
				expect(fake.writes.length).toBe(1);
				expect(errors.length).toBe(1);
				expect(store.tour.open()).toBe(true);
			},
		);
	});
});

describe("tour demo seam", () => {
	const ids = (rows: readonly { id: string }[]) => rows.map((r) => r.id);
	const agentIds = (store: AppStore) => store.agents().map((a) => a.account.id);

	test("demo rows appear in every base accessor only while active", async () => {
		await withStore({}, async (store) => {
			const has = (all: string[], demo: string[]) =>
				demo.every((id) => all.includes(id));
			const check = (expected: boolean) => {
				expect(has(ids(store.accounts()), ids(DEMO_ACCOUNTS))).toBe(expected);
				expect(
					has(
						agentIds(store),
						DEMO_AGENTS.map((a) => a.account.id),
					),
				).toBe(expected);
				expect(has(ids(store.issues()), ids(DEMO_ISSUES))).toBe(expected);
				expect(has(ids(store.channels()), ids(DEMO_CHANNELS))).toBe(expected);
				expect(has(ids(store.topics()), ids(DEMO_TOPICS))).toBe(expected);
				expect(has(ids(store.messages()), ids(DEMO_MESSAGES))).toBe(expected);
			};
			check(false);
			store.tour.start("replay");
			flush();
			expect(store.tour.demoActive()).toBe(true);
			check(true);
			store.tour.close();
			flush();
			check(false);
		});
	});

	test("real rows stay beside demo rows", async () => {
		await withStore({}, async (store) => {
			const realIssues = ids(store.issues());
			const realAgents = agentIds(store);
			store.tour.start("replay");
			flush();
			expect(ids(store.issues()).slice(0, realIssues.length)).toEqual(
				realIssues,
			);
			expect(agentIds(store).slice(0, realAgents.length)).toEqual(realAgents);
		});
	});

	test("the live roster joins demo agents without duplicating them", async () => {
		const comms = createFakeComms();
		await withStore({ comms: comms.client }, async (store) => {
			await settle();
			store.tour.start("replay");
			flush();
			const demo = agentIds(store).filter((id) => id.startsWith("demo:"));
			expect(demo).toEqual(DEMO_AGENTS.map((a) => a.account.id));
			expect(
				store.agents().find((a) => a.account.id === DEMO_AGENT)?.lifecycle,
			).toBe("working");
		});
	});

	test("derived memos see demo rows: selectedAgent and prs", async () => {
		await withStore({}, async (store) => {
			const demoPr = DEMO_ISSUES.find((i) => i.prs.length > 0);
			expect(demoPr).toBeDefined();
			expect(store.prs().some((r) => r.issue.id === demoPr?.id)).toBe(false);
			store.tour.start("replay");
			flush();
			expect(store.prs().some((r) => r.issue.id === demoPr?.id)).toBe(true);
			store.openAgent(DEMO_AGENT);
			flush();
			expect(store.selectedAgent()?.account.id).toBe(DEMO_AGENT);
			// The workspace anchors on the demo agent's own issue.
			expect(store.selectedIssue()?.assignee).toBe(DEMO_AGENT);
		});
	});

	test("without the tour a demo route resolves nothing", async () => {
		await withStore({}, async (store) => {
			store.openAgent(DEMO_AGENT);
			flush();
			expect(store.selectedAgent()).toBeUndefined();
		});
	});
});

describe("tour teardown leaves a demo route", () => {
	for (const exit of ["close", "dismiss", "complete"] as const) {
		test(`${exit} on /agent/demo:… lands on the Bridge`, async () => {
			await withStore({}, async (store) => {
				store.tour.start("replay");
				flush();
				store.openAgent(DEMO_AGENT);
				flush();
				expect(store.view()).toBe("agent");
				store.tour[exit]();
				flush();
				expect(store.view()).toBe("bridge");
				expect(store.tour.demoActive()).toBe(false);
			});
		});
	}

	test("teardown on a real route stays put", async () => {
		await withStore({}, async (store) => {
			store.tour.start("replay");
			flush();
			store.showBacklog();
			flush();
			store.tour.close();
			flush();
			expect(store.view()).toBe("backlog");
		});
	});
});

describe("demo targets never reach the server or storage", () => {
	const DEMO_CHANNEL = DEMO_CHANNELS[0]?.id ?? "";
	const DEMO_TOPIC = DEMO_TOPICS[0]?.id ?? "";
	const PIN_KEY = "compass.pinnedAgents.ws-demo";

	beforeEach(() => globalThis.localStorage.clear());
	afterEach(() => globalThis.localStorage.clear());

	const withLive = (
		body: (ctx: {
			store: AppStore;
			comms: FakeComms;
			compass: FakeCompass;
		}) => Promise<void>,
		snapshot?: FakeCommsSnapshot,
	) => {
		const comms = createFakeComms(snapshot);
		const compass = createFakeCompass();
		return withStore(
			{
				comms: comms.client,
				compass: compass.client,
				workspaceKey: "ws-demo",
				// A server-sourced session for the demo agent, so only the guard
				// stands between stopAgent and the wire.
				sessions: {
					[DEMO_AGENT]: {
						sessionId: "sess-1",
						agentAccountId: DEMO_AGENT,
						running: true,
						events: [],
					},
				},
			},
			async (store) => {
				await settle();
				store.tour.start("replay");
				flush();
				await body({ store, comms, compass });
			},
		);
	};

	test("postMessage to a demo channel or topic sends nothing", async () => {
		await withLive(async ({ store, comms }) => {
			await store
				.postMessage(DEMO_CHANNEL, { case: "topicId", value: DEMO_TOPIC }, "hi")
				.catch(() => {});
			await store
				.postMessage("chan-real", { case: "topicId", value: DEMO_TOPIC }, "hi")
				.catch(() => {});
			expect(comms.posts).toEqual([]);
		});
	});

	// A `demo:` ask served in a real channel: with the guard gone the recorders
	// would stage an answer and submit would send it.
	test("ask recorders and submit on a demo ask send nothing", async () => {
		const snapshot: FakeCommsSnapshot = {
			channels: [wireChannel("chan-1", "acc-matt")],
			messagesByChannel: {
				"chan-1": [
					wireAskMessage({
						id: "demo:msg-ask",
						topicId: "top-1",
						authorAccountId: "acc-matt",
						askId: "demo:ask",
						questionIds: ["q"],
					}),
				],
			},
		};
		await withLive(async ({ store, comms }) => {
			expect(store.messages().some((m) => m.id === "demo:msg-ask")).toBe(true);
			store.answerAsk("demo:msg-ask", "demo:ask", "q", "q-a");
			store.answerAskText("demo:msg-ask", "demo:ask", "q", "text");
			flush();
			store.submitAsk("demo:msg-ask", "demo:ask");
			await settle();
			expect(comms.askResponses).toEqual([]);
			expect(store.isAskSubmitted("demo:ask")).toBe(false);
		}, snapshot);
	});

	test("pin and unpin of a demo agent write nothing", async () => {
		await withLive(async ({ store }) => {
			store.pinAgent(DEMO_AGENT);
			flush();
			expect(store.isPinned(DEMO_AGENT)).toBe(false);
			store.unpinAgent(DEMO_AGENT);
			flush();
			expect(globalThis.localStorage.getItem(PIN_KEY)).toBeNull();
		});
	});

	test("stopAgent with a selected demo agent sends nothing", async () => {
		await withLive(async ({ store, compass }) => {
			store.openAgent(DEMO_AGENT);
			flush();
			expect(store.selectedAgentId()).toBe(DEMO_AGENT);
			await store.stopAgent();
			expect(compass.stops).toEqual([]);
		});
	});
});
