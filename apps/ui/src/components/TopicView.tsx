import { type Component, Show } from "solid-js";
import { channelGlyph, topicMessages } from "../comms";
import { useStore } from "../context";
import { Composer } from "./ChannelView";
import { MessageStream } from "./MessageStream";

/** The topic message view — one topic's messages, chronological, with the
 *  composer pinned at the bottom (the two-level Zulip model's leaf). Reached by
 *  the `/channel/:channelId/topic/:topicId` deep link (routes.tsx) or by drilling
 *  in from the channel index / a sidebar recent-topic row (store.openTopic). NO
 *  reply/threading — a topic IS the thread, so a post is just a message into this
 *  topic (PostMessage topic:topicId).
 *
 *  Reads the selection off the store (the route-sync's single-writer
 *  selectedTopic/selectedChannel), so the deep-link mount and the click path
 *  share one source of truth. */
export const TopicView: Component = () => {
	const store = useStore();
	const topic = () => store.selectedTopic();
	const channel = () => store.selectedChannel();
	const byId = () => new Map(store.accounts().map((a) => [a.id, a]));
	const byHandle = () => {
		const accounts = store.accounts();
		const byId = new Map(accounts.map((a) => [a.id, a]));
		const handles = new Map<string, (typeof accounts)[number]>();
		for (const account of accounts) {
			handles.set(account.handle.toLowerCase(), account);
			if (account.kind === "agent" && account.ownerUserId) {
				const ownerHandle = byId.get(account.ownerUserId)?.handle;
				if (ownerHandle) {
					handles.set(
						`${ownerHandle}/${account.handle}`.toLowerCase(),
						account,
					);
				}
			}
		}
		return handles;
	};
	const messages = () => {
		const t = topic();
		return t ? topicMessages(store.messages(), t.id) : [];
	};

	return (
		<section class="conversation topic-view">
			<Show
				when={topic()}
				fallback={<div class="conv-empty">Select a topic to start.</div>}
			>
				{(t) => (
					<>
						<header class="conv-head">
							<Show
								when={channel()}
								fallback={
									<span class="conv-glyph" aria-hidden="true">
										#
									</span>
								}
							>
								{(chan) => (
									<>
										<span class="conv-glyph" aria-hidden="true">
											{channelGlyph(chan().kind)}
										</span>
									</>
								)}
							</Show>
							<span class="conv-name">{t().name}</span>
							<Show when={channel()}>
								{(chan) => <span class="conv-topic">in {chan().name}</span>}
							</Show>
						</header>
						<div class="conv-body-row">
							<div class="conv-main">
								<MessageStream
									messages={messages()}
									scopeId={t().id}
									byId={byId()}
									byHandle={byHandle()}
									emptyMessage="No messages yet."
								/>
								{/* A FRESH composer per topic — keyed on the id so drilling
								    between two topics remounts it (fresh draft/error), never
								    carrying one topic's draft into another. The child MUST
								    declare its parameter so `Show` invokes it as a keyed
								    factory, not a getter. */}
								<Show when={channel()}>
									{(chan) => (
										<Show when={t().id} keyed>
											{(_topicId) => (
												<Composer
													channel={chan()}
													topic={{ case: "topicId", value: t().id }}
													placeholder={`Message #${t().name} — @ to mention or steer an agent`}
												/>
											)}
										</Show>
									)}
								</Show>
							</div>
						</div>
					</>
				)}
			</Show>
		</section>
	);
};
