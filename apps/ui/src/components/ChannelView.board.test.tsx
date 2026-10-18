import { describe, expect, test } from "bun:test";
import { render } from "@solidjs/testing-library";
import { createSignal, flush } from "solid-js";
import { blockText, handleOf, pinnedMessages } from "../comms";
import type { Channel } from "../comms-stub";
import { STUB_ACCOUNTS, STUB_CHANNELS, STUB_COMMS_STATE } from "../comms-stub";
import { StoreContext } from "../context";
import { type AppStore, createAppStore } from "../store";
import { testQueryClient } from "../test-support";
import { ChannelView } from "./ChannelView";

// The channel header renders pinned messages in position order, without edit controls.

// The channel that carries a pinned board, derived from the fixture.
const BOARD_CHANNEL = (() => {
	const c = STUB_CHANNELS.find((ch) => (ch.pinnedEntries?.length ?? 0) > 0);
	if (!c) throw new Error("fixture has no channel with a pinned board");
	return c;
})();

// A channel with no pinned board — the no-strip contrast.
const NO_BOARD_CHANNEL = (() => {
	const c = STUB_CHANNELS.find(
		(ch) => (ch.pinnedEntries?.length ?? 0) === 0 && ch.membership !== "none",
	);
	if (!c) throw new Error("fixture has no channel without a pinned board");
	return c;
})();

// Mount with a reactive channel prop so tests can swap snapshots without refetching.
function mountChannelView(channel: () => Channel): {
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
				<ChannelView channel={channel()} />
			</StoreContext>
		);
	});
	return { store, container };
}

const boardItems = (container: HTMLElement): HTMLElement[] => [
	...container.querySelectorAll<HTMLElement>(".pinned-board .pinned-item"),
];

const byId = new Map(STUB_ACCOUNTS.map((a) => [a.id, a]));

describe("ChannelView pinned board (T8)", () => {
	// Derive expected items with the helper used by the component, not copied literals.
	const expected = pinnedMessages(BOARD_CHANNEL, STUB_COMMS_STATE.messages);

	test("pin strip renders one item per resolved pin, in position order, with author + text", () => {
		expect(expected.length).toBeGreaterThan(1); // the fixture pins ≥2

		const { container } = mountChannelView(() => BOARD_CHANNEL);
		const items = boardItems(container);
		expect(items.length).toBe(expected.length);

		items.forEach((item, i) => {
			const msg = expected[i];
			const author = item.querySelector(".pinned-item-author")?.textContent;
			const text = item.querySelector(".pinned-item-text")?.textContent;
			// Author handle, in position order — dropping the render or ignoring
			// order reddens.
			expect(author).toBe(`@${handleOf(byId, msg.authorAccountId)}`);
			// The pin's text snippet.
			const snippet = msg.blocks.map(blockText).find((t) => t.length > 0) ?? "";
			expect(text).toBe(snippet);
		});

		// Render-only invariant (Matt's ruling): the board is agent-managed and
		// the human client has NO board-write path. No interactive control may
		// live in the strip — a stray pin/unpin/edit button reddens here.
		expect(
			container.querySelectorAll(".pinned-board button, .pinned-item button")
				.length,
		).toBe(0);
	});

	test("a channel with no pinnedEntries renders no strip", () => {
		expect(NO_BOARD_CHANNEL.pinnedEntries ?? []).toEqual([]);
		const { container } = mountChannelView(() => NO_BOARD_CHANNEL);
		expect(container.querySelector(".pinned-board")).toBeNull();
	});

	// A new ChannelChanged snapshot delivers a channel value with different
	// pinnedEntries; the strip is a memo, so it re-derives with no refetch. Start
	// with no pins (no strip), then swap in the board channel's pins → the strip
	// appears with one item per resolved pin.
	test("pin strip renders and updates on ChannelChanged", () => {
		const [channel, setChannel] = createSignal<Channel>({
			...BOARD_CHANNEL,
			pinnedEntries: undefined,
		});
		const { container } = mountChannelView(channel);
		expect(container.querySelector(".pinned-board")).toBeNull();

		// The ChannelChanged snapshot: same channel now carrying the board.
		setChannel({
			...BOARD_CHANNEL,
			pinnedEntries: BOARD_CHANNEL.pinnedEntries,
		});
		flush();
		const items = boardItems(container);
		expect(items.length).toBe(expected.length);
		expect(items[0].querySelector(".pinned-item-author")?.textContent).toBe(
			`@${handleOf(byId, expected[0].authorAccountId)}`,
		);
	});
});
