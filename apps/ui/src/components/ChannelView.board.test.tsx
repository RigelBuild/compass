import { describe, expect, test } from "bun:test";
import { render } from "@solidjs/testing-library";
import { createSignal } from "solid-js";
import { blockText, handleOf, pinnedMessages } from "../comms";
import type { Channel } from "../comms-stub";
import { STUB_ACCOUNTS, STUB_CHANNELS, STUB_COMMS_STATE } from "../comms-stub";
import { StoreContext } from "../context";
import { type AppStore, createAppStore } from "../store";
import { testQueryClient } from "../test-support";
import { ChannelView } from "./ChannelView";

// The pinned-board STRIP in the channel header (comms substrate §T8/§A3): the
// human client RENDERS the channel's pinned board — the "permanent thread"
// headline — but never edits/unpins/pins it (pins are agent-managed, Matt's
// ruling). These tests mount ChannelView over the offline fixture store and
// assert the render-only contract:
//   - a channel with pinnedEntries resolving to real messages renders one strip
//     item per resolved pin, in position order, carrying each pin's author +
//     text;
//   - a channel with no pinnedEntries renders NO strip;
//   - the strip re-derives reactively when the channel's pinnedEntries change on
//     a new ChannelChanged snapshot — no refetch.
//
// Fixture ground truth (grepped from comms-stub.ts, derived here so a reshuffle
// can't stale the test): `ch-announcements` is owner_only + carries
// pinnedEntries [msg-a1@0, msg-a2@1], both authored by acc-supervisor. An `open`
// channel with no board (ch-svc-compass) is the no-strip contrast.

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

// Mount ChannelView over the offline fixture store with an explicit `channel`
// prop (the workspace mount form), so the surface renders THAT channel's header
// deterministically rather than the global selection. The prop is a reactive
// accessor so a test can swap the channel value (a ChannelChanged snapshot) and
// observe the header re-derive with no refetch.
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
			<StoreContext.Provider value={store}>
				<ChannelView channel={channel()} />
			</StoreContext.Provider>
		);
	});
	return { store, container };
}

const boardItems = (container: HTMLElement): HTMLElement[] => [
	...container.querySelectorAll<HTMLElement>(".pinned-board .pinned-item"),
];

const byId = new Map(STUB_ACCOUNTS.map((a) => [a.id, a]));

describe("ChannelView pinned board (T8)", () => {
	// The expected render, derived through the SAME foundation helper the
	// component uses, so the assertion tracks the fixture rather than copied
	// literals.
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
		const items = boardItems(container);
		expect(items.length).toBe(expected.length);
		expect(items[0].querySelector(".pinned-item-author")?.textContent).toBe(
			`@${handleOf(byId, expected[0].authorAccountId)}`,
		);
	});
});
