# Compass message cost gate

Tracker: RIG-1713. Freezes on merge.

Builds on: [compass-manager-comms-substrate](../compass-manager-comms-substrate/design.md), [compass-agent-comms-tools](../compass-agent-comms-tools/design.md), [compass-agent-inbound-batching](../compass-agent-inbound-batching/design.md).

Ledger-impact: adds `docs/designs/decisions/agent/DL-459.md` to `DL-465.md`, one row per `Q` heading Q1 to Q7 in order, plus `DL-470.md` and `DL-471.md` for Q8.

## Problem / Intent

One agent post can wake many agents, and each woken agent runs a turn. Today a Manager cannot see that cost before it posts.

This record adds a server-side cost estimate for an agent post, a gate that holds a post over an operator threshold until the agent confirms, and hard limits a confirm cannot pass. Human posts are never gated.

## Approach

### What exists today

- **Post path.** `comms_post_message`, `comms_post_ask` and `comms_dm`'s second leg (`createCommsTools`, `packages/compass-agent/src/comms.ts`) send the `post` arm. `executeCall` (`relay_comms.go`) calls `linkTrigger`, then `PostAsAccountByName` (`go/internal/comms/agent_caller.go`), the only agent post entry. Internal posts (`CommitAgentPost`) and the human `PostMessage` RPC bypass it. The tools throw "protocol violation" on a result with no `message`.
- **Store checks.** `AppendMessage` checks, in one transaction: membership (`requireChannelMember`), `OWNER_ONLY`, the topic (`resolveTopicForAppend`), then the `(author, client_request_id)` dedup. No message size limit exists.
- **Recipient set.** `fanOut` (`go/internal/delivery/dispatch.go`) steers live mentioned agents (`routeMentionsFor`), delivers to the other `SubscribedAgents`, and wakes offline ones; for a despawned agent the wake (`go/server/lifecycle.go`) is "a logged no-op". Agent posts fire at the author's settle (DL-382), when `fireHeld` re-resolves recipients. A batch queue "lives in `CompassAgent` memory, with no new server or Runner state" (inbound-batching Q4).
- **Context and last-active.** `session-tee.ts` tees each SDK session line into `agent_session_transcript_entries.entry_json`; a full-body rewrite becomes one `checkpoint` row. Every entry has a `timestamp`; an assistant message has `usage.input`, `cacheRead` and `cacheWrite`, whose sum is the context the model last read.
- **Prompt cache.** Anthropic cache entries last five minutes by default (`resolveCacheRetention(cacheRetention, "short")`).
- **Root-only channels.** The restricted-post ACL is not enforced yet (`config/skills/supervisor-channel/SKILL.md`); the gate does not depend on it.
- **Wire numbering.** `CommsCallRequest` arms 2 to 12 are used (12 is `list_topics`, #2082); draft PR #2078 reserves 13 and 14. This design adds no arm. Next free fields: `PostMessageRequest` 8, `PostMessageResponse` 2.

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

- `m` = `ceil(text bytes / 4) + 50`. Text bytes cover text blocks and ask question and option text (`textContent` in `go/internal/store/blocks.go`, plus option labels); `50` is the deliver envelope.
- `cost_r = m` when `r` is mentioned and WORKING: the steer joins the running turn. A WORKING subscriber's deliver runs as a new turn at `agent_end`.
- Every other placed recipient costs `m + turnOverhead`. `turnOverhead = 2000` is a deliberately low estimate of a turn start (about 20,000 prompt and tool tokens cost about 2,000 warm, 25,000 cold), so cold agents are not over-held. Its default is part of OQ1.
- A recipient with no placement is excluded: its wake is a no-op.
- `fanout_tokens = Σ_r cost_r`.

Worked examples against the default threshold of 100,000 (Q3):

- A 1,000-token message to 10 idle recipients: about 10 × 3,000 = 30,000. It posts.
- A 4,000-token message to 50 idle recipients: about 50 × 6,000 = 300,000. It is held.

Other messages in a recipient's batch are not counted (Matt, 2026-10-11). The server cannot see a batch, so an idle recipient inside its batch window still pays `turnOverhead`.

The context re-load is shown, never gated: most agents are stopped and cold, so gating it would hold nearly every post. `reload_tokens = Σ ctx_r × w_r` over non-WORKING recipients: `ctx_r` = `min(200000, input + cacheRead + cacheWrite)` from the newest assistant `usage` in `r`'s newest session (T3), else 0; `w_r` = `0.1` when the newest entry is within the 5 min warm window, else `1.25`. A compaction lowers it. Both numbers are approximate.

### Q2: Where the estimate is computed

Ledger row: DL-460.

Server-side, in the comms service: only the server holds the recipient set, live presence and transcript state, and an agent-side estimate would race the post.

Delivery already imports comms, so T2 moves the mention parse and resolve into package comms, keeping one mention body ("a second mention-routing body is a second correctness surface and is prohibited", `routeMentionsFor`). The estimate is taken at post time and `fanOut` re-resolves at settle, so small drift is expected.

### Q3: Threshold and its default

Ledger row: DL-461.

One server-wide setting, `--message-cost-threshold` / `COMPASS_MESSAGE_COST_THRESHOLD`, compared to `fanout_tokens`. It is parsed at startup like `resolveUsageEventRetention` (`go/cmd/compass-server/main.go`), not `positiveCap`, which treats `0` as unset. Empty means the default, `0` turns the gate off, and a non-integer fails startup.

The default is `100000`, proposed (OQ1): about 48 idle recipients of a short post (each ≈ `m` + 2,000), or 9 idle agents for the longest allowed post (8,000 tokens, Q8). A deployment chooses the value (see [self-host and managed](../../../concepts/self-host-and-managed.md) and the [OSS core boundary](../../meta/oss-core-managed-boundary/design.md)).

For tuning, comms counts outcomes on `compass.message_cost_gate` (`compass_message_cost_gate_total`, attribute `outcome` = `posted`, `held`, `confirmed`, `estimate_only`, `rejected_length` or `rejected_ceiling`), built like the `fabricPublishFailures` counter in `go/internal/comms/comms.go`.

### Q4: Per-message confirm, no session opt-out

Ledger row: DL-462.

Every held post needs its own confirm. A session opt-out would be forgotten at the next compaction.

The confirm binds to cost: the tool re-sends the held request with `confirmed_estimate_tokens`, and the server posts if the recomputed `fanout_tokens` ≤ confirmed × 1.1, else holds again. No key or server state is needed, and a shrinking audience never re-holds.

The re-sent request keeps the original `client_request_id`, so a repeated confirm lands once, and restamps `turn_sequence` to the current turn (DL-382).

### Q5: Wire and tool shape

Ledger row: DL-463.

The gate rides the existing `post` arm; no arm is added.

- `PostMessageRequest`: `estimate_only = 8`, `confirmed_estimate_tokens = 9`.
- `PostMessageResponse.cost_estimate = 2`: new message `MessageCostEstimate` with `fanout_tokens`, `threshold_tokens`, `recipient_count`, `cold_recipient_count`, `held` and `reload_tokens` (T1).
- `message` set means posted; otherwise `cost_estimate.held` tells a hold from `estimate_only`.
- Every agent post faces the gate (Matt, 2026-10-11). Older agent images throw "protocol violation" on a hold, so the release notes say to ship the agent image with the server.
- `executeCall` skips `linkTrigger` for `estimate_only`.

A typed result is needed: `commsCallError` carries only a code and a message.

The issue's two tools stay:

- `compass_message_cost({channel, text})` sends `estimate_only` with no topic (the topic does not change the cost) and renders both numbers.
- `compass_message_confirm({handle})` re-sends a held request (Q4). Handles last 10 minutes in tool memory; an unknown handle tells the agent to post again.

The post tools render a held result as "not posted: fan-out N tokens over threshold T across R recipients (C cold, re-load about L tokens); call compass_message_confirm with handle H to post".

### Q6: Gate order against the post ACL

Ledger row: DL-464.

The gate never holds a post the store will deny. In `PostAsAccountByName`, after the channel resolves and before any estimate:

1. If `client_request_id` is non-empty and a message exists for `(author, client_request_id)`, return it, so a landed retry is never reported "not posted".
2. If the author is not a channel member (`Store.IsChannelMember`), skip the gate.
3. If `create_topic` is false and the topic does not exist in the channel, skip the gate.
4. If the channel is `OWNER_ONLY` and the author is not its owner, skip the gate.

A skipped gate falls through to `PostAsAccount`, and the store returns its own error. For `estimate_only`, step 3 is skipped (Q5), and steps 2 and 4 return the store error directly.

### Q7: Last-active from the transcript

Ledger row: DL-465.

Last-active is the `timestamp` of the newest entry in `r`'s newest session, read by the T3 query that reads `ctx_r`. It feeds `reload_tokens` and the cold count. It is durable, so a restart or second replica sees the same value. Live presence comes from comms' existing `PresenceSource`.

### Q8: Hard limits

Ledger rows: DL-470 (length cap), DL-471 (fan-out ceiling).

Matt asked for a backstop a confirm cannot pass, leaning to length only. This record proposes two, each rejecting with `ResourceExhausted`; defaults are OQ1:

- **Length cap (DL-470).** An agent post whose `ceil(text bytes / 4)` exceeds `COMPASS_MESSAGE_MAX_TOKENS` is rejected. Default `8000`: a longer post belongs in a file or issue link, and one post's text stays at most 4% of a 200,000-token context. Human posts are not capped.
- **Fan-out ceiling (DL-471).** A post whose `fanout_tokens` exceeds `COMPASS_MESSAGE_FANOUT_HARD_LIMIT` is rejected. Default 10 × the threshold. It is a separate row, so dropping it (OQ1 b) changes nothing else.

Both parse like the threshold; `0` turns a limit off. Order: Q6 pre-checks, length cap, fan-out ceiling, threshold hold. `estimate_only` returns the same rejections.

## Alternatives considered

- **Gate on context re-load (`m + ctx_r × w_r`).** A cold recipient scored up to 250,000, so nearly every post to idle agents would be held, and Matt warned agents would stop sending. Kept as `reload_tokens`.
- **Checkpoint-onward transcript bytes as `ctx_r`.** Checkpoints do not follow every compaction, and the stored body keeps entries the loader drops.
- **In-memory last-active map.** Lost on restart, split across replicas.
- **HMAC token over a message and recipient digest.** Needs a key and re-holds a shrinking audience.
- **An `accepts_hold` capability flag.** Matt ruled every agent post faces the gate.
- **Client-side estimate, new call arms, or a gate in `Store.AppendMessage`.** The agent lacks the recipient state; arms duplicate the post arm; the store would gate human posts.

## Plan

### Global Constraints

- Proto fields use the Q5 tags. Regenerate with `moon run compass-proto:gen` and commit the output. `MessageCostEstimate` is public, so no `gen-fence` entry.
- No new table; no edit to `0001_init.sql`. New SQL is one sqlc query.
- No new `CommsCallRequest` or `CommsCallResult` arm; a later one starts at 15.
- Human `CommsService.PostMessage`, `PostAsAccount` and `CommitAgentPost` stay ungated and uncapped.
- Setting `COMPASS_MESSAGE_COST_THRESHOLD`, `COMPASS_MESSAGE_FANOUT_HARD_LIMIT` and `COMPASS_MESSAGE_MAX_TOKENS` to `0` restores today's behavior exactly.
- No tracker ids in source comments. No names of private repositories.
- Existing test assertions stay unmodified, except the `createCommsTools` exact-list assertion (`comms.test.ts`) and the `comms.ts` header comment, which gain the two tools. Tests moved in T2 keep their assertions.
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
- Delivery's `routeMentionsFor` calls the comms functions; on error it logs its two existing lines with the same attributes and drops all mentions, as today.
- Point the `go/internal/store/handle.go` comments naming delivery's `mentionRE` and `reservedMentions` at comms.
- Tests: existing delivery mention tests pass unchanged.

Interfaces:

```go
package comms

// MentionReads is the store surface mention resolution needs; *store.Store satisfies it.
type MentionReads interface {
    ChannelAgentMembers(ctx context.Context, channel store.ChannelID, author store.AccountID) ([]store.ChannelAgentMember, error)
    ResolveOwner(ctx context.Context, caller store.AccountID) (store.AccountID, error)
}

// ResolveMentions wraps its two read failures in these, so delivery keeps its two log lines.
var (
    ErrMentionMembers = errors.New("comms: resolve channel agent members")
    ErrMentionOwner   = errors.New("comms: resolve author owner")
)

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
- Cast `entry_json` to `jsonb` only for non-checkpoint rows; a value found only in a checkpoint body is parsed in Go from the body's end.
- Add `Store.MessageByRequestID` (exports `getMessageByRequestID`) and `Store.TopicInChannel` (over `GetTopicChannel` and `GetTopicByName`) for Q6. An empty key returns `(Message{}, false, nil)` without a query, since `GetMessageByRequestID` would match unkeyed messages.
- Tests (pgtest): usage sums the newest assistant delta's three fields; a checkpoint body supplies usage and timestamp when no later delta does; only the newest session counts; no session means absent; no placement gives `Placed = false`.

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
  2. All three settings `0` and not `estimate_only` → post as today.
  3. Run the Q6 pre-checks, then the length cap unless it is `0`.
  4. Recipients = `SubscribedAgents(channel, account)` ∪ `ResolveMentions(...)`, minus the author. Read `RecipientCostStates` and `PresenceFor`; compute `fanout_tokens` and `reload_tokens`.
  5. Apply the fan-out ceiling unless it is `0`.
  6. `estimate_only` → return the estimate. `threshold == 0` or `fanout_tokens ≤ threshold` → post. `confirmed_estimate_tokens > 0` and `fanout_tokens ≤ confirmed × 1.1` → post. Otherwise → hold; nothing is appended.
- In `executeCall`, skip `linkTrigger` when `estimate_only` is set.
- Add `resolveTokenLimit` in `go/cmd/compass-server/main.go` for the three settings (`firstNonEmpty(flag, env)`), into `ServeConfig`; wire `SetCostGate` in `go/server/serve.go`.
- Add the Q3 counter beside `fabricPublishFailures`.
- Tests:
  - Under the threshold posts; over it holds and appends nothing; `estimate_only` never posts and ignores the topic; all settings `0` reads no recipients; threshold `0` with limits on never holds.
  - A confirm within 10% posts; a grown audience holds again; a smaller one posts.
  - Over the length cap or ceiling returns `ResourceExhausted`, even with a confirm; a human post over the cap posts.
  - A large `reload_tokens` with a small `fanout_tokens` posts.
  - A stored `client_request_id` returns the stored message; an empty key with earlier unkeyed messages posts. A non-member, a missing topic without `create_topic`, and a non-owner `OWNER_ONLY` post return the store error, not a hold.
  - A mentioned WORKING recipient costs `m`; a WORKING subscriber or idle one costs `m + turnOverhead`; no placement costs nothing; ask text counts toward `m`; `ctx_r` clamps at `maxContextTokens`.
  - `resolveTokenLimit` maps empty to the default and `0` to off, and rejects junk.

Interfaces:

```go
type CostGateConfig struct {
    ThresholdTokens    uint64        // 0 = off; default 100000 (OQ1)
    HardLimitTokens    uint64        // 0 = off; default 10 × ThresholdTokens (OQ1)
    MaxMessageTokens   uint64        // 0 = off; default 8000 (OQ1)
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

- In `packages/compass-agent/src/comms.ts`, add `compass_message_cost` and `compass_message_confirm` to `createCommsTools` (twelve tools become fourteen).
- On a held result, `comms_post_message`, `comms_post_ask` and `comms_dm` store `{request, estimate}` under a local handle (`h1`, `h2`, …; 10-minute expiry, at most 32 entries, oldest evicted) and render the Q5 text. A `ResourceExhausted` renders through `commsFailure`.
- `compass_message_confirm` re-sends the stored request with the same `clientRequestId`, `confirmedEstimateTokens` = the stored `fanoutTokens`, and `turnSequence` = the current turn. A re-hold updates the stored estimate under the same handle.
- Update `docs/concepts/tools.md` (write-tool list) and `docs/concepts/comms-model.md` (one paragraph on the gate and limits).
- Tests (`comms.test.ts`): a held result renders handle, fan-out and re-load and posts nothing; confirm re-sends the original `clientRequestId` with the confirmed fan-out and current turn sequence; an unknown handle returns the re-post instruction; `compass_message_cost` sets `estimateOnly`.

Interfaces:

```ts
export const messageCostParameters = type({
	channel: type("string")
		.narrow((s, ctx) => s.trim().length > 0 || ctx.mustBe("non-blank"))
		.describe("Target channel by name; must not be blank"),
	text: type("string")
		.narrow((s, ctx) => s.trim().length > 0 || ctx.mustBe("non-blank"))
		.describe("Markdown message body to price; must not be blank"),
});

export const messageConfirmParameters = type({
	handle: type("string")
		.narrow((s, ctx) => s.trim().length > 0 || ctx.mustBe("non-blank"))
		.describe("Handle from a held post, such as h1; must not be blank"),
});
```

## Tasks

- [ ] T1: Wire fields in `comms.proto`, regenerated.
- [ ] T2: Mention body moved to package comms; delivery unchanged in behavior.
- [ ] T3: `RecipientCostStates`, `MessageByRequestID`, `TopicInChannel` store reads.
- [ ] T4: Estimate, hard limits and gate in `PostAsAccountByName`; three settings; counter.
- [ ] T5: `compass_message_cost`, `compass_message_confirm`, held rendering, docs.

## Resolved decisions

Settled by Matt on 2026-10-11:

- Option A: every agent post faces the server hold and cost-bound confirm, with no `accepts_hold` (Q5).
- Warm window 5 min; constants `0.1`, `1.25`, bytes/4, the 200,000 clamp and 1.1 slack, tuned from the Q3 counter (Q1).
- A recipient's other batched messages are not counted (Q1).

## Open Questions

### OQ1: Default numbers on the new `fanout_tokens` unit (load-bearing)

Matt has not seen the `fanout_tokens` unit. All options keep `turnOverhead = 2000`:

- (a) Threshold `100000`, length cap `8000`, plus the fan-out ceiling.
- (b) The same without the ceiling: a length-only backstop, as Matt leaned.
- (c) Threshold `250000`, his suggested start, length cap only.

**Recommendation:** (b): it follows his length-only lean, and the ceiling is a separate row (DL-471) to drop. DL-461, DL-470 and DL-471 stay proposed until he rules.

The Plan builds (a). For (b), drop T4 step 5, `CostGateConfig.HardLimitTokens`, `COMPASS_MESSAGE_FANOUT_HARD_LIMIT`, the `rejected_ceiling` outcome and DL-471. For (c), also set the threshold default to `250000`. The ruling is folded in before merge.
