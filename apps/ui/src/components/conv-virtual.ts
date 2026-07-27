import type { Thread } from "../comms";

// The conversation stream's chat-mode virtualization contract. The scroll
// behavior is TanStack Virtual's chat mode, adopted rather
// than hand-rolled: end-anchored with follow-on-append, keyed by thread ROOT id
// so prepend re-anchoring can re-find items. These constants and the options
// factory are extracted pure so the wiring is pinned by a unit assertion even
// where happy-dom's absent layout hides the pixel math.

/** The at-bottom slack (px) governing isAtEnd() / followOnAppend: how close to
 *  the end the viewport must be to count as "pinned to latest" (≈ one message
 *  row). Product-feel, tunable. */
export const CONV_SCROLL_END_THRESHOLD = 80;

/** Base per-thread height estimate (px): a root message row plus its reply
 *  affordance / summary pill. Corrected by measureElement once a row lays out. */
export const CONV_ESTIMATE_BASE = 96;

/** Per-reply increment (px) folded into the estimate. Replies render as a
 *  compact summary, not inline bodies, so this is a small nudge — the real
 *  height comes from measureElement; the estimate only seeds positioning. */
export const CONV_ESTIMATE_PER_REPLY = 8;

/** Rows to render beyond the visible window on each side, so a fast scroll does
 *  not flash blank before the next tick measures. */
export const CONV_OVERSCAN = 6;

/** A cheap per-thread size estimate: base row + a small per-reply nudge. The
 *  virtualizer runs variable-size, so this is only the seed measureElement
 *  corrects to the real laid-out height. */
export function estimateThreadSize(thread: Thread): number {
	return CONV_ESTIMATE_BASE + thread.replies.length * CONV_ESTIMATE_PER_REPLY;
}

/** The chat-mode option fields for the thread-list virtualizer: end-anchored,
 *  follow-on-append, root-id keyed, variable-size.
 *  Pure over a `threads` accessor so a config regression is caught by a unit
 *  test with no DOM. The component supplies the reactive `count` +
 *  `getScrollElement` and spreads this in. */
export function threadVirtualizerOptions(threads: () => Thread[]): {
	getItemKey: (index: number) => string;
	estimateSize: (index: number) => number;
	anchorTo: "end";
	followOnAppend: true;
	scrollEndThreshold: number;
	overscan: number;
} {
	return {
		// Root message id, never the index — TanStack's chat guidance ("Do not use
		// index keys for chat history"): prepend anchoring re-finds items by key.
		// Optional-chained because the core invokes these callbacks INTERNALLY
		// (measureElement → indexFromElement → getItemKey; getMeasurements →
		// estimateSize) with indices from its PREVIOUS measurement pass — so when
		// `threads` shrinks (channel switch to empty/shorter, last message
		// removed) the core can fire them with an index past the new length before
		// the reconcile prunes the item store. The JSX render guard cannot cover
		// these out-of-JSX calls, so tolerate a transient undefined here.
		getItemKey: (index) => threads()[index]?.root.id ?? String(index),
		estimateSize: (index) => {
			const thread = threads()[index];
			return thread ? estimateThreadSize(thread) : CONV_ESTIMATE_BASE;
		},
		anchorTo: "end",
		followOnAppend: true,
		scrollEndThreshold: CONV_SCROLL_END_THRESHOLD,
		overscan: CONV_OVERSCAN,
	};
}
