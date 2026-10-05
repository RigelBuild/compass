import { afterEach, beforeEach, describe, expect, test } from "bun:test";
import { TourOutcome } from "@compass/client";
import { createRoot, createSignal, flush } from "solid-js";
import { STUB_COMMS_STATE } from "./comms-stub";
import {
	createFakeComms,
	type FakeComms,
	type FakeCommsSnapshot,
	wireAskMessage,
	wireChannel,
	wireTextMessage,
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

// A fake TourClient scripts the boot read and claim and records every write.

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
	/** Holds the boot read until it resolves. */
	readGate?: Promise<void>;
	/** Holds the claim until it resolves. */
	claimGate?: Promise<void>;
}): TourFake {
	const claims: string[] = [];
	const writes: { outcome: TourOutcome; stepId: string }[] = [];
	return {
		claims,
		writes,
		client: {
			getTourState: async () => {
				await script.readGate;
				if (script.getError) throw script.getError;
				return {
					outcome: script.outcome ?? TourOutcome.UNSPECIFIED,
					stepId: script.stepId ?? "",
				};
			},
			claimTourStart: async ({ stepId }) => {
				claims.push(stepId);
				await script.claimGate;
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

function gate(): { promise: Promise<void>; open: () => void } {
	const { promise, resolve } = Promise.withResolvers<void>();
	return { promise, open: () => resolve() };
}

// Drain the boot read + claim chain and the serialized write chain.
async function settle(): Promise<void> {
	for (let i = 0; i < 50; i++) {
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

interface Nav {
	readonly path: string;
	readonly replace: boolean;
}

// The store bound to a fake router that records push vs. replace.
function withRouter(
	initialPath: string,
	body: (store: AppStore, navs: Nav[]) => Promise<void> | void,
): Promise<void> {
	const navs: Nav[] = [];
	let dispose!: () => void;
	const store = createRoot((d) => {
		dispose = d;
		const [path, setPath] = createSignal(initialPath);
		const [state, setState] = createSignal<unknown>(undefined);
		const s = createAppStore({
			queryClient: testQueryClient(),
			initialComms: STUB_COMMS_STATE,
		});
		s.bindRouter({
			navigate: (to, opts) => {
				navs.push({ path: to, replace: opts?.replace ?? false });
				setState(opts?.state);
				setPath(to);
			},
			currentPath: path,
			currentState: state,
		});
		return s;
	});
	flush();
	// The layout → hash sync runs in a microtask; let the boot entry land.
	return settle()
		.then(() => body(store, navs))
		.finally(() => dispose());
}

const LAST = TOUR_STEPS.length - 1;
const stepId = (i: number): string => TOUR_STEPS[i]?.id ?? "";
const DEMO_AGENT = DEMO_AGENTS[0]?.account.id ?? "";
const wireText = (id: string) =>
	wireTextMessage({
		id,
		topicId: "top-1",
		authorAccountId: "acc-matt",
		atUnixMs: 5000,
		text: id,
	});

describe("tour first-run arming", () => {
	test("without claimFirstRun the boot reads resume state but never claims", async () => {
		const fake = tourFake({ claim: true, stepId: stepId(2) });
		await withStore({ tour: fake.client }, async (store) => {
			await settle();
			expect(fake.claims).toEqual([]);
			expect(store.tour.shouldAutoStart()).toBe(false);
			store.tour.start("resume");
			flush();
			expect(store.tour.stepIndex()).toBe(2);
		});
	});

	test("a claimed first run arms auto-start and start opens the tour", async () => {
		const fake = tourFake({ claim: true });
		await withStore(
			{ tour: fake.client, claimFirstRun: true },
			async (store) => {
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
			},
		);
	});

	test("a lost claim arms nothing and first-run start stays closed", async () => {
		const fake = tourFake({ claim: false });
		await withStore(
			{ tour: fake.client, claimFirstRun: true },
			async (store) => {
				await settle();
				expect(fake.claims.length).toBe(1);
				expect(store.tour.shouldAutoStart()).toBe(false);
				store.tour.start("first-run");
				flush();
				expect(store.tour.open()).toBe(false);
			},
		);
	});

	test("a failed claim arms nothing and is reported", async () => {
		const fake = tourFake({ claim: new Error("claim down") });
		const errors: unknown[] = [];
		await withStore(
			{
				tour: fake.client,
				claimFirstRun: true,
				onCommsError: (e) => errors.push(e),
			},
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
		await withStore(
			{ tour: fake.client, claimFirstRun: true },
			async (store) => {
				await settle();
				expect(fake.claims).toEqual([]);
				expect(store.tour.shouldAutoStart()).toBe(false);
				store.tour.start("resume");
				await settle();
				expect(store.tour.open()).toBe(true);
				expect(store.tour.stepIndex()).toBe(3);
				expect(fake.writes).toEqual([
					{ outcome: TourOutcome.STARTED, stepId: stepId(3) },
				]);
			},
		);
	});

	test("an offline store never arms", async () => {
		await withStore({ claimFirstRun: true }, async (store) => {
			await settle();
			expect(store.tour.shouldAutoStart()).toBe(false);
		});
	});

	test("a manual start while the claim is pending suppresses auto-start", async () => {
		const claim = gate();
		const fake = tourFake({ claim: true, claimGate: claim.promise });
		await withStore(
			{ tour: fake.client, claimFirstRun: true },
			async (store) => {
				await settle();
				expect(fake.claims.length).toBe(1);
				store.tour.start("replay");
				flush();
				store.tour.next();
				flush();
				claim.open();
				await settle();
				expect(store.tour.shouldAutoStart()).toBe(false);
				store.tour.start("first-run");
				flush();
				expect(store.tour.stepIndex()).toBe(1);
			},
		);
	});

	test("a slow boot read does not overwrite a cursor the user moved", async () => {
		const read = gate();
		const fake = tourFake({
			outcome: TourOutcome.DISMISSED,
			stepId: stepId(3),
			readGate: read.promise,
		});
		await withStore({ tour: fake.client }, async (store) => {
			store.tour.start("replay");
			flush();
			store.tour.next();
			flush();
			store.tour.close();
			flush();
			read.open();
			await settle();
			store.tour.start("resume");
			flush();
			expect(store.tour.stepIndex()).toBe(1);
		});
	});

	test("a resume started before the boot read opens at the saved step", async () => {
		const read = gate();
		const fake = tourFake({
			outcome: TourOutcome.DISMISSED,
			stepId: stepId(3),
			readGate: read.promise,
		});
		await withStore({ tour: fake.client }, async (store) => {
			store.tour.start("resume");
			flush();
			read.open();
			await settle();
			expect(store.tour.open()).toBe(true);
			expect(store.tour.stepIndex()).toBe(3);
			expect(fake.writes).toEqual([
				{ outcome: TourOutcome.STARTED, stepId: stepId(3) },
			]);
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
			await settle();
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
			await settle();
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
			await settle();
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
			await settle();
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

	test("a stale STARTED never lands after a later DISMISSED", async () => {
		// The server applies each write when it arrives; the fake releases the
		// newest pending write first, so unordered writes would land backwards.
		let stored: { outcome: TourOutcome; stepId: string } | undefined;
		const pending: (() => void)[] = [];
		const client: TourClient = {
			getTourState: async () => ({
				outcome: TourOutcome.DISMISSED,
				stepId: "",
			}),
			claimTourStart: async () => ({ claimed: false }),
			setTourState: (req) => {
				const { promise, resolve } = Promise.withResolvers<unknown>();
				pending.push(() => {
					stored = { outcome: req.outcome, stepId: req.stepId };
					resolve({});
				});
				return promise;
			},
		};
		await withStore({ tour: client }, async (store) => {
			await settle();
			store.tour.start("replay");
			flush();
			store.tour.next();
			flush();
			store.tour.dismiss();
			await settle();
			for (let release = pending.pop(); release; release = pending.pop()) {
				release();
				await settle();
			}
			expect(stored).toEqual({
				outcome: TourOutcome.DISMISSED,
				stepId: stepId(1),
			});
		});
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

	test("message-only updates keep the merged slices' identity", async () => {
		const comms = createFakeComms({
			channels: [wireChannel("chan-1", "acc-matt")],
		});
		await withStore({ comms: comms.client }, async (store) => {
			await settle();
			store.tour.start("replay");
			flush();
			const before = {
				agents: store.agents(),
				accounts: store.accounts(),
				channels: store.channels(),
				topics: store.topics(),
			};
			await comms.emit(
				{ case: "messagePosted", value: { message: wireText("m-new") } },
				1n,
			);
			await settle();
			expect(store.messages().some((m) => m.id === "m-new")).toBe(true);
			expect(store.agents()).toBe(before.agents);
			expect(store.accounts()).toBe(before.accounts);
			expect(store.channels()).toBe(before.channels);
			expect(store.topics()).toBe(before.topics);
		});
	});

	test("a live update keeps a selected demo channel", async () => {
		const comms = createFakeComms({
			channels: [wireChannel("chan-1", "acc-matt")],
		});
		const demoChannel = DEMO_CHANNELS[0]?.id ?? "";
		await withStore({ comms: comms.client }, async (store) => {
			await settle();
			store.tour.start("replay");
			flush();
			store.openChannel(demoChannel);
			flush();
			expect(store.selectedChannelId()).toBe(demoChannel);
			await comms.emit(
				{ case: "messagePosted", value: { message: wireText("m-new") } },
				1n,
			);
			await settle();
			expect(store.selectedChannelId()).toBe(demoChannel);
			expect(store.view()).toBe("channel");
		});
	});

	test("derived memos see demo rows: selectedAgent, prs and agentRepos", async () => {
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
			const owned = DEMO_ISSUES.filter((i) => i.assignee === DEMO_AGENT);
			expect(store.agentRepos()[0]?.branches).toEqual(
				owned.map((i) => i.branch),
			);
		});
	});

	test("without the tour a demo route redirects to the Bridge", async () => {
		await withStore({}, async (store) => {
			store.openAgent(DEMO_AGENT);
			flush();
			expect(store.view()).toBe("bridge");
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

	test("teardown off a demo route replaces the history entry", async () => {
		await withRouter("/", async (store, navs) => {
			// Drop the boot entry's view stamp; only the tour's moves matter here.
			navs.length = 0;
			store.tour.start("replay");
			flush();
			store.openAgent(DEMO_AGENT);
			await settle();
			store.tour.close();
			await settle();
			expect(navs).toEqual([
				{ path: `/agent/${DEMO_AGENT}`, replace: false },
				{ path: "/", replace: true },
			]);
			expect(store.view()).toBe("bridge");
		});
	});

	test("a demo route with the tour off replaces, so Back cannot loop", async () => {
		await withRouter(`/agent/${DEMO_AGENT}`, async (store, navs) => {
			expect(navs).toEqual([{ path: "/", replace: true }]);
			expect(store.view()).toBe("bridge");
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

	// One demo id per call, so each half of a guard is exercised on its own.
	test("postMessage to a demo channel with a real topic sends nothing", async () => {
		await withLive(async ({ store, comms }) => {
			await store
				.postMessage(DEMO_CHANNEL, { case: "topicId", value: "top-1" }, "hi")
				.catch(() => {});
			expect(comms.posts).toEqual([]);
		});
	});

	test("postMessage to a real channel with a demo topic sends nothing", async () => {
		await withLive(async ({ store, comms }) => {
			await store
				.postMessage("chan-real", { case: "topicId", value: DEMO_TOPIC }, "hi")
				.catch(() => {});
			expect(comms.posts).toEqual([]);
		});
	});

	// Asks served in a real channel, each naming exactly one `demo:` id. The
	// staged asks arrive with a recorded answer, so a recorder must leave it
	// as is and only submit's guard stands between it and the wire.
	const ASKS = [
		{
			label: "demo message",
			blank: { messageId: "demo:msg-a", askId: "ask-a" },
			staged: { messageId: "demo:msg-c", askId: "ask-c" },
		},
		{
			label: "demo ask",
			blank: { messageId: "msg-b", askId: "demo:ask-b" },
			staged: { messageId: "msg-d", askId: "demo:ask-d" },
		},
	] as const;
	const askSnapshot: FakeCommsSnapshot = {
		channels: [wireChannel("chan-1", "acc-matt")],
		messagesByChannel: {
			"chan-1": ASKS.flatMap(({ blank, staged }) => [
				wireAskMessage({
					id: blank.messageId,
					topicId: "top-1",
					authorAccountId: "acc-matt",
					askId: blank.askId,
					questionIds: ["q"],
				}),
				wireAskMessage({
					id: staged.messageId,
					topicId: "top-1",
					authorAccountId: "acc-matt",
					askId: staged.askId,
					questionIds: ["q"],
					freeText: ["q"],
					recordedText: { q: "staged" },
				}),
			]),
		},
	};
	const question = (store: AppStore, messageId: string) => {
		const block = store
			.messages()
			.find((m) => m.id === messageId)
			?.blocks.find((b) => b.kind === "ask");
		return block?.kind === "ask" ? block.ask.questions[0] : undefined;
	};

	for (const { label, blank, staged } of ASKS) {
		test(`answerAsk on a ${label} stages nothing`, async () => {
			await withLive(async ({ store }) => {
				expect(question(store, blank.messageId)?.chosenOptionIds).toEqual([]);
				store.answerAsk(blank.messageId, blank.askId, "q", "q-a");
				flush();
				expect(question(store, blank.messageId)?.chosenOptionIds).toEqual([]);
			}, askSnapshot);
		});

		test(`answerAskText on a ${label} keeps the staged answer`, async () => {
			await withLive(async ({ store }) => {
				expect(question(store, staged.messageId)?.customText).toBe("staged");
				store.answerAskText(staged.messageId, staged.askId, "q", "changed");
				flush();
				expect(question(store, staged.messageId)?.customText).toBe("staged");
			}, askSnapshot);
		});

		test(`submitAsk on a staged ${label} sends nothing`, async () => {
			await withLive(async ({ store, comms }) => {
				store.submitAsk(staged.messageId, staged.askId);
				await settle();
				expect(comms.askResponses).toEqual([]);
				expect(store.isAskSubmitted(staged.askId)).toBe(false);
			}, askSnapshot);
		});
	}

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
