# RIG-2490 — Close the pre-settle mention-loss window (sibling to RIG-1641)

Status: Draft

Tracker: RIG-2490.

Sibling follow-up to: `compass-mention-offline-redelivery.md` (RIG-1641) — per
the sealed frozen-record convention this record ADDS a sibling; the merged
RIG-1641 record is never edited in place. All file+line grounding below was
verified against the working tree this run (jj workspace off main `48d4a7cd`).

## Problem / Intent

RIG-1641 made an offline mention durable **at the settle edge**: when
`routeMentions` resolves a mentioned member with no live session that is
outside the D2 sweep set (unsubscribed + non-home + non-mandatory — the
"mention-gap population"), it writes a durable `owed_mentions` row before any
wake attempt (`go/internal/delivery/dispatch.go:158-162`):

```go
} else if !inSweep {
    if err := c.st.RecordOwedMention(ctx, agent, channel, msg.GetId()); err != nil {
```

But durability only BEGINS there. `routeMentions` runs inside the delivery
consumer's in-memory bus-tail loop (`fanOut` calls it at
`dispatch.go:100`: `mentioned := c.routeMentions(ctx, channel, author, msg)`),
and the post path commits the store row BEFORE publishing the in-memory bus
event (`go/internal/comms/comms.go:361-375`: `c.store.AppendMessage(…)` then
`if inserted { c.publishMessagePosted(msg) }`, where `publishMessagePosted` is
a plain `c.bus.Publish` — `go/internal/comms/mapping.go:443-449`). Two windows
before the settle edge therefore lose a mention for the mention-gap
population with NO owed row ever written:

1. **Crash window.** Server crash after the message-post commit but before
   the consumer processes `MessagePosted` (`consumer.go:337-343`
   `handleEvent` → `onMessagePosted`) — or, for an agent-authored message
   HELD at the author's live session (`dispatch.go:75`: `c.hold(authorSession,
   messageID)`), before the settle edge fires. The bus is in-process; after a
   restart the event is gone and `routeMentions` never runs for that message.
2. **Bus-overrun window.** On overrun the consumer's `sub.Lagged()` branch
   re-subscribes and runs `sweepAllLive` (`consumer.go:279-320`: "Overrun: bus
   events were dropped. Not a loss — the cursor defines exactly what is
   undelivered, so RE-SUBSCRIBE, then sweep every live recipient"). That sweep
   is `sweepSession` → `UndeliveredMessages` (`settle.go:309-310`), whose
   query is subscription-gated (`go/internal/store/delivery_cursors.go:404`:
   `AND (cm.subscribed OR cm.channel_id = aa.home_channel_id OR
   ch.mandatory_subscription)`) — a mention in a dropped bus event is never
   re-parsed, and the mention-gap population is by definition outside that
   gate.

The subscribed/home/mandatory population has the cursor sweep — computed from
durable state — as its backstop in both windows; the mention-gap population
has none. RIG-1641 accepted this explicitly (frozen record §Decisions OQ-5,
`compass-mention-offline-redelivery.md:715-722`: "accept the residual
pre-settle window for MVP […] tracked as a follow-up: RIG-2490") and scoped
its no-loss invariant at the settle edge (frozen record :413-422).

Intent: extend the no-loss invariant from the settle edge back to the
message-post COMMIT — a committed mention of a channel agent member is never
silently dropped, even across a server crash or a bus overrun that swallows
the `MessagePosted` event before `routeMentions` sees it.

## Approach

**Chosen (ruled by Matt after the design red-team): a per-message durable
marker — `messages.mentions_routed_at BIGINT NULL` — set by the delivery
consumer when a message's settle-edge mention pass completes, and a recovery
scan over `WHERE mentions_routed_at IS NULL` at the consumer's two recovery
points.** No new table, no persisted high-water cursor, no persisted
held-floor arithmetic (the scan's batch loop keeps only a scan-local,
per-invocation seq floor for termination). (The
fork this replaces — a global `mention_scan_cursor` high-water over
`messages.seq` — was killed for a commit-visibility hazard; the full ruling is
recorded in §Resolved decisions RD-1.)

The insight that picks the mechanism: both loss windows are *recovery-time*
problems. The consumer's live pass is already correct — while the process is
up and the bus intact, every committed message reaches `routeMentions` at its
settle edge and the owed row lands. What is missing is the mention analogue
of what the cursor sweep already is for the subscribed population: a way to
recompute, FROM DURABLE STATE, what the in-memory pass would have produced.
The durable state that captures "the settle-edge mention pass ran for this
message" is attached to the message row itself:

- **The marker.** `mentions_routed_at BIGINT NULL` on `messages`
  (`0001_init.sql:222-232`), unix milliseconds — matching the table's own
  `at_unix_ms BIGINT NOT NULL` (`0001_init.sql:225`) and `owed_mentions`'
  `recorded_at_unix_ms BIGINT NOT NULL` (`0001_init.sql:495`); NOT a SQL
  TIMESTAMP. NULL means the settle-edge mention pass has never completed for
  this row; the contract is NULL vs non-NULL (no reader orders on the value —
  the timestamp is observability). The consumer sets it right after
  `routeMentions` completes inside `fanOut` (`dispatch.go:100`: `mentioned :=
  c.routeMentions(ctx, channel, author, msg)`), which is the settle edge for
  all three dispatch paths (human post, dead-author, `fireHeld`).
- **The scan.** At the two points where the in-memory pass is known to have
  holes, read committed messages `WHERE mentions_routed_at IS NULL`, ascending
  seq, batched, and for each one replay the settle-edge mention pass against
  *current* durable state, then set the marker:
  - **consumer start** (`Run`, `consumer.go:243` — beside the existing
    owed-backlog visibility count at `consumer.go:257-261`), covering the
    crash window; and
  - **the `Lagged()` overrun branch** (`consumer.go:281`), beside the existing
    `c.sweepAllLive(ctx)` call (`consumer.go:320`), covering the bus-overrun
    window.

**Why a marker and not a high-water cursor (RD-1 summary).** `messages.seq`
is BIGSERIAL assigned at INSERT time (`0001_init.sql:226`), but the bus event
publishes only after the post txn commits (`comms.go:361-375`:
`c.store.AppendMessage(…)` then `if inserted { c.publishMessagePosted(msg) }`)
— so seq order is NOT commit-visibility order. A high-water advanced past an
in-flight lower seq (a txn still uncommitted when a higher-seq message is
processed) silently excludes that message from every future scan once it
commits; if its bus event then dies in a crash, the mention is permanently
lost — a NEW loss mode introduced by the closure mechanism itself. The marker
carries no PERSISTED ordering state: recovery state lives on the row, a row is
visible to the scan exactly when its commit is visible, and an unprocessed
row is simply still NULL. (The scan's batch loop keeps a scan-local, per-
invocation seq floor for termination only — never persisted, reset to 0 each
recovery point — so it cannot exclude a late-committing row.) The hazard
cannot be expressed.

**The per-message pass** (shared with the live path — T2 factors it):

1. Parse handles with the EXACT parser the live path uses — `mentionHandles`
   over the message's blocks (`consumer.go:439-456`), which folds
   `parseMentions`/`mentionRE` (`consumer.go:403,417`). The scan re-reads the
   store row and maps it through the one store→wire mapper exactly as the
   dead-author and sweep paths already do (`storeMessageToWire`,
   `consumer.go:477-487`), so the parser input type is the same
   `*compassv1.Message` — no second grammar, no divergence.
2. Resolve the mentioned member set with the same `resolveMentioned`
   (`dispatch.go:184`: reserved-ping expansion + `ChannelAgentMembers` +
   `AgentByHandle`, author excluded).
3. For each mentioned member, run the same arms `routeMentions` runs
   (`dispatch.go:145-170`): live → steer directly (`dispatch.go:146-149`:
   `if live { c.dispatchSteerTo(…) }`); offline and outside the sweep set
   (`InSweepSet`, `delivery_cursors.go:424` — the disjunct that "mirrors
   UndeliveredMessages EXACTLY so the two never drift") →
   `RecordOwedMention` (`delivery_cursors.go:90` — idempotent: `ON CONFLICT
   (agent_account_id, message_id) DO NOTHING`) then wake; offline in the
   sweep set → wake only (the cursor sweep is its durable backstop).
   Concretely the scan calls the same `routeMentions` body — T2 factors the
   per-message routine (`routeMentionsFor`) so the live path and the scan
   share one function, not two similar ones.
4. **Held skip — keyed on `c.held` membership, never author-liveness.** The
   scan SKIPS a message iff its `message_id` is currently registered in the
   consumer's held-deliver registry (`c.held`, a
   `map[string][]string` of author-session → ordered held message ids,
   `consumer.go:150-171,220`; populated by `hold`, `dispatch.go:81-85`) —
   those are the messages the live settle path genuinely owns: their settle
   edge WILL fire (`fireHeld` drains exactly the registered ids,
   `settle.go:258-263`) and mark them. Author-liveness is the WRONG proxy: an
   agent-authored message dropped by a bus overrun never entered `c.held`
   (`handleEvent` never ran for it, `consumer.go:337-343`), so no settle edge
   ever fires for it — yet its author session may still be live; a
   liveness-keyed skip would exclude it forever, leaving it owned by NEITHER
   path. Under the marker there is nothing to pin: a skipped held message
   simply keeps `mentions_routed_at NULL` and re-enters the scan set at every
   recovery point until its settle pass marks it. After a crash the process
   restarts with an empty `c.held`, so every committed unmarked message is
   scannable — mirroring the existing dead-author deliver rule
   (`dispatch.go:64-73`).

**One surface, full body, spam-free.** The scan replays the FULL shared
`routeMentions` body — including the live-steer arm — not a subset. A
scoped-down replay would reintroduce the second-correctness-surface problem
this record's core principle forbids, and the live arm is load-bearing: a
gap-population member (unsubscribed + non-home + non-mandatory) that is
currently LIVE but whose `MessagePosted` was dropped in an overrun gets its
steer only from this arm (`dispatch.go:146-149`) — no owed row is due (it is
not offline) and no sweep covers it. The re-steer-spam concern the red-team
raised against the cursor design DISSOLVES here: the scan sees only messages
whose settle-edge pass never completed (`mentions_routed_at IS NULL`); an
already-routed message is marked and structurally excluded, so the scan never
re-processes a handled mention. The only residual at-least-once window is a
crash between processing a message and setting its marker: the next scan
reprocesses it, and the duplicate effects are absorbed by the existing dedup
layers — the agent-side per-session message_id dedup for a duplicate steer
(frozen record :423-427; populated within any live session) and
`RecordOwedMention`'s PK upsert (`delivery_cursors.go:92-94`). This is
exactly the existing at-least-once delivery contract, never at-most-once.

**First deploy: seed forward, no historical backfill (RD-2).** Existing rows
are seeded non-NULL at migration time so the first scan sees nothing; new
rows insert with the column NULL and are marked by their settle pass. Compass
is pre-live (no meaningful history), so this is stated
correct-by-construction rather than load-bearing — see RD-2 and T1.

**What the scan does NOT cover, on purpose.** The deliver arm (offline
subscribed recipients) needs nothing here: its durable backstop in both
windows is the cursor sweep itself (`UndeliveredMessages` is computed from
durable state; `sweepAllLive` on overrun, `sweepSession` on start), and its
wake is pure latency (frozen record §Decisions OQ-3/OQ-6). The scan is scoped
to mentions because mentions are the only signal derived from the in-memory
event rather than from durable state.

**Why not a same-txn owed-row write at post (the rejected Option B).** It
covers only human-authored messages — an agent-authored message's mention set
is not final at post (blocks stream in via `MessageUpdated`, settling later;
`consumer.go:333-336`: "a MessageUpdated grows an agent-authored message's
block set but is NOT itself a trigger"), which is the very reason the settle
split exists — so it leaves the agent-authored class exposed in BOTH windows,
moves mention parsing and sweep-set policy into the post path (a layer
inversion: the parse lives in delivery, `consumer.go:394-456`), and adds
per-post write amplification. The marker closes both windows for both author
classes with one mechanism at the two points recovery already happens. (Full
fork record: RD-1.)

**End-to-end flow (recovery pass):**

```mermaid
flowchart TD
    A[consumer start / Lagged re-subscribe] --> B[read messages WHERE mentions_routed_at IS NULL, ascending seq, batched]
    B --> C{message_id in c.held?}
    C -->|yes: live settle path owns it| D[skip; stays NULL until its settle pass marks it]
    C -->|no| E[shared routeMentionsFor: parse + resolve + per-member arms]
    E -->|live member| F[steer now; per-session dedup absorbs dupes]
    E -->|offline, outside sweep set| G[RecordOwedMention - idempotent - then wake]
    E -->|offline, in sweep set| H[wake only; cursor sweep is the backstop]
    E --> I[set mentions_routed_at]
```

## Global Constraints

- **Module:** `github.com/RigelBuild/compass/go`. Owner lane: every task below
  lands in `compass-comms` (delivery/store), red-green (test first, watch it
  fail, then implement). No-retries rule applies to every test.
- **Schema edits ONLY in `0001_init.sql`** — the store's single squashed
  pre-dogfood migration (`//go:embed migrations/*.sql`; `owed_mentions`
  precedent at `0001_init.sql:491-497`). The one new schema element is the
  nullable marker column `messages.mentions_routed_at BIGINT` plus its
  partial scan index (T1) — no new table. Reuse `owed_mentions` + its store
  methods (`RecordOwedMention` `delivery_cursors.go:90`, `OwedMentions`
  `:108`, `InSweepSet` `:424`, `ClearOwedMention` `:180`, `CountOwedMentions`
  `:193`) unchanged. Unix-ms BIGINT per the schema's existing convention
  (`at_unix_ms` `0001_init.sql:225`, `recorded_at_unix_ms` `:495`) — never a
  SQL TIMESTAMP.
- **One mention grammar.** The scan MUST reuse `mentionHandles` /
  `parseMentions` / `mentionRE` (`consumer.go:394-456`) and
  `resolveMentioned` (`dispatch.go:184`) via the shared `routeMentionsFor`
  body — a second parse or membership-resolution path is a second correctness
  surface and is prohibited. Same for the sweep-set predicate: `InSweepSet`
  only (`delivery_cursors.go:423`: "The disjunct mirrors UndeliveredMessages
  EXACTLY so the two never drift").
- **At-least-once, never at-most-once.** A message is marked only AFTER its
  mention pass completes; a fault in between leaves it NULL and re-scannable.
  Duplicate effects are absorbed by the existing dedup layers
  (`RecordOwedMention`'s PK upsert `delivery_cursors.go:92-94`; agent-side
  per-session message_id dedup, frozen record :423-427). No mechanism here
  may trade loss for dedup — in particular, never mark before the pass runs.
- **Never fail the post / the consumer.** The scan is recovery machinery: a
  scan error is logged loudly and the consumer proceeds (today's behavior
  minus the closure) — it must not wedge `Run` (`consumer.go:243`) or turn a
  recoverable overrun into a crash loop. Frozen "mention routing can never
  fail a post" holds unchanged.
- **Tests:** pgtest, `-tags pgtest` (+`unix` for delivery/server), DSN
  `postgres://postgres:compass-test@localhost:33970/compass?sslmode=disable`.
  The acceptance test (from the issue) is a pgtest that PROVABLY severs the
  bus event (fresh-bus construction, T4 — never a timing assumption) and
  still delivers on next start.
- **Frozen contracts untouched:** steer-only precedence, the D2 cursor shape,
  `AckDelivery`'s restructured owed-clear (`delivery_cursors.go:228-258`),
  the `AgentWaker` seam (`consumer.go:232-234`), and everything RIG-1641
  froze. This record adds a recovery pass plus one column write on the live
  settle edge; it changes no live-path delivery semantics.

## Plan

**T1 — the marker column + store surface.** Add to the `messages` DDL
(`0001_init.sql:222-232`) a nullable `mentions_routed_at BIGINT` (unix ms;
NULL = settle-edge mention pass never completed), plus the partial scan
index:

  ```sql
  CREATE INDEX messages_mentions_unrouted_idx
      ON messages (seq)
      WHERE mentions_routed_at IS NULL;
  ```

  The partial index is the right predicate support: the scan's only query
  shape is `WHERE mentions_routed_at IS NULL ORDER BY seq ASC LIMIT n`, which
  this index serves for both the filter and the order; in steady state nearly
  every row is marked, so the index holds only the thin in-flight/unsettled
  set. None of the existing messages indexes serve it
  (`messages_topic_seq_idx` leads on `topic_id` `0001_init.sql:236`; the GIN
  and the partial-unique idempotency index cover other predicates). Plain
  (non-CONCURRENT) index, matching the file's in-transaction migration
  constraint (`0001_init.sql:255-257`). Seed-forward (RD-2): because the
  squashed `0001_init.sql` is the only migration and pre-dogfood databases
  are recreated on schema change, every row that exists after migration was
  inserted with the column present — there are no pre-column rows to
  backfill. Record the intent anyway (for the first real incremental
  migration, if one ever carries this): existing rows would be seeded
  `mentions_routed_at = <migration time>` so a first scan sees nothing; no
  historical backfill-from-zero. Store methods beside the owed-mention family
  in `delivery_cursors.go`. Lane: compass-comms.

  Interfaces:

  ```go
  // store
  // MarkMentionsRouted stamps messageID's settle-edge mention pass complete
  // (mentions_routed_at = now, unix ms). Idempotent: a re-mark overwrites the
  // timestamp; the contract readers rely on is NULL vs non-NULL only.
  func (s *Store) MarkMentionsRouted(ctx context.Context, messageID string) error
  // UnroutedMentionMessages returns committed messages whose settle-edge
  // mention pass never completed (mentions_routed_at IS NULL) AND whose seq is
  // > afterSeq, ascending seq, each with its channel resolved through
  // topics.channel_id — the scan read. limit bounds one batch so a long-idle
  // deploy cannot hold the whole backlog in memory; the caller loops,
  // advancing afterSeq to the last returned seq, until a batch is short.
  // afterSeq is a scan-LOCAL cursor (start each recovery scan at 0), never
  // persisted — a held row skipped by the caller stays NULL and is re-scanned
  // from 0 at the next recovery point, so this is not the killed high-water.
  func (s *Store) UnroutedMentionMessages(ctx context.Context, afterSeq int64, limit int) ([]MessageWithChannel, error)
  type MessageWithChannel struct {
      Message
      Channel ChannelID
      Seq     int64
  }
  ```

  Red-green (pgtest): a fresh insert has a NULL marker and is returned by
  `UnroutedMentionMessages` (ascending seq > `afterSeq`, right channel); after
  `MarkMentionsRouted` it is excluded; re-mark is a no-op at the contract
  level; `limit` bounds one batch and a follow-up read with `afterSeq` = the
  last returned seq excludes the already-read prefix (the batch-walk
  termination contract). Lane: compass-comms.

**T2 — factor the per-message mention routine; the scan itself.** Extract the
body of `routeMentions`' per-member loop (`dispatch.go:145-170` — the
live-steer / record-then-wake / wake-only arms) into `routeMentionsFor`, a
routine both the live settle path and the scan call, so there is exactly one
implementation of the mention policy. Add `scanMissedMentions` to the
consumer: loop `UnroutedMentionMessages(ctx, afterSeq, limit)` batches,
advancing a scan-LOCAL `afterSeq` to the last returned row's seq each batch
and terminating when a batch comes back short; per message — skip iff its
`message_id` is registered in `c.held` (membership over the
author-session-keyed registry, `consumer.go:150-171`, read under `c.mu`; the
skipped message stays NULL and is re-scanned at the NEXT recovery invocation —
the scan-local `afterSeq` floor steps past it WITHIN this invocation, so a
held tail can never re-fill the same batch and hang the loop, but nothing is
persisted so the next recovery point re-scans from seq 0), else map through
`storeMessageToWire` (`consumer.go:477`), run the shared routine, then
`MarkMentionsRouted`. Errors: a per-message fault is logged loud and the
message left UNMARKED (rescanned at the next recovery point), then the scan
continues; a batch-read fault stops the current scan. Lane: compass-comms.

  Interfaces:

  ```go
  // delivery — the factored per-message mention pass, shared verbatim by the
  // live settle path (routeMentions) and the recovery scan. Returns the
  // mentioned set exactly as routeMentions does today.
  func (c *Consumer) routeMentionsFor(ctx context.Context, channel store.ChannelID, author store.AccountID, msg *compassv1.Message) map[store.AccountID]bool
  // delivery — the recovery scan (T2). Replays the mention pass over
  // committed messages whose marker is NULL (skipping messages currently in
  // c.held), marking each processed message. Never returns an error to Run;
  // faults are logged and the affected messages stay NULL for the next
  // recovery point.
  func (c *Consumer) scanMissedMentions(ctx context.Context)
  ```

  `DeliveryReads` (`consumer.go:63`) gains `MarkMentionsRouted` +
  `UnroutedMentionMessages`.
  Red-green (consumer test, fake store/resolver): unmarked message with an
  offline out-of-sweep-set mentioned member ⇒ owed row recorded + wake +
  message marked; unmarked message whose id is in `c.held` ⇒ skipped and NOT
  marked; unmarked message with a LIVE mentioned gap-population member ⇒
  steered (the live arm survives the factoring); per-message fault ⇒ that
  message unmarked, scan continues; re-scan of an already-recorded mention ⇒
  idempotent no-op. Lane: compass-comms.

**T3 — wire the scan at both recovery points; mark on the live path.** Call
`scanMissedMentions` (a) in `Run` at start, beside the owed-backlog count
(`consumer.go:255-261`), BEFORE draining `sub.Replay` (`consumer.go:263-270`)
— the scan covers the committed-but-unmarked set from durable state while the
replay+live tail covers the rest, mirroring the subscribe-first/sweep-second
seam-closure ordering already documented at `consumer.go:288-302`; and (b) in
the `Lagged()` branch after the fresh subscribe, beside `c.sweepAllLive(ctx)`
(`consumer.go:318-320`). The overlap (a message both scanned and
replayed/delivered live) is the tolerated at-least-once boundary set,
absorbed exactly as the existing boundary double-deliver is
(`consumer.go:298-302`). Also mark on the LIVE path: in `fanOut`, right after
`c.routeMentions(…)` returns (`dispatch.go:100`), call `MarkMentionsRouted`
for the message — one call site covers all three settle-edge callers (human
post `dispatch.go:55`, dead-author `dispatch.go:72`, `fireHeld`
`settle.go:263-271`) — so the steady-state scan set at the next recovery
point is near-empty. A held message is marked only when its settle edge fires
`fanOut`; until then it stays NULL (and is scan-skipped via `c.held`). Lane:
compass-comms.

  Interfaces: none new — wiring over T1/T2 surfaces.
  Red-green (consumer test): committed unmarked mention with the bus event
  ABSENT ⇒ owed row exists after Run's start scan; overrun path (forced
  Lagged) with a dropped mention event ⇒ owed row exists after recovery; live
  settle pass marks the message (a subsequent scan processes nothing).
  Lane: compass-comms.

**T4 — the acceptance pgtest.** End-to-end pgtest (`-tags pgtest,unix`), four
legs, no retries — determinism comes from CONSTRUCTION, never timing:

- **Crash leg.** Post a message mentioning an offline, unsubscribed,
  non-home, non-mandatory agent member; then construct the consumer over
  the SAME store but a FRESH `events.Bus` that never saw the publish (or,
  equivalently, append via the store without publishing). This is the pinned
  HOW of the event severance: merely "starting the consumer after the post"
  does NOT drop the event, because `Run` subscribes at `sinceSeq=0`
  (`consumer.go:244`), `Subscribe(0, …)` "replays the whole retained ring"
  (`events.go:212-214`, ring retains up to 1024 events, `events.go:22-24`),
  and `Run` drains `sub.Replay` before the live loop
  (`consumer.go:263-270`) — the fresh bus makes the replay empty by
  construction. Start the consumer ⇒ the owed row is materialized by the
  start scan; simulate the member's session start ⇒ the mention arrives as
  exactly one STEER (`sweepOwedMentions`, `settle.go:209`); ack ⇒ row
  cleared (`AckDelivery`'s owed-clear, `delivery_cursors.go:255`), second
  start sweeps nothing.
- **Negative-control leg.** The identical fresh-bus setup with the scan
  absent/disabled ⇒ NO owed row exists — the red half of red-green,
  proving the leg cannot green with the scan unimplemented.
- **Agent-authored leg.** An agent-authored mention posted while its author
  session is live is HELD (`dispatch.go:75`); restart before settle (fresh
  bus + fresh consumer: no live author session survives, `c.held` is empty,
  so the message is scannable) ⇒ the start scan recovers the mention.
- **Lagged-overrun leg.** Force the `Lagged()` overrun with the mention in
  the dropped window ⇒ owed row exists after the overrun-branch scan.

  Interfaces: none new — behavioral cycle over T1-T3. Lane: compass-comms.

**T5 — ledger delta + tracker (called out for the driver; NOT performed by
this design pass).** In the SAME PR that freezes this record: (a) append the
closure decision row(s) to `docs/designs/product/DECISIONS.md` (next free
DL id) recording the ruled marker mechanism and the no-loss invariant's new
scope (post-commit, not settle-edge); (b) annotate the RIG-1641 OQ-5
acceptance — the DL-226 row's "residual pre-settle window is accepted for MVP
(RIG-2490)" clause — as closed by this record (amend the Status cell by
citation, per the ledger's amend-not-flip convention). The PR description
carries a `Ledger-impact:` line; `tools/design-ledger-gate` enforces the
coupling. Lane: compass-comms (driver wires at submit).

## Tasks

- [ ] T1 — `messages.mentions_routed_at BIGINT NULL` marker column + partial
      scan index in `0001_init.sql`; store methods (`MarkMentionsRouted`,
      `UnroutedMentionMessages`) with pgtest cycle. Lane: compass-comms.
- [ ] T2 — factor `routeMentionsFor` out of `routeMentions`; add
      `scanMissedMentions` (`c.held`-membership skip, mark-after-process),
      unit cycle with fake store/resolver. Lane: compass-comms.
- [ ] T3 — wire the scan at consumer start + the `Lagged()` overrun branch;
      live-path marking in `fanOut`; consumer tests for both recovery points.
      Lane: compass-comms.
- [ ] T4 — acceptance pgtest: fresh-bus event severance ⇒ owed row on next
      start ⇒ exactly one steer ⇒ ack clears; negative-control leg;
      agent-authored held-then-restart leg; Lagged-overrun leg. Lane:
      compass-comms.
- [ ] T5 — ledger. This design PR ADDS the closure decision row (DL-240,
      recording the marker mechanism + the killed-cursor hazard) and carries a
      `Ledger-impact:` line — the design pass has already appended it. The IMPL
      PR that lands T1–T4 then annotates the DL-226 Status cell
      (residual-pre-settle-window → closed by DL-240/RIG-2490, amend-not-flip
      per the ledger's immutable-Decision-cell rule) once the code that
      actually closes the window merges — never before. `tools/design-ledger-gate`
      enforces the touch-coupling on both PRs. Lane: compass-comms.

## Resolved decisions

**RD-1 — Mechanism: per-message marker, not a global high-water cursor
(ruled by Matt after the design red-team).** The drafted mechanism — a
single-row `mention_scan_cursor` high-water over `messages.seq`, with
held-floor arithmetic — is KILLED for a commit-visibility hazard the red-team
found: `seq` is `BIGSERIAL` assigned at INSERT (`0001_init.sql:226`), but the
bus event publishes only after the post txn commits (`comms.go:361-375`:
`c.store.AppendMessage(…)` … `if inserted { c.publishMessagePosted(msg) }`),
so seq order ≠ commit-visibility order. A scan (or the live path) processing
a higher-seq message can advance the high-water past a LOWER seq whose txn is
still in flight; when that txn commits, its row sits below the high-water and
is excluded from every future scan — and if its bus event then dies in a
crash, the mention is PERMANENTLY lost. The closure mechanism would itself
have introduced a new silent loss mode. The per-message
`mentions_routed_at` marker carries no PERSISTED ordering state: a row is
visible to the scan exactly when its commit is visible, and an unprocessed
row is still NULL, so the hazard is structurally inexpressible. (The recovery
scan's batch loop does carry a scan-LOCAL, in-memory ascending-seq floor for
batch termination — see T1/T2; it is reset to zero at every recovery
invocation and never persisted, so it cannot exclude a late-committing lower
seq the way the killed high-water would, and the hazard stays inexpressible.)
Also resolves the
drafted OQ-2 (no new schema TABLE; the marker column + partial index replace
the cursor row) and the same-txn-write fork (rejected Option B: covers only
human-authored messages, since an agent-authored mention set is not final at
post, `consumer.go:333-336`; inverts the parse layer, `consumer.go:394-456`;
and still needs the scan for overrun symmetry — see §Approach).

**RD-2 — First deploy seeds forward; no historical backfill (Matt: compass
isn't live yet, not an issue).** Existing rows are non-NULL after migration
so the first scan sees nothing; only rows inserted after the feature ships
carry NULL until their settle pass. Under the squashed single-migration
convention this is correct-by-construction (no pre-column rows exist on a
recreated pre-dogfood database); T1 records the seed-forward intent for any
future incremental migration. Replaces the drafted OQ-3.

**RD-3 — The scan's skip predicate is `c.held` MEMBERSHIP, not
author-liveness.** The draft's claim that "a message whose agent author has a
LIVE session is skipped (the live settle path owns it)" was FALSE for an
overrun-dropped agent-authored event: it never entered `c.held`
(`handleEvent` never ran, `consumer.go:337-343`), so its settle edge never
fires (`fireHeld` drains only the registered ids, `settle.go:258-263`) — yet
its author session may be live, so a liveness-keyed skip would exclude it
forever: owned by NEITHER path. The fixed predicate: skip iff `message_id` ∈
`c.held` (`consumer.go:150-171`) — exactly the set the live settle path
genuinely owns. With the marker there is no floor to pin: a held message
stays NULL and is re-scannable at every recovery point until settled.

**RD-4 — The scan replays the FULL shared `routeMentions` body, and that is
spam-free under the marker.** The red-team's re-steer-spam concern (replayed
historical mentions interrupting live sessions) was a consequence of the
killed cursor's stale-low re-scan window plus backfill-from-zero. The marker
scan sees only never-completed settle passes; handled mentions are marked and
structurally excluded. Scoping the replay to a subset would reintroduce a
second policy surface (prohibited) and would break the live gap-population
case: a live member outside the sweep set whose event was dropped is reached
only by the live-steer arm (`dispatch.go:146-149`). Residual at-least-once —
a crash between processing and marking ⇒ one duplicate pass — is absorbed by
the agent-side per-session dedup (frozen record :423-427) and
`RecordOwedMention`'s upsert (`delivery_cursors.go:92-94`).

### Non-load-bearing deferrals (explicit)

- Retention/pruning of `owed_mentions` rows for permanently-dead agents —
  unchanged from RIG-1641 (`recorded_at_unix_ms` "bounds a future retention
  sweep", `delivery_cursors.go:79-89` docstring context;
  `0001_init.sql:479-480`).
- The cross-channel `above_seqs` advance-stall parked at
  `delivery_cursors.go:220-227` — pre-existing, untouched by this record.
- Scan batching size tuning (T1's `limit`) — an implementation constant, not
  a design fork.
