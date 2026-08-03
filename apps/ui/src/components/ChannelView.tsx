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
	type TopicGroup,
	topicSummary,
	topicsOf,
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
 *  the ask is SETTLED — our respond issued, or the server's own `answered` flag
 *  set by whoever answered it first — every option locks. */
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
	// The server burns an ask on the first RespondToAsk it ACCEPTS and refuses
	// every later one with ErrConflict (go/internal/store/messages.go:404-406),
	// so an ask arriving with `answered` set is CLOSED to this client whoever
	// closed it — another participant, or an earlier run of ours. `submitted`
	// cannot see those: it records only the responds THIS store issued. Offering
	// an enabled option on a closed ask promises a click that can only produce a
	// refusal, so the closed ask locks and the honest surface is a dead control
	// rather than a doomed RPC and an error span.
	const closed = () => submitted() || ask().answered;
	// A single-select question is settled once answered; further clicks are
	// locked. A settled ask locks every question outright.
	const locked = (q: AskQuestion) =>
		closed() || (!q.allowMultiple && q.chosenOptionIds.length > 0);
	const answeredCount = () =>
		ask().questions.filter((q) => q.chosenOptionIds.length > 0).length;
	// The skip affordance is meaningful only in between: an untouched ask has
	// nothing to submit, a complete one has already been sent by its completing
	// click, and a settled one takes no further respond at all.
	const canSubmit = () =>
		!closed() &&
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

/** The composer: posts a message through the wire `PostMessage`
 *  (store.postMessage) into the given `topic` oneof — an existing topic by id
 *  (the topic view) or a new topic by name (the channel index's "new topic"
 *  affordance). NOTHING is inserted locally — the stored
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
export const Composer: Component<{
	channel: Channel;
	topic:
		| { case: "topicId"; value: string }
		| { case: "topicName"; value: string };
	placeholder: string;
}> = (props) => {
	const store = useStore();
	const [draft, setDraft] = createSignal("");
	const [error, setError] = createSignal<string | null>(null);
	const send = () => {
		const text = draft().trim();
		if (!text || props.channel.membership === "none") return;
		setError(null);
		setDraft("");
		store
			.postMessage(props.channel.id, props.topic, text)
			.catch((e: unknown) => {
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
				placeholder={
					props.channel.membership === "none"
						? "Join to post…"
						: props.placeholder
				}
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

/** One topic-index row: the topic name + a compact activity summary (message
 *  count · people pile · last-activity time). Clicking drills into the topic's
 *  message view (store.openTopic). Read-only — the channel index posts nothing;
 *  the composer lives only in the topic view. */
const TopicRow: Component<{
	group: TopicGroup;
	byId: Map<string, Account>;
}> = (props) => {
	const store = useStore();
	const summary = createMemo(() => topicSummary(props.group));
	return (
		<button
			type="button"
			class="topic-row"
			onClick={() => store.openTopic(props.group.topic.id)}
		>
			<span class="topic-name">{props.group.topic.name}</span>
			<span class="topic-summary">
				<span class="topic-summary-count">
					{summary().messageCount === 1
						? "1 message"
						: `${summary().messageCount} messages`}
				</span>
				<span class="topic-summary-people">
					<For each={summary().participantIds.slice(0, 5)}>
						{(id) => (
							<span
								role="img"
								class="topic-summary-avatar"
								title={`@${handleOf(props.byId, id)}`}
								aria-label={`@${handleOf(props.byId, id)}`}
							>
								{handleOf(props.byId, id).charAt(0).toUpperCase()}
							</span>
						)}
					</For>
					<Show when={summary().participantIds.length > 5}>
						<span class="topic-summary-overflow">
							{`+${summary().participantIds.length - 5}`}
						</span>
					</Show>
				</span>
				<Show when={summary().messageCount > 0}>
					<span class="topic-summary-time">
						{`last ${hhmm(summary().lastActivityAtUnixMs)}`}
					</span>
				</Show>
			</span>
		</button>
	);
};

/** The "new topic" affordance: a topic-name field + a first-message field that
 *  posts the message into a get-or-create topic (PostMessage topic:topicName).
 *  It is the ONLY write path on the channel index — you cannot post into a
 *  channel, only a topic; naming a topic and posting its first message is one
 *  atomic act (the server creates the topic on the post). Failure keeps both the
 *  typed name and message (same restore contract as the composer). */
const NewTopic: Component<{ channel: Channel }> = (props) => {
	const store = useStore();
	const [name, setName] = createSignal("");
	const [message, setMessage] = createSignal("");
	const [error, setError] = createSignal<string | null>(null);
	const canStart = () =>
		props.channel.membership !== "none" &&
		name().trim().length > 0 &&
		message().trim().length > 0;
	const start = () => {
		const topicName = name().trim();
		const text = message().trim();
		if (!topicName || !text || props.channel.membership === "none") return;
		setError(null);
		setName("");
		setMessage("");
		store
			.postMessage(
				props.channel.id,
				{ case: "topicName", value: topicName },
				text,
			)
			.catch((e: unknown) => {
				setError(e instanceof Error ? e.message : String(e));
				setName((current) => (current === "" ? topicName : current));
				setMessage((current) => (current === "" ? text : current));
			});
	};
	return (
		<div class="new-topic">
			<input
				class="new-topic-name field"
				placeholder="New topic name…"
				value={name()}
				disabled={props.channel.membership === "none"}
				onInput={(e) => setName(e.currentTarget.value)}
			/>
			<input
				class="new-topic-message field"
				placeholder="First message…"
				value={message()}
				disabled={props.channel.membership === "none"}
				onInput={(e) => setMessage(e.currentTarget.value)}
				onKeyDown={(e) => {
					if (e.key === "Enter" && !e.shiftKey) {
						e.preventDefault();
						start();
					}
				}}
			/>
			<button
				type="button"
				class="new-topic-start"
				disabled={!canStart()}
				onClick={start}
			>
				new topic
			</button>
			<Show when={error()}>
				{(msg) => (
					<span class="new-topic-error" role="alert">
						{msg()}
					</span>
				)}
			</Show>
		</div>
	);
};

/** The channel's topic index (two-level Zulip model): the channel's topics as
 *  rows ordered last-activity-desc, each a name + activity summary, plus the
 *  "new topic" affordance. NO composer — you post into a topic, never a channel;
 *  clicking a row drills into that topic's message view. */
const TopicIndex: Component<{
	channel: Channel;
	byId: Map<string, Account>;
}> = (props) => {
	const store = useStore();
	const groups = createMemo(() =>
		topicsOf(store.topics(), store.messages(), props.channel.id),
	);
	return (
		<div class="conv-body-row">
			<div class="conv-main">
				<div class="topic-index">
					<Show
						when={groups().length > 0}
						fallback={
							<div class="conv-empty">
								{props.channel.membership === "none"
									? "Join to read this channel."
									: "No topics yet — start one."}
							</div>
						}
					>
						<For each={groups()}>
							{(group) => <TopicRow group={group} byId={props.byId} />}
						</For>
					</Show>
					<NewTopic channel={props.channel} />
				</div>
			</div>
		</div>
	);
};

/** The channel surface — ALWAYS a channel's TOPIC INDEX (topics + "new topic",
 *  no composer), for plain channels AND DMs alike. The model is uniform (Matt's
 *  ruling): a DM is topics too, and steering an agent means replying in a topic
 *  or starting a new one. You post into a topic, never a channel/DM directly, so
 *  this surface carries no composer; clicking a topic row drills into its
 *  message view (TopicView), which owns the composer.
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

	return (
		<section class="conversation">
			<Show
				when={channel()}
				fallback={<div class="conv-empty">Select a channel to start.</div>}
			>
				{(chan) => (
					<>
						<ChannelHeader channel={chan()} byId={byId()} />
						<TopicIndex channel={chan()} byId={byId()} />
					</>
				)}
			</Show>
		</section>
	);
};
