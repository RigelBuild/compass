import { createVirtualizer } from "@rigelbuild/solid-virtual";
import { type Component, createEffect, For, Show } from "solid-js";
import type { Account, Message } from "../comms-stub";
import { MessageRow } from "./ChannelView";
import { messageVirtualizerOptions } from "./conv-virtual";

/** The virtualized conversation stream. Owns `.conv-stream`
 *  — the single `overflow-y: auto` scroll element the layout's structural
 *  invariant requires — and renders a topic's (or a DM's) flat message list
 *  through a `@rigelbuild/solid-virtual` chat-mode virtualizer: end-anchored (opens
 *  at latest, follows appends only when already at bottom), variable-size over
 *  the MESSAGE list, keyed by message id so prepend re-anchors without a jump.
 *
 *  Layout is the standard two-div pattern: `.conv-stream` (the scroller) wraps a
 *  relative sizer of `getTotalSize()` height, inside which each visible message is
 *  absolutely positioned by `transform: translateY(item.start)`. Every rendered
 *  row registers `measureElement` so its real laid-out height corrects the
 *  estimate. */
export const MessageStream: Component<{
	messages: Message[];
	/** The scope id — drives the reset-to-latest effect on switch (a channel id
	 *  for a DM, a topic id for a topic view). */
	scopeId: string;
	/** Whether the stream's view is on screen. A hidden mount has no viewport,
	 *  so it anchors to the latest message on its first reveal. */
	shown: boolean;
	byId: Map<string, Account>;
	byHandle: Map<string, Account>;
	/** Shown when the scope has no messages (join prompt vs. empty state). */
	emptyMessage: string;
}> = (props) => {
	let scrollEl!: HTMLDivElement;
	const messages = () => props.messages;

	const virtualizer = createVirtualizer({
		// Reactive count: appends/prepends re-run the measurement + range.
		get count() {
			return messages().length;
		},
		getScrollElement: () => scrollEl,
		...messageVirtualizerOptions(messages),
	});

	// Reset to latest on scope switch (and initial mount — the first fire).
	// Switching the selected scope does NOT remount `.conv-stream`, so a
	// mount-only scroll would leave the new scope at the old scroll offset; a
	// scope-id-keyed effect re-anchors each scope to its latest message.
	createEffect(
		() => props.scopeId,
		() => {
			virtualizer.scrollToEnd();
		},
	);
	// A stream mounted hidden could not measure, so it anchors once on its first
	// reveal; later returns keep the user's scroll position.
	let revealed = false;
	createEffect(
		() => props.shown,
		(shown, wasShown) => {
			if (!shown || revealed) return;
			revealed = true;
			if (wasShown === false) virtualizer.scrollToEnd();
		},
	);

	return (
		<div class="conv-stream" ref={scrollEl}>
			<Show
				when={messages().length > 0}
				fallback={<div class="conv-empty">{props.emptyMessage}</div>}
			>
				<div
					class="conv-sizer"
					style={{
						height: `${virtualizer.getTotalSize()}px`,
						// Prevent flex shrink from collapsing the virtual scroll range.
						"flex-shrink": "0",
						position: "relative",
						width: "100%",
					}}
				>
					<For each={virtualizer.getVirtualItems()}>
						{(item) => {
							// Guard stale or out-of-range virtual items during reconciliation.
							const message = () => (item ? messages()[item.index] : undefined);
							// Re-key the cached row when a scope switch reuses its element.
							let rowEl: HTMLDivElement | undefined;
							createEffect(
								() => item?.key,
								(key, prevKey) => {
									if (prevKey === undefined || key === prevKey || !rowEl)
										return;
									if (virtualizer.elementsCache.get(prevKey) === rowEl) {
										virtualizer.elementsCache.delete(prevKey);
									}
									virtualizer.measureElement(rowEl);
								},
							);
							return (
								<Show when={item && message()}>
									<div
										class="conv-row"
										// Keep data-index available before virtualizer measurement.
										data-index={item.index}
										data-key={item.key}
										ref={(el) => {
											rowEl = el;
											// Set data-index first because the ref precedes binding flush.
											el.setAttribute("data-index", String(item.index));
											virtualizer.measureElement(el);
										}}
										style={{
											position: "absolute",
											top: "0",
											left: "0",
											width: "100%",
											transform: `translateY(${item.start}px)`,
										}}
									>
										<MessageRow
											msg={message() as Message}
											byId={props.byId}
											byHandle={props.byHandle}
										/>
									</div>
								</Show>
							);
						}}
					</For>
				</div>
			</Show>
		</div>
	);
};
