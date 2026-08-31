# RIG-2257 — Compass ask-answer recovery: answer-as-message

Status: Draft

This record is the RIG-2257 successor to the T7 runner/hub dependency filed by
`docs/designs/agent/compass-ask-comms-roundtrip/design.md` Decision 3. It is
a pre-freeze rewrite of this same record: Matt ruled the prior draft's
owed-marker + control-op-wake architecture out (see Alternatives considered),
in favor of **the answer to an ask becoming a normal message on the existing
durable delivery rail**. Pre-GA nothing is frozen — the RIG-1509 agent half
(the `AskAnswerControl` consumer) is reworked here, not preserved. Owner lane:
compass-server.

## Problem / Intent

The ask-answer wake is keyed on the **session id** live at `RespondToAsk`
time, so an answer submitted while the asking agent has no live session — or
after a Runner reconnect / container resume minted a new session id — is
silently dropped and never redelivered: `go/internal/runnerhub/ask_waker.go:34-36`
reads

```go
sessionID, ok := h.SessionForAccount(agent)
if !ok {
    return // no live session to wake; the agent reads the answer on its next turn
}
```

and the "reads the answer on its next turn" fallback in that comment is
**false for asks**: the durable delivery sweep excludes the author — `AND
m.author_account_id <> $1` (`go/internal/store/delivery_cursors.go:484`) — and
the ask (including its answered-in-place block) is **agent-authored**, so the
agent's own sweep skips it. That author-exclusion is the entire reason the ask
answer ever needed a bespoke wake.

Fix it at the root: make the answer a **new message authored by the
answerer** (a participant, ≠ the agent), carrying an answer block correlated
by `ask_id`. An answerer-authored message is NOT author-excluded, so it is
swept, delivered, acked, and reconnect-redelivered by the one delivery rail
that already exists — no new durability machinery, no agent-side boot poll
(the ratified RIG-1509 Decision 3 exclusion stands).

## Approach

### The mechanism in one line

`RespondToAsk` keeps recording the answer on the ask block (the answer-once
guard is untouched) and **additionally posts a new message** — authored by the
answering participant, into the ask's own channel + topic — whose single block
is a new `ask_answer` message-block variant carrying the answered ask
snapshot. That message rides the normal post → fan-out → deliver → ack →
sweep pipeline unchanged.

### Why the rail applies now

The rail was never unusable for answers — it was unusable for **agent-authored
rows**. `UndeliveredMessages` is the sweep read (`delivery_cursors.go:460`),
and its exclusion is precisely the author:

```sql
AND m.author_account_id <> $1
```

(`delivery_cursors.go:484`). Flip the author — the answer message's
`author_account_id` is the answerer, resolved server-side from the RPC actor
(`c.actorFromContext(ctx)`, the same value `AnswerAsk` authorizes on,
`comms.go:397`) — and the exclusion no longer bites. The agent is a member of
the ask's channel by construction (it posted the ask there, and `AppendMessage`
gates posting on membership: "the author must be a member of the target
channel", `store/messages.go:51-57`), so the answer lands in a channel the
agent can be swept for.

### At-least-once is already built

The rail is ack-gated at-least-once end to end, with the ack frame already on
the wire — no new proto ack is needed:

- **Ack-gated cursor.** The per-(agent, channel) delivery cursor advances only
  on the recipient's `delivery_ack` receipt: the hub's `deliverAck` "advances
  the durable delivery cursor for a recipient's delivery_ack … the
  Runner->Server receipt that a relayed deliver reached the session"
  (`go/internal/runnerhub/hub.go:699-710`), through `store.AckDelivery`
  (`delivery_cursors.go:307`).
- **Reconnect resweep.** `UndeliveredMessages` returns "the messages still
  owed to this agent, ascending seq per channel: seq > acked_seq"
  (`delivery_cursors.go:451-455`), and the delivery consumer runs it on every
  session start: `OnSessionStarted` "is the hub's SessionStartSink hook …
  called from promoteSession right after the hub binds account->session … and,
  in the single-Runner MVP, on the re-promotion each session takes after a
  Runner re-enroll clears the bindings" (`go/internal/delivery/settle.go:37-43`).
- **Agent-side dedup.** The agent dedups delivered messages by `msg.id` (the
  control source "dispatched through `immediate.steer` / `immediate.deliver`,
  where the CompassAgent dedups on `msg.id`",
  `packages/compass-agent/src/transport/control-source.ts:22-25`), so an
  at-least-once redelivery of the answer message is absorbed.

So offline answer, crash-before-ack, Runner reconnect, and container resume
all collapse into cases the rail already handles for every other message.

### The `ask_answer` block: shape and ownership

A third variant on the wire block oneof — today `message MessageBlock { oneof
block { string text = 1; Ask ask = 2; } }` (`proto/compass/v1/comms.proto:343-350`)
— and a matching third pointer on the store model, whose doc already states
the invariant to extend: "Exactly one of Text / Ask is non-nil, mirroring the
wire `block` oneof" (`go/internal/store/types.go:308-316`).

```proto
// comms.proto — new field on the existing oneof (additive, buf-safe)
message MessageBlock {
  oneof block {
    string text = 1;
    Ask ask = 2;
    AskAnswerBlock ask_answer = 3;
  }
}

// The answer to a previously-posted Ask, authored by the answerer. Entirely
// server-owned: constructed by RespondToAsk from the just-answered stored ask;
// a client-supplied ask_answer block (PostMessage / the agent update path) is
// rejected — answers enter only through RespondToAsk, which enforces the
// answer-once guard and answer validation.
message AskAnswerBlock {
  // The answered ask snapshot: ask_id for correlation, answered=true, and the
  // per-question recorded answers (chosen_option_ids / custom_text — the Ask
  // message already carries answer state "kept for audit", comms.proto:403).
  Ask ask = 1;
  // The asking agent's account id, denormalized so the delivery consumer can
  // target the asker without an ask_id -> author lookup per message.
  string asker_account_id = 2;
}
```

**Why a full snapshot, not a slim `{ask_id, answers}` pair:** the RIG-1509
agent half currently renders an answer against a session-scoped in-memory
`PendingAsks` registry ("keyed by the server-minted `ask_id` … session-scoped
and not durable", `packages/compass-agent/src/comms.ts:224-234`) — exactly the
state a restart wipes, producing the unknown-ask-id degraded arm
(`agent.ts:713-724`). The snapshot carries the questions AND recorded answers
in the delivered message itself, so the agent renders registry-free and the
registry is **deleted outright** rather than made durable. The snapshot is
cheap: it is the same `Ask` the answered message row already stores.

**Server-owned:** only `RespondToAsk` constructs the block (from `AnswerAsk`'s
returned, validated, answer-once-guarded ask). The wire mappers reject an
inbound `ask_answer` on both client write paths — `blocksFromWire` (POST,
`comms/mapping.go:312`) and `updateBlocksFromWire` (agent update,
`mapping.go:338`) — mirroring how `askFromWire` already strips client-supplied
`ask_id` because it is server-owned (`mapping.go:355-362`).

### One instance of a general pattern: special message blocks

The `ask_answer` variant is deliberately framed as **instance #1 of a "special
message block" pattern**, not a one-off. The pattern:

1. **Wire:** a new message type + a new field on the `MessageBlock` oneof
   (additive proto3 field — buf-breaking-safe, exactly as the repo already
   notes for control-oneof additions: "Defining the empty shells now is
   additive and buf-breaking-safe", `proto/compass/v1/agent.proto:152-153`).
2. **Store:** a new pointer field on `store.MessageBlock` + a new `blockKind`
   discriminant and `storedBlock` arm (`store/blocks.go:20-33`), with its
   validation in `marshalBlocks`' exactly-one-of switch (`blocks.go:71-91`)
   and its decode arm in `unmarshalBlocks` (`blocks.go:178-217`, whose
   "totality discipline: a missing arm must never pass silently" already
   enforces that every new kind is handled everywhere).
3. **Edge:** a `blocksToWire` arm (`mapping.go:187-198`) plus an ownership
   decision per variant — client-postable (like `text`/`ask`) or server-owned
   (like `ask_answer`), enforced in `blocksFromWire`/`updateBlocksFromWire`.
4. **Delivery: free for channel-broadcast blocks.** Any message carrying the
   new block inherits fan-out, the settle gate, live deliver, the ack-gated
   cursor, and the reconnect sweep with zero delivery-layer work, because the
   rail moves whole messages and never inspects block kinds. A block needing
   **recipient-targeted** delivery (an approval grant addressed to one agent, a
   status card for a specific session) adds one kind-aware arm in the shared
   fan-out/recovery body — exactly as `ask_answer` does in T5 to reach an
   out-of-sweep asker; the free inheritance is the untargeted broadcast case.
5. **Agent:** one rendering arm in the deliver lane keyed on the block kind.

Future variants — approval requests/grants, structured status cards, richer
attachments — each follow the same five steps and inherit the same durability.
This is the scaling argument against the rejected design: a bespoke control op
per interaction type re-invents delivery, ordering, ack, and recovery once per
type; a block variant inherits all four.

### Atomicity: answer-record and answer-message commit together

`AnswerAsk` is already a single serialized read-modify-write transaction
(FOR UPDATE on the matched message row, `store/messages.go:545-571`). The
answer message is inserted **in that same transaction**: flip `Answered`,
write the updated blocks, insert the answer message (same channel, same topic
— the ask row's `TopicID`), commit. So "ask answered" and "answer message
exists" are one atomic fact — there is no window where the answer is recorded
but the delivering message was lost to a crash. The insert reuses
`AppendMessage`'s core insert + topic `last_seq` maintenance, extracted into a
tx-scoped helper (T3) that runs **inside** `AnswerAsk`'s existing transaction.

**No idempotency key on the answer insert — the answer-once guard is the sole
single-fire mechanism.** The helper passes `client_request_id = ""`, so
`AppendMessage`'s dedup `ON CONFLICT (author_account_id, client_request_id)`
partial index (`store/messages.go:96-98`) is structurally unreachable for the
answer row. A derived key like `ask-answer:<ask_id>` was rejected: it lives in
the **client-controllable** namespace (`PostMessage` passes
`req.Msg.GetClientRequestId()` through verbatim, `comms.go:367`), so if the
answering account had ever posted any message with that exact
`client_request_id`, the answer insert would be silently suppressed by `DO
NOTHING` while the `Answered` flip commits — the ask reads answered, no
`MessagePosted` fires, and the asker never gets the answer: the exact silent-loss
class this design exists to kill, reintroduced by its own belt. It defends
nothing the guard does not: the answer-once guard already makes the insert
single-fire (a post-commit retry hits `ErrConflict` before reaching the insert;
a rollback undoes both flip and insert atomically).

**The extracted helper does NOT inherit `AppendMessage`'s conflict arm.**
`AppendMessage`'s `ON CONFLICT` path handles `noRows` by re-reading the
committed row **through the pool** and letting its own tx roll back
(`messages.go:105-110`, "the tx rolls back, unwinding the topic get-or-create
too"). That arm cannot be transplanted into `AnswerAsk`'s tx: a rollback there
would lose the `Answered` flip, and a pool read races the still-uncommitted
row. `insertMessageTx` therefore contains only the insert + `last_seq` bump —
no conflict arm, no pool re-read, no rollback-and-return. Since the answer
insert carries no dedup key, the conflict path is unreachable by construction;
if the insert ever reports zero rows affected it is an invariant violation (the
guard says it cannot happen), so the helper returns an error and `AnswerAsk`
rolls the whole answer back rather than committing a flip with no message.

The answer-once guard is untouched and remains the sole answered-gate: "if
ask.Answered { return … ErrConflict …}" — "Answered flips only here, so this
is the sole gate" (`store/messages.go:631-634`). A second `RespondToAsk` still
fails before any message is posted, so at most one answer message ever exists
per ask.

### Fan-out and the out-of-sweep-set edge

`RespondToAsk` publishes `MessagePosted` for the answer message (the delivery
trigger: "A MessagePosted is the delivery trigger",
`go/internal/delivery/consumer.go:356-365`) alongside the existing
`MessageUpdated` for the ask message (`comms.go:404`, kept — it is the UI
update, not a delivery trigger). Live agent ⇒ normal deliver control; offline
agent ⇒ the cursor sweep owes it.

**Audience is the channel, not just the asker.** The answer is a normal
channel message, so it fans out to every subscribed agent in the ask's channel
— each renders "X answered the ask: …" and pays one deliver+ack cycle. This is
intended channel semantics (the ask was asked in that channel; an ask in a
home channel or DM has an audience of one anyway), not asker-private delivery.
Only the out-of-sweep-set backstop below is asker-targeted. If channel-wide
agent rendering is ever unwanted, that is a T6 render-arm policy choice (render
only when `asker_account_id == self`) — named here as deliberately NOT taken.

One edge needs a targeted arm: the deliver set and the sweep set are the
**same** disjunct — "subscribed OR home OR mandatory". `SubscribedAgents`, the
set `fanOut` iterates for live delivery (`delivery_reads.go:29-39`), and
`InSweepSet`, the cursor-sweep predicate (`delivery_cursors.go:498-512`),
mirror each other exactly "so the two never drift". So an asker in the channel
but **not** subscribed (and not home, not mandatory) is invisible to **both**
the live deliver loop and the reconnect sweep — without a targeted arm its
answer is never delivered at all.

The owed row alone is **not** sufficient here: `OwedMentions` is drained only
by `sweepOwedMentions` at `OnSessionStarted` (`delivery/settle.go:136`), so a
durable-row-only backstop delivers to an unsubscribed asker only at its **next
session start** — an unsubscribed asker with a **live** session at answer time
would have its answer stranded for the whole session lifetime (potentially the
exact wedged-waiting-agent scenario RIG-2257 exists to fix). The fix is the
full shape RIG-1641's `routeMentionsFor` already uses (`dispatch.go:139-152`):
when the answer block's `asker_account_id` is outside the channel's sweep set,
(1) **record** the durable owed row via the existing `RecordOwedMention`
(`delivery_cursors.go:90`) — the offline backstop, drained by the existing
`OwedMentions` sweep (`consumer.go:91-93`); then (2) **re-check**
`SessionForAccount` and, if the asker is live, **dispatch the answer directly**
to it — the latency path. The owed row and the live dispatch are complements
(durability vs latency), not alternatives. No new table, no new sweep — the
answer is a structurally-targeted message exactly like a mention.

**The targeted arm must be recovery-replayable, not fan-out-only.** The owed
row is recorded at `fanOut`, which runs off the **ephemeral** `MessagePosted`
bus — the exact window RIG-2490's pre-settle scan exists to close. If the
consumer restarts between `AnswerAsk`'s commit and `fanOut` processing the
in-memory event, the answer message is durably committed but still
`mentions_routed_at IS NULL`, so it reappears in `UnroutedMentionMessages`
(the recovery read selects purely on the NULL mark, `delivery_cursors.go:240`)
— but `scanMissedMentions` replays only `routeMentionsFor` (`scan.go:55`),
a no-op for a mention-less answer message, and then marks it routed. So a
fan-out-only ask_answer arm would let a restart in that window **strand** the
out-of-sweep asker permanently — the very no-silent-loss hole this section
closes. The arm therefore lives in the **same shared body** `routeMentionsFor`
does: both `fanOut` (`dispatch.go:100`) and `scanMissedMentions` (`scan.go:55`)
call it, so recovery re-derives the owed row exactly as it re-derives mentions.

**The owed backstop delivers as a STEER, not a deliver.** `sweepOwedMentions`
dispatches every owed row through the steer vehicle (`settle.go:201-203`), so
the same `ask_answer` message reaches the asker via **two** agent lanes
depending on subscription state: the coalesced deliver-lane prompt (subscribed
asker) or a steer interrupt (owed backstop). Both render through the same
`formatDeliversForPrompt` block switch the agent grows below (the steer arm
already routes through it, `agent.ts:449`) and both `DeliveryAck` per `msg.id`,
so the single new render arm covers both lanes and dedup absorbs any overlap —
but the T6 render arm therefore has **two** callers, pinned by a steer-lane
test (T7).

### The agent half, reworked (RIG-1509 is fair game)

The agent stops consuming answers from the control lane and starts recognizing
them on the **deliver lane** it already runs:

- The deliver path's renderer today ignores non-text blocks
  (`formatDeliversForPrompt`; test: "The ask block is ignored — deliver
  carries channel text only", `agent.test.ts:1767-1771`). It grows an
  `ask_answer` arm: a delivered message carrying an answer block renders via
  the existing `formatAskAnswerForPrompt` (`agent.ts:846`, kept — adapted to
  read questions and answers from the block's answered-`Ask` snapshot instead
  of a registry + control payload).
- The `askAnswer` control case (`agent.ts:691-744`), the `PendingAsks`
  registry (`comms.ts:231-249`), the ask tool's `record()` call, and the
  `#askAnswerQueue` plumbing are **deleted**. Delivery ordering, the replay
  barrier, turn coalescing, dedup by `msg.id`, and the ack all come from the
  deliver lane for free.
- On the wire, `AgentControl.ask_answer` (tag 4) and `message AskAnswerControl`
  (`proto/compass/v1/agent.proto:164,181-191`) are **removed, not reserved** —
  the repo's established pre-GA convention: "the oneof and field 7 … are both
  REMOVED, not reserved (F9: pre-dogfood, zero stored payloads)"
  (`comms.proto:327-329`).

### Net deletion set

This design deletes more than it adds. Removed outright:

- `go/internal/runnerhub/ask_waker.go` — the whole file (`WakeAskAnswer`).
- `go/internal/comms/ask_waker.go` — the whole file (`AskAnswerWaker`).
- `comms.Comms.askWaker` field, `SetAskWaker` (`comms.go:49-57, 82-84`), and
  the wake call in `RespondToAsk` (`comms.go:412-414`).
- The `var _ comms.AskAnswerWaker = (*runnerhub.Hub)(nil)` assertion
  (`go/server/sinks.go:80`) and the `SetAskWaker` wiring in server assembly.
- Proto: `AgentControl.ask_answer` oneof field + `AskAnswerControl`
  (`agent.proto:164, 181-191`), removed not reserved; regenerated in all three
  gens (`go/internal/gen/…`, `go/gen/…`, `packages/compass-agent/src/gen/…`);
  the `AskAnswerControl` token drops from the proto leak-gate grep
  (`proto/moon.yml:162`).
- Agent: the `askAnswer` control arm (`agent.ts:691-744`), the
  `#askAnswerQueue` and `#pendingAsks` fields,
  `PendingAsks`/`createPendingAsks` (`comms.ts:231-249`), the raise tool's
  registry `record()`, and the `askAnswer` arms in
  `control.ts`/`frame.ts`/`control-source.ts` docs+code.
- Tests bound to the deleted wake rail (compiler-caught, but named so the tree
  builds): in `comms/comms_test.go`, `fakeAskWaker` + `wakeCall` (`283-312`)
  and the three `SetAskWaker` tests (`314-390, 488-520`) + the nil-waker test
  (`522-531`); in `runnerhub/deliveryarm_test.go`, the three `TestWakeAskAnswer*`
  tests (`483-581`) exercising the deleted `Hub.WakeAskAnswer`; in
  `runner/gateway/control_test.go:269-270`, the `AskAnswerControl` "representable
  variant" example — swapped to a surviving op (`prompt`/`deliver`), not deleted.
  On the TS side, `control-wire.test.ts` and `compassv1.ts:73-74`
  regenerate/rebuild with the removed control arm.
- From the superseded draft of this record (never implemented — verified by
  grep: no `AnswerDelivered`/`UndeliveredAskAnswers`/`MarkAskAnswerDelivered`
  symbols exist): the owed-marker, the owed queries, the router un-flip
  machinery, and the `WakeAskAnswer` sweep arm are simply never built.

## Alternatives considered

### Rejected: the prior draft of this record — owed-marker + control-op wake

The superseded draft kept the `AskAnswerControl` wake and made it durable with
an `AnswerDelivered bool` marker on the stored ask block, a derived owed-set
JSONB query, an `OnSessionStarted` drain arm, and un-flip-on-observed-refusal
machinery riding the router's refusal correlation. Rejected NOW because:

- **It builds a second delivery system.** Marker, owed query, sweep arm,
  refusal un-flip, ABA-race analysis — each a bespoke re-implementation of
  durability the message rail already provides (ack-gated cursor + resweep,
  `hub.go:699-710` / `delivery_cursors.go:451-455`). Its best case was
  "at-least-once for the *observable*-refusal class"; the rail is
  at-least-once for everything, because the cursor advances only on the
  recipient's own ack — no refusal inference needed.
- **It doesn't scale.** Every future structured interaction (approvals, etc.)
  would repeat the whole apparatus: its own control op, its own owed marker,
  its own sweep arm, its own refusal handling. The special-message-block
  pattern amortizes all of it into one block variant per type.
- **Its premise dissolved.** The draft treated the merged RIG-1509 agent half
  as frozen substrate it must not touch, which forced the answer onto the
  control lane. Pre-GA nothing is frozen; reworking the agent half to consume
  a delivered message removes the constraint that made the bespoke rail
  necessary.

Its sub-variants are subsumed with it: **marker-on-block (its Option A)** and
**owed-answer table (its Option B)** both exist only to track owed control-op
delivery, which no longer exists; **per-message delivered-column (its Option
C)** was rejected there on granularity and stays rejected — here granularity
is a non-issue because the answer block carries its own `ask_id` and one
answer message exists per ask.

### Rejected: slim answer block (`{ask_id, answers}` without the ask snapshot)

Smaller on the wire, but it forces the agent to keep (or re-fetch) the
question set to render an answer — either resurrecting the in-memory
`PendingAsks` registry this design deletes (with its restart-wipe hole,
`comms.ts:224-234`) or adding a fetch-by-ask_id RPC round-trip on the render
path. The snapshot costs one duplicated `Ask` JSONB per answered ask and buys
a stateless agent renderer.

### Rejected: in-place mutation as the delivery vehicle (status quo)

Keeping only today's `AnswerAsk` in-place block mutation + `MessageUpdated`
cannot deliver: `MessageUpdated` "is NOT itself a trigger"
(`delivery/consumer.go:356-359`), and the mutated message stays agent-authored
and thus sweep-excluded (`delivery_cursors.go:484`). This is the bug, not a
design.

### Rejected outright: agent-side boot poll

Still excluded by the ratified RIG-1509 Decision 3 ("There is deliberately no
agent-side boot poll", compass-ask-comms-roundtrip/design.md:192-194) — and now
also unnecessary: the start-edge sweep is the poll, server-side, already built.

## Ledger delta

Do NOT edit `DECISIONS.md` in this PR — the caller applies the rows at PR time
(the ledger is concurrently relocating `docs/designs/product/DECISIONS.md` →
`docs/designs/DECISIONS.md` under compass PR #576). `Ledger-impact: deferred to
freeze`. These rows REPLACE the prior draft's DL-241..244 (never applied to any
ledger; the draft did not merge). Rows start at DL-241:

- **DL-241** — The answer to an ask is delivered as a **new message authored by
  the answerer**, carrying a server-owned `ask_answer` block (an answered-`Ask`
  snapshot + `asker_account_id`), posted in the ask's channel/topic within the
  `AnswerAsk` transaction. Delivery, ordering, ack, and recovery are the
  existing message rail's (`MessagePosted` fan-out + ack-gated cursor +
  `OnSessionStarted` resweep); there is no ask-specific delivery machinery.
  Supersedes the owed-marker + `AskAnswerControl`-wake approach; the control
  variant is removed from the wire (removed, not reserved — pre-GA, zero
  stored payloads).
- **DL-242** — Structured interactions ride **special message blocks**: a new
  `MessageBlock` oneof variant (wire) + pointer field/`blockKind` (store) +
  edge-mapper arm with a per-variant ownership rule (client-postable vs
  server-owned) + one agent render arm. Every variant inherits the delivery
  rail's guarantees; adding a control op for a per-type interaction requires a
  design record justifying why a block cannot carry it.
- **DL-243** — Ask-answer delivery is **at-least-once (may over-deliver)** via
  the rail's ack-gated cursor; the agent absorbs redelivery by the deliver
  lane's existing `msg.id` dedup. Exactly-one-answer is preserved independently
  by the store's answer-once guard (`Answered` flip → `ErrConflict`), which
  also caps the answer messages at one per ask.
- **DL-244** — An asker outside the answer channel's sweep set is backstopped
  by the RIG-1641 owed-mentions machinery (an owed row recorded at fan-out,
  drained by the existing owed sweep) — no new table or sweep. Deploy is under
  the recreate-on-schema-change regime (Matt ruled recreate): no backfill for
  pre-existing answered asks; a fresh DB has none.

## Plan

### Global Constraints

- Go on the server (`compass/go`, go1.26.6); TypeScript/Bun on the agent
  (`compass/packages/compass-agent`). `//go:build unix` on every runnerhub /
  delivery arm file (matches `delivery/settle.go:1`).
- Proto changes are additive on `compass.v1.MessageBlock` (new oneof field 3)
  and **removal, not reservation**, of `AgentControl.ask_answer` (tag 4) +
  `AskAnswerControl` — the repo's pre-GA convention (`comms.proto:327-329`).
  All three gens regenerate together: `go/internal/gen/…`, `go/gen/…`,
  `packages/compass-agent/src/gen/…`.
- The `ask_answer` block is **server-owned**: constructed only by
  `RespondToAsk`. Server-ownership is enforced at the **wire edge** —
  `blocksFromWire`/`updateBlocksFromWire` reject an inbound `ask_answer` as
  `CodeInvalidArgument`, because that is the only layer with caller identity.
  `marshalBlocks(blocks []MessageBlock)` (`blocks.go:71`) receives only the
  block slice and cannot distinguish `RespondToAsk`'s insert from any other
  caller, so it does **not** reject the variant (rejecting it would break
  `AnswerAsk`'s own answer-message persist); it **accepts and structurally
  validates** it, per T2.
- The answer-once guard (`store/messages.go:632-634`) is untouched by every
  task; the answer-message insert is strictly downstream of it in the same tx.
- The store block invariant extends, never forks: exactly ONE of
  Text / Ask / AskAnswer is non-nil per block (`types.go:310`), enforced in
  `marshalBlocks` and `unmarshalBlocks`' totality switch (a missing arm never
  passes silently, `blocks.go:178-181`).
- The delivery layer stays block-kind-agnostic on the deliver/ack/sweep path;
  the only kind-aware delivery code is the `ask_answer` asker-targeting arm,
  which lives in the shared fan-out/recovery body (called by both `fanOut` and
  the RIG-2490 recovery scan `scanMissedMentions`, T5) so live and recovery
  both re-derive it.
- runnerhub never imports comms and vice versa (preserved; T4 only deletes
  from the boundary, adds nothing to it).
- Tests are red-green (rule://red-green-testing): each task writes its failing
  test first, then the smallest change to green.

### T1 — Proto: `ask_answer` block variant; remove the control op

Add `AskAnswerBlock` and the `MessageBlock.ask_answer = 3` oneof field
(`comms.proto:343-350`); delete `AskAnswerControl` and the
`AgentControl.ask_answer = 4` oneof field (`agent.proto:164, 181-191`),
removed not reserved. Regenerate all three gens; drop `\bAskAnswerControl\b`
from the proto leak-gate grep (`proto/moon.yml:162`).

Interfaces:

```proto
// comms.proto
message MessageBlock {
  oneof block {
    string text = 1;
    Ask ask = 2;
    AskAnswerBlock ask_answer = 3;
  }
}
message AskAnswerBlock {
  Ask ask = 1;                  // answered snapshot: ask_id, answered=true, recorded answers
  string asker_account_id = 2;  // the asking agent, denormalized for fan-out targeting
}
```

Tests: buf lint + generate clean; the leak gate passes with the token removed.

### T2 — Store: `AskAnswer` block model + JSONB marshal

Add the third pointer to `store.MessageBlock` (`types.go:311-316`), an
`AskAnswerBlock` store type, a `blockKindAskAnswer` discriminant + `storedBlock`
arm (`blocks.go:22-33`), marshal/unmarshal arms (`blocks.go:71-91, 178-217`),
and fold the answered snapshot's text into `textContent` (`blocks.go:219-236`)
so answers are searchable. `marshalBlocks` validates the variant: non-nil
`Ask` with a non-empty `AskID`, `Answered=true`, and well-formed questions
(reuse `askQuestionsWellFormed`, `blocks.go:121-143`); a non-empty
`AskerAccountID`. `mintAskIDs` skips it (the snapshot's id is the original
ask's, never re-minted).

Interfaces:

```go
// store/types.go
type MessageBlock struct {
    Text      *string
    Ask       *Ask
    AskAnswer *AskAnswerBlock // exactly one of the three is non-nil
}

// AskAnswerBlock is the answer to a previously-posted Ask, authored by the
// answerer: the answered ask snapshot plus the asking agent's account id.
// Server-owned — constructed only by AnswerAsk; the comms edge rejects it on
// every client write path.
type AskAnswerBlock struct {
    Ask             Ask       // answered snapshot (AskID set, Answered=true)
    AskerAccountID  AccountID // the ask message's author
}
```

Tests: round-trip marshal/unmarshal; exactly-one-of rejection for every
two-set combination; corrupted-row error on kind/payload disagreement;
`textContent` includes the answered custom text.

### T3 — Store: `AnswerAsk` posts the answer message in-tx

Extend `AnswerAsk` (`messages.go:540-603`): after `applyAskAnswer` +
`updateMessageBlocksExec`, build the answer message — author `actor`, topic
`askMsg.TopicID`, one `AskAnswer` block snapshotting the just-answered ask
with `AskerAccountID: askMsg.AuthorAccountID` — and insert it in the SAME tx
via an insert helper extracted from `AppendMessage`'s insert + topic
`last_seq` maintenance (`messages.go:90-137`). **The answer insert carries NO
idempotency key** — the helper is called with `clientRequestID = ""`, so
`AppendMessage`'s `ON CONFLICT (author_account_id, client_request_id)` dedup
(`messages.go:96-98`) is structurally unreachable; the answer-once guard is the
sole single-fire mechanism (see Atomicity). `insertMessageTx` contains only the
row insert + `last_seq` bump — it does **not** port `AppendMessage`'s conflict
arm (no pool re-read, no rollback-and-return, `messages.go:105-110`), which
cannot run inside `AnswerAsk`'s tx without losing the `Answered` flip. A zero
`rowsAffected` from the insert is an invariant violation → return an error so
the whole tx rolls back. Return both messages. The membership/post-policy
gates are not re-run for the insert: the actor's membership in the ask's
channel is already proven by `AnswerAsk`'s visibility JOIN
(`messages.go:565-571`), in this same tx.

Interfaces:

```go
// store/messages.go — signature change; both callers (comms.RespondToAsk and
// its tests) migrate in this task.
func (s *Store) AnswerAsk(ctx context.Context, actor AccountID, askID string, answers []AskAnswer) (askMsg Message, answerMsg Message, err error)

// tx-scoped insert core shared by AppendMessage and AnswerAsk: row insert +
// topic last_seq GREATEST maintenance ONLY. No ON CONFLICT / dedup arm — the
// dedup path stays in AppendMessage, which calls this then handles conflict
// itself; AnswerAsk calls this with clientRequestID="" and relies on the
// answer-once guard for single-fire.
func insertMessageTx(ctx context.Context, tx pgx.Tx, m Message, topicID string, clientRequestID string) (Message, error)
```

Tests: answering posts exactly one answer message in the ask's channel+topic,
authored by the answerer, block snapshot carrying the recorded answers; a
second `RespondToAsk` is `ErrConflict` with NO second message; a crash
simulation (tx rollback) leaves neither the flip nor the message; the
answer-once guard test corpus still green.

### T4 — Comms: publish the answer; delete the wake rail

`RespondToAsk` (`comms.go:383-416`): consume the new two-message return,
publish `MessageUpdated(askMsg)` (kept — the UI ask-state update) AND
`MessagePosted(answerMsg)` (the delivery trigger). Delete the `askWaker`
field, `SetAskWaker` (`comms.go:49-57, 82-84`), the wake call
(`comms.go:412-414`), `comms/ask_waker.go` (the `AskAnswerWaker` interface
file), `runnerhub/ask_waker.go` (the whole file), the structural assertion +
wiring (`server/sinks.go:75-80` and the `SetAskWaker` call in assembly). Add
the `ask_answer` arms to `blocksToWire` (`mapping.go:187-198`, maps the
snapshot via the existing `askToWire`) and the REJECTION arms to
`blocksFromWire` / `updateBlocksFromWire` (`mapping.go:312, 338`) —
`CodeInvalidArgument`, "ask_answer blocks are server-owned".

Interfaces:

```go
// comms/mapping.go — new wire<->store arms
// blocksToWire: case b.AskAnswer != nil -> &compassv1.MessageBlock{Block:
//   &compassv1.MessageBlock_AskAnswer{AskAnswer: askAnswerToWire(b.AskAnswer)}}
func askAnswerToWire(b *store.AskAnswerBlock) *compassv1.AskAnswerBlock
// blocksFromWire / updateBlocksFromWire: case *compassv1.MessageBlock_AskAnswer
//   -> connect.NewError(connect.CodeInvalidArgument, errServerOwnedBlock)
```

Tests: RespondToAsk fans out `MessagePosted` for the answer message;
`PostMessage` carrying an `ask_answer` block is invalid-argument; the waker's
test consumers are deleted with it — `fakeAskWaker`/`wakeCall` and the three
`SetAskWaker` tests (`comms_test.go:283-390, 489-520`) plus the nil-waker test
(`comms_test.go:522-531`).

### T5 — Delivery: targeted asker delivery (owed backstop + live dispatch)

At `fanOut` (`delivery/dispatch.go:99`), when a posted message carries an
`ask_answer` block, target the asker the way RIG-1641's `routeMentionsFor`
targets a mentioned agent (`dispatch.go:139-152`) — because the deliver set
(`SubscribedAgents`, `delivery_reads.go:29-39`) and the sweep set
(`InSweepSet`, `delivery_cursors.go:498-512`) are the **same** disjunct
(subscribed OR home OR mandatory), so an unsubscribed non-home asker is reached
by neither the live deliver loop nor the reconnect sweep without a targeted arm.
Resolve the block's `asker_account_id`; if `InSweepSet(asker, channel)` is
false: (1) **record** the durable owed row via `RecordOwedMention(ctx, asker,
channel, msg.id)` (`delivery_cursors.go:90`) — the offline backstop drained by
the existing `OwedMentions` start-edge sweep (`consumer.go:91-93`); then (2)
**re-check** `SessionForAccount(asker)` and, if the asker is live, dispatch the
answer directly to it — the latency path. Owed row and live dispatch are
complements (durability vs latency), not either/or: without the live dispatch,
an unsubscribed asker with a live session strands until its next restart. A
subscribed/home/mandatory asker needs neither arm — the normal
deliver+cursor-sweep path already covers it.

The targeted arm lives in the **shared fan-out/recovery body** (§"Fan-out and
the out-of-sweep-set edge"): both `fanOut` (`dispatch.go:100`) and the RIG-2490
recovery scan `scanMissedMentions` (`scan.go:55`) call it, so a consumer restart
between `AnswerAsk`'s commit and `fanOut` re-derives the owed row on the next
recovery scan (the answer message is committed `mentions_routed_at IS NULL`, so
it appears in `UnroutedMentionMessages`) rather than stranding the out-of-sweep
asker.

`sweepOwedMentions` dispatches an owed row as a **STEER** (`settle.go:201-203`),
not a deliver — so T6's render arm is reached by both the deliver lane and the
steer lane (both route through `formatDeliversForPrompt`, both ack per `msg.id`).

Interfaces:

```go
// delivery/dispatch.go — consumes only existing DeliveryReads methods:
//   InSweepSet(ctx, agent store.AccountID, channel store.ChannelID) (bool, error)
//   RecordOwedMention(ctx, agent store.AccountID, channel store.ChannelID, messageID string) error
//   SessionForAccount(agent store.AccountID) (sessionID string, live bool)  // hub, already used by routeMentionsFor
// (InSweepSet joins DeliveryReads if not yet on the interface; *store.Store
// already implements it.)
func askAnswerTarget(msg *compassv1.Message) (asker store.AccountID, ok bool)
```

Tests: answer posted while asker is member-not-subscribed + OFFLINE → owed row
recorded → next `OnSessionStarted` delivers; answer posted while asker is
member-not-subscribed + LIVE → dispatched directly WITHOUT a session restart
(the F2 regression); asker subscribed → normal deliver, no owed row, no direct
dispatch.

### T6 — Agent: consume the answer from the deliver lane; delete the control arm

In `packages/compass-agent`: regen picks up the wire changes (the `askAnswer`
control case no longer compiles — deletion is compiler-driven). Delete the
`askAnswer` arm in `#applyControl` (`agent.ts:691-744`), the
`#askAnswerQueue`, `PendingAsks`/`createPendingAsks` (`comms.ts:231-249`) and
the raise tool's `record()` call, plus the `ask_answer` mentions in
`control.ts`/`frame.ts`/`control-source.ts`. Rework
`formatAskAnswerForPrompt` (`agent.ts:846`) to render from the delivered
block's answered snapshot — questions and answers now live on the same
`AskQuestion` objects (`chosen_option_ids`/`custom_text`) — and call it from
the deliver render path (`formatDeliversForPrompt`, `agent.ts:814`) for a
message whose block is `ask_answer`. Ordering, replay barrier, coalescing,
`msg.id` dedup, and the `delivery_ack` all come from the deliver lane
unchanged.

Interfaces:

```ts
// agent.ts — replaces formatAskAnswerForPrompt(questions, answers)
export function formatAskAnswerForPrompt(ask: Ask): string
// formatDeliversForPrompt grows the block-kind switch: text -> as today,
// askAnswer -> formatAskAnswerForPrompt(block.value.ask); bare ask blocks stay
// ignored on deliver (unchanged).
```

Tests: a delivered `ask_answer` message renders question text + chosen option
labels + custom text (port the existing `formatAskAnswerForPrompt` corpus,
`agent.test.ts:1808-1891`); redelivery of the same message id injects once;
an answer delivered to a fresh session (post-restart, no registry) renders
fully — the old unknown-ask-id degraded arm is gone.

### T7 — E2E: recovery scenarios

Server-side integration tests over store + delivery + hub fakes (the existing
consumer/hub test harness): (1) answer submitted with NO live session →
`OnSessionStarted` sweep delivers the answer message → ack advances the
cursor → second start delivers nothing; (2) answer delivered but ack lost →
reconnect re-promotion redelivers (at-least-once); (3) duplicate
`RespondToAsk` → `ErrConflict`, exactly one answer message ever; (4)
member-not-subscribed OFFLINE asker → owed-row path delivers at next start;
(5) member-not-subscribed LIVE asker → direct dispatch delivers without a
restart; (6) owed answer swept at session start arrives as a STEER and the
agent's steer arm renders it through the same `ask_answer` render path;
(7) answer posted, consumer **restarts before `fanOut`** runs, out-of-sweep
asker still recovers — the committed answer message (`mentions_routed_at IS
NULL`) is re-picked by `scanMissedMentions`, which re-derives the owed row
through the shared targeting body, and the next `OnSessionStarted` delivers it.

Interfaces: none new — exercises T2-T5 surfaces as built.

## Tasks

- [ ] T1 — Proto: `AskAnswerBlock` + `MessageBlock.ask_answer=3`; remove
      `AskAnswerControl` + `AgentControl.ask_answer`; regen ×3; moon.yml gate.
- [ ] T2 — Store: `MessageBlock.AskAnswer` + `blockKindAskAnswer` +
      marshal/unmarshal/validation/textContent.
- [ ] T3 — Store: `AnswerAsk` returns `(askMsg, answerMsg, err)`, inserts the
      answer message in-tx via extracted `insertMessageTx`.
- [ ] T4 — Comms: publish `MessagePosted(answerMsg)`; wire-mapper arms
      (to-wire map, from-wire reject); delete waker interface/impl/wiring.
- [ ] T5 — Delivery: targeted asker delivery — owed row (`RecordOwedMention`)
      as offline backstop + live direct dispatch (`SessionForAccount` re-check).
- [ ] T6 — Agent: deliver-lane `ask_answer` render; delete control arm,
      `PendingAsks`, `#askAnswerQueue`.
- [ ] T7 — E2E: offline answer, reconnect redelivery, answer-once, owed-row
      backstop.

## Open Questions

The prior draft's three load-bearing OQs are **resolved by Matt's ruling**:
delivery mechanism → answer-as-message (this record); at-least-once vs ack
frame → the rail's existing `delivery_ack` (no new frame); deploy/backfill →
recreate regime, no backfill (DL-244).

- **OQ-1 (non-load-bearing) — human-facing rendering of the answer message.**
  Web/UI clients will now see an answerer-authored message carrying an
  `ask_answer` block in the channel timeline. Recommendation: render it as a
  compact "answered the ask" card keyed off the block; defer to the UI lane —
  the server contract (this record) is complete without it, and a client that
  ignores the block loses nothing (the ask block itself still shows answered
  state via `MessageUpdated`).
- **OQ-2 (RESOLVED in-design, not a fork) — is the live-targeted deliver arm
  droppable in favor of the owed row alone?** No. The deliver set
  (`SubscribedAgents`) and the sweep set (`InSweepSet`) are the identical
  disjunct, and the owed row is drained only at `OnSessionStarted`
  (`settle.go:136`) — so an unsubscribed asker with a **live** session would
  have its answer stranded until its next restart if the owed row were the only
  arm. The owed row (durability) and the live dispatch (latency) are
  complements, both required; T5 specifies both, and T7 case (5) pins the
  live-unsubscribed path. Recorded here (not deleted) because the prior draft
  framed this as a droppable simplification, which the red-team falsified.
