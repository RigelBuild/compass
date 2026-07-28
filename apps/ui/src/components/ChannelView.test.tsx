import { describe, expect, test } from "bun:test";
import { fireEvent, render } from "@solidjs/testing-library";
import {
	type Ask,
	STUB_ACCOUNTS,
	STUB_CHANNELS,
	STUB_COMMS_STATE,
	STUB_MESSAGES,
} from "../comms-stub";
import { StoreContext } from "../context";
import { type AppStore, createAppStore } from "../store";
import { ChannelView } from "./ChannelView";

// Acceptance spec for the standalone channel view's asks (design.md §219-256):
// asks are answerable wherever they are, no rerouting, first-responder-wins is
// the sole settlement. `ChannelView` renders ask blocks
// interactive in every mount; a single-select ask locks (`locked()`) only once
// answered. These tests defend that contract on the standalone-channel surface
// (App.tsx `view()==="channel"`): options enabled, a click records through
// `store.answerAsk`, NO read-only hint anywhere, and a settled ask renders every
// option disabled with the winner `chosen`. The interactive-guard test confirms
// the default mount behaves identically (there is no longer a gated variant).
//
// Fixture ground truth (grepped from comms-stub.ts / stub-data.ts, quoted here;
// DERIVED below, not hardcoded, so a fixture reshuffle can't stale the test):
//   - The ONLY ask sitting in a standalone (kind "channel") channel is
//     msg-c4 `ask-s4-integration` in `ch-svc-compass` ("svc.compass",
//     kind "channel", membership "subscribed" → a standalone member channel).
//     Its message author is `acc-livingstone` (handle "livingstone"). The
//     hint-absence leg asserts NO `@<author-handle>` text renders (the gate that
//     once routed answers to @<author>'s workspace is gone); @livingstone is that
//     author. NOTE: the brief said `@cook`, but `ask-cook-layout` (cook's ask)
//     lives in `dm-cook` (kind "dm") → openChannel routes THAT to the agent
//     workspace (store.ts:602-616), so it never renders in a standalone channel.
//     We derive the true author from the store's accounts.
//   - `ch-svc-compass` also carries a threaded text exchange (msg-c1 root +
//     msg-c2/msg-c3 replies) plus the ask, so both mounts render real threads —
//     the "threads render identically" leg has content to compare.

// The first ask that sits in a STANDALONE channel (kind "channel"), with the
// message + channel coordinates the tests drive. Derived from the fixture so a
// reshuffle can't stale it: finds whatever standalone-channel ask exists.
function standaloneChannelAsk(): {
	channelId: string;
	messageId: string;
	ask: Ask;
	authorAccountId: string;
} {
	const channelKind = new Map(STUB_CHANNELS.map((c) => [c.id, c.kind]));
	for (const m of STUB_MESSAGES) {
		if (channelKind.get(m.channelId) !== "channel") continue;
		for (const b of m.blocks) {
			if (b.kind === "ask") {
				return {
					channelId: m.channelId,
					messageId: m.id,
					ask: b.ask,
					authorAccountId: m.authorAccountId,
				};
			}
		}
	}
	throw new Error(
		"fixture has no ask in a standalone (kind 'channel') channel — T6 read-only tests need one",
	);
}

const STANDALONE_ASK = standaloneChannelAsk();
// The owning agent's handle — the ask author's handle, resolved through the same
// account set the store exposes (accounts()), so the hint assertion tracks the
// fixture's real author rather than a copied literal.
const AUTHOR_HANDLE = (() => {
	const account = STUB_ACCOUNTS.find(
		(a) => a.id === STANDALONE_ASK.authorAccountId,
	);
	if (!account) {
		throw new Error(
			`fixture ask author ${STANDALONE_ASK.authorAccountId} has no account — cannot resolve the hint handle`,
		);
	}
	return account.handle; // "livingstone" for ask-s4-integration
})();

// Mount ChannelView over a real store through the app's StoreContext (index.tsx
// wires it as `<StoreContext.Provider value={store}>`; there is no separate
// provider wrapper). The store is built inside render's reactive root so its
// memos are owned and disposed on the library's per-test cleanup; the reference
// is captured so tests drive `openChannel` and re-read `messages()`.
//
// The `readonly` param is vestigial: the read-only gate it once selected is gone,
// so both mounts render identically — a `true` mount spreads a now-unknown
// `readonlyAsks` key that ChannelView ignores. It is kept only so the
// interactive-guard test can mount "the same view twice" for its identical-render
// comparison; collapsing it is parked for Matt (see the PR's Open Questions).
function mountChannelView(readonly: boolean): {
	store: AppStore;
	container: HTMLElement;
} {
	let store!: AppStore;
	const { container } = render(() => {
		store = createAppStore({ initialComms: STUB_COMMS_STATE });
		const extra: Record<string, unknown> = readonly
			? { readonlyAsks: true }
			: {};
		return (
			<StoreContext.Provider value={store}>
				<ChannelView {...extra} />
			</StoreContext.Provider>
		);
	});
	return { store, container };
}

// The ask option buttons rendered for the currently-selected channel.
const askOptions = (container: HTMLElement): HTMLButtonElement[] => [
	...container.querySelectorAll<HTMLButtonElement>(".ask-option"),
];

// Read the standalone ask's recorded choice out of the store's reactive message
// list — the public observation of whether a click mutated.
const chosenIds = (store: AppStore): string[] => {
	const msg = store.messages().find((m) => m.id === STANDALONE_ASK.messageId);
	for (const b of msg?.blocks ?? []) {
		if (b.kind === "ask" && b.ask.askId === STANDALONE_ASK.ask.askId) {
			return b.ask.questions[0].chosenOptionIds;
		}
	}
	return [];
};

describe("ChannelView (T6)", () => {
	// A standalone-channel ask renders answerable to every viewer (design.md
	// §219-240): options ENABLED and NO read-only hint — there is no gate, and a
	// single-select ask is inert only once `locked()` (answered). Mutation-check:
	// re-introducing a `disabled` gate arm reddens the enabled loop; a resurrected
	// hint block reddens the hint-absence check.
	test("an ask in a standalone channel renders answerable (options enabled, no read-only hint)", () => {
		const { store, container } = mountChannelView(true);
		store.openChannel(STANDALONE_ASK.channelId);

		const options = askOptions(container);
		// Precondition: the ask block actually rendered (proves the red is an
		// assertion red, not an empty-render false-negative).
		expect(options.length).toBeGreaterThan(0);

		// Every option is interactive.
		for (const option of options) {
			expect(option.disabled).toBe(false);
		}

		// No read-only hint anywhere: neither the `.ask-readonly-hint` element nor
		// the "answer in @…'s workspace" routing text survives the gate removal.
		expect(container.querySelector(".ask-readonly-hint")).toBeNull();
		const hint = [...container.querySelectorAll<HTMLElement>("*")].find((el) =>
			el.textContent?.includes(`@${AUTHOR_HANDLE}`),
		);
		expect(hint).toBeUndefined();
	});

	// A click in the standalone channel RECORDS the answer through store.answerAsk
	// — no gate, no rerouting to a workspace. After clicking the first option, the
	// store's chosenOptionIds is exactly that option, and the button carries
	// aria-pressed="true" and the `chosen` class. Mutation-check: re-introducing a
	// gate early-return that swallows the click reddens all three post-click legs.
	test("an ask in a standalone channel records the answer on click", () => {
		const { store, container } = mountChannelView(true);
		store.openChannel(STANDALONE_ASK.channelId);

		const options = askOptions(container);
		expect(options.length).toBeGreaterThan(0);
		expect(chosenIds(store)).toEqual([]); // starts unanswered

		fireEvent.click(options[0]);

		const winningId = STANDALONE_ASK.ask.questions[0].options[0].id;
		// The store recorded the click …
		expect(chosenIds(store)).toEqual([winningId]);
		// … and the button reflects the choice.
		expect(options[0].getAttribute("aria-pressed")).toBe("true");
		expect(options[0].classList.contains("chosen")).toBe(true);
	});

	// Settled render on the standalone surface (design.md §242-256): once the ask
	// is answered (through the store, the first-wins winner), every option renders
	// disabled and the winning option carries `chosen`. `locked()` is the sole
	// reason they are disabled — this is the first-wins render surface. A broken
	// `locked()`/`chosen` (options enabled after settle, or a missing highlight)
	// reddens either way.
	test("a settled single-select ask on the standalone surface renders locked (all disabled, winner chosen)", () => {
		const { store, container } = mountChannelView(true);
		store.openChannel(STANDALONE_ASK.channelId);

		const question = STANDALONE_ASK.ask.questions[0];
		const winningId = question.options[0].id;
		// Settle the ask through the seam (not a click — the click path is the
		// separate gate-removal test above).
		store.answerAsk(
			STANDALONE_ASK.messageId,
			STANDALONE_ASK.ask.askId,
			question.questionId,
			winningId,
		);
		expect(chosenIds(store)).toEqual([winningId]);

		const options = askOptions(container);
		expect(options.length).toBeGreaterThan(0);
		for (const option of options) {
			expect(option.disabled).toBe(true);
		}
		const winner = options.find((o) => o.classList.contains("chosen"));
		expect(winner).toBeDefined();
		expect(winner?.getAttribute("aria-pressed")).toBe("true");
	});

	// Render-determinism + interaction guard: the standalone channel mounted twice
	// renders identical thread/message structure, and a click in the interactive
	// path records the choice. (Both mounts are interactive now — the ask block
	// never differs in structure, only ever in `locked()` state once answered.)
	// design §620-621: the standalone surface behaves as the ordinary interactive
	// view. Guards against a regression that makes one mount over-disable or drop
	// content relative to the other.
	test("the same ask stays answerable in the default (interactive) mount, and threads render identically", () => {
		// A second mount of the same channel — for the identical-render comparison.
		const secondMount = mountChannelView(true);
		secondMount.store.openChannel(STANDALONE_ASK.channelId);

		// Default (interactive) mount.
		const { store, container } = mountChannelView(false);
		store.openChannel(STANDALONE_ASK.channelId);

		const options = askOptions(container);
		expect(options.length).toBeGreaterThan(0);

		// Options are enabled in the interactive path …
		for (const option of options) {
			expect(option.disabled).toBe(false);
		}

		// … and clicking one records the choice (single-select → exactly it).
		fireEvent.click(options[0]);
		expect(chosenIds(store)).toEqual([
			STANDALONE_ASK.ask.questions[0].options[0].id,
		]);

		// Threads render identically in both mounts: same message + thread count.
		// The ask block never differs in structure between mounts.
		const count = (root: HTMLElement, sel: string): number =>
			root.querySelectorAll(sel).length;
		expect(count(container, ".thread")).toBe(
			count(secondMount.container, ".thread"),
		);
		expect(count(container, ".msg")).toBe(count(secondMount.container, ".msg"));
		// Non-triviality: the channel actually has threaded content to compare,
		// so an "identical" pass can't be two empty renders agreeing.
		expect(count(container, ".msg")).toBeGreaterThan(1);
	});
});

// The settled-state render lock and its interaction guard (design.md §242-256),
// exercised on the interactive mount so `locked()` — not any gate — is the sole
// reason a settled ask is inert. Any red here is a real bug in the render lock.
describe("ChannelView settled-state lock (T3)", () => {
	// Render half: a settled single-select ask renders every option disabled with
	// the winner carrying `chosen`. Backed purely by `locked()` (allowMultiple ===
	// false && chosenOptionIds.length > 0). Mutation-check: a `locked()` that
	// ignored chosenOptionIds (options stay enabled) or dropped the `chosen`
	// highlight reddens.
	test("a settled single-select ask renders all options disabled with the winner chosen", () => {
		const { store, container } = mountChannelView(false);
		store.openChannel(STANDALONE_ASK.channelId);

		const question = STANDALONE_ASK.ask.questions[0];
		const winningId = question.options[0].id;
		store.answerAsk(
			STANDALONE_ASK.messageId,
			STANDALONE_ASK.ask.askId,
			question.questionId,
			winningId,
		);

		const options = askOptions(container);
		expect(options.length).toBeGreaterThan(0);
		for (const option of options) {
			expect(option.disabled).toBe(true);
		}
		const winner = options.find((o) => o.classList.contains("chosen"));
		expect(winner).toBeDefined();
		expect(winner?.getAttribute("aria-pressed")).toBe("true");
	});

	// Interaction half (the loop the store guard exists for): clicking a LOSING
	// option on a settled single-select ask does NOT mutate chosenOptionIds, and
	// the winner's aria-pressed/chosen is unchanged. The render lock disables the
	// button, and even if a click slips through, the store's first-wins no-op
	// backs it. Mutation-check: a store that re-answered (dropping first-wins)
	// reddens the chosenIds equality; a lost winner highlight reddens the DOM legs.
	test("clicking a losing option on a settled ask is a no-op (first-wins holds)", () => {
		const { store, container } = mountChannelView(false);
		store.openChannel(STANDALONE_ASK.channelId);

		const question = STANDALONE_ASK.ask.questions[0];
		const winningId = question.options[0].id;
		store.answerAsk(
			STANDALONE_ASK.messageId,
			STANDALONE_ASK.ask.askId,
			question.questionId,
			winningId,
		);
		expect(chosenIds(store)).toEqual([winningId]);

		const options = askOptions(container);
		// Click a different option than the winner (the second, which lost).
		expect(options.length).toBeGreaterThan(1);
		fireEvent.click(options[1]);

		// The winner stands — no re-answer.
		expect(chosenIds(store)).toEqual([winningId]);
		const winner = options.find((o) => o.classList.contains("chosen"));
		expect(winner).toBeDefined();
		expect(winner?.getAttribute("aria-pressed")).toBe("true");
		// The clicked loser never became chosen.
		expect(options[1].classList.contains("chosen")).toBe(false);
		expect(options[1].getAttribute("aria-pressed")).toBe("false");
	});
});
