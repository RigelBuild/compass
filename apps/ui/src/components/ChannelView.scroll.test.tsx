import {
	afterEach,
	beforeEach,
	describe,
	expect,
	type Mock,
	spyOn,
	test,
} from "bun:test";
import * as solidVirtual from "@rigelbuild/solid-virtual";
import { render } from "@solidjs/testing-library";
import { createSignal, flush } from "solid-js";
import type { Account, Message } from "../comms-stub";
import { StoreContext } from "../context";
import { createAppStore } from "../store";
import { testQueryClient } from "../test-support";
import {
	CONV_ESTIMATE_BASE,
	CONV_SCROLL_END_THRESHOLD,
	estimateMessageSize,
	messageVirtualizerOptions,
} from "./conv-virtual";
import { MessageStream } from "./MessageStream";

// MessageStream keeps its virtualizer private; a pass-through spy on the real
// factory (behavior unchanged) exposes the measurement cache for inspection.
let lastVirtualizer:
	| { elementsCache: Map<solidVirtual.VirtualItem["key"], Element> }
	| undefined;
let virtualizerSpy: Mock<typeof solidVirtual.createVirtualizer> | undefined;

function captureVirtualizer(): void {
	const real = solidVirtual.createVirtualizer;
	virtualizerSpy = spyOn(solidVirtual, "createVirtualizer").mockImplementation(
		(options) => {
			const instance = real(options);
			lastVirtualizer = instance;
			return instance;
		},
	);
}

// Test virtual scrolling with injected geometry because happy-dom has no layout.

const VIEWPORT_H = 500;
const ROW_H = 100;
// .conv-stream's vertical padding (app.css: 12px top and bottom), which a
// browser counts in scrollHeight.
const STREAM_PAD_Y = 24;

// happy-dom does not back scrollTop, so track each scroller's offset separately.
const scrollTops = new WeakMap<HTMLElement, number>();

let restoreGeometry: (() => void) | undefined;

// Install geometry before mount because the virtualizer scrolls to the end on mount.
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
			// The content height the component sizes .conv-sizer to (getTotalSize())
			// plus the scroller's padding, floored at clientHeight as a browser does.
			const sizer = this.querySelector<HTMLElement>(".conv-sizer");
			const h = sizer?.style.height ?? "";
			const content = (Number.parseInt(h, 10) || 0) + STREAM_PAD_Y;
			return Math.max(content, this.clientHeight);
		},
	});
	define("scrollTop", {
		get(this: HTMLElement) {
			return scrollTops.get(this) ?? 0;
		},
		set(this: HTMLElement, v: number) {
			// A browser clamps scrollTop to [0, scrollHeight - clientHeight].
			const max = this.scrollHeight - this.clientHeight;
			scrollTops.set(this, Math.min(Math.max(v, 0), Math.max(max, 0)));
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
	virtualizerSpy?.mockRestore();
	virtualizerSpy = undefined;
	lastVirtualizer = undefined;
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

function mountStream(
	initial: Message[],
	initialShown = true,
): {
	messages: () => Message[];
	setMessages: (m: Message[]) => void;
	setScopeId: (id: string) => void;
	setShown: (shown: boolean) => void;
	container: HTMLElement;
	scroller: () => HTMLElement;
	rows: () => HTMLElement[];
	indices: () => number[];
	keys: () => (string | null)[];
} {
	const [messages, setMessages] = createSignal<Message[]>(initial);
	const [scopeId, setScopeId] = createSignal("top-x");
	const [shown, setShown] = createSignal(initialShown);
	const { container } = render(() => (
		<StoreContext value={createAppStore({ queryClient: testQueryClient() })}>
			<MessageStream
				messages={messages()}
				scopeId={scopeId()}
				shown={shown()}
				byId={byId}
				byHandle={byHandle}
				emptyMessage="No messages yet."
			/>
		</StoreContext>
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
		setShown,
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

	// A stream mounted in a hidden view has no viewport, so its mount-time
	// scroll lands nowhere; becoming shown must re-anchor it to the latest.
	test("a stream that becomes shown re-anchors to the latest message", () => {
		const { setShown, scroller, indices } = mountStream(
			makeMessages(200),
			false,
		);
		scrollToTop(scroller());
		flush();
		expect(Math.max(...indices())).toBeLessThan(199);
		setShown(true);
		flush();
		expect(Math.max(...indices())).toBe(199);
	});

	// Only the first reveal re-anchors; returning to a tab keeps where the user
	// had scrolled to.
	test("a later return to a shown stream keeps the user's scroll position", () => {
		const { setShown, scroller, indices } = mountStream(makeMessages(200));
		scrollToTop(scroller());
		flush();
		const before = Math.max(...indices());
		expect(before).toBeLessThan(199);
		setShown(false);
		flush();
		setShown(true);
		flush();
		expect(scroller().scrollTop).toBe(0);
		expect(Math.max(...indices())).toBe(before);
	});

	// Case (2): append while at bottom follows to the new latest.
	test("(2) append while at bottom follows to the new latest", () => {
		const { setMessages, indices } = mountStream(makeMessages(200));
		setMessages(makeMessages(201));
		flush();
		expect(Math.max(...indices())).toBe(200);
	});

	// Case (3): append while scrolled up past the threshold does NOT yank down.
	test("(3) append while scrolled up does not yank the view down", () => {
		const { setMessages, scroller, indices } = mountStream(makeMessages(200));
		scrollToTop(scroller());
		const beforeMax = Math.max(...indices());
		setMessages(makeMessages(201));
		const afterMax = Math.max(...indices());
		// Appending below the window must not move it when the reader is scrolled up.
		expect(afterMax).toBeLessThanOrEqual(beforeMax);
	});

	// Prepending keeps the same message at the same viewport position via message-ID keys.
	test("(4) prepend older messages keeps the visible message anchored", () => {
		const { setMessages, scroller, rows } = mountStream(makeMessages(200));
		scroller().scrollTo({ top: 100 * ROW_H }); // park mid-list (~index 100)

		const before = rows();
		expect(before.length).toBeGreaterThan(0);
		// Anchor on a specific message near the middle of the window, tracked by
		// its id so we follow the same message after the prepend.
		const anchorRow = before[Math.floor(before.length / 2)];
		const anchorIndex = Number(anchorRow.getAttribute("data-index"));
		const anchorId = `sm-${anchorIndex}`;
		expect(anchorRow.textContent).toContain(anchorId);
		const beforeViewportPos = translateY(anchorRow) - scroller().scrollTop;

		// Prepend older messages before the original sequence.
		const older: Message[] = Array.from({ length: 50 }, (_, i) =>
			msg(`older-${i}`, 1 + i),
		);
		setMessages([...older, ...makeMessages(200)]);

		// Find the same message by content because its index shifted by 50.
		const anchorAfter = rows().find((r) => r.textContent?.includes(anchorId));
		expect(anchorAfter).toBeDefined();
		const afterViewportPos =
			translateY(anchorAfter as HTMLElement) - scroller().scrollTop;
		expect(Math.abs(afterViewportPos - beforeViewportPos)).toBeLessThan(ROW_H);
	});

	// A scope change resets the existing scroller to the new topic's latest message.
	test("(6) switching scope resets to the new scope's latest", () => {
		const [messages, setMessages] = createSignal<Message[]>(
			makeMessages(200, 1_000),
		);
		const [scopeId, setScopeId] = createSignal("top-a");
		const { container } = render(() => (
			<StoreContext value={createAppStore({ queryClient: testQueryClient() })}>
				<MessageStream
					messages={messages()}
					scopeId={scopeId()}
					shown
					byId={byId}
					byHandle={byHandle}
					emptyMessage="No messages yet."
				/>
			</StoreContext>
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

	// A topic switch reuses the rendered rows by index; a reused row now shows a
	// different message, so it must be measured again or it keeps the estimate.
	test("switching scope re-measures reused rows so message spacing stays uniform", () => {
		const proto = Object.getPrototypeOf(document.createElement("div"));
		const geometry = Object.getOwnPropertyDescriptor(proto, "offsetHeight");
		Object.defineProperty(proto, "offsetHeight", {
			configurable: true,
			get(this: HTMLElement) {
				if (!this.hasAttribute?.("data-index"))
					return geometry?.get?.call(this);
				return this.textContent?.includes("tall") ? 160 : 40;
			},
		});
		const { setMessages, setScopeId, rows } = mountStream([
			msg("a-0", 1_000, "short a"),
			msg("a-1", 2_000, "short b"),
		]);
		setMessages([
			msg("b-0", 3_000, "short c"),
			msg("b-1", 4_000, "tall d"),
			msg("b-2", 5_000, "short e"),
		]);
		setScopeId("top-b");
		flush();
		expect(rows().map(translateY)).toEqual([0, 40, 200]);
	});

	// A reused row must leave its previous key's cache entry; otherwise one node
	// piles up under every key it has shown and is pinned after it detaches.
	test("a reused row is cached only under the key it currently shows", () => {
		captureVirtualizer();
		const { setMessages, setScopeId, rows } = mountStream([
			msg("a-0", 1_000),
			msg("a-1", 2_000),
		]);
		for (const scope of ["b", "c", "d"]) {
			setMessages([msg(`${scope}-0`, 3_000), msg(`${scope}-1`, 4_000)]);
			setScopeId(`top-${scope}`);
			flush();
		}
		const cache = lastVirtualizer?.elementsCache;
		expect(cache).toBeDefined();
		const live = rows();
		expect(live.map((r) => r.getAttribute("data-key"))).toEqual(["d-0", "d-1"]);
		for (const row of live) {
			expect(cache?.get(row.getAttribute("data-key") ?? "")).toBe(row);
		}
		for (const [key, el] of cache ?? []) {
			const shown = live.find((row) => row === el);
			if (shown) expect(key).toBe(shown.getAttribute("data-key") ?? "");
		}
	});

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
			flush();
		}).not.toThrow();
		expect(container.querySelector(".conv-empty")).not.toBeNull();
		expect(rows().length).toBe(0);
		// The scroller survives the transition (not torn down / errored out).
		expect(scroller()).not.toBeNull();
	});

	// Shrinking to five messages must not replay stale indexes from the previous window.
	test("(9) switching to a much shorter scope renders it, no throw on stale index", () => {
		const { setMessages, setScopeId, scroller, indices, rows } = mountStream(
			makeMessages(2000),
		);
		expect(rows().length).toBeLessThan(2000); // windowed
		// Park at high indexes so shrinking replays out-of-range values.
		scrollToTop(scroller());
		const shortScope = makeMessages(5, 9_000).map((m) => ({
			...m,
			id: `${m.id}-b`,
		}));
		expect(() => {
			setMessages(shortScope);
			setScopeId("top-b");
			flush();
		}).not.toThrow();
		const idx = indices();
		expect(idx.length).toBeGreaterThan(0);
		expect(Math.max(...idx)).toBeLessThanOrEqual(4);
		expect(Math.min(...idx)).toBeGreaterThanOrEqual(0);
	});

	// Keep the flex sizer from shrinking and collapsing the virtual scroll range.
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
