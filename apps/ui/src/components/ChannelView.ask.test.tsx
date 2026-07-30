import { describe, expect, test } from "bun:test";
import { fireEvent, render } from "@solidjs/testing-library";
import { StoreContext } from "../context";
import {
	createFakeComms,
	type FakeComms,
	wireAccount,
	wireAskMessage,
	wireChannel,
} from "../live/comms-fake";
import { type AppStore, createAppStore } from "../store";
import { ChannelView } from "./ChannelView";

// The MULTI-question ask surface, over the live wire. The single-question ask
// (the only shape in the offline fixture) is covered in ChannelView.test.tsx;
// this suite exists for what a single-question fixture structurally cannot
// show — the send GATE and its skip affordance (Matt's ruling):
//
//   - the server accepts exactly ONE RespondToAsk per ask
//     (go/internal/store/messages.go:404-406 rejects a later one; :438 flips
//     Answered on the first), so a partial answer sent on the first click would
//     be the permanent audit record and lock the rest of the ask out;
//   - therefore clicks stay LOCAL until the ask is complete, and the completing
//     click issues the one full respond;
//   - a user who wants to SKIP a question never completes the ask, so there is a
//     submit control that ships what is answered with the skipped questions
//     empty.
//
// The store contracts are pinned in store.live.test.ts; here the subject is the
// RENDER — that the affordance exists, appears only when it is meaningful, and
// reaches the wire.

const CALLER = "acc-me";
const CHANNEL = "chan-live";

/** A server whose one channel carries one TWO-question ask. */
const snapshot = () => ({
	accounts: [wireAccount(CALLER)],
	channels: [wireChannel(CHANNEL, CALLER)],
	messagesByChannel: {
		[CHANNEL]: [
			wireAskMessage({
				id: "m-ask",
				channelId: CHANNEL,
				authorAccountId: CALLER,
				askId: "ask-1",
				questionIds: ["q-1", "q-2"],
			}),
		],
	},
});

/** Mount the standalone ChannelView over a live store and wait out the driver's
 *  snapshot round-trip, so the ask has rendered before the body runs. Every hop
 *  is a resolved promise, so the bounded microtask drain is deterministic. */
async function mountAsk(fake: FakeComms): Promise<{
	store: AppStore;
	container: HTMLElement;
	settled: () => Promise<void>;
}> {
	let store!: AppStore;
	const { container } = render(() => {
		store = createAppStore({ comms: fake.client, callerId: CALLER });
		return (
			<StoreContext.Provider value={store}>
				<ChannelView />
			</StoreContext.Provider>
		);
	});
	const settled = async () => {
		for (let i = 0; i < 20; i++) await Promise.resolve();
	};
	await settled();
	return { store, container, settled };
}

const askOptions = (c: HTMLElement): HTMLButtonElement[] => [
	...c.querySelectorAll<HTMLButtonElement>(".block-ask .ask-option"),
];
const submitControl = (c: HTMLElement) =>
	c.querySelector<HTMLButtonElement>(".block-ask .ask-submit");
const askErrorText = (c: HTMLElement) =>
	c.querySelector(".block-ask .ask-error")?.textContent;

describe("multi-question ask (live RespondToAsk gate)", () => {
	// The gate, from the DOM: a click on the first question's option records
	// locally and puts NOTHING on the wire, because the ask is not yet complete.
	// Completing it from the DOM issues exactly one respond carrying both
	// questions. Mutation-check: the pre-ruling per-click respond records a
	// response after the first click and reddens the empty leg.
	test("the first click sends nothing; the completing click sends one full respond", async () => {
		const fake = createFakeComms(snapshot());
		const { container, settled } = await mountAsk(fake);
		try {
			const options = askOptions(container);
			// Two questions × two options — proves the ask actually rendered.
			expect(options.length).toBe(4);

			fireEvent.click(options[0]); // q-1 → q-1-a
			await settled();
			expect(fake.askResponses).toEqual([]);
			// The click IS recorded locally — the user sees their choice.
			expect(options[0].getAttribute("aria-pressed")).toBe("true");

			fireEvent.click(options[2]); // q-2 → q-2-a
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
		} finally {
			fake.close();
		}
	});

	// The skip affordance's visibility rule: it is meaningless on an untouched
	// ask (nothing to submit) and on a complete one (already sent), so it renders
	// only in between — a partially answered ask. Mutation-check: an
	// always-rendered submit control reddens the first and third legs.
	test("the submit control appears only on a partially answered ask", async () => {
		const fake = createFakeComms(snapshot());
		const { container, settled } = await mountAsk(fake);
		try {
			// Untouched: nothing to submit.
			expect(submitControl(container)).toBeNull();

			fireEvent.click(askOptions(container)[0]); // answer q-1, skip q-2
			await settled();
			expect(submitControl(container)).not.toBeNull();

			fireEvent.click(askOptions(container)[2]); // now complete → sent
			await settled();
			expect(submitControl(container)).toBeNull();
		} finally {
			fake.close();
		}
	});

	// The skip path end to end: answer one question, click submit, and the one
	// respond carries the answered question plus an EMPTY chosenOptionIds for the
	// skipped one (the wire requires coverage of every question, not an answer to
	// each). Mutation-check: a submit that omitted the skipped question drops the
	// q-2 entry; an inert control records nothing.
	test("submitting a partially answered ask ships the skipped question empty", async () => {
		const fake = createFakeComms(snapshot());
		const { container, settled } = await mountAsk(fake);
		try {
			fireEvent.click(askOptions(container)[0]); // q-1 → q-1-a, q-2 skipped
			await settled();

			const submit = submitControl(container);
			if (!submit) throw new Error("submit control did not render");
			fireEvent.click(submit);
			await settled();

			expect(fake.askResponses).toEqual([
				{
					askId: "ask-1",
					answers: [
						{ questionId: "q-1", chosenOptionIds: ["q-1-a"] },
						{ questionId: "q-2", chosenOptionIds: [] },
					],
				},
			]);
			// A submitted ask is settled: every option locks, and the skipped
			// question can no longer be answered behind the server's back.
			const options = askOptions(container);
			expect(options.length).toBe(4);
			for (const option of options) expect(option.disabled).toBe(true);
		} finally {
			fake.close();
		}
	});

	// A REFUSED respond rolls the local answer back — which, on its own, makes
	// the user's click vanish with no explanation (the only other sink is a
	// console line). The ask block says so, the way the composer does for a
	// failed post, and answering again clears it. Mutation-check: dropping the
	// error render reddens the first leg; a never-cleared error reddens the
	// second.
	test("a refused respond renders in the ask block and clears on the next answer", async () => {
		const fake = createFakeComms(snapshot());
		const { container, settled } = await mountAsk(fake);
		try {
			expect(askErrorText(container)).toBeUndefined();

			fake.failNextAskResponse(new Error("server refused the ask"));
			fireEvent.click(askOptions(container)[0]); // q-1 → q-1-a
			fireEvent.click(askOptions(container)[2]); // completes → respond → refused
			await settled();

			expect(askErrorText(container)).toContain("server refused the ask");
			// The rollback restored the pre-click ask (q-1 answered, q-2 not), so
			// the ask is not burnt: q-2 can be answered again …
			expect(askOptions(container)[2].disabled).toBe(false);

			// … and doing so clears the stale refusal.
			fireEvent.click(askOptions(container)[2]);
			await settled();
			expect(askErrorText(container)).toBeUndefined();
			expect(fake.askResponses.length).toBe(1);
		} finally {
			fake.close();
		}
	});

	// The other way an ask is settled, and the one `submitted` cannot see: the
	// server flips Ask.answered on the first RespondToAsk it ACCEPTS and refuses
	// every later one with ErrConflict (go/internal/store/messages.go:404-406).
	// An ask another participant already answered therefore arrives CLOSED to
	// this client, which has issued no respond of its own — so judged by the
	// submitted mark alone every option renders enabled and the skip control
	// renders too, offering clicks that can only produce a refusal. The closed
	// ask locks instead.
	//
	// Mutation-check: gating `locked` on `submitted()` alone leaves the options
	// enabled and reddens the disabled leg; gating `canSubmit` on it alone
	// renders the skip control and reddens the second.
	test("a server-closed ask renders locked with no submit control", async () => {
		const fake = createFakeComms({
			accounts: [wireAccount(CALLER)],
			channels: [wireChannel(CHANNEL, CALLER)],
			messagesByChannel: {
				[CHANNEL]: [
					// Another participant answered q-1; the server recorded their id
					// and closed the ask in the one write. Partially answered by
					// chosen ids, which is exactly when the skip control would show.
					wireAskMessage({
						id: "m-ask",
						channelId: CHANNEL,
						authorAccountId: CALLER,
						askId: "ask-1",
						questionIds: ["q-1", "q-2"],
						chosen: { "q-1": ["q-1-a"] },
					}),
				],
			},
		});
		const { store, container, settled } = await mountAsk(fake);
		try {
			expect(store.isAskSubmitted("ask-1")).toBe(false);

			const options = askOptions(container);
			expect(options.length).toBe(4);
			for (const option of options) expect(option.disabled).toBe(true);
			expect(submitControl(container)).toBeNull();

			// And the dead control is honest: a click on the untouched question
			// puts nothing on the wire.
			fireEvent.click(options[2]);
			await settled();
			expect(fake.askResponses).toEqual([]);
		} finally {
			fake.close();
		}
	});
});
