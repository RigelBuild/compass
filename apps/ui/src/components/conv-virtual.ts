import type { Message } from "../comms-stub";

// The conversation stream's chat-mode virtualization contract. TanStack Virtual's chat
// mode, adopted rather than hand-rolled: end-anchored with follow-on-append, keyed by
// message id so prepend re-anchoring re-finds items. Extracted pure so the wiring is pinned
// by a unit assertion even where happy-dom's absent layout hides the pixel math.

/** The at-bottom slack (px) governing isAtEnd() / followOnAppend: how close to
 *  the end the viewport must be to count as "pinned to latest" (≈ one message
 *  row). Product-feel, tunable. */
export const CONV_SCROLL_END_THRESHOLD = 80;

/** Per-message height estimate (px): a message row with its author head + body.
 *  Corrected by measureElement once a row lays out. */
export const CONV_ESTIMATE_BASE = 96;

/** Rows to render beyond the visible window on each side, so a fast scroll does
 *  not flash blank before the next tick measures. */
export const CONV_OVERSCAN = 6;

/** A cheap per-message size estimate. The virtualizer runs variable-size, so
 *  this is only the seed measureElement corrects to the real laid-out height. */
export function estimateMessageSize(_message: Message): number {
	return CONV_ESTIMATE_BASE;
}

/** The chat-mode option fields for the message-list virtualizer: end-anchored,
 *  follow-on-append, id-keyed, variable-size.
 *  Pure over a `messages` accessor so a config regression is caught by a unit
 *  test with no DOM. The component supplies the reactive `count` +
 *  `getScrollElement` and spreads this in. */
export function messageVirtualizerOptions(messages: () => Message[]): {
	getItemKey: (index: number) => string;
	estimateSize: (index: number) => number;
	anchorTo: "end";
	followOnAppend: true;
	scrollEndThreshold: number;
	overscan: number;
} {
	return {
		// Message id, never the index — TanStack's chat guidance: prepend anchoring re-finds
		// items by key. Optional-chained because the core invokes these callbacks INTERNALLY
		// with indices from its PREVIOUS measurement pass, so when `messages` shrinks it can
		// fire past the new length before reconcile prunes — tolerate a transient undefined.
		getItemKey: (index) => messages()[index]?.id ?? String(index),
		estimateSize: (index) => {
			const message = messages()[index];
			return message ? estimateMessageSize(message) : CONV_ESTIMATE_BASE;
		},
		anchorTo: "end",
		followOnAppend: true,
		scrollEndThreshold: CONV_SCROLL_END_THRESHOLD,
		overscan: CONV_OVERSCAN,
	};
}
