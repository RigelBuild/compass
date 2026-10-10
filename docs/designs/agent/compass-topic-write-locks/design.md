# Refuse stale agent posts to a topic

Tracker: RIG-1714. Freezes on merge.

Builds on: [Zulip threading model](../../server/compass-zulip-threading-model/design.md), [inbound batching](../compass-agent-inbound-batching/design.md) (DL-401 to DL-405), [agent comms tools](../compass-agent-comms-tools/design.md), [manager role taxonomy](../compass-manager-role-taxonomy/design.md).

Ledger-impact: adds `docs/designs/decisions/agent/DL-472.md` to `DL-476.md` for the stale-post check that replaces the proposed topic write lock. All five rows read `Active (proposed, pending Matt, 2026-10-11)` until the load-bearing Open Questions they depend on are ruled.

## Problem / Intent

Two Managers can each draft a reply to the same message in a topic, and both post. Neither saw the other's post, so the topic gets two answers that may disagree. RIG-1714 proposes a per-topic write lock: a Manager takes the lock before it posts, no other Manager can post while it is held, a blocked Manager must read the holder's message before it sends, and the lock releases on post.

The property that matters is the last one: **an agent must not post into a topic while a message from someone else is there that it has not seen.** This record gets that property with an optimistic check at post time (compare-and-set on the topic) instead of a lock. How often double posts happen is unmeasured; T1 adds a counter so the rate becomes known.

## Approach

### What exists today

- **Posting.** `Comms.PostMessage` (`go/internal/comms/comms.go`) calls `Store.AppendMessage` (`go/internal/store/messages.go`). It runs participation, post policy, topic resolution and the insert in one transaction. The topic row is locked only after the insert, by the `last_seq` update in `insertMessageTx`: `if err := q.UpdateTopicLastSeq(ctx, db.UpdateTopicLastSeqParams{ID: topicID, LastSeq: row.Seq}); err != nil {`. No step compares the post with what the author has read.
- **Idempotency.** A retry with the same `client_request_id` returns the stored row: the insert hits `errMessageInsertConflict` and `AppendMessage` re-reads with `getMessageByRequestID`.
- **Agent tools.** `comms_post_message`, `comms_post_ask` and leg 2 of `comms_dm` (`packages/compass-agent/src/comms.ts`) send a `PostMessageRequest` that names the channel and topic: `topic: { case: "topicName", value: params.topic }`. The agent keeps no record of what it has read per topic.
- **When a message reaches the model.** A delivered message enters context at turn start (`CompassAgent.#flushTurnEnd`, `packages/compass-agent/src/agent.ts`). A deliver that arrives during a turn waits: "a mid-turn deliver waits for the `agent_end` flush". A mention during a turn is a steer: `this.#session.agent.steer(agentMsg)`. The SDK (`@oh-my-pi/pi-agent-core`, `agent-loop.ts`) injects queued steers only at the next model-call boundary and calls `(message as CommittableAsideMessage)[ASIDE_MESSAGE_COMMIT]?.();` as each one enters context. The steer ack does not wait for that: "`agent.steer` is synchronous void, so the ack rides the next microtask."
- **Tool batches.** The comms tools set no `concurrency`, so they are shared. The SDK scheduler runs `const start = concurrency === "exclusive" ? Promise.all([lastExclusive, ...sharedTasks]) : lastExclusive;`, so a later call in a batch can start after an earlier call returned. The model wrote every call in the batch before it saw any result.
- **Settle hold.** Delivery holds an agent-authored post until its author's session settles ([settle turn order](../../infra/runtime/compass-managed-settle-turn-order/design.md), DL-382). Human posts go at once (`Consumer.onMessagePosted`, `go/internal/delivery`: "Human-authored: settled at post, deliver immediately"). An agent can see a newer human message before an older peer reply.
- **Shared state.** Servers scale horizontally, and Postgres is the only store of record. Hub memory is an instance-local cache. Comms has no typing indicator.

### The change

DL-472. Each agent post carries the agent's view of the target topic: a **mark** (the message up to which it has seen everything from others) and a capped **seen set** of other messages it has seen. In the post transaction, the server locks the topic row. It refuses the post if another author has a message after the mark that is not in the seen set. The refusal returns those unseen messages, oldest first. The tool shows them to the model and does not post. The model reads them and decides whether to post again.

There is no lock state, no acquire or release RPC, no TTL and no waiter queue. The topic row lock lives for one transaction, not for a drafting turn. The first post wins. A later post that did not see it fails once, with the winner's text in hand.

```proto
message PostMessageRequest {
  // fields 1-7 unchanged; 8 and 9 belong to #2120
  // The poster has seen every message from others in the target topic up to
  // and including this one. Resolved only inside the target topic.
  string last_seen_message_id = 10;
  // Other messages in the target topic the poster has seen. At most 50.
  // Both fields empty: no check.
  repeated string seen_message_ids = 11;
}

message PostMessageResponse {
  Message message = 1;
  // field 2 belongs to #2120 (cost_estimate)
  // Set instead of `message` when the post was refused as stale.
  StalePostDetail stale_post = 3;
}

message StalePostDetail {
  // Messages from others the poster had not seen, oldest first, at most 20.
  repeated Message unseen_messages = 1;
  // True when more unseen messages follow the last one returned.
  bool truncated = 2;
}
```

### Q1: Granularity is the topic

DL-473. A topic is the thread. A channel-wide check would refuse posts into unrelated topics. The server finds a floor seq, then looks for unseen rows:

```sql
SELECT 1 FROM messages m
WHERE m.topic_id = $topic AND m.seq > $floor
  AND m.author_account_id <> $poster AND m.id <> ALL($seen)
```

- The mark and the seen ids resolve only inside the target topic (`topic_id = $topic`). The poster has already passed the channel participation check. The floor is the mark's seq. With no resolvable mark, it is the lowest seq among the resolvable seen ids: the agent's view of a topic begins at the first message it saw there. With neither, it is 0 ("seen nothing"). An id from another topic, or a forged id, never marks anything as seen.
- The poster's own messages never make a post stale.
- The read uses `messages_topic_seq_idx (topic_id, seq DESC)`. An `EXISTS` runs first; the message projection is read only for a refusal.
- Every insert takes `SELECT id FROM topics WHERE id = $1 FOR NO KEY UPDATE` **before** it inserts (`lockTopicTx`). That mode conflicts with itself, so appenders serialize, but not with the `FOR KEY SHARE` that foreign-key checks take. Within one topic, seq order is then commit order. The check cannot miss a later commit with a lower seq, which is the `BIGSERIAL` commit-lag hazard `SubscribeCommsResponse.snapshot_seq` documents.
- Cost: posts to one topic now serialize over the check queries and the insert, including its `search_tsv` GIN update. This is unmeasured. It is likely a few milliseconds per post, which agent traffic tolerates.
- `AnswerAsk` takes the same lock and runs no check. Today it locks the ask message (`FindAskMessage`, `FOR UPDATE OF m`) and then the topic, while a merge locks the topic (`ResolveTopicForUpdate`, `FOR UPDATE OF t`) and then its messages. An answer during a merge can deadlock. T1 makes `AnswerAsk` read the ask without a lock, lock the topic, then lock the message: topic before message, as merge does.

### Q2: Release and crash recovery

Nothing is held across RPCs, so nothing needs release.

- A crashed agent holds nothing. Postgres drops the row lock on commit, on rollback or on connection loss.
- A Server restart or failover loses nothing, because all state is in `messages`.
- An agent restart restores its view from its session. `TopicSeen` writes `compass_topic_seen` custom entries and restores them from `getEntries()`, as `TurnSequence` (`packages/compass-agent/src/turn-sequence.ts`) does.
- A topic merge (`MoveMessagesToTopic`) keeps message ids and seqs, so a mark still resolves after the merge. A post that waited on a topic a merge then deleted finds no row to lock and gets NOT_FOUND.

### Q3: What counts as seen

DL-474. `TopicSeen` keeps, per topic id, a mark and a seen set (the newest 50 ids). The server treats every message at or below the mark as seen, so the mark moves only on observations that are contiguous from it.

| Observation | Applied at | Effect |
| --- | --- | --- |
| Mid-turn steer | `ASIDE_MESSAGE_COMMIT` on the steer message, never its ack (the ack runs before the SDK injects the steer) | seen set |
| Turn-start prompt (deliver flush, idle steer) | the ack microtask, where "ack means injected" holds | seen set |
| `comms_list_messages` page from the head | next `turn_start` | mark to the topic's newest message in the page, if the topic has no mark, the page holds the mark, or the page is not full; else seen set |
| `comms_list_messages` page with `before_message_id` | next `turn_start` | seen set |
| Refused post | next `turn_start` | mark to the last unseen message returned |
| Accepted post | next `turn_start` | mark to the posted message |

- Steers and delivers never move the mark, because they arrive out of seq order. Example: Manager A's reply M1 is held by the settle hold. A human then @-mentions Manager B with M2. B's mark stays, M2 joins the seen set, and B's post is refused with M1. That is the issue's "must read the holder's message before it can send", made mechanical.
- Tool results apply at the next model-call boundary (`turn_start`, which `CompassAgent.#trackTurn` already sees), not when the tool returns. A post in the same batch as a list, or after a refused post, still carries the old view and is refused.
- A head page is a newest-first suffix of the channel. Within a topic, seq order is commit order (Q1), so the page holds every message of that topic down to the page's oldest row. It is contiguous from the mark only when it reaches the mark.
- An accepted post proves that every message from others after the floor was in the view.

### Q4: What a refused post returns

DL-475.

- **Store.** `AppendMessage` returns `*StalePostError` (the oldest 20 unseen messages and a `Truncated` flag) and inserts nothing.
- **Idempotency first.** If `client_request_id` already names a stored row, `AppendMessage` returns that row before the check. A retried post that landed is never reported stale.
- **One response shape.** `PostMessage` returns a normal response with `stale_post` set, `message` unset and no `MessagePosted`. #2120 uses the same response for its hold (`cost_estimate = 2`), so a post tool reads one shape: `message` means posted, `stale_post` means refused, `cost_estimate` means held. The relay already carries `PostMessageResponse` in `CommsCallResult.post`, so no relay arm and no `edgeError` case is needed.
- **Oldest first.** Each refusal moves the mark forward with no gap. A truncated refusal leaves the rest for the next attempt.
- **Bounded render.** The tool renders the unseen messages with the nonce-fenced transcript renderer `comms_list_messages` uses. Each body is capped at 2000 characters with a fence-carrying truncation marker, following `renderBody` and `BODY_BUDGET` in `packages/compass-agent/src/forge.ts`. One refusal adds at most about 40,000 characters.
- **No duplicate delivery.** At the `turn_start` that applies a refusal, `CompassAgent` removes the refused messages still in `#deliverQueue` and acks them, because the tool result injected them. "Ack means injected" still holds.
- **No auto-retry.** After reading, the reply may be redundant, and only the model can judge that.

### Q5: Who is checked

DL-476. Every agent, whatever its role. The issue says Managers, but the choice is really agents versus humans:

- The mesh has no worker role: workers are subagents with no account, and every account node spawns as supervisor, owner or manager ([manager role taxonomy](../compass-manager-role-taxonomy/design.md)). Owners and supervisors can double-post the same way.
- A role gate would add a role lookup to the post transaction. It would also leave legacy rows with an empty `Role` undefined.

The web UI and the CLI never set the view fields, so **humans are never checked and never blocked**. The public `PostMessage` honors the fields on purpose, so a human client could later warn "someone replied while you typed". A human's post does count as unseen for an agent. Both points are open questions.

### Q6: Fairness and order

First commit wins. The topic row lock serializes posts, and nobody waits longer than one transaction, so there is no queue to order. A losing agent needs a new message from someone else to lose again, so no livelock forms without new content. In a very busy topic an agent can lose several times in a row; the T1 counter measures this.

### Q7: Order against the cost hold

DL-475. #2120 holds an over-threshold agent post in `PostAsAccountByName` before the store runs. A post that is both stale and over the threshold would be held, confirmed, and only then refused.

- A non-locking pre-check, `Store.CheckStalePost`, runs before the hold. The check in the transaction stays authoritative. Whichever of this record and #2120 merges second wires the call.
- #2120's confirm re-send restamps both view fields from `TopicSeen` at confirm time, as it restamps `turn_sequence`.

## Alternatives considered

- **Hard per-topic lock (the issue's shape).** It needs acquire and release RPCs, a TTL with heartbeat so a crashed holder cannot wedge the topic, a waiter queue, and a bypass for humans. Servers scale horizontally, so the lock needs a new tenant table (or a JetStream KV bucket). A Postgres advisory lock cannot outlive one pooled connection, so it cannot span an LLM turn. A lock held while drafting serializes Managers for minutes. It still needs the stale check: because of the settle hold, a waiter that gets the lock may not have the holder's message. Rejected.
- **Per-channel lock.** Same costs, and it blocks unrelated topics. Rejected.
- **Server-side "seen"** (from the delivery cursor, or from what the server returned). The server sees "delivered" or "returned", never "in context". The mid-turn steer ack and the `SessionInjection` frame both fire in the ack microtask, before `ASIDE_MESSAGE_COMMIT`; only the agent sees the commit. Rejected.
- **Mark only, no seen set.** One field, but a steer or a held reply makes the mark skip a message the agent never saw (Q3 example). Open Question 2, option C.
- **Refusal as an ABORTED error with a Connect error detail and a relay arm.** Open Question 4, option A.
- **Advisory composing presence only.** It lowers the chance of a race but cannot prevent one. Rejected as the fix; adding it beside the check is deferred until the counter shows refusals are frequent.
- **Automatic retry inside the tool.** This would post a reply the model wrote before it saw the unseen messages, which is the bug. Rejected. Open Question 2, option B is a narrow retry for messages already in context.
- **Do nothing.** Double posts stay possible and stay unmeasured. Rejected.

## Global Constraints

- Public repo: name no private repository, path or PR in code, tests or docs.
- No new table and no edit to `go/internal/store/migrations/0001_init.sql`. All server state is in `messages` and `topics`.
- Proto numbers: `PostMessageRequest` 8 and 9 and `PostMessageResponse` 2 belong to open PR #2120 (`estimate_only`, `confirmed_estimate_tokens`, `cost_estimate`). This record takes `PostMessageRequest` 10 and 11 and `PostMessageResponse` 3. It adds no `CommsCallRequest` or `CommsCallResult` arm, so open PRs #2082 and #2078 do not collide. If a number is taken when this lands, use the next free one. Never reuse a number.
- Both view fields empty means no check; neither is required. More than 50 `seen_message_ids` is INVALID_ARGUMENT.
- Human clients (web UI, CLI) never set the view fields.
- At most 20 unseen messages per refusal (`staleUnseenLimit`), oldest first. Each refused body renders at most 2000 characters.
- The topic lock mode is `FOR NO KEY UPDATE`, taken before the insert.
- Agent tools never auto-retry a stale post. Every tool result stays one text block, as the list renderer requires.
- Regenerate and commit generated code: `moon run compass-go:sqlc-gen` for `go/internal/store/db`, and the proto `gen` task. `sqlc-drift` and proto `drift` gate them. `StalePostDetail` is public, so no `gen-fence` edit is needed.
- Store tests that need Postgres go behind the existing `pgtest && unix` build tags. Agent tests use `bun:test`.

## Plan

Two tasks, in order. Each one is a PR with its own tests.

1. **T1, server.** The topic lock, the check, the proto fields and response arm, the field copies, the counter and the `AnswerAsk` lock order. The store check and the wire field ship together, so nothing lands that no caller can reach.
2. **T2, agent.** `TopicSeen`, the view on the three post paths, the refusal render, the deliver-queue cleanup, persistence and docs. Depends on T1's generated TypeScript.

## Tasks

### T1: Server check and wire

Files: `go/internal/store/queries/messages.sql`, `go/internal/store/db/*` (generated), `go/internal/store/types.go`, `go/internal/store/errors.go`, `go/internal/store/messages.go`, `proto/compass/v1/comms.proto`, generated proto trees, `go/internal/comms/comms.go`, `go/internal/comms/agent_caller.go`, a new `go/internal/store/messages_stale_pgtest_test.go`, and comms tests.

Interfaces:

```go
// types.go: TopicRef gains the poster's view. Both empty: no check.
type TopicRef struct {
	ID                string
	Name              string
	Create            bool
	LastSeenMessageID MessageID
	SeenMessageIDs    []MessageID
}

// errors.go
type StalePostError struct {
	Unseen    []Message // oldest first, at most staleUnseenLimit
	Truncated bool
}

func (e *StalePostError) Error() string // fixed text: "store: stale post"

// messages.go
const (
	staleUnseenLimit  = 20
	maxSeenMessageIDs = 50
)

// lockTopicTx takes the topic row lock; no row (merged away) is ErrNotFound.
func lockTopicTx(ctx context.Context, tx pgx.Tx, topicID string) error

// staleCheckTx returns *StalePostError or nil; the caller holds the topic lock
// for the authoritative check.
func staleCheckTx(ctx context.Context, tx pgx.Tx, author AccountID, topicID string, view TopicRef) error

// CheckStalePost is the non-locking pre-check for the cost hold (Q7): it checks
// participation, resolves the topic read-only (a name-miss returns nil) and
// runs staleCheckTx in a read transaction.
func (s *Store) CheckStalePost(ctx context.Context, author AccountID, channelID string, topic TopicRef) error
```

```sql
-- name: LockTopicForAppend :one
SELECT id FROM topics WHERE id = $1 FOR NO KEY UPDATE;

-- name: TopicSeqsByIDs :many
SELECT id, seq FROM messages WHERE topic_id = $1 AND id = ANY($2::text[]);

-- name: TopicHasUnseen :one
SELECT EXISTS (
  SELECT 1 FROM messages m
  WHERE m.topic_id = $1 AND m.seq > $2 AND m.author_account_id <> $3
    AND m.id <> ALL($4::text[]));

-- name: UnseenTopicMessages :many
-- The ListMessages projection and handle joins, the TopicHasUnseen predicate,
-- ORDER BY m.seq ASC LIMIT $5.

-- name: LockMessageForAnswer :one
SELECT topic_id, blocks FROM messages WHERE id = $1 FOR UPDATE;
```

Steps in `AppendMessage`, after `resolveTopicForAppend`:

1. Reject more than `maxSeenMessageIDs` seen ids with `ErrInvalidArgument` (before the transaction).
2. `lockTopicTx(ctx, tx, topicID)`.
3. If `clientRequestID != ""`, run `GetMessageByRequestID` on the tx. A row means a landed retry: return it with `inserted=false`.
4. If either view field is set, run `staleCheckTx`. Floor: the mark's seq from `TopicSeqsByIDs`; else the lowest resolved seen seq; else 0. If `TopicHasUnseen`, read `UnseenTopicMessages` with limit `staleUnseenLimit+1` and return `&StalePostError{Unseen: first 20, Truncated: len > 20}`.
5. `insertMessageTx` as today.

`AnswerAsk`: drop `FOR UPDATE OF m` from `FindAskMessage`, then `lockTopicTx` on the ask's topic, then `LockMessageForAnswer`. Use the re-read blocks for the answered check. If the topic is gone, or the message moved to another topic, return `ErrNotFound`.

`PostMessage` passes both fields into the `TopicRef` it already builds. On `errors.AsType[*store.StalePostError]`, it adds 1 to the counter and returns `PostMessageResponse{StalePost: &StalePostDetail{UnseenMessages: messagesToWire(e.Unseen), Truncated: e.Truncated}}` without publishing. `PostAsAccountByName` and `defaultChannel` (`go/internal/comms/agent_caller.go`) build a new `PostMessageRequest` field by field; both must copy `LastSeenMessageId` and `SeenMessageIds`, or the agent path drops them silently. The counter `compass.comms.stale_post_rejections` (Int64Counter) is created next to `compass.delivery.fabric_publish_failures` in the `Comms` constructor; a metric error never fails construction.

Tests:

- A posts M1 after M0; B posts with mark M0. B's response has `stale_post` with `[M1]`, no `message`, no `MessagePosted`, and no row is inserted. The counter adds 1.
- B's own posts never make B's post stale. A mark equal to the newest message is accepted. Both fields empty is accepted.
- M1 in the seen set is accepted. With no mark, the floor is the lowest seen seq: messages before it are not unseen.
- A mark from another topic in the same channel, and a forged mark, count as seen nothing.
- A retry with the same `client_request_id` after a newer message returns the stored row with `inserted=false`.
- 21 unseen messages return the oldest 20 and `truncated=true`. 51 seen ids is INVALID_ARGUMENT.
- Two concurrent posts with the same mark: exactly one is inserted, and the other is refused with it.
- A concurrent topic merge and `AnswerAsk` never fail with a deadlock (40P01).
- `PostAsAccountByName` keeps both fields (shown by a refusal through that path). `CheckStalePost` agrees with the in-transaction check.

### T2: Agent view tracker and tools

Files: new `packages/compass-agent/src/topic-seen.ts`, `packages/compass-agent/src/comms.ts`, `packages/compass-agent/src/agent.ts`, `packages/compass-agent/src/cli.ts`, `comms.test.ts`, `agent.test.ts`, new `topic-seen.test.ts`, `docs/concepts/tools.md`, `config/skills/comms-playbook/SKILL.md`.

Interfaces:

```ts
// topic-seen.ts
export interface SeenMessage {
	readonly id: string;
	readonly topicId: string;
}
export interface TopicView {
	readonly lastSeen: string; // "" when no mark
	readonly seen: readonly string[]; // at most 50
}
export type Observation =
	| { kind: "headPage"; messages: readonly SeenMessage[]; full: boolean } // newest first
	| { kind: "olderPage"; messages: readonly SeenMessage[] }
	| { kind: "refused"; unseen: readonly SeenMessage[] } // oldest first
	| { kind: "posted"; message: SeenMessage };
export class TopicSeen {
	/** Restores the latest compass_topic_seen entry per topic and per name. */
	constructor(manager: SessionManager);
	/** A steer or deliver entered context: seen set only, never the mark. */
	saw(m: SeenMessage): void;
	/** Learn that (channelName, topicName) is topicId. Topic names fold to lower case. */
	name(channelName: string, topicName: string, topicId: string): void;
	/** Buffer a tool-result observation until the next model call. */
	stage(obs: Observation): void;
	/** Apply staged observations (Q3 table); returns the refused message ids. */
	commitStaged(): string[];
	/** The view for the named topic; empty when unknown. */
	view(channelName: string, topicName: string): TopicView;
}

// comms.ts
export function createCommsTools(
	broker: CommsBroker,
	turnSequence?: TurnSequence,
	topicSeen?: TopicSeen,
): AgentTool[];
export function renderTranscript(messages: readonly Message[], bodyBudget?: number): string;

// agent.ts: CompassAgentOptions gains
readonly topicSeen?: TopicSeen;
```

- `cli.ts` builds one `TopicSeen` on the session manager and passes it to both `createCommsTools` and `CompassAgent`, as it does `TurnSequence`.
- `TopicSeen` writes one `compass_topic_seen` custom entry each time a topic's view or a name changes.
- **`agent.ts`.** In the `#flushTurnEnd` and idle-steer ack microtasks, for each message, before its source names are deleted: `name(...)` from `DeliverSourceNames`, then `saw(msg)`. For a mid-turn steer, set `[ASIDE_MESSAGE_COMMIT]` on `agentMsg` to a callback that does the same, never in its ack microtask. In `#trackTurn`, on `turn_start`, call `commitStaged()`. Remove the returned ids still in `#deliverQueue` and emit their `DeliveryAck` and DELIVER injection, as `#flushTurnEnd` does.
- **`comms.ts`.** `comms_post_message`, `comms_post_ask` and `comms_dm` leg 2 send `lastSeenMessageId` and `seenMessageIds` from `topicSeen?.view(channel, topic)`. On `message`: `name(channel, topic, posted.topicId)` and `stage({ kind: "posted", ... })`. On `stalePost`: `name(...)` from the first unseen message's topic id, `stage({ kind: "refused", ... })`, then throw an `Error` with the lead line "Not posted: this topic has messages you had not seen, oldest first. Read them; post again only if your reply still fits." and `renderTranscript(unseen, 2000)`. When `truncated`, add "More unseen messages follow; the next attempt shows them."
- `comms_list_messages` stages `headPage` when `before_message_id` is empty, else `olderPage`. `full` is `messages.length` equal to the requested limit after the server clamp (0 means 50; at most 200).
- Lift the list tool's transcript renderer to the module-level `renderTranscript`. `bodyBudget` truncates each body with a fence-carrying marker, as `renderBody` in `forge.ts` does. The list tool passes no budget.
- The three post tool descriptions add one sentence: a post into a topic with unseen messages is refused, and the messages are shown.
- Docs: describe the refusal in `docs/concepts/tools.md` and in the comms playbook.

Tests (`bun:test`):

- A post into a known topic carries its view; an unknown topic carries empty fields.
- In a batch of [list, exclusive tool, post], the post does not carry the mark the list set.
- A mid-turn steer joins the seen set only when the commit callback runs, not on its ack, and never moves the mark.
- The settle-hold case: deliver M0, then steer M2. The mark is unchanged, and the post carries seen `[M0, M2]`.
- A `stalePost` result throws the fenced, budgeted, oldest-first render. After the next `turn_start` the mark is the last unseen message, and a queued deliver of a refused id is acked and removed from the queue.
- Head page rules: the mark moves when the topic has no mark, the page holds the mark, or the page is not full; otherwise the page goes to the seen set.
- A new `TopicSeen` on the same session manager restores views and names.
- `comms_dm` leg 2 carries the view.

### Checklist

- [ ] T1: `lockTopicTx` (`FOR NO KEY UPDATE`), idempotency-first lookup, floor and seen-set check, `StalePostError`, `CheckStalePost`, proto fields 10 and 11, `stale_post = 3`, agent-caller field copies, counter, `AnswerAsk` lock order, tests.
- [ ] T2: `TopicSeen` with persistence, observe and stage points, `commitStaged` on `turn_start`, view on three post paths, budgeted refusal render, deliver-queue cleanup, shared renderer, docs, tests.

## Open Questions

1. **Feature shape: a stale-post check instead of the issue's lock (load-bearing).** The issue asks for a per-topic write lock. This record recommends a check at post time, which changes the feature shape.
   - Option A, stale-post check only: no lock state, no new table, nothing can wedge, humans are untouched. A Manager may write a draft that is then refused.
   - Option B, hard per-topic lock as filed: acquire and release RPCs, TTL and heartbeat, a waiter queue, a new tenant table, a human bypass, and still a stale check for the settle-hold case.
   - Option C, option A plus advisory composing presence now: a new `SubscribeComms` payload arm (20 or later, after open-PR collisions) and agent-side "composing" signals, to avoid wasted drafts.
   - **Recommendation: A.** Add C's presence only if the counter shows refusals are frequent.
2. **How the check knows what the agent saw (load-bearing).** Steers and settle-held replies arrive out of seq order, so a single mark cannot record them.
   - Option A, send a capped seen set (`seen_message_ids = 11`, at most 50): no extra round trip. This is the Plan.
   - Option B, no seen-set field: keep the seen set local, and when every refused message is already in it, retry once with the mark at the last refused message. This changes DL-475's "no auto-retry" and costs a round trip after each out-of-order observation. Plan delta: drop field 11 and the server's `<> ALL($seen)` and floor-from-seen; add the guarded retry in the three post tools.
   - Option C, mark only, and accept the gap: a held peer reply below a steered message is never checked, which fails the record's own property. Plan delta: drop field 11 and the agent seen set.
   - **Recommendation: A.**
3. **Human posts make an agent's post stale (load-bearing).** The issue covers Manager against Manager. With this design, a human's unseen message also refuses an agent's post.
   - Option A, every author counts: an agent never answers past a human's correction. Agents in busy human topics see more refusals.
   - Option B, only agent authors count: matches the issue's scope. Plan delta: the unseen predicates add `EXISTS (SELECT 1 FROM agent_accounts aa WHERE aa.account_id = m.author_account_id)`.
   - **Recommendation: A.**
4. **How a refusal reaches the agent (load-bearing).** Public API shape, shared with #2120 on the same RPC.
   - Option A, an ABORTED Connect error with a `StalePostDetail` error detail, plus a relay arm `CommsCallResult.stale_post = 15`. Plan delta: `StalePostError` unwraps to `ErrVersionConflict`; `edgeError` maps that to `CodeAborted`; `executeCall` reads the store error with `errors.As` and returns the arm; the tools branch on a second result shape; result arm 15 is then taken from a future request arm 15.
   - Option B, `PostMessageResponse.stale_post = 3`, the same shape as #2120's `cost_estimate = 2`. This is the Plan.
   - **Recommendation: B.**
5. **Order against #2120's cost hold (load-bearing).** The order of agent-visible outcomes across two records.
   - Option A, a non-locking stale pre-check before the hold, added to #2120's pre-check list; the in-transaction check stays authoritative. This is the Plan.
   - Option B, hold first; a confirmed post may then be refused as stale, which wastes a confirm. Plan delta: drop `CheckStalePost`.
   - Both options restamp both view fields on #2120's confirm re-send.
   - **Recommendation: A.**
6. **The public RPC honors the view fields (load-bearing).** A public contract choice.
   - Option A, yes, on purpose, so a human client can later warn "someone replied while you typed". This is the Plan.
   - Option B, agent path only, like #2120's fields 8 and 9 ("the human RPC ignores"). Plan delta: `PostMessage` drops both fields unless the actor is an agent account, and the public field comments say so.
   - **Recommendation: A.**
7. **Fail-open gaps (not load-bearing).** No check runs, as today, when an agent has never seen the topic, after a rename (the name map is stale), or when a delivered channel name is spelled differently from the posted one. With no mark, the view begins at the first message the agent saw there. Older messages count as seen, and so do messages merged in from another topic below the mark. Each of these limits a check that does not exist today. None of them weakens the main case: two agents replying to a message both were delivered.
8. **Stale playbook line (not load-bearing).** `config/skills/comms-playbook/SKILL.md` still says that an unknown topic name creates the topic, which `create_topic` contradicts. It is outside this record and needs its own small fix.
