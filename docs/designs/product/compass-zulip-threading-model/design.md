# Compass: Zulip threading model

Status: Active

> **Ratified by Matt 2026-08-02; no open questions remain**, then **refined by
> Matt 2026-08-02** with four follow-ups now folded in (see Decisions F8-F11):
> collapse the pre-dogfood SQL migrations rather than layer another (F8); remove
> superseded proto fields outright rather than `reserved` them (F9); a message
> holds only its `topic_id`, never a channel id — the channel is reached through
> the topic (F10); and the UI adopts Zulip's two-level drill-in — a channel
> shows only its topic index (no composer), a topic shows its messages plus the
> composer (no nested threading), and the left sidebar lists a channel's most
> recent topics (F11). Tracker: TBD. Lanes: compass-repo (proto), compass-server
> (store + comms), compass-agent (tools), compass-ui, compass-runner (gateway).
> **Supersedes by citation** (never rewrites the merged records):
> [compass-0.8-threading-and-session-renderer](../compass-0.8-threading-and-session-renderer/design.md),
> [compass-threading-ui](../compass-threading-ui/design.md),
> [compass-slack-thread-rendering](../compass-slack-thread-rendering.md),
> ledger rows **DL-040** and **DL-041**, and **SEA-1364 T3's conversation
> write-through** (the streamed-turn → comms auto-post path — D7/T7);
> **clarifies** DL-037, DL-029, DL-028, DL-072. The ledger delta ships in this
> same PR: DL-040 → Superseded, DL-041 → Superseded, plus new rows for the
> Zulip topic model and the write-through removal.

## Problem / Intent

Compass's built-and-frozen threading is the Slack model: a channel is a flat
stream of root messages, and replies hang one level off a root via
`Message.parent_message_id` (`proto/compass/v1/comms.proto:250` — `string
parent_message_id = 7;` under the comment "The message this one replies to,
threading it under its parent; unset for a root message"). That model does not
keep agents on-topic — every agent posts into one undifferentiated channel
stream — and it does not let the human track several parallel conversations in
a channel at a glance. Matt's ruling: adopt **Zulip's model** — a channel is a
container of **named topics**, and every message belongs to a topic. Topics
aggregate messages into self-contained conversations, giving agents a place to
self-organize and stay focused, and giving the human a legible per-conversation
index of what is going on. This record designs that inversion end to end
(proto, store, delivery, agent tools, UI) as a supersession of the frozen
Slack chain named above.

The ratified core intent (Matt, 2026-08-02): **agents call comms tools
specifically to surface something to the human.** The bulk of an agent's
output — tool calls, narration, streamed turn text — stays OUT of the threads
and lives in the session log; a topic holds only the deliberate, useful,
human-facing exchanges. A user who wants to watch an agent closely opens the
live session log; the channel's topics stay a legible index of what matters.
This fits Compass's model of independent agents owning issues and PRs end to
end. And it is a STRUCTURAL property, not a prompting affordance: with the
streaming write-through removed (D7), the ONLY path that writes a comms
Message is an explicit `comms_post_message(topic)` call — an agent cannot
flood a channel just by talking; it must deliberately post. The
human-legibility half is equally structural: a server-authoritative
per-conversation index (every message carries a server-validated `topic_id`)
replaces client-side root-chasing.

## Approach

### The model in one paragraph

A **Topic** becomes a first-class entity: a named, server-assigned-id container
that lives inside exactly one channel. Every message belongs to exactly one
topic (`messages.topic_id NOT NULL`) and holds **only** that topic id — never a
channel id (F10); the channel a message lives in is reached through its topic
(`topics.channel_id`). `parent_message_id` is removed from the model. A channel
is a container of topics; the channel's "stream" is a derived view (topics
ordered by last activity), never a message stream of its own. Posting names a
channel and a topic — the topic by id for an existing topic, or by name to
get-or-create one within that channel (the Zulip ergonomic: typing a new topic
name starts the conversation). Delivery, read-state, and the UI all key on the
topic. Because a message never records its channel, moving a topic to a
different channel later is a single-row update on `topics.channel_id` with no
message rewrite — a flexibility F10 buys deliberately.

### D1 — Proto: `Topic` message + topic-addressed posting (breaking comms.v1 change)

Pre-dogfood there are zero external clients, so this is a **clean breaking
change to `compass.v1`** (F2/F9, ruled — over a parallel v2 or an
additive-then-mandatory phase; see Alternatives). Superseded fields are
**removed outright, not `reserved`** (F9): with no external clients there is no
wire-compat reason to burn a tombstone, and field numbers may be reused freely
(reserving becomes the convention only once the service is usable). The delta:

- New `message Topic { string id; string channel_id; string name; int64
  created_at_unix_ms; string created_by_account_id; bool archived; }`.
- `Message` (`comms.proto:234-251`): the `oneof container { string channel_id =
  2; }` and field 7 `parent_message_id` are **both removed** (not reserved —
  F9), replaced by a single `string topic_id = 2;` (always set). A message
  carries its topic and nothing about its channel; a consumer that needs the
  channel resolves it through the topic (`topics.channel_id`).
- `PostMessageRequest` (`comms.proto:592-603`): `channel_id = 1` **stays** (the
  post names the channel whose topic namespace it addresses); `parent_message_id
  = 5` is removed; add a `oneof topic { string topic_id; string topic_name; }` —
  `topic_name` is the get-or-create path (server resolves case-insensitively
  within the named channel, creating the topic if absent); `topic_id` addresses
  an existing topic exactly.
- `ListMessagesRequest` (`comms.proto:571-579`): `channel_id = 1` stays; gains
  `string topic_id` (optional filter; unset pages the whole channel newest-first
  as today, resolved by joining messages through their topics to the channel).
- New RPCs on the comms service: `ListTopics(channel_id, include_archived)`,
  `UpdateTopic(topic_id, name?/archived?)`. Rename is id-stable; **rename to a
  name that already exists (case-insensitively) in the channel is a MERGE**
  (Zulip's move-messages model): all of the source topic's messages move to
  the existing target topic in one tx, the now-empty source row is deleted
  (safe — it holds zero messages post-move; standalone topic deletion remains
  impossible), and the response returns the surviving target `Topic`. Merge is
  the ONE operation that changes a message's `topic_id`, designed in now
  because retrofitting message-move later breaks the "a message's topic never
  changes" assumption consumers would otherwise bake in. `CreateTopic` is not
  a separate RPC: topics are born via `PostMessageRequest.topic_name` (a topic
  with zero messages is not a thing, same as Zulip).
- Fan-out events: `MessagePosted` carries the message (which now carries
  `topic_id`); a new `TopicUpserted` event covers create/rename/merge/archive
  so the UI's topic index stays live without refetch (on merge, subscribers
  see the surviving target upserted; the source disappears from `ListTopics`).

### D2 — Store: `topics` table, `messages.topic_id`, collapsed pre-dogfood schema

Current DDL (`go/internal/store/migrations/0001_init.sql:109-123`): `messages`
carries `channel_id TEXT NOT NULL REFERENCES channels (id) ON DELETE RESTRICT`
and `parent_message_id TEXT REFERENCES messages (id) ON DELETE RESTRICT`. Both
go. The delta:

- **Migration mechanics (F8):** pre-dogfood there are no users and therefore no
  data to preserve, so this record does **not** layer an incremental
  `NNNN_topics.sql` with an `ADD COLUMN`/backfill/`DROP COLUMN` dance. The schema
  is expressed in its **final shape** — `messages` with `topic_id NOT NULL` and
  no `channel_id`/`parent_message_id`, plus the new `topics` table — folded into
  the collapsed baseline schema (the 0001-0006 chain collapses to a single
  migration; that collapse is a repo-wide schema-hygiene task sequenced after the
  in-flight schema PRs land, and is not gated inside this record). If the
  collapse has not yet landed when T2 starts, T2 lands as the next incremental
  migration that still defines the final shape directly, with **no
  data-conversion step** (F6 is removed — there is no `parent_message_id` data
  to convert).
- New table `topics`:

  ```sql
  CREATE TABLE topics (
      id                    TEXT PRIMARY KEY,
      channel_id            TEXT NOT NULL REFERENCES channels (id) ON DELETE RESTRICT,
      name                  TEXT NOT NULL,
      created_by_account_id TEXT NOT NULL REFERENCES accounts (id) ON DELETE RESTRICT,
      created_at_unix_ms    BIGINT NOT NULL,
      archived              BOOLEAN NOT NULL DEFAULT FALSE,
      last_seq              BIGINT NOT NULL DEFAULT 0  -- denormalized activity order
  );
  CREATE UNIQUE INDEX topics_channel_name_idx ON topics (channel_id, lower(name));
  ```

- `messages`: `topic_id TEXT NOT NULL REFERENCES topics (id) ON DELETE
  RESTRICT`; **no `channel_id` column** (F10 — the channel is `topics.channel_id`,
  one join away) and no `parent_message_id`.
- Index `messages_topic_seq_idx ON messages (topic_id, seq DESC)` replaces
  `messages_channel_seq_idx` (`0001_init.sql:129`). `seq` stays table-monotonic
  (`BIGSERIAL`) — and therefore trivially channel-monotonic within any channel,
  the property D3's delivery cursor relies on — so it still totally orders every
  message; channel-level paging (newest-first across a channel) joins `messages`
  to `topics` on `topic_id`, filters `topics.channel_id`, and orders by `seq` —
  the same total order, one join wider.
- Validation moves from same-channel-parent (`messages.go` AppendMessage doc:
  "a parent_message_id that names a message outside this channel" is
  ErrInvalidArgument) to **topic-under-channel**: the resolved topic's
  `channel_id` must equal the `PostMessageRequest`'s `channel_id`, checked in the
  same tx as the insert and membership gate, preserving the existing one-tx
  race-free shape ("The membership check and the insert run in one
  transaction").
- Store surface: `AppendMessage` takes the target `channel_id` + a `TopicRef`
  (id or name; get-or-create on name inside the same tx) and records **only**
  `topic_id` on the row; new `ListTopics`, `UpdateTopic` (rename / archive /
  merge-on-name-collision per D1 — the merge moves message rows' `topic_id` and
  deletes the emptied source in one tx), and `ListMessages` grows a topic
  filter.
- Get-or-create resolving to an **archived** topic resolves into it and clears
  `archived` in the same tx — archive is a tidiness flag, not a lock; a post
  addressed at a tidied-away name revives the conversation rather than
  erroring or forking a case-variant duplicate.
- Concurrent get-or-create on one name is settled by the unique index, never
  by naive SELECT-then-INSERT (which surfaces unique-violations under load):
  inside the append tx, `INSERT INTO topics ... ON CONFLICT ((channel_id),
  lower(name)) DO NOTHING`, then re-SELECT the row — two racing posts converge
  on one topic row with neither seeing an error.

### D3 — Delivery: cursor stays per-(agent, channel), resolved through the topic join

DL-072 froze the durable cursor "per `(agent_account_id, channel_id)` on
`messages.seq`" (DECISIONS.md:125; DDL at
`0006_delivery_cursors.sql:10-24`). **This record keeps that keying** (F4) even
though a message no longer stores its channel: the cursor's channel is
`topics.channel_id`, so the owed-tail query for `(agent, channel)` is `messages
JOIN topics ON messages.topic_id = topics.id WHERE topics.channel_id = ? AND
seq > watermark` (F10). Rationale unchanged: the cursor's job is at-least-once
owed-message delivery per agent — a per-channel low-water mark over the
channel-monotonic (via the join) `messages.seq` already covers every topic in
the channel, because topics partition the channel's messages without changing
their seq order. Re-keying per-(agent, channel, topic) would multiply cursor
rows, complicate the reconnect sweep's gap arithmetic for zero delivery
benefit, and (worse) turn topic-creation into a cursor-provisioning event.
Instead:

- The deliver op payload gains the message's `topic_id` + topic name, so the
  agent-side queue can group by topic.
- RT-3 turn-end coalescing becomes **per-topic within the channel batch**: the
  held deliveries at the recipient's turn-settle edge coalesce into one digest
  per topic (rather than one flat per-channel digest), so an agent waking up
  sees "3 messages in ‘retry policy’, 1 in ‘deploy Friday’" — the legibility
  point of the whole model, applied to agents. Ownership per the frozen RT-3
  contract: the **agent owns the turn-end coalescing queue** ("the
  CompassAgent **queues** it and, at turn end, issues the queued set as a
  single `prompt`", compass-0.6/design.md:1454-1466); the server owns only the
  durable cursor (DL-072). So the per-topic digest **forms agent-side (T4)**;
  the server's whole contribution is the topic metadata on each deliver op
  (T3).
- The reconnect sweep (DL-072's gap-aware sweep) is unchanged: it replays the
  owed tail by channel seq (via the topic join); topic grouping happens at
  presentation.

### D4 — Agent comms tools: topic-mandatory posting, no home topic

Today (`packages/compass-agent/src/comms.ts:125-127`, 262-263):
`comms_post_message(text, channel_id?, parent_message_id?)` with "Omit
channel_id to post to your home channel. Set parent_message_id to reply in a
thread." The delta (clarifies DL-028):

- `comms_post_message(text, topic, channel_id?)` — `topic` is a **required
  string, the topic name** (get-or-create via
  `PostMessageRequest.topic_name`); `parent_message_id` is gone. `channel_id?`
  stays and still defaults to the home channel — it names the channel whose
  topic namespace the post addresses. Agents address topics by name, not id —
  names are the unit agents can produce without a lookup round-trip, and
  get-or-create makes a typo cost one stray topic (renameable), never a lost
  message.
- `comms_list_messages(channel_id?, topic?, limit?)` — optional topic filter;
  the rendered transcript groups by topic (replacing the current
  `parent="…"` attribute rendering at `comms.ts:521-522`).
- New read tool `comms_list_topics(channel_id?)` so an agent can survey the
  live conversations before posting — the self-organization affordance.
- Home default (clarifies DL-029; F5, ruled): the **home channel** stays the
  authz default exactly as frozen; there is **no distinguished home topic**
  and no default or fallback topic of any kind. An untargeted
  mention/delivery lands in whatever topic the triggering message lives in;
  every post names a topic — when no existing topic applies, the agent
  creates one by naming it (get-or-create). Channels carry zero messages
  directly: a channel is a collection of topics, and messages live only in
  topics (F3, ruled).

### D5 — UI: Zulip two-level drill-in — channel = topic index, topic = messages + composer (F11)

Today `ChannelView.tsx:403` derives `threadsOf(store.messages(), chan.id)` and
renders `ThreadStream`/`ThreadView` rows (`ChannelView.tsx:211-266`) with a
composer pinned at the bottom (`ChannelView.tsx:316,442`); a reply opens a side
`ThreadPanel` via `store.openThread` (`ChannelView.tsx:230,240`), and the client
chases roots with `rootOf`/`threadsOf` (`apps/ui/src/comms.ts:172-195`). The
whole Slack surface — `ThreadView`, `ThreadStream`, `ThreadPanel`, the
`.thread-summary` pill, `store.openThread`, `threadsOf`, and the root-chasing
`rootOf` — is ripped out and replaced by Zulip's two-level navigation (F11):

- **Channel view = topic index, no composer.** Selecting a channel
  (`store.openChannel`) shows a list of its topics (server-authoritative,
  ordered by last activity), each row a topic-name header + activity summary
  (message count, last-activity time, unread), plus a **"new topic"** affordance
  (a name field that starts a conversation via `topic_name` get-or-create). There
  is **no message composer at the channel level** — you cannot post into a
  channel, only into a topic. `topicsOf(topics, messages, channelId)` (replacing
  `threadsOf`) produces the ordered topic list; no client-side root-chasing or
  orphan adoption (the server guarantees every message has a topic).
- **Topic view = messages + composer, no nested threading.** Selecting a topic
  (a new `store.openTopic(topicId)`) drills into that topic: its messages in
  chronological order, with the **composer returning**, scoped to the topic.
  There is **no threading inside a topic** — no reply-to-message, no
  `parent_message_id`, no sub-threads; every post in the topic view is a flat
  message in that topic (you cannot make a thread within a thread). Deep-link
  route `#/channel/<channelId>/topic/<topicId>`.
- **Left sidebar = channel + its recent topics.** Each channel row
  (`LeftSidebar.tsx:119` `ChannelRow`) gains, beneath it, its **most recent
  topics** (default **3**; the exact count is a tunable UI constant — F11) as
  deep-nav sub-rows routing straight to a topic view, so the human reaches an
  active conversation in one click without first opening the channel's full
  index.
- SEA-1332 (compass-message-surface-rendering, designed-not-built; DL-041 "The
  message surface is a virtualized thread list") re-points: the virtualized
  list unit becomes the **topic's message list** (the topic view), and that
  record's implementation follows this contract rather than its original
  thread-list wording.

### D6 — Asks (clarifies DL-037)

DL-037 froze "Standalone channel asks are answerable wherever they are asked
(first-responder-wins)" (DECISIONS.md:145). Unchanged in substance: an ask is
carried by a message, a message lives in a topic, so an ask is **visible in
its topic and answerable channel-wide, first-responder-wins**. Topic scoping
changes where the ask renders, not who may answer it.

### D7 — One comms-write path: the streaming write-through is removed (supersedes SEA-1364 T3)

Ruled with F7's dissolution (see Decisions): an agent's streamed turn does
**not** write to comms at all. Today the EventMapper dual-surfaces streamed
text — "a streamed assistant `text_delta` produces BOTH a session
`assistant_text` chunk (per delta, live) AND — on `text_end` — a settled
comms MessageUpdated" (`packages/compass-agent/src/mapping.ts:29-33`) — and
the settled comms half rides a dedicated durable pipeline into the store (the
"conversation write-through", `go/internal/runnerhub/hub.go:44-53` — "the hub
threads it into the ConversationSink so the conversation write-through
commits KEYED"):

- **Agent**: the `text_end` arm settles the run into a `conversationUpdated`
  frame (`mapping.ts:292-306` → `#appendBlock`, `mapping.ts:376-392`), typed
  as `OutboundFrame`'s `conversationPosted`/`conversationUpdated` variants
  (`frame.ts:50-51`), which the frame sink sends "on the
  PostConversationFrame UNARY, awaited, and retried" with an agent-minted
  idempotency key (`transport/frame-sink.ts:12-15`).
- **Runner**: `Gateway.PostConversationFrame`
  (`go/internal/runner/gateway/post_conversation_frame.go:44-47`) forwards it upstream
  on the `RunnerService.CommitConversationFrame` unary.
- **Server**: `Handler.CommitConversationFrame`
  (`go/internal/runnerhub/handler.go:185`) → `Hub.CommitConversationFrame`
  (`go/internal/runnerhub/relay_comms.go:242`) commits through
  `CommitAgentPostKeyed` (`go/internal/comms/agent_caller.go:299-314` —
  "Container unset on purpose — routes to the agent's home channel") /
  `CommitAgentUpdateKeyed`; the Deliver-path twin is the ConversationSink
  (`go/server/sinks.go:72-90`, `commsConversationSink.PostAgentMessage`
  dispatching to the same two keyed commits).

**The conversation write-through half of that is removed (T7)** — the two
conversation variants and every keyed commit behind them. The shared durable
conversation-frame lane itself — `AgentGateway.PostConversationFrame` →
`RunnerService.CommitConversationFrame` — **survives**: SEA-1570's durable
transcript tee rides this exact lane by design (`transcript_entry`, field 7,
`agent.proto:73-79`), and its runner-forward half is merged and live —
`post_conversation_frame.go:76` calls `CommitConversationFrame`, gated by
`isConversationFrame`, which admits `AgentFrame_TranscriptEntry` at
`post_conversation_frame.go:105`, covered by
`TestPostConversationFrameForwardsTranscriptEntry`. Only the
`conversation_posted`/`conversation_updated` co-tenants and their comms
write-through are removed; the RPC pair becomes transcript_entry-only and
keeps its current name (a rename is separate scope, out of this record). A
comms `Message` comes into being only via an explicit
`comms_post_message(topic, …)` tool call (T4) or the human client's
`PostMessage` — both carry a mandatory topic, so there is **no topicless
comms-write path left** and the `messages.topic_id NOT NULL` constraint has
no adversary. The SESSION surface **survives unchanged**: the execution
trace — assistant-text chunks, thinking, tool calls, plans — keeps riding
`SessionFrame.typed_event` (`mapping.ts:20-27`) through the loss-tolerant
Publish spine into the session pane; the human watches an agent closely by
opening the live session log, not by reading auto-posted turn text in a
thread.

The simplification that falls out: the pre-ratification F7 plumbing (a
conversation-frame `topic_id` inherited from the triggering deliver and
threaded deliver→session→frame) is moot — frames no longer carry comms
content, so no frame-schema topic delta exists anywhere in this record
(T1/T3/T4 carry none).

## Alternatives considered

**Parallel `comms.v2` service** — stand up a second proto package and run both
models side by side, migrating clients gradually. Lost: there are no external
clients pre-dogfood, so the only thing a v2 buys is double the surface to
maintain and a live v1 that still renders the superseded model. Breaking v1 in
place — removing the superseded fields outright, since no client needs a
tombstone pre-dogfood (F9) — is strictly cheaper.

**Additive-then-mandatory migration** — add `topic_id` as optional beside
`parent_message_id`, run both, flip mandatory later. Lost: "later" is a second
breaking change plus an interim where the store, delivery, and UI must handle
both shapes; pre-dogfood there is no traffic that needs the bridge. One cutover
PR chain, one migration.

**Per-(agent, channel, topic) delivery cursor** — re-key DL-072's cursor to the
topic. Lost: `messages.seq` is table-monotonic and (through the topic join)
already totally orders every topic's messages, so a per-channel low-water mark
loses no delivery precision; per-topic rows multiply the sweep's gap arithmetic
and make topic creation a cursor-provisioning event. Topic granularity is a
*presentation and coalescing* concern (D3), not a durability one. (F4, ruled:
keep per-channel.)

**Push-all + client-side topic filter with per-topic *subscription*** — Zulip
proper lets users mute/follow topics. Lost for now: per-topic subscription
state is real scope (a new table, UI, and agent-tool surface) with no
pre-dogfood consumer; agents receive everything on their channels today and
the per-topic digest (D3) already gives focus. Deferred, explicitly out of
scope, trivially additive later.

**Default "(no topic)" bucket instead of mandatory topics** — let a topicless
post land in a per-channel catch-all. Lost: it recreates the flat stream one
lazy post at a time — precisely the failure mode Matt is buying out of. Zulip
itself walked this back (its "(no topic)" is now "general chat", discouraged).
Mandatory-with-get-or-create keeps posting one call with zero extra
round-trips, so the ergonomic cost is a required string field. (F3, ruled:
mandatory.)

**Keep the streaming write-through, teach it topics** — retain the
streamed-turn auto-post and give its conversation frames an
inherited/defaulted `topic_id` (the pre-ratification F7(c) recommendation).
Lost: it preserves the firehose — every turn's settled text lands in a thread
whether or not it is worth surfacing, the opposite of the ratified intent
(threads hold only deliberate posts) — and its untriggered-turn fallback
required exactly the default-bucket topic F3 rejects. Removing the path (D7)
is strictly simpler: one comms-write path, no frame-schema delta, no
inheritance plumbing.

**Message keeps its own `channel_id` beside `topic_id`** — denormalize the
channel onto the message row (as the shipped schema does) so channel-level
reads skip the topic join. Lost: it is the wart Matt named — the channel is
already implied by the topic, so storing it twice invites drift and, worse,
makes moving a topic to another channel a rewrite of every one of its message
rows instead of a single `topics.channel_id` update. The join is one index
hop; the flexibility is structural. (F10, ruled: `topic_id` only.)

**Topics as sugared `parent_message_id`** — keep the Slack carrier and render
the root's first line as a "topic". Lost: no rename, no archive, no mandatory
membership, orphan/cycle handling stays client-side (`comms.ts:186-189`'s
cycle guard), and the model stays a reply tree that agents demonstrably don't
use for organization. This is the status quo with a costume.

## Global Constraints

- **Pre-dogfood: breaking changes are on the table.** No external clients
  exist; `compass.v1` may break in place, with superseded fields **removed
  outright, not `reserved`** (F9 — no client needs a wire tombstone
  pre-dogfood, and field numbers may be reused). The buf-breaking CI gate is
  expected to fire on this chain and is overridden knowingly, once.
- **Pre-dogfood: the SQL migration chain collapses** (F8). With no users and
  no data to preserve, migrations are maintained as a single collapsible
  baseline rather than an ever-growing incremental chain; this record's schema
  lands in that baseline (or the next incremental migration if the collapse has
  not yet landed), never as a data-migration.
- **Go stack under `go/`**, proto in `compass.v1`; errors `%w`-wrapped and
  stage-tagged (`fmt.Errorf("store: begin append message: %w", err)` shape,
  `messages.go`); `ctx` first parameter on every store/comms method.
- **Proto tree has one writer**: compass-repo lane authors `.proto` text and
  runs the single buf.gen across all gen lanes (the convention frozen in
  compass-notification-delivery T1); other lanes co-design shape by DM, never
  edit proto files.
- **Delivery stays at-least-once per session** (DL-072 inheritance): the
  Server-owned cursor + gap-aware reconnect sweep semantics are inherited
  unchanged; this record only adds topic metadata to the deliver payload,
  re-scopes RT-3 coalescing per-topic (D3), and resolves the cursor's channel
  through the topic join (F10).
- **Agent authz stays owner-gated, home-channel-defaulted** (DL-029): identity
  is session-resolved server-side; nothing in this record touches the authz
  path. Topic membership is NOT a new ACL — visibility remains channel
  membership (write gate at `messages.go` AppendMessage: `requireChannelMember`
  in the same tx as the insert).
- **Every message has exactly one topic; a topic has exactly one channel.**
  `messages.topic_id NOT NULL`, topic→channel FK, topic-under-channel
  validated in the insert tx. **A message stores no channel id (F10); its
  channel is `topics.channel_id`.** No cross-channel topics, no nested topics,
  no message reparenting across channels. A message's `topic_id` is stable with
  exactly one exception: a topic **merge** (D1's rename-to-existing) re-points
  its messages at the surviving topic in the same channel — consumers MUST NOT
  bake in "a message's topic never changes".
- **Topic names are unique case-insensitively per channel** (the
  `topics_channel_name_idx` unique index), max 120 chars, non-blank after
  trim; rename preserves id (links and cursors never break on rename).
- **No standalone topic deletion** — archive only; a merge (D1) deletes the
  source row only after moving every message to the target in the same tx, so
  messages are never orphaned or cascaded away (`ON DELETE RESTRICT`
  everywhere, per `0001_init.sql` convention).
- **The comms tool is the only agent comms-write path.** With the SEA-1364 T3
  write-through removed (D7/T7), a comms Message row is created only by
  `comms_post_message` (agents) or the human client's PostMessage — both
  carry a mandatory topic. No server-side path materializes a Message from a
  streamed frame.

## Plan

Task ordering is the dependency order: **T7 lands first** — the write-through
removal is independent of the topic model and deletes the one comms-write
path that could not supply a topic, so T2's `NOT NULL` schema never has a
live adversary. T1 unblocks everything topic-shaped; T2 unblocks T3; T4/T5
ride the regenerated stubs; T6 is last (ledger + record flips travel with
this design PR itself, not a task).

**Merge-order constraint (in-flight PRs):** compass PR **#88** (SEA-1569 T6,
reconnect/start redelivery sweep — open at this writing) touches
`go/internal/delivery`, `go/internal/runnerhub`, and `go/server/sinks.go` —
the surfaces this record's T7 deletes from and T3's deliver-op payload change
rides through, and consumers of the regenerated stubs. It must **land or be
parked before T7's removals and T1's regen-everything breaking chain start**,
or they strand it on removed code. (#90 — SEA-1570's own T7 — already merged
as `f1d3aa595` — is NOT merely out of the way: it added `transcript_entry`
(field 7) as a co-tenant on the PostConversationFrame →
CommitConversationFrame lane, which is exactly why T1/T7 narrow the removal
to the two conversation variants and preserve the RPCs.)

### T1 — Proto delta: `Topic`, topic-addressed posting, topic RPCs — **compass-repo (sole proto writer; shape co-designed with compass-server + compass-agent)**

Break `compass.v1` in place per D1/F9: **remove** (not reserve) `Message`'s
`oneof container { channel_id = 2 }` and `parent_message_id` (field 7) and
`PostMessageRequest.parent_message_id` (field 5); add `Message.topic_id = 2`,
`PostMessageRequest.oneof topic { topic_id; topic_name; }`,
`ListMessagesRequest.topic_id`, `message Topic`, `ListTopics` / `UpdateTopic`
RPCs, and the `TopicUpserted` fan-out event. The same breaking chain carries
D7's proto delta: **remove** (not reserve) `AgentFrame.conversation_posted`
(field 1) / `conversation_updated` (field 2) (`agent.proto:44-47`). The
`AgentGateway.PostConversationFrame` (`agent_gateway.proto:67`) +
`RunnerService.CommitConversationFrame` (`runner.proto:134`) RPCs, their
request/response messages, and `transcript_entry` (field 7) are **kept** —
they are the shared durable conversation-frame lane SEA-1570's transcript
tee rides (D7); T7 removes only the conversation write-through, never the
transcript forward path. After the removal the lane is transcript_entry-only,
kept under its current name (a rename is separate scope). Regenerate all gen
lanes in the same PR.

- Interfaces:

  ```proto
  message Topic {
    string id = 1;
    string channel_id = 2;
    string name = 3;
    int64 created_at_unix_ms = 4;
    string created_by_account_id = 5;
    bool archived = 6;
  }
  rpc ListTopics(ListTopicsRequest) returns (ListTopicsResponse);
  message ListTopicsRequest { string channel_id = 1; bool include_archived = 2; }
  message ListTopicsResponse { repeated Topic topics = 1; }
  rpc UpdateTopic(UpdateTopicRequest) returns (UpdateTopicResponse);
  message UpdateTopicRequest {
    string topic_id = 1;
    optional string name = 2;      // rename; id-stable. Rename to an existing
                                   // (case-insensitive) name in the channel = MERGE (D1)
    optional bool archived = 3;    // archive / unarchive
  }
  message UpdateTopicResponse { Topic topic = 1; }
  // Message (fields other than the two below are unchanged):
  message Message {
    string topic_id = 2;         // was `oneof container { string channel_id = 2; }`
                                 //   — the oneof and field 7 parent_message_id are
                                 //   both REMOVED, not reserved (F9); field 2 is
                                 //   reused, same `string` wire type.
    // ... other Message fields unchanged ...
  }
  message PostMessageRequest {
    oneof container {            // KEPT verbatim — single-arm oneof gives unset
      string channel_id = 1;     //   (→ home channel) vs empty presence; a bare
    }                            //   proto3 `string` could not, so omit→home breaks
                                 //   if collapsed. channel_id names the channel
                                 //   whose topic namespace the post addresses.
    oneof topic {                // ADDED — the topic sub-selector (orthogonal to
      string topic_id = 6;       //   container; both oneofs coexist legally)
      string topic_name = 7;     //   topic_name = get-or-create; topic_id = exact
    }
    // parent_message_id = 5 REMOVED (F9); fields 3 (blocks), 4 (client_request_id)
    //   unchanged. 6,7 are free (5 vacated).
  }
  message ListMessagesRequest {
    oneof container {            // KEPT verbatim (same presence rationale as Post)
      string channel_id = 1;
    }
    string topic_id = 6;         // ADDED — optional filter (NOT a selector: bare,
                                 //   not in a oneof); empty pages the whole channel
                                 //   newest-first. 6 is free.
    // fields 3 (limit), 4 (before_message_id), 5 (snapshot_seq) unchanged.
  }
  ```

- Red-first: `buf breaking` fires on the removed fields (expected, overridden
  once with the pre-dogfood rationale in the PR body); regen drift gate green
  across all three gen lanes; a wire test confirms an old `parent_message_id`
  payload's removed field (7, **not** reused) simply does not decode — dropped
  as unknown, never misparsed. `channel_id` is different: F9 reuses field **2**
  as `topic_id` with the **same** string wire type, so a hypothetical old
  `channel_id=2` payload would decode straight into `topic_id` — the textbook
  field-number-reuse collision. That reuse is safe **only** because zero
  pre-dogfood payloads exist (F9's actual rationale), so the test asserts the
  reuse is confined to pre-dogfood, not that an old `channel_id` "does not
  decode".

### T2 — Store: `topics` table, `messages.topic_id`, collapsed schema, topic CRUD — **compass-server (store)**

Schema lands in its final shape per D2/F8/F10 (topics DDL + `messages.topic_id
NOT NULL` + `messages_topic_seq_idx`, no `messages.channel_id`, no
`parent_message_id`) — folded into the collapsed baseline, or the next
incremental migration defining that final shape directly, with **no
`ADD COLUMN`/`DROP COLUMN` dance and no F6 data conversion** (pre-dogfood, no
data). `AppendMessage` swaps same-channel-parent validation for in-tx topic
get-or-create + topic-under-channel validation; `last_seq` denormalization
updated in the insert tx.

- Migration numbering: if it lands incrementally (collapse not yet in), the
  file lands at the next free slot **checked against `main` at T2 time** — main
  currently ends at `0006_delivery_cursors.sql`, so `0007_topics.sql` unless
  another migration lands first; if the collapse has landed, the shape is part
  of the single baseline migration instead.
- The F6 first-~60-chars topic-naming / dedup rule is **removed with F6**:
  there is no shipped `parent_message_id` data to convert, so there is no
  backfill and no name-collision handling to design (get-or-create's ON
  CONFLICT still governs live posting).

- Interfaces:

  ```go
  type TopicRef struct { ID string; Name string } // exactly one set
  type Topic struct {
      ID, ChannelID, Name, CreatedByAccountID string
      CreatedAtUnixMS int64
      Archived        bool
      LastSeq         int64
  }
  func (s *Store) AppendMessage(ctx context.Context, m Message, channelID string, topic TopicRef, clientRequestID string) (Message, bool, error)
  func (s *Store) ListTopics(ctx context.Context, callerAccountID, channelID string, includeArchived bool) ([]Topic, error)
  func (s *Store) UpdateTopic(ctx context.Context, callerAccountID, topicID string, name *string, archived *bool) (Topic, error)
  func (s *Store) ListMessages(ctx context.Context, q ListMessagesQuery) ([]Message, error) // q gains TopicID string filter
  ```

  `Message.TopicID string` replaces both `Message.ChannelID` and
  `Message.ParentMessageID`; the stored row records only the topic, so
  `AppendMessage` takes the target `channelID` as a parameter (for topic
  resolution + topic-under-channel validation), not a field on the row. All
  reads channel-membership-gated exactly as today (ErrNotFound merge for
  non-members), the channel resolved through the topic join.
- Red-first: pgtest — a post naming a topic in another channel is
  ErrInvalidArgument; concurrent get-or-create on the same name yields one
  topic row (ON CONFLICT DO NOTHING + re-SELECT, no unique-violation surfaced);
  get-or-create on an archived name clears `archived`; `UpdateTopic`
  rename-to-existing merges (source messages carry the target `topic_id`,
  source row gone); a schema test asserts `messages` has `topic_id NOT NULL`
  and carries no `channel_id`/`parent_message_id` column; channel-level
  `ListMessages` pages newest-first across the channel via the topic join.

### T3 — Comms service: topic routing, `TopicUpserted` fan-out, deliver-op topic metadata — **compass-server (comms)**

Map the new RPCs through `go/internal/comms` (mapping.go / comms.go — the
agent_caller write-through path is gone post-T7); deliver ops gain `topic_id`
and topic name; `TopicUpserted` fans out on create/rename/merge/archive.
Cursor keying untouched (DL-072); the owed-tail query joins messages→topics to
resolve the cursor's channel (D3/F10). The per-topic digest itself is **not
here**: RT-3 puts the turn-end coalescing queue agent-side
(compass-0.6/design.md:1454-1466), so digest formatting is T4's scope — T3
only guarantees every deliver op carries the topic metadata the agent groups
by.

- Interfaces: proto↔store mapping for `Topic`; deliver-op payload gains
  `topic_id string, topic_name string`; ack/cursor bookkeeping unchanged, per
  `(agent_account_id, channel_id)` on `messages.seq` (channel resolved via the
  topic join).
- Red-first: pgtest — a delivered message's deliver op carries its topic id +
  name; reconnect sweep replays the owed tail regardless of topic;
  `TopicUpserted` reaches channel subscribers on create, rename, and merge.

### T4 — Agent comms tools + per-topic turn-end digest: topic-mandatory post, topic-grouped list, `comms_list_topics` — **compass-agent**

Per D4: `comms_post_message` gains required `topic` (name, get-or-create),
drops `parent_message_id`, keeps `channel_id?` (defaults home);
`comms_list_messages` gains optional `topic` filter and renders grouped by
topic; new `comms_list_topics`. Also — moved from T3, per RT-3's agent-owned
coalescing queue — the turn-end digest groups the held delivers per
`(channel, topic)` at digest-formatting time: one digest per topic within the
channel batch (D3).

- Interfaces:

  ```ts
  // postParameters (comms.ts): text: string (non-blank), topic: string
  // (non-blank, ≤120 chars, described "Named conversation within the channel;
  // an unknown name creates the topic"), channel_id?: string (non-blank).
  // listParameters: channel_id?: string, topic?: string, limit?: number.
  // New tool:
  //   comms_list_topics(channel_id?: string) → rendered list of
  //   {name, message count, last activity, archived} for the channel.
  ```

  Wire: `PostMessageRequestSchema` filled with `channel_id` + `topic: { case:
  "topicName", value: params.topic }`; transcript rendering replaces the
  `parent="…"` attribute (`comms.ts:521-522`) with `topic="…"` grouping
  headers.
- Red-first: comms.test.ts — a post without `topic` is a schema reject before
  any broker call; the wire call carries `topicName`; list output groups two
  topics' messages under distinct headers; two queued delivers in different
  topics of one channel format as two per-topic digests at turn end.

### T5 — UI inversion: `topicsOf()`, topic index + topic view, ThreadPanel removal, sidebar recent-topics, topic routing — **compass-ui**

Per D5/F11. Replace `threadsOf`/`ThreadView`/`ThreadStream`/`ThreadPanel`/
`openThread`/`rootOf` with the two-level topic navigation: the **channel view
becomes a topic index with a "new topic" affordance and no composer**; a new
**topic view (`openTopic`) renders one topic's messages with the composer, and
no nested threading**; the **left sidebar's channel rows list the channel's ~3
most recent topics** as deep-nav sub-rows. SEA-1332 virtualization re-points at
the topic message list when it builds.

The removal/sweep set also includes `RightSidebar.fleetpane.test.tsx` — its
fixture contract ("The DMs are flat (no parentMessageId), so each message is
its own thread → one .msg row", `RightSidebar.fleetpane.test.tsx:82`) leans on
the retired parent model and re-lands on topics.

- Interfaces:

  ```ts
  // apps/ui/src/comms.ts
  export type TopicGroup = { topic: Topic; messages: Message[] }; // chronological
  // channel index: one summary row per topic, last-activity-desc
  export function topicsOf(topics: readonly Topic[], messages: readonly Message[], channelId: string): TopicGroup[];
  // components:
  //   ChannelView -> topic index (topic rows + "new topic"); NO composer
  //   TopicView   -> one topic's messages + the composer; NO reply/threading
  // store: openTopic(topicId: string); openChannel shows the index;
  //        composer state lives only in the topic view
  // sidebar: LeftSidebar ChannelRow lists recentTopics(channelId) capped at
  //          RECENT_TOPIC_COUNT (= 3, a UI constant)
  // route: #/channel/<channelId>/topic/<topicId>
  ```

- Red-first: comms.test.ts — `topicsOf` orders by last activity and never
  drops a message; a
  `ThreadPanel|ThreadView|ThreadStream|openThread|threadsOf|rootOf` reference
  anywhere in `src/` is a failing grep gate (the full retired set D5/T5
  enumerate); the channel view renders NO
  composer (composer present only in the topic view); the sidebar shows at most
  3 recent topics per channel; composer submit inside a topic posts to that
  topic; the channel view's "new topic" affordance posts `topicName`
  get-or-create.

### T6 — Asks + docs sweep: DL-037 topic rendering, agent-facing comms docs — **compass-server (comms) + compass-agent**

Asks render inside their topic (D6); answerable channel-wide unchanged.
Agent-facing tool descriptions and any operator docs referencing "threads" /
"replies" move to topic vocabulary.

- Interfaces: none new — ask blocks already ride `MessageBlock` inside a
  message that now carries `topic_id`; `RespondToAsk` authz untouched.
- Red-first: pgtest — an ask posted in topic A is answerable by a channel
  member who has only read topic B (first-responder-wins is channel-scoped);
  UI renders the ask inside its topic group.

### T7 — Remove the streaming write-through: streamed turns stop writing comms — **compass-agent + compass-runner + compass-server** (proto removals ride T1)

Per D7 (supersedes SEA-1364 T3). Lands FIRST — independent of the topic
model, and required before T2's `NOT NULL` schema so no topicless writer
remains. The shared durable conversation-frame lane
(`PostConversationFrame` → `CommitConversationFrame`) **survives** for
SEA-1570's `transcript_entry`; only the `conversation_posted`/
`conversation_updated` co-tenants and their comms write-through are
removed. The lane split:

- **compass-agent**: `mapping.ts` drops the comms half of the dual-surface —
  the `text_end` arm no longer emits `conversationUpdated`
  (`mapping.ts:292-306`) and `#appendBlock`/`#blocks` go
  (`mapping.ts:376-392`); the per-delta session `assistant_text` chunk
  (`mapping.ts:284-291`) is untouched. `frame.ts` drops the
  `conversationPosted`/`conversationUpdated` `OutboundFrame` variants
  (`frame.ts:50-51`) but keeps `transcriptEntry`; `transport/frame-sink.ts`
  keeps the durable unary path and `transport/index.ts` keeps
  `postConversationFrame` — SEA-1570's `transcriptEntry` frames ride them.
- **compass-runner**: the gateway keeps `PostConversationFrame`
  (`go/internal/runner/gateway/post_conversation_frame.go`), the
  `ConversationCommitter` seam (`gateway.go:111-117`), and the
  `committedKeys` advisory fast-path — all serve the surviving transcript
  lane; `isConversationFrame` narrows to admit only
  `AgentFrame_TranscriptEntry` (`post_conversation_frame.go:105`).
- **compass-server**: runnerhub keeps `Handler.CommitConversationFrame`
  (`handler.go:185`) and `Hub.CommitConversationFrame`
  (`relay_comms.go:242`), dropping only `commitFrame`'s
  `ConversationPosted`/`ConversationUpdated` dispatch cases; with both
  cases gone the kept endpoint becomes the inert transcript-lane endpoint
  (every frame falls to `commitFrame`'s existing `default` →
  `CodeInvalidArgument` until SEA-1570's server-side transcript persist
  lands its case), which is exactly why the RPC/handler/hub surface is
  retained rather than deleted — deleting it would strand SEA-1570's
  durable transcript lane, the regression this narrowing prevents. comms
  drops `CommitAgentPostKeyed` / `CommitAgentUpdateKeyed`
  (`agent_caller.go:299-314`); server drops the `commsConversationSink`
  write-through (`sinks.go:72-90`). The parent-drop guard pgtest
  (`agent_conversation_pgtest_test.go`,
  `TestCommitAgentPostThreadsTheFramesParentMessageID`) retires with the
  path it defends.
- **compass-repo (T1)**: the proto delta — **remove** the `AgentFrame`
  conversation variants (fields 1 and 2) outright (F9); the
  `PostConversationFrame` / `CommitConversationFrame` RPCs and
  `transcript_entry` stay — rides T1's breaking chain (Global Constraints:
  proto has one writer).

- Interfaces: none new — this task only deletes. The session trace surface
  (`SessionFrame.typed_event`, the Publish spine) is explicitly out of scope
  and must be byte-identical before/after.
- Red-first: a repo-wide grep gate goes green only when
  `CommitAgentPostKeyed|CommitAgentUpdateKeyed|conversationPosted|conversationUpdated`
  has zero hits outside generated code slated for T1's regen — no
  conversation frame commits a comms row anywhere (`PostConversationFrame` /
  `CommitConversationFrame` are NOT in the gate: they survive as the
  transcript lane); mapping.test.ts flips
  `text_end → one comms conversationUpdated` to `text_end → no frame` (red
  against the old mapper); the session-surface tests (`text_delta → one
  session assistantText chunk`) stay green untouched, proving the session
  trace still emits; `TestPostConversationFrameForwardsTranscriptEntry`
  stays green untouched, proving the transcript lane still forwards.

## Tasks

- [ ] T7 — Streaming write-through removal: streamed turns stop writing comms; session trace + SEA-1570 transcript lane unchanged (compass-agent + compass-runner + compass-server; proto removals ride T1) — lands first
- [ ] T1 — Proto delta: `Topic`, topic-addressed posting, `ListTopics`/`UpdateTopic`, `TopicUpserted`; remove (not reserve) `parent_message_id` + the channel container (compass-repo)
- [ ] T2 — Store: `topics` table + `messages.topic_id` in the collapsed-baseline schema (no channel_id, no data conversion), topic CRUD, topic-under-channel validation (compass-server store)
- [ ] T3 — Comms service: topic routing + `TopicUpserted` fan-out, deliver-op topic metadata, cursor channel via topic join (compass-server comms)
- [ ] T4 — Agent tools: topic-mandatory `comms_post_message`, topic-filtered `comms_list_messages`, new `comms_list_topics`, per-topic turn-end digest (compass-agent)
- [ ] T5 — UI two-level drill-in: `topicsOf()` + topic index (no composer) + topic view (composer, no nested threading), ThreadPanel/ThreadView removal, sidebar recent-topics, topic deep-link route (compass-ui)
- [ ] T6 — Asks-in-topics rendering + docs vocabulary sweep (compass-server comms + compass-agent)

## Decisions (ratified)

Matt ruled on every fork batched in the pre-ratification draft (2026-08-02);
no open questions remain. He then refined the model the same day with four
follow-ups (F8-F11), folded in above. The F-numbers stay as stable ids for the
cross-references above.

### F1 — DL-040 and DL-041 are reversed

DL-040 ("Threaded replies use a Slack-style side-thread panel keyed by a
deterministic root id") and DL-041 (the message surface as a virtualized
*thread* list) both flip to **Superseded** citing this record. The Zulip
inversion is Matt's own ask and this record is its vehicle; the ledger delta
(both Status flips + the new rows) ships in this same PR.

### F2 — break `compass.v1` in place

One clean breaking change to comms.v1: superseded fields **removed outright**
(F9), no parallel v2, no additive-then-mandatory phase. The buf-breaking CI
gate is overridden knowingly, once, with the pre-dogfood rationale in the PR
body (D1, Global Constraints).

### F3 — topic is mandatory; no "(no topic)" bucket

Every message names a topic; no catch-all, default, or fallback topic exists
anywhere in the system. When no existing topic applies, the agent creates one
by naming it — `topic_name` get-or-create keeps that a single call
(D1/D2/D4). Channels carry zero messages directly: a channel is a collection
of topics, and messages live only in topics.

### F4 — delivery cursor stays per-(agent, channel)

DL-072's cursor keying is kept; topic is metadata on the deliver op, and RT-3
coalescing groups per-topic agent-side at digest time (D3). The cursor's
channel is resolved through the topic join (F10). This is a *clarification* of
DL-072, not a supersession.

### F5 — no distinguished home topic

The home channel (DL-029) stays the authz default; no home *topic* exists.
Untargeted deliveries/mentions land in the triggering message's topic; an
agent's own post always names a topic (D4). A home topic remains addable
later as pure agent-config sugar; nothing ships on it.

### F6 — no data conversion (superseded by F8)

The pre-ratification plan converted shipped `parent_message_id` data into
topics (each thread-root a topic named from its leading text; a reply-less
root its own single-message topic). With F8 (migrations collapse; pre-dogfood,
no users, no data) there is nothing to convert: the schema lands in its final
shape and the conversion step — with its first-~60-chars naming and
collision-dedup rules — is removed. The "no general/default topic" invariant
that plan carried is preserved independently by F3.

### F7 — dissolved: streaming turns do not write comms (D7)

The pre-ratification fork asked which topic a streamed turn's comms row
should get (inherit the triggering deliver's? a server-resolved default?).
Matt dissolved the question by ruling **Option A: agent streamed turns do not
write to comms at all** — the SEA-1364 T3 conversation write-through is
removed (D7, T7). A comms Message appears only on an explicit
`comms_post_message(topic)` call, which already carries a mandatory topic, so
no topicless comms-write path exists and no conversation-frame `topic_id`
delta is needed (the pre-fold recommendation (c) is moot). The streamed turn
survives in full on the session/trace surface (`SessionFrame.typed_event`,
`mapping.ts:20-27`).

### F8 — collapse the migrations; no incremental data-migration

Matt's refinement (2026-08-02): pre-dogfood, with no users and no data to
preserve, the SQL migration chain (`0001`-`0006`) collapses to a single
baseline migration rather than growing another increment. This record's
schema (topics table + `messages.topic_id`) folds into that baseline; the
collapse itself is a repo-wide schema-hygiene task, sequenced after the
in-flight schema PRs land and not gated inside this record. If T2 starts
before the collapse lands, it lands as the next incremental migration that
still defines the final shape directly — either way, no `ADD/DROP` dance and
no data backfill (this is what retires F6's conversion).

### F9 — remove superseded proto fields outright; do not reserve

Matt's refinement (2026-08-02): pre-dogfood there are no external clients, so a
removed field needs no wire tombstone — remove it outright and reuse field
numbers freely, rather than `reserved`-ing it. This covers
`Message.parent_message_id` + the channel container, `PostMessageRequest.
parent_message_id`, and the `AgentFrame` conversation variants. Reserving
becomes the convention only once the service is usable (real clients on the
wire); until then it is needless ceremony.

### F10 — a message holds only `topic_id`, not `channel_id`

Matt's refinement (2026-08-02): a message carrying its own channel id is a wart
— the channel is already implied by the topic (`topics.channel_id`). The
`Message` row and proto store **only** `topic_id`; every channel-level read
resolves the channel through the topic join. Beyond removing the redundancy,
this makes moving a topic to a different channel a single-row update on
`topics.channel_id` with no message rewrite — the flexibility Matt named as
the point.

### F11 — the UI is Zulip's two-level drill-in

Matt's refinement (2026-08-02): the UI is ripped down to Zulip's navigation.
A **channel view shows only its topic index** plus a "new topic" affordance —
no message composer at the channel level. Clicking a topic drills into a
**topic view that shows the topic's messages and the composer**, with **no
nested threading** (you cannot make a thread within a thread). The **left
sidebar lists a channel's most recent topics** (default 3, a tunable UI
constant) beneath the channel row for one-click deep-nav to an active
conversation.
