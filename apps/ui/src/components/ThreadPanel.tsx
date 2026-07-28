import { type Component, createSignal, Index, Show } from "solid-js";
import { type Thread, threadsOf } from "../comms";
import type { Account, Channel } from "../comms-stub";
import { useStore } from "../context";
import { MessageRow } from "./ChannelView";

/** The thread-scoped composer: mirrors the main Composer's `.conv-composer`
 *  markup and membership-based enablement (OQ-1 §711-719 — a reply is text
 *  posting, never read-only in a joined channel), posting through
 *  store.postReply (the wire PostMessage with parentMessageId) under the thread
 *  root. No local insert: the SubscribeComms echo renders the reply. Same
 *  failure contract as the main composer — the typed text is restored into an
 *  empty field when the post rejects. */
const ThreadComposer: Component<{ channel: Channel; rootId: string }> = (
	props,
) => {
	const store = useStore();
	const [draft, setDraft] = createSignal("");
	const [error, setError] = createSignal<string | null>(null);
	const send = () => {
		const text = draft().trim();
		if (!text || props.channel.membership === "none") return;
		setError(null);
		setDraft("");
		store
			.postReply(props.channel.id, props.rootId, text)
			.catch((e: unknown) => {
				setError(e instanceof Error ? e.message : String(e));
				setDraft((current) => (current === "" ? text : current));
			});
	};
	return (
		<div class="conv-composer">
			<input
				class="field"
				placeholder="Reply…"
				value={draft()}
				disabled={props.channel.membership === "none"}
				onInput={(e) => setDraft(e.currentTarget.value)}
				onKeyDown={(e) => {
					if (e.key === "Enter" && !e.shiftKey) {
						e.preventDefault();
						send();
					}
				}}
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
			<Show when={error()}>
				{(msg) => (
					<span class="conv-composer-error" role="alert">
						{msg()}
					</span>
				)}
			</Show>
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
					{/* A FRESH composer per thread root, for the same reason the channel
					    composer is keyed (ChannelView.tsx): the enclosing `<Show>` is
					    unkeyed, so moving between two truthy threads reuses this
					    instance and its draft/error would post one thread's text under
					    another's root. */}
					<Show when={t().root.id} keyed>
						{(rootId) => (
							<ThreadComposer channel={props.channel} rootId={rootId} />
						)}
					</Show>
				</aside>
			)}
		</Show>
	);
};
