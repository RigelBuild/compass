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
						// The scroller (`.conv-stream`) is a flex column, so the sizer
						// would otherwise flex-shrink below its set height when the
						// virtual height exceeds the viewport — collapsing scrollHeight
						// to one screen and stranding later rows. Pin it to its own
						// height so the full scroll range is always reachable.
						"flex-shrink": "0",
						position: "relative",
						width: "100%",
					}}
				>
					<For each={virtualizer.getVirtualItems()}>
						{(item) => {
							// During a data change the Solid adapter reconciles the virtual
							// item store keyed by index, so a row can transiently receive an
							// undefined item or an index past the new message list. Guard both
							// so a mid-change tick renders nothing rather than throwing.
							const message = () => (item ? messages()[item.index] : undefined);
							return (
								<Show when={item && message()}>
									<div
										class="conv-row"
										// data-index is intentionally set BOTH here and imperatively
										// in the ref below: the ref wins on timing (fires before
										// Solid flushes this binding, so it is present at measure
										// time), and this binding keeps it in the declared attribute
										// set. Do not drop either as "redundant".
										data-index={item.index}
										data-key={item.key}
										ref={(el) => {
											// The measure observer reads data-index off the node, so
											// set it before measuring (the ref fires before Solid
											// flushes the attribute binding).
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
