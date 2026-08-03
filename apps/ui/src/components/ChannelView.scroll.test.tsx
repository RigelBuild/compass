import { afterEach, beforeEach, describe, expect, test } from "bun:test";
import { render } from "@solidjs/testing-library";
import { createSignal } from "solid-js";
import type { Account, Message } from "../comms-stub";
import { StoreContext } from "../context";
import { createAppStore } from "../store";
import {
	CONV_ESTIMATE_BASE,
	CONV_SCROLL_END_THRESHOLD,
	estimateMessageSize,
	messageVirtualizerOptions,
} from "./conv-virtual";
import { MessageStream } from "./MessageStream";

// The conversation stream's scroll contract, designed fresh
// (ChannelView had NO scroll management before this lane). The behavior is
// TanStack Virtual's chat mode; these tests pin the CONTRACT, not pixel math.
// The T5 two-level model made the stream a FLAT message list (a topic IS the
// thread), so MessageStream renders Message[] directly — the same virtualizer
// contract, keyed by MESSAGE id.
//
// happy-dom has no layout: a scroll element's real rect is 0×0 and item
// offsetHeight is 0, so the virtualizer produces an empty window unmodified
// (verified this session). The suite injects geometry — a stubbed offsetHeight
// giving .conv-stream a viewport and each rendered message a row height — plus a
// scrollTop/clientHeight/scrollHeight + scrollTo emulation so scrollToEnd and
// element-size mocks / measureElement stubs the scroll contract needs. What
// happy-dom genuinely cannot exercise (real ResizeObserver
// layout, sub-pixel scroll coupling, the real webview's scroll-element identity)
// is deferred to manual-QA / browser-mode.

const VIEWPORT_H = 500;
const ROW_H = 100;

// Per-element scroll offset (happy-dom has no layout, so scrollTop is not backed
// by anything). A WeakMap keyed by the element gives each scroller its own live
// offset that scrollTo writes and the offset observer reads.
const scrollTops = new WeakMap<HTMLElement, number>();

let restoreGeometry: (() => void) | undefined;

// Install browser-like geometry on the div prototype so it is ACTIVE DURING
// MOUNT — the virtualizer's scope-id effect calls scrollToEnd() on mount, and
// that needs clientHeight/scrollHeight/scrollTo present to actually move the
// offset. offsetHeight gives .conv-stream a viewport and each [data-index] row a
// height; scrollHeight reads the component's .conv-sizer height (real browser
// semantics: scrollHeight = content height); scrollTo writes scrollTop and fires
// the scroll event the offset observer listens for.
beforeEach(() => {
	const proto = Object.getPrototypeOf(document.createElement("div"));
	const saved = new Map<string, PropertyDescriptor | undefined>();
	const define = (
		prop: string,
		desc: PropertyDescriptor & { get?: (this: HTMLElement) => unknown },
	) => {
		saved.set(prop, Object.getOwnPropertyDescriptor(proto, prop));
		Object.defineProperty(proto, prop, { configurable: true, ...desc });
	};

	define("offsetHeight", {
		get(this: HTMLElement) {
			if (this.classList?.contains("conv-stream")) return VIEWPORT_H;
			if (this.hasAttribute?.("data-index")) return ROW_H;
			return 0;
		},
	});
	define("clientHeight", {
		get(this: HTMLElement) {
			return this.classList?.contains("conv-stream") ? VIEWPORT_H : 0;
		},
	});
	define("scrollHeight", {
		get(this: HTMLElement) {
			if (!this.classList?.contains("conv-stream")) return 0;
			// The content height the component sizes .conv-sizer to (getTotalSize()).
			const sizer = this.querySelector<HTMLElement>(".conv-sizer");
			const h = sizer?.style.height ?? "";
			return Number.parseInt(h, 10) || 0;
		},
	});
	define("scrollTop", {
		get(this: HTMLElement) {
			return scrollTops.get(this) ?? 0;
		},
		set(this: HTMLElement, v: number) {
			scrollTops.set(this, v);
		},
	});
	saved.set("scrollTo", Object.getOwnPropertyDescriptor(proto, "scrollTo"));
	Object.defineProperty(proto, "scrollTo", {
		configurable: true,
		value(this: HTMLElement, arg: number | ScrollToOptions) {
			const next = typeof arg === "number" ? arg : (arg?.top ?? this.scrollTop);
			this.scrollTop = next;
			this.dispatchEvent(new Event("scroll"));
		},
	});

	restoreGeometry = () => {
		for (const [prop, desc] of saved) {
			if (desc) Object.defineProperty(proto, prop, desc);
			// No prior own descriptor → remove ours so the prop reverts to the
			// global test-setup shim / happy-dom inherited version. NEVER set a
			// value here: a data value:0 would shadow the inherited scrollTo
			// function and make the virtualizer's reconcile rAF throw after teardown.
			else delete (proto as Record<string, unknown>)[prop];
		}
	};
});

afterEach(() => {
	restoreGeometry?.();
	restoreGeometry = undefined;
});

function scrollToTop(el: HTMLElement): void {
	el.scrollTo({ top: 0 });
}

// Read the vertical offset a virtual row is positioned at. Rows are absolutely
// positioned via `transform: translateY(<start>px)`; parse that back out. The
// row's viewport-relative position is this minus the scroller's scrollTop.
function translateY(el: HTMLElement): number {
	const m = /translateY\(([-\d.]+)px\)/.exec(el.style.transform);
	return m ? Number.parseFloat(m[1]) : 0;
}

// ── Synthetic messages (no store dependency; MessageStream takes messages directly).
const ACC: Account = {
	id: "acc-matt",
	handle: "matt",
	displayName: "Matt",
	kind: "user",
};
const byId = new Map<string, Account>([[ACC.id, ACC]]);
const byHandle = new Map<string, Account>([[ACC.handle, ACC]]);

function msg(id: string, atUnixMs: number, text = id): Message {
	return {
		id,
		topicId: "top-x",
		authorAccountId: ACC.id,
		atUnixMs,
		blocks: [{ kind: "text", text }],
	};
}

function makeMessages(n: number, startMs = 1_000): Message[] {
	return Array.from({ length: n }, (_, i) =>
		msg(`sm-${i}`, startMs + i * 1_000),
	);
}

function mountStream(initial: Message[]): {
	messages: () => Message[];
	setMessages: (m: Message[]) => void;
	setScopeId: (id: string) => void;
	container: HTMLElement;
	scroller: () => HTMLElement;
	rows: () => HTMLElement[];
	indices: () => number[];
	keys: () => (string | null)[];
} {
	const [messages, setMessages] = createSignal<Message[]>(initial);
	const [scopeId, setScopeId] = createSignal("top-x");
	const store = createAppStore();
	const { container } = render(() => (
		<StoreContext.Provider value={store}>
			<MessageStream
				messages={messages()}
				scopeId={scopeId()}
				byId={byId}
				byHandle={byHandle}
				emptyMessage="No messages yet."
			/>
		</StoreContext.Provider>
	));
	const scroller = () => container.querySelector(".conv-stream") as HTMLElement;
	const rows = () => [
		...container.querySelectorAll<HTMLElement>(".conv-stream [data-index]"),
	];
	const indices = () => rows().map((r) => Number(r.getAttribute("data-index")));
	const keys = () => rows().map((r) => r.getAttribute("data-key"));
	return {
		messages,
		setMessages,
		setScopeId,
		container,
		scroller,
		rows,
		indices,
		keys,
	};
}

describe("MessageStream chat-mode config", () => {
	// Case (7): the chat-mode config is CONSTRUCTED correctly. Asserted against
	// the pure options factory so a config regression (index keys, wrong anchor,
	// dropped follow-on-append) reddens even where happy-dom's absent layout hides
	// the scroll math. The load-bearing wiring assertion.
	test("(7) options are the chat-mode contract (end-anchored, message-id keyed, follow-on-append)", () => {
		const messages = () => [msg("sm-0", 1_000), msg("msg-b", 2_000)];
		const opts = messageVirtualizerOptions(messages);
		expect(opts.anchorTo).toBe("end");
		expect(opts.followOnAppend).toBe(true);
		expect(opts.scrollEndThreshold).toBe(CONV_SCROLL_END_THRESHOLD);
		expect(opts.getItemKey(0)).toBe("sm-0");
		expect(opts.getItemKey(1)).toBe("msg-b"); // MESSAGE id, never the index
	});

	test("(7b) estimateMessageSize returns the base estimate", () => {
		expect(estimateMessageSize(msg("sm-0", 1_000))).toBe(CONV_ESTIMATE_BASE);
	});
});

describe("MessageStream scroll contract", () => {
	// Case (5): a long topic renders only a bounded window of message rows.
	test("(5) only a bounded window of message rows is in the DOM for a long topic", () => {
		const { rows } = mountStream(makeMessages(200));
		const n = rows().length;
		expect(n).toBeGreaterThan(0);
		expect(n).toBeLessThan(50); // far fewer than 200 → virtualized
	});

	// Case (1): opening a topic lands at latest (end-anchored mount). The final
	// message's row is within the rendered window.
	test("(1) opening lands at the latest message (end-anchored)", () => {
		const { indices } = mountStream(makeMessages(200));
		expect(Math.max(...indices())).toBe(199);
	});

	// Case (2): append while at bottom follows to the new latest.
	test("(2) append while at bottom follows to the new latest", () => {
		const { setMessages, indices } = mountStream(makeMessages(200));
		setMessages(makeMessages(201));
		expect(Math.max(...indices())).toBe(200);
	});

	// Case (3): append while scrolled up past the threshold does NOT yank down.
	test("(3) append while scrolled up does not yank the view down", () => {
		const { setMessages, scroller, indices } = mountStream(makeMessages(200));
		scrollToTop(scroller());
		const beforeMax = Math.max(...indices());
		setMessages(makeMessages(201));
		const afterMax = Math.max(...indices());
		// The window's top index must not advance toward the new end at all:
		// because we are scrolled up past the end-threshold, followOnAppend does
		// not fire, and the appended message sorts BELOW the current window, so it
		// never enters the rendered range.
		expect(afterMax).toBeLessThanOrEqual(beforeMax);
	});

	// Case (4): prepend older messages keeps the visible message anchored by
	// message id/key — the SAME message the user was viewing stays at the same
	// viewport-relative position (its translateY minus the scroller's scrollTop),
	// rather than jumping by the number of prepended rows.
	//
	// This is the behavioral proof of MESSAGE-ID keying: TanStack chat mode
	// re-finds the scroll anchor by key after the list changes and adjusts
	// scrollTop so the anchor stays put. Under index keys the "anchor key" (e.g.
	// 105) still resolves to index 105 — now a DIFFERENT message after a 50-item
	// prepend — so the virtualizer sees no move and the message the user was
	// reading jumps up by 50 rows. Identify the anchor by its rendered CONTENT
	// (message id text), NOT by data-key, so the assertion is robust to the key
	// scheme and reddens when keying regresses to indices.
	test("(4) prepend older messages keeps the visible message anchored", () => {
		const { setMessages, scroller, rows } = mountStream(makeMessages(200));
		scroller().scrollTo({ top: 100 * ROW_H }); // park mid-list (~index 100)

		const before = rows();
		expect(before.length).toBeGreaterThan(0);
		// Anchor on a specific message near the middle of the window, tracked by
		// its id (rendered as the message text) so we follow the SAME message.
		const anchorRow = before[Math.floor(before.length / 2)];
		const anchorIndex = Number(anchorRow.getAttribute("data-index"));
		const anchorId = `sm-${anchorIndex}`;
		expect(anchorRow.textContent).toContain(anchorId);
		const beforeViewportPos = translateY(anchorRow) - scroller().scrollTop;

		// Prepend 50 older messages (earlier timestamps → sort before the originals).
		const older: Message[] = Array.from({ length: 50 }, (_, i) =>
			msg(`older-${i}`, 1 + i),
		);
		setMessages([...older, ...makeMessages(200)]);

		// The same message must still be rendered AND at the same viewport-relative
		// position. Find it by content, not index (its index shifted by 50).
		const anchorAfter = rows().find((r) => r.textContent?.includes(anchorId));
		expect(anchorAfter).toBeDefined();
		const afterViewportPos =
			translateY(anchorAfter as HTMLElement) - scroller().scrollTop;
		// Same viewport position within a row (measurement/overscan slack). Index
		// keying would shift it by ~50 * ROW_H, far outside this tolerance.
		expect(Math.abs(afterViewportPos - beforeViewportPos)).toBeLessThan(ROW_H);
	});

	// Case (6): switching the selected scope resets to the new scope's latest
	// via a scope-id keyed effect (not a remount — .conv-stream persists). Driven
	// by swapping the messages + scopeId prop, as openTopic does upstream.
	test("(6) switching scope resets to the new scope's latest", () => {
		const [messages, setMessages] = createSignal<Message[]>(
			makeMessages(200, 1_000),
		);
		const [scopeId, setScopeId] = createSignal("top-a");
		const store = createAppStore();
		const { container } = render(() => (
			<StoreContext.Provider value={store}>
				<MessageStream
					messages={messages()}
					scopeId={scopeId()}
					byId={byId}
					byHandle={byHandle}
					emptyMessage="No messages yet."
				/>
			</StoreContext.Provider>
		));
		// Switch scope: new messages + new scopeId (the .conv-stream stays mounted).
		setMessages(
			makeMessages(200, 5_000).map((m) => ({ ...m, id: `${m.id}-b` })),
		);
		setScopeId("top-b");
		const rowsB = [
			...container.querySelectorAll<HTMLElement>(".conv-stream [data-index]"),
		];
		expect(rowsB.length).toBeGreaterThan(0);
		expect(
			Math.max(...rowsB.map((r) => Number(r.getAttribute("data-index")))),
		).toBe(199); // B's final message is in view
	});

	// Case (8): shrinking a large WINDOWED topic to empty ([]) renders the empty
	// state instead of throwing. REGRESSION for the HIGH crash finding: the
	// virtualizer core invokes getItemKey/estimateSize INTERNALLY with indices
	// from its previous measurement pass, so when the message list shrinks the
	// core fires them with `index >= new length` before Solid reconciles. The JSX
	// Show guard does NOT cover these out-of-JSX core calls. Must run under real
	// windowing (the suite's geometry harness), NOT the global 100_000px shim, so
	// the core actually has a prior measurement window to replay stale indices from.
	test("(8) shrinking a windowed topic to empty renders the empty state, no throw", () => {
		const { setMessages, scroller, container, rows } = mountStream(
			makeMessages(2000),
		);
		// Confirm it is genuinely windowed (a stale measurement pass exists to
		// replay) — far fewer than 2000 rows are in the DOM.
		expect(rows().length).toBeGreaterThan(0);
		expect(rows().length).toBeLessThan(2000);
		// Scroll away from the end so the core's live window sits at high indices;
		// shrinking then makes it replay those (now out-of-range) indices through
		// the callbacks before Solid prunes the item store — the crash path.
		scrollToTop(scroller());
		expect(() => {
			setMessages([]);
		}).not.toThrow();
		expect(container.querySelector(".conv-empty")).not.toBeNull();
		expect(rows().length).toBe(0);
		// The scroller survives the transition (not torn down / errored out).
		expect(scroller()).not.toBeNull();
	});

	// Case (9): switching from a large windowed topic to a much SHORTER one (with
	// a scopeId change, as openTopic does upstream) renders the short topic's
	// bounded rows instead of throwing on the stale-index core callbacks. Same
	// regression as (8) for the 2000→5 shrink path.
	test("(9) switching to a much shorter scope renders it, no throw on stale index", () => {
		const { setMessages, setScopeId, scroller, indices, rows } = mountStream(
			makeMessages(2000),
		);
		expect(rows().length).toBeLessThan(2000); // windowed
		// Park the window at high indices so the shrink replays out-of-range ones.
		scrollToTop(scroller());
		const shortScope = makeMessages(5, 9_000).map((m) => ({
			...m,
			id: `${m.id}-b`,
		}));
		expect(() => {
			setMessages(shortScope);
			setScopeId("top-b");
		}).not.toThrow();
		const idx = indices();
		expect(idx.length).toBeGreaterThan(0);
		// Every rendered index is bounded by the new (5-message) length.
		expect(Math.max(...idx)).toBeLessThanOrEqual(4);
		expect(Math.min(...idx)).toBeGreaterThanOrEqual(0);
	});

	// Case (10): the .conv-sizer carries the flex-shrink guard so the virtual
	// scroll range survives .conv-stream being a flex column. REGRESSION for the
	// P1 (SEA-1332 / PR #886): the scroller is `display: flex; flex-direction:
	// column`, so an unpinned sizer flex-shrinks below its set getTotalSize()
	// height once the content exceeds the viewport — collapsing scrollHeight to
	// one screen and stranding every message past the first viewport.
	//
	// This is a DOM-CONTRACT guard, not a behavioral one: happy-dom has no flex
	// layout engine, so the sizer's *measured* height never actually collapses
	// in-test regardless of the guard (which is exactly why the pre-fix bug slipped
	// past the green windowing suite above). So we assert the rendered node still
	// carries `flex-shrink: 0` inline — the entry that pins it in a real browser.
	test("(10) the sizer is pinned with flex-shrink:0 so its scroll range cannot collapse", () => {
		const { container } = mountStream(makeMessages(200));
		const sizer = container.querySelector<HTMLElement>(".conv-sizer");
		expect(sizer).not.toBeNull();
		// Precondition: content is genuinely taller than the viewport, so a
		// collapsing sizer WOULD strand rows — the guard is load-bearing here.
		const sizerHeight = Number.parseInt(
			(sizer as HTMLElement).style.height,
			10,
		);
		expect(sizerHeight).toBeGreaterThan(VIEWPORT_H);
		// The guard itself: reddens the moment the `"flex-shrink": "0"` style entry
		// is dropped from MessageStream's .conv-sizer.
		expect((sizer as HTMLElement).style.flexShrink).toBe("0");
	});
});
