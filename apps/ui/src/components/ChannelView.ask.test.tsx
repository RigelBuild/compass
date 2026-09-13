import { describe, expect, test } from "bun:test";
import { fireEvent, render } from "@solidjs/testing-library";
import { StoreContext } from "../context";
import {
	createFakeComms,
	type FakeComms,
	wireAccount,
	wireAskMessage,
	wireChannel,
	wireTopic,
} from "../live/comms-fake";
import { type AppStore, createAppStore } from "../store";
import { testQueryClient } from "../test-support";
import { TopicView } from "./TopicView";

// The MULTI-question ask surface, over the live wire. The single-question ask
// (the only shape in the offline fixture) is covered in ChannelView.test.tsx;
// this suite exists for what a single-question fixture structurally cannot
// show — the send model (Matt's ruling):
//
//   - the server accepts exactly ONE RespondToAsk per ask, so recording an
//     answer never sends: clicks stay LOCAL until an explicit submit;
//   - the submit control is the ONE send path, unconditional on a live ask,
//     shipping answered questions plus an empty answer for each skip;
//   - a refused respond leaves the staged answers in place (no rollback), and
//     the ask stays retryable.
//
// The store contracts are pinned in store.live.test.ts; here the subject is the
// RENDER — that the control exists, is enabled/labelled correctly, and reaches
// the wire.

const CALLER = "acc-me";
const CHANNEL = "chan-live";
const TOPIC = "top-live";

/** A server whose one channel has one topic carrying one TWO-question ask. */
const snapshot = () => ({
	accounts: [wireAccount(CALLER)],
	channels: [wireChannel(CHANNEL, CALLER)],
	topicsByChannel: {
		[CHANNEL]: [wireTopic({ id: TOPIC, channelId: CHANNEL, name: "primary" })],
	},
	messagesByChannel: {
		[CHANNEL]: [
			wireAskMessage({
				id: "m-ask",
				topicId: TOPIC,
				authorAccountId: CALLER,
				askId: "ask-1",
				questionIds: ["q-1", "q-2"],
			}),
		],
	},
});

/** Mount TopicView over a live store, wait out the driver's snapshot round-trip,
 *  then open the topic so its ask has rendered before the body runs. Every hop
 *  is a resolved promise, so the bounded microtask drain is deterministic. */
async function mountAsk(fake: FakeComms): Promise<{
	store: AppStore;
	container: HTMLElement;
	settled: () => Promise<void>;
}> {
	let store!: AppStore;
	const { container } = render(() => {
		store = createAppStore({
			comms: fake.client,
			callerId: CALLER,
			queryClient: testQueryClient(),
		});
		return (
			<StoreContext value={store}>
				<TopicView />
			</StoreContext>
		);
	});
	const settled = async () => {
		for (let i = 0; i < 20; i++) await Promise.resolve();
	};
	await settled();
	store.openTopic(TOPIC);
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
	// From the DOM: a click on the first question's option records locally and
	// puts NOTHING on the wire; the completing click also sends nothing. Only
	// the submit control's click ships the one respond carrying both questions.
	// Mutation-check: an auto-sending recorder records a response on a click and
	// reddens an empty leg.
	test("clicks send nothing; the submit control sends one full respond", async () => {
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

			fireEvent.click(options[2]); // q-2 → q-2-a: complete, but still unsent
			await settled();
			expect(fake.askResponses).toEqual([]);

			const submit = submitControl(container);
			if (!submit) throw new Error("submit control did not render");
			fireEvent.click(submit);
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

	// The submit control is the ONE send path, so it renders on every live ask:
	// disabled while untouched, enabled once anything is answered, and its label
	// switches on completeness. It disappears only when the ask settles.
	// Mutation-check: a control gated on the partially-answered window reddens
	// the untouched-disabled and fully-answered legs.
	test("the submit control is unconditional on a live ask", async () => {
		const fake = createFakeComms(snapshot());
		const { container, settled } = await mountAsk(fake);
		try {
			// Untouched: present but disabled — it announces the coming submit.
			const untouched = submitControl(container);
			if (!untouched) throw new Error("submit control did not render");
			expect(untouched.disabled).toBe(true);

			fireEvent.click(askOptions(container)[0]); // answer q-1, skip q-2
			await settled();
			const partial = submitControl(container);
			if (!partial)
				throw new Error("submit control vanished on partial answer");
			expect(partial.disabled).toBe(false);
			expect(partial.textContent).toContain("submit — skip the rest");

			fireEvent.click(askOptions(container)[2]); // now fully answered
			await settled();
			const full = submitControl(container);
			if (!full) throw new Error("submit control vanished on full answer");
			expect(full.disabled).toBe(false);
			expect(full.textContent?.trim()).toBe("submit");

			fireEvent.click(full); // submit → settled
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

	// A REFUSED respond leaves the staged answers in place — submit records
	// nothing to roll back — and surfaces the error so the user's submit does
	// not vanish into a console line, the way the composer does for a failed
	// post. Re-submitting clears it. Mutation-check: dropping the error render
	// reddens the first leg; a never-cleared error reddens the last.
	test("a refused respond renders in the ask block and clears on the next submit", async () => {
		const fake = createFakeComms(snapshot());
		const { container, settled } = await mountAsk(fake);
		try {
			expect(askErrorText(container)).toBeUndefined();

			fireEvent.click(askOptions(container)[0]); // q-1 → q-1-a
			fireEvent.click(askOptions(container)[2]); // q-2 → q-2-a: complete
			await settled();

			fake.failNextAskResponse(new Error("server refused the ask"));
			const submit = submitControl(container);
			if (!submit) throw new Error("submit control did not render");
			fireEvent.click(submit);
			await settled();

			expect(askErrorText(container)).toContain("server refused the ask");
			// No rollback: the staged answers survive, so the ask is retryable.
			expect(askOptions(container)[0].getAttribute("aria-pressed")).toBe(
				"true",
			);

			// Re-submitting ships the still-staged answers and clears the error.
			const retry = submitControl(container);
			if (!retry) throw new Error("submit control vanished after refusal");
			fireEvent.click(retry);
			await settled();
			expect(askErrorText(container)).toBeUndefined();
			expect(fake.askResponses.length).toBe(1);
		} finally {
			fake.close();
		}
	});

	// happy-dom does not implement a button's implicit Enter-to-click, so this
	// pins the PRECONDITIONS that behaviour needs rather than the keystroke:
	// type="button", no ancestor <form>, focusable, Enter left uncancelled. Real
	// Enter activation is manual/e2e — this suite cannot exercise it.
	test("the submit control preserves the native keyboard-activation preconditions", async () => {
		const fake = createFakeComms(snapshot());
		const { container, settled } = await mountAsk(fake);
		try {
			fireEvent.click(askOptions(container)[0]); // enable the control
			await settled();

			const submit = submitControl(container);
			if (!submit) throw new Error("submit control did not render");
			expect(submit.tagName).toBe("BUTTON");
			expect(submit.getAttribute("type")).toBe("button");
			expect(submit.closest("form")).toBeNull();

			submit.focus();
			expect(document.activeElement).toBe(submit);

			const enter = new KeyboardEvent("keydown", {
				key: "Enter",
				bubbles: true,
				cancelable: true,
			});
			submit.dispatchEvent(enter);
			expect(enter.defaultPrevented).toBe(false);
		} finally {
			fake.close();
		}
	});

	// The other way an ask is settled, and the one `submitted` cannot see: the
	// server flips Ask.answered on the first RespondToAsk it ACCEPTS and refuses
	// every later one with ErrConflict (the answer-once guard in `applyAskAnswer`,
	// `go/internal/store/messages.go`). An ask another participant already
	// answered arrives CLOSED to this client, which has issued no respond of its
	// own — so judged by the submitted mark alone every option renders enabled
	// and the submit control shows too, offering a gesture that can only produce
	// a refusal. The closed ask locks instead, and the submit control hides on
	// the `!closed()` gate.
	//
	// Mutation-check: gating `locked` on `submitted()` alone leaves the options
	// enabled and reddens the disabled leg; gating the control's visibility on
	// `submitted()` alone renders it and reddens the no-control leg.
	test("a server-closed ask renders locked with no submit control", async () => {
		const fake = createFakeComms({
			accounts: [wireAccount(CALLER)],
			channels: [wireChannel(CHANNEL, CALLER)],
			topicsByChannel: {
				[CHANNEL]: [
					wireTopic({ id: TOPIC, channelId: CHANNEL, name: "primary" }),
				],
			},
			messagesByChannel: {
				[CHANNEL]: [
					// Another participant answered q-1; the server recorded their id
					// and closed the ask in the one write.
					wireAskMessage({
						id: "m-ask",
						topicId: TOPIC,
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
