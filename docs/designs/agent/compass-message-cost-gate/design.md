# Compass message cost gate

Tracker: RIG-1713. Freezes on merge.

Builds on: [compass-manager-comms-substrate](../compass-manager-comms-substrate/design.md), [compass-agent-comms-tools](../compass-agent-comms-tools/design.md), [compass-agent-inbound-batching](../compass-agent-inbound-batching/design.md).

Ledger-impact: adds `docs/designs/decisions/agent/DL-459.md` to `DL-465.md`, one row per `Q` heading below (Q1 to Q7 in order).

## Problem / Intent

One agent post can wake many agents. Each woken agent runs a turn, and that turn re-reads the agent's whole context. A long-idle agent has a cold prompt cache, so its re-read costs the most. Today a Manager cannot see this cost before it posts.

This record adds a server-side cost estimate for an agent post, and a gate: an agent post whose estimate is over an operator threshold is held until the agent confirms it. Human posts are never gated.

## Approach

### What exists today

- **Post path.** `comms_post_message`, `comms_post_ask` and `comms_dm`'s second leg (`createCommsTools`, `packages/compass-agent/src/comms.ts`) send the `post` arm. `executeCall` (`relay_comms.go`) calls `linkTrigger`, then `PostAsAccountByName` (`go/internal/comms/agent_caller.go`): "This is the ONLY caller that treats the container arm as a name". Internal posts (`CommitAgentPost`) and the human `PostMessage` RPC bypass it. The tools throw "protocol violation" on a result with no `message`.
- **Store checks.** `AppendMessage` checks, in one transaction: membership (`requireChannelMember`), `OWNER_ONLY`, the topic (`resolveTopicForAppend`), then the `(author, client_request_id)` dedup.
- **Recipient set.** `fanOut` (`go/internal/delivery/dispatch.go`) steers live mentioned agents (`routeMentionsFor`), delivers to the other `SubscribedAgents`, and wakes offline ones; for a despawned agent `wakeOnce` (`go/server/lifecycle.go`) is "a logged no-op". Agent posts fire at the author's settle, ordered by `turn_sequence` (DL-382), and `fireHeld` re-resolves recipients then. Delivery already imports comms.
- **Context and last-active.** The agent tees each SDK session line into `agent_session_transcript_entries.entry_json` (`session-tee.ts`); a full-body rewrite becomes one `checkpoint` row. Every entry has a `timestamp` (`SessionEntryBase`). An assistant message has `usage.input`, `cacheRead` and `cacheWrite` (`AssistantMessage`, `pi-ai`); their sum is the context the model last read. Presence is in memory with no timestamp (DL-074).
- **Prompt cache.** The SDK's Anthropic provider writes five-minute cache entries by default (`resolveCacheRetention(cacheRetention, "short")`).
- **Root-only channels.** Not enforced yet: "[TODO RIG-1722: the restricted-post ACL is not yet an enforced primitive" (`config/skills/supervisor-channel/SKILL.md`). The gate does not depend on it.
- **Wire numbering.** `CommsCallRequest` arms 2 to 11 are used; 12 (`list_topics`, open PR #2082) and 13 and 14 (`update_board`, `boards`, draft PR #2078) are taken. A new arm would start at 15; this design adds none. `PostMessageRequest` next free field is 8; `PostMessageResponse` next free is 2.

### The change

1. A post tool that can handle a hold sets `accepts_hold` (Q5). For such a post, `PostAsAccountByName` runs the pre-checks (Q6), then estimates the cost (Q1, Q2).
2. Over `COMPASS_MESSAGE_COST_THRESHOLD` (Q3), the server does not post. It returns `cost_estimate` with `held = true` and `message` unset.
3. The agent calls `compass_message_confirm(handle)`. The tool re-sends the original request with `confirmed_estimate_tokens`. The server recomputes and posts if the cost has not grown more than 10% (Q4).
4. `compass_message_cost` asks for the estimate without posting (`estimate_only`).

```mermaid
sequenceDiagram
  participant A as Agent tool
  participant C as PostAsAccountByName
  participant S as Store.AppendMessage
  A->>C: post (accepts_hold)
  C->>C: pre-checks, estimate
  alt estimate <= threshold
    C->>S: append
    C-->>A: message
  else estimate > threshold
    C-->>A: cost_estimate, held (nothing posted)
    A->>C: compass_message_confirm → same post + confirmed_estimate_tokens
    C->>C: recompute; new <= confirmed × 1.1
    C->>S: append
    C-->>A: message
  end
```

### Q1: Cost model

Ledger row: DL-459.

The estimate is in input-token equivalents at the uncached price. For one post:

- `m` = message tokens = `ceil(text bytes / 4) + 50`. Text bytes sum the text of every `MessageBlock`; `50` covers the deliver envelope.
- `ctx_r` = `min(maxContextTokens, input + cacheRead + cacheWrite)` from the newest assistant entry's `usage` in `r`'s newest session (T3). `maxContextTokens = 200000`. No session or no assistant entry gives `ctx_r = 0`.
- `w_r` = `0.1` if `r` is warm, `1.25` if cold (the cache-read and cache-write price ratios). `r` is warm when its presence is WORKING, or its newest entry is within `warmWindow = 5 min` (the default cache entry life). Otherwise it is cold (Q7).
- Per recipient: a mentioned recipient that is WORKING is steered into its running turn, so it costs `m`. Any other recipient costs `m + ctx_r × w_r`. A recipient with no placement is excluded: its wake is a no-op, so the post starts no turn for it.
- `estimate` = the sum over recipients.

The estimate is approximate both ways: it leaves out the system prompt, reply tokens and extra model calls in a turn, and can count a long tool call as warm. After a compaction, the next assistant `usage` reflects the compacted context, so the compaction posture lowers estimates.

### Q2: Where the estimate is computed

Ledger row: DL-460.

Server-side, in the comms service: only the server holds the recipient set, live presence and transcript state, and an agent-side estimate would race the post.

Delivery already imports comms, so T2 moves the mention parse and resolve into package comms, keeping one mention body ("a second mention-routing body is a second correctness surface and is prohibited", `routeMentionsFor`). The estimate is taken at post time and `fanOut` re-resolves at settle, so small drift is expected.

### Q3: Threshold and its default

Ledger row: DL-461.

One server-wide setting: flag `--message-cost-threshold`, env `COMPASS_MESSAGE_COST_THRESHOLD`, in input-token equivalents. It is a string parsed at startup like `resolveUsageEventRetention` (`go/cmd/compass-server/main.go`), not `positiveCap`, which treats `0` as unset. Empty means the default, `0` turns the gate off, and a non-integer fails startup.

The default is `600000`, about two cold full-context recipients, so a single-peer post never trips it (OQ1). A deployment chooses the value (see [self-host and managed](../../../concepts/self-host-and-managed.md) and the [OSS core boundary](../../meta/oss-core-managed-boundary/design.md)).

For tuning, comms counts outcomes on `compass.message_cost_gate` (`compass_message_cost_gate_total`, attribute `outcome` = `posted`, `held`, `confirmed` or `estimate_only`), built like the `fabricPublishFailures` counter in `go/internal/comms/comms.go`.

### Q4: Per-message confirm, no session opt-out

Ledger row: DL-462.

Every held post needs its own confirm. There is no session-level opt-out: a Manager that compacts loses the memory that it opted out, so the gate's state would be invisible to the agent it governs.

The confirm binds to cost, not identity. The tool re-sends the held request with `confirmed_estimate_tokens`. The server recomputes, posts if `new ≤ confirmed × 1.1`, and otherwise holds again with the fresh estimate. No key or server state is needed, and a shrinking audience never re-holds.

The re-sent request keeps the original `client_request_id`, so a repeated confirm lands once, and restamps `turn_sequence` to the current turn (DL-382).

### Q5: Wire and tool shape

Ledger row: DL-463.

The gate rides the existing `post` arm, so no arm is added and `relay_arm_coverage_test.go` needs no new row.

- `PostMessageRequest`: `estimate_only = 8`, `confirmed_estimate_tokens = 9`, `accepts_hold = 10`.
- `PostMessageResponse.cost_estimate = 2` (new message `MessageCostEstimate`, with `bool held`).
- `message` set means posted. `message` unset and `cost_estimate` set means not posted: `held` tells a hold from `estimate_only`.
- The server never holds a request without `accepts_hold`; it posts as today. Older agent images, which throw on an unset `message`, keep working.
- `executeCall` skips `linkTrigger` for `estimate_only`, because nothing is posted.

A typed result is needed: `commsCallError` carries only a code and a message.

The two tools from the issue stay:

- `compass_message_cost({channel, topic?, text})` sends `estimate_only` and renders the estimate.
- `compass_message_confirm({handle})` re-sends a held request (Q4). Held requests stay in tool memory under a local handle for 10 minutes; an unknown handle tells the agent to post again.

The post tools render a held result as "not posted: estimated N tokens over threshold T across R recipients (C cold); call compass_message_confirm with handle H to post". This changes their result contract (OQ2).

### Q6: Gate order against the post ACL

Ledger row: DL-464.

The gate never holds a post the store will deny. In `PostAsAccountByName`, after `ChannelByNameForViewer` resolves the channel, and before any estimate:

1. If `client_request_id` is set and a message exists for `(author, client_request_id)`, return it. A retried post that already landed is never reported "not posted".
2. If the author is not a channel member (`Store.IsChannelMember`, the same probe `requireChannelMember` uses), skip the gate.
3. If `create_topic` is false and the topic does not exist in the channel, skip the gate.
4. If the channel is `OWNER_ONLY` and the author is not its owner, skip the gate.

A skipped gate falls through to `PostAsAccount`, which returns the store's own error. The store stays the authority. For `estimate_only`, steps 2 to 4 return that same error directly, and nothing is posted.

### Q7: Last-active from the transcript

Ledger row: DL-465.

Last-active is the `timestamp` of the newest entry in `r`'s newest session, read by the T3 query that reads `ctx_r`. No table, map or interface is added, and a restart or second replica sees the same value. Live presence comes from comms' existing `PresenceSource`.

## Alternatives considered

- **Checkpoint-onward transcript bytes as `ctx_r`.** A checkpoint comes from an SDK full-body rewrite, not from every compaction, and the stored body keeps entries the loader drops. Bytes/4 is wrong both ways, and the 64 MiB safety-valve cap leaves it unbounded. Rejected for observed `usage`.
- **In-memory last-active map in presence.** Empty after a restart, split across replicas, and stamped at drain time, not edge time. Rejected for the transcript timestamp.
- **HMAC token over a message and recipient digest.** Needs a per-process key, breaks across restarts and replicas, and re-holds when the audience shrinks; the agent-side map already binds the message. Rejected for the cost-bound confirm.
- **Client-side estimate.** The agent lacks the subscriber set, presence and transcript state, and its estimate races the post. Rejected.
- **New arms for estimate and confirm.** Two arms (15, 16), two `CommsCaller` methods and two coverage rows for behavior the post arm already carries. Rejected.
- **Gate in `Store.AppendMessage`.** It would also gate human and internal posts. Rejected.
- Advisory-only and session opt-out: see OQ2 and Q4.

## Plan

### Global Constraints

- Proto fields use the explicit tags in Q5. Regenerate with `moon run compass-proto:gen` and commit the output. `MessageCostEstimate` is public (`comms.proto`), so it needs no `gen-fence` entry in `proto/moon.yml`.
- No new table and no edit to `go/internal/store/migrations/0001_init.sql`. New SQL is one sqlc query.
- No new `CommsCallRequest` or `CommsCallResult` arm. If a later change needs one, it starts at 15.
- Human `CommsService.PostMessage`, `PostAsAccount` and `CommitAgentPost` stay ungated. A request without `accepts_hold` is never held.
- `COMPASS_MESSAGE_COST_THRESHOLD=0` restores today's behavior exactly.
- No tracker ids in source comments. No names of private repositories.
- Existing test assertions stay unmodified. Tests that move with code in T2 keep their assertions.
- Go: errors through `edgeError` and the existing connect codes. TS: render failures through `commsFailure`.

### T1: Wire fields (lane proto)

- Add the fields and message below to `proto/compass/v1/comms.proto`.
- Document on `PostMessageRequest` that the human RPC ignores fields 8 to 10.
- Tests: none beyond generation; the behavior lands in T4.

Interfaces:

```proto
message MessageCostEstimate {
  // Input-token equivalents at the uncached price (design: compass-message-cost-gate Q1).
  uint64 estimated_tokens = 1;
  // The server threshold in force; 0 means the gate is off.
  uint64 threshold_tokens = 2;
  uint32 recipient_count = 3;
  uint32 cold_recipient_count = 4;
  // True when the post was held for confirmation; false for estimate_only.
  bool held = 5;
}

message PostMessageRequest {
  // ... fields 1-7 unchanged ...
  bool estimate_only = 8;
  uint64 confirmed_estimate_tokens = 9;
  bool accepts_hold = 10;
}

message PostMessageResponse {
  Message message = 1;
  MessageCostEstimate cost_estimate = 2;
}
```

### T2: Shared mention body in comms (lane server)

- Move `mentionRE`, `parseMentions`, `mentionHandles`, `resolveMentioned` and `reservedMentions` from `go/internal/delivery` into `go/internal/comms/mentions.go`. Move the `parseMentions` table test with it.
- Delivery's `routeMentionsFor` calls the comms functions. On error it logs the same lines with the same attributes ("delivery: resolve channel agent members for mention routing", "delivery: resolve author owner for mention routing") and drops all mentions, as today.
- Update the `go/internal/store/handle.go` comments that name `delivery.mentionRE` and delivery's `reservedMentions` to name comms.
- Tests: existing delivery mention tests pass unchanged.

Interfaces:

```go
package comms

// MentionReads is the store surface mention resolution needs; *store.Store satisfies it.
type MentionReads interface {
    ChannelAgentMembers(ctx context.Context, channel store.ChannelID, author store.AccountID) ([]store.ChannelAgentMember, error)
    ResolveOwner(ctx context.Context, caller store.AccountID) (store.AccountID, error)
}

// ErrMentionMembers and ErrMentionOwner wrap ResolveMentions' two read failures,
// so delivery keeps its two log lines.
var ErrMentionMembers, ErrMentionOwner error

// MentionHandles returns the @-handles in blocks' text, in order, deduplicated.
func MentionHandles(blocks []*compassv1.MessageBlock) []string

// ResolveMentions returns the mentioned channel agents author may reach.
func ResolveMentions(ctx context.Context, r MentionReads, channel store.ChannelID, author store.AccountID, handles []string) (map[store.AccountID]bool, error)
```

### T3: Recipient state read (lane server)

- Add sqlc query `RecipientCostState` in `go/internal/store/queries/agent_transcripts.sql`. Per account, for its newest session (`recorded_at_unix_ms DESC, session_id DESC`, as `LatestSessionForAccount`), it returns:
  - whether an `agent_placements` row exists;
  - `message.usage.input + cacheRead + cacheWrite` of the newest non-checkpoint assistant row;
  - the newest row's `timestamp`.
- Cast `entry_json` to `jsonb` only for non-checkpoint rows (a checkpoint body is many lines); when a value lives only in the checkpoint body, the Go method parses that body from its end.
- Add `Store.MessageByRequestID` (exports `getMessageByRequestID`) and `Store.TopicInChannel` (over `GetTopicChannel` and `GetTopicByName`) for Q6.
- Tests (pgtest): usage sums the three fields of the newest assistant delta; usage and timestamp come from a checkpoint body when no later delta has them; only the newest session counts; no session is absent from the map; no placement gives `Placed = false`.

Interfaces:

```go
type RecipientCostState struct {
    ContextTokens    int64 // input + cacheRead + cacheWrite; 0 when unknown
    LastActiveUnixMs int64 // 0 when unknown
    Placed           bool
}

func (s *Store) RecipientCostStates(ctx context.Context, agents []AccountID) (map[AccountID]RecipientCostState, error)
func (s *Store) MessageByRequestID(ctx context.Context, author AccountID, clientRequestID string) (Message, bool, error)
func (s *Store) TopicInChannel(ctx context.Context, channel ChannelID, topic TopicRef) (bool, error)
```

### T4: Estimate and gate (lane server)

- New file `go/internal/comms/cost_gate.go` with the estimator (Q1) and the gate.
- `PostAsAccountByName` order:
  1. Resolve the channel.
  2. If neither `estimate_only` nor (`accepts_hold` and threshold > 0), post as today.
  3. Run the Q6 pre-checks.
  4. Recipients = `SubscribedAgents(channel, account)` ∪ `ResolveMentions(...)`, minus the author.
  5. Read `RecipientCostStates` and `PresenceFor`, then estimate.
  6. `estimate_only` → return the estimate. Under the threshold → post. `confirmed_estimate_tokens > 0` and `estimate ≤ confirmed × 1.1` → post. Otherwise → hold; nothing is appended.
- In `executeCall`, skip `linkTrigger` when `estimate_only` is set.
- Add `resolveMessageCostThreshold` in `go/cmd/compass-server/main.go`, fed by `firstNonEmpty(*f.messageCostThreshold, os.Getenv("COMPASS_MESSAGE_COST_THRESHOLD"))`, into `ServeConfig.MessageCostThreshold`. Wire it with `SetCostGate` in `go/server/serve.go` where the comms service is built.
- Add the Q3 counter beside `fabricPublishFailures`.
- Tests:
  - Under the threshold posts; over it holds and appends nothing; without `accepts_hold` never holds; `estimate_only` never posts; threshold 0 reads no recipients.
  - A confirm within 10% posts; a grown audience holds again; a smaller one posts.
  - A stored `client_request_id` returns the stored message. A non-member, a missing topic with `create_topic` false, and a non-owner `OWNER_ONLY` post each return the store error, not a hold.
  - A WORKING mentioned recipient costs `m`; no placement costs nothing; cold costs more than warm; `ctx_r` clamps at `maxContextTokens`.
  - `resolveMessageCostThreshold` maps empty to the default and `0` to off, and rejects junk.

Interfaces:

```go
type CostGateConfig struct {
    ThresholdTokens  uint64        // 0 = off
    WarmWindow       time.Duration // 5 * time.Minute
    MaxContextTokens int64         // 200000
    ConfirmSlack     float64       // 1.1
    Now              func() time.Time
}

func (c *Comms) SetCostGate(cfg CostGateConfig)

func (c *Comms) estimateCost(ctx context.Context, author store.AccountID, channel store.ChannelID, blocks []*compassv1.MessageBlock) (*compassv1.MessageCostEstimate, error)

// go/cmd/compass-server/main.go
func resolveMessageCostThreshold(v string) (uint64, error)
```

### T5: Agent tools and docs (lane agent)

- In `packages/compass-agent/src/comms.ts`, add `compass_message_cost` and `compass_message_confirm` to `createCommsTools` (eleven tools become thirteen).
- `comms_post_message`, `comms_post_ask` and `comms_dm` set `acceptsHold`. On a held result they store `{request, estimate}` under a local handle (`h1`, `h2`, …; 10-minute expiry, at most 32 entries, oldest evicted) and render the Q5 text.
- `compass_message_confirm` re-sends the stored request with the same `clientRequestId`, `confirmedEstimateTokens` = the stored estimate, and `turnSequence` = the current turn. A re-hold updates the stored estimate under the same handle.
- Update `docs/concepts/tools.md` (write-tool list) and `docs/concepts/comms-model.md` (one paragraph on the gate).
- Tests (`comms.test.ts`):
  - A held result renders the handle and posts nothing.
  - Confirm re-sends the original `clientRequestId` with the confirmed estimate and the current turn sequence.
  - An unknown handle returns the re-post instruction.
  - `compass_message_cost` sets `estimateOnly`.

Interfaces:

```ts
// compass_message_cost
parameters: Type.Object({ channel: Type.String(), topic: Type.Optional(Type.String()), text: Type.String() })
// compass_message_confirm
parameters: Type.Object({ handle: Type.String() })
```

## Tasks

- [ ] T1: Wire fields in `comms.proto`, regenerated.
- [ ] T2: Mention body moved to package comms; delivery unchanged in behavior.
- [ ] T3: `RecipientCostStates`, `MessageByRequestID`, `TopicInChannel` store reads.
- [ ] T4: Estimate, pre-checks and gate in `PostAsAccountByName`; threshold setting; counter.
- [ ] T5: `compass_message_cost`, `compass_message_confirm`, held rendering, docs.

## Open Questions

- **OQ1 (load-bearing): default threshold.** It must sit above one cold full-context recipient (`200000 × 1.25 = 250000`, plus `m`), or every post to a cold full-context peer is held. Options:
  - (a) On at `600000`, about two cold full-context recipients, so single-peer posts never trip.
  - (b) Off by default; operators opt in.
  - (c) On at `250000`, with single-recipient posts exempt.

  **Recommendation:** (a). The design uses (a).
- **OQ2 (load-bearing): post tools can hold a post.** This changes the result contract of `comms_post_message`, `comms_post_ask` and `comms_dm` for agents that set `accepts_hold`. Options:
  - (a) Server hold plus a cost-bound confirm through a local handle (this design).
  - (b) Advisory only: `compass_message_cost` exists, and posts are never held. This depends on the Manager remembering to check.
  - (c) Hold, and confirm by re-posting the full message with a flag. This costs the message tokens twice in the author's context.

  **Recommendation:** (a).
- **OQ3 (not load-bearing): warm window.** `5 min` matches the default cache entry life; the SDK's idle keep-alive refreshes can keep a cache warm longer. It is a constant, tuned from use.
- **OQ4 (not load-bearing): constants.** `0.1`, `1.25`, bytes/4 for `m`, `maxContextTokens` and the 1.1 slack are approximations. A wrong value moves the estimate, not correctness. Tuned from the Q3 counter.
- **OQ5 (not load-bearing): batching overcount.** With inbound batching, several delivers to one idle recipient collapse into one turn, but each post is priced as its own turn. The estimate runs high for bursts; accepted.
