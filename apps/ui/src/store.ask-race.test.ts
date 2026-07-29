import { describe, expect, test } from "bun:test";
import { createRoot } from "solid-js";
import {
	wireChannel as buildWireChannel,
	createFakeComms,
	type FakeComms,
	wireAccount,
	wireAskMessage,
} from "./live/comms-fake";
import { type AppStore, createAppStore } from "./store";

// The race between a MULTI-question ask being answered and the stream pushing a
// new state under it.
//
// The wire is ATOMIC: exactly one RespondToAsk per ask, issued only on the click
// that COMPLETES the ask (see the comment above `sendAsk`). So every choice
// before the last one lives ONLY in local `comms` state — the server has not
// been told and has nothing to send back. `adoptComms` replaces that state
// wholesale on every push, so a snapshot or a tail event landing mid-ask used to
// discard the user's clicks with no indication at all.
//
// The fix must hold BOTH ends:
//
//   - an ask with purely local, unsubmitted answers survives a push,
//   - an ask the server has an opinion about — because we shipped it, or because
//     another participant answered it — still takes the SERVER's value, which is
//     the property the refused-respond rollback rests on (store.live.test.ts's
//     "does not clobber an ask the stream moved meanwhile").
//
// The rollback and gate contracts themselves live in store.live.test.ts; this
// suite's subject is narrowly `adoptComms` — what a stream push does to an
// in-progress ask.

const CALLER = "acc-me";
const CHANNEL = "chan-1";

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
	over?: { answered?: boolean; freeText?: readonly string[] },
) =>
	wireAskMessage({
		id: "m-ask",
		channelId: CHANNEL,
		authorAccountId: CALLER,
		askId: "ask-1",
		questionIds,
		chosen,
		answered: over?.answered,
		freeText: over?.freeText,
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
		return createAppStore({ comms: fake.client, callerId: CALLER });
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
	// The gap the wire's atomicity opens: the first click on a two-question ask
	// sends NOTHING, so the answer exists only locally. A push re-stating the ask
	// as the server still holds it (unanswered — it was never told) must not take
	// the click away. Mutation-check: the wholesale `setComms(next)` reddens the
	// survives leg; a preserve that forgot to re-arm the ask reddens the still
	// completable leg.
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

			// … and the ask is still live: completing it ships BOTH answers, which
			// is only possible if the surviving answer is real state and not just a
			// rendered ghost.
			store.answerAsk("m-ask", "ask-1", "q-2", "q-2-a");
			await settled();
			expect(fake.askResponses).toEqual([
				{
					askId: "ask-1",
					answers: [
						{ questionId: "q-1", chosenOptionIds: ["q-1-a"] },
						{ questionId: "q-2", chosenOptionIds: ["q-2-a"] },
					],
				},
			]);
		});
	});

	// The other end of the rule, and the one the refusal rollback depends on: an
	// ask the SERVER has an opinion about takes the server's value, even while
	// the user has an unsubmitted local answer on it. Here another participant
	// answered q-1 differently; ours was never shipped, so theirs is the record.
	// Mutation-check: preserving local answers unconditionally reddens this.
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
						message: askMessage(["q-1", "q-2"], { "q-1": ["q-1-b"] }),
					},
				},
				1n,
			);
			await settled();

			expect(chosenIn(store, "q-1")).toEqual(["q-1-b"]);
		});
	});

	// A SHIPPED ask is the server's, full stop. Once the one RespondToAsk is
	// issued the local record is no longer "in progress" — it is a claim about
	// what the server was told — so a push replaces it even when the pushed ask
	// carries no answers yet (the server's own view has not caught up, or the
	// respond is still in flight). Keeping the local copy here would re-break the
	// conditional rollback in `sendAsk`, which decides by comparing the shipped
	// answers against what the stream has since put in their place.
	// Mutation-check: dropping the submitted-ask gate reddens this.
	test("a shipped ask takes the pushed server value", async () => {
		const fake = createFakeComms({
			accounts: [wireAccount(CALLER)],
			channels: [wireChannel(CHANNEL)],
			messagesByChannel: { [CHANNEL]: [askMessage(["q-only"])] },
		});

		await withLiveStore(fake, async (store, settled) => {
			// A one-question ask completes on its only click, so this ships.
			store.answerAsk("m-ask", "ask-1", "q-only", "q-only-a");
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

	// The shape that held this fix: a CLOSED ask with no chosen ids anywhere.
	// A deliberate skip is an ACCEPTED answer — an entry with no chosen ids and
	// empty custom_text satisfies the wire's coverage-of-every-question contract
	// — so the server flips Ask.answered and records nothing to see. The server
	// has closed this ask, so our unshipped click must NOT be restored over it:
	// preserving it would leave the UI offering a completing click the server is
	// guaranteed to refuse with ErrConflict.
	//
	// Mutation-check: this is precisely the case the old chosen-ids scan got
	// wrong — it read this ask as "the server has said nothing" and let local
	// state clobber it, so reverting the predicate reddens this test.
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

	// The second defeating shape: a free-text question carries NO options, so it
	// is answered by custom_text alone and its chosenOptionIds stays empty even
	// though the server accepted the answer and closed the ask. Same rule, same
	// reason — only Ask.answered can see it.
	//
	// Mutation-check: reverting to the chosen-ids scan reddens this too.
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

	// The other side of the same flag, and the original bug: `answered: false`
	// with empty chosen ids is the GENUINELY pending ask — the server was never
	// told, has nothing to send back, and the user's unshipped click must
	// survive. This is what stops the new predicate from being read as "any push
	// wins".
	//
	// Mutation-check: a predicate that always reported "the server has a value"
	// reddens this.
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

			// And it is real state, not a ghost: completing the ask ships both.
			store.answerAsk("m-ask", "ask-1", "q-2", "q-2-a");
			await settled();
			expect(fake.askResponses).toEqual([
				{
					askId: "ask-1",
					answers: [
						{ questionId: "q-1", chosenOptionIds: ["q-1-a"] },
						{ questionId: "q-2", chosenOptionIds: ["q-2-a"] },
					],
				},
			]);
		});
	});
});
