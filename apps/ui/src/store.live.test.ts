import { describe, expect, test } from "bun:test";
import {
	AccountSchema,
	AgentAccountSchema,
	AgentPresence,
	create,
	RosterEntrySchema,
} from "@compass/client";
import { createRoot, flush } from "solid-js";
import {
	wireChannel as buildWireChannel,
	wireTextMessage as buildWireTextMessage,
	createFakeComms,
	type FakeComms,
	wireAccount,
	wireAskMessage,
} from "./live/comms-fake";
import { createFakeCompass } from "./live/compass-fake";
import type { AgentSession } from "./session-events";
import { STUB_SESSION_EVENTS } from "./session-events-stub";
import { type AppStore, createAppStore } from "./store";
import { STUB_DAEMON } from "./stub-data";
import { testQueryClient } from "./test-support";

// The store's LIVE comms path: `createAppStore({ comms })` runs the
// SubscribeComms driver and mirrors each reduced CommsState into the four comms
// accessors, and the two comms writes (postMessage / answerAsk) issue real RPCs.
// double (live/comms-fake.ts) — no network, no timers — and defend the contracts
// that make the dogfood loop correct:
//
//   - a snapshot + a streamed MessagePosted land in the public accessors,
//   - a REDELIVERED post does not duplicate (the at-least-once guard the whole
//     no-optimistic-update design rests on),
//   - postMessage issues a PostMessage carrying a clientRequestId and inserts
//     NOTHING locally (the stream echo is what renders it),
//   - answerAsk (and every recorder) keeps answers LOCAL and sends nothing;
//     submitAsk is the ONE send path, issuing exactly one RespondToAsk (the ask
//     is answerable once, server-side), and a refused respond restages the
//     staged answers rather than rolling them back.
//
// The reduction itself (dedup, ordering, wire→domain adaptation) is covered in
// live/comms-state.test.ts and live/adapt.test.ts; here the subject is the
// STORE — that its accessors observe the stream and its actions reach the wire.

const CALLER = "acc-me";
const CHANNEL = "chan-1";

// Channel/message builders bound to this suite's caller + channel, so each test
// names only what it asserts on.
const wireChannel = (id: string) => buildWireChannel(id, CALLER);
const TOPIC = "top-1";
const wireTextMessage = (id: string, atUnixMs: number, text: string) =>
	buildWireTextMessage({
		id,
		topicId: TOPIC,
		authorAccountId: CALLER,
		atUnixMs,
		text,
	});
// A TWO-question ask: the wire is atomic (one AskQuestionAnswer per question),
// so a one-question fixture could not tell "sent every question" from "sent
// only the clicked one".
const askMessage = (id: string, askId: string) =>
	wireAskMessage({
		id,
		topicId: TOPIC,
		authorAccountId: CALLER,
		askId,
		questionIds: ["q-1", "q-2"],
	});
// A ONE-question ask: completeness is reached by its only click, so this is the
// fixture that pins "gating did not regress the single-question ask".
const singleQuestionAskMessage = (id: string, askId: string) =>
	wireAskMessage({
		id,
		topicId: TOPIC,
		authorAccountId: CALLER,
		askId,
		questionIds: ["q-only"],
	});

// The chosen option ids of one question of one ask, read out of the store's
// reactive message list — the public observation of what the LOCAL record says.
const chosenIn = (
	store: AppStore,
	messageId: string,
	askId: string,
	questionId: string,
): string[] | undefined => {
	const msg = store.messages().find((m) => m.id === messageId);
	for (const b of msg?.blocks ?? []) {
		if (b.kind !== "ask" || b.ask.askId !== askId) continue;
		const q = b.ask.questions.find((q) => q.questionId === questionId);
		if (q) return [...q.chosenOptionIds];
	}
	return undefined;
};
// The staged custom text of one question — the public observation of what the
// local free-text draft holds.
const customTextIn = (
	store: AppStore,
	messageId: string,
	askId: string,
	questionId: string,
): string | undefined => {
	const msg = store.messages().find((m) => m.id === messageId);
	for (const b of msg?.blocks ?? []) {
		if (b.kind !== "ask" || b.ask.askId !== askId) continue;
		const q = b.ask.questions.find((q) => q.questionId === questionId);
		if (q) return q.customText;
	}
	return undefined;
};

/** Build a live store over the fake inside a reactive root, run the async body,
 *  then close the stream and dispose. The store's stream boot is async (the
 *  driver awaits the snapshot read RPCs), so the body awaits `settled()` before
 *  asserting on the first snapshot. `onCommsError` is optional — pass it to
 *  observe the errors a write path routes out (the refused-respond test). */
async function withLiveStore(
	fake: FakeComms,
	body: (store: AppStore, settled: () => Promise<void>) => Promise<void>,
	onCommsError?: (error: unknown) => void,
): Promise<void> {
	let dispose!: () => void;
	const store = createRoot((d) => {
		dispose = d;
		return createAppStore({
			queryClient: testQueryClient(),
			comms: fake.client,
			callerId: CALLER,
			onCommsError,
		});
	});
	// Drain the microtask queue far enough for the driver's snapshot round-trip
	// (subscribe → boundary → three list calls + one per channel → onState) to
	// complete. Every hop is a resolved promise, so a bounded drain is
	// deterministic — no timers, no wall-clock wait.
	const settled = async () => {
		for (let i = 0; i < 20; i++) await Promise.resolve();
		flush();
	};
	try {
		await settled();
		await body(store, settled);
	} finally {
		fake.close();
		dispose();
	}
}

describe("store live read path", () => {
	// The whole point of the swap: the store's public accessors are fed by the
	// stream. After the snapshot the four collections hold the server's rows, and
	// a streamed MessagePosted lands in messages() — with NO component or
	// accessor change (the shape is the pre-existing seam). Mutation-check: a
	// store that kept its own seed, or dropped the onState wiring, reddens both
	// legs.
	test("reduces a snapshot and a streamed MessagePosted into the accessors", async () => {
		const fake = createFakeComms({
			accounts: [wireAccount(CALLER), wireAccount("acc-compass-ui")],
			channels: [wireChannel(CHANNEL)],
			messagesByChannel: {
				[CHANNEL]: [wireTextMessage("m-snap", 100, "from the snapshot")],
			},
		});

		await withLiveStore(fake, async (store, settled) => {
			// (a) the snapshot populated every accessor.
			expect(store.accounts().map((a) => a.id)).toEqual([
				CALLER,
				"acc-compass-ui",
			]);
			expect(store.channels().map((c) => c.id)).toEqual([CHANNEL]);
			expect(store.messages().map((m) => m.id)).toEqual(["m-snap"]);
			// …and the selection settled on the (subscribed) snapshot channel with
			// no hardcoded id.
			expect(store.selectedChannelId()).toBe(CHANNEL);
			expect(store.selectedChannel()?.id).toBe(CHANNEL);

			// (b) a tail MessagePosted appends through the reducer.
			await fake.emit(
				{
					case: "messagePosted",
					value: { message: wireTextMessage("m-tail", 200, "from the tail") },
				},
				1n,
			);
			await settled();

			expect(store.messages().map((m) => m.id)).toEqual(["m-snap", "m-tail"]);
		});
	});

	// The at-least-once guard, at the STORE level: the same MessagePosted
	// delivered twice (a reconnect replay, or the PostMessage response racing its
	// own stream echo) must render ONE row. This is what makes the
	// no-optimistic-update design correct — the post path relies on exactly this.
	test("a redelivered MessagePosted does not duplicate the message", async () => {
		const fake = createFakeComms({
			accounts: [wireAccount(CALLER)],
			channels: [wireChannel(CHANNEL)],
		});

		await withLiveStore(fake, async (store, settled) => {
			const posted = wireTextMessage("m-echo", 300, "hello");
			await fake.emit(
				{ case: "messagePosted", value: { message: posted } },
				1n,
			);
			await settled();
			expect(store.messages().map((m) => m.id)).toEqual(["m-echo"]);

			// The SAME message id again — the redelivery.
			await fake.emit(
				{ case: "messagePosted", value: { message: posted } },
				2n,
			);
			await settled();

			expect(store.messages().map((m) => m.id)).toEqual(["m-echo"]);
		});
	});
});

describe("store live write path", () => {
	// postMessage issues a real PostMessage to the named channel with the typed
	// text as a single text block, the topic oneof (here an existing topic by id)
	// and a non-empty clientRequestId (the server's idempotency key). And — the
	// architecture ruling — it inserts NOTHING locally: messages() is unchanged
	// until the stream echoes the stored message back.
	test("postMessage issues a PostMessage with a clientRequestId and does not insert locally", async () => {
		const fake = createFakeComms({
			accounts: [wireAccount(CALLER)],
			channels: [wireChannel(CHANNEL)],
		});

		await withLiveStore(fake, async (store, settled) => {
			expect(store.messages()).toEqual([]);

			await store.postMessage(
				CHANNEL,
				{ case: "topicId", value: TOPIC },
				"a live post",
			);
			await settled();

			expect(fake.posts.length).toBe(1);
			const [post] = fake.posts;
			expect(post.channelId).toBe(CHANNEL);
			expect(post.text).toBe("a live post");
			expect(post.topic).toEqual({ case: "topicId", value: TOPIC });
			expect(post.clientRequestId.length).toBeGreaterThan(0);

			// No local insert — the echo is what renders it.
			expect(store.messages()).toEqual([]);

			// …and the echo renders it exactly once.
			await fake.emit(
				{
					case: "messagePosted",
					value: { message: wireTextMessage("m-stored", 400, "a live post") },
				},
				1n,
			);
			await settled();
			expect(store.messages().map((m) => m.id)).toEqual(["m-stored"]);
		});
	});

	// Two posts carry DISTINCT idempotency keys: the key dedups a RETRY of one
	// post, so reusing it across two genuine posts would make the server swallow
	// the second. A constant or a reset counter reddens this.
	test("each post carries a fresh clientRequestId", async () => {
		const fake = createFakeComms({
			accounts: [wireAccount(CALLER)],
			channels: [wireChannel(CHANNEL)],
		});

		await withLiveStore(fake, async (store) => {
			await store.postMessage(
				CHANNEL,
				{ case: "topicId", value: TOPIC },
				"first",
			);
			await store.postMessage(
				CHANNEL,
				{ case: "topicId", value: TOPIC },
				"second",
			);

			expect(fake.posts.length).toBe(2);
			expect(fake.posts[0].clientRequestId).not.toBe(
				fake.posts[1].clientRequestId,
			);
		});
	});

	// A "new topic" post carries the topic oneof as `topicName` (get-or-create is
	// server-side) rather than a topicId. Same no-local-insert contract.
	test("postMessage with a topicName starts a new topic and does not insert locally", async () => {
		const fake = createFakeComms({
			accounts: [wireAccount(CALLER)],
			channels: [wireChannel(CHANNEL)],
		});

		await withLiveStore(fake, async (store) => {
			await store.postMessage(
				CHANNEL,
				{ case: "topicName", value: "deploy plan" },
				"first message in a new topic",
			);

			expect(fake.posts.length).toBe(1);
			expect(fake.posts[0].topic).toEqual({
				case: "topicName",
				value: "deploy plan",
			});
			expect(fake.posts[0].text).toBe("first message in a new topic");
			// No minted local message — the echo renders it.
			expect(store.messages()).toEqual([]);
		});
	});

	// A failed post REJECTS to its caller (the composer restores the user's
	// text from this) rather than being swallowed into an error accessor.
	test("a failed post rejects to the caller", async () => {
		const fake = createFakeComms({
			accounts: [wireAccount(CALLER)],
			channels: [wireChannel(CHANNEL)],
		});

		await withLiveStore(fake, async (store) => {
			fake.failNextPost(new Error("server said no"));

			await expect(
				store.postMessage(CHANNEL, { case: "topicId", value: TOPIC }, "doomed"),
			).rejects.toThrow("server said no");
		});
	});

	// Matt's ruling: recording never sends. Both clicks of a TWO-question ask
	// stay LOCAL (askResponses empty); only the explicit submitAsk issues the
	// one RespondToAsk carrying every question's answer. Mutation-check: an
	// auto-sending recorder reddens an empty leg; a partial mapper drops q-2.
	test("answerAsk never sends; submitAsk ships the full respond", async () => {
		const fake = createFakeComms({
			accounts: [wireAccount(CALLER)],
			channels: [wireChannel(CHANNEL)],
			messagesByChannel: {
				[CHANNEL]: [askMessage("m-ask", "ask-1")],
			},
		});

		await withLiveStore(fake, async (store, settled) => {
			// (a) the first click of a TWO-question ask records locally …
			store.answerAsk("m-ask", "ask-1", "q-1", "q-1-a");
			await settled();
			expect(chosenIn(store, "m-ask", "ask-1", "q-1")).toEqual(["q-1-a"]);
			// … and sends NOTHING: recording never reaches the wire.
			expect(fake.askResponses).toEqual([]);

			// (b) the completing click also records locally and sends nothing.
			store.answerAsk("m-ask", "ask-1", "q-2", "q-2-b");
			await settled();
			expect(fake.askResponses).toEqual([]);

			// (c) only the explicit submit issues one complete respond.
			store.submitAsk("m-ask", "ask-1");
			await settled();
			expect(fake.askResponses).toEqual([
				{
					askId: "ask-1",
					answers: [
						{ questionId: "q-1", chosenOptionIds: ["q-1-a"], customText: "" },
						{ questionId: "q-2", chosenOptionIds: ["q-2-b"], customText: "" },
					],
				},
			]);
		});
	});

	// A one-question ask is the shape that used to send on its own click: now the
	// only click sends NOTHING, and submitAsk ships it. Mutation-check: a
	// recorder that auto-sends reddens the empty leg.
	test("a single-question ask's only click sends nothing; submitAsk ships it", async () => {
		const fake = createFakeComms({
			accounts: [wireAccount(CALLER)],
			channels: [wireChannel(CHANNEL)],
			messagesByChannel: {
				[CHANNEL]: [singleQuestionAskMessage("m-one", "ask-one")],
			},
		});

		await withLiveStore(fake, async (store, settled) => {
			store.answerAsk("m-one", "ask-one", "q-only", "q-only-a");
			await settled();
			expect(fake.askResponses).toEqual([]);

			store.submitAsk("m-one", "ask-one");
			await settled();
			expect(fake.askResponses).toEqual([
				{
					askId: "ask-one",
					answers: [
						{
							questionId: "q-only",
							chosenOptionIds: ["q-only-a"],
							customText: "",
						},
					],
				},
			]);
		});
	});

	// The skip affordance: a user who wants to SKIP a question would never
	// complete the ask, so submitAsk sends what is answered and an EMPTY
	// chosenOptionIds for the skipped one (the wire permits it — the atomic
	// contract is coverage of every question, not a non-empty answer each).
	// Mutation-check: a submitAsk that omitted the unanswered question drops the
	// q-2 entry; one that refused to send an incomplete ask sends nothing.
	test("submitAsk sends an incomplete ask with the skipped question empty", async () => {
		const fake = createFakeComms({
			accounts: [wireAccount(CALLER)],
			channels: [wireChannel(CHANNEL)],
			messagesByChannel: {
				[CHANNEL]: [askMessage("m-ask", "ask-1")],
			},
		});

		await withLiveStore(fake, async (store, settled) => {
			store.answerAsk("m-ask", "ask-1", "q-1", "q-1-a");
			await settled();
			expect(fake.askResponses).toEqual([]);

			store.submitAsk("m-ask", "ask-1");
			await settled();

			expect(fake.askResponses).toEqual([
				{
					askId: "ask-1",
					answers: [
						{ questionId: "q-1", chosenOptionIds: ["q-1-a"], customText: "" },
						{ questionId: "q-2", chosenOptionIds: [], customText: "" },
					],
				},
			]);
		});
	});

	// At most ONE respond per ask. Two clicks record; submitAsk sends and marks
	// the ask submitted; every further path — a later answer, a second submit —
	// is inert. Mutation-check: dropping the submitted-ask guard fires a second
	// respond (which the double rejects, as the server does).
	test("a completed ask takes no further answer and issues no second respond", async () => {
		const fake = createFakeComms({
			accounts: [wireAccount(CALLER)],
			channels: [wireChannel(CHANNEL)],
			messagesByChannel: {
				[CHANNEL]: [askMessage("m-ask", "ask-1")],
			},
		});

		await withLiveStore(fake, async (store, settled) => {
			store.answerAsk("m-ask", "ask-1", "q-1", "q-1-a");
			store.answerAsk("m-ask", "ask-1", "q-2", "q-2-a");
			await settled();
			// Recorded, not sent: the ask is not submitted until the explicit gesture.
			expect(fake.askResponses).toEqual([]);
			expect(store.isAskSubmitted("ask-1")).toBe(false);

			store.submitAsk("m-ask", "ask-1");
			await settled();
			expect(fake.askResponses.length).toBe(1);
			expect(store.isAskSubmitted("ask-1")).toBe(true);

			// Every further path is inert: another answer, and a submit.
			store.answerAsk("m-ask", "ask-1", "q-2", "q-2-b");
			store.submitAsk("m-ask", "ask-1");
			await settled();

			expect(fake.askResponses.length).toBe(1);
			expect(chosenIn(store, "m-ask", "ask-1", "q-2")).toEqual(["q-2-a"]);
		});
	});

	// A REFUSED respond restages the staged answer rather than rolling it back:
	// submit records nothing, so the local answer the user staged stays honest
	// and retryable. Mutation-check: a catch that dropped the staged answer
	// reddens the survives-the-refusal leg; forgetting to unmark reddens retry.
	test("a refused RespondToAsk leaves the staged answer in place and stays retryable", async () => {
		const fake = createFakeComms({
			accounts: [wireAccount(CALLER)],
			channels: [wireChannel(CHANNEL)],
			messagesByChannel: {
				[CHANNEL]: [singleQuestionAskMessage("m-one", "ask-one")],
			},
		});
		const errors: unknown[] = [];

		await withLiveStore(
			fake,
			async (store, settled) => {
				fake.failNextAskResponse(new Error("server refused the ask"));

				store.answerAsk("m-one", "ask-one", "q-only", "q-only-a");
				await settled();
				store.submitAsk("m-one", "ask-one");
				await settled();

				// The refusal surfaced …
				expect((errors[0] as Error).message).toBe("server refused the ask");
				// … the staged answer stays put — submit recorded nothing to undo …
				expect(chosenIn(store, "m-one", "ask-one", "q-only")).toEqual([
					"q-only-a",
				]);
				// … and the ask is not burnt: it can be submitted again.
				expect(store.isAskSubmitted("ask-one")).toBe(false);

				store.submitAsk("m-one", "ask-one");
				await settled();

				expect(fake.askResponses).toEqual([
					{
						askId: "ask-one",
						answers: [
							{
								questionId: "q-only",
								chosenOptionIds: ["q-only-a"],
								customText: "",
							},
						],
					},
				]);
			},
			(error) => errors.push(error),
		);
	});

	// The restage is CONDITIONAL: a messageUpdated push landing between submit
	// and the refusal carries the AUTHORITATIVE server value (another
	// participant's accepted answer), and `!current.answered` declines the
	// restage. Mutation-check: dropping that guard reddens the survives leg.
	test("a refused respond does not clobber an ask the stream moved meanwhile", async () => {
		const fake = createFakeComms({
			accounts: [wireAccount(CALLER)],
			channels: [wireChannel(CHANNEL)],
			messagesByChannel: {
				[CHANNEL]: [singleQuestionAskMessage("m-one", "ask-one")],
			},
		});
		const errors: unknown[] = [];

		await withLiveStore(
			fake,
			async (store, settled) => {
				// (1) record the answer, then submit — the respond is HELD in flight.
				const gate = fake.holdNextAskResponse();
				store.answerAsk("m-one", "ask-one", "q-only", "q-only-a");
				await settled();
				store.submitAsk("m-one", "ask-one");
				await settled();
				expect(chosenIn(store, "m-one", "ask-one", "q-only")).toEqual([
					"q-only-a",
				]);

				// (2) a push carrying another participant's ACCEPTED answer.
				await fake.emit(
					{
						case: "messageUpdated",
						value: {
							message: wireAskMessage({
								id: "m-one",
								topicId: TOPIC,
								authorAccountId: CALLER,
								askId: "ask-one",
								questionIds: ["q-only"],
								chosen: { "q-only": ["q-only-b"] },
							}),
						},
					},
					1n,
				);
				await settled();
				expect(chosenIn(store, "m-one", "ask-one", "q-only")).toEqual([
					"q-only-b",
				]);

				// (3) only now is our respond refused.
				gate.reject(new Error("server refused the ask"));
				await settled();

				// The refusal surfaced, and the ask stays retryable …
				expect((errors[0] as Error).message).toBe("server refused the ask");
				expect(store.isAskSubmitted("ask-one")).toBe(false);
				// … but the SERVER's value survives: no stale restore.
				expect(chosenIn(store, "m-one", "ask-one", "q-only")).toEqual([
					"q-only-b",
				]);
			},
			(error) => errors.push(error),
		);
	});

	// First-responder-wins holds ACROSS the wire: a second single-select answer
	// is a local no-op, so submit ships the FIRST answer. Mutation-check:
	// dropping the "was it recorded?" guard ships q-1-b.
	test("a rejected single-select answer does not reach the wire", async () => {
		const fake = createFakeComms({
			accounts: [wireAccount(CALLER)],
			channels: [wireChannel(CHANNEL)],
			messagesByChannel: {
				[CHANNEL]: [askMessage("m-ask", "ask-1")],
			},
		});

		await withLiveStore(fake, async (store, settled) => {
			store.answerAsk("m-ask", "ask-1", "q-1", "q-1-a");
			store.answerAsk("m-ask", "ask-1", "q-1", "q-1-b");
			store.answerAsk("m-ask", "ask-1", "q-2", "q-2-a");
			await settled();
			store.submitAsk("m-ask", "ask-1");
			await settled();

			expect(fake.askResponses.length).toBe(1);
			expect(fake.askResponses[0].answers[0].chosenOptionIds).toEqual([
				"q-1-a",
			]);
		});
	});

	// A miss on any coordinate records nothing locally; with recording no longer
	// a send path the wire assertion is now vacuous, kept as records-nothing
	// coverage. Mutation-check: a guard that recorded a miss reddens chosenIn.
	test("an unknown ask coordinate sends no RespondToAsk", async () => {
		const fake = createFakeComms({
			accounts: [wireAccount(CALLER)],
			channels: [wireChannel(CHANNEL)],
			messagesByChannel: {
				[CHANNEL]: [askMessage("m-ask", "ask-1")],
			},
		});

		await withLiveStore(fake, async (store, settled) => {
			store.answerAsk("m-nope", "ask-1", "q-1", "q-1-a");
			store.answerAsk("m-ask", "ask-nope", "q-1", "q-1-a");
			store.answerAsk("m-ask", "ask-1", "q-nope", "q-1-a");
			store.answerAsk("m-ask", "ask-1", "q-1", "q-1-nope");
			await settled();

			// Nothing recorded locally, so a later submit has nothing to send.
			expect(chosenIn(store, "m-ask", "ask-1", "q-1")).toEqual([]);
			store.submitAsk("m-ask", "ask-1");
			await settled();
			expect(fake.askResponses).toEqual([]);
		});
	});

	// The design's central safety claim: typing that COMPLETES an ask still
	// sends nothing; only submitAsk ships, carrying the trimmed text. Mutation-
	// check: a text recorder that sent reddens the empty leg; dropping the trim
	// at the seam reddens the shipped value.
	test("typing that completes an ask sends nothing; submitAsk ships the trimmed text", async () => {
		const fake = createFakeComms({
			accounts: [wireAccount(CALLER)],
			channels: [wireChannel(CHANNEL)],
			messagesByChannel: {
				[CHANNEL]: [
					wireAskMessage({
						id: "m-ask",
						topicId: TOPIC,
						authorAccountId: CALLER,
						askId: "ask-1",
						questionIds: ["q-1", "q-free"],
						freeText: ["q-free"],
					}),
				],
			},
		});

		await withLiveStore(fake, async (store, settled) => {
			store.answerAsk("m-ask", "ask-1", "q-1", "q-1-a");
			store.answerAskText("m-ask", "ask-1", "q-free", "  ship it  ");
			await settled();
			// The ask is now complete, yet recording never reaches the wire.
			expect(fake.askResponses).toEqual([]);

			store.submitAsk("m-ask", "ask-1");
			await settled();
			expect(fake.askResponses).toEqual([
				{
					askId: "ask-1",
					answers: [
						{ questionId: "q-1", chosenOptionIds: ["q-1-a"], customText: "" },
						{
							questionId: "q-free",
							chosenOptionIds: [],
							customText: "ship it",
						},
					],
				},
			]);
		});
	});

	// Whitespace-only text is a skip: the trim seam ships customText "", the
	// accepted-skip shape. Mutation-check: shipping the raw draft reddens the
	// empty customText.
	test("whitespace-only typed text ships as an empty custom text", async () => {
		const fake = createFakeComms({
			accounts: [wireAccount(CALLER)],
			channels: [wireChannel(CHANNEL)],
			messagesByChannel: {
				[CHANNEL]: [
					wireAskMessage({
						id: "m-ask",
						topicId: TOPIC,
						authorAccountId: CALLER,
						askId: "ask-1",
						questionIds: ["q-1", "q-free"],
						freeText: ["q-free"],
					}),
				],
			},
		});

		await withLiveStore(fake, async (store, settled) => {
			store.answerAsk("m-ask", "ask-1", "q-1", "q-1-a");
			store.answerAskText("m-ask", "ask-1", "q-free", "   ");
			await settled();
			store.submitAsk("m-ask", "ask-1");
			await settled();
			expect(fake.askResponses[0].answers).toEqual([
				{ questionId: "q-1", chosenOptionIds: ["q-1-a"], customText: "" },
				{ questionId: "q-free", chosenOptionIds: [], customText: "" },
			]);
		});
	});

	// Single-select exclusivity, both directions. A pick clears any staged
	// draft, so submit ships the id with customText ""; and once picked, a later
	// answerAskText is a no-op. The pair (id + non-empty text) is what the server
	// refuses, burning the one respond. Mutation-check: dropping the click-side
	// clear reddens the cleared-draft leg; dropping the text-side gate reddens the
	// no-op leg.
	test("a single-select pick clears the draft, and text after a pick is a no-op", async () => {
		const fake = createFakeComms({
			accounts: [wireAccount(CALLER)],
			channels: [wireChannel(CHANNEL)],
			messagesByChannel: {
				[CHANNEL]: [askMessage("m-ask", "ask-1")],
			},
		});

		await withLiveStore(fake, async (store, settled) => {
			// Type a draft on the single-select q-1, then pick an option.
			store.answerAskText("m-ask", "ask-1", "q-1", "my own answer");
			await settled();
			expect(customTextIn(store, "m-ask", "ask-1", "q-1")).toBe(
				"my own answer",
			);
			store.answerAsk("m-ask", "ask-1", "q-1", "q-1-a");
			await settled();
			// The pick cleared the draft — the staged state never holds both.
			expect(customTextIn(store, "m-ask", "ask-1", "q-1")).toBe("");
			expect(chosenIn(store, "m-ask", "ask-1", "q-1")).toEqual(["q-1-a"]);

			// The other direction: text after the pick is refused.
			store.answerAskText("m-ask", "ask-1", "q-1", "sneak it back");
			await settled();
			expect(customTextIn(store, "m-ask", "ask-1", "q-1")).toBe("");

			store.submitAsk("m-ask", "ask-1");
			await settled();
			expect(fake.askResponses[0].answers[0]).toEqual({
				questionId: "q-1",
				chosenOptionIds: ["q-1-a"],
				customText: "",
			});
		});
	});

	// allowMultiple carries both: an option and typed text coexist and ship
	// together in one answer entry, which the server accepts. Mutation-check: a
	// send seam that blanked text whenever an option was present reddens the
	// carries-both leg.
	test("a multi-select ships both the chosen id and the trimmed text", async () => {
		const fake = createFakeComms({
			accounts: [wireAccount(CALLER)],
			channels: [wireChannel(CHANNEL)],
			messagesByChannel: {
				[CHANNEL]: [
					wireAskMessage({
						id: "m-ask",
						topicId: TOPIC,
						authorAccountId: CALLER,
						askId: "ask-1",
						questionIds: ["q-multi"],
						multi: ["q-multi"],
					}),
				],
			},
		});

		await withLiveStore(fake, async (store, settled) => {
			store.answerAsk("m-ask", "ask-1", "q-multi", "q-multi-a");
			store.answerAskText("m-ask", "ask-1", "q-multi", "  and this  ");
			await settled();
			// Both survive on the staged copy — the toggle did not clear the text.
			expect(chosenIn(store, "m-ask", "ask-1", "q-multi")).toEqual([
				"q-multi-a",
			]);
			expect(customTextIn(store, "m-ask", "ask-1", "q-multi")).toBe(
				"  and this  ",
			);

			store.submitAsk("m-ask", "ask-1");
			await settled();
			expect(fake.askResponses[0].answers[0]).toEqual({
				questionId: "q-multi",
				chosenOptionIds: ["q-multi-a"],
				customText: "and this",
			});
		});
	});

	// answerAskText no-op gates, beside the answerAsk gate coverage: a settled
	// single-select (a chosen option already answered it), a submitted ask, and a
	// server-closed ask each refuse a recorded draft. Mutation-check: dropping any
	// gate lets the matching leg record text.
	test("answerAskText no-ops on a settled, submitted, or closed ask", async () => {
		const fake = createFakeComms({
			accounts: [wireAccount(CALLER)],
			channels: [wireChannel(CHANNEL)],
			messagesByChannel: {
				[CHANNEL]: [
					askMessage("m-ask", "ask-1"),
					singleQuestionAskMessage("m-one", "ask-one"),
				],
			},
		});

		await withLiveStore(fake, async (store, settled) => {
			// (a) settled single-select: a pick answered q-1, so text is refused.
			store.answerAsk("m-ask", "ask-1", "q-1", "q-1-a");
			await settled();
			store.answerAskText("m-ask", "ask-1", "q-1", "too late");
			await settled();
			expect(customTextIn(store, "m-ask", "ask-1", "q-1")).toBe("");

			// (b) submitted ask: q-2 has no pick, but the ask is in flight.
			store.submitAsk("m-ask", "ask-1");
			await settled();
			expect(store.isAskSubmitted("ask-1")).toBe(true);
			store.answerAskText("m-ask", "ask-1", "q-2", "after submit");
			await settled();
			expect(customTextIn(store, "m-ask", "ask-1", "q-2")).toBe("");

			// (c) server-closed ask: a push closes ask-one, so text is refused.
			await fake.emit(
				{
					case: "messageUpdated",
					value: {
						message: wireAskMessage({
							id: "m-one",
							topicId: TOPIC,
							authorAccountId: CALLER,
							askId: "ask-one",
							questionIds: ["q-only"],
							answered: true,
						}),
					},
				},
				1n,
			);
			await settled();
			store.answerAskText("m-one", "ask-one", "q-only", "closed");
			await settled();
			expect(customTextIn(store, "m-one", "ask-one", "q-only")).toBe("");
		});
	});
});

describe("store stream lifetime", () => {
	// The stream is owned by the store's reactive owner: disposing it aborts the
	// subscription, so a torn-down store leaves no live SubscribeComms call
	// against the server. index.tsx's root is never disposed (the app-lifetime
	// singleton), but a leaked stream per store is exactly the bug that bites a
	// future multi-window shell, and a test root depends on it too.
	//
	// Asserted on the FAKE, not on the store's accessors: a disposed root's memos
	// stop recomputing, so reading them post-dispose proves nothing about whether
	// the subscription is still open. Mutation-check: dropping the onCleanup
	// abort leaves `isStreaming()` true.
	test("disposing the owner ends the SubscribeComms stream", async () => {
		const fake = createFakeComms({
			accounts: [wireAccount(CALLER)],
			channels: [wireChannel(CHANNEL)],
		});
		let dispose!: () => void;
		createRoot((d) => {
			dispose = d;
			return createAppStore({
				comms: fake.client,
				callerId: CALLER,
				queryClient: testQueryClient(),
			});
		});
		const settled = async () => {
			for (let i = 0; i < 20; i++) await Promise.resolve();
		};
		try {
			await settled();
			expect(fake.isStreaming()).toBe(true);

			dispose();
			await settled();

			expect(fake.isStreaming()).toBe(false);
		} finally {
			fake.close();
		}
	});
});

describe("store offline construction", () => {
	// The unit-testability contract: constructing WITHOUT a client must not
	// throw, must not dial anything, and must leave the comms surface empty
	// rather than half-populated. Every other suite depends on this.
	test("a store built with no client has empty comms and a null selection", () => {
		createRoot((dispose) => {
			const store = createAppStore({ queryClient: testQueryClient() });
			try {
				expect(store.accounts()).toEqual([]);
				expect(store.channels()).toEqual([]);
				expect(store.messages()).toEqual([]);
				expect(store.selectedChannelId()).toBeNull();
				expect(store.selectedChannel()).toBeUndefined();
			} finally {
				dispose();
			}
		});
	});

	// …and its write actions reject rather than silently no-op, so an offline
	// composer surfaces the failure instead of appearing to have posted.
	test("posting on an offline store rejects", async () => {
		let dispose!: () => void;
		const store = createRoot((d) => {
			dispose = d;
			return createAppStore({ queryClient: testQueryClient() });
		});
		try {
			await expect(
				store.postMessage("chan-x", { case: "topicId", value: TOPIC }, "hi"),
			).rejects.toThrow(/no comms client/);
		} finally {
			dispose();
		}
	});
});

describe("store stopAgent (StopAgentSession)", () => {
	// The workspace's Stop control is the one non-observational agent action, and
	// it is CompassClient-backed (not comms): StopAgentSession takes exactly one
	// field, the server-minted `session_id` (compass_pb.ts:831-836), so the store
	// must issue it for the OBSERVED session — `agentSession()`, keyed off the
	// selected agent — never for an account id.
	//
	// The RPC is Runner-backed: a server built with no RunnerHub answers
	// `Unavailable` (go/server/service.go:152-154). That is a real condition on
	// the socket-only path, so the refusal must reach `onCommsError` rather than
	// vanish — a silently-dead Stop button is the failure mode these pin.
	const runningAgentId = (): string => {
		const entry = Object.values(STUB_SESSION_EVENTS).find((s) => s.running);
		if (!entry) throw new Error("no running session in the fixture");
		return entry.agentAccountId;
	};
	// A server-minted session, shaped the way the runner actually mints one
	// (`"sess-" + <n>` via monotonicIDs, go/internal/runner/host.go:322-331) and
	// WITHOUT the `fixture` marker — the only kind of session the store may put
	// on the wire. The real-path tests below drive this, not the fixture, so
	// they keep defending the live request after the fixture guard lands.
	const serverSession = (agentId: string): AgentSession => ({
		sessionId: "sess-7",
		agentAccountId: agentId,
		running: true,
		events: [],
	});

	// THE GUARD. The store's session source is still the hand-written fixture
	// (STUB_SESSION_EVENTS), whose ids were never minted by a server. Issuing
	// StopAgentSession for one is worse than a no-op: the server's
	// unknown-session path is idempotent-success (go/internal/runner/host.go:
	// 217-228), so the call returns OK, stops nothing, and never reaches
	// onCommsError — a control inert in the one way indistinguishable from
	// working. The store must refuse locally and SAY so.
	test("a fixture-sourced session issues no stop and reports through onCommsError", async () => {
		const compass = createFakeCompass();
		const errors: unknown[] = [];
		let dispose!: () => void;
		const store = createRoot((d) => {
			dispose = d;
			return createAppStore({
				queryClient: testQueryClient(),
				compass: compass.client,
				onCommsError: (error) => errors.push(error),
			});
		});
		try {
			store.openAgent(runningAgentId());
			flush();
			expect(store.agentSession()?.fixture).toBe(true);

			await store.stopAgent();

			expect(compass.stops).toEqual([]); // nothing went on the wire
			expect(errors.length).toBe(1);
			expect(String(errors[0])).toMatch(/fixture data/);
		} finally {
			dispose();
		}
	});

	// THE REAL PATH: a server-sourced session (no `fixture` marker) still dials
	// StopAgentSession with exactly its server-minted id.
	test("issues StopAgentSession for the selected agent's session id", async () => {
		const compass = createFakeCompass();
		const agentId = runningAgentId();
		let dispose!: () => void;
		const store = createRoot((d) => {
			dispose = d;
			return createAppStore({
				queryClient: testQueryClient(),
				compass: compass.client,
				sessions: { [agentId]: serverSession(agentId) },
			});
		});
		try {
			store.openAgent(agentId);
			flush();
			const session = store.agentSession();
			expect(session).toBeDefined();
			expect(session?.fixture).toBeUndefined();

			await store.stopAgent();

			expect(compass.stops).toEqual([{ sessionId: session?.sessionId ?? "" }]);
		} finally {
			dispose();
		}
	});

	// No selection → no session → nothing to stop. The store must not invent a
	// session id (an empty-string Stop would be a wrong, live request).
	test("issues nothing when no agent is selected", async () => {
		const compass = createFakeCompass();
		let dispose!: () => void;
		const store = createRoot((d) => {
			dispose = d;
			return createAppStore({
				compass: compass.client,
				queryClient: testQueryClient(),
			});
		});
		try {
			await store.stopAgent();
			expect(compass.stops).toEqual([]);
		} finally {
			dispose();
		}
	});

	// A refused Stop (the Unavailable/no-RunnerHub path) reaches onCommsError.
	// Unlike a post there is no user text to preserve, so it does NOT reject to
	// the caller — but it is never swallowed either.
	test("a refused stop reaches onCommsError and does not reject", async () => {
		const compass = createFakeCompass();
		const errors: unknown[] = [];
		const agentId = runningAgentId();
		let dispose!: () => void;
		const store = createRoot((d) => {
			dispose = d;
			return createAppStore({
				queryClient: testQueryClient(),
				compass: compass.client,
				sessions: { [agentId]: serverSession(agentId) },
				onCommsError: (error) => errors.push(error),
			});
		});
		try {
			store.openAgent(agentId);
			flush();
			compass.failNextStop(
				new Error("[unavailable] compass: no runner hub attached"),
			);

			await store.stopAgent();

			expect(compass.stops.length).toBe(1); // it WAS issued
			expect(errors.length).toBe(1);
			expect(String(errors[0])).toMatch(/unavailable/);
		} finally {
			dispose();
		}
	});

	// Offline construction: no compass client at all. Stopping must not throw
	// (the store stays constructible and drivable with no network) and the
	// missing-client condition is surfaced through the same funnel, not dropped.
	test("an offline store does not throw and reports the missing client", async () => {
		const errors: unknown[] = [];
		const agentId = runningAgentId();
		let dispose!: () => void;
		const store = createRoot((d) => {
			dispose = d;
			return createAppStore({
				queryClient: testQueryClient(),
				sessions: { [agentId]: serverSession(agentId) },
				onCommsError: (error) => errors.push(error),
			});
		});
		try {
			store.openAgent(agentId);
			flush();
			await store.stopAgent();
			expect(errors.length).toBe(1);
			expect(String(errors[0])).toMatch(/no compass client/);
		} finally {
			dispose();
		}
	});

	// SURFACING. Routing to `onCommsError` alone puts a refusal in the console:
	// a refused Stop and a successful one are then observably identical (nothing
	// happens either way — `running` is fixture-sourced and never transitions).
	// `stopError` is the reactive hole the panel renders, the same shape
	// `askError` gives the ask block. Additive: onCommsError still fires.
	test("a refused stop records stopError for the panel to render", async () => {
		const compass = createFakeCompass();
		const errors: unknown[] = [];
		const agentId = runningAgentId();
		let dispose!: () => void;
		const store = createRoot((d) => {
			dispose = d;
			return createAppStore({
				queryClient: testQueryClient(),
				compass: compass.client,
				sessions: { [agentId]: serverSession(agentId) },
				onCommsError: (error) => errors.push(error),
			});
		});
		try {
			store.openAgent(agentId);
			flush();
			expect(store.stopError()).toBeUndefined();
			compass.failNextStop(
				new Error("[unavailable] compass: no runner hub attached"),
			);

			await store.stopAgent();

			expect(store.stopError()).toMatch(/unavailable/);
			expect(errors.length).toBe(1); // the console funnel is unchanged
		} finally {
			dispose();
		}
	});

	// The local refusals — fixture session and no client — are the DEFAULT on
	// today's socket-only path, so they must reach the same hole, not only the
	// console.
	test("the fixture and no-client refusals both record stopError", async () => {
		const compass = createFakeCompass();
		const agentId = runningAgentId();
		let disposeFixture!: () => void;
		const fixtureStore = createRoot((d) => {
			disposeFixture = d;
			return createAppStore({
				compass: compass.client,
				queryClient: testQueryClient(),
			});
		});
		try {
			fixtureStore.openAgent(agentId);
			flush();
			await fixtureStore.stopAgent();
			expect(fixtureStore.stopError()).toMatch(/fixture data/);
		} finally {
			disposeFixture();
		}

		let disposeOffline!: () => void;
		const offlineStore = createRoot((d) => {
			disposeOffline = d;
			return createAppStore({
				queryClient: testQueryClient(),
				sessions: { [agentId]: serverSession(agentId) },
			});
		});
		try {
			offlineStore.openAgent(agentId);
			flush();
			await offlineStore.stopAgent();
			expect(offlineStore.stopError()).toMatch(/no compass client/);
		} finally {
			disposeOffline();
		}
	});

	// A stale refusal must not outlive the retry that succeeded — Stop is
	// idempotent server-side, so retrying after a refusal is the expected move
	// and the panel must stop claiming the old failure.
	test("the next stop attempt clears the previous refusal", async () => {
		const compass = createFakeCompass();
		const agentId = runningAgentId();
		let dispose!: () => void;
		const store = createRoot((d) => {
			dispose = d;
			return createAppStore({
				queryClient: testQueryClient(),
				compass: compass.client,
				sessions: { [agentId]: serverSession(agentId) },
			});
		});
		try {
			store.openAgent(agentId);
			flush();
			compass.failNextStop(new Error("[unavailable] no runner hub"));
			await store.stopAgent();
			expect(store.stopError()).toBeDefined();

			await store.stopAgent(); // the retry the server accepts

			expect(store.stopError()).toBeUndefined();
			expect(compass.stops.length).toBe(2);
		} finally {
			dispose();
		}
	});
});

describe("daemon banner (live GetServerInfo)", () => {
	// The top-bar banner reads LIVE: a store built with `options.compass` fires a
	// one-shot GetServerInfo probe at boot and flips `daemon()` to the server's
	// liveness/version. An offline store keeps STUB_DAEMON (live:false), and a
	// probe rejection (server down / RPC error) must leave the banner offline and
	// route the error out — never gate boot, never surface a half-live banner.
	// The probe chains through a few async boundaries (probeServer awaits the
	// fake's async getServerInfo, then the store's .then/.catch runs), so flush
	// several microtasks to let the whole chain settle — no timers, no sleeps.
	const tick = async () => {
		for (let i = 0; i < 5; i++) await Promise.resolve();
	};

	// A successful probe surfaces the server's OWN version/apiVersion with
	// live:true. Mutation check: dropping live:true, or reading STUB_DAEMON's
	// version/apiVersion instead of the probe's, reddens this — the fixture uses
	// values (version 1.2.3, apiVersion compass.v2) distinct from STUB_DAEMON's
	// (0.1.0-dev / compass.v1), so each field is pinned to the probe as source.
	test("a successful probe surfaces the live server info", async () => {
		const compass = createFakeCompass();
		compass.serverInfo.version = "1.2.3";
		compass.serverInfo.apiVersion = "compass.v2";
		let dispose!: () => void;
		const store = createRoot((d) => {
			dispose = d;
			return createAppStore({
				compass: compass.client,
				queryClient: testQueryClient(),
			});
		});
		try {
			await tick();
			expect(store.daemon()).toEqual({
				version: "1.2.3",
				apiVersion: "compass.v2",
				live: true,
			});
		} finally {
			dispose();
		}
	});

	// No compass client → no probe → the banner stays on the stub fixture
	// exactly as before the wire. Mutation check: a probe that fired anyway (or
	// an initial value other than STUB_DAEMON) reddens this.
	test("an offline store keeps the stub banner", async () => {
		let dispose!: () => void;
		const store = createRoot((d) => {
			dispose = d;
			return createAppStore({ queryClient: testQueryClient() });
		});
		try {
			await tick();
			expect(store.daemon()).toEqual(STUB_DAEMON);
			expect(store.daemon().live).toBe(false);
		} finally {
			dispose();
		}
	});

	// A rejected probe (server down) leaves the banner offline and routes the
	// error to onCommsError — boot never throws, the error is never swallowed.
	// Mutation check: swallowing the rejection, or flipping live:true on failure,
	// reddens this.
	test("a rejected probe keeps the stub banner and routes onCommsError", async () => {
		const compass = createFakeCompass();
		const errors: unknown[] = [];
		compass.failNextProbe(new Error("[unavailable] server down"));
		let dispose!: () => void;
		const store = createRoot((d) => {
			dispose = d;
			return createAppStore({
				queryClient: testQueryClient(),
				compass: compass.client,
				onCommsError: (error) => errors.push(error),
			});
		});
		try {
			await tick();
			expect(store.daemon().live).toBe(false);
			expect(store.daemon()).toEqual(STUB_DAEMON);
			expect(errors.length).toBe(1);
			expect(String(errors[0])).toMatch(/unavailable/);
		} finally {
			dispose();
		}
	});
});

// An agent account on the wire — the durable identity `joinAgents` filters to
// and composes. Distinct from `wireAccount` (a user), because the join drops
// non-agent kinds.
const wireAgentAccount = (id: string) =>
	create(AccountSchema, {
		id,
		handle: id,
		displayName: id,
		kind: {
			case: "agent",
			value: create(AgentAccountSchema, { ownerUserId: CALLER }),
		},
	});

const rosterEntry = (agentAccountId: string, presence: AgentPresence) =>
	create(RosterEntrySchema, { agentAccountId, presence });

describe("store agents() live join (§T3)", () => {
	// The live seam: `agents()` joins the durable accounts (identity, from the
	// snapshot) with the presence map (lifecycle/activity, from GetRoster) via
	// joinAgents. A present roster entry projects its lifecycle; an agent account
	// with NO roster entry is a map miss → "stopped" (R2/DL-194), never the
	// components' false-live idle. Non-agent accounts (the caller) are filtered.
	test("joins live accounts + presence into agents(), miss→stopped", async () => {
		const fake = createFakeComms({
			accounts: [
				wireAccount(CALLER),
				wireAgentAccount("acc-compass-ui"),
				wireAgentAccount("acc-idle"),
			],
			channels: [wireChannel(CHANNEL)],
			roster: [rosterEntry("acc-compass-ui", AgentPresence.WORKING)],
		});

		await withLiveStore(fake, async (store) => {
			// Only the two agent accounts, in account order; the caller is filtered.
			expect(store.agents().map((a) => a.account.id)).toEqual([
				"acc-compass-ui",
				"acc-idle",
			]);
			// acc-compass-ui has a WORKING roster entry; acc-idle has none → the miss maps
			// to "stopped", NOT undefined/idle.
			expect(store.agentById("acc-compass-ui")?.lifecycle).toBe("working");
			expect(store.agentById("acc-idle")?.lifecycle).toBe("stopped");
		});
	});

	// The RIG-1645 reactivity the store.ts:790-795 comment owed: `agentById`
	// resolves through the reactive `agents()` memo, so a presence tick flips its
	// answer. A tail AgentPresenceChanged upserts acc-idle's lifecycle and the
	// seam re-resolves stopped→working with no accessor swap.
	test("agentById reacts to a presence change", async () => {
		const fake = createFakeComms({
			accounts: [wireAccount(CALLER), wireAgentAccount("acc-idle")],
			channels: [wireChannel(CHANNEL)],
			roster: [],
		});

		await withLiveStore(fake, async (store, settled) => {
			// No roster seed → the miss rule: the agent renders "stopped".
			expect(store.agentById("acc-idle")?.lifecycle).toBe("stopped");

			await fake.emit(
				{
					case: "agentPresenceChanged",
					value: {
						agentAccountId: "acc-idle",
						presence: AgentPresence.WORKING,
						activity: "now working",
					},
				},
				1n,
			);
			await settled();

			// The one seam re-resolved through the reactive agents() memo.
			expect(store.agentById("acc-idle")?.lifecycle).toBe("working");
			expect(store.agentById("acc-idle")?.activity).toBe("now working");
		});
	});

	// L3 (RIG-2100): a chat-message tail must NOT recompute the agents() join.
	// The memo depends only on accounts()/presence(); a MessagePosted touches
	// messages() with structural sharing, so accounts/presence keep identity and
	// agents() returns the SAME array reference. Reference equality is the point:
	// a message never re-joins the roster.
	test("a message does not re-join the roster (agents() keeps identity)", async () => {
		const fake = createFakeComms({
			accounts: [wireAccount(CALLER), wireAgentAccount("acc-compass-ui")],
			channels: [wireChannel(CHANNEL)],
			roster: [rosterEntry("acc-compass-ui", AgentPresence.WORKING)],
		});

		await withLiveStore(fake, async (store, settled) => {
			const before = store.agents();
			expect(before.map((a) => a.account.id)).toEqual(["acc-compass-ui"]);

			await fake.emit(
				{
					case: "messagePosted",
					value: { message: wireTextMessage("m-tail", 500, "a tail") },
				},
				1n,
			);
			await settled();

			// The memo did NOT recompute — same array reference.
			expect(store.agents()).toBe(before);
		});
	});
});
