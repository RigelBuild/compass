# Compass notification delivery — the server-side push into the agent's session

Status: Active

> Freezes on merge; later changes supersede by citation, never rewrite
> (`../compass-0.5/design.md:10-12`, convention restated in
> `../compass-0.6/design.md:1116-1118`). This record is EXECUTION AGAINST A
> FROZEN DELIVERY MODEL: Matt's round-two/round-three rulings in
> `../compass-0.6/design.md` (RT-2 home channel, RT-3 turn-end delivery, the
> delivery-timing amendment) and the merged Server-ownership-layer record
> (`../compass-server-ownership-layer/design.md`, #995) are consumed and cited,
> never re-decided. Tracker: RIG-1569. Lane: compass-comms (driver);
> co-owned pieces are named per task.

## Problem / Intent

A message posted to an agent's home or subscribed channel lands in Postgres and
fans out to every UI — and the agent never hears about it. `PostMessage` commits
the row and calls `publishMessagePosted`
(`compass/go/internal/comms/comms.go:270-272` → `mapping.go:366-372`), which
feeds only the Client-facing `SubscribeComms` stream (`subscribe.go:31-44`).
The agent-bound seam exists but is inert: the `AgentControl` oneof already
carries `deliver = 3` (`compass/proto/compass/v1/agent.proto:122`), yet
`DeliverControl` is an empty shell (`agent.proto:153-154`: "Empty shells —
payload fields parked (RIG-1310)") and the Runner's control lane refuses to send
one (`gateway/control.go:65-70`, `errEmptyControlVariant`). Nothing bridges a
`MessagePosted` to a running session — Matt: "if you send a message to an
agent's home channel and it doesn't get delivered the whole app is unusable"
(`~/notes/wave/compass-predogfood-gap-analysis.md:214-221`). This record designs
that bridge: the server-side fan-out consumer, the durable delivery cursor, the
`DeliverControl`/`delivery_ack` payload contract, MVP presence/status,
server-side mention→steer routing, and the generic rail Issue/PR subscription
notifications (#995) ride.

## Approach

### The frozen inputs, verbatim (consumed, never re-decided)

- **RT-3 — turn-end delivery: deliver → queue → coalesce → ack** (Matt,
  round-three, `../compass-0.6/design.md:1454-1468`, restated `:1839-1843`): a
  plain subscribed-channel message "is delivered to the agent **immediately** as
  an `AgentControl.deliver`"; the CompassAgent "**queues** each `deliver` while a
  turn is running and, at turn end, issues the queued set as a **single**
  `prompt`"; "the **Server** tracks per-session delivery from those acks —
  advancing a delivery cursor and redelivering any un-acked `deliver` from
  Postgres on reconnect"; "the agent owns the **turn-end coalescing queue**; the
  Server owns the **durable delivery cursor** — two queues, two owners, one ack
  that links them. The `@`-mention `steer` remains the only mid-turn interrupt."
- **RT-2 — home channel** (`../compass-0.6/design.md:1762-1769`, `:1834-1838`):
  the agent's own channel is its `home_channel_id` on `AgentAccount`, minted at
  `CreateAgent`, always-subscribed. BUILT: the store mints it transactionally
  (`compass/go/internal/store/accounts.go:156-158` — "INSERT INTO
  agent_accounts (account_id, owner_user_id, home_channel_id)") and seeds the
  agent's member row subscribed (`accounts.go:177` — "VALUES ($1, $2, FALSE),
  ($1, $3, TRUE)"); the proto field is Server-set
  (`compass/proto/compass/v1/comms.proto:141`).
- **Delivery timing amendment** (`../compass-0.6/design.md:396-406`, `:431-467`):
  "a frame arriving while the agent is **idle starts a new turn**"; mid-turn, an
  `@`-mention-borne `steer` "**always interjects a mid-turn steer** … for **any
  agent that is a member of the channel**, regardless of its subscribe state"
  (`:454-458`); a plain subscribed message rides the turn-end `deliver`; steer
  reuses `PostMessage` with server-side `@`-mention routing — no dedicated
  `SteerAgent` RPC (`:425-430`). Reserved pings `@agents`/`@users`/`@everyone`
  resolve server-side and fan out unbounded for MVP (RT-4, `:1844-1845`).
- **The control-lane rail is BUILT** (merged #38): `ControlSender.Send(sessionID,
  op)` stamps a Runner-assigned monotonic `control_seq`, retains until acked,
  and drains in send order ("Wire order is apply order",
  `compass/go/internal/runner/gateway/control.go:26-31`); `SendIfLive`
  fails instead of queueing for an absent agent (`control.go:241-243`). The
  agent-side dispatcher already classes `steer`/`deliver` as immediate-dispatch
  and counts them unmapped while the shells are empty
  (`compass/packages/compass-agent/src/transport/control-source.ts:20-24,
  353-377`).
- **RIG-1310 §8 proto state**: `AgentControl.deliver = 3` exists
  (`compass/proto/compass/v1/agent.proto:122`) but `DeliverControl {}` is an
  empty shell (`agent.proto:153-154`), and the Runner refuses to send an empty
  shell (`gateway/control.go:65-70`). `AgentFrame` has no `delivery_ack` variant
  today (`agent.proto:40-68`; grep `DeliveryAck` matches only the design-record
  citation in comments, `agent.proto:56`, `:159`). The proto delta below is
  co-owned: it lands as compass-spec's consolidated RIG-1310 follow-up, in
  coordination with compass/compass-agent — never unilaterally from this lane.
- **#995 Server ownership layer** (merged 2026-07-30,
  `../compass-server-ownership-layer/design.md`): Decision 5 freezes the forge
  subscription model — Server-stored subscriptions, a per-artifact FETCH cursor
  split from a per-subscriber DELIVERY cursor, poll-based in v1, and delivery
  over "the EXISTING push path": one additional `SessionsResponse` oneof
  variant Server→Runner and one `deliver`-adjacent `AgentControl` variant
  Runner→agent, with `SubscribeComms` explicitly NOT the delivery path ("the
  **Client**-facing stream — UIs subscribe to it; agents do not").
- **DL-028 / DL-029** (`DECISIONS.md:90-91`): the MVP agent comms toolset is two
  native tools, no ask-answering; agent comms identity is session-resolved
  server-side, home-channel default.
- **Anycast DROPPED, presence/status NEEDED** for MVP (Matt 2026-07-29,
  `~/notes/wave/compass-predogfood-gap-analysis.md:182, 234`).

### D1 — the fan-out consumer: a Server-side bus consumer dispatching over the existing Sessions relay

**Where the bridge lives: in the Server, as a comms-bus consumer beside
`SubscribeComms`, dispatching through the RunnerHub.** The comms event bus is
in-process Server state (`comms.Comms.bus`, fed by `publishMessagePosted`,
`mapping.go:366-372`), and the only Server→Runner push channel is the `Sessions`
bidi stream's response half (`compass/proto/compass/v1/runner.proto:57-66`,
command oneof at `:164-170`), routed per-Runner by the hub's command router
(`compass/go/internal/runnerhub/router.go:3-7`). So the bridge is a new
`internal/delivery` consumer constructed in server assembly (beside
`newRunnerHub`, `compass/go/server/sinks.go:127-138`): it subscribes to the
same bus `SubscribeComms` reads, and for each `MessagePosted` resolves the
subscribed agent sessions and hands the RunnerHub a typed delivery dispatch —
timed per the settle gate below (human-authored at post; agent-authored at
the author's turn-settle). The
alternative — the runnerhub consuming the bus itself — couples the
session-command relay to comms semantics (subscriber resolution, mention
parsing, cursor bookkeeping) that are squarely comms-lane; the hub stays what it
is today, a router with typed dispatch methods (`commands.go:48-119` pattern).

**Subscriber resolution is one SQL read — with the home channel as an explicit
disjunct.** On `MessagePosted(channel_id)`: resolve agent members —
`channel_members cm JOIN agent_accounts aa ON aa.account_id = cm.account_id
WHERE cm.channel_id = $1 AND (cm.subscribed OR cm.channel_id =
aa.home_channel_id)`. The disjunct is a frozen-model-fidelity repair, not an
optimization: RT-2 makes home-channel subscription "implicit, not a togglable
row" (`../compass-0.6/design.md:441-444`), but the BUILT store makes it an
ordinary togglable row — `CreateAgent` seeds it `subscribed=TRUE`
(`accounts.go:178-181`) while `addOrUpdateMember` can flip a directly-named
member's row via `ON CONFLICT … DO UPDATE SET subscribed = EXCLUDED.subscribed`
(`channels.go:442-456`); the no-clobber protection covers only the i>0
pulled-in owner rows (`DO NOTHING`, `channels.go:459-463`). The query enforces
the frozen guarantee read-side, independent of the row's flag. A store-side
guard rejecting a `subscribed=false` flip on the home row is optional
belt-and-suspenders owned by compass-server (`channels.go`), not required for
delivery correctness. The author is excluded (an agent never receives its own
post back as a `deliver`). Account → live session resolves through the durable
`agent_sessions` mapping (`store/agent_sessions.go:31-56`,
session_id → agent_account_id, recorded at `StartAgentSession`) intersected with
the hub's live-Runner routing — a new reverse lookup beside the hub's existing
`sessionAccounts` binding (`runnerhub/relay_comms.go:47-63,74-83`). No live
session → nothing is pushed now; the cursor sweep (D2) delivers on next start.

**The trigger: deliver-on-SETTLED — the author split (OQ-7, RATIFIED Matt
2026-07-29, overruling the on-posted recommendation).** Delivery does NOT
fire unconditionally on `MessagePosted`. A **human-authored** message does
not stream — it is settled at post — so it delivers at post, as before. An
**agent-authored** message STREAMS: the row posts with an initial block set
and grows via `MessageUpdated`, and NO wire flag marks completion —
`Message` (`comms.proto:234-251`), `MessagePosted` (`comms.proto:394-397`),
and `MessageUpdated` (`comms.proto:399-404`, "carries the full current block
set") carry no done/final marker — so the settle SIGNAL is the authoring
agent's turn-lifecycle edge to a SETTLED state: WORKING → READY (the
`agent_end` transition, `compass/packages/compass-agent/src/mapping.ts:146`;
presence IDLE) on normal turn-end, OR any TERMINAL state —
STOPPED/ERRORED/DISCONNECTED — on abnormal end, extracted by the D4 pipeline
when the author actually emits a terminal frame (`#emitStatus`,
`compass/packages/compass-agent/src/agent.ts:138-141` →
`SessionFrame.state` → `runnerhub/hub.go:443-453`, over the enum
`compass.proto:169-180`). One source, now three consumers: board state,
comms presence (D4), and this delivery-settle gate. Mechanism: on a
`MessagePosted` from an agent author, the consumer registers a
pending-deliver keyed by `(author_session, message_id)`; the same hub arm
that extracts the WORKING → READY transition fires the held delivers
for that author's session, re-reading the message's current (settled) block
set at dispatch. If the author session instead reaches a terminal state while
holding, the fire path turns on whether that transition surfaces as an
agent-emitted terminal frame: a clean control-stream close (`STOPPED`) or a
recoverable control-loop throw (`ERRORED`), both emitted by `#emitStatus`
(`compass/packages/compass-agent/src/agent.ts:115-130`), fire the held set
from each message's stored (last-known) block set — the exact post-hoc path
used below for an author already stopped at post — and clear the registry
entry. A no-frame author death emits nothing: a hard-kill (OOM / SIGKILL /
segfault) bypasses `#emitStatus`, and a dropped Runner link surfaces as
`DISCONNECTED` in T4 whose reattach-window / expiry→`ERRORED` machine is
T9-deferred (`compass.proto:163-168`), so no terminal frame fires. For that
path the held entry is soft in-memory state — neither fired nor cleared by an
author trigger, but reaped when the author's session unbinds on the next
Runner (re-)enroll (the hub clears the whole session map,
`runnerhub/hub.go:542-549`) or on Server restart — and delivery falls to the
cursor backstop below, not to this registry; the guarantee is no-loss, not
no-leak. A message with no matching live author session (author already
stopped at post) delivers immediately from its stored block set — the
sweep/post-hoc path; there is no live turn to wait on. Cursor interaction
(D2): a held, not-yet-fired deliver has never been dispatched, so its seq is
not acked and is indistinguishable to the cursor from an undelivered message
— the reconnect sweep simply redelivers it from stored (by then settled)
blocks; consistent, no special case. (Forward note, scoped OUT of this
record: Matt is separately considering agent↔agent channel mutexes/locks —
an "… is typing" equivalent to prevent concurrent sends; nothing here
designs it.)

**The push leg mirrors #995's ratified shape.** A new `SessionsResponse` command
variant carries `{session_id, AgentControl}` — the deliver (or steer) op fully
formed by the Server — and the Runner-side `Sessions` dispatch loop gains an
arm that calls the new `agentHost.Deliver(sessionID, op)` facade. The
`controlProducer` is per-container, created inside `gateway.Serve` and owned
by `SocketListener` (`socket.go:95`), so the arm cannot reach
`ControlSender.Send` directly: `Deliver` resolves session → container →
listener → `controlProducer.Send` under the host lock, mirroring what
`Start`/`Stop` do (`host.go:232`, `:251`); `Send` itself keeps the
retain-until-acked semantics RT-3's at-least-once redelivery rides
(`gateway/control.go:26-31`).
This is deliberately a *generic control-op relay variant*, not a
message-specific one: #995's forge notification is "one additional variant on
that existing oneof" too, and a generic relay arm means the forge leg (D6) and
any later notification class plug in without another Runner change. The
relay adds no new SUCCESS result variant — the success receipt rides
`AgentFrame.delivery_ack` per RT-3 — but a refusal rides the existing
in-band `RunnerError` result (`runner.proto:179-196`; the
`SessionsRequest.result` member `error = 7`, `runner.proto:151`),
correlated by `request_id`, which the Server tracks per in-flight
dispatch: a deliver refused because the session is unknown or
retention is full (`control.go:72-87`) is surfaced to the Server, which
leaves the cursor unadvanced — the sweep redelivers.

**Ordering.** Within a channel, message order is `messages.seq` (BIGSERIAL,
`store/migrations/0001_init.sql:114`, "a stable total order",
`:125-128`). The consumer serializes dispatch per session in ascending seq;
the control lane preserves send order end-to-end ("a single per-session queue
drained by one goroutine, so Send order is delivery order",
`control.go:26-27`). Cross-channel interleaving carries no ordering promise —
the agent coalesces everything queued at turn end anyway (RT-3).

At session start the reconnect sweep (D2/T6) and the live bus consumer would
otherwise race; a per-session dispatch gate serializes them — while a
session's sweep runs, live bus events for THAT session queue behind the sweep
and drain after it, preserving ascending-seq per session (the control lane
preserves SEND order; it cannot repair an interleaved send order). The race
is a T6 test case.

**The bus is loss-tolerant; the cursor is the durability.** The events ring can
lag a subscriber out (`subscribe.go:21-24`); the consumer treats a resync
exactly as `SubscribeComms` clients do — but instead of re-snapshotting it runs
the D2 sweep, because Postgres + the cursor already define exactly what is
undelivered. Missed bus events are therefore a latency blip, never a loss.

### D2 — the durable delivery cursor: Server-owned, keyed (agent_account, channel), advanced on ack

**Schema: a new table, not a column on the session row.** RT-3 fixes ownership
(Server) and mechanism (advance on ack, redeliver from Postgres on reconnect);
this record keys the cursor on the durable identity `(agent_account_id,
channel_id)` — a re-key surfaced to Matt as OQ-4 and RATIFIED (Matt,
2026-07-29): per-(agent, channel) durable, amending RT-3's "per-session"
wording. The motivation: a per-session cursor dies
with its session — and the MVP container is stateless and replaced routinely
(#995 Decision 5 banks on exactly this: "subscribers to one artifact routinely
have different liveness (containers are stateless and replaced)") — so a fresh
session of the same agent would either replay all history or silently skip the
gap. Keying on the durable identity makes "what has this AGENT been told"
survive session replacement, which is the property the redelivery sweep needs.

**Cursor shape: a contiguous low-water cursor PLUS a bounded above-cursor set
— not a single high-water mark.** `messages.seq` is a table-global BIGSERIAL
(`store/migrations/0001_init.sql:114`), sparse within any one channel, and the
codebase documents that BIGSERIAL assignment can lag commit
(`comms.proto:376-379`), so a single GREATEST-advanced high-water cursor loses
messages two ways: a concurrently-posted lower seq that commits after a higher
one is acked falls below the mark forever, and a mid-turn steer ack (D5) would
jump the mark past un-acked delivers still in the agent's memory-only turn-end
queue. The fix mirrors the codebase's own `ControlAck { acked_seq + repeated
uint64 applied_above }` (`agent.proto:163-169` — "Seqs applied out of order
ABOVE the contiguous cursor"): per (agent, channel) the Server keeps the
highest seq at or below which every message OWED to this agent is acked
(the contiguous cursor) plus a bounded sparse set of acked seqs above it —
reconstructed server-side from the per-message acks (D3), so the WIRE stays a
single `message_id`.

```sql
-- 000N_delivery_cursors
CREATE TABLE agent_delivery_cursors (
    agent_account_id TEXT NOT NULL REFERENCES agent_accounts (account_id) ON DELETE RESTRICT,
    channel_id       TEXT NOT NULL REFERENCES channels (id) ON DELETE RESTRICT,
    -- The contiguous low-water cursor: highest messages.seq at or below which
    -- every message OWED to this agent on this channel is acked (a
    -- self-authored seq is vacuously satisfied — never a hole). Seeded
    -- to the channel head at subscribe time (mirroring #995's "a fresh
    -- subscription is caught up … so subscribing does not replay history").
    acked_seq        BIGINT NOT NULL DEFAULT 0,
    -- Acked seqs ABOVE the contiguous cursor (out-of-order acks: steer jumps,
    -- lost live acks), bounded to a small documented out-of-order window;
    -- drained into acked_seq as gaps fill. Mirrors ControlAck's
    -- acked_seq + applied_above (agent.proto:163-169).
    above_seqs       BIGINT[] NOT NULL DEFAULT '{}',
    acked_at         TIMESTAMPTZ,
    PRIMARY KEY (agent_account_id, channel_id)
);
```

Internal seq arithmetic is int64/BIGINT throughout — `messages.seq` is
BIGSERIAL/int64 (`0001_init.sql:114`) — and no uint64 appears on the wire or
in store signatures: the wire ack coordinate is a string `message_id` (D3).

This is the same two-cursor discipline #995 ratified for forge artifacts — the
delivery cursor is per subscriber, "advanced only after that agent's own
successful notify", so one wedged agent never stalls another.

**Seeding: transactional with the membership row, so a missed hook is
LOUD.** A membership-subscribe transaction seeds `acked_seq` to the
subscribe-time channel head captured IN THE SAME TXN as the
`channel_members` row — NO history replay (matching #995's
seed-to-current-revision discipline) — so a missed seed hook is a loud
missing-row / constraint failure, not a silent skip. The seed paths are
enumerated: `CreateAgent`'s transactional home-channel member seed
(`accounts.go:178-181`) already does exactly this; `UpdateChannelMembers`
subscribe — the subscribe upsert is `addOrUpdateMember`,
`compass/go/internal/store/channels.go:443-463` (the
`INSERT … channel_members … subscribed` upsert at `:451-453`), and the
cursor seed rides that same txn; and RT-5's DM auto-subscribe
(`../compass-0.6/design.md`, RT-5). The "absent row = caught-up to the
channel head at first sight, head-at-sweep-time" reading applies ONLY as a
fail-safe for a genuinely pre-existing agent with no row (legacy), never as
the normal subscribe path — relying on it would bias fail-DANGEROUS to
silent under-delivery of anything posted between an un-seeded subscribe and
the agent's first session. The legacy absent-row case and the same-txn seed
are T2 red-first tests.

**Advance on ack: resolve the message id, mark, fill gaps.** The agent emits
one `AgentFrame.delivery_ack` per processed message (the new variant, D3),
carrying the acked `message_id` — the frozen shape
(`../compass-0.6/design.md:1426-1428`). It rides the `PublishEvents` spine
like the other two control-plane acks (`agent.proto:54-67`) and the RunnerHub
routes it to the cursor store (a new arm in `Deliver`'s frame switch,
`runnerhub/hub.go:237-253`) — never to a comms/session surface. On each ack
the Server resolves `message_id` → its `messages.seq`; the message MUST be one
dispatched to this agent for this channel — that resolution IS the clamp: an
id never dispatched is a no-op, so an agent cannot overshoot the channel head
with a fabricated ack (the same untrusted-side clamp discipline as
`control.go:319-323`, needing no numeric bound because the wire carries no raw
seq). Mark that seq delivered, then advance the contiguous cursor — defined
precisely as **the highest seq at or below which every message OWED to this
agent on this channel is acked**. The gap-fill step advances across any seq
that is EITHER acked (present in `above_seqs`) OR not owed to this agent —
`author_account_id = $agent`, the agent's own posts, never dispatched — the
SAME author-exclusion the sweep query below already applies. A
self-authored seq is vacuously satisfied and never a hole; without the
exclusion, every post the agent itself makes would be a permanent gap
(`messages.seq` is table-global, and the agent never receives — so never
acks — its own posts), wedging the cursor and growing `above_seqs` without
bound. With it, `above_seqs` stays bounded to the genuine out-of-order
delivery window — the enforcing mechanism behind the schema comment's
"small documented out-of-order window". Retain sparse acked seqs above the
cursor in `above_seqs`. A duplicate or reordered ack is a no-op.

**Redeliver on reconnect: the sweep.** At `StartAgentSession` (after the hub
binds session→account, `relay_comms.go:47-63`) and at Runner re-enrollment,
the Server sweeps: for each channel the agent subscribes to (home channel
included per D1's disjunct),
`SELECT … FROM messages WHERE channel_id = $1 AND seq > cursor.acked_seq AND
seq <> ALL (cursor.above_seqs) AND author_account_id <> $agent ORDER BY seq` →
re-issue each as a `deliver` through D1's dispatch. Two properties the
contiguous+sparse shape buys that a GREATEST high-water loses:

- **Commit-lag / lost-ack safety.** If A(seq 9) and B(seq 10) post
  concurrently and B commits, is delivered, and is acked first while A's live
  dispatch is lost, the contiguous cursor stays at 8 (A unacked) with seq 10
  in the above-set — the sweep's `seq > 8` still returns A. A GREATEST
  high-water would have jumped to 10 and skipped A forever.
- **Steer out-of-order safety.** A steer for message N acked mid-turn marks N
  in the above-set; the contiguous cursor does NOT jump past un-acked N-2/N-1
  still in the agent's turn-end queue — the sweep still redelivers them, and
  N is not re-swept (it is in the above-set). No lost delivery, no
  double-inject.

The sweep is at-least-once; exactly-once comes from agent-side **message_id**
dedup (T5): the agent drops a deliver whose `Message.id` it already processed
this session. The existing control_seq dedup (`control-source.ts:316-327`)
covers ONLY Runner-retention redelivery of the same retained op — a server
sweep re-issue is a fresh `Send` under a fresh `control_seq` and passes that
dedup untouched, so message_id dedup is what makes sweep redelivery safe. The
replay-barrier interaction is a named cross-lane dependency, not a freebie:
`HoldForReplay` has no production caller yet (`control.go:426-429` — "No
production caller exists yet… exercised only by tests. Not dead code; not yet
reachable"); it is raised only once the resume workstream (gap-analysis build
item #4, a different lane) wires it. Fresh-start sessions need no barrier, so
the MVP path is safe without it; T6's barrier assertion is gated on that
workstream.

### D3 — `DeliverControl` payload: the first-party `compass.v1.Message`; the ack is the message id

**The RIG-1310 parking reason does not apply here.** The shells were parked
because "their fields carry an inbound SDK `AgentMessage` (a four-way union with
an opaque provider payload)… neither of which any existing compass.v1 message
represents" (`agent.proto:103-112`). A channel post is not that: it is a
first-party `compass.v1.Message` (`comms.proto:234-251`), fully representable
today — and RIG-1310 §1's opaque-SDK parking applies to steer+config, not
deliver, so deliver un-parks independently. The payload contract is resolved
with the compass-agent co-owner (OQ-2, resolved):

```proto
message DeliverControl {
  // The channel post, verbatim — the first-party Message, not an SDK shape.
  // No seq rides the wire: the agent acks Message.id (comms.proto:234) and
  // the Server maps message_id → messages.seq internally for cursor math.
  compass.v1.Message message = 1;
}
// AgentFrame gains (additive, buf-breaking-safe like ReplayCompleteAck/ControlAck):
message DeliveryAck {
  // The acked message id — the FROZEN ack shape: compass-0.6:1426-1428 froze
  // delivery_ack as "carrying the acked message id so the Server advances
  // the … delivery cursor".
  string message_id = 1;
}
```

The ack is per message: N coalesced delivers → N `delivery_ack` frames, one
per processed message (the singular frozen shape; compass-agent independently
cites the same `delivery_ack{message_id}` and leans per-message). Acks ride
the loss-tolerant Publish spine (`agent.proto:54-67`), so N small frames is
cheap for MVP; a lost ack costs one redelivery, which the message_id dedup
absorbs (D2).

**Full `Message` over a projected subset — resolved with the co-owner.** A
projection (author + text) saves little — the internal gen lanes already
regenerate imported public types into the internal trees
(`compass/proto/moon.yml:65-69`: the TS internal lane uses
`--include-imports`; the Go internal lane M-maps imports to the public
`go/gen` package) — and costs real capability: blocks carry asks
(`comms.proto:258-264`), threading needs `parent_message_id`, and any
projection becomes a second message schema to keep in sync. The agent formats
what it needs from the full shape. compass-agent has confirmed exactly this
(full `Message`, agent formats from structured fields, citing RIG-1310), so
OQ-2 is a documented resolution, not an open fork.

**Mention-borne steer gets the same treatment.** The frozen model routes an
`@`-mention as `AgentControl.steer` sourced from a channel `PostMessage`
(`../compass-0.6/design.md:425-430`) — also a first-party `Message`. So the
channel-borne `SteerControl` carries the same single `Message` field (no seq;
the id is in the Message). The generic SDK-`AgentMessage` steer (a
Runner-originated steer outside any channel) stays parked under RIG-1310;
nothing here re-opens it.

**Ack timing.** RT-3 says the ack is emitted "on delivery"
(`../compass-0.6/design.md:1461-1463`) without fixing enqueue-time vs
injection-time. Recommended: emit each `delivery_ack` when the message is
**injected at turn end** (or immediately when idle), not at enqueue — the
agent-side queue is memory-only, so an enqueue-time ack would advance the
Server cursor past messages a crashed agent never processed, and the
redelivery sweep exists precisely to close that window. The Runner-hop
`ControlAck` still fires at decode (retention bookkeeping, unchanged). OQ-5,
non-load-bearing.

### D4 — presence: derived from the session lifecycle, no heartbeat

**Presence is a projection of state the pipeline already carries.** The agent
emits lifecycle transitions today — `#emitStatus`
(`compass/packages/compass-agent/src/agent.ts:138-141`) → `SessionFrame.state`
→ the RunnerHub extracts `AgentSessionStatus` onto `SubscribeEvents`
(`runnerhub/hub.go:443-453`) — over the enum
STARTING/READY/WORKING/STOPPED/ERRORED/DISCONNECTED
(`compass/proto/compass/v1/compass.proto:169-180`). A separate heartbeat
would add a liveness protocol the session stream already implies (a dropped
Runner link is DISCONNECTED, `compass.proto:156-160`). MVP presence is a pure
server-side projection, published to the comms fan-out where the channel UI
lives. Presence is **4-state — WORKING / IDLE / WAITING / OFFLINE** —
vocabulary aligned to Cotal's authoritative idle/working/waiting (the
`cotal_status` tool schema, where waiting = blocked on input, approval, or a
peer), plus OFFLINE for no-live-session/link-loss (which Cotal models as
absence/DISCONNECTED) (OQ-1, RATIFIED Matt 2026-07-29):

- `WORKING` ← `AGENT_SESSION_STATE_WORKING` (the agent actively running)
- `IDLE` ← `STARTING` / `READY`
- `WAITING` ← a live session with an open ask: the agent has an authored
  `Ask` with `Ask.answered=false` (`comms.proto:286-296`) in a visible
  channel
- `OFFLINE` ← `STOPPED` / `ERRORED` / `DISCONNECTED` / no recorded session

WAITING is NOT a new `AgentSessionState` enum value — the session enum
(STARTING/READY/WORKING/STOPPED/ERRORED/DISCONNECTED,
`compass.proto:169-180`) has no "waiting". It is a **server-side overlay**:
a comms-layer unanswered-authored-ask query layered on top of the lifecycle
mapping, so presence is a projection of TWO sources — the session lifecycle
(WORKING/IDLE/OFFLINE) plus the unanswered-ask overlay. Precedence, pinned:
**WAITING > IDLE** (a live, READY-but-ask-blocked session shows WAITING, not
IDLE); WORKING is the agent actively running; OFFLINE when no live session.

Carrier: a new `SubscribeCommsResponse` payload variant
`AgentPresenceChanged { string agent_account_id = 1; AgentPresence presence = 2; }`
(additive oneof member beside `MessagePosted`, `comms.proto:383-392`), published
from TWO arms, matching the two-source projection above. The lifecycle arm:
the same hub arm that extracts the lifecycle transition — one source, three
consumers (board state, comms presence, the D1 delivery-settle gate). The
ask arm: the presence publisher also subscribes to the comms bus (the same
bus D1's consumer consumes) and recomputes + publishes presence for the
authoring agent on the **Ask-open** event (a `MessagePosted` carrying an
`Ask`) and the **Ask-answered** event (a `MessageUpdated` flipping
`Ask.answered` true — the server-owned signal, `comms.proto:286-296`),
applying the ratified WAITING > IDLE precedence; the ask overlay flips on
COMMS events, not lifecycle transitions, so the lifecycle arm alone would
never republish a READY agent as WAITING (or back).
Grep confirms no presence symbol
exists in the proto today. **Visibility:** an `AgentPresenceChanged` is
visible to actors who share at least one visible channel with the agent — the
shared-channel rule, matching the comms fan-out's per-actor scoping
(`../compass-0.6/design.md:448-450`), chosen over reusing `AccountChanged`'s
rule because presence is channel-social data, not account data. `ListAccounts`
readers get current presence by snapshot: the hub keeps the last-published
state per agent in memory (presence is ephemeral truth about a live pipeline;
it deliberately has NO durable table). **Restart reconciliation:** a restarted
Server initially knows nothing, and a long-WORKING agent may emit no lifecycle
frame for hours — waiting for the next transition would leave it OFFLINE
unboundedly. At Runner re-enrollment the hub reconstructs presence:
`GetAgentStatus` per live session via the Status relay (`commands.go:117-127`),
seeded from `agent_sessions` liveness, AND re-derives the open-ask overlay
from the comms store (the agent's authored asks with `answered=false` in
visible channels) — `GetAgentStatus` returns only lifecycle state, so
without the comms-store pass a WAITING agent would rebuild as IDLE across a
restart (T8). State vocabulary + carrier: OQ-1,
RATIFIED
(Matt, 2026-07-29 — 4-state, Cotal-aligned, ask-overlay WAITING).

### D5 — server-side mention→steer routing

**Parse at the settle edge, with the client's exact grammar.** The
client grammar is built — `MENTION_RE = /@([a-z0-9][a-z0-9._-]*)/gi`,
`parseMentions` (`compass/apps/ui/src/comms.ts:262-287`), the composer
affordance ("typing `@` is how you reach an agent",
`apps/ui/src/components/ChannelView.tsx:303-308`) — but it is rendering
affordance only; nothing server-side acts on it. The server gains the same
regex (ported to Go, one constant, cross-referenced to `comms.ts:265` so drift
is greppable) applied in the D1 consumer to the SETTLED block set at the
author's turn-settle edge (D1's settle gate; for a human-authored,
non-streaming message, post == settle, so parsing is unchanged in effect).
Parsing at settle, not at the initial `MessagePosted` block set, closes a
real gap: a mention can stream in via a later `MessageUpdated` block
(`comms.proto:399-404`), which post-time parsing would miss entirely. Settle
is strictly after commit, so the property holds: mention routing can never
fail a post.

**Resolution and routing.** Each parsed handle resolves to an account; an agent
account that is a **member** of the channel (subscribe state irrelevant, per the
frozen amendment `../compass-0.6/design.md:454-458`) is routed a `steer` op
carrying the same `Message` (D3). The author exclusion applies here exactly as
it does to deliver (D1): an agent whose own post mentions its own handle — or
posts `@agents` in a channel it is a member of — never steers itself; the
author is excluded from handle resolution AND from reserved-ping expansion (a
T7 test case). Reserved pings (`@agents`, `@users`, `@everyone`) expand to the
matching member sets server-side; every resolved agent gets the steer,
unbounded for MVP (RT-4). An unresolved or non-member handle is a no-op (the
client already renders unresolved mentions as inert chips, `comms.ts:267-271`).

**One message, two paths — the precedence rule.** For a single `MessagePosted`:
the mentioned agent A receives **steer only** — never steer + deliver for the
same message (a double injection of the same content). Any other subscribed
agent B receives the plain `deliver`. A's steer **advances A's delivery cursor
too** — the agent acks the steer's `Message.id` through the same
`delivery_ack`, and because a steer lands mid-turn while earlier delivers may
still sit un-acked in the turn-end queue, the ack marks the steered message in
the cursor's above-set (D2) rather than jumping the contiguous cursor: the
un-acked earlier messages are still swept, and the steered message is not
re-swept. No lost delivery, no double-inject. A mentioned agent with no live
session gets nothing now and picks the message up as a swept `deliver` on next
start — a steer is a mid-turn interrupt by definition; there is no turn to
interrupt. This interaction rule is OQ-3, RATIFIED (Matt, 2026-07-29: steer
only, as recommended). Timing: the author-settle gate (D1) applies to steer
exactly as to deliver — a mid-stream, half-written `@`-mention must not fire
a steer carrying half a message — so **author-settle gates BOTH deliver and
steer**, including the terminal path: a held mention→steer whose author
emits an agent-emitted terminal frame (`STOPPED`/`ERRORED`) mid-hold fires
from the message's stored blocks with the registry entry cleared, exactly as
D1's terminal settle does for deliver — and a no-frame author death falls to
the same recipient reconnect-sweep backstop, no author-side force-fire. The
only deliver-vs-steer difference is
recipient-side: a steer interrupts the recipient mid-turn, a deliver
coalesces to the recipient's turn-end (the 6.2 "wakes/steers NOW" contract
is about the RECIPIENT'S turn,
orthogonal to whether the AUTHOR'S message is settled).

### D6 — Issue/PR notification delivery: the same rail, payload deferred

**The mechanism is D1's, generically.** #995 Decision 5 froze the subscription
model (Server-stored `forge_subscriptions` with a per-subscriber
`delivered_revision` delivery cursor, per-artifact fetch cursor, 60s conditional
polling) and the path ("no new transport": one `SessionsResponse` variant, one
`deliver`-adjacent `AgentControl` variant, `ControlSender.Send`, immediate-
dispatch agent-side). This record supplies the generic rail those legs ride:
D1's Server→Runner control-op relay variant is deliberately typed as
`{session_id, AgentControl}` so a forge notification is *just another op* — the
poller's notify step calls the same dispatch the comms consumer calls, inherits
the same in-band refusal handling, and advances its own per-subscriber cursor
(#995's `delivered_revision`, not D2's `acked_seq` — different cursors for
different sources, same discipline).

**The payload shape is deliberately NOT pinned here.** #995 is frozen as a
record, but its Issue/PR model is unbuilt (no proto/RPC/gen types exist yet)
and its shape is actively forking (compass-ui's "what is an Issue"
reconciliation fork to Matt, unresolved). Pinning `ForgeNotification` fields
now would freeze against a moving target. Deferred as OQ-6, RATIFIED (Matt,
2026-07-29: defer the payload; the mechanism is designed generically now —
load-bearing for the forge leg only; it gates no channel-delivery task).

**Scope note — ask-answer push is a fourth rider, deliberately out of scope.**
The frozen model routes a `RespondToAsk` answer into the session as
`AgentControl.ask_answer` and makes it the wake that ends an idle-waiting
agent (`../compass-0.6/design.md:405-407`, `:418-421`) — the same
Server→Runner→agent rail this record builds. `AskAnswerControl` is already
representable (`agent.proto:136-146`, not a parked shell), so it needs ZERO
proto work — yet grep finds no production sender of `AgentControl_AskAnswer`
(only gen code and a gateway test). It is OUT of Item-6's enumerated scope
(channel delivery / mentions / presence / forge), so this record does not
build it, but the generic `DispatchControl` rail makes it a small follow-up:
a `RespondToAsk` hook calling the same `ControlDispatcher` (T3) — filed as
**RIG-1577** (comms/agent lane owns; Refs RIG-1569), per OQ-8's ratification
(Matt, 2026-07-29).

## Global Constraints

- **RIG-1267 gen fence.** Internal symbols (`AgentFrame`, `AgentControl`,
  `DeliverControl`, `SteerControl`, `DeliveryAck`, …) MUST NOT leak into the
  public gen trees `packages/compass-client/src/gen` or `go/gen`; the fence
  greps for them (`compass/proto/moon.yml:121-151`). `DeliveryAck` and any
  new internal message join the fence's symbol list in the same PR that defines
  them. `AgentPresenceChanged` (D4) is a PUBLIC comms.proto message and rides
  the public lane — it must NOT appear in the fence list.
- **Egress seal.** No new network path out of the agent container. All
  agent-bound traffic rides the frozen per-container Unix socket
  (`AgentGateway.Control`); all Server→Runner traffic rides the existing
  Runner-opened `Sessions`/`PublishEvents` streams (dial-out inversion,
  `runner.proto:57-66`). Nothing in this record opens an inbound route.
- **Frozen-model fidelity.** RT-2/RT-3/the delivery-timing amendment and #995
  Decision 5 are consumed as written; a task that would deviate stops and
  escalates rather than reinterpreting.
- **Proto co-ownership.** Every `agent.proto`/`runner.proto` change here lands
  as part of compass-spec's consolidated RIG-1310 follow-up, coordinated with
  compass-agent — one schema PR, both sides regenerate (`buf generate` all
  three lanes, `moon.yml:50-71`), drift + gen-fence + breaking gates green.
- **Red-green testing.** Every task writes its failing test first
  (BDD/unit), watches it fail, then implements to green.
- **Format/lint gates.** biome for TS, gofmt + golangci-lint for Go,
  markdownlint for this record. `buf lint`/`buf breaking` for proto.
- **Ledger delta.** This record introduces ratified decisions; the PR appends
  DL-071..DL-074 to `DECISIONS.md` (see §Ledger impact) and carries a
  `Ledger-impact:` line. No existing row is flipped or superseded.

## Plan

Order: T1 (proto) gates T3/T4/T5; T2 (cursor store) gates T3; T3 (consumer +
dispatch) gates T5/T6/T7. T8 (presence) is independent of all but T1's public
comms.proto delta.

### T1 — Proto delta: `DeliverControl`/`SteerControl` payloads, `DeliveryAck`, the Sessions relay variant — **shape co-designed by compass-comms + compass-agent; .proto text authored + regenerated by compass-repo (sole proto-tree writer, single buf.gen)**

Populate `DeliverControl { compass.v1.Message message = 1; }` and the
channel-borne `SteerControl` field (same single `Message`); add
`AgentFrame.delivery_ack = 6` carrying `DeliveryAck { string message_id = 1; }`
(the frozen shape, `../compass-0.6/design.md:1426-1428`); add the generic
control-op relay variant to `SessionsResponse.command` — no new SUCCESS
result variant: the success receipt rides `AgentFrame.delivery_ack` per
RT-3, not the runner result stream, but a refusal rides the existing
`RunnerError` result (`runner.proto:151`, `error = 7`), correlated by
`request_id`, which the Server tracks per in-flight dispatch long enough to
correlate a refusal and leave that cursor unadvanced (no-live-session is
cursor-non-advance + redeliver-on-reconnect). All additive. Public
comms.proto side (shape owned here, same schema PR): the
`AgentPresenceChanged` payload variant + `AgentPresence` enum. All three
files T1 touches (`agent.proto`, `runner.proto`, `comms.proto`) ride the
one sole-writer lane and single buf.gen run above — no file is edited by an
independent editor to be discovered at a red drift-gate.

- Interfaces: `agent.proto` — `message DeliverControl { compass.v1.Message
  message = 1; }`; `message DeliveryAck { string message_id = 1; }`;
  `AgentFrame.oneof frame += DeliveryAck delivery_ack = 6` (fields 1-5 taken
  through `control_ack`, `agent.proto:40-68`). `runner.proto` —
  `SessionsResponse.oneof command += DispatchControl deliver_control` with
  `message DispatchControl { string session_id = 1; AgentControl op = 2; }`;
  `deliver_control` takes `command = 11` — the number ASSIGNED by
  compass-repo's canonical oneof allocation (`forge_notification = 7`,
  `secrets_version = 8` RIG-1327, `config_version = 9` RIG-1568,
  `remove = 10` #1019, `deliver_control = 11` RIG-1569 — the block 7-11
  reserved), ratified by compass-repo on `#svc.compass` (2026-07-29) and
  confirmed to this lane by DM. This lane CONSUMES the assigned number; the
  actual `.proto` edit is authored by compass-repo at implementation time
  (the sole-writer lane above). Do NOT read next-free off the live proto:
  the reserved-but-not-yet-in-proto tags are invisible on live main — the
  `runner.proto` command oneof currently stops at `provision = 6` — which
  is exactly why next-free must not be read off the file. No new SUCCESS
  result variant — the success receipt rides `AgentFrame.delivery_ack` per
  RT-3; a refusal rides the existing `RunnerError` result (`error = 7`),
  correlated by `request_id`. `RunnerErrorCode` gains
  `RUNNER_ERROR_CODE_RESOURCE_EXHAUSTED` (additive enum value, buf-safe per
  the DISCONNECTED precedent, `compass.proto:176-180`) so a retention-full
  refusal is distinguishable in-band (T4). `comms.proto` —
  `SubscribeCommsResponse.oneof payload += AgentPresenceChanged
  agent_presence_changed = 17` (16 is `resync_required`, `comms.proto:390`);
  `enum AgentPresence { UNSPECIFIED; IDLE; WORKING; WAITING; OFFLINE }`
  (four states per OQ-1's ratification — still PUBLIC, NOT gen-fenced,
  RIG-1267).
- Gen fence: add `\bDeliveryAck\b` and `\bDispatchControl\b` to
  `moon.yml:151`'s pattern.
- Test cycle: `buf lint` + `buf breaking` + regen all three lanes + drift +
  gen-fence green; a fence unit case proving `DeliveryAck` in a public tree
  reddens.

### T2 — Delivery-cursor store — **compass-comms**

Migration `agent_delivery_cursors` (schema in D2: contiguous `acked_seq` +
bounded `above_seqs`) + store methods. All seq arithmetic is int64 — no uint64
anywhere in store signatures.

- Interfaces: `func (s *Store) AckDelivery(ctx context.Context, agent
  AccountID, channel ChannelID, messageID string) error` (resolves
  message_id → `messages.seq`; a message never dispatched to this agent for
  this channel is a no-op — the resolution IS the overshoot clamp; marks the
  seq, advances the contiguous cursor across gaps that are acked OR not
  owed to this agent (`author_account_id = $agent`, never dispatched),
  retains sparse
  above-cursor seqs); `func (s *Store) SeedDeliveryCursor(ctx, agent, channel)
  error` (seeds `acked_seq` to the subscribe-time channel head in the same
  txn as the `channel_members` row — called from every seed path
  enumerated in D2); `func (s *Store) UndeliveredMessages(ctx, agent
  AccountID) (map[ChannelID][]Message, error)` (the sweep read: per channel,
  `seq > acked_seq AND seq <> ALL (above_seqs)`, author-excluded, ascending
  seq, home channel included).
- Test cycle (red first): a duplicate or reordered ack is a no-op; an ack for
  a message never dispatched to this agent is a no-op (cursor unchanged — the
  overshoot case); gap-fill advances the contiguous cursor and drains the
  above-set; an agent's own post interleaved below un-acked deliveries does
  not wedge the contiguous cursor, and `above_seqs` stays bounded across a
  run of self-posts; an absent cursor row (the legacy fail-safe) sweeps as
  caught-up (no history replay); a subscribe transaction that inserts a
  `channel_members` row also seeds the delivery cursor in the same txn (a
  subscribe that skips seeding is caught, not silently treated as
  caught-up); seed-at-subscribe yields an empty sweep; sweep returns exactly
  the un-acked gap, ordered, above-set excluded; author's own messages
  excluded; store restart survives (pgtest).

### T3 — The fan-out consumer + hub dispatch — **compass-comms**

`internal/delivery`: bus subscription, subscriber resolution (D1's one query,
home-channel disjunct included), per-session ordered dispatch + the sweep-time
dispatch gate (D1 §Ordering), the deliver-on-settled gate (D1's author
split: a pending-deliver registry keyed `(author_session, message_id)`,
fired from the hub's WORKING → READY lifecycle arm re-reading settled
blocks, or on an agent-emitted terminal frame (`STOPPED`/`ERRORED`) from
stored blocks — registry entry cleared on fire, with a no-frame author death
falling to the recipient sweep (D2)),
mention parsing at the settle edge (D5), resync→sweep fallback.
RunnerHub side: the `DispatchControl` relay arm (Server→Runner) and the
`delivery_ack` frame arm (Runner→Server → `AckDelivery` by `message_id`).

- Interfaces: `func NewConsumer(bus *events.Bus, st *store.Store, dispatch
  ControlDispatcher, log *slog.Logger) *Consumer`; `type ControlDispatcher
  interface { DispatchControl(ctx context.Context, sessionID string, op
  *compassv1internal.AgentControl) error }` (implemented by `runnerhub.Hub`
  via the Sessions relay, mirroring `Hub.Start`'s dispatch shape,
  `commands.go:68-84`) — unchanged by the runner-side `agentHost.Deliver`
  facade (T4), which lives behind the relay variant; hub `Deliver` gains
  `case *compassv1internal.AgentFrame_DeliveryAck`.
- Test cycle (red first): a `MessagePosted` on a subscribed channel dispatches
  exactly one deliver per live subscribed agent session, author excluded,
  ascending-seq order per session; an unsubscribed non-home member gets
  nothing; a home-channel row flipped `subscribed=false` still delivers (the
  D1 disjunct); a refused dispatch leaves the cursor unadvanced; a
  `delivery_ack` advances it; live events during a sweep queue behind it;
  bus-lag resync triggers the sweep, not a loss; a human-authored message
  delivers at post; an agent-authored message delivers only after the
  author's turn-settle (WORKING → READY), carrying the settled block
  set; an agent-authored message held at the author's WORKING state whose
  author then emits an `ERRORED` frame is delivered to a live recipient from
  stored blocks without waiting for the recipient to reconnect (and the
  mention→steer equivalent); an agent-authored message held whose author dies
  with no terminal frame (hard-kill / pre-T9 link-loss) is NOT force-delivered
  by an author trigger — the recipient gets it via its own reconnect sweep and
  the held registry entry is reaped on the author's next (re-)enroll; an
  agent-authored message whose author has no live session delivers immediately
  from stored blocks.

### T4 — Runner relay arm — **compass-runner**

The `Sessions` dispatch loop arm: `DispatchControl` →
`agentHost.Deliver(sessionID, op)` — a new facade compass-runner owns, because
the `controlProducer` is per-container, created inside `gateway.Serve` and
owned by `SocketListener` (`socket.go:95`), with no cross-package entrypoint
today. `Deliver` resolves session → container → listener →
`controlProducer.Send` under the host lock (the same resolution `Start`/`Stop`
do, `host.go:232`, `:251`), via a new `SocketListener.SendControl` delegating
to `l.control.Send` (mirroring the `BindSession`/`RetireSession` delegation,
`socket.go:259-275`), exposed up as `runnerhub.Deliver(sessionID, op)`. `Send`
retains until acked, so RT-3's at-least-once redelivery rides it. Refusal
(unknown session / retention full) is returned in-band as the
`request_id`-correlated `SessionsRequest` `error` (`RunnerError`,
`runner.proto:151`), never a stream teardown.

- Interfaces: a new `case *compassv1internal.SessionsResponse_DeliverControl`
  in the Runner's sessions dispatch (beside start/stop/reload/status), calling
  the `agentHost.Deliver(sessionID, op)` facade — NOT
  `gateway.ControlSender.Send` directly.
- Test cycle (red first): a relayed deliver reaches a bound session's control
  stream in order; an unknown session returns `RUNNER_ERROR_CODE_NOT_FOUND`
  in-band; retention-full returns `RUNNER_ERROR_CODE_RESOURCE_EXHAUSTED`
  in-band (the T1 enum addition).

### T5 — Agent-side deliver arm: dedup, queue, coalesce, ack — **compass-agent**

Deliver rides the IMMEDIATE-DISPATCH handle `immediate.deliver(msg)`
(`control-source.ts:153-156`, wired `cli.ts:175-178`) — not a `#applyControl`
iterator arm. (The current `cli.ts:177` wiring
`deliver: (msg) => appendMessage(msg)` is a placeholder that contradicts RT-3
— it seeds context instead of driving a turn-end prompt — replaced here, not
cited as contract.) Implement: deliver → message_id dedup (drop a deliver
whose `Message.id` was already processed this session — the control_seq dedup
at `control-source.ts:316-327` covers only Runner-retention redelivery of the
same retained op, not a server sweep re-issue under a fresh control_seq) →
turn-end coalescing queue → single `prompt` at turn end (immediate when idle)
→ emit one `AgentFrame.delivery_ack { message_id }` per injected message;
channel-borne steer → `agent.steer` with the formatted message, acked the
same way. Replay-barrier refusal semantics unchanged.

- Interfaces: `immediate.deliver(msg)` populated (frozen C4 signature,
  `control-source.ts:153-156`); a coalescing queue + a processed-`Message.id`
  set in `CompassAgent`; ack emission via the existing sink
  (`this.#sink.emit`, `agent.ts:138-141` pattern) as
  `AgentFrame.delivery_ack`.
- Test cycle (red first): mid-turn delivers coalesce into ONE turn-end prompt;
  idle deliver starts a turn immediately; one ack per injected message
  carrying its `message_id`; a swept duplicate under a FRESH control_seq is
  dropped by message_id dedup (no double injection); a crash between enqueue
  and injection re-receives on reconnect (no ack was emitted); steer still
  interrupts mid-turn.

### T6 — The reconnect sweep — **compass-comms**

On session start (post-binding) and Runner re-enroll: `UndeliveredMessages` →
ordered re-dispatch through T3's dispatcher, under D1's per-session dispatch
gate (live bus events for the sweeping session queue behind the sweep).

- Interfaces: `func (c *Consumer) SweepSession(ctx context.Context, sessionID
  string, agent store.AccountID) error`, invoked from the hub's session-start
  promotion path (`relay_comms.go:47-63` call site).
- Test cycle (red first): messages posted while no session was live arrive as
  delivers on next start, in seq order, and are not duplicated after ack
  (agent-side message_id dedup + the contiguous+sparse cursor both asserted);
  a live message posted mid-sweep queues behind the sweep and lands after it
  (the D1 dispatch gate); the replay-barrier assertion (swept delivers held
  until `ReplayCompleteAck`) is GATED on the resume workstream raising
  `HoldForReplay` (`control.go:426-429` — no production caller yet);
  fresh-start sessions need no barrier; a message whose live deliver was
  still HELD (author unsettled) at disconnect is redelivered by the sweep
  from its stored, by-then-settled blocks — a pending-but-unfired deliver is
  indistinguishable to the cursor from an undelivered message (no special
  case).

### T7 — Mention routing — **compass-comms**

D5 in the consumer: Go port of `MENTION_RE`, handle→account resolution,
member-not-subscriber check, author exclusion (self-mention and reserved-ping
expansion), reserved-ping expansion, steer-not-deliver precedence for the
mentioned agent.

- Interfaces: `func parseMentions(text string) []Mention` (Go, one regex
  constant cross-referenced to `apps/ui/src/comms.ts:265`); routing folded
  into the T3 consumer's per-message classification.
- Test cycle (red first): `@agent` member gets steer, not deliver; other
  subscribed agents get deliver; non-member mention is a no-op; a self-mention
  (or the author matching a reserved-ping expansion) is a no-op; `@agents`
  expands to agent members only, author excluded; mention of an agent with no
  live session falls to the sweep; the steer's `message_id` ack lands in the
  cursor's above-set without jumping the contiguous cursor; a mention absent
  from the initial `MessagePosted` block set but streamed in via a later
  `MessageUpdated` block still steers at the author's settle edge (D5).

### T8 — Presence — **compass-comms**

D4's mapping in the hub's lifecycle arm (`hub.go:443-453` call site) +
in-memory last-state per agent + `AgentPresenceChanged` publish onto the comms
bus, visibility-scoped by the shared-channel rule (D4), + the restart
re-enroll reconciliation (D4).

- Interfaces: `func (h *Hub) presenceFor(state compassv1.AgentSessionState,
  openAsk bool) compassv1.AgentPresence` (the `openAsk` overlay input — the
  agent has an authored `Ask` with `Ask.answered=false`,
  `comms.proto:286-296`, in a visible channel; WAITING > IDLE per D4);
  publish beside `lifecycle.PublishSessionStatus`,
  scoped to actors sharing at least one visible channel with the agent; an
  ask-overlay publish arm subscribed to the comms bus, recomputing +
  publishing for the authoring agent on Ask-open (a `MessagePosted`
  carrying an `Ask`) and Ask-answered (a `MessageUpdated` flipping
  `Ask.answered`); a
  `PresenceSnapshot() map[store.AccountID]compassv1.AgentPresence` read for
  account listings; a re-enroll reconciliation pass (`GetAgentStatus` per
  live session via the Status relay, `commands.go:117-127`, seeded from
  `agent_sessions` liveness, plus a comms-store query re-deriving the
  open-ask overlay: the agent's authored asks with `answered=false` in
  visible channels).
- Test cycle (red first): each lifecycle state maps per D4's table;
  a transition publishes exactly one `AgentPresenceChanged`, delivered only
  to shared-channel actors; Server restart followed by Runner re-enrollment
  reconstructs a WORKING agent's presence without waiting for a transition,
  and a WAITING agent's open-ask overlay from the comms store (not IDLE);
  UI stream receives the variant (comms fan-out integration); opening an
  ask with no concurrent lifecycle transition publishes exactly one
  `AgentPresenceChanged` (→ WAITING), and answering it publishes exactly
  one (→ the prior state) — the comms-bus ask arm; so an agent with
  an open unanswered ask projects WAITING (overriding IDLE), and returns to
  IDLE/WORKING once the ask is answered.

### T9 — Forge-notification rail hookup (deferred payload) — **compass-comms, gated on #995 build + OQ-6**

When #995's poller lands: its notify step calls T3's `ControlDispatcher` with
the forge `AgentControl` variant; per-subscriber `delivered_revision` advances
on the same in-band success signal. No work in this record's PR beyond the
generic `DispatchControl` variant (T1) that makes it possible.

- Interfaces: consumed, not defined here — the payload shape is OQ-6.
- Test cycle: owned by the #995 implementation lane.

## Tasks

- [ ] T1 — proto delta (DeliverControl/SteerControl payloads, DeliveryAck,
  DispatchControl relay variant, AgentPresenceChanged) — **shape co-designed
  by compass-comms + compass-agent; .proto text authored + regenerated by
  compass-repo (sole proto-tree writer, single buf.gen)**; gates T3/T4/T5/T8
- [ ] T2 — `agent_delivery_cursors` migration + store methods — **compass-comms**
- [ ] T3 — fan-out consumer + hub dispatch/ack arms — **compass-comms**
- [ ] T4 — Runner `DispatchControl` relay arm → `agentHost.Deliver` facade —
  **compass-runner**
- [ ] T5 — agent deliver arm: message_id dedup → queue → coalesce → prompt →
  `delivery_ack` — **compass-agent**
- [ ] T6 — reconnect/start redelivery sweep — **compass-comms**
- [ ] T7 — server-side mention→steer routing — **compass-comms**
- [ ] T8 — presence mapping + comms fan-out — **compass-comms**
- [ ] T9 — forge-notification rail hookup — **compass-comms**, gated on #995
  build + OQ-6

## Open Questions

Batched for Matt and RULED in one batched ask (Matt, 2026-07-29). All six
ruled questions below (OQ-1, OQ-3, OQ-4, OQ-6, OQ-7, OQ-8) are RATIFIED and
folded into the named sections; OQ-2 was resolved with the co-owner; OQ-5 is
non-load-bearing and merges on the recommendation. No open fork remains.

- **OQ-1 (load-bearing) — presence model shape. RATIFIED (Matt, 2026-07-29):
  4-state, Cotal-aligned** — a DEPARTURE from the recommended three-state
  mapping. Matt chose alternative (a): a fourth WAITING state ("agent
  blocked on an unanswered ask"), with the vocabulary aligned to Cotal's
  authoritative idle/working/waiting (+ OFFLINE): **WORKING / IDLE / WAITING
  / OFFLINE**. The honest-cost assessment held: ask state is ALREADY
  comms-layer data (asks are durable comms rows with a store-maintained
  Answered flag on the wire, `ask_mapping_test.go:155-159`; the
  answered-signal is `Ask.answered`, `comms.proto:286-296` — correcting this
  OQ's earlier imprecise `comms.proto:260-268` cite, which pointed at
  `MessageBlock`), so WAITING is one server-side overlay query + one enum
  value, not new plumbing — NOT a new `AgentSessionState` value. Heartbeat
  alternative (b) stays rejected: duplicates liveness the `Sessions`/control
  streams already prove, and DISCONNECTED already covers link loss. Folded
  into D4 (two-source projection, WAITING > IDLE precedence), T1 (4-value
  `AgentPresence`), T8 (overlay input + WAITING test).
- **OQ-2 (RESOLVED, documented) — `DeliverControl` payload: full
  `compass.v1.Message`.** Resolved by the compass-agent co-owner, matching
  this record's recommendation: full first-party `Message`, agent formats
  from structured fields (server fans structured Messages; agent renders,
  coalesces, acks). Deliver un-parks independently of steer/config —
  RIG-1310 §1's opaque-SDK parking applies to steer+config, not deliver. No
  Matt ask needed; the record's freeze ratifies. Kept here as a documented
  resolution, not an open fork.
- **OQ-3 (load-bearing) — mention-vs-deliver interaction. RATIFIED (Matt,
  2026-07-29): steer ONLY, as recommended; no mechanism change.** The
  mentioned agent gets steer ONLY (never steer + deliver of the same
  message); the steer's `message_id` ack lands in the delivery cursor's
  above-set WITHOUT jumping the contiguous cursor past un-acked earlier
  delivers still in the agent's turn-end queue (D2/D5 — no lost delivery, no
  double-inject); other subscribed agents get plain deliver; a mentioned
  agent with no live session falls to the sweep-as-deliver. Alternative
  (steer AND deliver to the mentioned agent) double-injects content into one
  session. Blocks merge: it fixes agent-visible semantics of every mention.
- **OQ-4 (load-bearing, UPGRADED) — cursor keying: per-(agent, channel)
  durable vs RT-3's literal "per-session". RATIFIED (Matt, 2026-07-29):
  per-(agent, channel) durable, amending RT-3's "per-session" wording.**
  RT-3's frozen text says "the
  **Server** tracks **per-session** delivery from those acks"
  (`../compass-0.6/design.md:1461-1464`) — and this record re-keys the cursor
  to `(agent_account_id, channel_id)`, a frozen-WORDING departure that was
  Matt's to ratify, not this record's to self-rule — and he has. The re-key
  is well-motivated: compass-0.6 also calls the cursor "durable" (`:1467`),
  containers are stateless and replaced routinely (#995 Decision 5's
  per-subscriber precedent), and a strict per-session-id cursor loses the
  delivery gap across replacement. The ratified reading: "per-session" as
  "per-session-worth-of-subscriptions", i.e. the durable account keying —
  presented as an amendment-by-citation, now ratified. (The cursor SHAPE —
  contiguous low-water + bounded above-set, D2 — is a driver-fix under
  either keying and was not part of this question.)
- **OQ-5 (non-load-bearing) — `delivery_ack` timing.** Recommended:
  injection-time (turn-end / idle-immediate), not enqueue-time (D3 §Ack
  timing) — RT-3's text does not fix it, and injection-time is the
  crash-safe reading. May merge on the recommendation.
- **OQ-6 (load-bearing for the forge leg only) — Issue/PR notification
  payload shape. RATIFIED (Matt, 2026-07-29): defer the payload; design the
  mechanism generically now — the deferral is confirmed, no change.**
  Deferred by design: #995's Issue/PR model is unbuilt (no
  proto/RPC/gen types yet) and its "what is an Issue" shape is an unresolved
  fork with compass-ui before Matt. D6 designs the mechanism generically (the
  `DispatchControl` rail); the payload pins only after #995's Issue model is
  built and its shape ruled. Gates T9 only — no channel-delivery task waits on
  it.
- **OQ-7 (load-bearing, NEW) — deliver-on-posted vs deliver-on-settled for
  streaming messages. RATIFIED (Matt, 2026-07-29): deliver-on-SETTLED —
  option (b), OVERRULING this record's on-posted recommendation** ("needs to
  wait to deliver to agents until the full message has posted"). The problem
  stands as stated: agent-authored messages STREAM — the row is posted with
  an initial block set and grows via `MessageUpdated` (`agent.proto:44-48`;
  `comms.proto:401-403`; `publishMessageUpdated` exists,
  `mapping.go:375-381`) — so an on-posted deliver hands a subscriber a
  snapshot of a half-finished turn. No wire settle flag exists, so the
  settle signal is the author's turn-lifecycle edge to a SETTLED state —
  WORKING → READY (`agent_end`) on normal turn-end, or an agent-emitted
  terminal frame (STOPPED/ERRORED) on abnormal end, delivering held sets from
  stored blocks (a no-frame author death falls to the recipient reconnect
  sweep); human-authored messages settle at post; an author with no
  live session delivers immediately from stored blocks. Folded into D1 (the
  settle-gate
  trigger + author split + D2-cursor interaction), D5 (author-settle gates
  steer too; mention parsing moves to the settle edge), T3/T6 (tests).
  Forward note, scoped OUT: Matt is separately considering a2a channel
  mutexes/locks (an "… is typing" equivalent); not designed here.
- **OQ-8 (NEW) — ask-answer push ownership. RATIFIED (Matt, 2026-07-29):
  separate follow-up, filed and owned — RIG-1577.** The `RespondToAsk` →
  `AgentControl.ask_answer` wake (see D6's scope note) rides the exact rail
  this record builds and needs zero proto work, but is outside Item-6's
  enumerated scope (channel delivery / mentions / presence / forge). It is a
  separate small follow-up — a `RespondToAsk` hook calling T3's
  `ControlDispatcher` — owned by the comms/agent lane, filed as **RIG-1577**
  (team SEA, project Compass, P2, Refs RIG-1569).

## Ledger impact

`Ledger-impact:` this PR appends four rows to
`docs/designs/product/DECISIONS.md` (claimed DL-071..074 above the current
declared reserved max of DL-070, per the DL single-writer convention — the
comms lane's assigned contiguous block):

- **DL-071** — Channel→agent delivery: a Server-side comms-bus consumer
  dispatches `deliver`/`steer` ops through the RunnerHub over a generic
  `SessionsResponse.DispatchControl` relay variant onto the built control lane
  (D1); agent-authored (streaming) messages are HELD and delivered at the
  author's turn-settle edge (WORKING → READY/IDLE), human-authored at post
  (OQ-7, Matt 2026-07-29).
- **DL-072** — The durable delivery cursor is Server-owned: a contiguous
  low-water cursor plus a bounded above-cursor set per
  `(agent_account_id, channel_id)` on `messages.seq` (mirroring `ControlAck`'s
  `acked_seq` + `applied_above`, `agent.proto:163-169`), reconstructed from
  per-message acks, swept gap-aware on session start/reconnect (D2). The
  per-(agent, channel) KEYING is ratified (Matt, 2026-07-29), amending
  RT-3's "per-session" wording.
- **DL-073** — `DeliverControl` (and channel-borne `SteerControl`) carry the
  full first-party `compass.v1.Message` and nothing else (no `channel_seq`);
  the agent acks per message via `AgentFrame.delivery_ack { message_id }` —
  the frozen ack shape (D3).
- **DL-074** — MVP presence is 4-state, Cotal-aligned
  (WORKING/IDLE/WAITING/OFFLINE): WORKING/IDLE/OFFLINE derived from the
  agent-session lifecycle, WAITING a server-side unanswered-ask overlay (an
  authored `Ask` with `Ask.answered=false` in a visible channel; WAITING >
  IDLE), no heartbeat, published as an additive `SubscribeCommsResponse`
  variant visible to actors sharing at least one visible channel with the
  agent, in-memory only with re-enroll reconciliation (D4, OQ-1 Matt
  2026-07-29).

No existing row is flipped or superseded.
