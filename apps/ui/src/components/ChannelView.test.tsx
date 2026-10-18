import { describe, expect, test } from "bun:test";
import { fireEvent, render } from "@solidjs/testing-library";
import { flush } from "solid-js";
import {
	type Account,
	type Ask,
	type Message,
	STUB_ACCOUNTS,
	STUB_CHANNELS,
	STUB_COMMS_STATE,
	STUB_MESSAGES,
	STUB_TOPICS,
} from "../comms-stub";
import { StoreContext } from "../context";
import { type AppStore, createAppStore } from "../store";
import { testQueryClient } from "../test-support";
import { ViewContext } from "../view-scope";
import { ChannelView, MessageRow } from "./ChannelView";
import { TopicView } from "./TopicView";

// Standalone-channel asks stay answerable; fixtures derive the channel and author.

// Derive the standalone ask from fixtures to tolerate fixture reshuffles.
function standaloneChannelAsk(): {
	channelId: string;
	topicId: string;
	messageId: string;
	ask: Ask;
	authorAccountId: string;
} {
	const channelKind = new Map(STUB_CHANNELS.map((c) => [c.id, c.kind]));
	const topicChannel = new Map(STUB_TOPICS.map((t) => [t.id, t.channelId]));
	for (const m of STUB_MESSAGES) {
		const channelId = topicChannel.get(m.topicId);
		if (channelId === undefined || channelKind.get(channelId) !== "channel") {
			continue;
		}
		for (const b of m.blocks) {
			if (b.kind === "ask") {
				return {
					channelId,
					topicId: m.topicId,
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
	return account.handle; // "compass-server" for ask-s4-integration
})();

// Build the store inside render so its memos are disposed with the test.
function mountTopicView(): {
	store: AppStore;
	container: HTMLElement;
} {
	let store!: AppStore;
	const { container } = render(() => {
		store = createAppStore({
			initialComms: STUB_COMMS_STATE,
			queryClient: testQueryClient(),
		});
		return (
			<StoreContext value={store}>
				<ViewContext value={store.focusedView()}>
					<TopicView />
				</ViewContext>
			</StoreContext>
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
	// Options stay enabled and no read-only hint appears for a standalone-channel ask.
	test("an ask in a standalone channel renders answerable (options enabled, no read-only hint)", () => {
		const { store, container } = mountTopicView();
		store.openTopic(STANDALONE_ASK.topicId);
		flush();

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

	// A click records the selected answer without routing to another workspace.
	test("an ask in a standalone channel records the answer on click", () => {
		const { store, container } = mountTopicView();
		store.openTopic(STANDALONE_ASK.topicId);
		flush();

		const options = askOptions(container);
		expect(options.length).toBeGreaterThan(0);
		expect(chosenIds(store)).toEqual([]); // starts unanswered

		fireEvent.click(options[0]);
		flush();

		const winningId = STANDALONE_ASK.ask.questions[0].options[0].id;
		// The store recorded the click …
		expect(chosenIds(store)).toEqual([winningId]);
		// … and the button reflects the choice.
		expect(options[0].getAttribute("aria-pressed")).toBe("true");
		expect(options[0].classList.contains("chosen")).toBe(true);
	});

	// A settled ask disables every option and preserves the winning choice.
	test("a settled single-select ask on the standalone surface renders locked (all disabled, winner chosen)", () => {
		const { store, container } = mountTopicView();
		store.openTopic(STANDALONE_ASK.topicId);
		flush();

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
		flush();
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

	// Two interactive mounts render the same structure and keep the ask answerable.
	test("the same ask stays answerable in a second (interactive) mount, and messages render identically", () => {
		// Mount the same channel again for a render comparison.
		const secondMount = mountTopicView();
		secondMount.store.openTopic(STANDALONE_ASK.topicId);
		flush();

		// Default interactive mount.
		const { store, container } = mountTopicView();
		store.openTopic(STANDALONE_ASK.topicId);
		flush();

		const options = askOptions(container);
		expect(options.length).toBeGreaterThan(0);

		// Options are enabled in the interactive path …
		for (const option of options) {
			expect(option.disabled).toBe(false);
		}

		// … and clicking one records the choice (single-select → exactly it).
		fireEvent.click(options[0]);
		flush();
		expect(chosenIds(store)).toEqual([
			STANDALONE_ASK.ask.questions[0].options[0].id,
		]);

		// Messages render identically in both mounts: same message-row count. The
		// ask block never differs in structure between mounts.
		const count = (root: HTMLElement, sel: string): number =>
			root.querySelectorAll(sel).length;
		expect(count(container, ".msg")).toBe(count(secondMount.container, ".msg"));
		// Non-triviality: the topic actually has message content to compare, so an
		// "identical" pass can't be two empty renders agreeing.
		expect(count(container, ".msg")).toBeGreaterThan(0);
	});
});

describe("MessageRow anatomy", () => {
	// The kind tag names the author's kind in Compass's domain words, so an
	// agent post and a human post read apart without author colors.
	const tagOf = (kind: Account["kind"]): string | null | undefined => {
		const author: Account = {
			id: "acc-a",
			handle: "a",
			displayName: "a",
			kind,
		};
		const msg: Message = {
			id: "m1",
			topicId: "t",
			authorAccountId: author.id,
			atUnixMs: 0,
			blocks: [{ kind: "text", text: "hi" }],
		};
		const byId = new Map([[author.id, author]]);
		const { container } = render(() => (
			<MessageRow msg={msg} byId={byId} byHandle={new Map()} />
		));
		return container.querySelector(".msg .msg-tag")?.textContent;
	};

	test("an agent row shows tag AGENT", () => {
		expect(tagOf("agent")).toBe("AGENT");
	});
	test("a human row shows tag HUMAN", () => {
		expect(tagOf("user")).toBe("HUMAN");
	});
	// Assistive tech reads adjacent spans as one word unless text separates them.
	test("the head reads author, tag, flag and time as separate words", () => {
		const author: Account = {
			id: "acc-a",
			handle: "cook",
			displayName: "cook",
			kind: "agent",
		};
		const msg: Message = {
			id: "m1",
			topicId: "t",
			authorAccountId: author.id,
			atUnixMs: 0,
			blocks: [{ kind: "text", text: "@cook rebase" }],
		};
		const { container } = render(() => (
			<MessageRow
				msg={msg}
				byId={new Map([[author.id, author]])}
				byHandle={new Map([[author.handle, author]])}
			/>
		));
		const words = container
			.querySelector(".msg-head")
			?.textContent?.trim()
			.split(/\s+/);
		expect(words?.slice(0, 3)).toEqual(["cook", "AGENT", "STEER"]);
	});
});

// On the interactive surface, only locked() disables a settled ask.
describe("ChannelView settled-state lock (T3)", () => {
	// A settled single-select ask disables all options and keeps the winner chosen.
	test("a settled single-select ask renders all options disabled with the winner chosen", () => {
		const { store, container } = mountTopicView();
		store.openTopic(STANDALONE_ASK.topicId);
		flush();

		const question = STANDALONE_ASK.ask.questions[0];
		const winningId = question.options[0].id;
		store.answerAsk(
			STANDALONE_ASK.messageId,
			STANDALONE_ASK.ask.askId,
			question.questionId,
			winningId,
		);
		flush();

		const options = askOptions(container);
		expect(options.length).toBeGreaterThan(0);
		for (const option of options) {
			expect(option.disabled).toBe(true);
		}
		const winner = options.find((o) => o.classList.contains("chosen"));
		expect(winner).toBeDefined();
		expect(winner?.getAttribute("aria-pressed")).toBe("true");
	});

	// A losing click cannot replace the first answer, even if it reaches the store.
	test("clicking a losing option on a settled ask is a no-op (first-wins holds)", () => {
		const { store, container } = mountTopicView();
		store.openTopic(STANDALONE_ASK.topicId);
		flush();

		const question = STANDALONE_ASK.ask.questions[0];
		const winningId = question.options[0].id;
		store.answerAsk(
			STANDALONE_ASK.messageId,
			STANDALONE_ASK.ask.askId,
			question.questionId,
			winningId,
		);
		flush();
		expect(chosenIds(store)).toEqual([winningId]);

		const options = askOptions(container);
		// Click a different option than the winner (the second, which lost).
		expect(options.length).toBeGreaterThan(1);
		fireEvent.click(options[1]);
		flush();

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

// ChannelView is a composerless topic index; only NewTopic writes at channel level.
describe("ChannelView is a composerless topic index (T5 model boundary)", () => {
	// Mount the real channel topic index over the offline fixture store.
	function mountChannelView(): { store: AppStore; container: HTMLElement } {
		let store!: AppStore;
		const { container } = render(() => {
			store = createAppStore({
				initialComms: STUB_COMMS_STATE,
				queryClient: testQueryClient(),
			});
			return (
				<StoreContext value={store}>
					<ViewContext value={store.focusedView()}>
						<ChannelView />
					</ViewContext>
				</StoreContext>
			);
		});
		return { store, container };
	}

	test("the channel surface renders topic rows + new-topic but NO message composer", () => {
		const { store, container } = mountChannelView();
		store.openChannel(STANDALONE_ASK.channelId);
		flush();

		const index = container.querySelector(".topic-index");
		expect(index).not.toBeNull();
		// Precondition: the index actually rendered rows (proves the composer-
		// absence assertion below is meaningful, not an empty-render false pass).
		expect(index?.querySelectorAll(".topic-row").length).toBeGreaterThan(0);
		// The sole channel-level write affordance is present…
		expect(index?.querySelector(".new-topic")).not.toBeNull();
		// …and NO message composer leaks onto the channel surface.
		expect(container.querySelector(".conv-composer")).toBeNull();
	});
});

// The real store must resolve @compass from its account map, not a test-local map.
const SYSTEM_MENTION = (() => {
	const system = STUB_ACCOUNTS.find((a) => a.kind === "system");
	if (!system) {
		throw new Error(
			"fixture has no kind:'system' account — the @compass end-to-end test needs one",
		);
	}
	const token = `@${system.handle}`; // "@compass"
	for (const m of STUB_MESSAGES) {
		for (const b of m.blocks) {
			if (b.kind === "text" && b.text.includes(token)) {
				return { topicId: m.topicId, token };
			}
		}
	}
	throw new Error(
		`fixture has no message mentioning ${token} — the @compass end-to-end test needs one`,
	);
})();

describe("@compass system sender resolves as a known mention end-to-end (T5)", () => {
	test("a fixture @compass mention chips as known through the real store", () => {
		const { store, container } = mountTopicView();
		store.openTopic(SYSTEM_MENTION.topicId);
		flush();

		const chip = [
			...container.querySelectorAll<HTMLElement>(".mention-chip"),
		].find((c) => c.textContent === SYSTEM_MENTION.token);
		// Precondition: the mention actually rendered (proves the class assertions
		// below are meaningful, not an empty-render false pass).
		expect(chip).toBeDefined();
		expect(chip?.classList.contains("reserved")).toBe(false);
		expect(chip?.classList.contains("unknown")).toBe(false);
	});
});
