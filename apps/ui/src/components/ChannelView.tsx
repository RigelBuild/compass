import {
	type Component,
	createMemo,
	createSignal,
	For,
	Index,
	Show,
} from "solid-js";
import {
	blockText,
	channelGlyph,
	dmLabel,
	handleOf,
	isDm,
	parseMentions,
	type Thread,
	threadSummary,
	threadsOf,
} from "../comms";
import type {
	Account,
	Ask,
	AskQuestion,
	Channel,
	ConvBlock,
	Message,
} from "../comms-stub";
import { useStore } from "../context";
import { ThreadPanel } from "./ThreadPanel";
import { ThreadStream } from "./ThreadStream";

/** UTC HH:MM for a message timestamp — deterministic, locale-independent (the
 *  fixture pins atUnixMs to a fixed clock; formatting to local time would make
 *  the render machine-dependent). */
function hhmm(atUnixMs: number): string {
	const d = new Date(atUnixMs);
	const h = String(d.getUTCHours()).padStart(2, "0");
	const m = String(d.getUTCMinutes()).padStart(2, "0");
	return `${h}:${m}`;
}

/** Render a text block as alternating plain runs and `@`-mention chips. A
 *  reserved mention (@everyone/@agents/@users) and a mention resolving to a
 *  known account both chip; an unresolved mention chips too but reads muted. */
const MentionText: Component<{
	text: string;
	byHandle: Map<string, Account>;
}> = (props) => {
	// Recompute on text change; the parse is cheap and the block is immutable per
	// render, so a memo would add ceremony without saving work.
	const parts = () => {
		const mentions = parseMentions(props.text);
		const out: {
			text: string;
			mention?: { handle: string; known: boolean; reserved: boolean };
		}[] = [];
		let cursor = 0;
		for (const men of mentions) {
			if (men.start > cursor)
				out.push({ text: props.text.slice(cursor, men.start) });
			out.push({
				text: props.text.slice(men.start, men.end),
				mention: {
					handle: men.handle,
					known: props.byHandle.has(men.handle.toLowerCase()),
					reserved: men.reserved,
				},
			});
			cursor = men.end;
		}
		if (cursor < props.text.length)
			out.push({ text: props.text.slice(cursor) });
		return out;
	};

	return (
		<span class="msg-text">
			<For each={parts()}>
				{(part) => (
					<Show when={part.mention} fallback={part.text}>
						{(m) => (
							<span
								class="mention-chip"
								classList={{
									reserved: m().reserved,
									unknown: !m().reserved && !m().known,
								}}
							>
								{part.text}
							</span>
						)}
					</Show>
				)}
			</For>
		</span>
	);
};

/** An inline async ask (comms.proto Ask): a question with selectable options,
 *  answerable in place — never a blocking modal. A single-select ask locks once
 *  answered; a multi-select stays open so choices can toggle. */
const AskBlock: Component<{
	messageId: string;
	ask: Ask;
}> = (props) => {
	const store = useStore();
	const ask = () => props.ask;
	const chosen = (q: AskQuestion, optionId: string) =>
		q.chosenOptionIds.includes(optionId);
	// A single-select question is settled once answered; further clicks are locked.
	const locked = (q: AskQuestion) =>
		!q.allowMultiple && q.chosenOptionIds.length > 0;

	return (
		<div
			class="block-ask"
			classList={{ answered: ask().questions.every((q) => locked(q)) }}
		>
			<Index each={ask().questions}>
				{(q) => (
					<>
						<div class="ask-question">{q().question}</div>
						<div class="ask-hint">
							{q().allowMultiple ? "choose any" : "choose one"} · async — answer
							when ready
						</div>
						<div class="ask-options">
							<For each={q().options}>
								{(option) => (
									<button
										type="button"
										class="ask-option"
										classList={{ chosen: chosen(q(), option.id) }}
										disabled={locked(q())}
										onClick={() => {
											store.answerAsk(
												props.messageId,
												ask().askId,
												q().questionId,
												option.id,
											);
										}}
										aria-pressed={chosen(q(), option.id)}
									>
										{option.label}
										<Show when={option.description}>
											<span class="ask-option-desc">{option.description}</span>
										</Show>
									</button>
								)}
							</For>
						</div>
					</>
				)}
			</Index>
		</div>
	);
};

/** One durable conversation block — text (with mention chips) or an inline ask.
 *  The rich ACP blocks (thought/tool_call/plan/diff) are not in the channel;
 *  they live in the session observation panel. */
const Block: Component<{
	messageId: string;
	block: ConvBlock;
	byHandle: Map<string, Account>;
}> = (props) => (
	<Show
		when={props.block.kind === "ask" ? props.block : undefined}
		fallback={
			<MentionText text={blockText(props.block)} byHandle={props.byHandle} />
		}
	>
		{(askBlock) => (
			<AskBlock messageId={props.messageId} ask={askBlock().ask} />
		)}
	</Show>
);

/** One message: author handle + time, then its blocks. `agent`/`user` styling
 *  distinguishes the poster kind. */
export const MessageRow: Component<{
	msg: Message;
	byId: Map<string, Account>;
	byHandle: Map<string, Account>;
}> = (props) => {
	const author = () => props.byId.get(props.msg.authorAccountId);
	const roleClass = () => (author()?.kind === "agent" ? "agent" : "user");
	return (
		<div class="msg" classList={{ [roleClass()]: true }}>
			<div class="msg-head">
				<span class="msg-role">
					{handleOf(props.byId, props.msg.authorAccountId)}
				</span>
				<span class="msg-at">{hhmm(props.msg.atUnixMs)}</span>
			</div>
			<Index each={props.msg.blocks}>
				{(block) => (
					<Block
						messageId={props.msg.id}
						block={block()}
						byHandle={props.byHandle}
					/>
				)}
			</Index>
		</div>
	);
};

/** One thread's stream row (Slack model): the root message, then EITHER a
 *  "reply" affordance (zero-reply roots stay startable) OR a compact
 *  `.thread-summary` pill (count · people pile · last-reply time) that opens the
 *  thread panel. Reply bodies never render inline — they live in the panel. */
export const ThreadView: Component<{
	thread: Thread;
	byId: Map<string, Account>;
	byHandle: Map<string, Account>;
}> = (props) => {
	const store = useStore();
	const summary = createMemo(() => threadSummary(props.thread));
	return (
		<div class="thread">
			<div class="thread-root">
				<MessageRow
					msg={props.thread.root}
					byId={props.byId}
					byHandle={props.byHandle}
				/>
				<Show when={props.thread.replies.length === 0}>
					<button
						type="button"
						class="thread-reply"
						onClick={() => store.openThread(props.thread.root.id)}
					>
						reply
					</button>
				</Show>
			</div>
			<Show when={props.thread.replies.length > 0}>
				<button
					type="button"
					class="thread-summary"
					onClick={() => store.openThread(props.thread.root.id)}
				>
					<span class="thread-summary-count">
						{summary().replyCount === 1
							? "1 reply"
							: `${summary().replyCount} replies`}
					</span>
					<span class="thread-summary-people">
						<For each={summary().participantIds.slice(0, 5)}>
							{(id) => (
								<span
									role="img"
									class="thread-summary-avatar"
									title={`@${handleOf(props.byId, id)}`}
									aria-label={`@${handleOf(props.byId, id)}`}
								>
									{handleOf(props.byId, id).charAt(0).toUpperCase()}
								</span>
							)}
						</For>
						<Show when={summary().participantIds.length > 5}>
							<span class="thread-summary-overflow">
								{`+${summary().participantIds.length - 5}`}
							</span>
						</Show>
					</span>
					<span class="thread-summary-time">
						{`last ${hhmm(summary().lastReplyAtUnixMs)}`}
					</span>
				</button>
			</Show>
		</div>
	);
};

/** The channel header: glyph + name/topic, the caller's membership state, and
 *  (for a channel the caller hasn't joined) a join prompt over the conversation. */
const ChannelHeader: Component<{
	channel: Channel;
	byId: Map<string, Account>;
}> = (props) => {
	const store = useStore();
	const label = () =>
		isDm(props.channel)
			? dmLabel(props.channel, store.caller().id, props.byId)
			: props.channel.name;
	return (
		<header class="conv-head">
			<span class="conv-glyph" aria-hidden="true">
				{channelGlyph(props.channel.kind)}
			</span>
			<span class="conv-name">{label()}</span>
			<Show when={props.channel.topic}>
				<span class="conv-topic">{props.channel.topic}</span>
			</Show>
			<span class="conv-spacer" />
			<span class="conv-membership" data-m={props.channel.membership}>
				{props.channel.membership}
			</span>
		</header>
	);
};

/** The composer (non-functional in the mockup — posting lands with PostMessage).
 *  Carries the @-mention affordance: typing `@` is how you reach an agent's
 *  immediate attention (a steer); a plain message reaches a subscribed agent at
 *  its turn end. */
const Composer: Component<{ channel: Channel }> = (props) => {
	const [draft, setDraft] = createSignal("");
	const placeholder = () => {
		if (props.channel.membership === "none") return "Join to post…";
		const target = isDm(props.channel)
			? `@${props.channel.name}`
			: `#${props.channel.name}`;
		return `Message ${target} — @ to mention or steer an agent`;
	};
	return (
		<div class="conv-composer">
			<input
				class="field"
				placeholder={placeholder()}
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
			>
				send
			</button>
		</div>
	);
};

/** The conversation center — a channel's threaded text+ask conversation, with
 *  the composer pinned at the bottom. This is where user-directed communication
 *  lives: a surface *within* the board-primary shell (the standalone `channel`
 *  view or the agent workspace's chat pane), not a replacement for the board.
 *
 *  The channel source is either an explicit `channel` prop (the agent
 *  workspace passes its bound home DM, so the pane can never drift onto the
 *  standalone surface's selection) or, when the prop is absent, the global
 *  `selectedChannel` (the standalone channel mount). The presence check is on
 *  the prop KEY, not its value: an explicit `undefined` channel is the
 *  workspace's empty state, NOT a fall-through to `selectedChannel` — that
 *  fall-through is what would let a standalone channel bleed into the
 *  interactive workspace pane (D3). */
export const ChannelView: Component<{
	channel?: Channel | undefined;
}> = (props) => {
	const store = useStore();
	// Bound to the prop when the caller passes one (workspace), else the global
	// selection (standalone). `"channel" in props` keeps an explicit undefined
	// from falling through to selectedChannel().
	const channel = (): Channel | undefined =>
		"channel" in props ? props.channel : store.selectedChannel();
	const byId = () => new Map(store.accounts().map((a) => [a.id, a]));
	const byHandle = () =>
		new Map(store.accounts().map((a) => [a.handle.toLowerCase(), a]));
	const threads = () => {
		const chan = channel();
		return chan ? threadsOf(store.messages(), chan.id) : [];
	};

	return (
		<section class="conversation">
			<Show
				when={channel()}
				fallback={<div class="conv-empty">Select a channel to start.</div>}
			>
				{(chan) => (
					<>
						<ChannelHeader channel={chan()} byId={byId()} />
						<div class="conv-body-row">
							<div class="conv-main">
								<ThreadStream
									threads={threads()}
									channelId={chan().id}
									byId={byId()}
									byHandle={byHandle()}
									emptyMessage={
										chan().membership === "none"
											? "Join to read this channel."
											: "No messages yet."
									}
								/>
								<Composer channel={chan()} />
							</div>
							<ThreadPanel
								channel={chan()}
								byId={byId()}
								byHandle={byHandle()}
							/>
						</div>
					</>
				)}
			</Show>
		</section>
	);
};
