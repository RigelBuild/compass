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
import { MarkdownText } from "./MarkdownText";
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

/** An inline async ask (comms.proto Ask): a question with selectable options,
 *  answerable in place — never a blocking modal. A single-select question locks
 *  once answered; a multi-select stays open so choices can toggle.
 *
 *  The wire send is GATED on completeness in the store: the server accepts
 *  exactly ONE RespondToAsk per ask, so clicks accumulate locally and the
 *  completing click ships them all at once. A question the user means to SKIP
 *  would never complete the ask, so a partially answered ask grows a `submit`
 *  control that ships what is answered with the skipped questions empty. Once
 *  the ask is submitted it is settled on the wire and every option locks. */
const AskBlock: Component<{
	messageId: string;
	ask: Ask;
}> = (props) => {
	const store = useStore();
	const ask = () => props.ask;
	const chosen = (q: AskQuestion, optionId: string) =>
		q.chosenOptionIds.includes(optionId);
	// The ask's one respond has been issued: it is settled server-side, so no
	// further click may record an answer the server will never receive.
	const submitted = () => store.isAskSubmitted(ask().askId);
	// The last refusal for this ask, if any. A refused respond rolls the local
	// answer back, so without this the user's click just disappears — the same
	// hole the composer's error span closes for a failed post.
	const error = () => store.askError(ask().askId);
	// A single-select question is settled once answered; further clicks are
	// locked. A submitted ask locks every question outright.
	const locked = (q: AskQuestion) =>
		submitted() || (!q.allowMultiple && q.chosenOptionIds.length > 0);
	const answeredCount = () =>
		ask().questions.filter((q) => q.chosenOptionIds.length > 0).length;
	// The skip affordance is meaningful only in between: an untouched ask has
	// nothing to submit, and a complete one has already been sent by its
	// completing click.
	const canSubmit = () =>
		!submitted() &&
		answeredCount() > 0 &&
		answeredCount() < ask().questions.length;

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
			<Show when={canSubmit()}>
				<div class="ask-submit-row">
					<button
						type="button"
						class="ask-submit"
						title="Send this ask now, leaving the unanswered questions blank. An ask can only be answered once."
						onClick={() => store.submitAsk(props.messageId, ask().askId)}
					>
						submit — skip the rest
					</button>
				</div>
			</Show>
			<Show when={error()}>
				{(msg) => (
					<span class="ask-error" role="alert">
						{msg()}
					</span>
				)}
			</Show>
		</div>
	);
};

/** One durable conversation block — markdown text (with mention chips + code
 *  highlighting) or an inline ask. The rich session blocks
 *  (thought/tool_call/plan/diff) are not in the channel; they render in the
 *  session observation panel. */
const Block: Component<{
	messageId: string;
	block: ConvBlock;
	byHandle: Map<string, Account>;
}> = (props) => (
	<Show
		when={props.block.kind === "ask" ? props.block : undefined}
		fallback={
			<MarkdownText text={blockText(props.block)} byHandle={props.byHandle} />
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

/** The composer: posts a root message to the channel through the wire
 *  `PostMessage` (store.postMessage). NOTHING is inserted locally — the stored
 *  message arrives on the SubscribeComms echo, deduped by id, so it renders
 *  exactly once.
 *
 *  Carries the @-mention affordance: typing `@` is how you reach an agent's
 *  immediate attention (a steer); a plain message reaches a subscribed agent at
 *  its turn end.
 *
 *  Failure keeps the user's text: the draft is cleared OPTIMISTICALLY so the
 *  input frees up immediately, and restored verbatim if the post rejects (with
 *  the error rendered beside the field). A later keystroke wins over the
 *  restore — the user's in-flight typing is never clobbered by a stale reject. */
const Composer: Component<{ channel: Channel }> = (props) => {
	const store = useStore();
	const [draft, setDraft] = createSignal("");
	const [error, setError] = createSignal<string | null>(null);
	const placeholder = () => {
		if (props.channel.membership === "none") return "Join to post…";
		const target = isDm(props.channel)
			? `@${props.channel.name}`
			: `#${props.channel.name}`;
		return `Message ${target} — @ to mention or steer an agent`;
	};
	const send = () => {
		const text = draft().trim();
		if (!text || props.channel.membership === "none") return;
		setError(null);
		setDraft("");
		store.postMessage(props.channel.id, text).catch((e: unknown) => {
			setError(e instanceof Error ? e.message : String(e));
			// Restore only into an empty field: if the user has started typing the
			// next message, their text stands and the failed one is theirs to
			// recover from the error, not something we splice in mid-word.
			setDraft((current) => (current === "" ? text : current));
		});
	};
	return (
		<div class="conv-composer">
			<input
				class="field"
				placeholder={placeholder()}
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
								{/* A FRESH composer per channel. The enclosing `<Show>` is
								    unkeyed — Solid memoizes its condition on truthiness, so
								    switching between two truthy channels does NOT re-run these
								    children — which would leave one Composer instance holding
								    channel A's draft/error while `props.channel` points at B,
								    posting a private draft into the wrong channel. Keying on
								    the id remounts the composer (fresh signals) and nothing
								    else: the stream above keeps its scroll position.

								    The child MUST declare its parameter: `Show` only invokes a
								    children function of arity > 0, and returns a zero-arg one
								    as a reactive getter instead — which re-renders in place
								    and defeats the keying. */}
								<Show when={chan().id} keyed>
									{(_channelId) => <Composer channel={chan()} />}
								</Show>
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
