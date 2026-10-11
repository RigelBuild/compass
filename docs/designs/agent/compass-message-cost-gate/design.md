# Compass message cost gate

Tracker: RIG-1713. Freezes on merge.

Builds on: [compass-manager-comms-substrate](../compass-manager-comms-substrate/design.md), [compass-agent-comms-tools](../compass-agent-comms-tools/design.md), [compass-agent-inbound-batching](../compass-agent-inbound-batching/design.md).

Ledger-impact: adds `docs/designs/decisions/agent/DL-459.md` to `DL-465.md`, one row per `Q` heading Q1 to Q7 in order, plus `DL-470.md` and `DL-471.md` for Q8.

## Problem / Intent

One agent post can wake many agents, and each woken agent runs a turn. Today a Manager cannot see that cost before it posts.

This record adds a server-side cost estimate for an agent post, a gate that holds an agent post over an operator threshold until the agent confirms it, and two hard limits a confirm cannot pass. Human posts are never gated.

## Approach

### What exists today

- **Post path.** `comms_post_message`, `comms_post_ask` and `comms_dm`'s second leg (`createCommsTools`, `packages/compass-agent/src/comms.ts`) send the `post` arm. `executeCall` (`relay_comms.go`) calls `linkTrigger`, then `PostAsAccountByName` (`go/internal/comms/agent_caller.go`): "This is the ONLY caller that treats the container arm as a name". Internal posts (`CommitAgentPost`) and the human `PostMessage` RPC bypass it. The tools throw "protocol violation" on a result with no `message`.
- **Store checks.** `AppendMessage` checks, in one transaction: membership (`requireChannelMember`), `OWNER_ONLY`, the topic (`resolveTopicForAppend`), then the `(author, client_request_id)` dedup. No message size limit exists in comms or the store.
- **Recipient set.** `fanOut` (`go/internal/delivery/dispatch.go`) steers live mentioned agents (`routeMentionsFor`), delivers to the other `SubscribedAgents`, and wakes offline ones; for a despawned agent `wakeOnce` (`go/server/lifecycle.go`) is "a logged no-op". Agent posts fire at the author's settle in `turn_sequence` order (DL-382), and `fireHeld` re-resolves recipients then. Delivery already imports comms. A batch queue "lives in `CompassAgent` memory, with no new server or Runner state" (inbound-batching Q4), so the server cannot see it.
- **Context and last-active.** The agent tees each SDK session line into `agent_session_transcript_entries.entry_json` (`session-tee.ts`); a full-body rewrite becomes one `checkpoint` row. Every entry has a `timestamp` (`SessionEntryBase`); an assistant message has `usage.input`, `cacheRead` and `cacheWrite` (`AssistantMessage`), whose sum is the context the model last read.
- **Prompt cache.** The SDK's Anthropic provider writes five-minute cache entries by default (`resolveCacheRetention(cacheRetention, "short")`).
- **Root-only channels.** Not enforced yet: "[TODO RIG-1722: the restricted-post ACL is not yet an enforced primitive" (`config/skills/supervisor-channel/SKILL.md`). The gate does not depend on it.
- **Wire numbering.** `CommsCallRequest` arms 2 to 11 are used; 12 (`list_topics`, open PR #2082) and 13 and 14 (`update_board`, `boards`, draft PR #2078) are taken. A new arm would start at 15; this design adds none. `PostMessageRequest` next free field is 8; `PostMessageResponse` next free is 2.

### The change

Every agent post passes the pre-checks (Q6) and hard limits (Q8). The server then computes `fanout_tokens` and `reload_tokens` (Q1, Q2), holds the post when `fanout_tokens` exceeds the threshold (Q3), and posts on a cost-bound confirm (Q4).

```mermaid
sequenceDiagram
  participant A as Agent tool
  participant C as PostAsAccountByName
  participant S as Store.AppendMessage
  A->>C: post
  C->>C: pre-checks, hard limits, estimate
  alt fanout_tokens <= threshold
    C->>S: append
    C-->>A: message
  else fanout_tokens > threshold
    C-->>A: cost_estimate, held (nothing posted)
    A->>C: compass_message_confirm → same post + confirmed_estimate_tokens
    C->>C: recompute; new <= confirmed × 1.1
    C->>S: append
    C-->>A: message
  end
```

### Q1: Cost model

Ledger row: DL-459.

`fanout_tokens` is a token count summed over all recipients, not a share of one context window. It prices what the post adds, not each recipient's context re-load:

- `m` = `ceil(text bytes / 4) + 50`; the `50` is the deliver envelope.
- `cost_r = m` when `r` is WORKING: the post joins a turn that runs anyway.
- Otherwise `cost_r = m + turnOverhead`. `turnOverhead = 2000` approximates starting one turn: about 20,000 tokens of system prompt and tools at the 0.1 cache-read price. It is a tunable estimate.
- A recipient with no placement is excluded: its wake is a no-op.
- `fanout_tokens = Σ_r cost_r`.

Worked examples against the default threshold of 100,000 (Q3):

- A 1,000-token message to 10 idle recipients: about 10 × 3,000 = 30,000. It posts.
- A 4,000-token message to 50 idle recipients: about 50 × 6,000 = 300,000. It is held.

Other messages in a recipient's batch are not counted (Matt, 2026-10-11). The server cannot see a pending batch, so an idle recipient inside its batch window pays `turnOverhead` per post, at most 2,000 extra each.

The context re-load is shown but never gated, because most agents are stopped and cold, so gating it would hold nearly every post (Matt, 2026-10-11). `reload_tokens = Σ ctx_r × w_r` over non-WORKING recipients, where `ctx_r` = `min(200000, input + cacheRead + cacheWrite)` from the newest assistant `usage` in `r`'s newest session (T3), or 0, and `w_r` = `0.1` when `r`'s newest entry is within the 5 min warm window, else `1.25`. A compaction lowers the next `usage`, so `reload_tokens` falls. Both numbers are approximate.

### Q2: Where the estimate is computed

Ledger row: DL-460.

Server-side, in the comms service: only the server holds the recipient set, live presence and transcript state, and an agent-side estimate would race the post.

Delivery already imports comms, so T2 moves the mention parse and resolve into package comms, keeping one mention body ("a second mention-routing body is a second correctness surface and is prohibited", `routeMentionsFor`). The estimate is taken at post time and `fanOut` re-resolves at settle, so small drift is expected.

### Q3: Threshold and its default

Ledger row: DL-461.

One server-wide setting, `--message-cost-threshold` / `COMPASS_MESSAGE_COST_THRESHOLD`, compared to `fanout_tokens`. It is parsed at startup like `resolveUsageEventRetention` (`go/cmd/compass-server/main.go`), not `positiveCap`, which treats `0` as unset. Empty means the default, `0` turns the gate off, and a non-integer fails startup.

The default is `100000` (Matt, 2026-10-11): about 50 idle recipients of a short post (50 × 2,000), and the longest allowed post (8,000 tokens, Q8) reaches 9 idle agents before a hold. It is lower than the 250,000 discussed in review because a re-load is no longer priced. A deployment chooses the value (see [self-host and managed](../../../concepts/self-host-and-managed.md) and the [OSS core boundary](../../meta/oss-core-managed-boundary/design.md)).

For tuning, comms counts outcomes on `compass.message_cost_gate` (`compass_message_cost_gate_total`, attribute `outcome` = `posted`, `held`, `confirmed`, `estimate_only`, `rejected_length` or `rejected_ceiling`), built like the `fabricPublishFailures` counter in `go/internal/comms/comms.go`.

### Q4: Per-message confirm, no session opt-out

Ledger row: DL-462.

Every held post needs its own confirm. A session opt-out would be forgotten at the next compaction.

The confirm binds to cost, not identity. The tool re-sends the held request with `confirmed_estimate_tokens`. The server recomputes `fanout_tokens`, posts if `new ≤ confirmed × 1.1`, and otherwise holds again with the fresh estimate. No key or server state is needed, and a shrinking audience never re-holds.

The re-sent request keeps the original `client_request_id`, so a repeated confirm lands once, and restamps `turn_sequence` to the current turn (DL-382).

### Q5: Wire and tool shape

Ledger row: DL-463.

The gate rides the existing `post` arm, so no arm is added and `relay_arm_coverage_test.go` needs no new row.

- `PostMessageRequest`: `estimate_only = 8`, `confirmed_estimate_tokens = 9`.
- `PostMessageResponse.cost_estimate = 2` (new message `MessageCostEstimate`, with `fanout_tokens`, `reload_tokens` and `held`).
- `message` set means posted; otherwise `cost_estimate.held` tells a hold from `estimate_only`.
- Every agent post faces the gate (Matt, 2026-10-11). Agent images older than this change throw "protocol violation" on a hold, so the release notes say to ship the agent image with the server.
- `executeCall` skips `linkTrigger` for `estimate_only`.

A typed result is needed: `commsCallError` carries only a code and a message.

The issue's two tools stay (Matt, 2026-10-11):

- `compass_message_cost({channel, topic?, text})` sends `estimate_only` and renders both numbers.
- `compass_message_confirm({handle})` re-sends a held request (Q4). Held requests stay in tool memory under a local handle for 10 minutes; an unknown handle tells the agent to post again.

The post tools render a held result as "not posted: fan-out N tokens over threshold T across R recipients (C cold, re-load about L tokens); call compass_message_confirm with handle H to post".

### Q6: Gate order against the post ACL

Ledger row: DL-464.

The gate never holds a post the store will deny. In `PostAsAccountByName`, after `ChannelByNameForViewer` resolves the channel, and before any estimate:

1. If a message exists for `(author, client_request_id)`, return it, so a landed retry is never reported "not posted".
2. If the author is not a channel member (`Store.IsChannelMember`, the same probe `requireChannelMember` uses), skip the gate.
3. If `create_topic` is false and the topic does not exist in the channel, skip the gate.
4. If the channel is `OWNER_ONLY` and the author is not its owner, skip the gate.

A skipped gate falls through to `PostAsAccount`, and the store returns its own error. For `estimate_only`, steps 2 to 4 return that error directly.

### Q7: Last-active from the transcript

Ledger row: DL-465.

Last-active is the `timestamp` of the newest entry in `r`'s newest session, read by the T3 query that reads `ctx_r`. It feeds `reload_tokens` and the cold count. It is durable, so a restart or second replica sees the same value. Live presence comes from comms' existing `PresenceSource`.

### Q8: Hard limits

Ledger rows: DL-470 (length cap), DL-471 (fan-out ceiling).

A confirm is easy to give, so two limits reject with `ResourceExhausted` and cannot be confirmed past (Matt, 2026-10-11):

- **Length cap (DL-470).** An agent post whose text exceeds `COMPASS_MESSAGE_MAX_TOKENS` (text bytes / 4) is rejected. Default `8000`: a longer coordination post belongs in a file or issue link, and the cap keeps one post under 4% of a 200,000-token context. Human posts are not covered, matching the gate.
- **Fan-out ceiling (DL-471).** A post whose `fanout_tokens` exceeds `COMPASS_MESSAGE_FANOUT_HARD_LIMIT` is rejected. Default 10 × the threshold (1,000,000). It is a separate row, so dropping it changes nothing else.

Both parse like the threshold, and `0` turns a limit off. The order is: Q6 pre-checks, length cap, fan-out ceiling, then the threshold hold. `estimate_only` returns the same rejections.

## Alternatives considered

- **Gate on context re-load (`m + ctx_r × w_r`).** Most agents are stopped and cold, so a cold recipient scored up to 250,000 and nearly every post to idle agents would be held; agents would stop sending (Matt, 2026-10-11). Kept as the informational `reload_tokens`.
- **Checkpoint-onward transcript bytes as `ctx_r`.** A checkpoint does not follow every compaction, and the stored body keeps entries the loader drops. Rejected for observed `usage`.
- **In-memory last-active map.** Lost on restart, split across replicas. Rejected.
- **HMAC token over a message and recipient digest.** Needs a key and re-holds a shrinking audience. Rejected.
- **An `accepts_hold` capability flag.** Every agent post faces the gate (Matt, 2026-10-11). Rejected.
- **Client-side estimate, new call arms, or a gate in `Store.AppendMessage`.** The agent lacks the recipient state; arms duplicate the post arm; the store would gate human posts. Rejected.

## Plan

### Global Constraints

- Proto fields use the explicit tags in Q5. Regenerate with `moon run compass-proto:gen` and commit the output. `MessageCostEstimate` is public (`comms.proto`), so it needs no `gen-fence` entry in `proto/moon.yml`.
- No new table and no edit to `go/internal/store/migrations/0001_init.sql`. New SQL is one sqlc query.
- No new `CommsCallRequest` or `CommsCallResult` arm. If a later change needs one, it starts at 15.
- Human `CommsService.PostMessage`, `PostAsAccount` and `CommitAgentPost` stay ungated and uncapped.
- Setting `COMPASS_MESSAGE_COST_THRESHOLD`, `COMPASS_MESSAGE_FANOUT_HARD_LIMIT` and `COMPASS_MESSAGE_MAX_TOKENS` to `0` restores today's behavior exactly.
- No tracker ids in source comments. No names of private repositories.
- Existing test assertions stay unmodified. Tests that move with code in T2 keep their assertions.
- Go: errors through `edgeError` and the existing connect codes. TS: render failures through `commsFailure`.

### T1: Wire fields (lane proto)

- Add the fields and message below to `proto/compass/v1/comms.proto`.
- Document on `PostMessageRequest` that the human RPC ignores fields 8 and 9.
- Tests: none beyond generation; the behavior lands in T4.

Interfaces:

```proto
message MessageCostEstimate {
  // Sum over recipients of what the post adds; the gated number (design: compass-message-cost-gate Q1).
  uint64 fanout_tokens = 1;
  // The server threshold in force; 0 means the gate is off.
  uint64 threshold_tokens = 2;
  uint32 recipient_count = 3;
  uint32 cold_recipient_count = 4;
  // True when the post was held for confirmation; false for estimate_only.
  bool held = 5;
  // Approximate context re-load of non-WORKING recipients; never compared to the threshold.
  uint64 reload_tokens = 6;
}

message PostMessageRequest {
  // ... fields 1-7 unchanged ...
  bool estimate_only = 8;
  uint64 confirmed_estimate_tokens = 9;
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

### T4: Estimate, limits and gate (lane server)

- New file `go/internal/comms/cost_gate.go`: the estimator (Q1), hard limits (Q8) and gate.
- `PostAsAccountByName` order:
  1. Resolve the channel.
  2. If all three settings are `0` and `estimate_only` is false, post as today.
  3. Run the Q6 pre-checks, then the length cap.
  4. Recipients = `SubscribedAgents(channel, account)` ∪ `ResolveMentions(...)`, minus the author. Read `RecipientCostStates` and `PresenceFor`; compute `fanout_tokens` and `reload_tokens`.
  5. Apply the fan-out ceiling.
  6. `estimate_only` → return the estimate. `fanout_tokens` ≤ threshold → post. `confirmed_estimate_tokens > 0` and `fanout_tokens ≤ confirmed × 1.1` → post. Otherwise → hold; nothing is appended.
- In `executeCall`, skip `linkTrigger` when `estimate_only` is set.
- Add `resolveTokenLimit` in `go/cmd/compass-server/main.go` for the three settings (`firstNonEmpty(flag, env)`), into `ServeConfig`; wire `SetCostGate` in `go/server/serve.go`.
- Add the Q3 counter beside `fabricPublishFailures`.
- Tests:
  - Under the threshold posts; over it holds and appends nothing; `estimate_only` never posts; all settings `0` reads no recipients.
  - A confirm within 10% posts; a grown audience holds again; a smaller one posts.
  - Over the length cap or the ceiling returns `ResourceExhausted`, with or without a confirm; a human post over the length cap posts.
  - A large `reload_tokens` with a small `fanout_tokens` posts.
  - A stored `client_request_id` returns the stored message. A non-member, a missing topic with `create_topic` false, and a non-owner `OWNER_ONLY` post each return the store error, not a hold.
  - A WORKING recipient costs `m`; an idle one costs `m + turnOverhead`; no placement costs nothing; `ctx_r` clamps at `maxContextTokens`.
  - `resolveTokenLimit` maps empty to the default and `0` to off, and rejects junk.

Interfaces:

```go
type CostGateConfig struct {
    ThresholdTokens    uint64        // 0 = off; default 100000
    HardLimitTokens    uint64        // 0 = off; default 10 × ThresholdTokens
    MaxMessageTokens   uint64        // 0 = off; default 8000
    TurnOverheadTokens uint64        // 2000
    WarmWindow         time.Duration // 5 * time.Minute
    MaxContextTokens   int64         // 200000
    ConfirmSlack       float64       // 1.1
    Now                func() time.Time
}

func (c *Comms) SetCostGate(cfg CostGateConfig)

func (c *Comms) estimateCost(ctx context.Context, author store.AccountID, channel store.ChannelID, blocks []*compassv1.MessageBlock) (*compassv1.MessageCostEstimate, error)

// go/cmd/compass-server/main.go
func resolveTokenLimit(v string, def uint64) (uint64, error)
```

### T5: Agent tools and docs (lane agent)

- In `packages/compass-agent/src/comms.ts`, add `compass_message_cost` and `compass_message_confirm` to `createCommsTools` (eleven tools become thirteen).
- On a held result, `comms_post_message`, `comms_post_ask` and `comms_dm` store `{request, estimate}` under a local handle (`h1`, `h2`, …; 10-minute expiry, at most 32 entries, oldest evicted) and render the Q5 text. A `ResourceExhausted` renders through `commsFailure`.
- `compass_message_confirm` re-sends the stored request with the same `clientRequestId`, `confirmedEstimateTokens` = the stored `fanoutTokens`, and `turnSequence` = the current turn. A re-hold updates the stored estimate under the same handle.
- Update `docs/concepts/tools.md` (write-tool list) and `docs/concepts/comms-model.md` (one paragraph on the gate and limits).
- Tests (`comms.test.ts`):
  - A held result renders the handle, fan-out and re-load, and posts nothing.
  - Confirm re-sends the original `clientRequestId` with the confirmed fan-out and the current turn sequence.
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
- [ ] T4: Estimate, hard limits and gate in `PostAsAccountByName`; three settings; counter.
- [ ] T5: `compass_message_cost`, `compass_message_confirm`, held rendering, docs.

## Resolved decisions

Settled by Matt on 2026-10-11:

- Default threshold `100000` on `fanout_tokens` (Q3).
- Server hold with a cost-bound confirm for every agent post, plus hard limits (Q5, Q8).
- Warm window 5 min; constants `0.1`, `1.25`, bytes/4, the 200,000 clamp and 1.1 slack, tuned from the Q3 counter. `turnOverhead = 2000` is new here and tunes the same way.
- A recipient's other batched messages are not counted (Q1).
