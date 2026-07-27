import { createVirtualizer } from "@tanstack/solid-virtual";
import { type Component, createEffect, For, on, Show } from "solid-js";
import type { Thread } from "../comms";
import type { Account } from "../comms-stub";
import { ThreadView } from "./ChannelView";
import { threadVirtualizerOptions } from "./conv-virtual";

/** The virtualized conversation stream. Owns `.conv-stream`
 *  — the single `overflow-y: auto` scroll element the layout's structural
 *  invariant requires — and renders the channel's threads through a
 *  `@tanstack/solid-virtual` chat-mode virtualizer: end-anchored (opens at
 *  latest, follows appends only when already at bottom), variable-size over the
 *  THREAD list, keyed by thread root id so prepend re-anchors without a jump.
 *
 *  Layout is the standard two-div pattern: `.conv-stream` (the scroller) wraps a
 *  relative sizer of `getTotalSize()` height, inside which each visible thread is
 *  absolutely positioned by `transform: translateY(item.start)`. Every rendered
 *  row registers `measureElement` so its real laid-out height corrects the
 *  estimate. `ThreadPanel` stays OUTSIDE this element (a sibling in ChannelView),
 *  so it never interferes with measurement. */
export const ThreadStream: Component<{
	threads: Thread[];
	/** The selected channel id — drives the reset-to-latest effect on switch. */
	channelId: string;
	byId: Map<string, Account>;
	byHandle: Map<string, Account>;
	/** Shown when the channel has no threads (join prompt vs. empty state). */
	emptyMessage: string;
}> = (props) => {
	let scrollEl!: HTMLDivElement;
	const threads = () => props.threads;

	const virtualizer = createVirtualizer({
		// Reactive count: appends/prepends re-run the measurement + range.
		get count() {
			return threads().length;
		},
		getScrollElement: () => scrollEl,
		...threadVirtualizerOptions(threads),
	});

	// Reset to latest on channel switch (and initial mount — the first fire).
	// Switching the selected channel does NOT remount `.conv-stream`, so a
	// mount-only scroll would leave the new channel at the old scroll offset; a
	// channel-id-keyed effect re-anchors each channel to its latest message.
	createEffect(
		on(
			() => props.channelId,
			() => {
				virtualizer.scrollToEnd();
			},
		),
	);

	return (
		<div class="conv-stream" ref={scrollEl}>
			<Show
				when={threads().length > 0}
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
							// undefined item or an index past the new thread list. Guard both
							// so a mid-change tick renders nothing rather than throwing.
							const thread = () => (item ? threads()[item.index] : undefined);
							return (
								<Show when={item && thread()}>
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
										<ThreadView
											thread={thread() as Thread}
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
