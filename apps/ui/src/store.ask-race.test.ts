import { describe, expect, test } from "bun:test";
import { createRoot } from "solid-js";
import {
	wireChannel as buildWireChannel,
	createFakeComms,
	type FakeComms,
	wireAccount,
	wireAskMessage,
	wireTextMessage,
} from "./live/comms-fake";
import { type AppStore, createAppStore } from "./store";
import { testQueryClient } from "./test-support";

// The race between an ask being answered LOCALLY and the stream pushing a new state under it.
// Recording never sends (only submitAsk does), so staged choices live ONLY in local state and
// `adoptComms` replaces it wholesale, so a mid-ask push used to discard them. Subject is narrowly
// `adoptComms`; the send-model + restage contracts live in store.live.test.ts.

const CALLER = "acc-me";
const CHANNEL = "chan-1";
const TOPIC = "top-1";

const wireChannel = (id: string) => buildWireChannel(id, CALLER);
/** The ask as the SERVER holds it, with whatever answers it has recorded and
 *  whatever its spent-flag says — the payload both the snapshot and a
 *  `messageUpdated` push carry. `answered` is passed separately from `chosen`
 *  because the server records a CLOSED ask with no chosen ids at all in two
 *  shapes (a deliberate skip, a custom_text-only answer), and those shapes are
 *  the ones a chosen-ids scan cannot see. */
const askMessage = (
	questionIds: readonly string[],
	chosen?: Readonly<Record<string, readonly string[]>>,
	over?: {
		answered?: boolean;
		freeText?: readonly string[];
		optionIds?: Readonly<Record<string, readonly string[]>>;
		recordedText?: Readonly<Record<string, string>>;
		multi?: readonly string[];
	},
) =>
	wireAskMessage({
		id: "m-ask",
		topicId: TOPIC,
		authorAccountId: CALLER,
		askId: "ask-1",
		questionIds,
		chosen,
		answered: over?.answered,
		freeText: over?.freeText,
		optionIds: over?.optionIds,
		recordedText: over?.recordedText,
		multi: over?.multi,
	});

// The chosen option ids of one question, read out of the store's reactive
// message list — the public observation of what the LOCAL record says.
const chosenIn = (
	store: AppStore,
	questionId: string,
): string[] | undefined => {
	const msg = store.messages().find((m) => m.id === "m-ask");
	for (const b of msg?.blocks ?? []) {
		if (b.kind !== "ask" || b.ask.askId !== "ask-1") continue;
		const q = b.ask.questions.find((q) => q.questionId === questionId);
		if (q) return [...q.chosenOptionIds];
	}
	return undefined;
};
// The staged custom text of one question, read out of the store's reactive
// message list — the public observation of what the LOCAL draft holds.
const customTextIn = (
	store: AppStore,
	questionId: string,
): string | undefined => {
	const msg = store.messages().find((m) => m.id === "m-ask");
	for (const b of msg?.blocks ?? []) {
		if (b.kind !== "ask" || b.ask.askId !== "ask-1") continue;
		const q = b.ask.questions.find((q) => q.questionId === questionId);
		if (q) return q.customText;
	}
	return undefined;
};

// The ask itself as the store holds it — the whole block value, so a test can
// observe the pushed SHAPE (question ids, an option list) and the OBJECT the
// adoption settled on, neither of which the chosen ids can express.
const askIn = (store: AppStore) => {
	const msg = store.messages().find((m) => m.id === "m-ask");
	for (const b of msg?.blocks ?? []) {
		if (b.kind === "ask" && b.ask.askId === "ask-1") return b.ask;
	}
	return undefined;
};

/** Build a live store over the fake inside a reactive root, run the async body,
 *  then close the stream and dispose — the same harness shape as
 *  store.live.test.ts, so both suites describe one server the same way. */
async function withLiveStore(
	fake: FakeComms,
	body: (store: AppStore, settled: () => Promise<void>) => Promise<void>,
): Promise<void> {
	let dispose!: () => void;
	const store = createRoot((d) => {
		dispose = d;
		return createAppStore({
			comms: fake.client,
			callerId: CALLER,
			queryClient: testQueryClient(),
		});
	});
	// Every hop of the driver's snapshot round-trip is a resolved promise, so a
	// bounded microtask drain is deterministic — no timers, no wall-clock wait.
	const settled = async () => {
		for (let i = 0; i < 20; i++) await Promise.resolve();
	};
	try {
		await settled();
		await body(store, settled);
	} finally {
		fake.close();
		dispose();
	}
}

describe("adoptComms vs an in-progress ask", () => {
	// The gap the wire's atomicity opens: the first click on a two-question ask sends NOTHING,
	// so the answer exists only locally. A push re-stating the ask as the server still holds it
	// (unanswered — never told) must not take the click away. Mutation-check: wholesale
	// `setComms(next)` reddens the survives leg; a preserve that forgot to re-arm reddens completable.
	test("an unsubmitted local answer survives a stream push", async () => {
		const fake = createFakeComms({
			accounts: [wireAccount(CALLER)],
			channels: [wireChannel(CHANNEL)],
			messagesByChannel: { [CHANNEL]: [askMessage(["q-1", "q-2"])] },
		});

		await withLiveStore(fake, async (store, settled) => {
			// (1) the click: recorded locally, and — the gate — nothing shipped.
			store.answerAsk("m-ask", "ask-1", "q-1", "q-1-a");
			await settled();
			expect(chosenIn(store, "q-1")).toEqual(["q-1-a"]);
			expect(fake.askResponses).toEqual([]);

			// (2) a push carrying the ask exactly as the server holds it: still
			// unanswered, because the completing click has not happened.
			await fake.emit(
				{
					case: "messageUpdated",
					value: { message: askMessage(["q-1", "q-2"]) },
				},
				1n,
			);
			await settled();

			// The user's choice is still there …
			expect(chosenIn(store, "q-1")).toEqual(["q-1-a"]);
			expect(chosenIn(store, "q-2")).toEqual([]);

			// … and the ask is still live: submitting it ships BOTH answers, which
			// is only possible if the surviving answer is real state and not just a
			// rendered ghost.
			store.answerAsk("m-ask", "ask-1", "q-2", "q-2-a");
			await settled();
			store.submitAsk("m-ask", "ask-1");
			await settled();
			expect(fake.askResponses).toEqual([
				{
					askId: "ask-1",
					answers: [
						{ questionId: "q-1", chosenOptionIds: ["q-1-a"], customText: "" },
						{ questionId: "q-2", chosenOptionIds: ["q-2-a"], customText: "" },
					],
				},
			]);
		});
	});

	// The other end of the rule, the one the refusal rollback depends on: an ask the SERVER
	// has an opinion about takes the server's value, even while the user has an unsubmitted
	// local answer. Here another participant answered q-1 and the server closed the ask, so the
	// push carries the chosen id AND the spent flag. Mutation-check: preserving local reddens this.
	test("an authoritative server answer beats an unsubmitted local one", async () => {
		const fake = createFakeComms({
			accounts: [wireAccount(CALLER)],
			channels: [wireChannel(CHANNEL)],
			messagesByChannel: { [CHANNEL]: [askMessage(["q-1", "q-2"])] },
		});

		await withLiveStore(fake, async (store, settled) => {
			store.answerAsk("m-ask", "ask-1", "q-1", "q-1-a");
			await settled();
			expect(chosenIn(store, "q-1")).toEqual(["q-1-a"]);

			await fake.emit(
				{
					case: "messageUpdated",
					value: {
						message: askMessage(
							["q-1", "q-2"],
							{ "q-1": ["q-1-b"] },
							{ answered: true },
						),
					},
				},
				1n,
			);
			await settled();

			expect(chosenIn(store, "q-1")).toEqual(["q-1-b"]);
		});
	});

	// A SHIPPED ask is the server's, full stop. Once its one RespondToAsk is submitted the local
	// record is a claim about what the server was told, so a push replaces it even when the pushed
	// ask carries no answers yet. Keeping the local copy would re-break the conditional restage in
	// `sendAsk`. Mutation-check: dropping the submitted-ask gate reddens this.
	test("a shipped ask takes the pushed server value", async () => {
		const fake = createFakeComms({
			accounts: [wireAccount(CALLER)],
			channels: [wireChannel(CHANNEL)],
			messagesByChannel: { [CHANNEL]: [askMessage(["q-only"])] },
		});

		await withLiveStore(fake, async (store, settled) => {
			// Record then submit — the explicit gesture ships the one respond.
			store.answerAsk("m-ask", "ask-1", "q-only", "q-only-a");
			await settled();
			store.submitAsk("m-ask", "ask-1");
			await settled();
			expect(store.isAskSubmitted("ask-1")).toBe(true);
			expect(chosenIn(store, "q-only")).toEqual(["q-only-a"]);

			await fake.emit(
				{
					case: "messageUpdated",
					value: { message: askMessage(["q-only"]) },
				},
				1n,
			);
			await settled();

			expect(chosenIn(store, "q-only")).toEqual([]);
		});
	});

	// The shape that held this fix: a CLOSED ask with no chosen ids anywhere. A deliberate skip
	// is an ACCEPTED answer, so the server flips answered with nothing to see. Our unshipped
	// click must NOT be restored — it would offer a click the server refuses with ErrConflict.
	// Mutation-check: the old chosen-ids scan read this as "server said nothing" and clobbered it.
	test("a fully-skipped answered ask beats an unsubmitted local one", async () => {
		const fake = createFakeComms({
			accounts: [wireAccount(CALLER)],
			channels: [wireChannel(CHANNEL)],
			messagesByChannel: { [CHANNEL]: [askMessage(["q-1", "q-2"])] },
		});

		await withLiveStore(fake, async (store, settled) => {
			store.answerAsk("m-ask", "ask-1", "q-1", "q-1-a");
			await settled();
			expect(chosenIn(store, "q-1")).toEqual(["q-1-a"]);
			expect(fake.askResponses).toEqual([]);

			// The push: answered, and EVERY question's chosen ids empty.
			await fake.emit(
				{
					case: "messageUpdated",
					value: {
						message: askMessage(["q-1", "q-2"], undefined, {
							answered: true,
						}),
					},
				},
				1n,
			);
			await settled();

			// The server's closed ask wins: the local pick is gone.
			expect(chosenIn(store, "q-1")).toEqual([]);
			expect(chosenIn(store, "q-2")).toEqual([]);
		});
	});

	// The second defeating shape: a free-text question carries NO options, so it is answered by
	// custom_text alone and chosenOptionIds stays empty though the server closed the ask. Same
	// rule — only Ask.answered can see it. Mutation-check: reverting to the chosen-ids scan reddens this.
	test("a custom-text-only answered ask beats an unsubmitted local one", async () => {
		const fake = createFakeComms({
			accounts: [wireAccount(CALLER)],
			channels: [wireChannel(CHANNEL)],
			messagesByChannel: {
				[CHANNEL]: [askMessage(["q-1", "q-free"])],
			},
		});

		await withLiveStore(fake, async (store, settled) => {
			store.answerAsk("m-ask", "ask-1", "q-1", "q-1-a");
			await settled();
			expect(chosenIn(store, "q-1")).toEqual(["q-1-a"]);

			await fake.emit(
				{
					case: "messageUpdated",
					value: {
						message: askMessage(["q-1", "q-free"], undefined, {
							answered: true,
							freeText: ["q-free"],
						}),
					},
				},
				1n,
			);
			await settled();

			expect(chosenIn(store, "q-1")).toEqual([]);
			expect(chosenIn(store, "q-free")).toEqual([]);
		});
	});

	// The other side of the same flag, the original bug: `answered: false` with empty chosen ids
	// is the GENUINELY pending ask — the server was never told, so the user's unshipped click
	// must survive. This stops the new predicate from being read as "any push wins". Mutation-
	// check: a predicate that always reported "the server has a value" reddens this.
	test("an unanswered pushed ask still preserves the local answer", async () => {
		const fake = createFakeComms({
			accounts: [wireAccount(CALLER)],
			channels: [wireChannel(CHANNEL)],
			messagesByChannel: { [CHANNEL]: [askMessage(["q-1", "q-2"])] },
		});

		await withLiveStore(fake, async (store, settled) => {
			store.answerAsk("m-ask", "ask-1", "q-1", "q-1-a");
			await settled();

			await fake.emit(
				{
					case: "messageUpdated",
					value: {
						message: askMessage(["q-1", "q-2"], undefined, {
							answered: false,
						}),
					},
				},
				1n,
			);
			await settled();

			expect(chosenIn(store, "q-1")).toEqual(["q-1-a"]);

			// And it is real state, not a ghost: submitting the ask ships both.
			store.answerAsk("m-ask", "ask-1", "q-2", "q-2-a");
			await settled();
			store.submitAsk("m-ask", "ask-1");
			await settled();
			expect(fake.askResponses).toEqual([
				{
					askId: "ask-1",
					answers: [
						{ questionId: "q-1", chosenOptionIds: ["q-1-a"], customText: "" },
						{ questionId: "q-2", chosenOptionIds: ["q-2-a"], customText: "" },
					],
				},
			]);
		});
	});

	// The write gate, one half. Once reconciliation ADOPTS a server-closed ask, recording a click
	// would stage an answer the ask can never send — the server already spent its one respond.
	// Nothing says "submitted" here (the closing respond was someone else's), so the record is
	// refused where it lands. Mutation-check: dropping answerAsk's `answered` guard records the pick.
	test("a click on a server-closed ask ships nothing", async () => {
		const fake = createFakeComms({
			accounts: [wireAccount(CALLER)],
			channels: [wireChannel(CHANNEL)],
			messagesByChannel: { [CHANNEL]: [askMessage(["q-1"])] },
		});

		await withLiveStore(fake, async (store, settled) => {
			// The ask arrives closed with nothing recorded — the skip shape, which
			// only `answered` can see.
			await fake.emit(
				{
					case: "messageUpdated",
					value: {
						message: askMessage(["q-1"], undefined, { answered: true }),
					},
				},
				1n,
			);
			await settled();
			expect(store.isAskSubmitted("ask-1")).toBe(false);

			// A click here would record a pick the closed ask can never send; the
			// answered guard refuses it.
			store.answerAsk("m-ask", "ask-1", "q-1", "q-1-a");
			await settled();

			expect(fake.askResponses).toEqual([]);
			expect(chosenIn(store, "q-1")).toEqual([]);
		});
	});

	// The write gate, other half — the shape the user meets: another participant answered q-1,
	// so the server recorded their id AND closed the ask in one write. Judged by chosen ids alone
	// this looks partially answered, so `submitAsk` ships into a guaranteed ErrConflict; the
	// server's flag knows better. Mutation-check: gating `submitAsk` on `isAskSubmitted` reddens this.
	test("a submit on a server-closed ask ships nothing", async () => {
		const fake = createFakeComms({
			accounts: [wireAccount(CALLER)],
			channels: [wireChannel(CHANNEL)],
			messagesByChannel: { [CHANNEL]: [askMessage(["q-1", "q-2"])] },
		});

		await withLiveStore(fake, async (store, settled) => {
			await fake.emit(
				{
					case: "messageUpdated",
					value: {
						message: askMessage(
							["q-1", "q-2"],
							{ "q-1": ["q-1-b"] },
							{ answered: true },
						),
					},
				},
				1n,
			);
			await settled();
			expect(chosenIn(store, "q-1")).toEqual(["q-1-b"]);
			expect(store.isAskSubmitted("ask-1")).toBe(false);

			store.submitAsk("m-ask", "ask-1");
			await settled();

			expect(fake.askResponses).toEqual([]);
		});
	});

	// An ask whose QUESTIONS moved is a different ask: nothing to line the local answers up
	// against, so the pushed shape is adopted whole. Here the server grew a third question under
	// an in-progress answer. Mutation-check: dropping the `sameQuestions` clause reddens this —
	// the two-question local ask is restored over the three-question pushed one, losing a question.
	test("a pushed ask that grew a question beats the local shape", async () => {
		const fake = createFakeComms({
			accounts: [wireAccount(CALLER)],
			channels: [wireChannel(CHANNEL)],
			messagesByChannel: { [CHANNEL]: [askMessage(["q-1", "q-2"])] },
		});

		await withLiveStore(fake, async (store, settled) => {
			store.answerAsk("m-ask", "ask-1", "q-1", "q-1-a");
			await settled();
			expect(chosenIn(store, "q-1")).toEqual(["q-1-a"]);

			await fake.emit(
				{
					case: "messageUpdated",
					value: { message: askMessage(["q-1", "q-2", "q-3"]) },
				},
				1n,
			);
			await settled();

			expect(askIn(store)?.questions.map((q) => q.questionId)).toEqual([
				"q-1",
				"q-2",
				"q-3",
			]);
			expect(chosenIn(store, "q-1")).toEqual([]);
		});
	});

	// The same rule where the shape moved without the COUNT moving: q-2 became q-9. Positionally
	// the local answers still fit, which is why the comparison is by question id not length —
	// carrying the pick across would attach the answer to a question never shown. Mutation-check:
	// weakening `sameQuestions` to length-only reddens this specifically; the grew case cannot see it.
	test("a pushed ask that renamed a question beats the local shape", async () => {
		const fake = createFakeComms({
			accounts: [wireAccount(CALLER)],
			channels: [wireChannel(CHANNEL)],
			messagesByChannel: { [CHANNEL]: [askMessage(["q-1", "q-2"])] },
		});

		await withLiveStore(fake, async (store, settled) => {
			store.answerAsk("m-ask", "ask-1", "q-1", "q-1-a");
			await settled();
			expect(chosenIn(store, "q-1")).toEqual(["q-1-a"]);

			await fake.emit(
				{
					case: "messageUpdated",
					value: { message: askMessage(["q-1", "q-9"]) },
				},
				1n,
			);
			await settled();

			expect(askIn(store)?.questions.map((q) => q.questionId)).toEqual([
				"q-1",
				"q-9",
			]);
			expect(chosenIn(store, "q-1")).toEqual([]);
		});
	});

	// The fast path the preserve is built around: with no unshipped pick anywhere,
	// `preserveLocalAsks` collects nothing and hands the pushed state back UNTOUCHED — references
	// and all — so a push not naming the ask leaves its message object identical, sparing every
	// downstream memo (nearly every push). Mutation-check: dropping the local chosen-ids scan reddens this.
	test("a push over an untouched ask is adopted by reference", async () => {
		const fake = createFakeComms({
			accounts: [wireAccount(CALLER)],
			channels: [wireChannel(CHANNEL)],
			messagesByChannel: { [CHANNEL]: [askMessage(["q-1", "q-2"])] },
		});

		await withLiveStore(fake, async (store, settled) => {
			const before = store.messages().find((m) => m.id === "m-ask");
			expect(before).toBeDefined();

			// A push about a DIFFERENT message: the ask row is not restated, so
			// nothing about it may be rebuilt.
			await fake.emit(
				{
					case: "messagePosted",
					value: {
						message: wireTextMessage({
							id: "m-2",
							topicId: TOPIC,
							authorAccountId: CALLER,
							atUnixMs: 2000,
							text: "unrelated",
						}),
					},
				},
				1n,
			);
			await settled();

			expect(store.messages().find((m) => m.id === "m-2")).toBeDefined();
			expect(store.messages().find((m) => m.id === "m-ask")).toBe(before);
		});
	});

	// The accepted-then-lost-reply race, the case the restage's guards can't see by answers alone.
	// The server COMMITTED our respond and published the MessageUpdated, but our RPC's reply never
	// landed, so the promise rejects. The push carries OUR ids, so only `answered` distinguishes a
	// restate from a CLOSE. Mutation-check: dropping `!current.answered` from the restage reddens this.
	test("a refusal after the server accepted does not reopen the closed ask", async () => {
		const fake = createFakeComms({
			accounts: [wireAccount(CALLER)],
			channels: [wireChannel(CHANNEL)],
			messagesByChannel: { [CHANNEL]: [askMessage(["q-1"])] },
		});

		await withLiveStore(fake, async (store, settled) => {
			// (1) record the answer, then submit — shipped and HELD in flight so
			// the push can land before the refusal.
			const gate = fake.holdNextAskResponse();
			store.answerAsk("m-ask", "ask-1", "q-1", "q-1-a");
			await settled();
			store.submitAsk("m-ask", "ask-1");
			await settled();
			expect(chosenIn(store, "q-1")).toEqual(["q-1-a"]);
			expect(store.isAskSubmitted("ask-1")).toBe(true);

			// (2) the write-through of the respond the server ACCEPTED: our ids,
			// and the ask closed.
			await fake.emit(
				{
					case: "messageUpdated",
					value: {
						message: askMessage(["q-1"], { "q-1": ["q-1-a"] }),
					},
				},
				1n,
			);
			await settled();
			expect(askIn(store)?.answered).toBe(true);

			// (3) only now does our own call fail.
			gate.reject(new Error("connection reset"));
			await settled();

			// The restage DECLINED: the ask is still the server's closed record.
			expect(askIn(store)?.answered).toBe(true);
			expect(chosenIn(store, "q-1")).toEqual(["q-1-a"]);
			// The refusal really happened, so the user is told …
			expect(store.askError("ask-1")).toBe("connection reset");
			// … and the ask is not left falsely in flight — it is left CLOSED, so
			// the write gates refuse a further click and nothing more ships. The
			// count is read rather than written as a literal because the double
			// records a respond only on its ACCEPT path: the refused one above was
			// thrown ahead of the bookkeeping, so the baseline here is zero and the
			// contract under test is that it does not GROW.
			expect(store.isAskSubmitted("ask-1")).toBe(false);
			const shipped = fake.askResponses.length;
			store.answerAsk("m-ask", "ask-1", "q-1", "q-1-b");
			store.submitAsk("m-ask", "ask-1");
			await settled();
			expect(fake.askResponses).toHaveLength(shipped);
		});
	});

	// A submit HELD in flight, then a blank push adopted over the submitted ask, then the
	// respond refused: the shipped answers restage over the blank, and the ask is retryable.
	// Mutation-check: a catch that omits the restage leaves the adopted blank ask.
	test("a refused submit restages the clicked answer over a blank push", async () => {
		const fake = createFakeComms({
			accounts: [wireAccount(CALLER)],
			channels: [wireChannel(CHANNEL)],
			messagesByChannel: { [CHANNEL]: [askMessage(["q-1", "q-2"])] },
		});

		await withLiveStore(fake, async (store, settled) => {
			// (1) click records locally; submit ships, HELD in flight.
			const gate = fake.holdNextAskResponse();
			store.answerAsk("m-ask", "ask-1", "q-1", "q-1-a");
			await settled();
			store.submitAsk("m-ask", "ask-1");
			await settled();
			expect(store.isAskSubmitted("ask-1")).toBe(true);

			// (2) a blank unanswered restatement — adopted, because the preserve
			// skips the submitted ask.
			await fake.emit(
				{
					case: "messageUpdated",
					value: { message: askMessage(["q-1", "q-2"]) },
				},
				1n,
			);
			await settled();
			expect(chosenIn(store, "q-1")).toEqual([]);

			// (3) the held respond is refused: the shipped answers restage over the
			// blank push, and the ask is retryable.
			gate.reject(new Error("server refused the ask"));
			await settled();
			expect(chosenIn(store, "q-1")).toEqual(["q-1-a"]);
			expect(store.isAskSubmitted("ask-1")).toBe(false);
		});
	});

	// An ask's SHAPE includes the options it offers, not just its question ids. The block-update
	// path rewrites the whole block set requiring only ask_id, so an agent may restate an ask
	// with REVISED options; carrying the local pick would ship an id the server rejects. Designed-
	// for, not yet wired. Mutation-check: reverting `sameQuestions` to question-id-only reddens this.
	test("a pushed ask that revised its options beats the local pick", async () => {
		const fake = createFakeComms({
			accounts: [wireAccount(CALLER)],
			channels: [wireChannel(CHANNEL)],
			messagesByChannel: { [CHANNEL]: [askMessage(["q-1", "q-2"])] },
		});

		await withLiveStore(fake, async (store, settled) => {
			store.answerAsk("m-ask", "ask-1", "q-1", "q-1-a");
			await settled();
			expect(chosenIn(store, "q-1")).toEqual(["q-1-a"]);

			// Same question ids, a withdrawn option and a fresh one in its place.
			await fake.emit(
				{
					case: "messageUpdated",
					value: {
						message: askMessage(["q-1", "q-2"], undefined, {
							optionIds: { "q-1": ["q-1-b", "q-1-c"] },
						}),
					},
				},
				1n,
			);
			await settled();

			// The PUSHED option list won — the assertion the chosen ids cannot
			// make, since a cleared pick alone would also satisfy them.
			expect(
				askIn(store)
					?.questions.find((q) => q.questionId === "q-1")
					?.options.map((o) => o.id),
			).toEqual(["q-1-b", "q-1-c"]);
			expect(chosenIn(store, "q-1")).toEqual([]);
		});
	});

	// A stream push does not discard a typed draft. A draft is an unshipped
	// edit just as a click is, so the widened preserve scan must carry it across a
	// restatement of the same unanswered ask. Mutation-check: a scan that saw only
	// chosen ids lets the push replace the draft with "".
	test("a stream push does not discard a typed draft", async () => {
		const fake = createFakeComms({
			accounts: [wireAccount(CALLER)],
			channels: [wireChannel(CHANNEL)],
			messagesByChannel: {
				[CHANNEL]: [
					askMessage(["q-1", "q-free"], undefined, { freeText: ["q-free"] }),
				],
			},
		});

		await withLiveStore(fake, async (store, settled) => {
			store.answerAskText("m-ask", "ask-1", "q-free", "my draft");
			await settled();
			expect(customTextIn(store, "q-free")).toBe("my draft");

			// A push restating the ask exactly as the server holds it: still unanswered.
			await fake.emit(
				{
					case: "messageUpdated",
					value: {
						message: askMessage(["q-1", "q-free"], undefined, {
							freeText: ["q-free"],
						}),
					},
				},
				1n,
			);
			await settled();
			expect(customTextIn(store, "q-free")).toBe("my draft");
		});
	});

	// A push carrying a CLOSED free-text ask wins over the draft: the local
	// draft yields and the server's recorded custom_text shows (the audit payoff).
	// Mutation-check: a preserve that carried the draft over an answered push
	// reddens the yields leg; dropping the customText mapping reddens the shows leg.
	test("a closed free-text push beats the draft and shows the recorded text", async () => {
		const fake = createFakeComms({
			accounts: [wireAccount(CALLER)],
			channels: [wireChannel(CHANNEL)],
			messagesByChannel: {
				[CHANNEL]: [
					askMessage(["q-1", "q-free"], undefined, { freeText: ["q-free"] }),
				],
			},
		});

		await withLiveStore(fake, async (store, settled) => {
			store.answerAskText("m-ask", "ask-1", "q-free", "my draft");
			await settled();
			expect(customTextIn(store, "q-free")).toBe("my draft");

			// The push: answered, with the server's recorded free-text answer.
			await fake.emit(
				{
					case: "messageUpdated",
					value: {
						message: askMessage(["q-1", "q-free"], undefined, {
							answered: true,
							freeText: ["q-free"],
							recordedText: { "q-free": "the recorded answer" },
						}),
					},
				},
				1n,
			);
			await settled();

			// The draft yielded, and the server's value shows.
			expect(customTextIn(store, "q-free")).toBe("the recorded answer");
		});
	});

	// The typed half of the refusal restage. A submit HELD in flight, a
	// blank push adopted over the submitted ask, then the respond refused: the
	// shipped answers restage over the blank, so BOTH the click and the typed
	// draft come back. Mutation-check: a catch that omits the restage leaves the
	// draft blank.
	test("a refused submit restages the typed draft alongside the click", async () => {
		const fake = createFakeComms({
			accounts: [wireAccount(CALLER)],
			channels: [wireChannel(CHANNEL)],
			messagesByChannel: {
				[CHANNEL]: [
					askMessage(["q-1", "q-free"], undefined, { freeText: ["q-free"] }),
				],
			},
		});

		await withLiveStore(fake, async (store, settled) => {
			const gate = fake.holdNextAskResponse();
			store.answerAsk("m-ask", "ask-1", "q-1", "q-1-a");
			store.answerAskText("m-ask", "ask-1", "q-free", "my draft");
			await settled();
			store.submitAsk("m-ask", "ask-1");
			await settled();
			expect(store.isAskSubmitted("ask-1")).toBe(true);

			// A blank unanswered restatement — adopted, because the preserve skips
			// the submitted ask.
			await fake.emit(
				{
					case: "messageUpdated",
					value: {
						message: askMessage(["q-1", "q-free"], undefined, {
							freeText: ["q-free"],
						}),
					},
				},
				1n,
			);
			await settled();
			expect(chosenIn(store, "q-1")).toEqual([]);
			expect(customTextIn(store, "q-free")).toBe("");

			// The held respond is refused: both the click and the draft restage.
			gate.reject(new Error("server refused the ask"));
			await settled();
			expect(chosenIn(store, "q-1")).toEqual(["q-1-a"]);
			expect(customTextIn(store, "q-free")).toBe("my draft");
			expect(store.isAskSubmitted("ask-1")).toBe(false);
		});
	});

	// A question's ARITY is part of its shape, not a label. On a multi-select an
	// option and typed text legally coexist; if a push flips that question to
	// single-select, carrying the local pair forward would ship an option plus
	// text the server now rejects — and re-adopt it on every later push, so it
	// never self-heals. The pushed shape wins instead, exactly as a revised
	// option id already does. Mutation-check: dropping `allowMultiple` from
	// `sameQuestions` carries the stale pair and reddens every leg below.
	test("a pushed ask that flipped a question to single-select beats the local pair", async () => {
		const fake = createFakeComms({
			accounts: [wireAccount(CALLER)],
			channels: [wireChannel(CHANNEL)],
			messagesByChannel: {
				[CHANNEL]: [askMessage(["q-1"], undefined, { multi: ["q-1"] })],
			},
		});

		await withLiveStore(fake, async (store, settled) => {
			// Legal on a multi-select: a chosen option AND a typed answer.
			store.answerAsk("m-ask", "ask-1", "q-1", "q-1-a");
			store.answerAskText("m-ask", "ask-1", "q-1", "typed");
			await settled();
			expect(chosenIn(store, "q-1")).toEqual(["q-1-a"]);
			expect(customTextIn(store, "q-1")).toBe("typed");

			// The agent restates the ask single-select, same question and option ids.
			await fake.emit(
				{ case: "messageUpdated", value: { message: askMessage(["q-1"]) } },
				1n,
			);
			await settled();

			// The pushed shape is adopted and the now-illegal pair is gone.
			expect(askIn(store)?.questions[0]?.allowMultiple).toBe(false);
			expect(chosenIn(store, "q-1")).toEqual([]);
			expect(customTextIn(store, "q-1")).toBe("");

			// Nothing was staged, so a submit says nothing rather than shipping a
			// respond the server would refuse with ErrInvalidArgument.
			store.submitAsk("m-ask", "ask-1");
			await settled();
			expect(fake.askResponses).toEqual([]);
		});
	});
});
