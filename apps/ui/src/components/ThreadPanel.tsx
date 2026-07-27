import { type Component, createSignal, Index, Show } from "solid-js";
import { type Thread, threadsOf } from "../comms";
import type { Account, Channel } from "../comms-stub";
import { useStore } from "../context";
import { MessageRow } from "./ChannelView";

/** The thread-scoped composer: mirrors the main Composer's `.conv-composer`
 *  markup and membership-based enablement (OQ-1 §711-719 — a reply is text
 *  posting, never read-only in a joined channel), posting through
 *  store.postReply under the thread root. */
const ThreadComposer: Component<{ channel: Channel; rootId: string }> = (
	props,
) => {
	const store = useStore();
	const [draft, setDraft] = createSignal("");
	const send = () => {
		store.postReply(props.channel.id, props.rootId, draft().trim());
		setDraft("");
	};
	return (
		<div class="conv-composer">
			<input
				class="field"
				placeholder="Reply…"
				value={draft()}
				disabled={props.channel.membership === "none"}
				onInput={(e) => setDraft(e.currentTarget.value)}
			/>
			<button
				type="button"
				class="send"
				disabled={
					props.channel.membership === "none" || draft().trim().length === 0
				}
				onClick={send}
			>
				send
			</button>
		</div>
	);
};

/** The thread panel — a split-beside-the-stream aside that ChannelView hosts,
 *  opened by a per-thread reply affordance and driven through the T-T1 store
 *  API (openThreadRootId / openThread / closeThread / postReply). Renders only
 *  when the open thread resolves to a thread in THIS channel; reuses MessageRow
 *  and `.thread-replies` so a posted reply appears in the panel ONLY — under the
 *  Slack model replies leave the main stream (both read store.messages()). */
export const ThreadPanel: Component<{
	channel: Channel;
	byId: Map<string, Account>;
	byHandle: Map<string, Account>;
}> = (props) => {
	const store = useStore();
	const thread = (): Thread | undefined => {
		const rootId = store.openThreadRootId();
		if (!rootId) return undefined;
		return threadsOf(store.messages(), props.channel.id).find(
			(t) => t.root.id === rootId,
		);
	};
	return (
		<Show when={thread()}>
			{(t) => (
				<aside class="thread-panel">
					<header class="thread-panel-head">
						<span class="thread-panel-title">Thread</span>
						<button
							type="button"
							class="thread-close"
							onClick={() => store.closeThread()}
						>
							close
						</button>
					</header>
					<div class="thread-panel-body">
						<MessageRow
							msg={t().root}
							byId={props.byId}
							byHandle={props.byHandle}
						/>
						<Show when={t().replies.length > 0}>
							<div class="thread-replies">
								<Index each={t().replies}>
									{(reply) => (
										<MessageRow
											msg={reply()}
											byId={props.byId}
											byHandle={props.byHandle}
										/>
									)}
								</Index>
							</div>
						</Show>
					</div>
					<ThreadComposer channel={props.channel} rootId={t().root.id} />
				</aside>
			)}
		</Show>
	);
};
